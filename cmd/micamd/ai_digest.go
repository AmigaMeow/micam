package main

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/micam/micam/internal/event"
	"github.com/micam/micam/internal/notify"
)

// ai_digest.go
// 24 小时安防早晚报与日程摘要智能生成引擎

type AIDailyDigest struct {
	Date             string   `json:"date"`
	VehicleArrivals  int      `json:"vehicleArrivals"`
	PersonVisits     int      `json:"personVisits"`
	PackageEvents    int      `json:"packageEvents"`
	SuspiciousAlerts int      `json:"suspiciousAlerts"`
	Summary          string   `json:"summary"`
	KeyEvents        []string `json:"keyEvents"`
	GeneratedAt      string   `json:"generatedAt"`
}

// GenerateDailyDigest 聚合全天事件生成智能早晚报
func GenerateDailyDigest(events []*event.Event, date string) *AIDailyDigest {
	if date == "" {
		date = time.Now().Format("2006-01-02")
	}

	digest := &AIDailyDigest{
		Date:        date,
		KeyEvents:   make([]string, 0),
		GeneratedAt: time.Now().Format("2006-01-02 15:04:05"),
	}

	// 车辆聚类与统计
	type vehicleCluster struct {
		canonicalPlate string
		isWhitelist    bool
		whitelistName  string
		count          int
	}
	clusters := make(map[string]*vehicleCluster)
	clusterOrder := make([]string, 0)

	for _, ev := range events {
		if ev == nil {
			continue
		}
		// 只看目标日期的事件
		if len(ev.Time) >= 10 && ev.Time[:10] != date {
			continue
		}

		timeStr := "未知时间"
		if len(ev.Time) >= 16 {
			timeStr = ev.Time[11:16]
		}

		switch ev.Type {
		case "vehicle":
			digest.VehicleArrivals++
			plate := ev.AIPlate
			if plate == "" && strings.Contains(ev.AITitle, "[") && strings.Contains(ev.AITitle, "]") {
				s := ev.AITitle[strings.Index(ev.AITitle, "[")+1 : strings.Index(ev.AITitle, "]")]
				plate = s
			}
			norm := normalizePlate(plate)
			if norm != "" {
				// 1. 优先白名单匹配
				var matchedWl *WhitelistItem
				if gWhitelistStore != nil {
					matchedWl = gWhitelistStore.MatchPlate(norm)
				}

				if matchedWl != nil {
					canon := matchedWl.Value
					if c, ok := clusters[canon]; ok {
						c.count++
					} else {
						clusters[canon] = &vehicleCluster{
							canonicalPlate: canon,
							isWhitelist:    true,
							whitelistName:  matchedWl.Name,
							count:          1,
						}
						clusterOrder = append(clusterOrder, canon)
						digest.KeyEvents = append(digest.KeyEvents, fmt.Sprintf("%s 家庭自用车 [%s] 驶入庭院", timeStr, canon))
					}
				} else {
					// 2. 外部访客车辆模糊归并
					foundCluster := false
					for _, existingKey := range clusterOrder {
						c := clusters[existingKey]
						if !c.isWhitelist && isPlateFuzzyMatch(norm, existingKey) {
							c.count++
							foundCluster = true
							break
						}
					}
					if !foundCluster {
						clusters[norm] = &vehicleCluster{
							canonicalPlate: plate,
							isWhitelist:    false,
							count:          1,
						}
						clusterOrder = append(clusterOrder, norm)
						digest.KeyEvents = append(digest.KeyEvents, fmt.Sprintf("%s 访客车辆 [%s] 驶入停放", timeStr, plate))
					}
				}
			} else {
				if len(clusters) == 0 && len(digest.KeyEvents) == 0 {
					digest.KeyEvents = append(digest.KeyEvents, fmt.Sprintf("%s 车辆驶入停放", timeStr))
				}
			}

		case "person":
			digest.PersonVisits++
			if ev.Duration >= 20 {
				digest.SuspiciousAlerts++
				digest.KeyEvents = append(digest.KeyEvents, fmt.Sprintf("%s 门前人员持续逗留 (%d秒)", timeStr, ev.Duration))
			}
		case "package":
			digest.PackageEvents++
			digest.KeyEvents = append(digest.KeyEvents, fmt.Sprintf("%s 门口检测到快递/包裹送达", timeStr))
		case "animal":
			// 萌宠日常活动记录
		}
	}

	// 生成清晰、高级、无冗余噪声的自然语言安防简报
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("【%s 安防日程简报】", date))

	if digest.VehicleArrivals > 0 {
		var vehicleDescs []string
		for _, key := range clusterOrder {
			c := clusters[key]
			if c.isWhitelist {
				name := c.whitelistName
				if name == "" {
					name = "家庭自用车"
				}
				vehicleDescs = append(vehicleDescs, fmt.Sprintf("%s %s（%d 次，已免打扰）", name, c.canonicalPlate, c.count))
			} else {
				vehicleDescs = append(vehicleDescs, fmt.Sprintf("访客车 %s（%d 次）", c.canonicalPlate, c.count))
			}
		}

		if len(vehicleDescs) > 0 {
			if len(vehicleDescs) > 2 {
				sb.WriteString(fmt.Sprintf("今日共记录车辆出入 %d 次，主要为 %s 等。", digest.VehicleArrivals, strings.Join(vehicleDescs[:2], "、")))
			} else {
				sb.WriteString(fmt.Sprintf("今日共记录车辆出入 %d 次，主要为 %s。", digest.VehicleArrivals, strings.Join(vehicleDescs, "、")))
			}
		} else {
			sb.WriteString(fmt.Sprintf("今日共记录车辆出入停放 %d 次。", digest.VehicleArrivals))
		}
	} else {
		sb.WriteString("今日院落车道无异常车辆进出。")
	}

	if digest.PersonVisits > 0 {
		sb.WriteString(fmt.Sprintf("检测到人员动态 %d 次", digest.PersonVisits))
		if digest.SuspiciousAlerts > 0 {
			sb.WriteString(fmt.Sprintf("（其中 %d 次在门前逗留超过 20 秒需关注）", digest.SuspiciousAlerts))
		}
		sb.WriteString("。")
	} else {
		sb.WriteString("门前人员动态正常。")
	}

	if digest.PackageEvents > 0 {
		sb.WriteString(fmt.Sprintf("门口共感知快递包裹送达 %d 件，请及时查收。", digest.PackageEvents))
	}

	if digest.SuspiciousAlerts == 0 {
		sb.WriteString("全天无周界翻越与安全警报，整体环境平稳安全。")
	}

	digest.Summary = sb.String()

	if len(digest.KeyEvents) > 8 {
		digest.KeyEvents = digest.KeyEvents[:8]
	}

	return digest
}

