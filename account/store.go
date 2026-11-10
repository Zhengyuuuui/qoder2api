package account

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// saveTmpSeq settings.json 原子写的临时文件序号（保证并发保存唯一）。
var saveTmpSeq int64

var mu sync.Mutex

func dir() (string, error) {
	d := accountsDir()
	return d, os.MkdirAll(d, 0700)
}

func List() ([]Account, error) {
	mu.Lock()
	defer mu.Unlock()
	d, err := dir()
	if err != nil {
		return nil, err
	}
	return listUnlocked(d)
}

func Save(a *Account) error {
	mu.Lock()
	defer mu.Unlock()
	d, err := dir()
	if err != nil {
		return err
	}
	if a.ID == "" {
		a.ID = SanitizeID(a.Email + a.Name + fmt.Sprintf("%d", time.Now().UnixNano()))
	}
	return saveUnlocked(d, a)
}

func Delete(id string) error {
	mu.Lock()
	defer mu.Unlock()
	d, err := dir()
	if err != nil {
		return err
	}
	return os.Remove(filepath.Join(d, SanitizeID(id)+".json"))
}

// MarkNeedsReauth 标记账号凭证已失效（需重新授权）。
//
// 持久化到账号文件的原因：该状态必须跨重启保留——凭证过期不是「等一会儿
// 就好」的事，重启后若忘记标记，账号会立刻参与选号并再次失败。
// 账号不存在时忽略（调用方可能正在处理已被删除的账号）。
func MarkNeedsReauth(id string) error {
	acct, err := Get(id)
	if err != nil || acct == nil {
		return err
	}
	if acct.NeedsReauth {
		return nil
	}
	acct.NeedsReauth = true
	return Save(acct)
}

// SetActive 把指定账号设为其所在区域的「启用账号」。
// Active 按 region 隔离：同一 region 内只有一个账号可为启用，不同 region
// （cn / global）彼此独立——单控制台双桥下两区可各有一个启用账号。
func SetActive(id string) error {
	mu.Lock()
	defer mu.Unlock()
	d, err := dir()
	if err != nil {
		return err
	}
	accounts, err := listUnlocked(d)
	if err != nil {
		return err
	}
	// 目标账号所属区域（空值归一化为 global）
	targetRegion := RegionGlobal
	found := false
	for i := range accounts {
		if accounts[i].ID == id {
			targetRegion = NormalizeRegion(string(accounts[i].Region))
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("account not found: %s", id)
	}
	for i := range accounts {
		// 只改同 region 的账号，其他 region 的启用状态保持不变
		if NormalizeRegion(string(accounts[i].Region)) == targetRegion {
			accounts[i].Active = accounts[i].ID == id
		}
		if err := saveUnlocked(d, &accounts[i]); err != nil {
			return err
		}
	}
	return nil
}

// GetActiveByRegion 返回指定区域的启用账号；该区域没有则返回 nil。
func GetActiveByRegion(region Region) (*Account, error) {
	accounts, err := List()
	if err != nil {
		return nil, err
	}
	want := NormalizeRegion(string(region))
	for i := range accounts {
		if accounts[i].Active && NormalizeRegion(string(accounts[i].Region)) == want {
			return &accounts[i], nil
		}
	}
	return nil, nil
}

func GetActive() (*Account, error) {
	accounts, err := List()
	if err != nil {
		return nil, err
	}
	for i := range accounts {
		if accounts[i].Active {
			return &accounts[i], nil
		}
	}
	return nil, nil
}

// Get 根据 ID 获取账号
func Get(id string) (*Account, error) {
	accounts, err := List()
	if err != nil {
		return nil, err
	}
	for i := range accounts {
		if accounts[i].ID == id {
			return &accounts[i], nil
		}
	}
	return nil, fmt.Errorf("account not found: %s", id)
}

func listUnlocked(d string) ([]Account, error) {
	entries, err := os.ReadDir(d)
	if err != nil {
		return nil, err
	}
	var accounts []Account
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(d, e.Name()))
		if err != nil {
			continue
		}
		var a Account
		if err := json.Unmarshal(data, &a); err != nil {
			continue
		}
		accounts = append(accounts, a)
	}
	sort.Slice(accounts, func(i, j int) bool {
		return accounts[i].SortOrder < accounts[j].SortOrder
	})
	return accounts, nil
}

func saveUnlocked(d string, a *Account) error {
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(d, a.ID+".json"), data, 0600)
}

func Reorder(ids []string) error {
	mu.Lock()
	defer mu.Unlock()
	d, err := dir()
	if err != nil {
		return err
	}
	accounts, err := listUnlocked(d)
	if err != nil {
		return err
	}
	orderMap := make(map[string]int)
	for i, id := range ids {
		orderMap[id] = i
	}
	for i := range accounts {
		if order, ok := orderMap[accounts[i].ID]; ok {
			accounts[i].SortOrder = order
		}
		if err := saveUnlocked(d, &accounts[i]); err != nil {
			return err
		}
	}
	return nil
}

func SanitizeID(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if len(s) > 64 {
		s = s[:64]
	}
	if s == "" {
		s = fmt.Sprintf("acct%d", time.Now().UnixNano())
	}
	return s
}

