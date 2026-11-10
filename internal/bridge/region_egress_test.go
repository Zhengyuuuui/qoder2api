package bridge

import (
	"strings"
	"testing"
	"time"
)

// --- N1. 区域级代理 ---------------------------------------------------------

// 区域级代理生效（国际版走 QD_PROXY_GLOBAL，国内版走 QD_PROXY_CN）。
func TestRegionProxyApplied(t *testing.T) {
	t.Setenv(ProxyEnvGlobal, "http://global-proxy:8080")
	t.Setenv(ProxyEnvCN, "http://cn-proxy:8081")

	global := resolveAccountProxyInRegion("acct-g", "", "global")
	if global.URL != "http://global-proxy:8080" || global.ConfigErr != nil {
		t.Errorf("global proxy = %+v, want http://global-proxy:8080", global)
	}
	cn := resolveAccountProxyInRegion("acct-c", "", "cn")
	if cn.URL != "http://cn-proxy:8081" || cn.ConfigErr != nil {
		t.Errorf("cn proxy = %+v, want http://cn-proxy:8081", cn)
	}
}

// 账号级代理覆盖区域级。
func TestAccountProxyOverridesRegionProxy(t *testing.T) {
	t.Setenv(ProxyEnvGlobal, "http://global-proxy:8080")

	// 账号配置文件里的代理
	got := resolveAccountProxyInRegion("acct-a", "http://acct-proxy:9090", "global")
	if got.URL != "http://acct-proxy:9090" || got.ConfigErr != nil {
		t.Errorf("account proxy should win: got %+v", got)
	}
	// 账号级环境变量优先级最高
	t.Setenv("QD_ACCOUNT_PROXY_acct-b", "http://acct-env-proxy:9091")
	got = resolveAccountProxyInRegion("acct-b", "http://acct-proxy:9090", "global")
	if got.URL != "http://acct-env-proxy:9091" || got.ConfigErr != nil {
		t.Errorf("account env proxy should win: got %+v", got)
	}
}

// 未配置任何代理 → 直连（空 URL、无错误）。
func TestNoProxyMeansDirect(t *testing.T) {
	got := resolveAccountProxyInRegion("acct-none", "", "global")
	if got.URL != "" || got.ConfigErr != nil {
		t.Errorf("expected direct (no error), got %+v", got)
	}
}

// 区域级代理的 scheme 校验与账号级一致：socks5 → 配置错误，绝不静默直连
// （直连会把真实出口暴露给上游）。
func TestRegionProxyRejectsUnsupportedScheme(t *testing.T) {
	t.Setenv(ProxyEnvGlobal, "socks5://127.0.0.1:1080")
	got := resolveAccountProxyInRegion("acct-x", "", "global")
	if got.URL != "" || got.ConfigErr == nil {
		t.Errorf("socks5 region proxy must be a config error (never silently direct), got %+v", got)
	}
}

// --- N2. 区域独立客户端 -----------------------------------------------------

// 两个区域用不同 client/transport（出口不同，连接池不能混）。
func TestDifferentRegionsUseDifferentClients(t *testing.T) {
	c1 := testClient(t)
	c1.SetAccountProxy("acct-same-id", "")
	c1.SetAccountRegion("global")
	c2 := testClient(t)
	c2.SetAccountProxy("acct-same-id", "")
	c2.SetAccountRegion("cn")

	cl1 := c1.httpClient(30 * time.Second)
	cl2 := c2.httpClient(30 * time.Second)
	if cl1 == cl2 {
		t.Fatal("different regions must not share an http.Client")
	}
	if cl1.Transport == cl2.Transport {
		t.Fatal("different regions must not share an http.Transport")
	}
}

// 同区域同账号 → 复用同一个 client/transport。
func TestSameRegionSameAccountReusesClient(t *testing.T) {
	c := testClient(t)
	c.SetAccountProxy("acct-x", "")
	c.SetAccountRegion("global")
	cl1 := c.httpClient(15 * time.Second)
	cl2 := c.httpClient(15 * time.Second)
	if cl1 != cl2 {
		t.Fatal("same region+account must reuse the client")
	}
	if cl1.Transport != cl2.Transport {
		t.Fatal("same region+account must reuse the transport")
	}
}

// --- N3. 诊断可见性 ---------------------------------------------------------

