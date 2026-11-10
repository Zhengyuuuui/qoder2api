package bridge

import (
	"context"
	"sync"
	"testing"
	"time"

	"qoder2api/account"
)

// guardSettings 隔离 settings 目录并写入闸门配置（测试后恢复原 DataRoot，
// 避免污染同包其他测试的全局状态）。
func guardSettings(t *testing.T, st account.Settings) {
	t.Helper()
	orig := account.DataRoot()
	account.SetDataRoot(t.TempDir())
	t.Cleanup(func() { account.SetDataRoot(orig) })
	if err := account.SaveSettings(&st); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
}

// TestAcquireLeaseConcurrencyLimit 并发上限不变量：任意时刻活跃租约数
// 不超过配置上限，且全部释放后不残留。
func TestAcquireLeaseConcurrencyLimit(t *testing.T) {
	slot := testSlot(t, "guard-conc", "personal_standard")
	guardSettings(t, account.Settings{TrafficGuard: boolPtr(true), ConcurrencyPerAccount: 3, MinRequestGapMs: 1})

	var wg sync.WaitGroup
	var mu sync.Mutex
	active, maxActive := 0, 0
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := AcquireLeaseOrReject(context.Background(), slot)
			if err != nil || id == "" {
				return
			}
			mu.Lock()
			active++
			if active > maxActive {
				maxActive = active
			}
			mu.Unlock()
			time.Sleep(15 * time.Millisecond)
			mu.Lock()
			active--
			mu.Unlock()
			ReleaseLease(slot, id)
		}()
	}
	wg.Wait()

	if maxActive > 3 {
		t.Errorf("max concurrent leases = %d, want <= 3", maxActive)
	}
	if maxActive < 1 {
		t.Error("no lease was ever acquired")
	}
	slot.mu.Lock()
	n := len(slot.leases)
	slot.mu.Unlock()
	if n != 0 {
		t.Errorf("leases after release = %d, want 0", n)
	}
}

// TestAcquireLeaseReleaseThenReacquire 释放后可再次获取；释放幂等。
func TestAcquireLeaseReleaseThenReacquire(t *testing.T) {
	slot := testSlot(t, "guard-reacq", "personal_standard")
	guardSettings(t, account.Settings{TrafficGuard: boolPtr(true), ConcurrencyPerAccount: 1, MinRequestGapMs: 1})

	id1, ok := AcquireLease(context.Background(), slot)
	if !ok {
		t.Fatal("first acquire failed")
	}
	ReleaseLease(slot, id1)

	id2, ok := AcquireLease(context.Background(), slot)
	if !ok {
		t.Fatal("reacquire after release failed")
	}
	if id2 == id1 {
		t.Errorf("reacquired same lease id %s", id2)
	}
	// 幂等释放 + 空 ID no-op（fail-open 路径统一 defer 用）
	ReleaseLease(slot, id2)
	ReleaseLease(slot, id2)
	ReleaseLease(slot, "")
	slot.mu.Lock()
	n := len(slot.leases)
	slot.mu.Unlock()
	if n != 0 {
		t.Errorf("leases = %d, want 0", n)
	}
}

// TestAcquireLeaseEnforcesMinGap 最小启动间隔生效（含抖动只加不减）。
func TestAcquireLeaseEnforcesMinGap(t *testing.T) {
	// 节流器是进程级全局：前面的测试可能把它打到降档档位（间隔被自适应
	// 抬到 hold），本测试断言的是「用户配置驱动的间隔」，必须先重置。
	throttleReset(t)
	slot := testSlot(t, "guard-gap", "personal_standard")
	guardSettings(t, account.Settings{TrafficGuard: boolPtr(true), ConcurrencyPerAccount: 1, MinRequestGapMs: 300})

	id, ok := AcquireLease(context.Background(), slot)
	if !ok {
		t.Fatal("first acquire failed")
	}
	ReleaseLease(slot, id)

	begin := time.Now()
	id2, ok := AcquireLease(context.Background(), slot)
	if !ok {
		t.Fatal("second acquire failed")
	}
	elapsed := time.Since(begin)
	ReleaseLease(slot, id2)

	// 300ms 最小间隔 + 0~120ms 抖动；下界留 20ms 吸纳调度误差
	if elapsed < 280*time.Millisecond {
		t.Errorf("second acquire after %v, want >= 300ms (min gap)", elapsed)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("second acquire after %v, want <= 300ms + 120ms jitter + slack", elapsed)
	}
}

