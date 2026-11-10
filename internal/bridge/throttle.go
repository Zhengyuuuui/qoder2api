package bridge

import (
	"sync"
	"time"

	"qoder2api/logger"
)

// 自适应节流：把风控遥测信号（排队 / 频控）变成「自动收紧节奏」的动作。
//
// 为什么需要：排队率、频控率升高说明当前节奏已经踩到上游红线，此时正确的
// 反应是网关自己先收手（降并发、拉间隔），而不是等上游把号打挂再被动冷却
// ——被动冷却是账号已经受伤之后的补救，主动节流是避免受伤。
//
// 设计约束：
//   - 决策与展示分开：risk.go 的累计计数用于面板总量，本文件的滑动窗口
//     计数用于决策——累计值永不清零，直接拿来做率会越来越钝。
//   - 迟滞：降档阈值（5% / 2%）高于恢复阈值（1% / 0.5%），且在阈值附近
//     波动不得引起档位抖动；恢复还需连续 2 个干净窗口，不一步跳回。
//   - 只紧不松：自适应生效值只会比用户配置更严（并发取 min、间隔取 max），
//     永远不存在「自适应比用户配置更宽松」的状态。
//   - 全局生效：节流作用于整个网关（所有账号共享出口节奏），不是单账号
//     ——风控信号本身就是账号池级别的拥塞度量。

const (
	// DefaultThrottleWindowSec 默认统计窗口（秒）。
	DefaultThrottleWindowSec = 300
	// DefaultThrottleQueueRatePct 默认排队率降档阈值（百分比）。
	DefaultThrottleQueueRatePct = 5.0
	// DefaultThrottleRatePct 默认频控率降档阈值（百分比）。
	DefaultThrottleRatePct = 2.0
	// DefaultThrottleHoldMs 降档后的最小请求间隔（毫秒）。
	DefaultThrottleHoldMs = 1500

	// 恢复阈值：低于降档阈值才有迟滞。阈值之间的「缓冲带」就是不抖动的区间。
	throttleRecoverQueuePct = 1.0
	throttleRecoverRatePct  = 0.5
	// throttleConsecQueue 窗口内连续排队多少次直接降档（不等率攒够）。
	throttleConsecQueue = 3
	// throttleCleanWindows 恢复一级所需「持续干净的窗口数」——按距上次
	// 档位变化的 elapsed 判定（无状态）：既要持续够久，又要此刻窗口率
	// 确实低于恢复线，两个条件缺一不可（单条件都会被瞬时波动蒙混）。
	throttleCleanWindows = 2

	// 档位：0 正常 / 1 收紧 / 2 最严。
	ThrottleLevelNormal = 0
	ThrottleLevelTight  = 1
	ThrottleLevelStrict = 2

	// throttleBuckets 窗口分桶数（环形缓冲）。
	throttleBuckets = 60
	// throttleMinBucketMS 单桶最小时长（毫秒）。窗口配得再小，有效窗口
	// 也不低于 60 × 100ms = 6s——避免窗口过短导致统计噪声主导决策。
	throttleMinBucketMS = 100
)

// throttleEvalInterval 评估最小间隔：每次打点全量重算没有意义，限流到
// 一秒一次足够（变量而非常量：测试置零以同步评估）。
var throttleEvalInterval = time.Second

// ThrottleStats 是对外暴露的自适应节流状态（/health 与控制台用）。
type ThrottleStats struct {
	Level       int     `json:"level"`        // 0 正常 / 1 收紧 / 2 最严
	WindowSec   int     `json:"window_sec"`   // 配置的窗口秒数
	Requests    int64   `json:"requests"`     // 窗口内请求数
	Success     int64   `json:"success"`      // 窗口内成功数
	Queue       int64   `json:"queue"`        // 窗口内排队信号数
	Rate        int64   `json:"rate"`         // 窗口内频控信号数
	QueuePct    float64 `json:"queue_pct"`    // 排队率（百分比）
	RatePct     float64 `json:"rate_pct"`     // 频控率（百分比）
	ConsecQueue int     `json:"consec_queue"` // 当前连续排队次数
	Enabled     bool    `json:"enabled"`      // 自适应节流开关
}

type throttleBucket struct {
	epoch    int64 // 该桶对应的时间片（unixMilli / bucketMS）；0 = 空桶
	requests int64
	success  int64
	queue    int64
	rate     int64
}

type throttleMachine struct {
	mu       sync.Mutex
	buckets  [throttleBuckets]throttleBucket
	bucketMS int64 // 当前桶时长（随配置变化；变化即重置窗口）

	level      int
	lastChange time.Time

	consecQueue int

	lastEval time.Time
}

