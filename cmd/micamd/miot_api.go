package main

// MiOT OAuth2 官方通道（2026-10-11）。
//
// ===== 背景 =====
//
// 原路径（passToken + miss_get_vendor）对该账号已被小米长期限流
// （实测 18 天不恢复，含 7 天零调用静默期）。
//
// 而小米官方仓库 XiaoMi/xiaomi-miloco 走的是【完全不同】的通道：
//   OAuth2 授权 → access_token → libmiot_camera_lite → PPCS 局域网 P2P
// 全程不调用 miss_get_vendor。
//
// 本文件把这条通道接进插件。
//
// ===== 接口 =====
//
//	GET  /api/miot/bind      生成授权 URL（用户在浏览器打开）
//	POST /api/miot/authorize 用回调的 code 换 token 并落盘
//	POST /api/miot/auto-auth 从已登录账号一键同步授权
//	GET  /api/miot/status    绑定状态
//	POST /api/miot/unbind    解绑
//
// ===== 状态存储 =====
//
// <dataDir>/miot-oauth.json（0600）:
//   { deviceUuid, accessToken, refreshToken, expiresAt, boundAt }

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/micam/micam/internal/miot"
)

type miotAuth struct {
	DeviceUUID   string `json:"deviceUuid"`
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"` // unix 秒
	BoundAt      string `json:"boundAt"`
	// RedirectURI 续期时必须与授权时一致（官方会校验）
	RedirectURI string `json:"redirectUri,omitempty"`
}

func miotAuthFile() string {
	return filepath.Join(dataDirPath, "miot-oauth.json")
}

var (
	miotAuthMu  = struct{ sync.Mutex }{}
	miotAuthSt  *miotAuth
	miotAuthCli *miot.OAuth2Client
)

// loadMiotAuth 启动与续期前载入最新授权状态。
func loadMiotAuth() *miotAuth {
	raw, err := os.ReadFile(miotAuthFile())
	if err != nil {
		return nil
	}
	var a miotAuth
	if json.Unmarshal(raw, &a) != nil || a.AccessToken == "" {
		return nil
	}
	miotAuthMu.Lock()
	miotAuthSt = &a
	miotAuthCli = miot.NewOAuth2Client(a.DeviceUUID, "")
	miotAuthMu.Unlock()
	return &a
}

var autoReauthMu sync.Mutex

// autoReauthFromAccount 使用用户已登录的小米账号会话 (PassToken) 重新拉取 OAuth code 并换取新令牌。
// 当 refresh_token 因轮换冲突或过期失效时，此方法可全自动自愈，无需用户重新手动授权。
func (s *Server) autoReauthFromAccount() error {
	autoReauthMu.Lock()
	defer autoReauthMu.Unlock()

	// 先检查磁盘上的令牌是否已被其它协程更新且有效
	if a := loadMiotAuth(); a != nil {
		left := time.Until(time.Unix(a.ExpiresAt, 0))
		if left > 2*time.Hour && a.AccessToken != "" {
			logf("[miot] 检测到磁盘已有最新有效令牌 (剩余 %.1f 小时)，直接复用", left.Hours())
			return nil
		}
	}

	sessions := s.store.AllSessions()
	if len(sessions) == 0 || sessions[0].PassToken == "" {
		return fmt.Errorf("尚未登录小米账号，无法自动重签")
	}
	sess := sessions[0]

	miotAuthMu.Lock()
	uid := ""
	if miotAuthSt != nil {
		uid = miotAuthSt.DeviceUUID
	}
	cli := miot.NewOAuth2Client(uid, "")
	miotAuthCli = cli
	miotAuthMu.Unlock()

	flow, err := miot.NewQRFlow(cli.DeviceUUID)
	if err != nil {
		return fmt.Errorf("创建授权客户端失败: %w", err)
	}
	flow.UserID = sess.UserID
	flow.PassToken = sess.PassToken
	flow.State = "success"

	if err := flow.FetchOAuthCodeDirect(); err != nil {
		return fmt.Errorf("自动重签拉取 OAuth code 失败: %w", err)
	}

	tok, err := cli.ExchangeCode(flow.Code)
	if err != nil {
		return fmt.Errorf("自动重签换取令牌失败: %w", err)
	}

	na := &miotAuth{
		DeviceUUID:   cli.DeviceUUID,
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix(),
		BoundAt:      time.Now().Format(time.RFC3339),
		RedirectURI:  cli.RedirectURI,
	}
	if err := saveMiotAuth(na); err != nil {
		logf("[miot] 保存重签令牌失败: %v", err)
	}

	miotAuthMu.Lock()
	miotAuthSt = na
	miotAuthMu.Unlock()

	logf("[miot] 自动根据小米账号凭据重签 OAuth2 令牌成功 (有效期 %d 小时)", tok.ExpiresIn/3600)
	return nil
}

func saveMiotAuth(a *miotAuth) error {
	raw, _ := json.MarshalIndent(a, "", "  ")
	return os.WriteFile(miotAuthFile(), raw, 0o600)
}

