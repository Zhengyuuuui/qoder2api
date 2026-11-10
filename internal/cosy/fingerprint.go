// Package cosy —— 稳定设备指纹派生 (derive_id)
//
// 以账号 UID + 业务盐单向哈希派生固定的伪物理设备特征，确保：
//   - 同一账号长期稳定：出站请求永远来自同一台虚拟设备，规避机器码漂移风控
//   - 多账号天然隔离：不同账号机器码彼此独立，阻断跨账号关联检测
//
// 派生规则（无盐种子的基准形态）：
//   - machineId  : md5("machine:{seed}")       → UUIDv4 形态（版本位 4/变体位 a）
//   - sessionId  : md5("session:{seed}")       → UUIDv4 形态
//   - machineType: md5("machinetype:{seed}")   → 32 位十六进制截 18 位（纯 hex）
//   - machineToken: sha512("machinetoken:{seed}") → base64url 截 43 位
//
// machineId/sessionId 采用 UUIDv4 形态是为了与官方客户端的设备标识形态
// 一致——纯 32 位 hex 在聚类视角下是显眼的非标准形态。只改形态不改熵：
// 值仍由账号种子稳定派生，同一 seed 永远得到同一 UUID。
// 存量部署可用 QD_DEVICE_ID_LEGACY=1 回退到旧的 32 位 hex 形态
// （形态变化对上游等于「账号换了一台设备」，必须可平滑保旧）。
//
// 本机盐（install salt）：可选混入所有派生种子。纯 uid 派生的问题是
// 「知道 uid 即可算出指纹，且所有同源部署派生值完全相同」——上游一旦
// 识别派生模式可全局拉黑。设置本机盐后，每个部署拥有独立的指纹空间；
// 盐为空时保持基准形态（测试向量成立）。盐生成后必须保持不变：
// 变更 salt 即指纹整体漂移，等价于换设备。
package cosy

import (
	"crypto/md5"
	"crypto/sha512"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"sync"
)

// legacyDeviceIDEnv 置 "1" 时 machineId/sessionId 保持旧的 32 位无连字符
// hex 形态。用于存量账号池平滑过渡：新形态上线初期若上游对 UUID 形态
// 有异常反馈，可一键保旧排障。
const legacyDeviceIDEnv = "QD_DEVICE_ID_LEGACY"

// noMachineSaltEnv 置 "1" 时不混本机盐：派生结果与「无盐」基准逐字节
// 一致。与 legacy 开关正交——那个管形态（UUID/hex），这个管是否混盐。
// 用于存量部署升级后平滑保旧指纹（盐从无到有对上游等于全员换设备）。
const noMachineSaltEnv = "QD_NO_MACHINE_SALT"

// machineSaltDisabled 是否显式关闭本机盐（环境变量权威，覆盖任何
// SetInstallSalt 调用——开关必须能真正压住已落盘的盐）。
func machineSaltDisabled() bool {
	return os.Getenv(noMachineSaltEnv) == "1"
}

var (
	installSaltMu sync.RWMutex
	installSalt   string
)

// SetInstallSalt 设置本机指纹盐。推荐由 settings.json 的 machine_salt 提供，
// 由 account.EnsureMachineSalt() 首次启动时自动生成并落盘。
// QD_NO_MACHINE_SALT=1 时调用无效（盐被强制关闭）。
func SetInstallSalt(salt string) {
	if machineSaltDisabled() {
		return
	}
	installSaltMu.Lock()
	installSalt = salt
	installSaltMu.Unlock()
}

// saltedSeed 把本机盐混入派生种子；盐为空或开关关闭时原样返回（基准形态）。
func saltedSeed(seed string) string {
	if machineSaltDisabled() {
		return seed
	}
	installSaltMu.RLock()
	defer installSaltMu.RUnlock()
	if installSalt == "" {
		return seed
	}
	return seed + "|salt:" + installSalt
}

// FingerprintSeed 选择指纹种子：优先账号 UID；
// UID 未知时（jobToken 交换发生在拿到 uid 之前）退回凭证令牌，
// 保证同一凭证跨次调用派生结果一致、不随进程重启漂移。
func FingerprintSeed(uid, credential string) string {
	if uid != "" {
		return uid
	}
	return "cred:" + credential
}

// rawDeriveID 原始 md5 hex（不做 UUID 整形）。machineType 等依赖纯 hex
// 形态的字段必须走这里——UUID 整形会引入连字符，破坏 18 位定长假设。
func rawDeriveID(seed, salt string) string {
	sum := md5.Sum([]byte(salt + ":" + saltedSeed(seed)))
	return fmt.Sprintf("%x", sum)
}

// deriveID 默认输出 UUIDv4 形态；legacy 环境开关下输出原始 32 位 hex。
func deriveID(seed, salt string) string {
	hex := rawDeriveID(seed, salt)
	if os.Getenv(legacyDeviceIDEnv) == "1" {
		return hex
	}
	return hexToUUIDv4(hex)
}

// hexToUUIDv4 把 32 位 md5 hex 整形为 UUIDv4 形态（8-4-4-4-12）：
// 第 13 位十六进制（1-indexed）固定版本位 '4'，第 17 位固定变体位 'a'。
func hexToUUIDv4(hex string) string {
	b := []byte(hex)
	b[12] = '4'
	b[16] = 'a'
	return fmt.Sprintf("%s-%s-%s-%s-%s", b[0:8], b[8:12], b[12:16], b[16:20], b[20:32])
}

// DeriveMachineID 派生稳定的 cosy-machineid（UUIDv4 形态，36 字符）。
func DeriveMachineID(seed string) string {
	return deriveID(seed, "machine")
}

// DeriveSessionID 派生稳定的会话标识（UUIDv4 形态，36 字符）。
func DeriveSessionID(seed string) string {
	return deriveID(seed, "session")
}

// DeriveMachineType 派生稳定的 18 位 machine type（纯 hex，不受 UUID
// 形态开关影响）。
func DeriveMachineType(seed string) string {
	return rawDeriveID(seed, "machinetype")[:18]
}

// DeriveMachineToken 派生稳定的 machine token（base64url 43 位）。
func DeriveMachineToken(seed string) string {
	sum := sha512.Sum512([]byte("machinetoken:" + saltedSeed(seed)))
	return base64.RawURLEncoding.EncodeToString(sum[:])[:43]
}

// DeriveMachineHostname 派生稳定的主机名形态（DESKTOP-XXXXXXX，后 7 位
// 为大写 hex）。按账号稳定派生，绝不用真实主机名——真实名会暴露网关
// 所在机器，且多账号相同会形成跨账号关联。
func DeriveMachineHostname(seed string) string {
	sum := md5.Sum([]byte("hostname:" + saltedSeed(seed)))
	return "DESKTOP-" + strings.ToUpper(fmt.Sprintf("%x", sum)[:7])
}
