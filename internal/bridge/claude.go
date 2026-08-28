package bridge

import (
	"qoder2api/internal/cosy"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"qoder2api/logger"
)

func (b *Bridge) HandleClaudeMessages(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	if r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		WriteClaudeErr(w, err)
		return
	}

	reqID := cosy.NewUUID()[:8]
	logger.Info("[Claude][%s] 收到请求 %s %s body_size=%d", reqID, r.Method, r.URL.Path, len(body))
	logger.Debug("[Claude][%s] 请求体（敏感字段已脱敏）: %s", reqID, RedactRequestBodyJSON(body))

	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		WriteClaudeErr(w, err)
		return
	}
	stream, _ := req["stream"].(bool)
	model := StrValDefault(req, "model", "lite")
	incomingMsgs, _ := req["messages"].([]interface{})

	// Claude format: tools use name+input_schema directly
	// Convert to OpenAI-style for Qoder upstream
	tools := ConvertClaudeToolsToOpenAI(req["tools"])
	toolsEnabled := tools != nil

	// If there's a system field, prepend it as a system message
	if sys := req["system"]; sys != nil {
		sysText := ""
		switch v := sys.(type) {
		case string:
			sysText = v
		case []interface{}:
			for _, block := range v {
				if bk, ok := block.(map[string]interface{}); ok {
					if t, ok := bk["text"].(string); ok {
						if sysText != "" {
							sysText += "\n"
						}
						sysText += t
					}
				}
			}
		}
		if sysText != "" {
			sysMsg := map[string]interface{}{"role": "system", "content": sysText}
			incomingMsgs = append([]interface{}{sysMsg}, incomingMsgs...)
		}
	}

	prompt := ExtractLatestUserPrompt(incomingMsgs)
	messages := BuildQoderMessages(b.templateMessages(), incomingMsgs, prompt, toolsEnabled)

	// 解析 thinking 参数和 max_tokens
	opts := CallOpts{}
	if thinking, ok := req["thinking"].(map[string]interface{}); ok {
		if tp, _ := thinking["type"].(string); tp == "enabled" {
			opts.IsReasoning = true
		}
	}
	if mt, ok := req["max_tokens"].(float64); ok && int(mt) > 0 {
		opts.MaxTokens = int(mt)
	}

	logger.Info("[Claude][%s] model=%s stream=%v tools=%v thinking=%v max_tokens=%d msgs=%d", reqID, model, stream, toolsEnabled, opts.IsReasoning, opts.MaxTokens, len(incomingMsgs))

	msgId := "msg_" + cosy.NewRequestID()
	ctx := r.Context()

	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)

		writeSse := func(eventType string, data interface{}) {
			d, _ := json.Marshal(data)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, string(d))
			if flusher != nil {
				flusher.Flush()
			}
		}

		writeSse("message_start", map[string]interface{}{
			"type": "message_start",
			"message": map[string]interface{}{
				"id": msgId, "type": "message", "role": "assistant", "model": model,
				"stop_reason": nil, "stop_sequence": nil,
				"content": []interface{}{},
				"usage":   map[string]interface{}{"input_tokens": 0, "output_tokens": 0},
			},
		})
		writeSse("content_block_start", map[string]interface{}{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]interface{}{"type": "text", "text": ""},
		})
		writeSse("ping", map[string]interface{}{"type": "ping"})

		// toolCallMerged: key=index, value=merged tool call
		toolCallMerged := map[int]map[string]interface{}{}
		contentBlockIndex := 0
		streamContentLen := 0
		var totalInputTokens, totalOutputTokens int

		// thinking block 状态追踪
		thinkingBlockOpen := false
		thinkingBuf := ""

		err = b.CallQoderWithOpts(ctx, "claude", messages, model, tools, opts, func(d Delta) {
			if d.Err != nil {
				return
			}
			if d.InputTokens > 0 || d.OutputTokens > 0 {
				totalInputTokens = d.InputTokens
				totalOutputTokens = d.OutputTokens
			}
			if d.Reasoning != "" && opts.IsReasoning {
				if !thinkingBlockOpen {
					// 先关闭默认的 text block，再开 thinking block
					writeSse("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": contentBlockIndex})
					contentBlockIndex++
					writeSse("content_block_start", map[string]interface{}{
						"type": "content_block_start", "index": contentBlockIndex,
						"content_block": map[string]interface{}{"type": "thinking", "thinking": ""},
					})
					thinkingBlockOpen = true
				}
				thinkingBuf += d.Reasoning
				writeSse("content_block_delta", map[string]interface{}{
					"type": "content_block_delta", "index": contentBlockIndex,
					"delta": map[string]interface{}{"type": "thinking_delta", "thinking": d.Reasoning},
				})
			}
			if d.Content != "" {
				if thinkingBlockOpen {
					// 关闭 thinking block，开新 text block
					writeSse("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": contentBlockIndex})
					contentBlockIndex++
					writeSse("content_block_start", map[string]interface{}{
						"type": "content_block_start", "index": contentBlockIndex,
						"content_block": map[string]interface{}{"type": "text", "text": ""},
					})
					thinkingBlockOpen = false
				}
				streamContentLen += len(d.Content)
				writeSse("content_block_delta", map[string]interface{}{
					"type": "content_block_delta", "index": contentBlockIndex,
					"delta": map[string]interface{}{"type": "text_delta", "text": d.Content},
				})
			}
			if d.ToolCalls != nil {
				MergeToolCallChunks(toolCallMerged, d.ToolCalls)
			}
		})
		if err != nil {
			logger.Error("[Claude][%s] stream 请求失败: %v (耗时 %dms)", reqID, err, time.Since(startTime).Milliseconds())
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
			if flusher != nil {
				flusher.Flush()
			}
			return
		}

		// Close text block
		writeSse("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": contentBlockIndex})

		// If there were tool calls, emit them as tool_use blocks
		mergedTools := SortedToolCalls(toolCallMerged)
		for i, tcMap := range mergedTools {
			blockIdx := contentBlockIndex + 1 + i
			fn, _ := tcMap["function"].(map[string]interface{})
			toolId, _ := tcMap["id"].(string)
			if toolId == "" {
				toolId = "toolu_" + cosy.NewRequestID()
			}
			name, _ := fn["name"].(string)
			argsStr, _ := fn["arguments"].(string)
			var input interface{}
			json.Unmarshal([]byte(argsStr), &input)
			if input == nil {
				input = map[string]interface{}{}
			}

			writeSse("content_block_start", map[string]interface{}{
				"type": "content_block_start", "index": blockIdx,
				"content_block": map[string]interface{}{
					"type": "tool_use", "id": toolId, "name": name, "input": map[string]interface{}{},
				},
			})
			writeSse("content_block_delta", map[string]interface{}{
				"type": "content_block_delta", "index": blockIdx,
				"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": argsStr},
			})
			writeSse("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": blockIdx})
		}

		stopReason := "end_turn"
		if len(mergedTools) > 0 {
			stopReason = "tool_use"
		}

		writeSse("message_delta", map[string]interface{}{
			"type":  "message_delta",
			"delta": map[string]interface{}{"stop_reason": stopReason, "stop_sequence": nil},
			"usage": map[string]interface{}{"input_tokens": totalInputTokens, "output_tokens": totalOutputTokens},
		})
		writeSse("message_stop", map[string]interface{}{"type": "message_stop"})
		logger.Info("[Claude][%s] stream 完成 stop=%s content_len=%d tool_calls=%d 耗时=%dms", reqID, stopReason, streamContentLen, len(mergedTools), time.Since(startTime).Milliseconds())
	} else {
		var full strings.Builder
		var reasoningBuf strings.Builder
		var toolCallBuf []interface{}
		var totalInputTokens, totalOutputTokens int
		err = b.CallQoderWithOpts(ctx, "claude", messages, model, tools, opts, func(d Delta) {
			if d.Err != nil {
				return
			}
			if d.InputTokens > 0 || d.OutputTokens > 0 {
				totalInputTokens = d.InputTokens
				totalOutputTokens = d.OutputTokens
			}
			if d.Reasoning != "" {
				reasoningBuf.WriteString(d.Reasoning)
			}
			if d.Content != "" {
				full.WriteString(d.Content)
			}
			if d.ToolCalls != nil {
				toolCallBuf = append(toolCallBuf, d.ToolCalls...)
			}
		})
		if err != nil {
			logger.Error("[Claude][%s] 请求失败: %v (耗时 %dms)", reqID, err, time.Since(startTime).Milliseconds())
			WriteClaudeErr(w, fmt.Errorf("request failed: %w", err))
			return
		}

		content := []interface{}{}
		if reasoningBuf.Len() > 0 && opts.IsReasoning {
			content = append(content, map[string]interface{}{"type": "thinking", "thinking": reasoningBuf.String()})
		}
		if full.Len() > 0 {
			content = append(content, map[string]interface{}{"type": "text", "text": full.String()})
		}
		for _, tc := range toolCallBuf {
			tcMap, _ := tc.(map[string]interface{})
			if tcMap == nil {
				continue
			}
			fn, _ := tcMap["function"].(map[string]interface{})
			toolId, _ := tcMap["id"].(string)
			if toolId == "" {
				toolId = "toolu_" + cosy.NewRequestID()
			}
			name, _ := fn["name"].(string)
			argsStr, _ := fn["arguments"].(string)
			var input interface{}
			json.Unmarshal([]byte(argsStr), &input)
			if input == nil {
				input = map[string]interface{}{}
			}
			content = append(content, map[string]interface{}{
				"type": "tool_use", "id": toolId, "name": name, "input": input,
			})
		}

		stopReason := "end_turn"
		if len(toolCallBuf) > 0 {
			stopReason = "tool_use"
		}

		resp := map[string]interface{}{
			"id": msgId, "type": "message", "role": "assistant", "model": model,
			"stop_reason": stopReason, "stop_sequence": nil,
			"content": content,
			"usage":   map[string]interface{}{"input_tokens": totalInputTokens, "output_tokens": totalOutputTokens},
		}
		logger.Info("[Claude][%s] 完成 stop=%s content_len=%d tool_calls=%d 耗时=%dms", reqID, stopReason, full.Len(), len(toolCallBuf), time.Since(startTime).Milliseconds())
		logger.Debug("[Claude][%s] 响应体: %s", reqID, func() string { d, _ := json.Marshal(resp); return string(d) }())
		WriteJSON(w, resp)
	}
}