// handleAIDigest 处理 /api/ai/digest 请求
func (s *Server) handleAIDigest(w http.ResponseWriter, r *http.Request) {
	if gWhitelistStore == nil {
		initWhitelistStore(s.dataDir)
	}

	date := r.URL.Query().Get("date")
	if date == "" {
		date = time.Now().Format("2006-01-02")
	}

	days := 1
	if date != time.Now().Format("2006-01-02") {
		days = 7
	}
	rawEvents := s.eventStore.Query(event.Query{
		Days:  days,
		Limit: 2000,
	})

	var evs []*event.Event
	for i := range rawEvents {
		e := rawEvents[i]
		if len(e.Time) >= 10 && e.Time[:10] == date {
			evs = append(evs, &e)
		}
	}

	sort.Slice(evs, func(i, j int) bool {
		return evs[i].Time < evs[j].Time
	})

	digest := GenerateDailyDigest(evs, date)
	writeJSON(w, 200, map[string]any{
		"ok":     true,
		"digest": digest,
	})
}

// sendDailyDigestNotification 汇总指定日期的事件并推送安防日报
func (s *Server) sendDailyDigestNotification(date string) (*AIDailyDigest, error) {
	if gWhitelistStore == nil {
		initWhitelistStore(s.dataDir)
	}

	if date == "" {
		date = time.Now().Format("2006-01-02")
	}

	days := 1
	if date != time.Now().Format("2006-01-02") {
		days = 7
	}
	rawEvents := s.eventStore.Query(event.Query{
		Days:  days,
		Limit: 2000,
	})

	var evs []*event.Event
	for i := range rawEvents {
		e := rawEvents[i]
		if len(e.Time) >= 10 && e.Time[:10] == date {
			evs = append(evs, &e)
		}
	}

	sort.Slice(evs, func(i, j int) bool {
		return evs[i].Time < evs[j].Time
	})

	digest := GenerateDailyDigest(evs, date)

	// 构建推送消息正文（富文本多行摘要）
	var msg strings.Builder
	msg.WriteString(digest.Summary)
	if len(digest.KeyEvents) > 0 {
		msg.WriteString("\n\n【重点事件追踪】\n")
		for i, ke := range digest.KeyEvents {
			msg.WriteString(fmt.Sprintf("%d. %s\n", i+1, ke))
		}
	}

	timeStr := time.Now().Format("2006-01-02 15:04:05")
	if s.notify != nil {
		s.notify.Send(notify.Event{
			Type:    notify.EventAIDigest,
			Level:   "info",
			Title:   fmt.Sprintf("【AI安防日报】%s 安全简报", digest.Date),
			Message: msg.String(),
			Time:    timeStr,
			Data: map[string]any{
				"date":             digest.Date,
				"vehicleArrivals":  digest.VehicleArrivals,
				"personVisits":     digest.PersonVisits,
				"packageEvents":    digest.PackageEvents,
				"suspiciousAlerts": digest.SuspiciousAlerts,
				"keyEvents":        digest.KeyEvents,
			},
		}, 0)
	}

	logf("[ai-digest] 已成功生成并投递 %s 安防日报 (出入车辆:%d, 人员活动:%d, 徘徊警报:%d)",
		date, digest.VehicleArrivals, digest.PersonVisits, digest.SuspiciousAlerts)

	return digest, nil
}

// startDailyDigestScheduler 启动每日安防日报定时调度器（每天早晨 08:00 自动推送）
func (s *Server) startDailyDigestScheduler() {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		var lastTriggerDate string
		for range ticker.C {
			now := time.Now()
			// 每天早晨 08:00 自动推送前一天的安防日报
			if now.Hour() == 8 && now.Minute() == 0 {
				todayStr := now.Format("2006-01-02")
				if lastTriggerDate != todayStr {
					lastTriggerDate = todayStr
					targetDate := now.AddDate(0, 0, -1).Format("2006-01-02")
					logf("[ai-digest] 触发早间 08:00 定时安防日报调度，目标统计日期: %s", targetDate)
					_, _ = s.sendDailyDigestNotification(targetDate)
				}
			}
		}
	}()
}

// handleAIDigestTrigger 处理手动触发安防日报推送 /api/ai/digest/trigger
func (s *Server) handleAIDigestTrigger(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		writeErr(w, 405, "只支持 GET/POST")
		return
	}
	date := r.URL.Query().Get("date")
	if date == "" {
		date = time.Now().Format("2006-01-02")
	}

	digest, err := s.sendDailyDigestNotification(date)
	if err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	writeJSON(w, 200, map[string]any{
		"ok":     true,
		"sent":   true,
		"digest": digest,
	})
}
