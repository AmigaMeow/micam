package main

// 录像 API。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/micam/micam/internal/config"
	"github.com/micam/micam/internal/record"
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
		s.stopAllRecordings()
		writeJSON(w, 200, map[string]any{"ok": true, "stopped": "all"})
		return
	}
	// 官方通道的 sidecar 也要停（否则留孤儿进程）
	s.stopRecordingMiot(req.Did)
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

// handleRecordCleanup：清理过期录像（支持时间范围、设备、AI保护与跨录像池统一清理）。
func (s *Server) handleRecordCleanup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 POST")
		return
	}

	var req struct {
		Days       any    `json:"days"` // 3, 7, 14, 30, "all", "0", 或数值
		KeepDays   int    `json:"keepDays"`
		Device     string `json:"device"`
		ProtectAI  bool   `json:"protectAI"`
		AllPools   *bool  `json:"allPools"`
		CleanCache bool   `json:"cleanCache"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	daysStr := ""
	switch v := req.Days.(type) {
	case string:
		daysStr = strings.TrimSpace(v)
	case float64:
		daysStr = strconv.Itoa(int(v))
	case int:
		daysStr = strconv.Itoa(v)
	case json.Number:
		daysStr = v.String()
	}

	cleanAll, deadline := parseCleanupDeadline(daysStr, req.KeepDays)

	allPools := true
	if req.AllPools != nil {
		allPools = *req.AllPools
	}

	var roots []string
	if allPools {
		roots = s.recRoots()
	} else {
		roots = []string{s.recRoot()}
	}

	var checker *fileProtectionChecker
	if req.ProtectAI {
		checker = newFileProtectionChecker(s)
	}

	items, _, protectedAICount, _, skippedActive := s.scanCleanableRecordings(roots, req.Device, cleanAll, deadline, req.ProtectAI, checker, 15)

	var deletedCount int
	var freedBytes int64
	for _, item := range items {
		if err := os.Remove(item.Path); err == nil {
			deletedCount++
			freedBytes += item.SizeBytes
			freedBytes += s.removeRecordingArtifacts(item.Path, item.Device, item.Name)
			removeEmptyParentDirs(item.Path, item.Root)
		}
	}
	if deletedCount > 0 {
		saveRecMeta()
	}

	if req.CleanCache {
		for _, cDir := range s.allCacheDirs() {
			_ = filepath.Walk(cDir, func(p string, fi os.FileInfo, err error) error {
				if err == nil && fi != nil && !fi.IsDir() {
					freedBytes += fi.Size()
					_ = os.Remove(p)
				}
				return nil
			})
		}
		recordCacheCleanTime(time.Now())
	}
	if skippedActive > 0 {
		logf("[storage] 清理跳过 %d 个正在录制的活跃分片", skippedActive)
	}

	logf("[storage] 手动清理执行完毕: 删除了 %d 个文件，释放 %.2f MB 空间 (保护 AI 事件 %d 个)",
		deletedCount, float64(freedBytes)/(1<<20), protectedAICount)

	writeJSON(w, 200, map[string]any{
		"ok":               true,
		"deleted":          deletedCount,
		"freedMB":          float64(freedBytes) / (1 << 20),
		"freedGB":          float64(freedBytes) / (1 << 30),
		"protectedAICount": protectedAICount,
	})
}

var _ = time.Now

// startRecording 为一台设备开始录像。
//
// 公共逻辑：建连接 -> 取 consumer -> 起后台协程喂数据 -> 交给录像器。
func (s *Server) checkLowSpacePauseBeforeStart() error {
	if !s.isPausedForLowSpace() {
		return nil
	}
	lowMode := ""
	minFree := 0
	if s.cfg != nil {
		lowMode = config.NormalizeLowSpaceMode(s.cfg.RecordLowSpaceMode)
		minFree = s.cfg.RecordMinFreeGB
	}
	if lowMode != "stopRecording" {
		s.setPausedForLowSpace(false)
		return nil
	}
	du := record.DiskUsageOfReadOnly(s.recRoot())
	if du.Err == "" && (minFree <= 0 || du.FreeGB >= float64(minFree)) {
		s.setPausedForLowSpace(false)
		return nil
	}
	return fmt.Errorf("磁盘剩余空间 %.1fGB 低于最低阈值 %dGB，已暂停录像", du.FreeGB, minFree)
}

func (s *Server) startRecording(dev *xiaomi.Device) error {
	if err := s.checkLowSpacePauseBeforeStart(); err != nil {
		return err
	}
	// ===== 优先走官方通道（2026-10-11）=====
	//
	// 旧路径 useConn() 依赖 miss_get_vendor（被限流）-> 录像文件数一直是 0。
	// 官方通道（OAuth2 + PPCS）不受限流影响，且不经过那个端点。
	// 所以只要授权了就优先用它。
	if _, ok := miotBound(); ok {
		return s.startRecordingMiot(dev)
	}

	// 静默期内不尝试（会撞限流）
	if s.throttleSilenced() {
		return fmt.Errorf("凭据受限（限流静默期），暂无法开始录像")
	}

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
	if err := s.checkLowSpacePauseBeforeStart(); err != nil {
		logf("[rec] %v", err)
		return
	}
	// 官方通道（OAuth2+PPCS）不受 miss_get_vendor 传统凭据限流影响。
	// 仅在未授权官方通道且处于静默期时才跳过。
	if _, ok := miotBound(); !ok && s.throttleSilenced() {
		logf("[rec] 凭据受限（传统限流静默期且未绑定官方通道），暂不开始录像")
		return
	}

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

// stopAllRecordings 停止所有录像（包括官方通道 watchdog 与传统录制器）。
func (s *Server) stopAllRecordings() {
	miotRecJobsMu.Lock()
	dids := make([]string, 0, len(miotRecJobs))
	for d := range miotRecJobs {
		dids = append(dids, d)
	}
	miotRecJobsMu.Unlock()
	stoppedMiot := 0
	for _, d := range dids {
		if s.stopRecordingMiot(d) {
			stoppedMiot++
		}
	}

	n := 0
	if s.rec != nil {
		for _, st := range s.rec.List() {
			if st.Running {
				n++
			}
		}
		s.rec.StopAll()
	}
	total := n
	if stoppedMiot > total {
		total = stoppedMiot
	}
	logf("[rec] 已停止全部录像（%d 路）", total)
}