// HandleListModels 实现 GET /v1/models，返回 Claude API 格式的模型列表。
// Claude Code 通过此接口获取 context_window 大小，用于决定何时触发 compact（80% 阈值）。
func (b *Bridge) HandleListModels(w http.ResponseWriter, r *http.Request) {
	models, err := b.ListAvailableModels()
	if err != nil {
		logger.Error("[models] listAvailableModels failed: %v, using fallback", err)
		models = nil
	}
	const fallbackContextWindow = 180000
	const defaultMaxOutputTokens = 16384
	data := make([]interface{}, 0, len(models))
	for _, m := range models {
		ctxWin := m.MaxInputTokens
		if ctxWin == 0 {
			ctxWin = fallbackContextWindow
		}
		maxOut := defaultMaxOutputTokens
		if m.IsReasoning {
			maxOut = 32768
		}
		entry := map[string]interface{}{
			"type":              "model",
			"id":               m.Key,
			"display_name":     m.DisplayName,
			"created_at":       "2024-01-01T00:00:00Z",
			"context_window":   ctxWin,
			"max_output_tokens": maxOut,
		}
		if m.IsReasoning {
			entry["supports_thinking"] = true
		}
		data = append(data, entry)
	}
	if len(data) == 0 {
		// 兜底：返回 Qoder 已知的 assistant 模型列表
		for _, key := range []string{"auto", "ultimate", "performance", "efficient", "lite"} {
			data = append(data, map[string]interface{}{
				"type":           "model",
				"id":             key,
				"display_name":   key,
				"created_at":     "2024-01-01T00:00:00Z",
				"context_window": fallbackContextWindow,
			})
		}
	}
	WriteJSON(w, map[string]interface{}{"data": data})
}

