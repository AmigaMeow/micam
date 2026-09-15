package main

// 开放模式（开发者选项）。
//
// 设计参照 Android 开发者选项：
//   - 默认隐藏，连续点击版本号 7 次解锁
//   - 解锁后侧边栏出现「开发者」菜单
//   - 提供完整 API 文档、接口自测、能力探测
//
// 接口：
//   GET  /api/dev/status    开发者模式状态
//   POST /api/dev/tap       点击计数（7 次解锁）
//   POST /api/dev/config    修改开放模式开关
//   GET  /api/dev/openapi   OpenAPI 3.0 规格（给第三方接入）
//   GET  /api/dev/docs      Markdown 文档
//   POST /api/dev/reset     关闭开发者模式

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/micam/micam/internal/hardware"
)

const tapToUnlock = 7

// handleDevStatus 返回开发者模式状态。
//
// GET /api/dev/status
func (s *Server) handleDevStatus(w http.ResponseWriter, r *http.Request) {
	d := s.cfg.Dev
	writeJSON(w, 200, map[string]any{
		"unlocked":   d.Unlocked,
		"tapCount":   d.TapCount,
		"tapNeeded":  tapToUnlock,
		"flags": map[string]any{
			"cors":       d.CORS,
			"allowLan":   d.AllowLAN,
			"verboseLog": d.VerboseLog,
			"mockMode":   d.MockMode,
			"exposeSpec": d.ExposeSpec,
		},
		"app": map[string]any{
			"name":    appName,
			"version": appVersion,
		},
	})
}

// handleDevTap 点击计数（连续点击版本号触发）。
//
// POST /api/dev/tap
func (s *Server) handleDevTap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 POST")
		return
	}

	if s.cfg.Dev.Unlocked {
		writeJSON(w, 200, map[string]any{
			"unlocked": true,
			"message":  "开发者模式已开启",
		})
		return
	}

	s.cfg.Dev.TapCount++

	if s.cfg.Dev.TapCount >= tapToUnlock {
		s.cfg.Dev.Unlocked = true
		_ = s.cfg.SaveSelf()
		logf("开发者模式已解锁")
		writeJSON(w, 200, map[string]any{
			"unlocked": true,
			"message":  "开发者模式已开启",
		})
		return
	}

	left := tapToUnlock - s.cfg.Dev.TapCount
	writeJSON(w, 200, map[string]any{
		"unlocked":  false,
		"tapCount":  s.cfg.Dev.TapCount,
		"remaining": left,
		"message":   "再点击 " + itoa(left) + " 次开启开发者模式",
	})
}

// handleDevConfig 修改开放模式开关。
//
// POST /api/dev/config  {"cors":true,"verboseLog":true,...}
func (s *Server) handleDevConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 POST")
		return
	}
	if !s.cfg.Dev.Unlocked {
		writeErr(w, 403, "开发者模式未开启")
		return
	}

	var req struct {
		CORS       *bool `json:"cors"`
		AllowLAN   *bool `json:"allowLan"`
		VerboseLog *bool `json:"verboseLog"`
		MockMode   *bool `json:"mockMode"`
		ExposeSpec *bool `json:"exposeSpec"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "参数错误")
		return
	}

	if req.CORS != nil {
		s.cfg.Dev.CORS = *req.CORS
	}
	if req.AllowLAN != nil {
		s.cfg.Dev.AllowLAN = *req.AllowLAN
	}
	if req.VerboseLog != nil {
		s.cfg.Dev.VerboseLog = *req.VerboseLog
	}
	if req.MockMode != nil {
		s.cfg.Dev.MockMode = *req.MockMode
	}
	if req.ExposeSpec != nil {
		s.cfg.Dev.ExposeSpec = *req.ExposeSpec
	}

	if err := s.cfg.SaveSelf(); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}

	logf("开放模式已更新: cors=%v lan=%v verbose=%v",
		s.cfg.Dev.CORS, s.cfg.Dev.AllowLAN, s.cfg.Dev.VerboseLog)

	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleDevReset 关闭开发者模式。
//
// POST /api/dev/reset
func (s *Server) handleDevReset(w http.ResponseWriter, r *http.Request) {
	s.cfg.Dev.Unlocked = false
	s.cfg.Dev.TapCount = 0
	s.cfg.Dev.CORS = false
	s.cfg.Dev.MockMode = false
	_ = s.cfg.SaveSelf()
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleDevOpenAPI 返回 OpenAPI 3.0 规格。
//
// GET /api/dev/openapi
func (s *Server) handleDevOpenAPI(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.openAPISpec(r))
}

// handleDevDocs 返回 Markdown 文档。
//
// GET /api/dev/docs
func (s *Server) handleDevDocs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	_, _ = w.Write([]byte(s.markdownDocs()))
}

// handleDevCapabilities 返回能力探测（客户端据此适配）。
//
// GET /api/dev/capabilities
func (s *Server) handleDevCapabilities(w http.ResponseWriter, r *http.Request) {
	cap := hardware.Detect(s.ffmpegPath())
	writeJSON(w, 200, map[string]any{
		"app": map[string]any{
			"name":    appName,
			"version": appVersion,
		},
		"server": map[string]any{
			"time":     time.Now().Format(time.RFC3339),
			"timezone": time.Now().Format("MST"),
			"platform": "fnos",
		},
		"hardware": map[string]any{
			"cpuModel":  cap.CPUModel,
			"cpuCores":  cap.CPUCores,
			"arch":      cap.Arch,
			"hasDRI":    cap.HasDRI,
			"gpuInfo":   cap.GPUInfo,
			"preferred": cap.Preferred,
			"encoders":  cap.Encoders,
		},
		"features": map[string]any{
			"live.mjpeg":      true,
			"live.hls":        true,
			"live.snapshot":   true,
			"ptz":             true,
			"record":          true,
			"storageMonitor":  true,
			"deviceControl":   false, // 已移除
			"multiAccount":    true,
			"sharedDevices":   true,
		},
		"limits": map[string]any{
			"maxFPS":        15,
			"maxWidth":      2560,
			"hlsSegmentSec": 2,
			"probeTimeoutSec": 40,
		},
	})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}
