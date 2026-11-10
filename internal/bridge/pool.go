package bridge

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"qoder2api/account"
	"qoder2api/internal/cosy"
	"qoder2api/logger"
)

// 最小档账号池：选号 + 冷却 + 失败换号。
// 不做加权 / 熔断 / 降权 / 成本探索（这些留待后续按需增加）。

// ErrNoAccount 池中没有任何可用账号。
var ErrNoAccount = errors.New("no available account in pool")

const (
	// CooldownQuota 额度 / 权限类失败（上游 401/403，如 code 112 需要升级套餐）：
	// 账号在当前计费周期内基本不可用，给长冷却。
	CooldownQuota = 30 * time.Minute
	// CooldownRate 上游频控（429）。
	// 注意：429 是模型级频控，默认只写模型级冷却（见 coolModel），不会整号冷却。
	CooldownRate = 60 * time.Second
	// CooldownTransient 瞬时上游故障（418/5xx/传输抖动）同账号重试耗尽后：
	// 短冷却，避免立刻再撞同一个账号。
	CooldownTransient = 15 * time.Second
	// CooldownQuotaExhausted 额度 / 配额耗尽：长冷却。
	// 为什么比 CooldownQuota 更长：额度耗尽通常要等额度重置（按日 / 按周期），
	// 30 分钟就醒过来只会再撞一次额度墙，白白消耗请求与账号健康度。
	CooldownQuotaExhausted = 12 * time.Hour
	// CooldownTrialRestricted 试用受限：长冷却。
	// 试用状态不会在短时间内自行恢复，等待的意义同样在于「别反复试」。
	CooldownTrialRestricted = 12 * time.Hour
)

// 熔断（硬失败）与降权（未知失败）的退避参数。
//
// 为什么区分两种机制：硬失败（401/403/429/5xx）说明这个账号当下确实用不了，
// 继续让它参与选号只是反复消耗请求额度，故直接熔断排除；未知失败（既非审核
// 也非参数错的流内业务错误）原因不明，不能断定账号坏了，故只降权——
// 降权期间仍可被选中，但排在候选列表末尾，让健康账号优先。
const (
	// BreakerThreshold 连续硬失败次数达到该值即进入熔断窗口。
	BreakerThreshold = 3
	// BreakerBase 熔断窗口初始时长（后续按 ×2 退避）。
	BreakerBase = 30 * time.Minute
	// BreakerCap 熔断窗口时长上限。
	BreakerCap = 6 * time.Hour
	// DegradeThreshold 连续未知失败次数达到该值即进入降权窗口。
	DegradeThreshold = 5
	// DegradeBase 降权窗口初始时长（后续按 ×2 退避）。
	DegradeBase = 10 * time.Minute
	// DegradeCap 降权窗口时长上限。
	DegradeCap = 2 * time.Hour
)

// 模型级冷却 / 熔断降权 的运行时开关。
//
// 为什么要开关：新行为默认更安全（影响范围更小、对账号更友好），
// 但排障时需要能一键回退到旧的整号冷却 + 固定冷却行为。
// 开关值由 ApplyHealthSettings 从控制台 settings 热更新注入。
var (
	// modelCooldownEnabled 开启时 429 / 排队只写模型级冷却；关闭则整号冷却。
	modelCooldownEnabled = true
	// breakerDegradeEnabled 开启时累计熔断 / 降权计数；关闭则只做固定冷却。
	breakerDegradeEnabled = true
)

// ApplyHealthSettings 应用模型级冷却与熔断降权开关（控制台热更新）。
// 两个参数为 false 时回退到旧行为；调用方传 nil 语义时用各自的默认值。
func ApplyHealthSettings(modelCooldown, breakerDegrade bool) {
	modelCooldownEnabled = modelCooldown
	breakerDegradeEnabled = breakerDegrade
	logger.Info("health policy applied: modelCooldown=%v breakerDegrade=%v",
		modelCooldownEnabled, breakerDegradeEnabled)
}

// IsModelCooldownEnabled 返回模型级冷却是否启用（供测试与状态展示）。
func IsModelCooldownEnabled() bool { return modelCooldownEnabled }

// IsBreakerDegradeEnabled 返回熔断 / 降权是否启用（供测试与状态展示）。
func IsBreakerDegradeEnabled() bool { return breakerDegradeEnabled }

