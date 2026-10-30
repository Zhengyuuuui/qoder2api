package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
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

// nestedResourceLimitDetail 构造上游三层嵌套业务错误报文（RESOURCE_LIMIT）。
//
// 上游把业务错误嵌在 message 字段里，每嵌一层就做一次 JSON 字符串编码：
//
//	outer.message = json(mid)      // mid 的引号变成 \"
//	mid.message   = json(inner)    // inner 的引号变成 \"
//
// 因此用 json.Marshal 逐层编码最可靠（避免手写转义出错）。
func nestedResourceLimitDetail(isQueued bool, serviceAvailable bool, queueCount int) string {
	// 排队时才带 retryAfterSeconds/waitTime；未排队（无权限）则为 0。
	wait := 0
	if isQueued {
		wait = 30
	}
	inner := fmt.Sprintf(
		`{"isQueued":%v,"modelKey":"qfmodel","queueCount":%d,`+
			`"queueType":"p3","retryAfterSeconds":%d,"serviceAvailable":%v,`+
			`"subCode":"RESOURCE_LIMIT","waitTime":%d}`,
		isQueued, queueCount, wait, serviceAvailable, wait)

	mid := fmt.Sprintf(
		`{"code":"10605","message":%s,"subCode":"RESOURCE_LIMIT"}`,
		mustJSONString(inner))

	outer := fmt.Sprintf(
		`{"code":"403","message":%s}`,
		mustJSONString(mid))

	return "upstream 403: " + outer
}

// mustJSONString 把任意字符串编码成 JSON 字符串字面量（含两端双引号）。
func mustJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestParseUpstreamNestedError_ResourceLimit(t *testing.T) {
	detail := nestedResourceLimitDetail(true, false, 0)
	d, ok := parseUpstreamNestedError(detail)
	if !ok {
		t.Fatalf("should parse nested RESOURCE_LIMIT error, detail=%s", detail)
	}
	if d.ModelKey != "qfmodel" {
		t.Errorf("ModelKey = %q, want qfmodel", d.ModelKey)
	}
	if d.SubCode != "RESOURCE_LIMIT" {
		t.Errorf("SubCode = %q, want RESOURCE_LIMIT", d.SubCode)
	}
	if d.ServiceAvailable {
		t.Error("ServiceAvailable should be false")
	}
	if d.RetryAfterSeconds != 30 {
		t.Errorf("RetryAfterSeconds = %d, want 30", d.RetryAfterSeconds)
	}
}

func TestParseUpstreamNestedError_NotNested(t *testing.T) {
	for _, detail := range []string{
		"",
		"upstream 500: internal error",
		`{"code":"403","message":"forbidden"}`,
		`{"error":"something else"}`,
	} {
		if _, ok := parseUpstreamNestedError(detail); ok {
			t.Errorf("parseUpstreamNestedError(%q) should fail", detail)
		}
	}
}

// 回归：CALL_COUNT_LIMIT 与 RESOURCE_LIMIT 结构相同，但预筛选若只匹配带引号的
// "subCode" 会因内层 JSON 引号被转义而漏掉它（isQueued/retryAfter 来不及解析），
// 导致排队被误判成普通错误、进而冷却+换号。
func TestParseUpstreamNestedErrorCallCountLimit(t *testing.T) {
	detail := strings.ReplaceAll(
		nestedResourceLimitDetail(true, true, 10),
		"RESOURCE_LIMIT", "CALL_COUNT_LIMIT",
	)
	d, ok := parseUpstreamNestedError(detail)
	if !ok {
		t.Fatal("isQueued=true 的 CALL_COUNT_LIMIT 报文被预筛选漏掉")
	}
	if d.SubCode != "CALL_COUNT_LIMIT" || !d.IsQueued {
		t.Errorf("parsed detail = %+v", d)
	}
}

func TestFriendlyUpstreamError_CallCountLimitQueued(t *testing.T) {
	// 真实场景：CALL_COUNT_LIMIT + isQueued=true 应按「排队」处理，
	// 判为 upstream_queued 且不换号（否则误冷却+换号、等待策略不生效）。
	detail := strings.ReplaceAll(
		nestedResourceLimitDetail(true, true, 10),
		"RESOURCE_LIMIT", "CALL_COUNT_LIMIT",
	)
	msg, et := FriendlyUpstreamError(403, detail)
	if et != ErrTypeUpstreamQueue {
		t.Errorf("errType = %q, want %q", et, ErrTypeUpstreamQueue)
	}
	if !strings.Contains(msg, "排队") {
		t.Errorf("message should mention queueing, got: %s", msg)
	}
	if ShouldFailover(NewUpstreamError(403, detail)) {
		t.Error("queued CALL_COUNT_LIMIT should NOT failover")
	}
}

func TestFriendlyUpstreamError_ResourceLimit(t *testing.T) {
	// 未排队且服务不可用（无 retryAfter）：账号无权使用该模型（确定性拒绝）
	detail := nestedResourceLimitDetail(false, false, 0)
	msg, et := FriendlyUpstreamError(403, detail)
	if et != ErrTypeResourceLimit {
		t.Errorf("errType = %q, want %q", et, ErrTypeResourceLimit)
	}
	if !strings.Contains(msg, "qfmodel") {
		t.Errorf("message should mention model key, got: %s", msg)
	}
	if !strings.Contains(msg, "不可用") {
		t.Errorf("message should explain unavailability, got: %s", msg)
	}
	if got := ErrorStatus(NewUpstreamError(403, detail)); got != 403 {
		t.Errorf("ErrorStatus = %d, want 403", got)
	}
}

