package account

import "time"

type Account struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Email     string     `json:"email,omitempty"`
	UserType  string     `json:"user_type,omitempty"`
	Plan      string     `json:"plan,omitempty"`
	Region    Region     `json:"region,omitempty"` // "global" | "cn"，空值视为 global
	AuthMode  string     `json:"auth_mode"`        // "pat" | "oauth"
	APIMode   string     `json:"api_mode"`         // "openai" | "anthropic" | "gemini" | "claude-code"
	Tags      []string   `json:"tags"`
	CreatedAt time.Time  `json:"created_at"`
	LastUsed  *time.Time `json:"last_used,omitempty"`
	Active    bool       `json:"active"`
	SortOrder int        `json:"sort_order"`

	// CheckinFingerprint 签到专用设备指纹（可选）。
	// 国际版（qoder.com）的签到活动**按真实设备身份下发**：请求必须携带与登录
	// 时协商一致的 cosy 指纹，否则服务端不下发 CLAIM_BENEFIT 活动（表现为
	// no_campaign）。自造指纹无效，需从官方客户端抓包取得。
	// 国内版（cn）反之——发送自造 machinetoken 会被拒，留空即可。
	CheckinFingerprint *CheckinFingerprint `json:"checkin_fingerprint,omitempty"`

	// Proxy 账号绑定的出口代理地址（可选）。
	// 空 = 直连（存量账号默认行为不变）。
	// 只支持 http:// 与 https:// 代理；其他 scheme（如 socks5://）会被忽略
	// 并打 WARN 日志（标准库不支持 SOCKS5，且静默直连等于泄露真实出口）。
	//
	// 也可用环境变量 QD_ACCOUNT_PROXY_<账号ID前8位> 覆盖本字段（便于临时
	// 排障，不必改账号文件）。
	Proxy string `json:"proxy,omitempty"`

	// NeedsReauth 该账号的凭证已失效，需要重新授权。
	//
	// 为什么要有这个标志：上游报「凭证过期」时，冷却等重试是纯浪费
	// （重试多少次都不会成功）。标记出来可以让控制台明确显示「需重新授权」，
	// 并让该账号暂时不参与选号。用户重新登录 / 重新导入凭证后自动清除。
	//
	// 存量账号默认 false（= 正常参与选号），行为不变。
	NeedsReauth bool `json:"needs_reauth,omitempty"`

	// DeviceIdentity 该账号的稳定设备身份（推理请求与签到请求共用同源）。
	//
	// 为什么需要持久化：推理侧的指纹是按账号 uid 派生的，而签到路径拿不到
	// uid（签到不做 userinfo 往返）。没有这份持久化身份，签到只能自己重新
	// 派生一份——那样签到与推理就是「两套设备身份」，上游看到的是同一个账号
	// 在两个客户端上活动，画像立刻断裂。
	//
	// 写入时机：账号入池时（推理侧已算出 uid 派生的结果）。
	// Real=true 表示这份身份来自官方抓包的真实设备指纹。
	DeviceIdentity *DeviceIdentity `json:"device_identity,omitempty"`
}

// DeviceIdentity 账号的稳定设备身份。
//
// 不变量：同一个账号在推理请求与签到请求中必须使用同一份身份（同一个
// machineId / clienttype / version）。两处用不同的身份会让上游判定
// 「同一账号在多台设备上活动」，是最容易被关联的特征之一。
type DeviceIdentity struct {
	MachineID    string `json:"machine_id"`
	MachineToken string `json:"machine_token"`
	MachineType  string `json:"machine_type"`
	ClientType   string `json:"client_type,omitempty"`
	Version      string `json:"version,omitempty"`
	Hostname     string `json:"hostname,omitempty"`
	MachineOS    string `json:"machine_os,omitempty"`
	MachineCode  string `json:"machine_code,omitempty"`
	// Real 为真表示来自官方抓包的真实设备指纹；否则是按账号稳定派生的指纹。
	Real bool `json:"real,omitempty"`
}

