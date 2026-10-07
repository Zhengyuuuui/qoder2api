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
	CooldownRate = 60 * time.Second
	// CooldownTransient 瞬时上游故障（418/5xx/传输抖动）同账号重试耗尽后：
	// 短冷却，避免立刻再撞同一个账号。
	CooldownTransient = 15 * time.Second
)

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

	// 该账号所在区域的可用模型目录缓存（用于按模型路由到正确区域）。
	catalogMu sync.Mutex
	catalog   map[string]bool
	catalogAt time.Time
}

// NewSlot 组装一个池内账号。
func NewSlot(id, name string, region account.Region, sess *cosy.SessionContext, client *BearerClient) *Slot {
	return &Slot{ID: id, Name: name, Region: region, sess: sess, client: client}
}

func (s *Slot) available(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !now.Before(s.cooldownUntil)
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

func (s *Slot) noteSuccess() {
	s.mu.Lock()
	s.lastErr = ""
	s.mu.Unlock()
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
func (p *Pool) Pick(exclude map[string]struct{}, filters ...func(*Slot) bool) (*Slot, bool) {
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
	for i := 0; i < n; i++ {
		idx := (start + i) % n
		s := slots[idx]
		if _, skip := exclude[s.ID]; skip {
			continue
		}
		if !s.available(now) {
			continue
		}
		if filter != nil && !filter(s) {
			continue
		}
		p.mu.Lock()
		p.cursor = (idx + 1) % n
		p.mu.Unlock()
		return s, true
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

// ShouldFailover 判断某次上游失败是否值得换号。
// 只有「换号可能成功」的错误才换：额度/权限(401/403)、频控(429)、
// 瞬时上游故障(418/5xx/传输层)。客户端参数错、内容审核换号无意义，直接上抛。
func ShouldFailover(err error) bool {
	if err == nil {
		return false
	}
	var ue *UpstreamError
	if errors.As(err, &ue) {
		if ue.ErrType == ErrTypeContentPolicy {
			return false
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
}

// Status 返回该账号的运行时状态。
func (s *Slot) Status() SlotStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rem int64
	if now := time.Now(); now.Before(s.cooldownUntil) {
		rem = s.cooldownUntil.Sub(now).Milliseconds()
	}
	return SlotStatus{ID: s.ID, Name: s.Name, Region: string(s.Region), CoolingRemainingMS: rem, LastError: s.lastErr}
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
