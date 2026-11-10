package main

import (
	"net/http"

	"qoder2api/internal/bridge"
)

// 健康检查 + 风控遥测端点。
//
// 为什么单独开一个 /health（而不是塞进 /api/status）：
//   - /api/status 在控制台鉴权之后，外部探针拿不到；
//   - /health 必须是「零副作用」的：不能触发任何上游请求，只读内存计数。
//
// 信息暴露边界（/health 不做鉴权，务必克制）：
//   - 账号 ID 只给前 8 位（bridge.shortAccountID 已在采集侧截断）；
//   - 不返回 token、密码、代理凭据；
//   - detail 只是上游错误摘要（截断 200 字符）。

// healthResponse 是 /health 的响应体。
type healthResponse struct {
	OK   bool             `json:"ok"`
	Risk bridge.RiskStats `json:"risk"`
	// Pool 账号池汇总（可能为 null —— 桥未启动时没有池）。
	Pool *bridge.PoolSummary `json:"pool,omitempty"`
	// Endpoints 端点健康状态（当前生效端点、连续失败次数、冷却剩余）。
	Endpoints []bridge.EndpointStatus `json:"endpoints,omitempty"`
	// Egress 按区域分组的出口配置（脱敏）。
	Egress []bridge.RegionEgressStatus `json:"egress,omitempty"`
}

// handleHealth 返回健康状态与风控遥测。
//
// 约束：只读内存状态，绝不触发上游请求——否则健康检查本身就会
// 消耗账号额度、加剧风控。
func handleHealth(w http.ResponseWriter, r *http.Request) {
	resp := healthResponse{
		OK:        true,
		Risk:      bridge.RiskSnapshot(),
		Endpoints: bridge.EndpointStatuses(),
		Egress:    bridge.RegionEgressStatuses(),
	}
	if svc != nil {
		if sum := svc.PoolSummary(); sum.Total > 0 {
			resp.Pool = &sum
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
