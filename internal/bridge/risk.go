package bridge

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 风控遥测：把「上游给我们的风险信号」变成可计数的指标。
//
// 为什么需要：排队率、频控率这些数字一变好一变坏，如果只有日志文本，
// 调参就只能靠猜。把信号聚合成计数，才能对比「改前 vs 改后」到底是
// 变好了还是变差了——先量再调。

// 风险信号类型（对应上游可观测的四类拒绝 / 拥塞）。
const (
	// RiskQueue 上游模型排队（10605 / isQueued / RESOURCE_LIMIT 排队语义）。
	RiskQueue = "queue"
	// RiskRate 频控（HTTP 429）。
	RiskRate = "rate"
	// RiskRejected 凭证 / 额度 / 权限拒绝（HTTP 401 / 403，及无权限的 RESOURCE_LIMIT）。
	RiskRejected = "rejected"
	// RiskVersionGate 客户端版本过低（上游错误码 430）。
	RiskVersionGate = "version_gate"
)

// riskDetailMaxLen 「最近一次信号」摘要的最大字符数。
// 上游错误详情可能是整段 JSON，不截断会让 /health 响应体与内存无限膨胀。
const riskDetailMaxLen = 200

// riskAccountIDLen 「最近一次信号」里对外暴露的账号 ID 长度。
// 只给前几位：排查需要能定位账号，但 /health 不做鉴权，不能泄露全量 ID。
const riskAccountIDLen = 8

// RiskSignal 一次风险信号快照（用于「最近一次信号」展示）。
type RiskSignal struct {
	Type    string    `json:"type"`
	Model   string    `json:"model"`
	Account string    `json:"account"`
	At      time.Time `json:"at"`
	Detail  string    `json:"detail"`
}

// RiskStats 是对外暴露的风控遥测快照。
type RiskStats struct {
	Queue       int64       `json:"queue"`
	Rate        int64       `json:"rate"`
	Rejected    int64       `json:"rejected"`
	VersionGate int64       `json:"version_gate"`
	Last        *RiskSignal `json:"last,omitempty"`
}

// riskCounters 包级计数器。
//
// 约束：并发请求会同时打点，计数器必须用 atomic（普通 int64 在 -race 下
// 会报竞争）；「最近一次信号」是复合写（读改写整个结构体），用 mutex 保护。
type riskCounters struct {
	queue       atomic.Int64
	rate        atomic.Int64
	rejected    atomic.Int64
	versionGate atomic.Int64

	signalMu sync.Mutex
	last     *RiskSignal
}

// risk 进程级计数器（包级单例，跨所有账号 / 模型共享）。
var risk = &riskCounters{}

// RecordRiskSignal 打点一次风险信号：计数 +1，并更新「最近一次信号」。
//
// 这是唯一的计数入口：调用方只负责把「信号类型」算对，
// 计数与最近信号维护都收敛在这里，避免多处重复打点导致口径漂移。
//
// model / account 用于定位；account 会被截断到前 riskAccountIDLen 位
// （/health 不做鉴权，不能泄露全量账号 ID）。detail 截断到 riskDetailMaxLen。
func RecordRiskSignal(kind, model, account, detail string) {
	switch kind {
	case RiskQueue:
		risk.queue.Add(1)
	case RiskRate:
		risk.rate.Add(1)
	case RiskRejected:
		risk.rejected.Add(1)
	case RiskVersionGate:
		risk.versionGate.Add(1)
	default:
		// 未知类型不计数（避免污染指标口径），但仍记录最近一次信号便于排查
	}

	sig := &RiskSignal{
		Type:    kind,
		Model:   model,
		Account: shortAccountID(account),
		At:      time.Now(),
		Detail:  truncateRiskDetail(detail),
	}
	risk.signalMu.Lock()
	risk.last = sig
	risk.signalMu.Unlock()
}

// RiskSnapshot 返回当前风控遥测快照（并发安全）。
func RiskSnapshot() RiskStats {
	st := RiskStats{
		Queue:       risk.queue.Load(),
		Rate:        risk.rate.Load(),
		Rejected:    risk.rejected.Load(),
		VersionGate: risk.versionGate.Load(),
	}
	risk.signalMu.Lock()
	if risk.last != nil {
		cp := *risk.last
		st.Last = &cp
	}
	risk.signalMu.Unlock()
	return st
}

// shortAccountID 截断账号 ID，只保留前若干位用于展示。
func shortAccountID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) <= riskAccountIDLen {
		return id
	}
	return id[:riskAccountIDLen]
}