// Slot 是一个可用账号的运行时状态（session + 冷却）。
type Slot struct {
	ID     string
	Name   string
	Region account.Region

	sess   *cosy.SessionContext
	client *BearerClient

	mu            sync.Mutex
	cooldownUntil time.Time
	lastErr       string

	// modelCooldowns 是模型级冷却表（key = 模型 key）。
	// 上游的频控与排队都是模型级的：一个模型被限频不应把同账号的其他模型
	// 一起拖死，所以这类失败只写这里，不写账号级 cooldownUntil。
	modelCooldowns map[string]time.Time

	// 熔断（硬失败）与降权（未知失败）状态。
	// hardFails / unknownFails 是连续失败计数，账号成功一次即清零；
	// 退避时长直接由这两个计数推导（见 breakerBackoff / degradeBackoff），
	// 不另存档位，避免两套计数互相漂移。
	hardFails    int
	unknownFails int
	breakerUntil time.Time
	degradeUntil time.Time

	// needsReauth 该账号凭证已失效，需要重新授权。
	//
	// 与冷却的区别：冷却是「等一会儿可能好」，needs_reauth 是「等多久都不会
	// 好，必须用户重新授权」。所以它不走定时恢复，只在用户重新授权后清除。
	needsReauth bool

	// proxyConfigErr 该账号配置了代理但配置非法（socks5 / 无 host / 解析
	// 失败）。非空时账号不参与选号——「配了代理却静默直连」会把真实数据
	// 中心出口暴露给上游，比请求失败更糟（见 proxy.go resolveAccountProxyBinding）。
	proxyConfigErr string

	// 流量闸门运行时状态（见 guard.go）：在飞租约（含心跳时间）与上次
	// 请求启动时间。复用同一把 mu——闸门操作极短（纯内存），与冷却合并锁
	// 避免锁序问题。
	leases    map[string]leaseInfo
	lastStart time.Time

	// 该账号所在区域的可用模型目录缓存（用于按模型路由到正确区域）。
	catalogMu sync.Mutex
	catalog   map[string]bool
	catalogAt time.Time
}

// NewSlot 组装一个池内账号。
func NewSlot(id, name string, region account.Region, sess *cosy.SessionContext, client *BearerClient) *Slot {
	return &Slot{ID: id, Name: name, Region: region, sess: sess, client: client}
}

// availableFor 判断该账号此刻能否承接指定模型的请求。
//
// 这是唯一的可用性判定入口：账号级冷却、模型级冷却、熔断窗口三者
// 任一命中即不可用。选号路径（Pool.Pick / Ready / 桥接选号）必须全部
// 走这里，否则会出现「已经冷却却仍被选中」的漏洞。
//
// model 为空表示不限定模型（模型列表等与模型无关的请求），此时只看
// 账号级冷却与熔断。
func (s *Slot) availableFor(now time.Time, model string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.needsReauth {
		// 凭证已失效：不参与选号（但不是永久踢出——重新授权后自动恢复）
		return false
	}
	if s.proxyConfigErr != "" {
		// 代理配置非法：不参与选号。直连会把真实出口暴露给上游，
		// 必须让用户修好配置（诊断接口有高亮提示），而不是静默放行。
		return false
	}
	if now.Before(s.cooldownUntil) {
		return false
	}
	if now.Before(s.breakerUntil) {
		return false
	}
	if model != "" && modelCooldownEnabled {
		if until, ok := s.modelCooldowns[model]; ok {
			if now.Before(until) {
				return false
			}
			// 到期即删：避免 modelCooldowns 随模型种类无限增长
			delete(s.modelCooldowns, model)
		}
	}
	return true
}

// available 保留原有的「账号级可用」判定（不含模型维度）。
// 供与模型无关的场景使用（如模型列表拉取）。
func (s *Slot) available(now time.Time) bool {
	return s.availableFor(now, "")
}

// degraded 判断该账号是否处于降权窗口（原因不明的连续失败）。
// 降权不排除账号，只让它排在候选列表末尾。
func (s *Slot) degraded(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return now.Before(s.degradeUntil)
}

func (s *Slot) cool(d time.Duration, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	until := time.Now().Add(d)
	if until.After(s.cooldownUntil) {
		s.cooldownUntil = until
	}
	s.lastErr = reason
}

