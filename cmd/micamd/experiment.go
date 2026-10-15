package main

// 恢复期实验锁（2026-09-23）。
//
// ===== 背景 =====
//
// 为回答"限流多久恢复"，设置了 7 天【零调用】观察期：
//   起始 2026-09-23 20:22 +08
//   测试 2026-09-30 20:22 +08
//
// 期间任何一次 miss_get_vendor 调用都会污染测量
//（如果限流是滑动窗口，调用会把窗口推后）。
//
// ===== 为什么锁在后端而不是只禁用按钮 =====
//
// 前端有 3 处会调 /api/live/renew，而且：
//   · handleCredRenew 里有 ResetThrottleSilence()，会【绕过】静默
//   · 排查脚本（curl / vendor-probe）也直接调接口
// 只禁按钮挡不住这些。所以在 handler 层统一拒绝。
//
// ===== 用法 =====
//
//   touch <dataDir>/experiment.lock    开启（内容为实验说明，可空）
//   rm    <dataDir>/experiment.lock    结束实验
//
// 被拒的接口：
//   POST /api/live/renew          （刷新凭据）
//   GET  /api/dev/vendor-probe    （参数探测 —— 也会调 miss_get_vendor）
//   POST /api/dev/debug-vendor    （同上）

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func experimentLockFile() string {
	return filepath.Join(dataDirPath, "experiment.lock")
}

// experimentLocked 返回（是否锁定, 说明文字）。
func experimentLocked() (bool, string) {
	raw, err := os.ReadFile(experimentLockFile())
	if err != nil {
		return false, ""
	}
	note := strings.TrimSpace(string(raw))
	if note == "" {
		note = "恢复期实验中：静默观察中，期间禁止任何 miss_get_vendor 调用"
	}
	return true, note
}

// blockIfExperiment 锁定期间拒绝请求（返回 true 表示已拒绝）。
func blockIfExperiment(w http.ResponseWriter, r *http.Request) bool {
	locked, note := experimentLocked()
	if !locked {
		return false
	}
	writeJSON(w, 423, map[string]any{ // 423 Locked
		"ok":         false,
		"experiment": true,
		"msg":        "⏳ " + note,
		"hint":       "这是为了测出真实的恢复时间。提前解除会污染测量（删 <dataDir>/experiment.lock 可结束实验）。",
	})
	logf("[exp] 已拦截 %s %s（恢复期实验锁定中）", r.Method, r.URL.Path)
	return true
}
