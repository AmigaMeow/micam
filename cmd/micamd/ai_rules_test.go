package main

import (
	"testing"

	"github.com/micam/micam/internal/event"
)

func TestEvalRules(t *testing.T) {
	cfg := aiConfig{
		Rules: map[string]bool{
			"door_person":  true,
			"night_person": true,
			"linger":       true,
			"repeat_car":   true,
			"day_car":      false,
		},
	}

	// 1. 白天普通人员
	ev1 := &event.Event{
		ID:       "ev1",
		Time:     "2026-10-13 14:20:00",
		Type:     "person",
		Duration: 10,
	}
	r1 := evalRules(ev1, cfg, 0)
	if !r1.Analyze {
		t.Errorf("door_person should trigger for person event, got Analyze=false")
	}

	// 2. 夜间动检
	ev2 := &event.Event{
		ID:       "ev2",
		Time:     "2026-10-13 23:35:00",
		Type:     "motion",
		Duration: 5,
	}
	r2 := evalRules(ev2, cfg, 0)
	if !r2.Analyze {
		t.Errorf("night_person should trigger for night motion, got Analyze=false")
	}

	// 3. 停留超过30秒
	ev3 := &event.Event{
		ID:       "ev3",
		Time:     "2026-10-13 15:00:00",
		Type:     "vehicle",
		Duration: 35,
	}
	r3 := evalRules(ev3, cfg, 0)
	if !r3.Analyze {
		t.Errorf("linger should trigger for duration >= 30, got Analyze=false")
	}

	// 4. 白天车辆（day_car 默认关闭）
	ev4 := &event.Event{
		ID:       "ev4",
		Time:     "2026-10-13 15:00:00",
		Type:     "vehicle",
		Duration: 10,
	}
	r4 := evalRules(ev4, cfg, 0)
	if r4.Analyze {
		t.Errorf("day_car is off, should not analyze day vehicle, got Analyze=true")
	}

	// 5. 重复出现车辆 (recentSame >= 2)
	r5 := evalRules(ev4, cfg, 2)
	if !r5.Analyze {
		t.Errorf("repeat_car should trigger when recentSame >= 2, got Analyze=false")
	}
}

func TestDedupeKey(t *testing.T) {
	ev1 := &event.Event{
		Did:  "9900000001",
		Type: "vehicle",
		Time: "2026-10-13 23:35:10",
	}
	ev2 := &event.Event{
		Did:  "9900000001",
		Type: "vehicle",
		Time: "2026-10-13 23:36:20",
	}
	ev3 := &event.Event{
		Did:  "9900000001",
		Type: "vehicle",
		Time: "2026-10-13 23:45:00",
	}

	k1 := dedupeKey(ev1, 5)
	k2 := dedupeKey(ev2, 5)
	k3 := dedupeKey(ev3, 5)

	if k1 != k2 {
		t.Errorf("events within 5-min window should have same dedupe key: %q != %q", k1, k2)
	}
	if k1 == k3 {
		t.Errorf("event after 10 mins should have different dedupe key: %q == %q", k1, k3)
	}
}

func TestPureCloudMotionRule(t *testing.T) {
	cfg := aiConfig{
		Enabled:      false,
		CloudEnabled: true,
		Rules:        map[string]bool{},
	}
	ev := &event.Event{
		ID:       "ev_pure_cloud",
		Time:     "2026-10-13 14:00:00",
		Type:     "motion",
		Duration: 10,
	}
	r := evalRules(ev, cfg, 0)
	if !r.Analyze {
		t.Errorf("pure cloud mode should trigger for daytime motion, got Analyze=false")
	}
}
