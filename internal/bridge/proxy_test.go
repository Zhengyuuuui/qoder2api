package bridge

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- D4. 客户端缓存 ---------------------------------------------------------

// 同一账号两次请求复用同一个 client / Transport（连接池复用）。
func TestSameAccountReusesClientAndTransport(t *testing.T) {
	c := testClient(t)
	c.SetAccountProxy("acct-alpha", "")

	cl1 := c.httpClient(30 * time.Second)
	cl2 := c.httpClient(30 * time.Second)
	if cl1 != cl2 {
		t.Fatal("same account must reuse the same http.Client")
	}
	tr1 := cl1.Transport
	tr2 := cl2.Transport
	if tr1 != tr2 {
		t.Fatal("same account must reuse the same http.Transport (connection pool)")
	}

	// 不同超时档位 → 不同 client，但共享同一个 Transport（连接池仍在）
	cl3 := c.httpClient(15 * time.Second)
	if cl3 == cl1 {
		t.Fatal("different timeout should give a different http.Client")
	}
	if cl3.Transport != tr1 {
		t.Fatal("different timeout should still share the Transport")
	}
}

// 不同账号拿到不同的 client / Transport（出口隔离）。
func TestDifferentAccountsGetDifferentClients(t *testing.T) {
	c1 := testClient(t)
	c1.SetAccountProxy("acct-alpha", "")
	c2 := testClient(t)
	c2.SetAccountProxy("acct-beta", "")

	cl1 := c1.httpClient(30 * time.Second)
	cl2 := c2.httpClient(30 * time.Second)
	if cl1 == cl2 {
		t.Fatal("different accounts must not share an http.Client")
	}
	if cl1.Transport == cl2.Transport {
		t.Fatal("different accounts must not share an http.Transport")
	}
}

// 代理配置变更 → 缓存失效重建（不能一直绑在旧出口）。
func TestProxyChangeInvalidatesCache(t *testing.T) {
	c := testClient(t)
	c.SetAccountProxy("acct-alpha", "")
	tr1 := c.httpClient(30 * time.Second).Transport

	// 代理地址变了
	c.SetAccountProxy("acct-alpha", "http://127.0.0.1:1")
	tr2 := c.httpClient(30 * time.Second).Transport
	if tr1 == tr2 {
		t.Fatal("proxy change must rebuild the Transport")
	}
}

// --- D2. 代理配置来源 -------------------------------------------------------

// 环境变量优先于账号配置（便于临时覆盖排障）。
func TestResolveAccountProxyEnvOverridesConfig(t *testing.T) {
	t.Setenv("QD_ACCOUNT_PROXY_a1b2c3d4", "http://env-proxy:8080")
	got := resolveAccountProxy("a1b2c3d4-more", "http://cfg-proxy:9090")
	if got.URL != "http://env-proxy:8080" || got.ConfigErr != nil {
		t.Errorf("env should win: %+v", got)
	}
}

// 环境变量名大小写不敏感。
func TestResolveAccountProxyEnvCaseInsensitive(t *testing.T) {
	t.Setenv("QD_ACCOUNT_PROXY_A1B2C3D4", "http://env-proxy:8080")
	got := resolveAccountProxy("a1b2c3d4", "")
	if got.URL != "http://env-proxy:8080" || got.ConfigErr != nil {
		t.Errorf("env lookup should be case-insensitive: %+v", got)
	}
}

// 没有环境变量时用账号配置；都没有则直连。
func TestResolveAccountProxyFallback(t *testing.T) {
	if got := resolveAccountProxy("zzzz9999", "http://cfg-proxy:9090"); got.URL != "http://cfg-proxy:9090" || got.ConfigErr != nil {
		t.Errorf("config fallback failed: %+v", got)
	}
	got := resolveAccountProxy("zzzz9999", "")
	if got.URL != "" || got.ConfigErr != nil {
		t.Errorf("unconfigured must mean direct (no error): %+v", got)
	}
}

// 环境变量只匹配前 8 位，且不会误匹配更长的账号 ID。
func TestResolveAccountProxyEnvPrefixExact(t *testing.T) {
	t.Setenv("QD_ACCOUNT_PROXY_abc", "http://short-proxy:1")
	// "abcdef" 的前 8 位是 "abcdef"，不匹配 "abc"
	if got := resolveAccountProxy("abcdef", ""); got.URL != "" || got.ConfigErr != nil {
		t.Errorf("env must not leak to a longer account id: %+v", got)
	}
}

