package bridge

import "testing"

// TestDeltaDispatcherStopsAfterUpstreamError 验证 review P1 修复：
// 捕获到流内业务错误后，必须停止向 onDelta 分发任何后续 delta，
// 避免客户端收到「部分内容 + 错误」交错的数据流；
// 且错误帧/错误后的行必须返回 false，通知 openStreamLines 停止读取上游流。
func TestDeltaDispatcherStopsAfterUpstreamError(t *testing.T) {
	var upstreamErr error
	var got []string
	fn := deltaDispatcher(func(d Delta) { got = append(got, d.Content) }, &upstreamErr)

	normal := `data: {"body":"{\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}"}`
	boom := `data: {"body":"{\"code\":\"115\",\"message\":\"quota exceeded\"}"}`
	trailing := `data: {"body":"{\"choices\":[{\"delta\":{\"content\":\"late\"}}]}"}`

	// 错误发生前：正常分发，返回 true 表示继续读取
	if !fn(normal) {
		t.Fatal("normal frame must return true to keep reading")
	}
	if len(got) != 1 || got[0] != "hello" {
		t.Fatalf("normal dispatch got=%v", got)
	}
	// 非 data 行忽略，仍继续读取
	if !fn("event: ping") {
		t.Fatal("non-data line must return true to keep reading")
	}
	if len(got) != 1 {
		t.Fatalf("non-data line dispatched: %v", got)
	}
	// 错误帧：置位 upstreamErr，不调用 onDelta，返回 false 停止上游读取
	if fn(boom) {
		t.Fatal("error frame must return false to stop upstream read")
	}
	if upstreamErr == nil {
		t.Fatal("upstreamErr not set by error frame")
	}
	if len(got) != 1 {
		t.Fatalf("error frame dispatched delta: %v", got)
	}
	// 错误之后的行：一律丢弃，且返回 false（防御：即便调用方未停读也不再消费）
	if fn(trailing) || fn(normal) {
		t.Fatal("lines after error must return false")
	}
	if len(got) != 1 {
		t.Fatalf("delta dispatched after error: %v", got)
	}
}

// TestExtractDeltaUsageWithContent 回归测试（review P2-4）：
// 同帧 usage + choices content：Delta 必须同时携带内容与 tokens
// （修复前 usage 命中即提前 return，同帧内容丢失）。
func TestExtractDeltaUsageWithContent(t *testing.T) {
	inner := `{"choices":[{"delta":{"content":"hello"}}],"usage":{"prompt_tokens":12,"completion_tokens":34}}`
	d := ExtractDelta(envelopePayload(200, inner))
	if d.Err != nil {
		t.Fatalf("err = %v, want nil", d.Err)
	}
	if d.Content != "hello" || d.InputTokens != 12 || d.OutputTokens != 34 {
		t.Fatalf("delta = %+v, want content=hello with usage 12/34", d)
	}
}

// TestExtractDeltaUsageOnlyFrame 回归测试（review P2-4）：
// usage-only 帧（choices 未命中内容）：行为与修复前一致，仅返回 tokens。
// 注意顺序语义：业务错误 code 检查仍先于 usage-only 返回（见 envelope 业务错误用例）。
func TestExtractDeltaUsageOnlyFrame(t *testing.T) {
	inner := `{"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":34}}`
	d := ExtractDelta(envelopePayload(200, inner))
	if d.Err != nil {
		t.Fatalf("err = %v, want nil", d.Err)
	}
	if d.InputTokens != 12 || d.OutputTokens != 34 {
		t.Fatalf("usage = %d/%d, want 12/34", d.InputTokens, d.OutputTokens)
	}
	if d.Content != "" || d.Role != "" || d.Reasoning != "" || d.ToolCalls != nil {
		t.Fatalf("usage-only frame leaked content fields: %+v", d)
	}
}