// TestLeaseHeartbeatKeepsLongStreamAlive 核心回归（审计 P1-4）：
// 启动已 11 分钟（远超旧 10 分钟 TTL）但只要心跳新鲜，租约**不得**
// 被回收——否则在飞长流的并发上限被击穿。
func TestLeaseHeartbeatKeepsLongStreamAlive(t *testing.T) {
	slot := testSlot(t, "guard-hb", "personal_standard")
	guardSettings(t, account.Settings{TrafficGuard: boolPtr(true), ConcurrencyPerAccount: 1, LeaseTTLSec: 7200})

	now := time.Now()
	slot.mu.Lock()
	slot.leases = map[string]leaseInfo{
		"long-stream": {started: now.Add(-11 * time.Minute), lastHeartbeat: now.Add(-30 * time.Second)},
	}
	slot.mu.Unlock()

	// 心跳续租（模拟流读取侧的定时 touch）
	TouchLease(slot, "long-stream")

	// 第二个请求必须拿不到租约（上限 1 且租约仍在）→ 等满超时走软放行，
	// 但绝不出现「租约被回收后名额复活」
	guardSettings(t, account.Settings{TrafficGuard: boolPtr(true), ConcurrencyPerAccount: 1, GuardWaitSec: 1, MinRequestGapMs: 1})
	id, err := AcquireLeaseOrReject(context.Background(), slot)
	if err == nil && id != "" {
		t.Fatal("lease of an in-flight long stream must not be reclaimed (heartbeat fresh)")
	}
	// 原租约仍在
	slot.mu.Lock()
	_, alive := slot.leases["long-stream"]
	slot.mu.Unlock()
	if !alive {
		t.Fatal("long-stream lease was reclaimed despite fresh heartbeat")
	}
}

// TestLeaseReclaimedWhenHeartbeatStale 心跳停止超过 TTL → 回收
// （原有泄漏回收能力不能丢）。
func TestLeaseReclaimedWhenHeartbeatStale(t *testing.T) {
	slot := testSlot(t, "guard-stale", "personal_standard")
	guardSettings(t, account.Settings{TrafficGuard: boolPtr(true), ConcurrencyPerAccount: 1, LeaseTTLSec: 60})

	now := time.Now()
	slot.mu.Lock()
	slot.leases = map[string]leaseInfo{
		"dead-lease": {started: now.Add(-2 * time.Hour), lastHeartbeat: now.Add(-2 * time.Minute)},
	}
	slot.mu.Unlock()

	id, err := AcquireLeaseOrReject(context.Background(), slot)
	if err != nil || id == "" {
		t.Fatalf("acquire after stale-lease reclaim failed: id=%q err=%v", id, err)
	}
	slot.mu.Lock()
	_, gone := slot.leases["dead-lease"]
	slot.mu.Unlock()
	if gone {
		t.Error("stale lease (heartbeat older than TTL) must be reclaimed")
	}
	ReleaseLease(slot, id)
}

// TestTouchLeaseUnknownIDIsSafe 对不存在 / 已回收的 leaseID 安全（幂等）。
func TestTouchLeaseUnknownIDIsSafe(t *testing.T) {
	slot := testSlot(t, "guard-touch", "personal_standard")
	TouchLease(slot, "no-such-lease") // 不得 panic
	TouchLease(nil, "x")              // 不得 panic
	ReleaseLease(slot, "")
}

