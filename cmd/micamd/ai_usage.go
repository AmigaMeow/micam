package main

// AI 用量统计（对齐效果图 P0-2 的「用量与成本」区块）。
//
// 返回：
//   今日 4 个数字（本地分析 / 云端分析 / 上传图片 / 预估费用）
//   近 7 天趋势（本地 vs 云端）
//   预算设置回显

import (
	"net/http"
	"strconv"
	"time"

	"github.com/micam/micam/internal/event"
)

// aiUsageResp 用量统计
type aiUsageResp struct {
	Today struct {
		LocalAnalyzed int     `json:"localAnalyzed"`
		CloudAnalyzed int     `json:"cloudAnalyzed"`
		ImagesUploaded int    `json:"imagesUploaded"`
		CostCNY       float64 `json:"costCny"`
		UpdatedAt     string  `json:"updatedAt"`
	} `json:"today"`
	Trend []struct {
		Date  string  `json:"date"`
		Label string  `json:"label"`
		Local int     `json:"local"`
		Cloud int     `json:"cloud"`
		Cost  float64 `json:"cost"`
	} `json:"trend"`
	Budget struct {
		DailyCNY float64 `json:"dailyCny"`
		Action   string  `json:"action"`
	} `json:"budget"`
}

// handleAIUsage 用量统计。
func (s *Server) handleAIUsage(w http.ResponseWriter, r *http.Request) {
	var out aiUsageResp

	// 近 7 天
	for i := 6; i >= 0; i-- {
		day := time.Now().AddDate(0, 0, -i)
		dayStr := day.Format("2006-01-02")

		// 从事件库统计（按天）
		list := s.eventStore.Query(event.Query{Days: i + 1, Limit: 20000})
		var local, cloud int
		for _, e := range list {
			if len(e.Time) < 10 || e.Time[:10] != dayStr {
				continue
			}
			if e.AICls != "" {
				local++
			}
			if e.AISummary != "" {
				cloud++
			}
		}

		// 费用：本地免费，云端按图片数估算
		// （真正的记账在 Provider 调用时做，这里给趋势用的估算）
		cost := 0.0
		if cloud > 0 {
			cost = float64(cloud) * 0.00042 * 7.2 // 约 4 帧/事件
		}

		entry := struct {
			Date  string  `json:"date"`
			Label string  `json:"label"`
			Local int     `json:"local"`
			Cloud int     `json:"cloud"`
			Cost  float64 `json:"cost"`
		}{
			Date:  dayStr,
			Label: strconv.Itoa(int(day.Month())) + "/" + strconv.Itoa(day.Day()),
			Local: local,
			Cloud: cloud,
			Cost:  cost,
		}
		out.Trend = append(out.Trend, entry)

		if i == 0 {
			out.Today.LocalAnalyzed = local
			out.Today.CloudAnalyzed = cloud
			out.Today.ImagesUploaded = cloud * 4
			out.Today.CostCNY = s.ai.costToday()
			if out.Today.CostCNY == 0 {
				out.Today.CostCNY = cost
			}
		}
	}

	out.Today.UpdatedAt = time.Now().Format("15:04")

	cf := s.ai.getConfig()
	out.Budget.DailyCNY = cf.DailyBudgetCNY
	out.Budget.Action = cf.BudgetAction
	if out.Budget.Action == "" {
		out.Budget.Action = "alert"
	}

	writeJSON(w, 200, out)
}
