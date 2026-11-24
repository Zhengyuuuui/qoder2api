// Command modellist 拉取上游 model/list，并与上一次快照做 diff。
//
// 用途：上游模型清单会随版本变动（新增 key、改显示名、调倍率、下线）。
// 本工具用本机账号凭证实时拉一份，自动对比上次结果，直接告出「新增 /
// 下线 / 显示名变化 / 倍率变化 / 最大输出变化」，不需要人肉比对。
//
// 用法：
//
//	go run ./cmd/modellist                    # 拉全部账号 + diff
//	go run ./cmd/modellist -json              # 只输出结构化 JSON（无 diff）
//	go run ./cmd/modellist -save              # 把本次结果存为新的基线快照
//	go run ./cmd/modellist -state ./x.json    # 指定快照文件位置
//
// 环境变量：
//
//	QODER2API_HOME  数据目录（默认 ~/.qoder2api-cn）
//	ACCT            只拉指定账号 ID
//
// 快照默认落在 <QODER2API_HOME>/model_snapshot.json。首次运行只建基线，
// 之后每次运行都会打印相对基线的变化。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"qoder2api/account"
	"qoder2api/internal/bridge"
	"qoder2api/internal/cosy"
	"qoder2api/logger"
)

// modelInfo 快照里每个模型的最小可 diff 字段集。
type modelInfo struct {
	Key          string   `json:"key"`
	DisplayName  string   `json:"display_name"`
	PriceFactor  float64  `json:"price_factor"`
	MaxOutput    int      `json:"max_output_tokens"`
	IsReasoning  bool     `json:"is_reasoning"`
	ContextTiers []string `json:"context_tiers"`
}

type snapshot map[string]map[string]modelInfo // region -> key -> info

func main() {
	var (
		jsonOut = flag.Bool("json", false, "只输出结构化 JSON（不做 diff）")
		save    = flag.Bool("save", false, "把本次结果写入基线快照")
		state   = flag.String("state", "", "快照文件路径（默认 <数据目录>/model_snapshot.json）")
		verbose = flag.Bool("verbose", false, "把日志也打到 stdout（排障用）")
	)
	flag.Parse()

	// 日志默认打到 stdout，会污染 -json 输出。工具统一静默 stdout，
	// 需要排障时用 -verbose 打开。
	if !*verbose {
		logger.SilenceStdout()
	}

	root := os.Getenv("QODER2API_HOME")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, "[modellist] resolve home:", err)
			os.Exit(1)
		}
		root = filepath.Join(home, ".qoder2api-cn")
	}
	account.SetDataRoot(root)

	// 与主程序共享指纹盐，否则本工具派生的设备身份与网关不一致，
	// 上游会把请求当成陌生设备（可能直接拒绝）。
	if salt, err := account.EnsureMachineSalt(); err == nil {
		cosy.SetInstallSalt(salt)
	} else {
		fmt.Fprintln(os.Stderr, "[modellist] init machine salt failed:", err, "（运行于无盐模式）")
	}

	statePath := *state
	if statePath == "" {
		statePath = filepath.Join(root, "model_snapshot.json")
	}

	// ---- 拉取 ----
	target := os.Getenv("ACCT")
	accounts, err := account.List()
	if err != nil {
		fmt.Fprintln(os.Stderr, "[modellist] list accounts:", err)
		os.Exit(1)
	}

	now := snapshot{}
	var failures []string
	for i := range accounts {
		a := accounts[i]
		if target != "" && a.ID != target {
			continue
		}
		secret, err := account.GetSecret(a.ID)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s (%s): no secret", a.Name, a.Region))
			continue
		}
		b, err := bridge.NewBridgeWithFP(secret, a.Region, nil, a.CheckinFingerprint)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s (%s): %v", a.Name, a.Region, err))
			continue
		}
		models, err := b.ListAvailableModels()
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s (%s): %v", a.Name, a.Region, shortErr(err)))
			continue
		}
		// 同一区域多个账号时以先拉到的为准（清单按区域下发，与账号无关）
		region := string(a.Region)
		if _, ok := now[region]; ok {
			continue
		}
		now[region] = toModelInfoMap(models)
		reportf(*jsonOut, "✅ %-12s models=%d (account: %s)\n", region, len(models), a.Name)
	}

	if len(now) == 0 {
		fmt.Fprintln(os.Stderr, "\n❌ 所有账号都拉取失败，无法比对：")
		for _, f := range failures {
			fmt.Fprintln(os.Stderr, "   -", f)
		}
		fmt.Fprintln(os.Stderr, "\n提示：token 过期需在控制台重新 OAuth 授权；"+
			"refresh_token 也失效时无法自动续期。")
		os.Exit(1)
	}
	if len(failures) > 0 {
		reportf(*jsonOut, "\n⚠️  以下账号拉取失败（对应区域本次无数据）：\n")
		for _, f := range failures {
			reportf(*jsonOut, "   - %s\n", f)
		}
	}

	// ---- 输出 ----
	if *jsonOut {
		out, _ := json.MarshalIndent(now, "", "  ")
		fmt.Println(string(out))
		return
	}

	printTables(now)

	// ---- diff ----
	prev, hadPrev := loadSnapshot(statePath)
	if !hadPrev {
		fmt.Printf("\n（首次运行：已记录基线，下次运行起将自动显示变化。基线：%s）\n", statePath)
	} else {
		printDiff(prev, now)
	}

	if *save || !hadPrev {
		if err := saveSnapshot(statePath, now); err != nil {
			fmt.Fprintln(os.Stderr, "[modellist] save snapshot:", err)
		} else if hadPrev {
			fmt.Printf("\n📌 基线已更新：%s\n", statePath)
		}
	}
}

