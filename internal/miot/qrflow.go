package miot

// OAuth2 扫码授权完整流程（2026-10-16 增强版）。
//
// ===== 为什么需要两步 =====
//
// 官方 OAuth2 authorize 端点要求"已登录"才直接返回 code，
// 否则跳登录页。而米家的扫码接口只认 sid=xiaomiio。
//
// 所以串联：
//   ① 用 sid=xiaomiio 扫码 -> 拿到登录 cookies（userId/passToken/serviceToken/...）
//   ② 将凭据注入 cookiejar -> 访问 /oauth2/authorize
//   ③ 自动跟随小米 STS 内部跳转 -> 捕获 redirect_uri 中的授权 code
//   ④ code 换 access_token 落盘
//
// 好处：用户只扫一次码，自动完成全套授权。

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	xiaomiAccountHost = "account.xiaomi.com"
	sidXiaomiIO       = "xiaomiio"
	stsHost           = "sts.api.io.mi.com"
)

// QRFlow 一次扫码授权会话。
type QRFlow struct {
	mu         sync.Mutex
	httpc      *http.Client
	jar        *cookiejar.Jar
	DeviceUUID string // mico.<uuid>
	QRURL      string // 二维码图片地址
	LoginURL   string
	LPURL      string // 长轮询地址
	Timeout    int

	State     string // waiting / scanned / success / failed
	Err       string
	Code      string // OAuth2 授权码
	UserID    string
	PassToken string
	Cookies   map[string]string
	Created   time.Time
}

// NewQRFlow 创建一个带 cookie jar 的会话。
func NewQRFlow(deviceUUID string) (*QRFlow, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	if deviceUUID == "" {
		deviceUUID = randomUUID()
	}
	return &QRFlow{
		httpc:      &http.Client{Jar: jar, Timeout: 30 * time.Second},
		jar:        jar,
		DeviceUUID: deviceUUID,
		State:      "waiting",
		Cookies:    map[string]string{},
		Created:    time.Now(),
	}, nil
}

func (f *QRFlow) setUA(req *http.Request) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Linux; Android 13; M2012K11AC) AppleWebKit/537.36 Chrome/120 Mobile Safari/537.36")
}

// decodeLogin 剥掉 &&&START&&& / &&&END&&& 再解析 JSON。
func decodeLogin(body []byte) ([]byte, error) {
	s := string(body)
	if i := strings.Index(s, "&&&START&&&"); i >= 0 {
		s = s[i+len("&&&START&&&"):]
	}
	if i := strings.Index(s, "&&&END&&&"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("空响应")
	}
	return []byte(s), nil
}