// 诊断接口按区域分组返回出口信息，且密码脱敏。
func TestRegionEgressStatusesGroupedAndRedacted(t *testing.T) {
	// 重置登记表（测试隔离）
	egressRegistry.mu.Lock()
	egressRegistry.byReg = map[string][]egressEntry{}
	egressRegistry.byAcc = map[string]egressEntry{}
	egressRegistry.mu.Unlock()

	t.Setenv(ProxyEnvGlobal, "http://user:password123@global-proxy:8080")
	t.Setenv(ProxyEnvCN, "http://cn-proxy:8081")

	registerEgress("global", "acct-g1", "http://user:password123@global-proxy:8080")
	registerEgress("global", "acct-g2", "http://user:password123@global-proxy:8080")
	registerEgress("cn", "acct-c1", "http://cn-proxy:8081")

	st := RegionEgressStatuses()
	if len(st) != 2 {
		t.Fatalf("expected 2 regions, got %d", len(st))
	}
	byRegion := map[string]RegionEgressStatus{}
	for _, s := range st {
		byRegion[s.Region] = s
	}

	global, ok := byRegion["global"]
	if !ok {
		t.Fatal("global region missing")
	}
	if len(global.Accounts) != 2 {
		t.Errorf("global accounts = %d, want 2", len(global.Accounts))
	}
	if global.UniqueEgress != 1 {
		t.Errorf("global unique egress = %d, want 1", global.UniqueEgress)
	}
	// 共用同一出口的风险提示
	if global.SharedWarning == "" {
		t.Error("shared egress warning should be present")
	}
	// 密码脱敏
	for _, a := range global.Accounts {
		if strings.Contains(a.Egress, "password123") {
			t.Errorf("egress leaks the proxy password: %s", a.Egress)
		}
	}
	if strings.Contains(global.RegionProxy, "password123") {
		t.Errorf("region proxy leaks the password: %s", global.RegionProxy)
	}

	cn, ok := byRegion["cn"]
	if !ok {
		t.Fatal("cn region missing")
	}
	if len(cn.Accounts) != 1 {
		t.Errorf("cn accounts = %d, want 1", len(cn.Accounts))
	}
}

// 未配置区域代理 → 诊断里给出「无专用出口」的提示。
func TestRegionEgressNoDedicatedWarning(t *testing.T) {
	egressRegistry.mu.Lock()
	egressRegistry.byReg = map[string][]egressEntry{}
	egressRegistry.byAcc = map[string]egressEntry{}
	egressRegistry.mu.Unlock()

	// 清掉区域代理环境变量
	t.Setenv(ProxyEnvGlobal, "")
	t.Setenv(ProxyEnvCN, "")

	registerEgress("global", "acct-direct", "")
	st := RegionEgressStatuses()
	if len(st) != 1 {
		t.Fatalf("expected 1 region, got %d", len(st))
	}
	if st[0].NoDedicatedEgress == "" {
		t.Error("no-dedicated-egress warning should be present")
	}
	if !st[0].Accounts[0].Direct {
		t.Error("account should be marked direct")
	}
}

// TestSameEgressForChatAndCheckin 审计 P1-5：同一账号的对话流量与签到
// / 额度查询必须走同一个出口。checkin 等账号 ID 之前的流量按
// (region, account) 维度取客户端（clientKey 同构）——两边 key 一致 →
// Transport 一致 → 出口画像不分裂。
func TestSameEgressForChatAndCheckin(t *testing.T) {
	c := testClient(t)
	c.SetAccountProxy("acct-egress", "http://proxy-egress:8080")
	c.SetAccountRegion("cn")

	chatTransport := c.httpClient(30 * time.Second).Transport
	// 签到/续期侧用 (region|account) 维度取客户端（同一账号同一代理）
	auxTransport := HTTPClientForKey(c.clientKey(), "http://proxy-egress:8080", 15*time.Second).Transport
	if chatTransport != auxTransport {
		t.Fatal("chat and checkin traffic of one account must share the same egress Transport")
	}
}

// TestRegionEgressStatusesTruncatesAccountID 审计 P2-3：诊断接口不得
// 暴露完整账号 ID（/health 是公开端点）。
func TestRegionEgressStatusesTruncatesAccountID(t *testing.T) {
	egressRegistry.mu.Lock()
	egressRegistry.byReg = map[string][]egressEntry{}
	egressRegistry.byAcc = map[string]egressEntry{}
	egressRegistry.mu.Unlock()

	const fullID = "abcdef1234567890abcdef"
	registerEgress("cn", fullID, "http://cn-proxy:8081")
	st := RegionEgressStatuses()
	if len(st) != 1 || len(st[0].Accounts) != 1 {
		t.Fatalf("unexpected statuses: %+v", st)
	}
	got := st[0].Accounts[0].AccountID
	if got == fullID {
		t.Error("diagnostics must not expose the full account id")
	}
	if len(got) > 8 {
		t.Errorf("account id in diagnostics too long: %q", got)
	}
	if !strings.HasPrefix(fullID, got) {
		t.Errorf("account id should be a prefix of the real one: %q vs %q", got, fullID)
	}
}
