#!/usr/bin/env python3
"""
Qoder CN 自动签到脚本（基于抓包分析的 API 链路）

链路：
  1. GET  /sash/api/v1/me/campaigns           → 找 CLAIMABLE 的 CLAIM_BENEFIT 活动
  2. POST /sash/api/v1/me/campaigns/{id}/claim → 领取

认证：仅需 Bearer device token + cosy-clienttype: 10，无需签名、无需 body
"""

import json
import sys
import ssl
import http.client
from pathlib import Path

BASE_HOST = "openapi.qoder.com.cn"

# 关键：桌面端标识（抓包确认 cosy-clienttype=10）
APP_HEADERS = {
    "accept": "application/json",
    "accept-language": "zh-CN",
    "user-agent": "Qoder",
    "cosy-clienttype": "10",
}


def _ssl_ctx():
    ctx = ssl.create_default_context()
    for cafile in ["/etc/ssl/cert.pem", "/usr/local/etc/ca-certificates.crt"]:
        try:
            ctx.load_verify_locations(cafile)
            break
        except Exception:
            continue
    return ctx


def _request(method, path, token, extra_headers=None, timeout=60):
    """发送请求，返回 (status, parsed_body)"""
    headers = dict(APP_HEADERS)
    headers["authorization"] = "Bearer " + token
    if extra_headers:
        headers.update(extra_headers)

    ctx = _ssl_ctx()
    conn = http.client.HTTPSConnection(BASE_HOST, timeout=timeout, context=ctx)
    conn.request(method, path, headers=headers)
    resp = conn.getresponse()
    raw = resp.read().decode("utf-8", errors="replace")
    conn.close()

    body = None
    try:
        body = json.loads(raw) if raw else None
    except Exception:
        body = raw
    return resp.status, body


def load_token(account_name="nick8034851628"):
    """从 qoder2api 数据目录加载指定账号的 device token"""
    home = Path.home()
    for instance in [".qoder2api-cn", ".qoder2api-global", ".qoder2api"]:
        accounts_dir = home / instance / "accounts"
        secrets_dir = home / instance / "secrets"
        if not accounts_dir.exists():
            continue
        for acc_file in accounts_dir.glob("*.json"):
            try:
                acc = json.loads(acc_file.read_text())
            except Exception:
                continue
            if acc.get("name") != account_name:
                continue
            secret_file = secrets_dir / f"{acc['id']}.token"
            if not secret_file.exists():
                continue
            secret = secret_file.read_text().strip()
            if secret.startswith("{"):
                return json.loads(secret).get("device_token", "")
            return secret
    return None


def main():
    account_name = sys.argv[1] if len(sys.argv) > 1 else "nick8034851628"

    token = load_token(account_name)
    if not token:
        print(f"❌ 未找到账号 {account_name} 的 token")
        sys.exit(1)

    print(f"账号: {account_name}")
    print(f"Token: {token[:20]}...")
    print(f"Host: {BASE_HOST}")
    print()

    # Step 1: 查询活动列表
    print("[1/2] 查询活动列表...")
    status, body = _request("GET", "/sash/api/v1/me/campaigns", token)
    if status != 200:
        print(f"  ❌ HTTP {status}: {json.dumps(body, ensure_ascii=False)[:500]}")
        sys.exit(1)

    if not isinstance(body, dict):
        print(f"  ❌ 响应格式异常: {body}")
        sys.exit(1)

    print(f"  ✅ HTTP 200, claimable={body.get('claimable')}")
    campaigns = body.get("campaigns", [])
    print(f"  找到 {len(campaigns)} 个活动")

    # 找可领取的 CLAIM_BENEFIT 活动（每日 100 Credits）
    target = None
    for c in campaigns:
        cid = c.get("campaignId", "?")
        key = c.get("campaignKey", "?")
        atype = c.get("actionType", "?")
        cstatus = c.get("claimStatus", "?")
        benefit = c.get("benefit") or {}
        amount = benefit.get("amount", "-")
        kind = benefit.get("kind", "-")
        print(f"    - {key} ({cid[:8]}...)")
        print(f"      type={atype} status={cstatus} benefit={amount} {kind}")

        if atype == "CLAIM_BENEFIT" and cstatus == "CLAIMABLE":
            target = c

    if not target:
        print()
        print("⚠️  没有可领取的 CLAIM_BENEFIT 活动")
        # 检查是否已领取
        for c in campaigns:
            if c.get("actionType") == "CLAIM_BENEFIT" and c.get("claimStatus") == "CLAIMED":
                print("   今日已领取过了")
        sys.exit(0)

    campaign_id = target["campaignId"]
    campaign_key = target.get("campaignKey", "?")
    print()
    print(f"[2/2] 领取活动 {campaign_key} ...")

    # Step 2: 领取（空 body，仅靠 token + cosy 头）
    status, body = _request(
        "POST",
        f"/sash/api/v1/me/campaigns/{campaign_id}/claim",
        token,
        extra_headers={"origin": f"https://{BASE_HOST}"},
    )

    if status == 200 and isinstance(body, dict):
        st = body.get("status", "?")
        replayed = body.get("replayed", False)
        benefit = body.get("benefit") or {}
        amount = benefit.get("amount", "-")
        expires = body.get("expiresAt", "-")

        if st == "CLAIMED":
            if replayed:
                print(f"  ✅ 今日已领取过（幂等返回，replayed=true）")
            else:
                print(f"  🎉 领取成功！{amount} Credits，有效期至 {expires}")
        else:
            print(f"  ⚠️ status={st}")
            print(f"  {json.dumps(body, ensure_ascii=False)}")
    else:
        print(f"  ❌ HTTP {status}")
        print(f"  {json.dumps(body, ensure_ascii=False, indent=2) if isinstance(body, dict) else body}")


if __name__ == "__main__":
    main()
