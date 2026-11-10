package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"qoder2api/account"
	"qoder2api/internal/bridge"
)

// forwardingProxy 起一个会把请求转发到目标的 HTTP 代理，记录命中次数。
func forwardingProxy(t *testing.T, hits *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		outReq, err := http.NewRequest(r.Method, r.URL.String(), r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		resp, err := http.DefaultTransport.RoundTrip(outReq)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
	}))
}

// TestCheckinUsesAccountProxy 审计 P1-5：签到等「账号 ID 之前」的流量
// 必须走该账号的出口代理。旧实现签到直连，与对话流量形成双出口画像。
func TestCheckinUsesAccountProxy(t *testing.T) {
	var proxyHits int32
	proxy := forwardingProxy(t, &proxyHits)
	defer proxy.Close()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer target.Close()

	acct := &account.Account{ID: "acct-checkin-proxy", Region: account.RegionCN}
	t.Setenv("QD_ACCOUNT_PROXY_acct-che", proxy.URL) // 账号级环境变量优先

	client, err := checkinHTTPClientFor(acct)
	if err != nil {
		t.Fatalf("checkinHTTPClientFor: %v", err)
	}
	if client == nil {
		t.Fatal("nil client")
	}
	// 经该客户端发一次请求：必须命中代理（而不是直连目标）
	resp, err := client.Get(target.URL)
	if err != nil {
		t.Fatalf("get via checkin client: %v", err)
	}
	resp.Body.Close()
	if atomic.LoadInt32(&proxyHits) == 0 {
		t.Error("checkin request did not go through the account proxy")
	}
}

// TestCheckinUnconfiguredProxyIsDirect 未配置代理 → 直连（合法路径，
// 存量行为不变）。
func TestCheckinUnconfiguredProxyIsDirect(t *testing.T) {
	var proxyHits int32
	proxy := forwardingProxy(t, &proxyHits)
	defer proxy.Close()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer target.Close()

	acct := &account.Account{ID: "acct-checkin-direct", Region: account.RegionCN}
	client, err := checkinHTTPClientFor(acct)
	if err != nil {
		t.Fatalf("checkinHTTPClientFor: %v", err)
	}
	resp, err := client.Get(target.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if atomic.LoadInt32(&proxyHits) != 0 {
		t.Error("unconfigured proxy must stay direct")
	}
}

// TestCheckinProxyConfigErrBlocksRequest 代理配置非法（socks5）→
// 签到明确失败，绝不静默直连。
func TestCheckinProxyConfigErrBlocksRequest(t *testing.T) {
	t.Setenv("QD_ACCOUNT_PROXY_acct-bad", "socks5://127.0.0.1:1080")
	acct := &account.Account{ID: "acct-bad-checkin", Region: account.RegionCN}
	client, err := checkinHTTPClientFor(acct)
	if err == nil {
		t.Fatal("socks5 account proxy must fail checkin client setup")
	}
	if client != nil {
		t.Error("no client should be returned on config error")
	}
	if !strings.Contains(err.Error(), "http/https") {
		t.Errorf("error should explain supported schemes: %v", err)
	}
}

// TestCheckinSharesEgressWithChat 同一账号的签到与对话共用同一个
// Transport（出口与连接池同源）——「同一账号同一出口」可自证。
func TestCheckinSharesEgressWithChat(t *testing.T) {
	acct := &account.Account{ID: "acct-shared-egress", Region: account.RegionCN}
	client, err := checkinHTTPClientFor(acct)
	if err != nil {
		t.Fatalf("checkinHTTPClientFor: %v", err)
	}
	chat := bridge.HTTPClientForKey(string(acct.Region)+"|"+acct.ID, "", 60*time.Second)
	if client.Transport != chat.Transport {
		t.Fatal("checkin and chat traffic of one account must share the egress Transport")
	}
}
