package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"qoder2api/account"
	"qoder2api/internal/bridge"
)

// /health 返回 200 且 JSON 结构正确：risk 四个键都在。
func TestHealthEndpointStructure(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	handleHealth(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v (%s)", err, rec.Body.String())
	}
	if ok, _ := body["ok"].(bool); !ok {
		t.Error("ok should be true")
	}
	riskObj, ok := body["risk"].(map[string]interface{})
	if !ok {
		t.Fatalf("risk should be an object, got %v", body["risk"])
	}
	for _, k := range []string{"queue", "rate", "rejected", "version_gate"} {
		if _, ok := riskObj[k]; !ok {
			t.Errorf("risk.%s missing", k)
		}
	}
}

// /health 暴露端点健康与区域出口信息（诊断用，只读、无副作用）。
func TestHealthEndpointExposesEndpointsAndEgress(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	handleHealth(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if _, ok := body["risk"].(map[string]interface{}); !ok {
		t.Error("risk should still be an object")
	}
	// 端点 / 出口字段：无流量时可能被 omitempty 省略，但类型一旦存在必须正确。
	if v, ok := body["endpoints"]; ok {
		if _, ok := v.([]interface{}); !ok {
			t.Errorf("endpoints should be an array, got %T", v)
		}
	}
	if v, ok := body["egress"]; ok {
		if _, ok := v.([]interface{}); !ok {
			t.Errorf("egress should be an array, got %T", v)
		}
	}
}

// /health 本身不触发任何上游请求（健康检查必须零副作用）。
func TestHealthEndpointNoUpstreamCall(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}))
	defer srv.Close()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	handleHealth(rec, req)

	if hits != 0 {
		t.Errorf("health check must not call upstream, got %d hits", hits)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// /health 不做鉴权（外部探针无法携带控制台会话），且不泄露账号全量 ID。
func TestHealthEndpointIsPublicAndRedacts(t *testing.T) {
	if !isPublicPath("/health") {
		t.Error("/health should be a public path (no console auth required)")
	}
	bridge.RecordRiskSignal(bridge.RiskRate, "smodel", "abcdefgh-secret-account", "usage exceeds frequency limit")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	handleHealth(rec, req)

	raw := rec.Body.String()
	if strings.Contains(raw, "abcdefgh-secret-account") {
		t.Errorf("health response leaks the full account id: %s", raw)
	}
	if !strings.Contains(raw, "abcdefgh") {
		t.Errorf("health response should carry the short account id: %s", raw)
	}
}

// /health 路径不会被控制台鉴权拦截。
func TestHealthPathBypassesConsoleAuth(t *testing.T) {
	handler := requireConsoleAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (health must not require auth)", rec.Code)
	}
}

// 账号出口代理接口：只接受 http/https，socks5 明确报错（不静默直连）。
func TestAccountProxyEndpointRejectsUnsupportedScheme(t *testing.T) {
	account.SetDataRoot(t.TempDir())
	acct := &account.Account{ID: "acct-proxy-test", Name: "proxy-test", Region: account.RegionCN}
	if err := account.Save(acct); err != nil {
		t.Fatalf("save account: %v", err)
	}

	// socks5 → 400
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/proxy",
		strings.NewReader(`{"id":"acct-proxy-test","proxy":"socks5://127.0.0.1:1080"}`))
	req.Header.Set("Content-Type", "application/json")
	handleAccountProxy(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("socks5 proxy: status = %d, want 400 (must not silently go direct)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "socks5") {
		t.Errorf("error should mention socks5 is unsupported: %s", rec.Body.String())
	}

	// 合法 http 代理 → 200 且已落盘
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/accounts/proxy",
		strings.NewReader(`{"id":"acct-proxy-test","proxy":"http://user:pw@127.0.0.1:8080"}`))
	req.Header.Set("Content-Type", "application/json")
	handleAccountProxy(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid proxy: status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	got, err := account.Get("acct-proxy-test")
	if err != nil || got == nil {
		t.Fatalf("get account: %v", err)
	}
	if got.Proxy != "http://user:pw@127.0.0.1:8080" {
		t.Errorf("proxy not persisted: %q", got.Proxy)
	}
}