// coolModel 写入模型级冷却（同模型取较晚的到期时间）。
//
// 只用于模型级的失败（频控 429 / 排队 10605）：这类失败反映的是该模型
// 在该账号上的配额或容量状态，与账号本身无关，整号冷却会误伤同账号的
// 其他模型。
func (s *Slot) coolModel(model string, d time.Duration, reason string) {
	if model == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.modelCooldowns == nil {
		s.modelCooldowns = map[string]time.Time{}
	}
	until := time.Now().Add(d)
	if cur, ok := s.modelCooldowns[model]; !ok || until.After(cur) {
		s.modelCooldowns[model] = until
	}
	s.lastErr = reason
}

func (s *Slot) noteSuccess() {
	s.mu.Lock()
	s.lastErr = ""
	// 成功即清零连续失败计数：熔断 / 降权都是「连续」失败触发的，
	// 一次成功说明账号恢复正常，不该继续背着历史包袱。
	s.hardFails = 0
	s.unknownFails = 0
	s.mu.Unlock()
}

// markNeedsReauth 标记该账号凭证失效（需重新授权）。
//
// 与冷却的区别：冷却是定时恢复的，needs_reauth 不会自己恢复——凭证真过期
// 时，等待毫无意义，必须用户重新授权。
func (s *Slot) markNeedsReauth(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.needsReauth {
		s.needsReauth = true
		logger.Info("pool: account %s needs re-auth (%s)", s.ID, reason)
	}
	s.lastErr = reason
}

// clearNeedsReauth 清除 needs_reauth 标志（用户重新授权后调用）。
func (s *Slot) clearNeedsReauth() {
	s.mu.Lock()
	s.needsReauth = false
	s.mu.Unlock()
}

// NeedsReauth 报告该账号是否需要重新授权。
func (s *Slot) NeedsReauth() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.needsReauth
}

// markProxyConfigErr 标记该账号的代理配置非法（不参与选号）。
func (s *Slot) markProxyConfigErr(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proxyConfigErr == "" {
		logger.Error("pool: account %s has an invalid proxy config and will not serve requests: %s",
			s.ID, reason)
	}
	s.proxyConfigErr = reason
	s.lastErr = "proxy config error: " + reason
}

// ProxyConfigErr 返回该账号的代理配置错误原因（空 = 无异常）。
func (s *Slot) ProxyConfigErr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proxyConfigErr
}

// Pool 是账号池：轮询选号，跳过冷却中与已试过的账号。
type Pool struct {
	mu        sync.Mutex
	slots     []*Slot
	cursor    int
	statePath string // 非空时把冷却状态持久化到该文件（重启不丢）
}

func NewPool(slots ...*Slot) *Pool { return &Pool{slots: slots} }

// NewPoolWithState 创建带持久化的账号池：先从 statePath 载入冷却状态，
// 之后每次冷却 / 恢复都会写回，使重启不丢失冷却（避免重启后立刻再撞
// 已知失效的账号）。
func NewPoolWithState(statePath string, slots ...*Slot) *Pool {
	p := &Pool{slots: slots, statePath: statePath}
	p.load()
	return p
}

// poolState 是落盘的池状态（仅保存需要跨重启保留的字段）。
type poolState struct {
	Slots map[string]slotState `json:"slots"`
}

type slotState struct {
	CooldownUntil time.Time `json:"cooldown_until"`
	LastError     string    `json:"last_error,omitempty"`
}

// LoadState 从磁盘恢复冷却状态。必须在账号全部 Add 之后调用——
// 构造时空池 load 匹配不到任何 ID（这正是先前重启丢冷却的原因）。
func (p *Pool) LoadState() { p.load() }

// load 从磁盘恢复冷却状态（仅恢复尚未过期的冷却）。
func (p *Pool) load() {
	if p == nil || p.statePath == "" {
		return
	}
	data, err := os.ReadFile(p.statePath)
	if err != nil {
		return
	}
	var st poolState
	if json.Unmarshal(data, &st) != nil {
		return
	}
	now := time.Now()
	restored := 0
	for _, s := range p.snapshot() {
		e, ok := st.Slots[s.ID]
		if !ok {
			continue
		}
		s.mu.Lock()
		if e.CooldownUntil.After(now) {
			s.cooldownUntil = e.CooldownUntil
			restored++
		}
		s.lastErr = e.LastError
		s.mu.Unlock()
	}
	if restored > 0 {
		logger.Info("pool: restored %d cooling account(s) from %s", restored, p.statePath)
	}
}

