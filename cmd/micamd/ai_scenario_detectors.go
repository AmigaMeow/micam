package main

import (
	"math"
)

// ai_scenario_detectors.go
// 监控全场景边缘 AI 算法与多模态研判分发器

// 1. 人车空间距离检测 (car_loiter_guard)
// box: [x1, y1, x2, y2] 相对坐标 (0.0 ~ 1.0)
// 返回中心点归一化欧式距离
func boxDistance(b1, b2 []float64) float64 {
	if len(b1) < 4 || len(b2) < 4 {
		return 999.0
	}
	cx1 := (b1[0] + b1[2]) / 2.0
	cy1 := (b1[1] + b1[3]) / 2.0
	cx2 := (b2[0] + b2[2]) / 2.0
	cy2 := (b2[1] + b2[3]) / 2.0

	dx := cx1 - cx2
	dy := cy1 - cy2
	return math.Sqrt(dx*dx + dy*dy)
}

// DetectPersonVehicleProximity 判断人员与停放车辆是否处于高危近距离 (如距离 < 0.28 画面宽度，且停留 > 15秒)
func DetectPersonVehicleProximity(personBox, carBox []float64, lingerSec int) (bool, string) {
	dist := boxDistance(personBox, carBox)
	// 距离小于 0.28 并且停留超过 15 秒判定为车旁可疑逗留
	if dist <= 0.28 && lingerSec >= 15 {
		return true, "检测到人员在停放车辆周边持续逗留异常"
	}
	return false, ""
}

// 2. 老人意外摔倒检测 (family_fall_down)
// 正常直立人像宽深比约为 0.35 ~ 0.55，意外跌倒卧地后宽深比突变且 > 1.15，且下边缘贴近地面 (y2 > 0.55)
func DetectHumanFallDown(personBox []float64) (bool, float64) {
	if len(personBox) < 4 {
		return false, 0
	}
	w := personBox[2] - personBox[0]
	h := personBox[3] - personBox[1]
	if h <= 0.001 {
		return false, 0
	}
	aspectRatio := w / h
	// 宽深比 > 1.15 且位置偏画面下半部 (y2 >= 0.55)
	if aspectRatio >= 1.15 && personBox[3] >= 0.55 {
		return true, aspectRatio
	}
	return false, aspectRatio
}

// 3. 儿童危险区域靠近 (child_danger_zone)
// 儿童身高相对低矮 (h < 0.42 画面高度)，且进入设定的 ROI 区域
func DetectChildDangerZone(personBox []float64, dangerROI []float64) bool {
	if len(personBox) < 4 || len(dangerROI) < 4 {
		return false
	}
	h := personBox[3] - personBox[1]
	isChild := h < 0.42

	// 踩脚点
	cx := (personBox[0] + personBox[2]) / 2.0
	cy := personBox[3]
	inROI := cx >= dangerROI[0] && cx <= dangerROI[2] && cy >= dangerROI[1] && cy <= dangerROI[3]

	return isChild && inROI
}

// 4. 包裹被取走与疑似被盗关联 (pkg_taken_theft)
// 包裹原本存在，当前帧包裹消失，而现场有人员目标
func DetectPackageTheftCorrelation(hadPackage bool, curHasPackage bool, curHasPerson bool) (bool, string) {
	if hadPackage && !curHasPackage && curHasPerson {
		return true, "门口包裹被移走，现场检测到人员活动"
	}
	return false, ""
}

// 5. 外来违停占道超时 (car_illegal_park)
func DetectOvertimeParking(parkDurationSec int, maxAllowedSec int) (bool, string) {
	if maxAllowedSec <= 0 {
		maxAllowedSec = 600 // 默认 10 分钟
	}
	if parkDurationSec >= maxAllowedSec {
		return true, "车辆在通道停放已超过设定时限"
	}
	return false, ""
}

// 6. 云端多模态大模型 (Deepseek-V4.1-flash) 场景化专属 Prompt 定制
// 根据不同触发场景注入针对性视觉核验重点，极大提升意图判断精准度
func ScenarioPromptForVLM(scId string, baseDesc string) string {
	switch scId {
	case "car_loiter_guard":
		return "【重点研判：车旁异常徘徊与防盗】请仔细观察画面中人员与停放车辆的交互动作：是否有拉车门、试探把手、透过车窗向内窥探、蹲下观察底盘或疑似刮蹭行为？请明确给出结论并描述具体动作。"
	case "night_suspicious":
		return "【重点研判：夜间高危窥探与门前异常】请核实夜间人员外貌特征与行为：是否有遮面、戴帽压低帽檐、撬锁、试探门把手或携带撬棍工具的可疑举动？判断安全威胁等级（高危/一般访客/正常活动）。"
	case "pkg_taken_theft":
		return "【重点研判：包裹取走与防盗甄别】请判定人员是在正常取件还是抱走快递？如果是陌生人员私自取走包裹请发出警报，描述人员衣着体貌特征。"
	case "family_fall_down":
		return "【重点研判：老人意外跌倒快速核验】请仔细确认地面上的人员姿态：是处于痛苦倒地、无力起身、求救状态，还是正常的日常活动（如擦地、整理地垫）？此为紧急生命安全研判，请立刻给出明确判断！"
	case "gesture_sos":
		return "【重点研判：镜头前挥手求救】请分析人物手臂与身体姿势：是否正面向摄像头大幅度连续挥手求助？有无身体不适或紧急求救特征？"
	case "hazard_fire_smoke":
		return "【重点研判：明火与浓烟险情复核】请核实视野内是否存在明火火苗闪烁、反常浓烟扩散、燃烧气雾或大面积高温红外反光？排除车灯反光或阳光眩光。"
	default:
		if baseDesc != "" {
			return "请详细分析事件画面内容：" + baseDesc
		}
		return "请详细分析画面中人员、车辆、物品的动作与安全状况。"
	}
}
