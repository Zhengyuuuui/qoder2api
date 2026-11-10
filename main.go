package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"qoder2api/account"
	"qoder2api/internal/bridge"
	"qoder2api/internal/cosy"
	"qoder2api/logger"
)

//go:embed all:webconsole
var webAssets embed.FS

//go:embed baseprompt.json
var basePromptRaw []byte

var svc *Service

func main() {
	webPort := flag.Int("web-port", 3588, "web console port")
	bridgePort := flag.Int("bridge-port", 8963, "bridge api port (CN 优先，内部按模型 region 自动路由)")
	globalPort := flag.Int("global-port", 8964, "global bridge api port (强制 Global 账号池)；0=不启动")
	bind := flag.String("bind", "0.0.0.0", "console bind address")
	dataDir := flag.String("data-dir", "", "data root (default: $QODER2API_HOME or ~/.qoder2api)")
	instance := flag.String("instance", "", "preset: cn (单控制台 3588 + 双桥 8963/8964) 或 global (旧版独立实例 3589/8964)")
	flag.Parse()

	// 是否显式指定了 --instance（用于后续端口优先级判断）
	isInstanceSet := strings.TrimSpace(*instance) != ""

	// 实例预设：默认 cn = 单控制台 + 双桥（CN 8963 / Global 8964）
	// global 预设保留给「只跑海外」的场景（独立控制台 3589 + 单桥 8964）
	switch strings.ToLower(strings.TrimSpace(*instance)) {
	case "cn", "china", "":
		if !isFlagPassed("web-port") {
			*webPort = 3588
		}
		if !isFlagPassed("bridge-port") {
			*bridgePort = 8963
		}
		if !isFlagPassed("global-port") {
			*globalPort = 8964
		}
		if *dataDir == "" && os.Getenv("QODER2API_HOME") == "" {
			if home, err := os.UserHomeDir(); err == nil {
				*dataDir = filepath.Join(home, ".qoder2api-cn")
			}
		}
	case "global", "intl", "international":
		if !isFlagPassed("web-port") {
			*webPort = 3589
		}
		if !isFlagPassed("bridge-port") {
			*bridgePort = 8964
		}
		if !isFlagPassed("global-port") {
			*globalPort = 0 // 该模式只保留一个桥，8963 不启动
		}
		if *dataDir == "" && os.Getenv("QODER2API_HOME") == "" {
			if home, err := os.UserHomeDir(); err == nil {
				*dataDir = filepath.Join(home, ".qoder2api-global")
			}
		}
	}

	if *dataDir != "" {
		account.SetDataRoot(*dataDir)
	}
	_ = os.MkdirAll(account.DataRoot(), 0700)
	if err := logger.InitFile(account.LogsDir()); err != nil {
		fmt.Fprintf(os.Stderr, "[logger] init failed: %v\n", err)
	}
	logger.Info("data root: %s", account.DataRoot())

	svc = NewService(basePromptRaw)
	svc.LoadRuntimeSettings()

	// 设备指纹本机盐：首次启动自动生成并落盘（settings.json machine_salt）。
	// 使每个部署的指纹派生空间独立，防止 uid 派生模式被上游全局识别；
	// 生成后保持不变——变更 salt 即所有账号指纹整体漂移（等价于换设备）。
	//
	// QD_NO_MACHINE_SALT=1：显式关闭本机盐——不生成、不使用，派生结果
	// 与「无盐」基准逐字节一致。这是存量部署升级后的保旧路径（与
	// QD_DEVICE_ID_LEGACY 正交：那个管 ID 形态，这个管是否混盐）。
	if account.MachineSaltDisabled() {
		cosy.SetInstallSalt("") // 双保险：即使 settings 里已有盐也不混入
		logger.Error("machine salt DISABLED via %s=1: fingerprints use the salt-free baseline "+
			"(identical to pre-upgrade values). Unset it to get per-install fingerprint isolation",
			account.NoMachineSaltEnv)
	} else if salt, err := account.EnsureMachineSalt(); err == nil {
		cosy.SetInstallSalt(salt)
		logger.Info("machine salt loaded (len=%d), fingerprints salted", len(salt))
	} else {
		// 仅提升可观测性、不阻断启动：无盐模式可继续排障，但后果必须明示
		logger.Error("init machine salt: %v；运行于无盐模式（基准形态指纹），下次启动恢复加盐将导致全部账号指纹漂移", err)
	}

	// cookie 按控制台端口隔离，避免 3588/3589 同 host 登录互踢
	consoleWebPort = *webPort

	// 控制台密码：环境变量 > settings；都没有则自动生成并落盘
	_ = ensureConsolePassword()

	// ★ 端口最终裁决（必须在 ensureConsolePassword 之后）：
	//   ensureConsolePassword 首次启动会 LoadSettings()（无文件时返回默认 Port=8963）
	//   并写回 settings.json，若不在此处重新覆盖，--instance=global 预设的 8964 会被冲掉。
	//
	//   优先级：显式 --instance 预设 > 显式端口 flag > settings.json > 内置默认。
	if isInstanceSet {
		// --instance 预设是权威值，直接采用（已含 cn/global 的完整端口组合）
		svc.bridgePort = *bridgePort
		svc.globalPort = *globalPort
	} else {
		// 未指定 --instance：显式 flag 优先，否则沿用 settings.json 加载到的值
		if isFlagPassed("bridge-port") {
			svc.bridgePort = *bridgePort
		}
		if isFlagPassed("global-port") {
			svc.globalPort = *globalPort
		} else if svc.globalPort == 0 && svc.bridgePort != 0 {
			// settings 未提供 global 端口时，按 cn 默认启用 8964 双桥
			svc.globalPort = 8964
		}
	}

	// 每日 10:00 自动签到调度（需在控制台手动开启，默认关闭）
	StartCheckinScheduler()

	// token 自动续期调度（默认关闭；开启后每 15 天用 refresh_token 换新 token）
	StartRefreshScheduler()

	// 启动时尽量自动拉起 Bridge：优先激活账号，否则尝试任意有 secret 的账号
	if err := svc.EnsureBridgeRunning(); err != nil {
		logger.Error("auto start bridge failed: %v", err)
		logger.Info("open console and login/activate an account: http://127.0.0.1:%d", *webPort)
	}

	// 设备指纹审计 + 漂移醒目告警（只打一次）：升级后指纹因人为开关 /
	// 盐变更而整体变化时，必须让运维立刻知道——这对上游等于「账号换了
	// 一台设备」，可能触发重新画像与风控。
	bridge.WarnIdentityShift(svc.IdentityAudit())
	// 保护开关体检：有账号但三个封控开关都显式关闭时，明确提示本轮保护
	// 未生效（*bool 语义下 nil=默认开，只有显式 false 才会到这）。
	warnProtectionDisabled()

	sub, err := fs.Sub(webAssets, "webconsole")
	if err != nil {
		log.Fatalf("web assets: %v", err)
	}

	mux := http.NewServeMux()
	// health (public)：健康检查 + 风控遥测，不做鉴权、无副作用
	mux.HandleFunc("/health", handleHealth)
	// 身份审计 / 风控体检：需要控制台鉴权（isPublicPath 只放行 /health），
	// 这两个端点暴露账号池明细，绝不放行到无鉴权侧。
	mux.HandleFunc("/api/identity/audit", handleIdentityAudit)
	mux.HandleFunc("/api/diagnostics/risk", handleRiskDiagnostics)
	// auth (public)
	mux.HandleFunc("/api/auth/login", handleAuthLogin)
	mux.HandleFunc("/api/auth/logout", handleAuthLogout)
	mux.HandleFunc("/api/auth/me", handleAuthMe)

	mux.HandleFunc("/api/status", handleStatus)
	mux.HandleFunc("/api/accounts", handleAccounts)
	mux.HandleFunc("/api/accounts/add", handleAddAccount)
	mux.HandleFunc("/api/accounts/delete", handleDeleteAccount)
	mux.HandleFunc("/api/accounts/proxy", handleAccountProxy)
	mux.HandleFunc("/api/accounts/quota", handleAccountQuota)
	mux.HandleFunc("/api/oauth/start", handleOAuthStart)
	mux.HandleFunc("/api/oauth/wait", handleOAuthWait)
	mux.HandleFunc("/api/oauth/cancel", handleOAuthCancel)
	mux.HandleFunc("/api/active", handleSetActiveAccount)
	mux.HandleFunc("/api/pool", handlePoolStatus)
	mux.HandleFunc("/api/pool/cool", handlePoolCool)
	mux.HandleFunc("/api/queue/log", handleQueueWaitLog)
	mux.HandleFunc("/api/settings", handleGetSettings)
	mux.HandleFunc("/api/settings/save", handleSaveSettings)
	mux.HandleFunc("/api/models", handleListModels)
	mux.HandleFunc("/api/logs", handleLogs)
	mux.HandleFunc("/api/cleanup", handleCleanup)
	mux.HandleFunc("/api/bridge/start", handleStartBridge)
	mux.HandleFunc("/api/bridge/stop", handleStopBridge)
	mux.HandleFunc("/api/checkin", handleCheckin)
	mux.HandleFunc("/api/refresh", handleRefreshStatus)
	mux.HandleFunc("/api/refresh/run", handleRefreshRun)
	mux.Handle("/", http.FileServer(http.FS(sub)))

	addr := fmt.Sprintf("%s:%d", *bind, *webPort)
	srv := &http.Server{
		Addr:         addr,
		Handler:      withCORS(requireConsoleAuth(mux)),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 10 * time.Minute, // oauth wait may block
		IdleTimeout:  60 * time.Second,
	}

	logger.Info("qoder2api console: http://%s (password protected)", addr)
	logger.Info("bridge target port: %d (API key separate from console password)", svc.bridgePort)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func isFlagPassed(name string) bool {
	found := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func readJSON(r *http.Request, dst interface{}) error {
	defer r.Body.Close()
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(dst)
}

func connectionInfo() map[string]interface{} {
	port := 8963
	token := "qccg"
	if svc != nil {
		if svc.bridgePort > 0 {
			port = svc.bridgePort
		}
		token = svc.EffectiveToken()
	}
	// 控制台展示用：服务器上请改成公网/内网 IP
	host := "127.0.0.1"
	base := fmt.Sprintf("http://%s:%d", host, port)
	return map[string]interface{}{
		"port":            port,
		"api_key":         token,
		"base_url":        base,
		"openai_base_url": base + "/v1",
		"claude_base_url": base,
		"newapi_hint": map[string]string{
			"type":     "openai",
			"base_url": base + "/v1",
			"api_key":  token,
			"model":    "auto",
		},
	}
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, svc.GetStatus())
}

func handleAccounts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, svc.ListAccounts())
}

func handleAddAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
		return
	}
	var req struct {
		PAT    string `json:"pat"`
		Region string `json:"region"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	req.PAT = strings.TrimSpace(req.PAT)
	if req.PAT == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("pat is required"))
		return
	}
	acct, err := svc.AddAccountByPAT(req.PAT, req.Region)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := svc.SetActiveAccount(acct.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, acct)
}

func handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := svc.DeleteAccount(req.ID); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleAccountProxy 修改账号绑定的出口代理（空串 = 直连）。
//
// 约束：只接受 http:// 与 https://；其他 scheme 明确报错而不是静默
// 直连——静默直连等于把真实出口暴露给上游，比失败更糟。
func handleAccountProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
		return
	}
	var req struct {
		ID    string `json:"id"`
		Proxy string `json:"proxy"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	req.ID = strings.TrimSpace(req.ID)
	if req.ID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("id is required"))
		return
	}
	proxy := strings.TrimSpace(req.Proxy)
	if proxy != "" && !bridge.ProxySchemeSupported(proxyScheme(proxy)) {
		writeError(w, http.StatusBadRequest, fmt.Errorf(
			"仅支持 http:// 或 https:// 代理（socks5 需要额外依赖，当前未实现）"))
		return
	}
	acct, err := account.Get(req.ID)
	if err != nil || acct == nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("account not found: %s", req.ID))
		return
	}
	acct.Proxy = proxy
	if err := account.Save(acct); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	logger.Info("account %s: egress proxy updated (%s)", req.ID, proxyLabel(proxy))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// proxyScheme 取出代理 URL 的 scheme（解析失败返回空串）。
