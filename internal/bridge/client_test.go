package bridge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"qoder2api/internal/cosy"
)

// testClient 构造离线可用的 BearerClient（签名本地完成，无需网络）。
func testClient(t *testing.T) *BearerClient {
	t.Helper()
	sess, err := cosy.NewSession(
		cosy.AuthIdentity{Uid: "test-uid", UserType: "personal_standard"},
		"mid-test-000000000000000000000000",
		"mtoken-test",
		"mtype-test-000000",
	)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return NewBearerClient(sess)
}

// withFastBackoff 把重试退避缩短到毫秒级，测试结束后恢复。
func withFastBackoff(t *testing.T) {
	t.Helper()
	old := RetryBackoff
	RetryBackoff = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { RetryBackoff = old })
}

func upstreamErrFrom(t *testing.T, err error) *UpstreamError {
	t.Helper()
	var ue *UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v, want *UpstreamError", err)
	}
	return ue
}

// --- callPost 瞬时重试 ---

func TestCallPostRetriesTransientThenSucceeds(t *testing.T) {
	withFastBackoff(t)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits <= 2 {
			w.WriteHeader(500)
			fmt.Fprint(w, `{"err":"boom"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	got, err := testClient(t).callPost(srv.URL, map[string]interface{}{"a": 1})
	if err != nil {
		t.Fatalf("callPost: %v", err)
	}
	if hits != 3 {
		t.Errorf("server hits = %d, want 3 (2 retries + success)", hits)
	}
	if ok, _ := got["ok"].(bool); !ok {
		t.Errorf("result = %v, want ok=true", got)
	}
}

func TestCallPostNoRetryOnClientFault(t *testing.T) {
	withFastBackoff(t)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(400)
		fmt.Fprint(w, `invalid_parameter_error: Range of max_tokens`)
	}))
	defer srv.Close()

	_, err := testClient(t).callPost(srv.URL, map[string]interface{}{"a": 1})
	ue := upstreamErrFrom(t, err)
	if hits != 1 {
		t.Errorf("server hits = %d, want 1 (client fault must not retry)", hits)
	}
	if ue.ErrType != ErrTypeUpstream {
		t.Errorf("ErrType = %s, want %s", ue.ErrType, ErrTypeUpstream)
	}
}

func TestCallPostNoRetryOnContentPolicy(t *testing.T) {
	withFastBackoff(t)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(500)
		fmt.Fprint(w, "InternalError.Algo.DataInspectionFailed: Input text data may contain inappropriate content.")
	}))
	defer srv.Close()

	_, err := testClient(t).callPost(srv.URL, map[string]interface{}{"a": 1})
	ue := upstreamErrFrom(t, err)
	// 内容审核属确定性拒绝：即使状态码是 500 也绝不重试
	if hits != 1 {
		t.Errorf("server hits = %d, want 1 (content policy must not retry)", hits)
	}
	if ue.ErrType != ErrTypeContentPolicy {
		t.Errorf("ErrType = %s, want %s", ue.ErrType, ErrTypeContentPolicy)
	}
	if ErrorStatus(err) != 400 {
		t.Errorf("ErrorStatus = %d, want 400", ErrorStatus(err))
	}
}

func TestCallPostRetryExhaustedReturnsTransient(t *testing.T) {
	withFastBackoff(t)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(418)
		fmt.Fprint(w, "provider_error: upstream broke")
	}))
	defer srv.Close()

	_, err := testClient(t).callPost(srv.URL, map[string]interface{}{"a": 1})
	ue := upstreamErrFrom(t, err)
	if hits != TransientMaxRetries+1 {
		t.Errorf("server hits = %d, want %d", hits, TransientMaxRetries+1)
	}
	if ue.ErrType != ErrTypeTransient {
		t.Errorf("ErrType = %s, want %s", ue.ErrType, ErrTypeTransient)
	}
	if !strings.Contains(ue.Message, "稍后重试") {
		t.Errorf("message missing retry guidance: %s", ue.Message)
	}
	if ErrorStatus(err) != 502 {
		t.Errorf("ErrorStatus = %d, want 502", ErrorStatus(err))
	}
}

// --- openStreamLines 连接阶段瞬时重试 ---

func TestOpenStreamLinesRetriesThenStreams(t *testing.T) {
	withFastBackoff(t)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.WriteHeader(503)
			fmt.Fprint(w, "provider_error")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"body\":\"{\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"hi\\\"}}]}\"}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	var lines []string
	err := testClient(t).openStreamLines(context.Background(), srv.URL,
		map[string]interface{}{"x": 1}, nil,
		func(l string) bool { lines = append(lines, l); return true })
	if err != nil {
		t.Fatalf("openStreamLines: %v", err)
	}
	if hits != 2 {
		t.Errorf("server hits = %d, want 2 (1 retry + success)", hits)
	}
	if len(lines) != 2 {
		t.Errorf("lines = %v, want 2 lines", lines)
	}
}

func TestOpenStreamLinesNoRetryOn401(t *testing.T) {
	withFastBackoff(t)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(401)
		fmt.Fprint(w, "authentication_error")
	}))
	defer srv.Close()

	err := testClient(t).openStreamLines(context.Background(), srv.URL,
		map[string]interface{}{"x": 1}, nil,
		func(string) bool { return true })
	ue := upstreamErrFrom(t, err)
	if hits != 1 {
		t.Errorf("server hits = %d, want 1 (401 must not retry)", hits)
	}
	if ue.ErrType != ErrTypeUpstream || ue.Status != 401 {
		t.Errorf("ue = %+v, want upstream_error/401", ue)
	}
}

func TestOpenStreamLinesExhaustedReturnsTransient(t *testing.T) {
	withFastBackoff(t)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(502)
		fmt.Fprint(w, "provider_error")
	}))
	defer srv.Close()

	err := testClient(t).openStreamLines(context.Background(), srv.URL,
		map[string]interface{}{"x": 1}, nil,
		func(string) bool { return true })
	ue := upstreamErrFrom(t, err)
	if hits != TransientMaxRetries+1 {
		t.Errorf("server hits = %d, want %d", hits, TransientMaxRetries+1)
	}
	if ue.ErrType != ErrTypeTransient {
		t.Errorf("ErrType = %s, want %s", ue.ErrType, ErrTypeTransient)
	}
}

func TestOpenStreamLinesRespectsCanceledContext(t *testing.T) {
	withFastBackoff(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := testClient(t).openStreamLines(ctx, "http://127.0.0.1:1",
		map[string]interface{}{"x": 1}, nil,
		func(string) bool { return true })
	if err == nil {
		t.Fatal("expected error for canceled context")
	}
	// 取消场景不得被包装成“瞬时故障，请稍后重试”
	if ue, ok := err.(*UpstreamError); ok && ue.ErrType == ErrTypeTransient {
		t.Errorf("canceled context wrapped as transient: %v", err)
	}
}
