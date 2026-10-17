package main

// 官方通道实时流（2026-10-11）。
//
// 与 live.go 的区别：
//   live.go  走 miss_get_vendor → CS2（被小米限流）
//   本文件    走 OAuth2 → PPCS（官方通道，不受限流影响）
//
// 链路:
//   HTTP 请求 → 起 sidecar（app/miot/miot_mjpeg.py）
//              → sidecar 用 access_token 建 PPCS 连接
//              → ffmpeg 把 H264/H265 转 MJPEG
//              → 我们把 stdout 直接转发给浏览器
//
// 接口:
//   GET /api/miot/live?did=xxx&w=640&fps=8   MJPEG 流
//   GET /api/miot/cameras                    官方通道可见的摄像头

import (
	"strings"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/micam/micam/internal/miot"
	"github.com/micam/micam/internal/stream"
)

// miotBound 返回当前是否已绑定官方通道。
func miotBound() (string, bool) {
	miotAuthMu.Lock()
	defer miotAuthMu.Unlock()
	if miotAuthSt == nil || miotAuthSt.AccessToken == "" {
		return "", false
	}
	return miotAuthSt.AccessToken, true
}

// handleMiotLive GET /api/miot/live?did=&w=&fps=&channel=
//
// 用官方通道拉 MJPEG。失败时返回 JSON 错误（不是半截流）。
func (s *Server) handleMiotLive(w http.ResponseWriter, r *http.Request) {
	token, ok := miotBound()
	if !ok {
		writeErr(w, 400, "尚未绑定官方通道，请先调用 /api/miot/bind 完成授权")
		return
	}
	libPath := miot.FindLib("")
	if libPath == "" {
		// 闭源库缺失 -> 回退到纯 Go 路径（cs2 协议）
		//
		// 2026-10-13 实测确认：CW500（vendor 4）与 CW300（vendor cs2）
		// 都可以走 cs2 纯 Go 路径，不需要闭源库。
		// 闭源库只在"账号被 miss_get_vendor 限流且无有效凭据缓存"时才需要。
		//
		// 注意：纯 Go 路径的 handleMJPEG 目前不支持 channel 参数
		// （双摄第二路），回退时双摄会退化为单路。
		logf("[miot] 未找到闭源库，/api/miot/live 回退到纯 Go(cs2) 路径")
		s.handleMJPEG(w, r)
		return
	}
	sidecar := miot.FindSidecar()
	if sidecar == "" {
		writeErr(w, 503, "找不到 miot_mjpeg.py sidecar")
		return
	}

	did := r.URL.Query().Get("did")
	if did == "" {
		writeErr(w, 400, "缺少 did")
		return
	}

	// 找设备（要 model 和局域网 IP）
	var dev *xiaomiDevice
	for _, d := range s.allCameras() {
		if d.Did == did {
			// ⚠️ IP 必须实时查 —— 摄像头 IP 会变，用缓存会 PPCS_Connect -3
			ip, src := s.resolveDeviceIP(d)
			logf("[miot] %s 使用 %s IP: %s (%s)", did, src, ip, d.Model)
			dev = &xiaomiDevice{Name: d.Name, Model: d.Model, IP: ip}
			break
		}
	}
	if dev == nil {
		writeErr(w, 404, "未知设备")
		return
	}

	w0 := r.URL.Query().Get("w")
	if w0 == "" {
		w0 = "1280"
	}
	// 摄像头原生约 17-18 fps。原来默认 8 太保守（画面不够流畅），
	// 实测 ffmpeg 转 MJPEG 在 2K 下只占 3% CPU，提到 15 无压力。
	fps := r.URL.Query().Get("fps")
	if fps == "" {
		fps = "15"
	}
	ch := r.URL.Query().Get("channel")
	if ch == "" {
		ch = "1"
	}
	// 画质档位（官方库：LOW=1, HIGH=3）。
	// 前端「2K 画质」必须传 3，否则只会拿到 640x360 的低码率流。
	q := r.URL.Query().Get("q")
	if q == "" {
		q = "3" // 默认高清（原画）
	}
	// JPEG 质量：2K 用 8 省带宽（实测 2K q:v=6 是 27Mbps，偏大）；
	// 小分辨率本来码率就低，可以用更好的质量。
	qv := r.URL.Query().Get("qv")
	if qv == "" {
		if w0 == "0" {
			qv = "10" // 2K/3K 原画（兼顾画质与网络吞吐）
		} else if w0 == "1280" {
			qv = "6" // 720p 高清（平衡画质与极低延迟）
		} else {
			qv = "5" // 360p 流畅
		}
	}

	channels := "1"
	if stream.HasSecondLens(dev.Model) {
		channels = "2"
	}
	args := []string{
		sidecar,
		"--lib", libPath,
		"--token", token,
		"--did", did,
		"--model", dev.Model,
		"--ip", dev.IP,
		"--channel", ch, // 镜头号（1 起）
		"--channels", channels, // 动态匹配单摄(1)与双摄(2)
		"--ffmpeg", s.ffm,
		"--w", w0,
		"--fps", fps,
		"--infmt", "auto",
		"--quality", q,
		"--qv", qv,
		"--out", "-",
	}

	// ⚠️ 每设备串行化：摄像头同一时刻只接受一条 PPCS 连接。
	//    切画质时前端会立刻发新流，必须先把旧流踢掉，否则新流建不上。
	ctx, cancel, finishStream := beginMiotStream(r.Context(), did)
	defer cancel()
	defer finishStream()

	cmd := exec.CommandContext(ctx, miot.PythonBin(), args...)
	// 独立进程组：超时时能连 ffmpeg 子进程一起杀掉，不留孤儿。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		writeErr(w, 500, "启动失败: "+err.Error())
		return
	}
	stderr, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		writeErr(w, 500, "启动 sidecar 失败: "+err.Error())
		return
	}
	logf("[miot] 官方通道拉流 did=%s model=%s ip=%s", did, dev.Model, dev.IP)

	// ===== 空闲看门狗（2026-10-13）=====
	//
	// 教训：sidecar 卡住不出帧时，旧代码会永远阻塞在 stdout.Read()，
	// sidecar 一直占着摄像头唯一的 PPCS 连接槽 —— 连米家 App 都会被挤掉
	// （用户报「超过最大同时连接数量」）。
	// 现在：20 秒没有任何真实帧就主动杀掉，把连接槽让出来。
	var lastData atomic.Int64
	lastData.Store(time.Now().Unix())
	go func() {
		tk := time.NewTicker(3 * time.Second)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				if time.Now().Unix()-lastData.Load() > 20 {
					logf("[miot] 空闲超时 20s，杀掉 sidecar 释放连接槽 did=%s", did)
					go s.forceMiotRefresh()
					cancel()
					return
				}
			}
		}
	}()

	// sidecar 日志转进插件日志（便于诊断 401 之类与 TUTK 限制）
	go func() {
		buf := make([]byte, 2048)
		for {
			n, e := stderr.Read(buf)
			if n > 0 {
				msg := string(buf[:n])
				logf("[miot] %s", msg)
				if strings.Contains(msg, "Can't support TUTK vendor") || strings.Contains(msg, "miss_client_vendor") {
					tutkDevicesMu.Lock()
					tutkDevices[did] = true
					tutkDevicesMu.Unlock()
					logf("[miot] 设备 %s (model=%s) 采用 TUTK P2P 协议，官方闭源组件无法直连", did, dev.Model)
				}
			}
			if e != nil {
				return
			}
		}
	}()

	// ===== MJPEG 输出（2026-10-11 修正）=====
	//
	// ⚠️ 之前直接写 sidecar 的裸字节流，但 Content-Type 声明的是 multipart。
	//    浏览器按 multipart 解析却找不到分隔符 -> naturalWidth=0 -> 黑屏。
	//    必须自己把连续 JPEG 切出来，组装成标准 multipart 帧。
	const boundary = "mjpegframe"

	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+boundary)
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")

	flusher, _ := w.(http.Flusher)
	w.WriteHeader(200)
	if flusher != nil {
		flusher.Flush()
	}

	// 立即输出首帧封面（0ms出图，彻底杜绝浏览器启动等待期间黑屏或Safari [?] 破损图）
	thumbMu.Lock()
	if e, ok := thumbCache[did]; ok && len(e.Data) > 0 {
		initialFrame := make([]byte, len(e.Data))
		copy(initialFrame, e.Data)
		thumbMu.Unlock()
		head := "--" + boundary + "\r\n" +
			"Content-Type: image/jpeg\r\n" +
			"Content-Length: " + strconv.Itoa(len(initialFrame)) + "\r\n\r\n"
		_, _ = w.Write([]byte(head))
		_, _ = w.Write(initialFrame)
		_, _ = w.Write([]byte("\r\n"))
		if flusher != nil {
			flusher.Flush()
		}
	} else {
		thumbMu.Unlock()
	}

	var pending []byte
	readBuf := make([]byte, 64*1024)
	sent := 0
	realFrames := 0
	sentBytes := 0
	startAt := time.Now()
	skipN := 0 // 极速首帧优化：首帧一解码立即推送给前端，零丢弃等待

	for {
		n, rerr := stdout.Read(readBuf)
		if n > 0 {
			lastData.Store(time.Now().Unix())
		}
		if n > 0 {
			pending = append(pending, readBuf[:n]...)

			// 尽可能多地切出完整帧
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

			for i, frame := range frames {
				if skipN > 0 {
					skipN--
					continue
				}
				// 丢弃滞后帧：若积压多帧，只推送最后一帧，保证极低延迟
				if len(frames) > 1 && i < len(frames)-1 {
					continue
				}

				// 标准 multipart 帧
				head := "--" + boundary + "\r\n" +
					"Content-Type: image/jpeg\r\n" +
					"Content-Length: " + strconv.Itoa(len(frame)) + "\r\n\r\n"
				if _, werr := w.Write([]byte(head)); werr != nil {
					break
				}
				if _, werr := w.Write(frame); werr != nil {
					break
				}
				if _, werr := w.Write([]byte("\r\n")); werr != nil {
					break
				}
				sent++
				sentBytes += len(frame)
				// 真实图像帧计数（<1KB 的是 x265 碎片/控制帧，不是画面）
				if len(frame) > 1024 {
					if realFrames == 0 {
						// 用户正在观看实时流：将第一张清晰画面更新至缩略图缓存（零额外开销）
						frameCopy := make([]byte, len(frame))
						copy(frameCopy, frame)
						thumbMu.Lock()
						thumbCache[did] = &thumbEntry{
							Data:    frameCopy,
							Time:    time.Now(),
							LastHit: time.Now(),
						}
						thumbMu.Unlock()
						logf("[thumb] 实时流首帧自动更新缩略图 did=%s (%d 字节)", did, len(frame))
					}
					realFrames++
				}
				if flusher != nil {
					flusher.Flush()
				}
				// 每 30 帧更新一次统计（前端 2.5 秒轮询一次）
				if sent%15 == 0 {
					// 传真实图像帧数，否则帧率会被碎片帧虚高
					recordMiotPush(did, startAt, int64(realFrames), int64(sentBytes))
				}
			}
			if len(pending) > 8<<20 {
				pending = pending[len(pending)-1<<20:]
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				logf("[miot] 流结束: %v", rerr)
			}
			break
		}
	}
	logf("[miot] 推流结束 did=%s（真实帧 %d / 总帧 %d，%d 字节）", did, realFrames, sent, sentBytes)
	recordMiotPush(did, startAt, int64(realFrames), int64(sentBytes))
	_ = cmd.Wait()
}