// CheckinFingerprint 账号级「真实设备指纹」。
//
// 用途一：国际版（qoder.com）每日签到需要真实 cosy 设备身份，缺了指纹服务端
// 不下发 CLAIM_BENEFIT 活动。
//
// 用途二：启用新版推理协议。实测（2026-10-23）上游按
// (cosy-clienttype, cosy-version) 决定 model/list 下发范围——旧值
// （5 / 1.0.10）只给 12~13 个模型，换成官方桌面端同款的 10 / 1.1.69 后才会
// 额外下发 fast / smodel(Sonus) / cmodel(Cantus)。但推理侧会校验真实设备
// 指纹，自造值会被拒（RESOURCE_LIMIT）。因此配置了指纹的账号走新版协议，
// 未配置的账号保持旧协议不变。
//
// 新版指纹形态（客户端 0.4.8+）：machineId = machineToken = clientIp 为同一
// UUID，machineType 为 "5"。从官方客户端抓包获取，同一账号多次请求稳定。
type CheckinFingerprint struct {
	MachineID    string `json:"machine_id"`
	MachineToken string `json:"machine_token"`
	MachineType  string `json:"machine_type"`
	ClientIP     string `json:"client_ip,omitempty"`
	ClientType   string `json:"client_type,omitempty"` // 空则默认 10
	Version      string `json:"version,omitempty"`     // 空则默认 1.1.69
	MachineOS    string `json:"machine_os,omitempty"`
	Hostname     string `json:"hostname,omitempty"`
	Scene        string `json:"scene,omitempty"`       // 空则 app
	Product      string `json:"product,omitempty"`     // 空则 app
	DataPolicy   string `json:"data_policy,omitempty"` // 空则 agree
	MachineCode  string `json:"machine_code,omitempty"`
}

type OAuthSession struct {
	LoginID  string `json:"login_id"`
	LoginURL string `json:"login_url"`
}

type Status struct {
	Running       bool   `json:"running"`
	Port          int    `json:"port"`
	ActiveAccount string `json:"active_account"`
	APIMode       string `json:"api_mode"`

	// 按 region 的启用账号与运行状态（单控制台双桥）
	CNActive         string `json:"cn_active,omitempty"`
	CNActiveName     string `json:"cn_active_name,omitempty"`
	GlobalActive     string `json:"global_active,omitempty"`
	GlobalActiveName string `json:"global_active_name,omitempty"`
	GlobalRunning    bool   `json:"global_running,omitempty"`
}