// throttle 进程级节流器（所有账号 / 模型共享）。
var throttle = &throttleMachine{lastChange: time.Now()}

// autoThrottleEnabled 自适应节流总开关。
// *bool 语义见 Settings.EffectiveAutoThrottle：nil（缺字段）= 默认开启，
// 显式 false 关闭。早期 bool 版本存量配置缺字段会读成 false（保护没开），
// 属审计 P1-3 修复的行为变更。
func autoThrottleEnabled() bool {
	if st := loadSettingsOrNil(); st != nil && !st.EffectiveAutoThrottle() {
		return false
	}
	return true
}

func throttleWindowSec() int {
	sec := DefaultThrottleWindowSec
	if st := loadSettingsOrNil(); st != nil && st.ThrottleWindowSec > 0 {
		sec = st.ThrottleWindowSec
	}
	return sec
}

func throttleQueueRatePct() float64 {
	if st := loadSettingsOrNil(); st != nil && st.ThrottleQueueRatePct > 0 {
		return st.ThrottleQueueRatePct
	}
	return DefaultThrottleQueueRatePct
}

func throttleRatePct() float64 {
	if st := loadSettingsOrNil(); st != nil && st.ThrottleRatePct > 0 {
		return st.ThrottleRatePct
	}
	return DefaultThrottleRatePct
}

// effectiveThrottleHold 降档后的最小间隔（level 2 加倍）。
func effectiveThrottleHold() time.Duration {
	ms := DefaultThrottleHoldMs
	if st := loadSettingsOrNil(); st != nil && st.ThrottleHoldMs > 0 {
		ms = st.ThrottleHoldMs
	}
	return time.Duration(ms) * time.Millisecond
}

// bucketMSLocked 由窗口配置推导桶时长；配置变化时返回的桶长变化，
// 调用方据此重置窗口（旧桶的时间片口径失效，混算会出错）。
func (m *throttleMachine) bucketMSLocked() int64 {
	windowMS := int64(throttleWindowSec()) * 1000
	b := windowMS / throttleBuckets
	if b < throttleMinBucketMS {
		b = throttleMinBucketMS
	}
	return b
}

// alignLocked 对齐桶口径：桶时长变化（配置被改）时清空全部桶。
func (m *throttleMachine) alignLocked() {
	b := m.bucketMSLocked()
	if b != m.bucketMS {
		m.bucketMS = b
		for i := range m.buckets {
			m.buckets[i] = throttleBucket{}
		}
	}
}

func (m *throttleMachine) indexLocked(now time.Time) int {
	epoch := now.UnixMilli() / m.bucketMS
	return int(epoch % throttleBuckets)
}

// ThrottleNoteRequest 记录一次请求进入（每个客户端请求调用一次）。
func ThrottleNoteRequest() {
	if !autoThrottleEnabled() {
		return
	}
	m := throttle
	m.mu.Lock()
	m.alignLocked()
	now := time.Now()
	idx := m.indexLocked(now)
	epoch := now.UnixMilli() / m.bucketMS
	if m.buckets[idx].epoch != epoch {
		m.buckets[idx] = throttleBucket{epoch: epoch}
	}
	m.buckets[idx].requests++
	m.mu.Unlock()
}

// ThrottleNoteSuccess 记录一次请求成功（成功会打断「连续排队」计数）。
func ThrottleNoteSuccess() {
	if !autoThrottleEnabled() {
		return
	}
	m := throttle
	m.mu.Lock()
	m.alignLocked()
	now := time.Now()
	idx := m.indexLocked(now)
	epoch := now.UnixMilli() / m.bucketMS
	if m.buckets[idx].epoch != epoch {
		m.buckets[idx] = throttleBucket{epoch: epoch}
	}
	m.buckets[idx].success++
	m.consecQueue = 0
	m.mu.Unlock()
	m.evaluate(now)
}