func TestFriendlyUpstreamError_UpstreamQueued(t *testing.T) {
	// serviceAvailable=true：上游拥塞排队，稍后重试可能成功
	detail := nestedResourceLimitDetail(true, true, 3)
	msg, et := FriendlyUpstreamError(403, detail)
	if et != ErrTypeUpstreamQueue {
		t.Errorf("errType = %q, want %q", et, ErrTypeUpstreamQueue)
	}
	if !strings.Contains(msg, "排队") {
		t.Errorf("message should mention queueing, got: %s", msg)
	}
	if !strings.Contains(msg, "30") {
		t.Errorf("message should mention wait time, got: %s", msg)
	}
	if got := ErrorStatus(NewUpstreamError(403, detail)); got != 503 {
		t.Errorf("ErrorStatus = %d, want 503", got)
	}
}

func TestFriendlyUpstreamError_QueuedServiceUnavailable(t *testing.T) {
	// 用户实况：热门模型被挤爆——isQueued=true 但 serviceAvailable=false 且带 retryAfter。
	// 这属于「排队拥塞」而非「无权限」，必须判为可重试的 upstream_queued（否则会被
	// 误判成 RESOURCE_LIMIT 而冷却全部账号、直接报错断开）。
	detail := nestedResourceLimitDetail(true, false, 5)
	msg, et := FriendlyUpstreamError(403, detail)
	if et != ErrTypeUpstreamQueue {
		t.Errorf("errType = %q, want %q", et, ErrTypeUpstreamQueue)
	}
	for _, want := range []string{"排队", "5" /* 前方约 5 人 */, "30" /* 预计等 30 秒 */} {
		if !strings.Contains(msg, want) {
			t.Errorf("message should contain %q, got: %s", want, msg)
		}
	}
	if got := ErrorStatus(NewUpstreamError(403, detail)); got != 503 {
		t.Errorf("ErrorStatus = %d, want 503", got)
	}
}

func TestShouldFailover_Queue(t *testing.T) {
	// 排队换号无益（同模型同队列）：不应换号、不应冷却账号。
	detail := nestedResourceLimitDetail(true, false, 5)
	if ShouldFailover(NewUpstreamError(403, detail)) {
		t.Error("queue error should NOT failover")
	}
}

func TestQueueRetryAfter(t *testing.T) {
	// 上游 retryAfter=30 在 cap 内 → 30s
	err := NewUpstreamError(403, nestedResourceLimitDetail(true, false, 0))
	if got := queueRetryAfter(err); got != 30*time.Second {
		t.Errorf("queueRetryAfter = %v, want 30s", got)
	}
	// 非队列错误 → 默认等待
	if got := queueRetryAfter(NewUpstreamError(500, "boom")); got != QueueWaitDefault {
		t.Errorf("queueRetryAfter(non-queue) = %v, want %v", got, QueueWaitDefault)
	}
	// 无 isQueued 的权限类错误 → 默认等待（不会被当成队列）
	perm := NewUpstreamError(403, nestedResourceLimitDetail(false, false, 0))
	if isUpstreamQueue(perm) {
		t.Error("permission error must not be classified as queue")
	}
}

func TestApplyQueueSettings(t *testing.T) {
	omr, owc, owd, oft := QueueMaxRetries, QueueWaitCap, QueueWaitDefault, QueueWaitFirstToken
	defer func() { QueueMaxRetries, QueueWaitCap, QueueWaitDefault, QueueWaitFirstToken = omr, owc, owd, oft }()

	// 全 0 → 各自默认
	ApplyQueueSettings(0, 0, 0, false)
	if QueueMaxRetries != DefaultQueueMaxRetries ||
		QueueWaitCap != DefaultQueueWaitCapSec*time.Second ||
		QueueWaitDefault != DefaultQueueWaitDefSec*time.Second ||
		QueueWaitFirstToken != false {
		t.Errorf("defaults not applied: %d %v %v %v", QueueMaxRetries, QueueWaitCap, QueueWaitDefault, QueueWaitFirstToken)
	}
	// 正常值透传（含开关）
	ApplyQueueSettings(5, 10, 8, true)
	if QueueMaxRetries != 5 || QueueWaitCap != 10*time.Second || QueueWaitDefault != 8*time.Second || QueueWaitFirstToken != true {
		t.Errorf("custom not applied: %d %v %v %v", QueueMaxRetries, QueueWaitCap, QueueWaitDefault, QueueWaitFirstToken)
	}
	// 超上限夹取
	ApplyQueueSettings(99, 9999, 9999, false)
	if QueueMaxRetries != maxQueueRetriesCeiling ||
		QueueWaitCap != maxQueueWaitCeilingSec*time.Second ||
		QueueWaitDefault != maxQueueWaitCeilingSec*time.Second {
		t.Errorf("clamp failed: %d %v %v", QueueMaxRetries, QueueWaitCap, QueueWaitDefault)
	}
}

func TestFriendlyUpstreamError_Plain403Unchanged(t *testing.T) {
	// 普通 403 不走嵌套解析，保持原有行为
	msg, et := FriendlyUpstreamError(403, "access denied")
	if et != ErrTypeUpstream {
		t.Errorf("errType = %q, want %q", et, ErrTypeUpstream)
	}
	if !strings.Contains(msg, "upstream 403") {
		t.Errorf("message = %q, want upstream 403 prefix", msg)
	}
}
