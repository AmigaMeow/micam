package notify

// 事件通知：全渠道推送与事件总线。
//
// 监控产品的核心需求：出事了要能第一时间知道。
//
// 支持的推送通道：
//   1. Bark          —— iOS 极速推送神器，即时弹窗与自定义铃声
//   2. 企业微信机器人  —— 微信原生直接接收，支持 Markdown 富文本排版
//   3. 钉钉群机器人    —— 钉钉群机器人，支持加签防伪安全策略
//   4. 飞书群机器人    —— 飞书群机器人，支持富文本消息与卡片
//   5. Server酱       —— 微信服务号消息直接触达（Turbo / 3）
//   6. PushDeer      —— 开源轻量推送，支持快贴与全平台通知
//   7. Telegram      —— 全球流行 Bot 推送
//   8. 自定义 Webhook —— 标准 HTTP POST/GET JSON 投递

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Event 一个通知事件。
type Event struct {
	ID      string         `json:"id"`
	Type    string         `json:"type"`
	Level   string         `json:"level"` // info / warn / error
	Title   string         `json:"title"`
	Message string         `json:"message"`
	Time    string         `json:"time"`
	Data    map[string]any `json:"data,omitempty"`
}

// Config 通知配置。
type Config struct {
	Enabled   bool              `json:"enabled"`
	Channel   string            `json:"channel"` // bark, wecom, dingtalk, feishu, serverchan, pushdeer, telegram, webhook
	Webhook   string            `json:"webhook"`
	Params    map[string]string `json:"params"`
	Events    []string          `json:"events"` // 空 = 全部
	QuietFrom string            `json:"quietFrom,omitempty"`
	QuietTo   string            `json:"quietTo,omitempty"`
}

// 事件类型常量。
const (
	EventAIAlert       = "ai.alert"
	EventAIPerson      = "ai.person"
	EventAIVehicle     = "ai.vehicle"
	EventAIPackage     = "ai.package"
	EventAIDigest      = "ai.digest"
	EventDeviceOffline = "device.offline"
	EventDeviceOnline  = "device.online"
	EventDiskLow       = "disk.low"
	EventRecordStart   = "record.start"
	EventRecordStop    = "record.stop"
	EventRecordFail    = "record.fail"
	EventExportDone    = "export.done"
	EventCredFail      = "cred.fail"
	EventStreamFail    = "stream.fail"
)

// AllEventTypes 所有可订阅的事件（给配置界面用）。
var AllEventTypes = []struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Category string `json:"category"`
}{
	{EventAIAlert, "AI 高危安全预警（摔倒/求救/险情）", "ai"},
	{EventAIPerson, "人员停留与陌生人徘徊", "ai"},
	{EventAIVehicle, "车辆进出与车牌识别", "ai"},
	{EventAIPackage, "包裹送达与被取走", "ai"},
	{EventAIDigest, "每日 AI 智能安防日报总结", "ai"},
	{EventDeviceOffline, "摄像头离线", "sys"},
	{EventDeviceOnline, "摄像头上线", "sys"},
	{EventDiskLow, "磁盘空间不足", "sys"},
	{EventRecordFail, "录像失败告警", "sys"},
	{EventCredFail, "凭据刷新失败", "sys"},
	{EventExportDone, "录像导出完成", "sys"},
}

// ChannelField 通道配置字段描述
type ChannelField struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Required    bool   `json:"required"`
	Placeholder string `json:"placeholder"`
	Hint        string `json:"hint"`
	Type        string `json:"type,omitempty"` // text, password, url
}

// ChannelMeta 推送渠道元数据
type ChannelMeta struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Icon        string         `json:"icon"`
	Badge       string         `json:"badge"`
	Description string         `json:"description"`
	Fields      []ChannelField `json:"fields"`
}

