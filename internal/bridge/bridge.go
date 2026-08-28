package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"qoder2api/internal/cosy"
	"sort"
	"strings"

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
	// DEBUG: dump raw userinfo to understand actual response structure
	logger.Info("userinfo raw (%d bytes): %s", len(raw), string(raw))
	return result, nil
}

type Bridge struct {
	sess         *cosy.SessionContext
	client       *BearerClient
	region       account.Region
	templateBase map[string]interface{}
}

// QoderModel 是返回给前端的精简模型条目（仅保留下拉选择必要字段）
type QoderModel struct {
	Key            string  `json:"key"`
	DisplayName    string  `json:"display_name"`
	Enable         bool    `json:"enable"`
	IsDefault      bool    `json:"is_default"`
	IsReasoning    bool    `json:"is_reasoning,omitempty"`
	ContextWindow  int     `json:"context_window,omitempty"`
	MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
	MaxInputTokens int     `json:"max_input_tokens,omitempty"`
	PriceFactor    float64 `json:"price_factor,omitempty"`
}

// NewBridge 创建 API 转换桥接。
// 支持两种认证方式：
//  1. OAuth device token (dt-xxx): 直接使用，调用 /api/v1/userinfo 获取用户信息
//  2. Personal Access Token (PAT): 调用 ExchangeJobToken 转换为 session token
func NewBridge(pat string, region account.Region, templateBase map[string]interface{}) (*Bridge, error) {
	mid := cosy.NewUUID()
	mtoken := cosy.NewBase64Token()
	mtype := cosy.NewHexToken(18)

	logger.Info("Bridge using token: %s (prefix: %s)", pat[:10]+"...", pat[:4])

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
		jt, err := cosy.ExchangeJobToken(pat, mid, mtoken, mtype, account.GetEndpoints(region).JobTokenURL)
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
	sess, err := cosy.NewSession(identity, mid, mtoken, mtype)
	if err != nil {
		return nil, err
	}
	client := NewBearerClient(sess)

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
	modelListURL := qoderModelListURL(b.region)
	resp, err := b.client.callGet(modelListURL)
	if err != nil {
		return nil, err
	}
	// debug: 打印响应顶层 key
	keys := make([]string, 0, len(resp))
	for k := range resp {
		keys = append(keys, k)
	}
	logger.Info("model list response keys: %v (region=%s)", keys, b.region)
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
			var defaultCfg map[string]interface{}
			for _, cfg := range cc {
				cfgMap, _ := cfg.(map[string]interface{})
				if cfgMap == nil {
					continue
				}
				isDefault, _ := cfgMap["is_default"].(bool)
				if isDefault {
					defaultCfg = cfgMap
					break
				}
			}
			if defaultCfg == nil {
				for _, cfg := range cc {
					cfgMap, _ := cfg.(map[string]interface{})
					if cfgMap != nil {
						defaultCfg = cfgMap
						break
					}
				}
			}
			if defaultCfg != nil {
				model.ContextWindow = int(cosy.FloatVal(defaultCfg, "token_count"))
			}
		}
		if model.ContextWindow == 0 {
			model.ContextWindow = model.MaxInputTokens
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
}

func (b *Bridge) CallQoder(ctx context.Context, agent string, messages []interface{}, model string, tools interface{}, onDelta func(Delta)) error {
	return b.CallQoderWithOpts(ctx, agent, messages, model, tools, CallOpts{}, onDelta)
}

func (b *Bridge) CallQoderWithOpts(ctx context.Context, agent string, messages []interface{}, model string, tools interface{}, opts CallOpts, onDelta func(Delta)) error {
	// 将客户端模型名（claude-sonnet-4-6 等）映射成 Qoder 上游内部 model.key（auto/ultimate/performance/lite/efficient）。
	// 上游对未知 key 会走兜底返回内容，但不会把这次调用计入 quota，这是「请求成功但 dashboard 无用量」的根因。
	originalModel := model
	model = MapModel(agent, model)
	if model != originalModel {
		logger.Debug("mapModel %s/%s -> %s", agent, originalModel, model)
	}
	body := DeepCopyMap(b.templateBase)

	nid := cosy.NewUUID()
	body["request_id"] = nid
	body["chat_record_id"] = nid
	body["request_set_id"] = cosy.NewUUID()
	body["session_id"] = cosy.NewUUID()
	body["stream"] = true
	body["aliyun_user_type"] = b.sess.Identity.UserType

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

	qurl := qoderChatStreamURL(b.region)
	extra := map[string]string{
		"x-model-key":    model,
		"x-model-source": mcSource,
	}

	preview := prompt
	if len(preview) > 80 {
		preview = preview[:80] + "..."
	}
	logger.Info("callQoder model=%s prompt=%s", model, preview)
	logger.Debug("callQoder request body: %s", func() string { d, _ := json.Marshal(body); return string(d) }())

	var upstreamErr error
	streamErr := b.client.openStreamLines(ctx, qurl, body, extra, func(line string) {
		if !strings.HasPrefix(line, "data:") {
			return
		}
		dataPayload := strings.TrimSpace(line[5:])
		if dataPayload == "[DONE]" {
			return
		}
		delta := ExtractDelta(dataPayload)
		if delta.Err != nil {
			upstreamErr = delta.Err
			return
		}
		if !delta.isEmpty() {
			onDelta(delta)
		}
	})
	if upstreamErr != nil {
		return upstreamErr
	}
	return streamErr
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
// Qoder 上游合法 key 仅 auto/ultimate/performance/lite/efficient（见 cmd/fetchmodels/models_result.json），
// GPT / Gemini 没有专属 SKU，统一映射到主力档 performance —— 既不浪费 ultimate 高 price_factor，
// 也不被 lite 限频；想分档（如 gpt-5→ultimate / gpt-5-mini→efficient）请在 UI 配置。
var defaultModelMapping = map[string]string{
	// Claude 三档
	"opus":   "ultimate",
	"sonnet": "performance",
	"haiku":  "lite",
	// 非 Claude 家族兜底
	"gpt":    "performance",
	"gemini": "performance",
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
