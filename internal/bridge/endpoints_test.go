package bridge

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"qoder2api/account"
)

// resetEndpointHealth 清空端点健康登记表（测试隔离用）。
func resetEndpointHealth(t *testing.T) {
	t.Helper()
	reg := endpoints()
	reg.mu.Lock()
	reg.entries = map[string]*endpointHealth{}
	reg.mu.Unlock()
	t.Cleanup(func() {
		reg.mu.Lock()
		reg.entries = map[string]*endpointHealth{}
		reg.mu.Unlock()
	})
}

// --- K1. 候选列表 -----------------------------------------------------------

// 国际版有多个推理端点候选；国内版只有单一 gateway。
func TestEndpointCandidatesPerRegion(t *testing.T) {
	global := account.GetEndpoints(account.RegionGlobal)
	if len(global.ChatStreamCandidates()) < 2 {
		t.Errorf("global should have multiple chat candidates, got %d",
			len(global.ChatStreamCandidates()))
	}
	if len(global.ModelListCandidates()) < 2 {
		t.Errorf("global should have multiple model list candidates, got %d",
			len(global.ModelListCandidates()))
	}
	cn := account.GetEndpoints(account.RegionCN)
	if len(cn.ChatStreamCandidates()) != 1 {
		t.Errorf("cn should have a single chat candidate, got %d",
			len(cn.ChatStreamCandidates()))
	}
	// 单值字段与候选首项一致（向后兼容）
	if global.ChatStreamURL != global.ChatStreamCandidates()[0] {
		t.Error("ChatStreamURL must equal the first candidate")
	}
	if global.ModelListURL != global.ModelListCandidates()[0] {
		t.Error("ModelListURL must equal the first candidate")
	}
}

// --- K2. 故障转移 -----------------------------------------------------------

// 首选端点传输失败 → 自动切到备选并成功。
func TestEndpointFailoverOnTransportError(t *testing.T) {
	resetEndpointHealth(t)

	var backupHits int32
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&backupHits, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseContent("ok"))
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer backup.Close()

	// 首选端点指向一个没人监听的端口 → 传输层失败
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	c := testClient(t)
	var lines []string
	err := c.openStreamCandidates(context.Background(), "chat_stream",
		[]string{deadURL, backup.URL},
		map[string]interface{}{"x": 1}, nil,
		func(l string) bool { lines = append(lines, l); return true })
	if err != nil {
		t.Fatalf("expected failover to succeed, got %v", err)
	}
	if atomic.LoadInt32(&backupHits) == 0 {
		t.Error("backup endpoint was not used")
	}
	if len(lines) == 0 {
		t.Error("no content received from backup endpoint")
	}
}

// 首选返回 401 → 不换端点（只请求首选一次）。
func TestEndpointNoFailoverOn401(t *testing.T) {
	resetEndpointHealth(t)

	var primaryHits, backupHits int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&primaryHits, 1)
		w.WriteHeader(401)
		fmt.Fprint(w, "authentication_error")
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&backupHits, 1)
		w.WriteHeader(200)
	}))
	defer backup.Close()

	c := testClient(t)
	err := c.openStreamCandidates(context.Background(), "chat_stream",
		[]string{primary.URL, backup.URL},
		map[string]interface{}{"x": 1}, nil, func(string) bool { return true })
	if err == nil {
		t.Fatal("expected 401 error")
	}
	if atomic.LoadInt32(&primaryHits) != 1 {
		t.Errorf("primary hits = %d, want 1", atomic.LoadInt32(&primaryHits))
	}
	if atomic.LoadInt32(&backupHits) != 0 {
		t.Errorf("backup must not be tried on 4xx, got %d hits", atomic.LoadInt32(&backupHits))
	}
}

// 首选返回 503 → 换端点。
func TestEndpointFailoverOn5xx(t *testing.T) {
	resetEndpointHealth(t)

	var primaryHits, backupHits int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&primaryHits, 1)
		w.WriteHeader(503)
		fmt.Fprint(w, "provider_error")
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&backupHits, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseContent("ok"))
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer backup.Close()

	c := testClient(t)
	err := c.openStreamCandidates(context.Background(), "chat_stream",
		[]string{primary.URL, backup.URL},
		map[string]interface{}{"x": 1}, nil, func(string) bool { return true })
	if err != nil {
		t.Fatalf("expected failover to succeed, got %v", err)
	}
	if atomic.LoadInt32(&backupHits) == 0 {
		t.Error("backup endpoint was not used after 503")
	}
}

// --- K3. 端点健康记忆 -------------------------------------------------------

