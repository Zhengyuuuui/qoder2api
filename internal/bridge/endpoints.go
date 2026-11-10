package bridge

import (
	"sync"
	"time"

	"qoder2api/logger"
)

// 端点健康记忆 + 故障转移。
//
// 为什么需要：官方客户端的推理流量按用途分散在多个集群上，且服务端可按
// 账号 / 区域 / 实验分组把客户端选举到不同集群。已验证的实测结论是：
// 某个候选常年超时、另一个候选正常响应——把端点写死成单点会稳定踩雷。
//
// 转移的判定边界（很重要）：
//   - 传输层失败（连接超时 / TLS EOF / 连接重置）→ 换端点（这个集群不通）
//   - HTTP 5xx → 换端点（这个集群在报错）
//   - HTTP 4xx → **不**换端点（那是账号 / 模型 / 频控问题，换集群也一样，
//     换过去只是把同一个错误换个地方报，还白白多打一次上游）
//   - 业务流内错误 → 不换端点（同上）
//
// 健康记忆：某端点连续失败 N 次后进入冷却窗口，窗口内直接跳过；窗口过期
// 自动回到首选顺序（不永久拉黑——集群故障通常是暂时的，永久拉黑会让
// 恢复后仍然一直用备用端点）。

const (
	// DefaultEndpointFailThreshold 连续失败多少次后进入端点冷却窗口。
	DefaultEndpointFailThreshold = 3
	// DefaultEndpointCooldown 端点冷却窗口时长。
	DefaultEndpointCooldown = 5 * time.Minute
)

// 端点健康参数（运行时可调，便于排障与测试）。
var (
	// EndpointFailThreshold 连续失败阈值；≤0 用默认。
	EndpointFailThreshold = DefaultEndpointFailThreshold
	// EndpointCooldown 端点冷却窗口；≤0 用默认。
	EndpointCooldown = DefaultEndpointCooldown
)

// endpointHealth 记录单个端点的健康状态。
type endpointHealth struct {
	// fails 当前连续失败次数（成功即清零）。
	fails int
	// coolingUntil 冷却到期时间；零值表示不在冷却中。
	coolingUntil time.Time
	// lastErr 最近一次失败原因（诊断用）。
	lastErr string
	// lastFailAt 最近一次失败时间。
	lastFailAt time.Time
	// lastSuccessAt 最近一次成功时间（用于判定「当前生效端点」）。
	lastSuccessAt time.Time
	// successes 累计成功次数（诊断用）。
	successes int
}

// endpointRegistry 端点健康登记表。
//
// key 用「用途 + 端点 URL」：同一个 URL 既做推理又做模型目录时，两种用途
// 的健康状态互不干扰（上游可能只让其中一个用途不通）。
type endpointRegistry struct {
	mu      sync.Mutex
	entries map[string]*endpointHealth
}

var (
	endpointRegOnce sync.Once
	endpointReg     *endpointRegistry
)

// endpoints 返回全局端点健康登记表。
func endpoints() *endpointRegistry {
	endpointRegOnce.Do(func() {
		endpointReg = &endpointRegistry{entries: map[string]*endpointHealth{}}
	})
	return endpointReg
}

// endpointKey 生成端点的健康登记 key（用途 + URL）。
func endpointKey(purpose, url string) string {
	return purpose + "|" + url
}

// endpointHealthOf 取（并在必要时创建）某端点的健康记录。
func (r *endpointRegistry) endpointHealthOf(purpose, url string) *endpointHealth {
	k := endpointKey(purpose, url)
	h, ok := r.entries[k]
	if !ok {
		h = &endpointHealth{}
		r.entries[k] = h
	}
	return h
}

// NoteEndpointSuccess 记录一次端点成功（清零连续失败计数）。
func (r *endpointRegistry) NoteEndpointSuccess(purpose, url string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := r.endpointHealthOf(purpose, url)
	h.fails = 0
	h.coolingUntil = time.Time{}
	h.lastErr = ""
	h.successes++
	h.lastSuccessAt = time.Now()
}

// NoteEndpointFailure 记录一次端点失败；连续失败达阈值则进入冷却窗口。
//
// 返回是否进入了冷却窗口（调用方据此打日志）。
func (r *endpointRegistry) NoteEndpointFailure(purpose, url string, cause error) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := r.endpointHealthOf(purpose, url)
	h.fails++
	h.lastFailAt = time.Now()
	if cause != nil {
		h.lastErr = cause.Error()
	}
	if h.fails < endpointFailThreshold() {
		return false
	}
	until := time.Now().Add(endpointCooldownDuration())
	if until.After(h.coolingUntil) {
		h.coolingUntil = until
	}
	return true
}

// endpointFailThreshold 取连续失败阈值。
func endpointFailThreshold() int {
	if EndpointFailThreshold <= 0 {
		return DefaultEndpointFailThreshold
	}
	return EndpointFailThreshold
}

// endpointCooldownDuration 取端点冷却窗口时长。
func endpointCooldownDuration() time.Duration {
	if EndpointCooldown <= 0 {
		return DefaultEndpointCooldown
	}
	return EndpointCooldown
}