type Settings struct {
	Port                 int                          `json:"port"`
	AutoStart            bool                         `json:"auto_start"`
	LogLevel             string                       `json:"log_level"`                        // "info" | "debug" | "error"
	QuotaRefreshInterval int                          `json:"quota_refresh_interval"`           // 秒，0=不自动刷新
	BridgeToken          string                       `json:"bridge_token,omitempty"`           // 自定义鉴权 token，空则使用默认值 "qccg"
	ConsolePassword      string                       `json:"console_password,omitempty"`       // Web 控制台登录密码（类似 CPA management key）
	ModelMapping         map[string]string            `json:"model_mapping,omitempty"`          // [DEPRECATED] 旧扁平映射（向后兼容，MapModel 中作为兜底回退使用）
	ModelMappings        map[string]map[string]string `json:"model_mappings,omitempty"`         // agent (claude/codex/gemini) → 客户端模型名 → Qoder model.key
	AutoCheckin          bool                         `json:"auto_checkin"`                     // 自动签到开关（默认关闭）
	AutoCheckinTimes     []string                     `json:"auto_checkin_times,omitempty"`     // 自动签到时间点，HH:MM 列表，如 ["09:15","17:00"]；空=10:00
	AutoRefreshToken     bool                         `json:"auto_refresh_token"`               // 自动续期 token 开关（默认关闭）；开启后每 15 天用 refresh_token 换新 token
	ContextTierDefault   string                       `json:"context_tier_default,omitempty"`   // 全局默认 context 档位名（如 "400K"）；空=用上游默认档
	ContextTierOverrides map[string]string            `json:"context_tier_overrides,omitempty"` // 按模型 key 覆盖档位：model_key -> 档位名
	MachineSalt          string                       `json:"machine_salt,omitempty"`           // 本机设备指纹盐（首次启动自动生成；勿修改，变更即指纹漂移）
	QueueMaxRetries      int                          `json:"queue_max_retries,omitempty"`      // 模型排队（上游 10605 isQueued）同账号最多重试次数；≤0 用默认 2，范围 1~10
	QueueWaitCapSec      int                          `json:"queue_wait_cap_sec,omitempty"`     // 单次排队等待上限（秒）；≤0 用默认 30，范围 1~300
	QueueWaitDefaultSec  int                          `json:"queue_wait_default_sec,omitempty"` // 上游未给 retryAfter 时的默认等待（秒）；≤0 用默认 15，范围 1~300
	QueueWaitFirstToken  *bool                        `json:"queue_wait_first_token,omitempty"` // 排队时「首字前一直等」；nil=用默认(true)。true=直到出首字或客户端断开才停（忽略重试次数）；false=按 queue_max_retries 封顶
	QueueWaitBudgetSec   int                          `json:"queue_wait_budget_sec,omitempty"`  // 一次请求排队等待的总预算（秒）；≤0 用默认 1800。即使「首字前一直等」也不能超过它——无限挂起会同时占用客户端连接与账号配额

	// 流量闸门（单账号并发上限 + 请求节奏整形，见 internal/bridge/guard.go）。
	// 目的：上游对同账号高并发与固定节奏请求敏感（10605 排队 / 429 频控），
	// 本地限流避免客户端把账号打爆。全部热更新，改完即生效。
	ConcurrencyPerAccount int `json:"concurrency_per_account,omitempty"` // 单账号并发上限；≤0 用默认 1（完全串行，最保守）
	MinRequestGapMs       int `json:"min_request_gap_ms,omitempty"`      // 最小请求启动间隔（毫秒）；≤0 用默认 400（另加 0~120ms 真随机抖动）
	GuardWaitSec          int `json:"guard_wait_sec,omitempty"`          // 闸门等待上限（秒）；≤0 用默认 30。超时先软放行（有上限），超软上限才拒流
	// TrafficGuard 流量闸门总开关（*bool 消除「缺字段 vs 显式 false」歧义）。
	//
	// 行为变更说明（重要）：早期版本是 bool，存量 settings.json 缺该字段时
	// 读到零值 false = 保护关闭——这正是审计报告的 P1-3（「宣称有保护、
	// 实际默认没开」）。改为 *bool 后 nil = 默认 true（保护默认开），
	// 显式 false 才关闭。存量配置若显式写过 false 仍然生效。
	//   - nil（缺字段）→ true：所有账号启用闸门
	//   - false → 仅团队/企业账号启用（个人账号保持旧行为）
	TrafficGuard *bool `json:"traffic_guard,omitempty"`

	// LeaseTTLSec 租约心跳 TTL（秒）：流式请求期间靠心跳续租，只有
	// 「距上次心跳超过 TTL」的租约才按泄漏回收。≤0 用默认 7200（2 小时，
	// 显著长于最长合法流生命周期，作为心跳彻底失效的兜底）。
	LeaseTTLSec int `json:"lease_ttl_sec,omitempty"`

	// ModelCooldown 模型级冷却开关（nil = 默认开启）。
	// 开启后：上游频控（429）与模型排队（10605）只冷却「该账号的该模型」，
	// 不再整号冷却——一个模型被限频不会把同账号的其他模型一起拖死。
	// 显式设为 false 即回退到旧的整号冷却行为（排障用）。
	ModelCooldown *bool `json:"model_cooldown,omitempty"`

	// BreakerDegrade 熔断 / 降权开关（nil = 默认开启）。
	// 开启后：连续硬失败达阈值触发熔断（指数退避，不再反复唤醒账号），
	// 连续未知失败达阈值触发降权（账号排到候选末尾而非直接排除）。
	// 显式设为 false 即关闭，只保留旧的固定冷却。
	BreakerDegrade *bool `json:"breaker_degrade,omitempty"`

	// ---- 签到防风控参数（热更新生效） ----
	//
	// 理由：签到是「非对话类高频行为」，与对话流量画像完全不同。多个账号
	// 在同一秒批量签到是明显的自动化特征，容易导致活动被隐藏（账号看不到
	// 权益，而非直接报错）。
	// 存量账号默认行为不变：间隔与错峰只影响签到节奏，不影响功能。
	CheckinAccountGapMS int   `json:"checkin_account_gap_ms,omitempty"` // 账号之间最小间隔（毫秒）；≤0 用默认 1000
	CheckinJitterMS     int   `json:"checkin_jitter_ms,omitempty"`      // 账号间隔随机抖动上限（毫秒）；≤0 用默认 500
	CheckinStaggerSec   int   `json:"checkin_stagger_sec,omitempty"`    // 签到时间点错峰窗口（秒）；≤0 用默认 60
	CheckinStagger      *bool `json:"checkin_stagger,omitempty"`        // 错峰开关；nil=用默认(true)。false=偏移恒为 0，回到旧行为

	// CheckinFullDeviceHeaders 签到时是否携带完整设备头集合。
	//
	// 默认 false = 沿用当前行为（只带身份标识，不带完整机器头集合）。
	// 之所以做成可配置而不是硬编码：活动平台对「机器头是否成套」的判定与
	// 推理侧不同，某些活动场景带完整设备头反而会被判非官方客户端——
	// 这个行为是可观测、可调整的，不该写死在代码里。
	CheckinFullDeviceHeaders bool `json:"checkin_full_device_headers,omitempty"`

	// 自适应节流（见 internal/bridge/throttle.go）：按风控遥测的排队率 /
	// 频控率自动收紧全局节奏（降并发 / 拉间隔），带迟滞与逐级恢复。
	// 与用户配置的关系：生效并发 = min(用户配置, 自适应上限)，
	// 生效间隔 = max(用户配置, 自适应间隔)——自适应只会更严，不会更松。
	//
	// *bool 语义同 TrafficGuard：nil=默认 true（保护默认开），显式 false 关闭。
	AutoThrottle         *bool   `json:"auto_throttle,omitempty"`           // 总开关；nil=用默认(true)
	ThrottleWindowSec    int     `json:"throttle_window_sec,omitempty"`     // 统计窗口（秒）；≤0 用默认 300，下限 6s（60 桶 × 100ms）
	ThrottleQueueRatePct float64 `json:"throttle_queue_rate_pct,omitempty"` // 排队率降档阈值（百分比）；≤0 用默认 5
	ThrottleRatePct      float64 `json:"throttle_rate_pct,omitempty"`       // 频控率降档阈值（百分比）；≤0 用默认 2
	ThrottleHoldMs       int     `json:"throttle_hold_ms,omitempty"`        // 降档后最小请求间隔（毫秒）；≤0 用默认 1500

	// 请求体积与输出上限（见 internal/bridge/limits.go）：本地提前拦下
	// 超大请求 / 超长工具结果 / 超大 max_tokens——上游对这类请求会直接
	// 断连（客户端只看到 broken pipe）并连锁触发熔断，误伤账号。
	MaxBodyMB            int `json:"max_body_mb,omitempty"`             // 请求体上限（MB）；≤0 用默认 29。超限返回 413，不转发
	ToolResultBytes      int `json:"tool_result_bytes,omitempty"`       // 单条工具结果截断上限（字节）；≤0 用默认 32768
	ToolResultTotalBytes int `json:"tool_result_total_bytes,omitempty"` // 单次请求工具结果总和上限（字节）；≤0 用默认 131072，超出按「旧的先截」
	MaxOutputTokens      int `json:"max_output_tokens,omitempty"`       // max_tokens 上限保护；≤0 用默认 32768。超限下调并打日志（不拒请求）

	// 会话亲和（见 internal/bridge/affinity.go）：同一会话优先粘同一账号。
	// 两个收益：避免「同会话在多号间跳」的异常画像；保住上游前缀缓存
	// （换账号 = 前缀全失效 = 全价积分）。排队等待期间不换号即不换绑定。
	//
	// *bool 语义同 TrafficGuard：nil=默认 true（保护默认开），显式 false 关闭。
	SessionAffinity       *bool `json:"session_affinity,omitempty"`         // 总开关；nil=用默认(true)
	SessionAffinityTTLSec int   `json:"session_affinity_ttl_sec,omitempty"` // 绑定 TTL（秒）；≤0 用默认 1800
}

