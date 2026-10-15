package main

import (
	"strings"
	"testing"
)

func TestIsNightTime(t *testing.T) {
	if !isNightTime("2026-10-13 23:35:28") {
		t.Errorf("23:35 should be night time")
	}
	if !isNightTime("2026-10-13 03:10:00") {
		t.Errorf("03:10 should be night time")
	}
	if isNightTime("2026-10-13 14:00:00") {
		t.Errorf("14:00 should not be night time")
	}
}

func TestBuildLocalSummary(t *testing.T) {
	stZero := aiTodayStat{}
	if s := buildLocalSummary(stZero); s != "今天还没有检测到事件。" {
		t.Errorf("zero stats expected '今天还没有检测到事件。', got %q", s)
	}

	stMixed := aiTodayStat{
		Total:   5,
		Person:  2,
		Vehicle: 2,
		Package: 1,
		Watch:   1,
	}
	s := buildLocalSummary(stMixed)
	if !strings.Contains(s, "5 个有效事件") {
		t.Errorf("summary should contain total count: %s", s)
	}
	if !strings.Contains(s, "2 次人员活动") || !strings.Contains(s, "1 个包裹送达") {
		t.Errorf("summary missing details: %s", s)
	}
}

func TestCleanBriefText(t *testing.T) {
	raw := "```markdown\n\"今日一切安好，共检测到2次人员与车辆活动。\"\n```"
	cleaned := cleanBriefText(raw)
	expected := "今日一切安好，共检测到2次人员与车辆活动。"
	if cleaned != expected {
		t.Errorf("cleanBriefText failed, expected %q, got %q", expected, cleaned)
	}
}
