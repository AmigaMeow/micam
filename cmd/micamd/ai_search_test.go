package main

import (
	"testing"
)

func TestParseNaturalQuery_TimeRanges(t *testing.T) {
	cams := []string{"小米室外摄像机CW500双摄版", "小米室外摄像机CW300"}

	tests := []struct {
		q    string
		days int
	}{
		{"今天有人经过吗", 1},
		{"昨天下午有车吗", 2},
		{"前天晚上有人吗", 3},
		{"最近一周有哪些事件", 7},
		{"最近三天", 3},
		{"最近30天", 30},
	}

	for _, tc := range tests {
		res := parseNaturalQuery(tc.q, cams, nil)
		if res.Days != tc.days {
			t.Errorf("q=%q expected days=%d, got %d", tc.q, tc.days, res.Days)
		}
	}
}

func TestParseNaturalQuery_PlateRecognition(t *testing.T) {
	cams := []string{"小米室外摄像机CW500双摄版", "小米室外摄像机CW300"}

	res := parseNaturalQuery("查询昨天车牌京A88888有没有来过", cams, nil)
	if res.Plate != "京A88888" {
		t.Errorf("expected Plate=京A88888, got %q", res.Plate)
	}
	if res.Days != 2 {
		t.Errorf("expected Days=2, got %d", res.Days)
	}
	hasVehicle := false
	for _, ty := range res.Types {
		if ty == "vehicle" {
			hasVehicle = true
		}
	}
	if !hasVehicle {
		t.Errorf("expected Types to contain vehicle, got %v", res.Types)
	}
}

func TestParseNaturalQuery_ClsKeywords(t *testing.T) {
	cams := []string{"小米室外摄像机CW500双摄版"}

	res1 := parseNaturalQuery("今天有小狗经过车位吗", cams, nil)
	foundDog := false
	for _, k := range res1.ClsKeywords {
		if k == "dog" {
			foundDog = true
		}
	}
	if !foundDog {
		t.Errorf("expected dog keyword for query '小狗', got %v", res1.ClsKeywords)
	}

	res2 := parseNaturalQuery("找最近出现的货车", cams, nil)
	foundTruck := false
	for _, k := range res2.ClsKeywords {
		if k == "truck" {
			foundTruck = true
		}
	}
	if !foundTruck {
		t.Errorf("expected truck keyword, got %v", res2.ClsKeywords)
	}
}

func TestParseNaturalQuery_DeviceMatch(t *testing.T) {
	cams := []string{"小米室外摄像机CW500双摄版", "小米室外摄像机CW300"}

	res1 := parseNaturalQuery("今天CW300有人吗", cams, nil)
	if res1.Device != "小米室外摄像机CW300" {
		t.Errorf("expected CW300 matched, got %q", res1.Device)
	}

	res2 := parseNaturalQuery("CW500昨晚动静", cams, nil)
	if res2.Device != "小米室外摄像机CW500双摄版" {
		t.Errorf("expected CW500 matched, got %q", res2.Device)
	}
}

func TestParseNaturalQuery_ZoneRecognition(t *testing.T) {
	cams := []string{"小米室外摄像机CW500双摄版", "小米室外摄像机CW300"}

	tests := []struct {
		q    string
		zone string
	}{
		{"今天有小狗经过车位吗", "车位"},
		{"昨晚车库有异动吗", "车位"},
		{"门口今天有人路过吗", "门口"},
		{"大门前有包裹吗", "门口"},
		{"后院昨晚有猫咪吗", "后院"},
	}

	for _, tc := range tests {
		res := parseNaturalQuery(tc.q, cams, nil)
		if res.Zone != tc.zone {
			t.Errorf("q=%q expected zone=%q, got %q", tc.q, tc.zone, res.Zone)
		}
	}
}

func TestMatchDetectionZone(t *testing.T) {
	box := []int{150, 150, 400, 500}
	zone := matchDetectionZone("小米室外摄像机CW300", box)
	if zone != "车位" {
		t.Errorf("expected zone 车位, got %q", zone)
	}
}

func TestParseNaturalQuery_ColorAndDuration(t *testing.T) {
	cams := []string{"小米室外摄像机CW500双摄版"}

	res1 := parseNaturalQuery("昨天下午停在院里的黑色轿车", cams, nil)
	if res1.ColorKeyword != "黑" {
		t.Errorf("expected ColorKeyword=黑, got %q", res1.ColorKeyword)
	}
	if res1.Days != 2 {
		t.Errorf("expected Days=2, got %d", res1.Days)
	}

	res2 := parseNaturalQuery("门口有可疑人员徘徊逗留吗", cams, nil)
	if res2.MinDuration != 15 {
		t.Errorf("expected MinDuration=15, got %d", res2.MinDuration)
	}
}
