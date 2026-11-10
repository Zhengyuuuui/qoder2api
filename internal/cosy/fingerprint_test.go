package cosy

import (
	"strings"
	"testing"
)

// 基准形态（无盐种子）的参考向量，用于锁定 legacy 开关下的存量值：
//
//	machine 005b8945c0659064f8d25299980a27b3
//	session e1527624132e0a39ebcb328440517365
//	mtype   66ea01f7983702b088
//	mtoken  zzpUYGGMSPEfJVrGQWHj7SBYaRUMwPMK0B4QN_aqKP0
const testUID = "test-uid-123"

// assertUUIDv4 断言 UUIDv4 形态：36 字符、8-4-4-4-12 分组、
// 第 13 位（1-indexed）为版本位 '4'、第 17 位为变体位 'a'。
func assertUUIDv4(t *testing.T, id string) {
	t.Helper()
	if len(id) != 36 {
		t.Fatalf("len = %d, want 36: %s", len(id), id)
	}
	for _, pos := range []int{8, 13, 18, 23} {
		if id[pos] != '-' {
			t.Errorf("dash expected at %d, got %q in %s", pos, id[pos], id)
		}
	}
	if id[14] != '4' {
		t.Errorf("version nibble = %q, want '4'", id[14])
	}
	if id[19] != 'a' {
		t.Errorf("variant nibble = %q, want 'a'", id[19])
	}
	for i := 0; i < 36; i++ {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		c := id[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("non-hex char %q at %d in %s", c, i, id)
		}
	}
}

// TestDeriveMachineIDUUIDForm 默认形态：machineId/sessionId 为 UUIDv4。
func TestDeriveMachineIDUUIDForm(t *testing.T) {
	SetInstallSalt("")
	t.Setenv("QD_DEVICE_ID_LEGACY", "")

	id := DeriveMachineID(testUID)
	assertUUIDv4(t, id)
	sid := DeriveSessionID(testUID)
	assertUUIDv4(t, sid)

	// 幂等：同一种子永不漂移
	if DeriveMachineID(testUID) != id {
		t.Error("machine id not idempotent")
	}
	// 跨账号隔离
	if DeriveMachineID("acct-a") == DeriveMachineID("acct-b") {
		t.Error("machine id collides across accounts")
	}
	// 有盐：仍为 UUID 形态，且偏离无盐值
	SetInstallSalt("unit-test-salt")
	salted := DeriveMachineID(testUID)
	assertUUIDv4(t, salted)
	if salted == id {
		t.Error("salted id equals plain id")
	}
	SetInstallSalt("")
}

// TestLegacyDeviceIDForm legacy 开关锁定存量 32 位 hex 值。
func TestLegacyDeviceIDForm(t *testing.T) {
	SetInstallSalt("")
	t.Setenv("QD_DEVICE_ID_LEGACY", "1")

	if got := DeriveMachineID(testUID); got != "005b8945c0659064f8d25299980a27b3" {
		t.Errorf("legacy machineid = %s", got)
	}
	if got := DeriveSessionID(testUID); got != "e1527624132e0a39ebcb328440517365" {
		t.Errorf("legacy sessionid = %s", got)
	}
	// machineType / machineToken 不受形态开关影响
	if got := DeriveMachineType(testUID); got != "66ea01f7983702b088" {
		t.Errorf("machinetype = %s", got)
	}
	if got := DeriveMachineToken(testUID); got != "zzpUYGGMSPEfJVrGQWHj7SBYaRUMwPMK0B4QN_aqKP0" {
		t.Errorf("machinetoken = %s", got)
	}
}

func TestDeriveStableAndIsolated(t *testing.T) {
	SetInstallSalt("")
	t.Setenv("QD_DEVICE_ID_LEGACY", "")

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
	// 格式：UUIDv4 / 18 位纯 hex / 43 位 token
	assertUUIDv4(t, DeriveMachineID(testUID))
	if len(DeriveMachineType(testUID)) != 18 {
		t.Errorf("machinetype len = %d, want 18", len(DeriveMachineType(testUID)))
	}
	if len(DeriveMachineToken(testUID)) != 43 {
		t.Errorf("machinetoken len = %d, want 43", len(DeriveMachineToken(testUID)))
	}
}

