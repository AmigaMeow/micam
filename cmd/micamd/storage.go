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

	"github.com/micam/micam/internal/event"
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

// estimateRecordHours 按剩余空间估算可录小时数。
//
// 工程假设：每路录像约 250KB/s（约 880MB/小时）。
//
// ⚠️ 这是**唯一**的估算来源。首页和存储页都必须用它，
//
//	不要再在别处维护第二套公式 —— 2026-10-12 实测过一次分叉：
//	前端存储页自己用「文件数 × 5 分钟」反推码率，算出 390 天，
//	而这里给出 6.2 天，同一件事差 60 倍，用户会直接怀疑数据可信度。
func estimateRecordHours(freeGB float64) float64 {
	const recPerHourMB = 250 * 3600 / 1024 // 约 880MB/小时
	if freeGB <= 0 {
		return 0
	}
	return freeGB * 1024 / recPerHourMB
}

// camStorage 单台设备的录像占用。
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

// collectCamStorage 统计各设备的录像占用（按占用降序）。
//
// 从 handleStorage 抽出来，因为首页的「存储明细」卡也要用同一份数据。
// 抽出来的意义不只是复用 —— 更重要的是**两处口径必须一致**：
// 如果首页自己再遍历一次目录，就可能出现「首页说 17 段、存储页说 18 段」
// 这类新的分叉（本项目已经因为两套公式吃过两次亏）。
func (s *Server) collectCamStorage(recDir string) ([]camStorage, int, int64) {
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

		dir := filepath.Join(recDir, safeDirName(d.Name))
		if _, err := os.Stat(dir); err == nil {
			b, n, _ := record.DirUsage(dir)
			cs.Files = n
			cs.SizeMB = float64(b) / (1 << 20)
			totalFiles += n
			totalBytes += b

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

	sort.Slice(cams, func(i, j int) bool { return cams[i].SizeMB > cams[j].SizeMB })
	return cams, totalFiles, totalBytes
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

	// 按摄像头统计（与首页存储明细卡共用同一实现，保证口径一致）
	cams, totalFiles, totalBytes := s.collectCamStorage(recDir)

	// 估算可录时长（假设每路 250KB/s）
	hoursLeft := estimateRecordHours(disk.FreeGB)

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

// handleCleanupPreview 预览可清理的内容（支持按时间段、摄像头、AI智能瘦身、转码缓存等精准维度）。
//
// GET /api/storage/cleanup-preview?days=7&device=...&protectAI=1&allPools=1&cleanCache=1
func (s *Server) handleCleanupPreview(w http.ResponseWriter, r *http.Request) {
	daysParam := r.URL.Query().Get("days")
	keepDaysParam := r.URL.Query().Get("keepDays")
	targetDev := strings.TrimSpace(r.URL.Query().Get("device"))
	protectAI := r.URL.Query().Get("protectAI") == "1" || r.URL.Query().Get("protectAI") == "true"
	includeCache := r.URL.Query().Get("cleanCache") == "1" || r.URL.Query().Get("cleanCache") == "true"
	allPools := r.URL.Query().Get("allPools") != "0" // 默认聚合所有已知录像池

	var deadline time.Time
	now := time.Now()
	cleanAll := false

	if daysParam == "all" {
		cleanAll = true
		deadline = now.Add(24 * time.Hour)
	} else if daysParam == "0" {
		// 仅保留今天（清理今天零点之前）
		deadline = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	} else if n, err := strconv.Atoi(daysParam); err == nil && n > 0 {
		deadline = now.AddDate(0, 0, -n)
	} else if n, err := strconv.Atoi(keepDaysParam); err == nil && n > 0 {
		deadline = now.AddDate(0, 0, -n)
	} else {
		// 默认 7 天
		deadline = now.AddDate(0, 0, -7)
	}

	var roots []string
	if allPools {
		roots = s.recRoots()
	} else {
		roots = []string{s.recRoot()}
	}

	aiFiles := make(map[string]bool)
	if protectAI && s.eventStore != nil {
		evs := s.eventStore.Query(event.Query{Days: 60})
		for _, e := range evs {
			if e.RecFile != "" {
				aiFiles[e.RecFile] = true
			}
		}
	}

	type cleanItem struct {
		Name    string  `json:"name"`
		Path    string  `json:"path"`
		SizeMB  float64 `json:"sizeMB"`
		ModTime string  `json:"modTime"`
		Device  string  `json:"device"`
		Pool    string  `json:"pool"`
	}

	var files []cleanItem
	var totalMB float64
	var protectedAICount int
	var protectedAIMB float64
	var cacheMB float64
	var cacheFiles int

	seenFiles := make(map[string]bool)

	for _, root := range roots {
		ents, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, de := range ents {
			if !de.IsDir() || strings.HasPrefix(de.Name(), ".") || strings.HasPrefix(de.Name(), "@") {
				continue
			}
			devName := de.Name()
			if targetDev != "" && devName != targetDev {
				continue
			}
			devDir := filepath.Join(root, devName)
			dateEnts, err := os.ReadDir(devDir)
			if err != nil {
				continue
			}
			for _, dateE := range dateEnts {
				if !dateE.IsDir() || strings.HasPrefix(dateE.Name(), ".") {
					continue
				}
				dateDir := filepath.Join(devDir, dateE.Name())
				fileEnts, err := os.ReadDir(dateDir)
				if err != nil {
					continue
				}
				for _, fe := range fileEnts {
					if fe.IsDir() || !strings.HasSuffix(fe.Name(), ".mkv") {
						continue
					}
					info, err := fe.Info()
					if err != nil {
						continue
					}
					mb := float64(info.Size()) / (1 << 20)

					if cleanAll || info.ModTime().Before(deadline) {
						if protectAI && aiFiles[fe.Name()] {
							protectedAICount++
							protectedAIMB += mb
							continue
						}
						fKey := root + "/" + devName + "/" + dateE.Name() + "/" + fe.Name()
						if seenFiles[fKey] {
							continue
						}
						seenFiles[fKey] = true

						files = append(files, cleanItem{
							Name:    fe.Name(),
							Path:    filepath.Join(dateDir, fe.Name()),
							SizeMB:  mb,
							ModTime: info.ModTime().Format("2006-01-02 15:04:05"),
							Device:  devName,
							Pool:    friendlyVolName(filepath.Base(root)),
						})
						totalMB += mb
					}
				}
			}
		}

		if includeCache {
			cDir := filepath.Join(root, ".cache")
			_ = filepath.Walk(cDir, func(p string, fi os.FileInfo, err error) error {
				if err == nil && fi != nil && !fi.IsDir() {
					cacheMB += float64(fi.Size()) / (1 << 20)
					cacheFiles++
				}
				return nil
			})
		}
	}

	sort.Slice(files, func(i, j int) bool { return files[i].ModTime < files[j].ModTime })

	sampleLimit := 30
	sampleFiles := files
	if len(sampleFiles) > sampleLimit {
		sampleFiles = sampleFiles[:sampleLimit]
	}

	writeJSON(w, 200, map[string]any{
		"days":              daysParam,
		"deadline":          deadline.Format("2006-01-02 15:04:05"),
		"fileCount":         len(files),
		"totalMB":           totalMB,
		"totalGB":           totalMB / 1024,
		"protectedAICount":  protectedAICount,
		"protectedAIGB":     protectedAIMB / 1024,
		"cacheMB":           cacheMB,
		"cacheFiles":        cacheFiles,
		"files":             sampleFiles,
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

// checkStorageOnce 执行一次存储检查（兼容保留天数/永久保留/存储容量上限/低空间保护）。
func (s *Server) checkStorageOnce() {
	recMgr := s.rec
	cfg := recMgr.GetConfig()
	recDir := cfg.Dir
	if recDir == "" {
		return
	}

	du := record.DiskUsageOf(recDir)

	// 1. 自动清理总开关：若关闭自动清理或设为 neverDelete，则跳过删除（用户选择不删除/仅告警）
	if !s.cfg.RecordAutoClean || s.cfg.RecordLowSpaceMode == "neverDelete" {
		if cfg.MinFreeGB > 0 && du.FreeGB < float64(cfg.MinFreeGB) {
			logf("[storage] 磁盘剩余 %.1fGB 低于阈值 %dGB（已设置不自动删除，仅告警）", du.FreeGB, cfg.MinFreeGB)
			s.notifyDiskLow(du.FreeGB, float64(cfg.MinFreeGB))
		}
		return
	}

	// 2. 磁盘剩余低于阈值 -> 紧急安全保护
	if cfg.MinFreeGB > 0 && du.FreeGB < float64(cfg.MinFreeGB) {
		logf("[storage] 磁盘剩余 %.1fGB 低于阈值 %dGB，触发低空间保护（策略: %s）",
			du.FreeGB, cfg.MinFreeGB, s.cfg.RecordLowSpaceMode)

		s.notifyDiskLow(du.FreeGB, float64(cfg.MinFreeGB))

		if s.cfg.RecordLowSpaceMode == "stopRecording" {
			logf("[storage] 低空间模式为停止录像，安全暂停所有录制")
			s.stopAllRecordings()
			return
		}

		protectAI := (s.cfg.RecordLowSpaceMode == "protectAI")
		n, freed, err := emergencyCleanupEx(recDir, float64(cfg.MinFreeGB), protectAI, s)
		if err != nil {
			logf("[storage] 紧急清理失败: %v", err)
		} else {
			logf("[storage] 紧急清理完成：删除 %d 个文件，释放 %.1fGB", n, float64(freed)/(1<<30))
		}
		return
	}

	// 3. 存储容量上限检查（MaxStorageGB）
	if s.cfg.RecordMaxStorageGB > 0 {
		totalBytes, _, _ := record.DirUsage(recDir)
		usedGB := float64(totalBytes) / (1 << 30)
		maxGB := float64(s.cfg.RecordMaxStorageGB)

		if usedGB > maxGB {
			logf("[storage] 录像占用 %.2fGB 超过上限 %dGB，触发限额清理（策略: %s）",
				usedGB, s.cfg.RecordMaxStorageGB, s.cfg.RecordLowSpaceMode)

			if s.cfg.RecordLowSpaceMode == "stopRecording" {
				s.stopAllRecordings()
				return
			}

			targetBytes := int64(maxGB * 0.90 * (1 << 30))
			protectAI := (s.cfg.RecordLowSpaceMode == "protectAI")
			n, freed, _ := cleanupToTargetSize(recDir, targetBytes, protectAI, s)
			logf("[storage] 配额清理完成：删除 %d 个文件，释放 %.1fGB", n, float64(freed)/(1<<30))
		}
	}

	// 4. 超过保留天数 -> 常规清理（0 表示永久保留，跳过删除）
	if s.cfg.RecordKeepDays > 0 {
		deadline := time.Now().AddDate(0, 0, -s.cfg.RecordKeepDays)
		protectAI := (s.cfg.RecordLowSpaceMode == "protectAI")
		var n int
		var freed int64
		_ = filepath.Walk(recDir, func(p string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() {
				return nil
			}
			if filepath.Ext(p) != ".mkv" {
				return nil
			}
			if info.ModTime().Before(deadline) {
				if protectAI && isFileProtected(p, s) {
					return nil
				}
				if e := os.Remove(p); e == nil {
					n++
					freed += info.Size()
				}
			}
			return nil
		})
		if n > 0 {
			logf("[storage] 定期清理：删除 %d 个过期文件（>%d 天），释放 %.1fMB",
				n, s.cfg.RecordKeepDays, float64(freed)/(1<<20))
		}
	}
}

func isFileProtected(path string, s *Server) bool {
	if s == nil || s.marks == nil {
		return false
	}
	base := filepath.Base(path)
	m := s.marks.get(base)
	return m.Bookmark || m.Watch
}

func cleanupToTargetSize(recDir string, targetBytes int64, protectAI bool, s *Server) (int, int64, error) {
	type fileInfo struct {
		path string
		size int64
		mod  time.Time
	}
	var files []fileInfo
	var totalBytes int64

	_ = filepath.Walk(recDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if filepath.Ext(p) == ".mkv" {
			files = append(files, fileInfo{path: p, size: info.Size(), mod: info.ModTime()})
			totalBytes += info.Size()
		}
		return nil
	})

	if totalBytes <= targetBytes {
		return 0, 0, nil
	}

	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })

	var deletedCount int
	var freedBytes int64

	for _, f := range files {
		if totalBytes-freedBytes <= targetBytes {
			break
		}
		if protectAI && isFileProtected(f.path, s) {
			continue
		}
		if err := os.Remove(f.path); err == nil {
			deletedCount++
			freedBytes += f.size
		}
	}

	return deletedCount, freedBytes, nil
}

func emergencyCleanupEx(recDir string, needGB float64, protectAI bool, s *Server) (int, int64, error) {
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
		if filepath.Ext(p) == ".mkv" {
			files = append(files, fileInfo{path: p, size: info.Size(), mod: info.ModTime()})
		}
		return nil
	})

	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })

	var n int
	var freed int64
	for _, f := range files {
		if du := record.DiskUsageOf(recDir); du.FreeGB >= needGB {
			break
		}
		if protectAI && isFileProtected(f.path, s) {
			continue
		}
		if err := os.Remove(f.path); err == nil {
			n++
			freed += f.size
		}
	}
	return n, freed, nil
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
