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
  "$GO" build -trimpath -ldflags="-s -w -X main.appVersion=$EXPECT" \
  -o "$PLATFORM/app/server/micamd" "$ROOT/cmd/micamd"

# 用二进制自己的 --version 校验（权威）
#
# 为什么不用 grep：二进制里含依赖的版本号（如 go2rtc 0.8.1），
# grep 会抓错。--version 是程序自己报的，可靠。
GOT="$("$PLATFORM/app/server/micamd" --version 2>/dev/null | awk '{print $2}')"
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

# 部署前端（很容易忘——忘了就是「改了界面但没生效」）
mkdir -p /vol1/@appcenter/micam/www
cp "$PLATFORM/app/www/"* /vol1/@appcenter/micam/www/ 2>/dev/null || true
chown -R micam:micam /vol1/@appcenter/micam/www
# 强制让服务重新部署 web（删除目标，触发 deployWeb 的 mtime 判断）
rm -f /vol1/@appdata/micam/web/index.html
echo "    前端已同步"

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
