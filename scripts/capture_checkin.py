#!/usr/bin/env python3
"""
Qoder CN 签到链路抓包脚本

用途：明天有领取机会时，抓取 Qoder CN 桌面端领取 100 Credits 的完整 HTTP 链路，
     用于分析真实的 API 请求格式（URL / headers / body / response）。

前置条件：
    1. 安装 mitmproxy:  brew install mitmproxy
    2. 安装并信任 mitmproxy CA 证书:
       - 启动一次: mitmdump
       - 证书位于: ~/.mitmproxy/mitmproxy-ca-cert.pem
       - macOS: 双击导入"钥匙串访问" → 系统 → 始终信任
       - 或执行: sudo security add-trusted-cert -d -r trustRoot \
                 -k /Library/Keychains/System.keychain ~/.mitmproxy/mitmproxy-ca-cert.pem

用法：
    1. 运行本脚本（保持终端打开）
    2. 设置系统代理指向 127.0.0.1:8080（或在 Qoder CN App 中配置）
       方式一（临时，推荐）:
         networksetup -setsecurewebproxy "Wi-Fi" 127.0.0.1 8080
         networksetup -setwebproxy "Wi-Fi" 127.0.0.1 8080
       方式二：系统设置 → 网络 → 代理 → 手动配置
    3. 在 Qoder CN 桌面 App 中点击「用量面板 → 礼物图标」领取 Credits
    4. 观察本脚本输出，抓取到的请求会保存到 ./capture/
    5. 领取完成后，关闭代理:
         networksetup -setsecurewebproxystate "Wi-Fi" off
         networksetup -setwebproxystate "Wi-Fi" off

产物：
    ./capture/ 目录下按时间戳保存每个请求的:
      - 请求: method, url, headers, body
      - 响应: status, headers, body
"""

import os
import sys
import json
import time
from pathlib import Path

CAPTURE_DIR = Path(__file__).parent / "capture"
CAPTURE_DIR.mkdir(exist_ok=True)

# 关注的域名（Qoder CN 相关）
TARGET_DOMAINS = [
    "qoder.com.cn",
    "qoder.cn",
    "aliyuncs.com",
    "aliyun.com",
]

# 关注的关键词（URL 中包含则重点标记）
KEYWORDS = [
    "activity", "credit", "claim", "gift", "reward", "campaign",
    "signin", "checkin", "check-in", "daily", "bonus", "event",
    "100credits", "usage", "quota",
]


def should_capture(url: str) -> bool:
    """判断是否是需要抓取的请求"""
    url_lower = url.lower()
    return any(d in url_lower for d in TARGET_DOMAINS)


def is_key_request(url: str) -> bool:
    """判断是否是关键请求（签到相关）"""
    url_lower = url.lower()
    return any(k in url_lower for k in KEYWORDS)


def save_capture(flow, index: int, key: bool):
    """保存抓取到的请求/响应"""
    ts = time.strftime("%Y%m%d-%H%M%S")
    prefix = "KEY" if key else "req"
    filename = f"{prefix}-{ts}-{index:03d}.json"
    path = CAPTURE_DIR / filename

    req = flow.request
    resp = flow.response

    # 解析 body（尝试 JSON）
    def parse_body(content: bytes):
        if not content:
            return None
        try:
            return json.loads(content.decode("utf-8", errors="replace"))
        except Exception:
            return content.decode("utf-8", errors="replace")[:5000]

    record = {
        "time": ts,
        "key_request": key,
        "request": {
            "method": req.method,
            "url": req.url,
            "host": req.host,
            "path": req.path,
            "headers": dict(req.headers),
            "body": parse_body(req.content) if req.content else None,
        },
        "response": {
            "status": resp.status_code if resp else None,
            "headers": dict(resp.headers) if resp else None,
            "body": parse_body(resp.content) if resp and resp.content else None,
        },
    }

    path.write_text(json.dumps(record, ensure_ascii=False, indent=2), encoding="utf-8")

    marker = "🔑 KEY" if key else "   "
    print(f"{marker} [{index:03d}] {req.method} {req.url} -> {resp.status_code if resp else '?'}")
    print(f"        saved: {path.name}")


def main():
    print("=" * 70)
    print("  Qoder CN 签到链路抓包")
    print("=" * 70)
    print()
    print(f"抓包目录: {CAPTURE_DIR}")
    print("关注域名:", ", ".join(TARGET_DOMAINS))
    print("关注关键词:", ", ".join(KEYWORDS))
    print()
    print("请确保:")
    print("  1. mitmproxy CA 证书已安装并信任")
    print("  2. 系统代理已指向 127.0.0.1:8080")
    print("  3. Qoder CN 桌面 App 已打开")
    print()
    print("等待流量中... (Ctrl+C 停止)")
    print("=" * 70)

    try:
        from mitmproxy import options
        from mitmproxy.tools import dump
    except ImportError:
        print("错误: 未安装 mitmproxy")
        print("请执行: brew install mitmproxy")
        sys.exit(1)

    counter = {"n": 0}

    class CaptureAddon:
        def response(self, flow):
            url = flow.request.url
            if not should_capture(url):
                return
            counter["n"] += 1
            key = is_key_request(url)
            save_capture(flow, counter["n"], key)

    opts = options.Options(
        listen_host="127.0.0.1",
        listen_port=8080,
        http2=False,  # 关闭 http2 便于查看明文
    )
    master = dump.DumpMaster(opts, with_termlog=False, with_dumper=False)
    master.addons.add(CaptureAddon())

    try:
        asyncio_run(master.run())
    except KeyboardInterrupt:
        print("\n\n抓包已停止")
        print(f"共抓取 {counter['n']} 个请求，保存在 {CAPTURE_DIR}")
        master.shutdown()


def asyncio_run(coro):
    import asyncio
    try:
        loop = asyncio.get_event_loop()
    except RuntimeError:
        loop = asyncio.new_event_loop()
        asyncio.set_event_loop(loop)
    loop.run_until_complete(coro)


if __name__ == "__main__":
    main()
