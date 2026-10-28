package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"qoder2api/account"
	"qoder2api/internal/bridge"
	"qoder2api/internal/cosy"
	"qoder2api/logger"
)

type QoderModel struct {
	Key             string  `json:"id"`
	DisplayName     string  `json:"display_name"`
	Enable          bool    `json:"enable"`
	IsDefault       bool    `json:"is_default"`
	IsReasoning     bool    `json:"is_reasoning,omitempty"`
	ContextWindow   int     `json:"context_window,omitempty"`
	MaxOutputTokens int     `json:"max_output_tokens,omitempty"`
	MaxInputTokens  int     `json:"max_input_tokens,omitempty"`
	PriceFactor     float64 `json:"price_factor,omitempty"`
	// 思考能力（来自上游 thinking_config，与 is_reasoning 相互独立）
	ThinkingSupport bool     `json:"thinking_support,omitempty"`
	ThinkingDefault bool     `json:"thinking_default,omitempty"`
	ThinkingEfforts []string `json:"thinking_efforts,omitempty"`
	// ContextTiers 上游 context_config 的全部档位（如 200K/400K/1M）。
	ContextTiers []bridge.ContextTier `json:"context_tiers,omitempty"`
}

type Service struct {
	bridge      *bridge.Bridge
	pool        *bridge.Pool
	bridgeSrv   *http.Server
	globalSrv   *http.Server // 8964：强制 Global 账号池
	bridgeMu    sync.Mutex
	bridgePort  int
	globalPort  int
	bridgeToken string
	basePrompt  []byte
}

func NewService(basePrompt []byte) *Service {
	return &Service{bridgePort: 8963, basePrompt: basePrompt}
}

func (s *Service) LoadRuntimeSettings() {
	settings, err := account.LoadSettings()
	if err != nil {
		logger.Error("load settings: %v", err)
		return
	}
	logger.SetLevel(settings.LogLevel)
	if settings.Port > 0 {
		s.bridgePort = settings.Port
	}
	s.bridgeToken = settings.BridgeToken
	// 排队重试参数（控制台可配）：启动时应用，保存设置时再由 handleSaveSettings 热更新
	bridge.ApplyQueueSettings(settings.QueueMaxRetries, settings.QueueWaitCapSec, settings.QueueWaitDefaultSec, settings.EffectiveQueueWaitFirstToken())
}

// ApplyQueueSettings 热更新模型排队重试参数（控制台保存设置后调用，即时生效）。
func (s *Service) ApplyQueueSettings(maxRetries, waitCapSec, waitDefaultSec int, waitFirstToken bool) {
	bridge.ApplyQueueSettings(maxRetries, waitCapSec, waitDefaultSec, waitFirstToken)
}

// RecentWaitLog 返回最近排队等待记录（最新在前），供控制台「最近等待记录」面板使用。
func (s *Service) RecentWaitLog() []bridge.WaitEpisode {
	return bridge.RecentWaitLog()
}

func (s *Service) EffectiveToken() string {
	if s.bridgeToken != "" {
		return s.bridgeToken
	}
	return "qccg"
}

func (s *Service) ListAccounts() []account.Account {
	accounts, _ := account.List()
	if accounts == nil {
		return []account.Account{}
	}
	return accounts
}