// AvailableChannels 所有支持的推送通道列表
var AvailableChannels = []ChannelMeta{
	{
		ID:          "pushplus",
		Name:        "PushPlus (推送加)",
		Icon:        "plus",
		Badge:       "微信公众号",
		Description: "国内广受欢迎的微信模板消息推送平台，支持一对一与群组通知",
		Fields: []ChannelField{
			{Key: "pushPlusToken", Label: "用户 Token", Required: true, Placeholder: "如: 1a2b3c4d5e6f...", Hint: "打开 pushplus.plus 微信扫码登录，在个人中心复制您的 Token"},
			{Key: "pushPlusTopic", Label: "群组编码 (Topic)", Required: false, Placeholder: "一对一推送留空；一对多填群组编码", Hint: "若需推送给家人或群组，可在 PushPlus 后台创建群组后填写群组编码"},
			{Key: "pushPlusChannel", Label: "发送渠道", Required: false, Placeholder: "留空默认: wechat (可选 cp/mail/webhook)", Hint: "默认通过「pushplus推送加」微信公众号接收"},
			{Key: "pushPlusServer", Label: "服务器地址", Required: false, Placeholder: "留空使用官方: https://www.pushplus.plus", Hint: "默认无需填写，留空走官方接口"},
		},
	},
	{
		ID:          "bark",
		Name:        "Bark (iOS)",
		Icon:        "bell",
		Badge:       "iOS 首选",
		Description: "iOS 极速推送神器，无需后台常驻，即时弹窗与丰富铃声",
		Fields: []ChannelField{
			{Key: "barkKey", Label: "Device Key", Required: true, Placeholder: "如: eK8xxxxx", Hint: "打开 iOS Bark App，首页复制设备 Key 填入此处"},
			{Key: "barkServer", Label: "自建服务器地址", Required: false, Placeholder: "留空使用官方: https://api.day.app", Hint: "默认无需填写；自建 Bark 服务器填写完整 HTTP(S) 地址"},
			{Key: "barkSound", Label: "提示音", Required: false, Placeholder: "如: minuet / alarm.caf / bell", Hint: "自定义收到通知时的铃声，留空使用默认"},
		},
	},
	{
		ID:          "wecom",
		Name:        "企业微信机器人",
		Icon:        "briefcase",
		Badge:       "微信原生",
		Description: "微信内直接弹窗接收，无需另装 App，Markdown 图文展示",
		Fields: []ChannelField{
			{Key: "wecomKey", Label: "Webhook Key 或完整地址", Required: true, Placeholder: "如: 693a91f6-xxxx 或完整 Webhook URL", Hint: "在企业微信群右上角 -> 添加机器人 -> 复制 Webhook 地址中的 key"},
		},
	},
	{
		ID:          "dingtalk",
		Name:        "钉钉群机器人",
		Icon:        "message-square",
		Badge:       "协同办公",
		Description: "钉钉群通知，支持加签安全密钥，团队安防高效协同",
		Fields: []ChannelField{
			{Key: "dingToken", Label: "Access Token 或完整 Webhook", Required: true, Placeholder: "如: access_token=xxxx 或完整地址", Hint: "在钉钉群管理 -> 机器人 -> 添加自定义机器人 -> 复制 Webhook"},
			{Key: "dingSecret", Label: "加签密钥 (Secret)", Required: false, Placeholder: "以 SEC 开头的加签密钥（选填）", Hint: "若机器人在钉钉后台勾选了「加签」，请填写此处"},
		},
	},
	{
		ID:          "feishu",
		Name:        "飞书群机器人",
		Icon:        "send",
		Badge:       "卡片交互",
		Description: "飞书群机器人，支持高优先级警报排版与加签验证",
		Fields: []ChannelField{
			{Key: "feishuWebhook", Label: "Webhook 地址或 Token", Required: true, Placeholder: "如: https://open.feishu.cn/open-apis/bot/v2/hook/xxxx", Hint: "在飞书群设置 -> 机器人 -> 添加自定义机器人获取"},
			{Key: "feishuSecret", Label: "签名校验 Secret", Required: false, Placeholder: "飞书加签密钥（选填）", Hint: "若机器人在飞书后台开启了签名校验请填入"},
		},
	},
	{
		ID:          "serverchan",
		Name:        "Server酱",
		Icon:        "mail",
		Badge:       "微信推送",
		Description: "经典微信服务号推送（Turbo/3），微信消息直接提醒",
		Fields: []ChannelField{
			{Key: "serverChanKey", Label: "SendKey", Required: true, Placeholder: "如: SCTxxxxxxxx", Hint: "在 sct.ftqq.com 微信扫码登录并获取 SendKey"},
		},
	},
	{
		ID:          "pushdeer",
		Name:        "PushDeer",
		Icon:        "smartphone",
		Badge:       "开源轻量",
		Description: "开源轻量快推，支持 iOS / Android / Mac 客户端与微信轻应用",
		Fields: []ChannelField{
			{Key: "pushDeerKey", Label: "PushKey", Required: true, Placeholder: "如: PDUxxxxxxxx", Hint: "在 PushDeer App 或小程序中生成 PushKey"},
			{Key: "pushDeerServer", Label: "自建服务器地址", Required: false, Placeholder: "留空使用官方: https://api2.pushdeer.com", Hint: "官方云服务留空即可"},
		},
	},
	{
		ID:          "telegram",
		Name:        "Telegram Bot",
		Icon:        "globe",
		Badge:       "全球通用",
		Description: "海外与全平台流行 Bot 推送，支持 HTML 富文本与群组广播",
		Fields: []ChannelField{
			{Key: "tgToken", Label: "Bot Token", Required: true, Placeholder: "如: 123456789:ABCdefGhIJKlmNoPQRsTUVwxyZ", Hint: "通过 @BotFather 创建 Bot 获取"},
			{Key: "tgChatId", Label: "Chat ID", Required: true, Placeholder: "如: 12345678 或群组 -100123456789", Hint: "接收消息的个人 ID 或群组 ID"},
		},
	},
	{
		ID:          "homeassistant",
		Name:        "Home Assistant",
		Icon:        "home",
		Badge:       "智能家居",
		Description: "联动 Home Assistant 自动化生态，支持 Webhook 与 REST API 实体同步",
		Fields: []ChannelField{
			{Key: "haMode", Label: "对接模式", Required: false, Placeholder: "webhook (默认) 或 rest_api", Hint: "选择 webhook (免鉴权) 或 rest_api (实体与全局事件)"},
			{Key: "haWebhookUrl", Label: "Webhook 接收地址", Required: false, Placeholder: "如: http://192.168.2.50:8123/api/webhook/micam_event", Hint: "Webhook 模式填写：在 HA 自动化创建 Webhook 触发器后的 URL"},
			{Key: "haBaseUrl", Label: "HA 基础地址", Required: false, Placeholder: "如: http://192.168.2.50:8123", Hint: "REST API 模式填写：Home Assistant 的 IP:端口"},
			{Key: "haToken", Label: "长期访问令牌 (Token)", Required: false, Placeholder: "eyJhbGciOi...", Hint: "REST API 模式填写：在 HA 个人资料最下方创建 Long-Lived Access Token"},
			{Key: "haEntityId", Label: "同步实体 ID", Required: false, Placeholder: "默认: sensor.micam_latest_event", Hint: "可选，自动同步最新告警状态与车牌/画面属性至该 HA 传感器"},
		},
	},
	{
		ID:          "pushover",
		Name:        "Pushover",
		Icon:        "bell",
		Badge:       "全平台即时",
		Description: "支持 iOS / Android / 桌面端高可靠即时通知与分级提醒",
		Fields: []ChannelField{
			{Key: "pushoverUser", Label: "User Key", Required: true, Placeholder: "用户或群组 User Key", Hint: "登录 pushover.net 首页获取 User Key"},
			{Key: "pushoverToken", Label: "API Token", Required: true, Placeholder: "应用 API Token/Key", Hint: "在 Pushover 创建应用获取 Application API Token"},
			{Key: "pushoverDevice", Label: "指定设备名", Required: false, Placeholder: "留空推送至所有设备", Hint: "可选，仅推送到特定手机或设备"},
			{Key: "pushoverPriority", Label: "优先级 (-2 到 2)", Required: false, Placeholder: "默认普通(0)，紧急填 1 或 2", Hint: "可选，2 为紧急报警需确认"},
			{Key: "pushoverSound", Label: "提示音", Required: false, Placeholder: "如: siren / echo / alien / persistent", Hint: "可选提示音，留空使用 Pushover 默认铃声"},
			{Key: "pushoverUrl", Label: "点击跳转地址", Required: false, Placeholder: "如: http://192.168.2.100:8900", Hint: "可选，收到通知点击直接打开 MiCam 监控后台"},
		},
	},
	{
		ID:          "webhook",
		Name:        "自动化 Webhook / n8n",
		Icon:        "link",
		Badge:       "自动化联动",
		Description: "向 n8n、Node-RED、Zapier 或自建服务投递事件，支持签名认证",
		Fields: []ChannelField{
			{Key: "webhook", Label: "Webhook URL", Required: true, Placeholder: "如: https://n8n.your-nas.local/webhook/micam-events", Hint: "接收事件的 HTTP(S) 地址"},
			{Key: "webhookPreset", Label: "数据格式预设", Required: false, Placeholder: "standard (标准) / n8n (优化) / simple (极简)", Hint: "默认 standard。n8n 预设会将事件放入 json 节点；simple 为扁平键值对"},
			{Key: "webhookSecret", Label: "签名密钥 (Secret)", Required: false, Placeholder: "可选自定义密钥", Hint: "填写后请求头附带 X-Webhook-Secret 与 X-Hub-Signature-256 签名"},
			{Key: "webhookMethod", Label: "HTTP 方法", Required: false, Placeholder: "POST (可选 PUT/GET)", Hint: "默认 POST"},
			{Key: "webhookHeaders", Label: "自定义请求头", Required: false, Placeholder: "如: Authorization: Bearer token123", Hint: "每行一个 Header，用于对接第三方鉴权"},
		},
	},
}

