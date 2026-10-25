package main

// 录像回放。
//
// ===== 为什么需要转码 =====
//
// 录像文件是 MKV 容器 + HEVC 编码（摄像头原生格式，-c copy 录制）。
// 浏览器兼容性：
//
//   Chrome  -> MKV 可以，HEVC 不行
//   Safari  -> MKV 不行，HEVC 可以
//   Firefox -> 都不行
//
// 所以回放必须转成 H.264 + MP4。实测（10 秒 2K 素材）：
//
//   libx264   6.43s  CPU 262%
//   h264_qsv  3.33s  CPU 200%   <- 文件输入时硬件编码是稳定的
//
// 注意：直播时硬件编码不稳定（pipe 输入），但文件输入没问题。
//
// ===== 接口 =====
//
//   GET  /api/recordings/dates?name=xxx        可用日期
//   GET  /api/recordings/list?name=&date=      文件列表（含时长）
//   GET  /api/recordings/play?name=&file=&t=   转码播放（t=起始秒）
//   GET  /api/recordings/thumb?name=&file=     缩略图
//   GET  /api/recordings/download?name=&file=  下载原文件
//   POST /api/recordings/delete                删除

import (
	"github.com/micam/micam/internal/record"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	livePlayMu        sync.Mutex
	activeLiveStreams int
)

// ffprobePath 找 ffprobe（与 ffmpeg 同目录）。
func (s *Server) ffprobePath() string {
	ff := s.ffmpegPath()
	// /usr/bin/ffmpeg -> /usr/bin/ffprobe
	dir := filepath.Dir(ff)
	cand := filepath.Join(dir, "ffprobe")
	if _, err := os.Stat(cand); err == nil {
		return cand
	}
	// 交给 PATH
	if p, err := exec.LookPath("ffprobe"); err == nil {
		return p
	}
	return "ffprobe"
}

// recRoot 当前活跃写入的录像根目录。
func (s *Server) recRoot() string {
	if s == nil || s.rec == nil {
		if s != nil && s.dataDir != "" {
			return filepath.Join(s.dataDir, "recordings")
		}
		if matches, err := filepath.Glob("/vol*/@appdata/micam/recordings"); err == nil && len(matches) > 0 {
			return matches[0]
		}
		return "/vol1/@appdata/micam/recordings"
	}
	cfg := s.rec.GetConfig()
	if cfg.Dir != "" {
		return cfg.Dir
	}
	return filepath.Join(s.dataDir, "recordings")
}

// recRoots 返回所有纳管录像根目录（当前活跃写入目录 + 历史配置目录 + 系统默认与检测到的各卷录像池）。
// 确保无论用户如何切换录像存储位置，历史录像全部统一聚合、永不丢失。
var (
	recRootsMu   sync.Mutex
	cachedRoots  []string
	recRootsTime time.Time
)

func (s *Server) recRoots() []string {
	recRootsMu.Lock()
	if len(cachedRoots) > 0 && time.Since(recRootsTime) < 30*time.Second {
		out := make([]string, len(cachedRoots))
		copy(out, cachedRoots)
		recRootsMu.Unlock()
		return out
	}
	recRootsMu.Unlock()

	var roots []string
	seen := make(map[string]bool)
	add := func(p string) {
		p = filepath.Clean(strings.TrimSpace(p))
		if p == "" || p == "." || p == "/" || seen[p] {
			return
		}
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			seen[p] = true
			roots = append(roots, p)
		}
	}

	// 1. 当前活跃写入目录（最高优先级）
	if s != nil && s.rec != nil {
		if d := s.rec.GetConfig().Dir; d != "" {
			add(d)
		}
	}
	if s != nil && s.cfg != nil && s.cfg.RecordDir != "" {
		add(s.cfg.RecordDir)
	}

	// 2. 配置中记录的历史录像目录
	if s != nil && s.cfg != nil {
		for _, h := range s.cfg.GetHistoricalRecordDirs() {
			add(h)
		}
	}

	// 3. 系统默认 appdata 录像目录
	if s != nil && s.dataDir != "" {
		add(filepath.Join(s.dataDir, "recordings"))
	}
	if matches, err := filepath.Glob("/vol*/@appdata/micam/recordings"); err == nil {
		for _, m := range matches {
			add(m)
		}
	}

	// 4. 自动纳管已存在的飞牛摄像头录像目录（/vol02/...、/vol1/... 等），要求必须含有真实有效录像
	if matches, err := filepath.Glob("/vol*/*/recordings"); err == nil {
		for _, m := range matches {
			if scanRecDirUsage(m).Files > 0 {
				add(m)
			}
		}
	}
	if matches, err := filepath.Glob("/vol*/*/*小米*"); err == nil {
		for _, m := range matches {
			if scanRecDirUsage(m).Files > 0 {
				add(m)
			}
		}
	}
	if matches, err := filepath.Glob("/vol*/*/*摄像头*"); err == nil {
		for _, m := range matches {
			if scanRecDirUsage(m).Files > 0 {
				add(m)
			}
		}
	}

	if len(roots) == 0 {
		roots = []string{s.recRoot()}
	}

	recRootsMu.Lock()
	cachedRoots = roots
	recRootsTime = time.Now()
	recRootsMu.Unlock()

	return roots
}

// safeName 目录/文件名安全化（防路径穿越）。
func safeName(s string) string {
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	s = strings.ReplaceAll(s, "..", "_")
	return s
}

// ---- 元数据缓存（时长）----

type recMeta struct {
	Duration float64 `json:"duration"`
	Size     int64   `json:"size"`
	ModTime  int64   `json:"modTime"`
}

var (
	metaMu    sync.Mutex
	metaCache = map[string]recMeta{}
	metaFile  string
)

func (s *Server) metaFilePath() string {
	if s != nil && s.dataDir != "" {
		return filepath.Join(s.dataDir, "rec_meta.json")
	}
	return "/vol1/@appdata/micam/rec_meta.json"
}

func loadRecMeta(root string) {
	if metaFile == "" {
		metaFile = "/vol1/@appdata/micam/rec_meta.json"
	}
	if raw, err := os.ReadFile(metaFile); err == nil {
		metaMu.Lock()
		_ = json.Unmarshal(raw, &metaCache)
		metaMu.Unlock()
	}
	if root != "" {
		legacy := filepath.Join(root, ".meta.json")
		if raw, err := os.ReadFile(legacy); err == nil {
			var legacyMap map[string]recMeta
			if err := json.Unmarshal(raw, &legacyMap); err == nil {
				metaMu.Lock()
				for k, v := range legacyMap {
					if _, exists := metaCache[k]; !exists {
						metaCache[k] = v
					}
				}
				metaMu.Unlock()
			}
		}
	}
}

func saveRecMeta() {
	metaMu.Lock()
	raw, err := json.Marshal(metaCache)
	path := metaFile
	metaMu.Unlock()
	if err == nil && path != "" {
		_ = os.WriteFile(path, raw, 0o644)
	}
}