func proxyScheme(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return u.Scheme
}

// proxyLabel 生成可读的代理标签（密码脱敏）。
func proxyLabel(raw string) string {
	if raw == "" {
		return "direct"
	}
	return bridge.RedactProxyURL(raw)
}

func handleAccountQuota(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" && r.Method == http.MethodPost {
		var req struct {
			ID string `json:"id"`
		}
		if err := readJSON(r, &req); err == nil {
			id = strings.TrimSpace(req.ID)
		}
	}
	if id == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("id is required"))
		return
	}
	quota, err := svc.GetAccountQuota(id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, quota)
}

func handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
		return
	}
	var req struct {
		Region string `json:"region"`
	}
	_ = readJSON(r, &req)
	session, err := svc.StartOAuthLogin(req.Region)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, session)
}

func handleOAuthWait(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
		return
	}
	var req struct {
		LoginID string `json:"login_id"`
		Active  *bool  `json:"active"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	req.LoginID = strings.TrimSpace(req.LoginID)
	if req.LoginID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("login_id is required"))
		return
	}
	acct, err := account.WaitLogin(req.LoginID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := account.Save(acct); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	activate := true
	if req.Active != nil {
		activate = *req.Active
	}
	if activate {
		if err := svc.SetActiveAccount(acct.ID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, acct)
}

func handleOAuthCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
		return
	}
	var req struct {
		LoginID string `json:"login_id"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	svc.CancelOAuthLogin(strings.TrimSpace(req.LoginID))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func handleSetActiveAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := svc.SetActiveAccount(req.ID); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func handlePoolStatus(w http.ResponseWriter, r *http.Request) {
	slots := svc.PoolStatus()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"slots":   slots,
		"summary": svc.PoolSummary(),
	})
}