// Manager 通知管理器。
type Manager struct {
	mu       sync.Mutex
	cfg      Config
	events   []Event              // 最近事件（环形，最新在前）
	lastSent map[string]time.Time // 去重：事件键 -> 上次发送时间
	client   *http.Client
	maxKeep  int
}

// New 创建管理器。
func New(cfg Config) *Manager {
	if cfg.Channel == "" {
		if cfg.Webhook != "" {
			cfg.Channel = "webhook"
		} else {
			cfg.Channel = "bark"
		}
	}
	if cfg.Params == nil {
		cfg.Params = make(map[string]string)
	}
	return &Manager{
		cfg:      cfg,
		lastSent: map[string]time.Time{},
		client:   &http.Client{Timeout: 12 * time.Second},
		maxKeep:  200,
	}
}

// SetConfig 更新配置。
func (m *Manager) SetConfig(cfg Config) {
	m.mu.Lock()
	if cfg.Channel == "" {
		cfg.Channel = "webhook"
	}
	if cfg.Params == nil {
		cfg.Params = make(map[string]string)
	}
	m.cfg = cfg
	m.mu.Unlock()
}

// GetConfig 读配置。
func (m *Manager) GetConfig() Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

// Events 返回最近事件。
func (m *Manager) Events(limit int) []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 || limit > len(m.events) {
		limit = len(m.events)
	}
	out := make([]Event, limit)
	copy(out, m.events[:limit])
	return out
}

// ClearEvents 清空事件记录。
func (m *Manager) ClearEvents() {
	m.mu.Lock()
	m.events = nil
	m.mu.Unlock()
}

