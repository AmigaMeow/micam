package main

// 诊断信息导出。
//
// 设计目标：
//   1. 用户遇到问题时，一键导出诊断包
//   2. 诊断包含环境信息 + 日志 + 配置（自动脱敏）
//   3. 方便提交到 GitHub Issues，我们据此做适配
//
// 脱敏规则：凭据、token、IP 后两段、设备 did 全部打码

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/micam/micam/internal/hardware"
)

// diagReport 诊断报告。
type diagReport struct {
	GeneratedAt string           `json:"generatedAt"`
	App         map[string]any   `json:"app"`
	Runtime     map[string]any   `json:"runtime"`
	Hardware    map[string]any   `json:"hardware"`
	Config      map[string]any   `json:"config"`
	Devices     []map[string]any `json:"devices"`
	Checks      []CheckItem      `json:"checks"`
	Stats       map[string]any   `json:"stats"`
}

// handleDiagReport 返回诊断报告（JSON，已脱敏）。
//
// GET /api/diag/report
func (s *Server) handleDiagReport(w http.ResponseWriter, r *http.Request) {
	rep := s.buildDiagReport()
	writeJSON(w, 200, rep)
}

// handleDiagBundle 打包诊断信息（tar.gz，含日志）。
//
// GET /api/diag/bundle
func (s *Server) handleDiagBundle(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	addFile := func(name string, data []byte) {
		_ = tw.WriteHeader(&tar.Header{
			Name:    name,
			Mode:    0o644,
			Size:    int64(len(data)),
			ModTime: time.Now(),
		})
		_, _ = tw.Write(data)
	}

	// 1) 诊断报告（JSON，脱敏）
	rep := s.buildDiagReport()
	if data, err := json.MarshalIndent(rep, "", "  "); err == nil {
		addFile("report.json", []byte(sanitize(string(data))))
	}

	// 2) 日志文件（脱敏）
	for _, name := range []string{"micamd.log", "info.log"} {
		path := filepath.Join(s.dataDir, name)
		if data, err := os.ReadFile(path); err == nil {
			// 只保留最后 500 行
			lines := strings.Split(string(data), "\n")
			if len(lines) > 500 {
				lines = lines[len(lines)-500:]
			}
			addFile("logs/"+name, []byte(sanitize(strings.Join(lines, "\n"))))
		}
	}

	// 3) 配置（脱敏）
	if data, err := json.MarshalIndent(s.cfg, "", "  "); err == nil {
		addFile("config.json", []byte(sanitize(string(data))))
	}

	// 4) 硬件信息（脱敏）
	cap := hardware.Detect(s.ffmpegPath())
	if data, err := json.MarshalIndent(cap, "", "  "); err == nil {
		addFile("hardware.json", []byte(sanitize(string(data))))
	}

	// 5) 编码器实测（社区适配的关键信息）
	//
	// 只列「ffmpeg 支持」不够 —— 服务进程可能拿不到设备权限。
	// 这里给出每个编码器的真实可用性。
	probe := map[string]any{
		"encoders": s.probeAllEncoders(),
		"note": "ok=false 表示实测不可用（可能是权限或驱动问题）。" +
			"请把这一节提供给开发者，便于针对性适配。",
	}
	if data, err := json.MarshalIndent(probe, "", "  "); err == nil {
		addFile("encoder-probe.json", data)
	}

	// 6) 转码缓存状态
	if data, err := json.MarshalIndent(s.cacheStats(), "", "  "); err == nil {
		addFile("transcode-cache.json", data)
	}

	// 5) 说明文件
	readme := `MiCam 诊断信息包
=====================

这个压缩包包含排查问题所需的信息：

  report.json    诊断报告（应用状态、设备列表、自检结果）
  hardware.json  硬件能力（CPU、核显、编码器）
  config.json    配置
  logs/          日志文件（最后 500 行）

所有敏感信息已自动脱敏：
  - 小米账号 ID / token
  - 摄像头 P2P 凭据
  - IP 地址后两段
  - 设备 did 部分打码

提交问题时请附上这个文件：
  https://github.com/AmigaMeow/micam/issues

生成时间：` + rep.GeneratedAt

	addFile("README.txt", []byte(readme))

	_ = tw.Close()
	_ = gz.Close()

	filename := fmt.Sprintf("micam-diag-%s.tar.gz", time.Now().Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", "attachment; filename="+filename)
	_, _ = w.Write(buf.Bytes())
}

// buildDiagReport 构建诊断报告。
func (s *Server) buildDiagReport() *diagReport {
	rep := &diagReport{
		GeneratedAt: time.Now().Format(time.RFC3339),
		App: map[string]any{
			"name":    appName,
			"version": appVersion,
		},
		Runtime: map[string]any{
			"goVersion": runtime.Version(),
			"os":        runtime.GOOS,
			"arch":      runtime.GOARCH,
			"numCPU":    runtime.NumCPU(),
			"goroutine": runtime.NumGoroutine(),
		},
		Hardware: map[string]any{},
		Config: map[string]any{
			"subtype":       s.cfg.Subtype,
			"recordEnabled": s.cfg.RecordEnabled,
			"recordSegment": s.cfg.RecordSegment,
			"recordDir":     s.cfg.RecordDir,
		},
	}

	// 硬件能力
	cap := hardware.Detect(s.ffmpegPath())
	rep.Hardware = map[string]any{
		"cpuModel":  cap.CPUModel,
		"cpuCores":  cap.CPUCores,
		"arch":      cap.Arch,
		"hasDRI":    cap.HasDRI,
		"gpuInfo":   cap.GPUInfo,
		"preferred": cap.Preferred,
		"encoders":  cap.Encoders,
	}

	// 设备（脱敏）
	for _, d := range s.store.Devices() {
		rep.Devices = append(rep.Devices, map[string]any{
			"name":      d.Name,
			"model":     d.Model,
			"online":    d.IsOnline,
			"didMask":   maskString(d.Did),
			"ipMask":    maskIP(d.LocalIP),
			"shareFlag": d.ShareFlag,
		})
	}

	// 自检结果
	var checks []CheckItem
	// 简化版自检（不重复完整逻辑）
	checks = append(checks, CheckItem{
		ID: "accounts", Name: "账号数", OK: len(s.store.Accounts()) > 0, Level: "ok",
		Detail: fmt.Sprintf("%d 个", len(s.store.Accounts())),
	})
	checks = append(checks, CheckItem{
		ID: "devices", Name: "设备数", OK: len(s.store.Devices()) > 0, Level: "ok",
		Detail: fmt.Sprintf("%d 台", len(s.store.Devices())),
	})
	rep.Checks = checks

	// 统计
	rep.Stats = map[string]any{
		"accountCount": len(s.store.Accounts()),
		"deviceCount":  len(s.store.Devices()),
		"recordings":   len(s.rec.List()),
	}

	return rep
}

// sanitize 脱敏内容。
//
// 处理：
//   - IP 地址后两段
//   - 小米账号 userId（9-11 位数字）
//   - 设备 did（9-11 位数字）
//   - token / passToken 等长字符串
func sanitize(s string) string {
	// 先脱敏 IP
	s = maskIPsInText(s)

	// 脱敏长 token（32 位以上十六进制/base64）
	s = maskTokens(s)

	// 脱敏 did / userId（9-11 位纯数字，但排除常见版本号/端口）
	s = maskDigits(s)

	return s
}

// maskTokens 脱敏长 token。
func maskTokens(s string) string {
	var out strings.Builder
	i := 0
	for i < len(s) {
		c := s[i]
		// 找十六进制或 base64 长串
		if isHexOrB64(c) {
			j := i
			for j < len(s) && isHexOrB64(s[j]) {
				j++
			}
			if j-i >= 32 {
				out.WriteString("***TOKEN***")
				i = j
				continue
			}
		}
		out.WriteByte(c)
		i++
	}
	return out.String()
}

func isHexOrB64(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') || c == '+' || c == '/' || c == '='
}

// maskDigits 脱敏 9-11 位数字（did/userId）。
//
// 注意：只处理独立出现的数字串，避免误伤版本号。
func maskDigits(s string) string {
	var out strings.Builder
	i := 0
	for i < len(s) {
		if s[i] >= '0' && s[i] <= '9' {
			j := i
			for j < len(s) && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			n := j - i
			// 9-11 位数字 -> 可能是 did/userId
			// 但如果前面是 '.' 或后面是 '.'，可能是 IP（已处理）
			// 如果后面跟 "MB"/"KB"/"%" 等，是数值
			isNum := false
			if j < len(s) {
				next := s[j]
				if next == 'M' || next == 'K' || next == '%' || next == 's' || next == 'm' {
					isNum = true
				}
			}
			// 前面是 'v' 或 '.' 的是版本号
			prev := byte(0)
			if i > 0 {
				prev = s[i-1]
			}
			if (prev == 'v' || prev == '.') && n <= 5 {
				isNum = true
			}

			if n >= 9 && n <= 11 && !isNum {
				out.WriteString("***ID***")
			} else {
				out.WriteString(s[i:j])
			}
			i = j
			continue
		}
		out.WriteByte(s[i])
		i++
	}
	return out.String()
}

// maskIPsInText 把文本里的 IP 后两段打码。
func maskIPsInText(s string) string {
	var out strings.Builder
	i := 0
	for i < len(s) {
		// 找数字开头的 IP 模式
		if s[i] >= '0' && s[i] <= '9' {
			j := i
			dots := 0
			for j < len(s) && ((s[j] >= '0' && s[j] <= '9') || s[j] == '.') {
				if s[j] == '.' {
					dots++
				}
				j++
			}
			if dots == 3 {
				ip := s[i:j]
				out.WriteString(maskIP(ip))
				i = j
				continue
			}
		}
		out.WriteByte(s[i])
		i++
	}
	return out.String()
}

// maskIP 把 IP 后两段打码：192.168.1.100 -> 192.168.*.*
func maskIP(ip string) string {
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		return ip
	}
	return parts[0] + "." + parts[1] + ".*.*"
}

// maskString 字符串打码（保留前 3 位）。
func maskString(s string) string {
	if len(s) <= 3 {
		return "***"
	}
	return s[:3] + "***"
}