// 端点连续失败 3 次 → 进入冷却，后续请求跳过它。
func TestEndpointCooldownAfterConsecutiveFailures(t *testing.T) {
	resetEndpointHealth(t)
	oldThreshold, oldCooldown := EndpointFailThreshold, EndpointCooldown
	EndpointFailThreshold, EndpointCooldown = 3, 5*time.Minute
	t.Cleanup(func() { EndpointFailThreshold, EndpointCooldown = oldThreshold, oldCooldown })

	var primaryHits int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&primaryHits, 1)
		w.WriteHeader(503)
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseContent("ok"))
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer backup.Close()

	c := testClient(t)
	candidates := []string{primary.URL, backup.URL}
	for i := 0; i < 3; i++ {
		_ = c.openStreamCandidates(context.Background(), "chat_stream", candidates,
			map[string]interface{}{"x": 1}, nil, func(string) bool { return true })
	}
	if !endpoints().EndpointCooling("chat_stream", primary.URL) {
		t.Fatal("primary endpoint should be in cooldown after 3 consecutive failures")
	}
	hitsBefore := atomic.LoadInt32(&primaryHits)

	// 冷却期内：应优先用健康的备用端点，首选被跳过
	_ = c.openStreamCandidates(context.Background(), "chat_stream", candidates,
		map[string]interface{}{"x": 1}, nil, func(string) bool { return true })
	if atomic.LoadInt32(&primaryHits) != hitsBefore {
		t.Errorf("cooling endpoint should be skipped, hits went %d -> %d",
			hitsBefore, atomic.LoadInt32(&primaryHits))
	}
}

// 冷却窗口过期 → 重新尝试首选端点。
func TestEndpointCooldownExpires(t *testing.T) {
	resetEndpointHealth(t)
	oldThreshold, oldCooldown := EndpointFailThreshold, EndpointCooldown
	EndpointFailThreshold, EndpointCooldown = 3, 50*time.Millisecond
	t.Cleanup(func() { EndpointFailThreshold, EndpointCooldown = oldThreshold, oldCooldown })

	var primaryHits int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&primaryHits, 1)
		w.WriteHeader(503)
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseContent("ok"))
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer backup.Close()

	c := testClient(t)
	candidates := []string{primary.URL, backup.URL}
	for i := 0; i < 3; i++ {
		_ = c.openStreamCandidates(context.Background(), "chat_stream", candidates,
			map[string]interface{}{"x": 1}, nil, func(string) bool { return true })
	}
	if !endpoints().EndpointCooling("chat_stream", primary.URL) {
		t.Fatal("primary should be cooling")
	}
	// 窗口过期
	time.Sleep(80 * time.Millisecond)
	if endpoints().EndpointCooling("chat_stream", primary.URL) {
		t.Fatal("cooldown should have expired")
	}
	hitsBefore := atomic.LoadInt32(&primaryHits)
	_ = c.openStreamCandidates(context.Background(), "chat_stream", candidates,
		map[string]interface{}{"x": 1}, nil, func(string) bool { return true })
	if atomic.LoadInt32(&primaryHits) <= hitsBefore {
		t.Error("expired cooldown should let the primary be retried")
	}
}

// 端点成功后失败计数清零。
func TestEndpointSuccessResetsFailureCount(t *testing.T) {
	resetEndpointHealth(t)
	reg := endpoints()
	reg.NoteEndpointFailure("chat_stream", "http://x/1", fmt.Errorf("boom"))
	reg.NoteEndpointFailure("chat_stream", "http://x/1", fmt.Errorf("boom"))
	reg.NoteEndpointSuccess("chat_stream", "http://x/1")

	st := reg.EndpointStatuses()
	for _, s := range st {
		if s.URL == "http://x/1" {
			if s.Fails != 0 {
				t.Errorf("fails = %d, want 0 after success", s.Fails)
			}
			if s.Successes != 1 {
				t.Errorf("successes = %d, want 1", s.Successes)
			}
			return
		}
	}
	t.Fatal("endpoint status not found")
}

// 端点健康快照可在诊断接口看到。
func TestEndpointStatusesExposed(t *testing.T) {
	resetEndpointHealth(t)
	endpoints().NoteEndpointFailure("chat_stream", "http://x/1", fmt.Errorf("boom"))
	st := EndpointStatuses()
	if len(st) == 0 {
		t.Fatal("endpoint statuses should not be empty")
	}
	found := false
	for _, s := range st {
		if s.URL == "http://x/1" {
			found = true
			if s.Purpose != "chat_stream" {
				t.Errorf("purpose = %q, want chat_stream", s.Purpose)
			}
			if s.Fails != 1 {
				t.Errorf("fails = %d, want 1", s.Fails)
			}
		}
	}
	if !found {
		t.Error("failed endpoint should appear in statuses")
	}
}

// 当前生效端点可查（诊断用）：故障转移后应指向实际在用的那个。
func TestCurrentEndpointsReflectsActiveEndpoint(t *testing.T) {
	resetEndpointHealth(t)
	reg := endpoints()

	reg.NoteEndpointSuccess("chat_stream", "http://a/stream")
	reg.NoteEndpointFailure("chat_stream", "http://a/stream", fmt.Errorf("boom"))
	reg.NoteEndpointSuccess("chat_stream", "http://b/stream")

	cur := CurrentEndpoints()
	if cur["chat_stream"] != "http://b/stream" {
		t.Errorf("current chat_stream endpoint = %q, want http://b/stream", cur["chat_stream"])
	}
}
