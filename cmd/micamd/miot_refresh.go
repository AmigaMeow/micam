package main

// OAuth2 令牌自动续期与自愈（2026-10-25 升级）。
//
// ===== 为什么必须有自愈 =====
//
// 1. access_token 只有 3 天有效期。
// 2. refresh_token 存在轮换机制 (Rotation)，若多进程并发或网络异常导致 refresh_token 失效，
//    普通 Refresh 接口会返回 96009 invalid refresh token。
// 3. 此时必须自动调用 autoReauthFromAccount，通过用户已登录的 PassToken 无缝重签，
//    实现零人工介入的永久自愈，绝不让用户遭遇动不动就无法查看画面的问题。

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
	a := loadMiotAuth()
	if a == nil || a.AccessToken == "" {
		return // 未授权，无需续期
	}

	left := time.Until(time.Unix(a.ExpiresAt, 0))
	if left > miotRefreshAhead {
		return // 还没到该续期的时候
	}

	// 防止并发续期（tick 与手动触发可能同时）
	if !miotRefreshMu.TryLock() {
		return
	}
	defer miotRefreshMu.Unlock()

	logf("[miot] 令牌剩余 %.1f 小时，开始自动续期...", left.Hours())

	var refreshErr error
	if a.RefreshToken != "" {
		cli := miot.NewOAuth2Client(a.DeviceUUID, a.RedirectURI)
		tok, err := cli.Refresh(a.RefreshToken)
		if err == nil {
			na := &miotAuth{
				DeviceUUID:   a.DeviceUUID,
				AccessToken:  tok.AccessToken,
				RefreshToken: tok.RefreshToken,
				ExpiresAt:    time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix(),
				BoundAt:      a.BoundAt,
				RedirectURI:  a.RedirectURI,
			}
			if err := saveMiotAuth(na); err != nil {
				logf("[miot] 新令牌保存失败: %v", err)
			}
			miotAuthMu.Lock()
			miotAuthSt = na
			miotAuthMu.Unlock()
			logf("[miot] 令牌已续期 (新有效期 %d 小时)", tok.ExpiresIn/3600)
			return
		}
		refreshErr = err
		logf("[miot] 令牌常规续期失败: %v，尝试通过账号凭据自动重签自愈...", err)
	} else {
		logf("[miot] 令牌即将在 %.1f 小时后过期且无 refresh_token，尝试通过账号凭据自动重签自愈...", left.Hours())
	}

	// 常规续期失败，通过账号 Session 自动自愈
	if err := s.autoReauthFromAccount(); err != nil {
		logf("[miot] 自动重签自愈失败: %v (续期原因: %v，%.1f 小时后重试)", err, refreshErr, miotRefreshCheck.Hours())
	}
}

// handleMiotRefresh POST /api/miot/refresh
//
// 手动触发续期（诊断用，也方便用户"立即刷新"）。
func (s *Server) handleMiotRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeErr(w, 405, "只支持 POST")
		return
	}
	a := loadMiotAuth()
	if a == nil {
		writeErr(w, 400, "尚未授权")
		return
	}
	if a.RefreshToken == "" {
		if err := s.autoReauthFromAccount(); err != nil {
			writeErr(w, 400, "无 refresh_token 且自动重签失败: "+err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{
			"ok":  true,
			"msg": "令牌已通过账号会话重新签发",
		})
		return
	}

	cli := miot.NewOAuth2Client(a.DeviceUUID, a.RedirectURI)
	tok, err := cli.Refresh(a.RefreshToken)
	if err != nil {
		logf("[miot] 手动常规续期失败: %v，尝试通过账号自动重签...", err)
		if rerr := s.autoReauthFromAccount(); rerr != nil {
			logf("[miot] 手动重签自愈失败: %v", rerr)
			writeErr(w, 502, "续期失败: "+err.Error()+"；重签失败: "+rerr.Error())
			return
		}
		writeJSON(w, 200, map[string]any{
			"ok":  true,
			"msg": "常规续期失败，已通过账号会话自动重签自愈成功",
		})
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

	logf("[miot] 手动续期成功 (%d 小时)", tok.ExpiresIn/3600)
	writeJSON(w, 200, map[string]any{
		"ok":        true,
		"expiresIn": tok.ExpiresIn,
		"msg":       "令牌已续期",
	})
}

// forceMiotRefresh 在拉流失败/401/空闲超时时主动触发一次续期与自愈。
func (s *Server) forceMiotRefresh() {
	if !miotRefreshMu.TryLock() {
		return
	}
	defer miotRefreshMu.Unlock()

	a := loadMiotAuth()
	if a == nil {
		_ = s.autoReauthFromAccount()
		return
	}

	logf("[miot] 触发自动重试刷新令牌...")
	if a.RefreshToken != "" {
		cli := miot.NewOAuth2Client(a.DeviceUUID, a.RedirectURI)
		tok, err := cli.Refresh(a.RefreshToken)
		if err == nil {
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
			logf("[miot] 令牌已自动续期 (%d 小时)", tok.ExpiresIn/3600)
			return
		}
		logf("[miot] 自动刷新令牌失败: %v，启动账号重签自愈...", err)
	}
	if err := s.autoReauthFromAccount(); err != nil {
		logf("[miot] 账号重签自愈失败: %v", err)
	}
}
