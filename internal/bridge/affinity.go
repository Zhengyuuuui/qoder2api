package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"sync"
	"time"

	"qoder2api/logger"
)

// 会话亲和：同一会话优先粘同一个账号。
//
// 两个收益：
//  1. 风控画像：同一会话在多号间来回跳，是「代理池」最典型的异常特征；
//     稳定账号 + 稳定前缀才是正常客户端的样子。
//  2. 成本：上游对 prompt 前缀有缓存（约 5 分钟闲置过期），换账号等于
//     前缀全失效、全价计费。粘住账号就是省钱。
//
// 关键约束（H3）：上游 10605 是模型级队列，换号无益。因此排队等待发生在
// streamWithSlot 内部（同账号重试），根本不走到选号逻辑——亲和绑定只在
// 「请求成功」或「账号真的不可用而换号成功」时更新，排队永不解绑。
//
// 实现要点：
//   - 内存映射 + TTL + 容量上限（满了先清过期再逐最旧），无限增长会泄漏；
//   - 绑定账号不可用（冷却/熔断）时允许换号并重绑，不死守坏号；
//   - 会话键必须稳定：显式 ID 优先，否则用首条 user 消息的内容哈希，
//     哈希前剥离时间戳/路径/行号等易变部分。

const (
	// DefaultSessionAffinityTTLSec 默认绑定 TTL（秒）。略长于上游前缀
	// 缓存闲置窗口，让「隔几分钟的下一轮」仍能粘住。
	DefaultSessionAffinityTTLSec = 1800
	// affinityMaxEntries 绑定表容量上限。
	affinityMaxEntries = 1000
	// affinityHashLen 内容哈希取多少位 hex（16 位足够区分且短）。
	affinityHashLen = 16
	// affinityTextLimit 参与哈希的文本长度（rune）。只取开头——会话身份
	// 信息集中在首条消息，全量哈希长上下文时代价过高。
	affinityTextLimit = 2048
)