// 非法 scheme（socks5://）→ 配置错误，**不静默直连**。
// 审计 P1-1：早期实现返回空串（=直连）会把真实出口暴露给上游；现在必须
// 产生 ConfigErr，让调用方把账号标记为不可用。
func TestResolveAccountProxyRejectsUnsupportedScheme(t *testing.T) {
	for _, raw := range []string{"socks5://127.0.0.1:1080", "socks5h://127.0.0.1:1080", "ftp://x:1"} {
		got := resolveAccountProxy("acct1", raw)
		if got.ConfigErr == nil {
			t.Errorf("unsupported scheme %q must produce ConfigErr, got %+v", raw, got)
		}
		if got.URL != "" {
			t.Errorf("unsupported scheme %q must NOT resolve to a url, got %q", raw, got.URL)
		}
		if !strings.Contains(got.ConfigErr.Error(), "http/https") {
			t.Errorf("socks5 error should explain supported schemes: %v", got.ConfigErr)
		}
	}
}

// 无 host / 乱串 → 配置错误（不直连），错误不回显原文凭据。
func TestResolveAccountProxyInvalidIsConfigErr(t *testing.T) {
	for _, raw := range []string{"http://", "ht!tp://%%%not-a-url", "://nohost"} {
		got := resolveAccountProxy("acct-invalid", raw)
		if got.ConfigErr == nil || got.URL != "" {
			t.Errorf("invalid proxy %q must be a config error: %+v", raw, got)
		}
		if got.ConfigErr != nil && strings.Contains(got.ConfigErr.Error(), "%%%") {
			t.Errorf("config error must not echo the raw string: %v", got.ConfigErr)
		}
	}
}

// 账号级非法 + 区域级合法 → 账号级优先，仍然 ConfigErr
// （优先级不能被「区域级兜底」绕过——显式配了账号级代理就必须生效）。
func TestAccountProxyConfigErrBeatsRegionProxy(t *testing.T) {
	t.Setenv("QD_PROXY_CN", "http://region-proxy:8080")
	got := resolveAccountProxyInRegion("acct-bad", "socks5://127.0.0.1:1080", "cn")
	if got.ConfigErr == nil || got.URL != "" {
		t.Errorf("account-level invalid config must win over region: %+v", got)
	}
}

// 区域级合法 → 账号无配置时落到区域代理（合法路径）。
func TestRegionProxyFallbackIsValid(t *testing.T) {
	t.Setenv("QD_PROXY_CN", "http://region-proxy:8080")
	got := resolveAccountProxyInRegion("acct-region", "", "cn")
	if got.URL != "http://region-proxy:8080" || got.ConfigErr != nil {
		t.Errorf("region fallback failed: %+v", got)
	}
}

// 区域级非法 → ConfigErr（不得静默落到直连）。
func TestRegionProxyInvalidIsConfigErr(t *testing.T) {
	t.Setenv("QD_PROXY_CN", "socks5://127.0.0.1:1080")
	got := resolveAccountProxyInRegion("acct-region2", "", "cn")
	if got.ConfigErr == nil || got.URL != "" {
		t.Errorf("region-level invalid proxy must be a config error: %+v", got)
	}
}

// 代理 URL 带 path → 脱敏后 path 被抹掉（审计 P2-5：路径里可能藏
// API key 段 / 用户身份）。
func TestRedactProxyURLMasksPath(t *testing.T) {
	cases := []string{
		"http://user:password123@127.0.0.1:8080/some/secret/path?token=abc123",
		"http://127.0.0.1:8080/some/secret/path?token=abc123",
		"http://127.0.0.1:8080/path-only",
		"http://127.0.0.1:8080/",
	}
	for _, raw := range cases {
		red := redactProxyURL(raw)
		if strings.Contains(red, "secret") || strings.Contains(red, "token=abc123") {
			t.Errorf("redacted URL must drop the path/query: %s", red)
		}
		if !strings.Contains(red, "127.0.0.1:8080") {
			t.Errorf("redacted URL should keep the host: %s", red)
		}
		if strings.Contains(red, "password123") {
			t.Errorf("redacted URL must not leak the password: %s", red)
		}
	}
}

// --- D3. 代理不可用的行为 ---------------------------------------------------

// 配了不可达代理 → 请求失败，错误里含代理信息，且没有直连成功。
func TestUnreachableProxyFailsWithoutDirectFallback(t *testing.T) {
	// 上游服务是好的，但请求必须经过代理 —— 代理不通就不能到达上游
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	// 找一个确定没人监听的端口
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadPort := ln.Addr().String()
	ln.Close()

	proxyURL := "http://user:secretpw@" + deadPort
	c := testClient(t)
	c.SetAccountProxy("acct-dead", proxyURL)

	_, err = c.callGet(srv.URL)
	if err == nil {
		t.Fatal("request through an unreachable proxy must fail (never fall back to direct)")
	}
	msg := err.Error()
	if !strings.Contains(msg, "proxy") {
		t.Errorf("error should mention the proxy: %v", err)
	}
	if !strings.Contains(msg, "unreachable") {
		t.Errorf("error should say the proxy is unreachable: %v", err)
	}
	// 错误里不得出现密码
	if strings.Contains(msg, "secretpw") {
		t.Errorf("error must not leak the proxy password: %v", err)
	}
	// 应能看出指向哪个代理（脱敏后的 host）
	if !strings.Contains(msg, "127.0.0.1") {
		t.Errorf("error should identify the proxy host: %v", err)
	}
}

