// Package cosy —— 稳定设备指纹派生 (derive_id)
//
// 参照 qoder2api-hub / WorkBuddy 的方案：以账号 UID + 业务盐单向哈希派生
// 固定的伪物理设备特征，确保：
//   - 同一账号长期稳定：出站请求永远来自同一台虚拟设备，规避机器码漂移风控
//   - 多账号天然隔离：不同账号机器码彼此独立，阻断跨账号关联检测
//
// 与 hub 的 qoder_fingerprint.py 在「无盐种子」下逐字节同构（同一 seed 产出相同值）：
//   - machineId  : md5("machine:{seed}")      → 32 位十六进制
//   - machineType: md5("machinetype:{seed}")  → 32 位十六进制截 18 位
//   - machineToken: sha512("machinetoken:{seed}") → base64url 截 43 位
//
// 本机盐（install salt）：可选混入所有派生种子。纯 uid 派生的问题是
// 「知道 uid 即可算出指纹，且所有同源部署派生值完全相同」——上游一旦
// 识别派生模式可全局拉黑。设置本机盐后，每个部署拥有独立的指纹空间；
// 盐为空时保持 hub 兼容（测试向量成立）。盐生成后必须保持不变：
// 变更 salt 即指纹整体漂移，等价于换设备。
package cosy

import (
	"crypto/md5"
	"crypto/sha512"
	"encoding/base64"
	"fmt"
	"sync"
)

var (
	installSaltMu sync.RWMutex
	installSalt   string
)

// SetInstallSalt 设置本机指纹盐。推荐由 settings.json 的 machine_salt 提供，
// 由 account.EnsureMachineSalt() 首次启动时自动生成并落盘。
func SetInstallSalt(salt string) {
	installSaltMu.Lock()
	installSalt = salt
	installSaltMu.Unlock()
}

// saltedSeed 把本机盐混入派生种子；盐为空时原样返回（hub 兼容）。
func saltedSeed(seed string) string {
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

func deriveID(seed, salt string) string {
	sum := md5.Sum([]byte(salt + ":" + saltedSeed(seed)))
	return fmt.Sprintf("%x", sum)
}

// DeriveMachineID 派生稳定的 cosy-machineid（32 位十六进制）。
func DeriveMachineID(seed string) string {
	return deriveID(seed, "machine")
}

// DeriveSessionID 派生稳定的会话标识（与 hub derive_id(uid,"session") 同构）。
func DeriveSessionID(seed string) string {
	return deriveID(seed, "session")
}

// DeriveMachineType 派生稳定的 18 位 machine type（与 hub derive_machine_type 同构）。
func DeriveMachineType(seed string) string {
	return deriveID(seed, "machinetype")[:18]
}

// DeriveMachineToken 派生稳定的 machine token（与 hub derive_machine_token 同构）。
func DeriveMachineToken(seed string) string {
	sum := sha512.Sum512([]byte("machinetoken:" + saltedSeed(seed)))
	return base64.RawURLEncoding.EncodeToString(sum[:])[:43]
}
