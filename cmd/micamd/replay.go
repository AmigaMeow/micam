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

	"github.com/micam/micam/internal/hardware"
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

	cap := hardware.Detect(s.ffmpegPath())

	buildArgs := func(hw bool) []string {
		args := []string{"-hide_banner", "-loglevel", "error"}
		if start > 0 {
			// -ss 放在 -i 前面 -> 快速定位（关键帧）
			args = append(args, "-ss", fmt.Sprintf("%.2f", start))
		}
		args = append(args, "-i", src)

		if hw {
			args = append(args, "-c:v", "h264_qsv", "-preset", "veryfast", "-global_quality", "26")
		} else {
			args = append(args, "-c:v", "libx264", "-preset", "veryfast", "-crf", "26")
		}

		args = append(args,
			"-c:a", "aac", "-b:a", "64k",
			"-movflags", "frag_keyframe+empty_moov+default_base_moof",
			"-f", "mp4", "pipe:1",
		)
		return args
	}

	// 先探测硬件编码是否可用。
	//
	// 原因：服务进程里 h264_qsv 可能拿不到 /dev/dri 权限，
	// 报 "Failed to create a VAAPI device"。
	// 必须在写响应头之前探测 —— 否则已发 200 + 空 body，客户端看不到错误。
	tryHW := strings.HasSuffix(cap.Preferred, "_qsv")
	if tryHW {
		if err := s.probeTranscode(src, tryHW); err != nil {
			logf("[replay] 硬件编码不可用（%v），改用软件编码", err)
			tryHW = false
		}
	}

	args := buildArgs(tryHW)
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
			if len(msg) > 400 {
				msg = msg[:400]
			}
			logf("[replay] ffmpeg: %s", strings.TrimSpace(msg))
		}
	}()

	enc := "libx264"
	if tryHW {
		enc = "h264_qsv"
	}
	logf("[replay] 开始转码 %s (t=%.1f, 编码器=%s)", file, start, enc)

	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	_, _ = io.Copy(w, stdout)
}

// probeTranscode 试编码 1 秒，确认编码器可用。
//
// 为什么需要：服务进程里 h264_qsv 可能拿不到 /dev/dri 权限。
// 提前探测可以避免「已发 200 + 空 body 才发现失败」。
func (s *Server) probeTranscode(src string, hw bool) error {
	args := []string{"-hide_banner", "-loglevel", "error", "-t", "1", "-i", src}
	if hw {
		args = append(args, "-c:v", "h264_qsv", "-preset", "veryfast")
	} else {
		args = append(args, "-c:v", "libx264", "-preset", "ultrafast")
	}
	args = append(args, "-f", "null", "-")

	out, err := exec.Command(s.ffmpegPath(), args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
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