// reportf 在人类可读模式下打到 stdout，在 -json 模式下打到 stderr，
// 保证 stdout 只有一份可直接管道给 jq 的 JSON。
func reportf(jsonMode bool, format string, args ...interface{}) {
	if jsonMode {
		fmt.Fprintf(os.Stderr, format, args...)
		return
	}
	fmt.Printf(format, args...)
}

func toModelInfoMap(models []bridge.QoderModel) map[string]modelInfo {
	m := make(map[string]modelInfo, len(models))
	for _, x := range models {
		tiers := make([]string, 0, len(x.ContextTiers))
		for _, t := range x.ContextTiers {
			s := fmt.Sprintf("%s=%d", t.Name, t.Tokens)
			if t.IsDefault {
				s += "*"
			}
			tiers = append(tiers, s)
		}
		m[x.Key] = modelInfo{
			Key:          x.Key,
			DisplayName:  x.DisplayName,
			PriceFactor:  x.PriceFactor,
			MaxOutput:    x.MaxOutputTokens,
			IsReasoning:  x.IsReasoning,
			ContextTiers: tiers,
		}
	}
	return m
}

func printTables(s snapshot) {
	regions := sortedKeys(s)
	for _, region := range regions {
		m := s[region]
		fmt.Printf("\n=== %s (%d 个) ===\n", strings.ToUpper(region), len(m))
		fmt.Printf("%-18s %-24s %8s %8s %6s %8s  %s\n",
			"key", "display_name", "price", "max_out", "reas", "tiers", "")
		fmt.Println(strings.Repeat("-", 92))
		for _, k := range sortedKeys(m) {
			x := m[k]
			fmt.Printf("%-18s %-24s %8.2f %8d %6v %8s\n",
				x.Key, x.DisplayName, x.PriceFactor, x.MaxOutput, x.IsReasoning,
				shortTiers(x.ContextTiers))
		}
	}
}

// printDiff 打印相对基线的变化（新增 / 下线 / 字段变更）。
func printDiff(prev, now snapshot) {
	regions := unionKeys(prev, now)
	total := 0
	fmt.Println("\n=== 相对基线的变化 ===")
	for _, region := range regions {
		pm, nm := prev[region], now[region]
		if pm == nil && nm != nil {
			fmt.Printf("\n[%s] 新增区域（基线无此区域数据）\n", strings.ToUpper(region))
		}
		var lines []string
		for _, k := range sortedKeys(nm) {
			if _, ok := pm[k]; !ok {
				lines = append(lines, fmt.Sprintf("  🆕 新增  %-18s %s", k, nm[k].DisplayName))
			}
		}
		for _, k := range sortedKeys(pm) {
			if _, ok := nm[k]; !ok {
				lines = append(lines, fmt.Sprintf("  ➖ 下线  %-18s %s", k, pm[k].DisplayName))
			}
		}
		for _, k := range sortedKeys(nm) {
			o, ok := pm[k]
			if !ok {
				continue
			}
			n := nm[k]
			var ch []string
			if o.DisplayName != n.DisplayName {
				ch = append(ch, fmt.Sprintf("显示名 %q→%q", o.DisplayName, n.DisplayName))
			}
			if o.PriceFactor != n.PriceFactor {
				ch = append(ch, fmt.Sprintf("倍率 %g→%g", o.PriceFactor, n.PriceFactor))
			}
			if o.MaxOutput != n.MaxOutput {
				ch = append(ch, fmt.Sprintf("最大输出 %d→%d", o.MaxOutput, n.MaxOutput))
			}
			if o.IsReasoning != n.IsReasoning {
				ch = append(ch, fmt.Sprintf("推理 %v→%v", o.IsReasoning, n.IsReasoning))
			}
			if strings.Join(o.ContextTiers, ",") != strings.Join(n.ContextTiers, ",") {
				ch = append(ch, fmt.Sprintf("档位 [%s]→[%s]",
					shortTiers(o.ContextTiers), shortTiers(n.ContextTiers)))
			}
			if len(ch) > 0 {
				lines = append(lines, fmt.Sprintf("  ✏️  变更  %-18s %s", k, strings.Join(ch, "; ")))
			}
		}
		if nm == nil && pm != nil {
			lines = append(lines, fmt.Sprintf("  ⚠️  本次未拉到此区域（凭据失效？），无法比对 %d 个模型", len(pm)))
		}
		if len(lines) > 0 {
			fmt.Printf("\n[%s]\n", strings.ToUpper(region))
			for _, l := range lines {
				fmt.Println(l)
			}
			total += len(lines)
		}
	}
	if total == 0 {
		fmt.Println("\n无变化（与基线一致）。")
	}
}

func loadSnapshot(path string) (snapshot, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var s snapshot
	if err := json.Unmarshal(data, &s); err != nil || len(s) == 0 {
		return nil, false
	}
	return s, true
}

func saveSnapshot(path string, s snapshot) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	out, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func shortTiers(tiers []string) string {
	if len(tiers) == 0 {
		return "-"
	}
	names := make([]string, 0, len(tiers))
	for _, t := range tiers {
		if i := strings.IndexAny(t, "=*"); i > 0 {
			t = t[:i]
		}
		names = append(names, t)
	}
	return strings.Join(names, "/")
}

func shortErr(err error) string {
	s := err.Error()
	if len(s) > 90 {
		s = s[:90] + "…"
	}
	return s
}

func sortedKeys[M ~map[string]V, V any](m M) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func unionKeys(a, b snapshot) []string {
	set := map[string]struct{}{}
	for k := range a {
		set[k] = struct{}{}
	}
	for k := range b {
		set[k] = struct{}{}
	}
	return sortedKeys(set)
}
