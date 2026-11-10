package account

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// 文件存储密钥（服务器友好，不依赖 macOS Keychain）
// 路径: <DataRoot>/secrets/<id>.token
//
// 兼容从 QCCG Keychain 导出的值：
// - 明文 JSON: {"device_token":"dt-...","refresh_token":"drt-..."}
// - PAT 明文
// - go-keyring 前缀: go-keyring-base64:<base64>

const keyringPrefix = "go-keyring-base64:"

func secretsDir() (string, error) {
	d := SecretsDir()
	return d, os.MkdirAll(d, 0700)
}

func secretPath(accountID string) (string, error) {
	d, err := secretsDir()
	if err != nil {
		return "", err
	}
	id := SanitizeID(accountID)
	if id == "" {
		return "", fmt.Errorf("empty account id")
	}
	return filepath.Join(d, id+".token"), nil
}

// SecretPathFor 返回指定账号 secret 文件的路径（供 token 年龄判断等使用）。
func SecretPathFor(accountID string) (string, error) {
	return secretPath(accountID)
}

func normalizeSecret(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, keyringPrefix) {
		enc := strings.TrimPrefix(s, keyringPrefix)
		if b, err := base64.StdEncoding.DecodeString(enc); err == nil {
			s = strings.TrimSpace(string(b))
		} else if b, err := base64.RawStdEncoding.DecodeString(enc); err == nil {
			s = strings.TrimSpace(string(b))
		} else if b, err := base64.URLEncoding.DecodeString(enc); err == nil {
			s = strings.TrimSpace(string(b))
		}
	}
	return s
}

// SaveSecret 保存账号凭证。
//
// 顺带清除该账号的 needs_reauth 标志：写入新凭证意味着用户已经重新
// 授权 / 重新导入，之前「凭证过期」的判断已不再成立。集中在这里处理，
// 是因为这是所有重新授权路径（OAuth 登录、PAT 导入、token 刷新后回写）
// 的唯一汇合点——漏掉任何一条都会让账号一直「需重新授权」而无法恢复。
func SaveSecret(accountID, secret string) error {
	path, err := secretPath(accountID)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(normalizeSecret(secret)), 0600); err != nil {
		return err
	}
	clearNeedsReauth(accountID)
	return nil
}

// clearNeedsReauth 清除账号的 needs_reauth 标志（账号文件不存在时忽略）。
func clearNeedsReauth(accountID string) {
	acct, err := Get(accountID)
	if err != nil || acct == nil || !acct.NeedsReauth {
		return
	}
	acct.NeedsReauth = false
	_ = Save(acct)
}

func GetSecret(accountID string) (string, error) {
	path, err := secretPath(accountID)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("secret get %s: %w", accountID, err)
	}
	s := normalizeSecret(string(data))
	if s == "" {
		return "", fmt.Errorf("empty secret for %s", accountID)
	}
	// 若读到 keyring 编码，写回明文，方便服务器长期使用
	if strings.HasPrefix(strings.TrimSpace(string(data)), keyringPrefix) {
		_ = os.WriteFile(path, []byte(s), 0600)
	}
	return s, nil
}

func DeleteSecret(accountID string) error {
	path, err := secretPath(accountID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// HasSecret 判断账号是否已有可用凭证文件
func HasSecret(accountID string) bool {
	_, err := GetSecret(accountID)
	return err == nil
}
