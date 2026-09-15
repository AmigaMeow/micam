package main

// 事件通知 API 与事件源。

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/micam/micam/internal/notify"
)

// handleNotifyConfig 读写通知配置。
//
// GET  /api/notify/config
// POST /api/notify/config  {enabled, webhook, events}
func (s *Server) handleNotifyConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		c := s.notify.GetConfig()
		writeJSON(w, 200, map[string]any{
			"enabled": c.Enabled,
			"webhook": c.Webhook,
			"events":  c.Events,
			"types":   notify.AllEventTypes,
		})
		return

	case http.MethodPost:
		var req struct {
			Enabled *bool     `json:"enabled"`
			Webhook *string   `json:"webhook"`
			Events  *[]string `json:"events"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, "参数错误")
			return
		}

		if err := s.cfg.SetNotify(req.Enabled, req.Webhook, req.Events); err != nil {
			writeErr(w, 500, "保存失败: "+err.Error())
			return
		}

		c := s.cfg
		s.notify.SetConfig(notify.Config{
			Enabled: c.NotifyEnabled,
			Webhook: c.NotifyWebhook,
			Events:  c.NotifyEvents,
		})

		logf("[notify] 配置已更新: enabled=%v events=%d", c.NotifyEnabled, len(c.NotifyEvents))
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

// handleNotifyTest 测试 Webhook。
//
// POST /api/notify/test  {webhook}
func (s *Server) handleNotifyTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 POST")
		return
	}

	var req struct {
		Webhook string `json:"webhook"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	url := req.Webhook
	if url == "" {
		url = s.notify.GetConfig().Webhook
	}

	if err := s.notify.TestWebhook(url); err != nil {
		writeErr(w, 502, err.Error())
		return
	}

	// 同时记入站内事件（方便前端看到效果）
	s.notify.Send(notify.Event{
		Type:    "test",
		Level:   "info",
		Title:   "测试通知已发送",
		Message: "Webhook: " + url,
	}, 0)

	writeJSON(w, 200, map[string]any{"ok": true, "msg": "测试消息已发送"})
}

// ---- 事件源 ----

// startDeviceWatcher 监控设备在线状态变化，发通知。
func (s *Server) startDeviceWatcher() {
	go func() {
		time.Sleep(30 * time.Second)

		state := map[string]bool{}   // did -> 上次已知在线状态
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
		Type:  notify.EventDiskLow,
		Level: "error",
		Title: "磁盘空间不足",
		Message: "剩余 " + fmtGB(freeGB) + "，已低于阈值 " + fmtGB(threshold) + "，旧录像将被清理",
		Data: map[string]any{
			"freeGB":    freeGB,
			"threshold": threshold,
		},
	}, 6*time.Hour)   // 6 小时内不重复
}

func fmtGB(v float64) string {
	return strconv.FormatFloat(v, 'f', 1, 64) + " GB"
}
