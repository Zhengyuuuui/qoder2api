package bridge

import (
	"strings"
	"sync"

	"qoder2api/logger"
)

// ============================================================================
// 运行时模型别名表（display_name → model.key）
//
// 为什么需要这份「运行时」表，而不是写死在代码里：
//
// 上游 model/list 每一次返回都自带 display_name，那才是显示名的**唯一权威
// 来源**。历史做法是把显示名→key 硬编码到代码里，导致上游每次新增模型 /
// 改显示名，都得「改代码 → 编译 → 重启 → 人工抄 README」，既慢又必然滞后，
// 还会因为漏改而让客户端把显示名当 key 发出去 —— 上游对未知 key 会兜底
// 返回内容但不计 quota（issue #13「请求成功但 dashboard 无用量」）。
//
// 现在的做法：只要成功拉到一次清单，就把当前全部 display_name 注册进内存
// 别名表，之后客户端传显示名即可自动落到正确 key。上游改版后下一次拉取
// （5 分钟缓存、或 /v1/models 请求、或控制台打开模型面板）就自动跟上，
// **无需改代码、无需重启**。
//
// 静态的 upstreamDisplayNameAliases 只作为「还没拉到过清单」时的种子：
// 网关刚启动、上游全挂、凭据失效时仍能认出常见名字。
// ============================================================================

var runtimeAliases = struct {
	mu   sync.RWMutex
	m    map[string]string // 归一化显示名 → key
	raw  map[string]string // 原样小写显示名 → key（精确命中优先）
	keys map[string]struct{}
}{m: map[string]string{}, raw: map[string]string{}, keys: map[string]struct{}{}}

// RegisterModelCatalog 用上游实时清单刷新运行时别名表与合法 key 集合。
//
// 幂等：可反复调用（每次拉取清单都会调一次）。只增不删——上游暂时隐藏某
// 模型（如按档位/活动下发）时，已注册的别名仍然有效，避免客户端手里旧的
// 显示名突然变成未知 key。真正下线的 key 由 knownUpstreamKeys 兜底判定。
func RegisterModelCatalog(models []QoderModel) {
	if len(models) == 0 {
		return
	}
	runtimeAliases.mu.Lock()
	defer runtimeAliases.mu.Unlock()

	added := 0
	for _, m := range models {
		key := strings.TrimSpace(m.Key)
		if key == "" {
			continue
		}
		runtimeAliases.keys[strings.ToLower(key)] = struct{}{}
		// key 自身也要能被查到（大小写归一）
		runtimeAliases.raw[strings.ToLower(key)] = key

		name := strings.TrimSpace(m.DisplayName)
		if name == "" {
			continue
		}
		low := strings.ToLower(name)
		if _, ok := runtimeAliases.raw[low]; !ok {
			added++
		}
		runtimeAliases.raw[low] = key
		runtimeAliases.m[normalizeModelName(low)] = key
	}
	if added > 0 {
		logger.Info("[model] registered %d display-name alias(es) from upstream catalog (total keys=%d)",
			added, len(runtimeAliases.keys))
	}
}

// normalizeModelName 归一化模型名：小写、去掉 - _ 空格。
//
// 用于别名表的模糊匹配（客户端常省略 "-preview" 后缀或去掉分隔符），
// 例如 "Qwen4-Max" / "qwen4max" / "qwen4 max" 归一化后一致。
func normalizeModelName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, "_", "")
	s = strings.ReplaceAll(s, " ", "")
	return s
}

// lookupRuntimeAlias 在运行时别名表里查显示名对应的 key。
//
// 两级：先精确（小写）命中，再归一化命中。归一化命中要求**唯一**——
// 多个显示名归一化后撞车说明有歧义，宁可返回空让上游兜底，也不猜错
// （猜错会消耗另一个模型的额度）。
func lookupRuntimeAlias(model string) string {
	low := strings.ToLower(strings.TrimSpace(model))
	if low == "" {
		return ""
	}
	runtimeAliases.mu.RLock()
	defer runtimeAliases.mu.RUnlock()

	if key, ok := runtimeAliases.raw[low]; ok {
		return key
	}
	want := normalizeModelName(low)
	if want == "" {
		return ""
	}
	if key, ok := runtimeAliases.m[want]; ok {
		return key
	}
	return ""
}

// isRuntimeKnownKey 报告 key 是否出现在最近一次拉取的上游清单里。
func isRuntimeKnownKey(key string) bool {
	runtimeAliases.mu.RLock()
	defer runtimeAliases.mu.RUnlock()
	_, ok := runtimeAliases.keys[strings.ToLower(strings.TrimSpace(key))]
	return ok
}

// RuntimeAliasCount 返回当前运行时别名表规模（诊断/测试用）。
func RuntimeAliasCount() int {
	runtimeAliases.mu.RLock()
	defer runtimeAliases.mu.RUnlock()
	return len(runtimeAliases.raw)
}
