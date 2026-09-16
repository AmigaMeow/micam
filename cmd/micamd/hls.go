package main

// HLS 播放实现（2K 流畅方案）。
//
// 为什么用 HLS：
//   - MJPEG 逐帧压缩，2K 下数据量放大 27 倍（49.8MB vs 1.8MB 原始）
//   - H264 帧间压缩，数据量只有 MJPEG 的 1/3
//   - 浏览器原生 <video> 解码，不占 NAS CPU
//   - 配合硬件编码（QSV），CPU 从 272% 降到 48%
//
// 架构：
//   摄像头 --HEVC--> ffmpeg(硬件编码) --> HLS 分片 --> <video> 播放

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/micam/micam/internal/hardware"
	"github.com/micam/micam/internal/xiaomi"
)

// hlsSession 一个 HLS 转码会话。
type hlsSession struct {
	Did       string
	Dir       string
	FPS       int
	Width     int
	Started   time.Time
	LastUsed  time.Time
	mu        sync.Mutex
	cleanup   func()
}

var (
	hlsMu       sync.Mutex
	hlsSessions = map[string]*hlsSession{}
)

// hlsDir 返回某设备的 HLS 目录。
func (s *Server) hlsDir(did string, lens int) string {
	// 双摄要分目录，否则两路会互相覆盖分片
	name := did
	if lens >= 2 {
		name = did + "@2"
	}
	return filepath.Join(s.dataDir, "hls", name)
}

// handleHLS 提供 HLS 播放列表和分片。
//
// 路径：
//   GET /api/live/hls?did=xxx          -> 返回 m3u8 内容（并启动转码）
//   GET /api/live/hls/seg?did=xxx&f=xx -> 返回分片文件
func (s *Server) handleHLS(w http.ResponseWriter, r *http.Request) {
	did := r.URL.Query().Get("did")
	if did == "" {
		writeErr(w, 400, "缺少 did")
		return
	}
	lens := lensFromRequest(r)

	// 分片请求
	if seg := r.URL.Query().Get("seg"); seg != "" {
		s.serveHLSSegment(w, r, did, seg)
		return
	}

	// m3u8 请求
	dev, err := s.ensureDevice(did)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}

	fps, _ := strconv.Atoi(r.URL.Query().Get("fps"))
	if fps <= 0 || fps > 20 {
		fps = 15
	}
	width, _ := strconv.Atoi(r.URL.Query().Get("w"))

	// 确保转码在跑
	if err := s.ensureHLS(dev, lens, fps, width); err != nil {
		writeErr(w, 502, err.Error())
		return
	}

	tWait := time.Now()
	// 等 m3u8 生成
	//
	// 实测正常 4-5 秒出 m3u8。20 秒还没出就放弃（前端会降级 MJPEG）。
	playlist := filepath.Join(s.hlsDir(did, lens), "index.m3u8")
	for i := 0; i < 300; i++ {
		if fi, err := os.Stat(playlist); err == nil && fi.Size() > 0 {
			break
		}
		time.Sleep(80 * time.Millisecond)
	}

	logf("⏱ [6] 等 m3u8 %v", time.Since(tWait).Round(time.Millisecond))

	data, err := os.ReadFile(playlist)
	if err != nil {
		writeErr(w, 504, "播放列表尚未生成，请稍候重试")
		return
	}

	// 改写分片路径。
	//
	// 关键：相对路径必须保留 "hls" 这一段。
	//
	// m3u8 的 URL 形如 <prefix>/api/live/hls?did=X
	// 相对路径解析规则：替换最后一个路径段。
	//
	//   "seg?did=X&seg=seg0.ts"   -> <prefix>/api/live/seg?...     ❌ 404
	//   "hls?did=X&seg=seg0.ts"   -> <prefix>/api/live/hls?...     ✅ 正确
	//
	// 这样写不管网关前缀是什么（/app/micam 或空）都能解析正确。
	content := rewritePlaylist(string(data), did)

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(content))
}

