package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"qoder2api/account"
)

func TestSessionKeyStableAcrossVolatileParts(t *testing.T) {
	// 同一会话的连续两轮：时间戳 / 路径 / 行号 / epoch 都不同，
	// 归一化后必须算出同一个键——否则亲和永远命中不了。
	msgs1 := []interface{}{map[string]interface{}{
		"role": "user", "content": "Fix the crash in /src/app/main.go:142 reported at 2026-11-09T10:00:00, ts=1731146400",
	}}
	msgs2 := []interface{}{map[string]interface{}{
		"role": "user", "content": "Fix the crash in /src/app/other.go:57 reported at 2026-11-09T15:30:00, ts=1731160800",
	}}
	k1 := ExtractSessionKey("", msgs1)
	k2 := ExtractSessionKey("", msgs2)
	if k1 == "" || k1 != k2 {
		t.Errorf("volatile parts changed session key: %q vs %q", k1, k2)
	}

	// 不同会话 → 不同键
	msgs3 := []interface{}{map[string]interface{}{
		"role": "user", "content": "Explain quantum computing basics",
	}}
	if ExtractSessionKey("", msgs3) == k1 {
		t.Error("different sessions produced the same key")
	}

	// 显式键优先；无 user 消息不派生（空键会把不同会话绑到一起，必须禁止）
	if got := ExtractSessionKey("sess-123", nil); got != "k:sess-123" {
		t.Errorf("explicit key = %q, want k:sess-123", got)
	}
	if got := ExtractSessionKey("", []interface{}{map[string]interface{}{"role": "system", "content": "x"}}); got != "" {
		t.Errorf("no user message key = %q, want empty", got)
	}
}

func TestAffinityBindAndTTL(t *testing.T) {
	guardSettings(t, account.Settings{SessionAffinity: boolPtr(true), SessionAffinityTTLSec: 1, MinRequestGapMs: 1})
	AffinityBind("ttl-key", "acct-a")
	if got := AffinityAccount("ttl-key"); got != "acct-a" {
		t.Fatalf("bound account = %q, want acct-a", got)
	}
	time.Sleep(1200 * time.Millisecond)
	if got := AffinityAccount("ttl-key"); got != "" {
		t.Errorf("account after TTL = %q, want empty (expired)", got)
	}
	// 关闭亲和：查询直接返回空（不建绑定）
	guardSettings(t, account.Settings{SessionAffinity: boolPtr(false)})
	AffinityBind("off-key", "acct-b")
	if got := AffinityAccount("off-key"); got != "" {
		t.Errorf("affinity disabled but got %q", got)
	}
}

func TestAffinityCapacityBounded(t *testing.T) {
	guardSettings(t, account.Settings{SessionAffinity: boolPtr(true), SessionAffinityTTLSec: 3600})
	for i := 0; i < affinityMaxEntries+50; i++ {
		AffinityBind(fmt.Sprintf("cap-%d", i), fmt.Sprintf("acct-%d", i))
	}
	if n := AffinitySnapshot().Bound; n > affinityMaxEntries {
		t.Errorf("bound = %d, want <= %d (LRU eviction)", n, affinityMaxEntries)
	}
}

// sseOKFrame 正常内容帧。
func sseOKFrame(text string) string {
	inner := fmt.Sprintf(`{"choices":[{"delta":{"content":%q}}]}`, text)
	return fmt.Sprintf("data: {\"body\":%q,\"statusCodeValue\":200}\n\n", inner)
}

// queueEnvelopeFrame 上游排队错误帧（三层嵌套 RESOURCE_LIMIT + isQueued）。
func queueEnvelopeFrame(retryAfter int) string {
	inner, _ := json.Marshal(map[string]interface{}{
		"isQueued": true, "retryAfterSeconds": retryAfter, "modelKey": "auto", "subCode": "10605",
	})
	mid, _ := json.Marshal(map[string]interface{}{
		"code": "10605", "message": string(inner), "subCode": "RESOURCE_LIMIT",
	})
	outer, _ := json.Marshal(map[string]interface{}{"code": "403", "message": string(mid)})
	return fmt.Sprintf("data: {\"body\":%q,\"statusCodeValue\":403}\n\n", string(outer))
}

