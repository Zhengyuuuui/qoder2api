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

func TestBuildRiskDiagnosticsHints(t *testing.T) {
	slots := []bridge.SlotStatus{{
		ID: "a1b2c3d4", Region: "global", CoolingRemainingMS: 0,
		BreakerFor: 1200, ModelCooldowns: map[string]int64{"qmodel": 30000},
		HardFails: 3, UnknownFails: 1, ActiveLeases: 1,
	}}
	sum := bridge.PoolSummary{Total: 1, ReadyCount: 0}
	accounts := []account.Account{{ID: "a1b2c3d4", Proxy: "http://user:secret@127.0.0.1:1080"}}
	identity := []bridge.IdentityAuditRow{{
		Account: "a1b2c3d4", Conclusion: bridge.IdentityDrift, Drift: true, Detail: "derivation changed",
	}}
	throttle := bridge.ThrottleStats{Level: 1, WindowSec: 300, QueuePct: 6.5}

	d := buildRiskDiagnostics(slots, sum, accounts,
		bridge.RiskStats{Queue: 3}, throttle, bridge.AffinityStats{Bound: 5}, identity)

	joined := strings.Join(d.Hints, "\n")
	for _, want := range []string{"熔断", "qmodel", "不可用", "漂移", "节流"} {
		if !strings.Contains(joined, want) {
			t.Errorf("hints missing %q:\n%s", want, joined)
		}
	}

	// 账号卡片字段完整（熔断中 → 不健康）
	if len(d.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(d.Accounts))
	}
	if d.Accounts[0].Healthy {
		t.Error("account in breaker window must not be healthy")
	}
	if d.Accounts[0].BreakerFor != 1200 || d.Accounts[0].ActiveLeases != 1 {
		t.Errorf("account card = %+v", d.Accounts[0])
	}

	// 代理密码必须脱敏
	for _, e := range d.Egress {
		if strings.Contains(e.Proxy, "secret") {
			t.Errorf("proxy password leaked: %s", e.Proxy)
		}
		if !strings.Contains(e.Proxy, "***") {
			t.Errorf("proxy not redacted: %s", e.Proxy)
		}
	}

	// 身份只暴露前缀 + 结论
	if d.Identity[0].Account != "a1b2c3d4" || !d.Identity[0].Drift {
		t.Errorf("identity = %+v", d.Identity[0])
	}
}

func TestHandleRiskDiagnosticsNilSafe(t *testing.T) {
	// svc 为 nil（测试环境）→ 返回结构完整的空报告，不 panic
	req := httptest.NewRequest(http.MethodGet, "/api/diagnostics/risk", nil)
	rec := httptest.NewRecorder()
	handleRiskDiagnostics(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var d riskDiagnostics
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if d.Accounts == nil || d.Identity == nil || d.Egress == nil || d.Hints == nil {
		t.Error("nil slices in response (want empty arrays)")
	}
}

// 体检报告要暴露端点健康、区域出口与需重新授权状态。
func TestRiskDiagnosticsExposesEndpointsAndReauth(t *testing.T) {
	slots := []bridge.SlotStatus{{
		ID: "a1b2c3d4", Region: "global",
		BreakerFor: 0, NeedsReauth: true,
	}}
	sum := bridge.PoolSummary{Total: 1}
	accounts := []account.Account{{ID: "a1b2c3d4"}}

	d := buildRiskDiagnostics(slots, sum, accounts,
		bridge.RiskStats{}, bridge.ThrottleStats{}, bridge.AffinityStats{}, nil)

	if !d.Accounts[0].NeedsReauth {
		t.Error("needs_reauth should be exposed on the account card")
	}
	if d.Accounts[0].Healthy {
		t.Error("account needing re-auth must not be healthy")
	}
	joined := strings.Join(d.Hints, "\n")
	if !strings.Contains(joined, "重新授权") {
		t.Errorf("hints should mention re-auth:\n%s", joined)
	}
	// 结构完整（新增字段不能是 nil 之外的不一致状态）
	if d.CurrentEndpoints == nil {
		t.Error("current_endpoints should be present (may be empty map)")
	}
}

// TestDiagnosticsExposesProxyConfigErr 审计 P1-1：代理配置非法的账号必须
// 在诊断里高亮 + 给出可操作修法（socks5 → 本地 http 转发层），否则
// 「账号不参与选号」在面板上完全看不出原因。
func TestDiagnosticsExposesProxyConfigErr(t *testing.T) {
	slots := []bridge.SlotStatus{{
		ID: "a1b2c3d4", Region: "cn", CoolingRemainingMS: 0,
		ProxyConfigErr: `account acct-1: unsupported proxy scheme "socks5"`,
	}}
	sum := bridge.PoolSummary{Total: 1, ReadyCount: 0}
	accounts := []account.Account{{ID: "a1b2c3d4", Region: account.RegionCN}}

	d := buildRiskDiagnostics(slots, sum, accounts,
		bridge.RiskStats{}, bridge.ThrottleStats{}, bridge.AffinityStats{}, nil)

	if d.Accounts[0].ProxyConfigErr == "" {
		t.Error("account card must carry the proxy config error")
	}
	joined := strings.Join(d.Hints, "\n")
	if !strings.Contains(joined, "socks5") || !strings.Contains(joined, "绝不静默直连") {
		t.Errorf("proxy config error hint missing or weak:\n%s", joined)
	}
}

// TestDiagnosticsExposesSoftAllow 审计 P2-1：闸门软放行计数必须可见——
// 它是「过载程度」的直接度量，运维看不到就无法提前扩容。
func TestDiagnosticsExposesSoftAllow(t *testing.T) {
	slots := []bridge.SlotStatus{{ID: "a1b2c3d4", Region: "cn"}}
	sum := bridge.PoolSummary{Total: 1, ReadyCount: 1}
	accounts := []account.Account{{ID: "a1b2c3d4", Region: account.RegionCN}}

	d := buildRiskDiagnostics(slots, sum, accounts,
		bridge.RiskStats{}, bridge.ThrottleStats{}, bridge.AffinityStats{}, nil)

	if d.GuardSoftAllowUsed != 0 || d.GuardSoftAllowCap <= 0 {
		t.Errorf("soft allow snapshot = (%d, %d), want (0, >0)", d.GuardSoftAllowUsed, d.GuardSoftAllowCap)
	}
	// 计数为 0 时不产生软放行告警（噪声）
	for _, h := range d.Hints {
		if strings.Contains(h, "软放行") {
			t.Errorf("unexpected soft-allow hint at zero usage: %s", h)
		}
	}
}