// volatilePatterns 内容哈希前要剥离的易变部分：同一会话的连续两轮里，
// 这些内容每次都不同（时间戳、绝对路径、行列号、长数字 ID），不剥离
// 会让每轮都算出不同的键，亲和永远命中不了。
var volatilePatterns = []*regexp.Regexp{
	regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?`), // ISO 时间
	regexp.MustCompile(`\b\d{1,2}:\d{2}(?::\d{2})?\b`),                     // 时刻
	regexp.MustCompile(`\b\d{10,13}\b`),                                    //  epoch 秒/毫秒
	regexp.MustCompile(`(?:/[\w.@+-]+){2,}/?`),                             // 绝对路径
	regexp.MustCompile(`:\d+(?::\d+)?\b`),                                  // 行:列
}

// AffinityStats 会话亲和的对外可观测状态。
type AffinityStats struct {
	Enabled bool    `json:"enabled"`  // 开关
	Bound   int     `json:"bound"`    // 当前绑定数
	Hits    int64   `json:"hits"`     // 命中次数（绑定账号被直接采用）
	Misses  int64   `json:"misses"`   // 未命中次数（无绑定 / 绑定已过期）
	HitRate float64 `json:"hit_rate"` // 命中率（0~1；无样本为 0）
	TTLSec  int     `json:"ttl_sec"`  // 生效的 TTL（秒）
}

type affinityEntry struct {
	accountID string
	expiry    time.Time
	lastUsed  time.Time
}

type affinityTable struct {
	mu      sync.Mutex
	entries map[string]*affinityEntry
	hits    int64
	misses  int64
}

var affinity = &affinityTable{entries: map[string]*affinityEntry{}}

// affinityEnabled 会话亲和总开关。
// *bool 语义见 Settings.EffectiveSessionAffinity：nil（缺字段）= 默认开启，
// 显式 false 关闭。早期 bool 版本存量配置缺字段会读成 false（保护没开），
// 属审计 P1-3 修复的行为变更。
func affinityEnabled() bool {
	if st := loadSettingsOrNil(); st != nil && !st.EffectiveSessionAffinity() {
		return false
	}
	return true
}

func affinityTTL() time.Duration {
	sec := DefaultSessionAffinityTTLSec
	if st := loadSettingsOrNil(); st != nil && st.SessionAffinityTTLSec > 0 {
		sec = st.SessionAffinityTTLSec
	}
	return time.Duration(sec) * time.Second
}

// normalizeForHash 剥离内容里的易变部分，让同一会话的连续多轮归一化到
// 同一文本。只做「明显易变」的替换，不过度归一（过度归一会把不同会话
// 哈希到同一键，亲和形同虚设）。
func normalizeForHash(s string) string {
	for _, re := range volatilePatterns {
		s = re.ReplaceAllString(s, " ")
	}
	return s
}

// ExtractSessionKey 派生会话键。explicit 非空时直接用（客户端显式提供）；
// 否则取首条 user 消息的归一化内容哈希。
//
// 返回空串表示无法派生（没有 user 消息等）——调用方应跳过亲和，走普通
// 选号，绝不允许「空键把不同会话全绑到同一个账号」。
func ExtractSessionKey(explicit string, messages []interface{}) string {
	if explicit = strings.TrimSpace(explicit); explicit != "" {
		return "k:" + explicit
	}
	text := firstUserText(messages)
	if text == "" {
		return ""
	}
	r := []rune(normalizeForHash(text))
	if len(r) > affinityTextLimit {
		r = r[:affinityTextLimit]
	}
	sum := sha256.Sum256([]byte(string(r)))
	return "h:" + hex.EncodeToString(sum[:])[:affinityHashLen]
}

// firstUserText 取首条 user 消息的文本内容。
func firstUserText(messages []interface{}) string {
	for _, m := range messages {
		mm, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		if role, _ := mm["role"].(string); role != "user" {
			continue
		}
		if t := NormalizeMessageContent(mm); t != "" {
			return t
		}
		// content 为空时尝试 contents 块（Qoder 内部消息格式）
		if contents, ok := mm["contents"].([]interface{}); ok {
			var sb strings.Builder
			for _, c := range contents {
				if cm, ok := c.(map[string]interface{}); ok {
					if t, ok := cm["text"].(string); ok {
						sb.WriteString(t)
					}
				}
			}
			if sb.Len() > 0 {
				return sb.String()
			}
		}
		return ""
	}
	return ""
}

// AffinityBind 绑定会话到账号（幂等；同键重复绑定只刷新时间）。
func AffinityBind(key, accountID string) {
	if !affinityEnabled() || key == "" || accountID == "" {
		return
	}
	now := time.Now()
	a := affinity
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.entries) >= affinityMaxEntries {
		a.evictLocked(now)
	}
	a.entries[key] = &affinityEntry{
		accountID: accountID,
		expiry:    now.Add(affinityTTL()),
		lastUsed:  now,
	}
}

// evictLocked 容量清理：先逐过期的，仍满则逐最旧使用的。
// 调用方必须已持有锁。
func (a *affinityTable) evictLocked(now time.Time) {
	for k, e := range a.entries {
		if now.After(e.expiry) {
			delete(a.entries, k)
		}
	}
	if len(a.entries) < affinityMaxEntries {
		return
	}
	var oldestKey string
	var oldest time.Time
	for k, e := range a.entries {
		if oldestKey == "" || e.lastUsed.Before(oldest) {
			oldestKey, oldest = k, e.lastUsed
		}
	}
	if oldestKey != "" {
		delete(a.entries, oldestKey)
	}
}

// AffinityAccount 查询会话绑定的账号；过期 / 未绑定返回空串。
// 命中率在此统计（命中 = 返回非空）。
func AffinityAccount(key string) string {
	if !affinityEnabled() || key == "" {
		return ""
	}
	now := time.Now()
	a := affinity
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.entries[key]
	if !ok || now.After(e.expiry) {
		if ok {
			delete(a.entries, key) // 过期即清，不留给容量清理
		}
		a.misses++
		return ""
	}
	e.lastUsed = now
	a.hits++
	return e.accountID
}

// AffinityForget 解绑（账号被确认不可用时由调用方选用；换号成功后
// 会重新绑定，这里只是及时释放旧映射）。
func AffinityForget(key string) {
	if key == "" {
		return
	}
	a := affinity
	a.mu.Lock()
	delete(a.entries, key)
	a.mu.Unlock()
}

// AffinitySnapshot 返回亲和表统计（只读）。
//
// 命名不用 AffinityStats 是为了避开同名的结构体类型（Go 不允许类型与
// 函数重名）；「Snapshot」也更能表达「这是只读快照，不是可变状态」。
func AffinitySnapshot() AffinityStats {
	now := time.Now()
	a := affinity
	a.mu.Lock()
	defer a.mu.Unlock()
	bound := 0
	for _, e := range a.entries {
		if now.Before(e.expiry) {
			bound++
		}
	}
	st := AffinityStats{
		Enabled: affinityEnabled(),
		Bound:   bound,
		Hits:    a.hits,
		Misses:  a.misses,
		TTLSec:  int(affinityTTL().Seconds()),
	}
	if total := a.hits + a.misses; total > 0 {
		st.HitRate = float64(a.hits) / float64(total)
	}
	return st
}

// explicitSessionKey 从客户端请求体提取显式会话键。
// 认几种常见约定：顶层 session_id / conversation_id，metadata 里的
// 同名字段，以及 metadata.user_id（Claude Code 的会话标识习惯）。
// 都不命中返回空串——调用方回退到内容哈希派生。
func explicitSessionKey(req map[string]interface{}) string {
	for _, k := range []string{"session_id", "conversation_id"} {
		if v, ok := req[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	if md, ok := req["metadata"].(map[string]interface{}); ok {
		for _, k := range []string{"session_id", "conversation_id", "user_id"} {
			if v, ok := md[k].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

// pickSlotWithAffinity 带会话亲和的选号：首选绑定账号（同 region 且当前
// 可用），否则回退普通选号。
//
// 绑定账号不可用（冷却/熔断/模型冷却）时**允许换号**——不死守坏号；
// 换号后由成功路径重新绑定。pool 为 nil（单账号模式）时直接走普通选号：
// 只有一个账号时亲和没有意义。
func (b *Bridge) pickSlotWithAffinity(model string, exclude map[string]struct{}, preferredID string) (*Slot, bool) {
	if preferredID != "" && b.pool != nil {
		now := time.Now()
		for _, s := range b.pool.snapshot() {
			if s.ID != preferredID {
				continue
			}
			if _, dup := exclude[s.ID]; dup {
				break // 本请求已试过（刚换号失败）——不回头
			}
			// 区域必须一致：跨区会把请求派给别区账号并扣其 credits
			if b.region != "" && s.Region != b.region {
				break
			}
			if !s.availableFor(now, model) {
				break // 账号真的不可用：允许换号（亲和让位于可用性）
			}
			logger.Debug("session affinity: using bound account %s (model=%s)", s.ID, model)
			return s, true
		}
	}
	return b.pickSlotForModel(model, exclude)
}
