package miot

// OAuth2 扫码授权完整流程（2026-10-11）。
//
// ===== 为什么需要两步 =====
//
// 官方 OAuth2 authorize 端点要求"已登录"才直接返回 code，
// 否则跳登录页。而米家的扫码接口只认 sid=xiaomiio。
//
// 所以串联：
//   ① 用 sid=xiaomiio 扫码 -> 拿到登录 cookies（userId/serviceToken/...）
//   ② 带这些 cookies 访问 /oauth2/authorize -> 302 直接带 code
//   ③ code 换 access_token
//
// 好处：用户只扫一次码（复用插件已有的扫码体验），
//       不需要额外打开浏览器、不需要手动复制 base64。

import (
	"encoding/json"
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
	mu       sync.Mutex
	httpc    *http.Client
	jar      *cookiejar.Jar
	QRURL    string // 二维码图片地址
	LoginURL string
	LPURL    string // 长轮询地址
	Timeout  int

	State   string // waiting / scanned / success / failed
	Err     string
	Code    string // OAuth2 授权码
	Cookies map[string]string
	Created time.Time
}

// NewQRFlow 创建一个带 cookie jar 的会话。
func NewQRFlow() (*QRFlow, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &QRFlow{
		httpc:   &http.Client{Jar: jar, Timeout: 30 * time.Second},
		jar:     jar,
		State:   "waiting",
		Cookies: map[string]string{},
		Created: time.Now(),
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

// Wait 长轮询等扫码，成功后自动走 OAuth2 换 code。
//
// 这是一个阻塞调用，应在 goroutine 里跑。
func (f *QRFlow) Wait() {
	deadline := time.Now().Add(time.Duration(f.Timeout) * time.Second)
	lp := f.LPURL
	if lp == "" {
		lp = f.LoginURL
	}

	for time.Now().Before(deadline) {
		req, _ := http.NewRequest("GET", lp, nil)
		f.setUA(req)
		res, err := f.httpc.Do(req)
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		raw, _ := io.ReadAll(res.Body)
		res.Body.Close()
		body, derr := decodeLogin(raw)
		if derr != nil {
			time.Sleep(2 * time.Second)
			continue
		}

		var v struct {
			Code     int    `json:"code"`
			Location string `json:"location"`
			UserID   string `json:"userId"`
		}
		_ = json.Unmarshal(body, &v)

		if v.Location != "" {
			// 扫码成功，跟随 location 收集 cookies
			f.followLocation(v.Location)
			f.collectCookies()
			f.mu.Lock()
			f.State = "success"
			f.mu.Unlock()

			// 关键：用登录态自动走 OAuth2 拿 code
			if err := f.fetchOAuthCode(); err != nil {
				f.mu.Lock()
				f.Err = "获取 OAuth 授权码失败: " + err.Error()
				f.State = "failed"
				f.mu.Unlock()
			}
			return
		}
		time.Sleep(2 * time.Second)
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
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
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
//
// 原理：authorize 在"已登录"状态下直接 302 到 redirect_uri?code=xxx
func (f *QRFlow) fetchOAuthCode() error {
	q := url.Values{
		"client_id":     {OAuth2ClientID},
		"response_type": {"code"},
		"redirect_uri":  {"https://127.0.0.1"},
		"scope":         {"1 6000"},
		"state":         {"micam"},
		"skip_confirm":  {"true"}, // 已授权过则不再弹确认页
	}
	u := "https://" + xiaomiAccountHost + "/oauth2/authorize?" + q.Encode()

	// 不自动跟随重定向，自己看 Location
	cli := &http.Client{
		Jar:     f.jar,
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, _ := http.NewRequest("GET", u, nil)
	f.setUA(req)
	res, err := cli.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	loc := res.Header.Get("Location")
	if loc == "" {
		// 没跳转：可能仍需用户确认，或返回了页面
		raw, _ := io.ReadAll(res.Body)
		return fmt.Errorf("authorize 未返回重定向（HTTP %d，%d 字节）",
			res.StatusCode, len(raw))
	}
	pu, err := url.Parse(loc)
	if err != nil {
		return err
	}
	code := pu.Query().Get("code")
	if code == "" {
		return fmt.Errorf("重定向里没有 code: %s", truncate(loc, 120))
	}
	f.mu.Lock()
	f.Code = code
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
