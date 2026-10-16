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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type thumbEntry struct {
	Data    []byte
	Time    time.Time
	Busy    bool      // 正在刷新
	LastHit time.Time // 最后一次被前端请求（用于判断「有没有人在看」）
}

var (
	thumbMu    sync.Mutex
	thumbCache = map[string]*thumbEntry{}
	thumbTTL   = 3 * time.Minute  // 3分钟内新鲜；超过3分钟且有人浏览时在后台异步刷新
)

// handleLiveThumb 返回缓存的实时缩略图。

// latestDiskThumb 从磁盘找某设备最近的一张缩略图。
//
// 为什么需要（2026-09-24）：
//
//	用户反馈"静默期完全看不到任何画面"。
//	但 recordings/.thumbs/<设备名>/ 下有【真实的历史画面】（本次实测 176 张）。
//	内存缓存重启后是空的，原实现只查内存 -> 直接返回 202「凭据受限」。
//	=> 手里有画面却不给看。
//
// 注意：.thumbs 的文件名是 <原mkv名>.jpg，如 20260917_181635.mkv.jpg，
// 文件名前缀就是拍摄时间，比 mtime 更可靠（迁移/复制会改 mtime）。
func latestDiskThumb(recDir, name string) ([]byte, time.Time, bool) {
	if recDir == "" || name == "" {
		return nil, time.Time{}, false
	}
	dir := filepath.Join(recDir, ".thumbs", name)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, time.Time{}, false
	}

	best := ""
	var bestT time.Time
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasSuffix(n, ".jpg") {
			continue
		}
		// 文件名形如 20260917_181635.mkv.jpg -> 取 20260917_181635
		t, ok := parseThumbName(n)
		if !ok {
			// 解析不出来就退回 mtime
			if info, e2 := e.Info(); e2 == nil {
				t = info.ModTime()
				ok = true
			}
		}
		if !ok {
			continue
		}
		if t.After(bestT) {
			bestT = t
			best = n
		}
	}
	if best == "" {
		return nil, time.Time{}, false
	}
	data, err := os.ReadFile(filepath.Join(dir, best))
	if err != nil || len(data) == 0 {
		return nil, time.Time{}, false
	}
	return data, bestT, true
}

