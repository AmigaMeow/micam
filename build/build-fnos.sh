#!/usr/bin/env bash
# 构建 MiCam fpk 安装包
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PLATFORM="$ROOT/platform/fnos"
DIST="$ROOT/dist"
STAGE="$DIST/stage"
VERSION="$(grep -E '^version' "$PLATFORM/manifest" | cut -d= -f2 | tr -d ' ')"

GO=/usr/local/go/bin/go
export GOPROXY=https://goproxy.cn,direct

echo "==> 清理与准备"
rm -rf "$STAGE"
mkdir -p "$STAGE" "$DIST"

echo "==> 编译最新前端（组装单一静态文件 index.html）"
node "$ROOT/build/assemble-web.js"

echo "==> 交叉编译后端（linux/amd64）版本 $VERSION"
APP_DIR="$STAGE/app_root"
mkdir -p "$APP_DIR/server"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  "$GO" build -trimpath -ldflags="-s -w -X main.appVersion=$VERSION" \
  -o "$APP_DIR/server/micamd" "$ROOT/cmd/micamd"

# 校验版本
BUILT="$("$APP_DIR/server/micamd" --version 2>/dev/null | awk '{print $2}')"
if [ "$BUILT" != "$VERSION" ]; then
  echo "!! 版本不一致：二进制=$BUILT manifest=$VERSION" >&2
  exit 1
fi
echo "    版本校验通过: $BUILT"

echo "==> 复制应用核心资源到 app_root"
cp -r "$PLATFORM/app/ui" "$APP_DIR/ui"
cp -r "$PLATFORM/app/www" "$APP_DIR/www"
cp "$PLATFORM/app/shell.html" "$APP_DIR/shell.html"
if [ -d "$ROOT/app/ai" ]; then
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
cp -r "$PLATFORM/config" "$STAGE/"
cp -r "$PLATFORM/cmd" "$STAGE/"
cp "$PLATFORM/ICON.PNG" "$STAGE/"
cp "$PLATFORM/ICON_256.PNG" "$STAGE/"

chmod +x "$STAGE/cmd/"*

echo "==> 生成最终 .fpk 插件包"
OUT="$DIST/micam_${VERSION}_amd64.fpk"
tar -C "$STAGE" -czf "$OUT" .
ls -lh "$OUT"
echo "🎉 打包成功: $OUT"
