package xiaomi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// CaptchaError 表示小米要求图形验证码。
type CaptchaError struct {
	URL  string
	Data []byte // 图片二进制
}

func (e *CaptchaError) Error() string { return "xiaomi: 需要图形验证码" }

// VerifyError 表示小米要求短信/邮箱验证码。
type VerifyError struct {
	NotificationURL string
	Flag            int
	IdentitySession string
	MaskedPhone     string
	MaskedEmail     string
	session         *Session
}

func (e *VerifyError) Error() string {
	if e.MaskedPhone != "" {
		return "xiaomi: 需要手机验证码 " + e.MaskedPhone
	}
	if e.MaskedEmail != "" {
		return "xiaomi: 需要邮箱验证码 " + e.MaskedEmail
	}
	return "xiaomi: 需要二次验证"
}

// verifyName 根据 flag 返回 Phone / Email。
// flag=4 手机，flag=8 邮箱。其它值说明账号需要先「升级/绑定安全信息」。
func verifyName(flag int) string {
	switch flag {
	case 4:
		return "Phone"
	case 8:
		return "Email"
	}
	return ""
}

// StartVerify 进入二次验证：拉取 flag，发送验证码。
func (e *VerifyError) StartVerify() error {
	s := e.session
	listURL := strings.Replace(e.NotificationURL, "/fe/service/identity/authStart", "/identity/list", 1)

	req, _ := http.NewRequest("GET", listURL, nil)
	req.Header.Set("User-Agent", s.userAgent)
	req.Header.Set("Cookie", "deviceId="+s.deviceID)
	res, err := s.client.Do(req)
	if err != nil {
		return err
	}
	var v struct {
		Code     int    `json:"code"`
		Flag     int    `json:"flag"`
		Location string `json:"location"`
		Retrieve string `json:"retrieveType"`
	}
	body, err := decodeLoginRaw(res.Body, &v)
	if err != nil {
		return err
	}

	// 收集 identity_session
	var isess string
	for _, c := range res.Cookies() {
		if c.Name == "identity_session" {
			isess = c.Value
		}
	}
	e.Flag = v.Flag
	e.IdentitySession = isess

	if name := verifyName(v.Flag); name == "" {
		return fmt.Errorf(
			"xiaomi: 该账号需要先完成「账号升级/绑定安全信息」才能二次验证 "+
				"(flag=%d retrieveType=%s location=%s) body=%s",
			v.Flag, v.Retrieve, v.Location, trim(body, 160))
	}

	name := verifyName(e.Flag)
	cookie := "identity_session=" + isess

	// 1) 查询掩码手机/邮箱
	req, _ = http.NewRequest("GET",
		"https://"+DefaultAccount+"/identity/auth/verify"+name+"?_flag="+strconv.Itoa(e.Flag)+"&_json=true", nil)
	req.Header.Set("User-Agent", s.userAgent)
	req.Header.Set("Cookie", cookie)
	res, err = s.client.Do(req)
	if err != nil {
		return err
	}
	var v1 struct {
		Code        int    `json:"code"`
		MaskedPhone string `json:"maskedPhone"`
		MaskedEmail string `json:"maskedEmail"`
	}
	decodeLoginRaw(res.Body, &v1)
	e.MaskedPhone = v1.MaskedPhone
	e.MaskedEmail = v1.MaskedEmail

	// 2) 发送验证码
	form := url.Values{"_json": {"true"}, "icode": {""}, "retry": {"0"}}
	req, _ = http.NewRequest("POST",
		"https://"+DefaultAccount+"/identity/auth/send"+name+"Ticket",
		strings.NewReader(form.Encode()))
	req.Header.Set("User-Agent", s.userAgent)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", cookie)
	res, err = s.client.Do(req)
	if err != nil {
		return err
	}
	var v2 struct {
		Code       int    `json:"code"`
		CaptchaURL string `json:"captchaURL"`
		Desc       string `json:"description"`
	}
	body, err = decodeLoginRaw(res.Body, &v2)
	if err != nil {
		return err
	}
	if v2.CaptchaURL != "" {
		return &CaptchaError{URL: v2.CaptchaURL}
	}
	if v2.Code != 0 {
		return fmt.Errorf("xiaomi: 发送验证码失败 code=%d desc=%s", v2.Code, v2.Desc)
	}
	return nil
}

