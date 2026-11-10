package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"qoder2api/account"
	"qoder2api/internal/cosy"
	"qoder2api/logger"
)

// 设备身份一致性审计：自证「账号的设备指纹没有漂移」。
//
// 为什么需要：设备指纹一旦漂移，对上游等于「账号换了一台设备」——可能触发
// 重新画像、重新风控，前期攒的可信度清零。漂移的原因必须能区分：
//   - 人为变更（legacy 开关切换、machine_salt 被改）→ WARN，说明原因即可；
//   - 无法解释的变更（派生逻辑被改动，属回归）→ ERROR，必须报警。
//
// 快照与账号数据同目录（identity_snapshot.json），首次运行自动初始化。
// 快照里只存派生值与盐的哈希——不存盐原文、不存任何 token。

// 审计结论。
const (
	IdentityOK     = "ok"            // 与快照一致
	IdentityInit   = "initialized"   // 首次运行（已写入快照）
	IdentityManual = "manual_change" // 不一致，但由开关 / 盐变更解释（人为）
	IdentityDrift  = "drift"         // 不一致且无法解释（疑似回归，告警）
)

// identityRecord 单个账号的派生值快照。
type identityRecord struct {
	MachineID    string `json:"machine_id"`
	SessionID    string `json:"session_id"`
	MachineToken string `json:"machine_token"`
	MachineType  string `json:"machine_type"`
	SaltHash     string `json:"salt_hash"` // sha256(MachineSalt) 前 16 位；不存盐原文
	Legacy       bool   `json:"legacy"`    // 记录时的 legacy 形态开关状态
	NoSalt       bool   `json:"no_salt"`   // 记录时的「禁用本机盐」开关状态
}

// IdentityAuditRow 对外暴露的审计结果（只给 machineId 前 8 位，
// 不给全量派生值 / token / 盐）。
type IdentityAuditRow struct {
	Account         string `json:"account"`
	MachineIDPrefix string `json:"machine_id_prefix"`
	Conclusion      string `json:"conclusion"`
	Drift           bool   `json:"drift"`
	Detail          string `json:"detail,omitempty"`
}

type identitySnapshotFile struct {
	Accounts map[string]identityRecord `json:"accounts"`
}

// identitySnapshotPath 快照路径：与账号数据同目录，随 DataRoot 隔离
// （CN / Global 双实例各有各的快照，互不干扰）。
func identitySnapshotPath() string {
	return filepath.Join(account.DataRoot(), "identity_snapshot.json")
}

// currentSaltHash 当前本机盐的指纹（哈希，不暴露原文）。
func currentSaltHash() string {
	salt := ""
	if st := loadSettingsOrNil(); st != nil {
		salt = st.MachineSalt
	}
	sum := sha256.Sum256([]byte(salt))
	return hex.EncodeToString(sum[:])[:16]
}

// identityLegacyOn 当前是否处于 legacy 设备 ID 形态。
func identityLegacyOn() bool {
	return os.Getenv("QD_DEVICE_ID_LEGACY") == "1"
}

// identityNoSaltOn 当前是否显式关闭了本机盐（QD_NO_MACHINE_SALT=1）。
func identityNoSaltOn() bool {
	return account.MachineSaltDisabled()
}

// deriveIdentityFor 由账号会话现场计算派生值（与出站请求用的同一路径）。
func deriveIdentityFor(s *Slot) identityRecord {
	seed := cosy.FingerprintSeed(s.sess.Identity.Uid, s.sess.Identity.SecurityOauthToken)
	return identityRecord{
		MachineID:    cosy.DeriveMachineID(seed),
		SessionID:    cosy.DeriveSessionID(seed),
		MachineToken: cosy.DeriveMachineToken(seed),
		MachineType:  cosy.DeriveMachineType(seed),
		SaltHash:     currentSaltHash(),
		Legacy:       identityLegacyOn(),
		NoSalt:       identityNoSaltOn(),
	}
}

// loadIdentitySnapshot 读取快照；文件不存在 / 损坏返回空快照（首次运行）。
func loadIdentitySnapshot() identitySnapshotFile {
	var snap identitySnapshotFile
	data, err := os.ReadFile(identitySnapshotPath())
	if err != nil {
		return identitySnapshotFile{Accounts: map[string]identityRecord{}}
	}
	if err := json.Unmarshal(data, &snap); err != nil || snap.Accounts == nil {
		return identitySnapshotFile{Accounts: map[string]identityRecord{}}
	}
	return snap
}