// save 原子写回池状态（临时文件 + rename）。
func (p *Pool) save() {
	if p == nil || p.statePath == "" {
		return
	}
	st := poolState{Slots: map[string]slotState{}}
	for _, s := range p.snapshot() {
		s.mu.Lock()
		until := s.cooldownUntil
		lastErr := s.lastErr
		s.mu.Unlock()
		if !until.IsZero() || lastErr != "" {
			st.Slots[s.ID] = slotState{CooldownUntil: until, LastError: lastErr}
		}
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	tmp := p.statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		logger.Error("pool: save state: %v", err)
		return
	}
	if err := os.Rename(tmp, p.statePath); err != nil {
		logger.Error("pool: rename state: %v", err)
	}
}

func (p *Pool) Add(s *Slot) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.slots = append(p.slots, s)
}

func (p *Pool) Size() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.slots)
}

// snapshot 返回账号切片的副本（供 Bridge 遍历，不持有锁）。
func (p *Pool) snapshot() []*Slot {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*Slot, len(p.slots))
	copy(out, p.slots)
	return out
}

// Pick 从轮询游标开始扫描，返回第一个「未冷却、不在 exclude 中、且满足
// 可选 filter」的账号。filter 在锁外调用（允许其中做缓存/网络操作）。
//
// 该入口不限定模型（模型列表等与模型无关的请求）：只看账号级冷却与熔断。
// 带模型维度的选号请用 PickForModel。
func (p *Pool) Pick(exclude map[string]struct{}, filters ...func(*Slot) bool) (*Slot, bool) {
	return p.PickForModel(exclude, "", filters...)
}

// PickForModel 按模型选号：候选账号必须同时满足「账号级可用」与
// 「该模型未被模型级冷却」，熔断中的账号直接排除。
//
// 降权（原因不明的连续失败）不排除账号，只把它排到候选列表末尾：
// 一轮里若存在未降权的账号就优先用它们，全部降权时才用降权账号兜底。
func (p *Pool) PickForModel(exclude map[string]struct{}, model string, filters ...func(*Slot) bool) (*Slot, bool) {
	if p == nil {
		return nil, false
	}
	var filter func(*Slot) bool
	if len(filters) > 0 {
		filter = filters[0]
	}
	slots := p.snapshot()
	n := len(slots)
	if n == 0 {
		return nil, false
	}
	p.mu.Lock()
	start := p.cursor
	p.mu.Unlock()
	now := time.Now()
	// fallback 记录本轮第一个降权账号，等确认没有健康账号时再用。
	var fallback *Slot
	fallbackIdx := -1
	for i := 0; i < n; i++ {
		idx := (start + i) % n
		s := slots[idx]
		if _, skip := exclude[s.ID]; skip {
			continue
		}
		if !s.availableFor(now, model) {
			continue
		}
		if filter != nil && !filter(s) {
			continue
		}
		if s.degraded(now) {
			if fallback == nil {
				fallback = s
				fallbackIdx = idx
			}
			continue
		}
		p.mu.Lock()
		p.cursor = (idx + 1) % n
		p.mu.Unlock()
		return s, true
	}
	if fallback != nil {
		p.mu.Lock()
		p.cursor = (fallbackIdx + 1) % n
		p.mu.Unlock()
		return fallback, true
	}
	return nil, false
}

// Cool 给指定账号设置冷却（取较晚的到期时间）。
func (p *Pool) Cool(id string, d time.Duration, reason string) {
	for _, s := range p.snapshot() {
		if s.ID == id {
			s.cool(d, reason)
			logger.Info("pool: account %s cooled for %s (%s)", id, d, reason)
			p.save()
			return
		}
	}
}

// CoolModel 给指定账号的指定模型设置模型级冷却。
//
// 与账号级冷却互斥：只影响该账号的该模型，同账号其他模型仍可用。
func (p *Pool) CoolModel(id, model string, d time.Duration, reason string) {
	for _, s := range p.snapshot() {
		if s.ID == id {
			s.coolModel(model, d, reason)
			logger.Info("pool: account %s model %s cooled for %s (%s)", id, model, d, reason)
			p.save()
			return
		}
	}
}

