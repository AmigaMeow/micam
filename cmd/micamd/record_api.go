package main

// 录像 API。

import (
	"encoding/json"
	"net/http"
	"time"
)

// handleRecordings：GET 列表 / POST 开始录像
func (s *Server) handleRecordings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, map[string]any{
			"recordings": s.rec.List(),
			"config":     s.rec.GetConfig(),
		})

	case http.MethodPost:
		var req struct {
			Did string `json:"did"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Did == "" {
			writeErr(w, 400, "需要 did")
			return
		}

		dev, err := s.ensureDevice(req.Did)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}

		// 独占连接 + consumer
		_, mc, release, err := s.useConn(dev)
		if err != nil {
			writeErr(w, 502, err.Error())
			return
		}
		pr := newBufferedPipe(16 << 20)
		go func() {
			defer release()
			_, _ = mc.Consumer().WriteTo(pr)
		}()

		if err := s.rec.Start(req.Did, dev.Name, pr); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "did": req.Did, "name": dev.Name})

	default:
		writeErr(w, 405, "方法不允许")
	}
}

// handleRecordStop：停止录像
func (s *Server) handleRecordStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 POST")
		return
	}
	var req struct {
		Did string `json:"did"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.Did == "" {
		// 停止全部
		s.rec.StopAll()
		writeJSON(w, 200, map[string]any{"ok": true, "stopped": "all"})
		return
	}
	s.rec.Stop(req.Did)
	writeJSON(w, 200, map[string]any{"ok": true, "did": req.Did})
}

// handleRecordFiles：录像文件列表
func (s *Server) handleRecordFiles(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	files, err := s.rec.ListFiles(name, 100)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"files": files})
}

// handleRecordCleanup：清理过期录像
func (s *Server) handleRecordCleanup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 POST")
		return
	}
	n, freed, err := s.rec.Cleanup()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"deleted": n,
		"freedMB": freed / (1 << 20),
	})
}

var _ = time.Now
