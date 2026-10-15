package main

import (
	"testing"
)

func TestEstimateCostCNY(t *testing.T) {
	// 1000 prompt tokens + 100 completion tokens
	cost := estimateCostCNY(1000, 100)
	if cost <= 0 {
		t.Errorf("cost should be positive, got %f", cost)
	}
	// 保守单帧约 ¥0.002 ~ ¥0.005
	if cost > 0.05 {
		t.Errorf("cost unreasonably high: %f", cost)
	}
}

func TestPrivacyCameraRule(t *testing.T) {
	privacyMap := map[string]bool{
		"CW300": false, // false 表示仅本地，禁止上云
		"CW500": true,  // 允许上云
	}

	isAllowed := func(dev string) bool {
		if allowed, ok := privacyMap[dev]; ok && !allowed {
			return false
		}
		return true
	}

	if isAllowed("CW300") {
		t.Errorf("CW300 should be rejected by privacy filter")
	}
	if !isAllowed("CW500") {
		t.Errorf("CW500 should be permitted by privacy filter")
	}
	if !isAllowed("UnknownCam") {
		t.Errorf("unconfigured camera should default to permitted")
	}
}
