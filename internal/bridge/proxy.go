package bridge

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

	"qoder2api/logger"
)

// 账号级出口粘性代理。
//
// 背景：数据中心 IP（容器 / 云主机 / VPS）无法用请求头伪装成住宅 IP——
// 上游按 ASN / 地理做风控，能换出口才是唯一解。而「换出口」必须满足
// 两条：每个账号绑一条固定出口、且请求之间不轮换。
//   - 所有账号共用一个 IP → 容易被归为同一「使用群体」，触发更严限流；
//   - 每请求轮换节点 → 出口画像断裂，最容易被判异常。
//
// 实现约束：零第三方依赖。Go 标准库 net/http 原生支持 HTTP/HTTPS 代理
// （Transport.Proxy），无需额外依赖；SOCKS5 标准库不支持（需 x/net/proxy），
// 本任务只实现 HTTP/HTTPS 代理。

// accountProxyEnvPrefix 环境变量前缀：QD_ACCOUNT_PROXY_<uid前8位>=<proxy_url>。
//
// 取账号 ID 前 8 位（大小写不敏感）便于人工配置与记忆，同时避免把
// 完整账号 ID 暴露在环境变量名里。
const accountProxyEnvPrefix = "QD_ACCOUNT_PROXY_"

// 区域级代理环境变量。
//
// 为什么需要区域级：国内版与国际版的风控策略不同，把两个区域的流量混在
// 同一个出口 IP 上，更容易被上游关联为同一使用群体。区域级代理让
// 「该区域所有账号的默认出口」可单独指定，账号级配置再在其上覆盖。
const (
	// ProxyEnvGlobal 国际版（global）区域默认出口代理。
	ProxyEnvGlobal = "QD_PROXY_GLOBAL"
	// ProxyEnvCN 国内版（cn）区域默认出口代理。
	ProxyEnvCN = "QD_PROXY_CN"
)

// proxyEnvKeyLen 参与环境变量名的账号 ID 长度。
const proxyEnvKeyLen = 8

// ProxySchemeSupported 判断该 scheme 是否是本实现支持的代理协议。
// 仅支持 http / https（标准库 Transport.Proxy 原生支持）。
func ProxySchemeSupported(scheme string) bool {
	switch strings.ToLower(strings.TrimSpace(scheme)) {
	case "http", "https":
		return true
	}
	return false
}

// ProxyBinding 代理解析结果的三态表达。
//
// 为什么必须三态：「未配置代理」（合法，直连）与「配了但非法」
// （socks5 / 无 host / 解析失败）是语义完全不同的两种情况——后者若
// 退化成直连，等于把真实数据中心出口暴露给上游，比请求失败更糟。
// 用 error 字段把「配置非法」显式化，调用方（bindProxy）据此把账号
// 标记为不可用，而不是静默放行。
type ProxyBinding struct {
	URL       string // 生效的代理地址；空 = 直连（仅在未配置时为空）
	ConfigErr error  // 已配置但非法 / 不支持；非 nil 时绝不能直连
}

// resolveAccountProxy 解析某账号最终生效的代理（不启用区域级）。
//
// 优先级：账号级环境变量 > 账号配置 > 直连。
// 账号级环境变量放在最前是为了便于临时覆盖排障（不必改账号文件）。
func resolveAccountProxy(accountID, configured string) ProxyBinding {
	return resolveAccountProxyInRegion(accountID, configured, "")
}

// resolveAccountProxyInRegion 按区域解析账号的最终代理。
//
// region 为空表示不启用区域级代理（单账号 / 调试场景）。
// 返回值语义见 ProxyBinding：未配置 → URL 空且 ConfigErr  nil（直连）；
// 已配置但非法 → ConfigErr 非 nil（调用方不得直连）。
// label 指代配置归属（账号短 ID / "region cn"），source 指代配置来源
// （env / account / 区域环境变量名）——两者都进错误与日志，排障时能直接
// 定位到「哪一项配置」。
func resolveAccountProxyInRegion(accountID, configured string, region string) ProxyBinding {
	envVal := accountProxyFromEnv(accountID)
	raw := envVal
	source := "env"
	if raw == "" {
		raw = configured
		source = "account"
	}
	if strings.TrimSpace(raw) == "" && region != "" {
		raw = regionProxyFromEnv(region)
		source = regionProxyEnvName(region)
	}
	return validateProxyBinding(shortAccountID(accountID), source, raw)
}

