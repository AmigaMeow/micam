package main

// 设备控制 API。
//
// ===== 关键设计 =====
//
// 摄像头和其他米家设备走不同通道：
//
//   摄像头 (.camera.)  -> cs2 P2P（视频/云台），MIoT 不可用
//   其他设备           -> 小米云 MIoT，可用
//
// 属性 ID 从 miot-spec 自动推导（型号决定），并缓存。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/micam/micam/internal/xiaomi"
)

// findDevice 在所有设备里找 did（不限摄像头）。
func (s *Server) findDevice(did string) (*xiaomi.Device, error) {
	// 先查摄像头缓存
	if d, ok := s.store.Device(did); ok {
		return d, nil
	}
	// 从账号里找
	for _, sess := range s.store.AllSessions() {
		if devs, err := sess.Devices(); err == nil {
			for i := range devs {
				if devs[i].Did == did {
					return &devs[i], nil
				}
			}
		}
		// 共享设备
		if homes, err := sess.Homes(); err == nil {
			for _, h := range homes {
				if h.ShareFlag == 0 {
					continue
				}
				if devs, err := sess.SharedDevices(h.ID, h.OwnerID()); err == nil {
					for i := range devs {
						if devs[i].Did == did {
							return &devs[i], nil
						}
					}
				}
			}
		}
	}
	return nil, fmt.Errorf("设备不存在: %s", did)
}

// handleDeviceControls 返回设备的所有可控制项（从 spec 推导）。
//
// GET /api/device/controls?did=xxx
func (s *Server) handleDeviceControls(w http.ResponseWriter, r *http.Request) {
	did := r.URL.Query().Get("did")
	if did == "" {
		writeErr(w, 400, "缺少 did")
		return
	}

	dev, err := s.findDevice(did)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}

	// 摄像头：MIoT 不可用，返回云台信息
	if dev.IsCamera() {
		writeJSON(w, 200, map[string]any{
			"did":       dev.Did,
			"name":      dev.Name,
			"model":     dev.Model,
			"isCamera":  true,
			"channel":   "p2p",
			"available": []any{},
			"msg":       "摄像头控制走 cs2 P2P 协议，请用 /api/ptz/* 接口",
		})
		return
	}

	sess := s.store.SessionForDevice(did)
	if sess == nil {
		writeErr(w, 400, "未登录")
		return
	}

	// 从 spec 拿属性表
	spec, err := xiaomi.GetSpec(dev.Model)
	if err != nil {
		// spec 拿不到时，退回常见属性探测
		out := s.probeCommonProps(dev)
		writeJSON(w, 200, map[string]any{
			"did":       dev.Did,
			"name":      dev.Name,
			"model":     dev.Model,
			"isCamera":  false,
			"channel":   "miot",
			"available": out,
			"msg":       "spec 查询失败，已用常见属性探测",
		})
		return
	}

	// 组装可读写属性
	type item struct {
		Key     string         `json:"key"`
		Label   string         `json:"label"`
		Group   string         `json:"group"`
		Siid    int            `json:"siid"`
		Piid    int            `json:"piid"`
		Type    string         `json:"type"`
		CanWrite bool          `json:"canWrite"`
		Options []map[string]any `json:"options,omitempty"`
		Range   []int          `json:"range,omitempty"`
		Unit    string         `json:"unit,omitempty"`
		Value   any            `json:"value"`
		OK      bool           `json:"ok"`
	}

	var items []item
	var props []xiaomi.MIoTProp

	for _, svc := range spec.Services {
		if svc.IID == 1 {
			continue // 跳过设备信息
		}
		group := svc.Description
		if group == "" {
			group = fmt.Sprintf("服务 %d", svc.IID)
		}
		for _, p := range svc.Properties {
			if !p.CanRead() {
				continue
			}
			// 跳过只读的（除了开关类）
			it := item{
				Key:      fmt.Sprintf("s%dp%d", svc.IID, p.IID),
				Label:    p.Description,
				Group:    group,
				Siid:     svc.IID,
				Piid:     p.IID,
				Type:     p.Format,
				CanWrite: p.CanWrite(),
				Unit:     p.Unit,
			}
			for _, vl := range p.ValueList {
				it.Options = append(it.Options, map[string]any{
					"value": vl.Value,
					"label": vl.Description,
				})
			}
			if len(p.ValueRange) >= 2 {
				it.Range = []int{p.ValueRange[0], p.ValueRange[1]}
			}
			items = append(items, it)
			props = append(props, xiaomi.MIoTProp{Siid: svc.IID, Piid: p.IID})
		}
	}

	// 批量读当前值
	if len(props) > 0 {
		res, err := sess.GetProps(did, props)
		if err == nil {
			for i := range items {
				for _, x := range res {
					if x.Siid == items[i].Siid && x.Piid == items[i].Piid {
						items[i].OK = x.Code == 0
						if items[i].OK && len(x.Value) > 0 {
							var v any
							_ = json.Unmarshal(x.Value, &v)
							items[i].Value = v
						}
						break
					}
				}
			}
		}
	}

	// 只留有值的
	var avail []item
	for _, it := range items {
		if it.OK {
			avail = append(avail, it)
		}
	}

	writeJSON(w, 200, map[string]any{
		"did":       dev.Did,
		"name":      dev.Name,
		"model":     dev.Model,
		"isCamera":  false,
		"channel":   "miot",
		"available": avail,
	})
}

