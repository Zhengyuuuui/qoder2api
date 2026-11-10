package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"qoder2api/account"
	"qoder2api/internal/cosy"
	"runtime"
	"strings"
	"time"

	"qoder2api/logger"
)

type BearerClient struct {
	sess *cosy.SessionContext
	// fp 非空时启用新版协议（对齐官方桌面端 0.4.8+）：
	//   cosy-clienttype=10 / cosy-version=1.1.69 / user-agent=undici，
	//   并使用 fp 中的真实设备指纹。这是 model/list 额外下发
	//   fast / smodel(Sonus) / cmodel(Cantus) 以及推理通过设备校验的前提。
	// fp 为空时保持旧协议与派生指纹，行为与改造前完全一致。
	fp *account.CheckinFingerprint
	// accountID 该客户端归属的账号 ID（用于按账号缓存出口客户端）。
	// 为空表示不区分账号（单账号 / 调试场景）。
	accountID string
	// region 该客户端归属的区域（global / cn）。
	// 缓存 key 是 (region, account) 维度：不同区域的账号出口不同，
	// 连接池也不能混。
	region string
	// proxy 该账号绑定的出口代理地址（空 = 直连）。
	// 账号与出口的绑定是持久化的：这里存的是最终生效的地址，
	// 不会在每次请求时重算或重选。
	proxy string
}

func NewBearerClient(sess *cosy.SessionContext) *BearerClient {
	return &BearerClient{sess: sess}
}

// NewBearerClientWithFP 构造使用真实设备指纹的客户端（新版协议）。
func NewBearerClientWithFP(sess *cosy.SessionContext, fp *account.CheckinFingerprint) *BearerClient {
	return &BearerClient{sess: sess, fp: fp}
}

// SetAccountProxy 绑定该客户端的账号 ID 与出口代理。
//
// 绑定是持久的：只在账号入池时设置一次，之后每次请求复用同一个
// Transport（连接池复用），不会重新选出口——出口画像必须稳定。
func (c *BearerClient) SetAccountProxy(accountID, proxy string) {
	c.accountID = accountID
	c.proxy = proxy
}

// SetAccountRegion 记录该客户端归属的区域（用于按 (region, account)
// 维度隔离客户端缓存——不同区域的出口不能混用连接池）。
func (c *BearerClient) SetAccountRegion(region string) {
	c.region = region
}

// clientKey 返回该客户端的缓存 key（region|account）。
func (c *BearerClient) clientKey() string {
	if c.region == "" {
		return c.accountID
	}
	return c.region + "|" + c.accountID
}

// httpClient 返回该账号在指定超时档位下的 http.Client。
//
// 超时语义保持不变：callGet 15s / callPost 30s；流式请求不设整体超时
// （长流不能被截断），由 ctx 控制生命周期。
func (c *BearerClient) httpClient(timeout time.Duration) *http.Client {
	return clientForKey(c.clientKey(), c.proxy, timeout)
}

// noteRisk 打点一次上游错误（风控遥测 + 自适应节流信号）。
//
// 打点放在客户端层：排队的错误会在同账号内重试若干次，每次都是一次
// 真实的「上游拥塞」信号——若只在最终失败处打点，排队率会被严重低估。
// 同一个错误同时喂给累计遥测（risk.go）与滑动窗口节流（throttle.go）：
// 前者用于面板总量，后者用于自动降档决策，两者口径必须一致。
//
// model 由调用方从请求体解析后传入（不缓存在 client 上：同一个账号
// 可能并发处理不同模型的请求，缓存模型键会串号）。
func (c *BearerClient) noteRisk(err error, model string) {
	if err == nil {
		return
	}
	recordUpstreamRisk(err, model, c.accountID)
	// 自适应节流信号：与遥测同一分类口径（riskKindFor），排队 / 频控
	// 都算「节奏该收了」的证据；其余错误不喂（避免噪声触发降档）。
	var ue *UpstreamError
	if errors.As(err, &ue) {
		kind := ue.RiskKind
		if kind == "" {
			kind = riskKindFor(ue.Status, ue.Detail, ue.ErrType)
		}
		ThrottleNoteSignal(kind)
	}
}

// modernProtocol 返回该客户端是否启用新版协议。
func (c *BearerClient) modernProtocol() bool {
	return c.fp != nil && c.fp.MachineToken != ""
}

