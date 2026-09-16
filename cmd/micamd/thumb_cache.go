package main

// 实时缩略图缓存。
//
// ===== 为什么需要 ====
//
// 抓一张快照要 3-9 秒（新建 ffmpeg + 等关键帧）。
// 设备列表页有 N 台摄像头，如果每张都实时抓：
//   - 首屏要等 10-30 秒
//   - 每张都跑一次 ffmpeg，CPU 峰值很高
//
// 所以：**缓存 + 后台刷新**。
//
//   GET /api/live/thumb?did=X
//     - 有缓存（任意年龄）-> 立刻返回，并在后台刷新
//     - 无缓存 -> 返回 202，客户端稍后重试（不会卡住）
//
// 启动时预热一轮，之后每 60 秒刷新一次（只刷新有缓存的设备）。

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

type thumbEntry struct {
	Data []byte
	Time time.Time
	Busy bool // 正在刷新
}

var (
	thumbMu    sync.Mutex
	thumbCache = map[string]*thumbEntry{}
	thumbTTL   = 60 * time.Second  // 超过这个年龄就在后台刷新
)

// handleLiveThumb 返回缓存的实时缩略图。
func (s *Server) handleLiveThumb(w http.ResponseWriter, r *http.Request) {
	did := r.URL.Query().Get("did")
	if did == "" {
		writeErr(w, 400, "缺少 did")
		return
	}
	width := 640

	thumbMu.Lock()
	e, ok := thumbCache[did]
	var stale bool
	if ok {
		stale = time.Since(e.Time) > thumbTTL
	}
	thumbMu.Unlock()

	// 有缓存：立刻返回（陈旧也返回，同时后台刷新）
	if ok && len(e.Data) > 0 {
		if stale {
			s.refreshThumbAsync(did, width)
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Thumb-Age", time.Since(e.Time).Round(time.Second).String())
		_, _ = w.Write(e.Data)
		return
	}

	// 无缓存：触发一次抓取，让客户端稍后重试（避免首屏卡住）
	s.refreshThumbAsync(did, width)
	w.Header().Set("Retry-After", "3")
	writeErr(w, 202, "缩略图生成中")
}

// refreshThumbAsync 后台刷新某设备的缩略图。
func (s *Server) refreshThumbAsync(did string, width int) {
	thumbMu.Lock()
	e := thumbCache[did]
	if e == nil {
		e = &thumbEntry{}
		thumbCache[did] = e
	}
	if e.Busy {
		thumbMu.Unlock()
		return
	}
	e.Busy = true
	thumbMu.Unlock()

	go func() {
		defer func() {
			thumbMu.Lock()
			if cur := thumbCache[did]; cur != nil {
				cur.Busy = false
			}
			thumbMu.Unlock()
		}()

		data, err := s.captureThumb(did, width)
		if err != nil || len(data) == 0 {
			return
		}

		thumbMu.Lock()
		if cur := thumbCache[did]; cur != nil {
			cur.Data = data
			cur.Time = time.Now()
		}
		thumbMu.Unlock()
	}()
}

// captureThumb 抓一帧（复用快照逻辑）。
func (s *Server) captureThumb(did string, width int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	dev, err := s.ensureDevice(did)
	if err != nil {
		return nil, err
	}
	if !dev.IsOnline && dev.LocalIP != "" {
		return nil, fmt.Errorf("设备离线")
	}

	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-analyzeduration", "100000", "-probesize", "100000",
		"-f", "mpegts", "-i", "pipe:0",
		"-frames:v", "1",
		"-vf", vfScale(0, width),
		"-f", "image2", "-c:v", "mjpeg",
		"pipe:1",
	}

	cmd, stdout, _, cleanup, err := s.runFFmpeg(ctx, dev, 1, args)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	data, _ := io.ReadAll(stdout)
	if err := cmd.Wait(); err != nil && len(data) == 0 {
		return nil, err
	}
	return data, nil
}

// startThumbWarmer 启动缩略图预热与定时刷新。
//
// 预热：启动 30 秒后，逐个设备抓一张（错开 3 秒，避免同时压满 CPU）。
// 刷新：之后每 2 分钟刷新一轮（只刷新已有缓存的，不主动为没看过的设备抓）。
func (s *Server) startThumbWarmer() {
	go func() {
		time.Sleep(30 * time.Second)

		// 首轮预热
		for _, d := range s.allCameras() {
			if !d.IsCamera() || !d.IsOnline {
				continue
			}
			s.refreshThumbAsync(d.Did, 640)
			time.Sleep(3 * time.Second)
		}

		// 定时刷新
		t := time.NewTicker(2 * time.Minute)
		defer t.Stop()
		for range t.C {
			thumbMu.Lock()
			dids := make([]string, 0, len(thumbCache))
			for did := range thumbCache {
				dids = append(dids, did)
			}
			thumbMu.Unlock()

			for _, did := range dids {
				s.refreshThumbAsync(did, 640)
				time.Sleep(3 * time.Second)
			}
		}
	}()
}