// NoteSuccess 清除账号的失败标记。
func (p *Pool) NoteSuccess(id string) {
	for _, s := range p.snapshot() {
		if s.ID == id {
			s.noteSuccess()
			p.save()
			return
		}
	}
}

// FailClass 描述一次上游失败的归类，决定该失败如何影响账号状态。
type FailClass int

const (
	// FailClassNone 不影响账号状态（内容审核 / 参数错 / 客户端取消）：
	// 换号无意义，也不该计入熔断或降权计数。
	FailClassNone FailClass = iota
	// FailClassModel 模型级失败（429 频控 / 10605 排队）：只冷却该模型。
	FailClassModel
	// FailClassHard 硬失败（401/403/429/418/5xx）：换号或等待有用，
	// 计入熔断计数。
	FailClassHard
	// FailClassUnknown 未知失败（流内业务错误，既非审核也非参数错）：
	// 计入降权计数。
	FailClassUnknown
	// FailClassTokenExpired 凭证过期：标记账号 needs_reauth，
	// 不做「等一会儿再重试」的冷却（重试多少次都不会成功）。
	FailClassTokenExpired
	// FailClassQuotaExhausted 额度耗尽：账号级长冷却 + 换号。
	FailClassQuotaExhausted
	// FailClassTrialRestricted 试用受限：账号级长冷却 + 换号。
	FailClassTrialRestricted
	// FailClassModelNotAllowed 模型不在套餐内：不换号（同号换号无意义），
	// 直接提示用户换模型。
	FailClassModelNotAllowed
)

// ClassifyFail 把一次上游失败归类。
//
// 归类依据现有的 ShouldFailover 与错误分类（ErrType），保证「换号判定」
// 与「熔断/降权计数」用同一套语义，不会出现「不换号却计入熔断」的矛盾。
//
// 优先级说明：
//   - 内容审核 / 客户端参数错 → FailClassNone（换号无意义，不计任何计数）
//   - 429 频控 → FailClassModel（模型级冷却）+ 计入硬失败
//   - 排队（10605 / RESOURCE_LIMIT 排队语义）→ FailClassModel
//   - 凭证过期 → FailClassTokenExpired（需重新授权，不是等冷却）
//   - 额度耗尽 / 试用受限 → FailClassQuotaExhausted / FailClassTrialRestricted
//   - 模型不在套餐内 → FailClassModelNotAllowed（不换号）
//   - 401/403 其它情况 → FailClassHard（账号级冷却）
//   - 418 / 5xx → FailClassHard
//   - 其余流内业务错误 → FailClassUnknown
func ClassifyFail(err error) FailClass {
	if err == nil {
		return FailClassNone
	}
	var ue *UpstreamError
	if !errors.As(err, &ue) {
		// 裸传输错误（TLS EOF / 超时）：换号有用，按硬失败计
		if IsTransientTransport(err) {
			return FailClassHard
		}
		return FailClassNone
	}
	switch ue.ErrType {
	case ErrTypeContentPolicy:
		return FailClassNone
	case ErrTypeUpstreamQueue:
		// 排队是模型容量问题，与账号无关：只写模型级冷却
		return FailClassModel
	case ErrTypeResourceLimit:
		// 无权限用该模型：属账号/额度范畴，走账号级
		return FailClassHard
	case ErrTypeTokenExpired:
		return FailClassTokenExpired
	case ErrTypeQuotaExhausted:
		return FailClassQuotaExhausted
	case ErrTypeTrialRestricted:
		return FailClassTrialRestricted
	case ErrTypeModelNotAllowed:
		return FailClassModelNotAllowed
	}
	switch ue.Status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return FailClassHard
	case http.StatusTooManyRequests:
		return FailClassModel
	case 418:
		return FailClassHard
	}
	if ue.Status >= 500 {
		return FailClassHard
	}
	if ue.Status >= 400 && ue.Status < 500 {
		// 其余 4xx：客户端参数错，换号无意义
		return FailClassNone
	}
	// 流内业务错误（Status=0）：额度/频控类属硬失败，其余属未知失败
	if isAccountExhaustedDetail(ue.Detail) {
		return FailClassHard
	}
	return FailClassUnknown
}

