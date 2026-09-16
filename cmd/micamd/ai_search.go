package main

// AI 自然语言搜索。
//
// ===== 设计要点 =====
//
// **不依赖外部 LLM 也能用**：
//   没配 LLM 时用本地规则解析（时间词 + 类型词 + 设备词），
//   然后按解析结果过滤事件库。
//
// 配了 LLM 后（未来）：
//   把同样的问题交给 LLM 解析成结构化查询，再用同一套过滤。
//   所以这个接口的**输入输出契约不变**，只是解析器换了。
//
// 关键原则：**绝不让 LLM 直接扫视频** —— 它只解析查询，不碰画面。

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/micam/micam/internal/event"
)

// 型号片段（CW500 / CW300 / 029a02 之类）
var reModel = regexp.MustCompile("(?i)(cw[0-9]+|[0-9]{3}[a-z][0-9]{2})")

// aiQuery 解析后的结构化查询
type aiQuery struct {
	Days      int      `json:"days"`
	Types     []string `json:"types"`
	Device    string   `json:"device,omitempty"`
	HourFrom  int      `json:"hourFrom"`
	HourTo    int      `json:"hourTo"`
	Parsed    string   `json:"parsed"`
	MatchedBy string   `json:"matchedBy"`
}

// parseNaturalQuery 本地规则解析自然语言。
//
// 支持的说法（中文为主）：
//   时间：今天 / 昨天 / 前天 / 最近 N 天 / 这周 / 本周 / 昨晚 / 今早
//   类型：人 / 有人 / 人员 / 车 / 车辆 / 汽车 / 包裹 / 快递 / 移动 / 动静
//   设备：直接匹配设备名或别名里的子串
//   时段：早上 / 上午 / 中午 / 下午 / 晚上 / 凌晨 / 半夜
func parseNaturalQuery(q string, devNames []string, aliasOf func(string) string) aiQuery {
	r := aiQuery{Days: 1, HourFrom: 0, HourTo: 23, MatchedBy: "本地规则"}
	s := strings.ToLower(q)

	// ---- 时间范围 ----
	switch {
	case strings.Contains(s, "今天") || strings.Contains(s, "今日"):
		r.Days = 1
	case strings.Contains(s, "昨天") || strings.Contains(s, "昨日") || strings.Contains(s, "昨晚"):
		r.Days = 2
	case strings.Contains(s, "前天"):
		r.Days = 3
	case strings.Contains(s, "这周") || strings.Contains(s, "本周") || strings.Contains(s, "最近一周") || strings.Contains(s, "7天") || strings.Contains(s, "七天"):
		r.Days = 7
	case strings.Contains(s, "这个月") || strings.Contains(s, "本月") || strings.Contains(s, "30天"):
		r.Days = 30
	case strings.Contains(s, "最近三天") || strings.Contains(s, "3天"):
		r.Days = 3
	}
	// 「最近 N 天」
	for i := 1; i <= 60; i++ {
		if strings.Contains(s, strconv.Itoa(i)+"天") {
			r.Days = i
			break
		}
	}
	// 只说「最近」而没给天数 -> 默认 7 天
	if r.Days == 1 && (strings.Contains(s, "最近") || strings.Contains(s, "近期")) {
		r.Days = 7
	}

	// ---- 时段 ----
	switch {
	case strings.Contains(s, "凌晨") || strings.Contains(s, "半夜") || strings.Contains(s, "深夜"):
		r.HourFrom, r.HourTo = 0, 6
	case strings.Contains(s, "早上") || strings.Contains(s, "早晨") || strings.Contains(s, "今早"):
		r.HourFrom, r.HourTo = 6, 10
	case strings.Contains(s, "上午"):
		r.HourFrom, r.HourTo = 6, 12
	case strings.Contains(s, "中午"):
		r.HourFrom, r.HourTo = 11, 14
	case strings.Contains(s, "下午"):
		r.HourFrom, r.HourTo = 12, 18
	case strings.Contains(s, "晚上") || strings.Contains(s, "傍晚"):
		r.HourFrom, r.HourTo = 18, 24
	}
	// 「昨晚」同时含「晚」和「昨天」——上面已处理天，这里补时段
	if strings.Contains(s, "昨晚") {
		r.HourFrom, r.HourTo = 18, 6 // 跨夜，过滤时特殊处理
	}

	// ---- 类型 ----
	add := func(t string) {
		for _, x := range r.Types {
			if x == t {
				return
			}
		}
		r.Types = append(r.Types, t)
	}
	if strings.Contains(s, "人") || strings.Contains(s, "人员") || strings.Contains(s, "访客") {
		add("person")
	}
	if strings.Contains(s, "车") || strings.Contains(s, "汽车") || strings.Contains(s, "车辆") {
		add("vehicle")
	}
	if strings.Contains(s, "包裹") || strings.Contains(s, "快递") || strings.Contains(s, "箱子") {
		add("package")
	}
	if strings.Contains(s, "移动") || strings.Contains(s, "动静") || strings.Contains(s, "变化") {
		add("motion")
	}
	if strings.Contains(s, "离线") {
		add("offline")
	}
	if strings.Contains(s, "上线") || strings.Contains(s, "恢复") {
		add("online")
	}

	// ---- 设备 ----
	//
	// 用户通常说「门口」「后院」，而设备名是
	// 「小米室外摄像机CW500双摄版」这种长名。
	// 所以除了全名，还要匹配：别名、型号关键词、以及用户给设备起的短名。
	for _, n := range devNames {
		if n == "" {
			continue
		}
		sn := strings.ToLower(n)
		cands := []string{n}
		// 去掉常见前缀后缀
		cands = append(cands, strings.TrimPrefix(n, "小米"))
		cands = append(cands, strings.TrimPrefix(n, "小米室外摄像机"))
		cands = append(cands, strings.TrimPrefix(n, "小米室内摄像机"))
		// 提取型号片段（CW500 / CW300 / 029a02）
		if m := reModel.FindString(sn); m != "" {
			cands = append(cands, m)
		}
		// 别名（用户设置的）
		if aliasOf != nil {
			if al := aliasOf(n); al != "" {
				cands = append(cands, al)
			}
		}
		for _, piece := range cands {
			piece = strings.TrimSpace(piece)
			if len([]rune(piece)) >= 2 && strings.Contains(s, strings.ToLower(piece)) {
				r.Device = n
				break
			}
		}
		if r.Device != "" {
			break
		}
	}

	// ---- 人话回显 ----
	var parts []string
	dayWord := map[int]string{1: "今天", 2: "昨天", 3: "前天", 7: "最近 7 天", 30: "最近 30 天"}[r.Days]
	if dayWord == "" {
		dayWord = "最近 " + strconv.Itoa(r.Days) + " 天"
	}
	parts = append(parts, dayWord)
	if len(r.Types) > 0 {
		var tn []string
		for _, t := range r.Types {
			switch t {
			case "person":
				tn = append(tn, "有人")
			case "vehicle":
				tn = append(tn, "车辆")
			case "package":
				tn = append(tn, "包裹")
			case "motion":
				tn = append(tn, "移动")
			case "offline":
				tn = append(tn, "离线")
			case "online":
				tn = append(tn, "上线")
			}
		}
		parts = append(parts, strings.Join(tn, "/"))
	}
	if r.Device != "" {
		parts = append(parts, r.Device)
	}
	if r.HourFrom != 0 || r.HourTo != 23 {
		if r.HourFrom == 18 && r.HourTo == 6 {
			parts = append(parts, "夜间")
		} else {
			parts = append(parts, strconv.Itoa(r.HourFrom)+"~"+strconv.Itoa(r.HourTo)+" 点")
		}
	}
	r.Parsed = "理解为：" + strings.Join(parts, " · ")
	return r
}

