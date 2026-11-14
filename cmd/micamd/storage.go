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
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/micam/micam/internal/config"
	"github.com/micam/micam/internal/event"
	"github.com/micam/micam/internal/record"
)

func isVideoFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mkv", ".mp4", ".265", ".264":
		return true
	default:
		return false
	}
}

func (s *Server) isPausedForLowSpace() bool {
	if s == nil {
		return false
	}
	return s.lowSpacePaused.Load()
}

func (s *Server) setPausedForLowSpace(v bool) {
	if s != nil {
		s.lowSpacePaused.Store(v)
	}
}

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

	oldestTime time.Time
	newestTime time.Time
}

// collectCamStorage 统计单录像目录的各设备占用（按占用降序）。
func (s *Server) collectCamStorage(recDir string) ([]camStorage, int, int64) {
	return s.collectCamStorageForRoots([]string{recDir})
}

// collectAllCamStorage 聚合统计所有已纳管录像池下的设备占用，保证首页、存储总览与存储明细口径完全一致。
func (s *Server) collectAllCamStorage() ([]camStorage, int, int64) {
	return s.collectCamStorageForRoots(s.recRoots())
}

func (s *Server) collectCamStorageForRoots(roots []string) ([]camStorage, int, int64) {
	active := map[string]bool{}
	if s.rec != nil {
		for _, rec := range s.rec.List() {
			active[rec.Did] = true
		}
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

		dirName := safeDirName(d.Name)
		var camBytes int64
		var camFiles int
		seenDirs := make(map[string]bool)

		for _, r := range roots {
			if r == "" {
				continue
			}
			dir := filepath.Clean(filepath.Join(r, dirName))
			if seenDirs[dir] {
				continue
			}
			seenDirs[dir] = true
			if _, err := os.Stat(dir); err != nil {
				continue
			}
			_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
				if err != nil || info == nil {
					return nil
				}
				if info.IsDir() {
					if p != dir && strings.HasPrefix(info.Name(), ".") {
						return filepath.SkipDir
					}
					return nil
				}
				if strings.HasPrefix(info.Name(), ".") || !isVideoFile(info.Name()) {
					return nil
				}
				camFiles++
				camBytes += info.Size()
				mt := info.ModTime()
				if cs.newestTime.IsZero() || mt.After(cs.newestTime) {
					cs.newestTime = mt
					cs.LastFile = info.Name()
					cs.LastTime = mt.Format("2006-01-02 15:04:05")
				}
				if cs.oldestTime.IsZero() || mt.Before(cs.oldestTime) {
					cs.oldestTime = mt
				}
				return nil
			})
		}

		cs.Files = camFiles
		cs.SizeMB = float64(camBytes) / (1 << 20)
		totalFiles += camFiles
		totalBytes += camBytes
		cams = append(cams, cs)
	}

	sort.Slice(cams, func(i, j int) bool { return cams[i].SizeMB > cams[j].SizeMB })
	return cams, totalFiles, totalBytes
}

// handleStorage 存储总览。
//
// GET /api/storage
func (s *Server) handleStorage(w http.ResponseWriter, r *http.Request) {
	recDir := s.recRoot()
	cfg := s.rec.GetConfig()

	// 磁盘使用
	disk := record.DiskUsageOfReadOnly(recDir)

	// 按摄像头跨所有录像池统一聚合统计，与首页和明细页保持同口径
	cams, totalFiles, totalBytes := s.collectAllCamStorage()

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
			"keepDays":  s.cfg.RecordKeepDays,
			"minFreeGB": cfg.MinFreeGB,
		},
		"cameras":       cams,
		"estimateHours": hoursLeft,
		"generatedAt":   time.Now().Format(time.RFC3339),
	})
}

type cleanupItem struct {
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	Root      string    `json:"-"`
	SizeBytes int64     `json:"-"`
	SizeMB    float64   `json:"sizeMB"`
	ModTime   string    `json:"modTime"`
	ModAt     time.Time `json:"-"`
	Device    string    `json:"device"`
	DateDir   string    `json:"-"`
	Pool      string    `json:"pool"`
}