// breakerBackoff 熔断退避时长：min(base * 2^(n-threshold), cap)。
//
// n 是连续硬失败次数。指数退避让「持续失败」的账号被隔离得越来越久，
// 避免「冷却 30min → 醒 → 再失败 → 再冷却」的反复唤醒把号折腾坏。
func breakerBackoff(n int) time.Duration {
	return backoffDuration(BreakerBase, BreakerCap, n, BreakerThreshold)
}

// degradeBackoff 降权退避时长：min(base * 2^(n-threshold), cap)。
func degradeBackoff(n int) time.Duration {
	return backoffDuration(DegradeBase, DegradeCap, n, DegradeThreshold)
}

// backoffDuration 通用指数退避：min(base * 2^(n-threshold), cap)。
// n 小于阈值时返回 0（尚未触发）。
func backoffDuration(base, cap time.Duration, n, threshold int) time.Duration {
	if n < threshold {
		return 0
	}
	// 指数部分单独用 int 计算，避免 base * 2^k 溢出
	exp := n - threshold
	mult := int64(1) << uint(exp)
	d := time.Duration(int64(base) * mult)
	if d > cap || d <= 0 {
		return cap
	}
	return d
}

// noteFailure 记录一次失败并更新熔断 / 降权状态。
//
// 硬失败连续达到 BreakerThreshold 次 → 进入熔断窗口（不参与选号）；
// 未知失败连续达到 DegradeThreshold 次 → 进入降权窗口（排到候选末尾）。
func (s *Slot) noteFailure(class FailClass) {
	if class != FailClassHard && class != FailClassUnknown {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if class == FailClassHard {
		s.hardFails++
		if s.hardFails >= BreakerThreshold {
			d := breakerBackoff(s.hardFails)
			until := time.Now().Add(d)
			if until.After(s.breakerUntil) {
				s.breakerUntil = until
			}
			logger.Info("pool: account %s circuit breaker open for %s (consecutive hard fails=%d)",
				s.ID, d, s.hardFails)
		}
		return
	}
	s.unknownFails++
	if s.unknownFails >= DegradeThreshold {
		d := degradeBackoff(s.unknownFails)
		until := time.Now().Add(d)
		if until.After(s.degradeUntil) {
			s.degradeUntil = until
		}
		logger.Info("pool: account %s degraded for %s (consecutive unknown fails=%d)",
			s.ID, d, s.unknownFails)
	}
}

// NoteFailure 记录一次失败并更新熔断 / 降权状态（池模式入口）。
func (p *Pool) NoteFailure(id string, class FailClass) {
	for _, s := range p.snapshot() {
		if s.ID == id {
			s.noteFailure(class)
			return
		}
	}
}

// MarkNeedsReauth 标记指定账号凭证失效（需重新授权）。
func (p *Pool) MarkNeedsReauth(id, reason string) {
	for _, s := range p.snapshot() {
		if s.ID == id {
			s.markNeedsReauth(reason)
			return
		}
	}
}

// ClearNeedsReauth 清除指定账号的 needs_reauth 标志（重新授权后调用）。
func (p *Pool) ClearNeedsReauth(id string) {
	for _, s := range p.snapshot() {
		if s.ID == id {
			s.clearNeedsReauth()
			return
		}
	}
}

// NeedsReauthAccounts 返回处于「需重新授权」状态的账号 ID 列表。
func (p *Pool) NeedsReauthAccounts() []string {
	out := []string{}
	for _, s := range p.snapshot() {
		if s.NeedsReauth() {
			out = append(out, s.ID)
		}
	}
	return out
}

// ShouldFailover 判断某次上游失败是否值得换号。
// 只有「换号可能成功」的错误才换：额度/权限(401/403)、频控(429)、
// 瞬时上游故障(418/5xx/传输层)。客户端参数错、内容审核换号无意义，直接上抛。
//
// 凭证类细分的处置：
//   - 凭证过期 → 不换号（换过去也是同样过期的凭证），交给 needs_reauth 机制
//   - 额度耗尽 / 试用受限 → 换号（换一个账号可能还有额度）
//   - 模型不在套餐内 → 不换号（同号换号无意义，提示用户换模型）
func ShouldFailover(err error) bool {
	if err == nil {
		return false
	}
	var ue *UpstreamError
	if errors.As(err, &ue) {
		if ue.ErrType == ErrTypeContentPolicy {
			return false
		}
		// 队列拥塞：换号无益（同模型同队列），且已在 streamWithSlot 内同账号
		// 等待重试；直接上抛友好错误，不冷却、不换号。
		if ue.ErrType == ErrTypeUpstreamQueue {
			return false
		}
		switch ue.ErrType {
		case ErrTypeTokenExpired:
			// 凭证过期：换号也无益（其它账号的凭证是好的，但这个账号需要
			// 用户重新授权）。不换号，标记 needs_reauth。
			return false
		case ErrTypeModelNotAllowed:
			// 模型不在套餐内：同号换号无意义。
			return false
		case ErrTypeQuotaExhausted, ErrTypeTrialRestricted:
			// 额度 / 试用受限：换一个账号可能还有额度，值得换。
			return true
		}
		switch ue.Status {
		case http.StatusUnauthorized, http.StatusForbidden: // 额度 / 凭证
			return true
		case http.StatusTooManyRequests: // 频控
			return true
		}
		if ue.Status == 418 || ue.Status >= 500 {
			return true
		}
		if ue.Status >= 400 && ue.Status < 500 {
			return false // 其余 4xx：客户端参数错
		}
		// 流内业务错误（Status=0）：额度/频控类才换号
		return isAccountExhaustedDetail(ue.Detail)
	}
	// 裸传输错误（TLS EOF / 超时等）
	return IsTransientTransport(err)
}

// isAccountExhaustedDetail 识别流内业务错误里「该账号已耗尽」的语义。
func isAccountExhaustedDetail(detail string) bool {
	d := strings.ToLower(detail)
	for _, k := range []string{"quota", "credit", "exceed", "10605", "rate limit", "too many request"} {
		if strings.Contains(d, k) {
			return true
		}
	}
	return false
}

// CooldownFor 给出错误对应的冷却时长。
//
// 注意：429（频控）与 10605（排队）是模型级问题，应调用方改用
// CooldownModelFor 并写入模型级冷却表，不要用它做整号冷却——
// 否则一个模型被限频会把同账号的其他模型一起拖死。
// 此处保留 429 分支只为向后兼容旧调用方。
func CooldownFor(err error) time.Duration {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		// 上游给了明确重试时间（如 10605 排队 retryAfterSeconds）时优先用它，
		// 上限 5 分钟，避免异常大值把账号长期雪藏。
		if d, ok := retryAfterFromDetail(ue.Detail); ok {
			return d
		}
		switch {
		case ue.Status == http.StatusUnauthorized || ue.Status == http.StatusForbidden:
			return CooldownQuota
		case ue.Status == http.StatusTooManyRequests:
			return CooldownRate
		default:
			return CooldownTransient
		}
	}
	return CooldownTransient
}

