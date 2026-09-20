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
	"time"
)

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
	for i := range accounts {
		accounts[i].Active = accounts[i].ID == id
		if err := saveUnlocked(d, &accounts[i]); err != nil {
			return err
		}
	}
	return nil
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

func LoadSettings() (*Settings, error) {
	path := settingsPath()
	data, err := os.ReadFile(path)
	if err != nil {
		// 返回默认设置
		return &Settings{
			Port:      8963,
			AutoStart: false,
			LogLevel:  "info",
		}, nil
	}
	var s Settings
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func SaveSettings(s *Settings) error {
	path := settingsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// EnsureMachineSalt 读取（或首次生成并落盘）本机设备指纹盐（settings.json machine_salt）。
// 用途：使每个部署的指纹派生空间独立，防止 uid 派生模式被上游全局识别。
// 生成后保持不变——变更 salt 即所有账号指纹整体漂移（等价于换设备）。
func EnsureMachineSalt() (string, error) {
	st, err := LoadSettings()
	if err != nil || st == nil {
		st = &Settings{Port: 8963, LogLevel: "info"}
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
