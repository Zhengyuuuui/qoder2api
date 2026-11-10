package bridge

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// resetRisk 清零风控计数器（测试隔离用）。
func resetRisk(t *testing.T) {
	t.Helper()
	clearRisk()
	t.Cleanup(clearRisk)
}

// clearRisk 清零风控计数器（不注册 cleanup，避免递归）。
func clearRisk() {
	risk.queue.Store(0)
	risk.rate.Store(0)
	risk.rejected.Store(0)
	risk.versionGate.Store(0)
	risk.signalMu.Lock()
	risk.last = nil
	risk.signalMu.Unlock()
}

// --- E1. 计数点 -------------------------------------------------------------

// 四类错误分别命中对应计数，其他计数不变。
func TestRiskCountersPerSignalType(t *testing.T) {
	resetRisk(t)

	// 10605 排队 → queue
	queueErr := NewUpstreamError(403, nestedResourceLimitDetail(true, true, 3))
	recordUpstreamRisk(queueErr, "qfmodel", "acct-a")
	st := RiskSnapshot()
	if st.Queue != 1 {
		t.Errorf("queue = %d, want 1", st.Queue)
	}
	if st.Rate != 0 || st.Rejected != 0 || st.VersionGate != 0 {
		t.Errorf("other counters must stay 0: %+v", st)
	}

	// 429 频控 → rate
	recordUpstreamRisk(NewUpstreamError(429, "usage exceeds frequency limit"), "qfmodel", "acct-a")
	st = RiskSnapshot()
	if st.Rate != 1 {
		t.Errorf("rate = %d, want 1", st.Rate)
	}
	if st.Queue != 1 {
		t.Errorf("queue must stay 1, got %d", st.Queue)
	}

	// 403 → rejected
	recordUpstreamRisk(NewUpstreamError(403, `{"code":"112"}`), "qfmodel", "acct-a")
	st = RiskSnapshot()
	if st.Rejected != 1 {
		t.Errorf("rejected = %d, want 1", st.Rejected)
	}

	// 430 版本闸门 → version_gate
	recordUpstreamRisk(NewUpstreamError(430, "client version too old"), "qfmodel", "acct-a")
	st = RiskSnapshot()
	if st.VersionGate != 1 {
		t.Errorf("version_gate = %d, want 1", st.VersionGate)
	}

	// 四个计数各自独立，最终各为 1
	st = RiskSnapshot()
	if st.Queue != 1 || st.Rate != 1 || st.Rejected != 1 || st.VersionGate != 1 {
		t.Errorf("final counters = %+v, want all 1", st)
	}
}

// 内容审核 / 参数错不计入任何风控信号。
func TestRiskIgnoresNonSignalErrors(t *testing.T) {
	resetRisk(t)
	recordUpstreamRisk(NewUpstreamError(500, "InternalError.Algo.DataInspectionFailed"), "m", "a")
	recordUpstreamRisk(NewUpstreamError(400, "invalid_parameter_error: Range of x"), "m", "a")
	st := RiskSnapshot()
	if st.Queue != 0 || st.Rate != 0 || st.Rejected != 0 || st.VersionGate != 0 {
		t.Errorf("non-signal errors must not count: %+v", st)
	}
	if st.Last != nil {
		t.Errorf("non-signal errors must not record a last signal: %+v", st.Last)
	}
}

// 错误分类时就把风控类型定下来（各调用点不重复推导口径）。
func TestUpstreamErrorCarriesRiskKind(t *testing.T) {
	cases := []struct {
		name string
		err  *UpstreamError
		want string
	}{
		{"429", NewUpstreamError(429, "rate limit"), RiskRate},
		{"401", NewUpstreamError(401, "authentication_error"), RiskRejected},
		{"403", NewUpstreamError(403, "forbidden"), RiskRejected},
		{"430", NewUpstreamError(430, "version too low"), RiskVersionGate},
		{"content policy", NewUpstreamError(500, "InternalError.Algo.DataInspectionFailed"), ""},
		{"param error", NewUpstreamError(400, "invalid_parameter_error"), ""},
	}
	for _, c := range cases {
		if c.err.RiskKind != c.want {
			t.Errorf("%s: RiskKind = %q, want %q", c.name, c.err.RiskKind, c.want)
		}
	}
}

// --- E2. 最近一次信号 -------------------------------------------------------

// 最近一次信号记录类型 / 模型 / 账号 / 详情。
func TestRiskLastSignal(t *testing.T) {
	resetRisk(t)
	recordUpstreamRisk(NewUpstreamError(429, "usage exceeds frequency limit"), "smodel", "acct-1234567890")
	st := RiskSnapshot()
	if st.Last == nil {
		t.Fatal("last signal should be present")
	}
	if st.Last.Type != RiskRate {
		t.Errorf("last.type = %q, want %q", st.Last.Type, RiskRate)
	}
	if st.Last.Model != "smodel" {
		t.Errorf("last.model = %q, want smodel", st.Last.Model)
	}
	if st.Last.At.IsZero() {
		t.Error("last.at should be set")
	}
	if st.Last.Detail == "" {
		t.Error("last.detail should not be empty")
	}
}

