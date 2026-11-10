package bridge

import (
	"context"
	"errors"
	mrand "math/rand"
	"strings"
	"sync"
	"time"

	"qoder2api/account"
	"qoder2api/internal/cosy"
	"qoder2api/logger"
)

// 单账号流量闸门 + 请求节奏整形。
//
// 背景：上游对「同一账号并发过高」「机器枪式固定节奏」敏感，会以 10605
// （模型级排队）/ 429（频控）回拒。网关若不做本地限流，客户端每发一个
// 请求都在叠加账号风险。闸门做两件事：
//  1. 并发上限：单账号同时只放行有限个在飞请求（租约制）；
//  2. 节奏整形：相邻请求启动间隔强制最小 gap + 真随机抖动——固定节奏
//     本身是最容易被聚类的自动化特征，抖动必须真随机，不能用时间戳
//     取模之类可推导的伪随机。
//
// 设计约束：
//   - 租约心跳续租：长流式请求（agent 任务可跑数小时）期间由流读取侧
//     定期 touch 租约；回收判定看「距上次心跳」而非「距启动」——只看
//     启动时间会把在飞长流误判为泄漏，并发上限被击穿。
//   - fail-open 但有软上限：等待超时后放行（不因闸门拒流），但持续过载
//     时软放行计数有上限（limit×2），超过即拒流——否则每个请求等满
//     超时都放行，并发上限名存实亡。计数按窗口衰减，尖峰后能恢复。
//   - 配置热更新：经 settings 缓存读取（见 account.LoadSettings），
//     改配置无需重启。

const (
	// DefaultConcurrencyPerAccount 单账号默认并发上限。1 = 完全串行：
	// 最保守；单账号池的并发请求会排队，多账号池天然分散。
	DefaultConcurrencyPerAccount = 1
	// DefaultMinRequestGapMS 默认最小请求启动间隔（毫秒）。
	DefaultMinRequestGapMS = 400
	// DefaultGuardWaitSec 默认闸门等待上限（秒）。
	DefaultGuardWaitSec = 30
	// DefaultLeaseTTLSec 默认租约心跳 TTL（秒）。显著长于最长合法流
	// 生命周期，是「心跳彻底失效」的兜底，不是常规回收线。
	DefaultLeaseTTLSec = 7200
	// GuardJitterMaxMS 启动间隔随机抖动上限（毫秒，含端点）。
	GuardJitterMaxMS = 120
	// leasePollInterval 并发已满时的锁外轮询间隔。
	leasePollInterval = 50 * time.Millisecond
	// leaseHeartbeatInterval 流式请求的租约心跳间隔。显著短于 TTL
	// （默认 2h）：既保证在飞长流永不误回收，又不至于把心跳做成噪声。
	leaseHeartbeatInterval = time.Minute
	// softAllowFactor 软放行上限 = 并发上限 × 该系数。
	softAllowFactor = 2
	// softAllowMinCap 软放行上限的最小值（并发上限为 1 时也允许 2 个）。
	softAllowMinCap = 2
)

// softAllowWindow 软放行计数的衰减窗口（变量而非常量：测试可缩短）。
var softAllowWindow = time.Minute

// ErrGuardSaturated 闸门饱和：软放行配额已耗尽，拒绝本次请求。
// 客户端可重试——这是有意的拒流（过载保护），不是故障。
var ErrGuardSaturated = errors.New("traffic guard: account saturated (too many concurrent requests), please retry later")

// leaseInfo 租约详情。lastHeartbeat 是回收判定的唯一依据。
type leaseInfo struct {
	started       time.Time
	lastHeartbeat time.Time
}

// loadSettingsOrNil 读取设置；读失败返回 nil（调用方按默认值处理）。
func loadSettingsOrNil() *account.Settings {
	st, err := account.LoadSettings()
	if err != nil {
		return nil
	}
	return st
}

// leaseTTL 租约心跳 TTL（≤0 用默认）。
func leaseTTL() time.Duration {
	sec := DefaultLeaseTTLSec
	if st := loadSettingsOrNil(); st != nil && st.LeaseTTLSec > 0 {
		sec = st.LeaseTTLSec
	}
	return time.Duration(sec) * time.Second
}

