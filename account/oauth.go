package account

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"qoder2api/logger"
)

// OAuth 认证流程
// 1. StartLogin: 生成 PKCE 参数，返回登录 URL
// 2. WaitLogin: 轮询 deviceToken/poll 端点，等待用户授权
// 3. 获取 device token (dt-xxx)，用于后续 API 调用

const oauthClientID = "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb"

// oauthSessionTTL 是待授权 OAuth 会话的有效期。默认 10 分钟（用户需在
// 此时间内完成浏览器授权），可用 QODER2API_OAUTH_TIMEOUT 覆盖（如 "30m"）。
func oauthSessionTTL() time.Duration {
	if v := strings.TrimSpace(os.Getenv("QODER2API_OAUTH_TIMEOUT")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		logger.Error("invalid QODER2API_OAUTH_TIMEOUT=%q, falling back to 10m", v)
	}
	return 10 * time.Minute
}

type pendingOAuth struct {
	loginID  string
	nonce    string
	verifier string
	region   Region
	ctx      context.Context
	cancel   context.CancelFunc
	deadline time.Time
}

var (
	pendingMu sync.Mutex
	pending   *pendingOAuth
)

// StartLogin 启动 OAuth 登录流程
func StartLogin(region Region) (*OAuthSession, error) {
	verifier, challenge, err := pkce()
	if err != nil {
		return nil, err
	}
	nonce := newSimpleID()
	loginID := newSimpleID()

	ep := GetEndpoints(region)
	params := url.Values{}
	params.Set("nonce", nonce)
	params.Set("challenge", challenge)
	params.Set("challenge_method", "S256")
	params.Set("client_id", oauthClientID)
	loginURL := ep.DeviceLoginBase + "?" + params.Encode()

	logger.Info("OAuth: StartLogin loginID=%s region=%s", loginID, region)

	pendingMu.Lock()
	if pending != nil {
		pending.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	pending = &pendingOAuth{
		loginID:  loginID,
		nonce:    nonce,
		verifier: verifier,
		region:   region,
		ctx:      ctx,
		cancel:   cancel,
		deadline: time.Now().Add(oauthSessionTTL()),
	}
	pendingMu.Unlock()

	return &OAuthSession{LoginID: loginID, LoginURL: loginURL}, nil
}

// WaitLogin 等待用户完成 OAuth 授权
func WaitLogin(loginID string) (*Account, error) {
	logger.Debug("OAuth: WaitLogin called loginID=%s", loginID)

	pendingMu.Lock()
	p := pending
	pendingMu.Unlock()

	if p == nil || p.loginID != loginID {
		logger.Error("OAuth: no pending login for id %s", loginID)
		return nil, fmt.Errorf("no pending login for id %s", loginID)
	}

	ep := GetEndpoints(p.region)

	logger.Info("OAuth: Starting poll loop")
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if time.Now().After(p.deadline) {
			logger.Error("OAuth: Timeout reached")
			return nil, fmt.Errorf("oauth login timed out")
		}
		select {
		case <-p.ctx.Done():
			logger.Info("OAuth: Cancelled")
			return nil, fmt.Errorf("oauth login cancelled")
		case <-ticker.C:
			deviceToken, refreshToken, err := pollToken(p.nonce, p.verifier, ep)
			if err != nil {
				continue
			}
			logger.Info("OAuth: Got token, building account")
			acct, err := buildAccountFromToken(deviceToken, refreshToken, p.region, ep)
			if err != nil {
				logger.Error("OAuth: Build account error: %v", err)
				return nil, err
			}
			pendingMu.Lock()
			pending = nil
			pendingMu.Unlock()
			logger.Info("OAuth: Success! Account: %s", acct.Name)
			return acct, nil
		}
	}
}

func CancelLogin(loginID string) {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	if pending != nil && pending.loginID == loginID {
		pending.cancel()
		pending = nil
	}
}

// pollToken 轮询 deviceToken/poll 端点
func pollToken(nonce, verifier string, ep Endpoints) (string, string, error) {
	reqURL := fmt.Sprintf("%s?nonce=%s&verifier=%s&challenge_method=S256",
		ep.PollEndpoint,
		url.QueryEscape(nonce),
		url.QueryEscape(verifier))

	logger.Debug("OAuth: Polling %s", ep.PollEndpoint)

	resp, err := http.Get(reqURL)
	if err != nil {
		logger.Error("OAuth: Poll error: %v", err)
		return "", "", err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	logger.Debug("OAuth: Response [%d]: %s", resp.StatusCode, string(raw))

	// 404 表示还没授权，继续等待
	if resp.StatusCode == 404 {
		return "", "", fmt.Errorf("not authorized yet")
	}

	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("poll: HTTP %d", resp.StatusCode)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", "", err
	}

	deviceToken, _ := result["token"].(string)
	refreshToken, _ := result["refresh_token"].(string)

	if deviceToken == "" {
		return "", "", fmt.Errorf("no device token in response")
	}
	if refreshToken != "" {
		logger.Info("OAuth: Got device token and refresh token")
	} else {
		logger.Info("OAuth: Got device token")
	}
	return deviceToken, refreshToken, nil
}