// TestAcquireLeaseSoftCapThenReject 过载时软放行有上限（审计 P2-1）：
// 持续饱和下软放行次数不超过 limit×2，耗尽后拒流（ErrGuardSaturated）。
func TestAcquireLeaseSoftCapThenReject(t *testing.T) {
	slot := testSlot(t, "guard-soft", "personal_standard")
	guardSettings(t, account.Settings{
		TrafficGuard: boolPtr(true), ConcurrencyPerAccount: 1,
		GuardWaitSec: 1, MinRequestGapMs: 1,
	})
	// 重置软放行计数（进程级全局，前面测试可能已消耗）
	softAllow.mu.Lock()
	softAllow.count = 0
	softAllow.windowStart = time.Now()
	softAllow.cap = 0
	softAllow.mu.Unlock()

	held, err := AcquireLeaseOrReject(context.Background(), slot)
	if err != nil || held == "" {
		t.Fatalf("first acquire: id=%q err=%v", held, err)
	}
	defer ReleaseLease(slot, held)

	// 持续饱和：前 limit×2 次软放行（ok），超过即拒流
	rejected := false
	for i := 0; i < 10; i++ {
		_, err := AcquireLeaseOrReject(context.Background(), slot)
		if err == ErrGuardSaturated {
			rejected = true
			break
		}
	}
	if !rejected {
		t.Fatal("sustained saturation must eventually reject (soft fail-open cap)")
	}
	used, cap := SoftAllowSnapshot()
	if used > cap {
		t.Errorf("soft allow used=%d > cap=%d", used, cap)
	}
	if cap < 2 {
		t.Errorf("soft allow cap = %d, want >= 2", cap)
	}
}

// TestSoftAllowWindowDecays 软放行计数按窗口衰减：一次尖峰后能恢复放行
// （否则账号会被长时间拒流）。
func TestSoftAllowWindowDecays(t *testing.T) {
	old := softAllowWindow
	softAllowWindow = 60 * time.Millisecond
	defer func() { softAllowWindow = old }()

	slot := testSlot(t, "guard-decay", "personal_standard")
	guardSettings(t, account.Settings{
		TrafficGuard: boolPtr(true), ConcurrencyPerAccount: 1,
		GuardWaitSec: 1, MinRequestGapMs: 1,
	})
	softAllow.mu.Lock()
	softAllow.count = 0
	softAllow.windowStart = time.Now()
	softAllow.mu.Unlock()

	held, _ := AcquireLeaseOrReject(context.Background(), slot)
	defer ReleaseLease(slot, held)

	// 打满软放行配额直到拒流
	for i := 0; i < 10; i++ {
		if _, err := AcquireLeaseOrReject(context.Background(), slot); err == ErrGuardSaturated {
			break
		}
	}
	if used, _ := SoftAllowSnapshot(); used == 0 {
		t.Fatal("soft allow counter never incremented")
	}
	// 窗口过后计数衰减 → 重新可以软放行
	time.Sleep(120 * time.Millisecond)
	if _, err := AcquireLeaseOrReject(context.Background(), slot); err == ErrGuardSaturated {
		t.Error("soft allow counter must decay after the window")
	}
}

// TestAcquireLeaseNilSlot nil slot 安全（不 panic）：无池可守时 fail-open
// （返回空 ID + true，调用方统一 defer ReleaseLease 空 ID 也无副作用）。
func TestAcquireLeaseNilSlot(t *testing.T) {
	id, ok := AcquireLease(context.Background(), nil)
	if !ok || id != "" {
		t.Errorf("nil slot acquire = (%q, %v), want (\"\", true)", id, ok)
	}
	ReleaseLease(nil, "x") // 不得 panic
}

// TestSlotStatusIncludesLeases 状态可见性：active_leases 与 last_start。
func TestSlotStatusIncludesLeases(t *testing.T) {
	slot := testSlot(t, "guard-status", "personal_standard")
	guardSettings(t, account.Settings{TrafficGuard: boolPtr(true), ConcurrencyPerAccount: 2, MinRequestGapMs: 1})

	// 未启动：零租约、零启动时间
	st := slot.Status()
	if st.ActiveLeases != 0 || st.LastStart != "" {
		t.Errorf("initial status = %+v", st)
	}

	id, ok := AcquireLease(context.Background(), slot)
	if !ok {
		t.Fatal("acquire failed")
	}
	st = slot.Status()
	if st.ActiveLeases != 1 {
		t.Errorf("active_leases = %d, want 1", st.ActiveLeases)
	}
	if st.LastStart == "" {
		t.Error("last_start empty after acquire")
	}
	if _, err := time.Parse(time.RFC3339, st.LastStart); err != nil {
		t.Errorf("last_start not RFC3339: %v", err)
	}
	ReleaseLease(slot, id)
	if st = slot.Status(); st.ActiveLeases != 0 {
		t.Errorf("active_leases after release = %d, want 0", st.ActiveLeases)
	}
}
