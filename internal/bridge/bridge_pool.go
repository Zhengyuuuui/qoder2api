package bridge

import (
	"fmt"
	"time"

	"qoder2api/account"
	"qoder2api/logger"
)

// NewSlotFromSecret 用账号凭证构建一个可入池的 Slot（复用 NewBridge 的
// 会话构建逻辑：OAuth 直接 userinfo，PAT 走 jobToken 交换 + 稳定指纹）。
func NewSlotFromSecret(id, name string, region account.Region, secret string) (*Slot, error) {
	return NewSlotFromSecretWithFP(id, name, region, secret, nil)
}

// NewSlotFromSecretWithFP 用账号凭证构建 Slot；fp 非空时该账号走新版推理协议
// （可访问 Sonus / Cantus / fast）。
func NewSlotFromSecretWithFP(id, name string, region account.Region, secret string, fp *account.CheckinFingerprint) (*Slot, error) {
	return NewSlotFromSecretWithProxy(id, name, region, secret, fp, "")
}

// NewSlotFromSecretWithProxy 用账号凭证构建 Slot，并绑定该账号的出口代理。
//
// proxy 为空表示直连（存量账号行为不变）。绑定是持久的：解析一次后
// 写入 client，后续所有请求复用同一个 Transport（连接池复用、出口稳定）。
func NewSlotFromSecretWithProxy(id, name string, region account.Region, secret string, fp *account.CheckinFingerprint, proxy string) (*Slot, error) {
	b, err := NewBridgeWithFP(secret, region, nil, fp)
	if err != nil {
		return nil, err
	}
	slot := NewSlot(id, name, region, b.sess, b.client)
	slot.bindProxy(id, proxy, string(region))
	return slot, nil
}

// bindProxy 绑定账号的出口代理（账号级 > 区域级 > 直连）。
//
// 调用时机是账号入池时（只此一次）：出口一旦绑定就不该在请求之间变动，
// 否则上游看到的是「同一账号不断换 IP」，比共用一个 IP 更容易被判异常。
//
// 配置非法（socks5 / 无 host / 解析失败）时**不直连**：把账号标记为
// 「代理配置错误」并不参与选号（availableFor 拦截），让问题在诊断接口
// 高亮可见。一个账号配错不拖垮整个池的启动。
func (s *Slot) bindProxy(accountID, configured, region string) {
	binding := resolveAccountProxyInRegion(accountID, configured, region)
	if binding.ConfigErr != nil {
		s.markProxyConfigErr(binding.ConfigErr.Error())
		// 不注册出口（该账号本就不该发请求），保留 client 直连能力也
		// 无所谓——选号路径永远到不了它。
		return
	}
	proxy := binding.URL
	s.client.SetAccountProxy(accountID, proxy)
	s.client.SetAccountRegion(region)
	registerEgress(region, accountID, proxy)
	initProxyLogging(accountID, proxy)
	// 区域无专用出口时明确告警：两个区域共用一个出口 IP 时，上游更容易
	// 把流量归为同一使用群体（国际版在数据中心 IP 上尤其容易触发排队）。
	if !regionProxyConfigured(region) && proxy == "" {
		logger.Error("region %s has no dedicated egress (%s unset) and account %s is direct: "+
			"this region likely shares one egress IP with the other region, which makes "+
			"upstream risk-control correlate them as one usage group",
			region, regionProxyEnvName(region), shortAccountID(accountID))
	}
}

// NewPoolBridge 用账号池创建桥接（多账号轮询 + 失败换号）。
func NewPoolBridge(pool *Pool, templateBase map[string]interface{}) *Bridge {
	return &Bridge{pool: pool, templateBase: templateBase}
}

