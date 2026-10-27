package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"qoder2api/internal/cosy"
	"sort"
	"strings"
	"sync"
	"time"

	"qoder2api/account"
	"qoder2api/logger"
)

func qoderChatStreamURL(region account.Region) string {
	return account.GetEndpoints(region).ChatStreamURL
}

func qoderModelListURL(region account.Region) string {
	return account.GetEndpoints(region).ModelListURL
}

func ParseOAuthSecret(secret string) (deviceToken, refreshToken string) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return "", ""
	}
	if strings.HasPrefix(secret, "dt-") || strings.HasPrefix(secret, "drt-") {
		return secret, ""
	}
	var payload struct {
		DeviceToken  string `json:"device_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal([]byte(secret), &payload); err != nil {
		return secret, ""
	}
	return payload.DeviceToken, payload.RefreshToken
}

func ResolveOAuthUserID(userInfo map[string]interface{}) string {
	if id := StrVal(userInfo, "id"); id != "" {
		return id
	}
	if id := StrVal(userInfo, "userId"); id != "" {
		return id
	}
	return StrVal(userInfo, "uid")
}

func FetchUserInfoWithToken(token string, region account.Region) (map[string]interface{}, error) {
	ep := account.GetEndpoints(region)
	req, _ := http.NewRequest("GET", ep.UserinfoBase, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result map[string]interface{}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	// DEBUG 用途排查原始 userinfo 响应结构：降为 Debug 级并截断至 500 字节，
	// 默认日志级别不再落盘用户名/组织等 PII，保留低级别排查能力
	logger.Debug("userinfo raw (%d bytes): %s", len(raw), truncate(string(raw), 500))
	return result, nil
}

type Bridge struct {
	sess         *cosy.SessionContext
	client       *BearerClient
	region       account.Region
	templateBase map[string]interface{}
	// pool 非空时启用多账号轮询 + 失败换号；为空则退化为单账号（sess/client/region）。
	pool *Pool
	// chatStreamURL 测试注入用：非空时覆盖 region 对应的上游 chat 流地址
	chatStreamURL string

	// 模型目录缓存（context 档位解析用）
	catalogCacheMu sync.Mutex
	catalogCache   []QoderModel
	catalogCacheAt time.Time
}

// QoderModel 是返回给前端的精简模型条目（仅保留下拉选择必要字段）
type QoderModel struct {
	Key             string  `json:"key"`
	DisplayName     string  `json:"display_name"`
	Enable          bool    `json:"enable"`
	IsDefault       bool    `json:"is_default"`
	IsReasoning     bool    `json:"is_reasoning,omitempty"`
	ContextWindow   int     `json:"context_window,omitempty"`
	MaxOutputTokens int     `json:"max_output_tokens,omitempty"`
	MaxInputTokens  int     `json:"max_input_tokens,omitempty"`
	PriceFactor     float64 `json:"price_factor,omitempty"`
	// ContextTiers 上游 context_config 的全部档位（如 200K/400K/1M）。
	// 上游按模型提供多档 context，App 端可切换；此前只取 is_default 档，
	// 导致大 context 档位丢失。
	ContextTiers []ContextTier `json:"context_tiers,omitempty"`
}

// ContextTier 单个 context 档位。
type ContextTier struct {
	Name      string `json:"name"`       // 档位名，如 "200K" / "400K" / "1M"
	Tokens    int    `json:"tokens"`     // token 数
	IsDefault bool   `json:"is_default"` // 是否上游默认档
}

// tokenPrefix 安全截取 token 前 n 字节用于日志展示（不足 n 返回全串）。
// 防止用户误粘贴短 token 时 pat[:10] 之类切片越界 panic；日志只输出前缀不输出全文。
func tokenPrefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// NewBridge 创建 API 转换桥接。
// 支持两种认证方式：
//  1. OAuth device token (dt-xxx): 直接使用，调用 /api/v1/userinfo 获取用户信息
//  2. Personal Access Token (PAT): 调用 ExchangeJobToken 转换为 session token
func NewBridge(pat string, region account.Region, templateBase map[string]interface{}) (*Bridge, error) {
	return NewBridgeWithFP(pat, region, templateBase, nil)
}

// NewBridgeWithFP 与 NewBridge 相同，但可传入账号级真实设备指纹；
// fp 非空时该账号使用新版推理协议（model/list 才会额外下发
// fast / smodel(Sonus) / cmodel(Cantus)，且推理能通过设备校验）。
func NewBridgeWithFP(pat string, region account.Region, templateBase map[string]interface{}, fp *account.CheckinFingerprint) (*Bridge, error) {
	logger.Info("Bridge using token: %s (prefix: %s)", tokenPrefix(pat, 10), tokenPrefix(pat, 4))

	var identity cosy.AuthIdentity
	var name, id string

	deviceToken, refreshToken := ParseOAuthSecret(pat)
	if strings.HasPrefix(deviceToken, "dt-") {
		userInfo, err := FetchUserInfoWithToken(deviceToken, region)
		if err != nil {
			return nil, fmt.Errorf("fetch user info: %w", err)
		}
		name = StrVal(userInfo, "name")
		id = ResolveOAuthUserID(userInfo)
		identity = cosy.AuthIdentity{
			Name:               name,
			Aid:                id,
			Uid:                id,
			OrganizationId:     StrVal(userInfo, "organization_id"),
			OrganizationName:   StrVal(userInfo, "organization_name"),
			UserType:           StrValDefault(userInfo, "userType", "personal_standard"),
			SecurityOauthToken: deviceToken,
			RefreshToken:       refreshToken,
		}
	} else {
		// jobToken 交换发生在拿到 uid 之前：机器头用凭证种子稳定派生
		// （hub 的交换请求不带机器头，此处保留 QCCG 原有行为但消除随机漂移）
		seed := cosy.FingerprintSeed("", pat)
		jt, err := cosy.ExchangeJobToken(pat, cosy.DeriveMachineID(seed), cosy.DeriveMachineToken(seed), cosy.DeriveMachineType(seed), account.GetEndpoints(region).JobTokenURL)
		if err != nil {
			return nil, fmt.Errorf("exchangeJobToken: %w", err)
		}
		name = StrVal(jt, "name")
		id = StrVal(jt, "id")
		identity = cosy.AuthIdentity{
			Name:               name,
			Aid:                id,
			Uid:                id,
			UserType:           StrValDefault(jt, "userType", "personal_standard"),
			SecurityOauthToken: StrVal(jt, "securityOauthToken"),
			RefreshToken:       StrVal(jt, "refreshToken"),
		}
	}

	logger.Info("Bridge session for %s (%s)", name, id)
	// 稳定设备指纹：优先 uid 派生（双区统一、重启不变），uid 缺失退回凭证种子
	seed := cosy.FingerprintSeed(id, pat)
	mid := cosy.DeriveMachineID(seed)
	mtoken := cosy.DeriveMachineToken(seed)
	mtype := cosy.DeriveMachineType(seed)
	logger.Info("Bridge fingerprint machineid=%s (stable derive)", mid)
	sess, err := cosy.NewSession(identity, mid, mtoken, mtype)
	if err != nil {
		return nil, err
	}
	var client *BearerClient
	if fp != nil && fp.MachineToken != "" {
		// 真实指纹：走新版协议；COSY 会话本身仍用派生身份签名
		client = NewBearerClientWithFP(sess, fp)
		logger.Info("Bridge modern protocol enabled (clienttype=%s version=%s)", orDefault(fp.ClientType, "10"), orDefault(fp.Version, "1.1.69"))
	} else {
		client = NewBearerClient(sess)
	}

	return &Bridge{
		sess:         sess,
		client:       client,
		region:       region,
		templateBase: templateBase,
	}, nil
}

// ListAvailableModels 通过 cosy 签名调用 /algo/api/v2/model/list 拉取上游模型清单。
// 返回顶层 assistant 数组中 enable=true 的模型，按 is_default desc + display_name asc 排序。
func (b *Bridge) ListAvailableModels() ([]QoderModel, error) {
	slot, ok := b.pickSlot(nil)
	if !ok {
		return nil, ErrNoAccount
	}
	modelListURL := qoderModelListURL(slot.Region)
	resp, err := slot.client.callGet(modelListURL)
	if err != nil {
		return nil, err
	}
	// debug: 打印响应顶层 key
	keys := make([]string, 0, len(resp))
	for k := range resp {
		keys = append(keys, k)
	}
	logger.Info("model list response keys: %v (region=%s)", keys, slot.Region)
	models := parseQoderModels(resp)
	if len(models) == 0 {
		return nil, fmt.Errorf("%s -> empty model list, keys=%v", modelListURL, keys)
	}
	return models, nil
}

func parseQoderModels(resp map[string]interface{}) []QoderModel {
	// 按优先级尝试多个分类
	categories := []string{"assistant", "developer", "chat"}
	for _, cat := range categories {
		rawList, _ := resp[cat].([]interface{})
		out := extractModels(rawList)
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

func extractModels(rawList []interface{}) []QoderModel {
	out := make([]QoderModel, 0, len(rawList))
	for _, it := range rawList {
		m, ok := it.(map[string]interface{})
		if !ok {
			continue
		}
		enable, _ := m["enable"].(bool)
		model := QoderModel{
			Key:            StrVal(m, "key"),
			DisplayName:    StrVal(m, "display_name"),
			Enable:         enable,
			IsDefault:      func() bool { v, _ := m["is_default"].(bool); return v }(),
			IsReasoning:    func() bool { v, _ := m["is_reasoning"].(bool); return v }(),
			MaxInputTokens: int(cosy.FloatVal(m, "max_input_tokens")),
			PriceFactor:    cosy.FloatVal(m, "price_factor"),
		}
		if cc, ok := m["context_config"].(map[string]interface{}); ok {
			// 保留全部档位（上游按模型提供 200K/400K/1M 等多档）
			for tierName, cfg := range cc {
				cfgMap, _ := cfg.(map[string]interface{})
				if cfgMap == nil {
					continue
				}
				tokens := int(cosy.FloatVal(cfgMap, "token_count"))
				if tokens <= 0 {
					continue
				}
				isDefault, _ := cfgMap["is_default"].(bool)
				model.ContextTiers = append(model.ContextTiers, ContextTier{
					Name:      tierName,
					Tokens:    tokens,
					IsDefault: isDefault,
				})
			}
			// 按 token 数升序，展示与选择更直观
			sort.Slice(model.ContextTiers, func(i, j int) bool {
				return model.ContextTiers[i].Tokens < model.ContextTiers[j].Tokens
			})
			// 默认 context_window 仍取上游默认档（无默认档时取最大档）
			for _, t := range model.ContextTiers {
				if t.IsDefault {
					model.ContextWindow = t.Tokens
					break
				}
			}
			if model.ContextWindow == 0 && len(model.ContextTiers) > 0 {
				model.ContextWindow = model.ContextTiers[len(model.ContextTiers)-1].Tokens
			}
		}
		if model.ContextWindow == 0 {
			model.ContextWindow = model.MaxInputTokens
		}
		if model.IsReasoning {
			model.MaxOutputTokens = 32768
		} else {
			model.MaxOutputTokens = 16384
		}
		out = append(out, model)
	}
	return out
}

// DeepCopyMap does a JSON round-trip deep copy.
func DeepCopyMap(m map[string]interface{}) map[string]interface{} {
	data, _ := json.Marshal(m)
	var out map[string]interface{}
	json.Unmarshal(data, &out)
	return out
}

// CallOpts 控制单次 CallQoder 的可选行为。
type CallOpts struct {
	IsReasoning bool // 是否启用推理模式（对应上游 model_config.is_reasoning）
	MaxTokens   int  // 客户端请求的 max_tokens，0 表示使用模板默认值
	// ContextTokens 指定的 context 档位 token 数（如 400000）。
	// 0 表示使用上游默认档。非 0 时写入 model_config.max_input_tokens。
	//
	// ⚠️ 该字段的传递机制【未证实】：上游 model/list 返回 context_config
	// 多档位（200K/400K/1M），但尚未抓到真实模型请求以确认 App 端
	// 用哪个字段传递所选档位。当前按最可能的 max_input_tokens 实现，
	// 需实测验证。
	ContextTokens int
}

func (b *Bridge) CallQoder(ctx context.Context, agent string, messages []interface{}, model string, tools interface{}, onDelta func(Delta)) error {
	return b.CallQoderWithOpts(ctx, agent, messages, model, tools, CallOpts{}, onDelta)
}

// deltaDispatcher 把上游 SSE 行分发为 Delta 回调，返回值供 openStreamLines
// 的 onLine 使用：false 表示停止读取上游流。
// 约束（review P1）：一旦捕获到流内业务错误（*upstreamErr 非 nil），
// 立即停止向 onDelta 分发任何后续 delta，且从置位错误的那一行起返回 false，
// 让 openStreamLines 及时退出——上游发出错误帧后若不关流，否则重开闸门
// 永不触发、请求挂起；错误由 CallQoder 在流结束后统一返回。
func deltaDispatcher(onDelta func(Delta), upstreamErr *error) func(string) bool {
	return func(line string) bool {
		if *upstreamErr != nil {
			return false
		}
		if !strings.HasPrefix(line, "data:") {
			return true
		}
		dataPayload := strings.TrimSpace(line[5:])
		if dataPayload == "[DONE]" {
			return true
		}
		delta := ExtractDelta(dataPayload)
		if delta.Err != nil {
			*upstreamErr = delta.Err
			return false
		}
		if !delta.isEmpty() {
			onDelta(delta)
		}
		return true
	}
}

// resolveContextTokens 按 settings 配置解析指定模型的 context 档位 token 数。
// 返回 0 表示使用上游默认档。
//
// 优先级：按模型覆盖 > 全局默认 > 0（上游默认）。
func (b *Bridge) resolveContextTokens(model string) int {
	settings, err := account.LoadSettings()
	if err != nil || settings == nil {
		return 0
	}
	tierName := ""
	if settings.ContextTierOverrides != nil {
		tierName = settings.ContextTierOverrides[model]
	}
	if tierName == "" {
		tierName = settings.ContextTierDefault
	}
	if tierName == "" {
		return 0
	}

	// 用模型目录把档位名换成 token 数
	tokens := b.contextTierTokens(model, tierName)
	if tokens == 0 {
		// 目录里没有该档位（模型不支持）：回落到上游默认档
		logger.Debug("context tier %q not available for model %s, using upstream default", tierName, model)
		return 0
	}
	logger.Debug("context tier resolved: model=%s tier=%s tokens=%d", model, tierName, tokens)
	return tokens
}

// contextTierTokens 从模型目录中查找指定档位的 token 数；找不到返回 0。
// 目录带 5 分钟缓存，避免每次请求都打上游 model/list。
func (b *Bridge) contextTierTokens(model, tierName string) int {
	catalog := b.modelCatalog()
	for _, m := range catalog {
		if !strings.EqualFold(m.Key, model) {
			continue
		}
		for _, t := range m.ContextTiers {
			if strings.EqualFold(t.Name, tierName) {
				return t.Tokens
			}
		}
	}
	return 0
}

// modelCatalog 返回模型目录（带 5 分钟缓存）。
func (b *Bridge) modelCatalog() []QoderModel {
	b.catalogCacheMu.Lock()
	defer b.catalogCacheMu.Unlock()
	if b.catalogCache != nil && time.Since(b.catalogCacheAt) < 5*time.Minute {
		return b.catalogCache
	}
	models, err := b.ListAvailableModels()
	if err != nil || len(models) == 0 {
		return b.catalogCache // 失败时返回旧缓存（可能为 nil）
	}
	b.catalogCache = models
	b.catalogCacheAt = time.Now()
	return models
}

func (b *Bridge) CallQoderWithOpts(ctx context.Context, agent string, messages []interface{}, model string, tools interface{}, opts CallOpts, onDelta func(Delta)) (err error) {
	// 将客户端模型名（claude-sonnet-4-6 等）映射成 Qoder 上游内部 model.key（auto/qmodel_38max/gmodel/dmodel 等）。
	// 上游对未知 key 会走兜底返回内容，但不会把这次调用计入 quota，这是「请求成功但 dashboard 无用量」的根因。
	originalModel := model
	model = MapModel(agent, model)
	if model != originalModel {
		logger.Debug("mapModel %s/%s -> %s", agent, originalModel, model)
	}

	// ---- 结构化调用日志（控制台「模型调用日志」面板）------------------------
	// 记录一次请求最终落到哪个账号、映射后的模型、token 用量、耗时与结果。
	// 与 logger 原始行日志并存：这里按字段拆分，便于可读渲染，不改变原有日志。
	startedAt := time.Now()
	meta := callMetaFrom(ctx)
	rec := CallRecord{
		Time:          startedAt,
		RequestID:     meta.RequestID,
		Protocol:      meta.Protocol,
		Agent:         agent,
		Model:         originalModel,
		UpstreamModel: model,
		Stream:        meta.Stream,
		Messages:      len(messages),
		Tools:         tools != nil,
	}
	defer func() {
		rec.DurationMS = time.Since(startedAt).Milliseconds()
		switch {
		case err == nil:
			rec.Status = CallStatusOK
		case errors.Is(err, context.Canceled):
			rec.Status = CallStatusCancelled
		default:
			rec.Status = CallStatusError
			rec.ErrorType, rec.ErrorMessage = FriendlyError(err)
			rec.ErrorMessage = truncateRunesForLog(rec.ErrorMessage, 240)
		}
		recentCallLog.record(rec)
	}()

	// 包装 delta 回调：顺带累计 token 用量与工具调用标记（不改动调用方逻辑）。
	userOnDelta := onDelta
	onDelta = func(d Delta) {
		if d.InputTokens > 0 {
			rec.InputTokens = d.InputTokens
		}
		if d.OutputTokens > 0 {
			rec.OutputTokens = d.OutputTokens
		}
		if d.CachedTokens > 0 {
			rec.CachedTokens = d.CachedTokens
		}
		if d.ReasoningTokens > 0 {
			rec.ReasoningTokens = d.ReasoningTokens
		}
		if d.Credits > 0 {
			rec.Credits = d.Credits
		}
		if d.OriginalCredits > 0 {
			rec.OriginalCredits = d.OriginalCredits
		}
		if d.ToolCalls != nil {
			rec.HadToolCalls = true
		}
		if !d.isEmpty() {
			rec.Emitted = true
		}
		userOnDelta(d)
	}

	// 解析 context 档位：opts 未指定时按 settings 的全局/按模型配置选取，
	// 并用该账号的模型目录把档位名换成 token 数。
	if opts.ContextTokens == 0 {
		opts.ContextTokens = b.resolveContextTokens(model)
	}

	body := DeepCopyMap(b.templateBase)

	body["stream"] = true

	if mc, ok := body["model_config"].(map[string]interface{}); ok {
		mc["key"] = model
		if opts.IsReasoning {
			mc["is_reasoning"] = true
		}
	}

	if opts.MaxTokens > 0 {
		if params, ok := body["parameters"].(map[string]interface{}); ok {
			params["max_tokens"] = opts.MaxTokens
		}
	}

	// 应用 context 档位（见 CallOpts.ContextTokens 的未证实说明）
	if opts.ContextTokens > 0 {
		if mc, ok := body["model_config"].(map[string]interface{}); ok {
			mc["max_input_tokens"] = opts.ContextTokens
			logger.Debug("context tier applied: model=%s max_input_tokens=%d", model, opts.ContextTokens)
		}
	}

	// Extract summary prompt from last user message for chat_context and business.name
	prompt := ""
	for i := len(messages) - 1; i >= 0; i-- {
		if mm, ok := messages[i].(map[string]interface{}); ok && mm["role"] == "user" {
			prompt = NormalizeMessageContent(mm)
			if prompt == "" {
				if contents, ok := mm["contents"].([]interface{}); ok {
					for _, block := range contents {
						if b, ok := block.(map[string]interface{}); ok {
							if t, ok := b["text"].(string); ok {
								prompt = t
								break
							}
						}
					}
				}
			}
			break
		}
	}

	if biz, ok := body["business"].(map[string]interface{}); ok {
		biz["id"] = cosy.NewUUID()
		biz["begin_at"] = cosy.UnixMs()
		if len(prompt) > 30 {
			biz["name"] = prompt[:30]
		} else {
			biz["name"] = prompt
		}
	}

	if cc, ok := body["chat_context"].(map[string]interface{}); ok {
		if txt, ok := cc["text"].(map[string]interface{}); ok {
			txt["text"] = prompt
		}
		if extra, ok := cc["extra"].(map[string]interface{}); ok {
			if oc, ok := extra["originalContent"].(map[string]interface{}); ok {
				oc["text"] = prompt
			}
		}
	}

	body["messages"] = messages
	if tools != nil {
		body["tools"] = tools
	}

	mcSource := "system"
	if mc, ok := body["model_config"].(map[string]interface{}); ok {
		if s, ok := mc["source"].(string); ok {
			mcSource = s
		}
	}

	extra := map[string]string{
		"x-model-key":    model,
		"x-model-source": mcSource,
	}

	preview := prompt
	if len(prompt) > 80 {
		preview = preview[:80] + "..."
	}
	rec.PromptPreview = truncateRunesForLog(prompt, 160)
	logger.Info("callQoder region=%s model=%s prompt=%s", b.region, model, preview)

	// ---- 账号池选号 + 失败换号 -------------------------------------------
	// 逐个尝试池内账号：某账号失败且「尚未向客户端发出任何内容」、且错误属于
	// 可换号类（额度/频控/瞬时），就冷却该账号并换下一个；已发出内容或
	// 客户端参数错/内容审核则立即上抛——避免重复内容、避免无意义换号。
	tried := make(map[string]struct{})
	var lastErr error
	for {
		slot, ok := b.pickSlotForModel(model, tried)
		if !ok {
			break
		}
		// 记录本次实际使用的账号（含 region），便于对账与排查
		// 「请求是否路由到了未启动的那一侧账号」类问题。
		logger.Info("callQoder: using account=%s id=%s region=%s model=%s",
			slot.Name, slot.ID, slot.Region, model)
		// 调用日志：以「最后一次尝试」的账号为准（成功即实际使用账号；
		// 全部失败时即最后撞上的账号），Attempts 反映是否发生换号。
		rec.Attempts++
		rec.Account = slot.Name
		rec.AccountID = slot.ID
		rec.Region = string(slot.Region)
		// 每个账号用全新的请求 id / session，避免跨账号复用同一标识
		nid := cosy.NewUUID()
		body["request_id"] = nid
		body["chat_record_id"] = nid
		body["request_set_id"] = cosy.NewUUID()
		body["session_id"] = cosy.NewUUID()
		body["aliyun_user_type"] = slot.sess.Identity.UserType
		if biz, ok := body["business"].(map[string]interface{}); ok {
			biz["id"] = cosy.NewUUID()
			biz["begin_at"] = cosy.UnixMs()
		}

		qurl := b.chatStreamURL
		if qurl == "" {
			qurl = qoderChatStreamURL(slot.Region)
		}
		logger.Debug("callQoder request body: %s", func() string { d, _ := json.Marshal(body); return string(d) }())

		emitted, err := b.streamWithSlot(ctx, slot, qurl, body, extra, onDelta)
		if err == nil {
			b.noteSlotSuccess(slot.ID)
			return nil
		}
		lastErr = err
		if emitted || !ShouldFailover(err) {
			return err
		}
		b.coolSlot(slot.ID, CooldownFor(err), err.Error())
		tried[slot.ID] = struct{}{}
		logger.Info("callQoder: account=%s id=%s region=%s failed, failing over to next account: %v",
			slot.Name, slot.ID, slot.Region, err)
	}
	if lastErr == nil {
		lastErr = ErrNoAccount
	}
	return lastErr
}

// streamWithSlot 在单个账号上执行流式请求，含两层有界重试：
//  1. 队列拥塞重试（本层）：上游 10605 isQueued 时，同账号等待 retryAfterSeconds
//     后重试，最多 QueueMaxRetries 次。换号无益（同模型同队列），故不在此层换号。
//  2. 瞬时故障重试（streamWithTransientRetry）：连接/418/5xx，最多 TransientMaxRetries 次。
//
// 返回是否已向 onDelta 发出过内容——上层据此决定能否换号（已发出内容则不能，
// 否则客户端会收到重复内容）。
func (b *Bridge) streamWithSlot(ctx context.Context, slot *Slot, qurl string, body map[string]interface{}, extra map[string]string, onDelta func(Delta)) (bool, error) {
	model := modelKeyFromBody(body)
	queueTries := 0
	totalWait := time.Duration(0)
	// finish 记录本次排队片段（仅当确实发生过排队）；outcome 见 WaitOutcome*。
	finish := func(outcome string, emitted bool, err error) (bool, error) {
		if queueTries > 0 {
			RecordWait(model, slot.Name, string(slot.Region), outcome, queueTries, totalWait)
		}
		return emitted, err
	}
	for {
		emitted, err := b.streamWithTransientRetry(ctx, slot, qurl, body, extra, onDelta)
		if err == nil {
			return finish(WaitOutcomeSuccess, emitted, nil)
		}
		// 队列拥塞且尚未发出内容：同账号等待重试（不换号——同模型同队列）
		if !emitted && isUpstreamQueue(err) {
			// 封顶判定：开关 ON=首字前一直等（仅 ctx 取消停止）；OFF=按 QueueMaxRetries
			if !QueueWaitFirstToken && queueTries >= QueueMaxRetries {
				logger.Info("upstream queued, gave up after %d tries (~%s): %v", queueTries, totalWait, err)
				return finish(WaitOutcomeGaveUp, emitted, err)
			}
			wait := queueRetryAfter(err)
			logger.Info("upstream queued (model congested), waiting %s before retry on same account (try %d, waited ~%s): %v",
				wait, queueTries+1, totalWait, err)
			if serr := sleepCtx(ctx, wait); serr != nil {
				// ctx 取消/超时：上抛最后一次错误
				return finish(WaitOutcomeCancelled, emitted, err)
			}
			totalWait += wait
			queueTries++
			continue
		}
		// 非队列错误：若之前排过队，按 error 记录
		return finish(WaitOutcomeError, emitted, err)
	}
}

// modelKeyFromBody 从请求体取出模型 key（model_config.key），用于等待记录。
func modelKeyFromBody(body map[string]interface{}) string {
	if mc, ok := body["model_config"].(map[string]interface{}); ok {
		if k, ok := mc["key"].(string); ok {
			return k
		}
	}
	return ""
}

// streamWithTransientRetry 在单个账号上执行「SSE 信封重试闸门」。
// 连接阶段错误已由 openStreamLines 内部重试；此处是第二层防护（有界：
// 最多 TransientMaxRetries 次），主要覆盖上游 HTTP200 建流后才在信封里
// 投递 418/5xx 的形态。返回是否已向 onDelta 发出过内容。
func (b *Bridge) streamWithTransientRetry(ctx context.Context, slot *Slot, qurl string, body map[string]interface{}, extra map[string]string, onDelta func(Delta)) (bool, error) {
	var lastErr error
	emittedAny := false
	for attempt := 0; attempt <= TransientMaxRetries; attempt++ {
		if attempt > 0 {
			if serr := sleepCtx(ctx, RetryBackoff(attempt)); serr != nil {
				return emittedAny, lastErr
			}
			logger.Info("reopen upstream after in-stream error (try %d/%d): %v",
				attempt+1, TransientMaxRetries+1, lastErr)
		}
		emitted := false
		var upstreamErr error
		onDeltaWrapped := func(d Delta) {
			if !d.isEmpty() {
				emitted = true
				emittedAny = true
			}
			onDelta(d)
		}
		streamErr := slot.client.openStreamLines(ctx, qurl, body, extra,
			deltaDispatcher(onDeltaWrapped, &upstreamErr))
		if upstreamErr == nil && streamErr == nil {
			// 上游正常关流但未发出任何有效 delta/usage 帧：对齐 hub
			// "empty upstream stream" 显式报错（不纳入重开闸门）。
			if !emitted {
				return emittedAny, ErrEmptyStream
			}
			return emittedAny, nil
		}
		if upstreamErr != nil {
			lastErr = upstreamErr
		} else {
			lastErr = streamErr
		}
		if emitted || attempt >= TransientMaxRetries || !isRetryableStreamError(lastErr) {
			return emittedAny, lastErr
		}
	}
	return emittedAny, lastErr
}

// isRetryableStreamError 判定流内/连接错误是否值得重开上游：
// 结构化上游错误按瞬时分类判定；裸传输错误（TLS EOF 等）按传输层判定；
// ctx 取消、业务错误、内容审核等一律不可重试。
func isRetryableStreamError(err error) bool {
	if err == nil {
		return false
	}
	var ue *UpstreamError
	if errors.As(err, &ue) {
		return IsTransientUpstream(ue.Status, ue.Detail)
	}
	return IsTransientTransport(err)
}

// RedactRequestBodyJSON 接收原始请求 JSON 字节，深拷贝后把可能含敏感对话内容的字段
// （messages[*].content / system / input / tools[*].description 等）的字符串值替换为
// `<redacted len=N>`，保留 JSON 结构和长度信息便于排查问题，但不泄露用户对话原文。
//
// 设计：
//  1. 只对 string 类型脱敏，结构和数字保留
//  2. 长度阈值：>32 才脱敏（避免把短角色名 "user" 也替换掉）
//  3. 按字段名递归：content/text/system/input/instructions/prompt/description
//  4. 失败时退回原始内容（脱敏只是日志辅助，不能影响主流程）
func RedactRequestBodyJSON(raw []byte) string {
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	redactValue(v, "")
	out, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(out)
}

// sensitiveFieldNames 命中后其字符串子值会被脱敏（递归进数组/对象继续处理）
var sensitiveFieldNames = map[string]bool{
	"content":      true,
	"text":         true,
	"system":       true,
	"input":        true,
	"instructions": true,
	"prompt":       true,
	"description":  true,
}

func redactValue(v interface{}, parentField string) {
	switch x := v.(type) {
	case map[string]interface{}:
		for k, child := range x {
			if s, ok := child.(string); ok {
				if (sensitiveFieldNames[k] || sensitiveFieldNames[parentField]) && len(s) > 32 {
					x[k] = fmt.Sprintf("<redacted len=%d>", len(s))
				}
				continue
			}
			redactValue(child, k)
		}
	case []interface{}:
		for i, item := range x {
			if s, ok := item.(string); ok {
				if sensitiveFieldNames[parentField] && len(s) > 32 {
					x[i] = fmt.Sprintf("<redacted len=%d>", len(s))
				}
				continue
			}
			redactValue(item, parentField)
		}
	}
}

func StrVal(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func StrValDefault(m map[string]interface{}, key, def string) string {
	if v, ok := m[key].(string); ok && v != "" {
		return v
	}
	return def
}

// InferAgent 根据模型名启发式推断 agent 类型，用于 chat/completions 这种多 agent 共用 endpoint
// 时选择正确的映射桶。模型名命中关键字按 gemini > claude > 默认 codex。
func InferAgent(model string) string {
	low := strings.ToLower(model)
	switch {
	case strings.Contains(low, "gemini"):
		return "gemini"
	case strings.Contains(low, "claude"), strings.Contains(low, "sonnet"), strings.Contains(low, "opus"), strings.Contains(low, "haiku"):
		return "claude"
	default:
		return "codex"
	}
}

// defaultModelMapping 是当用户未在 Settings.ModelMappings 中配置时，bridge 的内置兜底映射。
// 采用「家族关键字 → Qoder model.key」形式，依赖下面 mapModel 的双向 substring 模糊匹配，
// 一条 "sonnet" 即可覆盖 claude-sonnet-4-6 / claude-sonnet-4-20250514 等所有变体。
//
// 设计原则：默认表只负责让请求落到合法 SKU 不出错，差异化由用户在 UI 自行覆盖。
// Qoder 上游合法 key 见 /v1/models 返回，随上游变化动态调整。
// GPT / Gemini 没有专属 SKU，统一映射到主力档 performance —— 既不浪费 ultimate 高 price_factor，
// 也不被 lite 限频；想分档（如 gpt-5→ultimate / gpt-5-mini→efficient）请在 UI 配置。
var defaultModelMapping = map[string]string{
	// Claude 三档
	"opus":   "qmodel_38max",
	"sonnet": "gmodel",
	"haiku":  "qfmodel",
	// 非 Claude 家族兜底
	"gpt":    "dmodel",
	"gemini": "gmodel",
}

// MapModel 解析顺序（参考 ccx 的 RedirectModel 算法）：
//  1. 用户 Settings.ModelMappings[agent] 精确命中
//  2. 用户 Settings.ModelMappings[agent] 双向 substring 模糊匹配（按 source 长度倒序，最长优先）
//  3. 用户旧 Settings.ModelMapping (deprecated 扁平表) 命中
//  4. defaultModelMapping 精确命中
//  5. defaultModelMapping 双向 substring 模糊匹配
//  6. ToLower 兜底（如客户端传 "Performance" 这种 display_name 大小写）
//
// agent 取值 "claude" / "codex" / "gemini"，由 handler 入口决定；空串表示未知 agent，跳过 agent 桶。
//
// 双向 substring 含义：source 包含 model（短关键字匹配长模型名，如 "sonnet" → "claude-sonnet-4-6"）
// 或 model 包含 source（长别名匹配短模型名，反向也能命中）。
func MapModel(agent, model string) string {
	if model == "" {
		return model
	}

	settings, _ := account.LoadSettings()

	// 1. agent 维度的用户配置优先（信任用户配置值，UI 已限制为上游合法 key）
	if settings != nil && agent != "" {
		if table, ok := settings.ModelMappings[agent]; ok && len(table) > 0 {
			if mapped := lookupMapping(model, table); mapped != "" {
				return mapped
			}
		}
	}

	// 2. 兼容旧的扁平 ModelMapping（仅当未配置 agent 桶时生效）
	if settings != nil && len(settings.ModelMapping) > 0 {
		if mapped := lookupMapping(model, settings.ModelMapping); mapped != "" {
			return mapped
		}
	}

	// 3. 内置默认映射兜底
	if mapped := lookupMapping(model, defaultModelMapping); mapped != "" {
		return mapped
	}

	// 4. 大小写归一化兜底（客户端可能直接传 Qoder key 如 "Performance"/"dmodel"）
	return strings.ToLower(model)
}

// lookupMapping 在单张映射表内执行：精确匹配 → 长度倒序 + 双向 substring 模糊匹配。
// 未命中返回空串。
func lookupMapping(model string, table map[string]string) string {
	if len(table) == 0 {
		return ""
	}
	// 1. 精确命中
	if v, ok := table[model]; ok && v != "" {
		return v
	}
	// 2. 长度倒序，确保 "claude-sonnet-4-6" 优先于 "sonnet" 命中
	type kv struct{ src, dst string }
	pairs := make([]kv, 0, len(table))
	for k, v := range table {
		if k == "" || v == "" {
			continue
		}
		pairs = append(pairs, kv{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool { return len(pairs[i].src) > len(pairs[j].src) })
	mLow := strings.ToLower(model)
	for _, p := range pairs {
		sLow := strings.ToLower(p.src)
		if strings.Contains(mLow, sLow) || strings.Contains(sLow, mLow) {
			return p.dst
		}
	}
	return ""
}
