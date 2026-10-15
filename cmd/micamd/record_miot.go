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
	"strings"
	"sync"
	"time"

	"github.com/micam/micam/internal/miot"
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
	if model == "" || ip == "" {
		return fmt.Errorf("设备信息不完整（model=%s ip=%s）", model, ip)
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

	logf("[rec] ✅ 官方通道录像已启动 did=%s (%s @ %s) -> %s", dev.Did, model, ip, dir)
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

		startAt := time.Now()
		cmd := exec.CommandContext(ctx, miot.PythonBin(),
			sidecar,
			"--lib", libPath,
			"--token", token,
			"--did", j.did,
			"--model", model,
			"--ip", ip,
			"--channel", "1",
			"--channels", "2",
			"--quality", j.quality,
			"--segment", fmt.Sprint(j.segment),
			"--out", j.dir,
			"--ffmpeg", s.ffm,
		)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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
		logf("[rec] 官方通道录像已停止 did=%s（共重启 %d 次）", did, job.restarts)
	}
	return ok
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
