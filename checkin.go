package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"qoder2api/account"
	"qoder2api/internal/bridge"
	"qoder2api/logger"
)

// checkinHost 是签到 API 的兜底域名（国内）。国际走 openapi.qoder.sh，
// 由 checkinHostFor 按账号区域选择——此前写死国内域名导致国际号签到
// 返回 401 TOKEN_EXPIRE（拿国际 token 打国内端点）。
const checkinHost = "openapi.qoder.com.cn"

// checkinHostFor 返回该区域的签到/OpenAPI 主机。
func checkinHostFor(region account.Region) string {
	if h := account.GetEndpoints(region).OpenAPIHost; h != "" {
		return h
	}
	return checkinHost
}

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

	// CampaignsVisible 上游是否下发了任何活动条目。
	//
	// 为什么单独记这个：活动列表为空是很重要的风险信号——它通常意味着
	// 「服务端按账号灰度不下发活动」（设备身份不符 / 被判定为非官方客户端），
	// 而不是「今天真的没活动」。这个信号容易被忽略（表现为普通失败），
	// 但它恰恰是签到风控最直接的证据。
	CampaignsVisible bool `json:"campaigns_visible"`
	// CampaignCount 上游下发的活动条目数（0 = 未下发任何活动）。
	CampaignCount int `json:"campaign_count"`
	// HasClaimableActivity 是否存在可领取（CLAIMABLE）的活动。
	HasClaimableActivity bool `json:"has_claimable_activity"`
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

// checkinHeaders 构造签到请求头。
// 必须携带完整 cosy 设备指纹：国际版缺少这些头时服务端不下发签到活动。
//
// 关键结论（2026-10-13 对照实验，单变量逐一验证）：
//
//	国内版（openapi.qoder.com.cn）：
//	  - 发送自造 cosy-machinetoken → 服务端不下发 CLAIM_BEFEFIT 活动（no_campaign）
//	  - 完全不发送指纹头            → 正常下发 ✅
//	  - 因此 CN 只发必要头，刻意不发自造指纹
//
//	国际版（openapi.qoder.sh）：
//	  - 不发送指纹头                → 不下发活动 ❌
//	  - 发送官方客户端真实指纹      → 正常下发 ✅
//	  - 自造指纹同样无效（服务端校验签发来源）
//	  - 因此 Global 需要从账号配置读取官方抓包指纹
//
// fp 为 nil 时只发必要头（CN 行为）。
func checkinHeaders(deviceToken string, fp *account.CheckinFingerprint) map[string]string {
	h := map[string]string{
		"authorization":   "Bearer " + deviceToken,
		"accept":          "application/json",
		"accept-language": "zh-CN",
		"user-agent":      "Qoder",
		"cosy-clienttype": "10",
	}
	// 有真实指纹才附带（国际版必需；国内版留空）
	if fp != nil && fp.MachineToken != "" {
		h["cosy-version"] = orDefault(fp.Version, "0.4.4")
		h["cosy-machineid"] = fp.MachineID
		h["cosy-machinetoken"] = fp.MachineToken
		h["cosy-machinetype"] = fp.MachineType
		if fp.MachineCode != "" {
			h["cosy-machinecode"] = fp.MachineCode
		}
		h["cosy-machinehostname"] = orDefault(fp.Hostname, "localhost")
		h["cosy-machineos"] = orDefault(fp.MachineOS, "aarch64_darwin")
	}
	return h
}

