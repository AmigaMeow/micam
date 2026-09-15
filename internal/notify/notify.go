package notify

// 事件通知。
//
// 监控产品的核心需求：出事了要能知道。
//
// 支持两种投递：
//   1. Webhook  —— HTTP POST JSON 到用户配的地址
//      （可对接 钉钉/企业微信/Bark/Server酱/自建服务）
//   2. 站内事件 —— 保留最近 N 条，前端可查看
//
// 设计要点：
//   - 投递失败自动退避重试（3 次）
//   - 事件去重（同一事件短时间内不重复发）
//   - 队列有上限，避免内存无限增长

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
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
	Enabled bool     `json:"enabled"`
	Webhook string   `json:"webhook"`
	Events  []string `json:"events"` // 空 = 全部
}

// 事件类型常量。
const (
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
	Key   string `json:"key"`
	Label string `json:"label"`
}{
	{EventDeviceOffline, "摄像头离线"},
	{EventDeviceOnline, "摄像头上线"},
	{EventDiskLow, "磁盘空间不足"},
	{EventRecordStart, "录像开始"},
	{EventRecordStop, "录像停止"},
	{EventRecordFail, "录像失败"},
	{EventExportDone, "导出完成"},
	{EventCredFail, "凭据刷新失败"},
	{EventStreamFail, "播放失败"},
}

// Manager 通知管理器。
type Manager struct {
	mu       sync.Mutex
	cfg      Config
	events   []Event          // 最近事件（环形，最新在前）
	lastSent map[string]time.Time // 去重：事件键 -> 上次发送时间
	client   *http.Client
	maxKeep  int
}

// New 创建管理器。
func New(cfg Config) *Manager {
	return &Manager{
		cfg:      cfg,
		lastSent: map[string]time.Time{},
		client:   &http.Client{Timeout: 10 * time.Second},
		maxKeep:  200,
	}
}

// SetConfig 更新配置。
func (m *Manager) SetConfig(cfg Config) {
	m.mu.Lock()
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

// Send 发送一个事件。
//
// dedup 是去重窗口：同一个 (type+title) 在这个时间内只发一次。
// 传 0 表示不去重。
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
	// 记录到站内事件
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

	// Webhook 投递
	if cfg.Enabled && cfg.Webhook != "" && m.wants(cfg, ev.Type) {
		go m.deliver(cfg.Webhook, ev)
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

// deliver 投递 Webhook（带退避重试）。
func (m *Manager) deliver(url string, ev Event) {
	body, err := json.Marshal(ev)
	if err != nil {
		return
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(2*(1<<uint(attempt-1))) * time.Second)
		}

		req, err := http.NewRequest("POST", url, bytes.NewReader(body))
		if err != nil {
			lastErr = err
			break
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "micam-notify/1.0")

		res, err := m.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		_ = res.Body.Close()

		if res.StatusCode >= 200 && res.StatusCode < 300 {
			return // 成功
		}

		// 4xx 不重试（地址/格式问题，重试没用）
		if res.StatusCode >= 400 && res.StatusCode < 500 {
			lastErr = fmt.Errorf("webhook 返回 %d（不重试）", res.StatusCode)
			break
		}
		lastErr = fmt.Errorf("webhook 返回 %d", res.StatusCode)
	}

	if lastErr != nil {
		log.Printf("[notify] webhook 投递失败: %v", lastErr)
	}
}

// TestWebhook 测试 Webhook 是否可用。
func (m *Manager) TestWebhook(url string) error {
	if url == "" {
		return fmt.Errorf("未配置地址")
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return fmt.Errorf("地址必须以 http:// 或 https:// 开头")
	}

	ev := Event{
		ID:      fmt.Sprintf("%d", time.Now().UnixNano()),
		Type:    "test",
		Level:   "info",
		Title:   "MiCam 测试通知",
		Message: "如果你看到这条消息，说明 Webhook 配置成功。",
		Time:    time.Now().Format("2006-01-02 15:04:05"),
	}
	body, _ := json.Marshal(ev)

	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("请求失败: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("返回状态码 %d", res.StatusCode)
	}
	return nil
}