func (c *BearerClient) buildHeaders(pathSig, body, accept string, extra map[string]string) (map[string]string, error) {
	payloadB64, err := cosy.BuildPayloadB64(c.sess.Info)
	if err != nil {
		return nil, err
	}
	date := fmt.Sprintf("%d", cosy.UnixSec())
	sig := cosy.SignRequest(payloadB64, c.sess.CosyKey, date, body, pathSig)
	bearer := cosy.ComposeBearer(payloadB64, sig)

	// 默认旧协议（改造前行为）：CLI 客户端 5 / 1.0.10 + 派生指纹
	clientType := "5"
	version := cosy.Version
	userAgent := "Go-http-client/2.0"
	machineID := c.sess.MachineId
	machineToken := c.sess.MachineToken
	machineType := c.sess.MachineType
	scene := "assistant"
	product := "ide" // 决定配额池：ide 才能走个人拓展包
	dataPolicy := "agree"
	clientIP := ""

	// 配置了真实指纹 → 切换到官方桌面端同款的新版协议
	if c.modernProtocol() {
		fp := c.fp
		clientType = orDefault(fp.ClientType, "10")
		version = orDefault(fp.Version, "1.1.69")
		userAgent = "undici"
		machineID = fp.MachineID
		machineToken = fp.MachineToken
		machineType = fp.MachineType
		scene = orDefault(fp.Scene, "app")
		product = orDefault(fp.Product, "app")
		dataPolicy = orDefault(fp.DataPolicy, "agree")
		clientIP = orDefault(fp.ClientIP, fp.MachineID)
	}

	h := map[string]string{
		"cosy-data-policy":      dataPolicy,
		"content-type":          "application/json",
		"cosy-machinetype":      machineType,
		"cosy-clienttype":       clientType,
		"cosy-date":             date,
		"cosy-user":             c.sess.Identity.Uid,
		"cosy-key":              c.sess.CosyKey,
		"cache-control":         "no-cache",
		"accept":                accept,
		"authorization":         bearer,
		"cosy-version":          version,
		"cosy-machineid":        machineID,
		"cosy-machinetoken":     machineToken,
		"login-version":         "v2",
		"user-agent":            userAgent,
		"cosy-scene":            scene,
		"cosy-business-product": product,
		"cosy-business-type":    "agent",
	}
	if clientIP != "" {
		h["cosy-clientip"] = clientIP
	}
	if c.fp != nil && c.fp.MachineCode != "" {
		h["cosy-machinecode"] = c.fp.MachineCode
	}
	// 客户端形态对齐（官方双端一致）：machineos + machinehostname。
	// 取值策略见 machineOSValue / machineHostnameValue 注释。
	if osHeadersEnabled() {
		h["cosy-machineos"] = machineOSValue(c.fp)
		if hostname := c.machineHostnameValue(c.fp, accept); hostname != "" {
			h["cosy-machinehostname"] = hostname
		}
	}
	for k, v := range extra {
		h[k] = v
	}
	return h, nil
}

// osHeadersEnabled QD_COSY_OS_HEADERS=0 整体关闭 machineos/machinehostname
// 两个形态头（应急回退用）。
func osHeadersEnabled() bool {
	return os.Getenv("QD_COSY_OS_HEADERS") != "0"
}

// machineOSValue 返回 cosy-machineos（{arch}_{platform}，官方 6 种组合：
// x86_64/aarch64 × win32/linux/darwin）。
//
// 默认策略刻意不跟随真实运行平台：ARM / macOS 上也报 x86_64_linux
// （Windows 报 x86_64_win32）——形态突变对上游等于「账号换了一台设备」，
// 存量账号会整体漂移。QD_COSY_MACHINE_OS=auto 才按真实平台识别；
// 其他任意字符串原样使用。配置了真实设备指纹（fp）时以抓包值为准。
func machineOSValue(fp *account.CheckinFingerprint) string {
	if fp != nil && fp.MachineOS != "" {
		return fp.MachineOS
	}
	if v := strings.TrimSpace(os.Getenv("QD_COSY_MACHINE_OS")); v != "" {
		if v == "auto" {
			return archToken(runtime.GOARCH) + "_" + platformToken(runtime.GOOS)
		}
		return v
	}
	if runtime.GOOS == "windows" {
		return "x86_64_win32"
	}
	return "x86_64_linux"
}

func archToken(goarch string) string {
	if goarch == "arm64" {
		return "aarch64"
	}
	return "x86_64"
}

func platformToken(goos string) string {
	switch goos {
	case "windows":
		return "win32"
	case "darwin":
		return "darwin"
	default:
		return "linux"
	}
}

