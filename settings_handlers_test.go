package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"qoder2api/account"
	"qoder2api/internal/bridge"
	"qoder2api/logger"
)

// settingsTestSvc 隔离 DataRoot 并替换全局 svc。
func settingsTestSvc(t *testing.T) *Service {
	t.Helper()
	orig := account.DataRoot()
	account.SetDataRoot(t.TempDir())
	t.Cleanup(func() { account.SetDataRoot(orig) })
	s := NewService(nil)
	origSvc := svc
	svc = s
	t.Cleanup(func() { svc = origSvc })
	return s
}

// TestHandleGetSettingsEchoesEffectiveSwitches 审计 P1-3：控制台读到
// 的必须是「真实生效值」。存量 settings.json 缺这三个 key（早期 bool
// 零值会读成 false）→ 接口必须回 true，否则运维在面板上看到「保护全关」
// 以为自己在裸奔，实际开着——误导比缺省更糟。
func TestHandleGetSettingsEchoesEffectiveSwitches(t *testing.T) {
	settingsTestSvc(t)
	// 不写任何 settings：文件不存在 → 默认全开
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	handleGetSettings(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp struct {
		Settings account.Settings `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Settings.EffectiveTrafficGuard() || !resp.Settings.EffectiveAutoThrottle() || !resp.Settings.EffectiveSessionAffinity() {
		t.Errorf("missing settings must echo protections ON: %+v", resp.Settings)
	}
	if resp.Settings.TrafficGuard == nil || *resp.Settings.TrafficGuard != true {
		t.Error("traffic_guard must be echoed as an explicit true")
	}
}

// TestHandleSaveSettingsExplicitFalseWins 显式 false 落盘并回显 false
// （用户有权关，但必须是显式的）。
func TestHandleSaveSettingsExplicitFalseWins(t *testing.T) {
	settingsTestSvc(t)
	body := `{"traffic_guard": false, "session_affinity": false}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/settings/save", bytes.NewReader([]byte(body)))
	handleSaveSettings(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("save status = %d: %s", rec.Code, rec.Body.String())
	}
	st, err := svc.GetSettings()
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if st.EffectiveTrafficGuard() || st.EffectiveSessionAffinity() {
		t.Error("explicit false must be persisted and honored")
	}
	if st.TrafficGuard == nil || *st.TrafficGuard {
		t.Error("explicit false must round-trip as a non-nil false pointer")
	}
	// 未提到的 auto_throttle 保持 nil（= 默认开）
	if st.AutoThrottle != nil {
		t.Errorf("untouched switch must stay nil, got %v", *st.AutoThrottle)
	}
	if !st.EffectiveAutoThrottle() {
		t.Error("untouched switch must remain effectively ON")
	}
}

// TestHandleSaveSettingsQueueBudget 审计 P2-4：排队总预算可配且热更新
// 生效（ApplyQueueSettings 拿到保存值）。
func TestHandleSaveSettingsQueueBudget(t *testing.T) {
	settingsTestSvc(t)
	t.Cleanup(func() { bridge.ApplyQueueSettings(0, 0, 0, true, 0) })
	body := `{"queue_wait_budget_sec": 120, "queue_wait_cap_sec": 20, "queue_wait_default_sec": 10, "queue_max_retries": 4}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/settings/save", bytes.NewReader([]byte(body)))
	handleSaveSettings(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("save status = %d: %s", rec.Code, rec.Body.String())
	}
	if bridge.QueueWaitBudget.Seconds() != 120 {
		t.Errorf("queue budget = %v, want 2m", bridge.QueueWaitBudget)
	}
	st, err := svc.GetSettings()
	if err != nil || st.QueueWaitBudgetSec != 120 {
		t.Errorf("saved budget = %+v, %v", st, err)
	}
}

// TestWarnProtectionDisabled 审计 P1-2/P1-3：有账号但三个封控开关被显式
// 全关 → 启动时必须 ERROR 告警（否则账号裸奔而无人知晓）。
func TestWarnProtectionDisabled(t *testing.T) {
	settingsTestSvc(t)
	if err := account.Save(&account.Account{ID: "acct-warn-1", Name: "w", Region: account.RegionCN}); err != nil {
		t.Fatalf("save: %v", err)
	}

	// 全关 → 告警
	if err := account.SaveSettings(&account.Settings{
		Port: 8963, LogLevel: "info",
		TrafficGuard:    ptrBool(false),
		AutoThrottle:    ptrBool(false),
		SessionAffinity: ptrBool(false),
	}); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	before := logger.GetLogsSince(0, 1).LastSeq
	warnProtectionDisabled()
	var found bool
	for _, e := range logger.GetLogsSince(before, 50).Entries {
		if strings.Contains(e.Message, "封控保护未全开") {
			found = true
		}
	}
	if !found {
		t.Error("all protections explicitly off must produce a startup warning")
	}

	// 有任一开着 → 不告警
	tr := true
	if err := account.SaveSettings(&account.Settings{
		Port: 8963, LogLevel: "info",
		TrafficGuard: &tr,
	}); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	before = logger.GetLogsSince(0, 1).LastSeq
	warnProtectionDisabled()
	for _, e := range logger.GetLogsSince(before, 50).Entries {
		if strings.Contains(e.Message, "封控保护未全开") {
			t.Error("no warning expected while at least one protection is on")
		}
	}

	// 无账号 → 不告警（没有要保护的对象）
	if err := account.Delete("acct-warn-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := account.SaveSettings(&account.Settings{
		Port: 8963, LogLevel: "info",
		TrafficGuard: ptrBool(false), AutoThrottle: ptrBool(false), SessionAffinity: ptrBool(false),
	}); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	before = logger.GetLogsSince(0, 1).LastSeq
	warnProtectionDisabled()
	for _, e := range logger.GetLogsSince(before, 50).Entries {
		if strings.Contains(e.Message, "封控保护未全开") {
			t.Error("no warning expected without any account")
		}
	}
}

func ptrBool(b bool) *bool { return &b }
