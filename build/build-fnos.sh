#!/usr/bin/env bash
# 构建 MiCam fpk 安装包
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PLATFORM="$ROOT/platform/fnos"
DIST="$ROOT/dist"
STAGE="$DIST/stage"
VERSION="$(grep -E '^version' "$PLATFORM/manifest" | cut -d= -f2 | tr -d ' ')"
ARCH="${1:-amd64}"
case "$ARCH" in
  amd64|x86_64) TARGET_ARCH="amd64"; PLATFORM_TAG="x86" ;;
  arm64|aarch64) TARGET_ARCH="arm64"; PLATFORM_TAG="arm64" ;;
  *) echo "不支持的目标架构: $ARCH (支持: amd64, arm64)"; exit 1 ;;
esac

GO=/usr/local/go/bin/go
export GOPROXY=https://goproxy.cn,direct

echo "==> 清理与准备"
rm -rf "$STAGE"
mkdir -p "$STAGE" "$DIST"

echo "==> 编译最新前端（组装单一静态文件 index.html）"
node "$ROOT/build/assemble-web.js"

echo "==> 交叉编译后端（linux/$TARGET_ARCH）版本 $VERSION"
APP_DIR="$STAGE/app_root"
mkdir -p "$APP_DIR/server"
CGO_ENABLED=0 GOOS=linux GOARCH="$TARGET_ARCH" \
  "$GO" build -trimpath -ldflags="-s -w -X main.appVersion=$VERSION" \
  -o "$APP_DIR/server/micamd" "$ROOT/cmd/micamd"

# 校验版本（同架构可执行，跨架构跳过本地执行）
if [ "$TARGET_ARCH" = "amd64" ] && [ "$(uname -m)" = "x86_64" ]; then
  BUILT="$("$APP_DIR/server/micamd" --version 2>/dev/null | awk '{print $2}')"
  if [ "$BUILT" != "$VERSION" ]; then
    echo "!! 版本不一致：二进制=$BUILT manifest=$VERSION" >&2
    exit 1
  fi
  echo "    版本校验通过: $BUILT"
else
  echo "    交叉编译成功: linux/$TARGET_ARCH ($VERSION)"
fi

echo "==> 复制应用核心资源到 app_root"
cp -r "$PLATFORM/app/ui" "$APP_DIR/ui"
cp -r "$PLATFORM/app/www" "$APP_DIR/www"
cp "$PLATFORM/app/shell.html" "$APP_DIR/shell.html"
if [ -d "$PLATFORM/app/ai" ]; then
  cp -r "$PLATFORM/app/ai" "$APP_DIR/ai"
elif [ -d "$ROOT/app/ai" ]; then
  cp -r "$ROOT/app/ai" "$APP_DIR/ai"
fi
if [ -d "$ROOT/app/miot" ]; then
  mkdir -p "$APP_DIR/miot"
  cp -f "$ROOT/app/miot/"*.py "$APP_DIR/miot/" 2>/dev/null || true
fi

# 严格执行零专有二进制隔离：绝不打包 libmiot_camera_lite.so
find "$APP_DIR" -name "*.so*" -o -name "*.dylib*" | xargs rm -f 2>/dev/null || true

echo "==> 打包 app.tgz"
tar -C "$APP_DIR" -czf "$STAGE/app.tgz" .
rm -rf "$APP_DIR"

echo "==> 组装 fpk 根层级元数据与生命周期脚本"
cp "$PLATFORM/manifest" "$STAGE/"
sed -i "s/^platform *=.*/platform              = $PLATFORM_TAG/" "$STAGE/manifest"
cp -r "$PLATFORM/config" "$STAGE/"
cp -r "$PLATFORM/cmd" "$STAGE/"
cp "$PLATFORM/ICON.PNG" "$STAGE/"
cp "$PLATFORM/ICON_256.PNG" "$STAGE/"

chmod +x "$STAGE/cmd/"*

echo "==> 生成最终 .fpk 插件包"
OUT="$DIST/micam_${VERSION}_${TARGET_ARCH}.fpk"
tar -C "$STAGE" -czf "$OUT" .
ls -lh "$OUT"
echo "打包成功: $OUT"