// StartQR 申请二维码。
func (f *QRFlow) StartQR() error {
	q := url.Values{
		"_qrsize":      {"480"},
		"qs":           {"?sid=" + sidXiaomiIO + "&_json=true"},
		"callback":     {"https://" + stsHost + "/sts"},
		"_hasLogo":     {"false"},
		"sid":          {sidXiaomiIO},
		"serviceParam": {""},
		"_locale":      {"zh_CN"},
		"_dc":          {strconv.FormatInt(time.Now().UnixNano()/1e6, 10)},
	}
	u := "https://" + xiaomiAccountHost + "/longPolling/loginUrl?" + q.Encode()
	req, _ := http.NewRequest("GET", u, nil)
	f.setUA(req)

	res, err := f.httpc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	body, err := decodeLogin(raw)
	if err != nil {
		return fmt.Errorf("解析失败: %w (%s)", err, truncate(string(raw), 150))
	}

	var v struct {
		QR       string `json:"qr"`
		LoginURL string `json:"loginUrl"`
		LP       string `json:"lp"`
		Timeout  int    `json:"timeout"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return fmt.Errorf("JSON 解析失败: %w", err)
	}
	if v.QR == "" {
		return fmt.Errorf("未返回二维码")
	}
	f.QRURL = v.QR
	f.LoginURL = v.LoginURL
	f.LPURL = v.LP
	f.Timeout = v.Timeout
	if f.Timeout == 0 {
		f.Timeout = 300
	}
	if !strings.HasPrefix(f.QRURL, "http") {
		f.QRURL = "https://" + xiaomiAccountHost + f.QRURL
	}
	return nil
}

// FetchQRImage 下载二维码 PNG。
func (f *QRFlow) FetchQRImage() ([]byte, error) {
	req, _ := http.NewRequest("GET", f.QRURL, nil)
	f.setUA(req)
	res, err := f.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	return io.ReadAll(res.Body)
}

func anyToStr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatInt(int64(x), 10)
	case json.Number:
		return x.String()
	case bool:
		return strconv.FormatBool(x)
	}
	return fmt.Sprint(v)
}

func (f *QRFlow) injectAccountCookies(uid, passToken string) {
	if uid == "" && passToken == "" {
		return
	}
	for _, domain := range []string{"https://" + xiaomiAccountHost, "https://" + stsHost, "https://" + OAuth2APIHost} {
		u, err := url.Parse(domain)
		if err != nil {
			continue
		}
		var cookies []*http.Cookie
		if uid != "" {
			cookies = append(cookies, &http.Cookie{
				Name:   "userId",
				Value:  uid,
				Domain: u.Hostname(),
				Path:   "/",
			})
		}
		if passToken != "" {
			cookies = append(cookies, &http.Cookie{
				Name:   "passToken",
				Value:  passToken,
				Domain: u.Hostname(),
				Path:   "/",
			})
		}
		if len(cookies) > 0 {
			f.jar.SetCookies(u, cookies)
		}
	}
}

// Wait 长轮询等扫码，成功后自动走 OAuth2 换 code。
func (f *QRFlow) Wait() {
	deadline := time.Now().Add(time.Duration(f.Timeout) * time.Second)
	lp := f.LPURL
	if lp == "" {
		lp = f.LoginURL
	}

	lpClient := &http.Client{
		Jar:     f.jar,
		Timeout: 150 * time.Second,
	}

	for time.Now().Before(deadline) {
		req, _ := http.NewRequest("GET", lp, nil)
		f.setUA(req)
		res, err := lpClient.Do(req)
		if err != nil {
			time.Sleep(1 * time.Second)
			continue
		}
		raw, _ := io.ReadAll(res.Body)
		res.Body.Close()
		body, derr := decodeLogin(raw)
		if derr != nil {
			time.Sleep(1 * time.Second)
			continue
		}

		var v struct {
			Code      int    `json:"code"`
			Location  string `json:"location"`
			UserID    any    `json:"userId"`
			PassToken string `json:"passToken"`
			Ssecurity any    `json:"ssecurity"`
			Psecurity string `json:"psecurity"`
		}
		_ = json.Unmarshal(body, &v)

		uid := anyToStr(v.UserID)
		if uid != "" {
			f.UserID = uid
		}
		if v.PassToken != "" {
			f.PassToken = v.PassToken
		}

		if v.Location != "" || v.PassToken != "" {
			if v.Location != "" {
				f.followLocation(v.Location)
			}
			f.injectAccountCookies(f.UserID, f.PassToken)
			f.collectCookies()

			// 关键：用登录态自动走 OAuth2 拿 code
			if err := f.fetchOAuthCode(); err != nil {
				f.mu.Lock()
				f.Err = err.Error()
				f.State = "failed"
				f.mu.Unlock()
			}
			return
		}
		time.Sleep(1 * time.Second)
	}
	f.mu.Lock()
	f.State = "failed"
	f.Err = "扫码超时"
	f.mu.Unlock()
}

// followLocation 跟随登录跳转，收集 cookie。
func (f *QRFlow) followLocation(loc string) {
	req, _ := http.NewRequest("GET", loc, nil)
	f.setUA(req)
	res, err := f.httpc.Do(req)
	if err != nil {
		return
	}
	defer res.Body.Close()
	io.Copy(io.Discard, res.Body)

	for _, c := range res.Cookies() {
		if c.Name == "userId" && f.UserID == "" {
			f.UserID = c.Value
		}
		if c.Name == "passToken" && f.PassToken == "" {
			f.PassToken = c.Value
		}
	}
	f.injectAccountCookies(f.UserID, f.PassToken)
}

func (f *QRFlow) collectCookies() {
	u, _ := url.Parse("https://" + xiaomiAccountHost + "/")
	for _, c := range f.jar.Cookies(u) {
		f.Cookies[c.Name] = c.Value
	}
	u2, _ := url.Parse("https://" + stsHost + "/")
	for _, c := range f.jar.Cookies(u2) {
		f.Cookies[c.Name] = c.Value
	}
}

// fetchOAuthCode 带登录 cookie 访问 OAuth2 authorize，捕获 302 里的 code。
// FetchOAuthCodeDirect 对已有 UserID 和 PassToken 的会话直接拉取 OAuth code。
func (f *QRFlow) FetchOAuthCodeDirect() error {
	return f.fetchOAuthCode()
}

func (f *QRFlow) fetchOAuthCode() error {
	f.injectAccountCookies(f.UserID, f.PassToken)

	redURI := "https://" + OAuth2APIHost + "/login_redirect"
	did := OAuth2Project + "." + f.DeviceUUID
	sum := sha1.Sum([]byte("d=" + did))
	state := hex.EncodeToString(sum[:])

	q := url.Values{
		"client_id":     {OAuth2ClientID},
		"response_type": {"code"},
		"redirect_uri":  {redURI},
		"device_id":     {did},
		"state":         {state},
		"scope":         {"1 6000"},
		"skip_confirm":  {"true"},
	}
	u := "https://" + xiaomiAccountHost + "/oauth2/authorize?" + q.Encode()

	var finalCode string
	cli := &http.Client{
		Jar:     f.jar,
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			f.setUA(req)
			if c := req.URL.Query().Get("code"); c != "" {
				finalCode = c
				return http.ErrUseLastResponse
			}
			if req.URL.Host == OAuth2APIHost && req.URL.Path == "/login_redirect" {
				return http.ErrUseLastResponse
			}
			if len(via) >= 15 {
				return fmt.Errorf("重定向次数过多")
			}
			return nil
		},
	}
	req, _ := http.NewRequest("GET", u, nil)
	f.setUA(req)
	res, err := cli.Do(req)
	if res != nil {
		defer res.Body.Close()
		loc := res.Header.Get("Location")
		if finalCode == "" && loc != "" {
			if pu, err := url.Parse(loc); err == nil {
				finalCode = pu.Query().Get("code")
			}
		}
	}
	if finalCode == "" && res != nil && res.Request != nil && res.Request.URL != nil {
		finalCode = res.Request.URL.Query().Get("code")
	}
	if finalCode == "" {
		if err != nil && !errors.Is(err, http.ErrUseLastResponse) {
			return fmt.Errorf("请求 authorize 失败: %w", err)
		}
		return fmt.Errorf("未能自动完成授权，需首次网页端确认。请点击【网页授权】完成首次确认")
	}

	f.mu.Lock()
	f.Code = finalCode
	f.State = "success"
	f.mu.Unlock()
	return nil
}

// Status 返回当前状态（供 HTTP 接口轮询）。
func (f *QRFlow) Status() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]any{
		"state": f.State,
		"err":   f.Err,
	}
	if f.Code != "" {
		out["hasCode"] = true
	}
	if len(f.Cookies) > 0 {
		out["cookies"] = len(f.Cookies)
	}
	return out
}
