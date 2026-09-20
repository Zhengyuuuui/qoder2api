package bridge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"qoder2api/account"
	"qoder2api/internal/cosy"
)

// envelopeBridge 构造指向测试服务器的 Bridge（隔离 settings，避免读写真实配置）。
func envelopeBridge(t *testing.T, streamURL string) *Bridge {
	t.Helper()
	account.SetDataRoot(t.TempDir())
	sess, err := cosy.NewSession(
		cosy.AuthIdentity{Uid: "env-test-uid", UserType: "personal_standard"},
		"mid-env-test-0000000000000000000000",
		"mtoken-env-test",
		"mtype-env-0000000",
	)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return &Bridge{
		sess:   sess,
		client: NewBearerClient(sess),
		region: account.RegionCN,
		templateBase: map[string]interface{}{
			"model_config": map[string]interface{}{"key": "auto"},
			"messages":     []interface{}{},
		},
		chatStreamURL: streamURL,
	}
}

func testMessages() []interface{} {
	return []interface{}{
		map[string]interface{}{"role": "user", "content": "hi"},
	}
}

// sseEnvelope 生成 hub iter_inner_sse 形态的信封帧。
func sseEnvelope(status int, detail string) string {
	return fmt.Sprintf("data: {\"body\":%q,\"statusCodeValue\":%d}\n\n", detail, status)
}

// sseContent 生成正常内容帧（statusCodeValue=200）。
func sseContent(text string) string {
	inner := fmt.Sprintf(`{"choices":[{"delta":{"content":%q}}]}`, text)
	return fmt.Sprintf("data: {\"body\":%q,\"statusCodeValue\":200}\n\n", inner)
}

// envelopePayload 生成信封 JSON 载荷（不含 "data: " 前缀——ExtractDelta
// 接收的是分发层剥掉前缀后的 payload）。
func envelopePayload(status int, detail string) string {
	return fmt.Sprintf("{\"body\":%q,\"statusCodeValue\":%d}", detail, status)
}

// --- delta 信封解析单元测试 ---

func TestExtractDeltaEnvelopeStatus(t *testing.T) {
	// 信封 418：结构化错误，带真实 HTTP 状态，可参与瞬时重试判定
	d := ExtractDelta(envelopePayload(418, "provider_error: upstream broke"))
	ue := upstreamErrFrom(t, d.Err)
	if ue.Status != 418 || ue.ErrType != ErrTypeTransient {
		t.Errorf("envelope 418 -> %+v", ue)
	}

	// 字符串状态 "200"：正常解析内层内容
	inner := `{"choices":[{"delta":{"content":"hello"}}]}`
	d = ExtractDelta(fmt.Sprintf("{\"body\":%q,\"statusCodeValue\":\"200\"}", inner))
	if d.Err != nil || d.Content != "hello" {
		t.Errorf("envelope \"200\" -> %+v", d)
	}

	// 信封错误 + 内容审核详情：分类为 content-policy（绝不重试）
	d = ExtractDelta(envelopePayload(500, "InternalError.Algo.DataInspectionFailed: Input text data may contain inappropriate content."))
	ue = upstreamErrFrom(t, d.Err)
	if ue.ErrType != ErrTypeContentPolicy {
		t.Errorf("envelope content policy type = %s", ue.ErrType)
	}

	// 内层业务 code（body 里的 {"code":"115"}）：Status=0，与信封错误严格区分，不可重试
	d = ExtractDelta(`{"body":"{\"code\":\"115\",\"message\":\"quota exceeded\"}"}`)
	ue = upstreamErrFrom(t, d.Err)
	if ue.Status != 0 || ue.ErrType != ErrTypeUpstream {
		t.Errorf("inner business error -> %+v", ue)
	}
	if isRetryableStreamError(ue) {
		t.Error("inner business error must not be retryable")
	}

	// 无 statusCodeValue：行为不变
	d = ExtractDelta(fmt.Sprintf("{\"body\":%q}", `{"choices":[{"delta":{"content":"x"}}]}`))
	if d.Err != nil || d.Content != "x" {
		t.Errorf("frame without statusCodeValue -> %+v", d)
	}
}

