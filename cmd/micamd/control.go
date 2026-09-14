package main

// 设备控制 API（实验性）。
//
// ===== 实测结论（2026-09-15）=====
//
// 小米摄像头的控制【不走】小米云 MIoT 接口：
//
//   POST /miotspec/prop/get  返回 code=-704010000（属性不存在）
//   POST /miotspec/prop/set  同样被拒绝
//
// 即使 did 格式正确、属性 ID 来自官方 miot-spec，设备仍拒绝。
//
// 说明摄像头固件不实现标准 MIoT 属性表，
// 控制走 cs2 P2P 的私有协议：
//
//   0x100  auth      认证
//   0x102  video     视频流
//   0x112  motor     云台电机（已实现，见 ptz.go）
//   0x1001 encoded   编码包装
//
// 所以：
//   - 云台控制可用（ptz.go）
//   - 其他控制（夜视/水印/侦测）需要逆向更多 P2P 命令
//
// 本文件保留 MIoT 实现作为参考，接口标记为实验性。
//
// 接口：
//   GET  /api/device/controls?did=xxx    查询可用控制项
//   POST /api/device/control             设置属性
//   POST /api/device/action              调用动作

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/micam/micam/internal/xiaomi"
)

// ControlDef 控制项定义。
type ControlDef struct {
	Key     string   `json:"key"`
	Label   string   `json:"label"`
	Group   string   `json:"group"`
	Siid    int      `json:"siid"`
	Piid    int      `json:"piid"`
	Type    string   `json:"type"` // bool / int / enum
	Options []Option `json:"options,omitempty"`
	Hint    string   `json:"hint,omitempty"`
}

// Option 枚举选项。
type Option struct {
	Value int    `json:"value"`
	Label string `json:"label"`
}

// controlsForModel 按型号返回控制项（来自 miot-spec）。
//
// 实测数据：
//   mxiang.camera.moc001 (CW300)  - 16 个服务
//   isa.camera.hlmax     (CW500)  - 11 个服务
func controlsForModel(model string) []ControlDef {
	// 通用：摄像头控制（两台都有）
	common := []ControlDef{
		{Key: "switch", Label: "摄像头开关", Group: "基础", Siid: 2, Piid: 1, Type: "bool"},
		{Key: "nightShot", Label: "夜视模式", Group: "基础", Siid: 2, Piid: 3, Type: "enum",
			Options: []Option{{0, "自动"}, {1, "关闭"}, {2, "开启"}}},
		{Key: "watermark", Label: "时间水印", Group: "基础", Siid: 2, Piid: 4, Type: "bool"},
		{Key: "statusLight", Label: "指示灯", Group: "基础", Siid: 3, Piid: 1, Type: "bool"},
		{Key: "motionDetect", Label: "移动侦测", Group: "侦测", Siid: 5, Piid: 1, Type: "bool"},
		{Key: "sensitivity", Label: "侦测灵敏度", Group: "侦测", Siid: 5, Piid: 3, Type: "int",
			Hint: "0-100"},
	}

	// 按型号加特有项
	switch {
	case strings.Contains(model, "hlmax") || strings.Contains(model, "cw500"):
		// CW500 特有
		common = append(common,
			ControlDef{Key: "rollover", Label: "画面翻转", Group: "画面", Siid: 2, Piid: 2, Type: "enum",
				Options: []Option{{0, "正常"}, {1, "180°"}, {2, "90°"}, {3, "270°"}}},
			ControlDef{Key: "hdr", Label: "宽动态 HDR", Group: "画面", Siid: 2, Piid: 9, Type: "bool"},
			ControlDef{Key: "humanTrack", Label: "人形追踪", Group: "智能", Siid: 2, Piid: 10, Type: "bool"},
			ControlDef{Key: "motionTrack", Label: "移动追踪", Group: "智能", Siid: 2, Piid: 8, Type: "bool"},
			ControlDef{Key: "aiFrame", Label: "AI 画框", Group: "智能", Siid: 2, Piid: 11, Type: "bool"},
			ControlDef{Key: "distortion", Label: "畸变校正", Group: "画面", Siid: 2, Piid: 12, Type: "bool"},
			ControlDef{Key: "recMode", Label: "录像模式", Group: "录像", Siid: 2, Piid: 7, Type: "enum",
				Options: []Option{{0, "关闭"}, {1, "连续"}, {2, "事件"}}},
		)
	case strings.Contains(model, "moc001") || strings.Contains(model, "cw300"):
		// CW300 特有
		common = append(common,
			ControlDef{Key: "rollover", Label: "画面翻转", Group: "画面", Siid: 2, Piid: 4, Type: "enum",
				Options: []Option{{0, "正常"}, {1, "180°"}}},
			ControlDef{Key: "humanTrack", Label: "人形追踪", Group: "智能", Siid: 2, Piid: 9, Type: "bool"},
			ControlDef{Key: "aiFrame", Label: "AI 画框", Group: "智能", Siid: 2, Piid: 10, Type: "bool"},
			ControlDef{Key: "distortion", Label: "畸变校正", Group: "画面", Siid: 2, Piid: 14, Type: "bool"},
			ControlDef{Key: "recMode", Label: "录像模式", Group: "录像", Siid: 2, Piid: 11, Type: "enum",
				Options: []Option{{0, "关闭"}, {1, "连续"}, {2, "事件"}}},
			ControlDef{Key: "localStore", Label: "本地存储", Group: "录像", Siid: 2, Piid: 12, Type: "bool"},
			ControlDef{Key: "soundLightWarn", Label: "声光警告", Group: "警戒", Siid: 2, Piid: 15, Type: "bool"},
			ControlDef{Key: "manualWarn", Label: "手动警告", Group: "警戒", Siid: 2, Piid: 16, Type: "bool"},
			ControlDef{Key: "whiteLight", Label: "白光灯", Group: "警戒", Siid: 16, Piid: 1, Type: "bool"},
			ControlDef{Key: "powerFreq", Label: "电源频率", Group: "画面", Siid: 2, Piid: 18, Type: "enum",
				Options: []Option{{0, "50Hz"}, {1, "60Hz"}}},
		)
	}

	return common
}

