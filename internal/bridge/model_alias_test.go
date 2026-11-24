package bridge

import (
	"os"
	"testing"

	"qoder2api/account"
)

// TestMapModelDisplayNameAliases 验证客户端传「上游显示名」时能正确落到 key。
//
// 背景（issue #13「请求成功但 dashboard 无用量」）：MapModel 此前只认
// Claude / GPT / Gemini 家族关键字，其余一律 ToLower 后原样发给上游。
// 客户端传 "Qwen4-Max-Preview" 这种显示名时，上游收到未知 key
// "qwen4-max-preview"，走兜底返回内容但**不计 quota**，且这类请求没有
// 真实配额支撑，更容易撞排队 / 429。
func TestMapModelDisplayNameAliases(t *testing.T) {
	account.SetDataRoot(t.TempDir()) // 空 settings，只测内置表

	cases := []struct{ in, want string }{
		// 上游显示名 → key
		{"Qwen4-Max-Preview", "qmodel_latest"}, // 旧显示名（2026-11-19 前）
		{"Qwen4-Max", "qmodel_latest"},         // 新显示名
		{"qwen4-max-preview", "qmodel_latest"},
		{"Step-5-Preview", "jmodel"},
		{"Qwen3.8-Max", "qmodel_38max"},
		{"Qwen3.8-Flash", "qfmodel"},
		{"DeepSeek-V4-Pro", "dmodel"},
		{"DeepSeek-Flash", "dfmodel"},
		{"GLM-5.3", "gmodel"},
		{"GLM-5.4", "gmodel_latest"},
		{"glm-5.4", "gmodel_latest"},
		{"GLM-5.3-Flash", "gfmodel"},
		{"Kimi-K3.1-Pro", "kmodel_latest"},
		{"Kimi-K2.8-Preview", "kmodel"},
		{"MiniMax-M3.1-Flash", "mmodel"},
		// 省略后缀 / 去分隔符的常见写法
		{"qwen4-max", "qmodel_latest"},
		{"qwen4max", "qmodel_latest"},
		{"step-5", "jmodel"},
		{"step5", "jmodel"},
		// 合法 key 必须原样通过（不能被别名表改写）
		{"auto", "auto"},
		{"qmodel_latest", "qmodel_latest"},
		{"qmodel_38max", "qmodel_38max"},
		{"jmodel", "jmodel"},
		{"kmodel", "kmodel"},
		{"mmodel", "mmodel"},
		{"s0model_not_a_key", "s0model_not_a_key"},
		// 家族关键字仍按原规则映射
		{"claude-sonnet-4-6", "gmodel"},
		{"claude-opus-4", "qmodel_38max"},
		{"claude-haiku-4", "qfmodel"},
		{"gpt-5", "dmodel"},
		{"gemini-3-pro", "gmodel"},
	}
	for _, c := range cases {
		for _, agent := range []string{"codex", "claude", "gemini", ""} {
			if got := MapModel(agent, c.in); got != c.want {
				t.Errorf("MapModel(%q, %q) = %q, want %q", agent, c.in, got, c.want)
			}
		}
	}
}

// TestMapModelDoesNotConfuseSimilarDisplayNames 验证近似显示名不会互相误命中
// （误命中会消耗别的模型额度，比"不命中"更糟）。
func TestMapModelDoesNotConfuseSimilarDisplayNames(t *testing.T) {
	account.SetDataRoot(t.TempDir())

	// Qwen4-Max 与 Qwen3.8-Max 不能串号
	if got := MapModel("codex", "Qwen4-Max-Preview"); got != "qmodel_latest" {
		t.Errorf("Qwen4-Max-Preview -> %q, want qmodel_latest", got)
	}
	if got := MapModel("codex", "Qwen3.8-Max"); got != "qmodel_38max" {
		t.Errorf("Qwen3.8-Max -> %q, want qmodel_38max", got)
	}
	// Kimi-K3.1-Pro 与 Kimi-K2.8-Preview 不能串号
	if got := MapModel("codex", "Kimi-K3.1-Pro"); got != "kmodel_latest" {
		t.Errorf("Kimi-K3.1-Pro -> %q, want kmodel_latest", got)
	}
	if got := MapModel("codex", "Kimi-K2.8-Preview"); got != "kmodel" {
		t.Errorf("Kimi-K2.8-Preview -> %q, want kmodel", got)
	}
}

