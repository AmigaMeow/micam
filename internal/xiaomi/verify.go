package xiaomi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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
	QRURL      string `json:"qr"`
	LoginURL   string `json:"loginUrl"`
	LongPoll   string `json:"lp"`
	Timeout    int    `json:"timeout"`
	QRImage    []byte `json:"-"`
	session    *Session
}

// StartQRLogin 申请二维码。
func (s *Session) StartQRLogin() (*QRLogin, error) {
	q := url.Values{
		"_qrsize":     {"480"},
		"qs":          {"?sid=" + SidXiaomiIO + "&_json=true"},
		"callback":    {"https://" + DefaultSTS + "/sts"},
		"_hasLogo":    {"false"},
		"sid":         {SidXiaomiIO},
		"serviceParam": {""},
		"_locale":     {"zh_CN"},
		"_dc":         {strconv.FormatInt(nowMillis(), 10)},
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
	for nowMillis() < deadline {
		req, _ := http.NewRequest("GET", q.LongPoll, nil)
		req.Header.Set("User-Agent", q.session.userAgent)
		res, err := q.session.client.Do(req)
		if err != nil {
			continue
		}
		if res.StatusCode != 200 {
			res.Body.Close()
			continue
		}
		var v struct {
			UserID    string `json:"userId"`
			Ssecurity []byte `json:"ssecurity"`
			CUserID   string `json:"cUserId"`
			PassToken string `json:"passToken"`
			Location  string `json:"location"`
		}
		_, err = decodeLoginRaw(res.Body, &v)
		if err != nil {
			continue
		}
		if v.Location == "" {
			continue
		}
		s := q.session
		s.UserID = v.UserID
		s.CUserID = v.CUserID
		s.PassToken = v.PassToken
		s.Ssecurity = v.Ssecurity
		return s.finishAuth(v.Location)
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
