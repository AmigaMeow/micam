#!/usr/bin/env bash
# 构建 MiCam fpk 安装包
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PLATFORM="$ROOT/platform/fnos"
DIST="$ROOT/dist"
STAGE="$DIST/stage"
VERSION="$(grep '^version=' "$PLATFORM/manifest" | cut -d= -f2)"

GO=/usr/local/go/bin/go
export GOPROXY=https://goproxy.cn,direct

echo "==> 清理"
rm -rf "$STAGE"
mkdir -p "$STAGE" "$DIST"

echo "==> 交叉编译后端（linux/amd64）"
mkdir -p "$STAGE/server"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  "$GO" build -trimpath -ldflags="-s -w -X main.appVersion=$VERSION" \
  -o "$STAGE/server/micamd" "$ROOT/cmd/micamd"

# 校验版本（用二进制自己报的）
BUILT="$("$STAGE/server/micamd" --version 2>/dev/null | awk '{print $2}')"
if [ "$BUILT" != "$VERSION" ]; then
  echo "!! 版本不一致：二进制=$BUILT manifest=$VERSION" >&2
  exit 1
fi
echo "    版本校验通过: $BUILT"

echo "==> 复制平台文件"
cp "$PLATFORM/manifest" "$STAGE/"
cp -r "$PLATFORM/config" "$STAGE/"
cp -r "$PLATFORM/ui" "$STAGE/"
cp -r "$PLATFORM/cmd" "$STAGE/"
cp -r "$PLATFORM/web" "$STAGE/"
if [ -d "$PLATFORM/wizard" ]; then cp -r "$PLATFORM/wizard" "$STAGE/"; fi

echo "==> 补齐目录占位"
mkdir -p "$STAGE/etc" "$STAGE/var" "$STAGE/tmp" "$STAGE/home" "$STAGE/meta" "$STAGE/shares"

echo "==> 权限"
chmod +x "$STAGE/server/micamd" "$STAGE/cmd/main"
find "$STAGE" -type d -exec chmod 0755 {} \;

echo "==> 打包"
OUT="$DIST/micam_${VERSION}_amd64.fpk"
tar -C "$STAGE" -czf "$OUT" .
ls -lh "$OUT"
echo "完成: $OUT"