// probeDuration 取时长（带元数据缓存与智能码率估算，0毫秒极速响应，绝不阻塞任何 HTTP 请求）。
func (s *Server) probeDuration(path string) float64 {
	fi, err := os.Stat(path)
	if err != nil || fi.Size() < 64*1024 {
		return 0
	}

	metaMu.Lock()
	m, ok := metaCache[path]
	metaMu.Unlock()
	if ok && m.Size == fi.Size() && m.ModTime == fi.ModTime().Unix() && m.Duration > 0 {
		return m.Duration
	}

	maxDur := 300.0
	if s != nil && s.rec != nil {
		if seg := s.rec.GetConfig().Segment.Seconds(); seg > 0 {
			maxDur = seg
		}
	}

	dur := float64(fi.Size()) / (260 * 1024)
	if dur > maxDur {
		dur = maxDur
	} else if dur < 3.0 {
		dur = 3.0
	}

	metaMu.Lock()
	metaCache[path] = recMeta{Duration: dur, Size: fi.Size(), ModTime: fi.ModTime().Unix()}
	metaMu.Unlock()

	// 对本地 SSD 录像异步后台精确探测，不阻断当前请求
	if strings.HasPrefix(path, "/vol1/") && !strings.HasPrefix(path, "/vol02/") {
		go func(pth string, sz, mt int64) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			out, perr := exec.CommandContext(ctx, s.ffprobePath(), "-v", "error",
				"-show_entries", "format=duration",
				"-of", "csv=p=0", pth).Output()
			if perr == nil {
				if realDur, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64); realDur > 0 {
					metaMu.Lock()
					metaCache[pth] = recMeta{Duration: realDur, Size: sz, ModTime: mt}
					metaMu.Unlock()
					go saveRecMeta()
				}
			}
		}(path, fi.Size(), fi.ModTime().Unix())
	}

	return dur
}

// ---- 日期列表 ----

// handleRecDates 返回可用日期（跨所有纳管录像池聚合并去重）。
var (
	autoRemuxMu       sync.Mutex
	autoRemuxActive   bool
	autoMigrateMu     sync.Mutex
	autoMigrateActive bool
)

// triggerAutoMigrate 异步扫描并自动将跨日期错位的录像文件迁移至正确日期目录
func (s *Server) triggerAutoMigrate(root string) {
	if root == "" {
		return
	}
	go func() {
		autoMigrateMu.Lock()
		if autoMigrateActive {
			autoMigrateMu.Unlock()
			return
		}
		autoMigrateActive = true
		autoMigrateMu.Unlock()

		defer func() {
			autoMigrateMu.Lock()
			autoMigrateActive = false
			autoMigrateMu.Unlock()
		}()

		devEntries, err := os.ReadDir(root)
		if err != nil {
			return
		}
		for _, devEnt := range devEntries {
			if !devEnt.IsDir() || strings.HasPrefix(devEnt.Name(), ".") || strings.HasPrefix(devEnt.Name(), "@") {
				continue
			}
			devPath := filepath.Join(root, devEnt.Name())
			dateEntries, err := os.ReadDir(devPath)
			if err != nil {
				continue
			}
			for _, dateEnt := range dateEntries {
				if !dateEnt.IsDir() || strings.HasPrefix(dateEnt.Name(), ".") {
					continue
				}
				curDateDir := dateEnt.Name()
				datePath := filepath.Join(devPath, curDateDir)
				files, err := os.ReadDir(datePath)
				if err != nil {
					continue
				}
				for _, f := range files {
					if f.IsDir() || !strings.HasSuffix(f.Name(), ".mkv") {
						continue
					}
					base := strings.TrimSuffix(f.Name(), ".mkv")
					if len(base) >= 8 {
						if t, err := time.ParseInLocation("20060102", base[:8], time.Local); err == nil {
							actualDate := t.Format("2006-01-02")
							if actualDate != curDateDir {
								targetDir := filepath.Join(devPath, actualDate)
								_ = os.MkdirAll(targetDir, 0o755)
								srcPath := filepath.Join(datePath, f.Name())
								dstPath := filepath.Join(targetDir, f.Name())
								if err := os.Rename(srcPath, dstPath); err == nil {
									logf("[replay] 自动修正错位录像: %s/%s/%s -> %s", devEnt.Name(), curDateDir, f.Name(), actualDate)
								}
							}
						}
					}
				}
			}
		}
	}()
}

// triggerAutoRemux 异步触发孤立码流修复（后台排队并发防护，绝不阻塞任何 HTTP 请求）
func (s *Server) triggerAutoRemux(root string) {
	if root == "" {
		return
	}
	go func() {
		autoRemuxMu.Lock()
		if autoRemuxActive {
			autoRemuxMu.Unlock()
			return
		}
		autoRemuxActive = true
		autoRemuxMu.Unlock()

		defer func() {
			autoRemuxMu.Lock()
			autoRemuxActive = false
			autoRemuxMu.Unlock()
		}()

		count := 0
		_ = filepath.Walk(root, func(pth string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() {
				return nil
			}
			if count >= 5 {
				return filepath.SkipAll
			}
			ext := strings.ToLower(filepath.Ext(pth))
			if ext != ".265" && ext != ".264" {
				return nil
			}
			if time.Since(info.ModTime()) < 15*time.Second {
				return nil
			}
			if info.Size() < 1024 {
				_ = os.Remove(pth)
				return nil
			}
			dst := strings.TrimSuffix(pth, ext) + ".mkv"
			if fi, err := os.Stat(dst); err == nil && fi.Size() > 1024 {
				_ = os.Remove(pth)
				return nil
			}
			inFmt := "hevc"
			if ext == ".264" {
				inFmt = "h264"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			tmpDst := filepath.Join(os.TempDir(), filepath.Base(dst)+fmt.Sprintf(".%d.mkv", time.Now().UnixNano()))
			cmd := exec.CommandContext(ctx, s.ffmpegPath(), "-hide_banner", "-loglevel", "error", "-y",
				"-f", inFmt, "-i", pth, "-c", "copy", tmpDst)
			if err := cmd.Run(); err == nil {
				if fi, e := os.Stat(tmpDst); e == nil && fi.Size() > 1024 {
					if data, rerr := os.ReadFile(tmpDst); rerr == nil {
						if werr := os.WriteFile(dst, data, 0o644); werr == nil {
							_ = os.Remove(pth)
							count++
							logf("[replay] 后台自动封装孤立录像完成: %s -> %s (%.2fMB)",
								filepath.Base(pth), filepath.Base(dst), float64(fi.Size())/(1<<20))
						}
					}
				}
			}
			_ = os.Remove(tmpDst)
			return nil
		})
	}()
}

func (s *Server) handleRecDates(w http.ResponseWriter, r *http.Request) {
	name := safeName(r.URL.Query().Get("name"))
	roots := s.recRoots()
	for _, root := range roots {
		loadRecMeta(root)
		s.triggerAutoRemux(root)
		s.triggerAutoMigrate(root)
	}

	type dayInfo struct {
		Date   string  `json:"date"`
		Count  int     `json:"count"`
		SizeMB float64 `json:"sizeMB"`
	}
	var out []dayInfo

	agg := map[string]*dayInfo{}
	seenFiles := map[string]bool{}

	for _, root := range roots {
		var dirs []string
		if name != "" {
			dirs = []string{filepath.Join(root, name)}
		} else {
			entries, _ := os.ReadDir(root)
			for _, e := range entries {
				if e.IsDir() && !strings.HasPrefix(e.Name(), ".") && !strings.HasPrefix(e.Name(), "@") {
					dirs = append(dirs, filepath.Join(root, e.Name()))
				}
			}
		}

		for _, d := range dirs {
			entries, err := os.ReadDir(d)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
					continue
				}
				date := e.Name()
				if agg[date] == nil {
					agg[date] = &dayInfo{Date: date}
				}
				files, _ := os.ReadDir(filepath.Join(d, date))
				for _, f := range files {
					if f.IsDir() {
						continue
					}
					ext := strings.ToLower(filepath.Ext(f.Name()))
					if ext != ".mkv" && ext != ".mp4" && ext != ".265" && ext != ".264" {
						continue
					}
					clipDate := date
					base := strings.TrimSuffix(f.Name(), ".mkv")
					if len(base) >= 8 {
						if t, err := time.ParseInLocation("20060102", base[:8], time.Local); err == nil {
							clipDate = t.Format("2006-01-02")
						}
					}
					if agg[clipDate] == nil {
						agg[clipDate] = &dayInfo{Date: clipDate}
					}
					fKey := clipDate + "/" + f.Name()
					if seenFiles[fKey] {
						continue
					}
					seenFiles[fKey] = true
					agg[clipDate].Count++
					if info, err := f.Info(); err == nil {
						agg[clipDate].SizeMB += float64(info.Size()) / (1 << 20)
					}
				}
			}
		}
	}

	for _, v := range agg {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date > out[j].Date })

	writeJSON(w, 200, map[string]any{"dates": out})
}

