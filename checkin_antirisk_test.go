package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"qoder2api/account"
)

// --- L1. 账号间隔 -----------------------------------------------------------

// 两个账号签到 → 断言间隔 ≥ 最小间隔（含抖动下界）。
func TestCheckinAccountGapAtLeastMinimum(t *testing.T) {
	oldGap, oldJitter := CheckinAccountGapMS, CheckinJitterMS
	CheckinAccountGapMS, CheckinJitterMS = 50, 20
	t.Cleanup(func() { CheckinAccountGapMS, CheckinJitterMS = oldGap, oldJitter })

	for i := 0; i < 20; i++ {
		got := checkinAccountGap()
		min := time.Duration(CheckinAccountGapMS) * time.Millisecond
		if got < min {
			t.Fatalf("gap %v must be >= minimum %v", got, min)
		}
		max := min + time.Duration(CheckinJitterMS)*time.Millisecond
		if got > max {
			t.Fatalf("gap %v must be <= %v", got, max)
		}
	}
}

// 间隔等待可被 ctx 取消。
func TestSleepCheckinGapCancellable(t *testing.T) {
	oldGap := CheckinAccountGapMS
	CheckinAccountGapMS = 5000
	t.Cleanup(func() { CheckinAccountGapMS = oldGap })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := sleepCheckinGap(ctx)
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if time.Since(start) > time.Second {
		t.Error("cancelled gap wait should return immediately")
	}
}

// --- L2. 时间点错峰 ---------------------------------------------------------

// 同一账号连续两天调度 → 偏移量稳定（相同账号ID → 相同偏移）。
func TestCheckinStaggerOffsetStableForSameAccount(t *testing.T) {
	oldSec, oldEnabled := CheckinStaggerSec, CheckinStaggerEnabled
	CheckinStaggerSec, CheckinStaggerEnabled = 60, true
	t.Cleanup(func() { CheckinStaggerSec, CheckinStaggerEnabled = oldSec, oldEnabled })

	day1 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)

	off1 := checkinSlotTime(day1, "acct-stable-001").Sub(day1)
	off2 := checkinSlotTime(day2, "acct-stable-001").Sub(day2)
	if off1 != off2 {
		t.Errorf("offset must be stable across days: %v vs %v", off1, off2)
	}
	// 多次调用也必须一致（不能每次随机）
	for i := 0; i < 5; i++ {
		if got := checkinSlotTime(day1, "acct-stable-001").Sub(day1); got != off1 {
			t.Fatalf("offset changed on call %d: %v vs %v", i, got, off1)
		}
	}
}

// 不同账号 → 偏移量不同。
func TestCheckinStaggerOffsetDiffersPerAccount(t *testing.T) {
	oldSec, oldEnabled := CheckinStaggerSec, CheckinStaggerEnabled
	CheckinStaggerSec, CheckinStaggerEnabled = 60, true
	t.Cleanup(func() { CheckinStaggerSec, CheckinStaggerEnabled = oldSec, oldEnabled })

	offsets := map[time.Duration]bool{}
	ids := []string{
		"acct-0000001", "acct-0000002", "acct-0000003", "acct-0000004",
		"acct-0000005", "acct-0000006", "acct-0000007", "acct-0000008",
	}
	for _, id := range ids {
		off := checkinStaggerOffset(id)
		if off > time.Duration(CheckinStaggerSec)*time.Second ||
			off < -time.Duration(CheckinStaggerSec)*time.Second {
			t.Errorf("offset %v out of window for %s", off, id)
		}
		offsets[off] = true
	}
	if len(offsets) < 2 {
		t.Errorf("different accounts should get different offsets, got %d distinct", len(offsets))
	}
}

// 关闭错峰开关 → 偏移为 0（回到旧行为）。
func TestCheckinStaggerDisabledMeansZeroOffset(t *testing.T) {
	oldSec, oldEnabled := CheckinStaggerSec, CheckinStaggerEnabled
	CheckinStaggerSec, CheckinStaggerEnabled = 60, false
	t.Cleanup(func() { CheckinStaggerSec, CheckinStaggerEnabled = oldSec, oldEnabled })

	for _, id := range []string{"acct-a", "acct-b", "acct-c"} {
		if off := checkinStaggerOffset(id); off != 0 {
			t.Errorf("stagger disabled must give zero offset, got %v for %s", off, id)
		}
	}
}

