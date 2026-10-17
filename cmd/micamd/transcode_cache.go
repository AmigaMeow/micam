package main

// 转码缓存 + 编码器自适应。
//
// ===== 为什么需要缓存 =====
//
// 录像回放要实时转码（MKV/HEVC -> MP4/H264）。
// 2K 素材软件编码大约 0.6 倍速，看 1 分钟要等 1.6 分钟 —— 体验很差。
//
// 缓存后：
//   首次播放  仍要转码（后台同时写入缓存）
//   二次播放  直接发缓存 MP4，支持 Range（秒开 + 随意拖动）
//
// ===== 缓存键包含编码器 =====
//
// 缓存文件名 = <原文件>.<编码器指纹>.mp4
// 这样换硬件（或硬件权限变化）后会自动重新生成，不会用错缓存。
//
// ===== 硬件自适应 =====
//
// 编码器优先级：
//   qsv   Intel 核显
//   nvenc NVIDIA
//   vaapi 通用（AMD/Intel）
//   amf   AMD
//   libx264 软件兜底
//
// **关键**：候选编码器必须实测可用，不能只看 `ffmpeg -encoders`。
// 实测坑：服务进程里 h264_qsv 报 "Failed to create a VAAPI device"，
// 但 shell 里能跑（权限差异）。所以这里会逐个试。

import (
	"bytes"
	"encoding/binary"
	"io"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/micam/micam/internal/hardware"
)

// ---- 缓存的编码器指纹 ----

var (
	encFPMu  sync.Mutex
	encFP    string // 已确认可用的编码器（进程内缓存）
	encTried = map[string]error{}
)

// pickFileEncoder 选一个实测可用的文件转码编码器。
//
// 返回编码器名（如 h264_qsv / libx264）和对应的 ffmpeg 参数。
func (s *Server) pickFileEncoder(sample string) (string, []string) {
	encFPMu.Lock()
	if encFP != "" {
		name := encFP
		encFPMu.Unlock()
		return name, encoderArgs(name)
	}
	encFPMu.Unlock()

	cap := hardware.Detect(s.ffmpegPath())

	// 候选顺序：按性能排
	//
	// 排除 h264_v4l2m2m：它是通用 V4L2 包装器，
	// 探测能过但实际编码不出数据（实测踩过）。
	var candidates []string
	for _, e := range cap.Encoders {
		if !e.Hardware {
			continue
		}
		if strings.Contains(e.Name, "v4l2m2m") {
			continue
		}
		candidates = append(candidates, e.Name)
	}
	candidates = append(candidates, "libx264")

	for _, name := range candidates {
		encFPMu.Lock()
		err, tried := encTried[name]
		encFPMu.Unlock()
		if tried && err != nil {
			continue
		}

		if !tried {
			err = probeEncoder(s.ffmpegPath(), name, sample)
			encFPMu.Lock()
			encTried[name] = err
			encFPMu.Unlock()
		}
		if err != nil {
			logf("[enc] %s 不可用: %v", name, err)
			continue
		}

		logf("[enc] 选定编码器: %s", name)
		encFPMu.Lock()
		encFP = name
		encFPMu.Unlock()
		return name, encoderArgs(name)
	}

	// 理论上 libx264 一定可用；真到这一步说明 ffmpeg 有问题
	return "libx264", encoderArgs("libx264")
}

// encoderArgs 返回编码器参数。
func encoderArgs(name string) []string {
	switch {
	case strings.HasSuffix(name, "_qsv"):
		return []string{"-c:v", "h264_qsv", "-preset", "veryfast", "-global_quality", "26"}
	case strings.HasSuffix(name, "_nvenc"):
		return []string{"-c:v", "h264_nvenc", "-preset", "p1", "-cq", "26"}
	case strings.HasSuffix(name, "_vaapi"):
		return []string{"-vaapi_device", vaapiDevice(), "-vf", "format=nv12,hwupload", "-c:v", "h264_vaapi", "-qp", "26"}
	case strings.HasSuffix(name, "_amf"):
		return []string{"-c:v", "h264_amf", "-quality", "speed", "-qp_i", "26", "-qp_p", "26"}
	default:
		return []string{"-c:v", "libx264", "-preset", "ultrafast", "-crf", "28", "-tune", "zerolatency"}
	}
}