func (s *Service) AddAccountByPAT(pat, region string) (*account.Account, error) {
	r := account.NormalizeRegion(region)
	ep := account.GetEndpoints(r)
	// 交换时尚无 uid：机器头按凭证稳定派生，避免每次导入生成新机器码
	seed := cosy.FingerprintSeed("", pat)
	jt, err := cosy.ExchangeJobToken(pat, cosy.DeriveMachineID(seed), cosy.DeriveMachineToken(seed), cosy.DeriveMachineType(seed), ep.JobTokenURL)
	if err != nil {
		return nil, fmt.Errorf("验证 PAT 失败: %w", err)
	}
	id := account.SanitizeID(bridge.StrVal(jt, "id") + bridge.StrVal(jt, "name"))
	now := time.Now()
	acct := &account.Account{
		ID:        id,
		Name:      bridge.StrVal(jt, "name"),
		Email:     bridge.StrVal(jt, "email"),
		UserType:  bridge.StrValDefault(jt, "userType", "personal_standard"),
		Region:    r,
		AuthMode:  "pat",
		APIMode:   "openai",
		Tags:      []string{},
		CreatedAt: now,
	}
	if err := account.SaveSecret(acct.ID, pat); err != nil {
		return nil, err
	}
	if err := account.Save(acct); err != nil {
		return nil, err
	}
	return acct, nil
}

func (s *Service) DeleteAccount(id string) error {
	_ = account.DeleteSecret(id)
	return account.Delete(id)
}

func (s *Service) SetActiveAccount(id string) error {
	if err := account.SetActive(id); err != nil {
		return err
	}
	acct, err := account.GetActive()
	if err != nil || acct == nil {
		return err
	}
	return s.restartBridge(acct)
}

func (s *Service) GetStatus() account.Status {
	s.bridgeMu.Lock()
	defer s.bridgeMu.Unlock()
	running := s.bridgeSrv != nil
	activeID := ""
	apiMode := ""
	if acct, _ := account.GetActive(); acct != nil {
		activeID = acct.ID
		apiMode = acct.APIMode
	}
	// 两区各自的启用账号，供控制台分区展示
	cnActive, _ := account.GetActiveByRegion(account.RegionCN)
	globalActive, _ := account.GetActiveByRegion(account.RegionGlobal)
	st := account.Status{Running: running, Port: s.bridgePort, ActiveAccount: activeID, APIMode: apiMode}
	if cnActive != nil {
		st.CNActive = cnActive.ID
		st.CNActiveName = cnActive.Name
	}
	if globalActive != nil {
		st.GlobalActive = globalActive.ID
		st.GlobalActiveName = globalActive.Name
	}
	st.GlobalRunning = s.globalSrv != nil
	return st
}

func (s *Service) StartBridge() error {
	acct, err := account.GetActive()
	if err != nil || acct == nil {
		return fmt.Errorf("no active account")
	}
	return s.startBridgeWithAccount(acct)
}

// EnsureBridgeRunning 启动时调用：为 cn / global 各区域确保存在启用账号，
// 然后启动桥。某区域没有启用账号时，取该区域第一个有 secret 的账号并启用。
func (s *Service) EnsureBridgeRunning() error {
	s.bridgeMu.Lock()
	running := s.bridgeSrv != nil
	s.bridgeMu.Unlock()
	if running {
		return nil
	}

	accounts, err := account.List()
	if err != nil {
		return err
	}

	// 按区域确保各有一个「启用且有凭证」的账号
	for _, region := range []account.Region{account.RegionCN, account.RegionGlobal} {
		if acct, _ := account.GetActiveByRegion(region); acct != nil {
			if account.HasSecret(acct.ID) {
				continue
			}
			logger.Error("region=%s active account %s has no secret file", region, acct.ID)
		}
		// 该区域没有可用启用账号：取第一个有 secret 的并启用
		for _, a := range accounts {
			if account.NormalizeRegion(string(a.Region)) != account.NormalizeRegion(string(region)) {
				continue
			}
			if !account.HasSecret(a.ID) {
				continue
			}
			logger.Info("auto-activate %s account with secret: %s", region, a.Name)
			if err := account.SetActive(a.ID); err != nil {
				logger.Error("auto-activate failed: %v", err)
				continue
			}
			break
		}
	}

	// 主桥用 CN 启用账号；没有 CN 账号时退回 global（兼容纯海外部署）
	acct, _ := account.GetActiveByRegion(account.RegionCN)
	if acct == nil {
		acct, _ = account.GetActiveByRegion(account.RegionGlobal)
	}
	if acct == nil {
		return fmt.Errorf("no account with usable secret; please OAuth/PAT login in web console")
	}
	return s.startBridgeWithAccount(acct)
}

