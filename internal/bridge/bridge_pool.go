package bridge

import (
	"time"

	"qoder2api/account"
)

// NewSlotFromSecret 用账号凭证构建一个可入池的 Slot（复用 NewBridge 的
// 会话构建逻辑：OAuth 直接 userinfo，PAT 走 jobToken 交换 + 稳定指纹）。
func NewSlotFromSecret(id, name string, region account.Region, secret string) (*Slot, error) {
	b, err := NewBridge(secret, region, nil)
	if err != nil {
		return nil, err
	}
	return NewSlot(id, name, region, b.sess, b.client), nil
}

// NewPoolBridge 用账号池创建桥接（多账号轮询 + 失败换号）。
func NewPoolBridge(pool *Pool, templateBase map[string]interface{}) *Bridge {
	return &Bridge{pool: pool, templateBase: templateBase}
}

// candidateSlots 返回当前可参与选号的账号：池优先；无池时退化为单账号
// （保持既有单账号构造方式与测试兼容）。
func (b *Bridge) candidateSlots() []*Slot {
	if b.pool != nil {
		return b.pool.snapshot()
	}
	if b.sess == nil || b.client == nil {
		return nil
	}
	return []*Slot{{ID: "default", Name: "default", Region: b.region, sess: b.sess, client: b.client}}
}

// pickSlot 选一个账号，跳过 exclude 与冷却中的账号。
func (b *Bridge) pickSlot(exclude map[string]struct{}) (*Slot, bool) {
	if b.pool != nil {
		return b.pool.Pick(exclude)
	}
	slots := b.candidateSlots()
	if len(slots) == 0 {
		return nil, false
	}
	if _, skip := exclude[slots[0].ID]; skip {
		return nil, false
	}
	return slots[0], true
}

// coolSlot 冷却账号（仅池模式下生效；单账号模式为 no-op）。
func (b *Bridge) coolSlot(id string, d time.Duration, reason string) {
	if b.pool != nil {
		b.pool.Cool(id, d, reason)
	}
}

// noteSlotSuccess 记录成功（仅池模式下生效）。
func (b *Bridge) noteSlotSuccess(id string) {
	if b.pool != nil {
		b.pool.NoteSuccess(id)
	}
}

// --- 按模型区域路由 ---------------------------------------------------------
// 混合国内/国际账号时，区域独占模型只能由对应区域的账号提供。这里缓存每个
// 账号所在区域的模型目录，选号时优先选「目录里有该模型」的账号；目录未知或
// 拉取失败则不参与过滤（保持宽松，避免误伤）。

const modelCatalogTTL = 30 * time.Minute

// ensureCatalog 确保账号的模型目录缓存新鲜。失败也记时间戳，避免每请求重试。
func (b *Bridge) ensureCatalog(slot *Slot) {
	slot.catalogMu.Lock()
	fresh := !slot.catalogAt.IsZero() && time.Since(slot.catalogAt) < modelCatalogTTL
	slot.catalogMu.Unlock()
	if fresh {
		return
	}
	models, err := b.fetchSlotModels(slot)
	slot.catalogMu.Lock()
	slot.catalogAt = time.Now()
	if err == nil && len(models) > 0 {
		set := make(map[string]bool, len(models))
		for _, m := range models {
			set[m.Key] = true
		}
		slot.catalog = set
	}
	slot.catalogMu.Unlock()
}

func (b *Bridge) fetchSlotModels(slot *Slot) ([]QoderModel, error) {
	resp, err := slot.client.callGet(qoderModelListURL(slot.Region))
	if err != nil {
		return nil, err
	}
	return parseQoderModels(resp), nil
}

// slotSupportsModel 该账号能否提供此模型；目录未知时返回 true（不过滤）。
func (b *Bridge) slotSupportsModel(slot *Slot, model string) bool {
	slot.catalogMu.Lock()
	defer slot.catalogMu.Unlock()
	if slot.catalog == nil {
		return true
	}
	return slot.catalog[model]
}

// pickSlotForModel 按模型选号：优先选目录里有该模型的账号；都没有则退回任意可用账号。
func (b *Bridge) pickSlotForModel(model string, exclude map[string]struct{}) (*Slot, bool) {
	if b.pool == nil {
		return b.pickSlot(exclude)
	}
	slot, ok := b.pool.Pick(exclude, func(s *Slot) bool {
		b.ensureCatalog(s)
		return b.slotSupportsModel(s, model)
	})
	if ok {
		return slot, true
	}
	return b.pool.Pick(exclude)
}