// Submit 提交短信/邮箱验证码。
// 关键：参数必须是 _flag=<n>，不是 _flag<n>（go2rtc 这里写错了，导致永远失败）。
func (e *VerifyError) Submit(code string) error {
	s := e.session
	name := verifyName(e.Flag)
	if name == "" {
		return fmt.Errorf("xiaomi: 未知验证类型 flag=%d", e.Flag)
	}
	u := "https://" + DefaultAccount + "/identity/auth/verify" + name +
		"?_flag=" + strconv.Itoa(e.Flag) +
		"&ticket=" + url.QueryEscape(code) +
		"&trust=false&_json=true"

	req, _ := http.NewRequest("POST", u, nil)
	req.Header.Set("User-Agent", s.userAgent)
	req.Header.Set("Cookie", "identity_session="+e.IdentitySession)
	res, err := s.client.Do(req)
	if err != nil {
		return err
	}
	var v struct {
		Code     int    `json:"code"`
		Location string `json:"location"`
		Desc     string `json:"description"`
	}
	body, err := decodeLoginRaw(res.Body, &v)
	if err != nil {
		return err
	}
	if v.Code != 0 || v.Location == "" {
		return fmt.Errorf("xiaomi: 验证码校验失败 code=%d desc=%s body=%s", v.Code, v.Desc, trim(body, 160))
	}
	return s.finishAuth(v.Location)
}

// SolveCaptcha 提交图形验证码。
func (e *CaptchaError) SolveCaptcha(s *Session, code string) error {
	u := "https://" + DefaultAccount + e.URL
	if !strings.HasPrefix(e.URL, "http") {
		u = "https://" + DefaultAccount + e.URL
	}
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", s.userAgent)
	res, err := s.client.Do(req)
	if err != nil {
		return err
	}
	img, _ := io.ReadAll(res.Body)
	e.Data = img
	_ = code
	return nil
}

// ---- 扫码登录 ----

// QRLogin 保存扫码登录的中间状态。
type QRLogin struct {
	QRURL    string `json:"qr"`
	LoginURL string `json:"loginUrl"`
	LongPoll string `json:"lp"`
	Timeout  int    `json:"timeout"`
	QRImage  []byte `json:"-"`
	session  *Session
}

// StartQRLogin 申请二维码。
func (s *Session) StartQRLogin() (*QRLogin, error) {
	q := url.Values{
		"_qrsize":      {"480"},
		"qs":           {"?sid=" + SidXiaomiIO + "&_json=true"},
		"callback":     {"https://" + DefaultSTS + "/sts"},
		"_hasLogo":     {"false"},
		"sid":          {SidXiaomiIO},
		"serviceParam": {""},
		"_locale":      {"zh_CN"},
		"_dc":          {strconv.FormatInt(nowMillis(), 10)},
	}
	req, _ := http.NewRequest("GET",
		"https://"+DefaultAccount+"/longPolling/loginUrl?"+q.Encode(), nil)
	req.Header.Set("User-Agent", s.userAgent)
	res, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	var v QRLogin
	if _, err = decodeLoginRaw(res.Body, &v); err != nil {
		return nil, err
	}
	v.session = s
	if !strings.HasPrefix(v.QRURL, "http") {
		v.QRURL = "https://" + DefaultAccount + v.QRURL
	}
	return &v, nil
}

// FetchQRImage 下载二维码图片。
func (q *QRLogin) FetchQRImage() ([]byte, error) {
	res, err := q.session.client.Get(q.QRURL)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	img, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	q.QRImage = img
	return img, nil
}