func (s *Service) StopBridge() error {
	s.bridgeMu.Lock()
	defer s.bridgeMu.Unlock()
	var firstErr error
	if s.globalSrv != nil {
		if err := s.globalSrv.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		s.globalSrv = nil
	}
	if s.bridgeSrv == nil {
		return firstErr
	}
	err := s.bridgeSrv.Close()
	s.bridgeSrv = nil
	s.bridge = nil
	s.pool = nil
	if err != nil {
		return err
	}
	return firstErr
}

func (s *Service) restartBridge(acct *account.Account) error {
	_ = s.StopBridge()
	return s.startBridgeWithAccount(acct)
}

// authMiddleware Bridge 端点（/v1/*）鉴权中间件：
// 接受 Authorization: Bearer <token> 或 x-api-key: <token>；
// token 每次请求实时取 svc.EffectiveToken()，容忍运行中改配置；
// 比较用 crypto/subtle.ConstantTimeCompare 防时序侧信道；
// OPTIONS 预检直接放行（204），避免浏览器客户端 CORS 失败；
// 校验失败返回 401 + OpenAI 风格错误体。
func (s *Service) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.Info("[HTTP] %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		token := ""
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			token = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		} else if k := r.Header.Get("x-api-key"); k != "" {
			token = k
		}
		expect := s.EffectiveToken()
		if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(expect)) != 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":{"message":"invalid api key","type":"authentication_error"}}`)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Service) startBridgeWithAccount(acct *account.Account) error {
	tmpl := string(s.basePrompt)
	for _, ukey := range []string{"{UUID1}", "{UUID2}", "{UUID3}", "{UUID4}", "{UUID5}"} {
		tmpl = strings.ReplaceAll(tmpl, ukey, cosy.NewUUID())
	}
	tmpl = strings.ReplaceAll(tmpl, "{TIME1}", fmt.Sprintf("%d", cosy.UnixMs()))
	var templateBase map[string]interface{}
	_ = json.Unmarshal([]byte(tmpl), &templateBase)

	// 主桥账号池：只纳入与本桥区域一致的账号。
	// 必须按 region 隔离：若把 global 账号也放进 8963 的池，模型目录命中
	// （如 qmodel_38max / qfmodel 两区都有）会把请求派给 global 账号并扣其 credits，
	// 而用户可能根本没启动 global 侧。请求时轮询选号，限额/限流/瞬时故障时
	// 冷却该账号并自动换下一个（见 internal/bridge/pool.go）。
	pool := s.buildRegionPool(acct.Region)
	if pool.Size() == 0 {
		return fmt.Errorf("no %s account with usable secret; please OAuth/PAT login in web console", acct.Region)
	}
	logger.Info("startBridgeWithAccount: region=%s pool size=%d (requested active=%s)", acct.Region, pool.Size(), acct.Name)

	b := bridge.NewPoolBridgeFor(acct.Region, pool, templateBase)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", b.HandleChatCompletions)
	mux.HandleFunc("/v1/messages", b.HandleClaudeMessages)
	mux.HandleFunc("/v1/models", b.HandleListModels)
	mux.HandleFunc("/v1/responses", b.HandleCodexResponses)

	// mux 外包鉴权中间件：同网络客户端必须携带有效 token 才能消耗账号配额
	handler := s.authMiddleware(mux)

	s.bridgeMu.Lock()
	s.bridge = b
	s.pool = pool
	s.bridgeSrv = &http.Server{
		Addr:    fmt.Sprintf("0.0.0.0:%d", s.bridgePort),
		Handler: handler,
	}
	s.bridgeMu.Unlock()

	logger.Info("Bridge started on port %d", s.bridgePort)
	go func() {
		if err := s.bridgeSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("bridge serve error: %v", err)
		}
	}()

	// 8964：Global 专用桥，强制只走 Global 账号（跨区模型不能误打到国内号）
	if s.globalPort > 0 && s.globalPort != s.bridgePort {
		if err := s.startGlobalBridge(templateBase); err != nil {
			// Global 池为空不应影响主桥，仅告警
			logger.Info("global bridge (port %d) not started: %v", s.globalPort, err)
		}
	}
	return nil
}

