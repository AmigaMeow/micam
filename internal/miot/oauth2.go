package miot

// 小米 MiOT OAuth2 客户端（2026-10-11）。
//
// ===== 为什么需要它 =====
//
// 插件原本走的是 passToken + /v2/device/miss_get_vendor（云端签发 P2P 凭据），
// 该端点对第三方客户端有长期限流（实测 18 天不恢复）。
//
// 小米官方仓库 XiaoMi/xiaomi-miloco 走的是完全不同的通道：
//
//   ① OAuth2 授权（用户在浏览器点"同意"）
//   ② 用 code 换 access_token
//   ③ access_token 交给 libmiot_camera_lite 建立 PPCS 局域网 P2P
//   ④ 全程不调用 miss_get_vendor
//
// 所以官方永远不会碰到那个限流。本文件实现第 ①② 步。
//
// ===== 协议细节（对齐官方 backend/miot/src/miot/cloud.py）=====
//
// 授权 URL:
//   https://account.xiaomi.com/oauth2/authorize
//     ?redirect_uri=<uri>
//     &client_id=2882303761520431603      （官方公开常量）
//     &response_type=code
//     &device_id=mico.<uuid>
//     &state=sha1("d=" + device_id)
//     &scope=1 6000                       （6000 = 智能家居服务）
//     &skip_confirm=false
//
// 换 token（注意是 GET + data=<json>，不是 POST form）:
//   GET https://mico.api.mijia.tech/app/v2/mico/oauth/get_token?data=<json>
//   json = {"client_id","redirect_uri","code","device_id"}
//   返回 {"code":0,"result":{"access_token","refresh_token","expires_in"}}
//
// 续期:
//   data 换成 {"client_id","redirect_uri","refresh_token","device_id"}

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	OAuth2ClientID        = "2882303761520431603"
	OAuth2AuthURL         = "https://account.xiaomi.com/oauth2/authorize"
	OAuth2APIHost         = "mico.api.mijia.tech"
	OAuth2Project         = "mico"
	OAuth2DefaultRedirect = "https://mico.api.mijia.tech/login_redirect"
)

// ScopeHome 智能家居服务权限（官方 scope 6000）。
var OAuth2Scopes = []int{1, 6000}

// OAuth2Client 一次授权会话。
type OAuth2Client struct {
	DeviceUUID  string
	DeviceID    string // "mico.<uuid>"
	State       string // sha1("d=" + DeviceID)
	RedirectURI string
	httpc       *http.Client
}

// NewOAuth2Client 用给定 uuid 建客户端（uuid 为空则随机）。
func NewOAuth2Client(deviceUUID, redirectURI string) *OAuth2Client {
	if deviceUUID == "" {
		deviceUUID = randomUUID()
	}
	if redirectURI == "" {
		redirectURI = OAuth2DefaultRedirect
	}
	did := OAuth2Project + "." + deviceUUID
	sum := sha1.Sum([]byte("d=" + did))
	return &OAuth2Client{
		DeviceUUID:  deviceUUID,
		DeviceID:    did,
		State:       hex.EncodeToString(sum[:]),
		RedirectURI: redirectURI,
		httpc:       &http.Client{Timeout: 30 * time.Second},
	}
}

// AuthURL 生成给用户在浏览器打开的授权链接。
func (c *OAuth2Client) AuthURL(skipConfirm bool) string {
	scopes := ""
	for i, s := range OAuth2Scopes {
		if i > 0 {
			scopes += " "
		}
		scopes += fmt.Sprint(s)
	}
	q := url.Values{
		"redirect_uri":  {c.RedirectURI},
		"client_id":     {OAuth2ClientID},
		"response_type": {"code"},
		"device_id":     {c.DeviceID},
		"state":         {c.State},
		"scope":         {scopes},
		"skip_confirm":  {fmt.Sprint(skipConfirm)},
	}
	return OAuth2AuthURL + "?" + q.Encode()
}

// OAuth2Token 换回来的令牌。
type OAuth2Token struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

// getToken 调 /oauth/get_token。
func (c *OAuth2Client) getToken(data map[string]string) (*OAuth2Token, error) {
	raw, _ := json.Marshal(data)
	u := fmt.Sprintf("https://%s/app/v2/%s/oauth/get_token?data=%s",
		OAuth2APIHost, OAuth2Project, url.QueryEscape(string(raw)))

	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "mico/docker")

	res, err := c.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)

	if res.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d: %s", res.StatusCode, truncate(string(body), 200))
	}

	var out struct {
		Code   int    `json:"code"`
		Msg    string `json:"message"`
		Result *struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			ExpiresIn    int    `json:"expires_in"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("响应解析失败: %w (%s)", err, truncate(string(body), 150))
	}
	if out.Result == nil || out.Result.AccessToken == "" {
		return nil, fmt.Errorf("授权失败: code=%d msg=%s", out.Code, out.Msg)
	}
	return &OAuth2Token{
		AccessToken:  out.Result.AccessToken,
		RefreshToken: out.Result.RefreshToken,
		ExpiresIn:    out.Result.ExpiresIn,
	}, nil
}

// ExchangeCode 用授权回调里的 code 换令牌。
func (c *OAuth2Client) ExchangeCode(code string) (*OAuth2Token, error) {
	if code == "" {
		return nil, fmt.Errorf("code 为空")
	}
	return c.getToken(map[string]string{
		"client_id":    OAuth2ClientID,
		"redirect_uri": c.RedirectURI,
		"code":         code,
		"device_id":    c.DeviceID,
	})
}

// Refresh 用 refresh_token 续期。
func (c *OAuth2Client) Refresh(refreshToken string) (*OAuth2Token, error) {
	if refreshToken == "" {
		return nil, fmt.Errorf("refresh_token 为空")
	}
	t, err := c.getToken(map[string]string{
		"client_id":     OAuth2ClientID,
		"redirect_uri":  c.RedirectURI,
		"refresh_token": refreshToken,
		"device_id":     c.DeviceID,
	})
	if err == nil && t.RefreshToken == "" {
		t.RefreshToken = refreshToken
	}
	return t, err
}

// VerifyState 校验回调带的 state（防 CSRF）。
func (c *OAuth2Client) VerifyState(got string) bool {
	return got != "" && got == c.State
}

// randomUUID 生成 v4 UUID（不引第三方库）。
func randomUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// 极端情况下退回时间戳，保证不 panic
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ParseAuthPayload 解析官方回调页给出的 base64 授权信息。
//
// 官方 CLI 的做法（cli/.../account.py::_parse_auth_payload）：
// 页面给的是一段 base64，解开是 {"code": "...", "state": "..."}。
func ParseAuthPayload(payload string) (code, state string, err error) {
	s := strings.TrimSpace(payload)
	if s == "" {
		return "", "", fmt.Errorf("授权信息为空")
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		// 可能用户直接粘了 JSON
		raw = []byte(s)
	}
	var v struct {
		Code  string `json:"code"`
		State string `json:"state"`
	}
	if e := json.Unmarshal(raw, &v); e != nil {
		return "", "", fmt.Errorf("格式错误（需要回调页给出的 base64）")
	}
	if v.Code == "" || v.State == "" {
		return "", "", fmt.Errorf("code 或 state 为空")
	}
	return v.Code, v.State, nil
}