// ---- 文件列表 ----

// handleRecList 返回某设备某日的录像文件。
// recFileInfo 一个录像文件
type recFileInfo struct {
	Name      string  `json:"name"`
	Date      string  `json:"date"`
	SizeMB    float64 `json:"sizeMB"`
	Duration  float64 `json:"duration"`
	StartTime string  `json:"startTime"`
	URL       string  `json:"url"`
	ThumbURL  string  `json:"thumbUrl"`
	Dev       string  `json:"dev,omitempty"`
}

// listRecDevices 列出有录像的设备目录名。
func listRecDevices(root string) []string {
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	return out
}

func (s *Server) handleRecList(w http.ResponseWriter, r *http.Request) {
	name := safeName(r.URL.Query().Get("name"))
	date := safeName(r.URL.Query().Get("date"))
	roots := s.recRoots()
	for _, root := range roots {
		loadRecMeta(root)
		s.triggerAutoRemux(root)
	}

	// name 为空 = 列出**所有设备**的录像。
	if name == "" {
		devMap := make(map[string]bool)
		for _, root := range roots {
			for _, n := range listRecDevices(root) {
				devMap[n] = true
			}
		}
		var names []string
		for n := range devMap {
			names = append(names, n)
		}
		sort.Strings(names)

		var all []recFileInfo
		seen := make(map[string]bool)
		for _, n := range names {
			for _, root := range roots {
				for _, f := range s.collectRecFiles(root, n, date) {
					if seen[f.Name] {
						continue
					}
					seen[f.Name] = true
					all = append(all, f)
				}
			}
		}
		sort.Slice(all, func(i, j int) bool { return all[i].StartTime > all[j].StartTime })
		var totalMB float64
		for _, f := range all {
			totalMB += f.SizeMB
		}
		writeJSON(w, 200, map[string]any{
			"count": len(all), "date": date, "files": all,
			"devs": names, "totalMB": totalMB,
		})
		return
	}

	var out []recFileInfo
	seen := make(map[string]bool)
	for _, root := range roots {
		for _, f := range s.collectRecFiles(root, name, date) {
			if seen[f.Name] {
				continue
			}
			seen[f.Name] = true
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartTime < out[j].StartTime })
	var totalMB float64
	for _, f := range out {
		totalMB += f.SizeMB
	}
	writeJSON(w, 200, map[string]any{
		"name": name, "date": date, "files": out,
		"count": len(out), "totalMB": totalMB,
	})
}

// collectRecFiles 收集某设备（可带日期）下的录像文件。
func (s *Server) collectRecFiles(root, name, date string) []recFileInfo {
	devDir := filepath.Join(root, name)
	if _, err := os.Stat(devDir); err != nil && name != "" {
		if fi, err := os.Stat(filepath.Join(root, sanitizeName(name))); err == nil && fi.IsDir() {
			devDir = filepath.Join(root, sanitizeName(name))
		}
	}
	walkTarget := devDir
	if date != "" {
		dateDir := filepath.Join(devDir, date)
		if fi, err := os.Stat(dateDir); err == nil && fi.IsDir() {
			walkTarget = dateDir
		}
	}

	var out []recFileInfo
	_ = filepath.Walk(walkTarget, func(pth string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(pth))
		if ext != ".mkv" && ext != ".mp4" && ext != ".265" && ext != ".264" {
			return nil
		}
		if info.Size() < 1024 {
			return nil
		}
		if strings.Contains(info.Name(), ".tmp.") || strings.HasPrefix(info.Name(), ".") {
			return nil
		}
		rel, _ := filepath.Rel(root, pth)
		parts := strings.Split(rel, string(filepath.Separator))
		st := info.ModTime().Format("2006-01-02 15:04:05")
		base := strings.TrimSuffix(strings.TrimSuffix(info.Name(), ".mkv"), ".mp4")
		fileDate := ""
		if len(base) >= 8 {
			if t, err := time.ParseInLocation("20060102", base[:8], time.Local); err == nil {
				fileDate = t.Format("2006-01-02")
			}
		}
		if len(base) >= 15 {
			if t, err := time.ParseInLocation("20060102_150405", base[:15], time.Local); err == nil {
				st = t.Format("2006-01-02 15:04:05")
			}
		}
		fi := recFileInfo{
			Name:      info.Name(),
			SizeMB:    float64(info.Size()) / (1 << 20),
			Duration:  s.probeDuration(pth),
			StartTime: st,
			Dev:       name,
		}
		if fileDate != "" {
			fi.Date = fileDate
		} else if len(parts) >= 2 {
			fi.Date = parts[len(parts)-2]
		}
		if date != "" && fi.Date != date {
			return nil
		}
		q := "name=" + urlEncode(name) + "&file=" + urlEncode(fi.Name) + "&date=" + urlEncode(fi.Date)
		fi.URL = "/api/recordings/play?" + q
		fi.ThumbURL = "/api/recordings/thumb?" + q
		out = append(out, fi)
		return nil
	})
	return out
}

func urlEncode(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, " ", "%20"), "&", "%26")
}

// ---- 播放（转码）----