func handlePoolCool(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
		return
	}
	var req struct {
		ID      string `json:"id"`
		Seconds int    `json:"seconds"`
		Reason  string `json:"reason"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := svc.CoolAccount(req.ID, req.Seconds, req.Reason); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleQueueWaitLog 返回最近排队等待记录（最新在前），供控制台「最近等待记录」面板。
func handleQueueWaitLog(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{"entries": svc.RecentWaitLog()})
}

// warnProtectionDisabled 启动体检：有账号但三个封控开关被显式关闭时
// 打印醒目告警。
//
// 背景（审计 P1-3）：这三个开关早期是 bool，存量 settings.json 缺字段会
// 读成 false——「宣称有保护、实际默认没开」。已改 *bool（nil=默认开），
// 但用户/旧配置仍可能显式写了 false。此时必须明示：本地限流 / 自适应
// 节流 / 会话亲和都没生效，账号正在「裸奔」。
func warnProtectionDisabled() {
	if svc == nil {
		return
	}
	if len(svc.ListAccounts()) == 0 {
		return // 没有账号，无所谓保护
	}
	st, err := svc.GetSettings()
	if err != nil || st == nil {
		return
	}
	if st.EffectiveTrafficGuard() && st.EffectiveAutoThrottle() && st.EffectiveSessionAffinity() {
		return
	}
	var off []string
	if !st.EffectiveTrafficGuard() {
		off = append(off, "流量闸门(traffic_guard)")
	}
	if !st.EffectiveAutoThrottle() {
		off = append(off, "自适应节流(auto_throttle)")
	}
	if !st.EffectiveSessionAffinity() {
		off = append(off, "会话亲和(session_affinity)")
	}
	logger.Error("封控保护未全开：%s 已被显式关闭。当前账号在本地限流 / 自适应节流 / 会话亲和上"+
		"处于裸奔状态，上游风控压力下建议在控制台「设置」中开启（默认即为开）",
		strings.Join(off, "、"))
}

func handleGetSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := svc.GetSettings()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := *settings
	if out.BridgeToken == "" {
		out.BridgeToken = "qccg"
	}
	if out.Port == 0 {
		out.Port = 8963
	}
	// 不回传明文控制台密码，只标记是否已设置
	hasConsolePwd := strings.TrimSpace(out.ConsolePassword) != "" || strings.TrimSpace(os.Getenv("QODER2API_CONSOLE_PASSWORD")) != ""
	out.ConsolePassword = ""
	// 排队「首字前一直等」回传有效值（未设置时默认开），供控制台复选框正确显示
	effFirstToken := out.EffectiveQueueWaitFirstToken()
	out.QueueWaitFirstToken = &effFirstToken
	// 账号健康策略开关回传有效值（未设置时默认开），供控制台复选框正确显示
	effModelCooldown := out.EffectiveModelCooldown()
	out.ModelCooldown = &effModelCooldown
	effBreakerDegrade := out.EffectiveBreakerDegrade()
	out.BreakerDegrade = &effBreakerDegrade
	// 本轮三个封控开关回传有效值（*bool 语义：nil=默认开）。
	// 语义变更说明：这三个开关早期是 bool，存量 settings.json 缺字段会读成
	// false（保护没开，审计 P1-3）；改 *bool 后 nil=true（保护默认开），
	// 显式 false 才关。控制台必须显示「真实生效值」而不是文件里的 nil。
	effTrafficGuard := out.EffectiveTrafficGuard()
	out.TrafficGuard = &effTrafficGuard
	effAutoThrottle := out.EffectiveAutoThrottle()
	out.AutoThrottle = &effAutoThrottle
	effSessionAffinity := out.EffectiveSessionAffinity()
	out.SessionAffinity = &effSessionAffinity
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"settings":             out,
		"connection":           connectionInfo(),
		"console_password_set": hasConsolePwd,
	})
}

func handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
		return
	}
	var req struct {
		Port             *int     `json:"port"`
		BridgeToken      *string  `json:"bridge_token"`
		LogLevel         *string  `json:"log_level"`
		AutoStart        *bool    `json:"auto_start"`
		AutoCheckin      *bool    `json:"auto_checkin"`
		AutoCheckinTimes []string `json:"auto_checkin_times"`
		AutoRefreshToken *bool    `json:"auto_refresh_token"`
		// 档位字段用 map/string 单独处理（nil 与空串语义不同：
		// nil=不修改，""=清空）
		ContextTierDefault   *string           `json:"context_tier_default"`
		ContextTierOverrides map[string]string `json:"context_tier_overrides"`
		ConsolePassword      *string           `json:"console_password"`
		// 模型排队重试参数（控制台可配，热更新生效）
		QueueMaxRetries     *int  `json:"queue_max_retries"`
		QueueWaitCapSec     *int  `json:"queue_wait_cap_sec"`
		QueueWaitDefaultSec *int  `json:"queue_wait_default_sec"`
		QueueWaitFirstToken *bool `json:"queue_wait_first_token"`
		QueueWaitBudgetSec  *int  `json:"queue_wait_budget_sec"`
		// 账号健康策略开关（热更新生效）
		ModelCooldown  *bool `json:"model_cooldown"`
		BreakerDegrade *bool `json:"breaker_degrade"`
		// 签到防风控参数（热更新生效）
		CheckinAccountGapMS     *int  `json:"checkin_account_gap_ms"`
		CheckinJitterMS         *int  `json:"checkin_jitter_ms"`
		CheckinStaggerSec       *int  `json:"checkin_stagger_sec"`
		CheckinStagger          *bool `json:"checkin_stagger"`
		CheckinFullDeviceHeader *bool `json:"checkin_full_device_headers"`
		// 流量闸门（单账号并发 + 节奏整形，热更新生效）
		ConcurrencyPerAccount *int  `json:"concurrency_per_account"`
		MinRequestGapMs       *int  `json:"min_request_gap_ms"`
		GuardWaitSec          *int  `json:"guard_wait_sec"`
		TrafficGuard          *bool `json:"traffic_guard"`
		// 自适应节流（按排队率/频控率自动收紧全局节奏，热更新生效）
		AutoThrottle         *bool    `json:"auto_throttle"`
		ThrottleWindowSec    *int     `json:"throttle_window_sec"`
		ThrottleQueueRatePct *float64 `json:"throttle_queue_rate_pct"`
		ThrottleRatePct      *float64 `json:"throttle_rate_pct"`
		ThrottleHoldMs       *int     `json:"throttle_hold_ms"`
		// 请求体积与输出上限（本地前置拦截，热更新生效）
		MaxBodyMB            *int `json:"max_body_mb"`
		ToolResultBytes      *int `json:"tool_result_bytes"`
		ToolResultTotalBytes *int `json:"tool_result_total_bytes"`
		MaxOutputTokens      *int `json:"max_output_tokens"`
		// 会话亲和（同会话粘同账号，热更新生效）
		SessionAffinity       *bool `json:"session_affinity"`
		SessionAffinityTTLSec *int  `json:"session_affinity_ttl_sec"`
		// 新格式：包含旧密码和新密码
		ConsolePasswordNew *struct {
			OldPassword string  `json:"old_password"`
			NewPassword *string `json:"new_password"`
		} `json:"console_password_new"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	cur, err := svc.GetSettings()
	if err != nil || cur == nil {
		// 三个保护开关留 nil：Effective*() 会给 true（保护默认开）
		cur = &account.Settings{Port: 8963, LogLevel: "info"}
	}
	if req.Port != nil {
		if *req.Port < 1 || *req.Port > 65535 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid port"))
			return
		}
		cur.Port = *req.Port
	}
	if req.BridgeToken != nil {
		tok := strings.TrimSpace(*req.BridgeToken)
		if tok == "" || tok == "qccg" {
			cur.BridgeToken = ""
		} else {
			cur.BridgeToken = tok
		}
	}
	if req.LogLevel != nil && *req.LogLevel != "" {
		cur.LogLevel = *req.LogLevel
	}
	if req.AutoStart != nil {
		cur.AutoStart = *req.AutoStart
	}
	if req.AutoCheckin != nil {
		cur.AutoCheckin = *req.AutoCheckin
		logger.Info("auto checkin set to %v", cur.AutoCheckin)
	}
	if req.AutoRefreshToken != nil {
		cur.AutoRefreshToken = *req.AutoRefreshToken
		logger.Info("auto refresh token set to %v", cur.AutoRefreshToken)
	}
	if req.ContextTierDefault != nil {
		cur.ContextTierDefault = strings.TrimSpace(*req.ContextTierDefault)
		logger.Info("context tier default set to %q", cur.ContextTierDefault)
	}
	if req.ContextTierOverrides != nil {
		over := map[string]string{}
		for k, v := range req.ContextTierOverrides {
			k = strings.TrimSpace(k)
			v = strings.TrimSpace(v)
			if k == "" || v == "" {
				continue
			}
			over[k] = v
		}
		cur.ContextTierOverrides = over
		logger.Info("context tier overrides set: %v", over)
	}
	if req.QueueMaxRetries != nil {
		v := *req.QueueMaxRetries
		if v < 1 || v > 10 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("queue_max_retries 需在 1~10 之间"))
			return
		}
		cur.QueueMaxRetries = v
	}
	if req.QueueWaitCapSec != nil {
		v := *req.QueueWaitCapSec
		if v < 1 || v > 300 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("queue_wait_cap_sec 需在 1~300 之间"))
			return
		}
		cur.QueueWaitCapSec = v
	}
	if req.QueueWaitDefaultSec != nil {
		v := *req.QueueWaitDefaultSec
		if v < 1 || v > 300 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("queue_wait_default_sec 需在 1~300 之间"))
			return
		}
		cur.QueueWaitDefaultSec = v
		logger.Info("queue retry set: maxRetries=%d waitCap=%ds waitDefault=%ds",
			cur.QueueMaxRetries, cur.QueueWaitCapSec, cur.QueueWaitDefaultSec)
	}
	if req.QueueWaitFirstToken != nil {
		v := *req.QueueWaitFirstToken
		cur.QueueWaitFirstToken = &v
		logger.Info("queue wait_first_token set to %v", v)
	}
	if req.QueueWaitBudgetSec != nil {
		v := *req.QueueWaitBudgetSec
		if v < 1 || v > 86400 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("queue_wait_budget_sec 需在 1~86400 之间"))
			return
		}
		cur.QueueWaitBudgetSec = v
		logger.Info("queue wait budget set to %ds", v)
	}
	if req.ModelCooldown != nil {
		v := *req.ModelCooldown
		cur.ModelCooldown = &v
		logger.Info("model cooldown set to %v", v)
	}
	if req.BreakerDegrade != nil {
		v := *req.BreakerDegrade
		cur.BreakerDegrade = &v
		logger.Info("breaker/degrade set to %v", v)
	}
	if req.CheckinAccountGapMS != nil {
		v := *req.CheckinAccountGapMS
		if v < 0 || v > 600000 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("checkin_account_gap_ms 需在 0~600000 之间"))
			return
		}
		cur.CheckinAccountGapMS = v
	}
	if req.CheckinJitterMS != nil {
		v := *req.CheckinJitterMS
		if v < 0 || v > 600000 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("checkin_jitter_ms 需在 0~600000 之间"))
			return
		}
		cur.CheckinJitterMS = v
	}
	if req.CheckinStaggerSec != nil {
		v := *req.CheckinStaggerSec
		if v < 0 || v > 3600 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("checkin_stagger_sec 需在 0~3600 之间"))
			return
		}
		cur.CheckinStaggerSec = v
	}
	if req.CheckinStagger != nil {
		v := *req.CheckinStagger
		cur.CheckinStagger = &v
		logger.Info("checkin stagger set to %v", v)
	}
	if req.CheckinFullDeviceHeader != nil {
		v := *req.CheckinFullDeviceHeader
		cur.CheckinFullDeviceHeaders = v
		logger.Info("checkin full device headers set to %v", v)
	}
	logger.Info("checkin anti-risk set: gap=%dms jitter=%dms stagger=%ds",
		cur.CheckinAccountGapMS, cur.CheckinJitterMS, cur.CheckinStaggerSec)
	if req.ConcurrencyPerAccount != nil {
		v := *req.ConcurrencyPerAccount
		if v < 1 || v > 16 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("concurrency_per_account 需在 1~16 之间"))
			return
		}
		cur.ConcurrencyPerAccount = v
		logger.Info("traffic guard: concurrency_per_account set to %d", v)
	}
	if req.MinRequestGapMs != nil {
		v := *req.MinRequestGapMs
		if v < 0 || v > 60000 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("min_request_gap_ms 需在 0~60000 之间"))
			return
		}
		cur.MinRequestGapMs = v
		logger.Info("traffic guard: min_request_gap_ms set to %d", v)
	}
	if req.GuardWaitSec != nil {
		v := *req.GuardWaitSec
		if v < 1 || v > 600 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("guard_wait_sec 需在 1~600 之间"))
			return
		}
		cur.GuardWaitSec = v
		logger.Info("traffic guard: guard_wait_sec set to %d", v)
	}
	if req.TrafficGuard != nil {
		v := *req.TrafficGuard
		cur.TrafficGuard = &v
		logger.Info("traffic guard set to %v (explicit)", v)
	}
	// 自适应节流
	if req.AutoThrottle != nil {
		v := *req.AutoThrottle
		cur.AutoThrottle = &v
		logger.Info("auto throttle set to %v (explicit)", v)
	}
	if req.ThrottleWindowSec != nil {
		v := *req.ThrottleWindowSec
		if v < 1 || v > 3600 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("throttle_window_sec 需在 1~3600 之间"))
			return
		}
		cur.ThrottleWindowSec = v
	}
	if req.ThrottleQueueRatePct != nil {
		v := *req.ThrottleQueueRatePct
		if v < 0 || v > 100 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("throttle_queue_rate_pct 需在 0~100 之间"))
			return
		}
		cur.ThrottleQueueRatePct = v
	}
	if req.ThrottleRatePct != nil {
		v := *req.ThrottleRatePct
		if v < 0 || v > 100 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("throttle_rate_pct 需在 0~100 之间"))
			return
		}
		cur.ThrottleRatePct = v
	}
	if req.ThrottleHoldMs != nil {
		v := *req.ThrottleHoldMs
		if v < 0 || v > 60000 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("throttle_hold_ms 需在 0~60000 之间"))
			return
		}
		cur.ThrottleHoldMs = v
	}
	logger.Info("auto throttle set: window=%ds queuePct=%.1f ratePct=%.1f hold=%dms enabled=%v",
		cur.ThrottleWindowSec, cur.ThrottleQueueRatePct, cur.ThrottleRatePct, cur.ThrottleHoldMs, cur.AutoThrottle)
	// 请求体积与输出上限
	if req.MaxBodyMB != nil {
		v := *req.MaxBodyMB
		if v < 1 || v > 1024 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("max_body_mb 需在 1~1024 之间"))
			return
		}
		cur.MaxBodyMB = v
	}
	if req.ToolResultBytes != nil {
		v := *req.ToolResultBytes
		if v < 0 || v > 10*1024*1024 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("tool_result_bytes 需在 0~10485760 之间"))
			return
		}
		cur.ToolResultBytes = v
	}
	if req.ToolResultTotalBytes != nil {
		v := *req.ToolResultTotalBytes
		if v < 0 || v > 50*1024*1024 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("tool_result_total_bytes 需在 0~52428800 之间"))
			return
		}
		cur.ToolResultTotalBytes = v
	}
	if req.MaxOutputTokens != nil {
		v := *req.MaxOutputTokens
		if v < 1 || v > 1000000 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("max_output_tokens 需在 1~1000000 之间"))
			return
		}
		cur.MaxOutputTokens = v
		logger.Info("max_output_tokens cap set to %d", v)
	}
	// 会话亲和
	if req.SessionAffinity != nil {
		v := *req.SessionAffinity
		cur.SessionAffinity = &v
		logger.Info("session affinity set to %v (explicit)", v)
	}
	if req.SessionAffinityTTLSec != nil {
		v := *req.SessionAffinityTTLSec
		if v < 1 || v > 86400 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("session_affinity_ttl_sec 需在 1~86400 之间"))
			return
		}
		cur.SessionAffinityTTLSec = v
	}
	if req.AutoCheckinTimes != nil {
		var times []string
		for _, t := range req.AutoCheckinTimes {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			if _, _, ok := parseHHMM(t); !ok {
				writeError(w, http.StatusBadRequest, fmt.Errorf("invalid auto_checkin_times entry: %q (want HH:MM)", t))
				return
			}
			times = append(times, t)
		}
		cur.AutoCheckinTimes = times
		logger.Info("auto checkin times set to %v", times)
	}

	// 处理新格式的密码修改
	if req.ConsolePasswordNew != nil {
		oldPwd := strings.TrimSpace(req.ConsolePasswordNew.OldPassword)
		newPwd := ""
		if req.ConsolePasswordNew.NewPassword != nil {
			newPwd = strings.TrimSpace(*req.ConsolePasswordNew.NewPassword)
		}

		// 验证旧密码
		currentPassword := effectiveConsolePassword()
		if currentPassword == "" {
			writeError(w, http.StatusUnauthorized, fmt.Errorf("当前未设置控制台密码"))
			return
		}

		a := sha256.Sum256([]byte(oldPwd))
		b := sha256.Sum256([]byte(currentPassword))
		if subtle.ConstantTimeCompare(a[:], b[:]) != 1 {
			writeError(w, http.StatusUnauthorized, fmt.Errorf("旧密码错误"))
			return
		}

		// 如果提供了新密码，则更新
		if newPwd != "" {
			cur.ConsolePassword = newPwd
		}
	} else if req.ConsolePassword != nil {
		// 旧格式：直接设置密码
		p := strings.TrimSpace(*req.ConsolePassword)
		if p != "" {
			cur.ConsolePassword = p
		}
	}

	if err := svc.SaveSettings(cur); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// 排队重试参数热更新（无需重启）
	svc.ApplyQueueSettings(cur.QueueMaxRetries, cur.QueueWaitCapSec, cur.QueueWaitDefaultSec, cur.EffectiveQueueWaitFirstToken(), cur.QueueWaitBudgetSec)
	// 模型级冷却 / 熔断降权开关热更新（无需重启）
	bridge.ApplyHealthSettings(cur.EffectiveModelCooldown(), cur.EffectiveBreakerDegrade())
	// 签到防风控参数热更新（无需重启）
	svc.ApplyCheckinSettings(cur)
	safe := *cur
	safe.ConsolePassword = ""
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":         true,
		"connection": connectionInfo(),
		"settings":   safe,
		"note":       "已保存；控制台密码立即生效，Bridge 端口变更需重启 Bridge",
	})
}

func handleListModels(w http.ResponseWriter, r *http.Request) {
	models, err := svc.ListQoderModels()
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, models)
}

func handleLogs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, svc.GetLogs(100))
}

func handleCleanup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
		return
	}
	if err := svc.CleanupAllData(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func handleStartBridge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
		return
	}
	if err := svc.StartBridge(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func handleStopBridge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("POST required"))
		return
	}
	if err := svc.StopBridge(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
