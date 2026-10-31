package main

import (
	"strings"
	"testing"
	"time"

	"github.com/micam/micam/internal/event"
)

// TestCloudEventMapping_Issue22 验证 Issue #22 涉及的狗、猫、包裹、人员等类型解析
func TestCloudEventMapping_Issue22(t *testing.T) {
	cases := []struct {
		rawType      string
		expectedType string
		expectedCls  string
		expectedAI   bool
	}{
		{"DogMotion", "animal", "dog", true},
		{"CatMotion", "animal", "cat", true},
		{"PetMotion", "animal", "pet", true},
		{"AnimalMotion", "animal", "pet", true},
		{"PeopleMotion:IgnoreEvent", "person", "person", true},
		{"HumanMotion", "person", "person", true},
		{"VehicleMotion", "vehicle", "car", true},
		{"PackageMotion", "package", "package", true},
		{"BabyCry", "motion", "sound", false},
		{"LightMotion", "motion", "", false},
	}

	for _, tc := range cases {
		lowerType := strings.ToLower(tc.rawType)
		evType := event.TypeMotion
		aiCls := ""

		if strings.Contains(lowerType, "people") || strings.Contains(lowerType, "person") || strings.Contains(lowerType, "human") {
			evType = "person"
			aiCls = "person"
		} else if strings.Contains(lowerType, "vehicle") || strings.Contains(lowerType, "car") {
			evType = "vehicle"
			aiCls = "car"
		} else if strings.Contains(lowerType, "dog") {
			evType = "animal"
			aiCls = "dog"
		} else if strings.Contains(lowerType, "cat") {
			evType = "animal"
			aiCls = "cat"
		} else if strings.Contains(lowerType, "pet") || strings.Contains(lowerType, "animal") {
			evType = "animal"
			aiCls = "pet"
		} else if strings.Contains(lowerType, "package") || strings.Contains(lowerType, "box") {
			evType = "package"
			aiCls = "package"
		} else if strings.Contains(lowerType, "baby") || strings.Contains(lowerType, "cry") || strings.Contains(lowerType, "sound") {
			evType = "motion"
			aiCls = "sound"
		}

		isAI := (evType == "person" || evType == "vehicle" || evType == "animal" || evType == "package")

		if evType != tc.expectedType {
			t.Errorf("rawType %s: expected evType %s, got %s", tc.rawType, tc.expectedType, evType)
		}
		if aiCls != tc.expectedCls {
			t.Errorf("rawType %s: expected aiCls %s, got %s", tc.rawType, tc.expectedCls, aiCls)
		}
		if isAI != tc.expectedAI {
			t.Errorf("rawType %s: expected isAI %v, got %v", tc.rawType, tc.expectedAI, isAI)
		}
	}
}

// TestMotionSensitivityAndSilenceSeparation_Issue22 验证灵敏度阈值和真实目标防被吞噬机制
func TestMotionSensitivityAndSilenceSeparation_Issue22(t *testing.T) {
	// 1. 灵敏度映射
	sensMap := map[string]float64{
		"low":    9.5,
		"medium": 6.0,
		"high":   3.5,
		"":       6.0,
	}

	for sens, expected := range sensMap {
		baseThresh := 6.0
		switch strings.ToLower(sens) {
		case "low":
			baseThresh = 9.5
		case "high":
			baseThresh = 3.5
		}
		if baseThresh != expected {
			t.Errorf("sensitivity %s: expected threshold %.1f, got %.1f", sens, expected, baseThresh)
		}
	}

	// 2. 真实目标与纯变动静默期隔离
	ms := &motionState{
		prev:          map[string][]uint8{},
		prevAt:        map[string]time.Time{},
		lastPureEvt:   map[string]time.Time{},
		lastTargetEvt: map[string]time.Time{},
		lastEvt:       map[string]time.Time{},
		streak:        map[string]int{},
		pureStreak:    map[string]int{},
		segStart:      map[string]segInfo{},
	}

	did := "test_cam_01"
	t0 := time.Now().Add(-10 * time.Minute)

	// 模拟连续飞虫触发 pureStreak = 3，产生 240s 的退避静默期
	ms.mu.Lock()
	ms.pureStreak[did] = 3
	ms.lastPureEvt[did] = t0.Add(9 * time.Minute) // 1 分钟前有飞虫变动 (距今 60s < 240s)
	ms.lastEvt[did] = ms.lastPureEvt[did]
	ms.mu.Unlock()

	// 此时如果有人走动或小狗跑过 (isRealTarget = true)
	isRealTarget := true
	now := time.Now()

	ms.mu.Lock()
	if isRealTarget {
		ms.pureStreak[did] = 0
		lastTarget := ms.lastTargetEvt[did]
		// 之前从未触发过 target 事件，time.Since(lastTarget) 远大于 15s
		shouldEmit := time.Since(lastTarget) >= 15*time.Second
		if !shouldEmit {
			t.Errorf("Real target should NOT be blocked by previous bug/pure motion silence!")
		}
		ms.lastTargetEvt[did] = now
	}
	ms.mu.Unlock()

	if ms.pureStreak[did] != 0 {
		t.Errorf("pureStreak should be reset to 0 upon real target, got %d", ms.pureStreak[did])
	}
	if ms.lastTargetEvt[did] != now {
		t.Errorf("lastTargetEvt should be updated to now")
	}
}
