#!/bin/sh
# 安装仓库 git hooks（源文件在 build/hooks/，因为 .git/hooks/ 不受版本控制）。
#
# 用法：bash build/install-hooks.sh
#
# 为什么要入库而不是直接写 .git/hooks/：
#   .git/hooks/ 不进 git，新克隆的仓库没有这些检查 ——
#   而这条检查恰恰是为了防止**并发/多人协作**时的夹带提交，
#   所以它必须能随仓库分发。
#
# 注意：build/hooks/ 里既有 hook 也有被 source 的共享库
# （mixed-lib.sh）。两者都要复制过去 —— hook 用
#   . "$(dirname "$0")/mixed-lib.sh"
# 加载它，而 $0 在安装后就是 .git/hooks/<name>。
set -e

REPO="$(cd "$(dirname "$0")/.." && pwd)"
SRC="$REPO/build/hooks"
DST="$REPO/.git/hooks"

[ -d "$DST" ] || { echo "不是 git 仓库：$REPO"; exit 1; }

installed=""
for f in "$SRC"/*; do
  [ -f "$f" ] || continue
  name=$(basename "$f")
  cp "$f" "$DST/$name"
  chmod +x "$DST/$name"
  case "$name" in
    *-lib.sh) : ;;                                  # 共享库，不是 hook
    *) installed="$installed $name" ;;
  esac
done

echo "已安装 hooks:$installed"
echo "（共享库 mixed-lib.sh 也已复制到 .git/hooks/，供 hook source）"
