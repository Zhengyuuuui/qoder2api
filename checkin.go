package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
		"authorization":     "Bearer " + deviceToken,
		"accept":            "application/json",
		"accept-language":   "zh-CN",
		"user-agent":        "Qoder",
		"cosy-clienttype":   "10",
	}
}

// doCheckinRequest 发送签到相关请求，返回 (httpStatus, parsedJSON, rawBody)
func doCheckinRequest(method, path, deviceToken string) (int, interface{}, string) {
	url := "https://" + checkinHost + path

	var body io.Reader
	if method == "POST" {
		body = nil // 抓包确认 claim 请求体为空
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
		req.ContentLength = 0
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
	CampaignID string `json:"campaignId"`
	CampaignKey string `json:"campaignKey"`
	ActionType string `json:"actionType"`
	ClaimStatus string `json:"claimStatus"`
	Benefit    *struct {
		Kind   string `json:"kind"`
		Amount int    `json:"amount"`
	} `json:"benefit"`
}

// claimResponse 领取响应
type claimResponse struct {
	GrantID   string `json:"grantId"`
	Status    string `json:"status"`
	Replayed  bool   `json:"replayed"`
	Benefit   *struct {
		Kind   string `json:"kind"`
		Amount int    `json:"amount"`
		Validity *struct {
			Mode string `json:"mode"`
			Days int    `json:"days"`
		} `json:"validity"`
	} `json:"benefit"`
	ExpiresAt string `json:"expiresAt"`
}

// checkinAccount 对单个账号执行签到
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

	// Step 1: 查询活动列表
	status, body, raw := doCheckinRequest("GET", "/sash/api/v1/me/campaigns", deviceToken)
	if status != 200 {
		res.Message = fmt.Sprintf("查询活动失败 HTTP %d: %s", status, truncate(raw, 300))
		return res
	}

	list, ok := body.(map[string]interface{})
	if !ok {
		res.Message = fmt.Sprintf("活动列表格式异常: %s", truncate(raw, 300))
		return res
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

	// 找可领取的 CLAIM_BENEFIT 活动
	var target *campaignInfo
	alreadyClaimed := false
	for i := range campaigns {
		c := &campaigns[i]
		if c.ActionType != "CLAIM_BENEFIT" {
			continue
		}
		if c.ClaimStatus == "CLAIMABLE" {
			target = c
		} else if c.ClaimStatus == "CLAIMED" {
			alreadyClaimed = true
		}
	}

	if target == nil {
		if alreadyClaimed {
			res.Status = checkinStatusAlreadyClaimed
			res.Message = "今日已领取"
		} else {
			res.Status = checkinStatusNoCampaign
			res.Message = "无可用签到活动"
		}
		return res
	}

	// Step 2: 领取
	claimPath := fmt.Sprintf("/sash/api/v1/me/campaigns/%s/claim", target.CampaignID)
	status, body, raw = doCheckinRequest("POST", claimPath, deviceToken)
	if status != 200 {
		res.Message = fmt.Sprintf("领取失败 HTTP %d: %s", status, truncate(raw, 300))
		return res
	}

	var cr claimResponse
	b, _ := json.Marshal(body)
	if json.Unmarshal(b, &cr) != nil {
		res.Message = fmt.Sprintf("领取响应格式异常: %s", truncate(raw, 300))
		return res
	}

	if cr.Status == "CLAIMED" {
		if cr.Replayed {
			res.Status = checkinStatusAlreadyClaimed
			res.Message = "今日已领取（幂等返回）"
		} else {
			res.Status = checkinStatusClaimed
			res.Message = fmt.Sprintf("领取成功 %s", target.CampaignKey)
			if cr.Benefit != nil {
				res.Amount = cr.Benefit.Amount
			}
			res.ExpiresAt = cr.ExpiresAt
		}
		return res
	}

	res.Message = fmt.Sprintf("未知状态: %s", cr.Status)
	return res
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
	checkinMu       sync.Mutex
	lastCheckinDay  string // 最近一次自动签到的日期 (YYYY-MM-DD)，防止同日重复
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
	lastCheckinDay = today

	logger.Info("[Checkin] auto checkin triggered at %s", now.Format("15:04:05"))
	results := CheckinAll()
	claimed := 0
	for _, r := range results {
		if r.Status == checkinStatusClaimed {
			claimed++
		}
	}
	logger.Info("[Checkin] auto checkin done: %d claimed, total %d accounts", claimed, len(results))
	return true
}
