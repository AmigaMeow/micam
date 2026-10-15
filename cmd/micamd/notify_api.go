package main

// 事件通知 API 与事件源（全渠道推送增强版）。

import (
	"fmt"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/micam/micam/internal/event"
	"github.com/micam/micam/internal/notify"
)

// handleNotifyConfig 读写通知配置。
//
// GET  /api/notify/config
// POST /api/notify/config  {enabled, channel, webhook, params, events, quietFrom, quietTo}
func (s *Server) handleNotifyConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		c := s.notify.GetConfig()
		cfg := s.cfg
		channel := cfg.NotifyChannel
		if channel == "" {
			channel = c.Channel
		}
		if channel == "" {
			channel = "bark"
		}
		params := cfg.NotifyParams
		if params == nil {
			params = c.Params
		}
		if params == nil {
			params = make(map[string]string)
		}

		writeJSON(w, 200, map[string]any{
			"enabled":   cfg.NotifyEnabled,
			"channel":   channel,
			"webhook":   cfg.NotifyWebhook,
			"params":    params,
			"events":    cfg.NotifyEvents,
			"quietFrom": cfg.UI.QuietFrom,
			"quietTo":   cfg.UI.QuietTo,
			"types":     notify.AllEventTypes,
			"channels":  notify.AvailableChannels,
		})
		return

	case http.MethodPost:
		var req struct {
			Enabled   *bool             `json:"enabled"`
			Channel   *string           `json:"channel"`
			Webhook   *string           `json:"webhook"`
			Params    map[string]string `json:"params"`
			Events    *[]string         `json:"events"`
			QuietFrom *string           `json:"quietFrom"`
			QuietTo   *string           `json:"quietTo"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, "参数错误")
			return
		}

		if req.QuietFrom != nil || req.QuietTo != nil {
			u := s.cfg.UI
			if req.QuietFrom != nil {
				u.QuietFrom = *req.QuietFrom
			}
			if req.QuietTo != nil {
				u.QuietTo = *req.QuietTo
			}
			_ = s.cfg.SetUI(u)
		}

		if err := s.cfg.SetNotify(req.Enabled, req.Channel, req.Webhook, req.Params, req.Events); err != nil {
			writeErr(w, 500, "保存失败: "+err.Error())
			return
		}

		c := s.cfg
		ch := c.NotifyChannel
		if ch == "" {
			ch = "bark"
		}
		s.notify.SetConfig(notify.Config{
			Enabled:   c.NotifyEnabled,
			Channel:   ch,
			Webhook:   c.NotifyWebhook,
			Params:    c.NotifyParams,
			Events:    c.NotifyEvents,
			QuietFrom: c.UI.QuietFrom,
			QuietTo:   c.UI.QuietTo,
		})

		logf("[notify] 配置已更新: enabled=%v channel=%s events=%d", c.NotifyEnabled, ch, len(c.NotifyEvents))
		writeJSON(w, 200, map[string]any{"ok": true})
		return

	default:
		writeErr(w, 405, "方法不允许")
	}
}

// handleNotifyEvents 最近事件。
//
// GET  /api/notify/events?limit=50
// POST /api/notify/events/clear
func (s *Server) handleNotifyEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.notify.ClearEvents()
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 50
	}
	writeJSON(w, 200, map[string]any{"events": s.notify.Events(limit)})
}

// handleNotifyTest 测试推送通道。
//
// POST /api/notify/test  {channel?, webhook?, params?}
func (s *Server) handleNotifyTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 POST")
		return
	}

	var req struct {
		Channel string            `json:"channel"`
		Webhook string            `json:"webhook"`
		Params  map[string]string `json:"params"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	cfg := s.notify.GetConfig()
	channel := req.Channel
	if channel == "" {
		channel = cfg.Channel
	}
	if channel == "" {
		channel = s.cfg.NotifyChannel
	}
	if channel == "" {
		channel = "webhook"
	}

	webhook := req.Webhook
	if webhook == "" {
		webhook = cfg.Webhook
	}
	if webhook == "" {
		webhook = s.cfg.NotifyWebhook
	}

	params := req.Params
	if len(params) == 0 {
		params = cfg.Params
	}
	if len(params) == 0 {
		params = s.cfg.NotifyParams
	}

	ms, err := s.notify.TestChannel(channel, webhook, params)
	if err != nil {
		writeJSON(w, 200, map[string]any{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}

	// 同时记入站内事件（方便前端看到效果）
	s.notify.Send(notify.Event{
		Type:    "test",
		Level:   "info",
		Title:   "测试通知发送成功",
		Message: "通道 [" + channel + "] 测试通过，延迟 " + strconv.Itoa(ms) + " ms",
	}, 0)

	writeJSON(w, 200, map[string]any{
		"ok":      true,
		"ms":      ms,
		"channel": channel,
		"msg":     "测试推送已成功送达（耗时 " + strconv.Itoa(ms) + " ms）",
	})
}

// dispatchAINotify 在 AI 分析完成后根据订阅与重要程度智能派发通知。
func (s *Server) dispatchAINotify(e *event.Event, res *aiAnalysis) {
	if s.notify == nil || res == nil {
		return
	}

	title := res.Title
	if title == "" {
		title = e.Device + " 检测到事件"
	}
	summary := res.Summary
	if summary == "" {
		summary = e.Desc
	}
	timeStr := time.Now().Format("2006-01-02 15:04:05")

	baseData := map[string]any{
		"did":        e.Did,
		"device":     e.Device,
		"event_id":   e.ID,
		"importance": res.Importance,
		"tags":       res.Tags,
		"plate":      res.Plate,
		"cls":        e.AICls,
		"snapshot":   fmt.Sprintf("/api/events/thumb?id=%s", e.ID),
		"time":       timeStr,
	}

	// 1. 高危安全预警 (摔倒/险情/SOS/夜间入侵等)
	isDanger := res.Importance >= 0.7
	tagsStr := strings.Join(res.Tags, " ") + " " + summary + " " + title
	if strings.Contains(tagsStr, "摔倒") || strings.Contains(tagsStr, "倒地") ||
		strings.Contains(tagsStr, "求救") || strings.Contains(tagsStr, "呼救") ||
		strings.Contains(tagsStr, "烟") || strings.Contains(tagsStr, "火") ||
		strings.Contains(tagsStr, "可疑") || strings.Contains(tagsStr, "翻越") ||
		strings.Contains(tagsStr, "入侵") {
		isDanger = true
	}

	if isDanger {
		d := make(map[string]any)
		for k, v := range baseData { d[k] = v }
		d["alert_level"] = "high"
		s.notify.Send(notify.Event{
			Type:    notify.EventAIAlert,
			Level:   "error",
			Title:   "【高危预警】" + title,
			Message: summary + "（摄像头：" + e.Device + "）",
			Time:    timeStr,
			Data:    d,
		}, 3*time.Minute)
		return
	}

	// 2. 车辆与车牌事件
	if e.Type == "vehicle" || res.Plate != "" {
		pTitle := "车辆动态: " + title
		if res.Plate != "" {
			pTitle = "发现车辆 [" + res.Plate + "]: " + title
		}
		d := make(map[string]any)
		for k, v := range baseData { d[k] = v }
		s.notify.Send(notify.Event{
			Type:    notify.EventAIVehicle,
			Level:   "info",
			Title:   pTitle,
			Message: summary + "（" + e.Device + "）",
			Time:    timeStr,
			Data:    d,
		}, 3*time.Minute)
		return
	}

	// 3. 包裹事件
	if e.Type == "package" || strings.Contains(tagsStr, "包裹") || strings.Contains(tagsStr, "快递") {
		d := make(map[string]any)
		for k, v := range baseData { d[k] = v }
		s.notify.Send(notify.Event{
			Type:    notify.EventAIPackage,
			Level:   "info",
			Title:   "包裹动态: " + title,
			Message: summary + "（" + e.Device + "）",
			Time:    timeStr,
			Data:    d,
		}, 5*time.Minute)
		return
	}

	// 4. 人员常规动态
	if e.Type == "person" {
		d := make(map[string]any)
		for k, v := range baseData { d[k] = v }
		s.notify.Send(notify.Event{
			Type:    notify.EventAIPerson,
			Level:   "info",
			Title:   "人员活动: " + title,
			Message: summary + "（" + e.Device + "）",
			Time:    timeStr,
			Data:    d,
		}, 5*time.Minute)
		return
	}
}

// ---- 事件源 ----

// startDeviceWatcher 监控设备在线状态变化，发通知。
func (s *Server) startDeviceWatcher() {
	go func() {
		time.Sleep(30 * time.Second)

		state := map[string]bool{} // did -> 上次已知在线状态
		first := true

		t := time.NewTicker(60 * time.Second)
		defer t.Stop()

		for {
			s.checkDeviceStates(state, &first)
			<-t.C
		}
	}()
}

func (s *Server) checkDeviceStates(state map[string]bool, first *bool) {
	for _, d := range s.allCameras() {
		prev, known := state[d.Did]
		state[d.Did] = d.IsOnline

		if !known {
			continue // 首次只记录，不发通知
		}
		if prev == d.IsOnline {
			continue
		}

		if d.IsOnline {
			s.notify.Send(notify.Event{
				Type:    notify.EventDeviceOnline,
				Level:   "info",
				Title:   "摄像头上线",
				Message: d.Name + " 已恢复在线",
				Data:    map[string]any{"did": d.Did, "name": d.Name},
			}, 5*time.Minute)
		} else {
			s.notify.Send(notify.Event{
				Type:    notify.EventDeviceOffline,
				Level:   "warn",
				Title:   "摄像头离线",
				Message: d.Name + " 已离线",
				Data:    map[string]any{"did": d.Did, "name": d.Name},
			}, 5*time.Minute)
		}
	}
	*first = false
}

// notifyDiskLow 磁盘空间不足通知（供存储守护调用）。
func (s *Server) notifyDiskLow(freeGB, threshold float64) {
	s.notify.Send(notify.Event{
		Type:    notify.EventDiskLow,
		Level:   "error",
		Title:   "磁盘空间不足",
		Message: "剩余 " + fmtGB(freeGB) + "，已低于阈值 " + fmtGB(threshold) + "，旧录像将被清理",
		Data: map[string]any{
			"freeGB":    freeGB,
			"threshold": threshold,
		},
	}, 6*time.Hour) // 6 小时内不重复
}

func fmtGB(v float64) string {
	return strconv.FormatFloat(v, 'f', 1, 64) + " GB"
}