// CooldownModelFor 给出模型级失败（429 频控 / 10605 排队）对应的冷却时长。
//
// 上游给了明确重试时间时优先用它（上限 5 分钟，避免异常大值把模型长期雪藏）；
// 否则 429 用 CooldownRate，排队用 CooldownTransient 起步。
func CooldownModelFor(err error) time.Duration {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		if d, ok := retryAfterFromDetail(ue.Detail); ok {
			return d
		}
		if ue.Status == http.StatusTooManyRequests {
			return CooldownRate
		}
		return CooldownTransient
	}
	return CooldownTransient
}

// retryAfterFromDetail 从上游详情解析 retryAfterSeconds（10605 排队等场景）。
func retryAfterFromDetail(detail string) (time.Duration, bool) {
	idx := strings.Index(detail, "retryAfterSeconds")
	if idx < 0 {
		return 0, false
	}
	rest := detail[idx+len("retryAfterSeconds"):]
	start := -1
	for i := 0; i < len(rest); i++ {
		if rest[i] >= '0' && rest[i] <= '9' {
			start = i
			break
		}
	}
	if start < 0 {
		return 0, false
	}
	end := start
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	n, err := strconv.Atoi(rest[start:end])
	if err != nil || n <= 0 {
		return 0, false
	}
	d := time.Duration(n) * time.Second
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d, true
}