// probeCommonProps 常见属性探测（spec 不可用时）。
func (s *Server) probeCommonProps(dev *xiaomi.Device) []any {
	sess := s.store.SessionForDevice(dev.Did)
	if sess == nil {
		return []any{}
	}

	type probe struct {
		Label string
		Siid  int
		Piid  int
	}
	probes := []probe{
		{"开关", 2, 1},
		{"模式", 2, 2},
		{"温度", 2, 3},
		{"风速", 2, 4},
		{"亮度", 2, 2},
		{"色温", 2, 3},
		{"音量", 2, 1},
		{"湿度", 2, 2},
		{"电量", 2, 1},
	}

	var out []any
	for _, p := range probes {
		res, err := sess.GetProps(dev.Did, []xiaomi.MIoTProp{{Siid: p.Siid, Piid: p.Piid}})
		if err != nil || len(res) == 0 || res[0].Code != 0 {
			continue
		}
		var v any
		_ = json.Unmarshal(res[0].Value, &v)
		out = append(out, map[string]any{
			"key":   fmt.Sprintf("s%dp%d", p.Siid, p.Piid),
			"label": p.Label,
			"siid":  p.Siid,
			"piid":  p.Piid,
			"value": v,
			"ok":    true,
		})
	}
	return out
}

// handleDeviceControl 设置属性。
//
// POST /api/device/control  {"did":"xxx","siid":2,"piid":1,"value":true}
func (s *Server) handleDeviceControl(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 POST")
		return
	}

	var req struct {
		Did   string `json:"did"`
		Siid  int    `json:"siid"`
		Piid  int    `json:"piid"`
		Value any    `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "参数错误")
		return
	}
	if req.Did == "" {
		writeErr(w, 400, "缺少 did")
		return
	}

	dev, err := s.findDevice(req.Did)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	if dev.IsCamera() {
		writeErr(w, 400, "摄像头不支持 MIoT 控制，请用 /api/ptz/* 接口")
		return
	}

	sessions := s.store.AllSessions()
	if len(sessions) == 0 {
		writeErr(w, 400, "未登录")
		return
	}

	sess := s.store.SessionForDevice(req.Did)
	if sess == nil {
		writeErr(w, 400, "未登录")
		return
	}
	if err := sess.SetProp(req.Did, req.Siid, req.Piid, req.Value); err != nil {
		writeErr(w, 502, "设置失败: "+err.Error())
		return
	}

	logf("[control] %s (%s) siid=%d piid=%d value=%v", dev.Name, req.Did, req.Siid, req.Piid, req.Value)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleDeviceAction 调用动作。
func (s *Server) handleDeviceAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 POST")
		return
	}

	var req struct {
		Did  string `json:"did"`
		Siid int    `json:"siid"`
		Aiid int    `json:"aiid"`
		In   []any  `json:"in"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "参数错误")
		return
	}

	sessions := s.store.AllSessions()
	if len(sessions) == 0 {
		writeErr(w, 400, "未登录")
		return
	}

	sess := s.store.SessionForDevice(req.Did)
	if sess == nil {
		writeErr(w, 400, "未登录")
		return
	}
	out, err := sess.CallAction(req.Did, req.Siid, req.Aiid, req.In)
	if err != nil {
		writeErr(w, 502, "动作失败: "+err.Error())
		return
	}

	logf("[control] action %s siid=%d aiid=%d", req.Did, req.Siid, req.Aiid)
	writeJSON(w, 200, map[string]any{"ok": true, "result": string(out)})
}

// handleControlScan 扫描设备支持的属性（调试用）。
func (s *Server) handleControlScan(w http.ResponseWriter, r *http.Request) {
	did := r.URL.Query().Get("did")
	if did == "" {
		writeErr(w, 400, "缺少 did")
		return
	}
	siidMax := 8
	piidMax := 6
	fmt.Sscanf(r.URL.Query().Get("siidMax"), "%d", &siidMax)
	fmt.Sscanf(r.URL.Query().Get("piidMax"), "%d", &piidMax)
	if siidMax > 20 {
		siidMax = 20
	}
	if piidMax > 20 {
		piidMax = 20
	}

	sess := s.store.SessionForDevice(did)
	if sess == nil {
		writeErr(w, 400, "未登录")
		return
	}

	type found struct {
		Siid  int    `json:"siid"`
		Piid  int    `json:"piid"`
		Value any    `json:"value"`
	}
	var results []found

	for siid := 1; siid <= siidMax; siid++ {
		var props []xiaomi.MIoTProp
		for piid := 1; piid <= piidMax; piid++ {
			props = append(props, xiaomi.MIoTProp{Siid: siid, Piid: piid})
		}
		res, err := sess.GetProps(did, props)
		if err != nil {
			continue
		}
		for _, x := range res {
			if x.Code == 0 && len(x.Value) > 0 {
				var v any
				_ = json.Unmarshal(x.Value, &v)
				results = append(results, found{Siid: x.Siid, Piid: x.Piid, Value: v})
			}
		}
	}

	writeJSON(w, 200, map[string]any{"did": did, "found": results, "count": len(results)})
}

var _ = strings.Contains
