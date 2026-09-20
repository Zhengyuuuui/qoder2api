"""
mitmdump addon：抓取 Qoder CN 签到链路
用法: mitmdump -s capture_addon.py -p 8080
"""
import json
import time
from pathlib import Path

CAPTURE_DIR = Path(__file__).parent / "capture"
CAPTURE_DIR.mkdir(exist_ok=True)

TARGET_DOMAINS = ["qoder.com.cn", "qoder.cn", "aliyuncs.com", "aliyun.com"]
KEYWORDS = [
    "activity", "credit", "claim", "gift", "reward", "campaign",
    "signin", "checkin", "check-in", "daily", "bonus", "event",
    "100credits", "usage", "quota",
]

_counter = {"n": 0}


def _parse_body(content):
    if not content:
        return None
    try:
        return json.loads(content.decode("utf-8", errors="replace"))
    except Exception:
        return content.decode("utf-8", errors="replace")[:8000]


def response(flow):
    url = flow.request.pretty_url
    host = flow.request.pretty_host
    if not any(d in host for d in TARGET_DOMAINS):
        return

    _counter["n"] += 1
    key = any(k in url.lower() for k in KEYWORDS)
    ts = time.strftime("%Y%m%d-%H%M%S")
    prefix = "KEY" if key else "req"
    path = CAPTURE_DIR / f"{prefix}-{ts}-{_counter['n']:03d}.json"

    record = {
        "time": ts,
        "key_request": key,
        "request": {
            "method": flow.request.method,
            "url": url,
            "host": host,
            "path": flow.request.path,
            "headers": dict(flow.request.headers),
            "body": _parse_body(flow.request.content),
        },
        "response": {
            "status": flow.response.status_code if flow.response else None,
            "headers": dict(flow.response.headers) if flow.response else None,
            "body": _parse_body(flow.response.content) if flow.response else None,
        },
    }
    path.write_text(json.dumps(record, ensure_ascii=False, indent=2), encoding="utf-8")

    marker = "KEY" if key else "   "
    status = flow.response.status_code if flow.response else "?"
    print(f"[{marker}] #{_counter['n']:03d} {flow.request.method} {url} -> {status}  saved={path.name}")
