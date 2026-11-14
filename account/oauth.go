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
			deviceToken, refreshToken, err := pollToken(p.nonce, p.verifier, ep, p.region)
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

// pollToken 轮询 deviceToken/poll 端点（按区域代理出口——设备登录属于
// 「账号 ID 之前」的阶段，只能按区域绑定；直连会与对话流量形成双出口）。
func pollToken(nonce, verifier string, ep Endpoints, region Region) (string, string, error) {
	reqURL := fmt.Sprintf("%s?nonce=%s&verifier=%s&challenge_method=S256",
		ep.PollEndpoint,
		url.QueryEscape(nonce),
		url.QueryEscape(verifier))

	logger.Debug("OAuth: Polling %s", ep.PollEndpoint)

	client, err := RegionHTTPClient(region, 30*time.Second)
	if err != nil {
		return "", "", fmt.Errorf("region proxy config invalid: %w", err)
	}
	resp, err := client.Get(reqURL)
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
	info, err := fetchUserInfo(deviceToken, ep, region)
	if err != nil {
		return nil, fmt.Errorf("fetch user info: %w", err)
	}
	plan := fetchPlan(deviceToken, ep, region)

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
	// 重新授权时保留同一账号已有的设备身份。
	preserveDeviceIdentity(acct, id)
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

// preserveDeviceIdentity 把同一账号已有的设备身份复制到新构造的账号上。
//
// 为什么必须保留：checkin_fingerprint 是「官方客户端抓包的真实设备指纹」，
// 它决定上游按 (clienttype, version) 下发哪些模型——旧协议
// （5 / 1.0.10）拿不到 Sonus/Cantus/Fast，重新授权时若把它抹掉，
// 账号会静默退化成旧协议清单（实测 2026-11-14：重新授权后
// smodel/cmodel/fast 三个模型从清单里消失）。
//
// 同一账号重新授权 = 同一台设备，不该被上游当成「换设备」；
// 抹掉指纹反而制造了这个风险。device_identity 同理（推理侧与
// 签到侧必须共用同一份身份）。
//
// 账号不存在（首次登录）时什么都不做——此时新账号本来就没有指纹。
func preserveDeviceIdentity(acct *Account, id string) {
	prev, err := Get(id)
	if err != nil || prev == nil {
		return
	}
	acct.CheckinFingerprint = prev.CheckinFingerprint
	acct.DeviceIdentity = prev.DeviceIdentity
	acct.CreatedAt = prev.CreatedAt
}

// fetchUserInfo 拉取 userinfo（账号 ID 之前的阶段，按区域代理出口）。
func fetchUserInfo(token string, ep Endpoints, region Region) (map[string]interface{}, error) {
	req, _ := http.NewRequest("GET", ep.UserinfoBase, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	client, err := RegionHTTPClient(region, 15*time.Second)
	if err != nil {
		return nil, fmt.Errorf("region proxy config invalid: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result map[string]interface{}
	raw, _ := io.ReadAll(resp.Body)
	json.Unmarshal(raw, &result)
	return result, nil
}

// fetchPlan 拉取套餐信息（同 fetchUserInfo：按区域代理出口）。
func fetchPlan(token string, ep Endpoints, region Region) string {
	req, _ := http.NewRequest("GET", ep.PlanEndpoint, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	client, err := RegionHTTPClient(region, 15*time.Second)
	if err != nil {
		logger.Error("fetchPlan: region proxy config invalid: %v", err)
		return ""
	}
	resp, err := client.Do(req)
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
	// 额度查询同样按区域代理出口：账号操作直连会与对话流量形成双出口
	client, err := RegionHTTPClient(region, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("region proxy config invalid: %w", err)
	}
	resp, err := client.Do(req)
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
		Plan:            fetchPlan(token, ep, region),
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

	// 专属资源包（活动奖励，如 SOTA 专属积分）。单独成桶，
	// 通常带独立过期时间，不在 user/addon 额度内。
	info.DedicatedPackages = extractDedicatedPackages(result)
	fmt.Printf("[QUOTA DEBUG] dedicatedResourcePackages found: %d\n", len(info.DedicatedPackages))

	return info, nil
}

// extractDedicatedPackages 解析上游 dedicatedResourcePackages 数组。
func extractDedicatedPackages(data map[string]interface{}) []DedicatedPackage {
	raw, ok := data["dedicatedResourcePackages"].([]interface{})
	if !ok || len(raw) == 0 {
		return nil
	}
	out := make([]DedicatedPackage, 0, len(raw))
	for _, item := range raw {
		obj, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		p := DedicatedPackage{
			ID:          stringOf(obj, "id"),
			Name:        stringOf(obj, "name"),
			Description: stringOf(obj, "description"),
			Used:        toFloat(obj, "used"),
			Total:       toFloat(obj, "total"),
			Remaining:   toFloat(obj, "remaining"),
			Unit:        stringOf(obj, "unit"),
			ExpiresAtMs: int64(toFloat(obj, "expiresAt")),
			Status:      stringOf(obj, "status"),
			Available:   obj["available"] == true,
		}
		if labels, ok := obj["displayLabels"].([]interface{}); ok {
			for _, li := range labels {
				lo, ok := li.(map[string]interface{})
				if !ok {
					continue
				}
				l := DedicatedPkgLabel{
					Dimension: stringOf(lo, "dimension"),
					Value:     stringOf(lo, "value"),
				}
				if i18n, ok := lo["valueI18n"].(map[string]interface{}); ok {
					l.ValueI18n = map[string]string{}
					for k, v := range i18n {
						if s, ok := v.(string); ok {
							l.ValueI18n[k] = s
						}
					}
				}
				p.Labels = append(p.Labels, l)
			}
		}
		out = append(out, p)
	}
	return out
}

func stringOf(m map[string]interface{}, key string) string {
	s, _ := m[key].(string)
	return s
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
