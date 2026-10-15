package main

// 动态侦测录像管理器（2026-10-14）。
// 仅当画面出现动体、位移或 AI 识别到人员/车辆等目标时才进行录像。
// 画面恢复静止后延录指定缓冲时长（默认 30 秒）并安全停止分片，
// 极大节省 NAS 存储空间。

import (
		"sync"
	"time"

	"github.com/micam/micam/internal/xiaomi"
)

type motionRecManager struct {
	mu          sync.Mutex
	activeUntil map[string]time.Time // did -> activeUntil
	server      *Server
}

var motionRec = &motionRecManager{
	activeUntil: make(map[string]time.Time),
}

// initMotionRec 启动动体录像超时看门狗。
func (s *Server) initMotionRec() {
	motionRec.server = s
	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for now := range ticker.C {
			motionRec.checkTimeouts(now)
		}
	}()
}

// triggerMotionRecording 动体侦测或 AI 事件触发录像。
func (s *Server) triggerMotionRecording(dev *xiaomi.Device, isAIEvent bool) {
	if dev == nil || !dev.IsOnline {
		return
	}
	if !s.cfg.RecordEnabled {
		return
	}
	mode := s.cfg.RecordMode
	if mode != "motion" && mode != "ai_event" {
		return
	}
	if mode == "ai_event" && !isAIEvent {
		return
	}

	postRoll := s.cfg.RecordPostRollSec
	if postRoll <= 0 {
		postRoll = 30
	}

	motionRec.mu.Lock()
	did := dev.Did
	motionRec.activeUntil[did] = time.Now().Add(time.Duration(postRoll) * time.Second)

	// 检查是否已经在录
	isRec := false
	for _, st := range s.rec.List() {
		if st.Did == did && st.Running {
			isRec = true
			break
		}
	}
	motionRec.mu.Unlock()

	if !isRec {
		logf("[rec-motion] 动体触发录像: %s (did=%s, 延录=%ds, AI=%v)", dev.Name, did, postRoll, isAIEvent)
		go func() {
			if err := s.startRecording(dev); err != nil {
				logf("[rec-motion] 启动录像失败 %s: %v", did, err)
			}
		}()
	}
}

// checkTimeouts 检查静止超时的设备并停止录像。
func (m *motionRecManager) checkTimeouts(now time.Time) {
	if m.server == nil || !m.server.cfg.RecordEnabled {
		return
	}
	mode := m.server.cfg.RecordMode
	if mode != "motion" && mode != "ai_event" {
		return
	}

	m.mu.Lock()
	var toStop []string
	for did, until := range m.activeUntil {
		if now.After(until) {
			toStop = append(toStop, did)
			delete(m.activeUntil, did)
		}
	}
	m.mu.Unlock()

	for _, did := range toStop {
		logf("[rec-motion] 画面恢复静止（延录结束），停止录像 did=%s", did)
		m.server.stopRecordingMiot(did)
		m.server.rec.Stop(did)
	}
}
