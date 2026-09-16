
package main

// 云台预置位 —— 不依赖绝对定位。
//
// ===== 背景 =====
//
// 小米 cs2 的 0x112 命令**没有可用的绝对定位**：
//   op=13 + {angle,elevation}  -> 参数被忽略，摄像机按「步进」处理
//   op=13 + {pan,tilt,...}     -> 摄像机直接断连（字段白名单拒绝）
//   op=6                       -> 唯一返回 ret=0，但语义不明、也不归零
//
// 但实测步长很稳（CW500）：
//   op=2（左）每次 angle -2.7°
//   op=5（右）每次 angle +2.7°
//
// 所以用「**读数 + 步进闭环**」实现预置位：
//   保存：记下当前 (angle, elevation)
//   调用：循环发送方向指令，直到接近目标（或到达步数上限）

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ptzPreset 一个预置位
type ptzPreset struct {
	Slot      int    `json:"slot"`
	Angle     int    `json:"angle"`
	Elevation int    `json:"elevation"`
	Name      string `json:"name,omitempty"`
	SavedAt   string `json:"savedAt"`
}

type presetStore struct {
	mu   sync.Mutex
	path string
	data map[string]map[int]ptzPreset // did -> slot -> preset
}

func newPresetStore(dataDir string) *presetStore {
	s := &presetStore{
		path: filepath.Join(dataDir, "ptz-presets.json"),
		data: map[string]map[int]ptzPreset{},
	}
	if raw, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(raw, &s.data)
	}
	return s
}

func (s *presetStore) get(did string, slot int) (ptzPreset, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.data[did]
	if !ok {
		return ptzPreset{}, false
	}
	p, ok := m[slot]
	return p, ok
}

func (s *presetStore) list(did string) map[int]ptzPreset {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[int]ptzPreset{}
	for k, v := range s.data[did] {
		out[k] = v
	}
	return out
}

func (s *presetStore) set(did string, p ptzPreset) error {
	s.mu.Lock()
	if s.data[did] == nil {
		s.data[did] = map[int]ptzPreset{}
	}
	s.data[did][p.Slot] = p
	raw, _ := json.MarshalIndent(s.data, "", "  ")
	err := os.WriteFile(s.path, raw, 0o644)
	s.mu.Unlock()
	return err
}

func (s *presetStore) del(did string, slot int) error {
	s.mu.Lock()
	if m, ok := s.data[did]; ok {
		delete(m, slot)
	}
	raw, _ := json.MarshalIndent(s.data, "", "  ")
	err := os.WriteFile(s.path, raw, 0o644)
	s.mu.Unlock()
	return err
}

// 每一步大约转动多少度（CW500 实测 ~2.7）
const ptzStepDeg = 2.7