// parseThumbName 从缩略图文件名解析拍摄时间。
func parseThumbName(n string) (time.Time, bool) {
	// 20260917_181635.mkv.jpg -> 20260917_181635
	base := strings.TrimSuffix(n, ".jpg")
	base = strings.TrimSuffix(base, ".mkv")
	if len(base) < 15 {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("20060102_150405", base[:15], time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// handleThumbMeta GET /api/live/thumb/meta?did=xxx
//
// 返回这张缩略图是【实时】还是【磁盘历史画面】及其时间。
//
// 为什么单独开接口（2026-09-24 教训）：
//
//	前端有【多条】缩略图加载路径（loadCameraSnaps / loadCameraSnapsFor /
//	loadSideThumbs / loadHomeThumbs / 网格视图 …），逐条读响应头必漏
//	（实测漏了一条，角标仍显示当前时间，用户误以为画面是实时的）。
//	改成由这个接口统一回答，前端用全局钩子查询，与加载路径无关。
func (s *Server) handleThumbMeta(w http.ResponseWriter, r *http.Request) {
	did := r.URL.Query().Get("did")
	if did == "" {
		writeErr(w, 400, "缺少 did")
		return
	}

	out := map[string]any{
		"did":      did,
		"historic": false,
		"time":     "",
		"ageMin":   0,
	}

	// 内存缓存优先（= 实时画面）
	thumbMu.Lock()
	e, ok := thumbCache[did]
	thumbMu.Unlock()
	if ok && len(e.Data) > 0 {
		out["time"] = e.Time.Format("2006-01-02 15:04")
		out["ageMin"] = int(time.Since(e.Time).Minutes())
		writeJSON(w, 200, out)
		return
	}

	// 回退：磁盘历史画面
	var devName string
	for _, d := range s.allCameras() {
		if d.Did == did {
			devName = d.Name
			break
		}
	}
	if devName != "" {
		if _, t, ok2 := latestDiskThumb(s.rec.GetConfig().Dir, devName); ok2 {
			out["historic"] = true
			out["time"] = t.Format("2006-01-02 15:04")
			out["ageMin"] = int(time.Since(t).Minutes())
		}
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleLiveThumb(w http.ResponseWriter, r *http.Request) {
	did := r.URL.Query().Get("did")
	if did == "" {
		writeErr(w, 400, "缺少 did")
		return
	}
	_ = 640 // width 参数保留（前端可能传），当前抓帧固定 640
	refresh := r.URL.Query().Get("refresh") == "1" || r.URL.Query().Get("refresh") == "true"

	thumbMu.Lock()
	e, ok := thumbCache[did]
	var stale bool
	if ok {
		stale = time.Since(e.Time) > thumbTTL
		// 记录「这个设备有人在看」—— 后台只刷有人的设备
		e.LastHit = time.Now()
	}
	thumbMu.Unlock()

	// 1. 用户显式点击「刷新封面」：强制按需抓取最新帧
	if refresh {
		if s.thumbShouldGrab(did, time.Now()) {
			done := make(chan []byte, 1)
			go func() {
				defer thumbGrabDone(did)
				data, err := s.grabFrameViaMiot(did)
				if err != nil {
					logf("[thumb] 手动刷新抓帧失败 did=%s: %v", did, err)
					done <- nil
					return
				}
				thumbMu.Lock()
				thumbCache[did] = &thumbEntry{Data: data, Time: time.Now(), LastHit: time.Now()}
				thumbMu.Unlock()
				logf("[thumb] 手动刷新抓帧成功 did=%s (%d 字节)", did, len(data))
				done <- data
			}()

			select {
			case data := <-done:
				if len(data) > 0 {
					w.Header().Set("Content-Type", "image/jpeg")
					w.Header().Set("Cache-Control", "no-cache")
					w.Header().Set("X-Thumb-Age", "0s")
					_, _ = w.Write(data)
					return
				}
			case <-time.After(3500 * time.Millisecond):
				w.Header().Set("Retry-After", "2")
				writeErr(w, 202, "正在刷新抓取最新画面")
				return
			}
		} else {
			w.Header().Set("Retry-After", "2")
			writeErr(w, 202, "正在刷新抓取最新画面")
			return
		}
	}

	// 2. 有内存缓存：立刻返回（保证首屏秒开）
	if ok && len(e.Data) > 0 {
		// 用户正在浏览当前列表，若缓存陈旧（超过 3 分钟）：
		// 后台按需异步抓取一帧更新缓存，下次浏览就是新画面。无人浏览时不触发。
		if stale && s.thumbShouldGrab(did, time.Now()) {
			go func() {
				defer thumbGrabDone(did)
				data, err := s.grabFrameViaMiot(did)
				if err != nil {
					logf("[thumb] 浏览按需更新失败 did=%s: %v", did, err)
					return
				}
				thumbMu.Lock()
				thumbCache[did] = &thumbEntry{Data: data, Time: time.Now(), LastHit: time.Now()}
				thumbMu.Unlock()
				logf("[thumb] 浏览按需更新成功 did=%s (%d 字节)", did, len(data))
			}()
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Thumb-Age", time.Since(e.Time).Round(time.Second).String())
		_, _ = w.Write(e.Data)
		return
	}

	// 3. 无内存缓存时的处理（首次启动或离线回退）
	// ① 官方通道抓帧（按需 + 节流）
	if s.thumbShouldGrab(did, time.Now()) {
		go func() {
			defer thumbGrabDone(did)
			data, err := s.grabFrameViaMiot(did)
			if err != nil {
				logf("[thumb] 官方通道抓帧失败 did=%s: %v", did, err)
				return
			}
			thumbMu.Lock()
			thumbCache[did] = &thumbEntry{Data: data, Time: time.Now(), LastHit: time.Now()}
			thumbMu.Unlock()
			logf("[thumb] 官方通道抓帧成功 did=%s (%d 字节)", did, len(data))
		}()
		w.Header().Set("Retry-After", "3")
		writeErr(w, 202, "正在通过官方通道抓取")
		return
	}

	// ② 回退磁盘历史画面
	var devName string
	for _, d := range s.allCameras() {
		if d.Did == did {
			devName = d.Name
			break
		}
	}
	if devName != "" {
		if data, t, ok := latestDiskThumb(s.rec.GetConfig().Dir, devName); ok {
			w.Header().Set("Content-Type", "image/jpeg")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("X-Thumb-Disk", "1")
			w.Header().Set("X-Thumb-Time", t.Format("2006-01-02 15:04"))
			w.Header().Set("X-Thumb-Age", time.Since(t).Round(time.Minute).String())
			_, _ = w.Write(data)
			return
		}
	}

	// ③ 兜底
	w.Header().Set("Retry-After", "3")
	writeErr(w, 202, "缩略图准备中")
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

	// 官方通道优先（PPCS 局域网直连，不走传统凭据申请，不触发限流）
	if _, ok := miotBound(); ok {
		return s.grabFrameViaMiot(did)
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
// ===== 调用量优化（2026-09-20）=====
//
// ⚠️ 原来每 2 分钟刷新一轮 —— 这是个严重的调用量问题。
//
// 为什么严重：
//
//	每次抓缩略图都要建 P2P 连接（useConnKey）。
//	如果凭据失效，useConnKey 会走到 requestFreshSource
//	-> 调 miss_get_vendor -> 撞限流。
//
//	2 台设备 × 每 2 分钟一轮 = 一天 1440 次建连。
//	而小米限流是【滑动窗口】：需要安静才能重置。
//	我们每 2 分钟就吵醒它一次 -> 窗口永远不重置
//	-> 限流持续 44 小时不解除。
//
// 现在的策略：
//  1. 刷新间隔 2 分钟 -> 30 分钟（降 15 倍）
//  2. 限流静默期内【完全不刷新】（一次都不建连）
//  3. 只刷新「最近被请求过」的设备（有人在看才值得刷）
//
// 用户感知几乎无差别：设备页的缩略图本来也是 12 秒超时兜底，
// 30 分钟内的缓存完全够用。
// startThumbWarmer 缩略图预热器（2026-10-11 重做）。
//
// ===== 为什么取消定时全量刷新 =====
//
// 旧实现：后台定时器每 30 分钟给【所有】在线摄像头抓一遍，
// 不管有没有人在看。用户反馈：
//
//	"抓取缓存的图片不一定要实时的，否则对性能要求很大，
//	 或者会限制、限速。可以按用户点击之后再刷新，
//	 或者点击后的一段时间自动刷新，用户不看就不刷新。"
//
// 新实现：
//
//	· 只在【有人请求缩略图】时才抓（见 handleLiveThumb + thumbShouldGrab）
//	· 活跃窗口 3 分钟、抓帧间隔 30 秒（节流）
//	· 窗口过期后自然停止 —— 不需要后台定时器
//
// 所以这里只保留【一次启动预热】，让首屏有图；之后完全按需。
func (s *Server) startThumbWarmer() {
	go func() {
		// 等页面加载完（前端首屏请求会触发按需抓帧，这里只兜底）
		time.Sleep(45 * time.Second)

		// 预热一次：只给【在线且纳入管理】的摄像头抓一轮。
		// 走官方通道（不受 miss_get_vendor 限流），所以不再判断静默。
		grabbed := 0
		for _, d := range s.allCameras() {
			if !d.IsCamera() || !d.IsOnline {
				continue
			}
			// 已经有内存缓存的跳过
			thumbMu.Lock()
			e, ok := thumbCache[d.Did]
			hasFresh := ok && len(e.Data) > 0 && time.Since(e.Time) < thumbTTL
			thumbMu.Unlock()
			if hasFresh {
				continue
			}
			if !s.thumbShouldGrab(d.Did, time.Now()) {
				continue
			}
			data, err := s.grabFrameViaMiot(d.Did)
			thumbGrabDone(d.Did)
			if err != nil {
				logf("[thumb] 预热失败 did=%s: %v", d.Did, err)
				continue
			}
			thumbMu.Lock()
			thumbCache[d.Did] = &thumbEntry{Data: data, Time: time.Now()}
			thumbMu.Unlock()
			grabbed++
			logf("[thumb] 预热成功 did=%s (%d 字节)", d.Did, len(data))
			time.Sleep(2 * time.Second)
		}
		logf("[thumb] 启动预热完成（%d 台）", grabbed)
	}()
}

// thumbWarmInterval 缩略图后台刷新间隔。
//
// 从 2 分钟放宽到 30 分钟 —— 因为每次刷新都要建 P2P 连接，
// 而建连在凭据失效时会触发云端凭据申请（撞限流）。
const thumbWarmInterval = 30 * time.Minute

// thumbActiveWindow 多久内被请求过的设备才算「有人在看」。
//
// 只有用户实际打开过页面的设备才值得后台刷 ——
// 没人看的设备刷了也没人看到，纯浪费建连。
const thumbActiveWindow = 15 * time.Minute

// refreshStaleThumbs 刷新「最近被看过且缓存已过期」的设备。
func (s *Server) refreshStaleThumbs(width int) {
	thumbMu.Lock()
	type job struct {
		did string
	}
	jobs := make([]job, 0, len(thumbCache))
	now := time.Now()
	for did, e := range thumbCache {
		if e == nil || e.Busy {
			continue
		}
		// 只看最近有人请求过的（LastHit 由 handleLiveThumb 更新）
		if e.LastHit.IsZero() || now.Sub(e.LastHit) > thumbActiveWindow {
			continue
		}
		// 缓存还新鲜就不刷
		if !e.Time.IsZero() && now.Sub(e.Time) < thumbWarmInterval {
			continue
		}
		jobs = append(jobs, job{did: did})
	}
	thumbMu.Unlock()

	for _, j := range jobs {
		// 每台之间再检查一次静默（中途可能刚撞限流）
		if _, ok := miotBound(); !ok && s.throttleSilenced() {
			return
		}
		s.refreshThumbAsync(j.did, width)
		time.Sleep(5 * time.Second)
	}
}
