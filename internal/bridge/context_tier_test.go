package bridge

import (
	"testing"
)

func TestParseQoderModels_ContextTiers(t *testing.T) {
	raw := map[string]interface{}{
		"chat": []interface{}{
			map[string]interface{}{
				"key":              "qmodel_38max",
				"display_name":     "Qwen3.8-Max",
				"enable":           true,
				"max_input_tokens": float64(180000),
				"context_config": map[string]interface{}{
					"200K": map[string]interface{}{"token_count": float64(200000), "is_default": true},
					"400K": map[string]interface{}{"token_count": float64(400000)},
					"1M":   map[string]interface{}{"token_count": float64(1000000)},
				},
			},
			map[string]interface{}{
				"key":              "auto",
				"display_name":     "Auto",
				"enable":           true,
				"max_input_tokens": float64(200000),
				"context_config": map[string]interface{}{
					"200K": map[string]interface{}{"token_count": float64(200000), "is_default": true},
				},
			},
		},
	}

	models := parseQoderModels(raw)
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2", len(models))
	}

	m := models[0]
	if m.Key != "qmodel_38max" {
		t.Errorf("first model key = %q, want qmodel_38max", m.Key)
	}
	// 默认档仍取上游 is_default
	if m.ContextWindow != 200000 {
		t.Errorf("ContextWindow = %d, want 200000 (upstream default)", m.ContextWindow)
	}
	// 三档都要保留
	if len(m.ContextTiers) != 3 {
		t.Fatalf("ContextTiers = %d, want 3", len(m.ContextTiers))
	}
	// 按 token 升序
	want := []struct {
		name   string
		tokens int
		def    bool
	}{
		{"200K", 200000, true},
		{"400K", 400000, false},
		{"1M", 1000000, false},
	}
	for i, w := range want {
		if m.ContextTiers[i].Name != w.name || m.ContextTiers[i].Tokens != w.tokens || m.ContextTiers[i].IsDefault != w.def {
			t.Errorf("tier[%d] = %+v, want %v/%d/%v", i, m.ContextTiers[i], w.name, w.tokens, w.def)
		}
	}

	// 单档模型
	m2 := models[1]
	if len(m2.ContextTiers) != 1 || m2.ContextTiers[0].Name != "200K" {
		t.Errorf("auto tiers = %+v, want single 200K", m2.ContextTiers)
	}
	if m2.ContextWindow != 200000 {
		t.Errorf("auto ContextWindow = %d, want 200000", m2.ContextWindow)
	}
}

func TestParseQoderModels_NoContextConfig(t *testing.T) {
	// 无 context_config 时回落到 max_input_tokens
	raw := map[string]interface{}{
		"chat": []interface{}{
			map[string]interface{}{
				"key":              "legacy",
				"enable":           true,
				"max_input_tokens": float64(128000),
			},
		},
	}
	models := parseQoderModels(raw)
	if len(models) != 1 {
		t.Fatalf("got %d models, want 1", len(models))
	}
	if models[0].ContextWindow != 128000 {
		t.Errorf("ContextWindow = %d, want 128000 (fallback to max_input_tokens)", models[0].ContextWindow)
	}
	if len(models[0].ContextTiers) != 0 {
		t.Errorf("ContextTiers should be empty, got %+v", models[0].ContextTiers)
	}
}

func TestContextTierTokensLookup(t *testing.T) {
	b := &Bridge{
		catalogCache: []QoderModel{
			{
				Key: "qmodel_38max",
				ContextTiers: []ContextTier{
					{Name: "200K", Tokens: 200000, IsDefault: true},
					{Name: "400K", Tokens: 400000},
					{Name: "1M", Tokens: 1000000},
				},
			},
		},
	}
	cases := []struct {
		model, tier string
		want        int
	}{
		{"qmodel_38max", "400K", 400000},
		{"qmodel_38max", "1M", 1000000},
		{"qmodel_38max", "200K", 200000},
		{"qmodel_38max", "8M", 0},        // 不存在的档位
		{"other_model", "400K", 0},       // 不存在的模型
		{"QMODEL_38MAX", "400k", 400000}, // 大小写不敏感
	}
	for _, c := range cases {
		if got := b.contextTierTokens(c.model, c.tier); got != c.want {
			t.Errorf("contextTierTokens(%q, %q) = %d, want %d", c.model, c.tier, got, c.want)
		}
	}
}

// TestParseQoderModels_Thinking 验证思考能力来自 thinking_config，
// 而不是 is_reasoning（上游对 dfmodel/kmodel_latest 标 false 但支持思考）。
func TestParseQoderModels_Thinking(t *testing.T) {
	raw := map[string]interface{}{
		"assistant": []interface{}{
			map[string]interface{}{
				"key": "dfmodel", "enable": true, "is_reasoning": false,
				"thinking_config": map[string]interface{}{
					"enabled": map[string]interface{}{
						"is_default": true,
						"efforts": map[string]interface{}{
							"max": map[string]interface{}{}, "low": map[string]interface{}{}, "high": map[string]interface{}{},
						},
					},
					"disabled": map[string]interface{}{"description": "Disable thinking"},
				},
			},
			map[string]interface{}{
				"key": "mmodel", "enable": true, "is_reasoning": false,
			},
			map[string]interface{}{
				"key": "auto", "enable": true, "is_reasoning": true,
			},
		},
	}
	models := parseQoderModels(raw)
	if len(models) != 3 {
		t.Fatalf("got %d models, want 3", len(models))
	}
	df := models[0]
	if !df.ThinkingSupport || !df.ThinkingDefault {
		t.Errorf("dfmodel thinking support/default = %v/%v, want true/true", df.ThinkingSupport, df.ThinkingDefault)
	}
	want := []string{"low", "high", "max"}
	if len(df.ThinkingEfforts) != len(want) {
		t.Fatalf("efforts = %v, want %v", df.ThinkingEfforts, want)
	}
	for i, w := range want {
		if df.ThinkingEfforts[i] != w {
			t.Errorf("efforts[%d] = %q, want %q (ordered)", i, df.ThinkingEfforts[i], w)
		}
	}
	if mm := models[1]; mm.ThinkingSupport || mm.IsReasoning {
		t.Errorf("mmodel should have no thinking support, got %+v", mm)
	}
	if au := models[2]; au.ThinkingSupport || !au.IsReasoning {
		t.Errorf("auto should be is_reasoning-only, got %+v", au)
	}
}
