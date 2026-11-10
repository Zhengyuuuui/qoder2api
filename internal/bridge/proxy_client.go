package bridge

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"qoder2api/logger"
)

// 每账号独立的 HTTP 客户端（承载账号绑定的出口代理）。
//
// 为什么必须有独立 client：出口是账号级属性，一个 Transport 只能带一个
// 代理（Transport.Proxy 是构造期固定的），因此每个账号一套 Transport。
// 顺带的好处是连接池天然按账号隔离——同账号的请求复用同一条 TCP 连接，
// 不会每次重新握手。

// 缓存条目：Transport + 各超时档位对应的 client。
//
// 连接池在 Transport 上，超时在 http.Client 上。两者分离才能同时满足：
//   - 连接复用（同账号共享同一 Transport 的 idle 连接池）；
//   - 超时语义不变（callGet 15s / callPost 30s / 流式不设整体超时）。
type proxyClientEntry struct {
	transport *http.Transport
	proxy     string
	// clients 按超时档位缓存 *http.Client（timeout=0 表示不设整体超时）。
	clients map[time.Duration]*http.Client
}

// proxyClientCache 按账号缓存客户端。
type proxyClientCache struct {
	mu      sync.Mutex
	clients map[string]*proxyClientEntry
}

var (
	sharedClientCache *proxyClientCache
	cacheOnce         sync.Once
)

// clientCache 返回进程级客户端缓存单例。
func clientCache() *proxyClientCache {
	cacheOnce.Do(func() {
		sharedClientCache = &proxyClientCache{clients: map[string]*proxyClientEntry{}}
	})
	return sharedClientCache
}

// splitRegionAccount 拆分 "region|account" 形式的账号标识。
func splitRegionAccount(key string) (region, id string) {
	if i := strings.IndexByte(key, '|'); i >= 0 {
		return key[:i], key[i+1:]
	}
	return "", key
}

// HTTPClientForKey 按缓存 key 返回 http.Client（导出供账号 ID 之前的
// 阶段使用：jobToken 交换 / userinfo / 设备登录轮询，见 bridge.go）。
//
// key 形如 "region|account"：不同区域的账号即使 ID 相同也必须用不同的
// Transport——出口不同，连接池也不能混（混了会让两条线路的请求走同一条
// TCP 连接，等于两个出口合一）。
func HTTPClientForKey(key, proxy string, timeout time.Duration) *http.Client {
	return clientForKey(key, proxy, timeout)
}

// ForgetAccountClients 淘汰该账号在所有区域下的客户端缓存（账号删除时
// 调用）。缓存只增不减会残留 Transport（含空闲连接），账号删除后必须清理。
//
// 除了从缓存摘除，还主动关闭旧 Transport 的空闲连接：否则已删除账号的
// 连接会滞留到 IdleConnTimeout（90s）才断，排障时 netstat 里看着像
// 「账号还在跑流量」。
func ForgetAccountClients(accountID string) {
	if accountID == "" {
		return
	}
	cache := clientCache()
	cache.mu.Lock()
	var dropped []*proxyClientEntry
	for key, e := range cache.clients {
		if _, id := splitRegionAccount(key); id == accountID {
			dropped = append(dropped, e)
			delete(cache.clients, key)
		}
	}
	cache.mu.Unlock()
	for _, e := range dropped {
		if e != nil && e.transport != nil {
			e.transport.CloseIdleConnections()
		}
	}
}

// clientForKey 按缓存 key 返回 http.Client。
//
// key 形如 "region|account"：不同区域的账号即使 ID 相同也必须用不同的
// Transport——出口不同，连接池也不能混（混了会让两条线路的请求走同一条
// TCP 连接，等于两个出口合一）。
func clientForKey(key, proxy string, timeout time.Duration) *http.Client {
	if key == "" {
		key = "-"
	}
	cache := clientCache()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	e, ok := cache.clients[key]
	if !ok || e.proxy != proxy {
		tr := newTransport(proxy)
		e = &proxyClientEntry{
			transport: tr,
			proxy:     proxy,
			clients:   map[time.Duration]*http.Client{},
		}
		cache.clients[key] = e
	}
	if c, ok := e.clients[timeout]; ok {
		return c
	}
	c := &http.Client{Transport: e.transport, Timeout: timeout}
	e.clients[timeout] = c
	return c
}

