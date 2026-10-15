package config

import (
	"encoding/json"
	"os"
	"sync"
)

// DevConfig 开放模式（开发者选项）配置。
//
// 设计参照 Android 开发者选项：默认隐藏，
// 连续点击版本号 7 次解锁。
type DevConfig struct {
	// Unlocked 是否已解锁开发者模式
	Unlocked bool `json:"unlocked"`
	// CORS 允许跨域（方便网页端/Electron 调试）
	CORS bool `json:"cors"`
	// AllowLAN 允许局域网直连（默认开启，:8900）
	AllowLAN bool `json:"allowLan"`
	// VerboseLog 详细日志（含凭据申请、连接生命周期）
	VerboseLog bool `json:"verboseLog"`
	// MockMode 模拟数据模式（不连真设备，方便 UI 开发）
	MockMode bool `json:"mockMode"`
	// AllowLANSensitive 允许局域网直接访问敏感接口（导出凭据等）。
	//
	// ⚠️ 默认 false = 保护生效：
	//    敏感接口要求访问码（<dataDir>/access-code.txt）。
	//    仅当用户明确知道风险时（如自己写脚本自动备份）才开关。
	AllowLANSensitive bool `json:"allowLANSensitive"`
	// ExposeSpec 暴露 API 规格文档接口
	ExposeSpec bool `json:"exposeSpec"`

	// ---- 开放能力（开发者页「开放模式」的 5 张能力卡）----
	//
	// 与上面 5 个「调试开关」的区别：
	//   上面是调试用途（CORS / 详细日志 / Mock），下面是**对外提供的能力**，
	//   效果图上每项一张卡（图标 + 标题 + 说明 + 开关）。

	// Enabled 全局开关：关掉后开放能力整体不可用（卡片区置灰）
	Enabled bool `json:"enabled"`
	// HLS 实时流（HLS）能力
	HLS bool `json:"hls"`
	// Webhook 事件推送能力
	Webhook bool `json:"webhook"`
	// FileAccess 本地文件访问（暴露录像文件，**安全敏感，默认必须关闭**）
	FileAccess bool `json:"fileAccess"`
	// WebSocket 实时事件流（**未实现**，占位）
	WebSocket bool `json:"websocket"`

	// TapCount 解锁进度（不落盘用途，仅内存）
	TapCount int `json:"-"`
}

// Config 应用配置。
// AIConfig AI 推理配置
type AIConfig struct {
	// ---- 本地推理 ----
	Enabled       bool     `json:"enabled"`
	Model         string   `json:"model"`
	ConfThreshold float64  `json:"confThreshold"`
	Classes       []string `json:"classes"`
	MaxPerMinute  int      `json:"maxPerMinute"`
	PythonPath    string   `json:"pythonPath"`
	Threads       int      `json:"threads"`

	// ---- 运行模式 ----
	//   local    只用本地（默认，不花钱）
	//   balanced 均衡：重要事件送云端
	//   full     完整智能：所有事件都送云端
	Mode string `json:"mode"`

	// ---- 本地检测项 ----
	DetectPerson   bool   `json:"detectPerson"`
	DetectVehicle  bool   `json:"detectVehicle"`
	DetectPackage  bool   `json:"detectPackage"`
	DetectPet      bool   `json:"detectPet"`
	EmbedEnabled   bool   `json:"embedEnabled"`
	FrameInterval  string `json:"frameInterval"`
	ResultKeepDays int    `json:"resultKeepDays"`

	// ---- 云端分析 ----
	//
	// 注意：**API Key 不在这里**，单独存在 <dataDir>/ai-key（权限 0600），
	// 避免随 config.json 一起被导出/分享。
	CloudEnabled   bool    `json:"cloudEnabled"`
	CloudImportant bool    `json:"cloudImportant"`
	DailyBudgetCNY float64 `json:"dailyBudgetCny"`
	BudgetAction   string  `json:"budgetAction"` // stop / alert
	ProviderName   string  `json:"providerName"`
	ProviderModel  string  `json:"providerModel"`
	ProviderURL    string  `json:"providerUrl"`
	APITimeout     int     `json:"apiTimeout"` // 秒
	MaxConcurrent  int     `json:"maxConcurrent"`
	MaxTokens      int     `json:"maxTokens"`  // 推理模型要给足，默认 1500
	UploadMode     string  `json:"uploadMode"` // snapshot / keyframes / clip

	// ---- 分析策略 ----
	Rules        map[string]bool `json:"rules"`
	Dedupe       bool            `json:"dedupe"`
	CacheEnabled bool            `json:"cacheEnabled"`

	// ---- 隐私：设备名 -> 是否允许上云 ----
	PrivacyCameras map[string]bool `json:"privacyCameras"`
}

