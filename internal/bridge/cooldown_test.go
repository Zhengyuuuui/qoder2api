package bridge

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// --- C1. 模型级冷却 ---------------------------------------------------------

// 429（频控）是模型级的：只冷却该模型，同账号的其他模型仍应可选。
func TestModelCooldownOnlyAffectsThatModel(t *testing.T) {
	p := NewPool(testSlot(t, "a", "t"))
	p.CoolModel("a", "smodel", time.Minute, "429")

	// 该模型不可选
	if _, ok := p.PickForModel(nil, "smodel"); ok {
		t.Fatal("model in cooldown should not be selectable")
	}
	// 同账号其他模型仍可选
	s, ok := p.PickForModel(nil, "qfmodel")
	if !ok || s.ID != "a" {
		t.Fatalf("other model should still be selectable, got %v", s)
	}
	// 账号级判定不受模型级冷却影响（无模型维度的请求仍可用该账号）
	if _, ok := p.Pick(nil); !ok {
		t.Fatal("account-level pick should still work (model cooldown is model-scoped)")
	}
}

// 模型级冷却到期后自动恢复，且过期条目会被清理（避免 map 无限增长）。
func TestModelCooldownExpiresAndCleansUp(t *testing.T) {
	s := testSlot(t, "a", "t")
	p := NewPool(s)
	p.CoolModel("a", "smodel", 20*time.Millisecond, "429")

	if _, ok := p.PickForModel(nil, "smodel"); ok {
		t.Fatal("should be cooling")
	}
	time.Sleep(30 * time.Millisecond)
	if _, ok := p.PickForModel(nil, "smodel"); !ok {
		t.Fatal("model cooldown should have expired")
	}
	s.mu.Lock()
	n := len(s.modelCooldowns)
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("expired model cooldown should be cleaned up, got %d entries", n)
	}
}

// 401/403（额度 / 凭证）是账号级问题：该账号对所有模型都不可选。
func TestAccountCooldownBlocksAllModels(t *testing.T) {
	p := NewPool(testSlot(t, "a", "t"))
	p.Cool("a", time.Minute, "403")

	for _, m := range []string{"smodel", "qfmodel", "gmodel"} {
		if _, ok := p.PickForModel(nil, m); ok {
			t.Fatalf("account cooling must block model %s too", m)
		}
	}
	if _, ok := p.Pick(nil); ok {
		t.Fatal("account cooling must block model-less pick too")
	}
}

// 分类：429 / 排队 → 模型级；401/403 → 账号级；内容审核 → 都不动。
func TestClassifyFailRouting(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want FailClass
	}{
		{"429 rate limit", NewUpstreamError(429, "usage exceeds frequency limit"), FailClassModel},
		{"401 unauthorized", NewUpstreamError(401, "authentication_error"), FailClassHard},
		{"403 quota", NewUpstreamError(403, `{"code":"112"}`), FailClassHard},
		{"502 transient", NewUpstreamError(502, "provider_error"), FailClassHard},
		{"418 provider", NewUpstreamError(418, "provider_error"), FailClassHard},
		{"content policy", NewUpstreamError(500, "InternalError.Algo.DataInspectionFailed"), FailClassNone},
		{"invalid param", NewUpstreamError(400, "invalid_parameter_error: Range of x"), FailClassNone},
		{"stream business", NewStreamBusinessError("upstream error code=115: boom"), FailClassUnknown},
	}
	for _, c := range cases {
		if got := ClassifyFail(c.err); got != c.want {
			t.Errorf("%s: ClassifyFail = %v, want %v", c.name, got, c.want)
		}
	}
}

// 排队（10605 / RESOURCE_LIMIT 排队语义）判为模型级：模型容量问题，与账号无关。
func TestClassifyFailQueueIsModelScoped(t *testing.T) {
	detail := nestedResourceLimitDetail(true, true, 3)
	if got := ClassifyFail(NewUpstreamError(403, detail)); got != FailClassModel {
		t.Errorf("queued error should be model-scoped, got %v", got)
	}
}

