package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
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
		"cosy-business-product":      "ide",
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
		return nil, fmt.Errorf("HTTP %d body=%s", resp.StatusCode, string(data))
	}
	preview := string(data)
	if len(preview) > 2000 {
		preview = preview[:2000]
	}
	logger.Info("callGet %s response (%d bytes): %s", fullURL, len(data), preview)
	// debug: 按 URL 区分写入不同文件
	if strings.Contains(fullURL, "model/list") {
		if strings.Contains(fullURL, "gateway.qoder.com.cn") {
			_ = os.WriteFile("/tmp/qccg-model-list-cn.json", data, 0644)
		} else {
			_ = os.WriteFile("/tmp/qccg-model-list-global.json", data, 0644)
		}
	}
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
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d body=%s", resp.StatusCode, string(data))
	}
	var result map[string]interface{}
	err = json.Unmarshal(data, &result)
	return result, err
}

// openStreamLines sends a POST, reads SSE lines, calls onLine for each non-empty line.
// ctx 可用于取消流式读取（例如客户端断开连接时）。
func (c *BearerClient) openStreamLines(ctx context.Context, fullURL string, jsonBody interface{}, extra map[string]string, onLine func(string)) error {
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
	// 不设整体 Timeout，改由 context 控制生命周期，避免长流式响应被截断
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d %s", resp.StatusCode, string(body))
	}

	lineCh := make(chan string)
	errCh := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
		for scanner.Scan() {
			select {
			case lineCh <- scanner.Text():
			case <-ctx.Done():
				errCh <- ctx.Err()
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
				onLine(line)
			}
		}
	}
}
