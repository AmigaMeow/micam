package miot

// 库查找与 sidecar 路径（纯 Go，无 cgo）。
//
// ===== 为什么不用 cgo =====
//
// 本项目硬约束：CGO_ENABLED=0（见 docs/HANDOFF-DEVELOPER-PAGE.md「不要引入 cgo」），
// 且本机没有 gcc/clang。而 libmiot_camera_lite 是 C ABI 的 .so。
//
// 解法：**Python sidecar**（官方 Miloco 本身就是 Python + ctypes 调同一个库）。
// Go 主程序只负责：
//   1) 生成 OAuth2 授权 URL、换令牌（见 oauth2.go，纯 HTTP）
//   2) 起 sidecar 子进程，读它的 MJPEG 输出
//
// 本文件提供路径解析，供 /api/miot/status 报告与 sidecar 启动使用。

import (
	"os"
	"path/filepath"
)

// FindLib 查找 libmiot_camera_lite.so 的绝对路径，找不到返回空串。
func FindLib(explicit string) string {
	var cands []string
	if explicit != "" {
		cands = append(cands, explicit)
	}
	if p := os.Getenv("MICAM_MIOT_LIB"); p != "" {
		cands = append(cands, p)
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		cands = append(cands,
			filepath.Join(dir, "libmiot_camera_lite.so"),
			filepath.Join(dir, "..", "third_party", "miloco", "libmiot_camera_lite.so"),
		)
	}
	if wd, err := os.Getwd(); err == nil {
		cands = append(cands,
			filepath.Join(wd, "third_party", "miloco", "libmiot_camera_lite.so"),
			filepath.Join(wd, "app", "miot", "libmiot_camera_lite.so"),
		)
	}
	cands = append(cands,
		"/vol1/@appcenter/micam/lib/libmiot_camera_lite.so",
		"/vol1/@appdata/micam/lib/libmiot_camera_lite.so",
		"/usr/local/lib/libmiot_camera_lite.so",
		"/usr/lib/libmiot_camera_lite.so",
	)
	for _, p := range cands {
		if p == "" {
			continue
		}
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			abs, _ := filepath.Abs(p)
			if abs != "" {
				return abs
			}
			return p
		}
	}
	return ""
}

// FindSidecar 查找 miot_mjpeg.py 的绝对路径。
func FindSidecar() string {
	var cands []string
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		cands = append(cands,
			filepath.Join(dir, "miot", "miot_mjpeg.py"),
			filepath.Join(dir, "app", "miot", "miot_mjpeg.py"),
			filepath.Join(dir, "..", "app", "miot", "miot_mjpeg.py"),
		)
	}
	if wd, err := os.Getwd(); err == nil {
		cands = append(cands, filepath.Join(wd, "app", "miot", "miot_mjpeg.py"))
	}
	cands = append(cands,
		"/vol1/@appcenter/micam/miot/miot_mjpeg.py",
		"/vol1/@appcenter/micam/app/miot/miot_mjpeg.py",
	)
	for _, p := range cands {
		if p == "" {
			continue
		}
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			abs, _ := filepath.Abs(p)
			if abs != "" {
				return abs
			}
			return p
		}
	}
	return ""
}

// PythonBin 找一个可用的 python3。
func PythonBin() string {
	for _, p := range []string{"/usr/bin/python3", "/usr/local/bin/python3", "python3"} {
		if p == "python3" {
			return p
		}
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return "python3"
}

// FindSidecarRaw 找【原始码流】sidecar（miot_camera.py）。
//
// 与 FindSidecar（miot_mjpeg.py）的区别：
//
//	miot_mjpeg.py  输出 MJPEG（已转码）—— 用于预览/缩略图
//	miot_camera.py 输出 H264/H265 原始码流 —— 用于录像（省 CPU、无损）
func FindSidecarRaw() string {
	var cands []string
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		cands = append(cands,
			filepath.Join(dir, "miot", "miot_camera.py"),
			filepath.Join(dir, "app", "miot", "miot_camera.py"),
			filepath.Join(dir, "..", "app", "miot", "miot_camera.py"),
		)
	}
	if wd, err := os.Getwd(); err == nil {
		cands = append(cands, filepath.Join(wd, "app", "miot", "miot_camera.py"))
	}
	cands = append(cands,
		"/vol1/@appcenter/micam/miot/miot_camera.py",
		"/vol1/@appcenter/micam/app/miot/miot_camera.py",
	)
	for _, p := range cands {
		if p == "" {
			continue
		}
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			abs, _ := filepath.Abs(p)
			if abs != "" {
				return abs
			}
			return p
		}
	}
	return ""
}

// FindMiioScript 找 miio.py（本地 miIO 协议客户端）。
func FindMiioScript() string {
	var cands []string
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		cands = append(cands,
			filepath.Join(dir, "miot", "miio.py"),
			filepath.Join(dir, "..", "app", "miot", "miio.py"),
		)
	}
	if wd, err := os.Getwd(); err == nil {
		cands = append(cands, filepath.Join(wd, "app", "miot", "miio.py"))
	}
	cands = append(cands, "/vol1/@appcenter/micam/miot/miio.py")
	for _, p := range cands {
		if p == "" {
			continue
		}
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			abs, _ := filepath.Abs(p)
			if abs != "" {
				return abs
			}
			return p
		}
	}
	return ""
}
