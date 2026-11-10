package bridge

import (
	"testing"

	"qoder2api/account"
	"qoder2api/internal/cosy"
)

// 签到与推理用同一账号 → 设备身份的 machineId 必须一致。
//
// 这是「同源」不变量：若签到自己派生一份而推理用另一份，上游看到的就是
// 「同一账号在两台设备上活动」——最容易被风控判定识别的形态。
func TestInferenceAndCheckinShareMachineID(t *testing.T) {
	sess, err := cosy.NewSession(
		cosy.AuthIdentity{Uid: "uid-same", UserType: "personal_standard"},
		"mid-same", "mtok-same", "mtype-same",
	)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	// 推理侧（无真实指纹）：用派生身份
	ident := DeviceIdentityOf(sess, nil)
	if ident == nil || ident.MachineID != sess.MachineId {
		t.Fatalf("derived identity = %+v, want machineId %q", ident, sess.MachineId)
	}
	if ident.Real {
		t.Error("derived identity must not be flagged as real")
	}
	// 推理侧（有真实指纹）：以真实指纹为准
	fp := &account.CheckinFingerprint{
		MachineID: "real-mid", MachineToken: "real-mtok", MachineType: "5",
	}
	ident2 := DeviceIdentityOf(sess, fp)
	if ident2.MachineID != "real-mid" {
		t.Errorf("real fingerprint should win, got %q", ident2.MachineID)
	}
	if !ident2.Real {
		t.Error("identity should be flagged real")
	}
}
