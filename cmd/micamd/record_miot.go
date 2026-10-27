package main

// 录像自动重连（2026-10-11）。
//
// ===== 问题 =====
//
// 官方通道的 sidecar 每次跑约 22 秒就收到 [status] 1（DISCONNECTED），
// 进程退出，录像中断。实测 3 次：446 / 446 / 448 帧后必断。
//
// 官方 camera.py 的解法是 enable_reconnect=True + 指数退避重启
// （__try_start_async）。但那只覆盖 miot_camera_start 失败的场景，
// 且我们要在 sidecar 层管理更简单可控。
//
// ===== 方案：Go 侧看门狗 =====
//
// 检测 sidecar 退出 -> 立即重启 -> 分片继续累积到同一目录。
// 对用户完全透明（录像看起来是连续的，只是多几个分片文件）。
//
// 退避策略：连续失败时逐渐拉长间隔（2s -> 4s -> 8s -> 最多 30s），
// 成功跑够 30 秒就重置退避（说明连接是好的，只是会被踢）。

import (
	"syscall"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/micam/micam/internal/miot"
	"github.com/micam/micam/internal/stream"
	"github.com/micam/micam/internal/xiaomi"
)

// miotRecJob 一个录像任务（可自动重连）。
type miotRecJob struct {
	did     string
	name    string
	dir     string
	quality string
	segment int

	mu       sync.Mutex
	cancel   context.CancelFunc
	stopped  bool
	restarts int
}

var (
	miotRecJobsMu sync.Mutex
	miotRecJobs   = map[string]*miotRecJob{} // did -> job
)

// startRecordingMiot 用官方通道开始录像（含自动重连）。
func (s *Server) startRecordingMiot(dev *xiaomi.Device) error {
	if _, ok := miotBound(); !ok {
		return fmt.Errorf("官方通道未授权")
	}
	libPath := miot.FindLib("")
	if libPath == "" {
		return fmt.Errorf("找不到 libmiot_camera_lite.so")
	}
	sidecar := miot.FindSidecarRaw()
	if sidecar == "" {
		return fmt.Errorf("找不到 miot_camera.py")
	}

	var model, ip string
	for _, d := range s.allCameras() {
		if d.Did == dev.Did {
			model = d.Model
			ip, _ = s.resolveDeviceIP(d)
			break
		}
	}
	if model == "" {
		return fmt.Errorf("设备信息不完整（model 为空）")
	}

	// TUTK 老旧机型通过云端 HLS 兼容通道进行按段录制
	if IsTutkCamera(dev.Did, model) {
		return s.startRecordingTutkCloud(dev, model)
	}

	if ip == "" {
		return fmt.Errorf("设备信息不完整（model=%s ip 为空）", model)
	}

	// 录像目录：<root>/<设备名>/<日期>/
	dir := filepath.Join(s.rec.GetConfig().Dir, sanitizeName(dev.Name),
		time.Now().Format("2006-01-02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建录像目录失败: %w", err)
	}

	seg := int(s.rec.GetConfig().Segment.Seconds())
	if seg <= 0 {
		seg = 300
	}

	ctx, cancel := context.WithCancel(context.Background())
	job := &miotRecJob{
		did: dev.Did, name: dev.Name, dir: dir,
		quality: "3", segment: seg,
		cancel: cancel,
	}

	miotRecJobsMu.Lock()
	if old, ok := miotRecJobs[dev.Did]; ok {
		old.stop()
	}
	miotRecJobs[dev.Did] = job
	miotRecJobsMu.Unlock()

	// 看门狗：跑 sidecar，退出就重启
	go job.run(ctx, s, libPath, sidecar, model, ip)

	// 登记到录制器（界面显示"在录"）
	s.rec.TrackExternal(dev.Did, dev.Name, nil, cancel)

	logf("[rec] 官方通道录像已启动 did=%s (%s @ %s) -> %s", dev.Did, model, ip, dir)
	return nil
}

