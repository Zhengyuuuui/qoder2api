package bridge

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"qoder2api/internal/cosy"
	"time"
)

func ProbeCNEndpoints(token string) {
	// First, get user info
	req, _ := http.NewRequest("GET", "https://openapi.qoder.com.cn/api/v1/userinfo", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Printf("userinfo error: %v\n", err)
		return
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	fmt.Printf("=== userinfo (openapi.qoder.com.cn) ===\n%s\n\n", string(body))

	var userInfo map[string]interface{}
	json.Unmarshal(body, &userInfo)

	uid := ""
	if id, ok := userInfo["id"].(string); ok {
		uid = id
	}

	// Create session（稳定指纹：uid 已知，优先 uid 派生）
	seed := cosy.FingerprintSeed(uid, token)
	mid := cosy.DeriveMachineID(seed)
	mtoken := cosy.DeriveMachineToken(seed)
	mtype := cosy.DeriveMachineType(seed)

	identity := cosy.AuthIdentity{
		Name:               "",
		Aid:                uid,
		Uid:                uid,
		SecurityOauthToken: token,
	}

	sess, err := cosy.NewSession(identity, mid, mtoken, mtype)
	if err != nil {
		fmt.Printf("session error: %v\n", err)
		return
	}

	// Try multiple potential model list URLs
	urls := []string{
		"https://api2-v2.qoder.sh/algo/api/v2/model/list?Encode=1",
		"https://openapi.qoder.com.cn/algo/api/v2/model/list?Encode=1",
		"https://gateway.qoder.com.cn/algo/api/v2/model/list?Encode=1",
	}

	for _, url := range urls {
		fmt.Printf("=== Trying: %s ===\n", url)

		payloadB64, _ := cosy.BuildPayloadB64(sess.Info)
		date := fmt.Sprintf("%d", cosy.UnixSec())

		// Extract path for signature
		pathSig := url
		for _, prefix := range []string{"https://api2-v2.qoder.sh", "https://openapi.qoder.com.cn", "https://gateway.qoder.com.cn"} {
			if len(pathSig) > len(prefix) && pathSig[:len(prefix)] == prefix {
				pathSig = pathSig[len(prefix):]
				break
			}
		}

		sig := cosy.SignRequest(payloadB64, sess.CosyKey, date, "", pathSig)
		bearer := cosy.ComposeBearer(payloadB64, sig)

		req2, _ := http.NewRequest("GET", url, nil)
		req2.Header.Set("cosy-data-policy", "agree")
		req2.Header.Set("content-type", "application/json")
		req2.Header.Set("cosy-machinetype", mtype)
		req2.Header.Set("cosy-clienttype", "5")
		req2.Header.Set("cosy-date", date)
		req2.Header.Set("cosy-user", uid)
		req2.Header.Set("cosy-key", sess.CosyKey)
		req2.Header.Set("cache-control", "no-cache")
		req2.Header.Set("accept", "application/json")
		req2.Header.Set("authorization", bearer)
		req2.Header.Set("cosy-version", cosy.Version)
		req2.Header.Set("cosy-machineid", mid)
		req2.Header.Set("cosy-machinetoken", mtoken)
		req2.Header.Set("login-version", "v2")
		req2.Header.Set("user-agent", "Go-http-client/2.0")
		req2.Header.Set("cosy-scene", "assistant")
		req2.Header.Set("cosy-business-product", "cli")
		req2.Header.Set("cosy-business-type", "agent")

		client := &http.Client{Timeout: 15 * time.Second}
		resp2, err := client.Do(req2)
		if err != nil {
			fmt.Printf("  error: %v\n\n", err)
			continue
		}
		body2, _ := io.ReadAll(resp2.Body)
		resp2.Body.Close()

		if resp2.StatusCode == 200 {
			var result map[string]interface{}
			json.Unmarshal(body2, &result)
			prettyJSON, _ := json.MarshalIndent(result, "", "  ")
			fmt.Printf("  SUCCESS:\n%s\n\n", string(prettyJSON))
		} else {
			fmt.Printf("  HTTP %d: %s\n\n", resp2.StatusCode, string(body2)[:500])
		}
	}
}