// ThrottleNoteSignal 记录一次风险信号（kind 取 risk.go 的 RiskQueue /
// RiskRate；其他值忽略）。每次观测都计数——排队会在同账号内重试若干次，
// 每次都是真实的上游拥塞信号（与 risk.go 的打点口径一致）。
func ThrottleNoteSignal(kind string) {
	if !autoThrottleEnabled() {
		return
	}
	m := throttle
	m.mu.Lock()
	m.alignLocked()
	now := time.Now()
	idx := m.indexLocked(now)
	epoch := now.UnixMilli() / m.bucketMS
	if m.buckets[idx].epoch != epoch {
		m.buckets[idx] = throttleBucket{epoch: epoch}
	}
	switch kind {
	case RiskQueue:
		m.buckets[idx].queue++
		m.consecQueue++
	case RiskRate:
		m.buckets[idx].rate++
	default:
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	m.evaluate(now)
}

// evaluate 降档 / 恢复判定。升级立即生效；升到更严档位需要「恶劣状态
// 持续满一个窗口」（排除偶发尖刺）；恢复需要连续 2 个干净窗口。
func (m *throttleMachine) evaluate(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if throttleEvalInterval > 0 && now.Sub(m.lastEval) < throttleEvalInterval {
		return
	}
	m.lastEval = now
	m.alignLocked()

	var req, success, q, r int64
	curEpoch := now.UnixMilli() / m.bucketMS
	for i := range m.buckets {
		b := &m.buckets[i]
		if b.epoch == 0 {
			continue
		}
		if curEpoch-b.epoch >= throttleBuckets {
			*b = throttleBucket{} // 滑出窗口，回收
			continue
		}
		req += b.requests
		success += b.success
		q += b.queue
		r += b.rate
	}
	if req == 0 {
		return
	}
	queuePct := float64(q) * 100 / float64(req)
	ratePct := float64(r) * 100 / float64(req)
	trigger := queuePct >= throttleQueueRatePct() ||
		ratePct >= throttleRatePct() ||
		m.consecQueue >= throttleConsecQueue
	window := time.Duration(m.bucketMS*throttleBuckets) * time.Millisecond

	switch {
	case trigger && m.level < ThrottleLevelStrict:
		// 0→1 立即（首次触线就要收手）；1→2 需恶劣状态持续满一个窗口
		// （排除偶发尖刺——尖刺不该把节奏打到最严）
		if m.level == ThrottleLevelNormal || now.Sub(m.lastChange) >= window {
			m.level++
			m.lastChange = now
			logger.Info("adaptive throttle: level -> %d (queue=%.1f%% rate=%.1f%% consecQueue=%d requests=%d)",
				m.level, queuePct, ratePct, m.consecQueue, req)
		}
	case !trigger && m.level > ThrottleLevelNormal:
		// 恢复需两个条件同时成立：
		//  1. 此刻窗口率低于恢复线（当前确实干净）；
		//  2. 距上次降档已持续 >= 2 个窗口（不是刚降就弹回去的震荡）。
		clean := queuePct < throttleRecoverQueuePct && ratePct < throttleRecoverRatePct
		if clean && now.Sub(m.lastChange) >= window*time.Duration(throttleCleanWindows) {
			m.level--
			m.lastChange = now
			logger.Info("adaptive throttle: level -> %d (recovered: queue=%.1f%% rate=%.1f%%, clean for %s)",
				m.level, queuePct, ratePct, now.Sub(m.lastChange).Round(time.Second))
		}
		// 处于缓冲带（低于降档线但未到恢复线）或持续时间不够：保持档位。
		// 这条 else 分支是迟滞的核心——阈值之间的区域不产生任何动作。
	}
}

// ThrottleLevel 当前档位（0/1/2）。
func ThrottleLevel() int {
	m := throttle
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.level
}

// ThrottleConcurrencyCap 自适应并发上限；0 表示不额外收紧（由用户配置决定）。
// 降档即压到 1：拥塞时任何「多请求并发打同一个号」的行为都在放大风险。
func ThrottleConcurrencyCap() int {
	m := throttle
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.level >= ThrottleLevelTight {
		return 1
	}
	return 0
}

// ThrottleMinGap 自适应最小间隔；0 表示不额外拉长。最严档位间隔加倍。
func ThrottleMinGap() time.Duration {
	m := throttle
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case m.level >= ThrottleLevelStrict:
		return 2 * effectiveThrottleHold()
	case m.level >= ThrottleLevelTight:
		return effectiveThrottleHold()
	}
	return 0
}

// ThrottleState 返回节流器完整状态（只读，供 /health 与诊断端点）。
func ThrottleState() ThrottleStats {
	m := throttle
	m.mu.Lock()
	defer m.mu.Unlock()
	m.alignLocked()
	now := time.Now()
	curEpoch := now.UnixMilli() / m.bucketMS
	var req, success, q, r int64
	for i := range m.buckets {
		b := &m.buckets[i]
		if b.epoch == 0 || curEpoch-b.epoch >= throttleBuckets {
			continue
		}
		req += b.requests
		success += b.success
		q += b.queue
		r += b.rate
	}
	st := ThrottleStats{
		Level:       m.level,
		WindowSec:   throttleWindowSec(),
		Requests:    req,
		Success:     success,
		Queue:       q,
		Rate:        r,
		ConsecQueue: m.consecQueue,
		Enabled:     autoThrottleEnabled(),
	}
	if req > 0 {
		st.QueuePct = float64(q) * 100 / float64(req)
		st.RatePct = float64(r) * 100 / float64(req)
	}
	return st
}
