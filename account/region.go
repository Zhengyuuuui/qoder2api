package account

// Region 标识 Qoder 站点区域
type Region string

const (
	RegionGlobal Region = "global"
	RegionCN     Region = "cn"
)

// Endpoints 包含某个 region 对应的所有 API 端点
type Endpoints struct {
	DeviceLoginBase string // OAuth 登录页
	PollEndpoint    string // deviceToken/poll
	UserinfoBase    string // /api/v1/userinfo
	PlanEndpoint    string // /api/v2/user/plan
	QuotaEndpoint   string // /api/v2/quota/usage
	ChatStreamURL   string // SSE chat
	ModelListURL    string // model list
	JobTokenURL     string // PAT exchange
	OpenAPIHost     string // OpenAPI 主机（签到/活动等 /sash、/api 接口）
}

var endpointsGlobal = Endpoints{
	DeviceLoginBase: "https://qoder.com/device/selectAccounts",
	PollEndpoint:    "https://openapi.qoder.sh/api/v1/deviceToken/poll",
	UserinfoBase:    "https://openapi.qoder.sh/api/v1/userinfo",
	PlanEndpoint:    "https://openapi.qoder.sh/api/v2/user/plan",
	QuotaEndpoint:   "https://openapi.qoder.sh/api/v2/quota/usage",
	ChatStreamURL:   "https://api1.qoder.sh/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1",
	ModelListURL:    "https://api2.qoder.sh/algo/api/v2/model/list?Encode=1",
	JobTokenURL:     "https://center.qoder.sh/algo/api/v3/user/jobToken?Encode=1",
	OpenAPIHost:     "openapi.qoder.sh",
}

var endpointsCN = Endpoints{
	DeviceLoginBase: "https://qoder.com.cn/device/selectAccounts",
	PollEndpoint:    "https://openapi.qoder.com.cn/api/v1/deviceToken/poll",
	UserinfoBase:    "https://openapi.qoder.com.cn/api/v1/userinfo",
	PlanEndpoint:    "https://openapi.qoder.com.cn/api/v2/user/plan",
	QuotaEndpoint:   "https://openapi.qoder.com.cn/api/v2/quota/usage",
	ChatStreamURL:   "https://gateway.qoder.com.cn/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1",
	ModelListURL:    "https://gateway.qoder.com.cn/algo/api/v2/model/list?Encode=1",
	JobTokenURL:     "https://gateway.qoder.com.cn/algo/api/v3/user/jobToken?Encode=1",
	OpenAPIHost:     "openapi.qoder.com.cn",
}

// GetEndpoints 根据 region 返回对应端点配置，空值或未知值视为 global
func GetEndpoints(region Region) Endpoints {
	if region == RegionCN {
		return endpointsCN
	}
	return endpointsGlobal
}

// NormalizeRegion 将字符串归一化为合法 Region 值
func NormalizeRegion(s string) Region {
	if s == "cn" || s == "CN" {
		return RegionCN
	}
	return RegionGlobal
}