// validateProxyBinding 解析并校验代理地址，返回三态绑定（未配置 /
// 可用 / 配置非法）。所有入口（账号级、区域级）都走这里，保证校验口径
// 与错误措辞一致。
func validateProxyBinding(label, source, raw string) ProxyBinding {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		// 未配置任何代理：直连是合法路径
		return ProxyBinding{}
	}
	u, err := url.Parse(raw)
	if err != nil {
		// 解析失败的原文不回显：用户配置里可能带 token 段；url.Parse 的错误
		// 消息本身也会回显原文，因此不直接包装它，只给来源与自查提示。
		return ProxyBinding{ConfigErr: fmt.Errorf(
			"%s: proxy url from %s is unparseable "+
				"(check for spaces, typos or an unsupported scheme)",
			label, source)}
	}
	if !ProxySchemeSupported(u.Scheme) {
		// SOCKS5 需要额外依赖，当前未实现。明确失败而不是静默直连：
		// 直连会把真实出口暴露给上游。给出可操作的建议。
		return ProxyBinding{ConfigErr: fmt.Errorf(
			"%s: unsupported proxy scheme %q from %s. Only http/https proxies are supported; "+
				"for SOCKS5 run a local forwarder that speaks http and point the proxy at it",
			label, u.Scheme, source)}
	}
	if u.Host == "" {
		return ProxyBinding{ConfigErr: fmt.Errorf(
			"%s: proxy url from %s has no host",
			label, source)}
	}
	logger.Info("%s: proxy resolved from %s: %s", label, source, redactProxyURL(raw))
	return ProxyBinding{URL: raw}
}

// ResolveAccountProxyBinding 解析账号最终代理（导出：签到等包外调用方
// 需要与对话侧完全相同的三态解析，避免「签到直连、对话走代理」的双出口）。
//
// 优先级：账号级环境变量 > 账号配置 > 区域级 > 直连。
// 返回值语义见 ProxyBinding。
func ResolveAccountProxyBinding(accountID, configured, region string) ProxyBinding {
	return resolveAccountProxyInRegion(accountID, configured, region)
}

// ResolveRegionProxyBinding 解析区域级代理（账号 ID 之前的阶段用：
// jobToken 交换 / userinfo / 设备登录轮询——那时还没有账号 ID，
// 只能按区域绑定出口，见 bridge.go 的 regionProxyBinding）。
//
// 未配置 → 空 binding（直连合法）；配置非法 → ConfigErr（调用方处理）。
// 错误信息以「region <区域>」指代归属：区域级配置修错了不能显示成
// 某个账号的问题，否则运维会去翻账号配置。
func ResolveRegionProxyBinding(region string) ProxyBinding {
	raw := strings.TrimSpace(regionProxyFromEnv(region))
	if raw == "" {
		return ProxyBinding{}
	}
	return validateProxyBinding("region "+strings.ToLower(strings.TrimSpace(region)),
		regionProxyEnvName(region), raw)
}

// regionProxyFromEnv 读取区域级代理配置。
//
// 未配置时返回空串；调用方负责在「该区域无专用出口」时打 WARN。
func regionProxyFromEnv(region string) string {
	key := ProxyEnvGlobal
	if strings.EqualFold(strings.TrimSpace(region), "cn") {
		key = ProxyEnvCN
	}
	return strings.TrimSpace(os.Getenv(key))
}

// regionProxyConfigured 报告该区域是否配置了区域级代理。
func regionProxyConfigured(region string) bool {
	return regionProxyFromEnv(region) != ""
}