func parseCleanupDeadline(daysStr string, keepDays int) (cleanAll bool, deadline time.Time) {
	now := time.Now()
	s := strings.TrimSpace(daysStr)
	if s == "all" {
		return true, now.Add(24 * time.Hour)
	}
	if s == "0" {
		return false, time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	}
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return false, now.AddDate(0, 0, -n)
	}
	if keepDays > 0 {
		return false, now.AddDate(0, 0, -keepDays)
	}
	return false, now.AddDate(0, 0, -7)
}

func (s *Server) scanCleanableRecordings(roots []string, targetDev string, cleanAll bool, deadline time.Time, protectAI bool, checker *fileProtectionChecker, skipActiveSec int) (items []cleanupItem, totalMB float64, protectedCount int, protectedMB float64, skippedActive int) {
	seenFiles := make(map[string]bool)
	now := time.Now()

	for _, root := range roots {
		if root == "" {
			continue
		}
		root = filepath.Clean(root)
		ents, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		poolName := friendlyVolName(filepath.Base(root))
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
					if fe.IsDir() || !isVideoFile(fe.Name()) {
						continue
					}
					info, err := fe.Info()
					if err != nil {
						continue
					}
					if skipActiveSec > 0 && now.Sub(info.ModTime()) < time.Duration(skipActiveSec)*time.Second {
						skippedActive++
						continue
					}
					if !cleanAll && !info.ModTime().Before(deadline) {
						continue
					}
					pth := filepath.Join(dateDir, fe.Name())
					mb := float64(info.Size()) / (1 << 20)
					if protectAI && checker.isProtected(pth) {
						protectedCount++
						protectedMB += mb
						continue
					}
					fKey := filepath.Clean(pth)
					if seenFiles[fKey] {
						continue
					}
					seenFiles[fKey] = true

					items = append(items, cleanupItem{
						Name:      fe.Name(),
						Path:      pth,
						Root:      root,
						SizeBytes: info.Size(),
						SizeMB:    mb,
						ModTime:   info.ModTime().Format("2006-01-02 15:04:05"),
						ModAt:     info.ModTime(),
						Device:    devName,
						DateDir:   dateE.Name(),
						Pool:      poolName,
					})
					totalMB += mb
				}
			}
		}
	}

	sort.Slice(items, func(i, j int) bool { return items[i].ModAt.Before(items[j].ModAt) })
	return
}

func (s *Server) scanAllCacheUsage() (files int, bytes int64) {
	for _, cDir := range s.allCacheDirs() {
		_ = filepath.Walk(cDir, func(p string, fi os.FileInfo, err error) error {
			if err == nil && fi != nil && !fi.IsDir() {
				bytes += fi.Size()
				files++
			}
			return nil
		})
	}
	return
}

func removeEmptyParentDirs(filePath, stopRoot string) {
	cleanRoot := filepath.Clean(stopRoot)
	dateDir := filepath.Dir(filepath.Clean(filePath))
	if dateDir != "" && dateDir != "." && dateDir != "/" && dateDir != cleanRoot {
		_ = os.Remove(dateDir)
		devDir := filepath.Dir(dateDir)
		if devDir != "" && devDir != "." && devDir != "/" && devDir != cleanRoot {
			_ = os.Remove(devDir)
		}
	}
}

func sameDevice(a, b string) bool {
	var sa, sb syscall.Stat_t
	findStat := func(p string, st *syscall.Stat_t) bool {
		p = filepath.Clean(p)
		for p != "" && p != "." {
			if syscall.Stat(p, st) == nil {
				return true
			}
			next := filepath.Dir(p)
			if next == p {
				break
			}
			p = next
		}
		return false
	}
	if !findStat(a, &sa) || !findStat(b, &sb) {
		return true
	}
	return sa.Dev == sb.Dev
}