func TestIsRetryableStreamError(t *testing.T) {
	if isRetryableStreamError(nil) {
		t.Error("nil must not be retryable")
	}
	if !isRetryableStreamError(NewUpstreamError(418, "provider_error")) {
		t.Error("envelope 418 must be retryable")
	}
	if isRetryableStreamError(NewUpstreamError(400, "invalid_parameter_error: Range of x")) {
		t.Error("client fault must not be retryable")
	}
	if !isRetryableStreamError(errors.New("read: connection reset by peer")) {
		t.Error("raw transport error must be retryable")
	}
	if isRetryableStreamError(context.Canceled) {
		t.Error("context canceled must not be retryable")
	}
}

// --- CallQoderWithOpts 信封重开闸门集成测试 ---

func TestCallQoderEnvelopeRetrySucceeds(t *testing.T) {
	withFastBackoff(t)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/event-stream")
		if hits == 1 {
			// 第一次：HTTP200 建流后信封投 418（hub 实证形态）
			fmt.Fprint(w, sseEnvelope(418, "provider_error: transient"))
			return
		}
		fmt.Fprint(w, sseContent("hello"))
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	var got []string
	err := envelopeBridge(t, srv.URL).CallQoderWithOpts(
		context.Background(), "codex", testMessages(), "auto", nil, CallOpts{},
		func(d Delta) { got = append(got, d.Content) })
	if err != nil {
		t.Fatalf("CallQoderWithOpts: %v", err)
	}
	if hits != 2 {
		t.Errorf("upstream hits = %d, want 2 (1 reopen after envelope 418)", hits)
	}
	if len(got) != 1 || got[0] != "hello" {
		t.Errorf("deltas = %v, want [hello]", got)
	}
}

func TestCallQoderEnvelopeNoRetryAfterEmit(t *testing.T) {
	withFastBackoff(t)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/event-stream")
		// 已发出内容后再投信封错误：闸门必须拒绝重开，避免重复内容
		fmt.Fprint(w, sseContent("partial"))
		fmt.Fprint(w, sseEnvelope(418, "provider_error"))
	}))
	defer srv.Close()

	var got []string
	err := envelopeBridge(t, srv.URL).CallQoderWithOpts(
		context.Background(), "codex", testMessages(), "auto", nil, CallOpts{},
		func(d Delta) { got = append(got, d.Content) })
	if err == nil {
		t.Fatal("expected error after emitted+envelope failure")
	}
	if hits != 1 {
		t.Errorf("upstream hits = %d, want 1 (must NOT reopen after emit)", hits)
	}
	if len(got) != 1 || got[0] != "partial" {
		t.Errorf("deltas = %v, want [partial]", got)
	}
}

func TestCallQoderEnvelopeContentPolicyNoRetry(t *testing.T) {
	withFastBackoff(t)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseEnvelope(500, "InternalError.Algo.DataInspectionFailed: Input text data may contain inappropriate content."))
	}))
	defer srv.Close()

	err := envelopeBridge(t, srv.URL).CallQoderWithOpts(
		context.Background(), "codex", testMessages(), "auto", nil, CallOpts{},
		func(Delta) {})
	ue := upstreamErrFrom(t, err)
	if hits != 1 {
		t.Errorf("upstream hits = %d, want 1 (content policy fast-fail)", hits)
	}
	if ue.ErrType != ErrTypeContentPolicy {
		t.Errorf("ErrType = %s, want %s", ue.ErrType, ErrTypeContentPolicy)
	}
}

