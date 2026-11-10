package bridge

import (
	"qoder2api/account"
	"qoder2api/internal/cosy"
)

// 账号设备身份：推理请求与签到请求必须使用同一份。
//
// 为什么必须同源：上游按「账号 + 设备」聚类。若签到带一套指纹、推理带
// 另一套，同一账号就表现为「同时在两台设备上活动」——这是最容易触发
// 风控判定的形态之一，且对签到的后果尤其隐蔽：活动被隐藏（账号看不到
// 权益），而不是直接报错。
//
// 来源优先级：官方抓包的真实设备指纹 > 按账号稳定派生的指纹。
// 真实指纹优先是因为派生值只是形态像、不是真设备；服务端按签发来源校验。

// DeviceIdentityOf 取出该账号推理请求实际使用的设备身份。
//
// sess 是推理侧的会话上下文（已含 uid 派生的指纹，或真实指纹）。
// fp 非空时以真实指纹为准。
func DeviceIdentityOf(sess *cosy.SessionContext, fp *account.CheckinFingerprint) *account.DeviceIdentity {
	if sess == nil {
		return nil
	}
	if fp != nil && fp.MachineToken != "" {
		return &account.DeviceIdentity{
			MachineID:    fp.MachineID,
			MachineToken: fp.MachineToken,
			MachineType:  fp.MachineType,
			ClientType:   orDefault(fp.ClientType, "10"),
			Version:      orDefault(fp.Version, "1.1.69"),
			Hostname:     fp.Hostname,
			MachineOS:    fp.MachineOS,
			MachineCode:  fp.MachineCode,
			Real:         true,
		}
	}
	// 无真实指纹：用推理侧派生的那份（按 uid 稳定派生）
	return &account.DeviceIdentity{
		MachineID:    sess.MachineId,
		MachineToken: sess.MachineToken,
		MachineType:  sess.MachineType,
		ClientType:   "5",
		Version:      cosy.Version,
		Real:         false,
	}
}

// SlotDeviceIdentity 返回该 Slot 的设备身份（推理侧口径）。
func (s *Slot) SlotDeviceIdentity() *account.DeviceIdentity {
	if s == nil {
		return nil
	}
	var fp *account.CheckinFingerprint
	if s.client != nil {
		fp = s.client.Fingerprint()
	}
	return DeviceIdentityOf(s.sess, fp)
}

// Fingerprint 返回该客户端使用的真实设备指纹（无则返回 nil）。
//
// 供签到路径取用：签到必须与推理用同一份设备身份，否则同一账号会表现为
// 在两台设备上活动。
func (c *BearerClient) Fingerprint() *account.CheckinFingerprint {
	if c == nil {
		return nil
	}
	return c.fp
}