func vaapiDevice() string {
	for _, d := range []string{"/dev/dri/renderD128", "/dev/dri/renderD129"} {
		if _, err := os.Stat(d); err == nil {
			return d
		}
	}
	return "/dev/dri/renderD128"
}

// probeEncoder 实测编码器。
//
// **必须验证真的产出了数据** —— 有些编码器（h264_v4l2m2m）
// 退出码为 0 但一帧都没编出来。
//
// 做法：编 1 秒到临时文件，检查文件大小。
func probeEncoder(ffmpegPath, name, sample string) error {
	tmp, err := os.CreateTemp("", "encprobe-*.mp4")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(tmpPath)

	args := []string{"-hide_banner", "-loglevel", "error", "-y"}
	if strings.HasSuffix(name, "_qsv") {
		args = append(args, "-hwaccel", "qsv", "-hwaccel_output_format", "qsv")
	}
	useSample := false
	if sample != "" {
		if fi, err := os.Stat(sample); err == nil && fi.Size() > 1024 {
			useSample = true
		}
	}
	if useSample {
		args = append(args, "-t", "1", "-i", sample)
	} else {
		args = append(args, "-f", "lavfi", "-i", "testsrc=size=640x360:rate=15", "-t", "1")
	}
	args = append(args, encoderArgs(name)...)
	args = append(args, "-f", "mp4", tmpPath)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, ffmpegPath, args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 150 {
			msg = msg[:150]
		}
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s", msg)
	}

	// 关键：检查是否真的产出数据
	fi, err := os.Stat(tmpPath)
	if err != nil || fi.Size() < 1024 {
		return fmt.Errorf("编码器无输出（退出码 0 但文件为空）")
	}
	return nil
}

// ---- 缓存路径 ----

// transcodeCacheDir 转码缓存目录（始终存放在本地高速 SSD 应用数据目录，杜绝写入慢速网络挂载/云盘导致 seek 报错）
func (s *Server) transcodeCacheDir() string {
	if s != nil && s.dataDir != "" {
		return filepath.Join(s.dataDir, "transcode_cache")
	}
	return filepath.Join(s.recRoot(), ".cache")
}

// cachePath 计算缓存文件路径。
//
// 文件名带编码器指纹，换硬件后自动失效。
func (s *Server) cachePath(enc, name, date, file string) string {
	return filepath.Join(s.transcodeCacheDir(), name, date, file+"."+enc+".mp4")
}

// findCachePath 跨多录像池/卷查找或分配转码缓存文件路径。
// 优先在源文件所属存储池的 .cache 下寻找（无论何种硬件编码指纹，只要已存在且有效即可秒开）。
// 其次在当前活跃录像池及其它存储池中查找。若均未命中，返回首选写入路径。
func (s *Server) findCachePath(enc, name, date, file, src string) string {
	localExact := filepath.Join(s.transcodeCacheDir(), name, date, file+"."+enc+".mp4")
	if cacheValid(localExact, src) {
		return localExact
	}
	if matches, err := filepath.Glob(filepath.Join(s.transcodeCacheDir(), name, date, file+".*.mp4")); err == nil {
		for _, m := range matches {
			if !strings.HasSuffix(m, ".part") && cacheValid(m, src) {
				return m
			}
		}
	}

	roots := s.recRoots()
	for _, r := range roots {
		legacy := filepath.Join(r, ".cache", name, date, file+"."+enc+".mp4")
		if cacheValid(legacy, src) {
			return legacy
		}
		if matches, err := filepath.Glob(filepath.Join(r, ".cache", name, date, file+".*.mp4")); err == nil {
			for _, m := range matches {
				if !strings.HasSuffix(m, ".part") && cacheValid(m, src) {
					return m
				}
			}
		}
	}

	return localExact
}