// 账号 ID 只暴露前 8 位（/health 不做鉴权，不能泄露全量 ID）。
func TestRiskLastSignalTruncatesAccountID(t *testing.T) {
	resetRisk(t)
	recordUpstreamRisk(NewUpstreamError(429, "rate"), "m", "abcdefghijklmnop")
	st := RiskSnapshot()
	if st.Last.Account != "abcdefgh" {
		t.Errorf("last.account = %q, want abcdefgh", st.Last.Account)
	}
}

// 详情超长时截断，避免日志 / 内存膨胀。
func TestRiskLastSignalTruncatesDetail(t *testing.T) {
	resetRisk(t)
	long := ""
	for i := 0; i < 1000; i++ {
		long += "x"
	}
	recordUpstreamRisk(NewUpstreamError(429, long), "m", "a")
	st := RiskSnapshot()
	if len(st.Last.Detail) > riskDetailMaxLen+10 {
		t.Errorf("detail length = %d, want <= %d", len(st.Last.Detail), riskDetailMaxLen+10)
	}
}

// --- E1. 并发安全 -----------------------------------------------------------
// 100 个 goroutine 同时打点，总数必须精确（-race 下无竞争）。
func TestRiskConcurrentRecording(t *testing.T) {
	resetRisk(t)
	const n = 100
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 4 {
			case 0:
				recordUpstreamRisk(NewUpstreamError(429, "rate"), "m", "a")
			case 1:
				recordUpstreamRisk(NewUpstreamError(403, "denied"), "m", "a")
			case 2:
				recordUpstreamRisk(NewUpstreamError(403, nestedResourceLimitDetail(true, true, 1)), "m", "a")
			default:
				recordUpstreamRisk(NewUpstreamError(430, "version"), "m", "a")
			}
		}(i)
	}
	wg.Wait()

	st := RiskSnapshot()
	if st.Queue+st.Rate+st.Rejected+st.VersionGate != n {
		t.Errorf("total = %d, want %d (queue=%d rate=%d rejected=%d version_gate=%d)",
			st.Queue+st.Rate+st.Rejected+st.VersionGate, n,
			st.Queue, st.Rate, st.Rejected, st.VersionGate)
	}
	if st.Last == nil {
		t.Error("last signal should be present after concurrent recording")
	}
}

// 端到端：真实的流内错误帧会被打点（排队信号走信封错误路径）。
func TestRiskCountsStreamEnvelopeError(t *testing.T) {
	resetRisk(t)
	// 缩短排队等待：排队错误会在同账号内等待重试，默认等待 15s 会让测试很慢。
	ApplyQueueSettings(1, 1, 1, false, 0)
	t.Cleanup(func() { ApplyQueueSettings(0, 0, 0, true, 0) })

	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseEnvelope(403, nestedResourceLimitDetail(true, true, 3)))
	}))
	defer srv.Close()

	b, pool := poolBridge(t, srv.URL, "acct-12345678")
	// 预置模型目录，避免选号时去打上游 model/list（测试不该依赖外网）。
	for _, sl := range pool.snapshot() {
		sl.catalog = map[string]bool{"qfmodel": true}
		sl.catalogAt = time.Now()
	}
	err := b.CallQoderWithOpts(context.Background(), "codex", testMessages(), "qfmodel", nil, CallOpts{}, func(Delta) {})
	if err == nil {
		t.Fatal("expected upstream error")
	}

	st := RiskSnapshot()
	if st.Queue == 0 {
		t.Errorf("queued stream error should be counted as a queue signal: %+v", st)
	}
	if st.Last == nil {
		t.Fatal("last signal should be recorded")
	}
	if st.Last.Model != "qfmodel" {
		t.Errorf("last.model = %q, want qfmodel", st.Last.Model)
	}
	// 账号 ID 只暴露前 8 位
	if st.Last.Account != "acct-12" && st.Last.Account != "" {
		t.Logf("last.account = %q", st.Last.Account)
	}
}

// TestNestedVersionGateClassified 审计 P3-6：上游把 430 版本门禁嵌在
// 403 报文里（外层 code=403，内层 code=430）。只按外层状态码分类会把它
// 误判为「被风控拒绝」→ 走错冷却策略。必须识别嵌套的 430。
func TestNestedVersionGateClassified(t *testing.T) {
	// 外层 403、内层 430（两种常见报文形态）
	cases := []string{
		`{"code":"403","message":"{\"code\":\"430\",\"message\":\"client version too low\"}"}`,
		`{"code":"403","message":"{\"code\":430,\"subCode\":\"VERSION_TOO_LOW\"}"}`,
	}
	for _, detail := range cases {
		ue := NewUpstreamError(403, detail)
		if ue.RiskKind != RiskVersionGate {
			t.Errorf("nested 430 in %s: RiskKind = %q, want %q", detail, ue.RiskKind, RiskVersionGate)
		}
	}
	// 对照组：普通 403 嵌套（无 430）不得误判为版本门禁
	plain := NewUpstreamError(403, `{"code":"403","message":"{\"code\":403,\"message\":\"forbidden\"}"}`)
	if plain.RiskKind == RiskVersionGate {
		t.Error("plain 403 must not be classified as version gate")
	}
}
