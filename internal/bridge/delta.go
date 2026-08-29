package bridge

import (
	"qoder2api/internal/cosy"
	"encoding/json"
	"fmt"

	"qoder2api/logger"
)

type Delta struct {
	Role         string
	Content      string
	Reasoning    string // 上游推理过程（reasoning_content）
	ToolCalls    []interface{}
	InputTokens  int
	OutputTokens int
	Err          error // 上游返回业务错误时非 nil
}

func (d Delta) isEmpty() bool {
	return d.Role == "" && d.Content == "" && d.Reasoning == "" && d.ToolCalls == nil && d.InputTokens == 0 && d.OutputTokens == 0 && d.Err == nil
}

func ExtractDelta(dataLine string) Delta {
	var wrapper map[string]interface{}
	if err := json.Unmarshal([]byte(dataLine), &wrapper); err != nil {
		logger.Debug("[delta] unmarshal wrapper failed: %v, raw=%s", err, dataLine)
		return Delta{}
	}
	inner, _ := wrapper["body"].(string)
	if inner == "" {
		logger.Debug("[delta] no body field, keys=%v, raw=%s", mapKeys(wrapper), dataLine)
		return Delta{}
	}
	var innerJSON map[string]interface{}
	if err := json.Unmarshal([]byte(inner), &innerJSON); err != nil {
		logger.Debug("[delta] unmarshal inner failed: %v", err)
		return Delta{}
	}
	// 先检查顶层 usage（上游可能在最后一个 chunk 中与 choices 一起返回）
	if usage, ok := innerJSON["usage"].(map[string]interface{}); ok {
		in := int(cosy.FloatVal(usage, "prompt_tokens"))
		out := int(cosy.FloatVal(usage, "completion_tokens"))
		if in > 0 || out > 0 {
			return Delta{InputTokens: in, OutputTokens: out}
		}
	}
	choices, _ := innerJSON["choices"].([]interface{})
	for _, ch := range choices {
		chMap, _ := ch.(map[string]interface{})
		delta, _ := chMap["delta"].(map[string]interface{})
		if delta == nil {
			continue
		}
		role, _ := delta["role"].(string)
		content, _ := delta["content"].(string)
		reasoning, _ := delta["reasoning_content"].(string)
		var toolCalls []interface{}
		if tc, ok := delta["tool_calls"].([]interface{}); ok && len(tc) > 0 {
			toolCalls = tc
		}
		if role != "" || content != "" || reasoning != "" || toolCalls != nil {
			return Delta{Role: role, Content: content, Reasoning: reasoning, ToolCalls: toolCalls}
		}
	}
	// 上游业务错误：{"code":"115","message":"..."}
	if code, ok := innerJSON["code"].(string); ok && code != "" && code != "0" {
		msg, _ := innerJSON["message"].(string)
		err := fmt.Errorf("upstream error code=%s: %s", code, msg)
		logger.Error("[delta] %v", err)
		return Delta{Err: err}
	}
	logger.Debug("[delta] no valid choices found, inner=%s", inner)
	return Delta{}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func mapKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