// handleRecPlay 把录像转码成 H.264 MP4 流式输出。
//
// 参数：
//
//	name  设备名
//	date  日期目录（可空，会自动找）
//	file  文件名
//	t     起始秒数（可选，用于拖动进度）
func (s *Server) handleRecPlay(w http.ResponseWriter, r *http.Request) {
	name := safeName(r.URL.Query().Get("name"))
	date := safeName(r.URL.Query().Get("date"))
	file := safeName(r.URL.Query().Get("file"))
	start, err := strconv.ParseFloat(r.URL.Query().Get("t"), 64)
	if err != nil || math.IsNaN(start) || math.IsInf(start, 0) || start < 0 {
		start = 0
	}
	nocache := r.URL.Query().Get("nocache") == "1"

	src, err := s.findRecFile(name, date, file)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}

	// 选编码器（实测可用，进程内缓存结果）
	enc, encArgs := s.pickFileEncoder(src)

	if date == "" || name == "" {
		for _, r := range s.recRoots() {
			if strings.HasPrefix(src, r) {
				rel, _ := filepath.Rel(r, src)
				parts := strings.Split(rel, string(filepath.Separator))
				if len(parts) >= 3 {
					name = parts[0]
					date = parts[1]
				}
				break
			}
		}
	}

	// 跨录像池自动定位缓存
	dst := s.findCachePath(enc, name, date, file, src)
	if nocache {
		_ = os.Remove(dst)
	}
	if !nocache && cacheValid(dst, src) {
		logf("[replay] 缓存命中 %s", filepath.Base(dst))
		_ = os.Chtimes(dst, time.Now(), time.Now())
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		http.ServeFile(w, r, dst) // ServeFile 自带 Range 支持
		return
	}

	// 未命中：转码，同时（start==0 时）在后台写缓存
	args := []string{
		"-hide_banner", "-loglevel", "error",
	}
	if strings.HasSuffix(enc, "_qsv") {
		args = append(args, "-hwaccel", "qsv", "-hwaccel_output_format", "qsv")
	}
	args = append(args,
		"-threads", "0",
		"-fflags", "+genpts",
		"-avoid_negative_ts", "make_zero",
	)
	if start > 0 {
		// -ss 放在 -i 前面 -> 快速定位到关键帧
		args = append(args, "-ss", fmt.Sprintf("%.2f", start))
	}
	args = append(args, "-i", src)
	args = append(args, encArgs...)
	// 极致首帧提速：0 B帧（无重排延迟，首帧即刻解码上屏）+ 1秒一个关键帧 + 300ms 快速切片刷新！
	args = append(args,
		"-bf", "0",
		"-g", "25",
		"-frag_duration", "300000",
		"-flush_packets", "1",
		"-an",
		"-movflags", "frag_keyframe+empty_moov+default_base_moof",
		"-f", "mp4", "pipe:1",
	)

	// 客户端连接级 Context：只要浏览器断开连接、切换片段或刷新，立即取消 Context
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	livePlayMu.Lock()
	activeLiveStreams++
	livePlayMu.Unlock()

	// 触发目标文件静态缓存生成（最高优先级），为后续秒开与拖动无缝升格
	if !cacheValid(dst, src) {
		go s.buildCache(src, dst, encArgs, enc, file, true)
	}

	cacheJobMu.Lock()
	for k, j := range activeCacheJobs {
		// 绝不杀掉当前正在播放的目标文件的缓存生成任务
		if k != dst && !j.isUser && j.cmd != nil && j.cmd.Process != nil {
			logf("[replay] 实时流开始，中断后台闲时缓存任务让路 GPU: %s", filepath.Base(k))
			_ = j.cmd.Process.Kill()
			delete(activeCacheJobs, k)
		}
	}
	cacheJobMu.Unlock()

	defer func() {
		livePlayMu.Lock()
		activeLiveStreams--
		stillStreaming := activeLiveStreams > 0
		livePlayMu.Unlock()

		if !stillStreaming && !cacheValid(dst, src) {
			time.AfterFunc(2*time.Second, func() {
				livePlayMu.Lock()
				active := activeLiveStreams > 0
				livePlayMu.Unlock()
				if !active && !cacheValid(dst, src) {
					go s.buildCache(src, dst, encArgs, enc, file, false)
				}
			})
		}
	}()

	makeCmd := func(seekSec float64) (*exec.Cmd, io.ReadCloser, *strings.Builder, error) {
		cmdArgs := []string{"-hide_banner", "-loglevel", "error"}
		if strings.HasSuffix(enc, "_qsv") {
			cmdArgs = append(cmdArgs, "-hwaccel", "qsv", "-hwaccel_output_format", "qsv")
		}
		cmdArgs = append(cmdArgs, "-threads", "0", "-fflags", "+genpts", "-avoid_negative_ts", "make_zero")
		if seekSec > 0 {
			cmdArgs = append(cmdArgs, "-ss", fmt.Sprintf("%.2f", seekSec))
		}
		cmdArgs = append(cmdArgs, "-i", src)
		cmdArgs = append(cmdArgs, encArgs...)
		cmdArgs = append(cmdArgs,
			"-bf", "0",
			"-g", "25",
			"-frag_duration", "300000",
			"-an",
			"-movflags", "frag_keyframe+empty_moov+default_base_moof",
			"-f", "mp4", "pipe:1",
		)
		c := exec.CommandContext(ctx, s.ffmpegPath(), cmdArgs...)
		p, err := c.StdoutPipe()
		if err != nil {
			return nil, nil, nil, err
		}
		var eb strings.Builder
		c.Stderr = &eb
		if err := c.Start(); err != nil {
			return nil, nil, nil, err
		}
		return c, p, &eb, nil
	}

	cmd, stdout, errBuf, err := makeCmd(start)
	if err != nil {
		writeErr(w, 500, "启动转码失败: "+err.Error())
		return
	}

	var initialBuf bytes.Buffer
	chunk := make([]byte, 16*1024)
	hasMdat := false

	readInitial := func(r io.Reader) {
		initialBuf.Reset()
		hasMdat = false
		for {
			nr, rerr := r.Read(chunk)
			if nr > 0 {
				initialBuf.Write(chunk[:nr])
				if bytes.Contains(initialBuf.Bytes(), []byte("mdat")) {
					hasMdat = true
					break
				}
			}
			if rerr != nil {
				break
			}
			if initialBuf.Len() > 256*1024 {
				break
			}
		}
	}

	readInitial(stdout)

	// 关键兜底：如果带偏移寻址因超出实际文件尾部或关键帧损坏而无视频帧（未见 mdat 媒体数据），立即无缝降级到 0 秒起播
	if !hasMdat && start > 0 {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		logf("[replay] 偏移 t=%.1f 无媒体数据(未见 mdat，仅 %d 字节)，自动回退到 0 秒起播: %s", start, initialBuf.Len(), file)
		cmd, stdout, errBuf, err = makeCmd(0)
		if err != nil {
			writeErr(w, 500, "回退转码失败: "+err.Error())
			return
		}
		readInitial(stdout)
	}

	if !hasMdat && initialBuf.Len() < 1024 {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if enc != "libx264" {
			logf("[replay] 硬件编码器 %s 未产出有效数据，自动降级为 libx264 软件编码: %s", enc, file)
			enc = "libx264"
			encArgs = encoderArgs("libx264")
			cmd, stdout, errBuf, err = makeCmd(start)
			if err == nil {
				readInitial(stdout)
				if !hasMdat && start > 0 {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
					cmd, stdout, errBuf, err = makeCmd(0)
					if err == nil {
						readInitial(stdout)
					}
				}
			}
		}
	}

	if !hasMdat && initialBuf.Len() < 1024 {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		msg := errBuf.String()
		logf("[replay] 转码无可用视频流: %s, stderr=%s", file, msg)
		writeErr(w, 500, "视频文件损坏或无可用画面")
		return
	}

	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if errBuf.Len() > 0 {
			msg := errBuf.String()
			if len(msg) > 300 {
				msg = msg[len(msg)-300:]
			}
			trimmed := strings.TrimSpace(msg)
			if trimmed != "" && !strings.Contains(trimmed, "va_openDriver() returns 0") {
				logf("[replay] ffmpeg stderr: %s", trimmed)
			}
		}
	}()

	logf("[replay] 转码首包就绪 %s (t=%.1f, %s, 首分片=%d 字节)", file, start, enc, initialBuf.Len())

	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	if initialBuf.Len() > 0 {
		_, _ = w.Write(initialBuf.Bytes())
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	buf := make([]byte, 32*1024)
	for {
		nr, rerr := stdout.Read(buf)
		if nr > 0 {
			if _, werr := w.Write(buf[:nr]); werr != nil {
				break
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if rerr != nil {
			break
		}
	}
}

type cacheJob struct {
	dst     string
	isUser  bool
	started time.Time
	cmd     *exec.Cmd
}

var (
	cacheJobMu      sync.Mutex
	activeCacheJobs = make(map[string]*cacheJob)
	userActiveUntil time.Time
)

// buildCache 把文件转码成缓存 MP4。
// isUser 表示是否为用户交互（点击回放/时间轴）触发。
func (s *Server) buildCache(src, dst string, encArgs []string, enc, label string, isUser bool) {
	cacheJobMu.Lock()
	if isUser {
		userActiveUntil = time.Now().Add(60 * time.Second)
		// 1. 如果后台有闲时任务在跑，立即杀掉后台任务，为用户腾出 100% GPU
		for k, j := range activeCacheJobs {
			if !j.isUser && j.cmd != nil && j.cmd.Process != nil {
				logf("[cache] 用户交互请求 %s，中断让路后台任务 %s", label, filepath.Base(k))
				_ = j.cmd.Process.Kill()
				delete(activeCacheJobs, k)
			}
		}
		// 2. 如果之前有旧的用户请求，且该任务才刚启动不久(<10s)，杀掉旧任务；若已跑了较长时间(接近完成)，允许其跑完避免浪费已转进度
		for k, j := range activeCacheJobs {
			if k != dst && j.isUser && j.cmd != nil && j.cmd.Process != nil {
				if time.Since(j.started) < 10*time.Second {
					logf("[cache] 用户切换片段 %s，中断刚启动的旧任务 %s", label, filepath.Base(k))
					_ = j.cmd.Process.Kill()
					delete(activeCacheJobs, k)
				}
			}
		}
	} else {
		// 后台预热：只要已有任务在跑，避让放弃
		if len(activeCacheJobs) >= 1 {
			cacheJobMu.Unlock()
			return
		}
	}

	if activeCacheJobs[dst] != nil {
		cacheJobMu.Unlock()
		return
	}

	job := &cacheJob{dst: dst, isUser: isUser, started: time.Now()}
	activeCacheJobs[dst] = job
	cacheJobMu.Unlock()

	defer func() {
		cacheJobMu.Lock()
		delete(activeCacheJobs, dst)
		cacheJobMu.Unlock()
	}()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return
	}
	tmp := fmt.Sprintf("%s.tmp.%d", dst, time.Now().UnixNano())
	defer os.Remove(tmp)

	t0 := time.Now()
	args := []string{
		"-hide_banner", "-loglevel", "error",
	}
	if strings.HasSuffix(enc, "_qsv") {
		args = append(args, "-hwaccel", "qsv", "-hwaccel_output_format", "qsv")
	}
	args = append(args,
		"-fflags", "+genpts",
		"-avoid_negative_ts", "make_zero",
		"-threads", "0",
		"-y", "-i", src,
	)
	args = append(args, encArgs...)
	args = append(args,
		"-an",
		"-movflags", "+faststart",
		"-f", "mp4",
		tmp,
	)

	cmd := exec.Command(s.ffmpegPath(), args...)
	job.cmd = cmd

	out, err := cmd.CombinedOutput()
	if err != nil || !hasValidMP4Structure(tmp) {
		if enc != "libx264" {
			logf("[cache] 硬件转码未生成有效 MP4，自动降级为 libx264 软件重试: %s", label)
			softArgs := encoderArgs("libx264")
			softCmdArgs := []string{
				"-hide_banner", "-loglevel", "error",
				"-fflags", "+genpts",
				"-avoid_negative_ts", "make_zero",
				"-threads", "0",
				"-y", "-i", src,
			}
			softCmdArgs = append(softCmdArgs, softArgs...)
			softCmdArgs = append(softCmdArgs,
				"-an",
				"-movflags", "+faststart",
				"-f", "mp4",
				tmp,
			)
			cmd = exec.Command(s.ffmpegPath(), softCmdArgs...)
			job.cmd = cmd
			out, err = cmd.CombinedOutput()
			enc = "libx264"
		}
	}

	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		logf("[cache] 转码结束 %s: %s", label, msg)
		return
	}

	if !hasValidMP4Structure(tmp) {
		logf("[cache] 生成失败 %s: 转码文件缺少有效 MP4 结构", label)
		return
	}
	if err := os.Rename(tmp, dst); err != nil {
		return
	}
	fi, _ := os.Stat(dst)
	var mb float64
	if fi != nil {
		mb = float64(fi.Size()) / (1 << 20)
	}
	logf("[cache] 已生成 %s (%.1fMB, %.1fs, %s)", label, mb, time.Since(t0).Seconds(), enc)

	s.evictCache()
}