// buildAccountFromToken 使用 device token 构建账号信息
func buildAccountFromToken(deviceToken, refreshToken string, region Region, ep Endpoints) (*Account, error) {
	info, err := fetchUserInfo(deviceToken, ep)
	if err != nil {
		return nil, fmt.Errorf("fetch user info: %w", err)
	}
	plan := fetchPlan(deviceToken, ep)

	id := SanitizeID(strGet(info, "userId") + strGet(info, "email"))
	if id == "" {
		id = newSimpleID()
	}
	now := time.Now()
	acct := &Account{
		ID:        id,
		Name:      strGet(info, "name"),
		Email:     strings.ToLower(strGet(info, "email")),
		UserType:  strGet(info, "userType"),
		Plan:      plan,
		Region:    region,
		AuthMode:  "oauth",
		APIMode:   "openai",
		Tags:      []string{},
		CreatedAt: now,
	}
	secretPayload, err := json.Marshal(map[string]string{
		"device_token":  deviceToken,
		"refresh_token": refreshToken,
	})
	if err != nil {
		return nil, err
	}
	if err := SaveSecret(id, string(secretPayload)); err != nil {
		return nil, err
	}
	return acct, nil
}

func fetchUserInfo(token string, ep Endpoints) (map[string]interface{}, error) {
	req, _ := http.NewRequest("GET", ep.UserinfoBase, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result map[string]interface{}
	raw, _ := io.ReadAll(resp.Body)
	json.Unmarshal(raw, &result)
	return result, nil
}

func fetchPlan(token string, ep Endpoints) string {
	req, _ := http.NewRequest("GET", ep.PlanEndpoint, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var result map[string]interface{}
	raw, _ := io.ReadAll(resp.Body)
	json.Unmarshal(raw, &result)
	return strGet(result, "plan_tier_name")
}

func pkce() (verifier, challenge string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	h := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(h[:])
	return
}

func newSimpleID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func strGet(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func FetchQuota(token string, region Region) (*QuotaInfo, error) {
	ep := GetEndpoints(region)
	req, _ := http.NewRequest("GET", ep.QuotaEndpoint, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	fmt.Printf("[QUOTA DEBUG] API URL: %s\n", ep.QuotaEndpoint)
	fmt.Printf("[QUOTA DEBUG] Raw response: %s\n", string(raw))

	var result map[string]interface{}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}

	info := &QuotaInfo{
		Plan:            fetchPlan(token, ep),
		IsQuotaExceeded: result["isQuotaExceeded"] == true,
		ExpiresAt:       int64(toFloat(result, "expiresAt")),
	}

	if uq := extractBucket(result, "userQuota"); uq != nil {
		info.UserQuota = uq
		fmt.Printf("[QUOTA DEBUG] userQuota found: %+v\n", uq)
	} else {
		fmt.Printf("[QUOTA DEBUG] userQuota NOT found in response\n")
	}

	if aq := extractBucket(result, "addOnQuota"); aq != nil {
		info.AddonQuota = aq
		fmt.Printf("[QUOTA DEBUG] addonQuota found: %+v\n", aq)
	} else {
		fmt.Printf("[QUOTA DEBUG] addonQuota NOT found in response\n")
	}

	return info, nil
}

func extractBucket(data map[string]interface{}, key string) *QuotaBucket {
	obj, ok := data[key].(map[string]interface{})
	if !ok {
		return nil
	}
	used := toFloat(obj, "used")
	total := toFloat(obj, "total")
	remaining := toFloat(obj, "remaining")
	if total == 0 && used == 0 && remaining == 0 {
		return nil
	}
	resetTime := ""
	if v, ok := obj["resetTime"].(string); ok {
		resetTime = v
	} else if v, ok := obj["reset_time"].(string); ok {
		resetTime = v
	}
	return &QuotaBucket{Used: used, Total: total, Remaining: remaining, ResetTime: resetTime}
}

func toFloat(m map[string]interface{}, key string) float64 {
	switch v := m[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	}
	return 0
}