// handleCleanupPreview 预览可清理的内容（支持按时间段、摄像头、AI智能瘦身、转码缓存等精准维度）。
//
// GET /api/storage/cleanup-preview?days=7&device=...&protectAI=1&allPools=1&cleanCache=1
func (s *Server) handleCleanupPreview(w http.ResponseWriter, r *http.Request) {
	daysParam := strings.TrimSpace(r.URL.Query().Get("days"))
	keepDaysParam := strings.TrimSpace(r.URL.Query().Get("keepDays"))
	if daysParam == "" && keepDaysParam != "" {
		daysParam = keepDaysParam
	}
	targetDev := strings.TrimSpace(r.URL.Query().Get("device"))
	protectAI := r.URL.Query().Get("protectAI") == "1" || r.URL.Query().Get("protectAI") == "true"
	includeCache := r.URL.Query().Get("cleanCache") == "1" || r.URL.Query().Get("cleanCache") == "true"
	allPools := r.URL.Query().Get("allPools") != "0"

	cleanAll, deadline := parseCleanupDeadline(daysParam, 0)

	var roots []string
	if allPools {
		roots = s.recRoots()
	} else {
		roots = []string{s.recRoot()}
	}

	var checker *fileProtectionChecker
	if protectAI {
		checker = newFileProtectionChecker(s)
	}

	files, totalMB, protectedAICount, protectedAIMB, _ := s.scanCleanableRecordings(roots, targetDev, cleanAll, deadline, protectAI, checker, 15)

	var cacheMB float64
	var cacheFiles int
	if includeCache {
		var cacheBytes int64
		cacheFiles, cacheBytes = s.scanAllCacheUsage()
		cacheMB = float64(cacheBytes) / (1 << 20)
	}

	sampleLimit := 30
	sampleFiles := files
	if len(sampleFiles) > sampleLimit {
		sampleFiles = sampleFiles[:sampleLimit]
	}

	writeJSON(w, 200, map[string]any{
		"days":             daysParam,
		"deadline":         deadline.Format("2006-01-02 15:04:05"),
		"fileCount":        len(files),
		"totalMB":          totalMB,
		"totalGB":          totalMB / 1024,
		"protectedAICount": protectedAICount,
		"protectedAIGB":    protectedAIMB / 1024,
		"cacheMB":          cacheMB,
		"cacheFiles":       cacheFiles,
		"files":            sampleFiles,
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
	if s == nil || s.rec == nil || s.cfg == nil {
		return
	}
	cfg := s.rec.GetConfig()
	recDir := cfg.Dir
	if recDir == "" {
		recDir = s.recRoot()
	}
	if recDir == "" {
		return
	}

	// 转码缓存与 AppData 临时产物始终按生命周期维护（即使用户设置录像不自动删，缓存与日志也不能无限膨胀）
	defer func() {
		s.evictCache()
		s.maintainAppData()
	}()

	du := record.DiskUsageOfReadOnly(recDir)
	lowMode := config.NormalizeLowSpaceMode(s.cfg.RecordLowSpaceMode)
	minFree := float64(cfg.MinFreeGB)
	protectAI := (lowMode == "protectAI")

	// 1. 低空间应急检查（仅当磁盘状态正常拿到时生效）
	if du.Err == "" && minFree > 0 && du.FreeGB < minFree {
		logf("[storage] 磁盘剩余 %.1fGB 低于阈值 %.0fGB，触发低空间保护（策略: %s, autoClean=%v）",
			du.FreeGB, minFree, lowMode, s.cfg.RecordAutoClean)
		s.notifyDiskLow(du.FreeGB, minFree)

		if !s.cfg.RecordAutoClean || lowMode == "neverDelete" {
			s.setPausedForLowSpace(false)
			return
		}
		if lowMode == "stopRecording" {
			s.setPausedForLowSpace(true)
			s.stopAllRecordings()
			return
		}
		s.setPausedForLowSpace(false)
		// 优先清理缓存，再清理最旧录像以尽可能保住录像
		s.evictCache()
		n, freed, err := s.emergencyCleanupPools(recDir, minFree, protectAI)
		if err != nil {
			logf("[storage] 紧急清理失败: %v", err)
		} else {
			logf("[storage] 紧急清理完成：删除 %d 个文件，释放 %.2fGB", n, float64(freed)/(1<<30))
		}
		return
	}

	// 自动清理关闭或「不自动删」：仅发低空间/配额告警，不删除用户录像
	if !s.cfg.RecordAutoClean || lowMode == "neverDelete" {
		s.setPausedForLowSpace(false)
		return
	}

	// 若此前因 stopRecording 暂停录制，当磁盘空间回到阈值+2GB 时恢复
	if s.isPausedForLowSpace() && (lowMode != "stopRecording" || (du.Err == "" && (minFree <= 0 || du.FreeGB >= minFree+2))) {
		s.setPausedForLowSpace(false)
		logf("[storage] 磁盘剩余已恢复（%.1fGB），解除低空间暂停录制状态", du.FreeGB)
		if s.cfg.RecordEnabled && s.cfg.RecordMode == "always" {
			s.startAllRecordings()
		}
	}

	// 2. 存储容量上限检查（跨所有纳管录像池聚合统计）
	if s.cfg.RecordMaxStorageGB > 0 {
		maxGB := float64(s.cfg.RecordMaxStorageGB)
		targetBytes := int64(maxGB * 0.90 * (1 << 30))
		var totalBytes int64
		for _, r := range s.recRoots() {
			b, _, _ := record.DirUsage(r)
			totalBytes += b
		}
		usedGB := float64(totalBytes) / (1 << 30)
		if usedGB > maxGB {
			logf("[storage] 全部纳管录像占用 %.2fGB 超过上限 %dGB，触发限额清理（策略: %s）",
				usedGB, s.cfg.RecordMaxStorageGB, lowMode)
			if lowMode == "stopRecording" {
				s.setPausedForLowSpace(true)
				s.stopAllRecordings()
				return
			}
			n, freed, _ := s.cleanupPoolsToTargetSize(s.recRoots(), targetBytes, protectAI)
			logf("[storage] 配额清理完成：删除 %d 个文件，释放 %.2fGB", n, float64(freed)/(1<<30))
		}
	}

	// 3. 超过保留天数 -> 跨所有纳管录像池定期清理（0 表示永久保留）
	if s.cfg.RecordKeepDays > 0 {
		deadline := time.Now().AddDate(0, 0, -s.cfg.RecordKeepDays)
		var checker *fileProtectionChecker
		if protectAI {
			checker = newFileProtectionChecker(s)
		}
		var n int
		var freed int64
		now := time.Now()
		for _, r := range s.recRoots() {
			_ = filepath.Walk(r, func(p string, info os.FileInfo, err error) error {
				if err != nil || info == nil || info.IsDir() {
					return nil
				}
				if !isVideoFile(info.Name()) {
					return nil
				}
				if now.Sub(info.ModTime()) < 30*time.Second {
					return nil
				}
				if info.ModTime().Before(deadline) {
					if protectAI && checker.isProtected(p) {
						return nil
					}
					if e := os.Remove(p); e == nil {
						n++
						freed += info.Size()
						s.removeRecordingArtifacts(p, "", info.Name())
						removeEmptyParentDirs(p, r)
					}
				}
				return nil
			})
		}
		if n > 0 {
			logf("[storage] 定期清理：删除 %d 个过期文件（>%d 天），释放 %.1fMB",
				n, s.cfg.RecordKeepDays, float64(freed)/(1<<20))
		}
	}
}

type protectedTimeRange struct {
	start time.Time
	end   time.Time
}

type fileProtectionChecker struct {
	s              *Server
	protectedFiles map[string]bool
	timeRanges     []protectedTimeRange
}

func newFileProtectionChecker(s *Server) *fileProtectionChecker {
	c := &fileProtectionChecker{
		s:              s,
		protectedFiles: make(map[string]bool),
	}
	if s == nil {
		return c
	}

	if s.eventStore != nil {
		days := 365
		if s.cfg != nil && s.cfg.AI.ResultKeepDays > days {
			days = s.cfg.AI.ResultKeepDays
		}
		evs := s.eventStore.Query(event.Query{Days: days, Limit: -1})
		for _, e := range evs {
			isL1 := false
			if s.marks != nil {
				m := s.marks.get(e.ID)
				if m.Bookmark || m.Watch {
					isL1 = true
				}
			}
			isL2 := (e.Type == "person" || e.Type == "vehicle" || e.Type == "package")

			if isL1 || isL2 {
				if e.RecFile != "" {
					c.protectedFiles[e.RecFile] = true
				}
				if e.Time != "" {
					if evTime, err := time.ParseInLocation("2006-01-02 15:04:05", e.Time, time.Local); err == nil {
						c.timeRanges = append(c.timeRanges, protectedTimeRange{
							start: evTime.Add(-15 * time.Minute),
							end:   evTime.Add(1 * time.Minute),
						})
					}
				}
			}
		}
	}
	return c
}

func (c *fileProtectionChecker) isProtected(path string) bool {
	if c == nil || c.s == nil {
		return false
	}
	base := filepath.Base(path)

	// 1. 用户直接在文件维度打的标（收藏/关注）
	if c.s.marks != nil {
		m := c.s.marks.get(base)
		if m.Bookmark || m.Watch {
			return true
		}
	}

	// 2. 对应高价值或被标记事件关联的 RecFile
	if c.protectedFiles[base] {
		return true
	}

	// 3. 时间窗口范围匹配
	if len(c.timeRanges) > 0 {
		cleanBase := strings.TrimSuffix(strings.TrimSuffix(base, ".mkv"), ".mp4")
		if len(cleanBase) >= 15 {
			if fileTime, err := time.ParseInLocation("20060102_150405", cleanBase[:15], time.Local); err == nil {
				for _, tr := range c.timeRanges {
					if !fileTime.Before(tr.start) && !fileTime.After(tr.end) {
						return true
					}
				}
			}
		}
	}

	return false
}

// isFileProtected 判断录像文件是否受分级归档保护（保持单次兼容）
func isFileProtected(path string, s *Server) bool {
	return newFileProtectionChecker(s).isProtected(path)
}

func cleanupToTargetSize(recDir string, targetBytes int64, protectAI bool, s *Server) (int, int64, error) {
	return s.cleanupPoolsToTargetSize([]string{recDir}, targetBytes, protectAI)
}

func (s *Server) cleanupPoolsToTargetSize(roots []string, targetBytes int64, protectAI bool) (int, int64, error) {
	type fileInfo struct {
		path string
		root string
		name string
		size int64
		mod  time.Time
	}
	var files []fileInfo
	var totalBytes int64
	seen := make(map[string]bool)
	skipActive := (s != nil && s.rec != nil)
	now := time.Now()

	for _, r := range roots {
		if r == "" {
			continue
		}
		cleanR := filepath.Clean(r)
		_ = filepath.Walk(cleanR, func(p string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() {
				return nil
			}
			if !isVideoFile(info.Name()) {
				return nil
			}
			cleanP := filepath.Clean(p)
			if seen[cleanP] {
				return nil
			}
			seen[cleanP] = true
			totalBytes += info.Size()
			if skipActive && now.Sub(info.ModTime()) < 30*time.Second {
				return nil
			}
			files = append(files, fileInfo{path: cleanP, root: cleanR, name: info.Name(), size: info.Size(), mod: info.ModTime()})
			return nil
		})
	}

	if totalBytes <= targetBytes {
		return 0, 0, nil
	}

	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })

	var deletedCount int
	var freedRecBytes int64

	var checker *fileProtectionChecker
	if protectAI {
		checker = newFileProtectionChecker(s)
	}

	for _, f := range files {
		if totalBytes-freedRecBytes <= targetBytes {
			break
		}
		if protectAI && checker.isProtected(f.path) {
			continue
		}
		if err := os.Remove(f.path); err == nil {
			deletedCount++
			freedRecBytes += f.size
			if s != nil {
				s.removeRecordingArtifacts(f.path, "", f.name)
			}
			removeEmptyParentDirs(f.path, f.root)
		}
	}

	return deletedCount, freedRecBytes, nil
}

