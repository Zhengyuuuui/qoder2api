package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"qoder2api/internal/cosy"
	"strings"
	"time"

	"qoder2api/logger"
)

type BearerClient struct {
	sess *cosy.SessionContext
}

func NewBearerClient(sess *cosy.SessionContext) *BearerClient {
	return &BearerClient{sess: sess}
}

func (c *BearerClient) buildHeaders(pathSig, body, accept string, extra map[string]string) (map[string]string, error) {
	payloadB64, err := cosy.BuildPayloadB64(c.sess.Info)
	if err != nil {
		return nil, err
	}
	date := fmt.Sprintf("%d", cosy.UnixSec())
	sig := cosy.SignRequest(payloadB64, c.sess.CosyKey, date, body, pathSig)
	bearer := cosy.ComposeBearer(payloadB64, sig)

	h := map[string]string{
		"cosy-data-policy":      "agree",
		"content-type":          "application/json",
		"cosy-machinetype":      c.sess.MachineType,
		"cosy-clienttype":       "5",
		"cosy-date":             date,
		"cosy-user":             c.sess.Identity.Uid,
		"cosy-key":              c.sess.CosyKey,
		"cache-control":         "no-cache",
		"accept":                accept,
		"authorization":         bearer,
		"cosy-version":          cosy.Version,
		"cosy-machineid":        c.sess.MachineId,
		"cosy-machinetoken":     c.sess.MachineToken,
		"login-version":         "v2",
		"user-agent":            "Go-http-client/2.0",
		"cosy-scene":            "assistant",
		"cosy-business-product": "ide",
		"cosy-business-type":    "agent",
	}
	for k, v := range extra {
		h[k] = v
	}
	return h, nil
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
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, NewUpstreamError(resp.StatusCode, string(data))
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
	client := &http.Client{Timeout: 30 * time.Second}
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
			if IsTransientUpstream(resp.StatusCode, detail) && attempt < TransientMaxRetries {
				logger.Info("transient upstream HTTP %d on POST %s (try %d/%d) - retry in %ds",
					resp.StatusCode, fullURL, attempt+1, TransientMaxRetries+1, attempt+1)
				continue
			}
			return nil, NewUpstreamError(resp.StatusCode, detail)
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
	client := &http.Client{}

	var lastErr error
	for attempt := 0; attempt <= TransientMaxRetries; attempt++ {
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
			lastErr = err
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if IsTransientTransport(err) && attempt < TransientMaxRetries {
				logger.Info("transient transport error opening stream %s (try %d/%d): %s - retry in %ds",
					fullURL, attempt+1, TransientMaxRetries+1, err.Error(), attempt+1)
				continue
			}
			return WrapTransportError(err)
		}
		if resp.StatusCode != 200 {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			detail := string(body)
			if IsTransientUpstream(resp.StatusCode, detail) && attempt < TransientMaxRetries {
				logger.Info("transient upstream HTTP %d opening stream %s (try %d/%d) - retry in %ds",
					resp.StatusCode, fullURL, attempt+1, TransientMaxRetries+1, attempt+1)
				continue
			}
			return NewUpstreamError(resp.StatusCode, detail)
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
