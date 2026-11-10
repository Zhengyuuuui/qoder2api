package bridge

import (
	"testing"
	"time"

	"qoder2api/account"
)

// throttleReset 重置节流器状态并同步评估（测试隔离）。
func throttleReset(t *testing.T) {
	t.Helper()
	old := throttleEvalInterval
	throttleEvalInterval = 0
	t.Cleanup(func() { throttleEvalInterval = old })
	throttle.mu.Lock()
	throttle.level = 0
	throttle.consecQueue = 0
	throttle.lastChange = time.Now()
	throttle.lastEval = time.Time{}
	throttle.bucketMS = 0
	for i := range throttle.buckets {
		throttle.buckets[i] = throttleBucket{}
	}
	throttle.mu.Unlock()
}

func TestThrottleEscalatesOnQueueRate(t *testing.T) {
	throttleReset(t)
	// 用户配 5 并发 / 200ms 间隔；窗口 60s
	guardSettings(t, account.Settings{
		AutoThrottle: boolPtr(true), ThrottleWindowSec: 60,
		ConcurrencyPerAccount: 5, MinRequestGapMs: 200,
	})
	// 20 次请求中 2 次排队（10% ≥ 5% 阈值）→ 降档
	for i := 0; i < 18; i++ {
		ThrottleNoteRequest()
	}
	ThrottleNoteRequest()
	ThrottleNoteSignal(RiskQueue)
	ThrottleNoteRequest()
	ThrottleNoteSignal(RiskQueue)

	if lvl := ThrottleLevel(); lvl < ThrottleLevelTight {
		t.Fatalf("level = %d, want >= 1 after 10%% queue rate", lvl)
	}
	if cap := ThrottleConcurrencyCap(); cap != 1 {
		t.Errorf("concurrency cap = %d, want 1", cap)
	}
	// 生效并发 = min(用户 5, 自适应 1) = 1
	if l := guardConcurrencyLimit(); l != 1 {
		t.Errorf("guardConcurrencyLimit = %d, want min(5, adaptive 1)", l)
	}
	// 生效间隔 = max(用户 200ms, 自适应 1500ms) ≥ 1500ms
	if g := guardMinGap(); g < 1500*time.Millisecond {
		t.Errorf("guardMinGap = %v, want >= 1500ms (adaptive hold)", g)
	}
}

func TestThrottleHysteresisNoFlapping(t *testing.T) {
	throttleReset(t)
	guardSettings(t, account.Settings{AutoThrottle: boolPtr(true), ThrottleWindowSec: 60})

	// 先打过降档线：100 请求 + 6 排队 ≈ 5.7% → L1
	for i := 0; i < 100; i++ {
		ThrottleNoteRequest()
		if i < 6 {
			ThrottleNoteSignal(RiskQueue)
		}
	}
	if lvl := ThrottleLevel(); lvl != ThrottleLevelTight {
		t.Fatalf("level = %d, want 1 after 5.7%% queue rate", lvl)
	}

	// 然后在缓冲带（降档线 5% 以下、恢复线 1% 以上）持续波动：
	// 累计排队率被压在 ~4.4%，既不该恢复也不该升级——档位必须稳定。
	for round := 0; round < 6; round++ {
		for i := 0; i < 50; i++ {
			ThrottleNoteRequest()
			if i < 1 {
				ThrottleNoteSignal(RiskQueue)
			}
		}
		if lvl := ThrottleLevel(); lvl != ThrottleLevelTight {
			t.Fatalf("level = %d at round %d, want stable 1 (hysteresis broken)", lvl, round)
		}
	}
}

