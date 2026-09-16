package main

// 录像 API。

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/micam/micam/internal/xiaomi"
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

		if err := s.startRecording(dev); err != nil {
			writeErr(w, 502, err.Error())
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

// startRecording 为一台设备开始录像。
//
// 公共逻辑：建连接 -> 取 consumer -> 起后台协程喂数据 -> 交给录像器。
func (s *Server) startRecording(dev *xiaomi.Device) error {
	_, mc, release, err := s.useConn(dev)
	if err != nil {
		return err
	}

	pr := newBufferedPipe(16 << 20)
	go func() {
		defer release()
		_, _ = mc.Consumer().WriteTo(pr)
	}()

	return s.rec.Start(dev.Did, dev.Name, pr)
}

// startAllRecordings 为所有在线摄像头开始录像。
//
// 为什么需要：配置里的「自动录像」开关原来只改标志位，不真的录。
// 现在开关 = 真实行为：打开就录，关闭就停。
func (s *Server) startAllRecordings() {
	cams := s.allCameras()
	var started, failed int

	for _, d := range cams {
		if !d.IsCamera() || !d.IsOnline {
			continue
		}
		// 已经在录就跳过
		for _, st := range s.rec.List() {
			if st.Did == d.Did && st.Running {
				started++
				goto next
			}
		}
		if err := s.startRecording(d); err != nil {
			logf("[rec] %s 开始录像失败: %v", d.Did, err)
			failed++
		} else {
			logf("[rec] %s 已开始录像", d.Name)
			started++
		}
	next:
	}

	logf("[rec] 自动录像已启动: %d 台成功, %d 台失败", started, failed)
}

// stopAllRecordings 停止所有录像。
func (s *Server) stopAllRecordings() {
	n := 0
	for _, st := range s.rec.List() {
		if st.Running {
			n++
		}
	}
	s.rec.StopAll()
	logf("[rec] 已停止全部录像（%d 路）", n)
}
