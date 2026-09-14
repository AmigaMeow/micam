package main

// 播放接口。
//
// 架构：每次请求独占一条摄像头连接 -> 转码 -> 输出 -> 立即释放。
// 详见 hub.go。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/micam/micam/internal/stream"
	"github.com/micam/micam/internal/xiaomi"
)

// resolveSource 取源地址：优先缓存，其次向小米云申请。
func (s *Server) resolveSource(dev *xiaomi.Device) (string, error) {
	if v, ok := s.store.Source(dev.Did); ok {
		return v, nil
	}
	sessions := s.store.AllSessions()
	if len(sessions) == 0 {
		return "", fmt.Errorf("未登录小米账号")
	}
	src, err := stream.BuildSource(sessions[0], dev, "cn")
	if err != nil {
		return "", fmt.Errorf("向小米云申请凭据失败: %w", err)
	}
	s.store.SaveSource(dev.Did, src)
	return src, nil
}

// ensureDevice 取设备。
//
// 重要：优先返回【有真实 IP 的】设备。
// 源缓存构造的占位设备没有 IP，会导致后续连接异常。
func (s *Server) ensureDevice(did string) (*xiaomi.Device, error) {
	// 1) 设备缓存里有真实信息（有 IP）直接用
	if d, ok := s.store.Device(did); ok && d.LocalIP != "" {
		return d, nil
	}

	// 2) 从云端刷新真实设备信息
	var all []xiaomi.Device
	seen := map[string]bool{}
	for _, sess := range s.store.AllSessions() {
		devs, err := sess.AllCameras()
		if err != nil {
			continue
		}
		for _, d := range devs {
			if !seen[d.Did] {
				seen[d.Did] = true
				all = append(all, d)
			}
		}
	}
	if len(all) > 0 {
		s.store.CacheDevices(all)
		if d, ok := s.store.Device(did); ok {
			return d, nil
		}
	}

	// 3) 云端拿不到，退回占位设备（至少有源缓存）
	if d, ok := s.store.Device(did); ok {
		return d, nil
	}
	if _, ok := s.store.Source(did); ok {
		d := &xiaomi.Device{Did: did, Name: "摄像头 " + did, IsOnline: true}
		s.store.CacheDevices([]xiaomi.Device{*d})
		return d, nil
	}
	return nil, fmt.Errorf("设备不存在: %s", did)
}

// runFFmpeg 独占连接 + 转码。
//
// 返回的 cleanup 必须调用（建议 defer），否则连接不会释放。
func (s *Server) runFFmpeg(ctx context.Context, dev *xiaomi.Device, args []string) (*exec.Cmd, io.ReadCloser, *bytes.Buffer, func(), error) {
	_, mc, release, err := s.useConn(dev)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	// 关键：不用 cmd.Stdin = pipe。
	//
	// Go 的 exec.Cmd 对 Stdin 的处理是「启动后同步拷贝」，
	// 配合 bufferedPipe 的丢数据策略，会导致 ffmpeg 收到不完整的流
	// （表现为 "PPS id out of range" + 0 帧）。
	//
	// 改成：显式拿 StdinPipe，用独立协程全速写入。
	cmd := exec.CommandContext(ctx, s.ffmpegPath(), args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		release()
		return nil, nil, nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		release()
		return nil, nil, nil, nil, err
	}
	errBuf := &bytes.Buffer{}
	cmd.Stderr = errBuf
	if err := cmd.Start(); err != nil {
		release()
		return nil, nil, nil, nil, err
	}

	// 全速读取 consumer 并写入 ffmpeg stdin
	pr := newBufferedPipe(32 << 20)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer recoverPanic("consumer")
		_, _ = mc.Consumer().WriteTo(pr)
	}()

	// 独立协程：从缓冲队列读到 ffmpeg stdin
	feedDone := make(chan struct{})
	go func() {
		defer close(feedDone)
		defer recoverPanic("feeder")
		defer stdin.Close()
		buf := make([]byte, 64*1024)
		var total int64
		for {
			n, rerr := pr.Read(buf)
			if n > 0 {
				if _, werr := stdin.Write(buf[:n]); werr != nil {
					logf("[feed] %s 写入失败（已写 %d 字节）: %v", dev.Did, total, werr)
					return
				}
				total += int64(n)
			}
			if rerr != nil {
				logf("[feed] %s 结束，共写入 %d 字节", dev.Did, total)
				return
			}
		}
	}()

	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			pr.Close()
			release()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
			}
			select {
			case <-feedDone:
			case <-time.After(2 * time.Second):
			}
		})
	}
	return cmd, stdout, errBuf, cleanup, nil
}

