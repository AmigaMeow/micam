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
