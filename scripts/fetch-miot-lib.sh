#!/bin/bash
# 按需从小米官方公开开源仓库下载 PPCS 摄像头组件（不随本软件分发）
#
# libmiot_camera_lite.so 是小米官方在其开源项目 XiaoMi/xiaomi-miloco 中发布的预编译库。
# 来源：https://github.com/XiaoMi/xiaomi-miloco
# 路径：backend/miot/src/miot/libs/<平台>/<架构>/libmiot_camera_lite.so
#
# 用法：
#   bash scripts/fetch-miot-lib.sh                 # 自动下载到系统数据与运行目录
#   bash scripts/fetch-miot-lib.sh /custom/dir     # 指定自定义安装目录
set -euo pipefail

DEST="${1:-}"

OS="$(uname -s)"
ARCH="$(uname -m)"
case "$OS/$ARCH" in
  Linux/x86_64)              SUB="linux/x86_64"; NAME="libmiot_camera_lite.so" ;;
  Linux/aarch64|Linux/arm64) SUB="linux/arm64";  NAME="libmiot_camera_lite.so" ;;
  Darwin/x86_64)             SUB="macos/x86_64"; NAME="libmiot_camera_lite.dylib" ;;
  Darwin/arm64)              SUB="macos/arm64";  NAME="libmiot_camera_lite.dylib" ;;
  *) echo "不支持的平台架构: $OS/$ARCH"; exit 1 ;;
esac

RAW_URL="https://raw.githubusercontent.com/XiaoMi/xiaomi-miloco/main/backend/miot/src/miot/libs/$SUB/$NAME"

# 官方直接源与高速加速镜像（保障国内网络通畅）
MIRRORS=(
  "https://ghproxy.net/$RAW_URL"
  "https://mirror.ghproxy.com/$RAW_URL"
  "$RAW_URL"
  "https://gh-proxy.com/$RAW_URL"
)

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
if [ -n "$DEST" ]; then
  targets=("$DEST/$NAME")
else
  targets=(
    "/vol1/@appdata/micam/lib/$NAME"
    "/vol1/@appcenter/micam/lib/$NAME"
    "$ROOT/app/miot/$NAME"
  )
fi

echo "=== MiCam 官方流媒体组件安全拉取工具 ==="
echo "检测平台: $OS/$ARCH"
echo "官方目标: $RAW_URL"

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

success=0
for mirror in "${MIRRORS[@]}"; do
  echo "--> 尝试节点: $mirror ..."
  if curl -fL --connect-timeout 8 --max-time 60 --retry 1 -o "$tmp" "$mirror" 2>/dev/null; then
    size=$(stat -c%s "$tmp" 2>/dev/null || stat -f%z "$tmp" 2>/dev/null || echo 0)
    if [ "$size" -ge 1000000 ]; then
      echo "    ✅ 下载成功（大小 $size 字节，约 $(awk "BEGIN {printf \"%.1f\", $size/1048576}") MB）"
      success=1
      break
    else
      echo "    ⚠️ 文件大小异常（仅 $size 字节），切换备用节点..."
    fi
  else
    echo "    ⚠️ 节点连接失败或超时，切换备用节点..."
  fi
done

if [ "$success" -ne 1 ]; then
  echo "❌ 无法从镜像源完成下载。请检查网络，或手动访问 XiaoMi/xiaomi-miloco 下载并将 $NAME 上传至 /vol1/@appdata/micam/lib/"
  exit 1
fi

for t in "${targets[@]}"; do
  mkdir -p "$(dirname "$t")"
  cp -f "$tmp" "$t"
  chmod 0755 "$t"
  chown micam:micam "$t" 2>/dev/null || true
  echo "已就绪: $t"
done

echo
echo "🎉 官方流媒体库已就绪，所有摄像头功能无感激活！"
