package bridge

import (
	"context"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// CallRecord 是一次「模型调用」的结构化记录。
//
// 与 logger 的原始行日志不同，这里按字段拆分，供控制台渲染成可读表格：
// 每次客户端请求（chat / claude / codex）最终落到一次或多次上游调用，
// 记录实际使用的账号、映射后的模型、token 用量、耗时与结果。
//
// 仅保留最近 N 条（内存环形缓冲，不落盘），重启清空。
type CallRecord struct {
	Seq             int       `json:"seq"`
	Time            time.Time `json:"time"`           // 请求开始时间
	RequestID       string    `json:"request_id"`     // 与原始日志 [Chat][xxxx] 对应的短 id
	Protocol        string    `json:"protocol"`       // chat | claude | codex
	Agent           string    `json:"agent"`          // claude | codex | gemini
	Model           string    `json:"model"`          // 客户端请求的模型名
	UpstreamModel   string    `json:"upstream_model"` // 映射后的上游 model.key
	Account         string    `json:"account"`        // 最终使用的账号名
	AccountID       string    `json:"account_id"`     // 账号 id（排查用）
	Region          string    `json:"region"`         // cn | global
	Stream          bool      `json:"stream"`         // 客户端是否要求流式
	Messages        int       `json:"messages"`       // 消息条数（上游请求体）
	Tools           bool      `json:"tools"`          // 是否携带工具定义
	PromptPreview   string    `json:"prompt_preview"` // 最后一条 user 输入的前 N 个字符
	Attempts        int       `json:"attempts"`       // 实际尝试过的账号数（>1 表示发生换号）
	Status          string    `json:"status"`         // ok | error | cancelled
	ErrorType       string    `json:"error_type,omitempty"`
	ErrorMessage    string    `json:"error_message,omitempty"`
	InputTokens     int       `json:"input_tokens"`
	OutputTokens    int       `json:"output_tokens"`
	CachedTokens    int       `json:"cached_tokens"`    // 上游 prompt 缓存命中 token 数
	ReasoningTokens int       `json:"reasoning_tokens"` // 其中推理 token（completion 明细）
	Credits         float64   `json:"credits"`          // 本次计费 credits
	OriginalCredits float64   `json:"original_credits"` // 优惠前 credits（不同即命中促销）
	DurationMS      int64     `json:"duration_ms"`
	HadToolCalls    bool      `json:"had_tool_calls"` // 上游是否返回了工具调用
	Emitted         bool      `json:"emitted"`        // 是否向客户端发过内容
}

// 调用结果枚举
const (
	CallStatusOK        = "ok"
	CallStatusError     = "error"
	CallStatusCancelled = "cancelled" // 客户端提前断开
)

// CallMeta 由各协议 handler 在入口处注入，CallQoder 记录下来源信息。
type CallMeta struct {
	RequestID string
	Protocol  string // chat | claude | codex
	Stream    bool
}

type callMetaKey struct{}

// WithCallMeta 把请求来源信息挂到 context。bridge 包内各 handler 入口调用。
func WithCallMeta(ctx context.Context, m CallMeta) context.Context {
	return context.WithValue(ctx, callMetaKey{}, m)
}

func callMetaFrom(ctx context.Context) CallMeta {
	if ctx == nil {
		return CallMeta{}
	}
	if m, ok := ctx.Value(callMetaKey{}).(CallMeta); ok {
		return m
	}
	return CallMeta{}
}

// callLog 固定容量环形缓冲（最新在前返回）。
type callLog struct {
	mu      sync.Mutex
	cap     int
	nextSeq int
	entries []CallRecord
}

const callLogCapacity = 200

var recentCallLog = &callLog{cap: callLogCapacity, nextSeq: 1}

func (c *callLog) record(rec CallRecord) {
	if rec.Time.IsZero() {
		rec.Time = time.Now()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	rec.Seq = c.nextSeq
	c.nextSeq++
	if len(c.entries) >= c.cap {
		// 淘汰最旧一条（整体前移，保持容量）
		copy(c.entries, c.entries[1:])
		c.entries[len(c.entries)-1] = rec
		return
	}
	c.entries = append(c.entries, rec)
}

// snapshot 返回最近 limit 条（最新在前）；limit<=0 返回全部。
func (c *callLog) snapshot(limit int) []CallRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(c.entries)
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]CallRecord, limit)
	for i := 0; i < limit; i++ {
		out[i] = c.entries[n-1-i] // 反转：最新在前
	}
	return out
}

// RecentCalls 返回最近模型调用记录（最新在前），供控制台 API 使用。
func RecentCalls(limit int) []CallRecord {
	return recentCallLog.snapshot(limit)
}

// ClearCalls 清空调用记录（测试用）。
func ClearCalls() {
	recentCallLog.mu.Lock()
	recentCallLog.entries = nil
	recentCallLog.nextSeq = 1
	recentCallLog.mu.Unlock()
}

// truncateRunesForLog 按 rune 截断，避免切断 UTF-8；空白折叠成单空格，
// 让多行 prompt 在表格中保持一行可读。
func truncateRunesForLog(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max]) + "…"
}
