package main

// 云台控制（2026-10-11 重做）。
//
// ===== 为什么重做 =====
//
// 旧实现走 cs2 P2P（ptz.go），它依赖 resolveSource -> miss_get_vendor，
// 而那条路被小米限流（实测 code=-12 throttle deny），所以云台完全不可用。
//
// 新实现走【MIoT-Spec over 小米云 RPC】（/home/rpc/{did}），
// 这条通道：
//   · 不经过 miss_get_vendor -> 不被限流（实测 code:0）
//   · 延迟 100-400ms（旧路径 4000ms 超时）—— 快 10-40 倍
//   · 不需要设备 token
//
// ===== 能力边界（实测确认）=====
//
// CW500 的 MIoT-Spec 里：
//   ✅ 读云台坐标    siid=6  piid=12 -> {"x":1017,"y":37}
//   ✅ 云台复位      siid=6  piid=13 （写 true）
//   ✅ 追踪开关      siid=2  piid=8/10/11/12
//   ✅ 巡航位置      siid=10 piid=7
//   ✅ 补光灯/夜视   siid=6  piid=11/16/17/18
//   ❌ 方向移动（上/下/左/右）—— 不在 spec 里，
//      走专用 P2P 协议（0x112 motor），而那条协议依赖被限流的 cs2。
//      详细记录见 docs/PTZ-CONTROL.md

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// miotProp 一次属性读写的描述。
type miotProp struct {
	// ⚠️ Did 必须带 —— 实测缺了它会返回
	//    code=-106 "device network unreachable"
	Did   string `json:"did"`
	Siid  int    `json:"siid"`
	Piid  int    `json:"piid"`
	Value any    `json:"value,omitempty"`
}

