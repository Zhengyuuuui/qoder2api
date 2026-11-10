package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"qoder2api/account"
	"qoder2api/internal/bridge"
)

// 风控体检（只读诊断入口）。
//
// 用途：运维排障时一眼看出「当前是账号问题、模型问题、还是出口问题」，
// 不用翻日志。与 /health 的分工：/health 是探针（极小响应），本端点
// 是体检报告（全量状态 + 人类可读结论）。
//
// 铁律：**不发任何上游请求**。诊断本身不能加剧风控——所以本文件只有
// 一个纯组装函数 + 一个读内存的 handler，结构上不存在网络调用点。
// 账号 ID 截断到前 8 位；代理地址脱敏（密码 → ***）；不返回任何
// token / 盐原文。

type diagAccount struct {
	ID             string           `json:"id"`
	Region         string           `json:"region"`
	Healthy        bool             `json:"healthy"`
	CooldownFor    int64            `json:"cooldown_for"` // 账号级冷却剩余毫秒
	BreakerFor     int64            `json:"breaker_for"`  // 熔断剩余秒数
	DegradeFor     int64            `json:"degrade_for"`  // 降权剩余秒数
	ModelCooldowns map[string]int64 `json:"model_cooldowns"`
	HardFails      int              `json:"hard_fails"`
	UnknownFails   int              `json:"unknown_fails"`
	ActiveLeases   int              `json:"active_leases"`
	// NeedsReauth 该账号凭证已失效、需用户重新授权（等待无意义，必须人工介入）。
	NeedsReauth bool `json:"needs_reauth"`
	// ProxyConfigErr 代理配置非法的原因（空 = 正常）。非空时账号不参与选号。
	ProxyConfigErr string `json:"proxy_config_err,omitempty"`
}

type diagEgress struct {
	Account string `json:"account"`
	Proxy   string `json:"proxy"` // 脱敏后的代理地址（密码 → ***）
	Bound   bool   `json:"bound"` // 是否配置了代理
}

type diagIdentity struct {
	Account string `json:"account"`
	Drift   bool   `json:"drift"`
}

// riskDiagnostics 体检报告。字段与任务约定一致。
type riskDiagnostics struct {
	Accounts  []diagAccount           `json:"accounts"`
	Risk      bridge.RiskStats        `json:"risk"`
	Throttle  bridge.ThrottleStats    `json:"throttle"`
	Affinity  bridge.AffinityStats    `json:"affinity"`
	Identity  []diagIdentity          `json:"identity"`
	Egress    []diagEgress            `json:"egress"`
	Endpoints []bridge.EndpointStatus `json:"endpoints"`
	// RegionEgress 按区域分组的出口配置（脱敏）。
	RegionEgress []bridge.RegionEgressStatus `json:"region_egress"`
	// CurrentEndpoints 当前生效的上游端点（用途 → URL）。
	CurrentEndpoints map[string]string `json:"current_endpoints"`
	// GuardSoftAllow 闸门 fail-open 软放行计数（已用 / 上限）。持续过载
	// 时它被耗尽后闸门开始拒流——这个计数是「过载程度」的直接度量。
	GuardSoftAllowUsed int      `json:"guard_soft_allow_used"`
	GuardSoftAllowCap  int      `json:"guard_soft_allow_cap"`
	Hints              []string `json:"hints"`
}

