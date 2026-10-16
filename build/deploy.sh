#!/bin/bash
# 部署前先清掉上一代残留（它们会占着摄像头的 PPCS 连接槽）
if [ -x "$(dirname "$0")/cleanup.sh" ]; then
  bash "$(dirname "$0")/cleanup.sh" >/dev/null 2>&1 || true
fi
# MiCam 部署脚本（2026-09-24 重建）。
#
# 为什么放仓库里：原来在 /tmp/deploy-step.sh，而 /tmp 会被系统定期清理
# （2026-09-24 实测丢失，部署报 No such file or directory）。
#
# 用法：bash build/deploy.sh
#
# 启动命令是从【运行中的进程】抄的（fnOS 用它拉起的），不要凭记忆改：
#   ps aux | grep [m]icamd
set -e
export PATH=/usr/local/go/bin:$PATH GOPROXY=https://goproxy.cn,direct

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEST=/vol1/@appcenter/micam
DATA=/vol1/@appdata/micam
APP="$DEST/server/micamd"
VERSION=$(grep -oE 'appVersion = "[0-9.]*"' "$REPO/cmd/micamd/main.go" | head -1 | grep -oE '[0-9.]+')

cd "$REPO"
echo "=== 部署 MiCam $VERSION ==="

echo "--- 1) 编译"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -ldflags="-s -w -X main.appVersion=$VERSION" -o /tmp/micamd-new ./cmd/micamd
echo "    ok"

echo "--- 2) 组装前端（内联 JS/CSS）"
node build/assemble-web.js | tail -2 | sed 's/^/    /'

echo "--- 3) 停服务"
pkill -9 -f micamd 2>/dev/null || killall -9 micamd 2>/dev/null || true
sleep 2

echo "--- 4) 复制"
install -m 0755 /tmp/micamd-new "$APP"
rm -rf "$DEST/www"
cp -r "$REPO/platform/fnos/app/www" "$DEST/www"
mkdir -p "$DEST/docs" && cp -f "$REPO/docs/"*.md "$DEST/docs/" 2>/dev/null || true
# MiOT 官方通道 sidecar + 闭源库（库不入 git，从 third_party 复制）
mkdir -p "$DEST/miot"
cp -f "$REPO/app/miot/"*.py "$DEST/miot/" 2>/dev/null || true
if [ -f "$REPO/third_party/miloco/libmiot_camera_lite.so" ]; then
  mkdir -p "$DEST/lib"
  cp -f "$REPO/third_party/miloco/libmiot_camera_lite.so" "$DEST/lib/" 2>/dev/null || true
  cp -f "$REPO/third_party/miloco/libmiot_camera_lite.so" "$DEST/miot/" 2>/dev/null || true
  echo "    + libmiot_camera_lite.so"
fi
chown -R micam:micam "$DEST/miot" "$DEST/lib" 2>/dev/null || true
chown -R micam:micam "$DEST/www" "$DEST/server" "$DEST/docs" 2>/dev/null || true
echo "    ok"

# 同步前端到数据目录（服务实际读这里）
# deployWeb() 只在源比目标新时才复制，且依赖 TRIM_APPDEST；
# 这里显式同步一次，确保部署一定生效。
mkdir -p "$DATA/web"
cp -f "$DEST/www/index.html" "$DATA/web/index.html" 2>/dev/null || true
echo "    + 已同步前端到数据目录"
if [ -d "$DEST/www/assets" ]; then
  mkdir -p "$DATA/web/assets"
  cp -f "$DEST/www/assets/"* "$DATA/web/assets/" 2>/dev/null || true
fi
chown -R micam:micam "$DATA/web" 2>/dev/null || true

echo "--- 5) 启动"
# ⚠️ 必须设 TRIM_APPDEST —— 服务用 deployWeb() 把前端从
#    $TRIM_APPDEST/www 复制到 $DATA/web，而【实际读的是 $DATA/web】。
#    不设的话部署的 index.html 永远进不了服务（曾导致前端改动"没生效"）。
setsid su -s /bin/bash micam -c \
  "TRIM_APPDEST=$DEST $APP --socket $DEST/app.sock --http :8900 --config /vol1/@appconf/micam/config.json --data $DATA --ffmpeg /usr/bin/ffmpeg" \
  >> "$DATA/micamd.log" 2>&1 < /dev/null &
sleep 5

echo "--- 6) 校验"
if curl -sf -m 15 http://127.0.0.1:8900/api/health >/dev/null 2>&1; then
  curl -s -m 15 http://127.0.0.1:8900/api/health | head -c 200
  echo
  echo "    ✅ 部署成功"
else
  echo "    ❌ 健康检查失败，最后 20 行日志："
  tail -20 "$DATA/micamd.log" | sed 's/^/      /'
  exit 1
fi