package main

// 设备 ID 持久化（2026-09-23）。
//
// ===== 为什么必须持久化 =====
//
// 原来 internal/xiaomi 的 NewSession 每次都随机生成 deviceId：
//
//	deviceID: "wb_" + randHex(16)
//
// 于是【每次服务重启都是一个"新设备"】。
//
// 反编译官方米家 App 发现：
//   · PREF_KEY_DEVICE_ANDROID_ID   —— 官方持久化 androidId
//   · PREF_KEY_DEVICE_RANDOM_UUID  —— 官方持久化随机 UUID
//   · passport_error_device_id     —— 登录体系里有"设备ID"错误码
//
// 说明 deviceId 是小米账号安全体系的一部分。
// 频繁出现"陌生设备申请设备凭据"极可能触发风控 ——
// 这很可能是 miss_get_vendor 持续限流的原因之一。
//
// 实现：存到 <dataDir>/device-id.json，每次 NewSession 复用同一个。

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/micam/micam/internal/xiaomi"
)

var cachedDeviceID string

func deviceIDFile() string {
	return filepath.Join(dataDirPath, "device-id.json")
}

// loadDeviceID 载入（或首次生成）持久化的设备 ID。
func loadDeviceID() {
	raw, err := os.ReadFile(deviceIDFile())
	if err == nil {
		var v struct {
			DeviceID string `json:"deviceId"`
		}
		if json.Unmarshal(raw, &v) == nil && v.DeviceID != "" {
			cachedDeviceID = v.DeviceID
			logf("[dev] 载入设备 ID: %s", cachedDeviceID)
		}
	}

	// 注入给 xiaomi 包
	xiaomi.SetDeviceIDProvider(func() string {
		return cachedDeviceID
	})

	// 首次运行：先生成一个（NewSession 会回填）
	if cachedDeviceID == "" {
		s := xiaomi.NewSession()
		cachedDeviceID = s.DeviceID()
		saveDeviceID()
		logf("[dev] 首次生成设备 ID: %s（此后固定不变）", cachedDeviceID)
	}
}

func saveDeviceID() {
	if cachedDeviceID == "" {
		return
	}
	raw, _ := json.MarshalIndent(map[string]string{"deviceId": cachedDeviceID}, "", "  ")
	_ = os.WriteFile(deviceIDFile(), raw, 0o600)
}

// ensureDeviceIDStable 在会话创建后调用，把实际用的 deviceId 固化下来。
//
// 场景：注入 provider 时 cachedDeviceID 还是空的，
// NewSession 内部会随机生成一个。这里把它记下来并落盘。
func ensureDeviceIDStable(s *xiaomi.Session) {
	if s == nil {
		return
	}
	id := s.DeviceID()
	if id == "" || id == cachedDeviceID {
		return
	}
	cachedDeviceID = id
	saveDeviceID()
}