// xiaomiDevice 传给 sidecar 的设备信息（避免依赖 xiaomi 包的字段名）。
type xiaomiDevice struct {
	Name  string
	Model string
	IP    string
	Token string // miIO token（来自小米云 device_list）
}

// handleMiotCameras GET /api/miot/cameras
//
// 列出可用官方通道拉流的摄像头（有 model + 局域网 IP 的）。
func (s *Server) handleMiotCameras(w http.ResponseWriter, r *http.Request) {
	libPath := miot.FindLib("")
	sidecar := miot.FindSidecar()
	_, bound := miotBound()

	type item struct {
		Did      string `json:"did"`
		Name     string `json:"name"`
		Model    string `json:"model"`
		IP       string `json:"ip"`
		Online   bool   `json:"online"`
		CanMiot  bool   `json:"canMiot"`
		IPSource string `json:"ipSource"`
	}
	var out []item
	for _, d := range s.allCameras() {
		// 这里也用实时 IP，界面显示的才是真的
		ip := s.liveDeviceIP(d.Did)
		src := "live"
		if ip == "" {
			ip = d.LocalIP
			src = "cache"
		}
		can := d.Model != "" && ip != ""
		out = append(out, item{
			Did: d.Did, Name: d.Name, Model: d.Model,
			IP: ip, Online: d.IsOnline, CanMiot: can, IPSource: src,
		})
	}
	writeJSON(w, 200, map[string]any{
		"bound":        bound,
		"libPath":      libPath,
		"sidecar":      sidecar,
		"python":       miot.PythonBin(),
		"ffmpeg":       s.ffm,
		"libAvailable": libPath != "",
		"cameras":      out,
		"generatedAt":  time.Now().Format(time.RFC3339),
	})
}

// 让编译器知道我们用了 json（保留扩展点）
var _ = json.Marshal
var _ = fmt.Sprintf
var _ = strconv.Itoa
