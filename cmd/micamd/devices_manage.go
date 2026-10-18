package main

// 设备「纳入管理 / 移出管理」接口。
//
// ===== 账号↔设备模型（2026-09-18 设计确认）=====
//
//	云端设备（米家的）  --获取设备(只读)-->  清单（展示给用户）
//	清单  --用户勾选-->  已纳入管理（插件真正管理的摄像头）
//
// 这三个状态必须分开：
//	  · 「获取设备」只是拉清单，不改任何管理状态
//	  · 「纳入管理」是用户的决定
//	  · 「移出管理」不影响米家，也不删录像/事件
//
//	POST /api/devices/manage   {"dids":["..."], "managed":true}
//	  => 批量纳入 / 移出
//
//	GET  /api/devices/cloud?refresh=1
//	  => 云端清单（含每台的 managed 状态），供账号页右栏使用

import (
	"encoding/json"
	"net/http"
	"strings"
)

// handleDeviceManage 批量纳入 / 移出管理。
func (s *Server) handleDeviceManage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 POST")
		return
	}
	var req struct {
		Dids    []string `json:"dids"`
		Managed bool     `json:"managed"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Dids) == 0 {
		writeErr(w, 400, "缺少 dids")
		return
	}

	action := "移出管理"
	if req.Managed {
		action = "纳入管理"
	}
	okCount := 0
	for _, did := range req.Dids {
		if err := s.meta.SetManaged(did, req.Managed); err != nil {
			logf("[dev] %s %s 失败: %v", did, action, err)
			continue
		}
		okCount++
	}

	logf("[dev] %s %d 台设备", action, okCount)
	writeJSON(w, 200, map[string]any{
		"ok":      true,
		"managed": req.Managed,
		"count":   okCount,
	})
}

// handleDevicesCloud 云端设备清单（含 managed 状态）。
//
// 与 /api/devices 的区别：
//
//	· /api/devices      只返回【已纳入管理】的设备（各页面用）
//	· /api/devices/cloud 返回【全部云端设备】+ managed 标记（账号页用）
func (s *Server) handleDevicesCloud(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("refresh") == "1"

	// 走缓存（除非显式 refresh）
	if !force {
		cached := s.store.Devices()
		out := make([]map[string]any, 0, len(cached))
		for _, d := range cached {
			m := s.meta.Get(d.Did)
			out = append(out, map[string]any{
				"did": d.Did, "name": d.Name, "model": d.Model,
				"isOnline": d.IsOnline, "shared": d.Shared,
				"accountId": d.AccountID, "lenses": d.Lenses, "hasPTZ": IsPTZCapable(d.Model),
				"managed": s.meta.IsManaged(d.Did),
				"alias":   m.Alias,
			})
		}
		writeJSON(w, 200, map[string]any{
			"devices": out, "cached": true, "count": len(out),
			"fetched": len(out) > 0,
		})
		return
	}

	// 拉云端
	sessions := s.store.AllSessions()
	if len(sessions) == 0 {
		writeJSON(w, 200, map[string]any{"devices": []any{}, "needLogin": true})
		return
	}
	all := []any{}
	seen := map[string]bool{}
	for _, sess := range sessions {
		devs, err := sess.AllCameras()
		if err != nil {
			continue
		}
		for _, d := range devs {
			if seen[d.Did] {
				continue
			}
			seen[d.Did] = true
			d.AccountID = sess.UserID
			m := s.meta.Get(d.Did)
			all = append(all, map[string]any{
				"did": d.Did, "name": d.Name, "model": d.Model,
				"isOnline": d.IsOnline, "shared": d.Shared,
				"accountId": d.AccountID, "lenses": d.Lenses, "hasPTZ": IsPTZCapable(d.Model),
				"managed": s.meta.IsManaged(d.Did),
				"alias":   m.Alias,
			})
		}
	}

	// 保留已接入的通用 RTSP 摄像机，防止刷新时丢失
	for _, d := range s.store.Devices() {
		if d.Model == "generic.camera.rtsp" || strings.HasPrefix(d.Did, "rtsp_") {
			if !seen[d.Did] {
				seen[d.Did] = true
				m := s.meta.Get(d.Did)
				all = append(all, map[string]any{
					"did": d.Did, "name": d.Name, "model": d.Model,
					"isOnline": d.IsOnline, "shared": false,
					"accountId": d.AccountID, "lenses": 1, "hasPTZ": IsPTZCapable(d.Model),
					"managed": s.meta.IsManaged(d.Did),
					"alias":   m.Alias,
				})
			}
		}
	}

	// 写缓存（拉到的设备存起来，下次读缓存就不用再麻烦云端）
	for _, sess := range sessions {
		if devs, err := sess.AllCameras(); err == nil && len(devs) > 0 {
			s.store.CacheDevices(devs)
			break
		}
	}

	writeJSON(w, 200, map[string]any{
		"devices": all, "cached": false, "count": len(all), "fetched": true,
	})
}
