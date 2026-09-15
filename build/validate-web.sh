#!/usr/bin/env bash
# 前端 HTML 结构校验。
#
# 为什么需要：曾经因为一次 h.replace() 用了空字符串锚点，
# 把整段 JS 插到了文件开头，导致页面把源码当文本显示。
#
# 检查项：
#   1. DOCTYPE 必须在文件开头
#   2. script 标签必须成对
#   3. JS 语法必须正确
#   4. 关键函数必须存在
#   5. 不能有重复的函数定义

set -u

HTML="${1:-platform/fnos/app/www/index.html}"

if [ ! -f "$HTML" ]; then
    echo "❌ 找不到文件: $HTML"
    exit 1
fi

fail=0
fail_msg() { echo "  ❌ $1"; fail=1; }
ok_msg()   { echo "  ✅ $1"; }

echo "=== HTML 结构校验: $HTML ==="

# 1) DOCTYPE 必须在开头（允许前导空白/BOM）
head_part=$(head -c 200 "$HTML" | tr -d '[:space:]\357\273\277')
case "$head_part" in
    "<!DOCTYPEhtml>"*) ok_msg "DOCTYPE 在文件开头" ;;
    *) fail_msg "DOCTYPE 不在开头（文件被污染？）" ;;
esac

# 2) script 标签配对
open_n=$(grep -o '<script' "$HTML" | wc -l)
close_n=$(grep -o '</script>' "$HTML" | wc -l)
if [ "$open_n" = "$close_n" ] && [ "$open_n" -gt 0 ]; then
    ok_msg "script 标签配对 ($open_n 对)"
else
    fail_msg "script 标签不配对 (开=$open_n 闭=$close_n)"
fi

# 3) 关键函数存在且唯一
for fn in "function startLive(" "function startLiveHLS(" "function startLiveMJPEG(" "function renderDevices("; do
    cnt=$(grep -c "$fn" "$HTML" || true)
    if [ "$cnt" = "1" ]; then
        ok_msg "$fn"
    elif [ "$cnt" = "0" ]; then
        fail_msg "$fn 缺失"
    else
        fail_msg "$fn 重复 ($cnt 次)"
    fi
done

# 4) JS 语法
tmp=$(mktemp /tmp/micam-js-XXXXXX.js)
python3 -c "
import re, sys
html = open('$HTML', encoding='utf-8').read()
m = re.search(r'<script[^>]*>(.*?)</script>', html, re.S)
open('$tmp','w',encoding='utf-8').write(m.group(1) if m else '')
if not m:
    sys.exit(1)
" || fail_msg "无法提取 script"
if [ -s "$tmp" ]; then
    if command -v node >/dev/null 2>&1; then
        if node --check "$tmp" 2>/dev/null; then
            ok_msg "JS 语法正确"
        else
            fail_msg "JS 语法错误"
            node --check "$tmp" 2>&1 | head -3
        fi
    fi
fi
rm -f "$tmp"

# 5) 检查文件里有没有把 JS 源码当正文（开头出现 function 关键字）
first_line=$(head -1 "$HTML")
case "$first_line" in
    *"function "*|*"// "*)
        fail_msg "文件开头是 JS 代码（应为 <!DOCTYPE）"
        ;;
esac

echo
if [ "$fail" = "0" ]; then
    echo "✅ 校验通过"
    exit 0
else
    echo "❌ 校验失败"
    exit 1
fi
