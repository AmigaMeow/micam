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
	"encoding/json"
	"fmt"
	"io"
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

// recRoot 录像根目录。
func (s *Server) recRoot() string {
	cfg := s.rec.GetConfig()
	if cfg.Dir != "" {
		return cfg.Dir
	}
	return filepath.Join(s.dataDir, "recordings")
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

func loadRecMeta(root string) {
	metaFile = filepath.Join(root, ".meta.json")
	raw, err := os.ReadFile(metaFile)
	if err != nil {
		return
	}
	metaMu.Lock()
	_ = json.Unmarshal(raw, &metaCache)
	metaMu.Unlock()
}

func saveRecMeta() {
	metaMu.Lock()
	raw, _ := json.Marshal(metaCache)
	path := metaFile
	metaMu.Unlock()
	if path != "" {
		_ = os.WriteFile(path, raw, 0o644)
	}
}

// probeDuration 取时长（带缓存）。
func (s *Server) probeDuration(path string) float64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}

	metaMu.Lock()
	m, ok := metaCache[path]
	metaMu.Unlock()
	if ok && m.Size == fi.Size() && m.ModTime == fi.ModTime().Unix() {
		return m.Duration
	}

	d := 0.0

	// 先试 ffprobe
	out, err := exec.Command(s.ffprobePath(), "-v", "error",
		"-show_entries", "format=duration",
		"-of", "csv=p=0", path).Output()
	if err == nil {
		d, _ = strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	}

	// 兜底：解析 ffmpeg 的 stderr（没有 ffprobe 时）
	if d <= 0 {
		c := exec.Command(s.ffmpegPath(), "-hide_banner", "-i", path)
		var buf strings.Builder
		c.Stderr = &buf
		_ = c.Run()
		// 形如 "Duration: 00:00:32.95,"
		if i := strings.Index(buf.String(), "Duration: "); i >= 0 {
			s := buf.String()[i+10:]
			if j := strings.Index(s, ","); j > 0 {
				if parts := strings.Split(strings.TrimSpace(s[:j]), ":"); len(parts) == 3 {
					h, _ := strconv.ParseFloat(parts[0], 64)
					m, _ := strconv.ParseFloat(parts[1], 64)
					sec, _ := strconv.ParseFloat(parts[2], 64)
					d = h*3600 + m*60 + sec
				}
			}
		}
	}

	// 只有拿到有效时长才缓存（避免把失败结果缓存住）
	if d > 0 {
		metaMu.Lock()
		metaCache[path] = recMeta{Duration: d, Size: fi.Size(), ModTime: fi.ModTime().Unix()}
		metaMu.Unlock()
		go saveRecMeta()
	}
	return d
}

// ---- 日期列表 ----

// handleRecDates 返回可用日期。
func (s *Server) handleRecDates(w http.ResponseWriter, r *http.Request) {
	name := safeName(r.URL.Query().Get("name"))
	root := s.recRoot()
	loadRecMeta(root)

	type dayInfo struct {
		Date  string  `json:"date"`
		Count int     `json:"count"`
		SizeMB float64 `json:"sizeMB"`
	}
	var out []dayInfo

	var dirs []string
	if name != "" {
		dirs = []string{filepath.Join(root, name)}
	} else {
		entries, _ := os.ReadDir(root)
		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				dirs = append(dirs, filepath.Join(root, e.Name()))
			}
		}
	}

	agg := map[string]*dayInfo{}
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
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
				agg[date].Count++
				if info, err := f.Info(); err == nil {
					agg[date].SizeMB += float64(info.Size()) / (1 << 20)
				}
			}
		}
	}

	for _, v := range agg {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date > out[j].Date });

	writeJSON(w, 200, map[string]any{"dates": out})
}

// ---- 文件列表 ----

// handleRecList 返回某设备某日的录像文件。
func (s *Server) handleRecList(w http.ResponseWriter, r *http.Request) {
	name := safeName(r.URL.Query().Get("name"))
	date := safeName(r.URL.Query().Get("date"))
	if name == "" {
		writeErr(w, 400, "缺少 name");
		return
	}
	root := s.recRoot()
	loadRecMeta(root)

	dir := filepath.Join(root, name)
	if date != "" {
		dir = filepath.Join(dir, date)
	}

	type fileInfo struct {
		Name      string  `json:"name"`
		Date      string  `json:"date"`
		SizeMB    float64 `json:"sizeMB"`
		Duration  float64 `json:"duration"`
		StartTime string  `json:"startTime"`
		URL       string  `json:"url"`
		ThumbURL  string  `json:"thumbUrl"`
	}
	var out []fileInfo

	_ = filepath.Walk(dir, func(pth string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if filepath.Ext(pth) != ".mkv" {
			return nil
		}
		rel, _ := filepath.Rel(root, pth)
		parts := strings.Split(rel, string(filepath.Separator))
		fi := fileInfo{
			Name:    info.Name(),
			SizeMB:  float64(info.Size()) / (1 << 20),
			Duration: func() float64 { _ = rel; return s.probeDuration(pth) }(),
			StartTime: info.ModTime().Format("2006-01-02 15:04:05"),
		}
		if len(parts) >= 2 {
			fi.Date = parts[len(parts)-2]
		}
		q := "name=" + urlEncode(name) + "&file=" + urlEncode(fi.Name) + "&date=" + urlEncode(fi.Date)
		fi.URL = "/api/recordings/play?" + q
		fi.ThumbURL = "/api/recordings/thumb?" + q
		out = append(out, fi)
		return nil
	})

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name });

	var totalMB float64
	for _, f := range out {
		totalMB += f.SizeMB
	}

	writeJSON(w, 200, map[string]any{
		"name": name, "date": date, "files": out,
		"count": len(out), "totalMB": totalMB,
	})
}