// EffectiveTrafficGuard 返回流量闸门的有效值；未设置（nil）时默认开启。
//
// 行为变更说明：早期为 bool，存量 settings.json 缺字段 = false（保护关闭），
// 属于「宣称有保护、实际默认没开」的偏差（审计 P1-3）。改为 *bool 后
// nil = true：新保护默认开。显式 false 仍然生效（仅团队/企业账号启用）。
func (s *Settings) EffectiveTrafficGuard() bool {
	if s.TrafficGuard != nil {
		return *s.TrafficGuard
	}
	return true
}

// EffectiveAutoThrottle 返回自适应节流的有效值；未设置（nil）时默认开启。
func (s *Settings) EffectiveAutoThrottle() bool {
	if s.AutoThrottle != nil {
		return *s.AutoThrottle
	}
	return true
}

// EffectiveSessionAffinity 返回会话亲和的有效值；未设置（nil）时默认开启。
func (s *Settings) EffectiveSessionAffinity() bool {
	if s.SessionAffinity != nil {
		return *s.SessionAffinity
	}
	return true
}

// EffectiveModelCooldown 返回模型级冷却的有效值；未设置（nil）时默认开启。
//
// 默认开启的理由：模型级冷却只缩小影响范围、不扩大——旧行为是把整号
// 拖死，新行为是精确隔离到「该账号的该模型」，明确更安全。
func (s *Settings) EffectiveModelCooldown() bool {
	if s.ModelCooldown != nil {
		return *s.ModelCooldown
	}
	return true
}