// handleAISearch 自然语言搜索事件。
func (s *Server) handleAISearch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Q string `json:"q"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Q) == "" {
		writeErr(w, 400, "缺少 q")
		return
	}

	// 拿设备名做匹配
	var devNames []string
	for _, d := range s.allCameras() {
		devNames = append(devNames, d.Name)
	}

	parsed := parseNaturalQuery(req.Q, devNames, s.meta.GetByDevName)

	// 查事件库（放大 days 以覆盖「昨晚」这类跨天）
	list := s.eventStore.Query(event.Query{Days: parsed.Days + 1, Limit: 2000})

	var hit []event.Event
	for _, e := range list {
		// 类型过滤
		if len(parsed.Types) > 0 {
			ok := false
			for _, t := range parsed.Types {
				if e.Type == t {
					ok = true
					break
				}
			}
			if !ok {
				continue
			}
		}
		// 设备过滤
		if parsed.Device != "" && e.Device != parsed.Device {
			continue
		}
		// 时段过滤
		if len(e.Time) >= 13 {
			if h, err := strconv.Atoi(e.Time[11:13]); err == nil {
				if parsed.HourFrom <= parsed.HourTo {
					if h < parsed.HourFrom || h > parsed.HourTo {
						continue
					}
				} else {
					// 跨夜（如 18~6）
					if h < parsed.HourFrom && h > parsed.HourTo {
						continue
					}
				}
			}
		}
		hit = append(hit, e)
	}

	// 「昨天」这种要排除今天的数据
	if parsed.Days == 2 {
		y := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
		var filtered []event.Event
		for _, e := range hit {
			if strings.HasPrefix(e.Time, y) {
				filtered = append(filtered, e)
			}
		}
		hit = filtered
	}

	writeJSON(w, 200, map[string]any{
		"q":        req.Q,
		"parsed":   parsed.Parsed,
		"query":    parsed,
		"events":   hit,
		"count":    len(hit),
		"provider": "local",
	})
}
