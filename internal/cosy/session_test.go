package cosy

import (
	"encoding/base64"
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

func TestNewRequestIDForm(t *testing.T) {
	id := NewRequestID()
	if len(id) != 32 {
		t.Errorf("len = %d, want 32", len(id))
	}
	for _, c := range id {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("non-hex char %q in %s", c, id)
		}
	}
	if NewRequestID() == id {
		t.Error("request id must differ per call")
	}
}

func TestMarshalSortedStringMap(t *testing.T) {
	m := map[string]string{
		"version": "v1", "cosyVersion": "1.0.10", "info": "x",
		"ideVersion": "", "requestId": "r",
	}
	want := `{"cosyVersion":"1.0.10","ideVersion":"","info":"x","requestId":"r","version":"v1"}`
	got := marshalSortedStringMap(m)
	if got != want {
		t.Errorf("marshalSortedStringMap =\n%s\nwant\n%s", got, want)
	}
	// 字节级稳定：同一输入两次 marshal 完全一致
	if marshalSortedStringMap(m) != got {
		t.Error("not byte-stable across calls")
	}
	// 键序为字典序
	keys := topLevelKeys(t, []byte(got))
	if !sort.StringsAreSorted(keys) {
		t.Errorf("keys not sorted: %v", keys)
	}
}

func TestAuthPayloadJSONSortedStable(t *testing.T) {
	id := AuthIdentity{
		Name: "nick", Aid: "a", Uid: "u", YxUid: "y",
		OrganizationId: "o", OrganizationName: "on", UserType: "personal_standard",
		SecurityOauthToken: "sot", RefreshToken: "rt",
	}
	a, err := authPayloadJSON(id)
	if err != nil {
		t.Fatalf("authPayloadJSON: %v", err)
	}
	b, _ := authPayloadJSON(id)
	if string(a) != string(b) {
		t.Error("auth payload not byte-stable")
	}
	keys := topLevelKeys(t, a)
	if !sort.StringsAreSorted(keys) {
		t.Errorf("auth payload keys not sorted: %v", keys)
	}
	// 值不丢失
	var back map[string]string
	if err := json.Unmarshal(a, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back["uid"] != "u" || back["security_oauth_token"] != "sot" {
		t.Errorf("payload values lost: %v", back)
	}
}

func TestBuildPayloadB64Decodes(t *testing.T) {
	b64, err := BuildPayloadB64("info-x")
	if err != nil {
		t.Fatalf("BuildPayloadB64: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["info"] != "info-x" || m["version"] != "v1" {
		t.Errorf("payload = %v", m)
	}
	keys := topLevelKeys(t, raw)
	if !sort.StringsAreSorted(keys) {
		t.Errorf("payload keys not sorted: %v", keys)
	}
}

// topLevelKeys 按出现顺序取出 JSON 对象的顶层键（用于断言键序）。
func topLevelKeys(t *testing.T, data []byte) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(string(data)))
	tok, err := dec.Token()
	if err != nil {
		t.Fatalf("read token: %v", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		t.Fatalf("want object, got %v", tok)
	}
	var keys []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			t.Fatalf("read key: %v", err)
		}
		key, ok := k.(string)
		if !ok {
			t.Fatalf("key not string: %v", k)
		}
		keys = append(keys, key)
		var v interface{}
		if err := dec.Decode(&v); err != nil {
			t.Fatalf("decode value: %v", err)
		}
	}
	return keys
}