// machineHostnameValue 返回 cosy-machinehostname；非流式请求返回空
// （官方仅在推理 SSE 场景携带该头，其他接口不带）。
//
// 取值优先级：真实设备指纹抓包值 > 按账号稳定派生（DESKTOP-XXXXXXX）。
// 绝不用真实主机名——真实名会暴露网关所在机器，且多账号相同会形成
// 跨账号关联。派生值按账号稳定，不随请求变化。
func (c *BearerClient) machineHostnameValue(fp *account.CheckinFingerprint, accept string) string {
	if accept != "text/event-stream" {
		return ""
	}
	if fp != nil && fp.Hostname != "" {
		return fp.Hostname
	}
	return cosy.DeriveMachineHostname(cosy.FingerprintSeed(c.sess.Identity.Uid, ""))
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func PathSigFrom(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	p := u.Path
	if strings.HasPrefix(p, "/algo") {
		p = p[len("/algo"):]
	}
	return p, nil
}

// CallGetForTest 供 cmd/checkin 调试工具使用的导出 GET 封装。
func (c *BearerClient) CallGetForTest(fullURL string) (map[string]interface{}, error) {
	return c.callGet(fullURL)
}

// callGet 用 cosy 签名发送 GET 请求，body 部分参与签名时为空字符串。
// 用于 /algo/api/v2/model/list 之类的纯查询接口。
func (c *BearerClient) callGet(fullURL string) (map[string]interface{}, error) {
	return c.callGetCandidates("model_list", []string{fullURL})
}

// callGetCandidates 在候选端点间故障转移地发送 GET。
//
// 转移边界见 endpoints.go：只有传输层失败与 5xx 才换端点；4xx 属账号 /
// 模型问题，换端点无意义（换过去只是把同一个错误换个地方报）。
func (c *BearerClient) callGetCandidates(purpose string, candidates []string) (map[string]interface{}, error) {
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no endpoint candidate for %s", purpose)
	}
	ordered := endpoints().OrderedEndpoints(purpose, candidates)
	var lastErr error
	for i, u := range ordered {
		result, err := c.callGetOnce(u)
		if err == nil {
			if i > 0 {
				NoteEndpointSwitch(purpose, ordered[0], u, "recovered on candidate")
			}
			endpoints().NoteEndpointSuccess(purpose, u)
			return result, nil
		}
		lastErr = err
		if !shouldFailoverEndpoint(err) {
			// 4xx / 业务错误：不是端点的问题，换端点无意义
			return nil, err
		}
		cooling := endpoints().NoteEndpointFailure(purpose, u, err)
		if i+1 < len(ordered) {
			NoteEndpointSwitch(purpose, u, ordered[i+1], endpointFailReason(err))
		} else if cooling {
			logger.Info("endpoint %s (purpose=%s) entered cooldown after %d consecutive failures: %v",
				endpointHostOf(u), purpose, endpointFailThreshold(), err)
		}
	}
	return nil, lastErr
}

