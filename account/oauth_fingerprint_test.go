package account

import (
	"testing"
	"time"
)

// TestPreserveDeviceIdentity 验证重新授权时保留账号已有的
// 真实设备指纹与设备身份。
//
// 背景（2026-11-14 实测 bug）：buildAccountFromToken 构造全新 Account
// 对象（指纹字段全空），handleOAuthWait 直接 Save 覆盖旧账号文件，
// 导致 checkin_fingerprint 丢失 → 账号退回旧协议（5 / 1.0.10）→
// Sonus(smodel) / Cantus(cmodel) / Fast(fast) 三个模型从清单消失。
//
// 同一账号重新授权 = 同一台设备，不该被上游当成「换设备」。
func TestPreserveDeviceIdentity(t *testing.T) {
	SetDataRoot(t.TempDir())

	fp := &CheckinFingerprint{
		MachineID:    "mid-real-0000000000000000000000000000",
		MachineToken: "mtoken-real",
		MachineType:  "5",
		ClientType:   "10",
		Version:      "1.1.69",
		MachineOS:    "aarch64_darwin",
		Scene:        "app",
		Product:      "app",
	}
	ident := &DeviceIdentity{
		MachineID:    "mid-real-0000000000000000000000000000",
		MachineToken: "mtoken-real",
		MachineType:  "5",
		ClientType:   "10",
		Version:      "1.1.69",
		Real:         true,
	}
	created := time.Date(2026, 10, 1, 12, 0, 0, 0, time.Local)
	old := &Account{
		ID:                 "acct-fp-1",
		Name:               "fp user",
		Email:              "fp@example.com",
		Region:             RegionGlobal,
		AuthMode:           "oauth",
		APIMode:            "openai",
		Active:             true,
		Tags:               []string{"keep"},
		CreatedAt:          created,
		CheckinFingerprint: fp,
		DeviceIdentity:     ident,
	}
	if err := Save(old); err != nil {
		t.Fatalf("save old: %v", err)
	}

	// 模拟重新授权：构造新 Account（指纹字段为空），再走与
	// handleOAuthWait 相同的「整体覆盖保存」路径。
	renewed := &Account{
		ID:        "acct-fp-1",
		Name:      "fp user",
		Email:     "fp@example.com",
		Region:    RegionGlobal,
		AuthMode:  "oauth",
		APIMode:   "openai",
		Active:    true,
		Tags:      []string{"keep"},
		CreatedAt: time.Now(),
	}
	preserveDeviceIdentity(renewed, "acct-fp-1")
	if err := Save(renewed); err != nil {
		t.Fatalf("save renewed: %v", err)
	}

	got, err := Get("acct-fp-1")
	if err != nil || got == nil {
		t.Fatalf("get: %v", err)
	}
	if got.CheckinFingerprint == nil {
		t.Fatal("checkin_fingerprint must survive re-auth (otherwise Sonus/Cantus/Fast vanish)")
	}
	if got.CheckinFingerprint.ClientType != "10" || got.CheckinFingerprint.Version != "1.1.69" {
		t.Errorf("fingerprint protocol changed: clienttype=%q version=%q, want 10 / 1.1.69",
			got.CheckinFingerprint.ClientType, got.CheckinFingerprint.Version)
	}
	if got.CheckinFingerprint.MachineID != fp.MachineID {
		t.Errorf("machine id changed: %q -> %q", fp.MachineID, got.CheckinFingerprint.MachineID)
	}
	if got.CheckinFingerprint.MachineToken != fp.MachineToken {
		t.Errorf("machine token changed: %q -> %q", fp.MachineToken, got.CheckinFingerprint.MachineToken)
	}
	if got.DeviceIdentity == nil {
		t.Fatal("device_identity must survive re-auth")
	}
	if !got.DeviceIdentity.Real {
		t.Error("device_identity.real flag lost")
	}
	if !got.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want %v (must not be reset on re-auth)", got.CreatedAt, created)
	}
}

// TestPreserveDeviceIdentityNewAccount 首次登录（账号不存在）时不应报错，
// 也不应凭空造出指纹。
func TestPreserveDeviceIdentityNewAccount(t *testing.T) {
	SetDataRoot(t.TempDir())

	fresh := &Account{ID: "acct-fp-new", Name: "new user", Region: RegionCN}
	preserveDeviceIdentity(fresh, "acct-fp-new")

	if fresh.CheckinFingerprint != nil {
		t.Error("new account must not get a fabricated fingerprint")
	}
	if fresh.DeviceIdentity != nil {
		t.Error("new account must not get a fabricated device identity")
	}
}
