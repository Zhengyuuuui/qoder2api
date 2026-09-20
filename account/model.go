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
}

type Settings struct {
	Port                 int                          `json:"port"`
	AutoStart            bool                         `json:"auto_start"`
	LogLevel             string                       `json:"log_level"`                  // "info" | "debug" | "error"
	QuotaRefreshInterval int                          `json:"quota_refresh_interval"`     // 秒，0=不自动刷新
	BridgeToken          string                       `json:"bridge_token,omitempty"`     // 自定义鉴权 token，空则使用默认值 "qccg"
	ConsolePassword      string                       `json:"console_password,omitempty"` // Web 控制台登录密码（类似 CPA management key）
	ModelMapping         map[string]string            `json:"model_mapping,omitempty"`    // [DEPRECATED] 旧扁平映射（向后兼容，MapModel 中作为兜底回退使用）
	ModelMappings        map[string]map[string]string `json:"model_mappings,omitempty"`   // agent (claude/codex/gemini) → 客户端模型名 → Qoder model.key
	AutoCheckin          bool                         `json:"auto_checkin"`               // 每日 10:00 自动签到（默认关闭）
	MachineSalt          string                       `json:"machine_salt,omitempty"`     // 本机设备指纹盐（首次启动自动生成；勿修改，变更即指纹漂移）
}

type QuotaInfo struct {
	Plan            string       `json:"plan"`
	UserQuota       *QuotaBucket `json:"user_quota,omitempty"`
	AddonQuota      *QuotaBucket `json:"addon_quota,omitempty"`
	IsQuotaExceeded bool         `json:"is_quota_exceeded"`
	ExpiresAt       int64        `json:"expires_at,omitempty"`
}

type QuotaBucket struct {
	Used      float64 `json:"used"`
	Total     float64 `json:"total"`
	Remaining float64 `json:"remaining"`
	ResetTime string  `json:"reset_time,omitempty"`
}
