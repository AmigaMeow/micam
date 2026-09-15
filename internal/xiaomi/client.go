package xiaomi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	SidXiaomiIO   = "xiaomiio"
	DefaultBase   = "api.io.mi.com"
	DefaultAccount = "account.xiaomi.com"
	DefaultSTS     = "sts.api.io.mi.com"
)

// Session 保存一个小米账号的登录态。
type Session struct {
	UserID       string
	PassToken    string
	ServiceToken string
	Ssecurity    []byte
	CUserID      string

	client    *http.Client
	deviceID  string
	userAgent string
}

// Device 小米设备。
type Device struct {
	Did       string `json:"did"`
	UID       any    `json:"uid"`
	Name      string `json:"name"`
	Model     string `json:"model"`
	MAC       string `json:"mac"`
	LocalIP   string `json:"localip"`
	IsOnline  bool   `json:"isOnline"`
	ShareFlag int    `json:"shareFlag"`
	Shared    bool   `json:"shared"`     // 来自共享家庭
	Permit    int    `json:"permitLevel"`
	ParentID  string `json:"parent_id"`
	Token     string `json:"token"`
	Extra     map[string]any `json:"extra"`
}

// DeviceType 返回设备类型（model 的中间段）。
//
// 例：
//   chuangmi.camera.029a02  -> camera
//   xiaomi.airc.h55h00      -> airc
//   viomi.hood.c1           -> hood
//   bean.switch.blns1       -> switch
//   xiaomi.wifispeaker.l05b -> wifispeaker
//   linp.sensor_ht.ks2bb    -> sensor_ht
//   blt.3.qdt0mj0c4400      -> 3
func (d *Device) DeviceType() string {
	parts := strings.Split(strings.ToLower(d.Model), ".")
	if len(parts) >= 2 {
		return parts[1]
	}
	return "other"
}

// IsCamera 判断是否摄像头类设备。
func (d *Device) IsCamera() bool {
	m := d.Model
	return strings.Contains(m, ".camera.") ||
		strings.Contains(m, ".cateye.") ||
		strings.Contains(m, ".feeder.")
}

// Home 小米家庭（含共享家庭）。
type Home struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	UID      any    `json:"uid"`
	ShareFlag int   `json:"shareflag"`
	Permit   int    `json:"permit_level"`
	Dids     []string `json:"dids"`
	RoomList []struct {
		ID   string   `json:"id"`
		Name string   `json:"name"`
		Dids []string `json:"dids"`
	} `json:"roomlist"`
}

// OwnerID 返回家庭所有者的 uid 字符串形式。
func (h *Home) OwnerID() string {
	return fmt.Sprint(h.UID)
}

// NewSession 创建一个新会话。
func NewSession() *Session {
	return &Session{
		client:    &http.Client{Timeout: 30 * time.Second},
		deviceID:  "wb_" + randHex(16),
		userAgent: "Mozilla/5.0 (Linux; Android 10; MI 9) AppleWebKit/537.36 Chrome/120 Mobile Safari/537.36",
	}
}

// SetTokens 用已有的 passToken 直接恢复会话（跳过登录）。
func (s *Session) SetTokens(userID, passToken string) error {
	s.UserID = userID
	s.PassToken = passToken
	return s.exchangeServiceToken()
}