// callGetOnce 对单个端点执行一次 GET。
func (c *BearerClient) callGetOnce(fullURL string) (map[string]interface{}, error) {
	pathSig, err := PathSigFrom(fullURL)
	if err != nil {
		return nil, err
	}
	headers, err := c.buildHeaders(pathSig, "", "application/json", nil)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("GET", fullURL, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// 每账号独立的 client（承载账号绑定的出口代理），连接池按账号复用。
	client := c.httpClient(15 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		// 代理不可达必须显式失败：绝不能静默 fallback 到直连
		// （那等于把真实出口暴露给上游）。
		return nil, wrapProxyError(c.proxy, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		ue := NewUpstreamError(resp.StatusCode, string(data))
		c.noteRisk(ue, "")
		return nil, ue
	}
	preview := string(data)
	if len(preview) > 2000 {
		preview = preview[:2000]
	}
	logger.Info("callGet %s response (%d bytes): %s", fullURL, len(data), preview)
	var result map[string]interface{}
	err = json.Unmarshal(data, &result)
	return result, err
}

// shouldFailoverEndpoint 判断该错误是否值得换端点重试。
//
// 传输层失败与 5xx → 换（这个集群不通 / 在报错）；
// 4xx 与业务错误 → 不换（账号 / 模型 / 频控问题，换集群结果一样）。
func shouldFailoverEndpoint(err error) bool {
	if err == nil {
		return false
	}
	var ue *UpstreamError
	if errors.As(err, &ue) {
		if ue.Status >= 500 {
			return true
		}
		return false
	}
	// 裸传输错误（TLS EOF / 超时 / 连接重置）
	return IsTransientTransport(err)
}

// endpointFailReason 生成端点切换原因的可读摘要。
func endpointFailReason(err error) string {
	if err == nil {
		return "unknown"
	}
	var ue *UpstreamError
	if errors.As(err, &ue) {
		return fmt.Sprintf("HTTP %d", ue.Status)
	}
	return "transport failure"
}

func (c *BearerClient) callPost(fullURL string, jsonBody interface{}) (map[string]interface{}, error) {
	pathSig, err := PathSigFrom(fullURL)
	if err != nil {
		return nil, err
	}
	var bodyStr string
	if jsonBody != nil {
		plain, err := json.Marshal(jsonBody)
		if err != nil {
			return nil, err
		}
		bodyStr, err = cosy.Encode(plain)
		if err != nil {
			return nil, err
		}
	}
	// 每账号独立的 client（承载账号绑定的出口代理），连接池按账号复用。
	client := c.httpClient(30 * time.Second)
	var lastErr error
	// 瞬时故障同账号快速重试：RetryBackoff（1s/2s）退避，仅瞬时类（418/5xx/provider_error/传输抖动）
	for attempt := 0; attempt <= TransientMaxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(RetryBackoff(attempt))
		}
		headers, err := c.buildHeaders(pathSig, bodyStr, "application/json", nil)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequest("POST", fullURL, strings.NewReader(bodyStr))
		if err != nil {
			return nil, err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			// 代理不可达不是「上游瞬时抖动」：重试没有意义（代理还是不通），
			// 但更不能静默直连。直接返回点名代理地址的错误。
			if werr := wrapProxyError(c.proxy, err); werr != err {
				return nil, werr
			}
			lastErr = err
			if IsTransientTransport(err) && attempt < TransientMaxRetries {
				logger.Info("transient transport error on POST %s (try %d/%d): %s - retry in %ds",
					fullURL, attempt+1, TransientMaxRetries+1, err.Error(), attempt+1)
				continue
			}
			return nil, WrapTransportError(err)
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			detail := string(data)
			ue := NewUpstreamError(resp.StatusCode, detail)
			c.noteRisk(ue, modelKeyFromAny(jsonBody))
			if IsTransientUpstream(resp.StatusCode, detail) && attempt < TransientMaxRetries {
				logger.Info("transient upstream HTTP %d on POST %s (try %d/%d) - retry in %ds",
					resp.StatusCode, fullURL, attempt+1, TransientMaxRetries+1, attempt+1)
				continue
			}
			return nil, ue
		}
		var result map[string]interface{}
		err = json.Unmarshal(data, &result)
		return result, err
	}
	return nil, WrapTransportError(lastErr)
}

// openStreamLines sends a POST, reads SSE lines, calls onLine for each non-empty line.
// onLine 返回 false 表示消费方要求停止读取（如 deltaDispatcher 已捕获信封错误帧），
// 此时立即结束本次流读取并返回，由上层（CallQoderWithOpts 信封重开闸门）决定是否重开。
// ctx 可用于取消流式读取（例如客户端断开连接时）。
//
// 连接阶段（建流之前）带同账号瞬时重试：418/5xx/传输抖动 → 1s/2s 退避最多
// TransientMaxRetries 次。建流成功后的流内错误由上层（第 3 步信封重试）处理。
func (c *BearerClient) openStreamLines(ctx context.Context, fullURL string, jsonBody interface{}, extra map[string]string, onLine func(string) bool) error {
	return c.openStreamCandidates(ctx, "chat_stream", []string{fullURL}, jsonBody, extra, onLine)
}

// openStreamCandidates 在候选端点间故障转移地建立 SSE 流。
//
// 转移只发生在「连接阶段」失败（传输层错误 / 建流前的 5xx）：一旦建流成功
// 并开始投递内容，再换端点重开会导致客户端收到重复内容——那由调用方的
// 信封重开闸门负责，不在本层做。
//
// 重试分工：有多个候选时不在同一端点上反复重试（RetryBackoff 是秒级的，
// 等完再换端点等于白白拖慢响应），直接换下一个端点；只有一个候选时才
// 沿用原有的同端点重试（那时换端点也没得换）。
func (c *BearerClient) openStreamCandidates(ctx context.Context, purpose string, candidates []string, jsonBody interface{}, extra map[string]string, onLine func(string) bool) error {
	if len(candidates) == 0 {
		return fmt.Errorf("no endpoint candidate for %s", purpose)
	}
	ordered := endpoints().OrderedEndpoints(purpose, candidates)
	sameEndpointRetry := len(ordered) == 1
	var lastErr error
	for i, u := range ordered {
		err := c.openStreamLinesOnce(ctx, u, jsonBody, extra, onLine, sameEndpointRetry)
		if err == nil {
			if i > 0 {
				NoteEndpointSwitch(purpose, ordered[0], u, "recovered on candidate")
			}
			endpoints().NoteEndpointSuccess(purpose, u)
			return nil
		}
		lastErr = err
		if !shouldFailoverEndpoint(err) {
			// 4xx / 业务错误：不是端点的问题，换端点无意义
			return err
		}
		cooling := endpoints().NoteEndpointFailure(purpose, u, err)
		if i+1 < len(ordered) {
			NoteEndpointSwitch(purpose, u, ordered[i+1], endpointFailReason(err))
		} else if cooling {
			logger.Info("endpoint %s (purpose=%s) entered cooldown after %d consecutive failures: %v",
				endpointHostOf(u), purpose, endpointFailThreshold(), err)
		}
	}
	return lastErr
}