// guardConcurrencyLimit 生效的单账号并发上限。
// 与自适应节流协作：min(用户配置, 自适应上限)——拥塞时自适应压到 1，
// 但永远不会比用户配置更宽松（不存在「自适应放宽」的状态）。
func guardConcurrencyLimit() int {
	limit := DefaultConcurrencyPerAccount
	if st := loadSettingsOrNil(); st != nil && st.ConcurrencyPerAccount > 0 {
		limit = st.ConcurrencyPerAccount
	}
	if adaptive := ThrottleConcurrencyCap(); adaptive > 0 && adaptive < limit {
		logger.Debug("traffic guard: concurrency limited to %d by adaptive throttle (level=%d)",
			adaptive, ThrottleLevel())
		return adaptive
	}
	return limit
}

// guardMinGap 生效的最小请求启动间隔 = max(用户配置, 自适应间隔) + 抖动。
// 与 guardConcurrencyLimit 同一协作原则：自适应只紧不松。
func guardMinGap() time.Duration {
	gapMS := DefaultMinRequestGapMS
	if st := loadSettingsOrNil(); st != nil && st.MinRequestGapMs > 0 {
		gapMS = st.MinRequestGapMs
	}
	gap := time.Duration(gapMS) * time.Millisecond
	if adaptive := ThrottleMinGap(); adaptive > gap {
		gap = adaptive
	}
	jitterMS := 0
	if GuardJitterMaxMS > 0 {
		jitterMS = mrand.Intn(GuardJitterMaxMS + 1)
	}
	return gap + time.Duration(jitterMS)*time.Millisecond
}

// guardWait 闸门等待上限。
func guardWait() time.Duration {
	sec := DefaultGuardWaitSec
	if st := loadSettingsOrNil(); st != nil && st.GuardWaitSec > 0 {
		sec = st.GuardWaitSec
	}
	return time.Duration(sec) * time.Second
}

// guardApplies 判断闸门是否对该账号生效。
// TrafficGuard=nil（缺字段，默认）：所有账号；显式 false：仅团队/企业
// 账号（个人账号回到旧行为）。*bool 语义见 Settings.EffectiveTrafficGuard。
func guardApplies(slot *Slot) bool {
	st := loadSettingsOrNil()
	if st == nil || st.EffectiveTrafficGuard() {
		return true
	}
	return isTeamOrEnterprise(slot)
}

// isTeamOrEnterprise 按 user_type 识别团队/企业账号。
func isTeamOrEnterprise(slot *Slot) bool {
	if slot == nil || slot.sess == nil {
		return false
	}
	ut := strings.ToLower(slot.sess.Identity.UserType)
	return strings.Contains(ut, "team") ||
		strings.Contains(ut, "enterprise") ||
		strings.Contains(ut, "organization")
}

// softAllow 计数器（进程级）：记录窗口内 fail-open 放行的次数。
type softAllowCounter struct {
	mu          sync.Mutex
	count       int
	cap         int
	windowStart time.Time
}

var softAllow = &softAllowCounter{windowStart: time.Now()}

// trySoftAllow 申请一次软放行额度。返回 false 表示配额已耗尽（应拒流）。
// softAllowCapFor 由并发上限推导软放行上限（与 trySoftAllow 同一口径）。
// 诊断接口在没有任何软放行发生时也要能展示上限，故独立成函数。
func softAllowCapFor(limit int) int {
	c := limit * softAllowFactor
	if c < softAllowMinCap {
		c = softAllowMinCap
	}
	return c
}

func trySoftAllow(limit int) bool {
	cap := softAllowCapFor(limit)
	softAllow.mu.Lock()
	defer softAllow.mu.Unlock()
	now := time.Now()
	if now.Sub(softAllow.windowStart) >= softAllowWindow {
		// 窗口衰减：一次尖峰不应让账号长时间拒流
		softAllow.count = 0
		softAllow.windowStart = now
	}
	softAllow.cap = cap
	if softAllow.count >= cap {
		return false
	}
	softAllow.count++
	return true
}

// SoftAllowSnapshot 返回软放行计数与上限（诊断接口用）。
// 上限按当前并发配置实时推导：没有任何软放行发生时也要能展示。
func SoftAllowSnapshot() (used, cap int) {
	softAllow.mu.Lock()
	used = softAllow.count
	softAllow.mu.Unlock()
	return used, softAllowCapFor(guardConcurrencyLimit())
}

