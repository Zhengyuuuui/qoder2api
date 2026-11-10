package bridge

import (
	"strings"
)

// 凭证 / 额度类错误的细分分类。
//
// 为什么必须细分：这四类错误的处置方式完全不同，压成同一个「401/403 →
// 冷却 30 分钟」会造成两类实际损失：
//   - 凭证真过期（TOKEN_EXPIRE）：冷却 30 分钟后再试仍是注定失败的请求，
//     期间账号白躺，且用户拿不到「请重新授权」的明确提示；
//   - 额度耗尽：30 分钟就醒过来再撞一次额度墙，反复消耗请求。
//
// 分类口径与上游错误体的 code / subCode / message 关键字对应。判定顺序有
// 讲究：先判凭证过期（最需要用户介入），再判额度，最后才是权限类。

// 凭证过期类关键字（匹配 code / subCode / message）。
var tokenExpiredMarkers = []string{
	"token_expire",
	"tokenexpire",
	"token expired",
	"credential expired",
	"credentials expired",
	"invalid_grant",
	"unauthorized_client",
	"access_denied_token",
	"凭证过期",
	"令牌过期",
	"凭证已过期",
	"需要重新授权",
	"重新登录",
}

// 额度耗尽类关键字。
var quotaExhaustedMarkers = []string{
	"quota_exhausted",
	"quotaexhausted",
	"quota exceeded",
	"quota_exceed",
	"exceed quota",
	"insufficient_quota",
	"insufficient quota",
	"insufficient balance",
	"credit exhausted",
	"credits exhausted",
	"balance exhausted",
	"out of credits",
	"no credits",
	"usage_limit_reached",
	"额度不足",
	"额度耗尽",
	"配额不足",
	"配额耗尽",
	"积分不足",
	"余额不足",
}

// 试用受限类关键字。
var trialRestrictedMarkers = []string{
	"trial_restricted",
	"trialrestricted",
	"trial restricted",
	"trial restriction",
	"trial limit",
	"trial expired",
	"trial_ended",
	"trial ended",
	"free_trial",
	"freetrial",
	"free trial",
	"试用受限",
	"试用限制",
	"试用已过期",
	"试用账号",
}

// 权限不足 / 模型不可用类关键字。
var modelNotAllowedMarkers = []string{
	"model_not_allowed",
	"modelnotallowed",
	"model not allowed",
	"model not available",
	"model unavailable",
	"not entitled to model",
	"no permission for model",
	"unsupported model",
	"model_not_in_plan",
	"模型不可用",
	"模型不在",
	"无权限使用该模型",
	"不支持的模型",
	"未包含该模型",
}

// credentialKind 凭证类错误的细分类型。
type credentialKind int

const (
	// credentialNone 不属于凭证类细分（保持原有分类）。
	credentialNone credentialKind = iota
	// credentialTokenExpired 凭证过期，需重新授权。
	credentialTokenExpired
	// credentialQuotaExhausted 额度 / 配额耗尽。
	credentialQuotaExhausted
	// credentialTrialRestricted 试用受限。
	credentialTrialRestricted
	// credentialModelNotAllowed 权限不足 / 模型不在套餐内。
	credentialModelNotAllowed
)

// errTypeOf 把凭证细分类映射到对外的错误类型。
func (k credentialKind) errTypeOf() string {
	switch k {
	case credentialTokenExpired:
		return ErrTypeTokenExpired
	case credentialQuotaExhausted:
		return ErrTypeQuotaExhausted
	case credentialTrialRestricted:
		return ErrTypeTrialRestricted
	case credentialModelNotAllowed:
		return ErrTypeModelNotAllowed
	}
	return ""
}

// classifyCredentialError 判定错误属于哪一类凭证 / 额度问题。
//
// detail 是上游原始错误详情（可能是整段嵌套 JSON）。
// 判定用关键字匹配而非解析固定结构：上游这几种错误的报文形态并不统一
// （有时在 code、有时在 subCode、有时只在 message 文本里），关键字匹配
// 对形态变化的鲁棒性更好。
//
// 顺序理由：TOKEN_EXPIRE 的报文里有时也会带 "quota" 字样（例如提示
// "凭证过期，请检查配额"），先判凭证过期才不会误判成额度耗尽。
func classifyCredentialError(detail string) credentialKind {
	d := strings.ToLower(detail)
	if d == "" {
		return credentialNone
	}
	switch {
	case containsAny(d, tokenExpiredMarkers):
		return credentialTokenExpired
	case containsAny(d, quotaExhaustedMarkers):
		return credentialQuotaExhausted
	case containsAny(d, trialRestrictedMarkers):
		return credentialTrialRestricted
	case containsAny(d, modelNotAllowedMarkers):
		return credentialModelNotAllowed
	}
	return credentialNone
}

// credentialMessage 生成对客户端可读的中文消息。
//
// model 用于在「模型不可用」时点名是哪个模型。
func credentialMessage(kind credentialKind, model, detail string) string {
	switch kind {
	case credentialTokenExpired:
		return "账号凭证已过期，需要重新授权：" +
			"该账号的登录凭证已失效，重试无效。请在控制台重新登录 / 重新导入该账号的凭证，" +
			"授权成功后账号会自动恢复。上游详情：" + truncateRunes(detail, 200)
	case credentialQuotaExhausted:
		return "账号额度已耗尽：" +
			"该账号的额度 / 配额已用尽，重试无效。网关已切换到其他账号，" +
			"若所有账号都耗尽请等待额度重置或升级套餐。上游详情：" + truncateRunes(detail, 200)
	case credentialTrialRestricted:
		return "试用账号受限：" +
			"该账号处于试用限制状态，无法继续使用当前能力。请等待试用结束、" +
			"升级套餐或改用其他账号。上游详情：" + truncateRunes(detail, 200)
	case credentialModelNotAllowed:
		m := model
		if m == "" {
			m = "该模型"
		}
		return "模型 " + m + " 不在此账号的套餐 / 权限范围内：" +
			"切换到其他账号同样无效，请改用其他模型，或为该账号开通对应套餐。" +
			"上游详情：" + truncateRunes(detail, 200)
	}
	return ""
}
