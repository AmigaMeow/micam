package main

import (
	"strings"
	"testing"

	"github.com/micam/micam/internal/event"
)

// 1. 边缘算法与空间检测器单元测试
func TestScenarioDetectors_SpatialAndMotion(t *testing.T) {
	// A. 人车空间近距离检测
	carBox := []float64{0.2, 0.4, 0.6, 0.8}    // 中心 (0.4, 0.6)
	farPerson := []float64{0.7, 0.1, 0.8, 0.3} // 中心 (0.75, 0.2)
	nearPerson := []float64{0.55, 0.5, 0.65, 0.7} // 中心 (0.6, 0.6), dist = 0.2

	// 远距离人员 -> false
	if ok, _ := DetectPersonVehicleProximity(farPerson, carBox, 30); ok {
		t.Errorf("Distant person should not trigger vehicle proximity loitering")
	}

	// 近距离人员但停留仅 5 秒 -> false
	if ok, _ := DetectPersonVehicleProximity(nearPerson, carBox, 5); ok {
		t.Errorf("Near person with <15s linger should not trigger loitering")
	}

	// 近距离人员且停留 20 秒 -> true
	if ok, reason := DetectPersonVehicleProximity(nearPerson, carBox, 20); !ok || reason == "" {
		t.Errorf("Near person with >15s linger should trigger loitering, got ok=%v", ok)
	}

	// B. 老人意外摔倒高宽比检测
	uprightPerson := []float64{0.4, 0.1, 0.55, 0.7} // w=0.15, h=0.6, ar=0.25 (直立)
	fallenPerson := []float64{0.2, 0.65, 0.7, 0.9}   // w=0.5, h=0.25, ar=2.0 (卧地贴近地面)

	if ok, _ := DetectHumanFallDown(uprightPerson); ok {
		t.Errorf("Upright person should not trigger fall down detection")
	}
	if ok, ar := DetectHumanFallDown(fallenPerson); !ok || ar < 1.15 {
		t.Errorf("Fallen person with aspect ratio 2.0 should trigger fall down, got ok=%v, ar=%f", ok, ar)
	}

	// C. 儿童靠近危险区域检测
	dangerBalcony := []float64{0.0, 0.6, 0.4, 1.0} // 危险区 (如阳台边缘)
	adultPerson := []float64{0.1, 0.2, 0.3, 0.9}   // h=0.7 (成人)
	childInDanger := []float64{0.1, 0.65, 0.25, 0.95} // h=0.3 (儿童), 脚在危险区
	childSafe := []float64{0.6, 0.65, 0.75, 0.95}     // h=0.3 (儿童), 安全区

	if DetectChildDangerZone(adultPerson, dangerBalcony) {
		t.Errorf("Adult should not trigger child danger zone")
	}
	if !DetectChildDangerZone(childInDanger, dangerBalcony) {
		t.Errorf("Child in danger zone should trigger alert")
	}
	if DetectChildDangerZone(childSafe, dangerBalcony) {
		t.Errorf("Child in safe zone should not trigger alert")
	}

	// D. 包裹被盗强关联检测
	if ok, _ := DetectPackageTheftCorrelation(true, false, true); !ok {
		t.Errorf("Package disappeared while person present should trigger theft correlation")
	}
	if ok, _ := DetectPackageTheftCorrelation(true, true, true); ok {
		t.Errorf("Package still present should not trigger theft")
	}

	// E. 违停占道超时检测
	if ok, _ := DetectOvertimeParking(300, 600); ok {
		t.Errorf("5 min parking should not exceed 10 min threshold")
	}
	if ok, reason := DetectOvertimeParking(900, 600); !ok || reason == "" {
		t.Errorf("15 min parking should trigger overtime alert")
	}

	// F. 云端专属 Prompt 注入
	vlmPromptCar := ScenarioPromptForVLM("car_loiter_guard", "")
	if !strings.Contains(vlmPromptCar, "车旁异常徘徊") || !strings.Contains(vlmPromptCar, "拉车门") {
		t.Errorf("VLM prompt for car_loiter_guard missing specialized keywords")
	}

	vlmPromptFall := ScenarioPromptForVLM("family_fall_down", "")
	if !strings.Contains(vlmPromptFall, "老人意外跌倒") || !strings.Contains(vlmPromptFall, "痛苦倒地") {
		t.Errorf("VLM prompt for family_fall_down missing fall keywords")
	}
}

