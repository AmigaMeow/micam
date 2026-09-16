#!/usr/bin/env bash
# 部署后自检：确认页面在**各种访问路径**下都正常
#
# 为什么需要：
#   飞牛网关和直连 8900 的路径形态不同，
#   只测一种会漏掉问题（曾经因此发布了无样式的版本）。
set -uo pipefail

BASE="http://127.0.0.1:8900"
FAIL=0

chk() { # 名称 期望 实际
  if [ "$2" = "$3" ]; then printf "  ✅ %-34s %s\n" "$1" "$3"
  else printf "  ❌ %-34s 期望 %s 实际 %s\n" "$1" "$2" "$3"; FAIL=1; fi
}

echo "== 页面可访问性 =="
for p in "/" "/app/micam/"; do
  CODE=$(timeout 15 curl -s -m 12 -o /tmp/_v.html -w '%{http_code}' "$BASE$p" 2>/dev/null)
  chk "GET $p" "200" "$CODE"
done

echo
echo "== 样式已内联（不依赖外部路径）=="
grep -q '<style>' /tmp/_v.html && echo "  ✅ 含内联 <style>" || { echo "  ❌ 无内联样式"; FAIL=1; }
if grep -q '<link rel="stylesheet"' /tmp/_v.html; then
  echo "  ❌ 仍有外部 CSS 引用（网关下会 404）"; FAIL=1
else
  echo "  ✅ 无外部 CSS 引用"
fi

echo
echo "== 关键渲染要素 =="
for k in 'class="app"' 'class="sider' 'id="menu"' 'id="content"' 'id="i-camera"' '--bg:'; do
  N=$(grep -c -- "$k" /tmp/_v.html 2>/dev/null || echo 0)
  if [ "$N" -gt 0 ]; then printf "  ✅ %-34s %s 处\n" "$k" "$N"
  else printf "  ❌ %-34s 缺失\n" "$k"; FAIL=1; fi
done

echo
echo "== HTML 结构 =="
POS=$(grep -bo '<!DOCTYPE' /tmp/_v.html | head -1 | cut -d: -f1)
chk "DOCTYPE 位置" "0" "$POS"

echo
echo "== 静态资源（hls.js 候选路径都要通）=="
for p in /assets/vendor/hls.min.js /app/micam/assets/vendor/hls.min.js; do
  CODE=$(timeout 15 curl -s -m 12 -o /dev/null -w '%{http_code}' "$BASE$p" 2>/dev/null)
  chk "GET $p" "200" "$CODE"
done

echo
if [ "$FAIL" = "0" ]; then echo "✅ 前端自检通过"; else echo "❌ 前端自检失败"; exit 1; fi
