package bridge

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"qoder2api/account"
)

// TestProxyConfigErrSlotUnselectable 审计 P1-1 集成回归：代理配置非法的
// 账号必须（a）被标记 ConfigErr、（b）不参与选号、（c）**没有任何连接
// 到达上游**（绝不静默直连）。旧实现返回空串 = 直连，真实出口直接暴露。
func TestProxyConfigErrSlotUnselectable(t *testing.T) {
	guardSettings(t, account.Settings{TrafficGuard: boolPtr(true), MinRequestGapMs: 1})

	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseOKFrame("done"))
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	slot := testSlot(t, "acct-badproxy", "personal_standard")
	slot.bindProxy("acct-badproxy", "socks5://127.0.0.1:1080", "cn")

	// (a) 配置错误被显式标记
	if slot.ProxyConfigErr() == "" {
		t.Fatal("socks5 account proxy must mark a proxy config error")
	}
	// (b) 选号路径直接跳过
	p := NewPool(slot)
	if _, ok := p.Pick(nil); ok {
		t.Error("slot with an invalid proxy config must not be selectable")
	}

	// (c) 走完整调用路径：不得产生任何上游连接
	b := NewPoolBridgeFor(account.RegionCN, p, map[string]interface{}{
		"model_config": map[string]interface{}{"key": "auto"},
		"messages":     []interface{}{},
	})
	b.chatStreamURL = srv.URL
	// 预置模型目录，避免选号时打外网 model/list
	for _, sl := range p.snapshot() {
		sl.catalog = map[string]bool{"qfmodel": true}
		sl.catalogAt = time.Now()
	}
	err := b.CallQoderWithOpts(context.Background(), "codex", testMessages(), "qfmodel", nil, CallOpts{}, func(Delta) {})
	if err == nil {
		t.Fatal("call with only a misconfigured-proxy account must fail")
	}
	if hits != 0 {
		t.Errorf("upstream hits = %d, want 0 (never silently go direct)", hits)
	}
	// 诊断可见性：错误信息应指向代理配置，而不是让运维以为是上游故障
	st := slot.Status()
	if st.ProxyConfigErr == "" {
		t.Error("SlotStatus must surface the proxy config error for diagnostics")
	}
}

// TestProxyConfigErrDoesNotBlockHealthyAccounts 一个账号配错不拖垮整个池：
// 其余账号照常选号、照常成功。
func TestProxyConfigErrDoesNotBlockHealthyAccounts(t *testing.T) {
	guardSettings(t, account.Settings{TrafficGuard: boolPtr(true), MinRequestGapMs: 1})

	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseOKFrame("done"))
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	bad := testSlot(t, "acct-bad2", "personal_standard")
	bad.bindProxy("acct-bad2", "socks5://127.0.0.1:1080", "cn")
	good := testSlot(t, "acct-good2", "personal_standard")
	good.bindProxy("acct-good2", "", "cn") // 直连（合法）

	p := NewPool(bad, good)
	for _, sl := range p.snapshot() {
		sl.catalog = map[string]bool{"qfmodel": true}
		sl.catalogAt = time.Now()
	}
	s, ok := p.Pick(nil)
	if !ok || s.ID != "acct-good2" {
		t.Fatalf("pick = %v, want the healthy account acct-good2", s)
	}
}

// TestUnconfiguredProxyStillDirect 未配置代理 → 直连（合法路径不受
// P1-1 收紧影响）：请求正常到达上游。
func TestUnconfiguredProxyStillDirect(t *testing.T) {
	guardSettings(t, account.Settings{TrafficGuard: boolPtr(true), MinRequestGapMs: 1})

	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseOKFrame("done"))
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	slot := testSlot(t, "acct-direct2", "personal_standard")
	slot.bindProxy("acct-direct2", "", "cn")
	if slot.ProxyConfigErr() != "" {
		t.Fatalf("unconfigured proxy must not be an error: %q", slot.ProxyConfigErr())
	}

	b, pool := poolBridge(t, srv.URL, "acct-direct2")
	_ = b
	for _, sl := range pool.snapshot() {
		sl.catalog = map[string]bool{"qfmodel": true}
		sl.catalogAt = time.Now()
	}
	if err := b.CallQoderWithOpts(context.Background(), "codex", testMessages(), "qfmodel", nil, CallOpts{}, func(Delta) {}); err != nil {
		t.Fatalf("direct account call: %v", err)
	}
	if hits == 0 {
		t.Error("direct (unconfigured proxy) requests must still reach upstream")
	}
}
