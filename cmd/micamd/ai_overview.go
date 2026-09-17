package main

// AI 概览聚合接口。
//
// 一次返回概览 Tab 需要的全部数据，减少往返：
//   统计（人员/车辆/包裹/需关注）
//   处理情况（本地/云端/已过滤/费用/环比）
//   最近事件（含 AI 摘要）

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/micam/micam/internal/event"
)

// aiOverviewResp 概览数据
type aiOverviewResp struct {
	Today     aiTodayStat   `json:"today"`
	Process   aiProcessStat `json:"process"`
	Recent    []event.Event `json:"recent"`
	Summary   string        `json:"summary"`
	SummaryAt string        `json:"summaryAt"`
	HasLLM    bool          `json:"hasLLM"`
}

type aiTodayStat struct {
	Person  int `json:"person"`
	Vehicle int `json:"vehicle"`
	Package int `json:"package"`
	Watch   int `json:"watch"` // 需关注（规则判定）
	Motion  int `json:"motion"`
	Total   int `json:"total"`
}

type aiProcessStat struct {
	LocalProcessed int     `json:"localProcessed"`
	CloudAnalyzed  int     `json:"cloudAnalyzed"`
	Filtered       int     `json:"filtered"`
	CostCNY        float64 `json:"costCny"`
	Percent        float64 `json:"percent"`  // 环形图百分比
	DeltaPct       float64 `json:"deltaPct"` // 较昨日变化
	HasCloud       bool    `json:"hasCloud"`
}

// handleAIOverview 概览聚合。
func (s *Server) handleAIOverview(w http.ResponseWriter, r *http.Request) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 {
		days = 1
	}

	list := s.eventStore.Query(event.Query{Days: days, Limit: 5000})

	var st aiTodayStat
	var watch int
	for _, e := range list {
		switch e.Type {
		case "person":
			st.Person++
		case "vehicle":
			st.Vehicle++
		case "package":
			st.Package++
		case "motion":
			st.Motion++
		}
		// 「需关注」规则（本地判定，不依赖 LLM）：
		//   夜间（22:00-06:00）的人员活动
		//   停留超过 30 秒
		//   包裹送达（本身就是关注点）
		if e.Type == "person" && isNightTime(e.Time) {
			watch++
		} else if e.Duration >= 30 && e.Type == "person" {
			watch++
		} else if e.Type == "package" {
			watch++
		}
	}
	st.Watch = watch
	st.Total = len(list)

	// 处理情况
	var localProcessed, cloudAnalyzed int
	for _, e := range list {
		if e.AICls != "" {
			localProcessed++ // 本地 AI 出过结果的
		}
		if e.AISummary != "" {
			cloudAnalyzed++ // 云端分析过的
		}
	}
	filtered := localProcessed - cloudAnalyzed
	if filtered < 0 {
		filtered = 0
	}
	pct := 0.0
	if st.Total > 0 {
		pct = float64(localProcessed) / float64(st.Total) * 100
	}

	// 环比（今天 vs 昨天）
	var delta float64
	if days == 1 {
		yList := s.eventStore.Query(event.Query{Days: 2, Limit: 5000})
		var yCount int
		y := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
		for _, e := range yList {
			if strings.HasPrefix(e.Time, y) {
				yCount++
			}
		}
		if yCount > 0 {
			delta = (float64(st.Total) - float64(yCount)) / float64(yCount) * 100
		}
	}

	// 最近事件（前 4 条有缩略图的）
	var recent []event.Event
	for _, e := range list {
		if e.Thumb != "" {
			recent = append(recent, e)
		}
		if len(recent) >= 4 {
			break
		}
	}

	// 摘要（无 LLM 时用模板拼）
	summary := buildLocalSummary(st)

	writeJSON(w, 200, aiOverviewResp{
		Today: st,
		Process: aiProcessStat{
			LocalProcessed: localProcessed,
			CloudAnalyzed:  cloudAnalyzed,
			Filtered:       filtered,
			CostCNY:        s.ai.costToday(),
			Percent:        pct,
			DeltaPct:       delta,
			HasCloud:       s.ai.hasProvider(),
		},
		Recent:  recent,
		Summary: summary,
		HasLLM:  s.ai.hasProvider(),
	})
}

// isNightTime 判断是否夜间（22:00-06:00）
func isNightTime(t string) bool {
	if len(t) < 13 {
		return false
	}
	h, err := strconv.Atoi(t[11:13])
	if err != nil {
		return false
	}
	return h >= 22 || h < 6
}

// buildLocalSummary 无 LLM 时用模板拼一句话。
//
// 有 LLM 后这里会换成真正的自然语言总结。
func buildLocalSummary(st aiTodayStat) string {
	if st.Total == 0 {
		return "今天还没有检测到事件。"
	}
	// 注意标点：首句用句号，后续用逗号连接，末尾统一加句号
	var head = "今天检测到 " + strconv.Itoa(st.Total) + " 个有效事件"
	var parts []string
	if st.Person > 0 {
		parts = append(parts, strconv.Itoa(st.Person)+" 次人员活动")
	}
	if st.Vehicle > 0 {
		parts = append(parts, strconv.Itoa(st.Vehicle)+" 次车辆活动")
	}
	if st.Package > 0 {
		parts = append(parts, strconv.Itoa(st.Package)+" 个包裹送达")
	}
	if st.Watch > 0 {
		parts = append(parts, strconv.Itoa(st.Watch)+" 个事件需要关注")
	}
	if len(parts) == 0 {
		return head + "。"
	}
	return head + "，" + strings.Join(parts, "，") + "。"
}
