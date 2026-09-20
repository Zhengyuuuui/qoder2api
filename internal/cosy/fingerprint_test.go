package cosy

import "testing"

// 参考值由 hub 的 qoder_fingerprint.py 对同一 seed 计算所得（交叉校验），
// 仅在「无本机盐」的种子下成立，测试入口统一 SetInstallSalt("")。
//
//	machine 005b8945c0659064f8d25299980a27b3
//	session e1527624132e0a39ebcb328440517365
//	mtype   66ea01f7983702b088
//	mtoken  zzpUYGGMSPEfJVrGQWHj7SBYaRUMwPMK0B4QN_aqKP0
const testUID = "test-uid-123"

func TestDeriveMatchesHubPython(t *testing.T) {
	SetInstallSalt("")
	if got := DeriveMachineID(testUID); got != "005b8945c0659064f8d25299980a27b3" {
		t.Errorf("DeriveMachineID = %s", got)
	}
	if got := DeriveSessionID(testUID); got != "e1527624132e0a39ebcb328440517365" {
		t.Errorf("DeriveSessionID = %s", got)
	}
	if got := DeriveMachineType(testUID); got != "66ea01f7983702b088" {
		t.Errorf("DeriveMachineType = %s", got)
	}
	if got := DeriveMachineToken(testUID); got != "zzpUYGGMSPEfJVrGQWHj7SBYaRUMwPMK0B4QN_aqKP0" {
		t.Errorf("DeriveMachineToken = %s", got)
	}
}

func TestDeriveStableAndIsolated(t *testing.T) {
	SetInstallSalt("")
	// 幂等：同一种子永不漂移
	if DeriveMachineID("acct-a") != DeriveMachineID("acct-a") {
		t.Fatal("machine id not stable for same uid")
	}
	// 隔离：不同账号机器码互不相同
	if DeriveMachineID("acct-a") == DeriveMachineID("acct-b") {
		t.Fatal("machine id collides across accounts")
	}
	// uid 未知时退回凭证种子，仍保持稳定
	seed := FingerprintSeed("", "pt-secret")
	if FingerprintSeed("", "pt-secret") != seed {
		t.Fatal("credential seed not stable")
	}
	if FingerprintSeed("real-uid", "pt-secret") == seed {
		t.Fatal("uid seed must take precedence over credential seed")
	}
	// 格式
	if len(DeriveMachineID(testUID)) != 32 {
		t.Errorf("machineid len = %d, want 32", len(DeriveMachineID(testUID)))
	}
	if len(DeriveMachineType(testUID)) != 18 {
		t.Errorf("machinetype len = %d, want 18", len(DeriveMachineType(testUID)))
	}
	if len(DeriveMachineToken(testUID)) != 43 {
		t.Errorf("machinetoken len = %d, want 43", len(DeriveMachineToken(testUID)))
	}
}

func TestDeriveInstallSalt(t *testing.T) {
	// 无盐：hub 兼容基线
	SetInstallSalt("")
	plainID := DeriveMachineID(testUID)
	plainToken := DeriveMachineToken(testUID)

	// 有盐：派生值必须偏离 hub 基线，但自身保持幂等、格式不变
	SetInstallSalt("unit-test-salt")
	saltedID := DeriveMachineID(testUID)
	saltedToken := DeriveMachineToken(testUID)

	if saltedID == plainID || len(saltedID) != 32 {
		t.Errorf("salted machineid invalid or equals plain: %s", saltedID)
	}
	if saltedToken == plainToken || len(saltedToken) != 43 {
		t.Errorf("salted machinetoken invalid or equals plain: %s", saltedToken)
	}
	if DeriveMachineID(testUID) != saltedID || DeriveMachineToken(testUID) != saltedToken {
		t.Error("salted derive not idempotent")
	}
	// 多账号隔离在盐下同样成立
	if DeriveMachineID("acct-a") == DeriveMachineID("acct-b") {
		t.Fatal("salted machine id collides across accounts")
	}

	// 盐变更 → 指纹整体变化（运维语义：变更 salt 等价于换设备）
	SetInstallSalt("another-salt")
	if DeriveMachineID(testUID) == saltedID {
		t.Error("changing salt must change fingerprint")
	}

	// 清空盐 → 恢复 hub 兼容值
	SetInstallSalt("")
	if DeriveMachineID(testUID) != plainID || DeriveMachineToken(testUID) != plainToken {
		t.Error("clearing salt must restore hub-compatible fingerprint")
	}
}
