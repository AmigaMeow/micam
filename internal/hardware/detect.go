package hardware

// 硬件能力自动检测。
//
// 设计目标：
//   1. 自动识别 CPU / 核显 / 编码器能力
//   2. 优先用硬件编码（实测 QSV 把 CPU 从 272% 降到 48%）
//   3. 检测失败时优雅降级到软件编码
//
// 支持平台：
//   - Intel 核显 -> QSV / VAAPI
//   - NVIDIA 显卡 -> NVENC
//   - AMD 显卡 -> AMF / VAAPI
//   - 无硬件 -> libx264 软件编码

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Encoder 编码器信息。
type Encoder struct {
	Name        string `json:"name"`   // 编码器名（如 h264_qsv）
	Kind        string `json:"kind"`   // qsv / vaapi / nvenc / amf / software
	Hardware    bool   `json:"hardware"`
	Description string `json:"description"`
}

// Capability 硬件能力。
type Capability struct {
	CPUModel    string    `json:"cpuModel"`
	CPUCores    int       `json:"cpuCores"`
	Arch        string    `json:"arch"`
	HasDRI      bool      `json:"hasDri"`
	DRIDevices  []string  `json:"driDevices"`
	GPUInfo     string    `json:"gpuInfo"`
	Encoders    []Encoder `json:"encoders"`
	Preferred   string    `json:"preferred"`   // 推荐使用的编码器
	DecodeHW    string    `json:"decodeHw"`    // 推荐解码方式
}

var (
	cached   *Capability
	cacheMu  sync.Mutex
)

// Detect 检测硬件能力（带缓存）。
func Detect(ffmpegPath string) *Capability {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if cached != nil {
		return cached
	}
	cached = detect(ffmpegPath)
	return cached
}

// Refresh 强制重新检测。
func Refresh(ffmpegPath string) *Capability {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	cached = detect(ffmpegPath)
	return cached
}

func detect(ffmpegPath string) *Capability {
	c := &Capability{
		Arch:   runtime.GOARCH,
		CPUCores: runtime.NumCPU(),
	}

	// CPU 型号
	if data, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "model name") {
				if i := strings.Index(line, ":"); i > 0 {
					c.CPUModel = strings.TrimSpace(line[i+1:])
				}
				break
			}
		}
	}

	// DRI 设备（核显/显卡）
	if entries, err := os.ReadDir("/dev/dri"); err == nil {
		for _, e := range entries {
			c.DRIDevices = append(c.DRIDevices, e.Name())
			if strings.HasPrefix(e.Name(), "renderD") || strings.HasPrefix(e.Name(), "card") {
				c.HasDRI = true
			}
		}
	}

	// GPU 信息
	if out, err := exec.Command("lspci").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			low := strings.ToLower(line)
			if strings.Contains(low, "vga") || strings.Contains(low, "display") || strings.Contains(low, "3d controller") {
				c.GPUInfo = strings.TrimSpace(line)
				break
			}
		}
	}

	// 可用的编码器
	encoders := listEncoders(ffmpegPath)
	c.Encoders = encoders

	// 判断 GPU / CPU 厂商
	gpuLow := strings.ToLower(c.GPUInfo)
	cpuLow := strings.ToLower(c.CPUModel)
	isAMD := strings.Contains(gpuLow, "amd") || strings.Contains(gpuLow, "advanced micro devices") ||
		strings.Contains(gpuLow, "radeon") || strings.Contains(cpuLow, "amd") || strings.Contains(cpuLow, "ryzen")
	isNvidia := strings.Contains(gpuLow, "nvidia") || strings.Contains(gpuLow, "geforce")
	isIntel := strings.Contains(gpuLow, "intel") || strings.Contains(cpuLow, "intel")

	// 解码方式
	if c.HasDRI {
		if isAMD {
			c.DecodeHW = "vaapi"
		} else if isNvidia {
			c.DecodeHW = "cuda"
		} else if isIntel {
			c.DecodeHW = "qsv"
		} else {
			c.DecodeHW = "vaapi"
		}
	}

	// 选择最优编码器（平台自适应并验证）
	c.Preferred = pickEncoder(ffmpegPath, encoders, c.HasDRI, isAMD, isNvidia, isIntel)

	return c
}

