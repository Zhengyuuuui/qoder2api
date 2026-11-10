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
	ChatStreamURL   string // SSE chat（首选候选，等价于 ChatStreamURLs[0]）
	ModelListURL    string // model list（首选候选，等价于 ModelListURLs[0]）
	JobTokenURL     string // PAT exchange
	OpenAPIHost     string // OpenAPI 主机（签到/活动等 /sash、/api 接口）

	// ChatStreamURLs 推理端点的候选列表（按优先级排列）。
	//
	// 为什么是列表而不是单值：官方客户端的推理流量按用途分散在多个集群上，
	// 且服务端可按账号 / 区域 / 实验分组把客户端选举到不同集群。已验证的
	// 实测结论是：某个候选常年超时、另一个候选正常响应——把端点写死成
	// 单点会稳定踩雷，必须能在候选之间故障转移。
	//
	// 顺序依据：官方客户端配置里各集群的优先级排列（首选在最前）。
	// 故障转移时按此顺序尝试，并在端点持续失败后把它临时降到末尾
	// （见 internal/bridge/endpoints.go）。
	ChatStreamURLs []string

	// ModelListURLs 模型目录端点的候选列表（顺序同上）。
	ModelListURLs []string
}

// 全球站（国际版）推理端点候选。
//
// 顺序依据：官方客户端配置里的集群优先级。api1 承载 SSE 推理主流量，
// api2 承载模型目录；两者在服务端可被选举覆盖，故各自都给出备用集群。
var globalChatStreamURLs = []string{
	"https://api1.qoder.sh/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1",
	"https://api3.qoder.sh/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1",
	"https://api2.qoder.sh/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1",
}

var globalModelListURLs = []string{
	"https://api2.qoder.sh/algo/api/v2/model/list?Encode=1",
	"https://api1.qoder.sh/algo/api/v2/model/list?Encode=1",
}

// 国内站推理端点候选：单一 gateway（国内版未做多集群分离）。
var cnChatStreamURLs = []string{
	"https://gateway.qoder.com.cn/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1",
}

var cnModelListURLs = []string{
	"https://gateway.qoder.com.cn/algo/api/v2/model/list?Encode=1",
}

var endpointsGlobal = Endpoints{
	DeviceLoginBase: "https://qoder.com/device/selectAccounts",
	PollEndpoint:    "https://openapi.qoder.sh/api/v1/deviceToken/poll",
	UserinfoBase:    "https://openapi.qoder.sh/api/v1/userinfo",
	PlanEndpoint:    "https://openapi.qoder.sh/api/v2/user/plan",
	QuotaEndpoint:   "https://openapi.qoder.sh/api/v2/quota/usage",
	ChatStreamURL:   globalChatStreamURLs[0],
	ModelListURL:    globalModelListURLs[0],
	JobTokenURL:     "https://center.qoder.sh/algo/api/v3/user/jobToken?Encode=1",
	OpenAPIHost:     "openapi.qoder.sh",
	ChatStreamURLs:  globalChatStreamURLs,
	ModelListURLs:   globalModelListURLs,
}

var endpointsCN = Endpoints{
	DeviceLoginBase: "https://qoder.com.cn/device/selectAccounts",
	PollEndpoint:    "https://openapi.qoder.com.cn/api/v1/deviceToken/poll",
	UserinfoBase:    "https://openapi.qoder.com.cn/api/v1/userinfo",
	PlanEndpoint:    "https://openapi.qoder.com.cn/api/v2/user/plan",
	QuotaEndpoint:   "https://openapi.qoder.com.cn/api/v2/quota/usage",
	ChatStreamURL:   cnChatStreamURLs[0],
	ModelListURL:    cnModelListURLs[0],
	JobTokenURL:     "https://gateway.qoder.com.cn/algo/api/v3/user/jobToken?Encode=1",
	OpenAPIHost:     "openapi.qoder.com.cn",
	ChatStreamURLs:  cnChatStreamURLs,
	ModelListURLs:   cnModelListURLs,
}

// GetEndpoints 根据 region 返回对应端点配置，空值或未知值视为 global
func GetEndpoints(region Region) Endpoints {
	if region == RegionCN {
		return endpointsCN
	}
	return endpointsGlobal
}

// ChatStreamCandidates 返回该区域的推理端点候选列表（按优先级）。
//
// 容错：候选列表为空时回退到单值字段，保证旧配置（只填了 ChatStreamURL）
// 也能正常工作。
func (e Endpoints) ChatStreamCandidates() []string {
	if len(e.ChatStreamURLs) > 0 {
		return e.ChatStreamURLs
	}
	if e.ChatStreamURL != "" {
		return []string{e.ChatStreamURL}
	}
	return nil
}

// ModelListCandidates 返回该区域的模型目录端点候选列表（按优先级）。
func (e Endpoints) ModelListCandidates() []string {
	if len(e.ModelListURLs) > 0 {
		return e.ModelListURLs
	}
	if e.ModelListURL != "" {
		return []string{e.ModelListURL}
	}
	return nil
}

// NormalizeRegion 将字符串归一化为合法 Region 值
func NormalizeRegion(s string) Region {
	if s == "cn" || s == "CN" {
		return RegionCN
	}
	return RegionGlobal
}