// 2. 场景规则判定引擎全覆盖测试
func TestEvalRules_AllScenarios(t *testing.T) {
	cfg := aiConfig{
		Rules: map[string]bool{
			"car_loiter_guard":   true,
			"person_loitering":   true,
			"night_suspicious":   true,
			"pkg_delivered":      true,
			"pkg_taken_theft":    true,
			"family_fall_down":   true,
			"hazard_fire_smoke":  true,
			"child_danger_zone":  false,
			"day_car":            false,
		},
	}

	// A. 人员滞留 (>30秒)
	evLoiter := &event.Event{
		ID:       "ev-loiter",
		Time:     "2026-10-14 14:00:00",
		Type:     "person",
		Duration: 35,
	}
	r1 := evalRules(evLoiter, cfg, 0)
	if !r1.Analyze {
		t.Errorf("person_loitering should trigger analyze for duration >= 30s")
	}

	// B. 深夜人员活动 (23:15)
	evNight := &event.Event{
		ID:       "ev-night",
		Time:     "2026-10-14 23:15:00",
		Type:     "person",
		Duration: 5,
	}
	r2 := evalRules(evNight, cfg, 0)
	if !r2.Analyze || !strings.Contains(r2.Reason, "夜间") {
		t.Errorf("night_suspicious should trigger analyze for nighttime person")
	}

	// C. 包裹送达
	evPkg := &event.Event{
		ID:       "ev-pkg",
		Time:     "2026-10-14 11:20:00",
		Type:     "package",
		Duration: 10,
	}
	r3 := evalRules(evPkg, cfg, 0)
	if !r3.Analyze || !strings.Contains(r3.Reason, "包裹") {
		t.Errorf("pkg_delivered should trigger analyze for package event")
	}

	// D. 老人跌倒
	evFall := &event.Event{
		ID:       "ev-fall",
		Time:     "2026-10-14 10:05:00",
		Type:     "person",
		Desc:     "检测到人员意外跌倒",
		Duration: 15,
	}
	r4 := evalRules(evFall, cfg, 0)
	if !r4.Analyze || !strings.Contains(r4.Reason, "跌倒") {
		t.Errorf("family_fall_down should trigger analyze for fall description")
	}

	// E. 明火浓烟
	evFire := &event.Event{
		ID:       "ev-fire",
		Time:     "2026-10-14 18:30:00",
		Type:     "motion",
		Desc:     "疑似火光闪烁",
		Duration: 8,
	}
	r5 := evalRules(evFire, cfg, 0)
	if !r5.Analyze || !strings.Contains(r5.Reason, "火警") {
		t.Errorf("hazard_fire_smoke should trigger analyze for fire motion")
	}
}

// 3. 24 小时安防日程早晚报聚合引擎测试
func TestDailyDigest_Aggregation(t *testing.T) {
	testEvents := []*event.Event{
		{
			ID:       "ev1",
			Time:     "2026-10-14 09:15:00",
			Type:     "vehicle",
			AIPlate:  "贵J·39X72",
			AITitle:  "白色轿车 [贵J·39X72] 驶入停放",
			Duration: 120,
		},
		{
			ID:       "ev2",
			Time:     "2026-10-14 10:30:00",
			Type:     "package",
			Desc:     "门口放置快递一件",
			Duration: 10,
		},
		{
			ID:       "ev3",
			Time:     "2026-10-14 14:20:00",
			Type:     "person",
			Desc:     "门前人员活动",
			Duration: 25, // 超过20秒需关注
		},
		{
			ID:       "ev4",
			Time:     "2026-10-14 15:45:00",
			Type:     "vehicle",
			AIPlate:  "京A·88888",
			AITitle:  "黑色SUV [京A·88888] 停放于车位",
			Duration: 60,
		},
	}

	digest := GenerateDailyDigest(testEvents, "2026-10-14")
	if digest.VehicleArrivals != 2 {
		t.Errorf("Expected 2 vehicle arrivals, got %d", digest.VehicleArrivals)
	}
	if digest.PackageEvents != 1 {
		t.Errorf("Expected 1 package event, got %d", digest.PackageEvents)
	}
	if digest.SuspiciousAlerts != 1 {
		t.Errorf("Expected 1 suspicious loitering alert, got %d", digest.SuspiciousAlerts)
	}
	if !strings.Contains(digest.Summary, "贵J·39X72") || !strings.Contains(digest.Summary, "京A·88888") {
		t.Errorf("Daily summary should mention identified license plates")
	}
	if len(digest.KeyEvents) != 4 {
		t.Errorf("Expected 4 key highlight events, got %d", len(digest.KeyEvents))
	}
}
