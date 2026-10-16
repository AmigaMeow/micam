package main

// OAuth2 扫码授权接口（2026-10-11）。
//
// 用户只需扫一次码（复用插件已有的扫码体验），
// 后端自动串联：
//   扫码登录 → 拿 cookies → 走 OAuth2 authorize → 拿 code → 换 access_token
//
// 接口:
//   POST /api/miot/qr          发起扫码，返回二维码 base64
//   GET  /api/miot/qr/status   轮询状态（成功时自动完成 token 交换）
//   POST /api/miot/qr/cancel   取消

import (
	"encoding/base64"
	"net/http"
	"sync"
	"time"

	"github.com/micam/micam/internal/miot"
)

var (
	miotQRMu   sync.Mutex
	miotQRFlow *miot.QRFlow
)

// handleMiotQRStart POST /api/miot/qr
func (s *Server) handleMiotQRStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 POST")
		return
	}

	uid := ""
	miotAuthMu.Lock()
	if miotAuthSt != nil {
		uid = miotAuthSt.DeviceUUID
	}
	miotAuthMu.Unlock()

	flow, err := miot.NewQRFlow(uid)
	if err != nil {
		writeErr(w, 500, "创建会话失败: "+err.Error())
		return
	}
	if err := flow.StartQR(); err != nil {
		writeErr(w, 502, "申请二维码失败: "+err.Error())
		return
	}
	img, err := flow.FetchQRImage()
	if err != nil {
		writeErr(w, 502, "下载二维码失败: "+err.Error())
		return
	}

	miotQRMu.Lock()
	miotQRFlow = flow
	miotQRMu.Unlock()

	// 后台长轮询：扫码成功会自动走 OAuth2 换 code，再换 token
	go func() {
		flow.Wait()
		st := flow.Status()
		if st["state"] == "success" && flow.Code != "" {
			s.finishMiotAuth(flow)
		}
	}()

	logf("[miot] 扫码授权已发起")
	writeJSON(w, 200, map[string]any{
		"ok":       true,
		"qrBase64": "data:image/png;base64," + base64.StdEncoding.EncodeToString(img),
		"timeout":  flow.Timeout,
		"note":     "用米家 App 或小米账号扫一扫；成功后自动完成授权，无需手动复制",
	})
}

// finishMiotAuth 用扫码拿到的 code 换 token 并落盘。
func (s *Server) finishMiotAuth(flow *miot.QRFlow) {
	cli := miot.NewOAuth2Client(flow.DeviceUUID, "")
	tok, err := cli.ExchangeCode(flow.Code)
	if err != nil {
		logf("[miot] 扫码后换取令牌失败: %v", err)
		return
	}
	a := &miotAuth{
		DeviceUUID:   cli.DeviceUUID,
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix(),
		BoundAt:      time.Now().Format(time.RFC3339),
		RedirectURI:  cli.RedirectURI,
	}
	_ = saveMiotAuth(a)
	miotAuthMu.Lock()
	miotAuthSt = a
	miotAuthMu.Unlock()
	logf("[miot] ✅ 扫码授权成功！access_token 有效期 %d 秒", tok.ExpiresIn)
}

// handleMiotQRStatus GET /api/miot/qr/status
func (s *Server) handleMiotQRStatus(w http.ResponseWriter, r *http.Request) {
	miotQRMu.Lock()
	flow := miotQRFlow
	miotQRMu.Unlock()
	if flow == nil {
		writeErr(w, 404, "没有进行中的扫码会话")
		return
	}
	out := flow.Status()

	// 顺便报告绑定状态
	miotAuthMu.Lock()
	bound := miotAuthSt != nil
	miotAuthMu.Unlock()
	out["bound"] = bound
	writeJSON(w, 200, out)
}

// handleMiotQRCancel POST /api/miot/qr/cancel
func (s *Server) handleMiotQRCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 POST")
		return
	}
	miotQRMu.Lock()
	miotQRFlow = nil
	miotQRMu.Unlock()
	writeJSON(w, 200, map[string]any{"ok": true, "msg": "已取消"})
}