// Login 账号密码登录。
// 注意：小米通常会要求二次验证，届时返回 *VerifyError，需配合 SendTicket/Verify 使用。
func (s *Session) Login(username, password string) error {
	// 1) 拿签名参数
	q := url.Values{"_json": {"true"}, "sid": {SidXiaomiIO}}
	req, _ := http.NewRequest("GET", "https://"+DefaultAccount+"/pass/serviceLogin?"+q.Encode(), nil)
	req.Header.Set("User-Agent", s.userAgent)
	req.Header.Set("Cookie", "deviceId="+s.deviceID)
	res, err := s.client.Do(req)
	if err != nil {
		return err
	}
	var v1 struct {
		Qs       string `json:"qs"`
		Sign     string `json:"_sign"`
		Sid      string `json:"sid"`
		Callback string `json:"callback"`
	}
	if err = decodeLogin(res.Body, &v1); err != nil {
		return err
	}

	// 2) 提交账密
	form := url.Values{
		"_json":    {"true"},
		"hash":     {MD5Upper(password)},
		"sid":      {v1.Sid},
		"callback": {v1.Callback},
		"_sign":    {v1.Sign},
		"qs":       {v1.Qs},
		"user":     {username},
	}
	req, _ = http.NewRequest("POST", "https://"+DefaultAccount+"/pass/serviceLoginAuth2",
		strings.NewReader(form.Encode()))
	req.Header.Set("User-Agent", s.userAgent)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", "deviceId="+s.deviceID)
	res, err = s.client.Do(req)
	if err != nil {
		return err
	}
	var v2 struct {
		Ssecurity       []byte `json:"ssecurity"`
		PassToken       string `json:"passToken"`
		Location        string `json:"location"`
		CaptchaURL      string `json:"captchaUrl"`
		NotificationURL string `json:"notificationUrl"`
		Code            int    `json:"code"`
		Desc            string `json:"description"`
	}
	body, err := decodeLoginRaw(res.Body, &v2)
	if err != nil {
		return err
	}

	if v2.CaptchaURL != "" {
		return &CaptchaError{URL: v2.CaptchaURL}
	}
	if v2.NotificationURL != "" {
		return &VerifyError{NotificationURL: v2.NotificationURL, session: s}
	}
	if v2.Location == "" {
		return fmt.Errorf("xiaomi: login failed code=%d desc=%s body=%s", v2.Code, v2.Desc, trim(body, 200))
	}

	s.Ssecurity = v2.Ssecurity
	s.PassToken = v2.PassToken
	return s.finishAuth(v2.Location)
}

// exchangeServiceToken 用 passToken 换 serviceToken。
func (s *Session) exchangeServiceToken() error {
	req, _ := http.NewRequest("GET",
		"https://"+DefaultAccount+"/pass/serviceLogin?_json=true&sid="+SidXiaomiIO, nil)
	req.Header.Set("User-Agent", s.userAgent)
	req.Header.Set("Cookie", fmt.Sprintf("userId=%s; passToken=%s; deviceId=%s",
		s.UserID, s.PassToken, s.deviceID))
	res, err := s.client.Do(req)
	if err != nil {
		return err
	}
	var v struct {
		Ssecurity []byte `json:"ssecurity"`
		Location  string `json:"location"`
		Code      int    `json:"code"`
	}
	if _, err = decodeLoginRaw(res.Body, &v); err != nil {
		return err
	}
	if v.Ssecurity == nil {
		return errors.New("xiaomi: passToken 无效或已过期")
	}
	s.Ssecurity = v.Ssecurity
	if v.Location == "" {
		return errors.New("xiaomi: 未返回 location")
	}
	return s.finishAuth(v.Location)
}

// finishAuth 跟随 location 收集 cookie。
func (s *Session) finishAuth(location string) error {
	req, _ := http.NewRequest("GET", location, nil)
	req.Header.Set("User-Agent", s.userAgent)
	req.Header.Set("Cookie", "deviceId="+s.deviceID)
	res, err := s.client.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()

	for _, c := range res.Cookies() {
		switch c.Name {
		case "userId":
			s.UserID = c.Value
		case "cUserId":
			s.CUserID = c.Value
		case "serviceToken":
			s.ServiceToken = c.Value
		case "passToken":
			s.PassToken = c.Value
		}
	}
	if s.ServiceToken == "" {
		names := make([]string, 0, len(res.Cookies()))
		for _, c := range res.Cookies() {
			names = append(names, c.Name)
		}
		return fmt.Errorf("xiaomi: 未取到 serviceToken (url=%s cookies=%v)", location, names)
	}
	return nil
}