// failSlot 按分类把失败写到正确的层级。
func TestFailSlotRoutesCooldownByClass(t *testing.T) {
	s := testSlot(t, "a", "t")
	p := NewPool(s)
	b := NewPoolBridge(p, nil)

	// 429 → 只写模型级
	b.failSlot("a", "smodel", NewUpstreamError(429, "rate limit"))
	if !s.cooldownUntil.IsZero() {
		t.Error("429 must not cool the whole account")
	}
	s.mu.Lock()
	_, hasModel := s.modelCooldowns["smodel"]
	s.mu.Unlock()
	if !hasModel {
		t.Error("429 should write model-level cooldown")
	}

	// 401 → 写账号级
	b.failSlot("a", "qfmodel", NewUpstreamError(401, "authentication_error"))
	if s.cooldownUntil.IsZero() {
		t.Error("401 should cool the whole account")
	}
}

// --- C2. 熔断与降权 ---------------------------------------------------------

// 连续 3 次硬失败 → 进入熔断窗口；熔断期间该账号不参与选号。
func TestBreakerOpensAfterConsecutiveHardFails(t *testing.T) {
	s := testSlot(t, "a", "t")
	p := NewPool(s)

	for i := 0; i < BreakerThreshold-1; i++ {
		s.noteFailure(FailClassHard)
	}
	if !s.availableFor(time.Now(), "") {
		t.Fatal("should not be breaker-opened before threshold")
	}

	// 第 3 次：进入熔断
	s.noteFailure(FailClassHard)
	if s.availableFor(time.Now(), "") {
		t.Fatal("should be breaker-opened after 3 consecutive hard fails")
	}
	if _, ok := p.Pick(nil); ok {
		t.Fatal("breaker-opened account must not be picked")
	}
}

// 成功一次 → 连续失败计数清零。
func TestSuccessClearsFailureCounters(t *testing.T) {
	s := testSlot(t, "a", "t")
	s.noteFailure(FailClassHard)
	s.noteFailure(FailClassHard)
	if s.hardFails != 2 {
		t.Fatalf("hardFails = %d, want 2", s.hardFails)
	}
	s.noteSuccess()
	if s.hardFails != 0 || s.unknownFails != 0 {
		t.Fatalf("success must clear counters, got hard=%d unknown=%d", s.hardFails, s.unknownFails)
	}
}

// 熔断退避序列：30min, 60min, 120min... 封顶 6h。
func TestBreakerBackoffSequence(t *testing.T) {
	want := []time.Duration{
		30 * time.Minute, // n = 3 (threshold)
		60 * time.Minute, // n = 4
		120 * time.Minute,
		240 * time.Minute,
		6 * time.Hour, // 封顶
		6 * time.Hour,
	}
	for i, w := range want {
		n := BreakerThreshold + i
		if got := breakerBackoff(n); got != w {
			t.Errorf("breakerBackoff(%d) = %v, want %v", n, got, w)
		}
	}
	// 未达阈值 → 0
	if got := breakerBackoff(BreakerThreshold - 1); got != 0 {
		t.Errorf("breakerBackoff below threshold = %v, want 0", got)
	}
}

// 降权退避序列：10min, 20min... 封顶 2h。
func TestDegradeBackoffSequence(t *testing.T) {
	want := []time.Duration{
		10 * time.Minute, // n = 5 (threshold)
		20 * time.Minute,
		40 * time.Minute,
		80 * time.Minute,
		2 * time.Hour, // 封顶
		2 * time.Hour,
	}
	for i, w := range want {
		n := DegradeThreshold + i
		if got := degradeBackoff(n); got != w {
			t.Errorf("degradeBackoff(%d) = %v, want %v", n, got, w)
		}
	}
	if got := degradeBackoff(DegradeThreshold - 1); got != 0 {
		t.Errorf("degradeBackoff below threshold = %v, want 0", got)
	}
}

// 熔断窗口时长按退避序列实际写入。
func TestBreakerWindowUsesBackoff(t *testing.T) {
	s := testSlot(t, "a", "t")
	for i := 0; i < BreakerThreshold; i++ {
		s.noteFailure(FailClassHard)
	}
	s.mu.Lock()
	rem := time.Until(s.breakerUntil)
	s.mu.Unlock()
	// 首次熔断 30min，允许少量执行误差
	if rem < 29*time.Minute || rem > 31*time.Minute {
		t.Errorf("first breaker window = %v, want ~30m", rem)
	}
}

// 连续硬失败会拉长熔断窗口（第 4 次失败 → 60min）。
func TestBreakerWindowGrowsWithFailures(t *testing.T) {
	s := testSlot(t, "a", "t")
	for i := 0; i < BreakerThreshold+1; i++ {
		s.noteFailure(FailClassHard)
	}
	s.mu.Lock()
	rem := time.Until(s.breakerUntil)
	s.mu.Unlock()
	if rem < 59*time.Minute || rem > 61*time.Minute {
		t.Errorf("second breaker window = %v, want ~60m", rem)
	}
}

