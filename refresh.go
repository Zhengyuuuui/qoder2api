package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"qoder2api/account"
	"qoder2api/internal/bridge"
	"qoder2api/internal/cosy"
	"qoder2api/logger"
)

// ============================================================================
// Token 自动续期模块
//
// 背景：bridge 早前只保存 refresh_token 却从未使用它。当 device_token 与
// refresh_token 双双过期时，账号会静默失效（控制台额度为空、请求 401），
// 只能重新走 OAuth。
//
// 机制：Qoder 的 jobToken 端点支持 needRefresh 模式 —— 传入 drt- 前缀的
// refresh_token，服务端会签发新的 access token 并回传新的 refresh_token
// （见 internal/cosy/auth.go ExchangeJobToken）。本模块据此在 token 临近
// 过期前主动续期，避免双 token 同时死亡。
//
// 默认间隔 15 天；调度器每 6 小时检查一次，token 年龄超过 12 天即触发续期。
// ============================================================================

const (
	// RefreshIntervalDays 续期间隔（天）。
	RefreshIntervalDays = 15
	// refreshWindowDays 提前续期窗口：token 年龄 ≥ 该值即续期。
	refreshWindowDays = RefreshIntervalDays - 3
	// refreshTick 调度粒度。
	refreshTick = 6 * time.Hour
)

// RefreshReport 单账号续期结果。
type RefreshReport struct {
	AccountID    string `json:"account_id"`
	Account      string `json:"account"`
	Region       string `json:"region"`
	Status       string `json:"status"` // ok | skipped | failed | no_secret | not_needed
	Message      string `json:"message"`
	RefreshedAt  int64  `json:"refreshed_at,omitempty"`
	NextRefresh  int64  `json:"next_refresh_at,omitempty"`
	TokenAgeDays int    `json:"token_age_days"`
}

// tokenSecret secrets/<id>.token 的双 token 存储形态。
type tokenSecret struct {
	DeviceToken  string `json:"device_token"`
	RefreshToken string `json:"refresh_token"`
}

var (
	refreshMu   = make(chan struct{}, 1)
	refreshLast = map[string]RefreshReport{}
)

func init() { refreshMu <- struct{}{} }

// parseTokenSecret 解析 secret 内容，兼容双 token JSON 与 PAT 明文。
func parseTokenSecret(raw string) (deviceToken, refreshToken, pat string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", ""
	}
	if !strings.HasPrefix(raw, "{") {
		return "", "", raw
	}
	var ts tokenSecret
	if err := json.Unmarshal([]byte(raw), &ts); err == nil &&
		(ts.DeviceToken != "" || ts.RefreshToken != "") {
		return ts.DeviceToken, ts.RefreshToken, ""
	}
	return "", "", raw
}

// tokenAgeDays 取 secret 文件修改时间距今天数；-1 表示无法确定。
func tokenAgeDays(accountID string) int {
	path, err := account.SecretPathFor(accountID)
	if err != nil {
		return -1
	}
	st, err := os.Stat(path)
	if err != nil {
		return -1
	}
	return int(time.Since(st.ModTime()).Hours() / 24)
}

