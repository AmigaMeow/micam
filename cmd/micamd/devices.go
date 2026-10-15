package main

// 设备与家庭 API。
//
// 关键：区分设备类型
//   - 摄像头 -> cs2 P2P（视频/云台）
//   - 其他米家设备 -> MIoT（属性读写）
//
// 家庭列表包含共享家庭，共享设备必须用 home_device_list 才能拿到。

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/micam/micam/internal/xiaomi"
)

// handleAllDevices 返回所有设备（含非摄像头）。
//
// GET /api/devices/all
func (s *Server) handleAllDevices(w http.ResponseWriter, r *http.Request) {
	sessions := s.store.AllSessions()
	if len(sessions) == 0 {
		writeJSON(w, 200, map[string]any{"devices": []any{}, "needLogin": true})
		return
	}

	type devInfo struct {
		Did      string `json:"did"`
		Name     string `json:"name"`
		Model    string `json:"model"`
		Type     string `json:"type"`
		IsCamera bool   `json:"isCamera"`
		IsOnline bool   `json:"isOnline"`
		LocalIP  string `json:"localip,omitempty"`
		Shared   bool   `json:"shared"`
		Permit   int    `json:"permitLevel"`
		UID      string `json:"uid,omitempty"`
	}

	var out []devInfo
	seen := map[string]bool{}

	for _, sess := range sessions {
		myID := sess.UserID

		// 自有设备
		if devs, err := sess.Devices(); err == nil {
			for _, d := range devs {
				if seen[d.Did] {
					continue
				}
				seen[d.Did] = true
				out = append(out, devInfo{
					Did:      d.Did,
					Name:     d.Name,
					Model:    d.Model,
					Type:     d.DeviceType(),
					IsCamera: d.IsCamera(),
					IsOnline: d.IsOnline,
					LocalIP:  d.LocalIP,
					Shared:   false,
					Permit:   d.Permit,
				})
			}
		}

		// 共享家庭的设备
		if homes, err := sess.Homes(); err == nil {
			for _, h := range homes {
				if h.ShareFlag == 0 {
					continue
				}
				devs, err := sess.SharedDevices(h.ID, h.OwnerID())
				if err != nil {
					continue
				}
				for _, d := range devs {
					if seen[d.Did] {
						continue
					}
					seen[d.Did] = true
					uid := ""
					if s, ok := d.UID.(string); ok {
						uid = s
					}
					out = append(out, devInfo{
						Did:      d.Did,
						Name:     d.Name,
						Model:    d.Model,
						Type:     d.DeviceType(),
						IsCamera: d.IsCamera(),
						IsOnline: d.IsOnline,
						LocalIP:  d.LocalIP,
						Shared:   uid != myID,
						Permit:   d.Permit,
						UID:      uid,
					})
				}
			}
		}
	}

	// 按类型排序：摄像头优先
	cams := []devInfo{}
	others := []devInfo{}
	for _, d := range out {
		if d.IsCamera {
			cams = append(cams, d)
		} else {
			others = append(others, d)
		}
	}
	all := append(cams, others...)

	writeJSON(w, 200, map[string]any{
		"devices":     all,
		"needLogin":   false,
		"totalCount":  len(all),
		"cameraCount": len(cams),
	})
}

// handleHomes 返回家庭列表（含共享家庭及成员数）。
//
// GET /api/homes
func (s *Server) handleHomes(w http.ResponseWriter, r *http.Request) {
	sessions := s.store.AllSessions()
	if len(sessions) == 0 {
		writeJSON(w, 200, map[string]any{"homes": []any{}, "needLogin": true})
		return
	}

	type homeInfo struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		OwnerID   string `json:"ownerId"`
		Shared    bool   `json:"shared"`
		DeviceNum int    `json:"deviceCount"`
	}

	var out []homeInfo
	for _, sess := range sessions {
		homes, err := sess.Homes()
		if err != nil {
			continue
		}
		for _, h := range homes {
			hi := homeInfo{
				ID:      h.ID,
				Name:    h.Name,
				OwnerID: h.OwnerID(),
				Shared:  h.ShareFlag != 0,
			}
			// 统计设备数
			if h.ShareFlag != 0 {
				if devs, err := sess.SharedDevices(h.ID, h.OwnerID()); err == nil {
					hi.DeviceNum = len(devs)
				}
			} else {
				if devs, err := sess.Devices(); err == nil {
					hi.DeviceNum = len(devs)
				}
			}
			out = append(out, hi)
		}
	}

	writeJSON(w, 200, map[string]any{"homes": out, "needLogin": false})
}

