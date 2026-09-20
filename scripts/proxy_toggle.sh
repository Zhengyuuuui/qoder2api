#!/bin/bash
# Qoder CN 抓包代理开关脚本
# 用法:
#   ./proxy_toggle.sh on    开启抓包代理
#   ./proxy_toggle.sh off   关闭抓包代理
#   ./proxy_toggle.sh status 查看当前状态

set -e

PROXY_HOST="127.0.0.1"
PROXY_PORT="8080"
SERVICE="Wi-Fi"   # 如使用有线网络，改成 "Ethernet"

if [ "$(uname)" != "Darwin" ]; then
    echo "本脚本仅支持 macOS"
    exit 1
fi

# 自动检测活动网络服务
detect_service() {
    local svc
    svc=$(networksetup -listnetworkserviceorder 2>/dev/null | grep -B1 "Device: en0" | head -1 | sed 's/^([0-9]*) //' | sed 's/.*) //' | head -1)
    if [ -n "$svc" ]; then
        echo "$svc"
    else
        echo "Wi-Fi"
    fi
}

case "$1" in
    on)
        SVC=$(detect_service)
        echo "开启代理: $SVC -> $PROXY_HOST:$PROXY_PORT"
        networksetup -setwebproxy "$SVC" "$PROXY_HOST" "$PROXY_PORT"
        networksetup -setsecurewebproxy "$SVC" "$PROXY_HOST" "$PROXY_PORT"
        # 绕过本地和 Qoder 不需要代理的地址（可选）
        networksetup -setproxybypassdomains "$SVC" "127.0.0.1" "localhost" "*.local"
        echo "✅ 代理已开启。现在打开 Qoder CN 桌面 App 领取 Credits，然后运行 ./proxy_toggle.sh off"
        echo ""
        echo "提示: 确保 mitmdump 正在运行 (scripts/capture_checkin.py)"
        ;;
    off)
        SVC=$(detect_service)
        echo "关闭代理: $SVC"
        networksetup -setsecurewebproxystate "$SVC" off
        networksetup -setwebproxystate "$SVC" off
        echo "✅ 代理已关闭"
        ;;
    status)
        SVC=$(detect_service)
        echo "网络服务: $SVC"
        echo ""
        echo "--- HTTP 代理 ---"
        networksetup -getwebproxy "$SVC"
        echo ""
        echo "--- HTTPS 代理 ---"
        networksetup -getsecurewebproxy "$SVC"
        ;;
    *)
        echo "用法: $0 {on|off|status}"
        exit 1
        ;;
esac
