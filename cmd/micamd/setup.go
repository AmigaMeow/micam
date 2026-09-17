package main

// 安装向导 + 自检修复。
//
// 设计目标：
//   1. 首次打开时引导用户完成配置
//   2. 出问题时能自诊断，并尽可能自修复
//   3. 修复不了的要给出明确的排查指引

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// CheckItem 一项检查结果。
type CheckItem struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Level   string `json:"level"` // ok / warn / error
	Detail  string `json:"detail"`
	Fixable bool   `json:"fixable"`
	Hint    string `json:"hint,omitempty"`
}

// SetupStatus 整体状态。
type SetupStatus struct {
	Version      string      `json:"version"`
	Stage        string      `json:"stage"` // welcome / ready / error
	Checks       []CheckItem `json:"checks"`
	HasAccounts  bool        `json:"hasAccounts"`
	DeviceCount  int         `json:"deviceCount"`
	AccountCount int         `json:"accountCount"`
}

// handleSetupCheck 环境自检。
func (s *Server) handleSetupCheck(w http.ResponseWriter, r *http.Request) {
	var checks []CheckItem

	// 1) 二进制可执行
	exe, err := os.Executable()
	if err == nil {
		if fi, e := os.Stat(exe); e == nil && fi.Mode()&0o111 != 0 {
			checks = append(checks, CheckItem{
				ID: "binary", Name: "程序文件", OK: true, Level: "ok",
				Detail: filepath.Base(exe),
			})
		} else {
			checks = append(checks, CheckItem{
				ID: "binary", Name: "程序文件", OK: false, Level: "error",
				Detail: "不可执行", Hint: "重新安装插件",
			})
		}
	}

	// 2) ffmpeg 可用
	ff := s.ffmpegPath()
	if out, e := exec.Command(ff, "-version").Output(); e == nil {
		ver := "可用"
		if len(out) > 0 {
			line := strings.SplitN(string(out), "\n", 2)[0]
			if i := strings.Index(line, "version"); i >= 0 {
				ver = strings.TrimSpace(line[i+7:])
				if j := strings.Index(ver, " "); j > 0 {
					ver = ver[:j]
				}
			}
		}
		checks = append(checks, CheckItem{
			ID: "ffmpeg", Name: "转码引擎 (ffmpeg)", OK: true, Level: "ok",
			Detail: ver,
		})
	} else {
		checks = append(checks, CheckItem{
			ID: "ffmpeg", Name: "转码引擎 (ffmpeg)", OK: false, Level: "error",
			Detail: "未找到 " + ff,
			Hint:   "系统缺少 ffmpeg，无法播放视频",
		})
	}

	// 3) 数据目录可写
	if err := os.MkdirAll(s.dataDir, 0o755); err == nil {
		testFile := filepath.Join(s.dataDir, ".write-test")
		if e := os.WriteFile(testFile, []byte("x"), 0o600); e == nil {
			_ = os.Remove(testFile)
			checks = append(checks, CheckItem{
				ID: "datadir", Name: "数据目录", OK: true, Level: "ok",
				Detail: s.dataDir,
			})
		} else {
			checks = append(checks, CheckItem{
				ID: "datadir", Name: "数据目录", OK: false, Level: "error",
				Detail: "不可写: " + e.Error(),
				Hint:   "检查目录权限",
			})
		}
	}

	// 4) 账号
	accounts := s.store.Accounts()
	hasAccounts := len(accounts) > 0
	if hasAccounts {
		online := 0
		for _, a := range accounts {
			if on, _ := a["online"].(bool); on {
				online++
			}
		}
		checks = append(checks, CheckItem{
			ID: "accounts", Name: "小米账号", OK: online > 0, Level: "ok",
			Detail: fmt.Sprintf("%d 个账号，%d 个在线", len(accounts), online),
		})
	} else {
		checks = append(checks, CheckItem{
			ID: "accounts", Name: "小米账号", OK: false, Level: "warn",
			Detail: "还没有账号",
			Hint:   "用米家 App 扫码登录",
		})
	}

	// 5) 设备
	devCount := 0
	if hasAccounts {
		for _, sess := range s.store.AllSessions() {
			if devs, e := sess.AllCameras(); e == nil {
				devCount += len(devs)
			}
		}
	}
	if hasAccounts {
		if devCount > 0 {
			checks = append(checks, CheckItem{
				ID: "devices", Name: "设备发现", OK: true, Level: "ok",
				Detail: fmt.Sprintf("发现 %d 台", devCount),
			})
		} else {
			checks = append(checks, CheckItem{
				ID: "devices", Name: "设备发现", OK: false, Level: "warn",
				Detail: "未发现设备",
				Hint:   "确认米家 App 里有摄像头",
			})
		}
	}

	// 6) 录像目录
	recDir := s.cfg.RecordDir
	if recDir == "" {
		recDir = filepath.Join(s.dataDir, "recordings")
	}
	if err := os.MkdirAll(recDir, 0o755); err == nil {
		checks = append(checks, CheckItem{
			ID: "recdir", Name: "录像目录", OK: true, Level: "ok",
			Detail: recDir,
		})
	}

	// 判定阶段
	stage := "ready"
	for _, c := range checks {
		if c.Level == "error" {
			stage = "error"
			break
		}
	}
	if !hasAccounts {
		stage = "welcome"
	}

	writeJSON(w, 200, SetupStatus{
		Version:      appVersion,
		Stage:        stage,
		Checks:       checks,
		HasAccounts:  hasAccounts,
		DeviceCount:  devCount,
		AccountCount: len(accounts),
	})
}