// handleCacheStats 转码缓存状态。
//
// GET /api/recordings/cache
func (s *Server) handleCacheStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.cacheStats())
}

// handleCacheClear 清空转码缓存。
//
// POST /api/recordings/cache/clear
func (s *Server) handleCacheClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 POST")
		return
	}
	var freed int64
	var n int
	for _, root := range s.recRoots() {
		dir := filepath.Join(root, ".cache")
		_ = filepath.Walk(dir, func(pth string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() {
				return nil
			}
			if err := os.Remove(pth); err == nil {
				freed += info.Size()
				n++
			}
			return nil
		})
	}
	logf("[cache] 手动清理 %d 个文件，释放 %.1fGB", n, float64(freed)/(1<<30))
	writeJSON(w, 200, map[string]any{"ok": true, "count": n, "freedMB": float64(freed) / (1 << 20)})
}

// findRecFile 定位录像文件（支持不传 date）。
func (s *Server) findRecFile(name, date, file string) (string, error) {
	if file == "" {
		return "", fmt.Errorf("缺少 file")
	}
	if !strings.HasSuffix(file, ".mkv") {
		return "", fmt.Errorf("只支持 mkv")
	}

	roots := s.recRoots()
	for _, root := range roots {
		if name != "" {
			if date != "" {
				pth := filepath.Join(root, name, date, file)
				if _, err := os.Stat(pth); err == nil {
					return pth, nil
				}
			}
			// 遍历该设备所有日期
			base := filepath.Join(root, name)
			entries, _ := os.ReadDir(base)
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				pth := filepath.Join(base, e.Name(), file)
				if _, err := os.Stat(pth); err == nil {
					return pth, nil
				}
			}
		} else {
			devEntries, _ := os.ReadDir(root)
			for _, de := range devEntries {
				if !de.IsDir() || strings.HasPrefix(de.Name(), ".") {
					continue
				}
				devName := de.Name()
				if date != "" {
					pth := filepath.Join(root, devName, date, file)
					if _, err := os.Stat(pth); err == nil {
						return pth, nil
					}
				}
				entries, _ := os.ReadDir(filepath.Join(root, devName))
				for _, e := range entries {
					if !e.IsDir() {
						continue
					}
					pth := filepath.Join(root, devName, e.Name(), file)
					if _, err := os.Stat(pth); err == nil {
						return pth, nil
					}
				}
			}
		}
	}
	return "", fmt.Errorf("找不到文件: %s", file)
}

