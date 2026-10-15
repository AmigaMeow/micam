#!/bin/sh
# ============================================================================
# 共享：检测「前端 + 后端」跨层混合提交
# ----------------------------------------------------------------------------
# 被 pre-commit 与 commit-msg 共同 source。
#
# 为什么抽成独立文件：两个 hook 需要**完全一致**的判定 ——
#   pre-commit  决定「拦还是放」
#   commit-msg  在放行时往 commit message 打标记
# 如果各写一份，改了判定条件却漏改另一处，就会出现
# 「pre-commit 放行了、但 commit-msg 没打标」这类静默不一致。
#
# 本文件不是 git hook（git 只调用固定名字的 hook），不会被直接执行。
# ============================================================================

# mixed_scan —— 扫描暂存区，设置 FE / BE 两个变量
#   返回 0 = 同时含前端与后端（跨层）
#   返回 1 = 不是跨层
mixed_scan() {
  FE=""
  BE=""
  _staged=$(git diff --cached --name-only --diff-filter=ACMR)
  for _f in $_staged; do
    case "$_f" in
      platform/fnos/app/www/*)              FE="$FE $_f" ;;
      cmd/*|internal/*|pkg/*|app/*)         BE="$BE $_f" ;;
      platform/fnos/*)                      BE="$BE $_f" ;;
      go.mod|go.sum)                        BE="$BE $_f" ;;
      *) : ;;   # docs / build / scripts / 其它：中立区，不参与判定
    esac
  done
  [ -n "$FE" ] && [ -n "$BE" ]
}

# mixed_override_used —— 是否显式声明了要混合提交
mixed_override_used() {
  [ "${ALLOW_MIXED_COMMIT:-}" = "1" ]
}

# 追加标记时用的固定 tag。COMMIT-ANOMALIES.md 的登记可用
#   git log --grep='mixed-commit-override'
# 反查所有「经显式放行的跨层提交」。
MIXED_TAG="[mixed-commit-override]"