// Request 调用小米云 API（自动签名 + 解密）。
func (s *Session) Request(path, data string) (json.RawMessage, error) {
	form, sn, err := SignRequest(s.Ssecurity, path, data)
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequest("POST", "https://"+DefaultBase+"/app"+path,
		strings.NewReader(form.Encode()))
	req.Header.Set("User-Agent", s.userAgent)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", fmt.Sprintf("userId=%s; serviceToken=%s; deviceId=%s",
		s.UserID, s.ServiceToken, s.deviceID))

	res, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("xiaomi: http %d", res.StatusCode)
	}
	plain, err := DecryptResponse(sn, raw)
	if err != nil {
		return nil, err
	}
	var out struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(plain, &out); err != nil {
		return nil, err
	}
	if out.Code != 0 {
		return nil, fmt.Errorf("xiaomi: code=%d msg=%s", out.Code, out.Message)
	}
	return out.Result, nil
}

// ---- 设备与家庭 ----

// Homes 拉取自有家庭与共享家庭。
func (s *Session) Homes() ([]Home, error) {
	raw, err := s.Request("/v2/homeroom/gethome",
		`{"fg":true,"fetch_share":true,"fetch_share_dev":true,"limit":300,"app_ver":7}`)
	if err != nil {
		return nil, err
	}
	var v struct {
		HomeList      []Home `json:"homelist"`
		ShareHomeList []Home `json:"share_home_list"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return append(v.HomeList, v.ShareHomeList...), nil
}

// Devices 拉取自有设备。
func (s *Session) Devices() ([]Device, error) {
	raw, err := s.Request("/v2/home/device_list", "{}")
	if err != nil {
		return nil, err
	}
	var v struct {
		List []Device `json:"list"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return v.List, nil
}

// SharedDevices 拉取共享家庭的设备。
// 关键：必须用 home_device_list + home_owner，用 device_list_page 拿不到共享设备。
func (s *Session) SharedDevices(homeID, ownerID string) ([]Device, error) {
	data, _ := json.Marshal(map[string]any{
		"home_owner":         ownerID,
		"home_id":            homeID,
		"limit":              200,
		"get_split_device":   true,
		"support_smart_home": true,
	})
	raw, err := s.Request("/v2/home/home_device_list", string(data))
	if err != nil {
		return nil, err
	}
	var v struct {
		DeviceInfo []Device `json:"device_info"`
		List       []Device `json:"list"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	if len(v.DeviceInfo) > 0 {
		return v.DeviceInfo, nil
	}
	return v.List, nil
}

// AllCameras 汇总：自有设备 + 所有共享家庭设备，只返回摄像头。
func (s *Session) AllCameras() ([]Device, error) {
	var out []Device
	seen := map[string]bool{}

	own, err := s.Devices()
	if err != nil {
		return nil, err
	}
	for _, d := range own {
		if d.IsCamera() && !seen[d.Did] {
			seen[d.Did] = true
			out = append(out, d)
		}
	}

	homes, err := s.Homes()
	if err == nil {
		for _, h := range homes {
			if h.ShareFlag == 0 {
				continue
			}
			devs, err := s.SharedDevices(h.ID, h.OwnerID())
			if err != nil {
				continue
			}
			for _, d := range devs {
				if d.IsCamera() && !seen[d.Did] {
					seen[d.Did] = true
					// 标记为共享设备（来自共享家庭）
					d.Shared = true
					out = append(out, d)
				}
			}
		}
	}
	return out, nil
}

// ---- 辅助 ----

func decodeLogin(r io.Reader, v any) error {
	_, err := decodeLoginRaw(r, v)
	return err
}

// decodeLoginRaw 去掉小米的 `&&&START&&&` 前缀再解析。
func decodeLoginRaw(r io.Reader, v any) ([]byte, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	body := strings.TrimPrefix(string(raw), "&&&START&&&")
	err = json.Unmarshal([]byte(body), v)
	return []byte(body), err
}

func trim(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n])
	}
	return string(b)
}
