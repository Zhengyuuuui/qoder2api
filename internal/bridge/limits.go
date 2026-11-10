package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"qoder2api/logger"
)

// 请求体积前置限制 + 工具结果截断 + max_tokens 上限保护。
//
// 为什么在本地拦而不是让上游拒：超大请求会被上游直接 RST / 断连，客户端
// 只看到「broken pipe」，还会连锁触发熔断把好账号一起误伤；超长工具结果
// 每轮都要重发整个上下文，是单请求体积的最大放大器。本地提前拦下，
// 失败是可读的 413 / 可识别的截断标记，代价明确、无连锁。

const (
	// DefaultMaxBodyMB 默认请求体上限（MB）。
	DefaultMaxBodyMB = 29
	// DefaultToolResultBytes 默认单条工具结果截断上限（字节）。
	DefaultToolResultBytes = 32 * 1024
	// DefaultToolResultTotalBytes 默认单次请求工具结果总和上限（字节）。
	DefaultToolResultTotalBytes = 128 * 1024
	// DefaultMaxOutputTokens 默认 max_tokens 上限。
	DefaultMaxOutputTokens = 32768
)

// toolResultScopeEnv 截断范围开关：tail（默认，保留尾部——尾部通常是
// 结论）/ all（保留头部）/ off（不截断，排障用）。
const toolResultScopeEnv = "QD_TOOL_RESULT_SCOPE"

// EffectiveMaxBodyBytes 请求体上限（字节）。
func EffectiveMaxBodyBytes() int64 {
	mb := DefaultMaxBodyMB
	if st := loadSettingsOrNil(); st != nil && st.MaxBodyMB > 0 {
		mb = st.MaxBodyMB
	}
	return int64(mb) * 1024 * 1024
}

func effectiveToolResultBytes() int {
	if st := loadSettingsOrNil(); st != nil && st.ToolResultBytes > 0 {
		return st.ToolResultBytes
	}
	return DefaultToolResultBytes
}

func effectiveToolResultTotalBytes() int {
	if st := loadSettingsOrNil(); st != nil && st.ToolResultTotalBytes > 0 {
		return st.ToolResultTotalBytes
	}
	return DefaultToolResultTotalBytes
}

// EffectiveMaxOutputTokens max_tokens 上限保护值。
func EffectiveMaxOutputTokens() int {
	if st := loadSettingsOrNil(); st != nil && st.MaxOutputTokens > 0 {
		return st.MaxOutputTokens
	}
	return DefaultMaxOutputTokens
}

// ClampMaxTokens 把客户端请求的 max_tokens 收敛到上限内。
// 超限时下调并打日志（不拒请求——客户端往往只是没设好，直接拒会把
// 可用的请求变成错误）。返回 (生效值, 是否被下调)。
func ClampMaxTokens(requested int) (int, bool) {
	cap := EffectiveMaxOutputTokens()
	if requested <= 0 || requested <= cap {
		return requested, false
	}
	logger.Info("[max_tokens] clamped %d -> %d (limit; client value too large)", requested, cap)
	return cap, true
}

// BodyLimitMiddleware 请求体体积前置限制。
//
// 两条路径都不把超大 body 读进内存：
//   - 有 Content-Length 且超限：直接 413，一个字节都不读；
//   - 无 Content-Length（chunked）：LimitReader 只读 上限+1 字节，
//     超出即拒——内存占用有界。
func BodyLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil || r.Body == http.NoBody || r.Method == http.MethodGet || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		limit := EffectiveMaxBodyBytes()
		if r.ContentLength > limit {
			writeBodyTooLarge(w, limit)
			return
		}
		if r.ContentLength > 0 {
			// 长度已知且未超限：放行。仍包 MaxBytesReader——客户端声明可能
			// 造假，读取侧必须有硬闸。
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			next.ServeHTTP(w, r)
			return
		}
		// chunked / 未知长度：限量读取
		data, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
		if err != nil {
			body, _ := json.Marshal(map[string]interface{}{
				"error": map[string]interface{}{
					"message": fmt.Sprintf("读取请求体失败：%v", err),
					"type":    "invalid_request",
				},
			})
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			w.Write(body)
			return
		}
		if int64(len(data)) > limit {
			writeBodyTooLarge(w, limit)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		r.ContentLength = int64(len(data))
		next.ServeHTTP(w, r)
	})
}

// writeBodyTooLarge 413 响应：必须可读——说明超了多少、怎么处理，
// 不能让客户端对着一句 "too large" 猜。
func writeBodyTooLarge(w http.ResponseWriter, limit int64) {
	msg := fmt.Sprintf(
		"请求体超过 %.1f MB 上限。请拆分请求：减少图片数量、截断历史消息、或分批发送。",
		float64(limit)/1024/1024)
	body, _ := json.Marshal(map[string]interface{}{
		"error": map[string]interface{}{
			"message": msg,
			"type":    "request_too_large",
			"limit":   limit,
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusRequestEntityTooLarge)
	w.Write(body)
}

// toolResultScope 当前截断范围（tail / all / off）。
func toolResultScope() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(toolResultScopeEnv))) {
	case "off":
		return "off"
	case "all":
		return "all"
	default:
		return "tail"
	}
}

// truncateMarker 截断标记：必须可识别——静默丢弃会让模型以为上下文完整，
// 对着被截掉的内容推理。
func truncateMarker(cut int) string {
	return fmt.Sprintf("\n[…truncated %d bytes…]", cut)
}

// TruncateToolResult 按范围截断单条工具结果（rune 安全，不切断多字节字符）。
func TruncateToolResult(s string, limit int) string {
	if limit <= 0 {
		limit = effectiveToolResultBytes()
	}
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	cut := len(r) - limit
	switch toolResultScope() {
	case "off":
		return s
	case "all":
		return string(r[:limit]) + truncateMarker(cut)
	default: // tail：保留尾部（结论通常在尾部），头部留说明
		return truncateMarker(cut) + "\n" + string(r[cut:])
	}
}

// ApplyToolResultBudgets 对一组工具结果（按发生顺序，旧→新）施加
// 「单条上限 + 总和上限」。总和超限时按「旧的先截」分配预算：
// 从最新往最旧分配，旧结果先被压到最小——最新的工具结果往往对应
// 当前轮次，信息价值最高。
func ApplyToolResultBudgets(texts []string) []string {
	if toolResultScope() == "off" || len(texts) == 0 {
		return texts
	}
	itemLimit := effectiveToolResultBytes()
	totalLimit := effectiveToolResultTotalBytes()

	// 从最新往最旧分配总预算
	limits := make([]int, len(texts))
	remaining := totalLimit
	for i := len(texts) - 1; i >= 0; i-- {
		l := itemLimit
		if remaining < l {
			l = remaining
		}
		if l < 0 {
			l = 0
		}
		limits[i] = l
		remaining -= len([]rune(texts[i]))
		if remaining < 0 {
			remaining = 0
		}
	}

	out := make([]string, len(texts))
	for i, t := range texts {
		out[i] = TruncateToolResult(t, limits[i])
	}
	return out
}
