package main

import (
	"encoding/json"
	"fmt"
	"os"

	"qoder2api/account"
	"qoder2api/internal/bridge"
	"qoder2api/internal/cosy"
)

func main() {
	home := os.Getenv("HOME")
	account.SetDataRoot(home + "/.qoder2api-cn")

	// 与主程序共享 settings.json 的指纹盐，保证同一部署内指纹一致
	if salt, err := account.EnsureMachineSalt(); err == nil {
		cosy.SetInstallSalt(salt)
	} else {
		// 调试工具保持可运行（不改变退出行为），仅向 stderr 告警降级后果
		fmt.Fprintln(os.Stderr, "[checkin] init machine salt failed:", err,
			"；运行于无盐模式（hub 兼容指纹），下次启动恢复加盐将导致全部账号指纹漂移")
	}

	accounts, _ := account.List()
	acct := accounts[0]
	token, _ := account.GetSecret(acct.ID)
	deviceToken, refreshToken := bridge.ParseOAuthSecret(token)
	if deviceToken == "" {
		deviceToken = token
	}

	userInfo, _ := bridge.FetchUserInfoWithToken(deviceToken, acct.Region)
	uid := bridge.ResolveOAuthUserID(userInfo)
	name := bridge.StrVal(userInfo, "name")

	// 稳定指纹：uid 已知，按 uid 派生（与 bridge 会话同一套机器码）
	seed := cosy.FingerprintSeed(uid, deviceToken)
	mid := cosy.DeriveMachineID(seed)
	mtoken := cosy.DeriveMachineToken(seed)
	mtype := cosy.DeriveMachineType(seed)
	identity := cosy.AuthIdentity{
		Name:               name,
		Aid:                uid,
		Uid:                uid,
		OrganizationId:     bridge.StrVal(userInfo, "organization_id"),
		OrganizationName:   bridge.StrVal(userInfo, "organization_name"),
		UserType:           bridge.StrValDefault(userInfo, "userType", "personal_standard"),
		SecurityOauthToken: deviceToken,
		RefreshToken:       refreshToken,
	}
	sess, _ := cosy.NewSession(identity, mid, mtoken, mtype)
	client := bridge.NewBearerClient(sess)

	fmt.Printf("User: %s\n\n", name)

	endpoints := []string{
		"https://gateway.qoder.com.cn/algo/api/v2/activity",
		"https://gateway.qoder.com.cn/algo/api/v2/activity/claim/eligibility",
		"https://gateway.qoder.com.cn/algo/api/v2/activity/quota/reset/latest",
		"https://openapi.qoder.com.cn/api/v2/activity",
		"https://openapi.qoder.com.cn/api/v2/activity/claim/eligibility",
		"https://openapi.qoder.com.cn/api/v2/user/plan",
	}

	for _, ep := range endpoints {
		fmt.Printf("=== GET %s ===\n", ep)
		result, err := client.CallGetForTest(ep)
		if err != nil {
			fmt.Printf("  error: %v\n\n", err)
			continue
		}
		pretty, _ := json.MarshalIndent(result, "  ", "  ")
		fmt.Printf("  %s\n\n", string(pretty))
	}
}