// newAffinityBridge 构造指向测试服务器的双账号池桥。
func newAffinityBridge(t *testing.T, srvURL string, ids ...string) *Bridge {
	t.Helper()
	slots := make([]*Slot, 0, len(ids))
	for _, id := range ids {
		slots = append(slots, testSlot(t, id, "personal_standard"))
	}
	pool := NewPool(slots...)
	b := NewPoolBridgeFor(account.RegionCN, pool, map[string]interface{}{
		"model_config": map[string]interface{}{"key": "auto"},
		"messages":     []interface{}{},
	})
	b.chatStreamURL = srvURL
	return b
}

func TestCallQoderAffinitySticksToAccount(t *testing.T) {
	guardSettings(t, account.Settings{SessionAffinity: boolPtr(true), SessionAffinityTTLSec: 600, MinRequestGapMs: 1})
	var mu sync.Mutex
	var users []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		users = append(users, r.Header.Get("cosy-user"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseOKFrame("hi"))
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	b := newAffinityBridge(t, srv.URL, "aff-a", "aff-b")
	msgs := []interface{}{map[string]interface{}{"role": "user", "content": "hello sticky session"}}
	for i := 0; i < 3; i++ {
		if err := b.CallQoderWithOpts(context.Background(), "codex", msgs, "auto", nil, CallOpts{}, func(Delta) {}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(users) != 3 {
		t.Fatalf("upstream hits = %d, want 3", len(users))
	}
	if users[0] == "" || users[0] != users[1] || users[1] != users[2] {
		t.Errorf("session affinity broken: upstream accounts = %v", users)
	}
}

func TestCallQoderAffinitySwitchesWhenCooled(t *testing.T) {
	guardSettings(t, account.Settings{SessionAffinity: boolPtr(true), SessionAffinityTTLSec: 600, MinRequestGapMs: 1})
	var mu sync.Mutex
	var users []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		users = append(users, r.Header.Get("cosy-user"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseOKFrame("hi"))
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	b := newAffinityBridge(t, srv.URL, "cool-a", "cool-b")
	msgs := []interface{}{map[string]interface{}{"role": "user", "content": "session that will switch"}}

	// 第一轮：建立绑定
	if err := b.CallQoderWithOpts(context.Background(), "codex", msgs, "auto", nil, CallOpts{}, func(Delta) {}); err != nil {
		t.Fatalf("first call: %v", err)
	}
	key := ExtractSessionKey("", msgs)
	first := AffinityAccount(key)
	if first == "" {
		t.Fatal("no affinity binding after first call")
	}

	// 绑定账号被冷却 → 换号并重绑
	b.pool.Cool(first, time.Minute, "test")
	if err := b.CallQoderWithOpts(context.Background(), "codex", msgs, "auto", nil, CallOpts{}, func(Delta) {}); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got := AffinityAccount(key); got == "" || got == first {
		t.Errorf("binding after switch = %q, want a different account (was %q)", got, first)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(users) == 2 && users[0] == users[1] {
		t.Errorf("request still routed to cooled account: %v", users)
	}
}

func TestCallQoderAffinityKeptDuringQueueRetry(t *testing.T) {
	guardSettings(t, account.Settings{
		SessionAffinity: boolPtr(true), SessionAffinityTTLSec: 600, MinRequestGapMs: 1,
		QueueWaitFirstToken: boolPtr(false), QueueMaxRetries: 2, QueueWaitDefaultSec: 1,
	})
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/event-stream")
		if hits == 1 {
			// 首次：排队（同账号等待重试，不换号）
			fmt.Fprint(w, queueEnvelopeFrame(1))
			return
		}
		fmt.Fprint(w, sseOKFrame("done"))
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	// 单账号池：若排队期间解绑亲和/换号，这里必然失败
	b := newAffinityBridge(t, srv.URL, "queue-only")
	msgs := []interface{}{map[string]interface{}{"role": "user", "content": "queued session"}}
	if err := b.CallQoderWithOpts(context.Background(), "codex", msgs, "auto", nil, CallOpts{}, func(Delta) {}); err != nil {
		t.Fatalf("call with queue retry: %v", err)
	}
	if hits != 2 {
		t.Errorf("upstream hits = %d, want 2 (same-account queue retry)", hits)
	}
	key := ExtractSessionKey("", msgs)
	if got := AffinityAccount(key); got != "queue-only" {
		t.Errorf("binding after queue retry = %q, want queue-only (affinity must survive queueing)", got)
	}
}

func boolPtr(b bool) *bool { return &b }