func WriteClaudeErr(w http.ResponseWriter, err error) {
	body, _ := json.Marshal(map[string]interface{}{
		"type": "error",
		"error": map[string]interface{}{
			"type":    "api_error",
			"message": err.Error(),
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(500)
	w.Write(body)
}

// MergeToolCallChunks 将流式 tool_call 增量 chunk 按 index 合并。
func MergeToolCallChunks(merged map[int]map[string]interface{}, chunks []interface{}) {
	for _, chunk := range chunks {
		tc, ok := chunk.(map[string]interface{})
		if !ok {
			continue
		}
		idx := 0
		if idxF, ok := tc["index"].(float64); ok {
			idx = int(idxF)
		}
		existing, exists := merged[idx]
		if !exists {
			existing = map[string]interface{}{}
			merged[idx] = existing
		}
		if id, ok := tc["id"].(string); ok && id != "" {
			existing["id"] = id
		}
		if t, ok := tc["type"].(string); ok && t != "" {
			existing["type"] = t
		}
		if fn, ok := tc["function"].(map[string]interface{}); ok {
			existingFn, _ := existing["function"].(map[string]interface{})
			if existingFn == nil {
				existingFn = map[string]interface{}{}
				existing["function"] = existingFn
			}
			if name, ok := fn["name"].(string); ok && name != "" {
				existingFn["name"] = name
			}
			if args, ok := fn["arguments"].(string); ok {
				prev, _ := existingFn["arguments"].(string)
				existingFn["arguments"] = prev + args
			}
		}
	}
}

// SortedToolCalls 按 index 排序返回合并后的 tool calls。
func SortedToolCalls(merged map[int]map[string]interface{}) []map[string]interface{} {
	if len(merged) == 0 {
		return nil
	}
	maxIdx := 0
	for idx := range merged {
		if idx > maxIdx {
			maxIdx = idx
		}
	}
	result := make([]map[string]interface{}, 0, len(merged))
	for i := 0; i <= maxIdx; i++ {
		if tc, ok := merged[i]; ok {
			result = append(result, tc)
		}
	}
	return result
}

func ConvertClaudeToolsToOpenAI(raw interface{}) interface{} {
	tools, ok := raw.([]interface{})
	if !ok || len(tools) == 0 {
		return nil
	}
	converted := make([]interface{}, 0, len(tools))
	for _, t := range tools {
		tm, ok := t.(map[string]interface{})
		if !ok {
			continue
		}
		// 如果已经是 OpenAI 格式（有 type 字段），直接保留
		if _, hasType := tm["type"]; hasType {
			converted = append(converted, tm)
			continue
		}
		// Claude 格式转 OpenAI 格式
		fn := map[string]interface{}{
			"name": tm["name"],
		}
		if desc, ok := tm["description"]; ok {
			fn["description"] = desc
		}
		if schema, ok := tm["input_schema"]; ok {
			fn["parameters"] = schema
		}
		converted = append(converted, map[string]interface{}{
			"type":     "function",
			"function": fn,
		})
	}
	if len(converted) == 0 {
		return nil
	}
	return converted
}
