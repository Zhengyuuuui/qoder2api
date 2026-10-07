package bridge

import (
	"encoding/json"
	"strings"
)

func BuildQoderMessages(templateMsgs []interface{}, incoming []interface{}, prompt string, toolsEnabled bool) []interface{} {
	var rebuilt []interface{}

	hasIncomingSystem := false
	for _, m := range incoming {
		if mm, ok := m.(map[string]interface{}); ok && mm["role"] == "system" {
			hasIncomingSystem = true
			break
		}
	}
	if !hasIncomingSystem && templateMsgs != nil {
		for _, m := range templateMsgs {
			if mm, ok := m.(map[string]interface{}); ok && mm["role"] == "system" {
				rebuilt = append(rebuilt, DeepCopyMap(mm))
			}
		}
	}

	for _, m := range incoming {
		mm, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		// Handle multi-tool-result: a single Claude user message may contain multiple tool_results
		if role, _ := mm["role"].(string); role == "user" {
			if contentArr, ok := mm["content"].([]interface{}); ok && len(contentArr) > 0 {
				if first, ok := contentArr[0].(map[string]interface{}); ok && first["type"] == "tool_result" {
					for _, block := range contentArr {
						b, _ := block.(map[string]interface{})
						if b == nil || b["type"] != "tool_result" {
							continue
						}
						toolCallId, _ := b["tool_use_id"].(string)
						resultContent := ""
						switch c := b["content"].(type) {
						case string:
							resultContent = c
						case []interface{}:
							for _, part := range c {
								if pm, ok := part.(map[string]interface{}); ok {
									if t, ok := pm["text"].(string); ok {
										resultContent += t
									}
								}
							}
						}
						rebuilt = append(rebuilt, map[string]interface{}{
							"role":         "tool",
							"tool_call_id": toolCallId,
							"content":      resultContent,
						})
					}
					continue
				}
			}
		}
		converted := ConvertIncomingMessage(mm, toolsEnabled)
		if converted != nil {
			rebuilt = append(rebuilt, converted)
		}
	}

	if len(rebuilt) == 0 && prompt != "" {
		rebuilt = append(rebuilt, BuildUserMessage(prompt))
	}
	return rebuilt
}

func ConvertIncomingMessage(msg map[string]interface{}, toolsEnabled bool) map[string]interface{} {
	role, _ := msg["role"].(string)
	content := msg["content"]

	// Handle Claude Messages API content blocks (array of typed blocks)
	if contentArr, ok := content.([]interface{}); ok && len(contentArr) > 0 {
		firstBlock, _ := contentArr[0].(map[string]interface{})
		blockType, _ := firstBlock["type"].(string)

		// assistant message with tool_use blocks
		if role == "assistant" && HasBlockType(contentArr, "tool_use") {
			out := map[string]interface{}{"role": "assistant", "content": ""}
			var toolCalls []interface{}
			var textParts string
			for _, block := range contentArr {
				b, _ := block.(map[string]interface{})
				if b == nil {
					continue
				}
				switch b["type"] {
				case "tool_use":
					inputJSON, _ := json.Marshal(b["input"])
					toolCalls = append(toolCalls, map[string]interface{}{
						"id":   b["id"],
						"type": "function",
						"function": map[string]interface{}{
							"name":      b["name"],
							"arguments": string(inputJSON),
						},
					})
				case "text":
					if t, ok := b["text"].(string); ok {
						textParts += t
					}
				}
			}
			if textParts != "" {
				out["content"] = textParts
			}
			if len(toolCalls) > 0 {
				out["tool_calls"] = toolCalls
			}
			return out
		}

		// user message with tool_result blocks
		if role == "user" && blockType == "tool_result" {
			var results []map[string]interface{}
			for _, block := range contentArr {
				b, _ := block.(map[string]interface{})
				if b == nil || b["type"] != "tool_result" {
					continue
				}
				toolCallId, _ := b["tool_use_id"].(string)
				resultContent := ""
				switch c := b["content"].(type) {
				case string:
					resultContent = c
				case []interface{}:
					for _, part := range c {
						if pm, ok := part.(map[string]interface{}); ok {
							if t, ok := pm["text"].(string); ok {
								resultContent += t
							}
						}
					}
				}
				results = append(results, map[string]interface{}{
					"role":         "tool",
					"tool_call_id": toolCallId,
					"content":      resultContent,
				})
			}
			if len(results) == 1 {
				return results[0]
			}
			// Multiple tool results: return first, caller should handle multi
			// Actually we need to return all - use a special marker
			if len(results) > 0 {
				return results[0]
			}
			return nil
		}

		// Handle thinking blocks - skip them
		if blockType == "thinking" || blockType == "redacted_thinking" {
			// Extract only text blocks
			var textParts string
			for _, block := range contentArr {
				b, _ := block.(map[string]interface{})
				if b != nil && b["type"] == "text" {
					if t, ok := b["text"].(string); ok {
						textParts += t
					}
				}
			}
			if textParts == "" {
				return nil
			}
			return BuildStructuredMessage(role, textParts)
		}
	}

	text := NormalizeMessageContent(msg)
	images := ExtractMessageImages(msg)

	if role == "assistant" && toolsEnabled {
		if tc, ok := msg["tool_calls"]; ok {
			out := BuildStructuredMessage("assistant", text)
			out["tool_calls"] = tc
			return out
		}
	}

	if role == "tool" {
		out := BuildStructuredMessage("tool", text)
		if name, ok := msg["name"].(string); ok {
			out["name"] = name
		}
		if tcid, ok := msg["tool_call_id"].(string); ok {
			out["tool_call_id"] = tcid
		}
		return out
	}

	if text == "" && len(images) == 0 {
		return nil
	}

	if role == "user" {
		return BuildUserMessageWithImages(text, images)
	}
	return BuildStructuredMessage(role, text)
}