// ---- 缩略图 ----

// handleRecThumb 生成/返回缩略图（磁盘缓存）。
func (s *Server) handleRecThumb(w http.ResponseWriter, r *http.Request) {
	name := safeName(r.URL.Query().Get("name"))
	date := safeName(r.URL.Query().Get("date"))
	file := safeName(r.URL.Query().Get("file"))

	src, err := s.findRecFile(name, date, file)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}

	thumbDir := filepath.Join(s.dataDir, "thumbs", name)
	if s.dataDir == "" {
		thumbDir = filepath.Join(s.recRoot(), ".thumbs", name)
	}
	_ = os.MkdirAll(thumbDir, 0o755)
	dst := filepath.Join(thumbDir, file+".jpg")

	// 缓存命中（比源文件新）
	if di, err := os.Stat(dst); err == nil {
		if si, err := os.Stat(src); err == nil && di.ModTime().After(si.ModTime()) {
			serveJPEG(w, dst)
			return
		}
	}

	// 取第 1 秒的帧（避开开头黑屏）
	cmd := exec.Command(s.ffmpegPath(),
		"-hide_banner", "-loglevel", "error",
		"-ss", "1", "-i", src,
		"-frames:v", "1",
		"-vf", "scale=320:-2",
		"-f", "image2", "-c:v", "mjpeg",
		"-y", dst,
	)
	if err := cmd.Run(); err != nil {
		writeErr(w, 500, "生成缩略图失败")
		return
	}
	serveJPEG(w, dst)
}

func serveJPEG(w http.ResponseWriter, path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		writeErr(w, 404, "读取失败")
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(data)
}

// ---- 下载 ----

// handleRecDownload 下载原始 MKV 文件（支持 Range）。
func (s *Server) handleRecDownload(w http.ResponseWriter, r *http.Request) {
	name := safeName(r.URL.Query().Get("name"))
	date := safeName(r.URL.Query().Get("date"))
	file := safeName(r.URL.Query().Get("file"))

	src, err := s.findRecFile(name, date, file)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	w.Header().Set("Content-Type", "video/x-matroska")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+file+"\"")
	http.ServeFile(w, r, src)
}

// ---- 删除 ----

// handleRecDelete 删除单个录像文件。
func (s *Server) handleRecDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 POST")
		return
	}
	var req struct {
		Name string `json:"name"`
		Date string `json:"date"`
		File string `json:"file"`
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

	fi, _ := os.Stat(src)
	var freed int64
	if fi != nil {
		freed = fi.Size()
	}
	if err := os.Remove(src); err != nil {
		writeErr(w, 500, "删除失败: "+err.Error())
		return
	}

	// 清掉缩略图和元数据缓存
	_ = os.Remove(filepath.Join(s.recRoot(), ".thumbs", safeName(req.Name), safeName(req.File)+".jpg"))
	metaMu.Lock()
	delete(metaCache, src)
	metaMu.Unlock()
	go saveRecMeta()

	logf("[replay] 已删除 %s (%.1fMB)", req.File, float64(freed)/(1<<20))
	writeJSON(w, 200, map[string]any{"ok": true, "freedMB": float64(freed) / (1 << 20)})
}

var _ = time.Now

// ---- 录像策略配置 ----