// run 看门狗主循环：起 sidecar，退出就重启。
func (j *miotRecJob) run(ctx context.Context, s *Server, libPath, sidecar, model, ip string) {
	backoff := 2 * time.Second
	const maxBackoff = 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		token, ok := miotBound()
		if !ok {
			logf("[rec] 官方通道未授权，录像停止 did=%s", j.did)
			return
		}

		// 每次重启前重新解析一次设备最新局域网 IP，避免摄像头重启或 DHCP 换 IP 后一直使用失效 IP 导致录像永久中断
		liveIP := ip
		for _, d := range s.allCameras() {
			if d.Did == j.did {
				if curIP, _ := s.resolveDeviceIP(d); curIP != "" {
					liveIP = curIP
				}
				break
			}
		}

		startAt := time.Now()
		curDir := filepath.Join(s.rec.GetConfig().Dir, sanitizeName(j.name),
			startAt.Format("2006-01-02"))
		_ = os.MkdirAll(curDir, 0o755)
		j.dir = curDir

		recChannels := "1"
		if stream.HasSecondLens(model) {
			recChannels = "2"
		}
		cmd := exec.CommandContext(ctx, miot.PythonBin(),
			sidecar,
			"--lib", libPath,
			"--token", token,
			"--did", j.did,
			"--model", model,
			"--ip", liveIP,
			"--channel", "1",
			"--channels", recChannels,
			"--quality", j.quality,
			"--segment", fmt.Sprint(j.segment),
			"--out", curDir,
			"--ffmpeg", s.ffmpegPath(),
		)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			return cmd.Process.Signal(syscall.SIGTERM)
		}
		cmd.WaitDelay = 4 * time.Second
		stderr, _ := cmd.StderrPipe()
		if err := cmd.Start(); err != nil {
			logf("[rec] sidecar 启动失败 did=%s: %v（%.0f 秒后重试）", j.did, err, backoff.Seconds())
			if !j.sleep(ctx, backoff) {
				return
			}
			backoff = minDur(backoff*2, maxBackoff)
			continue
		}

		go func() {
			buf := make([]byte, 2048)
			for {
				n, e := stderr.Read(buf)
				if n > 0 {
					logf("[rec-miot] %s", trimNL(string(buf[:n])))
				}
				if e != nil {
					return
				}
			}
		}()

		_ = cmd.Wait()
		s.triggerAutoRemux(j.dir)

		select {
		case <-ctx.Done():
			return
		default:
		}

		ran := time.Since(startAt)
		if ran < 5*time.Second {
			// 秒退：可能凭据/网络有问题，退避
			backoff = minDur(backoff*2, maxBackoff)
		} else if ran > 30*time.Second {
			backoff = 2 * time.Second // 跑得够久，说明连接正常，重置
		}

		j.mu.Lock()
		j.restarts++
		n := j.restarts
		j.mu.Unlock()

		logf("[rec] sidecar 退出 did=%s（跑了 %.0f 秒，第 %d 次重启，%.0f 秒后）",
			j.did, ran.Seconds(), n, backoff.Seconds())

		if !j.sleep(ctx, backoff) {
			return
		}
	}
}

func (j *miotRecJob) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (j *miotRecJob) stop() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.stopped {
		return
	}
	j.stopped = true
	if j.cancel != nil {
		j.cancel()
	}
}

// stopRecordingMiot 停掉官方通道的录像（含看门狗）。
func (s *Server) stopRecordingMiot(did string) bool {
	miotRecJobsMu.Lock()
	job, ok := miotRecJobs[did]
	if ok {
		delete(miotRecJobs, did)
	}
	miotRecJobsMu.Unlock()
	if ok {
		job.stop()
		s.triggerAutoRemux(job.dir)
		logf("[rec] 官方通道录像已停止 did=%s（共重启 %d 次）", did, job.restarts)
	}
	return ok
}

// yieldCameraForLive 实时预览请求接入时，若后台录像正在使用该摄像头，主动停止并让出连接
func (s *Server) yieldCameraForLive(did string) {
	if s.stopRecordingMiot(did) {
		logf("[miot] 实时预览请求接入，已主动让出录像连接 did=%s", did)
		time.Sleep(1500 * time.Millisecond) // 等待摄像头硬件会话表释放
	}
}

// resumeRecordingIfNeeded 实时预览结束后，按需自动恢复全天或动体录像
func (s *Server) resumeRecordingIfNeeded(did string) {
	time.Sleep(3 * time.Second) // 等待预览进程退出并释放连接
	if isMiotStreaming(did) {
		return
	}
	dev, err := s.ensureDevice(did)
	if err != nil || dev == nil || !dev.IsOnline {
		return
	}
	if !s.cfg.RecordEnabled {
		return
	}
	if s.cfg.RecordMode == "always" {
		logf("[rec] 实时预览结束，自动恢复全天录像 did=%s", did)
		_ = s.startRecording(dev)
		return
	}
	if s.cfg.RecordMode == "motion" || s.cfg.RecordMode == "ai_event" {
		motionRec.mu.Lock()
		until, ok := motionRec.activeUntil[did]
		motionRec.mu.Unlock()
		if ok && time.Now().Before(until) {
			logf("[rec-motion] 实时预览结束，恢复动体延录 did=%s (剩余 %.0fs)", did, time.Until(until).Seconds())
			_ = s.startRecording(dev)
		}
	}
}

