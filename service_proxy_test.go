package main

import (
	"strings"
	"testing"
	"time"

	"qoder2api/account"
	"qoder2api/internal/bridge"
)

// TestAddAccountByPATRejectsRegionProxyConfigErr 审计 P1-5：PAT 交换
// 发生在账号 ID 之前的阶段，按区域代理出口；区域代理配置非法时必须
// 拒绝导入（绝不静默直连——导入时的一跳直连就足以固定真实出口画像）。
func TestAddAccountByPATRejectsRegionProxyConfigErr(t *testing.T) {
	orig := account.DataRoot()
	account.SetDataRoot(t.TempDir())
	t.Cleanup(func() { account.SetDataRoot(orig) })

	t.Setenv("QD_PROXY_CN", "socks5://127.0.0.1:1080")
	svc := NewService(nil)
	if _, err := svc.AddAccountByPAT("pat-token", "cn"); err == nil {
		t.Fatal("region proxy config error must block PAT import")
	} else if !strings.Contains(err.Error(), "代理配置非法") {
		t.Errorf("error should explain the refusal: %v", err)
	}
}

// TestGetAccountQuotaRejectsRegionProxyConfigErr 额度查询的 token 交换
// 同样按区域代理出口：配置非法 → 拒绝（不直连）。
func TestGetAccountQuotaRejectsRegionProxyConfigErr(t *testing.T) {
	orig := account.DataRoot()
	account.SetDataRoot(t.TempDir())
	t.Cleanup(func() { account.SetDataRoot(orig) })

	acct := &account.Account{ID: "acct-quota-proxy", Name: "quota", Region: account.RegionCN, AuthMode: "oauth"}
	if err := account.Save(acct); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := account.SaveSecret("acct-quota-proxy", `{"refresh_token":"drt-refresh-token"}`); err != nil {
		t.Fatalf("save secret: %v", err)
	}
	t.Setenv("QD_PROXY_CN", "socks5://127.0.0.1:1080")

	svc := NewService(nil)
	if _, err := svc.GetAccountQuota("acct-quota-proxy"); err == nil {
		t.Fatal("region proxy config error must block quota query")
	} else if !strings.Contains(err.Error(), "代理配置非法") {
		t.Errorf("error should explain the refusal: %v", err)
	}
}

// TestRefreshAccountRejectsRegionProxyConfigErr 自动续期的 jobToken 交换
// 同样不得静默直连（续期在后台跑，直连了也没人知道）。
func TestRefreshAccountRejectsRegionProxyConfigErr(t *testing.T) {
	orig := account.DataRoot()
	account.SetDataRoot(t.TempDir())
	t.Cleanup(func() { account.SetDataRoot(orig) })

	acct := &account.Account{ID: "acct-refresh-proxy", Name: "refresh", Region: account.RegionCN, AuthMode: "oauth"}
	if err := account.Save(acct); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := account.SaveSecret("acct-refresh-proxy", `{"refresh_token":"drt-refresh-token"}`); err != nil {
		t.Fatalf("save secret: %v", err)
	}
	t.Setenv("QD_PROXY_CN", "socks5://127.0.0.1:1080")

	rep := refreshAccount(acct, true)
	if rep.Status != "failed" {
		t.Errorf("refresh status = %q, want failed", rep.Status)
	}
	if !strings.Contains(rep.Message, "代理配置非法") {
		t.Errorf("refresh should refuse on region proxy config error: %q", rep.Message)
	}
}

// TestDeleteAccountCleansUpClientsAndEgress 审计 P3-2：删除账号必须
// 清理出口客户端缓存与出口登记，否则残留 Transport（含空闲连接）与
// 诊断里幽灵账号。
func TestDeleteAccountCleansUpClientsAndEgress(t *testing.T) {
	orig := account.DataRoot()
	account.SetDataRoot(t.TempDir())
	t.Cleanup(func() { account.SetDataRoot(orig) })

	svc := NewService(nil)
	if err := account.Save(&account.Account{ID: "acct-del-1", Name: "del", Region: account.RegionCN}); err != nil {
		t.Fatalf("save: %v", err)
	}
	// 造一个客户端缓存（与删除相同的 key 维度）
	const proxy = "http://127.0.0.1:1"
	before := bridge.HTTPClientForKey("cn|acct-del-1", proxy, 15*time.Second).Transport

	if err := svc.DeleteAccount("acct-del-1"); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	// 再取一次：缓存应已被清掉（重建的新 Transport 与之前不同）
	after := bridge.HTTPClientForKey("cn|acct-del-1", proxy, 15*time.Second).Transport
	if after == before {
		t.Error("DeleteAccount must forget the account's cached egress client")
	}
}
