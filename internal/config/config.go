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
	// ExposeSpec 暴露 API 规格文档接口
	ExposeSpec bool `json:"exposeSpec"`
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

type Config struct {
	// 默认画质档位：2=标清, 3=2K（推荐）
	Subtype string `json:"subtype"`
	// 录像根目录（需飞牛授权）
	RecordDir string `json:"recordDir"`
	// 是否启用录像
	RecordEnabled bool `json:"recordEnabled"`
	// 录像分片时长（秒）
	RecordSegment int `json:"recordSegment"`
	// 录像保留天数（超期自动清理）
	RecordKeepDays int `json:"recordKeepDays"`
	// 磁盘最低剩余空间 GB（低于则紧急清理 + 停止录像）
	RecordMinFreeGB int `json:"recordMinFreeGB"`
	// 转码缓存上限 GB
	TranscodeCacheGB int `json:"transcodeCacheGB"`
	// 事件通知
	NotifyEnabled bool     `json:"notifyEnabled"`
	NotifyWebhook string   `json:"notifyWebhook"`
	NotifyEvents  []string `json:"notifyEvents"`
	// AI 推理（默认关闭；关闭后事件只依赖帧差侦测）
	AI AIConfig `json:"ai"`
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
		Subtype:          "3",
		RecordEnabled:    false,
		RecordSegment:    300,
		RecordKeepDays:   7,
		RecordMinFreeGB:  10,
		TranscodeCacheGB: 5,
		LogLevel:         "info",
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
	if cfg.Subtype == "" {
		cfg.Subtype = "3"
	}
	if cfg.RecordSegment == 0 {
		cfg.RecordSegment = 300
	}
	if cfg.RecordKeepDays == 0 {
		cfg.RecordKeepDays = 7
	}
	if cfg.RecordMinFreeGB == 0 {
		cfg.RecordMinFreeGB = 10
	}
	if cfg.TranscodeCacheGB == 0 {
		cfg.TranscodeCacheGB = 5
	}
	return cfg
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
func (c *Config) SetRecordPolicy(enabled *bool, segment, keepDays, minFreeGB, cacheGB *int) error {
	c.mu.Lock()
	if enabled != nil {
		c.RecordEnabled = *enabled
	}
	if segment != nil && *segment > 0 {
		c.RecordSegment = *segment
	}
	if keepDays != nil && *keepDays > 0 {
		c.RecordKeepDays = *keepDays
	}
	if minFreeGB != nil && *minFreeGB >= 0 {
		c.RecordMinFreeGB = *minFreeGB
	}
	if cacheGB != nil && *cacheGB > 0 {
		c.TranscodeCacheGB = *cacheGB
	}
	c.mu.Unlock()
	return c.SaveSelf()
}

// SetNotify 更新通知配置并落盘。
func (c *Config) SetNotify(enabled *bool, webhook *string, events *[]string) error {
	c.mu.Lock()
	if enabled != nil {
		c.NotifyEnabled = *enabled
	}
	if webhook != nil {
		c.NotifyWebhook = *webhook
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

// SetAI 更新 AI 配置并落盘。
func (c *Config) SetAI(a AIConfig) error {
	c.mu.Lock()
	c.AI = a
	c.mu.Unlock()
	return c.SaveSelf()
}