func emergencyCleanupEx(recDir string, needGB float64, protectAI bool, s *Server) (int, int64, error) {
	return s.emergencyCleanupRoots([]string{recDir}, recDir, needGB, protectAI)
}

func (s *Server) emergencyCleanupPools(recDir string, needGB float64, protectAI bool) (int, int64, error) {
	var sameDevRoots []string
	seen := make(map[string]bool)
	add := func(r string) {
		r = filepath.Clean(r)
		if r == "" || seen[r] {
			return
		}
		if sameDevice(recDir, r) {
			seen[r] = true
			sameDevRoots = append(sameDevRoots, r)
		}
	}
	add(recDir)
	for _, r := range s.recRoots() {
		add(r)
	}
	return s.emergencyCleanupRoots(sameDevRoots, recDir, needGB, protectAI)
}

func (s *Server) emergencyCleanupRoots(roots []string, diskPath string, needGB float64, protectAI bool) (int, int64, error) {
	type fileInfo struct {
		path string
		root string
		name string
		size int64
		mod  time.Time
	}

	var files []fileInfo
	seen := make(map[string]bool)
	skipActive := (s != nil && s.rec != nil)
	now := time.Now()

	for _, r := range roots {
		if r == "" {
			continue
		}
		cleanR := filepath.Clean(r)
		_ = filepath.Walk(cleanR, func(p string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() {
				return nil
			}
			if !isVideoFile(info.Name()) {
				return nil
			}
			if skipActive && now.Sub(info.ModTime()) < 30*time.Second {
				return nil
			}
			cleanP := filepath.Clean(p)
			if seen[cleanP] {
				return nil
			}
			seen[cleanP] = true
			files = append(files, fileInfo{path: cleanP, root: cleanR, name: info.Name(), size: info.Size(), mod: info.ModTime()})
			return nil
		})
	}

	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })

	var n int
	var freed int64
	var checker *fileProtectionChecker
	if protectAI {
		checker = newFileProtectionChecker(s)
	}
	for _, f := range files {
		if du := record.DiskUsageOfReadOnly(diskPath); du.Err == "" && du.FreeGB >= needGB {
			break
		}
		if protectAI && checker.isProtected(f.path) {
			continue
		}
		if err := os.Remove(f.path); err == nil {
			n++
			freed += f.size
			if s != nil {
				freed += s.removeRecordingArtifacts(f.path, "", f.name)
			}
			removeEmptyParentDirs(f.path, f.root)
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

// cleanStaleQRFiles 清理超过 15 分钟的临时扫码登录二维码，防止遗留 PNG 堆积。
func (s *Server) cleanStaleQRFiles() {
	if s == nil || s.dataDir == "" {
		return
	}
	matches, err := filepath.Glob(filepath.Join(s.dataDir, "qr_*.png"))
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-15 * time.Minute)
	for _, p := range matches {
		if fi, e := os.Stat(p); e == nil && fi.ModTime().Before(cutoff) {
			_ = os.Remove(p)
		}
	}
}

// maintainAppData 每 30 分钟执行一次应用数据与附属缓存生命周期清理。
func (s *Server) maintainAppData() {
	if s == nil || s.dataDir == "" {
		return
	}
	nowUnix := time.Now().Unix()
	last := s.lastAppDataMaintain.Load()
	if last > 0 && nowUnix-last < 1800 {
		return
	}
	s.lastAppDataMaintain.Store(nowUnix)
	s.maintainAppDataNow()
}

func (s *Server) maintainAppDataNow() {
	if s == nil || s.dataDir == "" {
		return
	}
	now := time.Now()
	var freed int64

	// 1. 临时登录二维码与过期导出包
	s.cleanStaleQRFiles()
	s.cleanExportsOnStart()

	// 2. 事件索引及事件缩略图按保留天数与孤儿清理（保护用户标星/关注的事件）
	eventKeep := 30
	if s.cfg != nil && s.cfg.AI.ResultKeepDays > 0 {
		eventKeep = s.cfg.AI.ResultKeepDays
	}
	if s.eventStore != nil {
		protectedIDs := make(map[string]bool)
		if s.marks != nil {
			for id, m := range s.marks.list() {
				if m.Bookmark || m.Watch {
					protectedIDs[id] = true
				}
			}
		}
		removedDays, removedThumbs, bytes, err := s.eventStore.CleanupWithRetention(eventKeep, protectedIDs)
		if err == nil {
			freed += bytes
			if removedDays > 0 || removedThumbs > 0 {
				logf("[storage] 事件清理：过期分片 %d，孤儿/过期缩略图 %d，释放 %.1fMB",
					removedDays, removedThumbs, float64(bytes)/(1<<20))
			}
		}
	}

	// 3. AI 推理缓存清理（7天过期 + 100MB 上限 LRU）
	aiCacheDir := filepath.Join(s.dataDir, "ai-cache")
	type cachedEnt struct {
		path string
		size int64
		mod  time.Time
	}
	var aiRem []cachedEnt
	var aiBytes int64
	aiCutoff := now.AddDate(0, 0, -7)
	_ = filepath.Walk(aiCacheDir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi == nil || fi.IsDir() {
			return nil
		}
		if fi.ModTime().Before(aiCutoff) {
			if os.Remove(p) == nil {
				freed += fi.Size()
			}
			return nil
		}
		aiBytes += fi.Size()
		aiRem = append(aiRem, cachedEnt{path: p, size: fi.Size(), mod: fi.ModTime()})
		return nil
	})
	const maxAICacheBytes int64 = 100 << 20
	const targetAICacheBytes int64 = 80 << 20
	if aiBytes > maxAICacheBytes {
		sort.Slice(aiRem, func(i, j int) bool { return aiRem[i].mod.Before(aiRem[j].mod) })
		for _, e := range aiRem {
			if aiBytes <= targetAICacheBytes {
				break
			}
			if os.Remove(e.path) == nil {
				aiBytes -= e.size
				freed += e.size
			}
		}
	}

	// 4. 清理超过 7 天的抽帧图（frames / .frames）
	frameRoots := []string{filepath.Join(s.dataDir, "frames")}
	roots := s.recRoots()
	for _, r := range roots {
		frameRoots = append(frameRoots, filepath.Join(r, ".frames"))
	}
	frameCutoff := now.AddDate(0, 0, -7)
	for _, fr := range frameRoots {
		_ = filepath.Walk(fr, func(p string, fi os.FileInfo, err error) error {
			if err != nil || fi == nil || fi.IsDir() {
				return nil
			}
			if fi.ModTime().Before(frameCutoff) {
				if os.Remove(p) == nil {
					freed += fi.Size()
					removeEmptyParentDirs(p, fr)
				}
			}
			return nil
		})
	}

	// 5. 清理孤儿录像缩略图（thumbs / .thumbs）与无主 rec_meta.json 索引项
	existingRel := make(map[string]bool)
	existingName := make(map[string]bool)
	for _, r := range roots {
		_ = filepath.Walk(r, func(p string, fi os.FileInfo, err error) error {
			if err != nil || infoDirSkip(p, r, fi) {
				if err == nil && fi != nil && fi.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if fi == nil || fi.IsDir() || !isVideoFile(fi.Name()) {
				return nil
			}
			existingName[fi.Name()] = true
			devName := filepath.Base(filepath.Dir(filepath.Dir(p)))
			if devName != "" && devName != "." {
				existingRel[devName+"/"+fi.Name()] = true
			}
			return nil
		})
	}
	thumbRoots := []string{filepath.Join(s.dataDir, "thumbs")}
	for _, r := range roots {
		thumbRoots = append(thumbRoots, filepath.Join(r, ".thumbs"))
	}
	thumbGrace := now.Add(-10 * time.Minute)
	for _, tr := range thumbRoots {
		_ = filepath.Walk(tr, func(p string, fi os.FileInfo, err error) error {
			if err != nil || fi == nil || fi.IsDir() {
				return nil
			}
			if fi.ModTime().After(thumbGrace) {
				return nil
			}
			fName := fi.Name()
			if !strings.HasSuffix(strings.ToLower(fName), ".jpg") {
				return nil
			}
			baseRec := strings.TrimSuffix(fName, filepath.Ext(fName))
			rel, _ := filepath.Rel(tr, p)
			camPart := filepath.Dir(filepath.ToSlash(rel))
			hasRec := false
			if camPart != "." && camPart != "" {
				hasRec = existingRel[camPart+"/"+baseRec]
			}
			if !hasRec {
				hasRec = existingName[baseRec]
			}
			if !hasRec {
				if os.Remove(p) == nil {
					freed += fi.Size()
				}
			}
			return nil
		})
	}

	// 6. 清理已失效的 rec_meta.json 条目
	prunedMeta := 0
	metaMu.Lock()
	for k := range metaCache {
		if _, err := os.Stat(k); os.IsNotExist(err) {
			delete(metaCache, k)
			prunedMeta++
		}
	}
	metaMu.Unlock()
	if prunedMeta > 0 {
		saveRecMeta()
	}

	// 7. 清理各录像根目录遗留的 0 字节写入探针文件 (.micam-wtest-*)
	wtestCutoff := now.Add(-5 * time.Minute)
	for _, r := range roots {
		cleanStaleWriteProbeFiles(r, wtestCutoff)
	}

	if freed > 0 || prunedMeta > 0 {
		logf("[storage] 应用数据定期维护：清理释放 %.1fMB，精简过期元数据索引 %d 条",
			float64(freed)/(1<<20), prunedMeta)
	}
}

func infoDirSkip(p, root string, fi os.FileInfo) bool {
	return fi != nil && fi.IsDir() && p != root && strings.HasPrefix(fi.Name(), ".")
}

func cleanStaleWriteProbeFiles(dir string, cutoff time.Time) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasPrefix(n, ".micam-wtest-") || n == ".micam-write-test" {
			if fi, err := e.Info(); err == nil && fi.ModTime().Before(cutoff) {
				_ = os.Remove(filepath.Join(dir, n))
			}
		}
	}
}