// UIConfig 界面与播放偏好（设置页「通用」Tab）
type UIConfig struct {
	// 外观
	Theme      string `json:"theme"`      // system / dark / light
	Language   string `json:"language"`   // zh-CN / en
	TimeFormat string `json:"timeFormat"` // 24h / 12h

	// 播放默认值
	DefaultQuality  string `json:"defaultQuality"`  // 2k / hd / sd
	DefaultProtocol string `json:"defaultProtocol"` // hls / mjpeg
	AutoplayLive    bool   `json:"autoplayLive"`
	AutoplayMute    bool   `json:"autoplayMute"`

	// 界面偏好
	HomeShowCameras  bool `json:"homeShowCameras"`
	HomeShowEvents   bool `json:"homeShowEvents"`
	HomeShowStorage  bool `json:"homeShowStorage"`
	ShowEnglishAlias bool `json:"showEnglishAlias"`
	CompactMode      bool `json:"compactMode"`

	// 存储页底部那条「录像路径 / 数据路径」状态栏是否显示
	// 默认 true（零值 false），所以下面用指针区分"没设置过"
	ShowStoragePaths  *bool  `json:"showStoragePaths,omitempty"`
	EventsDefaultView string `json:"eventsDefaultView"` // grid / list

	// 录像
	RecordSchedule string `json:"recordSchedule"` // always / custom
	LoopOverwrite  bool   `json:"loopOverwrite"`

	// 通知
	QuietFrom    string `json:"quietFrom"` // "22:00"
	QuietTo      string `json:"quietTo"`   // "07:00"
	PushEnabled  bool   `json:"pushEnabled"`
	EmailEnabled bool   `json:"emailEnabled"`

	// 系统
	AutoUpdate bool `json:"autoUpdate"`
}

type Config struct {
	// 默认画质档位：2=标清, 3=2K（推荐）
	Subtype string `json:"subtype"`
	// 录像根目录（需飞牛授权）
	RecordDir string `json:"recordDir"`
	// 是否启用录像
	RecordEnabled bool `json:"recordEnabled"`
	// 录像分片时长（秒）
	RecordSegment int `json:"recordSegment"`
	// 录像模式: always (全天持续录像) / motion (仅动态侦测录像) / ai_event (AI智能事件录像) / off (关闭录像)
	RecordMode string `json:"recordMode"`
	// 录像保留天数（0 表示永久保留，不自动删除）
	RecordKeepDays int `json:"recordKeepDays"`
	// 录像最大存储容量上限 GB（0 表示不设上限）
	RecordMaxStorageGB int `json:"recordMaxStorageGB"`
	// 磁盘最低剩余空间 GB（低于则紧急清理 + 停止录像）
	RecordMinFreeGB int `json:"recordMinFreeGB"`
	// 动体侦测延录时间（秒）：运动停止后继续录制时长（默认 30 秒）
	RecordPostRollSec int `json:"recordPostRollSec"`
	// 动体侦测灵敏度：low / medium / high
	RecordMotionSensitivity string `json:"recordMotionSensitivity"`
	// 转码缓存上限 GB
	TranscodeCacheGB int `json:"transcodeCacheGB"`
	// 自动清理过期录像文件
	RecordAutoClean bool `json:"recordAutoClean"`
	// 低空间策略：deleteOldest / stopRecording / none
	RecordLowSpaceMode string `json:"recordLowSpaceMode"`
	// 事件通知
	NotifyEnabled bool              `json:"notifyEnabled"`
	NotifyChannel string            `json:"notifyChannel"`
	NotifyWebhook string            `json:"notifyWebhook"`
	NotifyParams  map[string]string `json:"notifyParams"`
	NotifyEvents  []string          `json:"notifyEvents"`
	// AI 推理（默认关闭；关闭后事件只依赖帧差侦测）
	AI AIConfig `json:"ai"`
	// 界面与播放偏好
	UI UIConfig `json:"ui"`
	// 日志级别
	LogLevel string `json:"logLevel"`
	// 开放模式
	Dev DevConfig `json:"dev"`

	mu   sync.Mutex `json:"-"`
	path string     `json:"-"`
}

