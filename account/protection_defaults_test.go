package account

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func boolPtr(b bool) *bool { return &b }

// TestEffectiveProtectionSwitches 三个封控开关的三态语义（审计 P1-3 核心）：
// 未设置（nil）→ 默认开；显式 false → 关；显式 true → 开。
// 存量 settings.json 缺字段 = 缺省零值 nil → 保护仍在，不会因为升级
// 「宣称有保护、实际全关」。
func TestEffectiveProtectionSwitches(t *testing.T) {
	s := &Settings{}
	if !s.EffectiveTrafficGuard() {
		t.Error("traffic_guard must default to ON (nil)")
	}
	if !s.EffectiveAutoThrottle() {
		t.Error("auto_throttle must default to ON (nil)")
	}
	if !s.EffectiveSessionAffinity() {
		t.Error("session_affinity must default to ON (nil)")
	}
	s.TrafficGuard = boolPtr(false)
	s.AutoThrottle = boolPtr(false)
	s.SessionAffinity = boolPtr(false)
	if s.EffectiveTrafficGuard() || s.EffectiveAutoThrottle() || s.EffectiveSessionAffinity() {
		t.Error("explicit false must disable each switch")
	}
	s.TrafficGuard = boolPtr(true)
	s.AutoThrottle = boolPtr(true)
	s.SessionAffinity = boolPtr(true)
	if !s.EffectiveTrafficGuard() || !s.EffectiveAutoThrottle() || !s.EffectiveSessionAffinity() {
		t.Error("explicit true must enable each switch")
	}
}

// TestLoadSettingsMissingFileDefaultsOn settings.json 整个不存在
// （存量部署升级前的状态）→ 三个开关全部默认开。
func TestLoadSettingsMissingFileDefaultsOn(t *testing.T) {
	SetDataRoot(t.TempDir())
	st, err := LoadSettings()
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if !st.EffectiveTrafficGuard() || !st.EffectiveAutoThrottle() || !st.EffectiveSessionAffinity() {
		t.Fatal("missing settings file must default all protections ON")
	}
}