// EffectiveBreakerDegrade 返回熔断 / 降权的有效值；未设置（nil）时默认开启。
//
// 默认开启的理由：熔断替代的是「冷却 60s → 醒 → 再失败 → 再冷却」的
// 反复唤醒，对账号本身更友好；降权不排除账号，风险可控。
func (s *Settings) EffectiveBreakerDegrade() bool {
	if s.BreakerDegrade != nil {
		return *s.BreakerDegrade
	}
	return true
}

// EffectiveCheckinStagger 返回错峰开关的有效值；未设置（nil）时默认开启。
func (s *Settings) EffectiveCheckinStagger() bool {
	if s.CheckinStagger != nil {
		return *s.CheckinStagger
	}
	return true
}

// EffectiveQueueWaitFirstToken 返回排队「首字前一直等」的有效值；未设置（nil）时默认开（true）。
func (s *Settings) EffectiveQueueWaitFirstToken() bool {
	if s.QueueWaitFirstToken != nil {
		return *s.QueueWaitFirstToken
	}
	return true
}

type QuotaInfo struct {
	Plan              string             `json:"plan"`
	UserQuota         *QuotaBucket       `json:"user_quota,omitempty"`
	AddonQuota        *QuotaBucket       `json:"addon_quota,omitempty"`
	DedicatedPackages []DedicatedPackage `json:"dedicated_packages,omitempty"`
	IsQuotaExceeded   bool               `json:"is_quota_exceeded"`
	ExpiresAt         int64              `json:"expires_at,omitempty"`
}

// DedicatedPackage 上游「专属资源包」额度（dedicatedResourcePackages）。
// 活动奖励（如 SOTA 专属积分）单独成桶，不与 user/addon 混合，
// 通常带独立过期时间与使用范围限制。
type DedicatedPackage struct {
	ID          string              `json:"id,omitempty"`
	Name        string              `json:"name,omitempty"`
	Description string              `json:"description,omitempty"`
	Used        float64             `json:"used"`
	Total       float64             `json:"total"`
	Remaining   float64             `json:"remaining"`
	Unit        string              `json:"unit,omitempty"`
	ExpiresAtMs int64               `json:"expires_at_ms,omitempty"`
	Available   bool                `json:"available,omitempty"`
	Status      string              `json:"status,omitempty"`
	Labels      []DedicatedPkgLabel `json:"labels,omitempty"`
}

// DedicatedPkgLabel 资源包的展示标签（上游 displayLabels，含多语言）。
type DedicatedPkgLabel struct {
	Dimension string            `json:"dimension,omitempty"`
	Value     string            `json:"value,omitempty"`
	ValueI18n map[string]string `json:"value_i18n,omitempty"`
}

type QuotaBucket struct {
	Used      float64 `json:"used"`
	Total     float64 `json:"total"`
	Remaining float64 `json:"remaining"`
	ResetTime string  `json:"reset_time,omitempty"`
}
