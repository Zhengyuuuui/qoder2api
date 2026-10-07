package bridge

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildUserMessageWithImages(t *testing.T) {
	msg := BuildUserMessageWithImages("看图", []string{"data:image/png;base64,AAAA"})
	data, _ := json.Marshal(msg)
	s := string(data)
	if !strings.Contains(s, `"type":"image_url"`) {
		t.Fatalf("missing image part: %s", s)
	}
	if !strings.Contains(s, `"url":"data:image/png;base64,AAAA"`) {
		t.Fatalf("missing image url: %s", s)
	}
	if !strings.Contains(s, `"type":"text"`) {
		t.Fatalf("missing text part: %s", s)
	}
}

func TestBuildUserMessageTextOnlyUnchanged(t *testing.T) {
	msg := BuildUserMessage("hi")
	contents, _ := msg["contents"].([]interface{})
	if len(contents) != 1 {
		t.Fatalf("text-only should have exactly 1 part, got %d", len(contents))
	}
	first, _ := contents[0].(map[string]interface{})
	if first["type"] != "text" || first["text"] != "hi" {
		t.Fatalf("unexpected text part: %v", first)
	}
}

func TestConvertIncomingMessageImageOnly(t *testing.T) {
	msg := map[string]interface{}{
		"role": "user",
		"content": []interface{}{
			map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "data:image/jpeg;base64,BBBB"}},
		},
	}
	out := ConvertIncomingMessage(msg, false)
	if out == nil {
		t.Fatal("image-only user message was dropped")
	}
	contents, _ := out["contents"].([]interface{})
	if len(contents) != 1 {
		t.Fatalf("want 1 part (image), got %d: %v", len(contents), contents)
	}
	first, _ := contents[0].(map[string]interface{})
	if first["type"] != "image_url" {
		t.Fatalf("want image part, got %v", first)
	}
}

func TestConvertIncomingMessageTextAndImage(t *testing.T) {
	msg := map[string]interface{}{
		"role": "user",
		"content": []interface{}{
			map[string]interface{}{"type": "text", "text": "这是什么"},
			map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "https://example.com/a.png"}},
		},
	}
	out := ConvertIncomingMessage(msg, false)
	contents, _ := out["contents"].([]interface{})
	if len(contents) != 2 {
		t.Fatalf("want 2 parts, got %d: %v", len(contents), contents)
	}
	first, _ := contents[0].(map[string]interface{})
	if first["type"] != "image_url" {
		t.Fatalf("image should come first, got %v", first)
	}
	last, _ := contents[1].(map[string]interface{})
	if last["type"] != "text" || last["text"] != "这是什么" {
		t.Fatalf("text should follow image, got %v", last)
	}
}

func TestConvertIncomingMessageClaudeImage(t *testing.T) {
	msg := map[string]interface{}{
		"role": "user",
		"content": []interface{}{
			map[string]interface{}{
				"type": "image",
				"source": map[string]interface{}{
					"type": "base64", "media_type": "image/png", "data": "CCCC",
				},
			},
			map[string]interface{}{"type": "text", "text": "hi"},
		},
	}
	out := ConvertIncomingMessage(msg, false)
	contents, _ := out["contents"].([]interface{})
	if len(contents) != 2 {
		t.Fatalf("want 2 parts, got %d: %v", len(contents), contents)
	}
	first, _ := contents[0].(map[string]interface{})
	if first["type"] != "image_url" {
		t.Fatalf("claude image not converted: %v", first)
	}
	iu, _ := first["image_url"].(map[string]interface{})
	if iu["url"] != "data:image/png;base64,CCCC" {
		t.Fatalf("bad data url: %v", iu)
	}
}

func TestConvertIncomingMessageRejectsJunkImageURL(t *testing.T) {
	msg := map[string]interface{}{
		"role": "user",
		"content": []interface{}{
			map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "not-a-url"}},
		},
	}
	if out := ConvertIncomingMessage(msg, false); out != nil {
		t.Fatalf("junk image should be dropped, got %v", out)
	}
}

func TestExtractMessageImagesInputImageString(t *testing.T) {
	msg := map[string]interface{}{
		"content": []interface{}{
			map[string]interface{}{"type": "input_image", "image_url": "data:image/webp;base64,DDDD"},
		},
	}
	imgs := ExtractMessageImages(msg)
	if len(imgs) != 1 || imgs[0] != "data:image/webp;base64,DDDD" {
		t.Fatalf("want 1 data url, got %v", imgs)
	}
}