// accountProxyEnvName 返回该账号对应的环境变量名。
func accountProxyEnvName(accountID string) string {
	return accountProxyEnvPrefix + strings.ToLower(accountIDPrefix(accountID))
}

// accountIDPrefix 取账号 ID 前 N 位（不足则取全部）。
func accountIDPrefix(accountID string) string {
	id := strings.TrimSpace(accountID)
	if len(id) <= proxyEnvKeyLen {
		return id
	}
	return id[:proxyEnvKeyLen]
}

// accountProxyFromEnv 从环境变量读取该账号的代理配置。
//
// 变量名大小写不敏感（QD_ACCOUNT_PROXY_A1B2C3D4 与 QD_ACCOUNT_PROXY_a1b2c3d4 等价），
// 因此这里遍历环境变量做匹配，而不是直接用 os.Getenv。
func accountProxyFromEnv(accountID string) string {
	// 前缀整体转小写后再比较：环境变量名大小写不敏感。
	prefix := strings.ToLower(accountProxyEnvPrefix + accountIDPrefix(accountID))
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		k, v := kv[:eq], kv[eq+1:]
		lk := strings.ToLower(k)
		if !strings.HasPrefix(lk, prefix) {
			continue
		}
		// 前缀匹配后必须确认余下部分为空，否则 QD_ACCOUNT_PROXY_abc 会
		// 命中 QD_ACCOUNT_PROXY_abcdef（不同账号）。
		if len(lk) != len(prefix) {
			continue
		}
		if strings.TrimSpace(v) == "" {
			continue
		}
		return strings.TrimSpace(v)
	}
	return ""
}

// redactProxyURL 脱敏代理 URL：把 userinfo 里的账号密码替换成 ***。
//
// 日志里绝不能出现代理密码——代理凭据通常是长期有效的共享凭据，
// 落进日志文件就等于泄露。
func redactProxyURL(raw string) string {
	return RedactProxyURL(raw)
}

// rawCredentialPattern 匹配裸串里的 user:pass@ 段（URL 解析失败时的兜底脱敏）。
var rawCredentialPattern = regexp.MustCompile(`([^/@\s:]+):([^/@\s]+)@`)

// redactRawProxyString 无法解析为 URL 时的兜底脱敏：至少打掉明显的
// user:pass@ 凭据段。解析失败往往是因为串本身畸形，更不能原样落日志。
func redactRawProxyString(raw string) string {
	out := rawCredentialPattern.ReplaceAllString(raw, "$1:***@")
	if i := strings.Index(out, "://"); i >= 0 {
		rest := out[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			return out[:i+3] + rest[:j] // path 可能带鉴权 token，一并去掉
		}
	}
	return out
}

// RedactProxyURL 脱敏代理 URL（对外导出：控制台展示账号绑定的代理时
// 同样不能回显密码）。
//
// 脱敏范围：密码 → ***；path / query 一律丢弃——某些代理服务用 URL path
// 做鉴权 token，落进日志 / 控制台 / 诊断接口等同泄露。无 userinfo 的
// 情况也必须脱敏（早期版本直接返回原串，是最危险的一条路径）。
func RedactProxyURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil {
		return redactRawProxyString(s)
	}
	// host 去掉可能重复携带的 userinfo（u.Host 含 userinfo 时）
	host := u.Host
	if i := strings.LastIndex(host, "@"); i >= 0 {
		host = host[i+1:]
	}
	scheme := u.Scheme
	if scheme == "" {
		scheme = "http"
	}
	if u.User != nil {
		user := u.User.Username()
		if _, hasPW := u.User.Password(); hasPW {
			// 有密码：密码打码，用户名保留（用户名本身不是秘密）
			return scheme + "://" + user + ":***@" + host
		}
		if user != "" {
			return scheme + "://" + user + "@" + host
		}
	}
	return scheme + "://" + host
}
