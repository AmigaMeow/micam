package record

// 录像调度模块。
//
// 设计：
//   - 用 ffmpeg 的 segment muxer 做分段录像（每 N 秒一个文件）
//   - 输入来自 stream hub 的 MPEG-TS 管道
//   - 输出 MKV（对 HEVC 友好，且断电不损坏整段）
//
// 存储策略：
//   - 按 设备/日期 分目录
//   - 超过保留天数自动清理
//   - 磁盘水位保护：低于阈值停止录像

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Config 录像配置。
type Config struct {
	Enabled     bool          // 是否启用
	Dir         string        // 录像根目录
	Segment     time.Duration // 分片时长
	KeepDays    int           // 保留天数
	MinFreeGB   int           // 磁盘最低剩余空间
	FFmpegPath  string
}

// Recorder 一台摄像头的录像器。
type Recorder struct {
	Did     string
	Name    string
	cfg     Config
	mu      sync.Mutex
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	running bool
	lastErr error
	started time.Time
	segments int
}

// Manager 录像管理器。
type Manager struct {
	mu        sync.Mutex
	recorders map[string]*Recorder
	cfg       Config
}

// NewManager 创建管理器。
func NewManager(cfg Config) *Manager {
	if cfg.Segment <= 0 {
		cfg.Segment = 5 * time.Minute
	}
	if cfg.KeepDays <= 0 {
		cfg.KeepDays = 7
	}
	if cfg.MinFreeGB <= 0 {
		cfg.MinFreeGB = 10
	}
	if cfg.FFmpegPath == "" {
		cfg.FFmpegPath = "ffmpeg"
	}
	return &Manager{recorders: map[string]*Recorder{}, cfg: cfg}
}

// SetConfig 更新配置。
func (m *Manager) SetConfig(cfg Config) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg = cfg
}

// GetConfig 取配置。
func (m *Manager) GetConfig() Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

// Start 开始录像。
// input 是 MPEG-TS 数据源（从 hub 订阅）。
func (m *Manager) Start(did, name string, input io.Reader) error {
	m.mu.Lock()
	cfg := m.cfg
	if !cfg.Enabled {
		m.mu.Unlock()
		return fmt.Errorf("录像功能未启用")
	}
	if r, ok := m.recorders[did]; ok && r.running {
		m.mu.Unlock()
		return nil // 已在录
	}
	m.mu.Unlock()

	// 检查磁盘
	if err := checkDisk(cfg.Dir, cfg.MinFreeGB); err != nil {
		return err
	}

	// 建目录：<root>/<设备名>/<日期>/
	day := time.Now().Format("2006-01-02")
	dir := filepath.Join(cfg.Dir, sanitize(name), day)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建录像目录失败: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	pattern := filepath.Join(dir, "%Y%m%d_%H%M%S.mkv")
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "mpegts", "-i", "pipe:0",
		"-c", "copy", // 不转码，直接存
		"-f", "segment",
		"-segment_time", fmt.Sprint(int(cfg.Segment.Seconds())),
		"-segment_format", "matroska",
		"-segment_atclocktime", "1",              // 按整点切分
		"-reset_timestamps", "1",
		"-strftime", "1",
		pattern,
	}
	cmd := exec.CommandContext(ctx, cfg.FFmpegPath, args...)
	cmd.Stdin = input

	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("启动录像失败: %w", err)
	}

	r := &Recorder{
		Did:     did,
		Name:    name,
		cfg:     cfg,
		cmd:     cmd,
		cancel:  cancel,
		running: true,
		started: time.Now(),
	}

	m.mu.Lock()
	m.recorders[did] = r
	m.mu.Unlock()

	go func() {
		err := cmd.Wait()
		r.mu.Lock()
		r.running = false
		r.lastErr = err
		r.mu.Unlock()
	}()

	return nil
}

// Stop 停止录像。
func (m *Manager) Stop(did string) {
	m.mu.Lock()
	r, ok := m.recorders[did]
	m.mu.Unlock()
	if !ok {
		return
	}
	r.Stop()
}

// StopAll 停止所有录像。
func (m *Manager) StopAll() {
	m.mu.Lock()
	list := make([]*Recorder, 0, len(m.recorders))
	for _, r := range m.recorders {
		list = append(list, r)
	}
	m.mu.Unlock()
	for _, r := range list {
		r.Stop()
	}
}

// Stop 停止单个录像。
func (r *Recorder) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.running {
		return
	}
	if r.cancel != nil {
		r.cancel()
	}
	r.running = false
}

