package main

// 录像片段导出。
//
// 场景：用户想把某段录像发给别人（物业、警察、保险公司）。
// 直接发 MKV 对方可能播不了（HEVC），所以转成 MP4。
//
// 接口：
//   POST /api/recordings/export
//     {name, date, file, start, end, quality}
//   -> 返回一个下载 ID
//
//   GET /api/recordings/export/download?id=xxx
//     -> 下载 MP4
//
// 为什么要两步：
//   转码耗时，如果直接流式返回，用户看到的是「卡住」。
//   先转码（带进度），转完给下载链接，体验更好。

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// exportJob 一个导出任务。
type exportJob struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Date      string  `json:"date"`
	File      string  `json:"file"`
	Start     float64 `json:"start"`
	End       float64 `json:"end"`
	Status    string  `json:"status"` // running / done / failed
	Progress  int     `json:"progress"`
	OutPath   string  `json:"-"`
	SizeMB    float64 `json:"sizeMB"`
	Duration  float64 `json:"duration"`
	Error     string  `json:"error,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

var (
	exportMu   sync.Mutex
	exportJobs = map[string]*exportJob{}
// exportDir 导出文件目录
)

func (s *Server) exportDir() string {
	return filepath.Join(s.dataDir, "exports")
}

// handleRecExport 创建导出任务（异步转码）。
func (s *Server) handleRecExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 POST")
		return
	}

	var req struct {
		Name  string  `json:"name"`
		Date  string  `json:"date"`
		File  string  `json:"file"`
		Start float64 `json:"start"`
		End   float64 `json:"end"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "参数错误")
		return
	}

	src, err := s.findRecFile(safeName(req.Name), safeName(req.Date), safeName(req.File))
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}

	if req.End > 0 && req.End <= req.Start {
		writeErr(w, 400, "结束时间必须大于开始时间")
		return
	}

	job := &exportJob{
		ID:        newID(),
		Name:      req.Name,
		Date:      req.Date,
		File:      req.File,
		Start:     req.Start,
		End:       req.End,
		Status:    "running",
		CreatedAt: time.Now(),
	}
	job.OutPath = filepath.Join(s.exportDir(), job.ID+".mp4")

	exportMu.Lock()
	exportJobs[job.ID] = job
	exportMu.Unlock()

	go s.runExport(job, src)

	logf("[export] 创建任务 %s: %s %.0f-%.0fs", job.ID, req.File, req.Start, req.End)
	writeJSON(w, 200, map[string]any{"ok": true, "id": job.ID})
}

// runExport 后台转码。
func (s *Server) runExport(job *exportJob, src string) {
	defer func() {
		if rec := recover(); rec != nil {
			setExportFailed(job, fmt.Sprintf("%v", rec))
		}
	}()

	if err := os.MkdirAll(s.exportDir(), 0o755); err != nil {
		setExportFailed(job, err.Error())
		return
	}

	enc, encArgs := s.pickFileEncoder(src)

	args := []string{"-hide_banner", "-loglevel", "error", "-y"}
	if job.Start > 0 {
		args = append(args, "-ss", fmt.Sprintf("%.2f", job.Start))
	}
	args = append(args, "-i", src)
	if job.End > 0 && job.End > job.Start {
		args = append(args, "-t", fmt.Sprintf("%.2f", job.End-job.Start))
	}
	args = append(args, encArgs...)
	args = append(args,
		"-c:a", "aac", "-b:a", "64k",
		"-movflags", "+faststart",
		"-f", "mp4",
		job.OutPath,
	)

	cmd := exec.Command(s.ffmpegPath(), args...)
	var errBuf strings.Builder
	cmd.Stderr = &errBuf

	t0 := time.Now()
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(errBuf.String())
		if len(msg) > 300 {
			msg = msg[:300]
		}
		if msg == "" {
			msg = err.Error()
		}
		setExportFailed(job, msg)
		return
	}

	fi, _ := os.Stat(job.OutPath)
	exportMu.Lock()
	job.Status = "done"
	job.Progress = 100
	if fi != nil {
		job.SizeMB = float64(fi.Size()) / (1 << 20)
	}
	if job.End > job.Start {
		job.Duration = job.End - job.Start
	}
	exportMu.Unlock()

	logf("[export] %s 完成: %.1fMB (%.1fs, %s), 耗时 %v",
		job.ID, job.SizeMB, job.Duration, enc, time.Since(t0).Round(time.Second))
}

func setExportFailed(job *exportJob, msg string) {
	exportMu.Lock()
	job.Status = "failed"
	job.Error = msg
	exportMu.Unlock()
	logf("[export] %s 失败: %s", job.ID, msg)
}

// handleRecExportStatus 查询导出任务。
//
// GET /api/recordings/export/status?id=xxx   单个
// GET /api/recordings/export/status          全部
func (s *Server) handleRecExportStatus(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")

	exportMu.Lock()
	defer exportMu.Unlock()

	if id != "" {
		job, ok := exportJobs[id]
		if !ok {
			writeErr(w, 404, "任务不存在")
			return
		}
		writeJSON(w, 200, job)
		return
	}

	var list []*exportJob
	for _, j := range exportJobs {
		list = append(list, j)
	}
	writeJSON(w, 200, map[string]any{"jobs": list})
}

// handleRecExportDownload 下载导出结果。
func (s *Server) handleRecExportDownload(w http.ResponseWriter, r *http.Request) {
	id := safeName(r.URL.Query().Get("id"))
	if id == "" {
		writeErr(w, 400, "缺少 id")
		return
	}

	exportMu.Lock()
	job, ok := exportJobs[id]
	exportMu.Unlock()
	if !ok || job.Status != "done" {
		writeErr(w, 404, "导出未完成或不存在")
		return
	}

	// 文件名：设备_日期_时间段.mp4
	base := strings.TrimSuffix(job.File, ".mkv")
	filename := fmt.Sprintf("%s_%s_%s.mp4", job.Name, job.Date, base)
	filename = strings.ReplaceAll(filename, " ", "_")

	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+filename+"\"")
	http.ServeFile(w, r, job.OutPath)
}

// handleRecExportDelete 删除导出文件。
func (s *Server) handleRecExportDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 POST")
		return
	}
	id := safeName(r.URL.Query().Get("id"))

	exportMu.Lock()
	job, ok := exportJobs[id]
	if ok {
		delete(exportJobs, id)
	}
	exportMu.Unlock()

	if !ok {
		writeErr(w, 404, "任务不存在")
		return
	}
	_ = os.Remove(job.OutPath)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// newID 生成短随机 ID。
func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var _ = strconv.Itoa
