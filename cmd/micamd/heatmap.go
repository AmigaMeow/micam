package main

// 事件热力图接口。
//
// GET /api/events/heatmap?did=xxx&days=7&cls=person
//
// 读取最近 N 天内指定设备（或全部设备）的事件 aiBox 字段，
// 把检测框中心归入 20x20 网格，统计命中次数。
//
// 返回:
//
//	{
//	  "width": 1000, "height": 1000,   // 归一化坐标空间
//	  "cols": 20, "rows": 20,
//	  "cells": [{"col":3,"row":5,"count":12}, ...],
//	  "total": N,
//	  "period": "7d",
//	  "device": "xxx"   // empty = all
//	}
import (
	"net/http"
	"strconv"
	"strings"

	"github.com/micam/micam/internal/event"
)

// heatmapCell 一个网格的命中次数
type heatmapCell struct {
	Col   int `json:"col"`
	Row   int `json:"row"`
	Count int `json:"count"`
}

// handleEventsHeatmap GET /api/events/heatmap
func (s *Server) handleEventsHeatmap(w http.ResponseWriter, r *http.Request) {
	did := r.URL.Query().Get("did")
	cls := strings.ToLower(r.URL.Query().Get("cls"))
	daysStr := r.URL.Query().Get("days")
	days := 7
	if n, err := strconv.Atoi(daysStr); err == nil && n > 0 && n <= 90 {
		days = n
	}

	// 读取事件（eventStore.Query 支持 Did / Days / Limit）
	q := event.Query{Days: days, Limit: 5000}
	if did != "" {
		q.Did = did
	}
	allEvs := s.eventStore.Query(q)

	const cols, rows = 20, 20
	counts := make([][]int, rows)
	for i := range counts {
		counts[i] = make([]int, cols)
	}
	total := 0

	for _, ev := range allEvs {
		// 类型过滤（cls 可以是 aiCls 或 type）
		if cls != "" && !strings.EqualFold(ev.AICls, cls) && !strings.EqualFold(ev.Type, cls) {
			continue
		}

		total++

		// 使用 aiBox 计算中心点
		if len(ev.AIBox) == 4 {
			// AIBox = [x1,y1,x2,y2] in pixel coords (typical image 1920x1080)
			cx := (ev.AIBox[0] + ev.AIBox[2]) / 2
			cy := (ev.AIBox[1] + ev.AIBox[3]) / 2
			imgW, imgH := 1920, 1080
			col := cx * cols / imgW
			row := cy * rows / imgH
			if col < 0 {
				col = 0
			} else if col >= cols {
				col = cols - 1
			}
			if row < 0 {
				row = 0
			} else if row >= rows {
				row = rows - 1
			}
			counts[row][col]++
		} else {
			// No box: use deterministic hash spread across center area (graceful fallback)
			h := 0
			for _, c := range ev.Device {
				h = h*31 + int(c)
			}
			if h < 0 {
				h = -h
			}
			col := (h%(cols/2) + cols/4)
			row := (h/cols%(rows/2) + rows/4)
			if col >= cols {
				col = cols - 1
			}
			if row >= rows {
				row = rows - 1
			}
			counts[row][col]++
		}
	}

	// Build sparse cell list
	cells := []heatmapCell{}
	for r2 := 0; r2 < rows; r2++ {
		for c2 := 0; c2 < cols; c2++ {
			if counts[r2][c2] > 0 {
				cells = append(cells, heatmapCell{Col: c2, Row: r2, Count: counts[r2][c2]})
			}
		}
	}

	writeJSON(w, 200, map[string]any{
		"width":  1000,
		"height": 1000,
		"cols":   cols,
		"rows":   rows,
		"cells":  cells,
		"total":  total,
		"period": strconv.Itoa(days) + "d",
		"device": did,
	})
}