// isQuietTime 判断当前是否在免打扰时段内
func isQuietTime(from, to string) bool {
	if from == "" || to == "" {
		return false
	}
	now := time.Now()
	nowMin := now.Hour()*60 + now.Minute()

	parseMin := func(s string) int {
		parts := strings.Split(s, ":")
		if len(parts) != 2 {
			return -1
		}
		h, _ := strconv.Atoi(parts[0])
		m, _ := strconv.Atoi(parts[1])
		return h*60 + m
	}

	fMin := parseMin(from)
	tMin := parseMin(to)
	if fMin < 0 || tMin < 0 {
		return false
	}

	if fMin <= tMin {
		return nowMin >= fMin && nowMin < tMin
	}
	// 跨天，如 22:00 至 07:00
	return nowMin >= fMin || nowMin < tMin
}

// Send 发送一个事件。
func (m *Manager) Send(ev Event, dedup time.Duration) {
	if ev.ID == "" {
		ev.ID = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	if ev.Time == "" {
		ev.Time = time.Now().Format("2006-01-02 15:04:05")
	}
	if ev.Level == "" {
		ev.Level = "info"
	}

	m.mu.Lock()
	m.events = append([]Event{ev}, m.events...)
	if len(m.events) > m.maxKeep {
		m.events = m.events[:m.maxKeep]
	}

	cfg := m.cfg

	// 去重
	key := ev.Type + "|" + ev.Title
	if dedup > 0 {
		if last, ok := m.lastSent[key]; ok && time.Since(last) < dedup {
			m.mu.Unlock()
			return
		}
	}
	m.lastSent[key] = time.Now()
	m.mu.Unlock()

	log.Printf("[notify] %s [%s] %s - %s", ev.Type, ev.Level, ev.Title, ev.Message)

	if !cfg.Enabled {
		return
	}

	// 免打扰时段（非 error 级别静音）
	if ev.Level != "error" && isQuietTime(cfg.QuietFrom, cfg.QuietTo) {
		log.Printf("[notify] 当前处于免打扰时段 (%s - %s)，跳过非紧急推送", cfg.QuietFrom, cfg.QuietTo)
		return
	}

	if m.wants(cfg, ev.Type) {
		go m.DeliverWithRetry(cfg, ev)
	}
}

// wants 判断该事件是否被订阅。
func (m *Manager) wants(cfg Config, evType string) bool {
	if len(cfg.Events) == 0 {
		return true
	}
	for _, e := range cfg.Events {
		if e == evType {
			return true
		}
	}
	return false
}

// DeliverWithRetry 带退避重试的投递
func (m *Manager) DeliverWithRetry(cfg Config, ev Event) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(2*(1<<uint(attempt-1))) * time.Second)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		err := DeliverToChannel(ctx, m.client, cfg.Channel, cfg.Webhook, cfg.Params, ev)
		cancel()

		if err == nil {
			return // 成功
		}
		lastErr = err

		// 明确的不可重试错误（4xx 或参数错误）
		if strings.Contains(err.Error(), "400") || strings.Contains(err.Error(), "401") ||
			strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "404") ||
			strings.Contains(err.Error(), "未配置") {
			break
		}
	}

	if lastErr != nil {
		log.Printf("[notify] 通道 %s 投递失败: %v", cfg.Channel, lastErr)
	}
}

// TestChannel 测试指定通道推送
func (m *Manager) TestChannel(channel string, webhook string, params map[string]string) (int, error) {
	ev := Event{
		ID:      fmt.Sprintf("%d", time.Now().UnixNano()),
		Type:    "test",
		Level:   "info",
		Title:   "MiCam 智能监控测试通知",
		Message: "如果您看到这条消息，说明 MiCam 推送通道已成功联通！",
		Time:    time.Now().Format("2006-01-02 15:04:05"),
	}

	t0 := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	err := DeliverToChannel(ctx, m.client, channel, webhook, params, ev)
	if err != nil {
		return 0, err
	}
	return int(time.Since(t0).Milliseconds()), nil
}

// TestWebhook 兼容旧接口
func (m *Manager) TestWebhook(url string) error {
	_, err := m.TestChannel("webhook", url, nil)
	return err
}

// DeliverToChannel 核心全渠道路由投递
func DeliverToChannel(ctx context.Context, client *http.Client, channel string, webhook string, params map[string]string, ev Event) error {
	if client == nil {
		client = &http.Client{Timeout: 12 * time.Second}
	}
	if params == nil {
		params = make(map[string]string)
	}
	ch := strings.ToLower(strings.TrimSpace(channel))
	if ch == "" {
		ch = "webhook"
	}

	switch ch {
	case "pushplus":
		return deliverPushPlus(ctx, client, params, webhook, ev)
	case "bark":
		return deliverBark(ctx, client, params, webhook, ev)
	case "wecom":
		return deliverWeCom(ctx, client, params, webhook, ev)
	case "dingtalk":
		return deliverDingTalk(ctx, client, params, webhook, ev)
	case "feishu":
		return deliverFeishu(ctx, client, params, webhook, ev)
	case "serverchan":
		return deliverServerChan(ctx, client, params, webhook, ev)
	case "pushdeer":
		return deliverPushDeer(ctx, client, params, webhook, ev)
	case "telegram":
		return deliverTelegram(ctx, client, params, webhook, ev)
	case "homeassistant":
		return deliverHomeAssistant(ctx, client, params, webhook, ev)
	case "pushover":
		return deliverPushover(ctx, client, params, webhook, ev)
	case "webhook":
		fallthrough
	default:
		return deliverWebhook(ctx, client, webhook, params, ev)
	}
}