func TestDeriveInstallSalt(t *testing.T) {
	// 无盐：基准基线
	SetInstallSalt("")
	t.Setenv("QD_DEVICE_ID_LEGACY", "")
	plainID := DeriveMachineID(testUID)
	plainToken := DeriveMachineToken(testUID)

	// 有盐：派生值必须偏离基线，但自身保持幂等、格式不变
	SetInstallSalt("unit-test-salt")
	saltedID := DeriveMachineID(testUID)
	saltedToken := DeriveMachineToken(testUID)

	if saltedID == plainID {
		t.Error("salted machineid equals plain")
	}
	assertUUIDv4(t, saltedID)
	if saltedToken == plainToken || len(saltedToken) != 43 {
		t.Errorf("salted machinetoken invalid: %s", saltedToken)
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

	// 清空盐 → 恢复基准值
	SetInstallSalt("")
	if DeriveMachineID(testUID) != plainID || DeriveMachineToken(testUID) != plainToken {
		t.Error("clearing salt must restore baseline fingerprint")
	}
}

func TestDeriveMachineHostname(t *testing.T) {
	SetInstallSalt("")
	h := DeriveMachineHostname(testUID)
	if !strings.HasPrefix(h, "DESKTOP-") {
		t.Errorf("hostname = %s, want DESKTOP- prefix", h)
	}
	suffix := strings.TrimPrefix(h, "DESKTOP-")
	if len(suffix) != 7 {
		t.Errorf("suffix len = %d, want 7", len(suffix))
	}
	if suffix != strings.ToUpper(suffix) {
		t.Errorf("suffix not uppercase: %s", suffix)
	}
	for _, c := range suffix {
		if !((c >= '0' && c <= '9') || (c >= 'A' && c <= 'F')) {
			t.Errorf("non-hex char %q in %s", c, suffix)
		}
	}
	// 稳定 + 跨账号隔离
	if DeriveMachineHostname(testUID) != h {
		t.Error("hostname not idempotent")
	}
	if DeriveMachineHostname("other-uid") == h {
		t.Error("hostname collides across accounts")
	}
}

// TestNoMachineSaltEnvIsByteExactBaseline 审计 P1-2 核心回归：
// 升级后存量部署指纹漂移（上游把每个账号当新设备）。QD_NO_MACHINE_SALT=1
// 时，即使 salt 仍在 settings.json 里，派生结果也必须与「无盐基准」
// 逐字节一致 —— 这是老部署升级后平滑保旧指纹的唯一回退路径。
func TestNoMachineSaltEnvIsByteExactBaseline(t *testing.T) {
	// 基准：无盐、非 legacy
	SetInstallSalt("")
	t.Setenv("QD_DEVICE_ID_LEGACY", "")
	baseID := DeriveMachineID(testUID)
	baseToken := DeriveMachineToken(testUID)
	baseType := DeriveMachineType(testUID)
	baseHost := DeriveMachineHostname(testUID)

	// 有盐：必须偏离基准（否则盐形同虚设）
	SetInstallSalt("upgrade-salt")
	if DeriveMachineID(testUID) == baseID {
		t.Fatal("salted machineid must differ from baseline")
	}

	// 开关打开：salt 仍在内存/settings 里，但派生结果必须精确回落基准
	t.Setenv(noMachineSaltEnv, "1")
	if DeriveMachineID(testUID) != baseID {
		t.Errorf("machineid with salt disabled = %s, want baseline %s", DeriveMachineID(testUID), baseID)
	}
	if DeriveMachineToken(testUID) != baseToken {
		t.Error("machinetoken with salt disabled must equal baseline")
	}
	if DeriveMachineType(testUID) != baseType {
		t.Error("machinetype with salt disabled must equal baseline")
	}
	if DeriveMachineHostname(testUID) != baseHost {
		t.Error("hostname with salt disabled must equal baseline")
	}

	// SetInstallSalt 在开关打开时不得生效（盐被强制忽略）
	SetInstallSalt("yet-another-salt")
	if DeriveMachineID(testUID) != baseID {
		t.Error("SetInstallSalt must be a no-op while salt is disabled")
	}

	// 关掉开关 → 立即恢复有盐派生
	t.Setenv(noMachineSaltEnv, "")
	if DeriveMachineID(testUID) == baseID {
		t.Error("re-enabling salt must change the fingerprint again")
	}
}

// TestNoMachineSaltOrthogonalToLegacy 两个开关正交（审计 P1-2）：
// QD_NO_MACHINE_SALT 管是否混盐，QD_DEVICE_ID_LEGACY 管 UUID/hex 形态，
// 组合应得到「legacy 形态 + 无盐」的确定结果。
func TestNoMachineSaltOrthogonalToLegacy(t *testing.T) {
	SetInstallSalt("")
	// legacy + 无盐
	t.Setenv(noMachineSaltEnv, "1")
	t.Setenv("QD_DEVICE_ID_LEGACY", "1")
	legacyNoSalt := DeriveMachineID(testUID)
	// 非 legacy + 无盐
	t.Setenv("QD_DEVICE_ID_LEGACY", "")
	uuidNoSalt := DeriveMachineID(testUID)

	if legacyNoSalt == uuidNoSalt {
		t.Fatal("legacy and uuid forms must differ even without salt")
	}
	assertUUIDv4(t, uuidNoSalt)
	// legacy 形态：32 位 hex
	if len(legacyNoSalt) != 32 {
		t.Errorf("legacy machineid len = %d, want 32", len(legacyNoSalt))
	}
	for _, c := range legacyNoSalt {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("legacy machineid not lowercase hex: %s", legacyNoSalt)
			break
		}
	}

	// 加盐后 legacy 形态仍成立且不同（legacy 只管形态，盐照样参与）
	t.Setenv(noMachineSaltEnv, "")
	SetInstallSalt("ortho-salt")
	t.Setenv("QD_DEVICE_ID_LEGACY", "1")
	saltedLegacy := DeriveMachineID(testUID)
	if saltedLegacy == legacyNoSalt {
		t.Error("salt must still participate under legacy form")
	}
	if len(saltedLegacy) != 32 {
		t.Errorf("salted legacy machineid len = %d, want 32", len(saltedLegacy))
	}
}
