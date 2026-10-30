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
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"qoder2api/logger"
)

// 错误类型（透传给客户端的 error.type 字段）
const (
	ErrTypeContentPolicy = "content_policy_rejected"
	ErrTypeTransient     = "upstream_transient_error"
	ErrTypeUpstream      = "upstream_error"
	// ErrTypeNoAccount 账号池全部处于冷却（额度/频控/故障），请求无法分配账号。
	ErrTypeNoAccount = "no_available_account"
	// ErrTypeResourceLimit 上游账号资源/套餐限制：请求的模型不在账号可用范围
	// （RESOURCE_LIMIT）。确定性拒绝，重试或换号都无效，需换模型或升级套餐。
	ErrTypeResourceLimit = "resource_limit"
	// ErrTypeUpstreamQueue 上游队列拥塞（isQueued=true 或带 retryAfterSeconds）。
	// 属可重试的拥塞：同账号等待重试可能成功，换号无益（同模型同队列）。
	ErrTypeUpstreamQueue = "upstream_queued"
)

// TransientMaxRetries 同账号瞬时故障额外重试次数（退避时长见 RetryBackoff）。
const TransientMaxRetries = 2

// 队列拥塞重试参数：上游 10605 isQueued 时，同账号等待重试（换号无益）。
// 单次等待取上游 retryAfterSeconds，夹在 [1s, QueueWaitCap]；最多 QueueMaxRetries 次。
// 默认值见 Default*；运行时由 ApplyQueueSettings 按控制台 settings 覆盖。
var (
	QueueMaxRetries  = DefaultQueueMaxRetries
	QueueWaitCap     = DefaultQueueWaitCapSec * time.Second
	QueueWaitDefault = DefaultQueueWaitDefSec * time.Second
	// QueueWaitFirstToken 排队时「首字前一直等」：true=直到出首字或客户端断开才停
	// （忽略 QueueMaxRetries）；false=按 QueueMaxRetries 封顶。默认开（true）。
	QueueWaitFirstToken = true
)

const (
	DefaultQueueMaxRetries = 2  // 排队同账号最多重试次数
	DefaultQueueWaitCapSec = 30 // 单次排队等待上限（秒）
	DefaultQueueWaitDefSec = 15 // 上游未给 retryAfter 时的默认等待（秒）
	maxQueueRetriesCeiling = 10 // 重试次数硬上限（防误配过长）
	maxQueueWaitCeilingSec = 300
)

// ApplyQueueSettings 应用控制台配置的排队重试参数到包级变量（带默认值与夹取）。
// 启动（LoadRuntimeSettings）与保存设置后各调用一次，使改动即时生效、无需重启。
// 入参对应 settings 的 queue_max_retries / queue_wait_cap_sec / queue_wait_default_sec /
// queue_wait_first_token；每个时长 ≤0 时回退各自默认值。
// waitFirstToken=true 时忽略 maxRetries，改为「首字前一直等（仅客户端断开停止）」。
func ApplyQueueSettings(maxRetries, waitCapSec, waitDefaultSec int, waitFirstToken bool) {
	if maxRetries <= 0 {
		maxRetries = DefaultQueueMaxRetries
	}
	if maxRetries > maxQueueRetriesCeiling {
		maxRetries = maxQueueRetriesCeiling
	}
	QueueMaxRetries = maxRetries
	QueueWaitCap = clampQueueSeconds(waitCapSec, DefaultQueueWaitCapSec)
	QueueWaitDefault = clampQueueSeconds(waitDefaultSec, DefaultQueueWaitDefSec)
	QueueWaitFirstToken = waitFirstToken
	logger.Info("queue retry applied: maxRetries=%d waitCap=%s waitDefault=%s waitFirstToken=%v",
		QueueMaxRetries, QueueWaitCap, QueueWaitDefault, QueueWaitFirstToken)
}

func clampQueueSeconds(v, def int) time.Duration {
	if v <= 0 {
		v = def
	}
	if v > maxQueueWaitCeilingSec {
		v = maxQueueWaitCeilingSec
	}
	return time.Duration(v) * time.Second
}

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