// firstString 按顺序取 map 中第一个非空字符串字段。
func firstString(m map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// refreshAccount 对单账号执行续期。
func refreshAccount(acct *account.Account, force bool) RefreshReport {
	rep := RefreshReport{
		AccountID: acct.ID,
		Account:   acct.Name,
		Region:    string(acct.Region),
	}

	if !account.HasSecret(acct.ID) {
		rep.Status = "no_secret"
		rep.Message = "无 secret，跳过"
		return rep
	}

	raw, err := account.GetSecret(acct.ID)
	if err != nil {
		rep.Status = "failed"
		rep.Message = fmt.Sprintf("读取 secret 失败: %v", err)
		return rep
	}
	dt, rt, pat := parseTokenSecret(raw)

	// PAT 模式下每次请求都实时交换，无需续期
	if pat != "" && dt == "" && rt == "" {
		rep.Status = "not_needed"
		rep.Message = "PAT 模式，无需续期"
		return rep
	}

	age := tokenAgeDays(acct.ID)
	rep.TokenAgeDays = age
	if !force && age >= 0 && age < refreshWindowDays {
		// 已在上面赋值 TokenAgeDays
		rep.Status = "skipped"
		rep.Message = fmt.Sprintf("token 年龄 %d 天，未到续期窗口（≥%d 天触发）", age, refreshWindowDays)
		return rep
	}

	// 优先用 refresh_token（能同时拿到新的 rt），否则退回 device_token
	cred, mode := rt, "refresh_token"
	if cred == "" {
		cred, mode = dt, "device_token"
	}
	if cred == "" {
		rep.Status = "failed"
		rep.Message = "secret 中既无 refresh_token 也无 device_token"
		return rep
	}

	// 用与本账号既有的指纹种子一致的方式派生机器头，避免指纹漂移。
	// service.go 中 buildRegionPool 之前也是用 credential 作 seed。
	//
	// 出口按区域代理：续期发生在「账号 ID 之前」的阶段（还没有账号级代理
	// 可绑），只能按区域绑定——直连会与对话流量形成双出口画像。
	ep := account.GetEndpoints(acct.Region)
	binding := bridge.ResolveRegionProxyBinding(string(acct.Region))
	if binding.ConfigErr != nil {
		rep.Status = "failed"
		rep.Message = fmt.Sprintf("区域代理配置非法，拒绝续期（直连会暴露真实出口）: %v", binding.ConfigErr)
		return rep
	}
	client := bridge.HTTPClientForKey("region|"+string(acct.Region), binding.URL, 15*time.Second)
	seed := cosy.FingerprintSeed("", cred)
	jt, err := cosy.ExchangeJobTokenWithClient(
		client,
		cred,
		cosy.DeriveMachineID(seed),
		cosy.DeriveMachineToken(seed),
		cosy.DeriveMachineType(seed),
		ep.JobTokenURL,
	)
	if err != nil {
		rep.Status = "failed"
		rep.Message = fmt.Sprintf("jobToken 交换失败(via %s): %v", mode, err)
		return rep
	}

	newDT := firstString(jt, "device_token", "deviceToken", "access_token", "accessToken", "token")
	newRT := firstString(jt, "refresh_token", "refreshToken")
	if newDT == "" {
		// 部分版本把 token 放在 data 子对象里
		if data, ok := jt["data"].(map[string]interface{}); ok {
			newDT = firstString(data, "device_token", "deviceToken", "access_token", "accessToken", "token")
			if newRT == "" {
				newRT = firstString(data, "refresh_token", "refreshToken")
			}
		}
	}
	if newDT == "" {
		rep.Status = "failed"
		rep.Message = "jobToken 响应未包含新 token，字段: " + strings.Join(mapKeys(jt), ",")
		return rep
	}
	if newRT == "" {
		newRT = rt // 服务端未回传则保留旧值兜底
	}

	out, _ := json.Marshal(tokenSecret{DeviceToken: newDT, RefreshToken: newRT})
	if err := account.SaveSecret(acct.ID, string(out)); err != nil {
		rep.Status = "failed"
		rep.Message = fmt.Sprintf("保存新 token 失败: %v", err)
		return rep
	}

	now := time.Now()
	rep.Status = "ok"
	rep.Message = fmt.Sprintf("已续期(via %s，原 token 年龄 %d 天)", mode, age)
	rep.RefreshedAt = now.Unix()
	rep.NextRefresh = now.AddDate(0, 0, RefreshIntervalDays).Unix()

	// 名称若有变化顺带更新
	if name := firstString(jt, "name"); name != "" && name != acct.Name {
		acct.Name = name
		_ = account.Save(acct)
	}
	return rep
}

func mapKeys(m map[string]interface{}) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// RefreshAll 对所有账号执行续期检查。force=true 时忽略年龄窗口。
func RefreshAll(force bool) []RefreshReport {
	accounts, err := account.List()
	if err != nil {
		return []RefreshReport{{
			Status:  "failed",
			Message: fmt.Sprintf("读取账号列表失败: %v", err),
		}}
	}
	var reports []RefreshReport
	refreshed := 0
	for i := range accounts {
		acct := &accounts[i]
		rep := refreshAccount(acct, force)
		reports = append(reports, rep)
		if rep.Status == "ok" {
			refreshed++
		}

		<-refreshMu
		refreshLast[acct.ID] = rep
		refreshMu <- struct{}{}

		if rep.Status != "skipped" && rep.Status != "not_needed" && rep.Status != "no_secret" {
			logger.Info("[Refresh] account=%s region=%s status=%s msg=%s",
				rep.Account, rep.Region, rep.Status, rep.Message)
		}
	}
	if refreshed > 0 {
		logger.Info("[Refresh] %d account(s) renewed; restart bridge to apply", refreshed)
	}
	return reports
}

// RefreshLastReports 返回最近一次续期结果快照。
func RefreshLastReports() map[string]RefreshReport {
	<-refreshMu
	defer func() { refreshMu <- struct{}{} }()
	out := make(map[string]RefreshReport, len(refreshLast))
	for k, v := range refreshLast {
		out[k] = v
	}
	return out
}

// StartRefreshScheduler 启动自动续期调度器。
func StartRefreshScheduler() {
	go func() {
		// 启动后延迟一会先跑一次，让控制台立刻有状态
		time.Sleep(45 * time.Second)
		runScheduledRefresh()
		ticker := time.NewTicker(refreshTick)
		defer ticker.Stop()
		for range ticker.C {
			runScheduledRefresh()
		}
	}()
	logger.Info("[Refresh] auto token refresh scheduler started (interval=%dd, window=%dd, tick=%s)",
		RefreshIntervalDays, refreshWindowDays, refreshTick)
}

func runScheduledRefresh() {
	settings, err := account.LoadSettings()
	if err != nil || settings == nil || !settings.AutoRefreshToken {
		return
	}
	reports := RefreshAll(false)
	ok, skipped, failed, other := 0, 0, 0, 0
	for _, r := range reports {
		switch r.Status {
		case "ok":
			ok++
		case "skipped":
			skipped++
		case "failed", "no_secret":
			failed++
		default:
			other++
		}
	}
	logger.Info("[Refresh] scheduled run: %d renewed, %d skipped, %d failed, %d n/a (total %d)",
		ok, skipped, failed, other, len(reports))
}

// ============================================================================
// HTTP handlers
// ============================================================================

// handleRefreshStatus 返回续期状态快照 + 配置。
func handleRefreshStatus(w http.ResponseWriter, r *http.Request) {
	settings, _ := account.LoadSettings()
	enabled := false
	if settings != nil {
		enabled = settings.AutoRefreshToken
	}
	reports := RefreshLastReports()
	// 附带每个账号的 token 年龄，便于控制台展示
	ages := map[string]int{}
	if accounts, err := account.List(); err == nil {
		for i := range accounts {
			ages[accounts[i].ID] = tokenAgeDays(accounts[i].ID)
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled":        enabled,
		"interval_days":  RefreshIntervalDays,
		"window_days":    refreshWindowDays,
		"reports":        reports,
		"token_age_days": ages,
	})
}

// handleRefreshRun 手动触发一次续期。?force=1 忽略年龄窗口。
func handleRefreshRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
		return
	}
	force := r.URL.Query().Get("force") == "1"
	reports := RefreshAll(force)
	renewed, failed := 0, 0
	for _, rep := range reports {
		switch rep.Status {
		case "ok":
			renewed++
		case "failed", "no_secret":
			failed++
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"reports":          reports,
		"renewed":          renewed,
		"failed":           failed,
		"total":            len(reports),
		"restart_required": renewed > 0,
	})
}
