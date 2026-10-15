package main

// 界面偏好配置接口（设置页「通用」Tab）。
//
//	GET  /api/ui/config    读全部界面偏好 + 其他 Tab 的摘要
//	POST /api/ui/config    写（只覆盖请求里出现的字段）

import (
	"encoding/json"
	"net/http"

	"github.com/micam/micam/internal/config"
)

// handleUIConfig 读写界面偏好。
func (s *Server) handleUIConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		ai := s.cfg.AI
		writeJSON(w, 200, map[string]any{
			"config": s.cfg.UI,
			// 右侧摘要卡用的数据
			"summary": map[string]any{
				"recording": map[string]any{
					"schedule": s.cfg.UI.RecordSchedule,
					"keepDays": s.cfg.RecordKeepDays,
					"loop":     s.cfg.UI.LoopOverwrite,
					"enabled":  s.cfg.RecordEnabled,
				},
				"notify": map[string]any{
					"quietFrom": s.cfg.UI.QuietFrom,
					"quietTo":   s.cfg.UI.QuietTo,
					"push":      s.cfg.UI.PushEnabled,
					"email":     s.cfg.UI.EmailEnabled,
					"enabled":   s.cfg.NotifyEnabled,
					"webhook":   s.cfg.NotifyWebhook != "",
				},
				"system": map[string]any{
					"autoUpdate": s.cfg.UI.AutoUpdate,
					"aiEnabled":  ai.Enabled,
					"aiMode":     ai.Mode,
					"aiReady":    s.ai != nil && s.ai.Status()["status"] == "ready",
				},
			},
		})
		return
	}

	var req map[string]any
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "参数错误")
		return
	}

	cur := s.cfg.UI
	str := func(k string, dst *string) {
		if v, ok := req[k].(string); ok && v != "" {
			*dst = v
		}
	}
	bl := func(k string, dst *bool) {
		if v, ok := req[k].(bool); ok {
			*dst = v
		}
	}

	str("theme", &cur.Theme)
	str("language", &cur.Language)
	str("timeFormat", &cur.TimeFormat)
	str("defaultQuality", &cur.DefaultQuality)
	str("defaultProtocol", &cur.DefaultProtocol)
	bl("autoplayLive", &cur.AutoplayLive)
	bl("autoplayMute", &cur.AutoplayMute)
	bl("homeShowCameras", &cur.HomeShowCameras)
	bl("homeShowEvents", &cur.HomeShowEvents)
	bl("homeShowStorage", &cur.HomeShowStorage)
	bl("showEnglishAlias", &cur.ShowEnglishAlias)
	bl("compactMode", &cur.CompactMode)
	// 存储页底部状态栏是否显示（用指针：默认显示 = true，零值 false 无法表达"没设置过"）
	if v, ok := req["showStoragePaths"].(bool); ok {
		cur.ShowStoragePaths = &v
	}
	str("eventsDefaultView", &cur.EventsDefaultView)
	str("recordSchedule", &cur.RecordSchedule)
	bl("loopOverwrite", &cur.LoopOverwrite)
	str("quietFrom", &cur.QuietFrom)
	str("quietTo", &cur.QuietTo)
	bl("pushEnabled", &cur.PushEnabled)
	bl("emailEnabled", &cur.EmailEnabled)
	bl("autoUpdate", &cur.AutoUpdate)

	if err := s.cfg.SetUI(cur); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}

	// 录像相关的几项同步到录像配置
	if err := s.syncUItoRecording(cur, req); err != nil {
		writeJSON(w, 200, map[string]any{"ok": true, "config": cur, "warn": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "config": cur})
}

// syncUItoRecording 把界面里的录像设置同步到录像配置
func (s *Server) syncUItoRecording(ui config.UIConfig, req map[string]any) error {
	changed := false
	if v, ok := req["keepDays"].(float64); ok && int(v) > 0 {
		s.cfg.RecordKeepDays = int(v)
		changed = true
	}
	if !changed {
		return nil
	}
	return s.cfg.SaveSelf()
}