// Default 返回默认配置。
func Default() *Config {
	return &Config{
		Subtype:            "3",
		RecordEnabled:      false,
		RecordSegment:      300,
		RecordKeepDays:     7,
		RecordMinFreeGB:    10,
		TranscodeCacheGB:   5,
		RecordMode:              "always",
		RecordMaxStorageGB:      0,
		RecordPostRollSec:       30,
		RecordMotionSensitivity: "medium",
		RecordAutoClean:         true,
		RecordLowSpaceMode:      "deleteOldest",
		LogLevel:           "info",
		AI: AIConfig{
			Enabled:       false,
			Model:         "yolov8n",
			ConfThreshold: 0.45,
			Classes:       []string{"person", "vehicle"},
			MaxPerMinute:  12,
			Threads:       4,

			Mode:           "local",
			DetectPerson:   true,
			DetectVehicle:  true,
			DetectPackage:  true,
			DetectPet:      false,
			EmbedEnabled:   false,
			FrameInterval:  "realtime",
			ResultKeepDays: 30,

			CloudEnabled:   false,
			CloudImportant: true,
			DailyBudgetCNY: 5,
			BudgetAction:   "alert",
			ProviderName:   "deepseek",
			ProviderModel:  "deepseek-v4.1-flash",
			ProviderURL:    "https://api.deepseek.com/v1",
			APITimeout:     60,
			MaxConcurrent:  2,
			MaxTokens:      1500,
			UploadMode:     "snapshot",

			Rules: map[string]bool{
				"door_person":  true,
				"night_person": true,
				"linger":       true,
				"repeat_car":   true,
				"day_car":      false,
			},
			Dedupe:       true,
			CacheEnabled: true,

			PrivacyCameras: map[string]bool{},
		},
		Dev: DevConfig{
			AllowLAN:   true,
			ExposeSpec: true,
			Enabled:    true,
			HLS:        true,
			Webhook:    true,
			// FileAccess / WebSocket 默认关闭：
			// 前者暴露录像文件，后者尚未实现
		},
	}
}