// miotSpecRPC 调 MIoT-Spec RPC。
//
// path = /home/rpc/{did}
// payload = {"method": ..., "params": [...]}
func (s *Server) miotSpecRPC(did, method string, params any) (json.RawMessage, error) {
	sessions := s.store.AllSessions()
	if len(sessions) == 0 {
		return nil, fmt.Errorf("未登录小米账号")
	}
	payload := map[string]any{"method": method, "params": params}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	raw, err := sessions[0].Request("/home/rpc/"+did, string(body))
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// miioGetProp 用 miIO 本地协议读属性（快 30-200 倍）。
//
// 返回 (value, ok)。ok=false 表示本地不通，调用方应回退云端。
func (s *Server) miioGetProp(did string, siid, piid int) (string, bool) {
	dev, err := s.ensureDevice(did)
	if err != nil {
		return "", false
	}
	// 需要 IP + token
	ip, _ := s.resolveDeviceIP(dev)
	tok := s.deviceToken(did)
	if ip == "" || tok == "" {
		return "", false
	}
	r, err := s.miioCall(&xiaomiDevice{Name: dev.Name, Model: dev.Model, IP: ip, Token: tok},
		"get_properties",
		[]map[string]any{{"did": did, "siid": siid, "piid": piid}})
	if err != nil {
		return "", false
	}
	arr, _ := r["result"].([]any)
	if len(arr) == 0 {
		return "", false
	}
	item, _ := arr[0].(map[string]any)
	if item == nil {
		return "", false
	}
	if code, _ := item["code"].(float64); code != 0 {
		return "", false
	}
	v := item["value"]
	switch t := v.(type) {
	case string:
		return t, true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case bool:
		if t {
			return "true", true
		}
		return "false", true
	}
	b, _ := json.Marshal(v)
	return string(b), true
}

// miioSetProp 用 miIO 本地协议写属性。
func (s *Server) miioSetProp(did string, siid, piid int, value any) error {
	dev, err := s.ensureDevice(did)
	if err != nil {
		return err
	}
	ip, _ := s.resolveDeviceIP(dev)
	tok := s.deviceToken(did)
	if ip == "" || tok == "" {
		return fmt.Errorf("缺少 IP 或 token")
	}
	r, err := s.miioCall(&xiaomiDevice{Name: dev.Name, Model: dev.Model, IP: ip, Token: tok},
		"set_properties",
		[]map[string]any{{"did": did, "siid": siid, "piid": piid, "value": value}})
	if err != nil {
		return err
	}
	arr, _ := r["result"].([]any)
	if len(arr) > 0 {
		item, _ := arr[0].(map[string]any)
		if item != nil {
			if code, _ := item["code"].(float64); code != 0 {
				return fmt.Errorf("设备返回错误码 %.0f", code)
			}
		}
	}
	return nil
}

// deviceToken 取设备的 miIO token（来自小米云 device_list）。
func (s *Server) deviceToken(did string) string {
	if d, ok := s.store.Device(did); ok && d.Token != "" {
		return d.Token
	}
	return ""
}

// miotGetProp 读一个属性。
func (s *Server) miotGetProp(did string, siid, piid int) (string, error) {
	// 优先 miIO 本地（13-23ms vs 云端 500-3000ms）
	if v, ok := s.miioGetProp(did, siid, piid); ok {
		return v, nil
	}
	// 回退云端
	raw, err := s.miotSpecRPC(did, "get_properties", []miotProp{{Did: did, Siid: siid, Piid: piid}})
	if err != nil {
		return "", err
	}
	// ⚠️ 注意：Session.Request 返回的【已经是 result 字段】，
	//    即 [{"code":0,"did":..,"siid":6,"piid":12,"value":"..."}]
	//    不要再包一层 struct{Result []...} —— 那是双层解包，会失败。
	var arr []struct {
		Code  int             `json:"code"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(raw, &arr); err != nil {
		return "", fmt.Errorf("解析响应失败: %w (原始: %s)", err, truncate(string(raw), 120))
	}
	if len(arr) == 0 {
		return "", fmt.Errorf("响应为空")
	}
	if arr[0].Code != 0 {
		return "", fmt.Errorf("设备返回错误码 %d", arr[0].Code)
	}
	// value 可能是带引号的 JSON 字符串，剥掉外层引号
	val := string(arr[0].Value)
	if len(val) > 1 && val[0] == '"' {
		var s string
		if json.Unmarshal(arr[0].Value, &s) == nil {
			val = s
		}
	}
	return val, nil
}

// miotSetProp 写一个属性。
func (s *Server) miotSetProp(did string, siid, piid int, value any) error {
	// 优先 miIO 本地
	if err := s.miioSetProp(did, siid, piid, value); err == nil {
		return nil
	}
	// 回退云端
	raw, err := s.miotSpecRPC(did, "set_properties",
		[]miotProp{{Did: did, Siid: siid, Piid: piid, Value: value}})
	if err != nil {
		return err
	}
	// 同 get：raw 已经是 result 数组
	var arr []struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(raw, &arr); err != nil {
		return fmt.Errorf("解析响应失败: %w (原始: %s)", err, truncate(string(raw), 120))
	}
	if len(arr) > 0 && arr[0].Code != 0 {
		return fmt.Errorf("设备返回错误码 %d（可能该属性不支持此操作）", arr[0].Code)
	}
	return nil
}

// ===== 云台相关的属性定义（CW500 / isa.camera.hlmax 实测）=====

const (
	ptzPropLocation    = 12 // siid=6  读云台坐标 {"x":..,"y":..}
	ptzPropReset       = 13 // siid=6  写 true 复位云台
	ptzPropLightSwitch = 16 // siid=6  补光灯开关
	ptzPropLightLevel  = 11 // siid=6  补光灯亮度 0-100
	ptzPropNightState  = 17 // siid=6  夜视状态（只读）
	ptzPropMotionTrack = 8  // siid=2  移动追踪
	ptzPropHumanTrack  = 10 // siid=2  人形追踪
	ptzPropAiFrame     = 11 // siid=2  智能画框
	ptzPropLensCorr    = 12 // siid=2  图像畸变矫正
)

const ptzSiidCamera = 6
const ptzSiidControl = 2

// ptzState 云台完整状态。
type ptzState struct {
	Did         string `json:"did"`
	Available   bool   `json:"available"`
	X           int    `json:"x"`
	Y           int    `json:"y"`
	HasPos      bool   `json:"hasPos"`
	MotionTrack bool   `json:"motionTrack"`
	HumanTrack  bool   `json:"humanTrack"`
	AiFrame     bool   `json:"aiFrame"`
	LensCorr    bool   `json:"lensCorr"`
	LightOn     bool   `json:"lightOn"`
	LightLevel  int    `json:"lightLevel"`
	Error       string `json:"error,omitempty"`
	ReadAt      string `json:"readAt"`
}

// ptzStateCache 缓存最后一次成功读取的状态（前端秒开）。
var (
	ptzStateMu    sync.Mutex
	ptzStateCache = map[string]*ptzState{}
)

// readPTZState 读云台完整状态。
//
// 并发读（每个属性一次 RPC 太慢），但仍串行化同一设备。
func (s *Server) readPTZState(did string) *ptzState {
	st := &ptzState{Did: did, ReadAt: time.Now().Format(time.RFC3339)}

	tStart := time.Now()
	dev, err := s.ensureDevice(did)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	tDev := time.Now()
	ip, _ := s.resolveDeviceIP(dev)
	tIP := time.Now()
	tok := s.deviceToken(did)
	tTok := time.Now()
	logf("[ptz-t] ensureDevice=%.0fms resolveIP=%.0fms token=%.0fms",
		float64(tDev.Sub(tStart).Milliseconds()),
		float64(tIP.Sub(tDev).Milliseconds()),
		float64(tTok.Sub(tIP).Milliseconds()))

	// ===== 批量读（一次进程拿全部属性）=====
	//
	// 为什么批量：每个 miioCall 起一个 Python 进程要 ~200ms，
	// 9 个属性就是 1.8 秒。批量后只起一次（实测 210ms 拿 4 个）。
	t0 := time.Now()
	logf("[ptz] 准备: ip=%q token长度=%d model=%q", ip, len(tok), dev.Model)
	if ip != "" && tok != "" {
		sd := &xiaomiDevice{Name: dev.Name, Model: dev.Model, IP: ip, Token: tok}
		calls := []miioBatchCall{
			{"get_properties", []map[string]any{{"did": did, "siid": ptzSiidCamera, "piid": ptzPropLocation}}},
			{"get_properties", []map[string]any{{"did": did, "siid": ptzSiidControl, "piid": ptzPropMotionTrack}}},
			{"get_properties", []map[string]any{{"did": did, "siid": ptzSiidControl, "piid": ptzPropHumanTrack}}},
			{"get_properties", []map[string]any{{"did": did, "siid": ptzSiidControl, "piid": ptzPropAiFrame}}},
			{"get_properties", []map[string]any{{"did": did, "siid": ptzSiidControl, "piid": ptzPropLensCorr}}},
			{"get_properties", []map[string]any{{"did": did, "siid": ptzSiidCamera, "piid": ptzPropLightSwitch}}},
			{"get_properties", []map[string]any{{"did": did, "siid": ptzSiidCamera, "piid": ptzPropLightLevel}}},
		}
		batchStart := time.Now()
		results, batchErr := s.miioBatch(sd, calls)
		logf("[ptz] miIO 批量耗时 %.0fms (准备 %.0fms), err=%v",
			float64(time.Since(batchStart).Milliseconds()),
			float64(batchStart.Sub(t0).Milliseconds()), batchErr)
		if batchErr == nil && len(results) >= 7 {
			get := func(i int) any {
				arr, _ := results[i]["result"].([]any)
				if len(arr) == 0 {
					return nil
				}
				item, _ := arr[0].(map[string]any)
				if item == nil {
					return nil
				}
				return item["value"]
			}
			if v := get(0); v != nil {
				var pos struct {
					X int `json:"x"`
					Y int `json:"y"`
				}
				if sv, ok := v.(string); ok && json.Unmarshal([]byte(sv), &pos) == nil {
					st.X, st.Y, st.HasPos = pos.X, pos.Y, true
				}
			}
			truthy := func(v any) bool {
				switch t := v.(type) {
				case bool:
					return t
				case float64:
					return t != 0
				case string:
					return t == "1" || t == "true"
				}
				return false
			}
			st.MotionTrack = truthy(get(1))
			st.HumanTrack = truthy(get(2))
			st.AiFrame = truthy(get(3))
			st.LensCorr = truthy(get(4))
			st.LightOn = truthy(get(5))
			if f, ok := get(6).(float64); ok {
				st.LightLevel = int(f)
			}
			st.Available = true

			ptzStateMu.Lock()
			ptzStateCache[did] = st
			ptzStateMu.Unlock()
			return st
		}
	}

	// 回退：云端逐个读
	if v, err := s.miotGetProp(did, ptzSiidCamera, ptzPropLocation); err == nil {
		var pos struct {
			X int `json:"x"`
			Y int `json:"y"`
		}
		if json.Unmarshal([]byte(v), &pos) == nil {
			st.X, st.Y, st.HasPos = pos.X, pos.Y, true
			st.Available = true
		}
	} else {
		st.Error = err.Error()
	}

	ptzStateMu.Lock()
	ptzStateCache[did] = st
	ptzStateMu.Unlock()
	return st
}

// handlePTZSpecStatus GET /api/ptz/spec?did=
//
// 读云台状态（新实现，走 MIoT-Spec）。
func (s *Server) handlePTZSpecStatus(w http.ResponseWriter, r *http.Request) {
	did := r.URL.Query().Get("did")
	if did == "" {
		writeErr(w, 400, "缺少 did")
		return
	}

	// 10 秒内的缓存直接用（避免频繁打云端）
	if r.URL.Query().Get("fresh") == "" {
		ptzStateMu.Lock()
		if c, ok := ptzStateCache[did]; ok {
			if t, err := time.Parse(time.RFC3339, c.ReadAt); err == nil &&
				time.Since(t) < 10*time.Second {
				ptzStateMu.Unlock()
				writeJSON(w, 200, c)
				return
			}
		}
		ptzStateMu.Unlock()
	}

	st := s.readPTZState(did)
	writeJSON(w, 200, st)
}

// handlePTZGuard GET /api/ptz/guard
//
// 报告写操作护栏状态（前端用来决定要不要显示控制按钮）。
func (s *Server) handlePTZGuard(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, ptzWriteStatus())
}

// handlePTZReset POST /api/ptz/reset  {did}
//
// 云台回中（写 siid=6 piid=13）。
func (s *Server) handlePTZReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 POST")
		return
	}
	var req struct {
		Did string `json:"did"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Did == "" {
		writeErr(w, 400, "缺少 did")
		return
	}
	// 安全护栏：默认禁止写
	if err := allowPTZWrite(); err != nil {
		writeErr(w, 403, err.Error())
		return
	}
	// 写前备份当前状态（可人工回滚）
	s.backupPTZState(req.Did)

	logf("[ptz] 云台复位 did=%s", req.Did)
	if err := s.miotSetProp(req.Did, ptzSiidCamera, ptzPropReset, true); err != nil {
		writeErr(w, 502, "复位失败: "+err.Error())
		return
	}
	// 复位后坐标会变，清缓存
	ptzStateMu.Lock()
	delete(ptzStateCache, req.Did)
	ptzStateMu.Unlock()
	writeJSON(w, 200, map[string]any{"ok": true, "msg": "云台已复位"})
}

// handlePTZToggle POST /api/ptz/toggle  {did, key, on}
//
// 开关追踪类功能。key 取值：
//
//	motionTrack / humanTrack / aiFrame / lensCorr / light
func (s *Server) handlePTZToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 POST")
		return
	}
	var req struct {
		Did string `json:"did"`
		Key string `json:"key"`
		On  bool   `json:"on"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Did == "" || req.Key == "" {
		writeErr(w, 400, "缺少 did 或 key")
		return
	}

	var siid, piid int
	switch req.Key {
	case "motionTrack":
		siid, piid = ptzSiidControl, ptzPropMotionTrack
	case "humanTrack":
		siid, piid = ptzSiidControl, ptzPropHumanTrack
	case "aiFrame":
		siid, piid = ptzSiidControl, ptzPropAiFrame
	case "lensCorr":
		siid, piid = ptzSiidControl, ptzPropLensCorr
	case "light":
		siid, piid = ptzSiidCamera, ptzPropLightSwitch
	default:
		writeErr(w, 400, "未知的 key: "+req.Key)
		return
	}

	// 安全护栏
	if err := allowPTZWrite(); err != nil {
		writeErr(w, 403, err.Error())
		return
	}
	s.backupPTZState(req.Did)

	if err := s.miotSetProp(req.Did, siid, piid, req.On); err != nil {
		writeErr(w, 502, "设置失败: "+err.Error())
		return
	}
	// 清缓存让下次读到新值
	ptzStateMu.Lock()
	delete(ptzStateCache, req.Did)
	ptzStateMu.Unlock()

	logf("[ptz] %s = %v  did=%s", req.Key, req.On, req.Did)
	writeJSON(w, 200, map[string]any{"ok": true, "key": req.Key, "on": req.On})
}
