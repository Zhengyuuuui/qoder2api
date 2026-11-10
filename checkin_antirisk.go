package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"time"

	"qoder2api/logger"
)

// 签到防风控：账号间隔 + 时间点错峰。
//
// 背景：签到是「非对话类高频行为」，与对话流量的画像完全不同。多个账号在
// 同一秒批量签到，是非常明显的自动化特征——容易被活动平台的判定识别，
// 导致隐藏可领取活动（这类风险的直接后果是账号看不到权益，不是直接封号）。
//
// 两道防线：
//  1. 账号间隔：账号之间强制最小间隔 + 随机抖动。批量瞬间完成时，节奏
//     特征过于规整（机器行为往往如此）。
//  2. 时间点错峰：同一时刻打散到窗口内，每个账号的偏移由账号 ID 哈希
//     派生——必须稳定派生而非每次随机，否则同一账号的行为画像会逐日漂移。

const (
	// DefaultCheckinAccountGapMS 账号之间的最小间隔（毫秒）。
	DefaultCheckinAccountGapMS = 1000
	// DefaultCheckinJitterMS 账号间隔的随机抖动上限（毫秒）。
	DefaultCheckinJitterMS = 500
	// DefaultCheckinStaggerSec 签到时间点的错峰窗口（秒）。
	//
	// 含义：设定的时刻 ± 该窗口内为每个账号分配一个固定偏移。
	DefaultCheckinStaggerSec = 60
)

// 签到防风控参数（运行时可调，由控制台 settings 热更新）。
var (
	// CheckinAccountGapMS 账号之间的最小间隔（毫秒）；≤0 用默认。
	CheckinAccountGapMS = DefaultCheckinAccountGapMS
	// CheckinJitterMS 账号间隔的随机抖动上限（毫秒）；≤0 用默认。
	CheckinJitterMS = DefaultCheckinJitterMS
	// CheckinStaggerSec 时间点错峰窗口（秒）；≤0 用默认。
	CheckinStaggerSec = DefaultCheckinStaggerSec
	// CheckinStaggerEnabled 错峰开关。false = 偏移恒为 0（回到旧行为）。
	CheckinStaggerEnabled = true
	// CheckinFullDeviceHeaders 签到时是否携带完整设备头集合。
	// 默认 false = 沿用当前行为（只带身份标识）。
	CheckinFullDeviceHeaders = false
)

// ApplyCheckinSettings 应用签到防风控参数。
//
// gapMS / jitterMS / staggerSec ≤0 时回退各自默认值。
// staggerEnabled=false 时偏移恒为 0（回到「所有账号同一秒打」的旧行为）。
func ApplyCheckinSettings(gapMS, jitterMS, staggerSec int, staggerEnabled bool) {
	if gapMS <= 0 {
		gapMS = DefaultCheckinAccountGapMS
	}
	if jitterMS <= 0 {
		jitterMS = DefaultCheckinJitterMS
	}
	if staggerSec <= 0 {
		staggerSec = DefaultCheckinStaggerSec
	}
	CheckinAccountGapMS = gapMS
	CheckinJitterMS = jitterMS
	CheckinStaggerSec = staggerSec
	CheckinStaggerEnabled = staggerEnabled
	logger.Info("checkin anti-risk applied: gap=%dms jitter=%dms stagger=%ds enabled=%v",
		gapMS, jitterMS, staggerSec, staggerEnabled)
}

// checkinAccountGap 返回本次账号间隔等待时长（最小间隔 + 随机抖动）。
//
// 抖动是真随机：固定间隔本身就是最容易被聚类的自动化特征——
// 两个账号之间精确相隔 1000ms，比随机间隔更像机器。
func checkinAccountGap() time.Duration {
	gap := time.Duration(CheckinAccountGapMS) * time.Millisecond
	jitter := time.Duration(checkinJitterNanos(CheckinJitterMS)) * time.Nanosecond
	return gap + jitter
}

// checkinJitterNanos 返回 [0, maxMS*1e6) 纳秒范围内的真随机数。
func checkinJitterNanos(maxMS int) int64 {
	if maxMS <= 0 {
		return 0
	}
	span := int64(maxMS) * int64(time.Millisecond)
	return int64(randomUint64() % uint64(span))
}

// checkinStaggerOffset 返回该账号在错峰窗口内的固定偏移。
//
// 关键不变量：同一账号的偏移必须跨天稳定（由账号 ID 哈希派生，不是每次
// 随机）。否则同一账号每天在窗口里的位置都不同，行为画像会漂移——
// 上游正是靠这种漂移识别自动化。
//
// 不同账号的偏移自然分散：哈希把账号 ID 均匀映射到窗口内。
func checkinStaggerOffset(accountID string) time.Duration {
	if !CheckinStaggerEnabled {
		return 0
	}
	window := int64(CheckinStaggerSec)
	if window <= 0 {
		return 0
	}
	// 取哈希的若干字节映射到 [-window, +window]。
	sum := sha256.Sum256([]byte("checkin-stagger:" + accountID))
	v := int64(binary.BigEndian.Uint64(sum[:8]) % uint64(2*window+1))
	return time.Duration(v-window) * time.Second
}

// checkinSlotTime 返回该账号在某个签到时刻点上的实际执行时刻。
//
// base 是设定时刻（当天该 HH:MM 对应的时间），加上账号固定偏移。
// 偏移可能为负（早于设定时刻）或为正（晚于设定时刻）。
func checkinSlotTime(base time.Time, accountID string) time.Time {
	return base.Add(checkinStaggerOffset(accountID))
}

// sleepCheckinGap 在账号之间等待强制间隔（可被 ctx 取消）。
//
// 返回 ctx.Err() 表示等待期间被取消——调用方应据此停止后续账号的签到，
// 而不是继续把剩下的账号打完（那等于取消后仍然在批量打）。
func sleepCheckinGap(ctx context.Context) error {
	d := checkinAccountGap()
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// randomUint64 返回一个真随机 uint64。
//
// 用 crypto/rand 而不是 math/rand：math/rand 的序列可被观测者重建——
// 用它做「抖动」等于没有抖动（完全等长的间隔正是最容易被聚类的特征）。
func randomUint64() uint64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 熵源不可用时退化为时间派生：宁可抖动质量略差，也不要没有抖动。
		return uint64(time.Now().UnixNano())
	}
	return binary.LittleEndian.Uint64(b[:])
}
