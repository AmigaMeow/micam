package main

// 缩略图刷新策略（2026-10-11 重做）。
//
// ===== 用户要求 =====
//
//	"抓取缓存的图片不一定要实时的，否则对性能要求很大，
//	 或者会限制、限速。可以按用户点击之后再刷新，
//	 或者点击后的一段时间自动刷新，用户不看就不刷新。"
//
// ===== 设计 =====
//
// 核心思想：**按需刷新，且只对"正在被看"的设备刷新**。
//
//  1. 请求缩略图时记一次「有人在看」（LastHit）
//  2. 只有在「活跃窗口」内被请求过的设备，才允许抓新帧
//  3. 活跃窗口内，两次抓帧之间至少间隔 refreshGap（节流）
//  4. 窗口过期后自动停止抓帧（用户不看了）
//  5. 抓帧走【官方通道】（OAuth2+PPCS），不碰被限流的 miss_get_vendor
//
// 参数取值理由：
//   activeWindow = 3 分钟   用户点进来看时，3 分钟内保持新鲜
//   refreshGap   = 30 秒    活跃期内最多每 30 秒抓一次（够用且省资源）
//   首帧超时     = 12 秒    官方通道建连约 3~5 秒，留余量
//
// 与旧实现的区别：
//   旧: 后台定时器每 30 分钟给【所有】设备抓一遍（不管有没有人看）
//   新: 只在有人看时抓，且节流

import (
	"syscall"
	"context"
	"os/exec"
	"sync"
	"time"

	"github.com/micam/micam/internal/miot"
)

const (
	thumbRefreshGap  = 30 * time.Second // 活跃期内两次抓帧的最小间隔
	thumbGrabTimeout = 25 * time.Second // 单次抓帧的总超时
	thumbFrameWait   = 12 * time.Second // 等首帧的最长时间
)

var (
	thumbGateMu   sync.Mutex
	thumbLastGrab = map[string]time.Time{} // did -> 上次抓帧时刻
	thumbInFlight = map[string]bool{}      // did -> 是否正在抓
)

// thumbShouldGrab 判断是否该为该设备抓新帧。
//
// 条件：
//  1. 在活跃窗口内被请求过（有人在看）
//  2. 距离上次抓帧 >= refreshGap
//  3. 没有正在进行的抓帧
func (s *Server) thumbShouldGrab(did string, lastHit time.Time) bool {
	now := time.Now()
	if lastHit.IsZero() || now.Sub(lastHit) > thumbActiveWindow {
		return false // 没人在看
	}
	thumbGateMu.Lock()
	defer thumbGateMu.Unlock()
	if thumbInFlight[did] {
		return false
	}
	if t, ok := thumbLastGrab[did]; ok && now.Sub(t) < thumbRefreshGap {
		return false
	}
	thumbLastGrab[did] = now
	thumbInFlight[did] = true
	return true
}

// thumbGrabDone 标记抓帧结束。
func thumbGrabDone(did string) {
	thumbGateMu.Lock()
	thumbInFlight[did] = false
	thumbGateMu.Unlock()
}

// grabFrameViaMiot 用官方通道抓一帧 JPEG。
//
// 走 sidecar（PPCS + ffmpeg），拿到第一个 JPEG 就返回。
// 不依赖 miss_get_vendor，所以不受限流影响。
func (s *Server) grabFrameViaMiot(did string) ([]byte, error) {
	// 如果设备正在进行实时预览，或者缓存内有新鲜图片，直接复用缓存，绝不抢占 PPCS 连接槽
	thumbMu.Lock()
	if c, ok := thumbCache[did]; ok && len(c.Data) > 0 {
		if isMiotStreaming(did) || time.Since(c.Time) < 30*time.Second {
			data := make([]byte, len(c.Data))
			copy(data, c.Data)
			thumbMu.Unlock()
			return data, nil
		}
	}
	thumbMu.Unlock()

	token, ok := miotBound()
	if !ok {
		return nil, errNoMiotToken
	}
	libPath := miot.FindLib("")
	sidecar := miot.FindSidecar()
	if libPath == "" || sidecar == "" {
		return nil, errNoMiotLib
	}

	// 取实时 IP（会变）
	var model, ip string
	for _, d := range s.allCameras() {
		if d.Did == did {
			model = d.Model
			ip, _ = s.resolveDeviceIP(d)
			break
		}
	}
	if model == "" {
		return nil, errNoDevice
	}

	// TUTK 老旧机型（如 ipc009 等）：直接利用云端 HLS 流抓取首帧，不走闭源库
	if IsTutkCamera(did, model) {
		hlsURL, err := s.fetchCloudHLSStream(did)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, s.ffmpegPath(),
			"-hide_banner", "-loglevel", "error",
			"-user_agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36",
			"-rw_timeout", "10000000",
			"-i", hlsURL,
			"-vframes", "1",
			"-q:v", "3",
			"-f", "image2",
			"pipe:1",
		)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			if cmd.Process != nil && cmd.Process.Pid > 0 {
				return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
			return nil
		}
		cmd.WaitDelay = 2 * time.Second
		out, err := cmd.Output()
		if err != nil || len(out) == 0 {
			return nil, errNoFrame
		}
		return out, nil
	}

	if ip == "" {
		return nil, errNoDevice
	}

	ctx, cancel := context.WithTimeout(context.Background(), thumbGrabTimeout)
	defer cancel()

	// fps=1 只要一帧就够；ffmpeg 输出 MJPEG，我们取第一个完整 JPEG
	cmd := exec.CommandContext(ctx, miot.PythonBin(),
		sidecar,
		"--lib", libPath,
		"--token", token,
		"--did", did,
		"--model", model,
		"--ip", ip,
		"--channel", "1",
		"--quality", "3", // 高清抓帧（LOW 只有 640x360，太糊）
		"--ffmpeg", s.ffmpegPath(),
		"--infmt", "auto",
		"--w", "1280", // 缩略图 1280 宽更清晰
		"--fps", "2",
		"--out", "-",
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil && cmd.Process.Pid > 0 {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 2 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		_, _ = cmd.Process.Wait()
	}()

	// 从 MJPEG 流里取第一个完整 JPEG（FFD8...FFD9）
	buf := make([]byte, 0, 256*1024)
	tmp := make([]byte, 32*1024)
	deadline := time.Now().Add(thumbFrameWait)
	for time.Now().Before(deadline) {
		n, rerr := stdout.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if s := indexOfJPEGStart(buf); s >= 0 {
				if e := indexOfJPEGEnd(buf, s); e > 0 {
					frame := make([]byte, e+2-s)
					copy(frame, buf[s:e+2])
					return frame, nil
				}
			}
			if len(buf) > 4<<20 { // 超过 4MB 还没完整帧，放弃
				break
			}
		}
		if rerr != nil {
			break
		}
	}
	return nil, errNoFrame
}

func indexOfJPEGStart(b []byte) int {
	for i := 0; i+1 < len(b); i++ {
		if b[i] == 0xFF && b[i+1] == 0xD8 {
			return i
		}
	}
	return -1
}

func indexOfJPEGEnd(b []byte, from int) int {
	for i := from + 2; i+1 < len(b); i++ {
		if b[i] == 0xFF && b[i+1] == 0xD9 {
			return i
		}
	}
	return -1
}

// 错误（errStr 定义在 hub_helper.go）
var (
	errNoMiotToken = errStr("官方通道未授权")
	errNoMiotLib   = errStr("官方通道库不可用")
	errNoDevice    = errStr("设备信息不完整")
	errNoFrame     = errStr("未取到视频帧")
)