// handleSetupRepair 尝试自修复。
func (s *Server) handleSetupRepair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 POST")
		return
	}

	var fixed []string
	var failed []string

	// 1) 清理残留 socket（网关连不上时常见原因）
	sockPath := filepath.Join(os.Getenv("TRIM_APPDEST"), "app.sock")
	if sockPath != "/app.sock" {
		if _, err := os.Stat(sockPath); err == nil {
			// socket 存在但可能已失效，检查能否连接
			if !canConnectUnix(sockPath) {
				if err := os.Remove(sockPath); err == nil {
					fixed = append(fixed, "清理了失效的 socket 文件")
				}
			}
		}
	}

	// 2) 清理冲突的 systemd 服务（导致 Bad Gateway 的常见原因）
	unitPath := "/etc/systemd/system/micam.service"
	if _, err := os.Stat(unitPath); err == nil {
		// 有手动服务，尝试停用
		if err := exec.Command("systemctl", "stop", "micam").Run(); err != nil {
			logf("[setup] systemctl stop micam 失败: %v", err)
		}
		if err := exec.Command("systemctl", "disable", "micam").Run(); err != nil {
			logf("[setup] systemctl disable micam 失败: %v", err)
		}
		if err := os.Remove(unitPath); err == nil {
			if err := exec.Command("systemctl", "daemon-reload").Run(); err != nil {
				logf("[setup] systemctl daemon-reload 失败: %v", err)
			}
			fixed = append(fixed, "删除了冲突的手动 systemd 服务")
		} else {
			failed = append(failed, "无法删除 systemd 服务（需要 root 权限）")
		}
	}

	// 3) 清理残留 PID
	pidFile := filepath.Join(s.dataDir, "app.pid")
	if _, err := os.Stat(pidFile); err == nil {
		_ = os.Remove(pidFile)
		fixed = append(fixed, "清理了残留的 PID 文件")
	}

	// 4) 重建录像目录
	recDir := s.cfg.RecordDir
	if recDir == "" {
		recDir = filepath.Join(s.dataDir, "recordings")
	}
	if err := os.MkdirAll(recDir, 0o755); err == nil {
		fixed = append(fixed, "确认录像目录存在")
	}

	// 5) 重新加载账号
	s.store.ReloadAccounts()
	fixed = append(fixed, "重新加载了账号凭据")

	writeJSON(w, 200, map[string]any{
		"ok":     len(failed) == 0,
		"fixed":  fixed,
		"failed": failed,
	})
}

// canConnectUnix 测试 unix socket 是否可用。
func canConnectUnix(path string) bool {
	c, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

var _ = json.Marshal