// openStreamLinesOnce 对单个端点建立 SSE 流并逐行回调。
//
// allowSameEndpointRetry=true 时在同端点上做瞬时重试（只有一个候选可用时）。
func (c *BearerClient) openStreamLinesOnce(ctx context.Context, fullURL string, jsonBody interface{}, extra map[string]string, onLine func(string) bool, allowSameEndpointRetry bool) error {
	pathSig, err := PathSigFrom(fullURL)
	if err != nil {
		return err
	}
	plain, err := json.Marshal(jsonBody)
	if err != nil {
		return err
	}
	bodyStr, err := cosy.Encode(plain)
	if err != nil {
		return err
	}
	// 不设整体 Timeout，改由 context 控制生命周期，避免长流式响应被截断
	// 每账号独立的 client（承载账号绑定的出口代理），连接池按账号复用。
	client := c.httpClient(0)

	var lastErr error
	// maxAttempts：允许同端点重试时沿用原有的 TransientMaxRetries 次；
	// 不允许时只试一次（失败就交给上层换端点，别在这里空等退避）。
	maxAttempts := 0
	if allowSameEndpointRetry {
		maxAttempts = TransientMaxRetries
	}
	for attempt := 0; attempt <= maxAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(RetryBackoff(attempt))
		}
		headers, err := c.buildHeaders(pathSig, bodyStr, "text/event-stream", extra)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, "POST", fullURL, strings.NewReader(bodyStr))
		if err != nil {
			return err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			// 代理不可达直接失败（不重试、不直连）。
			if werr := wrapProxyError(c.proxy, err); werr != err {
				return werr
			}
			lastErr = err
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if IsTransientTransport(err) && attempt < maxAttempts {
				logger.Info("transient transport error opening stream %s (try %d/%d): %s - retry in %ds",
					fullURL, attempt+1, maxAttempts+1, err.Error(), attempt+1)
				continue
			}
			return WrapTransportError(err)
		}
		if resp.StatusCode != 200 {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			detail := string(body)
			ue := NewUpstreamError(resp.StatusCode, detail)
			c.noteRisk(ue, modelKeyFromAny(jsonBody))
			if IsTransientUpstream(resp.StatusCode, detail) && attempt < maxAttempts {
				logger.Info("transient upstream HTTP %d opening stream %s (try %d/%d) - retry in %ds",
					resp.StatusCode, fullURL, attempt+1, maxAttempts+1, attempt+1)
				continue
			}
			return ue
		}

		// 建流成功：进入流式读取，不再在本层重试
		defer resp.Body.Close()
		lineCh := make(chan string)
		errCh := make(chan error, 1)
		// done：主循环提前退出（onLine 返回 false）时关闭，解锁 scanner goroutine，
		// 避免其阻塞在 lineCh 发送处、仅剩 ctx.Done 一条出路造成 goroutine 泄漏
		done := make(chan struct{})
		defer close(done)
		go func() {
			scanner := bufio.NewScanner(resp.Body)
			scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
			for scanner.Scan() {
				select {
				case lineCh <- scanner.Text():
				case <-ctx.Done():
					errCh <- ctx.Err()
					return
				case <-done:
					return
				}
			}
			if err := scanner.Err(); err != nil {
				errCh <- err
				return
			}
			errCh <- nil
		}()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case err := <-errCh:
				logger.Debug("stream read complete")
				return err
			case line := <-lineCh:
				if line != "" {
					// onLine 返回 false：消费方已置位 upstreamErr（信封错误帧），
					// 立即停止读取，避免上游不关流时请求挂起；
					// 错误由 CallQoderWithOpts 的 emitted 闸门 + isRetryableStreamError 完成重开
					if !onLine(line) {
						return nil
					}
				}
			}
		}
	}
	// 防御性兜底：所有失败分支均在循环内直接 return，正常不可达
	return WrapTransportError(lastErr)
}

// sleepCtx 可被 ctx 取消的退避等待。
func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