// ---- 快照 ----

func (s *Server) handleSnap(w http.ResponseWriter, r *http.Request) {
	did := r.URL.Query().Get("did")
	if did == "" {
		writeErr(w, 400, "缺少 did")
		return
	}
	width, _ := strconv.Atoi(r.URL.Query().Get("w"))
	if width <= 0 {
		width = 1280
	}

	dev, err := s.ensureDevice(did)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	// 离线设备快速失败，不让客户端干等
	if !dev.IsOnline && dev.LocalIP != "" {
		writeErr(w, 503, "设备离线（"+dev.Name+"），无法抓图")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 优化：skip_frame=nokey 让 ffmpeg 只解码关键帧，
	// 这样不必等完整的关键帧间隔，抓图快很多。
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-analyzeduration", "3M",
		"-probesize", "5M",
		"-fflags", "+discardcorrupt+genpts",
		"-err_detect", "ignore_err",
		"-skip_frame", "nokey",
		"-f", "mpegts", "-i", "pipe:0",
		"-frames:v", "1",
		"-vf", fmt.Sprintf("scale=%d:-2", width),
		"-f", "image2", "-c:v", "mjpeg",
		"pipe:1",
	}
	cmd, stdout, errBuf, cleanup, err := s.runFFmpeg(ctx, dev, args)
	if err != nil {
		writeErr(w, 502, "连接摄像头失败: "+err.Error())
		return
	}
	defer cleanup()

	out, _ := io.ReadAll(stdout)
	_ = cmd.Wait()

	if len(out) == 0 {
		msg := "抓帧失败"
		if errBuf.Len() > 0 {
			e := errBuf.String()
			if len(e) > 200 {
				e = e[:200]
			}
			msg += "：" + e
		}
		writeErr(w, 504, msg)
		return
	}

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	_, _ = w.Write(out)
}

// ---- MJPEG 流 ----