// TestMapModelUserSettingsOverrideAlias 验证用户控制台配置优先于显示名别名表。
func TestMapModelUserSettingsAlias(t *testing.T) {
	dir := t.TempDir()
	account.SetDataRoot(dir)

	st, err := account.LoadSettings()
	if err != nil || st == nil {
		t.Fatalf("load settings: %v", err)
	}
	st.ModelMappings = map[string]map[string]string{
		"codex": {"Qwen4-Max-Preview": "gmodel"},
	}
	if err := account.SaveSettings(st); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	// 用户显式配置优先
	if got := MapModel("codex", "Qwen4-Max-Preview"); got != "gmodel" {
		t.Errorf("user mapping should win: got %q, want gmodel", got)
	}
	// 其他 agent 桶未配置，仍走别名表
	if got := MapModel("claude", "Qwen4-Max-Preview"); got != "qmodel_latest" {
		t.Errorf("claude bucket empty -> alias table: got %q, want qmodel_latest", got)
	}
}

// TestIsKnownUpstreamKey 验证未知 key 识别（用于 CallQoder 的告警）。
func TestIsKnownUpstreamKey(t *testing.T) {
	for _, k := range []string{"auto", "qmodel_latest", "jmodel", "kmodel", "QMODEL_LATEST"} {
		if !IsKnownUpstreamKey(k) {
			t.Errorf("%q should be a known key", k)
		}
	}
	for _, k := range []string{"qwen4-max-preview", "nonexistent", "claude-sonnet-4-6",
		// 2026-11-19 起上游不再下发，已从合法 key 集合移除
		"smodel", "cmodel", "fast", "qmodel", "q37fmodel", "gm51model"} {
		if IsKnownUpstreamKey(k) {
			t.Errorf("%q should NOT be a known key (retired upstream)", k)
		}
	}
}

// TestRuntimeAliasAutoRegistration 验证「上游新增模型后无需改代码」：
// 只要 RegisterModelCatalog 喂进来一个新模型，它的显示名立刻就能命中，
// 且该 key 立刻被认定为合法 key。
//
// 这是「不靠猜」的核心：上游 model/list 的 display_name 是唯一权威来源，
// 每次成功拉取都会调用 RegisterModelCatalog，别名表随之自动更新。
func TestRuntimeAliasAutoRegistration(t *testing.T) {
	account.SetDataRoot(t.TempDir())

	// 一个此前从未出现过的模型（模拟上游将来新增的 "hungerg"）
	const newKey = "hgm_2026x"
	const newName = "HungerG-Preview"
	if IsKnownUpstreamKey(newKey) {
		t.Fatal("precondition: new key should be unknown before registration")
	}
	if got := MapModel("codex", newName); got == newKey {
		t.Fatal("precondition: display name should not map before registration")
	}

	RegisterModelCatalog([]QoderModel{{Key: newKey, DisplayName: newName}})

	// 注册后：显示名 → key、省略后缀也能命中、key 本身被认定合法
	for _, in := range []string{newName, "hungerg-preview", "HungerG-Preview", "hungerg preview"} {
		if got := MapModel("codex", in); got != newKey {
			t.Errorf("MapModel(%q) = %q, want %q after registration", in, got, newKey)
		}
	}
	if !IsKnownUpstreamKey(newKey) {
		t.Error("registered key should be known")
	}
}

// TestRuntimeAliasDoesNotBreakRealKeys 验证运行时别名表不会改写合法 key。
func TestRuntimeAliasDoesNotBreakRealKeys(t *testing.T) {
	account.SetDataRoot(t.TempDir())
	RegisterModelCatalog([]QoderModel{
		{Key: "auto", DisplayName: "Auto"},
		{Key: "gmodel_latest", DisplayName: "GLM-5.4"},
		{Key: "gmodel", DisplayName: "GLM-5.3"},
	})
	cases := map[string]string{
		"auto":          "auto",
		"gmodel":        "gmodel",
		"gmodel_latest": "gmodel_latest",
		"GLM-5.4":       "gmodel_latest",
		"GLM-5.3":       "gmodel",
	}
	for in, want := range cases {
		if got := MapModel("codex", in); got != want {
			t.Errorf("MapModel(%q) = %q, want %q", in, got, want)
		}
	}
}