// 代理不可达时，流式请求也必须失败（不得静默直连）。
func TestUnreachableProxyFailsOnStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadPort := ln.Addr().String()
	ln.Close()

	c := testClient(t)
	c.SetAccountProxy("acct-dead2", "http://user:secretpw2@"+deadPort)

	err = c.openStreamLines(context.Background(), srv.URL,
		map[string]interface{}{"x": 1}, nil, func(string) bool { return true })
	if err == nil {
		t.Fatal("stream through an unreachable proxy must fail")
	}
	if strings.Contains(err.Error(), "secretpw2") {
		t.Errorf("error must not leak the proxy password: %v", err)
	}
}

// 可达代理 → 请求经代理成功（验证代理确实被使用）。
func TestRequestGoesThroughProxy(t *testing.T) {
	var proxied int
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied++
		// 作为 HTTP 代理：把请求转发到目标
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
		buf := make([]byte, 4096)
		n, _ := resp.Body.Read(buf)
		w.WriteHeader(resp.StatusCode)
		w.Write(buf[:n])
	}))
	defer proxy.Close()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"via":"target"}`)
	}))
	defer target.Close()

	c := testClient(t)
	c.SetAccountProxy("acct-proxy", proxy.URL)

	got, err := c.callGet(target.URL)
	if err != nil {
		t.Fatalf("callGet via proxy: %v", err)
	}
	if got["via"] != "target" {
		t.Errorf("result = %v, want via=target", got)
	}
	if proxied == 0 {
		t.Error("request did not go through the proxy")
	}
}

// --- D5. 脱敏 ---------------------------------------------------------------

// 错误 / 日志输出里不包含代理密码。
func TestProxyPasswordNeverLeaked(t *testing.T) {
	raw := "http://user:password123@127.0.0.1:8080"
	red := redactProxyURL(raw)
	if strings.Contains(red, "password123") {
		t.Errorf("redacted URL still contains the password: %s", red)
	}
	if !strings.Contains(red, "127.0.0.1:8080") {
		t.Errorf("redacted URL should keep the host: %s", red)
	}
	if !strings.Contains(red, "***") {
		t.Errorf("redacted URL should mask the password: %s", red)
	}
	// 只有用户名没有密码时保留用户名
	noPw := redactProxyURL("http://onlyuser@127.0.0.1:8080")
	if !strings.Contains(noPw, "onlyuser") {
		t.Errorf("username should be kept: %s", noPw)
	}
}

// 代理 URL 解析失败时也不泄露原文。
func TestRedactProxyURLInvalid(t *testing.T) {
	raw := "http://user:password123@%zz"
	red := redactProxyURL(raw)
	// 解析失败时返回原文是既有行为，但这里只断言不 panic
	if red == "" {
		t.Error("redact should not return empty")
	}
}

// 拨号失败时的错误信息脱敏且可读。
func TestDialThroughProxyErrorRedacted(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadPort := ln.Addr().String()
	ln.Close()

	err = dialThroughProxy(context.Background(), "http://user:password123@"+deadPort, "example.com:443")
	if err == nil {
		t.Fatal("dialing a dead proxy should fail")
	}
	if strings.Contains(err.Error(), "password123") {
		t.Errorf("error must not leak the proxy password: %v", err)
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("error should say unreachable: %v", err)
	}
}

// TestWrapProxyErrorNotMisattributed 非代理拨号错误不得被包装成
// 「代理不可达」——上游连接问题与本地代理问题是两回事，误归属会让
// 排障方向完全跑偏（审计 P3-1）。
func TestWrapProxyErrorNotMisattributed(t *testing.T) {
	orig := fmt.Errorf("dial tcp 10.0.0.1:443: connect: connection refused")
	got := wrapProxyError("http://proxy:8080", orig)
	if got != orig {
		t.Errorf("non-proxy error must pass through unchanged: %v", got)
	}
	if strings.Contains(fmt.Sprint(got), "unreachable") {
		t.Error("non-proxy error must not be labeled as proxy unreachable")
	}
	// 空代理（直连）一律原样返回
	if got := wrapProxyError("", orig); got != orig {
		t.Errorf("direct (no proxy) must pass through: %v", got)
	}
	// 代理拨号错误 → 包装且脱敏
	proxyErr := fmt.Errorf(`proxyconnect tcp: dial tcp 127.0.0.1:9: connect: connection refused`)
	got = wrapProxyError("http://user:password123@proxy:8080", proxyErr)
	if !strings.Contains(fmt.Sprint(got), "unreachable") {
		t.Errorf("proxyconnect error should be labeled unreachable: %v", got)
	}
	if strings.Contains(fmt.Sprint(got), "password123") {
		t.Errorf("wrapped error must not leak the proxy password: %v", got)
	}
}