// ============================================================================
// 上游三层嵌套错误解析（RESOURCE_LIMIT / 队列等待）
//
// Qoder 上游的业务错误是三层 JSON 套娃：
//
//	{"code":"403","message":"{\"code\":\"10605\",\"message\":
//	  \"{\"isQueued\":true,\"modelKey\":\"qfmodel\",...\",\"subCode\":\"RESOURCE_LIMIT\"}"}
//
// 直接透传会给客户端一大段转义 JSON。这里解析出关键字段并生成可读消息。
// ============================================================================

// upstreamErrorDetail 上游业务错误的最内层字段。
type upstreamErrorDetail struct {
	IsQueued          bool   `json:"isQueued"`
	ModelKey          string `json:"modelKey"`
	QueueCount        int    `json:"queueCount"`
	QueueType         string `json:"queueType"`
	RetryAfterSeconds int    `json:"retryAfterSeconds"`
	ServiceAvailable  bool   `json:"serviceAvailable"`
	SubCode           string `json:"subCode"`
	WaitTime          int    `json:"waitTime"`
}

// parseUpstreamNestedError 尝试解析三层嵌套的上游错误。
// 返回 (detail, ok)：ok=false 表示不是该格式（或解析失败），调用方应走原逻辑。
func parseUpstreamNestedError(detail string) (*upstreamErrorDetail, bool) {
	detail = strings.TrimSpace(detail)
	// 快速筛：含 subCode 字段名或 RESOURCE_LIMIT 关键字才尝试完整解析，
	// 避免对每个错误都做三次 JSON 反序列化。
	// 注意：内层 JSON 嵌在外层 message 字符串里，字段名的引号被转义成 \"subCode\"，
	// 故这里匹配不带引号的 subCode——否则 CALL_COUNT_LIMIT 等非 RESOURCE_LIMIT
	// 的嵌套报文会在预筛选阶段被漏掉（isQueued/retryAfter 来不及解析）。
	if !strings.Contains(detail, "subCode") && !strings.Contains(detail, "RESOURCE_LIMIT") {
		return nil, false
	}
	// detail 可能带 "upstream 403: " 之类前缀，从第一个 "{" 开始才是 JSON。
	if idx := strings.IndexByte(detail, '{'); idx > 0 {
		detail = detail[idx:]
	}
	// 外层：{"code":"403","message":"<json string>"}
	var outer map[string]interface{}
	if err := json.Unmarshal([]byte(detail), &outer); err != nil {
		return nil, false
	}
	msg1, ok := outer["message"].(string)
	if !ok {
		return nil, false
	}
	// 中层：{"code":"10605","message":"<json string>","subCode":"..."}
	var inner map[string]interface{}
	if err := json.Unmarshal([]byte(msg1), &inner); err != nil {
		return nil, false
	}
	msg2, ok := inner["message"].(string)
	if !ok {
		return nil, false
	}
	// 内层：业务字段
	var d upstreamErrorDetail
	if err := json.Unmarshal([]byte(msg2), &d); err != nil {
		return nil, false
	}
	if d.SubCode == "" {
		return nil, false
	}
	return &d, true
}