// truncateRiskDetail 截断上游错误摘要。
// 按 rune 截断，避免切断 UTF-8 多字节字符导致 JSON 乱码。
func truncateRiskDetail(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) <= riskDetailMaxLen {
		return s
	}
	return string(r[:riskDetailMaxLen]) + "..."
}

// riskKindFor 由「HTTP 状态 + 详情 + 已分类的错误类型」推导风险信号类型。
// 返回空串表示该错误不构成风险信号（内容审核 / 参数错 / 客户端取消等）。
//
// 判定口径与 pool.go 的失败分类保持一致，避免同一错误在两处被算成不同语义：
//   - 排队语义（isQueued / 10605 / 上游要求等待）→ queue
//   - 429 → rate
//   - 401 / 403 / 无权限的 RESOURCE_LIMIT → rejected
//   - 430 → version_gate
func riskKindFor(code int, detail, errType string) string {
	// 内容审核是确定性拒绝，不构成风控信号（否则信号量会被用户输入噪声淹没）
	if errType == ErrTypeContentPolicy {
		return ""
	}
	// 嵌套版本门禁（430）优先于一切其他判定：上游实测把 430 嵌在
	// message 的嵌套 JSON 里、外层仍是 403 —— 只认字面 430 会让这类
	// 信号永不触发（客户端版本过低 → model/list 缺模型，表象是「模型
	// 没了」而不是报错，极难排查）。
	if isVersionGateDetail(detail) {
		return RiskVersionGate
	}
	switch errType {
	case ErrTypeUpstreamQueue:
		return RiskQueue
	case ErrTypeResourceLimit:
		// RESOURCE_LIMIT 未排队 = 该账号无权使用该模型（额度 / 套餐范围问题）
		return RiskRejected
	}
	switch code {
	case http.StatusTooManyRequests:
		return RiskRate
	case http.StatusUnauthorized, http.StatusForbidden:
		return RiskRejected
	case 430:
		return RiskVersionGate
	}
	// 流内业务错误（HTTP 200 帧内 code != 0，Status=0）里的排队语义
	if code == 0 && isQueueDetail(detail) {
		return RiskQueue
	}
	return ""
}

// versionGatePattern 嵌套报文里的 430 错误码形态（"code":"430" /
// "code":430）。上游把 430 嵌在 message 字段的嵌套 JSON 里，内层引号
// 带反斜杠转义（\"code\":\"430\"），两种形态都要认。只匹配带键名的
// JSON 字段，避免把详情文本里偶然出现的 430 数字（比如 token 片段）
// 误判成版本门禁。
var versionGatePattern = regexp.MustCompile(`\\?"code\\?"\s*:\s*\\?"?430\\?"?`)

// isVersionGateDetail 判断详情里是否嵌有 430 版本门禁错误码。
func isVersionGateDetail(detail string) bool {
	return versionGatePattern.MatchString(detail)
}

// isQueueDetail 判断详情文本里是否带排队语义。
// 只认「明确在排队」的标记：10605 错误码、isQueued 标志、带重试等待时间。
// 不认单独的 RESOURCE_LIMIT——那可能是不排队的权限问题（由上层分类判定）。
func isQueueDetail(detail string) bool {
	d := strings.ToLower(detail)
	for _, k := range []string{"10605", "isqueued", "retryafterseconds"} {
		if strings.Contains(d, k) {
			return true
		}
	}
	return false
}

// recordUpstreamRisk 在「上游错误被观察到的那一层」统一打点。
//
// 为什么打点放在客户端层而不是分类层：分类层（NewUpstreamError）不知道
// 这次失败用的是哪个模型、哪个账号；打点需要这两项才能定位。放在客户端
// 层还有一个好处——排队的错误会在同账号内被重试若干次，每次都是一次真实
// 的「上游拥塞」信号，逐次计数才能反映真实的排队率。
//
// 计数入口仍然只有一个（RecordRiskSignal），所以不会重复计数。
func recordUpstreamRisk(err error, model, accountID string) {
	if err == nil {
		return
	}
	var ue *UpstreamError
	if !errors.As(err, &ue) {
		return
	}
	kind := ue.RiskKind
	if kind == "" {
		kind = riskKindFor(ue.Status, ue.Detail, ue.ErrType)
	}
	if kind == "" {
		return
	}
	RecordRiskSignal(kind, model, accountID, ue.Detail)
}