// handleRecConfig 读写录像策略。
//
// GET  /api/recordings/config   读取
// POST /api/recordings/config   修改
//
// POST body（都可选）：
//
//	{"enabled":true,"segmentSec":300,"keepDays":7,"minFreeGB":10,"cacheGB":5}
func (s *Server) handleRecConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := s.rec.GetConfig()
		totalBytes, fileCount, _ := record.DirUsage(cfg.Dir)
		du := record.DiskUsageOf(cfg.Dir)
		writeJSON(w, 200, map[string]any{
			"enabled":           s.cfg.RecordEnabled,
			"mode":              s.cfg.RecordMode,
			"dir":               cfg.Dir,
			"segmentSec":        int(cfg.Segment.Seconds()),
			"keepDays":          s.cfg.RecordKeepDays,
			"maxStorageGB":      s.cfg.RecordMaxStorageGB,
			"minFreeGB":         s.cfg.RecordMinFreeGB,
			"autoClean":         s.cfg.RecordAutoClean,
			"lowSpaceMode":      s.cfg.RecordLowSpaceMode,
			"postRollSec":       s.cfg.RecordPostRollSec,
			"motionSensitivity": s.cfg.RecordMotionSensitivity,
			"cacheGB":           s.cfg.TranscodeCacheGB,
			"usedGB":            float64(totalBytes) / (1 << 30),
			"fileCount":         fileCount,
			"diskFreeGB":        du.FreeGB,
			"diskTotalGB":       du.TotalGB,
		})
		return

	case http.MethodPost:
		var req struct {
			Dir               *string `json:"dir"`
			Enabled           *bool   `json:"enabled"`
			Mode              *string `json:"mode"`
			SegmentSec        *int    `json:"segmentSec"`
			KeepDays          *int    `json:"keepDays"`
			MaxStorageGB      *int    `json:"maxStorageGB"`
			MinFreeGB         *int    `json:"minFreeGB"`
			AutoClean         *bool   `json:"autoClean"`
			LowSpaceMode      *string `json:"lowSpaceMode"`
			PostRollSec       *int    `json:"postRollSec"`
			MotionSensitivity *string `json:"motionSensitivity"`
			CacheGB           *int    `json:"cacheGB"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, "参数错误: "+err.Error())
			return
		}

		// 校验范围
		if req.SegmentSec != nil && (*req.SegmentSec < 10 || *req.SegmentSec > 3600) {
			writeErr(w, 400, "分片时长需在 10-3600 秒之间")
			return
		}
		if req.KeepDays != nil && (*req.KeepDays < 0 || *req.KeepDays > 365) {
			writeErr(w, 400, "保留天数需在 0-365 之间（0 表示不删除）")
			return
		}
		if req.MaxStorageGB != nil && (*req.MaxStorageGB < 0 || *req.MaxStorageGB > 100000) {
			writeErr(w, 400, "存储上限需在 0-100000 GB 之间（0 表示不限）")
			return
		}
		if req.MinFreeGB != nil && (*req.MinFreeGB < 0 || *req.MinFreeGB > 10000) {
			writeErr(w, 400, "磁盘最低保留需在 0-10000 GB 之间")
			return
		}
		if req.CacheGB != nil && (*req.CacheGB < 1 || *req.CacheGB > 500) {
			writeErr(w, 400, "缓存上限需在 1-500 GB 之间")
			return
		}

		if req.Dir != nil && *req.Dir != "" {
			clean := filepath.Clean(*req.Dir)
			if filepath.IsAbs(clean) && clean != "/" && filepath.Dir(clean) != "/" {
				if err := os.MkdirAll(clean, 0o755); err == nil && isWritable(clean) {
					s.cfg.SetRecordDir(clean)
					cur := s.rec.GetConfig()
					cur.Dir = clean
					s.rec.SetConfig(cur)
				}
			}
		}

		oldMode := s.cfg.RecordMode
		oldEnabled := s.cfg.RecordEnabled

		if err := s.cfg.SetRecordPolicy(req.Enabled, req.Mode, req.SegmentSec, req.KeepDays, req.MaxStorageGB, req.MinFreeGB, req.PostRollSec, req.CacheGB, req.AutoClean, req.LowSpaceMode, req.MotionSensitivity); err != nil {
			writeErr(w, 500, "保存失败: "+err.Error())
			return
		}

		// 同步到 rec 管理器
		cur := s.rec.GetConfig()
		if req.Enabled != nil {
			cur.Enabled = *req.Enabled
		}
		if req.SegmentSec != nil {
			cur.Segment = time.Duration(*req.SegmentSec) * time.Second
		}
		if req.KeepDays != nil {
			cur.KeepDays = *req.KeepDays
		}
		if req.MinFreeGB != nil {
			cur.MinFreeGB = *req.MinFreeGB
		}
		s.rec.SetConfig(cur)

		// 模式响应变更
		newMode := s.cfg.RecordMode
		newEnabled := s.cfg.RecordEnabled
		if newMode != oldMode || newEnabled != oldEnabled {
			logf("[rec] 录像模式切换: %s (enabled=%v) -> %s (enabled=%v)", oldMode, oldEnabled, newMode, newEnabled)
			if !newEnabled || newMode == "off" {
				s.stopAllRecordings()
			} else if newMode == "always" {
				s.startAllRecordings()
			} else if newMode == "motion" || newMode == "ai_event" {
				s.stopAllRecordings()
			}
		}

		writeJSON(w, 200, map[string]any{"ok": true, "enabled": s.cfg.RecordEnabled, "mode": s.cfg.RecordMode})
		return
	}
}

// handleRecBatchDelete 批量删除录像。
//
// POST /api/recordings/batch-delete
//
// body:
//
//	{"name":"设备名","before":"2026-09-14"}   删除该日期之前的
//	{"name":"设备名","date":"2026-09-14"}     删除该日期全部
//	{"files":[{"name":..,"date":..,"file":..}]}  指定文件列表
func (s *Server) handleRecBatchDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 POST")
		return
	}
	var req struct {
		Name   string `json:"name"`
		Date   string `json:"date"`
		Before string `json:"before"`
		Files  []struct {
			Name string `json:"name"`
			Date string `json:"date"`
			File string `json:"file"`
		} `json:"files"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "参数错误")
		return
	}

	var targets []string

	if len(req.Files) > 0 {
		for _, f := range req.Files {
			pth, err := s.findRecFile(safeName(f.Name), safeName(f.Date), safeName(f.File))
			if err == nil {
				targets = append(targets, pth)
			}
		}
	} else if req.Name != "" {
		name := safeName(req.Name)
		base := filepath.Join(s.recRoot(), name)

		_ = filepath.Walk(base, func(pth string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() {
				return nil
			}
			if filepath.Ext(pth) != ".mkv" {
				return nil
			}
			rel, _ := filepath.Rel(base, pth)
			parts := strings.Split(rel, string(filepath.Separator))
			dirDate := ""
			if len(parts) >= 2 {
				dirDate = parts[0]
			}

			// 按日期筛选
			if req.Date != "" && dirDate != req.Date {
				return nil
			}
			// 按「之前」筛选
			if req.Before != "" && dirDate >= req.Before {
				return nil
			}
			targets = append(targets, pth)
			return nil
		})
	}

	var freed int64
	var n int
	var failed []string

	for _, pth := range targets {
		fi, err := os.Stat(pth)
		if err != nil {
			continue
		}
		if err := os.Remove(pth); err == nil {
			freed += fi.Size()
			n++
		} else {
			// 记录失败原因（常见：文件属主不是服务用户）
			failed = append(failed, filepath.Base(pth)+": "+err.Error())
			logf("[replay] 删除失败 %s: %v", pth, err)
		}
	}

	logf("[replay] 批量删除 %d/%d 个文件，释放 %.1fMB", n, len(targets), float64(freed)/(1<<20))

	resp := map[string]any{
		"ok":      len(failed) == 0,
		"deleted": n,
		"total":   len(targets),
		"freedMB": float64(freed) / (1 << 20),
	}
	if len(failed) > 0 {
		if len(failed) > 3 {
			failed = failed[:3]
		}
		resp["failed"] = failed
		resp["msg"] = "部分文件删除失败（可能是权限问题）"
	}
	writeJSON(w, 200, resp)
}

// handleRecFrame 取指定时间点的帧（用于时间轴预览）。
//
// GET /api/recordings/frame?name=&date=&file=&t=30
//
// 与 thumb 的区别：
//
//	thumb  固定取第 1 秒，用于列表缩略图
//	frame  取任意时间，用于拖动预览
func (s *Server) handleRecFrame(w http.ResponseWriter, r *http.Request) {
	name := safeName(r.URL.Query().Get("name"))
	date := safeName(r.URL.Query().Get("date"))
	file := safeName(r.URL.Query().Get("file"))
	t, _ := strconv.ParseFloat(r.URL.Query().Get("t"), 64)
	if t < 0 {
		t = 0
	}
	w0, _ := strconv.Atoi(r.URL.Query().Get("w"))
	// 上限放到 1600：云端视觉模型看车牌/门牌需要高分辨率，
	// 而这是本地 ffmpeg 抽帧，不额外花钱。
	if w0 <= 0 || w0 > 1600 {
		w0 = 240
	}

	src, err := s.findRecFile(name, date, file)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}

	// 缓存：文件名带时间点（取整到秒，避免缓存爆炸）
	key := filepath.Base(file) + fmt.Sprintf(".t%d.w%d.jpg", int(t), w0)
	cacheDir := filepath.Join(s.dataDir, "frames", name)
	if s.dataDir == "" {
		cacheDir = filepath.Join(s.recRoot(), ".frames", name)
	}
	dst := filepath.Join(cacheDir, key)

	if fi, err := os.Stat(dst); err == nil && fi.Size() > 0 {
		serveJPEG(w, dst)
		return
	}

	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		writeErr(w, 500, "创建缓存目录失败")
		return
	}

	// -ss 放在 -i 前面 = 快速定位到关键帧
	cmd := exec.Command(s.ffmpegPath(),
		"-hide_banner", "-loglevel", "error",
		"-ss", fmt.Sprintf("%.2f", t),
		"-i", src,
		"-frames:v", "1",
		"-vf", fmt.Sprintf("scale=%d:-2", w0),
		"-f", "image2", "-c:v", "mjpeg",
		"-y", dst,
	)
	if err := cmd.Run(); err != nil {
		writeErr(w, 500, "取帧失败")
		return
	}
	serveJPEG(w, dst)
}

