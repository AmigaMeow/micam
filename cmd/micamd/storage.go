package main

// 存储监控 API（家庭储存监控）。
//
// 提供：
//   GET /api/storage           总览：磁盘 + 录像占用 + 各摄像头状态
//   GET /api/storage/cleanup-preview  预览可清理的内容
//   POST /api/storage/cleanup  执行清理
//
// 设计目标：让用户一眼看到「录像占了多少空间、还能录多久」。

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/micam/micam/internal/record"
)

// safeDirName 把设备名转成安全的目录名。
func safeDirName(name string) string {
	r := strings.NewReplacer("/", "_", "\\", "_", ":", "_", "*", "_", "?", "_",
		"\"", "_", "<", "_", ">", "_", "|", "_")
	n := strings.TrimSpace(r.Replace(name))
	if n == "" {
		return "unknown"
	}
	return n
}

// handleStorage 存储总览。
//
// GET /api/storage
func (s *Server) handleStorage(w http.ResponseWriter, r *http.Request) {
	cfg := s.rec.GetConfig()
	recDir := cfg.Dir
	if recDir == "" {
		recDir = filepath.Join(s.dataDir, "recordings")
	}

	// 磁盘使用
	disk := record.DiskUsageOf(recDir)

	// 按摄像头统计
	type camStorage struct {
		Did       string  `json:"did"`
		Name      string  `json:"name"`
		Model     string  `json:"model"`
		Online    bool    `json:"online"`
		Recording bool    `json:"recording"`
		Files     int     `json:"files"`
		SizeMB    float64 `json:"sizeMB"`
		LastFile  string  `json:"lastFile,omitempty"`
		LastTime  string  `json:"lastTime,omitempty"`
	}

	// 正在录像的设备
	active := map[string]bool{}
	for _, rec := range s.rec.List() {
		active[rec.Did] = true
	}

	var cams []camStorage
	totalFiles := 0
	var totalBytes int64

	for _, d := range s.allCameras() {
		if !d.IsCamera() {
			continue
		}
		cs := camStorage{
			Did:       d.Did,
			Name:      d.Name,
			Model:     d.Model,
			Online:    d.IsOnline,
			Recording: active[d.Did],
		}

		// 该设备的录像目录
		dir := filepath.Join(recDir, safeDirName(d.Name))
		if _, err := os.Stat(dir); err == nil {
			b, n, _ := record.DirUsage(dir)
			cs.Files = n
			cs.SizeMB = float64(b) / (1 << 20)
			totalFiles += n
			totalBytes += b

			// 最新一个文件
			if entries, err := os.ReadDir(dir); err == nil {
				var newest os.FileInfo
				for _, e := range entries {
					if e.IsDir() {
						continue
					}
					if fi, err := e.Info(); err == nil {
						if newest == nil || fi.ModTime().After(newest.ModTime()) {
							newest = fi
							cs.LastFile = e.Name()
							cs.LastTime = fi.ModTime().Format("2006-01-02 15:04:05")
						}
					}
				}
			}
		}
		cams = append(cams, cs)
	}

	// 按占用排序
	sort.Slice(cams, func(i, j int) bool { return cams[i].SizeMB > cams[j].SizeMB })

	// 估算可录时长（假设每路 250KB/s）
	var recPerHourMB float64 = 250 * 3600 / 1024 // 约 880MB/小时
	hoursLeft := 0.0
	if recPerHourMB > 0 {
		hoursLeft = disk.FreeGB * 1024 / recPerHourMB
	}

	writeJSON(w, 200, map[string]any{
		"disk": disk,
		"recordings": map[string]any{
			"dir":       recDir,
			"totalMB":   float64(totalBytes) / (1 << 20),
			"totalGB":   float64(totalBytes) / (1 << 30),
			"fileCount": totalFiles,
			"enabled":   cfg.Enabled,
			"segment":   cfg.Segment.String(),
			"keepDays":  cfg.KeepDays,
			"minFreeGB": cfg.MinFreeGB,
		},
		"cameras":       cams,
		"estimateHours": hoursLeft,
		"generatedAt":   time.Now().Format(time.RFC3339),
	})
}