// Wait 长轮询等待用户扫码，成功后完成登录。
func (q *QRLogin) Wait() error {
	deadline := nowMillis() + int64(q.Timeout)*1000
	// ⚠️ 诊断日志（2026-09-18 用户反馈「手机提示登录成功但页面卡住」）
	//
	// 原来这里有四个静默 continue，出问题时完全看不到原因。
	// 现在每个分支都记一次（带序号，避免刷屏）。
	round := 0
	logf := func(format string, a ...any) {
		log.Printf("[qr] "+format, a...)
	}
	logf("Wait 开始，超时 %d 秒，longPoll=%s", q.Timeout, q.LongPoll)

	// ⚠️ long-poll 必须用自己的长超时 client！
	//
	// 根因（2026-09-18 用户反馈「手机提示登录成功但页面卡住」）：
	//
	//   session.client 的超时是 30 秒（client.go:127），
	//   但小米的 long-poll 接口是【挂起等待】的 ——
	//   用户没扫码时它会一直挂着，直到有结果或服务端超时。
	//
	//   于是 30 秒一到，客户端主动放弃 ->
	//   用户这时扫码确认 -> 但我们的请求早断了 -> 收不到 location ->
	//   重试同一个 URL（可能已被消费）-> 又超时 -> 页面永远卡在二维码。
	//
	//   日志证据（诊断版本实测）：
	//     #1 请求失败: context deadline exceeded (Client.Timeout ...)
	//     #2 请求失败: context deadline exceeded
	//     #3 请求失败: context deadline exceeded
	//
	// 现在给 long-poll 单独一个 150 秒超时的 client（复用同一个 Transport，
	// 保证代理/TLS 配置一致）。轮询总时长仍由 q.Timeout（300 秒）兜底。
	lpClient := &http.Client{
		Timeout:   150 * time.Second,
		Transport: q.session.client.Transport,
	}
	for nowMillis() < deadline {
		round++
		req, _ := http.NewRequest("GET", q.LongPoll, nil)
		req.Header.Set("User-Agent", q.session.userAgent)
		res, err := lpClient.Do(req)
		if err != nil {
			if round <= 3 || round%10 == 0 {
				logf("#%d 请求失败: %v", round, err)
			}
			time.Sleep(500 * time.Millisecond)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		res.Body.Close()
		if res.StatusCode != 200 {
			logf("#%d 状态码 %d, body=%s", round, res.StatusCode, trunc(string(body), 160))

			// 403/404 通常表示这个 long-poll URL 已经失效
			// （被消费过，或超过服务端给的时限）。
			// 继续重试同一个死 URL 只会空转，所以直接失败，
			// 让前端提示用户「重新获取二维码」。
			if res.StatusCode == 403 || res.StatusCode == 404 || res.StatusCode == 410 {
				return fmt.Errorf("xiaomi: 二维码已失效（HTTP %d），请重新获取", res.StatusCode)
			}
			time.Sleep(500 * time.Millisecond)
			continue
		}
		// 注意字段类型要宽容：
		//
		// ⚠️ 实测（2026-09-18）小米返回的 userId 是【数字】，
		//    声明成 string 会报：
		//      json: cannot unmarshal number into Go struct field .userId of type string
		//    所以这里用 any 接收，再自己转字符串。
		var v struct {
			UserID    any    `json:"userId"`
			Ssecurity any    `json:"ssecurity"`
			CUserID   any    `json:"cUserId"`
			PassToken string `json:"passToken"`
			Location  string `json:"location"`
			Psecurity string `json:"psecurity"`
		}
		if _, e2 := decodeLoginRaw(bytes.NewReader(body), &v); e2 != nil {
			if round <= 5 {
				logf("#%d 解析失败: %v | len=%d | 头=%s | 尾=%s",
					round, e2, len(body),
					trunc(string(body), 60), tailStr(string(body), 60))
			}
			time.Sleep(500 * time.Millisecond)
			continue
		}

		// 两种成功形态都要支持：
		//
		//   A) 返回 location -> 需要再走 finishAuth 换 token
		//   B) 直接返回 passToken + ssecurity -> 直接用，不需要 location
		//
		// ⚠️ 实测（2026-09-18）long-poll 返回的是形态 B：
		//    {"psecurity":...,"nonce":...,"ssecurity":...,"passToken":"V1:...",
		//     "userId":123456,"cUserId":"...","captchaUrl":null}
		//    **没有 location 字段**。
		//
		//    而原代码只认形态 A（location != "" 才继续），
		//    所以即使解析成功也会一直等下去 —— 页面永远卡住。
		s := q.session
		if uid := anyToStr(v.UserID); uid != "" {
			s.UserID = uid
		}
		if cid := anyToStr(v.CUserID); cid != "" {
			s.CUserID = cid
		}
		if v.PassToken != "" {
			s.PassToken = v.PassToken
		}
		if sec := anyToBytes(v.Ssecurity); len(sec) > 0 {
			s.Ssecurity = sec
		}

		if v.Location != "" {
			logf("#%d ✅ 形态A：拿到 location，userId=%s", round, s.UserID)
			return s.finishAuth(v.Location)
		}
		if v.PassToken != "" {
			logf("#%d ✅ 形态B：直接拿到 passToken，userId=%s", round, s.UserID)
			return s.finishQRDirect()
		}
		if round <= 3 || round%10 == 0 {
			logf("#%d 还没确认（无 location 也无 passToken）, body=%s", round, trunc(string(body), 160))
		}
		time.Sleep(500 * time.Millisecond)
		continue
	}
	return fmt.Errorf("xiaomi: 二维码超时")
}

// Token 导出可持久化的凭据。
func (s *Session) Token() map[string]string {
	return map[string]string{
		"userId":    s.UserID,
		"passToken": s.PassToken,
	}
}

// MarshalToken 序列化凭据。
func (s *Session) MarshalToken() ([]byte, error) {
	return json.Marshal(s.Token())
}

// trunc 截断字符串用于日志。
func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// tailStr 取字符串末尾 n 个字符（日志里看包装标记用）。
func tailStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// anyToStr 把 JSON 里可能是数字也可能是字符串的值转成字符串。
//
// 为什么需要：小米的 userId 有时是数字（1234567），有时是字符串（"1234567"）。
// 声明成 string 会在数字时报 unmarshal 错误；声明成 any 则要自己转。
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
	return ""
}

// anyToBytes 兼容 ssecurity 可能是 base64 字符串或 []byte 的情况。
func anyToBytes(v any) []byte {
	switch x := v.(type) {
	case nil:
		return nil
	case []byte:
		return x
	case string:
		if b, err := base64.StdEncoding.DecodeString(x); err == nil {
			return b
		}
		return []byte(x)
	}
	return nil
}
