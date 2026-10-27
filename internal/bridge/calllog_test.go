package bridge

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// sseContentWithUsage 生成带 usage 的正常内容帧（上游在末帧合并 usage）。
func sseContentWithUsage(text string, in, out int) string {
	inner := fmt.Sprintf(
		`{"choices":[{"delta":{"content":%q}}],"usage":{"prompt_tokens":%d,"completion_tokens":%d}}`,
		text, in, out)
	return fmt.Sprintf("data: {\"body\":%q,\"statusCodeValue\":200}\n\n", inner)
}

// TestCallLogRecordsSuccess 验证一次成功调用被完整记录：
// 协议/模型映射/账号/token/耗时/预览。
func TestCallLogRecordsSuccess(t *testing.T) {
	ClearCalls()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseContentWithUsage("hello", 12, 7))
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	b := envelopeBridge(t, srv.URL)
	ctx := WithCallMeta(context.Background(), CallMeta{RequestID: "abcd1234", Protocol: "chat", Stream: true})

	if err := b.CallQoderWithOpts(ctx, "codex", testMessages(), "claude-sonnet-4-6", nil, CallOpts{}, func(Delta) {}); err != nil {
		t.Fatalf("CallQoderWithOpts: %v", err)
	}

	calls := RecentCalls(10)
	if len(calls) != 1 {
		t.Fatalf("got %d call records, want 1", len(calls))
	}
	rec := calls[0]
	if rec.Status != CallStatusOK {
		t.Errorf("status = %q, want ok", rec.Status)
	}
	if rec.Protocol != "chat" || rec.RequestID != "abcd1234" {
		t.Errorf("meta = protocol %q req %q, want chat/abcd1234", rec.Protocol, rec.RequestID)
	}
	if rec.Model != "claude-sonnet-4-6" {
		t.Errorf("model = %q, want claude-sonnet-4-6", rec.Model)
	}
	if rec.UpstreamModel != "gmodel" {
		t.Errorf("upstream model = %q, want gmodel (sonnet 默认映射)", rec.UpstreamModel)
	}
	if rec.Account != "default" {
		t.Errorf("account = %q, want default (单账号模式)", rec.Account)
	}
	if rec.InputTokens != 12 || rec.OutputTokens != 7 {
		t.Errorf("tokens = %d/%d, want 12/7", rec.InputTokens, rec.OutputTokens)
	}
	if !rec.Emitted {
		t.Error("emitted should be true")
	}
	if rec.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", rec.Attempts)
	}
	if rec.PromptPreview != "hi" {
		t.Errorf("prompt preview = %q, want hi", rec.PromptPreview)
	}
	if rec.Messages != 1 || rec.Tools {
		t.Errorf("messages/tools = %d/%v, want 1/false", rec.Messages, rec.Tools)
	}
}

// TestCallLogRecordsError 验证失败调用带错误类型与消息。
func TestCallLogRecordsError(t *testing.T) {
	ClearCalls()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"code":"invalid_parameter_error"}}`)
	}))
	defer srv.Close()

	b := envelopeBridge(t, srv.URL)
	ctx := WithCallMeta(context.Background(), CallMeta{RequestID: "err1", Protocol: "claude"})

	if err := b.CallQoderWithOpts(ctx, "claude", testMessages(), "auto", nil, CallOpts{}, func(Delta) {}); err == nil {
		t.Fatal("expected error")
	}

	calls := RecentCalls(10)
	if len(calls) != 1 {
		t.Fatalf("got %d records, want 1", len(calls))
	}
	rec := calls[0]
	if rec.Status != CallStatusError {
		t.Errorf("status = %q, want error", rec.Status)
	}
	if rec.ErrorType == "" || rec.ErrorMessage == "" {
		t.Errorf("error type/message should be populated, got %q/%q", rec.ErrorType, rec.ErrorMessage)
	}
}

// TestCallLogRecordsNoAccount 池中无账号时也应落一条记录（status=error）。
func TestCallLogRecordsNoAccount(t *testing.T) {
	ClearCalls()
	b := &Bridge{templateBase: map[string]interface{}{}}
	ctx := WithCallMeta(context.Background(), CallMeta{Protocol: "codex"})
	if err := b.CallQoderWithOpts(ctx, "codex", testMessages(), "auto", nil, CallOpts{}, func(Delta) {}); err == nil {
		t.Fatal("expected ErrNoAccount")
	}
	calls := RecentCalls(10)
	if len(calls) != 1 || calls[0].Status != CallStatusError {
		t.Fatalf("want one error record, got %+v", calls)
	}
	if calls[0].Attempts != 0 {
		t.Errorf("attempts = %d, want 0", calls[0].Attempts)
	}
}

// TestCallLogRingCapacity 验证环形缓冲容量与「最新在前」顺序。
func TestCallLogRingCapacity(t *testing.T) {
	cl := &callLog{cap: 3, nextSeq: 1}
	for i := 1; i <= 5; i++ {
		cl.record(CallRecord{Protocol: fmt.Sprintf("p%d", i)})
	}
	got := cl.snapshot(0)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	want := []string{"p5", "p4", "p3"}
	for i, w := range want {
		if got[i].Protocol != w {
			t.Errorf("got[%d] = %q, want %q", i, got[i].Protocol, w)
		}
	}
	if got[0].Seq != 5 {
		t.Errorf("newest seq = %d, want 5", got[0].Seq)
	}
	// limit 生效
	if limited := cl.snapshot(2); len(limited) != 2 || limited[0].Protocol != "p5" {
		t.Errorf("limit snapshot = %+v", limited)
	}
}

// TestTruncateRunesForLog 验证多行折叠与 rune 截断（不切坏 UTF-8）。
func TestTruncateRunesForLog(t *testing.T) {
	if got := truncateRunesForLog("a\n\nb   c", 100); got != "a b c" {
		t.Errorf("collapse = %q, want %q", got, "a b c")
	}
	got := truncateRunesForLog("你好世界你好世界", 4)
	if got != "你好世界…" {
		t.Errorf("truncate = %q, want 你好世界…", got)
	}
}
