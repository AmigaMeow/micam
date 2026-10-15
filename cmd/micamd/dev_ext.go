package main

// 开发者页其余能力：API Token 鉴权、Webhook 测试、重置开放模式配置。
//
// 鉴权设计（**关键：向后兼容**）
//   · 不带 Authorization 头的请求**不进校验分支** —— 现有 UI / 局域网直连行为不变
//   · 带了 token：无效 401；只读 token 发非 GET 请求 403
//   · 「本地文件访问」开关**只对带 token 的**录像文件请求生效（403 拒绝），
//     这样开关真实有效，又不会打断 App 自己的回放 / 下载
//   · 另外支持 ?token=xxx 查询参数，方便在浏览器地址栏直接试

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/micam/micam/internal/config"
)

const (
	devWebhookTimeout = 10 * time.Second
	devWebhookMaxBody = 4 << 10 // 只回显响应前 4KB
)

// fileAccessPrefixes 暴露录像文件的接口（开关关闭时对 token 调用方 403）。
var fileAccessPrefixes = []string{
	"/api/recordings/files",
	"/api/recordings/download",
	"/api/recordings/play",
	"/api/recordings/thumb",
	"/api/recordings/frame",
	"/api/recordings/export/download",
	"/api/recordings/export/list",
}

func isFileAccessPath(p string) bool {
	for _, pre := range fileAccessPrefixes {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

// tokenCtxKey 把校验通过的 token 放进 request context（将来 handler 取权限用）。
type tokenCtxKey struct{}

// bearerToken 取请求里的 token：优先 Authorization 头，其次 ?token=。
func bearerToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
			return strings.TrimSpace(h[7:])
		}
		return strings.TrimSpace(h)
	}
	return strings.TrimSpace(r.URL.Query().Get("token"))
}

// devAuthMiddleware API Token 鉴权。
func (s *Server) devAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 只约束 API；静态资源（页面/图标）不带 token 也要能打开
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}

		// ===== 敏感接口：始终要求访问码（2026-09-23 新增）=====
		//
		// 背景：实测 /api/accounts/export 无需任何凭据即可返回 passToken。
		// 这里对敏感接口强制校验访问码（本机请求放行，便于 SSH/终端运维）。
		//
		// 注意：这段必须在「无 token 放行」之前 —— 否则漏洞依旧。
		if isSensitivePath(r.URL.Path) && !s.cfg.Dev.AllowLANSensitive {
			if !isLocalRequest(r) {
				if accessLocked() {
					writeErr(w, 429, "访问码尝试过于频繁，请 5 分钟后再试")
					return
				}
				if !verifyAccessCode(requestAccessCode(r)) {
					recordAccessFailure()
					writeErr(w, 401, "该接口需要访问码（在 NAS 上查看 access-code.txt）")
					return
				}
			}
		}

		tok := bearerToken(r)
		if tok == "" {
			next.ServeHTTP(w, r) // 无 token：走原有路径（向后兼容）
			return
		}

		item := s.tokens.verify(tok)
		if item == nil {
			writeErr(w, 401, "API Token 无效或已禁用")
			return
		}
		if item.Scopes == "ro" && r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeErr(w, 403, "该 Token 为只读，不允许 "+r.Method)
			return
		}
		if !s.cfg.Dev.FileAccess && isFileAccessPath(r.URL.Path) {
			writeErr(w, 403, "「本地文件访问」未开启：Token 调用方不可访问录像文件接口")
			return
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tokenCtxKey{}, item)))
	})
}

// handleDevWebhookTest 向指定地址发一条**真实**的示例事件请求。
//
// POST /api/dev/webhook/test  {"url":"http://..."}
func (s *Server) handleDevWebhookTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 POST")
		return
	}
	if !s.cfg.Dev.Unlocked {
		writeErr(w, 403, "开发者模式未开启")
		return
	}

	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "参数错误")
		return
	}
	u := strings.TrimSpace(req.URL)
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		writeErr(w, 400, "地址必须以 http:// 或 https:// 开头")
		return
	}

	payload := map[string]any{
		"type": "event",
		"test": true,
		"at":   time.Now().Format(time.RFC3339),
		"event": map[string]any{
			"id":         "evt_test_0001",
			"time":       time.Now().Format("2006-01-02 15:04:05"),
			"did":        "1000000105",
			"device":     "门口",
			"type":       "motion",
			"desc":       "有人经过",
			"aiCls":      "person",
			"conf":       0.93,
			"importance": 0.7,
			"thumbURL":   "<你的 NAS 地址>/api/events/thumb?id=evt_test_0001",
		},
	}
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		writeErr(w, 500, "构造示例负载失败: "+err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), devWebhookTimeout)
	defer cancel()
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		writeErr(w, 400, "地址不合法: "+err.Error())
		return
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("User-Agent", "MiCam-Webhook-Test/"+appVersion)

	start := time.Now()
	resp, err := (&http.Client{Timeout: devWebhookTimeout}).Do(hreq)
	ms := time.Since(start).Milliseconds()
	if err != nil {
		writeJSON(w, 200, map[string]any{
			"ok":      false,
			"ms":      ms,
			"error":   err.Error(),
			"request": string(body),
		})
		return
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, devWebhookMaxBody))

	logf("开发者页 Webhook 测试: %s -> %d (%dms)", u, resp.StatusCode, ms)
	writeJSON(w, 200, map[string]any{
		"ok":      resp.StatusCode >= 200 && resp.StatusCode < 300,
		"status":  resp.StatusCode,
		"ms":      ms,
		"body":    string(rb),
		"request": string(body),
	})
}

// handleDevResetConfig 把开放模式开关恢复默认（保留开发者模式解锁状态）。
//
// 与 /api/dev/reset 的区别：那个是「关闭开发者模式」（连解锁一起退），
// 这个是效果图「工具与资源」里的「重置配置」，属于危险操作，前端有二次确认。
//
// POST /api/dev/reset-config
func (s *Server) handleDevResetConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 POST")
		return
	}
	if !s.cfg.Dev.Unlocked {
		writeErr(w, 403, "开发者模式未开启")
		return
	}

	def := config.Default().Dev
	s.cfg.Dev.CORS = def.CORS
	s.cfg.Dev.AllowLAN = def.AllowLAN
	s.cfg.Dev.VerboseLog = def.VerboseLog
	s.cfg.Dev.MockMode = def.MockMode
	s.cfg.Dev.ExposeSpec = def.ExposeSpec
	s.cfg.Dev.Enabled = def.Enabled
	s.cfg.Dev.HLS = def.HLS
	s.cfg.Dev.Webhook = def.Webhook
	s.cfg.Dev.FileAccess = def.FileAccess
	s.cfg.Dev.WebSocket = def.WebSocket

	if err := s.cfg.SaveSelf(); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	logf("开放模式配置已重置为默认值")
	writeJSON(w, 200, map[string]any{"ok": true})
}