func TestCallQoderEnvelopeRetryExhausted(t *testing.T) {
	withFastBackoff(t)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseEnvelope(418, "provider_error"))
	}))
	defer srv.Close()

	err := envelopeBridge(t, srv.URL).CallQoderWithOpts(
		context.Background(), "codex", testMessages(), "auto", nil, CallOpts{},
		func(Delta) {})
	ue := upstreamErrFrom(t, err)
	if hits != TransientMaxRetries+1 {
		t.Errorf("upstream hits = %d, want %d", hits, TransientMaxRetries+1)
	}
	if ue.ErrType != ErrTypeTransient {
		t.Errorf("ErrType = %s, want %s", ue.ErrType, ErrTypeTransient)
	}
}

// TestCallQoderEnvelopeRetryWhenUpstreamKeepsStreamOpen 回归测试（review P1-1）：
// 上游 HTTP200 建流后先发错误信封帧（statusCodeValue=418），随后持续写入正常帧
// 且不关闭连接——模拟 hub「错误帧后不关流」形态。修复前 openStreamLines 会一直
// 阻塞读到上游关流，重开闸门永不触发；修复后 dispatcher 返回 false 即停止读取，
// CallQoderWithOpts 必须在有限时间内完成重开，第二次请求拿到正常内容。
func TestCallQoderEnvelopeRetryWhenUpstreamKeepsStreamOpen(t *testing.T) {
	withFastBackoff(t)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		flush := func() {
			if flusher != nil {
				flusher.Flush()
			}
		}
		if n == 1 {
			// 第一次：错误信封帧后持续写正常帧，故意不关连接
			fmt.Fprint(w, sseEnvelope(418, "provider_error: transient"))
			flush()
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-r.Context().Done():
					return
				case <-ticker.C:
					if _, err := fmt.Fprint(w, sseContent("junk")); err != nil {
						return
					}
					flush()
				}
			}
		}
		fmt.Fprint(w, sseContent("hello"))
		fmt.Fprint(w, "data: [DONE]\n\n")
		flush()
	}))
	defer srv.Close()

	// 有限时间约束：若修复失效导致挂起，ctx 超时使调用返回错误、测试失败
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var got []string
	err := envelopeBridge(t, srv.URL).CallQoderWithOpts(
		ctx, "codex", testMessages(), "auto", nil, CallOpts{},
		func(d Delta) { got = append(got, d.Content) })
	if err != nil {
		t.Fatalf("CallQoderWithOpts: %v (upstream hits=%d, want reopen)", err, atomic.LoadInt32(&hits))
	}
	if atomic.LoadInt32(&hits) < 2 {
		t.Fatalf("upstream hits = %d, want >= 2 (must reopen after envelope error)", atomic.LoadInt32(&hits))
	}
	if len(got) != 1 || got[0] != "hello" {
		t.Fatalf("deltas = %v, want [hello]", got)
	}
}

// TestCallQoderEmptyStreamReturnsError 回归测试（review P2-1）：
// 上游 HTTP200 建流后零有效帧即关流：必须返回 ErrEmptyStream
// （对齐 hub "empty upstream stream"），而不是空内容正常 finish。
func TestCallQoderEmptyStreamReturnsError(t *testing.T) {
	withFastBackoff(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// 不发任何帧，直接关流
	}))
	defer srv.Close()

	var got []string
	err := envelopeBridge(t, srv.URL).CallQoderWithOpts(
		context.Background(), "codex", testMessages(), "auto", nil, CallOpts{},
		func(d Delta) { got = append(got, d.Content) })
	if !errors.Is(err, ErrEmptyStream) {
		t.Fatalf("err = %v, want ErrEmptyStream", err)
	}
	// 确认：哨兵错误不纳入重开闸门（分类逻辑未改动，isRetryableStreamError 自然返回 false）
	if isRetryableStreamError(err) {
		t.Fatal("ErrEmptyStream must not be retryable")
	}
	if len(got) != 0 {
		t.Fatalf("deltas = %v, want none", got)
	}
}