func urlEncode(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, " ", "%20"), "&", "%26")
}

// ---- 播放（转码）----

// handleRecPlay 把录像转码成 H.264 MP4 流式输出。
//
// 参数：
//   name  设备名
//   date  日期目录（可空，会自动找）
//   file  文件名
//   t     起始秒数（可选，用于拖动进度）
func (s *Server) handleRecPlay(w http.ResponseWriter, r *http.Request) {
	name := safeName(r.URL.Query().Get("name"))
	date := safeName(r.URL.Query().Get("date"))
	file := safeName(r.URL.Query().Get("file"))
	start, _ := strconv.ParseFloat(r.URL.Query().Get("t"), 64)

	src, err := s.findRecFile(name, date, file)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}

	// 选编码器（实测可用，进程内缓存结果）
	enc, encArgs := s.pickFileEncoder(src)

	// 缓存命中 + 不需要偏移 -> 直接发文件（支持 Range，秒开可拖动）
	dst := s.cachePath(enc, name, date, file)
	if start == 0 && cacheValid(dst, src) {
		logf("[replay] 缓存命中 %s", filepath.Base(dst))
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		http.ServeFile(w, r, dst)   // ServeFile 自带 Range 支持
		return
	}

	// 未命中：转码，同时（start==0 时）在后台写缓存
	args := []string{"-hide_banner", "-loglevel", "error"}
	if start > 0 {
		// -ss 放在 -i 前面 -> 快速定位到关键帧
		args = append(args, "-ss", fmt.Sprintf("%.2f", start))
	}
	args = append(args, "-i", src)
	args = append(args, encArgs...)
	args = append(args,
		"-c:a", "aac", "-b:a", "64k",
		"-movflags", "frag_keyframe+empty_moov+default_base_moof",
		"-f", "mp4", "pipe:1",
	)

	cmd := exec.Command(s.ffmpegPath(), args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		writeErr(w, 500, "启动转码失败: "+err.Error())
		return
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if errBuf.Len() > 0 {
			msg := errBuf.String()
			if len(msg) > 300 {
				msg = msg[:300]
			}
			logf("[replay] ffmpeg: %s", strings.TrimSpace(msg))
		}
	}()

	logf("[replay] 转码 %s (t=%.1f, %s)", file, start, enc)

	// 顺便写缓存：起一个后台转码任务（不阻塞播放）
	if start == 0 && !cacheValid(dst, src) {
		go s.buildCache(src, dst, encArgs, enc, file)
	}

	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	_, _ = io.Copy(w, stdout)
}

// buildCache 后台把整个文件转码成缓存 MP4。
//
// 用临时文件 + 原子重命名，避免「转一半被杀」留下坏缓存。
func (s *Server) buildCache(src, dst string, encArgs []string, enc, label string) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return
	}
	tmp := dst + ".part"
	_ = os.Remove(tmp)

	t0 := time.Now()
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-i", src}
	args = append(args, encArgs...)
	args = append(args,
		"-c:a", "aac", "-b:a", "64k",
		"-movflags", "+faststart",
		"-f", "mp4",          // 必须显式指定：临时文件名是 .part，ffmpeg 猜不出格式
		tmp,
	)

	out, err := exec.Command(s.ffmpegPath(), args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		logf("[cache] 生成失败 %s: %s", label, msg)
		_ = os.Remove(tmp)
		return
	}

	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
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
	dir := s.transcodeCacheDir()
	var freed int64
	var n int
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
	logf("[cache] 手动清理 %d 个文件，释放 %.1fGB", n, float64(freed)/(1<<30))
	writeJSON(w, 200, map[string]any{"ok": true, "count": n, "freedMB": float64(freed) / (1 << 20)})
}

// findRecFile 定位录像文件（支持不传 date）。
func (s *Server) findRecFile(name, date, file string) (string, error) {
	root := s.recRoot()
	if name == "" || file == "" {
		return "", fmt.Errorf("缺少 name 或 file")
	}
	if !strings.HasSuffix(file, ".mkv") {
		return "", fmt.Errorf("只支持 mkv")
	}
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
		writeErr(w, 404, err.Error());
		return
	}

	root := s.recRoot()
	thumbDir := filepath.Join(root, ".thumbs", name)
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
		writeErr(w, 500, "生成缩略图失败");
		return
	}
	serveJPEG(w, dst)
}