// EndpointCooling 报告某端点当前是否处于冷却窗口。
func (r *endpointRegistry) EndpointCooling(purpose, url string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.entries[endpointKey(purpose, url)]
	if !ok {
		return false
	}
	return time.Now().Before(h.coolingUntil)
}

// EndpointStatus 是单个端点的健康快照（诊断用）。
type EndpointStatus struct {
	Purpose      string `json:"purpose"`
	URL          string `json:"url"`
	Fails        int    `json:"fails"`
	Successes    int    `json:"successes"`
	CoolingMS    int64  `json:"cooling_remaining_ms"`
	LastError    string `json:"last_error,omitempty"`
	LastFailAt   string `json:"last_fail_at,omitempty"`
	LastFailUnix int64  `json:"-"`
}

// EndpointStatuses 返回全部端点健康快照（诊断接口用）。
func EndpointStatuses() []EndpointStatus {
	return endpoints().EndpointStatuses()
}

// CurrentEndpoints 返回当前生效的端点（按用途分组）。
//
// 用途：诊断接口要能回答「现在实际在用哪个端点」——端点会因故障转移
// 而改变，只看配置里的首选是不准的。
//
// 取「最近一次成功」的端点：成功才说明这个端点此刻是通的；失败过的端点
// 即使之前成功过，此刻也是不通的。
func CurrentEndpoints() map[string]string {
	reg := endpoints()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	type lastUse struct {
		at  time.Time
		url string
	}
	best := map[string]lastUse{}
	for k, h := range reg.entries {
		if h.successes == 0 || h.lastSuccessAt.IsZero() {
			continue
		}
		purpose, url := splitEndpointKey(k)
		if cur, ok := best[purpose]; !ok || h.lastSuccessAt.After(cur.at) {
			best[purpose] = lastUse{at: h.lastSuccessAt, url: url}
		}
	}
	out := map[string]string{}
	for purpose, l := range best {
		out[purpose] = l.url
	}
	return out
}

// EndpointStatuses 返回全部端点健康快照（按用途 + URL 排序）。
func (r *endpointRegistry) EndpointStatuses() []EndpointStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	out := make([]EndpointStatus, 0, len(r.entries))
	for k, h := range r.entries {
		purpose, url := splitEndpointKey(k)
		st := EndpointStatus{
			Purpose:   purpose,
			URL:       url,
			Fails:     h.fails,
			Successes: h.successes,
			LastError: h.lastErr,
		}
		if now.Before(h.coolingUntil) {
			st.CoolingMS = h.coolingUntil.Sub(now).Milliseconds()
		}
		if !h.lastFailAt.IsZero() {
			st.LastFailAt = h.lastFailAt.Format(time.RFC3339)
		}
		out = append(out, st)
	}
	return out
}

// splitEndpointKey 拆回「用途 | URL」。
func splitEndpointKey(k string) (purpose, url string) {
	for i := 0; i < len(k); i++ {
		if k[i] == '|' {
			return k[:i], k[i+1:]
		}
	}
	return "", k
}

// OrderedEndpoints 按「健康优先 + 原优先级」重排候选列表。
//
// 规则：
//   - 处于冷却窗口的端点排到最后（但仍保留在列表里——全部候选都冷却时
//     还要能试，否则就彻底没有可用端点了）；
//   - 冷却窗口过期的端点自动回到原有优先级位置（不永久降级）。
//
// 返回重排后的副本与原始首选（用于日志）。
func (r *endpointRegistry) OrderedEndpoints(purpose string, candidates []string) []string {
	if len(candidates) <= 1 {
		return candidates
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	type scored struct {
		url   string
		cool  bool
		order int
	}
	list := make([]scored, 0, len(candidates))
	for i, u := range candidates {
		h, ok := r.entries[endpointKey(purpose, u)]
		cool := ok && now.Before(h.coolingUntil)
		list = append(list, scored{url: u, cool: cool, order: i})
	}
	// 稳定排序：未冷却的保持原相对顺序，冷却的按原顺序排到末尾。
	out := make([]string, 0, len(list))
	for _, s := range list {
		if !s.cool {
			out = append(out, s.url)
		}
	}
	for _, s := range list {
		if s.cool {
			out = append(out, s.url)
		}
	}
	return out
}

// NoteEndpointSwitch 记录一次端点切换（打日志，便于排查「为什么走了备用端点」）。
func NoteEndpointSwitch(purpose, from, to, reason string) {
	logger.Info("endpoint switch: purpose=%s from=%s to=%s reason=%s",
		purpose, endpointHostOf(from), endpointHostOf(to), reason)
}

// endpointHostOf 取出端点的 host 部分（日志里不必打全路径）。
func endpointHostOf(raw string) string {
	if raw == "" {
		return "(none)"
	}
	if i := indexOfSlash(raw); i > 0 {
		if h := raw[:i]; hasSchemePrefix(h) {
			return h
		}
	}
	return raw
}

func indexOfSlash(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}

func hasSchemePrefix(s string) bool {
	for i := 0; i+2 < len(s); i++ {
		if s[i] == ':' && s[i+1] == '/' && s[i+2] == '/' {
			return true
		}
	}
	return false
}