// resourceLimitMessage 把 RESOURCE_LIMIT / 队列等待错误转成对客户端可读的消息。
// 返回 (message, errType, handled)。
func resourceLimitMessage(d *upstreamErrorDetail) (string, string, bool) {
	model := d.ModelKey
	if model == "" {
		model = "(未指明)"
	}

	switch {
	// 已进入队列 / 上游明确要求稍后重试（isQueued 或给出 retryAfterSeconds）：
	// 属拥塞，非权限。即便 serviceAvailable=false，只要在队列或带 retryAfterSeconds，
	// 就按「排队」处理——换号无益（同模型同队列），应等待重试（见 streamWithSlot）。
	case d.IsQueued || d.RetryAfterSeconds > 0:
		wait := d.WaitTime
		if wait <= 0 {
			wait = d.RetryAfterSeconds
		}
		var b strings.Builder
		fmt.Fprintf(&b, "模型 %s 当前使用人数较多，正在排队", model)
		if d.QueueCount > 0 {
			fmt.Fprintf(&b, "（前方约 %d 人）", d.QueueCount)
		}
		if wait > 0 {
			fmt.Fprintf(&b, "，预计还需等待约 %d 秒", wait)
		}
		b.WriteString("：上游资源紧张，网关已自动等待重试；若仍未成功请稍后重试或改用其他模型。")
		return b.String(), ErrTypeUpstreamQueue, true

	// 未排队且服务不可用：账号无权使用该模型，确定性拒绝。
	case !d.ServiceAvailable:
		msg := fmt.Sprintf(
			"模型 %s 对当前账号不可用（上游 RESOURCE_LIMIT）："+
				"该模型可能不在账号套餐范围内、额度不足，或未覆盖到该账号。"+
				"重试或切换到其他账号无效，请改用其他模型或在官方客户端确认账号权限。",
			model)
		return msg, ErrTypeResourceLimit, true

	// 其他组合：保守按资源限制处理
	default:
		msg := fmt.Sprintf(
			"模型 %s 上游资源受限（RESOURCE_LIMIT，queueType=%s，serviceAvailable=%v）："+
				"请稍后重试或改用其他模型。",
			model, d.QueueType, d.ServiceAvailable)
		return msg, ErrTypeResourceLimit, true
	}
}

// isUpstreamQueue 判定错误是否为上游队列拥塞（可等待重试、换号无益）。
func isUpstreamQueue(err error) bool {
	var ue *UpstreamError
	return errors.As(err, &ue) && ue.ErrType == ErrTypeUpstreamQueue
}

// queueRetryAfter 从队列错误解析等待时长：优先上游 retryAfterSeconds，
// 夹在 [1s, QueueWaitCap]；上游未给则用 QueueWaitDefault。
func queueRetryAfter(err error) time.Duration {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		if d, ok := retryAfterFromDetail(ue.Detail); ok {
			if d < time.Second {
				return time.Second
			}
			if d > QueueWaitCap {
				return QueueWaitCap
			}
			return d
		}
	}
	return QueueWaitDefault
}

// FriendlyUpstreamError 把上游错误转成对客户端可读的消息；瞬时故障给出重试指引。
// 返回 (message, errType)。与 hub friendly_upstream_error 逐句对齐。
func FriendlyUpstreamError(code int, detail string) (string, string) {
	detail = strings.TrimSpace(detail)
	// 0) 上游业务错误（三层嵌套）：RESOURCE_LIMIT / 队列等待
	if d, ok := parseUpstreamNestedError(detail); ok {
		if msg, et, handled := resourceLimitMessage(d); handled {
			return msg, et
		}
	}
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
	if errors.Is(err, ErrNoAccount) {
		return "所有账号当前均不可用（额度 / 频控 / 故障冷却中），请稍后重试", ErrTypeNoAccount
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
//   - 账号池全冷却 → 503（服务暂时不可用，客户端应稍后重试）
//   - 其余结构化错误 → 透传上游合法 4xx/5xx
//   - 无结构化状态 → 500
func ErrorStatus(err error) int {
	if errors.Is(err, ErrNoAccount) {
		return 503
	}
	var ue *UpstreamError
	if errors.As(err, &ue) {
		switch ue.ErrType {
		case ErrTypeContentPolicy:
			return 400
		case ErrTypeTransient:
			return 502
		case ErrTypeResourceLimit:
			// 账号对模型无权限：确定性拒绝，映射 403 让客户端明确是权限/范围问题
			return 403
		case ErrTypeUpstreamQueue:
			// 上游拥塞：映射 503，客户端应稍后重试
			return 503
		}
		if ue.Status >= 400 && ue.Status <= 599 {
			return ue.Status
		}
	}
	return 500
}