// 降权窗口内账号仍可被选中，但排在候选列表末尾。
func TestDegradedAccountIsLastResort(t *testing.T) {
	healthy := testSlot(t, "healthy", "t")
	degraded := testSlot(t, "degraded", "t")
	p := NewPool(degraded, healthy)

	// 连续 5 次未知失败 → 降权
	for i := 0; i < DegradeThreshold; i++ {
		degraded.noteFailure(FailClassUnknown)
	}
	if !degraded.degraded(time.Now()) {
		t.Fatal("account should be degraded")
	}
	// 降权账号仍可选（不直接排除），但健康账号优先
	s, ok := p.Pick(nil)
	if !ok {
		t.Fatal("degraded account should still be selectable as fallback")
	}
	if s.ID != "healthy" {
		t.Fatalf("healthy account should be preferred, got %s", s.ID)
	}
	// 健康账号被排除后，降权账号兜底
	s, ok = p.Pick(map[string]struct{}{"healthy": {}})
	if !ok || s.ID != "degraded" {
		t.Fatalf("degraded account should be the fallback, got %v", s)
	}
}

// 内容审核错误不增加任何失败计数。
func TestContentPolicyDoesNotCountAsFailure(t *testing.T) {
	s := testSlot(t, "a", "t")
	for i := 0; i < 5; i++ {
		s.noteFailure(ClassifyFail(NewUpstreamError(500, "InternalError.Algo.DataInspectionFailed")))
	}
	s.mu.Lock()
	hard, unknown := s.hardFails, s.unknownFails
	breaker, degrade := s.breakerUntil, s.degradeUntil
	s.mu.Unlock()
	if hard != 0 || unknown != 0 {
		t.Errorf("content policy must not count as failure: hard=%d unknown=%d", hard, unknown)
	}
	if !breaker.IsZero() || !degrade.IsZero() {
		t.Error("content policy must not open breaker or degrade window")
	}
	if !s.availableFor(time.Now(), "") {
		t.Error("content policy must not make account unavailable")
	}
}

// 参数错同样不计数（换号无意义）。
func TestClientParamErrorDoesNotCountAsFailure(t *testing.T) {
	s := testSlot(t, "a", "t")
	for i := 0; i < 5; i++ {
		s.noteFailure(ClassifyFail(NewUpstreamError(400, "invalid_parameter_error: Range of max_tokens")))
	}
	if s.hardFails != 0 || s.unknownFails != 0 {
		t.Errorf("client param error must not count: hard=%d unknown=%d", s.hardFails, s.unknownFails)
	}
}

// 未知失败连续 5 次 → 降权窗口。
func TestDegradeOpensAfterConsecutiveUnknownFails(t *testing.T) {
	s := testSlot(t, "a", "t")
	for i := 0; i < DegradeThreshold-1; i++ {
		s.noteFailure(FailClassUnknown)
	}
	if s.degraded(time.Now()) {
		t.Fatal("should not be degraded before threshold")
	}
	s.noteFailure(FailClassUnknown)
	if !s.degraded(time.Now()) {
		t.Fatal("should be degraded after threshold")
	}
	// 降权不排除账号
	if !s.availableFor(time.Now(), "") {
		t.Fatal("degraded account must still be available (only reordered)")
	}
}

// --- C3. 状态可见性 ---------------------------------------------------------

// SlotStatus 暴露模型级冷却、熔断 / 降权剩余时间与失败计数。
func TestSlotStatusExposesCooldownAndBreakerState(t *testing.T) {
	s := testSlot(t, "a", "t")
	p := NewPool(s)
	p.CoolModel("a", "smodel", time.Minute, "429")
	for i := 0; i < BreakerThreshold; i++ {
		s.noteFailure(FailClassHard)
	}
	for i := 0; i < DegradeThreshold; i++ {
		s.noteFailure(FailClassUnknown)
	}

	st := s.Status()
	if st.ModelCooldowns == nil {
		t.Fatal("model_cooldowns should be present")
	}
	ms, ok := st.ModelCooldowns["smodel"]
	if !ok {
		t.Fatal("model_cooldowns should contain smodel")
	}
	if ms <= 0 || ms > 60_000 {
		t.Errorf("model cooldown remaining = %d ms, want (0, 60000]", ms)
	}
	if st.BreakerFor <= 0 {
		t.Errorf("breaker_for = %d, want > 0", st.BreakerFor)
	}
	if st.DegradeFor <= 0 {
		t.Errorf("degrade_for = %d, want > 0", st.DegradeFor)
	}
	if st.HardFails != BreakerThreshold {
		t.Errorf("hard_fails = %d, want %d", st.HardFails, BreakerThreshold)
	}
	if st.UnknownFails != DegradeThreshold {
		t.Errorf("unknown_fails = %d, want %d", st.UnknownFails, DegradeThreshold)
	}
}