// handleDeviceControls 查询设备的可用控制项。
//
// GET /api/device/controls?did=xxx
func (s *Server) handleDeviceControls(w http.ResponseWriter, r *http.Request) {
	did := r.URL.Query().Get("did")
	if did == "" {
		writeErr(w, 400, "缺少 did")
		return
	}

	dev, err := s.ensureDevice(did)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}

	sessions := s.store.AllSessions()
	if len(sessions) == 0 {
		writeErr(w, 400, "未登录")
		return
	}

	defs := controlsForModel(dev.Model)
	if len(defs) == 0 {
		writeJSON(w, 200, map[string]any{
			"did": dev.Did, "name": dev.Name, "model": dev.Model,
			"available": []any{}, "msg": "该型号暂不支持控制",
		})
		return
	}

	// 批量读当前值
	var props []xiaomi.MIoTProp
	for _, d := range defs {
		props = append(props, xiaomi.MIoTProp{Siid: d.Siid, Piid: d.Piid})
	}

	results, err := sessions[0].GetProps(did, props)
	if err != nil {
		writeErr(w, 502, "查询属性失败: "+err.Error())
		return
	}

	// 整理
	type item struct {
		ControlDef
		Value any  `json:"value"`
		OK    bool `json:"ok"`
	}
	var out []item
	for _, d := range defs {
		it := item{ControlDef: d}
		for _, res := range results {
			if res.Siid == d.Siid && res.Piid == d.Piid {
				it.OK = res.Code == 0
				if it.OK && len(res.Value) > 0 {
					var v any
					if json.Unmarshal(res.Value, &v) == nil {
						it.Value = v
					}
				}
				break
			}
		}
		// 只返回可用的
		if it.OK {
			out = append(out, it)
		}
	}

	writeJSON(w, 200, map[string]any{
		"did":       dev.Did,
		"name":      dev.Name,
		"model":     dev.Model,
		"available": out,
	})
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

	sessions := s.store.AllSessions()
	if len(sessions) == 0 {
		writeErr(w, 400, "未登录")
		return
	}

	if err := sessions[0].SetProp(req.Did, req.Siid, req.Piid, req.Value); err != nil {
		writeErr(w, 502, "设置失败: "+err.Error())
		return
	}

	logf("[control] %s siid=%d piid=%d value=%v", req.Did, req.Siid, req.Piid, req.Value)
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

	out, err := sessions[0].CallAction(req.Did, req.Siid, req.Aiid, req.In)
	if err != nil {
		writeErr(w, 502, "动作失败: "+err.Error())
		return
	}

	logf("[control] action %s siid=%d aiid=%d", req.Did, req.Siid, req.Aiid)
	writeJSON(w, 200, map[string]any{"ok": true, "result": string(out)})
}

// handleControlScan 扫描设备支持的 siid/piid（调试用）。
//
// GET /api/device/scan?did=xxx&siidMax=10&piidMax=10
//
// 用途：不同型号的属性 ID 不同，
// 用这个接口能找出设备真正支持的组合。
func (s *Server) handleControlScan(w http.ResponseWriter, r *http.Request) {
	did := r.URL.Query().Get("did")
	if did == "" {
		writeErr(w, 400, "缺少 did")
		return
	}

	siidMax := 8
	piidMax := 6
	if v := r.URL.Query().Get("siidMax"); v != "" {
		fmt.Sscanf(v, "%d", &siidMax)
	}
	if v := r.URL.Query().Get("piidMax"); v != "" {
		fmt.Sscanf(v, "%d", &piidMax)
	}
	if siidMax > 20 {
		siidMax = 20
	}
	if piidMax > 20 {
		piidMax = 20
	}

	sessions := s.store.AllSessions()
	if len(sessions) == 0 {
		writeErr(w, 400, "未登录")
		return
	}

	// 逐个 siid 扫描（每批一个 siid 的所有 piid）
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
		res, err := sessions[0].GetProps(did, props)
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

	writeJSON(w, 200, map[string]any{
		"did":   did,
		"found": results,
		"count": len(results),
	})
}

var _ = fmt.Sprintf