// Load 读配置；文件不存在时返回默认值。
func Load(path string) *Config {
	cfg := Default()
	cfg.path = path
	if path == "" {
		return cfg
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	_ = json.Unmarshal(raw, cfg)
	applyDevFlagDefaults(raw, cfg)
	if cfg.Subtype == "" {
		cfg.Subtype = "3"
	}
	if cfg.RecordSegment == 0 {
		cfg.RecordSegment = 300
	}
	var probeKeep struct {
		RecordKeepDays *int `json:"recordKeepDays"`
	}
	if err := json.Unmarshal(raw, &probeKeep); err == nil && probeKeep.RecordKeepDays == nil {
		cfg.RecordKeepDays = 7
	}
	if cfg.RecordMinFreeGB == 0 {
		cfg.RecordMinFreeGB = 10
	}
	if cfg.TranscodeCacheGB == 0 {
		cfg.TranscodeCacheGB = 5
	}
	if cfg.RecordMode == "" {
		if cfg.RecordEnabled {
			cfg.RecordMode = "always"
		} else {
			cfg.RecordMode = "off"
		}
	}
	if cfg.RecordPostRollSec == 0 {
		cfg.RecordPostRollSec = 30
	}
	if cfg.RecordMotionSensitivity == "" {
		cfg.RecordMotionSensitivity = "medium"
	}
	if cfg.RecordLowSpaceMode == "" {
		cfg.RecordLowSpaceMode = "deleteOldest"
	}
	return cfg
}

// applyDevFlagDefaults 给 v0.40 之前写下的 config.json 补新开关的默认值。
//
// 为什么需要单独做：这 3 个开关在旧文件里**不存在**，json.Unmarshal 会留成
// false —— 升级后「实时流 / 事件推送」会被静默关掉（用户什么都没改，功能没了）。
// 判断依据是原始 JSON 里有没有这个 key，而不是靠零值猜。
func applyDevFlagDefaults(raw []byte, cfg *Config) {
	var probe struct {
		Dev map[string]json.RawMessage `json:"dev"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return
	}
	setTrue := func(key string, dst *bool) {
		if _, ok := probe.Dev[key]; !ok {
			*dst = true
		}
	}
	setTrue("enabled", &cfg.Dev.Enabled)
	setTrue("hls", &cfg.Dev.HLS)
	setTrue("webhook", &cfg.Dev.Webhook)
	// fileAccess / websocket 默认就是 false，不写也对
}

// Save 写配置。
func (c *Config) Save(path string) error {
	if path == "" {
		path = c.path
	}
	if path == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

// SaveSelf 用记录过的路径保存。
func (c *Config) SaveSelf() error {
	return c.Save(c.path)
}

// ---- 录像策略设置 ----

// SetRecordPolicy 更新录像策略并落盘。
func (c *Config) SetRecordPolicy(enabled *bool, mode *string, segment, keepDays, maxStorageGB, minFreeGB, postRollSec, cacheGB *int, autoClean *bool, lowSpaceMode, motionSensitivity *string) error {
	c.mu.Lock()
	if enabled != nil {
		c.RecordEnabled = *enabled
	}
	if mode != nil && *mode != "" {
		c.RecordMode = *mode
		if *mode == "off" {
			c.RecordEnabled = false
		} else {
			c.RecordEnabled = true
		}
	}
	if segment != nil && *segment > 0 {
		c.RecordSegment = *segment
	}
	if keepDays != nil && *keepDays >= 0 {
		c.RecordKeepDays = *keepDays
	}
	if maxStorageGB != nil && *maxStorageGB >= 0 {
		c.RecordMaxStorageGB = *maxStorageGB
	}
	if minFreeGB != nil && *minFreeGB >= 0 {
		c.RecordMinFreeGB = *minFreeGB
	}
	if postRollSec != nil && *postRollSec >= 5 {
		c.RecordPostRollSec = *postRollSec
	}
	if cacheGB != nil && *cacheGB > 0 {
		c.TranscodeCacheGB = *cacheGB
	}
	if autoClean != nil {
		c.RecordAutoClean = *autoClean
	}
	if lowSpaceMode != nil && *lowSpaceMode != "" {
		c.RecordLowSpaceMode = *lowSpaceMode
	}
	if motionSensitivity != nil && *motionSensitivity != "" {
		c.RecordMotionSensitivity = *motionSensitivity
	}
	c.mu.Unlock()
	return c.SaveSelf()
}

// SetNotify 更新通知配置并落盘。
func (c *Config) SetNotify(enabled *bool, channel *string, webhook *string, params map[string]string, events *[]string) error {
	c.mu.Lock()
	if enabled != nil {
		c.NotifyEnabled = *enabled
	}
	if channel != nil && *channel != "" {
		c.NotifyChannel = *channel
	}
	if webhook != nil {
		c.NotifyWebhook = *webhook
	}
	if params != nil {
		if c.NotifyParams == nil {
			c.NotifyParams = make(map[string]string)
		}
		for k, v := range params {
			c.NotifyParams[k] = v
		}
	}
	if events != nil {
		c.NotifyEvents = *events
	}
	c.mu.Unlock()
	return c.SaveSelf()
}

// SetRecordDir 设置录像目录并落盘。
func (c *Config) SetRecordDir(dir string) error {
	c.mu.Lock()
	c.RecordDir = dir
	c.mu.Unlock()
	return c.SaveSelf()
}

// SetUI 更新界面配置并落盘。
func (c *Config) SetUI(u UIConfig) error {
	c.mu.Lock()
	c.UI = u
	c.mu.Unlock()
	return c.SaveSelf()
}

// SetAI 更新 AI 配置并落盘。
func (c *Config) SetAI(a AIConfig) error {
	c.mu.Lock()
	c.AI = a
	c.mu.Unlock()
	return c.SaveSelf()
}