func HasBlockType(blocks []interface{}, blockType string) bool {
	for _, block := range blocks {
		if b, ok := block.(map[string]interface{}); ok {
			if b["type"] == blockType {
				return true
			}
		}
	}
	return false
}

// BuildUserMessage 构造纯文本 user 消息（保留原有签名，内部转发到多模态版本）。
func BuildUserMessage(text string) map[string]interface{} {
	return BuildUserMessageWithImages(text, nil)
}

// BuildUserMessageWithImages 构造 Qoder 上游的 user 消息。
//
// Qoder 的 contents 数组接受 OpenAI 风格的 content parts：图片在前
// ({type:image_url, image_url:{url}})，文本在后 ({type:text, text})。
// 图片 URL 支持 data: base64 与 http(s) 两种形态，与官方客户端一致。
func BuildUserMessageWithImages(text string, images []string) map[string]interface{} {
	contents := make([]interface{}, 0, len(images)+1)
	for _, url := range images {
		contents = append(contents, map[string]interface{}{
			"type":      "image_url",
			"image_url": map[string]interface{}{"url": url},
		})
	}
	if text != "" || len(contents) == 0 {
		contents = append(contents, map[string]interface{}{"type": "text", "text": text})
	}
	return map[string]interface{}{
		"role":                        "user",
		"content":                     "",
		"contents":                    contents,
		"response_meta":               BlankResponseMeta(),
		"reasoning_content_signature": "",
	}
}

// ExtractMessageImages 从消息的 content 数组中提取图片 URL。
// 支持三种形态：
//   - OpenAI Chat Completions: {type:image_url, image_url:{url}}
//   - OpenAI Responses API:    {type:input_image, image_url:"..."}
//   - Claude Messages API:     {type:image, source:{type:base64|url, media_type, data|url}}
//
// 空 URL 与非图片 URL（既不是 data: 也不是 http(s)）会被丢弃。
func ExtractMessageImages(msg map[string]interface{}) []string {
	arr, ok := msg["content"].([]interface{})
	if !ok {
		// 兼容 Qoder 风格：图片可能直接放在 contents 数组里（参考 EchoPing07）
		arr, ok = msg["contents"].([]interface{})
		if !ok {
			return nil
		}
	}
	var images []string
	for _, block := range arr {
		b, ok := block.(map[string]interface{})
		if !ok {
			continue
		}
		t, _ := b["type"].(string)
		switch t {
		case "image_url", "input_image":
			if url := imageURLFromPart(b); url != "" && looksLikeImageURL(url) {
				images = append(images, url)
			}
		case "image":
			if url := claudeImageToURL(b); url != "" {
				images = append(images, url)
			}
		}
	}
	return images
}

// imageURLFromPart 兼容 {image_url:{url}}、{image_url:"..."} 与 {url:"..."} 三种写法。
func imageURLFromPart(part map[string]interface{}) string {
	switch v := part["image_url"].(type) {
	case string:
		return v
	case map[string]interface{}:
		if s, ok := v["url"].(string); ok {
			return s
		}
	}
	if s, ok := part["url"].(string); ok {
		return s
	}
	return ""
}

// claudeImageToURL 把 Claude 的 image block 转成 data:/http URL。
func claudeImageToURL(block map[string]interface{}) string {
	src, ok := block["source"].(map[string]interface{})
	if !ok {
		return ""
	}
	switch st, _ := src["type"].(string); st {
	case "base64":
		mt, _ := src["media_type"].(string)
		data, _ := src["data"].(string)
		if mt != "" && data != "" {
			return "data:" + mt + ";base64," + data
		}
	case "url":
		if u, ok := src["url"].(string); ok && looksLikeImageURL(u) {
			return u
		}
	}
	return ""
}

// looksLikeImageURL 只放行 data: 与 http(s) 两类，避免把垃圾塞给上游。
func looksLikeImageURL(url string) bool {
	return strings.HasPrefix(url, "data:") ||
		strings.HasPrefix(url, "http://") ||
		strings.HasPrefix(url, "https://")
}

func BuildStructuredMessage(role, text string) map[string]interface{} {
	return map[string]interface{}{
		"role":                        role,
		"content":                     text,
		"response_meta":               BlankResponseMeta(),
		"reasoning_content_signature": "",
	}
}

func BlankResponseMeta() map[string]interface{} {
	return map[string]interface{}{
		"id": "",
		"usage": map[string]interface{}{
			"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0,
			"completion_tokens_details": map[string]interface{}{"reasoning_tokens": 0},
			"prompt_tokens_details":     map[string]interface{}{"cached_tokens": 0},
		},
	}
}

func NormalizeMessageContent(msg map[string]interface{}) string {
	c := msg["content"]
	if s, ok := c.(string); ok {
		return s
	}
	if c == nil {
		return ""
	}
	arr, ok := c.([]interface{})
	if !ok {
		data, _ := json.Marshal(c)
		return string(data)
	}
	var sb string
	for _, block := range arr {
		b, _ := block.(map[string]interface{})
		if t, ok := b["text"].(string); ok {
			if sb != "" {
				sb += "\n\n"
			}
			sb += t
		}
	}
	return sb
}