// handleMiotBind GET /api/miot/bind
//
// 返回授权 URL。用户在浏览器打开、登录小米账号、点同意，
// 回调页会显示一段 base64（含 code + state），把它提交到
// /api/miot/authorize 即可。
func (s *Server) handleMiotBind(w http.ResponseWriter, r *http.Request) {
	miotAuthMu.Lock()
	// 复用已有 deviceUuid（官方同一 uuid 对应同一 device_id）
	uid := ""
	if miotAuthSt != nil {
		uid = miotAuthSt.DeviceUUID
	}
	cli := miot.NewOAuth2Client(uid, "")
	miotAuthCli = cli
	miotAuthMu.Unlock()

	writeJSON(w, 200, map[string]any{
		"ok":       true,
		"authUrl":  cli.AuthURL(false),
		"deviceId": cli.DeviceID,
		"state":    cli.State,
		"note":     "在浏览器打开 authUrl 完成授权；回调页给出的一段 base64 提交到 /api/miot/authorize",
	})
}

// handleMiotAutoAuth POST /api/miot/auto-auth
func (s *Server) handleMiotAutoAuth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 POST")
		return
	}
	if err := s.autoReauthFromAccount(); err != nil {
		writeErr(w, 500, "一键同步授权失败: "+err.Error())
		return
	}
	miotAuthMu.Lock()
	a := miotAuthSt
	miotAuthMu.Unlock()
	expiresIn := int64(0)
	if a != nil {
		expiresIn = a.ExpiresAt - time.Now().Unix()
	}
	writeJSON(w, 200, map[string]any{
		"ok":        true,
		"expiresIn": expiresIn,
		"msg":       "一键同步授权成功！官方通道已激活。",
	})
}

// handleMiotAuthorize POST /api/miot/authorize  {code, state}
func (s *Server) handleMiotAuthorize(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeErr(w, 405, "只支持 POST")
		return
	}
	var req struct {
		Code  string `json:"code"`
		State string `json:"state"`
		// 也支持直接粘贴回调页那段 base64（官方 CLI 的做法）
		Payload string `json:"payload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "参数错误")
		return
	}

	// 若给的是 base64，先解开
	if req.Payload != "" && req.Code == "" {
		if c, st, err := miot.ParseAuthPayload(req.Payload); err == nil {
			req.Code, req.State = c, st
		} else {
			writeErr(w, 400, "授权信息解析失败: "+err.Error())
			return
		}
	}
	if req.Code == "" {
		writeErr(w, 400, "缺少 code")
		return
	}

	miotAuthMu.Lock()
	cli := miotAuthCli
	miotAuthMu.Unlock()
	if cli == nil {
		// 没有先调 /bind —— 用已有 uuid 重建（state 校验会失败，所以提示）
		writeErr(w, 400, "请先调用 /api/miot/bind 获取授权 URL")
		return
	}
	if !cli.VerifyState(req.State) {
		writeErr(w, 400, "state 不匹配（授权会话已过期，请重新获取授权 URL）")
		return
	}

	tok, err := cli.ExchangeCode(req.Code)
	if err != nil {
		logf("[miot] 换取令牌失败: %v", err)
		writeErr(w, 502, "换取令牌失败: "+err.Error())
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
	if err := saveMiotAuth(a); err != nil {
		logf("[miot] 保存令牌失败: %v", err)
	}

	miotAuthMu.Lock()
	miotAuthSt = a
	miotAuthMu.Unlock()

	logf("[miot] OAuth2 授权成功（有效期 %d 秒）", tok.ExpiresIn)
	writeJSON(w, 200, map[string]any{
		"ok":        true,
		"expiresIn": tok.ExpiresIn,
		"msg":       "授权成功。现在可以用官方通道拉流，不受 miss_get_vendor 限流影响。",
	})
}

// handleMiotStatus GET /api/miot/status
func (s *Server) handleMiotStatus(w http.ResponseWriter, r *http.Request) {
	miotAuthMu.Lock()
	a := miotAuthSt
	miotAuthMu.Unlock()

	libPath := miot.FindLib("")
	libOK := libPath != ""
	libVer := ""

	out := map[string]any{
		"libAvailable": libOK,
		"libPath":      libPath,
		"libVersion":   libVer,
		"bound":        a != nil,
	}
	if a != nil {
		left := time.Until(time.Unix(a.ExpiresAt, 0))
		out["boundAt"] = a.BoundAt
		out["expiresAt"] = time.Unix(a.ExpiresAt, 0).Format("2006-01-02 15:04")
		out["expiresInMin"] = int(left.Minutes())
		out["expired"] = left <= 0
		out["hasRefresh"] = a.RefreshToken != ""
	}
	writeJSON(w, 200, out)
}

// handleMiotUnbind POST /api/miot/unbind
func (s *Server) handleMiotUnbind(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeErr(w, 405, "只支持 POST")
		return
	}
	miotAuthMu.Lock()
	miotAuthSt = nil
	miotAuthCli = nil
	miotAuthMu.Unlock()
	_ = os.Remove(miotAuthFile())
	writeJSON(w, 200, map[string]any{"ok": true, "msg": "已解绑 OAuth2 授权"})
}