// TestLoadSettingsMissingKeysDefaultOn settings.json 存在但缺这三个 key
// （升级前保存的旧文件）→ 默认开。这是 P1-3 的直接回归：旧实现用 bool
// 零值会把它们读成 false。
func TestLoadSettingsMissingKeysDefaultOn(t *testing.T) {
	SetDataRoot(t.TempDir())
	raw := `{"port": 8000, "concurrency_per_account": 2}`
	if err := os.WriteFile(filepath.Join(DataRoot(), "settings.json"), []byte(raw), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	st, err := LoadSettings()
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if !st.EffectiveTrafficGuard() || !st.EffectiveAutoThrottle() || !st.EffectiveSessionAffinity() {
		t.Fatal("settings.json missing the protection keys must default ON")
	}
}

// TestLoadSettingsExplicitFalse 显式 false 必须生效（用户有权关）。
func TestLoadSettingsExplicitFalse(t *testing.T) {
	SetDataRoot(t.TempDir())
	raw := `{"traffic_guard": false, "auto_throttle": false, "session_affinity": false}`
	if err := os.WriteFile(filepath.Join(DataRoot(), "settings.json"), []byte(raw), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	st, err := LoadSettings()
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if st.EffectiveTrafficGuard() || st.EffectiveAutoThrottle() || st.EffectiveSessionAffinity() {
		t.Fatal("explicit false in settings.json must be honored")
	}
	// 往返：nil 显式区分 —— false 存盘后重新加载仍为 false
	if st.TrafficGuard == nil || *st.TrafficGuard {
		t.Error("round-trip must preserve explicit false")
	}
}

// TestLoadSettingsInvalidJSONErrors 文件损坏（含残留的 .tmp 半写状态）→
// 明确报错，绝不静默按默认值继续（否则用户以为改了配置其实没生效）。
func TestLoadSettingsInvalidJSONErrors(t *testing.T) {
	SetDataRoot(t.TempDir())
	if err := os.WriteFile(filepath.Join(DataRoot(), "settings.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadSettings(); err == nil {
		t.Fatal("corrupt settings.json must error, not silently default")
	}
}

// TestSettingsCacheInvalidatedOnSave 审计 P2-2：save 后立即 load 必须读到
// 新值（mtime+size 缓存按 Ctrl+C 语义失效，不吃陈旧数据）。
func TestSettingsCacheInvalidatedOnSave(t *testing.T) {
	SetDataRoot(t.TempDir())
	st, err := LoadSettings()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	st.Port = 8123
	if err := SaveSettings(st); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := LoadSettings()
	if err != nil {
		t.Fatalf("load after save: %v", err)
	}
	if got.Port != 8123 {
		t.Errorf("port after save = %d, want 8123 (cache must be invalidated)", got.Port)
	}
}

// TestSettingsCacheHitSameValue 缓存命中：连续 load 不反复读盘，但返回值
// 是结构体副本（调用方改返回值不得污染缓存）。
func TestSettingsCacheHitSameValue(t *testing.T) {
	SetDataRoot(t.TempDir())
	if err := SaveSettings(&Settings{Port: 9000}); err != nil {
		t.Fatalf("save: %v", err)
	}
	a, _ := LoadSettings()
	a.Port = 1 // 改副本
	b, _ := LoadSettings()
	if b.Port != 9000 {
		t.Errorf("cache polluted by caller mutation: %d, want 9000", b.Port)
	}
}

// TestSettingsCacheSeesExternalWrite 外部进程改了文件 → mtime 变化 →
// 缓存失效读到新值。
func TestSettingsCacheSeesExternalWrite(t *testing.T) {
	SetDataRoot(t.TempDir())
	if err := SaveSettings(&Settings{Port: 9100}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if st, _ := LoadSettings(); st.Port != 9100 {
		t.Fatal("precondition")
	}
	// 模拟外部写入（确保 mtime 真的变化）
	path := filepath.Join(DataRoot(), "settings.json")
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(path, []byte(`{"port": 9200}`), 0o600); err != nil {
		t.Fatalf("external write: %v", err)
	}
	os.Chtimes(path, time.Now().Add(time.Second), time.Now().Add(time.Second))
	st, err := LoadSettings()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st.Port != 9200 {
		t.Errorf("external write not seen: %d, want 9200", st.Port)
	}
}

// TestSaveSettingsAtomicNoTmpLeft 原子写：不残留 .tmp 文件，文件内容完整。
func TestSaveSettingsAtomicNoTmpLeft(t *testing.T) {
	SetDataRoot(t.TempDir())
	if err := SaveSettings(&Settings{Port: 9300}); err != nil {
		t.Fatalf("save: %v", err)
	}
	entries, err := os.ReadDir(DataRoot())
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("atomic save left a tmp file behind: %s", e.Name())
		}
	}
	st, err := LoadSettings()
	if err != nil || st.Port != 9300 {
		t.Fatalf("settings not readable after atomic save: %+v %v", st, err)
	}
}

// TestSettingsConcurrentAccess 并发读写（-race 下跑）：缓存与原子写
// 不得产生数据竞争或撕裂读取。
func TestSettingsConcurrentAccess(t *testing.T) {
	SetDataRoot(t.TempDir())
	if err := SaveSettings(&Settings{Port: 9400}); err != nil {
		t.Fatalf("save: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				st, err := LoadSettings()
				if err != nil {
					t.Errorf("load: %v", err)
					return
				}
				st.Port = 9400 + i
				if err := SaveSettings(st); err != nil {
					t.Errorf("save: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	if st, err := LoadSettings(); err != nil || st.Port < 9400 {
		t.Fatalf("final state broken: %+v %v", st, err)
	}
}

// TestMachineSaltDisabled 安装盐总开关：QD_NO_MACHINE_SALT=1 时
// EnsureMachineSalt 不得生成 / 不得写文件，MachineSaltDisabled() 为真。
func TestMachineSaltDisabled(t *testing.T) {
	SetDataRoot(t.TempDir())
	if MachineSaltDisabled() {
		t.Fatal("precondition: env not set")
	}
	first, err := EnsureMachineSalt()
	if err != nil || first == "" {
		t.Fatalf("EnsureMachineSalt: %q %v", first, err)
	}
	// 开关打开：不生成新盐、不清掉已有的
	t.Setenv(NoMachineSaltEnv, "1")
	if !MachineSaltDisabled() {
		t.Fatal("MachineSaltDisabled must report true when env is set")
	}
	got, err := EnsureMachineSalt()
	if err != nil {
		t.Fatalf("EnsureMachineSalt with salt disabled: %v", err)
	}
	if got != "" {
		t.Errorf("disabling salt must return empty salt, got %q", got)
	}
	// settings.json 里的盐不得被清掉（否则等于静默轮换）
	st, err := LoadSettings()
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if st.MachineSalt != first {
		t.Errorf("disabling salt must not rotate the stored salt: %q -> %q", first, st.MachineSalt)
	}
}

// TestMachineSaltDisabledNoSaltGenerated 开关打开且从未生成过 → 不生成。
func TestMachineSaltDisabledNoSaltGenerated(t *testing.T) {
	SetDataRoot(t.TempDir())
	t.Setenv(NoMachineSaltEnv, "1")
	got, err := EnsureMachineSalt()
	if err != nil || got != "" {
		t.Errorf("salt must not be generated when disabled: %q %v", got, err)
	}
	st, err := LoadSettings()
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if st.MachineSalt != "" {
		t.Errorf("salt must not be persisted when disabled: %q", st.MachineSalt)
	}
}

// TestNoMachineSaltEnvValues 环境变量取值语义：只有 "1" 关闭盐。
func TestNoMachineSaltEnvValues(t *testing.T) {
	t.Setenv(NoMachineSaltEnv, "1")
	if !MachineSaltDisabled() {
		t.Error("QD_NO_MACHINE_SALT=1 must disable the salt")
	}
	t.Setenv(NoMachineSaltEnv, "0")
	if MachineSaltDisabled() {
		t.Error("QD_NO_MACHINE_SALT=0 must not disable the salt")
	}
	t.Setenv(NoMachineSaltEnv, "")
	if MachineSaltDisabled() {
		t.Error("empty QD_NO_MACHINE_SALT must not disable the salt")
	}
}

// TestRegionHTTPClientTimeoutIndependence 审计复查发现的问题：区域客户端
// 曾按「区域+代理」缓存整个 http.Client，导致后到的调用方拿到第一个
// 调用方的超时。现在缓存 Transport、Client 按档位新建——两种超时必须
// 各自生效，同时仍共享同一个 Transport（连接池复用）。
func TestRegionHTTPClientTimeoutIndependence(t *testing.T) {
	t.Setenv("QD_PROXY_CN", "http://region-proxy:8080")
	c15, err := RegionHTTPClient(RegionCN, 15*time.Second)
	if err != nil {
		t.Fatalf("RegionHTTPClient 15s: %v", err)
	}
	c60, err := RegionHTTPClient(RegionCN, 60*time.Second)
	if err != nil {
		t.Fatalf("RegionHTTPClient 60s: %v", err)
	}
	if c15.Timeout != 15*time.Second || c60.Timeout != 60*time.Second {
		t.Errorf("timeouts = %v / %v, want 15s / 60s", c15.Timeout, c60.Timeout)
	}
	if c15.Transport != c60.Transport {
		t.Error("same region+proxy must share the Transport (connection pool)")
	}
}

// TestRegionHTTPClientDirectSharesTransport 未配置代理（直连）也要复用
// 连接池：曾每次新建 Transport，TCP/TLS 握手白付。
func TestRegionHTTPClientDirectSharesTransport(t *testing.T) {
	t.Setenv("QD_PROXY_CN", "")
	a, err := RegionHTTPClient(RegionCN, 10*time.Second)
	if err != nil {
		t.Fatalf("direct: %v", err)
	}
	b, err := RegionHTTPClient(RegionCN, 20*time.Second)
	if err != nil {
		t.Fatalf("direct: %v", err)
	}
	if a.Transport == nil || a.Transport != b.Transport {
		t.Error("direct region clients must share one Transport")
	}
	if a.Timeout == b.Timeout {
		t.Error("timeouts must stay independent")
	}
}

// TestRegionHTTPClientRejectsBadProxy 区域代理非法 → 明确报错（不静默直连）。
func TestRegionHTTPClientRejectsBadProxy(t *testing.T) {
	for _, raw := range []string{"socks5://127.0.0.1:1080", "http://", "ht!tp://%%%"} {
		t.Setenv("QD_PROXY_CN", raw)
		if _, err := RegionHTTPClient(RegionCN, 15*time.Second); err == nil {
			t.Errorf("region proxy %q must be rejected", raw)
		}
	}
}
