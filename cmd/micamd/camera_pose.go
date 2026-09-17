package main

// 机位 / 镜头一致性检查（阶段 3 比对任务的前置闸门）。
//
// ===== 为什么必须有这个 =====
//
// 补测时发现：CW500（双摄版）的录像里有车，但当前实时缩略图是瓷砖墙。
// 原因是【双摄有两个镜头】，走的是不同画面。
// PTZ 云台也可能被转动。
//
// 如果不检查就做「我的车被划了吗」的比对，
// 「有变化」可能只是【机位变了】—— 会给出完全错误的结论。
//
// ===== 这个检查做什么 =====
//
//   1. 记录每个设备当前使用的镜头
//   2. 记录 PTZ 当前角度（如果能读到）
//   3. 比对前验证两次取帧是否来自同一机位
//
// 阶段 3 才真正启用闸门，本阶段先把元数据定义并记录下来。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// camPose 一次「机位快照」
type camPose struct {
	Did       string `json:"did"`
	Device    string `json:"device"`
	Lens      int    `json:"lens"`      // 镜头编号（双摄：1/2）
	Angle     int    `json:"angle"`     // PTZ 水平角（读不到为 -9999）
	Elevation int    `json:"elevation"` // PTZ 俯仰角
	HasPTZ    bool   `json:"hasPtz"`
	TakenAt   string `json:"takenAt"`
	// 画面指纹（缩略图的粗哈希），用于判断「机位是否变了」
	Fingerprint string `json:"fingerprint"`
}

const poseNoData = -9999

type poseStore struct {
	mu   sync.Mutex
	path string
	data map[string]camPose // did -> 最后一次
}

func newPoseStore(dir string) *poseStore {
	s := &poseStore{path: filepath.Join(dir, "camera-poses.json"), data: map[string]camPose{}}
	if raw, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(raw, &s.data)
	}
	return s
}

func (s *poseStore) put(p camPose) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[p.Did] = p
	raw, _ := json.MarshalIndent(s.data, "", "  ")
	_ = os.WriteFile(s.path+".tmp", raw, 0o644)
	_ = os.Rename(s.path+".tmp", s.path)
}

func (s *poseStore) get(did string) (camPose, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.data[did]
	return p, ok
}

func (s *poseStore) all() map[string]camPose {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]camPose{}
	for k, v := range s.data {
		out[k] = v
	}
	return out
}

// capturePose 抓一次当前机位快照
func (s *Server) capturePose(did, device string, lens int) camPose {
	p := camPose{
		Did:       did,
		Device:    device,
		Lens:      lens,
		Angle:     poseNoData,
		Elevation: poseNoData,
		TakenAt:   time.Now().Format("2006-01-02 15:04:05"),
	}
	if p.Lens <= 0 {
		p.Lens = 1
	}

	// PTZ 角度（只有支持的型号才有）
	if a, e, ok := s.readPTZAngle(did); ok {
		p.HasPTZ = true
		p.Angle = a
		p.Elevation = e
	}

	// 画面指纹：用缩略图的字节长度 + 前若干字节的简单哈希
	if img := s.captureHiRes(did); len(img) > 1024 {
		p.Fingerprint = quickFingerprint(img)
	}

	if s.poses != nil {
		s.poses.put(p)
	}
	return p
}

// quickFingerprint 画面粗指纹（0~1 的相似度比较够用）
func quickFingerprint(b []byte) string {
	var h uint32 = 2166136261
	step := len(b) / 512
	if step < 1 {
		step = 1
	}
	for i := 0; i < len(b); i += step {
		h ^= uint32(b[i])
		h *= 16777619
	}
	return fmt.Sprintf("%08x", h)
}

// comparePoses 判断两次取帧是否来自同一机位。
//
// 返回值：是否可比、说明原因
func comparePoses(a, b camPose) (bool, string) {
	if a.Lens != b.Lens {
		return false, fmt.Sprintf("镜头不同（%d vs %d）——双摄走的是不同画面", a.Lens, b.Lens)
	}
	if a.HasPTZ && b.HasPTZ {
		if a.Angle != poseNoData && b.Angle != poseNoData {
			d := a.Angle - b.Angle
			if d < 0 {
				d = -d
			}
			if d > 3 {
				return false, fmt.Sprintf("云台角度不同（%d° vs %d°，差 %d°）", a.Angle, b.Angle, d)
			}
		}
	}
	if a.Fingerprint != "" && b.Fingerprint != "" && a.Fingerprint != b.Fingerprint {
		// 指纹不同不一定是机位变了（画面本来就会变），只提示
		return true, "画面指纹不同（可能是机位变化，也可能是场景变化）——建议人工确认"
	}
	return true, "机位一致"
}

// ---- HTTP ----

// handlePoseCapture 记录当前机位
func (s *Server) handlePoseCapture(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Did  string `json:"did"`
		Lens int    `json:"lens"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Did == "" {
		writeErr(w, 400, "缺少 did")
		return
	}
	devName := req.Did
	for _, d := range s.allCameras() {
		if d.Did == req.Did {
			devName = d.Name
			break
		}
	}
	p := s.capturePose(req.Did, devName, req.Lens)
	writeJSON(w, 200, map[string]any{"ok": true, "pose": p})
}

// handlePoseList 列出各设备机位
func (s *Server) handlePoseList(w http.ResponseWriter, r *http.Request) {
	if s.poses == nil {
		writeJSON(w, 200, map[string]any{"ok": true, "poses": map[string]any{}})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "poses": s.poses.all()})
}

// readPTZAngle 通过本地接口读云台角度（读不到返回 ok=false）。
func (s *Server) readPTZAngle(did string) (int, int, bool) {
	cli := &http.Client{Timeout: 25 * time.Second}
	resp, err := cli.Get("http://127.0.0.1:8900/api/ptz/status?did=" + did)
	if err != nil {
		return 0, 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, 0, false
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		Angle     int  `json:"angle"`
		Elevation int  `json:"elevation"`
		OK        bool `json:"ok"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return 0, 0, false
	}
	return out.Angle, out.Elevation, true
}