// buildRiskDiagnostics 组装体检报告并生成中文 hints。
//
// 纯函数（无网络、无全局状态）——既为可测性，也从结构上保证「诊断不
// 发起上游请求」。hints 覆盖：熔断中 / 模型冷却中 / 无可用账号 /
// 代理配置异常 / 身份漂移 / 节流降档中。
func buildRiskDiagnostics(
	slots []bridge.SlotStatus,
	sum bridge.PoolSummary,
	accounts []account.Account,
	risk bridge.RiskStats,
	throttle bridge.ThrottleStats,
	affinity bridge.AffinityStats,
	identity []bridge.IdentityAuditRow,
) riskDiagnostics {
	d := riskDiagnostics{
		Accounts:         make([]diagAccount, 0, len(slots)),
		Risk:             risk,
		Throttle:         throttle,
		Affinity:         affinity,
		Identity:         make([]diagIdentity, 0, len(identity)),
		Egress:           make([]diagEgress, 0, len(accounts)),
		Endpoints:        bridge.EndpointStatuses(),
		RegionEgress:     bridge.RegionEgressStatuses(),
		CurrentEndpoints: bridge.CurrentEndpoints(),
		Hints:            []string{},
	}
	d.GuardSoftAllowUsed, d.GuardSoftAllowCap = bridge.SoftAllowSnapshot()

	for _, s := range slots {
		healthy := s.CoolingRemainingMS <= 0 && s.BreakerFor <= 0 && !s.NeedsReauth
		d.Accounts = append(d.Accounts, diagAccount{
			ID:             shortID(s.ID),
			Region:         s.Region,
			Healthy:        healthy,
			CooldownFor:    s.CoolingRemainingMS,
			BreakerFor:     s.BreakerFor,
			DegradeFor:     s.DegradeFor,
			ModelCooldowns: s.ModelCooldowns,
			HardFails:      s.HardFails,
			UnknownFails:   s.UnknownFails,
			ActiveLeases:   s.ActiveLeases,
			NeedsReauth:    s.NeedsReauth,
			ProxyConfigErr: s.ProxyConfigErr,
		})
		if s.ProxyConfigErr != "" {
			// 代理配置非法 = P1 级问题：账号被拒绝发请求（绝不静默直连），
			// 必须在诊断里高亮，并给出可操作的修法。
			d.Hints = append(d.Hints, fmt.Sprintf(
				"账号 %s 的代理配置非法，已拒绝发请求（绝不静默直连，真实出口暴露比失败更糟）：%s。"+
					"请改为 http/https 代理地址；SOCKS5 需先经本地 http 转发层",
				shortID(s.ID), s.ProxyConfigErr))
		}
		if s.BreakerFor > 0 {
			d.Hints = append(d.Hints, fmt.Sprintf("账号 %s 处于熔断窗口，剩余 %ds", shortID(s.ID), s.BreakerFor))
		}
		if s.NeedsReauth {
			// 凭证真过期时等待毫无意义，必须提示用户重新授权
			d.Hints = append(d.Hints, fmt.Sprintf(
				"账号 %s 的凭证已失效，需重新授权（等待冷却不会恢复）", shortID(s.ID)))
		}
		for model, ms := range s.ModelCooldowns {
			d.Hints = append(d.Hints, fmt.Sprintf("账号 %s 的模型 %s 冷却中，剩余 %dms",
				shortID(s.ID), model, ms))
		}
	}

	if sum.Total > 0 && sum.ReadyCount == 0 {
		d.Hints = append(d.Hints, "所有账号均不可用（冷却/熔断中），请求将直接失败——等待冷却结束或添加账号")
	}

	// 身份漂移
	for _, row := range identity {
		d.Identity = append(d.Identity, diagIdentity{Account: shortID(row.Account), Drift: row.Drift})
		if row.Drift {
			d.Hints = append(d.Hints, fmt.Sprintf("账号 %s 设备指纹漂移（%s），请检查派生逻辑或盐变更",
				shortID(row.Account), row.Detail))
		}
	}

	// 出口代理（脱敏）
	for _, a := range accounts {
		e := diagEgress{Account: shortID(a.ID), Bound: a.Proxy != ""}
		if e.Bound {
			e.Proxy = bridge.RedactProxyURL(a.Proxy)
			if _, err := url.Parse(a.Proxy); err != nil {
				d.Hints = append(d.Hints, fmt.Sprintf("账号 %s 的代理配置异常（无法解析）：%s",
					shortID(a.ID), e.Proxy))
			}
		}
		d.Egress = append(d.Egress, e)
	}

	// 自适应节流降档
	if throttle.Level > 0 {
		d.Hints = append(d.Hints, fmt.Sprintf(
			"自适应节流生效中（level=%d，窗口 %ds 排队 %.1f%% / 频控 %.1f%%）：已自动收紧并发与请求间隔",
			throttle.Level, throttle.WindowSec, throttle.QueuePct, throttle.RatePct))
	}

	// 闸门软放行：计数非 0 说明正在过载（等待超时被放行的请求），接近
	// 上限后闸门将开始拒流——提前让运维看到过载趋势。
	if d.GuardSoftAllowUsed > 0 {
		d.Hints = append(d.Hints, fmt.Sprintf(
			"流量闸门正在过载：已软放行 %d 个请求（上限 %d/分钟），达到上限后将拒绝新请求——"+
				"请调大 concurrency_per_account 或减少客户端并发",
			d.GuardSoftAllowUsed, d.GuardSoftAllowCap))
	}

	// 端点健康：冷却中的端点会被跳过（已自动降级到备选端点）
	for _, e := range d.Endpoints {
		if e.CoolingMS > 0 {
			d.Hints = append(d.Hints, fmt.Sprintf(
				"上游端点 %s 连续失败已进冷却（剩余 %dms），当前流量已切到备选端点",
				endpointHostLabel(e.URL), e.CoolingMS))
		}
	}

	// 区域出口：共用同一出口 / 无专用出口都是明确的风控风险
	for _, r := range d.RegionEgress {
		if r.SharedWarning != "" {
			d.Hints = append(d.Hints, "区域 "+r.Region+"："+r.SharedWarning)
		}
		if r.NoDedicatedEgress != "" {
			d.Hints = append(d.Hints, "区域 "+r.Region+"："+r.NoDedicatedEgress)
		}
	}

	return d
}

// endpointHostLabel 取出端点的 host（诊断输出不必打全路径与查询串）。
func endpointHostLabel(raw string) string {
	if i := strings.Index(raw, "://"); i >= 0 {
		rest := raw[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			return rest[:j]
		}
		return rest
	}
	return raw
}

// shortID 账号 ID 截断（/api 端点不做代理凭据级别的暴露，但 ID 也只给前缀）。
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// handleRiskDiagnostics GET /api/diagnostics/risk：风控体检报告。
// 只读：除了身份审计首次运行的快照落盘（本地磁盘），不发起任何上游请求。
func handleRiskDiagnostics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("GET required"))
		return
	}
	d := riskDiagnostics{
		Accounts: []diagAccount{},
		Identity: []diagIdentity{},
		Egress:   []diagEgress{},
		Hints:    []string{},
	}
	if svc != nil {
		d = buildRiskDiagnostics(
			svc.PoolStatus(), svc.PoolSummary(), svc.ListAccounts(),
			bridge.RiskSnapshot(), bridge.ThrottleState(), bridge.AffinitySnapshot(),
			svc.IdentityAudit(),
		)
	}
	writeJSON(w, http.StatusOK, d)
}

// handleIdentityAudit GET /api/identity/audit：设备身份一致性审计。
// 只给 machineId 前 8 位与结论；不返回 token / 盐原文。
func handleIdentityAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("GET required"))
		return
	}
	rows := []bridge.IdentityAuditRow{}
	if svc != nil {
		rows = svc.IdentityAudit()
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"accounts": rows})
}
