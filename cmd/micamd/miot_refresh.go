package main

// OAuth2 令牌自动续期（2026-10-11）。
//
// ===== 为什么必须有 =====
//
// access_token 只有 3 天有效期。没有续期的话，3 天后
// 实时画面会突然全部失效，用户看到的是"又坏了"。
//
// 官方给了 refresh_token（长期有效），用它换新的 access_token 即可。
// 这是官方标准流程（cloud.py 的 refresh_access_token_async）。
//
// ===== 策略 =====
//
//   · 每 30 分钟检查一次
//   · 剩余 < 12 小时就续期
//   · 续期失败不慌（还有时间），下次再试；连续失败会在日志里体现
//   · 成功后续期新的 refresh_token（官方每次都返回新的）

import (
	"net/http"
	"sync"
	"time"

	"github.com/micam/micam/internal/miot"
)

const (
	miotRefreshCheck = 30 * time.Minute // 检查间隔
	miotRefreshAhead = 12 * time.Hour   // 剩余少于这个就续期
)

var miotRefreshMu sync.Mutex

// startMiotRefresher 启动令牌续期守护。
func (s *Server) startMiotRefresher() {
	go func() {
		// 启动 2 分钟后做首次检查（避开启动期）
		time.Sleep(2 * time.Minute)
		s.miotRefreshTick()

		t := time.NewTicker(miotRefreshCheck)
		defer t.Stop()
		for range t.C {
			s.miotRefreshTick()
		}
	}()
}

// miotRefreshTick 一次续期检查。
func (s *Server) miotRefreshTick() {
	miotAuthMu.Lock()
	a := miotAuthSt
	miotAuthMu.Unlock()
	if a == nil || a.AccessToken == "" {
		return // 未授权，无需续期
	}

	left := time.Until(time.Unix(a.ExpiresAt, 0))
	if left > miotRefreshAhead {
		return // 还没到该续期的时候
	}
	if a.RefreshToken == "" {
		logf("[miot] ⚠️ 令牌将在 %.1f 小时后过期，但没有 refresh_token，需要重新授权", left.Hours())
		return
	}

	// 防止并发续期（tick 与手动触发可能同时）
	if !miotRefreshMu.TryLock() {
		return
	}
	defer miotRefreshMu.Unlock()

	logf("[miot] 令牌剩余 %.1f 小时，开始自动续期…", left.Hours())

	cli := miot.NewOAuth2Client(a.DeviceUUID, a.RedirectURI)
	tok, err := cli.Refresh(a.RefreshToken)
	if err != nil {
		logf("[miot] ⚠️ 令牌续期失败: %v（%.1f 小时后重试）", err, miotRefreshCheck.Hours())
		return
	}

	na := &miotAuth{
		DeviceUUID:   a.DeviceUUID,
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix(),
		BoundAt:      a.BoundAt,
		RedirectURI:  a.RedirectURI,
	}
	if err := saveMiotAuth(na); err != nil {
		logf("[miot] ⚠️ 新令牌保存失败: %v", err)
	}

	miotAuthMu.Lock()
	miotAuthSt = na
	miotAuthMu.Unlock()

	logf("[miot] ✅ 令牌已续期（新有效期 %d 小时）", tok.ExpiresIn/3600)
}

// handleMiotRefresh POST /api/miot/refresh
//
// 手动触发续期（诊断用，也方便用户"立即刷新"）。
func (s *Server) handleMiotRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeErr(w, 405, "只支持 POST")
		return
	}
	miotAuthMu.Lock()
	a := miotAuthSt
	miotAuthMu.Unlock()
	if a == nil {
		writeErr(w, 400, "尚未授权")
		return
	}
	if a.RefreshToken == "" {
		writeErr(w, 400, "没有 refresh_token，需要重新授权")
		return
	}

	cli := miot.NewOAuth2Client(a.DeviceUUID, a.RedirectURI)
	tok, err := cli.Refresh(a.RefreshToken)
	if err != nil {
		logf("[miot] 手动续期失败: %v", err)
		writeErr(w, 502, "续期失败: "+err.Error())
		return
	}
	na := &miotAuth{
		DeviceUUID:   a.DeviceUUID,
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix(),
		BoundAt:      a.BoundAt,
		RedirectURI:  a.RedirectURI,
	}
	_ = saveMiotAuth(na)
	miotAuthMu.Lock()
	miotAuthSt = na
	miotAuthMu.Unlock()

	logf("[miot] ✅ 手动续期成功（%d 小时）", tok.ExpiresIn/3600)
	writeJSON(w, 200, map[string]any{
		"ok":        true,
		"expiresIn": tok.ExpiresIn,
		"msg":       "令牌已续期",
	})
}

// forceMiotRefresh 在拉流失败/空闲超时时主动触发一次续期。
func (s *Server) forceMiotRefresh() {
	miotAuthMu.Lock()
	a := miotAuthSt
	miotAuthMu.Unlock()
	if a == nil || a.RefreshToken == "" {
		return
	}
	if !miotRefreshMu.TryLock() {
		return
	}
	defer miotRefreshMu.Unlock()

	logf("[miot] 触发自动重试刷新令牌…")
	cli := miot.NewOAuth2Client(a.DeviceUUID, a.RedirectURI)
	tok, err := cli.Refresh(a.RefreshToken)
	if err != nil {
		logf("[miot] 自动刷新令牌失败: %v", err)
		return
	}
	na := &miotAuth{
		DeviceUUID:   a.DeviceUUID,
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix(),
		BoundAt:      a.BoundAt,
		RedirectURI:  a.RedirectURI,
	}
	_ = saveMiotAuth(na)
	miotAuthMu.Lock()
	miotAuthSt = na
	miotAuthMu.Unlock()
	logf("[miot] ✅ 令牌已自动续期（%d 小时）", tok.ExpiresIn/3600)
}
