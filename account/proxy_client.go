package account

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// 账号包内的区域级出口客户端（最小实现）。
//
// 为什么 account 包要自己一份：bridge 包导入 account（账号模型），account
// 不能反向导入 bridge（循环依赖）；而账号阶段的流量——设备登录轮询 /
// userinfo / plan / 续期 / 额度查询——同样必须走区域代理出口。若这些
// 请求直连，「对话走代理、账号操作走数据中心」的双出口画像正是上游
// 关联「同一使用群体」的典型信号。
//
// 功能刻意最小：只做区域级（按区域缓存 Transport）。账号级出口由 bridge
// 在账号入池时绑定（bindProxy），那个阶段还没有账号 ID 可绑。

// 区域级代理环境变量（与 bridge 包同名同值——两端必须读同一组变量，
// 否则「对话走代理、账号操作直连」的割裂会再次出现）。
const (
	ProxyEnvGlobal = "QD_PROXY_GLOBAL"
	ProxyEnvCN     = "QD_PROXY_CN"
)

// RegionProxyURL 读取区域级代理配置（未配置返回空串 = 直连合法）。
func RegionProxyURL(region Region) string {
	key := ProxyEnvGlobal
	if strings.EqualFold(strings.TrimSpace(string(region)), "cn") {
		key = ProxyEnvCN
	}
	return strings.TrimSpace(os.Getenv(key))
}

// RegionProxyConfigured 该区域是否配置了区域级代理。
func RegionProxyConfigured(region Region) bool {
	return RegionProxyURL(region) != ""
}

// regionTransportCache 按区域缓存 Transport（连接池复用）。
//
// 为什么缓存 Transport 而不是 http.Client：调用方的超时档位各不相同
// （轮询 15s / userinfo 10s / 额度 15s……）。若按「区域+代理」缓存
// client，后到的调用方会拿到第一个调用方的超时——要么过早掐断长请求，
// 要么让短请求挂满别人的超时。Transport 与超时无关，正好按区域缓存；
// http.Client 每次新建（很轻），包住共享 Transport 即可同时复用连接。
var regionTransportCache = struct {
	mu         sync.Mutex
	transports map[string]http.RoundTripper
}{transports: map[string]http.RoundTripper{}}

// regionDirectTransport 未配置代理时的共享 Transport（直连也要复用连接池，
// 否则每次调用新建 Transport = 每次新开 TCP/TLS，账号阶段流量白付握手）。
var (
	regionDirectOnce      sync.Once
	regionDirectTransport http.RoundTripper
)

func directRegionTransport() http.RoundTripper {
	regionDirectOnce.Do(func() {
		regionDirectTransport = &http.Transport{
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
	})
	return regionDirectTransport
}

// RegionHTTPClient 返回该区域的 HTTP client（按区域代理出口，连接池按
// 区域共享；超时按调用方档位各自生效）。
//
// 返回 error 的唯一情况：区域配了代理但非法（socks5 / 无 host / 解析
// 失败）——此时绝不静默直连（真实出口暴露比请求失败更糟），由调用方
// 把错误呈现给用户。
func RegionHTTPClient(region Region, timeout time.Duration) (*http.Client, error) {
	proxy := RegionProxyURL(region)
	if proxy == "" {
		// 未配置：直连是合法路径（共享 Transport 复用连接）
		return &http.Client{Transport: directRegionTransport(), Timeout: timeout}, nil
	}
	u, err := url.Parse(proxy)
	if err != nil {
		return nil, fmt.Errorf("region %s proxy url unparseable (check for spaces or typos)", region)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return nil, fmt.Errorf("region %s proxy scheme %q unsupported (only http/https; "+
			"for SOCKS5 run a local http forwarder and point the proxy at it)", region, u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("region %s proxy url has no host", region)
	}

	key := string(region) + "|" + proxy
	regionTransportCache.mu.Lock()
	tr, ok := regionTransportCache.transports[key]
	if !ok {
		tr = &http.Transport{
			Proxy:                 http.ProxyURL(u),
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
		regionTransportCache.transports[key] = tr
	}
	regionTransportCache.mu.Unlock()
	return &http.Client{Transport: tr, Timeout: timeout}, nil
}