func serveJPEG(w http.ResponseWriter, path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		writeErr(w, 404, "读取失败");
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
		writeErr(w, 404, err.Error());
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
		writeErr(w, 405, "只支持 POST");
		return
	}
	var req struct {
		Name string `json:"name"`
		Date string `json:"date"`
		File string `json:"file"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "参数错误");
		return
	}

	src, err := s.findRecFile(safeName(req.Name), safeName(req.Date), safeName(req.File))
	if err != nil {
		writeErr(w, 404, err.Error());
		return
	}

	fi, _ := os.Stat(src)
	var freed int64
	if fi != nil {
		freed = fi.Size()
	}
	if err := os.Remove(src); err != nil {
		writeErr(w, 500, "删除失败: "+err.Error());
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
//   {"enabled":true,"segmentSec":300,"keepDays":7,"minFreeGB":10,"cacheGB":5}
func (s *Server) handleRecConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := s.rec.GetConfig()
		writeJSON(w, 200, map[string]any{
			"enabled":    cfg.Enabled,
			"dir":        cfg.Dir,
			"segmentSec": int(cfg.Segment.Seconds()),
			"keepDays":   cfg.KeepDays,
			"minFreeGB":  cfg.MinFreeGB,
			"cacheGB":    s.cfg.TranscodeCacheGB,
		})
		return

	case http.MethodPost:
		var req struct {
			Enabled    *bool `json:"enabled"`
			SegmentSec *int  `json:"segmentSec"`
			KeepDays   *int  `json:"keepDays"`
			MinFreeGB  *int  `json:"minFreeGB"`
			CacheGB    *int  `json:"cacheGB"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, "参数错误")
			return
		}

		// 校验范围
		if req.SegmentSec != nil && (*req.SegmentSec < 10 || *req.SegmentSec > 3600) {
			writeErr(w, 400, "分片时长需在 10-3600 秒之间")
			return
		}
		if req.KeepDays != nil && (*req.KeepDays < 1 || *req.KeepDays > 365) {
			writeErr(w, 400, "保留天数需在 1-365 之间")
			return
		}
		if req.MinFreeGB != nil && (*req.MinFreeGB < 0 || *req.MinFreeGB > 10000) {
			writeErr(w, 400, "磁盘阈值需在 0-10000 GB 之间")
			return
		}
		if req.CacheGB != nil && (*req.CacheGB < 1 || *req.CacheGB > 500) {
			writeErr(w, 400, "缓存上限需在 1-500 GB 之间")
			return
		}

		// 写配置
		if err := s.cfg.SetRecordPolicy(req.Enabled, req.SegmentSec, req.KeepDays, req.MinFreeGB, req.CacheGB); err != nil {
			writeErr(w, 500, "保存失败: "+err.Error())
			return
		}

		// 立刻生效（更新录像管理器）
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

		logf("[rec] 策略已更新: enabled=%v segment=%ds keepDays=%d minFreeGB=%d cacheGB=%d",
			cur.Enabled, int(cur.Segment.Seconds()), cur.KeepDays, cur.MinFreeGB, s.cfg.TranscodeCacheGB)

		writeJSON(w, 200, map[string]any{"ok": true})
		return

	default:
		writeErr(w, 405, "方法不允许")
	}
}

// handleRecBatchDelete 批量删除录像。
//
// POST /api/recordings/batch-delete
//
// body:
//   {"name":"设备名","before":"2026-09-14"}   删除该日期之前的
//   {"name":"设备名","date":"2026-09-14"}     删除该日期全部
//   {"files":[{"name":..,"date":..,"file":..}]}  指定文件列表
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
//   thumb  固定取第 1 秒，用于列表缩略图
//   frame  取任意时间，用于拖动预览
func (s *Server) handleRecFrame(w http.ResponseWriter, r *http.Request) {
	name := safeName(r.URL.Query().Get("name"))
	date := safeName(r.URL.Query().Get("date"))
	file := safeName(r.URL.Query().Get("file"))
	t, _ := strconv.ParseFloat(r.URL.Query().Get("t"), 64)
	if t < 0 {
		t = 0
	}
	w0, _ := strconv.Atoi(r.URL.Query().Get("w"))
	if w0 <= 0 || w0 > 640 {
		w0 = 240
	}

	src, err := s.findRecFile(name, date, file)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}

	// 缓存：文件名带时间点（取整到秒，避免缓存爆炸）
	key := filepath.Base(file) + fmt.Sprintf(".t%d.w%d.jpg", int(t), w0)
	cacheDir := filepath.Join(s.recRoot(), ".frames", name)
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

	cacheDir := filepath.Join(s.recRoot(), ".frames")
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