func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func trimNL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// sanitizeName 设备名转安全目录名（与 record 包同规则）。
func sanitizeName(s string) string {
	s = trimNL(s)
	repl := []string{"/", "_", "\\", "_", ":", "_", "*", "_", "?", "_",
		"\"", "_", "<", "_", ">", "_", "|", "_"}
	for i := 0; i < len(repl); i += 2 {
		s = strings.ReplaceAll(s, repl[i], repl[i+1])
	}
	if s == "" {
		s = "camera"
	}
	return s
}


// startRecordingTutkCloud 为 TUTK 早期协议机型启动云端 HLS 兼容录像。
func (s *Server) startRecordingTutkCloud(dev *xiaomi.Device, model string) error {
	dir := filepath.Join(s.rec.GetConfig().Dir, sanitizeName(dev.Name),
		time.Now().Format("2006-01-02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建录像目录失败: %w", err)
	}

	seg := int(s.rec.GetConfig().Segment.Seconds())
	if seg <= 0 {
		seg = 300
	}

	ctx, cancel := context.WithCancel(context.Background())
	job := &miotRecJob{
		did: dev.Did, name: dev.Name, dir: dir,
		quality: "3", segment: seg,
		cancel: cancel,
	}

	miotRecJobsMu.Lock()
	if old, ok := miotRecJobs[dev.Did]; ok {
		old.stop()
	}
	miotRecJobs[dev.Did] = job
	miotRecJobsMu.Unlock()

	go job.runTutkCloud(ctx, s, model)

	s.rec.TrackExternal(dev.Did, dev.Name, nil, cancel)
	logf("[rec] 老旧协议云端兼容录像已启动 did=%s (%s) -> %s", dev.Did, model, dir)
	return nil
}

// runTutkCloud TUTK 早期机型云端 HLS 录像主循环。
func (j *miotRecJob) runTutkCloud(ctx context.Context, s *Server, model string) {
	backoff := 3 * time.Second
	const maxBackoff = 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		hlsURL, err := s.fetchCloudHLSStream(j.did)
		if err != nil {
			logf("[rec-tutk] 获取云端兼容流失败 did=%s: %v (%.0f 秒后重试)", j.did, err, backoff.Seconds())
			if !j.sleep(ctx, backoff) {
				return
			}
			backoff = minDur(backoff*2, maxBackoff)
			continue
		}

		// 预热与就绪探测：避免小米云转码首片 404 引发录像频繁重启
		if !pollM3U8Ready(ctx, hlsURL, 6*time.Second) {
			logf("[rec-tutk] HLS 地址等待云端就绪超时 did=%s，重新申请", j.did)
			invalidateCloudStream(j.did)
			if !j.sleep(ctx, 2*time.Second) {
				return
			}
			continue
		}

		startAt := time.Now()
		curDir := filepath.Join(s.rec.GetConfig().Dir, sanitizeName(j.name),
			startAt.Format("2006-01-02"))
		_ = os.MkdirAll(curDir, 0o755)
		j.dir = curDir

		outFilePattern := filepath.Join(curDir, "%Y%m%d_%H%M%S.mkv")
		cmd := exec.CommandContext(ctx, s.ffmpegPath(),
			"-hide_banner", "-loglevel", "error",
			"-user_agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36",
			"-reconnect", "1",
			"-reconnect_at_eof", "1",
			"-reconnect_streamed", "1",
			"-reconnect_on_http_error", "4xx,5xx",
			"-reconnect_delay_max", "3",
			"-rw_timeout", "15000000",
			"-i", hlsURL,
			"-c", "copy",
			"-f", "segment",
			"-segment_time", strconv.Itoa(j.segment),
			"-reset_timestamps", "1",
			"-strftime", "1",
			outFilePattern,
		)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			if cmd.Process != nil && cmd.Process.Pid > 0 {
				return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
			return nil
		}
		cmd.WaitDelay = 3 * time.Second

		if err := cmd.Start(); err != nil {
			logf("[rec-tutk] ffmpeg 录像启动失败 did=%s: %v", j.did, err)
			if !j.sleep(ctx, backoff) {
				return
			}
			backoff = minDur(backoff*2, maxBackoff)
			continue
		}

		_ = cmd.Wait()

		select {
		case <-ctx.Done():
			return
		default:
		}

		ran := time.Since(startAt)
		if ran > 30*time.Second {
			backoff = 3 * time.Second
		} else {
			backoff = minDur(backoff*2, maxBackoff)
		}

		if !j.sleep(ctx, backoff) {
			return
		}
	}
}