// SlotStatus 是给控制台/运维看的账号池运行时状态。
type SlotStatus struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Region             string `json:"region"`
	CoolingRemainingMS int64  `json:"cooling_remaining_ms"`
	LastError          string `json:"last_error,omitempty"`
	// 流量闸门可见性：在飞租约数与上次请求启动时间（RFC3339；从未启动为空）。
	ActiveLeases int    `json:"active_leases"`
	LastStart    string `json:"last_start,omitempty"`
	// ModelCooldowns 模型级冷却剩余毫秒（模型 key → 剩余毫秒）。
	// 只列出尚未到期的条目，避免响应体随历史模型种类膨胀。
	ModelCooldowns map[string]int64 `json:"model_cooldowns,omitempty"`
	// BreakerFor 熔断剩余秒数（0 = 未熔断）。
	BreakerFor int64 `json:"breaker_for"`
	// DegradeFor 降权剩余秒数（0 = 未降权）。
	DegradeFor int64 `json:"degrade_for"`
	// HardFails / UnknownFails 连续硬失败 / 未知失败次数（成功即清零）。
	HardFails    int `json:"hard_fails"`
	UnknownFails int `json:"unknown_fails"`
	// NeedsReauth 该账号凭证已失效，需要重新授权。
	NeedsReauth bool `json:"needs_reauth"`
	// ProxyConfigErr 代理配置非法的原因（空 = 正常）。非空时该账号不参与
	// 选号——配了代理绝不静默直连（真实出口暴露比请求失败更糟）。
	ProxyConfigErr string `json:"proxy_config_err,omitempty"`
}

// Status 返回该账号的运行时状态。
func (s *Slot) Status() SlotStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var rem int64
	if now.Before(s.cooldownUntil) {
		rem = s.cooldownUntil.Sub(now).Milliseconds()
	}
	st := SlotStatus{
		ID:                 s.ID,
		Name:               s.Name,
		Region:             string(s.Region),
		CoolingRemainingMS: rem,
		LastError:          s.lastErr,
		ActiveLeases:       len(s.leases),
		HardFails:          s.hardFails,
		UnknownFails:       s.unknownFails,
		NeedsReauth:        s.needsReauth,
		ProxyConfigErr:     s.proxyConfigErr,
	}
	if now.Before(s.breakerUntil) {
		st.BreakerFor = int64(s.breakerUntil.Sub(now).Seconds())
	}
	if now.Before(s.degradeUntil) {
		st.DegradeFor = int64(s.degradeUntil.Sub(now).Seconds())
	}
	if len(s.modelCooldowns) > 0 {
		st.ModelCooldowns = map[string]int64{}
		for model, until := range s.modelCooldowns {
			if now.Before(until) {
				st.ModelCooldowns[model] = until.Sub(now).Milliseconds()
			}
		}
	}
	if !s.lastStart.IsZero() {
		st.LastStart = s.lastStart.Format(time.RFC3339)
	}
	return st
}

// PoolSummary 是池内账号状态的整体汇总（控制台顶层展示用）。
type PoolSummary struct {
	Total      int `json:"total"`
	Cooling    int `json:"cooling"`
	Breaker    int `json:"breaker"`
	Degraded   int `json:"degraded"`
	ModelCool  int `json:"model_cooling"`
	ReadyCount int `json:"ready"`
}

// Summary 返回池内账号状态汇总。
func (p *Pool) Summary() PoolSummary {
	if p == nil {
		return PoolSummary{}
	}
	var sum PoolSummary
	now := time.Now()
	for _, s := range p.snapshot() {
		sum.Total++
		s.mu.Lock()
		cooling := now.Before(s.cooldownUntil)
		breaker := now.Before(s.breakerUntil)
		degraded := now.Before(s.degradeUntil)
		modelCooling := false
		for _, until := range s.modelCooldowns {
			if now.Before(until) {
				modelCooling = true
				break
			}
		}
		s.mu.Unlock()
		if cooling {
			sum.Cooling++
		}
		if breaker {
			sum.Breaker++
		}
		if degraded {
			sum.Degraded++
		}
		if modelCooling {
			sum.ModelCool++
		}
		if !cooling && !breaker {
			sum.ReadyCount++
		}
	}
	return sum
}

// Status 返回池内所有账号的运行时状态。
func (p *Pool) Status() []SlotStatus {
	if p == nil {
		return nil
	}
	out := []SlotStatus{}
	for _, s := range p.snapshot() {
		out = append(out, s.Status())
	}
	return out
}
