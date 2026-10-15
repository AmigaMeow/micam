package stream

// 播放输出模块。
//
// 设计考虑：
//   - 源流是 H.265，浏览器普遍不支持（Chrome/Safari 都不能直接播 HEVC over FLV）
//   - 所以服务端用 ffmpeg 转成 H.264，再输出给浏览器
//   - 提供三种输出：MJPEG（快照流）、HLS（片段）、FLV（实时）
//
// 关键：ffmpeg 从 go2rtc 的 RTSP 输出拉流，或直接从 xiaomi:// 解封装
// 这里采用最稳的方案：go2rtc Producer 解封装 -> 管道给 ffmpeg -> 转码

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
)

// Transcoder 管理一次转码会话。
type Transcoder struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

// FFmpegPath ffmpeg 可执行文件路径。
// fnOS 上系统自带 /usr/bin/ffmpeg；插件也可自带。
var FFmpegPath = "ffmpeg"

// StartMJPEG 启动 MJPEG 转码（最兼容，浏览器 <img> 即可显示）。
// 参数：
//   - input: 输入地址（RTSP/FLV 等 ffmpeg 能认的）
//   - fps:   输出帧率
//   - width: 输出宽度（0=原始）
func StartMJPEG(input string, fps int, width int) (*Transcoder, error) {
	if fps <= 0 {
		fps = 5
	}
	filter := fmt.Sprintf("fps=%d", fps)
	if width > 0 {
		filter += fmt.Sprintf(",scale=%d:-2", width)
	}

	ctx, cancel := context.WithCancel(context.Background())
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-fflags", "nobuffer",
		"-flags", "low_delay",
		"-rtsp_transport", "tcp",
		"-i", input,
		"-vf", filter,
		"-c:v", "mjpeg",
		"-q:v", "7",
		"-f", "mpjpeg",
		"-boundary", "mjpegstream",
		"pipe:1",
	}
	cmd := exec.CommandContext(ctx, FFmpegPath, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("启动 ffmpeg 失败: %w", err)
	}
	return &Transcoder{cmd: cmd, stdout: stdout, cancel: cancel}, nil
}

// StartHLS 启动 HLS 转码（输出到目录）。
func StartHLS(input, outDir string, segDur int) (*Transcoder, error) {
	if segDur <= 0 {
		segDur = 2
	}
	ctx, cancel := context.WithCancel(context.Background())
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-rtsp_transport", "tcp",
		"-i", input,
		"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
		"-c:a", "aac", "-b:a", "64k",
		"-f", "hls",
		"-hls_time", fmt.Sprint(segDur),
		"-hls_list_size", "6",
		"-hls_flags", "delete_segments+omit_endlist",
		outDir + "/index.m3u8",
	}
	cmd := exec.CommandContext(ctx, FFmpegPath, args...)
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("启动 HLS 转码失败: %w", err)
	}
	return &Transcoder{cmd: cmd, cancel: cancel}, nil
}

// Read 读取转码输出。
func (t *Transcoder) Read(p []byte) (int, error) {
	if t.stdout == nil {
		return 0, fmt.Errorf("无输出流")
	}
	return t.stdout.Read(p)
}

// Stop 停止转码。
func (t *Transcoder) Stop() {
	t.once.Do(func() {
		if t.cancel != nil {
			t.cancel()
		}
		if t.cmd != nil && t.cmd.Process != nil {
			time.Sleep(100 * time.Millisecond)
			_ = t.cmd.Process.Kill()
			_, _ = t.cmd.Process.Wait()
		}
	})
}

// SnapshotJPEG 抓一帧 JPEG（用于缩略图）。
func SnapshotJPEG(input string, width int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	filter := "scale=" + fmt.Sprint(width) + ":-2"
	if width <= 0 {
		filter = "null"
	}
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-rtsp_transport", "tcp",
		"-i", input,
		"-frames:v", "1",
		"-vf", filter,
		"-f", "image2",
		"-c:v", "mjpeg",
		"pipe:1",
	}
	cmd := exec.CommandContext(ctx, FFmpegPath, args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("抓帧失败: %w", err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("抓帧返回空")
	}
	return out, nil
}

var _ = core.KindVideo