var _ = json.Marshal
var _ = strings.Contains
var _ = xiaomi.Device{}

// handleDevicesRTSP 管理通用 RTSP 摄像机流接入（类似 Clearcam）
// GET  /api/devices/rtsp         列出所有添加的 RTSP 摄像机
// POST /api/devices/rtsp/add     添加 RTSP 摄像机 {"name":"...", "url":"rtsp://...", "localIp":"..."}
// POST /api/devices/rtsp/remove  删除 RTSP 摄像机 {"did":"rtsp_..."}
func (s *Server) handleDevicesRTSP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		cached := s.store.Devices()
		var out []map[string]any
		for _, d := range cached {
			if d.Model == "generic.camera.rtsp" || strings.HasPrefix(d.Did, "rtsp_") {
				src, _ := s.store.Source(d.Did)
				out = append(out, map[string]any{
					"did":      d.Did,
					"name":     d.Name,
					"model":    d.Model,
					"url":      src,
					"isOnline": d.IsOnline,
					"localIp":  d.LocalIP,
					"managed":  s.meta.IsManaged(d.Did),
				})
			}
		}
		writeJSON(w, 200, map[string]any{"ok": true, "cameras": out})
		return
	}

	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 GET/POST")
		return
	}

	action := r.URL.Query().Get("action")
	var req struct {
		Action  string `json:"action"`
		Did     string `json:"did"`
		Name    string `json:"name"`
		URL     string `json:"url"`
		LocalIP string `json:"localIp"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Action != "" {
		action = req.Action
	}

	if action == "remove" || action == "delete" || strings.HasSuffix(r.URL.Path, "/remove") {
		if req.Did == "" {
			writeErr(w, 400, "缺少 did")
			return
		}
		_ = s.meta.SetManaged(req.Did, false)
		s.store.RemoveDevice(req.Did)
		logf("[rtsp] 成功移除通用 RTSP 摄像机: %s", req.Did)
		writeJSON(w, 200, map[string]any{"ok": true, "removed": req.Did})
		return
	}

	// 默认添加
	rawURL := strings.TrimSpace(req.URL)
	if rawURL == "" || (!strings.HasPrefix(rawURL, "rtsp://") && !strings.HasPrefix(rawURL, "rtsps://")) {
		writeErr(w, 400, "请提供合法的 RTSP 视频流地址 (以 rtsp:// 开头)")
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "RTSP 摄像机"
	}

	did := req.Did
	if did == "" {
		hash := fmt.Sprintf("%x", sha256.Sum256([]byte(rawURL)))
		did = "rtsp_" + hash[:8]
	}

	dev := xiaomi.Device{
		Did:      did,
		Name:     name,
		Model:    "generic.camera.rtsp",
		IsOnline: true,
		LocalIP:  req.LocalIP,
	}

	s.store.CacheDevices([]xiaomi.Device{dev})
	s.store.SaveSource(did, rawURL)
	_ = s.meta.SetManaged(did, true)
	logf("[rtsp] 成功接入通用 RTSP 摄像机: %s (%s) -> %s", name, did, rawURL)

	writeJSON(w, 200, map[string]any{
		"ok":      true,
		"did":     did,
		"name":    name,
		"model":   dev.Model,
		"url":     rawURL,
		"managed": true,
	})
}
