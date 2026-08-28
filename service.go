package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"qoder2api/account"
	"qoder2api/internal/bridge"
	"qoder2api/internal/cosy"
	"qoder2api/logger"
)

type QoderModel struct {
	Key            string  `json:"id"`
	DisplayName    string  `json:"display_name"`
	Enable         bool    `json:"enable"`
	IsDefault      bool    `json:"is_default"`
	IsReasoning    bool    `json:"is_reasoning,omitempty"`
	ContextWindow  int     `json:"context_window,omitempty"`
	MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
	MaxInputTokens int     `json:"max_input_tokens,omitempty"`
	PriceFactor    float64 `json:"price_factor,omitempty"`
}

type Service struct {
	bridge      *bridge.Bridge
	bridgeSrv   *http.Server
	bridgeMu    sync.Mutex
	bridgePort  int
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
	mid := cosy.NewUUID()
	mtoken := cosy.NewBase64Token()
	mtype := cosy.NewHexToken(18)

	jt, err := cosy.ExchangeJobToken(pat, mid, mtoken, mtype, ep.JobTokenURL)
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
	return account.Status{Running: running, Port: s.bridgePort, ActiveAccount: activeID, APIMode: apiMode}
}

func (s *Service) StartBridge() error {
	acct, err := account.GetActive()
	if err != nil || acct == nil {
		return fmt.Errorf("no active account")
	}
	return s.startBridgeWithAccount(acct)
}

// EnsureBridgeRunning 启动时调用：有激活账号且有 secret 则起桥；
// 若激活账号无 secret，尝试第一个有 secret 的账号并激活。
func (s *Service) EnsureBridgeRunning() error {
	s.bridgeMu.Lock()
	running := s.bridgeSrv != nil
	s.bridgeMu.Unlock()
	if running {
		return nil
	}

	if acct, _ := account.GetActive(); acct != nil {
		if account.HasSecret(acct.ID) {
			return s.startBridgeWithAccount(acct)
		}
		logger.Error("active account %s has no secret file", acct.ID)
	}

	accounts, err := account.List()
	if err != nil {
		return err
	}
	for _, a := range accounts {
		if !account.HasSecret(a.ID) {
			continue
		}
		logger.Info("auto-activate account with secret: %s", a.Name)
		if err := s.SetActiveAccount(a.ID); err != nil {
			logger.Error("auto-activate failed: %v", err)
			continue
		}
		return nil
	}
	return fmt.Errorf("no account with usable secret; please OAuth/PAT login in web console")
}

func (s *Service) StopBridge() error {
	s.bridgeMu.Lock()
	defer s.bridgeMu.Unlock()
	if s.bridgeSrv == nil {
		return nil
	}
	err := s.bridgeSrv.Close()
	s.bridgeSrv = nil
	s.bridge = nil
	return err
}

func (s *Service) restartBridge(acct *account.Account) error {
	_ = s.StopBridge()
	return s.startBridgeWithAccount(acct)
}

func (s *Service) startBridgeWithAccount(acct *account.Account) error {
	logger.Info("startBridgeWithAccount: account=%s", acct.Name)
	pat, err := account.GetSecret(acct.ID)
	if err != nil {
		return fmt.Errorf("failed to get secret: %w", err)
	}

	tmpl := string(s.basePrompt)
	for _, ukey := range []string{"{UUID1}", "{UUID2}", "{UUID3}", "{UUID4}", "{UUID5}"} {
		tmpl = strings.ReplaceAll(tmpl, ukey, cosy.NewUUID())
	}
	tmpl = strings.ReplaceAll(tmpl, "{TIME1}", fmt.Sprintf("%d", cosy.UnixMs()))
	var templateBase map[string]interface{}
	_ = json.Unmarshal([]byte(tmpl), &templateBase)

	b, err := bridge.NewBridge(pat, acct.Region, templateBase)
	if err != nil {
		return fmt.Errorf("failed to create bridge: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", b.HandleChatCompletions)
	mux.HandleFunc("/v1/messages", b.HandleClaudeMessages)
	mux.HandleFunc("/v1/models", b.HandleListModels)
	mux.HandleFunc("/v1/responses", b.HandleCodexResponses)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.Info("[HTTP] %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)
		mux.ServeHTTP(w, r)
	})

	srv := &http.Server{
		Addr:    fmt.Sprintf("0.0.0.0:%d", s.bridgePort),
		Handler: handler,
	}

	s.bridgeMu.Lock()
	s.bridge = b
	s.bridgeSrv = srv
	s.bridgeMu.Unlock()

	logger.Info("Bridge started on port %d", s.bridgePort)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("bridge serve error: %v", err)
		}
	}()
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
		jt, err := cosy.ExchangeJobToken(token, cosy.NewUUID(), cosy.NewBase64Token(), cosy.NewHexToken(18), ep.JobTokenURL)
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
			Key:            m.Key,
			DisplayName:    m.DisplayName,
			Enable:         m.Enable,
			IsDefault:      m.IsDefault,
			IsReasoning:    m.IsReasoning,
			MaxInputTokens: m.MaxInputTokens,
			PriceFactor:    m.PriceFactor,
			ContextWindow:  m.ContextWindow,
			MaxOutputTokens: m.MaxOutputTokens,
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