// handleCleanupPreview 预览可清理的内容。
//
// GET /api/storage/cleanup-preview?keepDays=7
func (s *Server) handleCleanupPreview(w http.ResponseWriter, r *http.Request) {
	cfg := s.rec.GetConfig()
	recDir := cfg.Dir
	if recDir == "" {
		recDir = filepath.Join(s.dataDir, "recordings")
	}

	keepDays := cfg.KeepDays
	if v := r.URL.Query().Get("keepDays"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			keepDays = n
		}
	}

	deadline := time.Now().AddDate(0, 0, -keepDays)

	type oldFile struct {
		Name    string  `json:"name"`
		Path    string  `json:"path"`
		SizeMB  float64 `json:"sizeMB"`
		ModTime string  `json:"modTime"`
	}
	var files []oldFile
	var totalMB float64

	_ = filepath.Walk(recDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if info.ModTime().Before(deadline) {
			mb := float64(info.Size()) / (1 << 20)
			files = append(files, oldFile{
				Name:    info.Name(),
				Path:    p,
				SizeMB:  mb,
				ModTime: info.ModTime().Format("2006-01-02 15:04:05"),
			})
			totalMB += mb
		}
		return nil
	})

	sort.Slice(files, func(i, j int) bool { return files[i].ModTime < files[j].ModTime })

	writeJSON(w, 200, map[string]any{
		"keepDays":  keepDays,
		"fileCount": len(files),
		"totalMB":   totalMB,
		"files":     files,
	})
}

// startStorageGuard 启动存储守护。
//
// 每 10 分钟检查一次：
//  1. 磁盘剩余低于阈值 -> 立即清理旧文件
//  2. 超过保留天数 -> 清理
//
// 设计目标：无人值守时自动腾空间，避免录到磁盘满。
func (s *Server) startStorageGuard() {
	go func() {
		// 启动后先等 1 分钟（避开启动高峰）
		time.Sleep(1 * time.Minute)

		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()

		for {
			s.checkStorageOnce()
			<-t.C
		}
	}()
}

// checkStorageOnce 执行一次存储检查。
func (s *Server) checkStorageOnce() {
	recMgr := s.rec
	cfg := recMgr.GetConfig()
	recDir := cfg.Dir
	if recDir == "" {
		return
	}

	du := record.DiskUsageOf(recDir)

	// 情况 1：磁盘低于阈值 -> 紧急清理
	if cfg.MinFreeGB > 0 && du.FreeGB < float64(cfg.MinFreeGB) {
		logf("[storage] 磁盘剩余 %.1fGB 低于阈值 %dGB，开始紧急清理",
			du.FreeGB, cfg.MinFreeGB)

		// 发通知（6 小时内不重复）
		s.notifyDiskLow(du.FreeGB, float64(cfg.MinFreeGB))

		n, freed, err := emergencyCleanup(recDir, float64(cfg.MinFreeGB))
		if err != nil {
			logf("[storage] 紧急清理失败: %v", err)
		} else {
			logf("[storage] 紧急清理完成：删除 %d 个文件，释放 %.1fGB",
				n, float64(freed)/(1<<30))
		}
		return
	}

	// 情况 2：超过保留天数 -> 常规清理
	if cfg.KeepDays > 0 {
		deadline := time.Now().AddDate(0, 0, -cfg.KeepDays)
		var n int
		var freed int64
		_ = filepath.Walk(recDir, func(p string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() {
				return nil
			}
			if info.ModTime().Before(deadline) {
				if e := os.Remove(p); e == nil {
					n++
					freed += info.Size()
				}
			}
			return nil
		})
		if n > 0 {
			logf("[storage] 定期清理：删除 %d 个过期文件（>%d 天），释放 %.1fMB",
				n, cfg.KeepDays, float64(freed)/(1<<20))
		}
	}
}

// emergencyCleanup 紧急清理：删除最旧的文件直到剩余空间达标。
func emergencyCleanup(recDir string, needGB float64) (int, int64, error) {
	type fileInfo struct {
		path string
		size int64
		mod  time.Time
	}

	var files []fileInfo
	_ = filepath.Walk(recDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		files = append(files, fileInfo{path: p, size: info.Size(), mod: info.ModTime()})
		return nil
	})

	// 按时间排序（最旧的先删）
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })

	var n int
	var freed int64
	for _, f := range files {
		if du := record.DiskUsageOf(recDir); du.FreeGB >= needGB {
			break
		}
		if err := os.Remove(f.path); err == nil {
			n++
			freed += f.size
		}
	}
	return n, freed, nil
}

var _ = http.MethodGet