// startGlobalBridge 启动 8964 端口的 Global 专用桥。
// 只纳入 region=global 的账号，避免国内账号被误用于国际模型请求。
func (s *Service) startGlobalBridge(templateBase map[string]interface{}) error {
	pool := s.buildRegionPool(account.RegionGlobal)
	if pool.Size() == 0 {
		return fmt.Errorf("no global account with usable secret")
	}
	b := bridge.NewPoolBridgeFor(account.RegionGlobal, pool, templateBase)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", b.HandleChatCompletions)
	mux.HandleFunc("/v1/messages", b.HandleClaudeMessages)
	mux.HandleFunc("/v1/models", b.HandleListModels)
	mux.HandleFunc("/v1/responses", b.HandleCodexResponses)

	srv := &http.Server{
		Addr:    fmt.Sprintf("0.0.0.0:%d", s.globalPort),
		Handler: s.authMiddleware(mux),
	}

	s.bridgeMu.Lock()
	s.globalSrv = srv
	s.bridgeMu.Unlock()

	logger.Info("Global bridge started on port %d (pool=%d global accounts)", s.globalPort, pool.Size())
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("global bridge serve error: %v", err)
		}
	}()
	return nil
}

// buildRegionPool 只把指定区域的账号建成池（用于 8964 Global 专用桥）。
// 状态文件按区域隔离，避免与主桥互相覆盖冷却记录。
func (s *Service) buildRegionPool(region account.Region) *bridge.Pool {
	pool := bridge.NewPoolWithState(filepath.Join(account.DataRoot(), "pool_state_"+string(region)+".json"))
	accounts, err := account.List()
	if err != nil {
		logger.Error("buildRegionPool(%s): list accounts: %v", region, err)
		return pool
	}
	for _, a := range accounts {
		if a.Region != region {
			continue
		}
		if !account.HasSecret(a.ID) {
			continue
		}
		pat, err := account.GetSecret(a.ID)
		if err != nil {
			logger.Error("buildRegionPool(%s): get secret for %s: %v", region, a.Name, err)
			continue
		}
		slot, err := bridge.NewSlotFromSecretWithFP(a.ID, a.Name, a.Region, pat, a.CheckinFingerprint)
		if err != nil {
			logger.Error("buildRegionPool(%s): account %s unusable, skipped: %v", region, a.Name, err)
			continue
		}
		pool.Add(slot)
		if a.CheckinFingerprint != nil && a.CheckinFingerprint.MachineToken != "" {
			logger.Info("buildRegionPool(%s): added account %s (modern protocol)", region, a.Name)
		} else {
			logger.Info("buildRegionPool(%s): added account %s", region, a.Name)
		}
	}
	pool.LoadState()
	return pool
}

// PoolStatus 返回账号池运行时状态（桥未启动时为空）。
func (s *Service) PoolStatus() []bridge.SlotStatus {
	s.bridgeMu.Lock()
	pool := s.pool
	s.bridgeMu.Unlock()
	if pool == nil {
		return nil
	}
	return pool.Status()
}

// CoolAccount 手动冷却指定账号（运维/调试用，便于观察选号与换号）。
func (s *Service) CoolAccount(id string, seconds int, reason string) error {
	s.bridgeMu.Lock()
	pool := s.pool
	s.bridgeMu.Unlock()
	if pool == nil {
		return fmt.Errorf("bridge not running")
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("id required")
	}
	if seconds <= 0 {
		seconds = 60
	}
	if strings.TrimSpace(reason) == "" {
		reason = "manual cooldown"
	}
	pool.Cool(id, time.Duration(seconds)*time.Second, reason)
	return nil
}

