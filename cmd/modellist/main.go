// Command modellist 拉取上游 model/list 并打印结构化清单。
//
// 用途：上游模型清单会随版本变动（新增 key、改显示名、调倍率），
// 本工具用本机账号凭证实时拉一份，供 README 模型总表与
// webconsole 的 NewAPI models 数组对照更新。
//
// 用法：
//
//	go run ./cmd/modellist                 # 拉全部账号（默认 ~/.qoder2api-cn）
//	QODER2API_HOME=~/.qoder2api-cn go run ./cmd/modellist
//	ACCT=<account_id> go run ./cmd/modellist   # 只拉指定账号
//	RAW=1 go run ./cmd/modellist           # 额外把原始 JSON 落到 /tmp
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"qoder2api/account"
	"qoder2api/internal/bridge"
	"qoder2api/internal/cosy"
)

func main() {
	root := os.Getenv("QODER2API_HOME")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, "[modellist] resolve home:", err)
			os.Exit(1)
		}
		root = home + "/.qoder2api-cn"
	}
	account.SetDataRoot(root)

	// 与主程序共享 settings.json 的指纹盐，保证同一部署内指纹一致
	// （盐不同 = 上游眼中的新设备 = 账号被风控）。
	if salt, err := account.EnsureMachineSalt(); err == nil {
		cosy.SetInstallSalt(salt)
	} else {
		fmt.Fprintln(os.Stderr, "[modellist] init machine salt failed:", err,
			"；运行于无盐模式（基准形态指纹）")
	}

	target := os.Getenv("ACCT")
	accounts, err := account.List()
	if err != nil {
		fmt.Fprintln(os.Stderr, "[modellist] list accounts:", err)
		os.Exit(1)
	}

	for i := range accounts {
		a := accounts[i]
		if target != "" && a.ID != target {
			continue
		}
		secret, err := account.GetSecret(a.ID)
		if err != nil {
			fmt.Printf("== %s (%s region=%s): no secret: %v\n", a.Name, a.ID, a.Region, err)
			continue
		}
		b, err := bridge.NewBridgeWithFP(secret, a.Region, nil, a.CheckinFingerprint)
		if err != nil {
			fmt.Printf("== %s (%s region=%s): bridge error: %v\n", a.Name, a.ID, a.Region, err)
			continue
		}
		models, err := b.ListAvailableModels()
		if err != nil {
			fmt.Printf("== %s (%s region=%s): list error: %v\n", a.Name, a.ID, a.Region, err)
			continue
		}
		modern := a.CheckinFingerprint != nil && a.CheckinFingerprint.MachineToken != ""
		fmt.Printf("\n===== %s | id=%s | region=%s | modern_protocol=%v | models=%d =====\n",
			a.Name, a.ID, a.Region, modern, len(models))
		rows := make([]map[string]interface{}, 0, len(models))
		for _, m := range models {
			row := map[string]interface{}{
				"key":               m.Key,
				"display_name":      m.DisplayName,
				"enable":            m.Enable,
				"is_default":        m.IsDefault,
				"is_reasoning":      m.IsReasoning,
				"max_input_tokens":  m.MaxInputTokens,
				"price_factor":      m.PriceFactor,
				"context_window":    m.ContextWindow,
				"max_output_tokens": m.MaxOutputTokens,
			}
			if len(m.ContextTiers) > 0 {
				tiers := make([]string, 0, len(m.ContextTiers))
				for _, t := range m.ContextTiers {
					s := fmt.Sprintf("%s=%d", t.Name, t.Tokens)
					if t.IsDefault {
						s += "(default)"
					}
					tiers = append(tiers, s)
				}
				row["context_tiers"] = tiers
			}
			rows = append(rows, row)
		}
		sort.Slice(rows, func(x, y int) bool {
			return rows[x]["key"].(string) < rows[y]["key"].(string)
		})
		out, _ := json.MarshalIndent(rows, "  ", "  ")
		fmt.Println(string(out))
	}
}
