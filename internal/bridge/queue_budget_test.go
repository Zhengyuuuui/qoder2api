package bridge

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"qoder2api/account"
)

// TestApplyQueueSettingsBudgetDefault 排队预算默认值：0 → 30 分钟
// （兜底值，不能是「无限」）。
func TestApplyQueueSettingsBudgetDefault(t *testing.T) {
	ApplyQueueSettings(0, 0, 0, true, 0)
	t.Cleanup(func() { ApplyQueueSettings(0, 0, 0, true, 0) })
	if QueueWaitBudget != 30*time.Minute {
		t.Errorf("default queue budget = %v, want 30m", QueueWaitBudget)
	}
}

// TestQueueWaitBudgetExhaustedGivesUp 审计 P2-4：排队等待必须有总预算
// 上限。「等到首 token」默认开，但无限挂起会同时占着客户端连接与账号
// 并发额度。预算耗尽 → 按拥塞上抛，不得继续等。
func TestQueueWaitBudgetExhaustedGivesUp(t *testing.T) {
	guardSettings(t, account.Settings{
		MinRequestGapMs:     1,
		QueueWaitFirstToken: boolPtr(true), // 默认开：只靠预算兜底
		QueueMaxRetries:     5,
		QueueWaitDefaultSec: 15, // 单次封顶 15s（远大于预算）
		QueueWaitBudgetSec:  1,  // 总预算 1s
	})
	hits := 0
	// 排队参数由控制台 settings 经 ApplyQueueSettings 下发到包级变量
	// （生产里在 main 启动时做；测试里显式调用）。
	ApplyQueueSettings(5, 15, 15, true, 1) // maxRetries=5 cap=15s default=15s firstToken=true budget=1s
	t.Cleanup(func() { ApplyQueueSettings(0, 0, 0, true, 0) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, queueEnvelopeFrame(60)) // 上游要求等 60s
	}))
	defer srv.Close()

	b := newAffinityBridge(t, srv.URL, "budget-only")
	msgs := []interface{}{map[string]interface{}{"role": "user", "content": "queued"}}
	start := time.Now()
	err := b.CallQoderWithOpts(context.Background(), "codex", msgs, "auto", nil, CallOpts{}, func(Delta) {})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("budget exhaustion must surface an error")
	}
	if elapsed > 5*time.Second {
		t.Errorf("call took %v; budget exhaustion must not wait for the upstream retryAfter", elapsed)
	}
	if hits != 1 {
		t.Errorf("upstream hits = %d, want 1 (give up before the first paid wait)", hits)
	}
}

// TestQueueWaitBudgetAccumulatesSmallWaits 预算按累计等待判定：
// 多次小等待叠加到超限也必须收手（不能只看单次）。
func TestQueueWaitBudgetAccumulatesSmallWaits(t *testing.T) {
	guardSettings(t, account.Settings{
		MinRequestGapMs:     1,
		QueueWaitFirstToken: boolPtr(true),
		QueueMaxRetries:     10,
		QueueWaitDefaultSec: 1,
		QueueWaitBudgetSec:  2, // 2s 预算：1s + 1s 后，第三次 1s 会超限
	})
	ApplyQueueSettings(10, 1, 1, true, 2)
	t.Cleanup(func() { ApplyQueueSettings(0, 0, 0, true, 0) })
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, queueEnvelopeFrame(1))
	}))
	defer srv.Close()

	b := newAffinityBridge(t, srv.URL, "budget-acc")
	msgs := []interface{}{map[string]interface{}{"role": "user", "content": "queued"}}
	start := time.Now()
	err := b.CallQoderWithOpts(context.Background(), "codex", msgs, "auto", nil, CallOpts{}, func(Delta) {})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("must give up once the accumulated wait exceeds the budget")
	}
	if elapsed > 4*time.Second {
		t.Errorf("elapsed = %v, want ~2s (budget) + slack", elapsed)
	}
	if hits > 4 {
		t.Errorf("upstream hits = %d, want <= 4 (budget must cap the retries)", hits)
	}
}

// TestQueueWaitCancelIsImmediate ctx 取消必须立即返回（排队等待不吞取消）。
func TestQueueWaitCancelIsImmediate(t *testing.T) {
	guardSettings(t, account.Settings{
		MinRequestGapMs:     1,
		QueueWaitFirstToken: boolPtr(true),
		QueueMaxRetries:     10,
		QueueWaitDefaultSec: 30,
		QueueWaitBudgetSec:  3600,
	})
	ApplyQueueSettings(10, 30, 30, true, 3600)
	t.Cleanup(func() { ApplyQueueSettings(0, 0, 0, true, 0) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, queueEnvelopeFrame(30))
	}))
	defer srv.Close()

	b := newAffinityBridge(t, srv.URL, "budget-cancel")
	msgs := []interface{}{map[string]interface{}{"role": "user", "content": "queued"}}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := b.CallQoderWithOpts(ctx, "codex", msgs, "auto", nil, CallOpts{}, func(Delta) {})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("cancelled call must return an error")
	}
	if elapsed > 3*time.Second {
		t.Errorf("ctx cancel not honored promptly: %v", elapsed)
	}
}