// handlePTZPreset 预置位读写。
//
//	GET    /api/ptz/preset?did=X          列出
//	POST   /api/ptz/preset {did,slot}     保存当前位置到槽位
//	DELETE /api/ptz/preset?did=X&slot=N   删除
func (s *Server) handlePTZPreset(w http.ResponseWriter, r *http.Request) {
	did := r.URL.Query().Get("did")

	switch r.Method {
	case http.MethodGet:
		if did == "" {
			writeErr(w, 400, "缺少 did")
			return
		}
		writeJSON(w, 200, map[string]any{"presets": s.presets.list(did)})
		return

	case http.MethodDelete:
		slot, _ := atoiSafe(r.URL.Query().Get("slot"))
		if did == "" || slot <= 0 {
			writeErr(w, 400, "缺少 did 或 slot")
			return
		}
		_ = s.presets.del(did, slot)
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}

	var req struct {
		Did  string `json:"did"`
		Slot int    `json:"slot"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Did == "" || req.Slot <= 0 {
		writeErr(w, 400, "参数错误（需要 did 和 slot>0）")
		return
	}
	if req.Slot > 9 {
		writeErr(w, 400, "槽位最多 9 个")
		return
	}

	// 读当前位置
	pos, err := s.ptzReadPosition(req.Did)
	if err != nil {
		writeErr(w, 504, "读取位置失败："+err.Error())
		return
	}

	p := ptzPreset{
		Slot:      req.Slot,
		Angle:     pos.Angle,
		Elevation: pos.Elevation,
		Name:      req.Name,
		SavedAt:   time.Now().Format("2006-01-02 15:04:05"),
	}
	if err := s.presets.set(req.Did, p); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "preset": p})
}

// handlePTZGoto 调用预置位（闭环保守步进）。
func (s *Server) handlePTZGoto(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Did  string `json:"did"`
		Slot int    `json:"slot"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Did == "" || req.Slot <= 0 {
		writeErr(w, 400, "参数错误")
		return
	}

	p, ok := s.presets.get(req.Did, req.Slot)
	if !ok {
		writeErr(w, 404, "该槽位没有保存过位置")
		return
	}

	// 每次转动要独占云台，所以整个闭环串行执行
	ptzMu.Lock()
	defer ptzMu.Unlock()

	sess, err := s.openPTZForGoto(req.Did)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	defer sess.Close()

	cur, err := sess.send(1) // 查询当前位置
	if err != nil {
		writeErr(w, 504, "读取位置失败："+err.Error())
		return
	}

	steps := 0
	const maxSteps = 40
	// 容差必须小于单步步长（~2.7°），否则「差 2° 就认为到了」会直接不动
	const tolDeg = 1

	// 先等摄像机稳定（上一条指令可能还在动）
	time.Sleep(800 * time.Millisecond)
	if c, err := sess.send(1); err == nil {
		cur = c
	}

	for steps < maxSteps {
		dA := p.Angle - cur.Angle
		dE := p.Elevation - cur.Elevation

		if absInt(dA) <= tolDeg && absInt(dE) <= tolDeg {
			break
		}

		op := 0
		if absInt(dA) >= absInt(dE) && absInt(dA) > tolDeg {
			// 角度优先：op=2 减小，op=5 增大
			if dA < 0 {
				op = 2
			} else {
				op = 5
			}
		} else if absInt(dE) > tolDeg {
			// 仰角：op=4 减小，op=3 增大
			if dE < 0 {
				op = 4
			} else {
				op = 3
			}
		} else {
			break
		}

		resp, err := sess.send(op)
		if err != nil {
			writeJSON(w, 200, map[string]any{
				"ok": false, "partial": true, "steps": steps,
				"current": cur, "target": p, "error": err.Error(),
			})
			return
		}
		steps++

		// 等它转完再读数。
		//
		// 关键：步进指令的**响应是移动前的位置**（滞后一拍），
		// 直接拿响应当当前位置会导致多走一步。
		time.Sleep(700 * time.Millisecond)
		if c, err := sess.send(1); err == nil {
			cur = c
		} else {
			cur = resp
		}

		// 到极限了就停（ret=-1001 = 走到头）
		if resp.Ret == -1001 || cur.Ret == -1001 {
			break
		}
	}

	writeJSON(w, 200, map[string]any{
		"ok":      absInt(p.Angle-cur.Angle) <= tolDeg && absInt(p.Elevation-cur.Elevation) <= tolDeg,
		"steps":   steps,
		"current": cur,
		"target":  p,
		"delta":   map[string]int{"angle": p.Angle - cur.Angle, "elevation": p.Elevation - cur.Elevation},
	})
}

// ptzReadPosition 读当前位置（开一个短连接）。
func (s *Server) ptzReadPosition(did string) (*ptzResp, error) {
	ptzMu.Lock()
	defer ptzMu.Unlock()
	sess, err := s.openPTZForGoto(did)
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	return sess.send(1)
}

// openPTZForGoto 按 did 建云台连接（预置位逻辑用）。
func (s *Server) openPTZForGoto(did string) (*ptzSession, error) {
	dev, err := s.ensureDevice(did)
	if err != nil {
		return nil, err
	}
	return s.openPTZ(dev)
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func atoiSafe(s string) (int, error) {
	var n int
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, nil
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}