// checkinDeviceHeaders 构造签到请求头（携带持久化的设备身份）。
//
// ident 是该账号的设备身份：官方抓包的真实指纹优先，回退到该账号持久化的
// 设备身份（推理侧算出来的那份）。
//
// 为什么要持久化而不是签到时现算：签到路径拿不到 uid（签到不做 userinfo
// 往返），现算只能按凭证派生——那样签到与推理就是两套设备身份，上游看到
// 的是「同一账号在两台设备上活动」，这是最容易被风控判定识别的形态。
//
// fullDeviceHeaders=false 时不携带完整设备头集合：某些活动场景带了完整
// 集合反而会被判非官方客户端，故做成可配置而非硬编码（可观测、可调整）。
//
// 身份为空时退回到旧行为（只发必要头，CN 形态）。
func checkinDeviceHeaders(deviceToken string, ident *account.DeviceIdentity, fullDeviceHeaders bool) map[string]string {
	h := checkinHeaders(deviceToken, nil)
	if ident == nil || ident.MachineToken == "" {
		return h
	}
	// clienttype / version 与推理侧保持一致（同一套客户端身份）
	if ident.ClientType != "" {
		h["cosy-clienttype"] = ident.ClientType
	}
	if ident.Version != "" {
		h["cosy-version"] = ident.Version
	}
	h["cosy-machineid"] = ident.MachineID
	if !fullDeviceHeaders {
		return h
	}
	h["cosy-machinetoken"] = ident.MachineToken
	h["cosy-machinetype"] = ident.MachineType
	if ident.MachineCode != "" {
		h["cosy-machinecode"] = ident.MachineCode
	}
	if ident.Hostname != "" {
		h["cosy-machinehostname"] = ident.Hostname
	}
	if ident.MachineOS != "" {
		h["cosy-machineos"] = ident.MachineOS
	}
	return h
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// doCheckinRequest 发送签到相关请求，返回 (httpStatus, parsedJSON, rawBody)
// reqBody 为 nil 时发送空 body（抓包确认 campaigns/claim 即为空 body）
//
// client 为该账号的出口客户端（账号级代理，见 checkinHTTPClientFor）；
// 传 nil 时退回包级直连客户端（仅测试 / 排障用）。
func doCheckinRequest(method, path, deviceToken string, reqBody interface{}, host string, fp *account.CheckinFingerprint, client *http.Client) (int, interface{}, string) {
	return doCheckinRequestWithIdentity(method, path, deviceToken, reqBody, host, fp, nil, client)
}

// checkinHTTPClientFor 该账号的签到 HTTP 客户端（账号级出口，与对话同一条）。
//
// 为什么签到也必须走账号代理：「对话走代理、签到直连」的双出口画像是
// 上游把同一账号关联为「代理池群体」的典型信号——比共用出口更可疑。
// 账号代理配置非法时返回 error，由调用方把该账号标记为签到失败并提示，
// **绝不静默直连**（真实出口暴露比签到失败更糟）。
func checkinHTTPClientFor(acct *account.Account) (*http.Client, error) {
	if acct == nil {
		return nil, fmt.Errorf("nil account")
	}
	binding := bridge.ResolveAccountProxyBinding(acct.ID, acct.Proxy, string(acct.Region))
	if binding.ConfigErr != nil {
		return nil, binding.ConfigErr
	}
	// 缓存 key 与对话侧一致（region|account）：同账号的签到与对话复用同一
	// Transport——出口与连接池都同源，「同一账号同一出口」可自证。
	return bridge.HTTPClientForKey(string(acct.Region)+"|"+acct.ID, binding.URL, 60*time.Second), nil
}

// doCheckinRequestWithIdentity 发送签到相关请求（携带指定设备身份 + 账号出口）。
//
// ident 非空时用它构造设备头（与推理侧同源）；为空时退回 fp / 无设备头。
// client 为该账号的出口客户端（见 checkinHTTPClientFor）；nil 退回直连。
func doCheckinRequestWithIdentity(method, path, deviceToken string, reqBody interface{}, host string, fp *account.CheckinFingerprint, ident *account.DeviceIdentity, client *http.Client) (int, interface{}, string) {
	if host == "" {
		host = checkinHost
	}
	url := "https://" + host + path

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
	var headers map[string]string
	if ident != nil {
		headers = checkinDeviceHeaders(deviceToken, ident, CheckinFullDeviceHeaders)
	} else {
		headers = checkinHeaders(deviceToken, fp)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if method == "POST" {
		req.Header.Set("origin", "https://"+host)
		if reqBody == nil {
			req.ContentLength = 0 // 抓包确认 claim 无 body
		}
	}

	if client == nil {
		client = checkinClient
	}
	resp, err := client.Do(req)
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

// checkinAccountFn 是签到单个账号的入口（包级变量，便于测试打桩）。
// 生产路径恒为 checkinAccount。
var checkinAccountFn = checkinAccount

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
	host := checkinHostFor(acct.Region)
	// 出口：该账号绑定的代理（与对话同一条）。配置非法时明确失败——
	// 绝不静默直连把真实出口暴露给上游。
	checkinClient, cerr := checkinHTTPClientFor(acct)
	if cerr != nil {
		res.Status = checkinStatusError
		res.Message = fmt.Sprintf("账号代理配置非法，已拒绝签到（直连会暴露真实出口）: %v", cerr)
		logger.Error("[Checkin] account %s: %v", acct.ID, cerr)
		return res
	}
	// 设备身份：与推理请求同源（见 checkinDeviceHeaders 的说明）。
	// 来源：账号配置的真实指纹 > 账号持久化的设备身份 > 无（退回旧行为）。
	ident := checkinIdentityOf(acct)
	r := campaignsCheckinWithIdentity(deviceToken, &res, host, acct.CheckinFingerprint, ident, checkinClient)

	// 只读补充 legacy 统计（DISABLED 时恒 0，不影响结果；上游恢复后可提供 streak）
	readDailyCheckinStatsWithIdentity(deviceToken, &r, host, acct.CheckinFingerprint, ident, checkinClient)

	return finalizeCheckin(acct.ID, r)
}

// checkinIdentityOf 取出该账号签到时应使用的设备身份。
//
// 优先级：
//  1. 账号配置的真实设备指纹（官方抓包）——国际版必需；
//  2. 账号持久化的设备身份（推理侧算出来的那份，DeviceIdentity 字段）；
//  3. nil —— 退回旧行为（只发必要头，CN 形态）。
//
// 为什么 2 也要有：推理侧按 uid 派生指纹，签到侧拿不到 uid。若签到自己
// 派生一份，两侧就是两套设备身份——上游看到的是「同一账号在两台设备上
// 活动」，这是最容易被风控判定识别的形态，且对签到的后果隐蔽（活动被
// 隐藏，账号看不到权益）。
func checkinIdentityOf(acct *account.Account) *account.DeviceIdentity {
	if acct == nil {
		return nil
	}
	if fp := acct.CheckinFingerprint; fp != nil && fp.MachineToken != "" {
		return &account.DeviceIdentity{
			MachineID:    fp.MachineID,
			MachineToken: fp.MachineToken,
			MachineType:  fp.MachineType,
			ClientType:   orDefault(fp.ClientType, "10"),
			Version:      orDefault(fp.Version, "1.1.69"),
			Hostname:     fp.Hostname,
			MachineOS:    fp.MachineOS,
			MachineCode:  fp.MachineCode,
			Real:         true,
		}
	}
	return acct.DeviceIdentity
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
func readDailyCheckinStats(deviceToken string, res *CheckinResult, host string, fp *account.CheckinFingerprint, client *http.Client) {
	readDailyCheckinStatsWithIdentity(deviceToken, res, host, fp, nil, client)
}

// readDailyCheckinStatsWithIdentity 只读获取 legacy 统计（携带指定设备身份 + 账号出口）。
func readDailyCheckinStatsWithIdentity(deviceToken string, res *CheckinResult, host string, fp *account.CheckinFingerprint, ident *account.DeviceIdentity, client *http.Client) {
	status, body, raw := doCheckinRequestWithIdentity("GET", "/sash/api/v1/me/daily-check-in/status", deviceToken, nil, host, fp, ident, client)
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
func campaignsCheckin(deviceToken string, res *CheckinResult, host string, fp *account.CheckinFingerprint, client *http.Client) CheckinResult {
	return campaignsCheckinWithIdentity(deviceToken, res, host, fp, nil, client)
}

// campaignsCheckinWithIdentity campaigns 流程（携带指定设备身份 + 账号出口）。
func campaignsCheckinWithIdentity(deviceToken string, res *CheckinResult, host string, fp *account.CheckinFingerprint, ident *account.DeviceIdentity, client *http.Client) CheckinResult {
	// Step 1: 查询活动列表
	status, body, raw := doCheckinRequestWithIdentity("GET", "/sash/api/v1/me/campaigns", deviceToken, nil, host, fp, ident, client)
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

	// 活动可见性信号：活动列表为空是很重要的风险信号（服务端按账号灰度
	// 不下发活动，常见于设备身份不符 / 被判定为非官方客户端），
	// 容易被当成普通失败而忽略，故显式记录下来供诊断。
	res.CampaignsVisible = len(campaigns) > 0
	res.CampaignCount = len(campaigns)

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
			res.HasClaimableActivity = true
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
			// 打印原始活动列表，便于排查「为何不下发 CLAIM_BENEFIT」。
			// 已知原因（见 issue #4）：Global 账号首次领取前需携带官方真实
			// cosy 设备指纹（machineid + machinetoken + machinetype），否则
			// 服务端按账号灰度不下发活动；首次领取后转为懒下发，之后无需指纹。
			actionTypes := make([]string, 0, len(campaigns))
			for i := range campaigns {
				actionTypes = append(actionTypes, campaigns[i].ActionType+"/"+campaigns[i].ClaimStatus)
			}
			logger.Info("[Checkin] no CLAIM_BENEFIT: account fingerprint=%v, campaigns=%v, raw=%s",
				fp != nil, actionTypes, truncate(raw, 400))
		}
		return *res
	}

	// Step 2: 领取（空 body，抓包确认）
	claimPath := fmt.Sprintf("/sash/api/v1/me/campaigns/%s/claim", target.CampaignID)
	status, body, raw = doCheckinRequestWithIdentity("POST", claimPath, deviceToken, nil, host, fp, ident, client)
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
//
// 账号之间强制最小间隔 + 随机抖动（见 checkin_antirisk.go）：批量签到若
// 瞬间完成，节奏特征过于规整，容易被活动平台的判定识别。
func CheckinAll() []CheckinResult {
	return CheckinAllContext(context.Background())
}

// CheckinAllContext 对所有有 secret 的账号执行签到（可取消）。
//
// ctx 取消时停止后续账号的签到：取消意味着「不要继续打了」，继续打完
// 反而违背调用方的意图。
func CheckinAllContext(ctx context.Context) []CheckinResult {
	return checkinAll(ctx, time.Time{})
}

// CheckinAllAt 在指定基准时刻点执行签到，每个账号按自己的固定偏移错峰。
//
// base 为零值时退化为「立即执行」（手动触发 / 立即签到按钮）。
// 非零时：每个账号的实际触发时刻 = base + 该账号的固定偏移。
//
// 为什么要按账号错峰：所有账号在同一秒打签到，是非常明显的自动化特征。
// 偏移由账号 ID 哈希派生（跨天稳定），不同账号自然分散。
func CheckinAllAt(base time.Time) []CheckinResult {
	return checkinAll(context.Background(), base)
}

// checkinAll 执行签到主循环。
//
// base 非零时按账号错峰（每个账号等到自己的触发时刻再打）。
func checkinAll(ctx context.Context, base time.Time) []CheckinResult {
	accounts, err := account.List()
	if err != nil {
		return []CheckinResult{{
			Status:  checkinStatusError,
			Message: fmt.Sprintf("读取账号失败: %v", err),
		}}
	}

	var results []CheckinResult
	// done 标记是否已经打过至少一个账号——第一个账号不等待间隔。
	done := false
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
		// 错峰：等到该账号自己的触发时刻。
		if !base.IsZero() {
			target := checkinSlotTime(base, acct.ID)
			if wait := time.Until(target); wait > 0 {
				if serr := sleepCtxCtx(ctx, wait); serr != nil {
					logger.Info("[Checkin] cancelled before account %s: %v", acct.Name, serr)
					break
				}
			}
		}
		if done {
			if serr := sleepCheckinGap(ctx); serr != nil {
				logger.Info("[Checkin] cancelled before account %s: %v", acct.Name, serr)
				break
			}
		}
		r := checkinAccountFn(acct)
		logger.Info("[Checkin] account=%s status=%s msg=%s", r.Account, r.Status, r.Message)
		results = append(results, r)
		done = true
	}
	return results
}

// sleepCtxCtx 等待指定时长，可被 ctx 取消。
func sleepCtxCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
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

// ---------- 每日自动签到调度 ----------

var (
	checkinMu   sync.Mutex
	checkinDone = map[string]bool{} // key: "YYYY-MM-DD HH:MM"，当日该时段已执行
)

// defaultCheckinTimes 未配置时的默认签到时间（保持原行为）。
var defaultCheckinTimes = []string{"10:00"}

// CheckinTimes 返回生效的自动签到时间列表（HH:MM）。
func CheckinTimes(settings *account.Settings) []string {
	if settings != nil && len(settings.AutoCheckinTimes) > 0 {
		return settings.AutoCheckinTimes
	}
	return defaultCheckinTimes
}

// parseHHMM 解析 "09:15" → (9, 15, true)
func parseHHMM(s string) (int, int, bool) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return 0, 0, false
	}
	h, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	m, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

// StartCheckinScheduler 启动后台调度器（非阻塞）。每分钟检查一次：对每个配置的
// 时间点，若开关开启、当前已过该点、且当日该点尚未执行，则自动签到。
func StartCheckinScheduler() {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			runScheduledCheckin()
		}
	}()
	logger.Info("auto checkin scheduler started (times=%s, default off)", strings.Join(CheckinTimes(nil), ","))
}