// listEncoders 列出可用的 H264 编码器。
func listEncoders(ffmpegPath string) []Encoder {
	out, err := exec.Command(ffmpegPath, "-hide_banner", "-encoders").Output()
	if err != nil {
		return []Encoder{{Name: "libx264", Kind: "software", Description: "软件编码"}}
	}

	all := string(out)
	var encs []Encoder

	// 按优先级检查（硬件优先）
	candidates := []Encoder{
		{Name: "h264_qsv", Kind: "qsv", Hardware: true, Description: "Intel Quick Sync（推荐）"},
		{Name: "h264_nvenc", Kind: "nvenc", Hardware: true, Description: "NVIDIA NVENC"},
		{Name: "h264_vaapi", Kind: "vaapi", Hardware: true, Description: "VAAPI 通用硬件编码"},
		{Name: "h264_amf", Kind: "amf", Hardware: true, Description: "AMD AMF"},
		{Name: "h264_v4l2m2m", Kind: "v4l2m2m", Hardware: true, Description: "V4L2 硬件编码"},
		{Name: "libx264", Kind: "software", Hardware: false, Description: "软件编码（兼容性最好）"},
	}

	for _, c := range candidates {
		if strings.Contains(all, c.Name) {
			encs = append(encs, c)
		}
	}

	if len(encs) == 0 {
		encs = append(encs, Encoder{Name: "libx264", Kind: "software", Description: "软件编码"})
	}
	return encs
}

// verifyEncoder 测试编码器在当前环境中能否实际工作。
func verifyEncoder(ffmpegPath, name string) bool {
	if name == "libx264" || name == "" {
		return true
	}
	var args []string
	switch {
	case strings.HasSuffix(name, "_qsv"):
		args = []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "color=c=black:s=64x64:d=0.1", "-c:v", "h264_qsv", "-f", "null", "-"}
	case strings.HasSuffix(name, "_nvenc"):
		args = []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "color=c=black:s=64x64:d=0.1", "-c:v", "h264_nvenc", "-f", "null", "-"}
	case strings.HasSuffix(name, "_vaapi"):
		args = []string{"-hide_banner", "-loglevel", "error", "-vaapi_device", "/dev/dri/renderD128", "-f", "lavfi", "-i", "color=c=black:s=64x64:d=0.1", "-vf", "format=nv12,hwupload", "-c:v", "h264_vaapi", "-f", "null", "-"}
	case strings.HasSuffix(name, "_amf"):
		args = []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "color=c=black:s=64x64:d=0.1", "-c:v", "h264_amf", "-f", "null", "-"}
	default:
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	return cmd.Run() == nil
}

// pickEncoder 选择最优编码器（平台自适应，避免 AMD 误用 QSV）。
func pickEncoder(ffmpegPath string, encs []Encoder, hasDRI, isAMD, isNvidia, isIntel bool) string {
	available := make(map[string]bool)
	for _, e := range encs {
		available[e.Name] = true
	}

	var candidates []string
	if isAMD {
		candidates = []string{"h264_vaapi", "h264_amf"}
	} else if isNvidia {
		candidates = []string{"h264_nvenc", "h264_vaapi"}
	} else if isIntel {
		candidates = []string{"h264_qsv", "h264_vaapi"}
	} else {
		candidates = []string{"h264_vaapi", "h264_qsv", "h264_nvenc", "h264_amf"}
	}

	if hasDRI {
		for _, name := range candidates {
			if available[name] && verifyEncoder(ffmpegPath, name) {
				return name
			}
		}
	}
	return "libx264"
}

// BuildEncodeArgs 根据硬件能力生成编码参数。
//
// 返回：编码参数、是否为硬件编码
func (c *Capability) BuildEncodeArgs() ([]string, bool) {
	switch {
	case strings.HasSuffix(c.Preferred, "_qsv"):
		return []string{
			"-c:v", "h264_qsv",
			"-preset", "veryfast",
			"-global_quality", "23",
		}, true

	case strings.HasSuffix(c.Preferred, "_nvenc"):
		return []string{
			"-c:v", "h264_nvenc",
			"-preset", "p1",
			"-cq", "23",
		}, true

	case strings.HasSuffix(c.Preferred, "_vaapi"):
		return []string{
			"-c:v", "h264_vaapi",
			"-qp", "23",
		}, true

	case strings.HasSuffix(c.Preferred, "_amf"):
		return []string{
			"-c:v", "h264_amf",
			"-quality", "speed",
			"-qp_i", "23", "-qp_p", "23",
		}, true

	default:
		return []string{
			"-c:v", "libx264",
			"-preset", "ultrafast",
			"-tune", "zerolatency",
			"-crf", "23",
		}, false
	}
}

// BuildDecodeArgs 生成解码参数。
func (c *Capability) BuildDecodeArgs() []string {
	if !c.HasDRI {
		return nil
	}
	switch c.Preferred {
	case "h264_qsv":
		return []string{"-hwaccel", "qsv", "-qsv_device", "/dev/dri/renderD128"}
	case "h264_vaapi":
		return []string{"-hwaccel", "vaapi", "-hwaccel_device", "/dev/dri/renderD128"}
	default:
		return nil
	}
}
