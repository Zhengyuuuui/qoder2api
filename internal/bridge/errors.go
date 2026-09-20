// Package bridge —— 上游错误四分类与友好映射（移植自 qoder2api-hub qoder_proxy.py:2308-2398）。
//
// 分类语义：
//   - 瞬时可重试：418/5xx、传输层 TLS/EOF/reset/超时、4xx 带 provider_error
//   - 永不重试  ：客户端参数/权限错（invalid_parameter_error 等）、
//     内容审核拒绝（DataInspectionFailed）、401/403/429、配置/证书类传输错误
//   - 友好映射  ：内容审核 → content_policy_rejected（中文解释、明示重试无效，
//     客户端状态统一 400）；瞬时耗尽 → upstream_transient_error（提示稍后重试，
//     客户端状态统一 502，不透传上游包装的 418 等无意义状态码）
package bridge

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// 错误类型（透传给客户端的 error.type 字段）
const (
	ErrTypeContentPolicy = "content_policy_rejected"
	ErrTypeTransient     = "upstream_transient_error"
	ErrTypeUpstream      = "upstream_error"
)

// TransientMaxRetries 同账号瞬时故障额外重试次数（退避时长见 RetryBackoff）。
const TransientMaxRetries = 2

// ErrEmptyStream 上游建流成功（HTTP200）但未投递任何有效 delta/usage 帧即正常关流
// （对齐 hub "empty upstream stream"）：按约定不重试，由 CallQoderWithOpts 直接上抛，
// 避免客户端收到空 assistant 消息却按正常 finish 结束。
var ErrEmptyStream = errors.New("empty upstream stream")

// RetryBackoff 返回第 attempt 次重试（attempt 从 1 计）前的等待时长。
// 生产为 1s、2s；测试中替换为毫秒级以加速。
var RetryBackoff = func(attempt int) time.Duration {
	return time.Duration(attempt) * time.Second
}

// transientHTTPCodes 上游把自己的 provider 故障包装成 418，或直接 5xx。
var transientHTTPCodes = map[int]bool{418: true, 500: true, 502: true, 503: true, 504: true}

// clientFaultMarkers 客户端参数/权限/内容侧问题：重试无效，快速失败。
var clientFaultMarkers = []string{
	"invalid_parameter_error", // 如 Range of max_tokens 校验失败
	"invalid_request_error",
	"authentication_error",
	"permission_error",
	`"Range of `,
	// 上游内容安全审核（日志实证 InternalError.Algo.DataInspectionFailed:
	// Input text data may contain inappropriate content.）
	"DataInspectionFailed",
	"inappropriate content",
	"input text data may contain",
	"ContentFilter",
	"SensitiveContent",
}

// contentPolicyMarkers 内容审核类（用户输入侧问题，需专门的中文解释）。
var contentPolicyMarkers = []string{
	"DataInspectionFailed",
	"inappropriate content",
	"input text data may contain",
	"ContentFilter",
	"SensitiveContent",
}

// nonTransientTransportMarkers 配置/证书类传输错误：重试无意义，快速失败。
var nonTransientTransportMarkers = []string{
	"unsupported protocol scheme",
	"invalid url",
	"unknown scheme",
	"x509:", // 证书验证失败（区别于 TLS 握手超时——后者可重试）
}

func containsAny(detail string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(detail, m) {
			return true
		}
	}
	return false
}

// IsContentPolicy 判断是否上游内容安全审核拒绝。
func IsContentPolicy(detail string) bool {
	return containsAny(detail, contentPolicyMarkers)
}

// IsTransientUpstream 判断一次上游 HTTP 错误是否属于瞬时故障（可重试）。
//
//   - 客户端参数/权限类错误（invalid_parameter_error 等）→ 永不重试
//   - 418（上游把 provider 故障包装成 418+provider_error）与 5xx → 瞬时
//   - 其余 4xx 带 provider_error（"Error in upstream response"）→ 瞬时
//   - 401/403/429：凭证/频控各有专门路径，不属瞬时重试类
func IsTransientUpstream(code int, detail string) bool {
	if code == 401 || code == 403 || code == 429 {
		return false
	}
	if containsAny(detail, clientFaultMarkers) {
		return false
	}
	if transientHTTPCodes[code] || code >= 500 {
		return true
	}
	if code >= 400 && code < 500 && strings.Contains(detail, "provider_error") {
		return true
	}
	return false
}