func (s *Server) handleMJPEG(w http.ResponseWriter, r *http.Request) {
	did := r.URL.Query().Get("did")
	if did == "" {
		writeErr(w, 400, "缺少 did")
		return
	}
	fps, _ := strconv.Atoi(r.URL.Query().Get("fps"))
	if fps <= 0 {
		fps = 8
	}
	// 上限保护：2K 摄像头转码很重，fps 太高会导致首帧超时
	if fps > 10 {
		fps = 10
	}
	width, _ := strconv.Atoi(r.URL.Query().Get("w"))
	if width <= 0 {
		width = 640
	}
	// 上限保护：1280 对 2K 流压力大，实测 960 是安全上限
	if width > 960 {
		width = 960
	}

	logf("[mjpeg] 请求 did=%s fps=%d w=%d", did, fps, width)

	dev, err := s.ensureDevice(did)
	if err != nil {
		logf("[mjpeg] ensureDevice 失败: %v", err)
		writeErr(w, 400, err.Error())
		return
	}
	logf("[mjpeg] 设备: %s online=%v ip=%s", dev.Name, dev.IsOnline, dev.LocalIP)
	if !dev.IsOnline && dev.LocalIP != "" {
		writeErr(w, 503, "设备离线（"+dev.Name+"），无法播放")
		return
	}

	// 关键：不用 ffmpeg 的 mpjpeg（它的 boundary 固定是 "ffmpeg"，不可自定义，
	// 而且输出格式不规范）。改成 ffmpeg 输出连续 JPEG 帧，我们自己组装 multipart。
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-analyzeduration", "10M",
		"-probesize", "10M",
		"-fflags", "+discardcorrupt+genpts",
		"-err_detect", "ignore_err",
		"-f", "mpegts", "-i", "pipe:0",
		"-vf", fmt.Sprintf("fps=%d,scale=%d:-2", fps, width),
		"-c:v", "mjpeg", "-q:v", "7",
		"-f", "image2pipe",
		"pipe:1",
	}
	cmd, stdout, errBuf, cleanup, err := s.runFFmpeg(r.Context(), dev, args)
	if err != nil {
		logf("[mjpeg] runFFmpeg 失败: %v", err)
		writeErr(w, 502, "连接摄像头失败: "+err.Error())
		return
	}
	logf("[mjpeg] ffmpeg 已启动")
	defer func() {
		cleanup()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	const boundary = "mjpegframe"

	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+boundary)
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")

	flusher, _ := w.(http.Flusher)
	w.WriteHeader(200)
	if flusher != nil {
		flusher.Flush()
	}

	// 从连续 JPEG 字节流里切出完整帧（FFD8...FFD9），组装成标准 multipart
	var pending []byte
	readBuf := make([]byte, 64*1024)
	sent := 0

	// 首帧看门狗。
	//
	// 重要：某些机型（如 CW500 isa.camera.hlmax）的关键帧间隔很长
	// （实测 >15 秒），所以超时要给足，否则会误判为连接失败。
	//
	// 注意：这里【不能】调用 flusher.Flush()！
	// 主循环在写响应，看门狗并发 Flush 会导致
	// "concurrent write to response" panic，直接杀掉进程。
	firstFrame := make(chan struct{})
	go func() {
		defer recoverPanic("watchdog")
		select {
		case <-firstFrame:
		case <-time.After(45 * time.Second):
			logf("[mjpeg] %s 首帧超时（45秒），断开连接", did)
			_ = stdout.Close()
		}
	}()

	for {
		n, err := stdout.Read(readBuf)
		if n > 0 {
			pending = append(pending, readBuf[:n]...)

			// 尽可能多地切出完整帧
			for {
				start := indexOf(pending, []byte{0xFF, 0xD8, 0xFF})
				if start < 0 {
					// 没有帧头，保留尾部少量数据
					if len(pending) > 2 {
						pending = pending[len(pending)-2:]
					}
					break
				}
				end := indexOf(pending[start+2:], []byte{0xFF, 0xD9})
				if end < 0 {
					// 帧不完整，等更多数据
					if start > 0 {
						pending = pending[start:]
					}
					break
				}
				frame := pending[start : start+2+end+2]
				pending = pending[start+2+end+2:]

				// 组装 multipart 帧
				CRLF := "\r\n"
				hdr := fmt.Sprintf("--%s"+CRLF+"Content-Type: image/jpeg"+CRLF+"Content-Length: %d"+CRLF+CRLF, boundary, len(frame))
				if _, werr := w.Write([]byte(hdr)); werr != nil {
					return
				}
				if _, werr := w.Write(frame); werr != nil {
					return
				}
				if _, werr := w.Write([]byte("\r\n")); werr != nil {
					return
				}
				if sent == 0 {
					close(firstFrame) // 首帧已出，取消看门狗
				}
				sent++
				if flusher != nil {
					flusher.Flush()
				}
			}

			// 防止 pending 无限增长
			if len(pending) > 8<<20 {
				pending = pending[len(pending)-1<<20:]
			}
		}
		if err != nil {
			if errBuf.Len() > 0 {
				logf("[mjpeg] ffmpeg: %s", errBuf.String())
			}
			logf("[mjpeg] 流结束，已发送 %d 帧", sent)
			return
		}
	}
}

// indexOf 查找子串位置。
func indexOf(haystack, needle []byte) int {
	if len(needle) == 0 || len(haystack) < len(needle) {
		return -1
	}
	for i := 0; i <= len(haystack)-len(needle); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// ---- 源注入 ----

func (s *Server) handleInjectSource(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 POST")
		return
	}
	var req struct {
		Did string `json:"did"`
		Src string `json:"src"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "参数错误")
		return
	}
	s.store.SaveSource(req.Did, req.Src)
	writeJSON(w, 200, map[string]any{"ok": true, "did": req.Did})
}