// NewPoolBridgeFor 指定本桥 region，用于选号时拒绝跨区账号。
func NewPoolBridgeFor(region account.Region, pool *Pool, templateBase map[string]interface{}) *Bridge {
	return &Bridge{pool: pool, templateBase: templateBase, region: region}
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

// failSlot 处理一次上游失败：按失败归类决定写账号级冷却、模型级冷却，
// 还是只记熔断 / 降权计数。
//
// 分类与「是否换号」共用一套判定（ClassifyFail），避免出现
// 「不换号却把整号冷却」的矛盾——那会白白拖死同账号的其他模型。
func (b *Bridge) failSlot(id, model string, err error) {
	if b.pool == nil {
		return
	}
	class := ClassifyFail(err)
	switch class {
	case FailClassNone:
		// 内容审核 / 参数错：换号无意义，不动账号状态
		return
	case FailClassTokenExpired:
		// 凭证过期：等冷却毫无意义（重试多少次都不会成功）。
		// 标记 needs_reauth 让控制台明确提示用户重新授权，并让该账号
		// 暂停参与选号——但不是永久踢出，重新授权后自动恢复。
		b.pool.MarkNeedsReauth(id, "credential expired: "+err.Error())
		// 持久化到账号文件：凭证过期必须跨重启保留，否则重启后账号会
		// 立刻参与选号并再次失败。
		if err := account.MarkNeedsReauth(id); err != nil {
			logger.Error("persist needs_reauth for account %s: %v", id, err)
		}
		return
	case FailClassQuotaExhausted:
		// 额度耗尽：长冷却 + 换号（换一个账号可能还有额度）。
		// 冷却时长取「等到额度重置」的量级，不该 30 分钟就醒过来再撞额度墙。
		b.pool.Cool(id, CooldownQuotaExhausted, err.Error())
		return
	case FailClassTrialRestricted:
		// 试用受限：长冷却 + 换号。
		b.pool.Cool(id, CooldownTrialRestricted, err.Error())
		return
	case FailClassModelNotAllowed:
		// 模型不在套餐内：换号无意义（同账号换号还是这个账号）。
		// 交给上层把明确提示返回给客户端，不冷却账号。
		return
	case FailClassModel:
		if modelCooldownEnabled {
			// 模型级失败（429 / 排队）：只冷却该模型，同账号其他模型不受影响
			b.pool.CoolModel(id, model, CooldownModelFor(err), err.Error())
		} else {
			// 开关关闭：回退到旧的整号冷却行为（排障用）
			b.pool.Cool(id, CooldownFor(err), err.Error())
		}
	case FailClassHard:
		// 账号级硬失败：整号冷却
		b.pool.Cool(id, CooldownFor(err), err.Error())
	case FailClassUnknown:
		// 原因不明的失败：不确定账号是否坏了，先做短暂冷却再降权
		b.pool.Cool(id, CooldownTransient, err.Error())
	}
	// 熔断 / 降权计数：硬失败与未知失败分别累计。
	// 模型级失败（429）也计入硬失败——它说明该账号在这个模型上确实被拒了。
	// 凭证类失败不计数：它们已由各自的专属机制处置，再叠加熔断计数
	// 会让「需重新授权」的账号在重新授权后仍背着熔断包袱。
	if breakerDegradeEnabled {
		b.pool.NoteFailure(id, class)
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
	// 模型目录端点也走候选故障转移：目录拉不到会直接影响按模型路由。
	candidates := qoderModelListCandidates(slot.Region)
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no model list endpoint for region %s", slot.Region)
	}
	resp, err := slot.client.callGetCandidates("model_list", candidates)
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

// pickSlotForModel 按模型选号：优先选目录里有该模型的同 region 账号。
// 兜底同样限定同 region——跨 region 会把请求派给别区账号并扣其 credits
// （实测 8963 曾把 CN 请求派给 global 账号产生扣费）。
//
// 模型维度同时参与可用性判定：该模型处于模型级冷却的账号会被跳过，
// 但同账号的其他模型仍可用。
func (b *Bridge) pickSlotForModel(model string, exclude map[string]struct{}) (*Slot, bool) {
	// 本桥期望的区域：Bridge.region（单账号模式天然有值）；
	// 池模式若 region 为空则回退到池内首个 slot。
	wantRegion := b.region
	if wantRegion == "" && b.pool != nil {
		b.pool.mu.Lock()
		for _, s := range b.pool.slots {
			wantRegion = s.Region
			break
		}
		b.pool.mu.Unlock()
	}
	// 防御性过滤：即使池被误构建为跨区，也不选别区账号
	// （跨区会扣到用户根本没启动的那侧账号的 credits）。
	sameRegion := func(s *Slot) bool { return wantRegion == "" || s.Region == wantRegion }

	if b.pool == nil {
		return b.pickSlot(exclude)
	}
	slot, ok := b.pool.PickForModel(exclude, model, func(s *Slot) bool {
		if !sameRegion(s) {
			return false
		}
		b.ensureCatalog(s)
		return b.slotSupportsModel(s, model)
	})
	if ok {
		return slot, true
	}
	// 池内同 region 账号目录均不含该模型：仍在同 region 内兜底。
	return b.pool.PickForModel(exclude, model, sameRegion)
}
