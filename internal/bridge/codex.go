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

func (b *Bridge) HandleCodexResponses(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	if r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		WriteCodexErr(w, err)
		return
	}

	reqID := cosy.NewUUID()[:8]
	logger.Info("[Codex][%s] 收到请求 %s %s body_size=%d", reqID, r.Method, r.URL.Path, len(body))
	logger.Debug("[Codex][%s] 请求体（敏感字段已脱敏）: %s", reqID, RedactRequestBodyJSON(body))

	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		WriteCodexErr(w, err)
		return
	}
	stream, _ := req["stream"].(bool)
	model := StrValDefault(req, "model", "lite")
	instructions, _ := req["instructions"].(string)
	tools := req["tools"]
	toolsEnabled := tools != nil

	// Convert input to messages
	incomingMsgs := CodexInputToMessages(req["input"], instructions)
	prompt := ExtractLatestUserPrompt(incomingMsgs)
	messages := BuildQoderMessages(b.templateMessages(), incomingMsgs, prompt, toolsEnabled)

	logger.Info("[Codex][%s] model=%s stream=%v tools=%v msgs=%d", reqID, model, stream, toolsEnabled, len(incomingMsgs))

	respId := "resp_" + cosy.NewRequestID()
	ctx := r.Context()

	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)

		writeEvent := func(eventType string, data interface{}) {
			d, _ := json.Marshal(data)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, string(d))
			if flusher != nil {
				flusher.Flush()
			}
		}

		writeEvent("response.created", map[string]interface{}{
			"type": "response.created",
			"response": map[string]interface{}{
				"id": respId, "model": model, "status": "in_progress",
				"output": []interface{}{},
			},
		})

		outputItemId := "msg_" + cosy.NewRequestID()
		writeEvent("response.output_item.added", map[string]interface{}{
			"type":         "response.output_item.added",
			"output_index": 0,
			"item": map[string]interface{}{
				"id": outputItemId, "type": "message", "role": "assistant",
				"status": "in_progress", "content": []interface{}{},
			},
		})

		contentPartId := "cp_" + cosy.NewRequestID()
		writeEvent("response.content_part.added", map[string]interface{}{
			"type":          "response.content_part.added",
			"item_id":       outputItemId,
			"output_index":  0,
			"content_index": 0,
			"part": map[string]interface{}{
				"type": "output_text", "text": "",
			},
		})
		_ = contentPartId

		var toolCallBuf []interface{}

		err = b.CallQoder(ctx, "codex", messages, model, tools, func(d Delta) {
			if d.Content != "" {
				writeEvent("response.output_text.delta", map[string]interface{}{
					"type":          "response.output_text.delta",
					"item_id":       outputItemId,
					"output_index":  0,
					"content_index": 0,
					"delta":         d.Content,
				})
			}
			if d.ToolCalls != nil {
				toolCallBuf = append(toolCallBuf, d.ToolCalls...)
			}
		})
		if err != nil {
			logger.Error("[Codex][%s] stream 请求失败: %v (耗时 %dms)", reqID, err, time.Since(startTime).Milliseconds())
			writeEvent("error", map[string]interface{}{
				"type":    "error",
				"message": err.Error(),
			})
			return
		}

		writeEvent("response.output_text.done", map[string]interface{}{
			"type":          "response.output_text.done",
			"item_id":       outputItemId,
			"output_index":  0,
			"content_index": 0,
		})

		// Emit tool calls as function_call items if any
		for i, tc := range toolCallBuf {
			tcMap, _ := tc.(map[string]interface{})
			if tcMap == nil {
				continue
			}
			fn, _ := tcMap["function"].(map[string]interface{})
			callId, _ := tcMap["id"].(string)
			if callId == "" {
				callId = "call_" + cosy.NewRequestID()
			}
			name, _ := fn["name"].(string)
			args, _ := fn["arguments"].(string)

			fcItemId := "fc_" + cosy.NewRequestID()
			writeEvent("response.output_item.added", map[string]interface{}{
				"type":         "response.output_item.added",
				"output_index": i + 1,
				"item": map[string]interface{}{
					"id": fcItemId, "type": "function_call",
					"call_id": callId, "name": name, "arguments": "",
					"status": "in_progress",
				},
			})
			writeEvent("response.function_call_arguments.delta", map[string]interface{}{
				"type":         "response.function_call_arguments.delta",
				"item_id":      fcItemId,
				"output_index": i + 1,
				"delta":        args,
			})
			writeEvent("response.function_call_arguments.done", map[string]interface{}{
				"type":         "response.function_call_arguments.done",
				"item_id":      fcItemId,
				"output_index": i + 1,
				"arguments":    args,
			})
		}

		status := "completed"
		writeEvent("response.completed", map[string]interface{}{
			"type": "response.completed",
			"response": map[string]interface{}{
				"id": respId, "model": model, "status": status,
				"usage": map[string]interface{}{
					"input_tokens": 0, "output_tokens": 0, "total_tokens": 0,
				},
			},
		})
		logger.Info("[Codex][%s] stream 完成 tool_calls=%d 耗时=%dms", reqID, len(toolCallBuf), time.Since(startTime).Milliseconds())
	} else {
		var full strings.Builder
		var toolCallBuf []interface{}
		err = b.CallQoder(ctx, "codex", messages, model, tools, func(d Delta) {
			if d.Content != "" {
				full.WriteString(d.Content)
			}
			if d.ToolCalls != nil {
				toolCallBuf = append(toolCallBuf, d.ToolCalls...)
			}
		})
		if err != nil {
			logger.Error("[Codex][%s] 请求失败: %v (耗时 %dms)", reqID, err, time.Since(startTime).Milliseconds())
			WriteCodexErr(w, fmt.Errorf("request failed: %w", err))
			return
		}

		output := []interface{}{}
		if full.Len() > 0 {
			output = append(output, map[string]interface{}{
				"type": "message", "role": "assistant",
				"content": []interface{}{
					map[string]interface{}{"type": "output_text", "text": full.String()},
				},
			})
		}
		for _, tc := range toolCallBuf {
			tcMap, _ := tc.(map[string]interface{})
			if tcMap == nil {
				continue
			}
			fn, _ := tcMap["function"].(map[string]interface{})
			callId, _ := tcMap["id"].(string)
			if callId == "" {
				callId = "call_" + cosy.NewRequestID()
			}
			name, _ := fn["name"].(string)
			args, _ := fn["arguments"].(string)
			output = append(output, map[string]interface{}{
				"type": "function_call", "call_id": callId,
				"name": name, "arguments": args,
			})
		}

		resp := map[string]interface{}{
			"id": respId, "model": model, "status": "completed",
			"output": output,
			"usage": map[string]interface{}{
				"input_tokens": 0, "output_tokens": 0, "total_tokens": 0,
			},
		}
		logger.Info("[Codex][%s] 完成 content_len=%d tool_calls=%d 耗时=%dms", reqID, full.Len(), len(toolCallBuf), time.Since(startTime).Milliseconds())
		logger.Debug("[Codex][%s] 响应体: %s", reqID, func() string { d, _ := json.Marshal(resp); return string(d) }())
		WriteJSON(w, resp)
	}
}

