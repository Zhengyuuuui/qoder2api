package bridge

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"qoder2api/account"
)

func TestBodyLimitRejectsLargeContentLength(t *testing.T) {
	guardSettings(t, account.Settings{MaxBodyMB: 1})
	called := false
	h := BodyLimitMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	}))

	// 2MB body，Content-Length 已知 → 一个字节都不读，直接 413
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(strings.Repeat("x", 2*1024*1024)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
	if called {
		t.Error("downstream handler must not run for oversized body")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "请求体超过") || !strings.Contains(body, "拆分请求") {
		t.Errorf("413 body not readable: %s", body)
	}
}

func TestBodyLimitAllowsWithinLimit(t *testing.T) {
	guardSettings(t, account.Settings{MaxBodyMB: 1})
	called := false
	h := BodyLimitMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		buf := make([]byte, 64)
		n, _ := r.Body.Read(buf)
		w.WriteHeader(200)
		w.Write(buf[:n])
	}))
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"auto"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !called {
		t.Errorf("within-limit request rejected: code=%d called=%v", rec.Code, called)
	}
	if !strings.Contains(rec.Body.String(), "auto") {
		t.Errorf("body not readable downstream: %s", rec.Body.String())
	}
}

func TestBodyLimitChunkedOverLimit(t *testing.T) {
	guardSettings(t, account.Settings{MaxBodyMB: 1})
	called := false
	h := BodyLimitMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	// chunked（无 Content-Length）：限量读取，超出即拒
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(strings.Repeat("y", 2*1024*1024)))
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge || called {
		t.Errorf("chunked oversized: code=%d called=%v, want 413/not-called", rec.Code, called)
	}
}

func TestTruncateToolResultTailKeepsTail(t *testing.T) {
	t.Setenv("QD_TOOL_RESULT_SCOPE", "tail")
	s := strings.Repeat("A", 200) + "TAIL_MARKER" + strings.Repeat("B", 50)
	// 取尾部 100 rune：39 个 A + 完整标记 + 50 个 B（标记不跨截断边界）
	out := TruncateToolResult(s, 100)

	if !strings.HasSuffix(out, "TAIL_MARKER"+strings.Repeat("B", 50)) {
		t.Errorf("tail scope must preserve the tail (conclusion); got suffix %q", out[len(out)-70:])
	}
	if !strings.Contains(out, "truncated") {
		t.Errorf("truncation marker missing: %s", out)
	}
	if len(out) >= len(s) {
		t.Error("result not truncated")
	}
}

func TestApplyToolResultBudgetsTruncatesOldestFirst(t *testing.T) {
	t.Setenv("QD_TOOL_RESULT_SCOPE", "tail")
	guardSettings(t, account.Settings{ToolResultBytes: 40, ToolResultTotalBytes: 100})

	oldest := strings.Repeat("O", 300)
	middle := strings.Repeat("M", 300)
	newest := strings.Repeat("N", 30)
	out := ApplyToolResultBudgets([]string{oldest, middle, newest})

	if len(out) != 3 {
		t.Fatalf("out = %d items, want 3", len(out))
	}
	// 总和 630 > 100：最新的基本完整，最旧的被压到最小
	if out[2] != newest {
		t.Errorf("newest result changed: len=%d want %d", len(out[2]), len(newest))
	}
	if len(out[0]) >= 300 {
		t.Errorf("oldest not truncated: len=%d", len(out[0]))
	}
	if len(out[1]) >= 300 {
		t.Errorf("middle not truncated: len=%d", len(out[1]))
	}
}

func TestToolResultScopeOffDisablesTruncation(t *testing.T) {
	t.Setenv("QD_TOOL_RESULT_SCOPE", "off")
	guardSettings(t, account.Settings{ToolResultBytes: 10, ToolResultTotalBytes: 20})
	s := strings.Repeat("Z", 5000)
	if out := TruncateToolResult(s, 10); out != s {
		t.Error("scope=off must not truncate")
	}
	if out := ApplyToolResultBudgets([]string{s, s}); out[0] != s || out[1] != s {
		t.Error("scope=off must not apply budgets")
	}
}

func TestClampMaxTokens(t *testing.T) {
	guardSettings(t, account.Settings{MaxOutputTokens: 1000})
	if v, clamped := ClampMaxTokens(5000); v != 1000 || !clamped {
		t.Errorf("ClampMaxTokens(5000) = (%d, %v), want (1000, true)", v, clamped)
	}
	if v, clamped := ClampMaxTokens(500); v != 500 || clamped {
		t.Errorf("ClampMaxTokens(500) = (%d, %v), want (500, false)", v, clamped)
	}
	// 未配置 → 默认 32768
	guardSettings(t, account.Settings{})
	if v, _ := ClampMaxTokens(100000); v != DefaultMaxOutputTokens {
		t.Errorf("default cap = %d, want %d", v, DefaultMaxOutputTokens)
	}
}
