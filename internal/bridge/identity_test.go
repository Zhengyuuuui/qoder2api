package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"qoder2api/account"
	"qoder2api/internal/cosy"
)

// identityTestPool 构造带一个账号的池（DataRoot 已隔离）。
func identityTestPool(t *testing.T) *Pool {
	t.Helper()
	orig := account.DataRoot()
	account.SetDataRoot(t.TempDir())
	t.Cleanup(func() { account.SetDataRoot(orig) })
	sess, err := cosy.NewSession(
		cosy.AuthIdentity{Uid: "id-uid-1", UserType: "personal_standard"},
		"mid-test", "mtok-test", "mtype-test",
	)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return NewPool(NewSlot("id-acct-1", "id-acct-1", account.RegionCN, sess, NewBearerClient(sess)))
}

func TestIdentityAuditInitializesThenOK(t *testing.T) {
	p := identityTestPool(t)

	rows := p.AuditIdentities()
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].Conclusion != IdentityInit || rows[0].Drift {
		t.Errorf("first run = %+v, want initialized (not drift)", rows[0])
	}
	if _, err := os.Stat(filepath.Join(account.DataRoot(), "identity_snapshot.json")); err != nil {
		t.Errorf("snapshot not written: %v", err)
	}

	// 二次运行且派生值未变 → 一致
	rows = p.AuditIdentities()
	if rows[0].Conclusion != IdentityOK {
		t.Errorf("second run = %s, want ok", rows[0].Conclusion)
	}
}

func TestIdentityAuditLegacySwitchIsManualChange(t *testing.T) {
	p := identityTestPool(t)
	p.AuditIdentities() // 基线

	t.Setenv("QD_DEVICE_ID_LEGACY", "1")
	rows := p.AuditIdentities()
	if rows[0].Conclusion != IdentityManual || rows[0].Drift {
		t.Errorf("legacy switch = %+v, want manual_change (not drift)", rows[0])
	}
}

func TestIdentityAuditSaltChangeIsManualChange(t *testing.T) {
	p := identityTestPool(t)
	p.AuditIdentities() // 基线

	st := &account.Settings{Port: 8963, LogLevel: "info", MachineSalt: "rotated-salt"}
	if err := account.SaveSettings(st); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	rows := p.AuditIdentities()
	if rows[0].Conclusion != IdentityManual || rows[0].Drift {
		t.Errorf("salt change = %+v, want manual_change", rows[0])
	}
}

func TestIdentityAuditUnexplainedChangeIsDrift(t *testing.T) {
	p := identityTestPool(t)
	p.AuditIdentities() // 基线

	// 模拟「派生逻辑被改动」：盐与 legacy 均未变，但快照里的派生值被换掉
	snapPath := filepath.Join(account.DataRoot(), "identity_snapshot.json")
	data, err := os.ReadFile(snapPath)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	var snap identitySnapshotFile
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	for k, rec := range snap.Accounts {
		rec.MachineID = "deadbeef-0000-4000-a000-000000000000"
		snap.Accounts[k] = rec
	}
	tampered, _ := json.MarshalIndent(snap, "", "  ")
	if err := os.WriteFile(snapPath, tampered, 0600); err != nil {
		t.Fatalf("tamper snapshot: %v", err)
	}

	rows := p.AuditIdentities()
	if rows[0].Conclusion != IdentityDrift || !rows[0].Drift {
		t.Errorf("tampered snapshot = %+v, want drift", rows[0])
	}
	// 漂移时不覆盖快照：告警状态保持可见，直到人工处理
	after, _ := os.ReadFile(snapPath)
	if !strings.Contains(string(after), "deadbeef-0000-4000-a000-000000000000") {
		t.Error("snapshot overwritten on drift; alarm should persist")
	}
}

// TestIdentityAuditSaltSwitchIsManualChange 审计 P1-2：盐开关切换
// （QD_NO_MACHINE_SALT）导致的派生值变化是「人为变更」而非「指纹漂移」——
// 存量部署升级时打开该开关保旧指纹，审计不得误报为回归。
func TestIdentityAuditSaltSwitchIsManualChange(t *testing.T) {
	p := identityTestPool(t)
	p.AuditIdentities() // 基线

	t.Setenv("QD_NO_MACHINE_SALT", "1")
	rows := p.AuditIdentities()
	if rows[0].Conclusion != IdentityManual || rows[0].Drift {
		t.Errorf("salt switch = %+v, want manual_change (not drift)", rows[0])
	}
	if rows[0].Detail == "" || !strings.Contains(rows[0].Detail, "QD_NO_MACHINE_SALT") {
		t.Errorf("detail should explain the switch: %q", rows[0].Detail)
	}
}