// AcquireLeaseOrReject 申请租约（带过载保护的完整语义）。
//
// 返回值语义：
//   - (leaseID, nil)：拿到租约；或账号不适用闸门 / slot 为空（不设防）
//   - ("", nil)：等待超时但软放行配额未耗尽——调用方「不带租约继续发」
//     （fail-open：闸门削峰不拒流），已记 WARN 与软放行计数
//   - ("", ErrGuardSaturated)：软放行配额也耗尽——拒流，客户端可重试
//   - ("", ctx.Err())：ctx 取消 / 超时
//
// 并发不变量：持 slot.mu 期间完成「回收过期租约 → 判定名额 → 登记租约」，
// 活跃租约数永远不会超过并发上限。
func AcquireLeaseOrReject(ctx context.Context, slot *Slot) (string, error) {
	if slot == nil {
		return "", nil
	}
	if !guardApplies(slot) {
		logger.Debug("traffic guard: not applied to account %s", slot.ID)
		return "", nil
	}
	limit := guardConcurrencyLimit()
	wait := guardWait()
	deadline := time.Now().Add(wait)
	gap := guardMinGap() // 每次调用重算抖动，避免固定间隔
	for {
		slot.mu.Lock()
		now := time.Now()
		// 惰性回收租约：判定依据是「距上次心跳」而非「距启动」——
		// 在飞长流有心跳续租，不会被误回收；真泄漏（panic / 忘释放）
		// 心跳停止后超过 TTL 才回收，避免账号被永久卡死。
		for id, l := range slot.leases {
			if now.Sub(l.lastHeartbeat) > leaseTTL() {
				delete(slot.leases, id)
				logger.Info("traffic guard: reclaimed stale lease on account %s (last heartbeat %s ago)",
					slot.ID, now.Sub(l.lastHeartbeat).Round(time.Second))
			}
		}
		if len(slot.leases) < limit {
			// 节奏整形：距上次启动不足最小间隔，先在锁外等待
			if delay := gapDelay(slot.lastStart, now, gap); delay > 0 {
				slot.mu.Unlock()
				if serr := sleepCtx(ctx, delay); serr != nil {
					return "", serr
				}
				continue
			}
			id := cosy.NewHexToken(16)
			if slot.leases == nil {
				slot.leases = map[string]leaseInfo{}
			}
			slot.leases[id] = leaseInfo{started: now, lastHeartbeat: now}
			slot.lastStart = now
			slot.mu.Unlock()
			return id, nil
		}
		slot.mu.Unlock()

		// 并发已满：锁外轮询等待，直到 deadline
		if !time.Now().Before(deadline) {
			if trySoftAllow(limit) {
				logger.Warn("traffic guard: lease wait timeout (%v) on account %s, soft fail-open (quota limited; sustained overload will be rejected)",
					wait, slot.ID)
				return "", nil
			}
			logger.Warn("traffic guard: account %s saturated: soft fail-open quota exhausted, rejecting request",
				slot.ID)
			return "", ErrGuardSaturated
		}
		if serr := sleepCtx(ctx, leasePollInterval); serr != nil {
			return "", serr
		}
	}
}

// AcquireLease 兼容旧签名的租约申请（无软上限语义）。
// 新代码请用 AcquireLeaseOrReject——那个才能在过载时拒流。
// ok=false 表示「没拿到租约」：调用方按 fail-open 继续发。
func AcquireLease(ctx context.Context, slot *Slot) (string, bool) {
	id, err := AcquireLeaseOrReject(ctx, slot)
	if err != nil {
		return "", false
	}
	return id, true
}

// TouchLease 续租（心跳）。幂等；对已回收 / 不存在的 leaseID 静默忽略。
//
// 由流式读取侧定期调用（见 streamWithSlot 的心跳 goroutine）：长流即使
// 长时间没有新数据（模型思考 / 工具执行中）也能保住租约。
func TouchLease(slot *Slot, leaseID string) {
	if slot == nil || leaseID == "" {
		return
	}
	now := time.Now()
	slot.mu.Lock()
	if l, ok := slot.leases[leaseID]; ok {
		l.lastHeartbeat = now
		slot.leases[leaseID] = l
	}
	slot.mu.Unlock()
}

// gapDelay 计算还需等待多久才能启动（距 lastStart 至少 gap）；
// lastStart 为零值（从未启动）时无需等待。
func gapDelay(lastStart, now time.Time, gap time.Duration) time.Duration {
	if lastStart.IsZero() {
		return 0
	}
	if d := lastStart.Add(gap).Sub(now); d > 0 {
		return d
	}
	return 0
}

// ReleaseLease 释放租约。幂等；空 ID 为 no-op（供 fail-open 路径统一
// defer ReleaseLease，无需判断是否真拿到租约）。
func ReleaseLease(slot *Slot, leaseID string) {
	if slot == nil || leaseID == "" {
		return
	}
	slot.mu.Lock()
	delete(slot.leases, leaseID)
	slot.mu.Unlock()
}
