#!/usr/bin/env bash
# 本地部署（开发用）
#
# 为什么要有这个脚本：
#   手动部署时容易踩两个坑——
#     1. 复用了陈旧的构建产物（版本对不上）
#     2. 忘了重启，跑的还是旧进程
#   这个脚本强制「全新构建 + 版本校验 + 重启 + 验证」。
#
# 用法：bash build/deploy-local.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PLATFORM="$ROOT/platform/fnos"
GO=/usr/local/go/bin/go
export GOPROXY=https://goproxy.cn,direct

EXPECT="$(grep -E '^version' "$PLATFORM/manifest" | head -1 | sed 's/.*=\s*//' | tr -d '[:space:]')"
echo "==> 目标版本: $EXPECT"

echo "==> 1/5 清理陈旧产物"
rm -f "$PLATFORM/app/server/micamd"

echo "==> 2/5 全新编译"
mkdir -p "$PLATFORM/app/server"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  "$GO" build -trimpath -ldflags="-s -w" \
  -o "$PLATFORM/app/server/micamd" "$ROOT/cmd/micamd"

# 从二进制里读出版本，和 manifest 对比
GOT="$(grep -aoE '0\.[0-9]+\.[0-9]+' "$PLATFORM/app/server/micamd" | sort -u | tail -1)"
if [ "$GOT" != "$EXPECT" ]; then
  echo "!! 版本不一致：二进制=$GOT manifest=$EXPECT" >&2
  exit 1
fi
echo "    版本校验通过: $GOT"

echo "==> 3/5 部署到应用目录"
export TRIM_APPDEST=/vol1/@appcenter/micam
export TRIM_PKGETC=/vol1/@appconf/micam
export TRIM_PKGVAR=/vol1/@appdata/micam

pkill -9 -f micamd 2>/dev/null || true
pkill -9 -f ffmpeg 2>/dev/null || true
sleep 3

cp "$PLATFORM/app/server/micamd" /vol1/@appcenter/micam/server/micamd
chown micam:micam /vol1/@appcenter/micam/server/micamd
rm -f /vol1/@appcenter/micam/app.sock /vol1/@appdata/micam/app.pid

echo "==> 4/5 重启"
sudo -u micam env TRIM_APPDEST=$TRIM_APPDEST TRIM_PKGETC=$TRIM_PKGETC TRIM_PKGVAR=$TRIM_PKGVAR \
  TRIM_PKGTMP=/vol1/@apptemp/micam TRIM_PKGHOME=/vol1/@apphome/micam \
  bash /var/apps/micam/cmd/main start
sleep 6

echo "==> 5/5 验证"
RUNNING="$(curl -s -m 5 http://127.0.0.1:8900/api/health)"
echo "    $RUNNING"
if ! echo "$RUNNING" | grep -q "\"version\":\"$EXPECT\""; then
  echo "!! 运行版本与目标不符" >&2
  exit 1
fi
echo "完成: v$EXPECT 已上线"