// 错峰窗口的时长可配置。
func TestApplyCheckinSettings(t *testing.T) {
	oldGap, oldJitter, oldSec, oldEnabled := CheckinAccountGapMS, CheckinJitterMS, CheckinStaggerSec, CheckinStaggerEnabled
	t.Cleanup(func() {
		CheckinAccountGapMS, CheckinJitterMS = oldGap, oldJitter
		CheckinStaggerSec, CheckinStaggerEnabled = oldSec, oldEnabled
	})

	// 全部 0 → 各自默认
	ApplyCheckinSettings(0, 0, 0, true)
	if CheckinAccountGapMS != DefaultCheckinAccountGapMS {
		t.Errorf("gap default = %d, want %d", CheckinAccountGapMS, DefaultCheckinAccountGapMS)
	}
	if CheckinJitterMS != DefaultCheckinJitterMS {
		t.Errorf("jitter default = %d, want %d", CheckinJitterMS, DefaultCheckinJitterMS)
	}
	if CheckinStaggerSec != DefaultCheckinStaggerSec {
		t.Errorf("stagger default = %d, want %d", CheckinStaggerSec, DefaultCheckinStaggerSec)
	}
	if !CheckinStaggerEnabled {
		t.Error("stagger should be enabled")
	}

	// 正常值透传
	ApplyCheckinSettings(2000, 300, 120, false)
	if CheckinAccountGapMS != 2000 || CheckinJitterMS != 300 || CheckinStaggerSec != 120 {
		t.Errorf("custom not applied: %d %d %d", CheckinAccountGapMS, CheckinJitterMS, CheckinStaggerSec)
	}
	if CheckinStaggerEnabled {
		t.Error("stagger should be disabled")
	}
}

// --- L3. 设备头同源 ---------------------------------------------------------

// 签到请求头与该账号推理请求头的 machineId 一致。
func TestCheckinHeadersShareDeviceIdentity(t *testing.T) {
	ident := &account.DeviceIdentity{
		MachineID:    "machine-id-xyz",
		MachineToken: "machine-token-xyz",
		MachineType:  "5",
		ClientType:   "10",
		Version:      "1.1.69",
	}
	h := checkinDeviceHeaders("dt-token", ident, true)
	if h["cosy-machineid"] != ident.MachineID {
		t.Errorf("machineid = %q, want %q", h["cosy-machineid"], ident.MachineID)
	}
	if h["cosy-machinetoken"] != ident.MachineToken {
		t.Errorf("machinetoken = %q, want %q", h["cosy-machinetoken"], ident.MachineToken)
	}
	// clienttype / version 必须与推理侧一致（同一套客户端身份）
	if h["cosy-clienttype"] != ident.ClientType {
		t.Errorf("clienttype = %q, want %q", h["cosy-clienttype"], ident.ClientType)
	}
	if h["cosy-version"] != ident.Version {
		t.Errorf("version = %q, want %q", h["cosy-version"], ident.Version)
	}
}

// 关闭「完整设备头」开关 → 不带 machinetoken 等完整集合。
func TestCheckinFullDeviceHeadersToggle(t *testing.T) {
	ident := &account.DeviceIdentity{
		MachineID:    "machine-id-xyz",
		MachineToken: "machine-token-xyz",
		MachineType:  "5",
		ClientType:   "10",
		Version:      "1.1.69",
	}
	h := checkinDeviceHeaders("dt-token", ident, false)
	if h["cosy-machineid"] != ident.MachineID {
		t.Error("identity marker should still be present")
	}
	if _, ok := h["cosy-machinetoken"]; ok {
		t.Error("full device headers disabled must not include machinetoken")
	}
}

