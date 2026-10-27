package main

// 老旧机型（TUTK 协议，如 chuangmi.camera.ipc009）云端拉流兼容模块。
//
// 背景：
//   1. 创米等早期 1080p 摄像头（如 ipc009、ipc019 等）采用 TUTK P2P 协议。
//   2. 小米官方闭源库 (libmiot_camera_lite.so) 与纯 Go cs2 仅支持 CS2/PPCS，无法直接局域网直连。
//   3. 参考社区成熟实践（如 hass-xiaomi-miot），调用小米云端 MIoT Action
//      （如 siid=4, aiid=1 "camera-stream-for-google-home" 或 siid=3, aiid=1 "camera-stream-for-amazon-alexa"）
//      可获取小米云端实时转码的 HLS (.m3u8) 视频流。
//   4. 该 HLS 流有效期一般为 4~5 分钟，后台带 3.5 分钟静默缓存与自动续期，
//      ffmpeg 接入后可无缝输出 MJPEG 与 HLS，彻底消除旧机型黑屏与无法播放。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/micam/micam/internal/xiaomi"
)

type cachedCloudStream struct {
	URL       string
	ExpiresAt time.Time
}

var (
	cloudStreamMu    sync.Mutex
	cloudStreamCache = map[string]*cachedCloudStream{}
)
// invalidateCloudStream 废弃失效或断开的云端流缓存。
func invalidateCloudStream(did string) {
	cloudStreamMu.Lock()
	delete(cloudStreamCache, did)
	cloudStreamMu.Unlock()
}

// pollM3U8Ready 预检探测云端 HLS 转码器是否已切出首片 m3u8 并返回 200 OK。
func pollM3U8Ready(ctx context.Context, hlsURL string, maxWait time.Duration) bool {
	client := &http.Client{
		Timeout: 2 * time.Second,
	}
	deadline := time.Now().Add(maxWait)
	for {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		if time.Now().After(deadline) {
			return false
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, hlsURL, nil)
		if err == nil {
			req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
			resp, err := client.Do(req)
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return true
				}
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(350 * time.Millisecond):
		}
	}
}


// IsTutkCamera 判断设备是否为 TUTK 协议或已知老旧协议机型。
func IsTutkCamera(did, model string) bool {
	if did != "" {
		tutkDevicesMu.Lock()
		isTutk := tutkDevices[did]
		tutkDevicesMu.Unlock()
		if isTutk {
			return true
		}
	}
	switch model {
	case "chuangmi.camera.ipc009",
		"chuangmi.camera.ipc019",
		"chuangmi.camera.ipc013",
		"chuangmi.camera.019a01":
		return true
	}
	return xiaomi.IsLegacyModel(model)
}

// fetchCloudHLSStream 请求小米云端生成临时 HLS 播放地址（带缓存与自动续期）。
func (s *Server) fetchCloudHLSStream(did string) (string, error) {
	cloudStreamMu.Lock()
	if c, ok := cloudStreamCache[did]; ok && c != nil {
		if time.Now().Before(c.ExpiresAt) && c.URL != "" {
			u := c.URL
			cloudStreamMu.Unlock()
			return u, nil
		}
	}
	cloudStreamMu.Unlock()

	sessions := s.store.AllSessions()
	if len(sessions) == 0 {
		return "", fmt.Errorf("未登录小米账号")
	}
	sess := sessions[0]

	// 候选 action 列表（主流型号定义：siid=4 aiid=1 为 Google Home HLS, siid=3 aiid=1 为 Alexa HLS）
	type actionCandidate struct {
		siid int
		aiid int
		in   []any
	}
	candidates := []actionCandidate{
		{siid: 4, aiid: 1, in: []any{1}},
		{siid: 4, aiid: 1, in: []any{0}},
		{siid: 4, aiid: 1, in: []any{}},
		{siid: 3, aiid: 1, in: []any{1}},
		{siid: 3, aiid: 1, in: []any{}},
		{siid: 5, aiid: 1, in: []any{}},
	}

	var lastErr error
	for _, cand := range candidates {
		payload := map[string]any{
			"params": map[string]any{
				"did":  did,
				"siid": cand.siid,
				"aiid": cand.aiid,
				"in":   cand.in,
			},
		}
		rawBody, err := json.Marshal(payload)
		if err != nil {
			continue
		}

		res, err := sess.Request("/miotspec/action", string(rawBody))
		if err != nil {
			lastErr = err
			continue
		}

		streamURL := parseStreamURLFromActionResponse(res)
		if streamURL != "" {
			logf("[tutk-cloud] 设备 %s 成功通过 siid=%d aiid=%d 申请到云端 HLS 流", did, cand.siid, cand.aiid)
			cloudStreamMu.Lock()
			cloudStreamCache[did] = &cachedCloudStream{
				URL:       streamURL,
				ExpiresAt: time.Now().Add(210 * time.Second), // 3.5分钟自动轮转续期
			}
			cloudStreamMu.Unlock()
			return streamURL, nil
		}
	}

	if lastErr != nil {
		return "", fmt.Errorf("向小米云申请老协议兼容流失败: %w", lastErr)
	}
	return "", fmt.Errorf("该设备未提供云端 HLS 流服务")
}

