package config

import (
	"encoding/json"
	"os"
)

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
	// 日志级别
	LogLevel string `json:"logLevel"`
}

// Default 返回默认配置。
func Default() *Config {
	return &Config{
		Subtype:       "3",
		RecordEnabled: false,
		RecordSegment: 300,
		LogLevel:      "info",
	}
}

// Load 读配置；文件不存在时返回默认值。
func Load(path string) *Config {
	cfg := Default()
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
	return cfg
}

// Save 写配置。
func (c *Config) Save(path string) error {
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}