// newTransport 构造带代理的 Transport。
//
// 拨号参数是刻意保守的默认值：MaxIdleConnsPerHost 调高以复用连接，
// 但不为单个账号开太多空闲连接——账号数多时连接总数会失控。
func newTransport(proxy string) *http.Transport {
	return &http.Transport{
		Proxy: proxyFunc(proxy),
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// proxyFunc 返回 http.Transport.Proxy 用的函数。
//
// 关键不变量：proxy 非空时绝不返回 nil——返回 nil 就等于「直连」，
// 那会把真实出口暴露给上游，代理封控努力全部作废。代理连不上时必须
// 让请求失败（见 wrapProxyError）。
func proxyFunc(proxy string) func(*http.Request) (*url.URL, error) {
	if proxy == "" {
		return nil
	}
	u, err := url.Parse(proxy)
	if err != nil {
		// 解析失败也要让请求失败（不能退化成直连），故返回一个固定错误。
		bad := fmt.Errorf("invalid proxy url %s: %w", redactProxyURL(proxy), err)
		return func(*http.Request) (*url.URL, error) { return nil, bad }
	}
	return func(*http.Request) (*url.URL, error) { return u, nil }
}

// wrapProxyError 判断错误是否由「代理拨号失败」引起，若是则包装成点名
// 代理地址的可读错误。
//
// 识别方式：只认标准库在代理拨号失败时产生的 "proxyconnect tcp: ..."
// 前缀。早期用宽泛的 "proxy" 子串匹配，会把与代理无关、但错误文本里
// 恰好含 "proxy" 字样的错误误标成代理故障——错误仍会返回，但排查时
// 会被带偏。
//
// 代理不可达必须表现为「请求失败」，绝不能静默 fallback 到直连
// （那等于把真实出口暴露给上游）。
func wrapProxyError(proxy string, err error) error {
	if err == nil || proxy == "" {
		return err
	}
	if !strings.Contains(err.Error(), "proxyconnect") {
		return err
	}
	return fmt.Errorf("account proxy unreachable (%s): %w", redactProxyURL(proxy), err)
}

// dialThroughProxy 按代理地址拨号一次（用于诊断与测试）。
// 返回底层错误，便于断言「代理不通时确实没走直连」。
func dialThroughProxy(ctx context.Context, proxy, addr string) error {
	u, err := url.Parse(proxy)
	if err != nil {
		return fmt.Errorf("account proxy unreachable (%s): %w", redactProxyURL(proxy), err)
	}
	d := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return fmt.Errorf("account proxy unreachable (%s): %w", redactProxyURL(proxy), err)
	}
	conn.Close()
	return nil
}

// initProxyLogging 在账号入池时打印一次代理绑定情况（脱敏）。
func initProxyLogging(accountID, proxy string) {
	if proxy == "" {
		logger.Info("account %s: direct connection (no proxy)", shortAccountID(accountID))
		return
	}
	logger.Info("account %s: egress proxy bound: %s", shortAccountID(accountID), redactProxyURL(proxy))
}

// ---------------------------------------------------------------------------
// 区域出口诊断
// ---------------------------------------------------------------------------

// 出口登记：记录每个账号最终绑定的出口（脱敏），供诊断接口按区域分组展示。
//
// 为什么需要登记：代理配置来自三处（账号环境变量 / 账号文件 / 区域环境变量），
// 光看配置无法确定「实际生效的是哪个」；而「多个账号共用同一出口」是
// 明确的风控风险（会被上游归为同一使用群体），必须能一眼看出。
var egressRegistry = struct {
	mu    sync.Mutex
	byReg map[string][]egressEntry
	byAcc map[string]egressEntry
}{byReg: map[string][]egressEntry{}, byAcc: map[string]egressEntry{}}

// egressEntry 单个账号的出口绑定记录。
type egressEntry struct {
	AccountID string
	Proxy     string // 脱敏后的出口标识（无凭据）
	Direct    bool
}

// registerEgress 登记账号的出口绑定。
func registerEgress(region, accountID, proxy string) {
	e := egressEntry{AccountID: accountID, Direct: proxy == ""}
	if proxy != "" {
		e.Proxy = redactProxyURL(proxy)
	}
	egressRegistry.mu.Lock()
	defer egressRegistry.mu.Unlock()
	egressRegistry.byAcc[accountID] = e
	key := strings.ToLower(strings.TrimSpace(region))
	list := egressRegistry.byReg[key]
	// 同账号重复登记（重建池）时替换旧记录，避免重复计数。
	replaced := false
	for i := range list {
		if list[i].AccountID == accountID {
			list[i] = e
			replaced = true
			break
		}
	}
	if !replaced {
		list = append(list, e)
	}
	egressRegistry.byReg[key] = list
}

// ForgetEgress 注销该账号的出口登记（账号删除时调用）。
// 残留登记会让诊断接口继续展示已删除账号的出口，误导排障。
func ForgetEgress(accountID string) {
	if accountID == "" {
		return
	}
	egressRegistry.mu.Lock()
	defer egressRegistry.mu.Unlock()
	delete(egressRegistry.byAcc, accountID)
	for region, list := range egressRegistry.byReg {
		out := list[:0]
		for _, e := range list {
			if e.AccountID != accountID {
				out = append(out, e)
			}
		}
		if len(out) == 0 {
			delete(egressRegistry.byReg, region)
			continue
		}
		egressRegistry.byReg[region] = out
	}
}

// RegionEgressStatus 是某区域的出口配置汇总（诊断用）。
type RegionEgressStatus struct {
	Region string `json:"region"`
	// RegionProxy 区域级代理（脱敏）；空 = 未配置。
	RegionProxy string `json:"region_proxy,omitempty"`
	// Accounts 区域内各账号的实际出口（脱敏）。
	Accounts []EgressAccountStatus `json:"accounts"`
	// UniqueEgress 区域内不同出口的数量（1 = 全区共用一个出口）。
	UniqueEgress int `json:"unique_egress"`
	// SharedWarning 共用同一出口的风险提示（为空表示无风险）。
	SharedWarning string `json:"shared_warning,omitempty"`
	// NoDedicatedEgress 该区域没有专用出口时的提示。
	NoDedicatedEgress string `json:"no_dedicated_egress,omitempty"`
}

// EgressAccountStatus 单个账号的出口状态。
type EgressAccountStatus struct {
	AccountID string `json:"account_id"`
	// Egress 脱敏后的出口标识；空 = 直连。
	Egress string `json:"egress,omitempty"`
	Direct bool   `json:"direct"`
}

// RegionEgressStatuses 按区域分组返回出口配置（脱敏）。
//
// 同时标出两类风险：
//   - 区域内多个账号共用同一出口 → 容易被上游归为同一使用群体；
//   - 某区域完全没有专用出口（直连）→ 很可能与其他区域共享同一个出口 IP。
//
// 账号 ID 截断到前 8 位：本函数的结果会经无鉴权的 /health 暴露，
// 完整 ID 内嵌上游 uid 与显示名，不能明文出去（与 risk last 信号口径一致）。
func RegionEgressStatuses() []RegionEgressStatus {
	egressRegistry.mu.Lock()
	defer egressRegistry.mu.Unlock()
	out := make([]RegionEgressStatus, 0, len(egressRegistry.byReg))
	for region, list := range egressRegistry.byReg {
		st := RegionEgressStatus{Region: region}
		if rp := regionProxyFromEnv(region); rp != "" {
			st.RegionProxy = redactProxyURL(rp)
		} else {
			st.NoDedicatedEgress = "该区域未配置专用出口（" + regionProxyEnvName(region) + "），" +
				"账号很可能与其他区域共享同一个出口 IP"
		}
		uniq := map[string]bool{}
		for _, e := range list {
			st.Accounts = append(st.Accounts, EgressAccountStatus{
				AccountID: shortAccountID(e.AccountID),
				Egress:    e.Proxy,
				Direct:    e.Direct,
			})
			key := e.Proxy
			if e.Direct {
				key = "(direct)"
			}
			uniq[key] = true
		}
		st.UniqueEgress = len(uniq)
		if len(list) > 1 && len(uniq) == 1 && !st.Accounts[0].Direct {
			st.SharedWarning = fmt.Sprintf("该区域 %d 个账号共用同一出口，易被上游归为同一使用群体", len(list))
		}
		out = append(out, st)
	}
	return out
}

// regionProxyEnvName 返回该区域对应的环境变量名（诊断提示用）。
func regionProxyEnvName(region string) string {
	if strings.EqualFold(strings.TrimSpace(region), "cn") {
		return ProxyEnvCN
	}
	return ProxyEnvGlobal
}
