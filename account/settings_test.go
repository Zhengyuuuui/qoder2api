package account

import "testing"

func TestEffectiveQueueWaitFirstToken(t *testing.T) {
	// 未设置（nil）→ 默认开
	s := &Settings{}
	if !s.EffectiveQueueWaitFirstToken() {
		t.Error("nil should default to true (ON)")
	}
	// 显式 true
	tr := true
	s.QueueWaitFirstToken = &tr
	if !s.EffectiveQueueWaitFirstToken() {
		t.Error("explicit true should be true")
	}
	// 显式 false
	fa := false
	s.QueueWaitFirstToken = &fa
	if s.EffectiveQueueWaitFirstToken() {
		t.Error("explicit false should be false")
	}
}

// 模型级冷却 / 熔断降权开关：未设置（nil）时默认开启（存量 settings.json
// 缺字段即为零值 nil → 走默认）。
func TestEffectiveHealthSwitches(t *testing.T) {
	s := &Settings{}
	if !s.EffectiveModelCooldown() {
		t.Error("model cooldown should default to true")
	}
	if !s.EffectiveBreakerDegrade() {
		t.Error("breaker/degrade should default to true")
	}

	fa := false
	s.ModelCooldown = &fa
	s.BreakerDegrade = &fa
	if s.EffectiveModelCooldown() {
		t.Error("explicit false should disable model cooldown")
	}
	if s.EffectiveBreakerDegrade() {
		t.Error("explicit false should disable breaker/degrade")
	}

	tr := true
	s.ModelCooldown = &tr
	s.BreakerDegrade = &tr
	if !s.EffectiveModelCooldown() || !s.EffectiveBreakerDegrade() {
		t.Error("explicit true should enable both")
	}
}

// 凭证写入后 needs_reauth 被清除：重新授权（OAuth / PAT 导入 / token 刷新
// 回写）后账号自动恢复参与选号。
func TestSaveSecretClearsNeedsReauth(t *testing.T) {
	SetDataRoot(t.TempDir())
	acct := &Account{ID: "acct-reauth-1", Name: "reauth"}
	if err := Save(acct); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := MarkNeedsReauth("acct-reauth-1"); err != nil {
		t.Fatalf("mark: %v", err)
	}
	got, err := Get("acct-reauth-1")
	if err != nil || got == nil {
		t.Fatalf("get: %v", err)
	}
	if !got.NeedsReauth {
		t.Fatal("account should be marked needs_reauth")
	}
	// 重新授权：写入新凭证 → 标志清除
	if err := SaveSecret("acct-reauth-1", "dt-newtoken"); err != nil {
		t.Fatalf("save secret: %v", err)
	}
	got, err = Get("acct-reauth-1")
	if err != nil || got == nil {
		t.Fatalf("get: %v", err)
	}
	if got.NeedsReauth {
		t.Error("needs_reauth must be cleared after re-auth (credential saved)")
	}
}