// hasValidMP4Structure 严格校验快开 MP4 文件的结构完整性。
// 必须同时包含 moov、trak、mdat 元数据原子，且 mvhd 头部尺寸合法，杜绝截断或 faststart 移位失败的损坏缓存。
func hasValidMP4Structure(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil || fi.Size() < 4096 {
		return false
	}

	readLen := int64(4 * 1024 * 1024)
	if fi.Size() < readLen {
		readLen = fi.Size()
	}
	buf := make([]byte, readLen)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return false
	}
	data := buf[:n]

	if !bytes.Contains(data, []byte("moov")) ||
		!bytes.Contains(data, []byte("trak")) ||
		!bytes.Contains(data, []byte("mdat")) {
		return false
	}

	mvhdIdx := bytes.Index(data, []byte("mvhd"))
	if mvhdIdx < 4 {
		return false
	}
	mvhdSize := binary.BigEndian.Uint32(data[mvhdIdx-4 : mvhdIdx])
	// ISO 标准 mvhd: v0 为 108 字节 (0x6c)，v1 为 120 字节 (0x78)
	if mvhdSize != 108 && mvhdSize != 120 {
		return false
	}

	return true
}

func hasMoovAtom(path string) bool {
	return hasValidMP4Structure(path)
}

// cacheValid 缓存是否可用（存在、比源文件新且 MP4 结构有效）。
func cacheValid(dst, src string) bool {
	di, err := os.Stat(dst)
	if err != nil || di.Size() < 4096 {
		return false
	}
	si, err := os.Stat(src)
	if err != nil {
		return false
	}
	if di.ModTime().Before(si.ModTime().Add(-2 * time.Second)) {
		return false
	}
	if !hasValidMP4Structure(dst) {
		logf("[cache] 发现损坏缓存（MP4 结构损坏），自动清除: %s", filepath.Base(dst))
		_ = os.Remove(dst)
		return false
	}
	return true
}

// ---- 缓存清理 ----

// cacheMaxBytes 缓存上限（从配置读，默认 50GB）。
func (s *Server) cacheMaxBytes() int64 {
	gb := s.cfg.TranscodeCacheGB
	if gb <= 0 {
		gb = 50
	}
	return int64(gb) << 30
}

// evictCache 超限时删最旧的缓存（跨所有存储池聚合 LRU 清理）。
func (s *Server) evictCache() {
	roots := s.recRoots()

	type ent struct {
		path string
		size int64
		mod  time.Time
	}
	var all []ent
	var total int64

	for _, root := range roots {
		dir := filepath.Join(root, ".cache")
		_ = filepath.Walk(dir, func(pth string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() {
				return nil
			}
			all = append(all, ent{pth, info.Size(), info.ModTime()})
			total += info.Size()
			return nil
		})
	}

	if total <= s.cacheMaxBytes() {
		return
	}

	// 最旧的先删 (LRU: 访问/修改时间升序排列)
	sort.Slice(all, func(i, j int) bool {
		return all[i].mod.Before(all[j].mod)
	})

	var freed int64
	for _, e := range all {
		if total-freed <= s.cacheMaxBytes()*8/10 {
			break
		}
		if err := os.Remove(e.path); err == nil {
			freed += e.size
		}
	}
	if freed > 0 {
		logf("[cache] 清理转码缓存 %.1fGB", float64(freed)/(1<<30))
	}
}

// cacheStats 缓存统计（跨所有存储池聚合统计）。
func (s *Server) cacheStats() map[string]any {
	roots := s.recRoots()
	var total int64
	var count int
	for _, root := range roots {
		dir := filepath.Join(root, ".cache")
		_ = filepath.Walk(dir, func(pth string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() {
				return nil
			}
			total += info.Size()
			count++
			return nil
		})
	}
	encFPMu.Lock()
	enc := encFP
	encFPMu.Unlock()
	return map[string]any{
		"dir":     filepath.Join(s.recRoot(), ".cache"),
		"count":   count,
		"totalMB": float64(total) / (1 << 20),
		"maxMB":   float64(s.cacheMaxBytes()) / (1 << 20),
		"encoder": enc,
	}
}
