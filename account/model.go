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
	// OrgResourcePackage 组织共享流量包（Team 账号）。个人套餐额度用尽后，
	// 上游会自动改从这个共享包扣费，因此 isQuotaExceeded 仍为 false。
	// 仅在 userType=teams 且组织配置了共享包时出现。
	OrgResourcePackage *OrgResourcePackage `json:"org_resource_package,omitempty"`
	IsQuotaExceeded    bool                `json:"is_quota_exceeded"`
	ExpiresAt          int64               `json:"expires_at,omitempty"`
}

// OrgResourcePackage 上游 orgResourcePackage 额度（Team 账号的组织共享流量）。
//
// 与其余额度桶的差异：
//   - 总量字段叫 cap，不叫 total（最容易漏解析的点）
//   - 单对象而非数组，字段名是单数 orgResourcePackage
//   - available=false 表示该组织当前未开放共享包
type OrgResourcePackage struct {
	Cap        float64 `json:"cap"`
	Used       float64 `json:"used"`
	Remaining  float64 `json:"remaining"`
	Percentage float64 `json:"percentage,omitempty"`
	Unit       string  `json:"unit,omitempty"`
	Available  bool    `json:"available"`
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