// handleRecFrameStrip 生成时间轴缩略图带（多帧拼成一张图）。
//
// GET /api/recordings/frame-strip?name=&date=&file=&n=10
//
// 返回一张横向拼接的 JPEG，前端按等分切片显示。
// 这样一次请求就能拿到整条时间轴的预览。
func (s *Server) handleRecFrameStrip(w http.ResponseWriter, r *http.Request) {
	name := safeName(r.URL.Query().Get("name"))
	date := safeName(r.URL.Query().Get("date"))
	file := safeName(r.URL.Query().Get("file"))
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n <= 0 || n > 30 {
		n = 10
	}

	src, err := s.findRecFile(name, date, file)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}

	cacheDir := filepath.Join(s.dataDir, "frames")
	if s.dataDir == "" {
		cacheDir = filepath.Join(s.recRoot(), ".frames")
	}
	_ = os.MkdirAll(cacheDir, 0o755)
	dst := filepath.Join(cacheDir, filepath.Base(file)+fmt.Sprintf(".strip%d.jpg", n))

	if fi, err := os.Stat(dst); err == nil && fi.Size() > 0 {
		if si, err2 := os.Stat(src); err2 == nil && fi.ModTime().After(si.ModTime()) {
			serveJPEG(w, dst)
			return
		}
	}

	dur := s.probeDuration(src)
	if dur <= 0 {
		writeErr(w, 500, "无法读取时长")
		return
	}

	// 均匀取 n 帧，横向拼接
	//
	// fps = n / dur 会取到 t=0, dur/n, 2*dur/n, ... (n-1)*dur/n
	// 最后一帧贴近末尾，视频结尾常有空洞 -> 出现黑边。
	// 所以留 8% 余量：fps = n / (dur * 0.92)
	fps := float64(n) / (dur * 0.92)

	// 太短的视频减少帧数（避免重复帧）
	if dur < float64(n)*1.5 {
		n = int(dur / 1.5)
		if n < 3 {
			n = 3
		}
		fps = float64(n) / (dur * 0.92)
	}
	cmd := exec.Command(s.ffmpegPath(),
		"-hide_banner", "-loglevel", "error",
		"-i", src,
		"-vf", fmt.Sprintf("fps=%.6f,scale=160:-2,tile=%dx1", fps, n),
		"-frames:v", "1",
		"-f", "image2", "-c:v", "mjpeg",
		"-y", dst,
	)
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errBuf.String())
		if len(msg) > 150 {
			msg = msg[:150]
		}
		writeErr(w, 500, "生成缩略图带失败: "+msg)
		return
	}
	serveJPEG(w, dst)
}


// startRecCacheWorker 后台自动把最新录像转码成快开 MP4 缓存。
func (s *Server) startRecCacheWorker() {
	go func() {
		time.Sleep(3 * time.Second)
		for {
			s.preCacheRecentRecordings()
			time.Sleep(25 * time.Second)
		}
	}()
}

func (s *Server) preCacheRecentRecordings() {
	root := s.recRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	today := time.Now().Format("2006-01-02")
	var dates []string
	for d := 0; d < 7; d++ {
		dates = append(dates, time.Now().AddDate(0, 0, -d).Format("2006-01-02"))
	}

	for _, devEntry := range entries {
		if !devEntry.IsDir() || strings.HasPrefix(devEntry.Name(), ".") {
			continue
		}
		devName := devEntry.Name()
		for _, d := range dates {
			dateDir := filepath.Join(root, devName, d)
			files, err := os.ReadDir(dateDir)
			if err != nil {
				continue
			}
			for i := len(files) - 1; i >= 0; i-- {
				f := files[i]
				if f.IsDir() || !strings.HasSuffix(f.Name(), ".mkv") {
					continue
				}
				info, err := f.Info()
				if err != nil || (d == today && time.Since(info.ModTime()) < 10*time.Second) {
					continue
				}
				src := filepath.Join(dateDir, f.Name())
				enc, encArgs := s.pickFileEncoder(src)
				dst := s.findCachePath(enc, devName, d, f.Name(), src)
				if !cacheValid(dst, src) {
					cacheJobMu.Lock()
					busy := len(activeCacheJobs) >= 1
					cacheJobMu.Unlock()
					if busy {
						time.Sleep(2 * time.Second)
						continue
					}
					s.buildCache(src, dst, encArgs, enc, f.Name(), false)
				}
			}
		}
	}
}

// handleRecCacheStatus 查询指定录像是否已生成快开 MP4 缓存，未就绪时自动触发后台极速转码。
func (s *Server) handleRecCacheStatus(w http.ResponseWriter, r *http.Request) {
	name := safeName(r.URL.Query().Get("name"))
	file := filepath.Base(r.URL.Query().Get("file"))
	date := safeName(r.URL.Query().Get("date"))
	nocache := r.URL.Query().Get("nocache") == "1"
	if name == "" || file == "" || date == "" {
		writeErr(w, 400, "缺少参数")
		return
	}
	src, err := s.findRecFile(name, date, file)
	if err != nil {
		writeErr(w, 404, "文件不存在")
		return
	}
	if date == "" || name == "" {
		rel, _ := filepath.Rel(s.recRoot(), src)
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) >= 3 {
			name = parts[0]
			date = parts[1]
		}
	}
	enc, encArgs := s.pickFileEncoder(src)
	dst := s.findCachePath(enc, name, date, file, src)
	if nocache {
		_ = os.Remove(dst)
	}
	ready := cacheValid(dst, src)

	cacheJobMu.Lock()
	userActiveUntil = time.Now().Add(60 * time.Second)
	_, building := activeCacheJobs[dst]
	cacheJobMu.Unlock()

	// 关键优化：只要用户点击或进入该片段，立刻在后台生成永久快开 MP4 缓存
	if !ready && !building {
		if _, err := os.Stat(src); err == nil {
			go s.buildCache(src, dst, encArgs, enc, file, true)
			building = true
		}
	}

	writeJSON(w, 200, map[string]any{
		"cached":   ready,
		"building": building,
	})
}
