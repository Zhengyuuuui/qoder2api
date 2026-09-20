#!/usr/bin/env python3
"""
Qoder CN 自动签到脚本
每日领取 100 Credits
"""

import json
import sys
import argparse
import urllib.request
import urllib.error
import uuid
import base64
import hashlib
import ssl
import time
from pathlib import Path


BASE_URL = "https://openapi.qoder.com.cn"


def load_token_from_qoder2api(account_name=None):
    """从 qoder2api 数据目录加载 token"""
    home = Path.home()
    for instance in [".qoder2api-cn", ".qoder2api-global", ".qoder2api"]:
        data_dir = home / instance
        if not data_dir.exists():
            continue
        accounts_dir = data_dir / "accounts"
        secrets_dir = data_dir / "secrets"
        if not accounts_dir.exists():
            continue
        for acc_file in accounts_dir.glob("*.json"):
            try:
                acc = json.loads(acc_file.read_text())
            except Exception:
                continue
            if account_name and acc.get("name") != account_name:
                continue
            acc_id = acc.get("id")
            secret_file = secrets_dir / f"{acc_id}.token"
            if not secret_file.exists():
                continue
            try:
                secret = secret_file.read_text().strip()
                if secret.startswith("{"):
                    parsed = json.loads(secret)
                    device_token = parsed.get("device_token", "")
                    refresh_token = parsed.get("refresh_token", "")
                else:
                    device_token = secret
                    refresh_token = ""
                return {
                    "name": acc.get("name", acc_id),
                    "id": acc_id,
                    "device_token": device_token,
                    "refresh_token": refresh_token,
                    "region": acc.get("region", "cn"),
                }
            except Exception as e:
                print(f"Failed to read secret for {acc_id}: {e}")
                continue
    return None


def build_cosy_headers(machine_id=None):
    """构建 cosy headers（参考 Qoder CN IDE 实现）"""
    if machine_id is None:
        machine_id = str(uuid.uuid4())
    import platform
    machine_os = f"aarch64_darwin" if platform.machine() == "arm64" else f"x86_64_darwin"

    # generate machine token (base64 url-safe)
    raw = (uuid.uuid4().hex + uuid.uuid4().hex)[:50]
    machine_token = base64.urlsafe_b64encode(raw.encode()).decode()

    return {
        "Cosy-Version": "1.0.10",
        "Cosy-MachineToken": machine_token,
        "Cosy-MachineType": uuid.uuid4().hex[:18],
        "Cosy-MachineCode": hashlib.md5(machine_id.encode()).hexdigest(),
        "Cosy-MachineId": machine_id,
        "Cosy-MachineHostname": "localhost",
        "Cosy-MachineOS": machine_os,
        "Cosy-ClientType": "0",
        "Accept-Language": "zh-CN",
        "User-Agent": "Qoder-CN-IDE/1.0.0",
    }


def _build_ssl_context():
    """构建 SSL context：优先使用 certifi，失败则回退到系统证书或创建默认 context"""
    try:
        import certifi
        return ssl.create_default_context(cafile=certifi.where())
    except ImportError:
        pass
    ctx = ssl.create_default_context()
    # macOS 上 Python 可能找不到系统根证书，尝试常见路径
    for cafile in [
        "/etc/ssl/cert.pem",
        "/usr/local/etc/openssl/cert.pem",
        "/opt/homebrew/etc/openssl@3/cert.pem",
    ]:
        try:
            ctx.load_verify_locations(cafile)
            return ctx
        except Exception:
            continue
    return ctx


_SSL_CONTEXT = None


def _get_ssl_context():
    global _SSL_CONTEXT
    if _SSL_CONTEXT is None:
        _SSL_CONTEXT = _build_ssl_context()
    return _SSL_CONTEXT


def http_request(method, path, token=None, cosy_headers=None, body=None, timeout=60):
    """发送 HTTP 请求"""
    url = BASE_URL + path
    headers = {}
    if cosy_headers:
        headers.update(cosy_headers)
    if token:
        headers["Authorization"] = "Bearer " + token
    if body is not None:
        headers["Content-Type"] = "application/json"
        data = json.dumps(body).encode()
    else:
        data = None

    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout, context=_get_ssl_context()) as resp:
            raw = resp.read().decode("utf-8", errors="replace")
            return resp.status, raw
    except urllib.error.HTTPError as e:
        raw = e.read().decode("utf-8", errors="replace")
        return e.code, raw
    except Exception as e:
        return 0, str(e)


