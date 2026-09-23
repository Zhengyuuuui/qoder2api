package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"qoder2api/account"
	"qoder2api/logger"
)

// checkinHost 是签到 API 域名（抓包确认为 openapi.qoder.com.cn）
const checkinHost = "openapi.qoder.com.cn"

// CheckinResult 单个账号的签到结果
type CheckinResult struct {
	Account   string `json:"account"`
	AccountID string `json:"account_id"`
	Status    string `json:"status"` // claimed / already_claimed / no_campaign / error / no_token
	Amount    int    `json:"amount,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	Message   string `json:"message"`

	// 签到统计（来自 daily-check-in/status 或本地历史）
	StreakDays         int `json:"streak_days,omitempty"`          // 连续签到天数
	TotalClaimDays     int `json:"total_claim_days,omitempty"`     // 累计签到天数
	TotalRewardCredits int `json:"total_reward_credits,omitempty"` // 累计奖励积分
	RewardCredits      int `json:"reward_credits,omitempty"`       // 每日签到奖励额

	// WindowDate 本次领取对应的活动窗口日期（YYYY-MM-DD），用于本地历史记录。
	// 用窗口日期而非自然日，避免 10:00 前把「昨天的窗口」误记到今天。
	WindowDate string `json:"-"`
}

const (
	checkinStatusClaimed        = "claimed"
	checkinStatusAlreadyClaimed = "already_claimed"
	checkinStatusNoCampaign     = "no_campaign"
	checkinStatusNoToken        = "no_token"
	checkinStatusError          = "error"
)

// checkinClient 复用系统证书的 HTTPS 客户端
var checkinClient = &http.Client{
	Timeout: 60 * time.Second,
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	},
}

// checkinHeaders 构造桌面端签到请求头（抓包确认的必需头）
func checkinHeaders(deviceToken string) map[string]string {
	return map[string]string{
		"authorization":   "Bearer " + deviceToken,
		"accept":          "application/json",
		"accept-language": "zh-CN",
		"user-agent":      "Qoder",
		"cosy-clienttype": "10",
	}
}

// doCheckinRequest 发送签到相关请求，返回 (httpStatus, parsedJSON, rawBody)
// reqBody 为 nil 时发送空 body（抓包确认 campaigns/claim 即为空 body）
func doCheckinRequest(method, path, deviceToken string, reqBody interface{}) (int, interface{}, string) {
	url := "https://" + checkinHost + path

	var body io.Reader
	var bodyBytes []byte
	if reqBody != nil {
		bodyBytes, _ = json.Marshal(reqBody)
		body = bytes.NewReader(bodyBytes)
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return 0, nil, fmt.Sprintf("build request: %v", err)
	}
	for k, v := range checkinHeaders(deviceToken) {
		req.Header.Set(k, v)
	}
	if method == "POST" {
		req.Header.Set("origin", "https://"+checkinHost)
		if reqBody == nil {
			req.ContentLength = 0 // 抓包确认 claim 无 body
		}
	}

	resp, err := checkinClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Sprintf("do request: %v", err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	raw := string(data)

	var parsed interface{}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &parsed)
	}
	return resp.StatusCode, parsed, raw
}

// campaignInfo 活动列表中的活动条目
type campaignInfo struct {
	CampaignID  string `json:"campaignId"`
	CampaignKey string `json:"campaignKey"`
	ActionType  string `json:"actionType"`
	ClaimStatus string `json:"claimStatus"`
	StartAt     int64  `json:"startAt"` // 窗口开始时间（Unix 秒）
	EndAt       int64  `json:"endAt"`
	Benefit     *struct {
		Kind   string `json:"kind"`
		Amount int    `json:"amount"`
	} `json:"benefit"`
}

// claimResponse 领取响应
type claimResponse struct {
	GrantID  string `json:"grantId"`
	Status   string `json:"status"`
	Replayed bool   `json:"replayed"`
	Benefit  *struct {
		Kind     string `json:"kind"`
		Amount   int    `json:"amount"`
		Validity *struct {
			Mode string `json:"mode"`
			Days int    `json:"days"`
		} `json:"validity"`
	} `json:"benefit"`
	ExpiresAt string `json:"expiresAt"`
}

// dailyCheckinStatus GET /sash/api/v1/me/daily-check-in/status 响应
type dailyCheckinStatus struct {
	CampaignKey        string `json:"campaignKey"` // 例: cn_daily_check_in_legacy
	Status             string `json:"status"`      // CLAIMABLE | CLAIMED | DISABLED
	RewardCredits      int    `json:"rewardCredits"`
	CurrentStreakDays  int    `json:"currentStreakDays"`
	TotalClaimDays     int    `json:"totalClaimDays"`
	TotalRewardCredits int    `json:"totalRewardCredits"`
}

// ---------- 本地签到历史（计算连续天数） ----------
// 上游 daily-check-in legacy 系统已全局 DISABLED（streak 恒为 0），
// 因此由 qoder2api 本地记录签到日期，计算真实连续签到天数。
// 数据存于 <DataRoot>/checkin_history.json

type checkinRecord struct {
	Date   string `json:"date"`   // YYYY-MM-DD
	Amount int    `json:"amount"` // 当次领取积分
}

type checkinHistory struct {
	Accounts map[string][]checkinRecord `json:"accounts"`
}

var historyMu sync.Mutex

func checkinHistoryPath() string {
	return filepath.Join(account.DataRoot(), "checkin_history.json")
}

func loadCheckinHistory() *checkinHistory {
	h := &checkinHistory{Accounts: map[string][]checkinRecord{}}
	data, err := os.ReadFile(checkinHistoryPath())
	if err != nil {
		return h
	}
	_ = json.Unmarshal(data, h)
	if h.Accounts == nil {
		h.Accounts = map[string][]checkinRecord{}
	}
	return h
}

func saveCheckinHistory(h *checkinHistory) {
	data, _ := json.MarshalIndent(h, "", "  ")
	_ = os.WriteFile(checkinHistoryPath(), data, 0600)
}

// calcLocalStats 计算连续天数与累计
// streak：今天已领从今天起数；今天未领则从昨天起数（当天未领不断链）
func calcLocalStats(recs []checkinRecord) (streak, totalDays, totalCredits int) {
	if len(recs) == 0 {
		return 0, 0, 0
	}
	days := map[string]bool{}
	for _, r := range recs {
		days[r.Date] = true
		totalCredits += r.Amount
	}
	totalDays = len(recs)

	cursor := time.Now()
	if !days[cursor.Format("2006-01-02")] {
		cursor = cursor.AddDate(0, 0, -1)
	}
	for days[cursor.Format("2006-01-02")] {
		streak++
		cursor = cursor.AddDate(0, 0, -1)
	}
	return streak, totalDays, totalCredits
}

// recordCheckinOn 记录该账号某日期签到（同日幂等），返回本地统计
func recordCheckinOn(accountID, date string, amount int) (streak, totalDays, totalCredits int) {
	historyMu.Lock()
	defer historyMu.Unlock()

	h := loadCheckinHistory()
	recs := h.Accounts[accountID]

	found := false
	for _, r := range recs {
		if r.Date == date {
			found = true
			break
		}
	}
	if !found {
		recs = append(recs, checkinRecord{Date: date, Amount: amount})
		h.Accounts[accountID] = recs
		saveCheckinHistory(h)
	} else if amount > 0 {
		// 已有记录但金额为 0（早前版本记录）→ 补记金额
		for i := range recs {
			if recs[i].Date == date && recs[i].Amount == 0 {
				recs[i].Amount = amount
				h.Accounts[accountID] = recs
				saveCheckinHistory(h)
				break
			}
		}
	}
	return calcLocalStats(recs)
}

// localCheckinStats 只读获取本地统计（不记录）
func localCheckinStats(accountID string) (streak, totalDays, totalCredits int) {
	historyMu.Lock()
	defer historyMu.Unlock()
	return calcLocalStats(loadCheckinHistory().Accounts[accountID])
}

// applyLocalStreak 用本地签到历史覆盖统计字段（本地数据比上游 legacy 更可信）
// record=true 时按 date 记录（date 空则用今天）；本地有记录时以本地为准
func applyLocalStreak(accountID string, res *CheckinResult, record bool, date string, amount int) {
	var streak, totalDays, totalCredits int
	if record {
		if date == "" {
			date = time.Now().Format("2006-01-02")
		}
		streak, totalDays, totalCredits = recordCheckinOn(accountID, date, amount)
	} else {
		streak, totalDays, totalCredits = localCheckinStats(accountID)
	}
	// 本地有记录 → 以本地为准；本地无记录 → 保留上游读到的值
	if totalDays > 0 {
		res.StreakDays = streak
		res.TotalClaimDays = totalDays
		res.TotalRewardCredits = totalCredits
	}
}

// checkinAccount 对单个账号执行签到
// 优先走 daily-check-in 简化端点（带连续签到统计），不可用时回退 campaigns 流程
func checkinAccount(acct *account.Account) CheckinResult {
	res := CheckinResult{
		Account:   acct.Name,
		AccountID: acct.ID,
		Status:    checkinStatusError,
	}

	secret, err := account.GetSecret(acct.ID)
	if err != nil {
		res.Message = fmt.Sprintf("读取 token 失败: %v", err)
		return res
	}

	// 解析 device token
	var deviceToken string
	if len(secret) > 0 && secret[0] == '{' {
		var s struct {
			DeviceToken string `json:"device_token"`
		}
		if json.Unmarshal([]byte(secret), &s) == nil {
			deviceToken = s.DeviceToken
		}
	} else {
		deviceToken = secret
	}
	if deviceToken == "" {
		res.Status = checkinStatusNoToken
		res.Message = "无 device token"
		return res
	}

	// ========== 权威领取：campaigns 流程（真实发放积分的系统） ==========
	// 注意：不走 daily-check-in/claim —— 该 legacy 端点已 DISABLED，
	// 却对未领取日也恒返回 409，会误判"已领取"导致跳过真实领取（实测 2026-09-21 不发积分）
	r := campaignsCheckin(deviceToken, &res)

	// 只读补充 legacy 统计（DISABLED 时恒 0，不影响结果；上游恢复后可提供 streak）
	readDailyCheckinStats(deviceToken, &r)

	return finalizeCheckin(acct.ID, r)
}

// finalizeCheckin 收尾：写入/读取本地签到历史并把统计拼进消息
// 领取成功或已领取 → 记录今日；其他状态只读统计
func finalizeCheckin(accountID string, r CheckinResult) CheckinResult {
	record := r.Status == checkinStatusClaimed || r.Status == checkinStatusAlreadyClaimed

	// 领取金额：响应值 → status 接口的 rewardCredits → 活动默认 100
	amount := r.Amount
	if amount == 0 {
		amount = r.RewardCredits
	}
	if amount == 0 {
		amount = 100
	}

	applyLocalStreak(accountID, &r, record, r.WindowDate, amount)
	// 领取类结果统一追加统计后缀（applyLocalStreak 已更新统计）
	if record || r.StreakDays > 0 || r.TotalClaimDays > 0 {
		r.Message = r.Message + streakSuffix(&r, "")
	}
	return r
}

// readDailyCheckinStats 只读获取 legacy daily-check-in 的统计（绝不通过它领取！）
// 背景：该端点的 legacy 活动已 DISABLED，claim 会恒返回 409 造成误判，
// 且不发放任何积分（2026-09-21 实测）。真实领取只走 campaigns 流程。
// 上游恢复后若返回非 0 统计，可作为 streak 的补充数据源。
func readDailyCheckinStats(deviceToken string, res *CheckinResult) {
	status, body, raw := doCheckinRequest("GET", "/sash/api/v1/me/daily-check-in/status", deviceToken, nil)
	if status != 200 {
		if status != 404 && status != 401 {
			logger.Info("[Checkin] daily-check-in/status HTTP %d: %s", status, truncate(raw, 150))
		}
		return
	}

	var st dailyCheckinStatus
	b, _ := json.Marshal(body)
	if json.Unmarshal(b, &st) != nil || st.Status == "" {
		return
	}
	logger.Info("[Checkin] daily-check-in(status only) state=%s key=%s streak=%d",
		st.Status, st.CampaignKey, st.CurrentStreakDays)

	// 仅当 legacy 返回非 0 统计时才补充（DISABLED 时恒 0，不覆盖本地数据）
	if st.CurrentStreakDays > 0 || st.TotalClaimDays > 0 {
		if st.CurrentStreakDays > res.StreakDays {
			res.StreakDays = st.CurrentStreakDays
		}
		if st.TotalClaimDays > res.TotalClaimDays {
			res.TotalClaimDays = st.TotalClaimDays
		}
		if st.TotalRewardCredits > res.TotalRewardCredits {
			res.TotalRewardCredits = st.TotalRewardCredits
		}
	}
	if res.RewardCredits == 0 && st.RewardCredits > 0 {
		res.RewardCredits = st.RewardCredits
	}
}

// streakSuffix 在消息后追加连续签到统计（无统计时不追加）
func streakSuffix(res *CheckinResult, prefix string) string {
	if res.StreakDays <= 0 && res.TotalClaimDays <= 0 {
		if prefix != "" {
			return prefix
		}
		return ""
	}
	s := fmt.Sprintf("（连续 %d 天 · 累计 %d 天 · 共 %d 积分）", res.StreakDays, res.TotalClaimDays, res.TotalRewardCredits)
	if prefix != "" {
		return prefix + s
	}
	return s
}

// windowHint 生成更准确的"已领取"提示
// 若当前窗口是上一日开的（10:00 前点击），明确告知新窗口开放时间，避免误以为没领到
func windowHint(res *CheckinResult, fallback string) string {
	if res.WindowDate == "" {
		return fallback
	}
	today := time.Now().Format("2006-01-02")
	if res.WindowDate == today {
		return fallback
	}
	if wd, err := time.Parse("2006-01-02", res.WindowDate); err == nil {
		next := wd.AddDate(0, 0, 1).Format("01-02")
		return fmt.Sprintf("已领取 %s 窗口额度（新窗口 %s 10:00 开放）", res.WindowDate, next)
	}
	return fallback
}

// campaignsCheckin 兜底：走桌面端抓包还原的 campaigns 流程
func campaignsCheckin(deviceToken string, res *CheckinResult) CheckinResult {
	// Step 1: 查询活动列表
	status, body, raw := doCheckinRequest("GET", "/sash/api/v1/me/campaigns", deviceToken, nil)
	if status != 200 {
		res.Message = fmt.Sprintf("查询活动失败 HTTP %d: %s", status, truncate(raw, 300))
		return *res
	}

	list, ok := body.(map[string]interface{})
	if !ok {
		res.Message = fmt.Sprintf("活动列表格式异常: %s", truncate(raw, 300))
		return *res
	}

	// 解析 campaigns
	campaignsRaw, _ := list["campaigns"].([]interface{})
	var campaigns []campaignInfo
	for _, c := range campaignsRaw {
		b, _ := json.Marshal(c)
		var ci campaignInfo
		if json.Unmarshal(b, &ci) == nil {
			campaigns = append(campaigns, ci)
		}
	}

	// 找可领取的 CLAIM_BENEFIT 活动，并记录窗口开始时间（用于本地历史日期）
	var target *campaignInfo
	var benefitCampaign *campaignInfo
	alreadyClaimed := false
	for i := range campaigns {
		c := &campaigns[i]
		if c.ActionType != "CLAIM_BENEFIT" {
			continue
		}
		benefitCampaign = c // 任意状态，用于取窗口日期
		if c.ClaimStatus == "CLAIMABLE" {
			target = c
		} else if c.ClaimStatus == "CLAIMED" {
			alreadyClaimed = true
		}
	}

	// 窗口日期：取 CLAIM_BENEFIT 活动的 startAt（本地时区）。
	// 每日窗口 10:00→次日 10:00，用窗口日期避免 10:00 前把昨日窗口误记到今天。
	windowSrc := target
	if windowSrc == nil {
		windowSrc = benefitCampaign
	}
	if windowSrc != nil && windowSrc.StartAt > 0 {
		res.WindowDate = time.Unix(windowSrc.StartAt, 0).Format("2006-01-02")
	}

	if target == nil {
		if alreadyClaimed {
			res.Status = checkinStatusAlreadyClaimed
			res.Message = windowHint(res, "今日已领取")
		} else {
			res.Status = checkinStatusNoCampaign
			res.Message = "无可用签到活动"
		}
		return *res
	}

	// Step 2: 领取（空 body，抓包确认）
	claimPath := fmt.Sprintf("/sash/api/v1/me/campaigns/%s/claim", target.CampaignID)
	status, body, raw = doCheckinRequest("POST", claimPath, deviceToken, nil)
	if status != 200 {
		res.Message = fmt.Sprintf("领取失败 HTTP %d: %s", status, truncate(raw, 300))
		return *res
	}

	var cr claimResponse
	b, _ := json.Marshal(body)
	if json.Unmarshal(b, &cr) != nil {
		res.Message = fmt.Sprintf("领取响应格式异常: %s", truncate(raw, 300))
		return *res
	}

	if cr.Status == "CLAIMED" {
		if cr.Replayed {
			res.Status = checkinStatusAlreadyClaimed
			res.Message = windowHint(res, "今日已领取")
		} else {
			res.Status = checkinStatusClaimed
			if cr.Benefit != nil {
				res.Amount = cr.Benefit.Amount
			}
			res.ExpiresAt = cr.ExpiresAt
			res.Message = fmt.Sprintf("领取成功 +%d %s", res.Amount, target.CampaignKey)
		}
		return *res
	}

	res.Message = fmt.Sprintf("未知状态: %s", cr.Status)
	return *res
}

// CheckinAll 对所有有 secret 的账号执行签到
func CheckinAll() []CheckinResult {
	accounts, err := account.List()
	if err != nil {
		return []CheckinResult{{
			Status:  checkinStatusError,
			Message: fmt.Sprintf("读取账号失败: %v", err),
		}}
	}

	var results []CheckinResult
	for i := range accounts {
		acct := &accounts[i]
		if !account.HasSecret(acct.ID) {
			results = append(results, CheckinResult{
				Account:   acct.Name,
				AccountID: acct.ID,
				Status:    checkinStatusNoToken,
				Message:   "无 secret，跳过",
			})
			continue
		}
		r := checkinAccount(acct)
		logger.Info("[Checkin] account=%s status=%s msg=%s", r.Account, r.Status, r.Message)
		results = append(results, r)
	}
	return results
}

// CheckinOne 对指定账号签到，账号不存在返回 error 结果
func CheckinOne(accountID string) CheckinResult {
	acct, err := account.Get(accountID)
	if err != nil || acct == nil {
		return CheckinResult{
			AccountID: accountID,
			Status:    checkinStatusError,
			Message:   fmt.Sprintf("账号不存在: %s", accountID),
		}
	}
	if !account.HasSecret(acct.ID) {
		return CheckinResult{
			Account:   acct.Name,
			AccountID: acct.ID,
			Status:    checkinStatusNoToken,
			Message:   "无 secret",
		}
	}
	r := checkinAccount(acct)
	logger.Info("[Checkin] single account=%s status=%s msg=%s", r.Account, r.Status, r.Message)
	return r
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// handleCheckin 处理 POST /api/checkin
// body 可选 {"account_id": "xxx"} 表示单账号签到；空 body 表示全账号签到
func handleCheckin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
		return
	}

	var req struct {
		AccountID string `json:"account_id"`
	}
	_ = readJSON(r, &req)

	if req.AccountID != "" {
		// 单账号签到
		result := CheckinOne(req.AccountID)
		summary := map[string]int{result.Status: 1}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"ok":      true,
			"summary": summary,
			"results": []CheckinResult{result},
		})
		return
	}

	// 全账号签到
	results := CheckinAll()
	summary := map[string]int{}
	for _, res := range results {
		summary[res.Status]++
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":      true,
		"summary": summary,
		"results": results,
	})
}

// ---------- 每日 10:00 自动签到调度 ----------

var (
	checkinMu      sync.Mutex
	lastCheckinDay string // 最近一次自动签到的日期 (YYYY-MM-DD)，防止同日重复
)

// StartCheckinScheduler 启动后台调度器（非阻塞）
// 每分钟检查一次：若开关开启且当前时间 >= 当日 10:00 且当日未执行，则自动签到
func StartCheckinScheduler() {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			runScheduledCheckin()
		}
	}()
	logger.Info("auto checkin scheduler started (daily 10:00, default off)")
}

// runScheduledCheckin 执行一次调度检查（可手动调用测试）
func runScheduledCheckin() bool {
	// 读取开关（默认关闭）
	settings, err := account.LoadSettings()
	if err != nil || settings == nil {
		return false
	}
	if !settings.AutoCheckin {
		return false
	}

	now := time.Now()
	today := now.Format("2006-01-02")

	// 必须已过 10:00
	if now.Hour() < 10 {
		return false
	}

	checkinMu.Lock()
	defer checkinMu.Unlock()

	// 当日已执行过
	if lastCheckinDay == today {
		return false
	}

	logger.Info("[Checkin] auto checkin triggered at %s", now.Format("15:04:05"))
	results := CheckinAll()

	claimed, already, retryable := 0, 0, 0
	for _, r := range results {
		switch r.Status {
		case checkinStatusClaimed:
			claimed++
		case checkinStatusAlreadyClaimed:
			already++
		case checkinStatusError, checkinStatusNoCampaign, checkinStatusNoToken:
			// 可能活动尚未创建（10:00 整点服务器有延迟）→ 允许重试
			retryable++
		}
	}

	// 标记当日完成的条件：无可重试状态；或已过 12:00（兜底不再重试，避免无限轮询）
	if retryable == 0 || now.Hour() >= 12 {
		lastCheckinDay = today
		logger.Info("[Checkin] auto checkin finished: %d claimed, %d already, %d retryable, total %d",
			claimed, already, retryable, len(results))
	} else {
		logger.Info("[Checkin] auto checkin partial: %d claimed, %d already, %d retryable -> will retry next tick",
			claimed, already, retryable)
	}
	return true
}
