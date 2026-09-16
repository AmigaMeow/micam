package xiaomi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
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

	// authMu 保护 serviceToken 的续期（多 goroutine 会并发调 API）
	authMu       sync.Mutex
	lastExchange time.Time
	exchangeErr  error

	// gen 每次成功续期 +1。
	//
	// 用途：多个 goroutine 同时遇到 401 时，
	// 请求前记下 gen，拿锁后发现 gen 变了，说明别人已经换好了，直接复用。
	// 比「按时间防抖」精确 —— 时间判断会误伤「刚登录就遇到坏 token」的场景。
	gen uint64
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
	// AccountID 哪个登录账号能访问这台设备（不来自小米 API，由我们填充）
	AccountID string `json:"accountId,omitempty"`
	// Lenses 镜头数量（1=单摄，2=双摄；不来自小米云 API，由我们判断）
	Lenses int `json:"lenses,omitempty"`
	// 以下字段不来自小米 API，由我们填充
	Alias        string `json:"alias,omitempty"`        // 中文别名
	AliasEn      string `json:"aliasEn,omitempty"`      // 英文名
	Favorite     bool   `json:"favorite,omitempty"`     // 收藏
	LastActivity string `json:"lastActivity,omitempty"` // 最后活动时间

	// SecondLensClaimed 型号声称有第二镜头，但协议层暂未证实能取到不同画面。
	// 客户端**不要**据此显示镜头切换按钮。
	SecondLensClaimed bool `json:"secondLensClaimed,omitempty"`
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
	if err := s.exchangeServiceToken(); err != nil {
		return err
	}
	s.authMu.Lock()
	s.lastExchange = time.Now()
	s.gen++
	s.authMu.Unlock()
	return nil
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

// logf_Session 包内日志（避免引入主包依赖）。
func logf_Session(format string, a ...any) {
	log.Printf(format, a...)
}

// Request 调用小米云 API（自动签名 + 解密）。
//
// ===== 凭据分两层，别混淆 =====
//
//	第一层：passToken（扫码登录时拿到）
//	  长期有效（实测 >= 24 小时，实际可达数月）
//	  只在换设备/换密码/主动登出时失效
//	  -> 用户扫一次码就够了，不需要反复扫
//
//	第二层：serviceToken + ssecurity（由 passToken 换取）
//	  短期有效（小米通常几小时到几天）
//	  过期后接口返回未登录错误
//	  -> 必须自动续期，否则监控就会中断
//
// 这里实现自动续期：遇到未登录错误时，用 passToken 重新换一次，
// 然后重试原请求。用户完全无感。
func (s *Session) Request(path, data string) (json.RawMessage, error) {
	return s.request(path, data, true)
}

// request 内部实现，allowReauth 控制是否允许续期重试。
func (s *Session) request(path, data string, allowReauth bool) (json.RawMessage, error) {
	// 记下当前代际：如果续期后 gen 变了，说明是别的 goroutine 换的，我们直接用
	genAt := s.Gen()

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

	// HTTP 401/403 也是「会话过期」的一种表达。
	//
	// 实测：把 serviceToken 改成非法值时，小米返回 HTTP 401
	// （不是 JSON 里的 code 字段）。所以这里也要触发续期。
	if res.StatusCode == 401 || res.StatusCode == 403 {
		if allowReauth {
			logf_Session("收到 HTTP %d，判定会话过期，强制续期", res.StatusCode)
			if err := s.reauth(true, genAt); err != nil {
				return nil, fmt.Errorf("xiaomi: 会话过期且续期失败: %w", err)
			}
			return s.request(path, data, false)
		}
		return nil, fmt.Errorf("xiaomi: http %d（续期后仍失败）", res.StatusCode)
	}

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
		// 未登录/会话过期 -> 用 passToken 重新换 serviceToken 后重试一次
		if allowReauth && isSessionExpired(out.Code, out.Message) {
			logf_Session("检测到会话过期（code=%d msg=%s），强制续期", out.Code, out.Message)
			if err := s.reauth(true, genAt); err != nil {
				return nil, fmt.Errorf("xiaomi: 会话过期且续期失败: %w", err)
			}
			return s.request(path, data, false)
		}
		return nil, fmt.Errorf("xiaomi: code=%d msg=%s", out.Code, out.Message)
	}
	return out.Result, nil
}

// isSessionExpired 判断是否为「需要重新登录」的错误。
//
// 小米云的各种未登录表达：
//
//	code 401 / -1 / 1 + message 含 "login" / "auth" / "token"
func isSessionExpired(code int, msg string) bool {
	if code == 401 {
		return true
	}
	low := strings.ToLower(msg)
	for _, kw := range []string{"login", "auth", "token", "expired", "session"} {
		if strings.Contains(low, kw) {
			return true
		}
	}
	return false
}

// Reauth 主动续期（带防抖，用于定时守护）。
//
// 30 秒内刚换过就跳过，避免周期性任务重复调用。
func (s *Session) Reauth() error {
	return s.reauth(false, 0)
}

// ReauthForce 强制续期（用于「请求已经失败」的场景）。
//
// 与 Reauth 的区别：
//   - Reauth 是「预防性」的，可以防抖跳过
//   - ReauthForce 是「补救性」的，说明当前 token 确定坏了，必须真换
//
// 实测踩坑：一开始只有 Reauth，防抖窗口内直接返回 nil 不换，
// 结果 401 之后「续期成功但 token 没变」，重试还是 401。
//
// 仍然串行化 + 2 秒窗口：避免大量并发 401 同时去打小米云。
func (s *Session) ReauthForce() error {
	return s.reauth(true, 0)
}

func (s *Session) reauth(force bool, genAt uint64) error {
	s.authMu.Lock()
	defer s.authMu.Unlock()

	// 并发保护：拿锁后发现 gen 变了，说明别的 goroutine 已经换好了
	if force && genAt != 0 && s.gen != genAt {
		return nil
	}

	// 预防性续期的防抖（30 秒内不重复）
	if !force && !s.lastExchange.IsZero() && time.Since(s.lastExchange) < 30*time.Second && s.exchangeErr == nil {
		return nil
	}

	// 续期刚失败过（30 秒内）就不重复打，避免把限流撞得更狠
	if s.exchangeErr != nil && !s.lastExchange.IsZero() && time.Since(s.lastExchange) < 30*time.Second {
		return s.exchangeErr
	}

	if err := s.exchangeServiceToken(); err != nil {
		s.exchangeErr = err
		s.lastExchange = time.Now()
		return err
	}
	s.gen++
	s.lastExchange = time.Now()
	s.exchangeErr = nil
	return nil
}

// Gen 当前凭据代际（续期成功会变）。
func (s *Session) Gen() uint64 {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	return s.gen
}

// LastExchange 上次续期时间。
func (s *Session) LastExchange() time.Time {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	return s.lastExchange
}

// SetServiceToken 直接设置 serviceToken（从磁盘恢复时用，跳过续期）。
func (s *Session) SetServiceToken(tok string, sec []byte) {
	s.ServiceToken = tok
	s.Ssecurity = sec
	s.lastExchange = time.Now()
}

// TokenValid 粗略判断 serviceToken 是否还在有效期内。
func (s *Session) TokenValid() bool {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	if s.ServiceToken == "" || s.lastExchange.IsZero() {
		return false
	}
	// 保守估计 12 小时
	return time.Since(s.lastExchange) < 12*time.Hour
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
