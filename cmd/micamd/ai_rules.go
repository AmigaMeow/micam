package main

// 自动分析策略引擎。
//
// ===== 为什么需要它 =====
//
// 云端分析每次要花钱（¥0.002~0.003）。如果每个『画面变化』都送云端，
// 一天几百条就纯属浪费 —— 而且大部分是树叶晃动、光影变化这类噪音。
//
// 这个引擎负责判断：**这条事件值不值得花钱**。
//
// ===== 规则 =====
//
//   door_person   有人出现在门口       -> 高价值
//   night_person  夜间任何人员活动     -> 高价值
//   linger        可疑停留超过 30 秒   -> 高价值
//   repeat_car    重复出现未知车辆     -> 中价值
//   day_car       白天车辆经过         -> 默认关闭（噪音多）
//
// 所有规则默认开启（day_car 默认关），用户可逐条开关。

import (
	"strconv"
	"strings"
	"time"

	"github.com/micam/micam/internal/event"
)

// aiRuleResult 规则判定结果
type aiRuleResult struct {
	Analyze bool
	Reason  string   // 命中的规则（展示给用户看）
	Hits    []string // 全部命中的规则
	Score   float64  // 0~1，用于排序
}

// evalRules 判断一条事件是否应该送云端分析。
func evalRules(e *event.Event, cfg aiConfig, recentSame int) aiRuleResult {
	rules := cfg.Rules
	on := func(k string) bool {
		v, ok := rules[k]
		if !ok {
			// 默认值：day_car 关闭，其余开启
			return k != "day_car"
		}
		return v
	}

	var hits []string
	night := isNightTimeStr(e.Time)

	// 1) 门口与人员活动
	if (on("door_person") || on("person_loitering")) && e.Type == "person" {
		hits = append(hits, "有人出现在门口")
	}

	// 2) 夜间人员活动与高危窥探
	if (on("night_person") || on("night_suspicious")) && night && (e.Type == "person" || e.Type == "motion") {
		hits = append(hits, "夜间人员活动与异常窥探")
	}

	// 3) 可疑停留超过 30 秒
	if (on("linger") || on("person_loitering")) && e.Duration >= 30 {
		hits = append(hits, "可疑停留超过 30 秒")
	}

	// 4) 车辆与车道防护 (车旁徘徊 / 重复车辆)
	if (on("repeat_car") || on("car_loiter_guard")) && e.Type == "vehicle" && recentSame >= 2 {
		hits = append(hits, "重复出现的车辆")
	}

	// 5) 白天车辆经过（默认关）
	if on("day_car") && e.Type == "vehicle" && !night {
		hits = append(hits, "白天车辆经过")
	}

	// 6) 包裹资产与防盗
	if (on("pkg_delivered") || on("pkg_taken_theft")) && e.Type == "package" {
		hits = append(hits, "包裹资产动态")
	}

	// 7) 老幼照料 (跌倒 / 呼救)
	if on("family_fall_down") && e.Type == "person" && strings.Contains(e.Desc, "跌倒") {
		hits = append(hits, "疑似老人跌倒")
	}

	// 8) 风险隐患 (明火 / 烟雾)
	if on("hazard_fire_smoke") && (strings.Contains(e.Desc, "火") || strings.Contains(e.Desc, "烟")) {
		hits = append(hits, "火警与浓烟隐患")
	}

	// 包裹本身就是关注点，直接分析
	if e.Type == "package" {
		hits = append(hits, "包裹送达")
	}

	// 夜间噪音也值得看一眼（光影变化在夜里往往是异常）
	if night && e.Type == "motion" && on("night_person") {
		if !containsStr(hits, "夜间人员活动") {
			hits = append(hits, "夜间画面变化")
		}
	}

	// 9) 纯云端接管：若用户关闭了本地模型（或无本地算力），白天画面动态也交由云端大模型判定
	if !cfg.Enabled && cfg.CloudEnabled && e.Type == "motion" && !night {
		hits = append(hits, "云端视觉接管（画面动态待识别）")
	}

	if len(hits) == 0 {
		return aiRuleResult{Analyze: false, Reason: "不符合任何分析策略"}
	}

	// 重要度评分（用于排队优先级）
	score := 0.3
	if e.Type == "person" {
		score += 0.3
	}
	if e.Type == "package" {
		score += 0.35
	}
	if night {
		score += 0.2
	}
	if e.Duration >= 30 {
		score += 0.15
	}
	if score > 1 {
		score = 1
	}

	return aiRuleResult{
		Analyze: true,
		Reason:  strings.Join(hits, " + "),
		Hits:    hits,
		Score:   score,
	}
}

func containsStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// isNightTimeStr 22:00 - 06:00
func isNightTimeStr(t string) bool {
	if len(t) < 13 {
		return false
	}
	h, err := strconv.Atoi(t[11:13])
	if err != nil {
		return false
	}
	return h >= 22 || h < 6
}

// dedupeKey 去重键：同一设备 + 同一类型 + 5 分钟窗口
//
// 目的：一辆车开过去可能触发 5 条事件，只分析第一条即可。
func dedupeKey(e *event.Event, windowMin int) string {
	t, err := time.ParseInLocation("2006-01-02 15:04:05", e.Time, time.Local)
	if err != nil {
		return e.Did + "|" + e.Type
	}
	bucket := t.Unix() / int64(windowMin*60)
	return e.Did + "|" + e.Type + "|" + strconv.FormatInt(bucket, 10)
}
