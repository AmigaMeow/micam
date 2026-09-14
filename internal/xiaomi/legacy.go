package xiaomi

import "github.com/AlexxIT/go2rtc/pkg/xiaomi"

// IsLegacyModel 判断是否老机型（走 tutk）。
func IsLegacyModel(model string) bool {
	return xiaomi.IsLegacy(model)
}