// 1. Bark 投递
func deliverBark(ctx context.Context, client *http.Client, params map[string]string, webhook string, ev Event) error {
	key := strings.TrimSpace(params["barkKey"])
	if key == "" {
		key = strings.TrimSpace(webhook)
	}
	if key == "" {
		return fmt.Errorf("未填写 Bark Device Key")
	}

	server := strings.TrimSpace(params["barkServer"])
	if server == "" {
		server = "https://api.day.app"
	}
	server = strings.TrimRight(server, "/")

	sound := strings.TrimSpace(params["barkSound"])
	if sound == "" {
		sound = "minuet"
	}

	level := "active"
	if ev.Level == "warn" || ev.Level == "error" {
		level = "timeSensitive"
	}

	reqBody := map[string]any{
		"device_key": key,
		"title":      "MiCam: " + ev.Title,
		"body":       ev.Message,
		"group":      "MiCam",
		"icon":       "https://img.icons8.com/color/96/cctv.png",
		"sound":      sound,
		"level":      level,
	}
	b, _ := json.Marshal(reqBody)

	req, err := http.NewRequestWithContext(ctx, "POST", server+"/push", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("请求 Bark 失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("Bark 返回 HTTP %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// 2. 企业微信群机器人投递
func deliverWeCom(ctx context.Context, client *http.Client, params map[string]string, webhook string, ev Event) error {
	key := strings.TrimSpace(params["wecomKey"])
	if key == "" {
		key = strings.TrimSpace(webhook)
	}
	if key == "" {
		return fmt.Errorf("未填写企业微信 Webhook Key 或地址")
	}

	targetURL := key
	if !strings.HasPrefix(targetURL, "http://") && !strings.HasPrefix(targetURL, "https://") {
		targetURL = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=" + key
	}

	levelColor := "info"
	if ev.Level == "warn" || ev.Level == "error" {
		levelColor = "warning"
	}

	mdContent := fmt.Sprintf("### 🔔 MiCam 监控通知: %s\n> **事件级别**: <font color=\"%s\">%s</font>\n> **发生时间**: %s\n> **详细内容**: %s",
		ev.Title, levelColor, strings.ToUpper(ev.Level), ev.Time, ev.Message)

	reqBody := map[string]any{
		"msgtype": "markdown",
		"markdown": map[string]string{
			"content": mdContent,
		},
	}
	b, _ := json.Marshal(reqBody)

	req, err := http.NewRequestWithContext(ctx, "POST", targetURL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("请求企业微信失败: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err == nil && res.ErrCode != 0 {
		return fmt.Errorf("企业微信错误 (%d): %s", res.ErrCode, res.ErrMsg)
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("企业微信返回 HTTP %d: %s", resp.StatusCode, string(raw))
	}
	return nil
}

// 3. 钉钉群机器人投递（支持加签）
func deliverDingTalk(ctx context.Context, client *http.Client, params map[string]string, webhook string, ev Event) error {
	token := strings.TrimSpace(params["dingToken"])
	if token == "" {
		token = strings.TrimSpace(webhook)
	}
	if token == "" {
		return fmt.Errorf("未填写钉钉 Access Token 或 Webhook")
	}

	targetURL := token
	if !strings.HasPrefix(targetURL, "http://") && !strings.HasPrefix(targetURL, "https://") {
		targetURL = "https://oapi.dingtalk.com/robot/send?access_token=" + token
	}

	secret := strings.TrimSpace(params["dingSecret"])
	if secret != "" {
		timestamp := time.Now().UnixMilli()
		stringToSign := fmt.Sprintf("%d\n%s", timestamp, secret)
		h := hmac.New(sha256.New, []byte(secret))
		h.Write([]byte(stringToSign))
		sign := url.QueryEscape(base64.StdEncoding.EncodeToString(h.Sum(nil)))
		sep := "&"
		if !strings.Contains(targetURL, "?") {
			sep = "?"
		}
		targetURL += fmt.Sprintf("%stimestamp=%d&sign=%s", sep, timestamp, sign)
	}

	mdContent := fmt.Sprintf("### 🔔 MiCam 监控通知: %s\n\n- **级别**: %s\n- **时间**: %s\n- **详情**: %s",
		ev.Title, strings.ToUpper(ev.Level), ev.Time, ev.Message)

	reqBody := map[string]any{
		"msgtype": "markdown",
		"markdown": map[string]string{
			"title": "MiCam: " + ev.Title,
			"text":  mdContent,
		},
	}
	b, _ := json.Marshal(reqBody)

	req, err := http.NewRequestWithContext(ctx, "POST", targetURL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("请求钉钉失败: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err == nil && res.ErrCode != 0 {
		return fmt.Errorf("钉钉错误 (%d): %s", res.ErrCode, res.ErrMsg)
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("钉钉返回 HTTP %d: %s", resp.StatusCode, string(raw))
	}
	return nil
}

// 4. 飞书群机器人投递（支持签名）
func deliverFeishu(ctx context.Context, client *http.Client, params map[string]string, webhook string, ev Event) error {
	hook := strings.TrimSpace(params["feishuWebhook"])
	if hook == "" {
		hook = strings.TrimSpace(webhook)
	}
	if hook == "" {
		return fmt.Errorf("未填写飞书 Webhook 地址")
	}

	targetURL := hook
	if !strings.HasPrefix(targetURL, "http://") && !strings.HasPrefix(targetURL, "https://") {
		targetURL = "https://open.feishu.cn/open-apis/bot/v2/hook/" + hook
	}

	cardTemplate := "blue"
	if ev.Level == "warn" {
		cardTemplate = "orange"
	} else if ev.Level == "error" {
		cardTemplate = "red"
	}

	reqBody := map[string]any{
		"msg_type": "interactive",
		"card": map[string]any{
			"header": map[string]any{
				"title": map[string]string{
					"tag":     "plain_text",
					"content": "🔔 MiCam 监控通知: " + ev.Title,
				},
				"template": cardTemplate,
			},
			"elements": []any{
				map[string]any{
					"tag": "div",
					"text": map[string]string{
						"tag":     "lark_md",
						"content": fmt.Sprintf("**级别**: %s\n**时间**: %s\n**详情**: %s", strings.ToUpper(ev.Level), ev.Time, ev.Message),
					},
				},
			},
		},
	}

	secret := strings.TrimSpace(params["feishuSecret"])
	if secret != "" {
		timestamp := time.Now().Unix()
		stringToSign := fmt.Sprintf("%d\n%s", timestamp, secret)
		h := hmac.New(sha256.New, []byte(stringToSign))
		h.Write([]byte{})
		sign := base64.StdEncoding.EncodeToString(h.Sum(nil))
		reqBody["timestamp"] = strconv.FormatInt(timestamp, 10)
		reqBody["sign"] = sign
	}

	b, _ := json.Marshal(reqBody)
	req, err := http.NewRequestWithContext(ctx, "POST", targetURL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("请求飞书失败: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(raw, &res); err == nil && res.Code != 0 {
		return fmt.Errorf("飞书错误 (%d): %s", res.Code, res.Msg)
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("飞书返回 HTTP %d: %s", resp.StatusCode, string(raw))
	}
	return nil
}

// 5. Server酱投递
func deliverServerChan(ctx context.Context, client *http.Client, params map[string]string, webhook string, ev Event) error {
	key := strings.TrimSpace(params["serverChanKey"])
	if key == "" {
		key = strings.TrimSpace(webhook)
	}
	if key == "" {
		return fmt.Errorf("未填写 Server酱 SendKey")
	}

	targetURL := fmt.Sprintf("https://sctapi.ftqq.com/%s.send", key)
	data := url.Values{}
	data.Set("title", "MiCam: "+ev.Title)
	data.Set("desp", fmt.Sprintf("### %s\n\n- **级别**: %s\n- **时间**: %s\n- **详情**: %s", ev.Title, strings.ToUpper(ev.Level), ev.Time, ev.Message))

	req, err := http.NewRequestWithContext(ctx, "POST", targetURL, strings.NewReader(data.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("请求 Server酱 失败: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	var res struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &res); err == nil && res.Code != 0 {
		return fmt.Errorf("Server酱错误 (%d): %s", res.Code, res.Message)
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("Server酱返回 HTTP %d: %s", resp.StatusCode, string(raw))
	}
	return nil
}

// 6. PushDeer 投递
func deliverPushDeer(ctx context.Context, client *http.Client, params map[string]string, webhook string, ev Event) error {
	key := strings.TrimSpace(params["pushDeerKey"])
	if key == "" {
		key = strings.TrimSpace(webhook)
	}
	if key == "" {
		return fmt.Errorf("未填写 PushDeer PushKey")
	}

	server := strings.TrimSpace(params["pushDeerServer"])
	if server == "" {
		server = "https://api2.pushdeer.com"
	}
	targetURL := strings.TrimRight(server, "/") + "/message/push"

	reqBody := map[string]string{
		"pushkey": key,
		"text":    "MiCam: " + ev.Title,
		"desp":    fmt.Sprintf("**级别**: %s\n**时间**: %s\n\n%s", strings.ToUpper(ev.Level), ev.Time, ev.Message),
		"type":    "markdown",
	}
	b, _ := json.Marshal(reqBody)

	req, err := http.NewRequestWithContext(ctx, "POST", targetURL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("请求 PushDeer 失败: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	var res struct {
		Code  int    `json:"code"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &res); err == nil && res.Code != 0 {
		return fmt.Errorf("PushDeer错误 (%d): %s", res.Code, res.Error)
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("PushDeer返回 HTTP %d: %s", resp.StatusCode, string(raw))
	}
	return nil
}

// 7. Telegram Bot 投递
func deliverTelegram(ctx context.Context, client *http.Client, params map[string]string, webhook string, ev Event) error {
	token := strings.TrimSpace(params["tgToken"])
	chatId := strings.TrimSpace(params["tgChatId"])
	if token == "" || chatId == "" {
		return fmt.Errorf("未填写 Telegram Bot Token 或 Chat ID")
	}

	targetURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token)
	msgText := fmt.Sprintf("<b>🔔 MiCam: %s</b>\n<i>级别: %s · 时间: %s</i>\n\n%s",
		ev.Title, strings.ToUpper(ev.Level), ev.Time, ev.Message)

	reqBody := map[string]any{
		"chat_id":    chatId,
		"text":       msgText,
		"parse_mode": "HTML",
	}
	b, _ := json.Marshal(reqBody)

	req, err := http.NewRequestWithContext(ctx, "POST", targetURL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("请求 Telegram 失败: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	var res struct {
		Ok          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(raw, &res); err == nil && !res.Ok {
		return fmt.Errorf("Telegram 错误: %s", res.Description)
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("Telegram 返回 HTTP %d: %s", resp.StatusCode, string(raw))
	}
	return nil
}

// 8. 通用 Webhook 投递（支持 n8n 预设、HMAC 签名、自定义 Method 与 Headers）
func deliverWebhook(ctx context.Context, client *http.Client, webhook string, params map[string]string, ev Event) error {
	urlStr := strings.TrimSpace(webhook)
	if urlStr == "" && params != nil {
		urlStr = strings.TrimSpace(params["webhook"])
	}
	if urlStr == "" {
		return fmt.Errorf("未配置 Webhook URL")
	}
	if !strings.HasPrefix(urlStr, "http://") && !strings.HasPrefix(urlStr, "https://") {
		return fmt.Errorf("Webhook 地址必须以 http:// 或 https:// 开头")
	}

	method := "POST"
	preset := "standard"
	secret := ""
	if params != nil {
		if m := strings.ToUpper(strings.TrimSpace(params["webhookMethod"])); m == "PUT" || m == "GET" {
			method = m
		}
		if p := strings.ToLower(strings.TrimSpace(params["webhookPreset"])); p != "" {
			preset = p
		}
		secret = strings.TrimSpace(params["webhookSecret"])
	}

	var payload any
	switch preset {
	case "n8n":
		payload = map[string]any{"json": ev}
	case "simple":
		sMap := map[string]any{
			"id":      ev.ID,
			"title":   ev.Title,
			"message": ev.Message,
			"level":   ev.Level,
			"type":    ev.Type,
			"time":    ev.Time,
		}
		if ev.Data != nil {
			for k, v := range ev.Data {
				sMap[k] = v
			}
		}
		payload = sMap
	default:
		payload = ev
	}

	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, method, urlStr, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("User-Agent", "micam-notify/2.0")

	if secret != "" {
		req.Header.Set("X-Webhook-Secret", secret)
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(b)
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}

	if params != nil {
		if hdrs := strings.TrimSpace(params["webhookHeaders"]); hdrs != "" {
			lines := strings.Split(hdrs, "\n")
			for _, l := range lines {
				l = strings.TrimSpace(l)
				if idx := strings.Index(l, ":"); idx > 0 {
					k := strings.TrimSpace(l[:idx])
					v := strings.TrimSpace(l[idx+1:])
					if k != "" {
						req.Header.Set(k, v)
					}
				}
			}
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("Webhook 请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("Webhook 返回 HTTP %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// 9. PushPlus (推送加) 微信模板消息投递
func deliverPushPlus(ctx context.Context, client *http.Client, params map[string]string, webhook string, ev Event) error {
	token := strings.TrimSpace(params["pushPlusToken"])
	if token == "" {
		token = strings.TrimSpace(webhook)
	}
	if token == "" {
		return fmt.Errorf("未填写 PushPlus 用户 Token")
	}

	server := strings.TrimSpace(params["pushPlusServer"])
	if server == "" {
		server = "https://www.pushplus.plus"
	}
	server = strings.TrimRight(server, "/")

	topic := strings.TrimSpace(params["pushPlusTopic"])
	channel := strings.TrimSpace(params["pushPlusChannel"])
	if channel == "" {
		channel = "wechat"
	}

	levelColor := "#2563eb"
	if ev.Level == "warn" {
		levelColor = "#d97706"
	} else if ev.Level == "error" {
		levelColor = "#dc2626"
	}

	content := fmt.Sprintf("### 🔔 MiCam 监控通知: %s\n\n- **级别**: <span style=\"color:%s\">**%s**</span>\n- **时间**: %s\n- **详情**: %s\n\n> 来自 MiCam 智能摄像头安防系统",
		ev.Title, levelColor, strings.ToUpper(ev.Level), ev.Time, ev.Message)

	reqBody := map[string]any{
		"token":    token,
		"title":    "MiCam: " + ev.Title,
		"content":  content,
		"template": "markdown",
		"channel":  channel,
	}
	if topic != "" {
		reqBody["topic"] = topic
	}

	b, _ := json.Marshal(reqBody)

	req, err := http.NewRequestWithContext(ctx, "POST", server+"/send", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("请求 PushPlus 失败: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("PushPlus 返回 HTTP %d: %s", resp.StatusCode, string(body))
	}

	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(body, &res); err == nil {
		if res.Code != 200 {
			return fmt.Errorf("PushPlus 错误 [%d]: %s", res.Code, res.Msg)
		}
	}
	return nil
}

// 10. Home Assistant 投递（支持 Webhook 与 REST API 实体同步）
func deliverHomeAssistant(ctx context.Context, client *http.Client, params map[string]string, webhook string, ev Event) error {
	mode := "webhook"
	if params != nil {
		if m := strings.ToLower(strings.TrimSpace(params["haMode"])); m == "rest_api" || m == "rest" || m == "api" {
			mode = "rest_api"
		}
	}

	haPayload := map[string]any{
		"event":   ev.Type,
		"title":   ev.Title,
		"message": ev.Message,
		"level":   ev.Level,
		"time":    ev.Time,
		"source":  "micam",
	}
	if ev.Data != nil {
		for k, v := range ev.Data {
			haPayload[k] = v
		}
	}

	if mode == "rest_api" {
		baseURL := ""
		token := ""
		if params != nil {
			baseURL = strings.TrimRight(strings.TrimSpace(params["haBaseUrl"]), "/")
			token = strings.TrimSpace(params["haToken"])
		}
		if baseURL == "" || token == "" {
			return fmt.Errorf("REST API 模式必须填写 HA 基础地址与长期访问令牌 (Token)")
		}

		// 1. 发送全局事件到 /api/events/micam_event
		eventURL := baseURL + "/api/events/micam_event"
		b, _ := json.Marshal(haPayload)
		req, err := http.NewRequestWithContext(ctx, "POST", eventURL, bytes.NewReader(b))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		req.Header.Set("User-Agent", "micam-homeassistant/2.0")

		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("请求 Home Assistant Events API 失败: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("Home Assistant Events API 返回 HTTP %d: %s", resp.StatusCode, string(body))
		}

		// 2. 更新传感器实体状态 (sensor.micam_latest_event)
		entityID := "sensor.micam_latest_event"
		if params != nil && strings.TrimSpace(params["haEntityId"]) != "" {
			entityID = strings.TrimSpace(params["haEntityId"])
		}
		stateURL := baseURL + "/api/states/" + entityID
		stateAttrs := map[string]any{
			"level":         ev.Level,
			"event_type":    ev.Type,
			"time":          ev.Time,
			"message":       ev.Message,
			"icon":          "mdi:cctv",
			"friendly_name": "MiCam 最新监控告警",
		}
		if ev.Data != nil {
			for k, v := range ev.Data {
				stateAttrs[k] = v
			}
		}
		sb, _ := json.Marshal(map[string]any{
			"state":      ev.Title,
			"attributes": stateAttrs,
		})
		sReq, err := http.NewRequestWithContext(ctx, "POST", stateURL, bytes.NewReader(sb))
		if err == nil {
			sReq.Header.Set("Authorization", "Bearer "+token)
			sReq.Header.Set("Content-Type", "application/json; charset=utf-8")
			if sResp, sErr := client.Do(sReq); sErr == nil {
				_ = sResp.Body.Close()
			}
		}
		return nil
	}

	// 默认 Webhook 模式
	haURL := ""
	if params != nil {
		haURL = strings.TrimSpace(params["haWebhookUrl"])
	}
	if haURL == "" {
		haURL = strings.TrimSpace(webhook)
	}
	if haURL == "" && params != nil {
		bURL := strings.TrimRight(strings.TrimSpace(params["haBaseUrl"]), "/")
		wID := strings.TrimSpace(params["haWebhookId"])
		if bURL != "" && wID != "" {
			haURL = bURL + "/api/webhook/" + wID
		}
	}
	if haURL == "" {
		return fmt.Errorf("未配置 Home Assistant Webhook URL")
	}

	b, _ := json.Marshal(haPayload)
	req, err := http.NewRequestWithContext(ctx, "POST", haURL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("User-Agent", "micam-homeassistant/2.0")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("请求 Home Assistant Webhook 失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("Home Assistant Webhook 返回 HTTP %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// 11. Pushover 投递（支持自定义声音、跳转 URL、HTML 富文本）
func deliverPushover(ctx context.Context, client *http.Client, params map[string]string, webhook string, ev Event) error {
	user := strings.TrimSpace(params["pushoverUser"])
	token := strings.TrimSpace(params["pushoverToken"])
	if user == "" || token == "" {
		return fmt.Errorf("未填写 Pushover User Key 或 API Token")
	}

	data := url.Values{}
	data.Set("user", user)
	data.Set("token", token)
	data.Set("title", "MiCam: "+ev.Title)

	levelColor := "#2563eb"
	if ev.Level == "warn" {
		levelColor = "#d97706"
	} else if ev.Level == "error" {
		levelColor = "#dc2626"
	}
	htmlMsg := fmt.Sprintf("<b><font color=\"%s\">[%s]</font> %s</b><br>%s<br><small>%s</small>",
		levelColor, strings.ToUpper(ev.Level), ev.Title, ev.Message, ev.Time)
	data.Set("message", htmlMsg)
	data.Set("html", "1")

	if dev := strings.TrimSpace(params["pushoverDevice"]); dev != "" {
		data.Set("device", dev)
	}
	if sound := strings.TrimSpace(params["pushoverSound"]); sound != "" {
		data.Set("sound", sound)
	}
	if link := strings.TrimSpace(params["pushoverUrl"]); link != "" {
		data.Set("url", link)
		data.Set("url_title", "打开 MiCam 查看监控")
	}

	if prio := strings.TrimSpace(params["pushoverPriority"]); prio != "" {
		data.Set("priority", prio)
	} else if ev.Level == "warn" || ev.Level == "error" {
		data.Set("priority", "1")
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.pushover.net/1/messages.json", strings.NewReader(data.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("请求 Pushover 失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("Pushover 返回 HTTP %d: %s", resp.StatusCode, string(body))
	}
	return nil
}