// serveHLSSegment 提供分片文件。
func (s *Server) serveHLSSegment(w http.ResponseWriter, r *http.Request, did, seg string) {
	lens := lensFromRequest(r)
	// 安全检查：防止路径穿越
	if strings.Contains(seg, "..") || strings.Contains(seg, "/") {
		writeErr(w, 400, "非法文件名")
		return
	}

	path := filepath.Join(s.hlsDir(did, lens), seg)
	data, err := os.ReadFile(path)
	if err != nil {
		writeErr(w, 404, "分片不存在")
		return
	}

	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Cache-Control", "public, max-age=10")
	_, _ = w.Write(data)
}

// ensureHLS 确保 HLS 转码在运行。
func (s *Server) ensureHLS(dev *xiaomi.Device, lens, fps, width int) error {
	// 会话键必须带镜头维度
	//
	// 之前只用 dev.Did，导致请求第二镜头时发现「已有会话」直接返回，
	// 但那个会话在转第一镜头 -> 第二镜头的播放列表永远不生成 -> 504。
	key := connKey(dev.Did, lens)

	hlsMu.Lock()
	if sess, ok := hlsSessions[key]; ok {
		sess.LastUsed = time.Now()
		hlsMu.Unlock()
		return nil
	}
	hlsMu.Unlock()

	dir := s.hlsDir(dev.Did, lens)
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建 HLS 目录失败: %w", err)
	}

	// 硬件能力检测
	cap := hardware.Detect(s.ffmpegPath())

	// 构建参数
	args := []string{"-hide_banner", "-loglevel", "error"}

	// 解码参数
	//
	// 注意：pipe 输入 + QSV 硬件解码有兼容问题（实测 broken pipe），
	// 所以只对文件输入用硬件解码，pipe 输入用软件解码 + 硬件编码。
	// 实测：软件解码 + QSV 编码 CPU 只有 48%，已经足够。
	_ = cap.BuildDecodeArgs()

	args = append(args,
		// 启动延迟优化（实测数据）：
		//
		//   analyzeduration  首分片
		//   ---------------  --------
		//   5M (旧)           9.31s
		//   1M                4.81s
		//   100k              4.61s   <- 采用
		//
		// 摄像头 dial 只要 0.27s，首个 IDR 在 2.3s 左右，
		// 所以 4.6s 已接近理论下限（首IDR 2.3s + 1s 分片）。
		//
		// 注意：不要用 -use_wallclock_as_timestamps，
		// 它会让 ffmpeg 等真实时间流逝，反而更慢。
		"-analyzeduration", "100000",
		"-probesize", "100000",
		"-fflags", "+discardcorrupt+genpts+igndts",
		"-err_detect", "ignore_err",
		"-f", "mpegts", "-i", "pipe:0",
	)

	// 视频滤镜
	vf := fmt.Sprintf("fps=%d", fps)
	if width > 0 {
		vf += fmt.Sprintf(",scale=%d:-2", width)
	}
	// 硬件编码需要特定像素格式
	if strings.HasSuffix(cap.Preferred, "_vaapi") {
		vf += ",format=nv12,hwupload"
	}
	// 关键：强制每 2 秒生成一个关键帧
	// 否则 HLS 切片等不到 I 帧，一直不输出
	args = append(args, "-force_key_frames", "expr:gte(t,n_forced*1)")
	args = append(args, "-vf", vf)

	// 编码参数
	//
	// 注意：QSV/VAAPI 硬件编码在 pipe 实时流下不稳定
	// （实测 "Could not open encoder before EOF"），
	// 所以 HLS 场景强制用 libx264 软件编码。
	// 实测 libx264 ultrafast 对 2K@15fps 约 270% CPU（4核够用）。
	encArgs := []string{
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-tune", "zerolatency",
		"-crf", "26",
		"-g", "15",              // 每 1 秒一个关键帧（15fps * 1）
		"-keyint_min", "15",
		"-sc_threshold", "0",    // 禁用场景切换检测，保证固定 GOP
	}
	isHW := false
	args = append(args, encArgs...)

	// HLS 输出
	args = append(args,
		"-f", "hls",
		// 1 秒分片：更快起播（实测 2s -> 1s 省约 1 秒）
		"-hls_time", "1",
		"-hls_list_size", "6",
		// split_by_time: 按时间切分，不等 I 帧
		"-hls_flags", "delete_segments+split_by_time",
		"-hls_playlist_type", "event",
		"-hls_segment_filename", filepath.Join(dir, "seg%d.ts"),
		filepath.Join(dir, "index.m3u8"),
	)

	cmd, _, errBuf, cleanup, err := s.runFFmpegWithArgs(dev, lens, args)
	if err != nil {
		return fmt.Errorf("启动 HLS 转码失败: %w", err)
	}

	// 把 ffmpeg stderr 完整落盘（便于排查）
	errFile := filepath.Join(dir, "ffmpeg.log")
	go func() {
		f, _ := os.Create(errFile)
		if f != nil {
			defer f.Close()
		}
		lastLen := 0
		for i := 0; i < 60; i++ {
			time.Sleep(1 * time.Second)
			if errBuf == nil {
				return
			}
			cur := errBuf.String()
			if len(cur) > lastLen && f != nil {
				_, _ = f.WriteString(cur[lastLen:])
				_ = f.Sync()
				lastLen = len(cur)
			}
			// ffmpeg 退出就停止
			if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
				if f != nil {
					_, _ = f.WriteString("\n=== ffmpeg 已退出: " + cmd.ProcessState.String() + " ===\n")
				}
				return
			}
		}
	}()

	sess := &hlsSession{
		Did:      dev.Did,
		Dir:      dir,
		FPS:      fps,
		Width:    width,
		Started:  time.Now(),
		LastUsed: time.Now(),
		cleanup:  cleanup,
	}

	hlsMu.Lock()
	hlsSessions[key] = sess
	hlsMu.Unlock()

	logf("HLS 转码已启动: %s (编码器=%s 硬件=%v)", dev.Name, cap.Preferred, isHW)
	_ = cmd

	// 空闲回收
	go s.hlsReaper(key)
	return nil
}