// 身份来源优先级：真实指纹 > 持久化身份 > 无。
func TestCheckinIdentityOfPriority(t *testing.T) {
	// 真实指纹优先
	acct := &account.Account{
		ID: "a1",
		CheckinFingerprint: &account.CheckinFingerprint{
			MachineID: "real-id", MachineToken: "real-token", MachineType: "5",
		},
		DeviceIdentity: &account.DeviceIdentity{MachineID: "persisted-id", MachineToken: "persisted-token"},
	}
	ident := checkinIdentityOf(acct)
	if ident.MachineID != "real-id" {
		t.Errorf("real fingerprint should win, got %q", ident.MachineID)
	}
	if !ident.Real {
		t.Error("identity should be marked as real")
	}

	// 无真实指纹 → 用持久化身份
	acct2 := &account.Account{
		ID:             "a2",
		DeviceIdentity: &account.DeviceIdentity{MachineID: "persisted-id", MachineToken: "persisted-token"},
	}
	ident2 := checkinIdentityOf(acct2)
	if ident2.MachineID != "persisted-id" {
		t.Errorf("persisted identity should be used, got %q", ident2.MachineID)
	}
	if ident2.Real {
		t.Error("persisted identity is not a real fingerprint")
	}

	// 都没有 → nil（退回旧行为）
	if got := checkinIdentityOf(&account.Account{ID: "a3"}); got != nil {
		t.Errorf("expected nil identity, got %+v", got)
	}
}

// --- L4. 活动可见性 ---------------------------------------------------------

// 活动列表为空时，no_campaign 结果要能看出「没有可见活动」。
func TestCheckinResultCarriesCampaignVisibility(t *testing.T) {
	r := CheckinResult{CampaignsVisible: false, CampaignCount: 0}
	if r.CampaignsVisible {
		t.Error("campaigns_visible should be false")
	}
	// no_campaign 状态下活动不可见是重要风险信号，必须能被诊断接口读到
	if r.CampaignCount != 0 {
		t.Errorf("campaign count = %d, want 0", r.CampaignCount)
	}
}

// 签到接口返回体带活动可见性字段。
func TestCheckinResultJSONIncludesVisibility(t *testing.T) {
	r := CheckinResult{Status: checkinStatusNoCampaign, CampaignsVisible: false}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	for _, key := range []string{"campaigns_visible", "campaign_count", "has_claimable_activity"} {
		if !strings.Contains(s, key) {
			t.Errorf("checkin result JSON missing %q: %s", key, s)
		}
	}
}

// --- L1 端到端：两个账号签到 → 断言间隔 ≥ 最小间隔 -------------------------

// checkinAll 的账号间隔：用打桩记录每个账号的实际签到时刻。
func TestCheckinAllEnforcesAccountGap(t *testing.T) {
	oldGap, oldJitter := CheckinAccountGapMS, CheckinJitterMS
	CheckinAccountGapMS, CheckinJitterMS = 120, 20
	t.Cleanup(func() { CheckinAccountGapMS, CheckinJitterMS = oldGap, oldJitter })

	account.SetDataRoot(t.TempDir())
	ids := []string{"acct-gap-1", "acct-gap-2"}
	for _, id := range ids {
		acct := &account.Account{ID: id, Name: id, Region: account.RegionCN}
		if err := account.Save(acct); err != nil {
			t.Fatalf("save account: %v", err)
		}
		if err := account.SaveSecret(id, "dt-"+id); err != nil {
			t.Fatalf("save secret: %v", err)
		}
	}

	// 打桩：记录每个账号被签到的时刻。
	var mu sync.Mutex
	var times []time.Time
	origCheckin := checkinAccountFn
	checkinAccountFn = func(acct *account.Account) CheckinResult {
		mu.Lock()
		times = append(times, time.Now())
		mu.Unlock()
		return CheckinResult{Account: acct.Name, AccountID: acct.ID, Status: checkinStatusClaimed}
	}
	t.Cleanup(func() { checkinAccountFn = origCheckin })

	start := time.Now()
	results := CheckinAll()
	elapsed := time.Since(start)

	if len(results) != len(ids) {
		t.Fatalf("results = %d, want %d", len(results), len(ids))
	}
	if len(times) != len(ids) {
		t.Fatalf("recorded %d checkins, want %d", len(times), len(ids))
	}
	// 第一个账号立即执行，第二个账号要等最小间隔
	gap := times[1].Sub(times[0])
	minGap := time.Duration(CheckinAccountGapMS) * time.Millisecond
	if gap < minGap {
		t.Errorf("gap between accounts = %v, want >= %v", gap, minGap)
	}
	if elapsed < minGap {
		t.Errorf("total elapsed = %v, want >= %v", elapsed, minGap)
	}
}