// handleStoragePolicy 处理存储与安全保障策略的查询与保存
func (s *Server) handleStoragePolicy(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, map[string]any{
			"ok":           true,
			"keepDays":     s.cfg.RecordKeepDays,
			"maxStorageGB": s.cfg.RecordMaxStorageGB,
			"minFreeGB":    s.cfg.RecordMinFreeGB,
			"autoClean":    s.cfg.RecordAutoClean,
			"lowSpaceMode": s.cfg.RecordLowSpaceMode,
			"segmentSec":   s.cfg.RecordSegment,
			"cacheGB":      s.cfg.TranscodeCacheGB,
		})
	case http.MethodPost:
		var req struct {
			KeepDays     *int    `json:"keepDays"`
			MaxStorageGB *int    `json:"maxStorageGB"`
			MinFreeGB    *int    `json:"minFreeGB"`
			AutoClean    *bool   `json:"autoClean"`
			LowSpaceMode *string `json:"lowSpaceMode"`
			SegmentSec   *int    `json:"segmentSec"`
			CacheGB      *int    `json:"cacheGB"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, "参数错误: "+err.Error())
			return
		}
		if req.SegmentSec != nil && (*req.SegmentSec < 10 || *req.SegmentSec > 3600) {
			writeErr(w, 400, "分片时长需在 10-3600 秒之间")
			return
		}
		if req.KeepDays != nil && (*req.KeepDays < 0 || *req.KeepDays > 365) {
			writeErr(w, 400, "保留天数需在 0-365 之间（0 表示永久保留）")
			return
		}
		if req.MaxStorageGB != nil && (*req.MaxStorageGB < 0 || *req.MaxStorageGB > 100000) {
			writeErr(w, 400, "存储上限需在 0-100000 GB 之间（0 表示不限）")
			return
		}
		if req.MinFreeGB != nil && (*req.MinFreeGB < 0 || *req.MinFreeGB > 10000) {
			writeErr(w, 400, "最低保留空间需在 0-10000 GB 之间")
			return
		}
		if req.CacheGB != nil && (*req.CacheGB < 1 || *req.CacheGB > 500) {
			writeErr(w, 400, "转码缓存上限需在 1-500 GB 之间")
			return
		}

		if err := s.cfg.SetRecordPolicy(nil, nil, req.SegmentSec, req.KeepDays, req.MaxStorageGB, req.MinFreeGB, nil, req.CacheGB, req.AutoClean, req.LowSpaceMode, nil); err != nil {
			writeErr(w, 500, "保存失败: "+err.Error())
			return
		}
		if req.LowSpaceMode != nil && config.NormalizeLowSpaceMode(*req.LowSpaceMode) != "stopRecording" {
			s.setPausedForLowSpace(false)
		}

		cur := s.rec.GetConfig()
		if req.SegmentSec != nil && *req.SegmentSec > 0 {
			cur.Segment = time.Duration(*req.SegmentSec) * time.Second
		}
		if req.KeepDays != nil && *req.KeepDays >= 0 {
			cur.KeepDays = *req.KeepDays
		}
		if req.MinFreeGB != nil && *req.MinFreeGB >= 0 {
			cur.MinFreeGB = *req.MinFreeGB
		}
		s.rec.SetConfig(cur)

		go s.checkStorageOnce()

		writeJSON(w, 200, map[string]any{
			"ok":           true,
			"keepDays":     s.cfg.RecordKeepDays,
			"maxStorageGB": s.cfg.RecordMaxStorageGB,
			"minFreeGB":    s.cfg.RecordMinFreeGB,
			"autoClean":    s.cfg.RecordAutoClean,
			"lowSpaceMode": s.cfg.RecordLowSpaceMode,
			"segmentSec":   s.cfg.RecordSegment,
			"cacheGB":      s.cfg.TranscodeCacheGB,
		})
	default:
		writeErr(w, 405, "方法不允许")
	}
}