// cleanHLSOnStart 启动时清理残留的 HLS 缓存。
//
// HLS 分片会快速累积（实测 2K 下约 70MB/分钟），
// 所以启动时必须清理上次的残留。
func (s *Server) cleanHLSOnStart() {
	dir := filepath.Join(s.dataDir, "hls")
	if _, err := os.Stat(dir); err == nil {
		_ = os.RemoveAll(dir)
		logf("已清理 HLS 缓存: %s", dir)
	}
}

// hlsReaper 回收空闲的 HLS 会话。
func (s *Server) hlsReaper(key string) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for range t.C {
		hlsMu.Lock()
		sess, ok := hlsSessions[key]
		if !ok {
			hlsMu.Unlock()
			return
		}
		idle := time.Since(sess.LastUsed)
		hlsMu.Unlock()

		if idle > 60*time.Second {
			hlsMu.Lock()
			delete(hlsSessions, key)
			hlsMu.Unlock()
			if sess.cleanup != nil {
				sess.cleanup()
			}
			logf("HLS 转码已回收: %s（空闲 %v）", key, idle.Round(time.Second))
			return
		}
	}
}

// rewritePlaylist 把 m3u8 里的分片路径改写成可正确解析的相对路径。
//
// 保留 "hls" 段是关键：相对路径解析会替换最后一个路径段，
// 写 "hls?..." 才能解析回 /api/live/hls?...。
func rewritePlaylist(content, did string) string {
	lines := strings.Split(content, "\n")
	var out []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			out = append(out, line)
			continue
		}
		out = append(out, "hls?did="+did+"&seg="+line)
	}
	return strings.Join(out, "\n")
}

// handleHLSInfo 返回 HLS 能力信息（供前端选择）。
func (s *Server) handleHLSInfo(w http.ResponseWriter, r *http.Request) {
	cap := hardware.Detect(s.ffmpegPath())
	writeJSON(w, 200, map[string]any{
		"hardware": cap,
		"hls": map[string]any{
			"available": true,
			"encoder":   cap.Preferred,
		},
	})
}

var _ = json.Marshal
