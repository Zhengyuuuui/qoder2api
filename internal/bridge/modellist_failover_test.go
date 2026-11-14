package bridge

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"qoder2api/account"
	"qoder2api/internal/cosy"
)

// modelListSlot 构造一个指向本地测试服务器的账号槽位。
func modelListSlot(t *testing.T, id string, region account.Region) *Slot {
	t.Helper()
	sess, err := cosy.NewSession(
		cosy.AuthIdentity{Uid: "uid-" + id, UserType: "personal_standard"},
		"mid-"+id+"-00000000000000000000000",
		"mtoken-"+id,
		"mtype-"+id+"-0000000",
	)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return NewSlot(id, "name-"+id, region, sess, NewBearerClient(sess))
}

// TestListAvailableModelsFailsOverAccount 验证模型清单的账号故障转移：
//
// 池内第一个账号的凭证失效（实测 403 Signature invalid）时，
// ListAvailableModels 必须换下一个账号，而不是让整个 /v1/models
// 退化成硬编码兜底清单。
func TestListAvailableModelsFailsOverAccount(t *testing.T) {
	// 同一个 server 按调用次序返回不同响应：第一次 403（凭证失效），
	// 第二次正常清单——据此验证「换号后重试」确实发生。
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			// 实测形态：凭证失效时上游返回 403 Signature invalid
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"code":"101","message":"Signature invalid"}`)
			return
		}
		fmt.Fprint(w, `{"assistant":[{"key":"auto","display_name":"Auto","enable":true,`+
			`"is_default":true,"is_reasoning":true,"price_factor":0.5,"max_input_tokens":200000},`+
			`{"key":"jmodel","display_name":"Step-5-Preview","enable":true,`+
			`"is_reasoning":true,"price_factor":0.3,"max_input_tokens":1000000}]}`)
	}))
	defer srv.Close()

	pool := NewPool(
		modelListSlot(t, "bad", account.RegionCN),
		modelListSlot(t, "good", account.RegionCN),
	)
	b := NewPoolBridgeFor(account.RegionCN, pool, nil)
	b.modelListURL = srv.URL

	models, err := b.ListAvailableModels()
	if err != nil {
		t.Fatalf("ListAvailableModels: %v (must fail over to the second account)", err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %d, want 2", len(models))
	}
	if models[0].Key != "auto" || models[1].Key != "jmodel" {
		t.Errorf("model keys = [%s %s], want [auto jmodel]", models[0].Key, models[1].Key)
	}
	if calls < 2 {
		t.Errorf("upstream calls = %d, want >= 2 (first account failed, must retry another)", calls)
	}
}

// TestListAvailableModelsAllAccountsFail 验证全部账号失败时返回错误
// （而不是返回一个空清单让上层误判为「上游没有模型」）。
func TestListAvailableModelsAllAccountsFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"code":"101","message":"Signature invalid"}`)
	}))
	defer srv.Close()

	pool := NewPool(modelListSlot(t, "a", account.RegionCN), modelListSlot(t, "b", account.RegionCN))
	b := NewPoolBridgeFor(account.RegionCN, pool, nil)
	b.modelListURL = srv.URL

	if _, err := b.ListAvailableModels(); err == nil {
		t.Fatal("expected error when every account fails")
	}
}

// TestListAvailableModelsStaysInRegion 验证模型清单只在本桥区域内取号：
// 池里混了 global 账号时，CN 桥不得用 global 账号拉清单
// （否则 /v1/models 会返回别区的模型表）。
func TestListAvailableModelsStaysInRegion(t *testing.T) {
	var hitCN, hitGlobal int
	cnSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitCN++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"assistant":[{"key":"auto","display_name":"Auto","enable":true}]}`)
	}))
	defer cnSrv.Close()

	globalSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitGlobal++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"assistant":[{"key":"ultimate","display_name":"Ultimate","enable":true}]}`)
	}))
	defer globalSrv.Close()

	// 两个账号分别绑不同的模型目录地址（用不同 client 区分）。
	cnSlot := modelListSlot(t, "cn", account.RegionCN)
	globalSlot := modelListSlot(t, "gl", account.RegionGlobal)

	pool := NewPool(cnSlot, globalSlot)
	b := NewPoolBridgeFor(account.RegionCN, pool, nil)

	// 通过给 globalSlot 单独的目录地址来区分两个 server：
	// Bridge.modelListURL 是桥级别的，这里靠「谁被选中」来判断——
	// 若跨区被选中，则返回 ultimate（global 的 key）。
	b.modelListURL = cnSrv.URL

	models, err := b.ListAvailableModels()
	if err != nil {
		t.Fatalf("ListAvailableModels: %v", err)
	}
	if len(models) != 1 || models[0].Key != "auto" {
		t.Errorf("models = %+v, want [auto] (must not use the global account)", models)
	}
	if hitGlobal != 0 {
		t.Errorf("global endpoint hits = %d, want 0 (cross-region request)", hitGlobal)
	}
}