// settingsCache 进程内 settings 缓存（mtime + size 失效）。
//
// 为什么需要：guard / throttle / affinity / MapModel / context 等每个请求
// 都要读 settings，直接读盘等于每请求数次磁盘 IO；更糟的是「写一半的
// 文件」会被读到 → Unmarshal 失败 → 回落默认值 → 口径瞬时抖动。
// 以 mtime+size 为失效条件：保存后立刻可见，外部改文件也立刻生效。
//
// 返回的是结构体副本（浅拷贝）：调用方普遍「读字段」或整体替换字段，
// 浅拷贝足以防止把缓存本身改花；map 字段不被原地修改（各调用方均为
// 整体赋值），故不深拷贝。
var settingsCache struct {
	mu     sync.Mutex
	valid  bool
	mod    time.Time
	size   int64
	parsed *Settings
}

// loadSettingsUncached 真正读盘解析。
func loadSettingsUncached(path string) (*Settings, time.Time, int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, time.Time{}, 0, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, info.ModTime(), info.Size(), err
	}
	var s Settings
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, info.ModTime(), info.Size(), err
	}
	return &s, info.ModTime(), info.Size(), nil
}

// defaultSettings 无 settings.json 时的默认值。
// 三个保护开关用 *bool 且留 nil —— Effective*() 会给 true（保护默认开）。
func defaultSettings() *Settings {
	return &Settings{Port: 8963, LogLevel: "info"}
}

func LoadSettings() (*Settings, error) {
	path := settingsPath()

	settingsCache.mu.Lock()
	defer settingsCache.mu.Unlock()

	info, err := os.Stat(path)
	if err != nil {
		// 文件不存在：返回默认值，不写缓存（等文件出现再走正常路径）
		settingsCache.valid = false
		return defaultSettings(), nil
	}
	if settingsCache.valid && settingsCache.parsed != nil &&
		settingsCache.mod.Equal(info.ModTime()) && settingsCache.size == info.Size() {
		cp := *settingsCache.parsed
		return &cp, nil
	}
	st, mod, size, err := loadSettingsUncached(path)
	if err != nil {
		// 读盘 / 解析失败：不写缓存，下次重试（可能是写入中途）
		settingsCache.valid = false
		return nil, err
	}
	settingsCache.valid = true
	settingsCache.mod = mod
	settingsCache.size = size
	settingsCache.parsed = st
	cp := *st
	return &cp, nil
}

// SaveSettings 原子写 settings（临时文件 + rename）。
//
// 为什么必须原子：直接 os.WriteFile 是「截断 → 写」两步，并发读会读到
// 半截 JSON → 解析失败 → 回落默认值 → 保护开关口径瞬时抖动。rename 保证
// 读者只会看到「完整的旧文件」或「完整的新文件」（与 Pool.save 同模式）。
func SaveSettings(s *Settings) error {
	path := settingsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	// tmp 名带唯一后缀：并发保存（多个 goroutine 同时改配置）时各自写
	// 自己的临时文件，不会互相把对方的 .tmp rename 走。pid + 进程内
	// 自增序号保证同进程并发也唯一。
	tmp := fmt.Sprintf("%s.tmp.%d.%d", path, os.Getpid(), atomic.AddInt64(&saveTmpSeq, 1))
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// 主动刷新缓存：rename 后 mtime 一定变，但顺手更新可让同进程内的
	// 下一次读取立刻命中（不必等一次 stat 比对）。
	settingsCache.mu.Lock()
	if info, err := os.Stat(path); err == nil {
		settingsCache.valid = true
		settingsCache.mod = info.ModTime()
		settingsCache.size = info.Size()
		cp := *s
		settingsCache.parsed = &cp
	} else {
		settingsCache.valid = false
	}
	settingsCache.mu.Unlock()
	return nil
}

// NoMachineSaltEnv 置 "1" 时不生成、不使用本机盐：派生结果与「无盐」
// 基准完全一致。语义与 QD_DEVICE_ID_LEGACY（控制形态）正交——那个管
// UUID/hex 形态，这个管是否混盐。用于存量部署升级后平滑保旧指纹。
const NoMachineSaltEnv = "QD_NO_MACHINE_SALT"

// MachineSaltDisabled 报告当前是否显式关闭了本机盐。
func MachineSaltDisabled() bool {
	return os.Getenv(NoMachineSaltEnv) == "1"
}

// EnsureMachineSalt 读取（或首次生成并落盘）本机设备指纹盐（settings.json machine_salt）。
// 用途：使每个部署的指纹派生空间独立，防止 uid 派生模式被上游全局识别。
// 生成后保持不变——变更 salt 即所有账号指纹整体漂移（等价于换设备）。
//
// QD_NO_MACHINE_SALT=1 时直接返回空盐且不落盘：调用方据此进入「无盐
// 基准形态」，与升级前的派生值逐字节一致（真正的回退路径）。
func EnsureMachineSalt() (string, error) {
	if MachineSaltDisabled() {
		return "", nil
	}
	st, err := LoadSettings()
	if err != nil || st == nil {
		st = defaultSettings()
	}
	if st.MachineSalt != "" {
		return st.MachineSalt, nil
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	st.MachineSalt = hex.EncodeToString(b)
	if err := SaveSettings(st); err != nil {
		return "", err
	}
	return st.MachineSalt, nil
}