// Status 录像状态。
type Status struct {
	Did     string `json:"did"`
	Name    string `json:"name"`
	Running bool   `json:"running"`
	Started string `json:"started,omitempty"`
	Dir     string `json:"dir"`
	Error   string `json:"error,omitempty"`
}

// List 列出所有录像状态。
func (m *Manager) List() []Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Status, 0, len(m.recorders))
	for _, r := range m.recorders {
		r.mu.Lock()
		st := Status{
			Did:     r.Did,
			Name:    r.Name,
			Running: r.running,
			Dir:     filepath.Join(r.cfg.Dir, sanitize(r.Name)),
		}
		if r.running {
			st.Started = r.started.Format(time.RFC3339)
		}
		if r.lastErr != nil {
			st.Error = r.lastErr.Error()
		}
		r.mu.Unlock()
		out = append(out, st)
	}
	return out
}

// ---- 文件管理 ----

// File 录像文件。
type File struct {
	Path    string `json:"path"`
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	ModTime string `json:"modTime"`
}

// ListFiles 列出某设备的录像文件。
func (m *Manager) ListFiles(name string, limit int) ([]File, error) {
	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()

	root := filepath.Join(cfg.Dir, sanitize(name))
	var files []File

	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if filepath.Ext(p) != ".mkv" {
			return nil
		}
		files = append(files, File{
			Path:    p,
			Name:    info.Name(),
			Size:    info.Size(),
			ModTime: info.ModTime().Format(time.RFC3339),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}

	// 按时间倒序
	sort.Slice(files, func(i, j int) bool {
		return files[i].ModTime > files[j].ModTime
	})
	if limit > 0 && len(files) > limit {
		files = files[:limit]
	}
	return files, nil
}

// Cleanup 清理过期录像。
func (m *Manager) Cleanup() (int, int64, error) {
	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()

	if cfg.KeepDays <= 0 {
		return 0, 0, nil
	}
	deadline := time.Now().AddDate(0, 0, -cfg.KeepDays)

	var n int
	var freed int64
	err := filepath.Walk(cfg.Dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if info.ModTime().Before(deadline) {
			if e := os.Remove(p); e == nil {
				n++
				freed += info.Size()
			}
		}
		return nil
	})
	return n, freed, err
}

// DiskUsage 磁盘使用情况。
type DiskUsage struct {
	TotalGB  float64 `json:"totalGB"`
	UsedGB   float64 `json:"usedGB"`
	FreeGB   float64 `json:"freeGB"`
	UsedPct  float64 `json:"usedPercent"`
	Path     string  `json:"path"`
	Err      string  `json:"err,omitempty"`
}

// DiskUsageOf 查询目录所在磁盘的使用情况。
func DiskUsageOf(dir string) DiskUsage {
	du := DiskUsage{Path: dir}
	_ = os.MkdirAll(dir, 0o755)

	var st syscallStatfs
	if err := statfs(dir, &st); err != nil {
		du.Err = err.Error()
		return du
	}

	bs := float64(st.Bsize)
	du.TotalGB = float64(st.Blocks) * bs / (1 << 30)
	du.FreeGB = float64(st.Bavail) * bs / (1 << 30)
	du.UsedGB = du.TotalGB - float64(st.Bfree)*bs/(1 << 30)
	if du.TotalGB > 0 {
		du.UsedPct = du.UsedGB / du.TotalGB * 100
	}
	return du
}

// DirUsage 统计目录占用（递归）。
func DirUsage(dir string) (int64, int, error) {
	var total int64
	var count int
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		total += info.Size()
		count++
		return nil
	})
	return total, count, err
}

// ---- 工具 ----

// checkDisk 检查磁盘剩余空间。
func checkDisk(dir string, minFreeGB int) error {
	if minFreeGB <= 0 {
		return nil
	}
	// 确保目录存在
	_ = os.MkdirAll(dir, 0o755)

	var st syscallStatfs
	if err := statfs(dir, &st); err != nil {
		return nil // 拿不到就算了，不阻塞
	}
	freeGB := float64(st.Bavail) * float64(st.Bsize) / (1 << 30)
	if freeGB < float64(minFreeGB) {
		return fmt.Errorf("磁盘剩余 %.1fGB 低于阈值 %dGB，已停止录像", freeGB, minFreeGB)
	}
	return nil
}

// sanitize 把设备名转成安全目录名。
func sanitize(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', 0:
			out = append(out, '_')
		default:
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return "unknown"
	}
	return string(out)
}
