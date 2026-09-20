package bridge

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestIsTransientUpstream(t *testing.T) {
	cases := []struct {
		code   int
		detail string
		want   bool
	}{
		{418, "", true}, // 上游包装的 provider 故障
		{418, "provider_error in upstream", true},
		{500, "", true},
		{502, "", true},
		{503, "", true},
		{504, "", true},
		{400, "invalid_parameter_error: Range of max_tokens", false}, // 客户端参数错
		{400, "invalid_request_error", false},
		{401, "", false}, // 凭证
		{403, "", false},
		{429, "usage exceeds frequency limit", false},             // 频控走专门路径
		{400, "Error in upstream response: provider_error", true}, // 4xx 带 provider_error
		{404, "not found", false},
	}
	for _, c := range cases {
		if got := IsTransientUpstream(c.code, c.detail); got != c.want {
			t.Errorf("IsTransientUpstream(%d, %q) = %v, want %v", c.code, c.detail, got, c.want)
		}
	}
}

func TestIsContentPolicy(t *testing.T) {
	detail := "InternalError.Algo.DataInspectionFailed: Input text data may contain inappropriate content."
	if !IsContentPolicy(detail) {
		t.Fatal("DataInspectionFailed should be content policy")
	}
	if IsContentPolicy("some random upstream error") {
		t.Fatal("unrelated error should not be content policy")
	}
}

func TestIsTransientTransport(t *testing.T) {
	if !IsTransientTransport(errors.New(`Post "https://api1.qoder.sh": net/http: TLS handshake timeout`)) {
		t.Error("TLS timeout should be transient")
	}
	if !IsTransientTransport(errors.New("read: connection reset by peer")) {
		t.Error("connection reset should be transient")
	}
	if !IsTransientTransport(errors.New("SSL: UNEXPECTED_EOF_WHILE_READING")) {
		t.Error("SSL EOF should be transient")
	}
	// url.Error 包装的拨号失败（网关重启窗口）→ 可重试
	uerr := &url.Error{Op: "Post", URL: "https://api1.qoder.sh/x", Err: errors.New("dial tcp: connection refused")}
	if !IsTransientTransport(uerr) {
		t.Error("url.Error wrapping connection refused should be transient")
	}
	// 配置/证书类错误 → 重试无意义
	if IsTransientTransport(errors.New(`Post "::bad": unsupported protocol scheme`)) {
		t.Error("config error must not be retried")
	}
	if IsTransientTransport(errors.New("x509: certificate signed by unknown authority")) {
		t.Error("cert validation error must not be retried")
	}
	if IsTransientTransport(context.Canceled) {
		t.Error("context canceled must not be retried")
	}
	if IsTransientTransport(nil) {
		t.Error("nil should not be transient")
	}
}

func TestFriendlyUpstreamError(t *testing.T) {
	// 内容审核 → 中文解释 + content_policy_rejected（先于瞬时判断）
	msg, et := FriendlyUpstreamError(500, "InternalError.Algo.DataInspectionFailed: Input text data may contain inappropriate content.")
	if et != ErrTypeContentPolicy {
		t.Errorf("errType = %s, want %s", et, ErrTypeContentPolicy)
	}
	if !strings.Contains(msg, "内容安全审核") || !strings.Contains(msg, "重试无效") {
		t.Errorf("content policy message should explain deterministic rejection: %s", msg)
	}

	// 瞬时耗尽 → upstream_transient_error + 稍后重试指引
	msg, et = FriendlyUpstreamError(418, "provider_error")
	if et != ErrTypeTransient {
		t.Errorf("errType = %s, want %s", et, ErrTypeTransient)
	}
	if !strings.Contains(msg, "稍后重试") {
		t.Errorf("transient message should advise retry later: %s", msg)
	}

	// 客户端参数错 → 不进瞬时分支
	_, et = FriendlyUpstreamError(400, "invalid_parameter_error: Range of max_tokens")
	if et != ErrTypeUpstream {
		t.Errorf("client fault errType = %s, want %s", et, ErrTypeUpstream)
	}
}

func TestUpstreamErrorClassification(t *testing.T) {
	ue := NewUpstreamError(418, "provider_error boom")
	if ue.ErrType != ErrTypeTransient {
		t.Errorf("NewUpstreamError(418) type = %s", ue.ErrType)
	}
	if !IsTransientUpstream(ue.Status, ue.Detail) {
		t.Error("418 UpstreamError must be retryable at client layer")
	}

	// 流内业务错误：非内容审核 → upstream_error，不做瞬时重试
	be := NewStreamBusinessError("upstream error code=115: quota exceeded")
	if be.ErrType != ErrTypeUpstream || be.Status != 0 {
		t.Errorf("stream business error = %+v", be)
	}
	// 流内内容审核 → content_policy_rejected
	be = NewStreamBusinessError("DataInspectionFailed: inappropriate content")
	if be.ErrType != ErrTypeContentPolicy {
		t.Errorf("stream content policy type = %s", be.ErrType)
	}
}

func TestFriendlyErrorAndStatus(t *testing.T) {
	// 418 瞬时耗尽 → 消息/类型正确；客户端状态统一 502，不透传 418
	ue := NewUpstreamError(418, "x")
	msg, et := FriendlyError(ue)
	if msg != ue.Message || et != ErrTypeTransient {
		t.Errorf("FriendlyError(UpstreamError) = %s, %s", msg, et)
	}
	if ErrorStatus(ue) != 502 {
		t.Errorf("ErrorStatus(transient 418) = %d, want 502", ErrorStatus(ue))
	}
	// 内容审核 → 客户端状态统一 400
	cp := NewUpstreamError(500, "InternalError.Algo.DataInspectionFailed: Input text data may contain inappropriate content.")
	if ErrorStatus(cp) != 400 {
		t.Errorf("ErrorStatus(content policy) = %d, want 400", ErrorStatus(cp))
	}
	// 上游普通错误 → 状态透传
	nf := NewUpstreamError(404, "not found")
	if nf.ErrType != ErrTypeUpstream || ErrorStatus(nf) != 404 {
		t.Errorf("passthrough status: type=%s status=%d", nf.ErrType, ErrorStatus(nf))
	}

	generic := errors.New("boom")
	msg, et = FriendlyError(generic)
	if msg != "boom" || et != "qoder_error" {
		t.Errorf("FriendlyError(generic) = %s, %s", msg, et)
	}
	if ErrorStatus(generic) != 500 {
		t.Errorf("ErrorStatus(generic) = %d, want 500", ErrorStatus(generic))
	}
	if ErrorStatus(nil) != 500 {
		t.Errorf("ErrorStatus(nil) = %d, want 500", ErrorStatus(nil))
	}
}

func TestFriendlyMessageRuneSafe(t *testing.T) {
	// 超长中文详情截断后必须仍是合法 UTF-8（按 rune 截断）
	long := strings.Repeat("上游返回了不合适的输入内容", 100)
	msg, _ := FriendlyUpstreamError(500, "DataInspectionFailed "+long)
	if !utf8.ValidString(msg) {
		t.Error("message truncated mid-rune")
	}
	if !strings.HasSuffix(msg, "...") {
		t.Error("expected truncation marker")
	}
}

func TestRetryBackoffSchedule(t *testing.T) {
	// 生产退避节奏：第 1 次 1s、第 2 次 2s（与 hub 一致）
	if RetryBackoff(1).Seconds() != 1 || RetryBackoff(2).Seconds() != 2 {
		t.Errorf("backoff schedule = %v, %v", RetryBackoff(1), RetryBackoff(2))
	}
}