// parseStreamURLFromActionResponse 从 MIoT Action 响应体中解析出 m3u8 地址。
func parseStreamURLFromActionResponse(data []byte) string {
	var v struct {
		Code int   `json:"code"`
		Out  []any `json:"out"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return ""
	}
	if v.Code != 0 || len(v.Out) == 0 {
		return ""
	}

	for _, item := range v.Out {
		if str, ok := item.(string); ok {
			if strings.HasPrefix(str, "http://") || strings.HasPrefix(str, "https://") {
				if strings.Contains(str, ".m3u8") || strings.Contains(str, "playlist") || strings.Contains(str, "hlstranscoder") {
					return str
				}
				if _, err := url.Parse(str); err == nil {
					return str
				}
			}
		} else if m, ok := item.(map[string]any); ok {
			for _, val := range m {
				if str, ok2 := val.(string); ok2 && (strings.HasPrefix(str, "http://") || strings.HasPrefix(str, "https://")) {
					return str
				}
			}
		}
	}
	return ""
}

// streamFromHLSURL 将云端 HLS (.m3u8) 码流转码为实时 MJPEG 流推送给前端。
func (s *Server) streamFromHLSURL(w http.ResponseWriter, r *http.Request, did, hlsURL, w0, fps, qv string) {
	ctx, cancel, finishStream := beginMiotStream(r.Context(), did)
	defer cancel()
	defer finishStream()

	// 预检探测与云端预热等待：小米云转码服务首次生成 m3u8 切片需 1.5~3 秒
	// 若尚未切出首个分片则会返回 HTTP 404，过早启动 ffmpeg 会导致秒退中断
	if !pollM3U8Ready(ctx, hlsURL, 6*time.Second) {
		logf("[tutk-cloud] 设备 %s 首次预检未就绪，重试申请新鲜 HLS 地址并等待预热...", did)
		invalidateCloudStream(did)
		freshURL, err := s.fetchCloudHLSStream(did)
		if err == nil && freshURL != "" {
			hlsURL = freshURL
			pollM3U8Ready(ctx, hlsURL, 6*time.Second)
		}
	}

	scaleFilter := "scale=" + w0 + ":-2"
	if w0 == "0" || w0 == "" {
		scaleFilter = "null"
	}
	args := []string{
		"-hide_banner", "-loglevel", "warning",
		"-user_agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36",
		"-reconnect", "1",
		"-reconnect_at_eof", "1",
		"-reconnect_streamed", "1",
		"-reconnect_on_http_error", "4xx,5xx",
		"-reconnect_delay_max", "3",
		"-rw_timeout", "15000000",
		"-i", hlsURL,
		"-an",
		"-vf", scaleFilter,
		"-r", fps,
		"-q:v", qv,
		"-f", "mjpeg",
		"pipe:1",
	}

	cmd := exec.CommandContext(ctx, s.ffmpegPath(), args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil && cmd.Process.Pid > 0 {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 3 * time.Second

	stderr, _ := cmd.StderrPipe()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, err := stderr.Read(buf)
			if n > 0 {
				logf("[tutk-cloud] ffmpeg [%s]: %s", did, trimNL(string(buf[:n])))
			}
			if err != nil {
				return
			}
		}
	}()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		writeErr(w, 500, "启动转码管道失败: "+err.Error())
		return
	}

	if err := cmd.Start(); err != nil {
		writeErr(w, 500, "启动云端流转码失败: "+err.Error())
		return
	}
	logf("[tutk-cloud] 设备 %s 云端 HLS 兼容流已启动转码", did)

	const boundary = "mjpegframe"
	flusher, _ := w.(http.Flusher)
	headerSent := false

	var pending []byte
	readBuf := make([]byte, 64*1024)
	sent := 0
	startAt := time.Now()

	for {
		n, rerr := stdout.Read(readBuf)
		if n > 0 {
			pending = append(pending, readBuf[:n]...)
			var frames [][]byte
			for {
				start := indexOfJPEGStart(pending)
				if start < 0 {
					if len(pending) > 2 {
						pending = pending[len(pending)-2:]
					}
					break
				}
				end := indexOfJPEGEnd(pending, start)
				if end < 0 {
					if start > 0 {
						pending = pending[start:]
					}
					break
				}
				frames = append(frames, pending[start:end+2])
				pending = pending[end+2:]
			}

			if len(frames) > 0 && !headerSent {
				w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+boundary)
				w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
				w.Header().Set("Pragma", "no-cache")
				w.WriteHeader(http.StatusOK)
				if flusher != nil {
					flusher.Flush()
				}
				headerSent = true
			}

			for i, frame := range frames {
				if len(frames) > 1 && i < len(frames)-1 {
					continue
				}
				head := "--" + boundary + "\r\nContent-Type: image/jpeg\r\nContent-Length: " + strconv.Itoa(len(frame)) + "\r\n\r\n"
				if _, werr := w.Write([]byte(head)); werr != nil {
					return
				}
				if _, werr := w.Write(frame); werr != nil {
					return
				}
				if _, werr := w.Write([]byte("\r\n")); werr != nil {
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
				sent++
			}
		}
		if rerr != nil {
			break
		}
	}
	if !headerSent {
		invalidateCloudStream(did)
		logf("[tutk-cloud] 设备 %s 兼容流拉流结束但未收到任何有效帧 (耗时 %.2fs)", did, time.Since(startAt).Seconds())
		writeErr(w, 502, "云端兼容流未输出视频帧，请稍后重试")
	}
}