func TestThrottleRecoversGradually(t *testing.T) {
	throttleReset(t)
	// 短窗口加速：配置 2s → 有效窗口 6s（60 桶 × 100ms 下限）
	guardSettings(t, account.Settings{
		AutoThrottle: boolPtr(true), ThrottleWindowSec: 2,
		ConcurrencyPerAccount: 4, MinRequestGapMs: 100,
	})

	// 持续拥塞直到最严档位（50% 排队率维持超过一个窗口）
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		ThrottleNoteRequest()
		ThrottleNoteSignal(RiskQueue)
		if ThrottleLevel() >= ThrottleLevelStrict {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if lvl := ThrottleLevel(); lvl < ThrottleLevelStrict {
		t.Fatalf("level = %d, want 2 after sustained congestion", lvl)
	}
	if g := ThrottleMinGap(); g != 2*effectiveThrottleHold() {
		t.Errorf("strict gap = %v, want %v", g, 2*effectiveThrottleHold())
	}

	// 恢复：只喂成功。断言逐级恢复——先看到 L1（间隔减半、并发仍压 1），
	// 再看到 L0（并发恢复用户配置 4）。不允许 L2 一步跳回 L0。
	sawIntermediate := false
	deadline = time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		ThrottleNoteRequest()
		ThrottleNoteSuccess()
		time.Sleep(100 * time.Millisecond)

		switch ThrottleLevel() {
		case ThrottleLevelTight:
			sawIntermediate = true
			if g := ThrottleMinGap(); g != effectiveThrottleHold() {
				t.Errorf("L1 gap = %v, want %v (interval must recover first)", g, effectiveThrottleHold())
			}
			if c := ThrottleConcurrencyCap(); c != 1 {
				t.Errorf("L1 concurrency cap = %d, want still 1 (concurrency recovers last)", c)
			}
		case ThrottleLevelNormal:
			if !sawIntermediate {
				t.Fatal("recovered L2 -> L0 in one step; want gradual (interval first, then concurrency)")
			}
			if c := ThrottleConcurrencyCap(); c != 0 {
				t.Errorf("L0 concurrency cap = %d, want 0", c)
			}
			if l := guardConcurrencyLimit(); l != 4 {
				t.Errorf("guardConcurrencyLimit = %d, want user config 4", l)
			}
			return
		}
	}
	t.Fatal("did not recover to normal level within timeout")
}

func TestThrottleEffectiveLimitsCombineWithUserConfig(t *testing.T) {
	throttleReset(t)
	guardSettings(t, account.Settings{
		AutoThrottle: boolPtr(true), ThrottleWindowSec: 60,
		ConcurrencyPerAccount: 5, MinRequestGapMs: 200,
	})
	// 正常档：完全按用户配置
	if l := guardConcurrencyLimit(); l != 5 {
		t.Errorf("limit = %d, want user config 5", l)
	}
	if g := guardMinGap(); g < 200*time.Millisecond || g > 320*time.Millisecond {
		t.Errorf("gap = %v, want 200ms + jitter(<=120ms)", g)
	}
	// 降档后：min/max 组合正确
	for i := 0; i < 20; i++ {
		ThrottleNoteRequest()
		if i < 2 {
			ThrottleNoteSignal(RiskQueue)
		}
	}
	if l := guardConcurrencyLimit(); l != 1 {
		t.Errorf("limit under throttle = %d, want min(5,1)", l)
	}
	if g := guardMinGap(); g < 1500*time.Millisecond {
		t.Errorf("gap under throttle = %v, want max(200ms,1500ms)", g)
	}
}

func TestThrottleDisabledNeverEscalates(t *testing.T) {
	throttleReset(t)
	guardSettings(t, account.Settings{
		AutoThrottle: boolPtr(false), ThrottleWindowSec: 2, ConcurrencyPerAccount: 3,
	})
	for i := 0; i < 50; i++ {
		ThrottleNoteRequest()
		ThrottleNoteSignal(RiskQueue)
		ThrottleNoteSignal(RiskRate)
	}
	if lvl := ThrottleLevel(); lvl != ThrottleLevelNormal {
		t.Errorf("level = %d, want 0 when auto_throttle=false", lvl)
	}
	if l := guardConcurrencyLimit(); l != 3 {
		t.Errorf("limit = %d, want user config 3 (adaptive disabled)", l)
	}
}