func (s *Service) GetSettings() (*account.Settings, error) {
	return account.LoadSettings()
}

func (s *Service) SaveSettings(st *account.Settings) error {
	logger.SetLevel(st.LogLevel)
	if st.Port > 0 {
		s.bridgePort = st.Port
	}
	s.bridgeToken = st.BridgeToken
	if st.QuotaRefreshInterval > 0 && st.QuotaRefreshInterval < 10 {
		st.QuotaRefreshInterval = 10
	}
	return account.SaveSettings(st)
}

func (s *Service) GetLogs(limit int) []logger.Entry {
	return logger.GetLogs(limit)
}

func (s *Service) GetAccountQuota(accountID string) (*account.QuotaInfo, error) {
	acct, err := account.Get(accountID)
	if err != nil {
		return nil, err
	}
	token, err := account.GetSecret(acct.ID)
	if err != nil {
		return nil, fmt.Errorf("get token: %w", err)
	}
	deviceToken, _ := bridge.ParseOAuthSecret(token)
	if strings.HasPrefix(deviceToken, "dt-") {
		token = deviceToken
	} else {
		ep := account.GetEndpoints(acct.Region)
		seed := cosy.FingerprintSeed("", token)
		jt, err := cosy.ExchangeJobToken(token, cosy.DeriveMachineID(seed), cosy.DeriveMachineToken(seed), cosy.DeriveMachineType(seed), ep.JobTokenURL)
		if err != nil {
			return nil, fmt.Errorf("exchange token: %w", err)
		}
		oauthToken := bridge.StrVal(jt, "securityOauthToken")
		if oauthToken == "" {
			return nil, fmt.Errorf("no securityOauthToken in response")
		}
		token = oauthToken
	}
	quota, err := account.FetchQuota(token, acct.Region)
	if err != nil {
		return nil, err
	}
	fmt.Printf("[SERVICE DEBUG] Quota result: user_quota=%+v, addon_quota=%+v\n", quota.UserQuota, quota.AddonQuota)
	return quota, nil
}

func (s *Service) ListQoderModels() ([]QoderModel, error) {
	s.bridgeMu.Lock()
	b := s.bridge
	s.bridgeMu.Unlock()
	if b == nil {
		return nil, fmt.Errorf("BRIDGE_NOT_RUNNING: bridge 未启动，请先激活账号并启动 bridge")
	}
	models, err := b.ListAvailableModels()
	if err != nil {
		return nil, err
	}
	out := make([]QoderModel, len(models))
	for i, m := range models {
		out[i] = QoderModel{
			Key:             m.Key,
			DisplayName:     m.DisplayName,
			Enable:          m.Enable,
			IsDefault:       m.IsDefault,
			IsReasoning:     m.IsReasoning,
			MaxInputTokens:  m.MaxInputTokens,
			PriceFactor:     m.PriceFactor,
			ContextWindow:   m.ContextWindow,
			MaxOutputTokens: m.MaxOutputTokens,
			ThinkingSupport: m.ThinkingSupport,
			ThinkingDefault: m.ThinkingDefault,
			ThinkingEfforts: m.ThinkingEfforts,
			ContextTiers:    m.ContextTiers,
		}
	}
	return out, nil
}

func (s *Service) StartOAuthLogin(region string) (*account.OAuthSession, error) {
	return account.StartLogin(account.NormalizeRegion(region))
}

func (s *Service) CancelOAuthLogin(loginID string) {
	account.CancelLogin(loginID)
}

func (s *Service) CleanupAllData() error {
	_ = s.StopBridge()
	accounts, err := account.List()
	if err == nil {
		for _, acct := range accounts {
			_ = account.DeleteSecret(acct.ID)
			_ = account.Delete(acct.ID)
		}
	}
	return nil
}