// 池汇总统计熔断 / 冷却账号数。
func TestPoolSummaryCounts(t *testing.T) {
	a := testSlot(t, "a", "t")
	b := testSlot(t, "b", "t")
	p := NewPool(a, b)

	p.Cool("a", time.Minute, "403")
	for i := 0; i < BreakerThreshold; i++ {
		b.noteFailure(FailClassHard)
	}

	sum := p.Summary()
	if sum.Total != 2 {
		t.Errorf("total = %d, want 2", sum.Total)
	}
	if sum.Cooling != 1 {
		t.Errorf("cooling = %d, want 1", sum.Cooling)
	}
	if sum.Breaker != 1 {
		t.Errorf("breaker = %d, want 1", sum.Breaker)
	}
}

// 端到端：429 频控后，该模型在该账号上不可选，但其他模型仍可选。
func TestEndToEndRateLimitCoolsOnlyThatModel(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseEnvelope(429, "usage exceeds frequency limit"))
	}))
	defer srv.Close()

	b, pool := poolBridge(t, srv.URL, "acct-1")
	for _, sl := range pool.snapshot() {
		sl.catalog = map[string]bool{"qfmodel": true, "smodel": true}
		sl.catalogAt = time.Now()
	}

	err := b.CallQoderWithOpts(context.Background(), "codex", testMessages(), "qfmodel", nil, CallOpts{}, func(Delta) {})
	if err == nil {
		t.Fatal("expected rate limit error")
	}
	if hits == 0 {
		t.Fatal("upstream should have been called")
	}

	// 该模型已被模型级冷却 → 选号时跳过
	if _, ok := pool.PickForModel(nil, "qfmodel"); ok {
		t.Error("rate-limited model should not be selectable")
	}
	// 同账号其他模型仍可用
	if _, ok := pool.PickForModel(nil, "smodel"); !ok {
		t.Error("other models on the same account should stay selectable")
	}
	// 账号级未被冷却（不应整号拖死）
	st := pool.Status()[0]
	if st.CoolingRemainingMS > 0 {
		t.Errorf("account should not be cooled for a model-level failure, got %d ms", st.CoolingRemainingMS)
	}
	if len(st.ModelCooldowns) == 0 {
		t.Error("model-level cooldown should be recorded")
	}
}

// --- 开关（向后兼容回退） ---------------------------------------------------

// 关闭模型级冷却开关 → 回退到旧的整号冷却行为。
func TestModelCooldownSwitchFallsBackToAccountCooling(t *testing.T) {
	ApplyHealthSettings(false, true)
	t.Cleanup(func() { ApplyHealthSettings(true, true) })

	s := testSlot(t, "a", "t")
	p := NewPool(s)
	b := NewPoolBridge(p, nil)

	b.failSlot("a", "smodel", NewUpstreamError(429, "rate limit"))

	s.mu.Lock()
	cooled := !s.cooldownUntil.IsZero()
	_, hasModel := s.modelCooldowns["smodel"]
	s.mu.Unlock()
	if !cooled {
		t.Error("switch off: 429 should cool the whole account (legacy behavior)")
	}
	if hasModel {
		t.Error("switch off: should not write model-level cooldown")
	}
}

// 关闭熔断降权开关 → 不累计计数，不进熔断 / 降权窗口。
func TestBreakerDegradeSwitchDisablesCounting(t *testing.T) {
	ApplyHealthSettings(true, false)
	t.Cleanup(func() { ApplyHealthSettings(true, true) })

	s := testSlot(t, "a", "t")
	p := NewPool(s)
	b := NewPoolBridge(p, nil)

	for i := 0; i < 5; i++ {
		b.failSlot("a", "smodel", NewUpstreamError(403, "denied"))
	}
	if s.hardFails != 0 {
		t.Errorf("switch off: hardFails = %d, want 0", s.hardFails)
	}
	if !s.breakerUntil.IsZero() {
		t.Error("switch off: breaker must stay closed")
	}
}