// saveIdentitySnapshot 原子写回快照（临时文件 + rename）。
func saveIdentitySnapshot(snap identitySnapshotFile) error {
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	path := identitySnapshotPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// identitySaltChangeDetail 解释盐指纹变化的两种人为原因（盐被改 /
// 开关被切），让运维一眼看出该回退还是该接受。
func identitySaltChangeDetail(old, cur identityRecord) string {
	if old.NoSalt != cur.NoSalt {
		if cur.NoSalt {
			return "machine salt disabled via QD_NO_MACHINE_SALT=1; fingerprints fell back to the salt-free baseline (expected)"
		}
		return "machine salt re-enabled (QD_NO_MACHINE_SALT unset); fingerprints are salted again (expected)"
	}
	return "machine salt changed; all account fingerprints shifted (expected)"
}

// identityWarnOnce 保证启动漂移告警只打一次（不随请求 / 重复审计刷屏）。
var identityWarnOnce sync.Once

// WarnIdentityShift 启动时醒目告警：指纹因人为开关 / 盐变更而整体变化。
//
// 为什么必须醒目：这对上游等于「账号换了一台设备」——可能触发重新画像、
// 重新风控，前期攒的可信度清零。运维必须立刻知道：哪些账号受影响、
// 如何回退（QD_NO_MACHINE_SALT=1 保旧盐形态 / QD_DEVICE_ID_LEGACY=1 保旧
// ID 形态 / 恢复升级前的 settings.json 备份）。
//
// 只打一次：诊断端点可反复查，启动日志不该被重复告警淹没。
func WarnIdentityShift(rows []IdentityAuditRow) {
	identityWarnOnce.Do(func() {
		shifted := 0
		drifted := 0
		for _, r := range rows {
			switch r.Conclusion {
			case IdentityManual:
				shifted++
			case IdentityDrift:
				drifted++
			}
		}
		if shifted == 0 && drifted == 0 {
			return
		}
		if drifted > 0 {
			logger.Error("设备指纹漂移告警：%d 个账号的派生值发生无法解释的变化（%d 个为人为开关/盐变更）。"+
				"这对上游等于「账号换了一台设备」，可能触发重新画像与风控。"+
				"回退方法：QD_NO_MACHINE_SALT=1（不混盐，回到升级前基准）或 QD_DEVICE_ID_LEGACY=1（回到 32 位 hex 形态），"+
				"或恢复升级前的 settings.json 备份。详情见 GET /api/identity/audit",
				drifted, shifted)
			return
		}
		logger.Error("设备指纹变更告警：%d 个账号的指纹因人为开关/盐变更而变化（升级的正常伴随）。"+
			"这对上游等于「账号换了一台设备」——如不希望此次漂移："+
			"设 QD_NO_MACHINE_SALT=1（不混盐）或 QD_DEVICE_ID_LEGACY=1（旧 ID 形态）后重启，"+
			"或恢复升级前的 settings.json 备份。详情见 GET /api/identity/audit",
			shifted)
	})
}

// AuditIdentities 对池内每个账号做派生值比对，并按需更新快照。
//
// 三态判定（见文件头注释）：一致 / 人为变更 / 无法解释的漂移。
// 漂移时不覆盖快照——让告警状态保持可见，直到人工确认处理。
func (p *Pool) AuditIdentities() []IdentityAuditRow {
	if p == nil {
		return nil
	}
	slots := p.snapshot()
	if len(slots) == 0 {
		return nil
	}
	snap := loadIdentitySnapshot()
	changed := false
	rows := make([]IdentityAuditRow, 0, len(slots))

	for _, s := range slots {
		cur := deriveIdentityFor(s)
		prefix := cur.MachineID
		if len(prefix) > 8 {
			prefix = prefix[:8]
		}
		row := IdentityAuditRow{
			Account:         s.ID,
			MachineIDPrefix: prefix,
			Conclusion:      IdentityOK,
		}

		old, ok := snap.Accounts[s.ID]
		switch {
		case !ok:
			// 首次运行：落盘基线
			snap.Accounts[s.ID] = cur
			changed = true
			row.Conclusion = IdentityInit
			row.Detail = "first run; identity snapshot written"
			logger.Info("identity audit: account %s initialized (machineid=%s…)", s.ID, prefix)

		case old == cur:
			row.Conclusion = IdentityOK

		case old.SaltHash != cur.SaltHash:
			// 盐变了 = 全体账号换设备：人为变更，更新基线并明示
			snap.Accounts[s.ID] = cur
			changed = true
			row.Conclusion = IdentityManual
			row.Detail = identitySaltChangeDetail(old, cur)
			logger.Info("identity audit: account %s machine salt changed (fingerprint shift expected)", s.ID)

		case old.Legacy != cur.Legacy:
			// legacy 形态开关切换：人为变更
			snap.Accounts[s.ID] = cur
			changed = true
			row.Conclusion = IdentityManual
			row.Detail = "device id form switched (legacy env); fingerprint form changed (expected)"
			logger.Info("identity audit: account %s device id form switched (legacy=%v)", s.ID, cur.Legacy)

		case old.NoSalt != cur.NoSalt:
			// 「禁用本机盐」开关切换：人为变更（这正是存量部署的保旧路径）
			snap.Accounts[s.ID] = cur
			changed = true
			row.Conclusion = IdentityManual
			row.Detail = "machine salt on/off switched (QD_NO_MACHINE_SALT); fingerprint base changed (expected)"
			logger.Info("identity audit: account %s machine salt switch changed (no_salt=%v)", s.ID, cur.NoSalt)

		default:
			// 无法解释：派生逻辑疑似被改动（回归）。保留旧快照使告警持续可见。
			row.Conclusion = IdentityDrift
			row.Drift = true
			row.Detail = "derived values changed without salt/legacy change; check derivation logic"
			logger.Error("identity audit: account %s device fingerprint DRIFT (machineid %s… -> %s…) without salt/legacy change; possible regression",
				s.ID, old.MachineID[:8], cur.MachineID[:8])
		}
		rows = append(rows, row)
	}

	if changed {
		if err := saveIdentitySnapshot(snap); err != nil {
			logger.Error("identity audit: save snapshot: %v", err)
		}
	}
	return rows
}