// IsTransientTransport 判断传输层瞬时故障（对 qoder.sh 的 TLS/连接抖动常见）：
// SSL EOF/重置、连接重置/中止、超时等。context 主动取消、配置/证书类错误不算。
func IsTransientTransport(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// 定向判断：Go 标准库 net.Error 超时（含 url.Error 包装的超时）
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	msg := strings.ToLower(err.Error())
	if containsAny(msg, nonTransientTransportMarkers) {
		return false
	}
	for _, k := range []string{"ssl", "eof", "reset", "timed out", "timeout", "deadline exceeded", "broken pipe", "connection"} {
		if strings.Contains(msg, k) {
			return true
		}
	}
	return false
}

// truncateRunes 按 rune 截断，避免切断 UTF-8 多字节字符导致客户端乱码。
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}

// UpstreamError 携带结构化上游错误：HTTP 状态 + 详情 + 分类后的
// 错误类型与对客户端可读的中文消息。
type UpstreamError struct {
	Status  int    // 上游 HTTP 状态（流内业务错误为 0）
	Detail  string // 上游原始详情（完整保留，供日志排查）
	ErrType string // content_policy_rejected / upstream_transient_error / upstream_error
	Message string // 对客户端可读的消息（已做长度截断）
}

func (e *UpstreamError) Error() string { return e.Message }

// FriendlyUpstreamError 把上游错误转成对客户端可读的消息；瞬时故障给出重试指引。
// 返回 (message, errType)。与 hub friendly_upstream_error 逐句对齐。
func FriendlyUpstreamError(code int, detail string) (string, string) {
	detail = strings.TrimSpace(detail)
	// 1) 内容安全审核（确定性拒绝，先于瞬时判断——重试无效）
	if IsContentPolicy(detail) {
		return "上游内容安全审核未通过 (DataInspectionFailed)：输入可能含不当内容，" +
				"属确定性拒绝、重试无效。请检查/缩短输入（系统提示词、超长历史、" +
				"工具定义或粘贴的代码/文本）后重试。上游详情：" + truncateRunes(detail, 300),
			ErrTypeContentPolicy
	}
	if IsTransientUpstream(code, detail) ||
		(strings.Contains(detail, "provider_error") && !strings.Contains(detail, "invalid_")) {
		d := truncateRunes(detail, 300)
		if d == "" {
			d = "(无详情)"
		}
		return fmt.Sprintf("上游瞬时故障 (HTTP %d)：网关已对同账号自动重试仍失败，请稍后重试。上游详情：%s", code, d),
			ErrTypeTransient
	}
	return fmt.Sprintf("upstream %d: %s", code, detail), ErrTypeUpstream
}

// NewUpstreamError 按 HTTP 状态 + 详情构造结构化错误并完成友好映射。
func NewUpstreamError(code int, detail string) *UpstreamError {
	msg, et := FriendlyUpstreamError(code, detail)
	return &UpstreamError{Status: code, Detail: detail, ErrType: et, Message: msg}
}

// NewStreamBusinessError 流内业务错误（HTTP 200 帧内 code != 0）：
// 不是 HTTP 层瞬时故障，除内容审核外不做重试指引。
func NewStreamBusinessError(detail string) *UpstreamError {
	if IsContentPolicy(detail) {
		msg, et := FriendlyUpstreamError(502, detail)
		return &UpstreamError{Status: 502, Detail: detail, ErrType: et, Message: msg}
	}
	return &UpstreamError{Status: 0, Detail: detail, ErrType: ErrTypeUpstream, Message: detail}
}

// WrapTransportError 传输层错误包装：瞬时类转成带友好消息的 UpstreamError，
// 非瞬时（如 URL 配置错误）原样返回。
func WrapTransportError(err error) error {
	if err == nil {
		return nil
	}
	if IsTransientTransport(err) {
		return NewUpstreamError(502, err.Error())
	}
	return err
}

// FriendlyError 供 handler 使用：从任意错误中取出 (对客户端消息, 错误类型)。
func FriendlyError(err error) (string, string) {
	if err == nil {
		return "", ""
	}
	var ue *UpstreamError
	if errors.As(err, &ue) {
		return ue.Message, ue.ErrType
	}
	return err.Error(), "qoder_error"
}

// ErrorStatus 取错误对应的 HTTP 状态。分类映射优先于上游状态透传：
//   - 内容审核拒绝 → 400（用户输入问题，客户端应改输入而非重试）
//   - 瞬时故障耗尽 → 502（不透传上游包装的 418 等无意义状态码）
//   - 其余结构化错误 → 透传上游合法 4xx/5xx
//   - 无结构化状态 → 500
func ErrorStatus(err error) int {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		switch ue.ErrType {
		case ErrTypeContentPolicy:
			return 400
		case ErrTypeTransient:
			return 502
		}
		if ue.Status >= 400 && ue.Status <= 599 {
			return ue.Status
		}
	}
	return 500
}
