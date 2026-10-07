package bridge

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"qoder2api/internal/cosy"
)

// TestCallQoderSendsImageParts 验证图片 part 从客户端请求一路传到上游请求体
// （端到端覆盖 BuildQoderMessages -> CallQoderWithOpts -> cosy.Encode）。
func TestCallQoderSendsImageParts(t *testing.T) {
	var rawBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseContent("ok"))
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	msgs := BuildQoderMessages(nil, []interface{}{
		map[string]interface{}{
			"role": "user",
			"content": []interface{}{
				map[string]interface{}{"type": "text", "text": "看图"},
				map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "data:image/png;base64,ZZZZ"}},
			},
		},
	}, "看图", false)

	err := envelopeBridge(t, srv.URL).CallQoderWithOpts(
		context.Background(), "codex", msgs, "auto", nil, CallOpts{},
		func(Delta) {})
	if err != nil {
		t.Fatalf("CallQoderWithOpts: %v", err)
	}

	plain, err := cosy.Decode(string(rawBody))
	if err != nil {
		t.Fatalf("decode upstream body: %v", err)
	}
	body := string(plain)
	if !strings.Contains(body, `"image_url"`) {
		t.Fatalf("upstream body missing image part: %s", body)
	}
	if !strings.Contains(body, "data:image/png;base64,ZZZZ") {
		t.Fatalf("upstream body missing image url: %s", body)
	}
	if !strings.Contains(body, `"type":"text"`) {
		t.Fatalf("upstream body missing text part: %s", body)
	}
}
