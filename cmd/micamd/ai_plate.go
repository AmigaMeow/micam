package main

// 车牌识别接线：读车牌 -> 写回事件 -> 可检索（阶段 2 A + B）

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/micam/micam/internal/event"
)

// enrichEventPlate 对一个事件读车牌并写回。
//
// ===== 多帧取材（实测驱动）=====
//
// 实测：单帧读车牌只有约 50% 准确率（省份汉字不稳定），
// 但多帧投票能纠正到 100%。所以这里一次给 3~4 帧。
//
// 取材顺序：
//  1. 事件关联录像 -> 在事件前后各抽 1~2 帧（不同时刻，投票更有意义）
//  2. 事件缩略图
//  3. 很新的事件补一张实时帧
func (s *Server) enrichEventPlate(ev *event.Event) (*lprResult, error) {
	if !lprAvailable() {
		return nil, fmt.Errorf("本地车牌识别未安装")
	}
	if lprc == nil {
		lprc = newLPRClient()
	}

	paths := s.gatherPlateFrames(ev)
	if len(paths) == 0 {
		return nil, fmt.Errorf("没有可用的画面")
	}
	defer func() {
		// 临时文件用完就删
		for _, p := range paths {
			if strings.HasPrefix(p, os.TempDir()) {
				_ = os.Remove(p)
			}
		}
	}()

	res, err := lprc.Read(paths, 0.85)
	if err != nil {
		return nil, err
	}
	if res.Plate == "" {
		return res, nil
	}

	// 写回事件
	var alt []string
	for _, c := range res.Candidates {
		if c.Code != res.Plate {
			alt = append(alt, c.Code)
		}
	}
	_ = s.eventStore.Enrich(ev.ID, event.Enrichment{
		AIPlate:     res.Plate,
		AIPlateConf: res.Conf,
		AIPlateSrc:  "local",
		AIPlateAlt:  alt,
	})
	return res, nil
}

// gatherPlateFrames 收集用于车牌识别的多帧
func (s *Server) gatherPlateFrames(ev *event.Event) []string {
	var paths []string

	// 1) 录像抽帧：事件时刻前后各抽
	if ev.RecFile != "" {
		for i, off := range []float64{-1.0, 0, 1.0} {
			t := ev.RecOff + off
			if t < 0 {
				t = 0
			}
			if p := s.fetchRecFrameToFile(ev, t, i); p != "" {
				paths = append(paths, p)
			}
		}
	}

	// 2) 事件缩略图
	if ev.Thumb != "" {
		if _, err := os.Stat(s.eventStore.ThumbPath(ev.Thumb)); err == nil {
			paths = append(paths, s.eventStore.ThumbPath(ev.Thumb))
		}
	}

	// 3) 很新的事件补实时帧
	if s.isRecent(ev.Time, 3) {
		if img := s.captureHiRes(ev.Did); len(img) > 2048 {
			p := filepath.Join(os.TempDir(), fmt.Sprintf("micam-lpr-live-%d.jpg", time.Now().UnixNano()))
			if os.WriteFile(p, img, 0o644) == nil {
				paths = append(paths, p)
			}
		}
	}

	// 最多 5 帧（再多收益递减，且耗时线性增长）
	if len(paths) > 5 {
		paths = paths[:5]
	}
	return paths
}

// fetchRecFrameToFile 从录像抽一帧写到临时文件
func (s *Server) fetchRecFrameToFile(ev *event.Event, t float64, seq int) string {
	img := s.frameFromRecordingAt(ev, t)
	if len(img) < 2048 {
		return ""
	}
	p := filepath.Join(os.TempDir(), fmt.Sprintf("micam-lpr-%d-%d.jpg", time.Now().UnixNano(), seq))
	if os.WriteFile(p, img, 0o644) != nil {
		return ""
	}
	return p
}

// ---- HTTP 接口 ----

// handlePlateRead 对指定事件读车牌。
//
//	POST /api/ai/plate/read  {id}
func (s *Server) handlePlateRead(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		writeErr(w, 400, "缺少 id")
		return
	}

	var ev *event.Event
	for _, x := range s.eventStore.Query(event.Query{Days: 30, Limit: 3000}) {
		if x.ID == req.ID {
			cp := x
			ev = &cp
			break
		}
	}
	if ev == nil {
		writeErr(w, 404, "找不到事件")
		return
	}

	res, err := s.enrichEventPlate(ev)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "result": res, "installed": lprAvailable()})
}

// handlePlateSearch 按车牌检索事件。
//
//	GET /api/ai/plate/search?plate=鄂W61J59&days=30
func (s *Server) handlePlateSearch(w http.ResponseWriter, r *http.Request) {
	plate := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("plate")))
	if plate == "" {
		writeErr(w, 400, "缺少 plate")
		return
	}
	days := 30
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := time.ParseDuration(v + "h"); err == nil {
			days = int(n.Hours() / 24)
		}
	}
	if days <= 0 {
		days = 30
	}

	var hits []event.Event
	for _, e := range s.eventStore.Query(event.Query{Days: days, Limit: 5000}) {
		if strings.ToUpper(e.AIPlate) == plate {
			hits = append(hits, e)
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true, "plate": plate, "events": hits, "count": len(hits)})
}

// handlePlateStatus 本地车牌识别是否就绪
func (s *Server) handlePlateStatus(w http.ResponseWriter, r *http.Request) {
	lib, models, script, ok := lprPaths()
	writeJSON(w, 200, map[string]any{
		"installed": ok,
		"lib":       lib,
		"models":    models,
		"script":    script,
	})
}

// handlePlateInstall 下载本地车牌识别资源（阶段 2 C）。
//
//	POST /api/ai/plate/install
func (s *Server) handlePlateInstall(w http.ResponseWriter, r *http.Request) {
	if lprAvailable() {
		writeJSON(w, 200, map[string]any{"ok": true, "message": "已安装"})
		return
	}
	go func() {
		defer func() { _ = recover() }()
		_ = installLPRResources()
	}()
	writeJSON(w, 200, map[string]any{"ok": true, "message": "开始下载（约 31MB），稍后查看状态"})
}