def get_activities(token, cosy_headers, timeout=60):
    """获取活动列表"""
    print("[1/3] 获取活动列表...")
    status, raw = http_request("GET", "/api/v2/activity", token=None, cosy_headers=cosy_headers, timeout=timeout)
    print(f"      HTTP {status}")
    if status != 200:
        print(f"      响应: {raw[:300]}")
        return None
    try:
        data = json.loads(raw)
    except Exception as e:
        print(f"      JSON 解析失败: {e}")
        print(f"      原始响应: {raw[:500]}")
        return None

    # Parse activities
    if "data" in data and isinstance(data["data"], dict):
        activities = data["data"].get("activities", [])
    else:
        activities = data.get("activities", [])

    if data.get("code") not in (None, 0):
        print(f"      业务错误: code={data.get('code')}, msg={data.get('msg')}")

    print(f"      找到 {len(activities)} 个活动")
    return activities


def check_eligibility(token, cosy_headers, timeout=60):
    """检查领取资格"""
    print("[2/3] 检查领取资格...")
    status, raw = http_request("GET", "/api/v2/activity/claim/eligibility", token=token, cosy_headers=cosy_headers, timeout=timeout)
    print(f"      HTTP {status}")
    if status != 200:
        print(f"      响应: {raw[:300]}")
        return None
    try:
        data = json.loads(raw)
    except Exception:
        print(f"      原始响应: {raw[:500]}")
        return None
    print(f"      响应: {json.dumps(data, ensure_ascii=False)[:500]}")
    return data


def claim_activity(token, cosy_headers, activity_id, timeout=60):
    """领取奖励"""
    print(f"[3/3] 领取奖励 activityId={activity_id}...")
    path = f"/api/v2/activity/claim?activityId={urllib.parse.quote(activity_id)}"
    status, raw = http_request("POST", path, token=token, cosy_headers=cosy_headers, timeout=timeout)
    print(f"      HTTP {status}")
    print(f"      响应: {raw[:500]}")
    return status, raw


def main():
    parser = argparse.ArgumentParser(description="Qoder CN 自动签到 - 每日领取 100 Credits")
    parser.add_argument("--token", help="Device token (dt-xxx)")
    parser.add_argument("--account", help="qoder2api 中的账号名")
    parser.add_argument("--list-only", action="store_true", help="只列出活动，不领取")
    parser.add_argument("--timeout", type=int, default=60, help="HTTP 超时秒数")
    args = parser.parse_args()

    # Get token
    token = args.token
    account_name = None
    if not token:
        info = load_token_from_qoder2api(args.account)
        if not info:
            print("错误: 未找到账号，请用 --token 指定 token，或在 qoder2api 中添加账号")
            sys.exit(1)
        token = info["device_token"]
        account_name = info["name"]
        print(f"账号: {account_name}")
        print(f"Token: {token[:20]}...")
    print(f"Base URL: {BASE_URL}")
    print()

    cosy_headers = build_cosy_headers()

    # Step 1: Get activities
    activities = get_activities(token, cosy_headers, args.timeout)
    if activities is None:
        print("\n获取活动列表失败，服务可能暂时不可用")
        sys.exit(1)

    if not activities:
        print("\n没有可领取的活动")
        sys.exit(0)

    # Print activities
    for act in activities:
        act_id = act.get("activityId", "?")
        title = act.get("title", act.get("name", "?"))
        can_claim = act.get("canClaim", False)
        claimed = act.get("claimed", False)
        print(f"      - {title} (id={act_id}) canClaim={can_claim} claimed={claimed}")

    if args.list_only:
        sys.exit(0)

    # Step 2: Check eligibility
    check_eligibility(token, cosy_headers, args.timeout)

    # Step 3: Claim activities that can be claimed
    claimed_any = False
    for act in activities:
        act_id = act.get("activityId")
        can_claim = act.get("canClaim", False)
        claimed = act.get("claimed", False)
        if not act_id:
            continue
        if claimed:
            print(f"      活动 {act_id} 今日已领取，跳过")
            continue
        if not can_claim:
            print(f"      活动 {act_id} 不可领取，跳过")
            continue
        status, raw = claim_activity(token, cosy_headers, act_id, args.timeout)
        if status == 200:
            claimed_any = True
            print(f"      ✅ 领取成功！")
        else:
            print(f"      ❌ 领取失败: HTTP {status}")

    if not claimed_any:
        print("\n没有成功领取任何活动")
    else:
        print("\n✅ 签到完成")


if __name__ == "__main__":
    import urllib.parse
    main()