// runScheduledCheckin 执行一次调度检查（可手动调用测试）。
func runScheduledCheckin() bool {
	settings, err := account.LoadSettings()
	if err != nil || settings == nil || !settings.AutoCheckin {
		return false
	}
	now := time.Now()
	date := now.Format("2006-01-02")
	nowMin := now.Hour()*60 + now.Minute()

	ran := false
	for _, t := range CheckinTimes(settings) {
		h, m, ok := parseHHMM(t)
		if !ok {
			logger.Error("[Checkin] invalid auto checkin time %q, skipped", t)
			continue
		}
		slotMin := h*60 + m
		// 进入错峰窗口就开始跑：账号的实际触发时刻 = 设定时刻 + 该账号的
		// 固定偏移（可能是负的，即早于设定时刻）。不提前进窗口的话，
		// 负偏移的账号会被挤到设定时刻之后，错峰就只剩一半生效。
		windowMin := staggerWindowMinutes()
		if nowMin < slotMin-windowMin {
			continue // 还没进入错峰窗口
		}
		key := date + " " + t
		checkinMu.Lock()
		done := checkinDone[key]
		checkinMu.Unlock()
		if done {
			continue
		}

		// 该时刻点的基准时间（当天 HH:MM），账号偏移在此基础上叠加。
		base := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
		logger.Info("[Checkin] auto checkin triggered at %s (slot %s, stagger window %ds)",
			now.Format("15:04:05"), t, windowMin*60)
		results := CheckinAllAt(base)
		claimed, already, retryable := 0, 0, 0
		for _, r := range results {
			switch r.Status {
			case checkinStatusClaimed:
				claimed++
			case checkinStatusAlreadyClaimed:
				already++
			case checkinStatusError, checkinStatusNoCampaign, checkinStatusNoToken:
				retryable++
			}
		}
		checkinMu.Lock()
		if retryable == 0 || nowMin >= slotMin+windowMin+60 {
			// 无可重试状态，或已过该时段（错峰窗口 + 1 小时兜底，避免无限轮询）
			checkinDone[key] = true
			logger.Info("[Checkin] slot %s finished: %d claimed, %d already, %d retryable, total %d",
				t, claimed, already, retryable, len(results))
		} else {
			logger.Info("[Checkin] slot %s partial: %d claimed, %d already, %d retryable -> retry next tick",
				t, claimed, already, retryable)
		}
		checkinMu.Unlock()
		ran = true
	}
	return ran
}

// staggerWindowMinutes 返回错峰窗口（分钟）。
func staggerWindowMinutes() int {
	sec := CheckinStaggerSec
	if sec <= 0 {
		sec = DefaultCheckinStaggerSec
	}
	return (sec + 59) / 60
}