func CodexInputToMessages(input interface{}, instructions string) []interface{} {
	var msgs []interface{}

	if instructions != "" {
		msgs = append(msgs, map[string]interface{}{"role": "system", "content": instructions})
	}

	switch v := input.(type) {
	case string:
		msgs = append(msgs, map[string]interface{}{"role": "user", "content": v})
	case []interface{}:
		for _, item := range v {
			itemMap, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			itemType, _ := itemMap["type"].(string)
			switch itemType {
			case "message":
				role, _ := itemMap["role"].(string)
				content := itemMap["content"]
				if contentStr, ok := content.(string); ok {
					msgs = append(msgs, map[string]interface{}{"role": role, "content": contentStr})
				} else if contentArr, ok := content.([]interface{}); ok {
					var text string
					for _, block := range contentArr {
						if bm, ok := block.(map[string]interface{}); ok {
							if t, ok := bm["text"].(string); ok {
								if text != "" {
									text += "\n"
								}
								text += t
							}
						}
					}
					msgs = append(msgs, map[string]interface{}{"role": role, "content": text})
				}
			case "function_call":
				name, _ := itemMap["name"].(string)
				args, _ := itemMap["arguments"].(string)
				callId, _ := itemMap["call_id"].(string)
				msgs = append(msgs, map[string]interface{}{
					"role": "assistant", "content": "",
					"tool_calls": []interface{}{
						map[string]interface{}{
							"id": callId, "type": "function",
							"function": map[string]interface{}{"name": name, "arguments": args},
						},
					},
				})
			case "function_call_output":
				callId, _ := itemMap["call_id"].(string)
				output, _ := itemMap["output"].(string)
				msgs = append(msgs, map[string]interface{}{
					"role": "tool", "tool_call_id": callId, "content": output,
				})
			}
		}
	}

	return msgs
}

func WriteCodexErr(w http.ResponseWriter, err error) {
	body, _ := json.Marshal(map[string]interface{}{
		"error": map[string]interface{}{"message": err.Error(), "type": "server_error"},
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(500)
	w.Write(body)
}
