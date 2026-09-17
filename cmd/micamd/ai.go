package main

// AI 推理客户端：管理 Python 子进程 + JSON 行通信。
//
// ===== 为什么常驻子进程 =====
//
//   每次调用 python3: 解释器 300ms + 加载模型 140ms = 440ms（还没算推理）
//   常驻子进程:       启动一次，之后只付推理时间（~270ms）
//
// ===== 为什么用 stdin/stdout 而不是 HTTP =====
//
//   · 本地进程通信，不需要端口（不占端口、不会冲突）
//   · 不需要鉴权
//   · 简单：一行一个 JSON
//
// ===== 设计要点 =====
//
//   · **默认关闭**：不启用时完全不启动进程、不加载模型
//   · 环境不可用时优雅降级：把原因记下来，继续用帧差
//   · 进程崩溃自动重启（最多 3 次），连续失败就禁用
//   · 所有调用串行（单进程），避免 CPU 抢占

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// aiConfig AI 配置（持久化到 config.json）
type aiConfig struct {
	Enabled       bool     `json:"enabled"`       // 总开关（默认 false）
	Model         string   `json:"model"`         // yolov8n / yolov8s / 自定义
	ConfThreshold float64  `json:"confThreshold"` // 置信度阈值
	Classes       []string `json:"classes"`       // 关心的类（空=全部已映射的）
	MaxPerMinute  int      `json:"maxPerMinute"`  // 限流
	PythonPath    string   `json:"pythonPath"`    // "" = 自动探测
	Threads       int      `json:"threads"`       // onnxruntime 线程数

	// ---- AI 模式 ----
	//   local    只用本地（默认，不花钱）
	//   balanced 均衡：重要事件送云端
	//   full     完整智能：所有事件都送云端
	Mode string `json:"mode"`

	// ---- 云端分析（第二阶段）----
	CloudEnabled   bool    `json:"cloudEnabled"`
	CloudImportant bool    `json:"cloudImportant"`
	DailyBudgetCNY float64 `json:"dailyBudgetCny"`
	ProviderName   string  `json:"providerName"`
	ProviderModel  string  `json:"providerModel"`
	ProviderURL    string  `json:"providerUrl"`
	ProviderKey    string  `json:"-"` // **绝不下发前端**
	ProviderReady  bool    `json:"providerReady"`

	// ---- 本地检测项 ----
	DetectPerson  bool `json:"detectPerson"`
	DetectVehicle bool `json:"detectVehicle"`
	DetectPackage bool `json:"detectPackage"`
	EmbedEnabled  bool `json:"embedEnabled"`

	// ---- 隐私 ----
	PrivacyCameras map[string]bool `json:"privacyCameras"`
	UploadMode     string          `json:"uploadMode"`

	// ---- 精致版新增（对齐效果图 P0-2）----
	APIBaseURL     string  `json:"apiBaseUrl"`     // API 地址
	APITimeout     int     `json:"apiTimeout"`     // 请求超时（秒）
	MaxConcurrent  int     `json:"maxConcurrent"`  // 最大并发
	MaxTokens      int     `json:"maxTokens"`      // 单次分析最大 token（推理模型要给足）
	FrameInterval  string  `json:"frameInterval"`  // 帧采样：realtime / 1fps / 5fps / 10fps
	ResultKeepDays int     `json:"resultKeepDays"` // 结果保留天数
	DetectPet      bool    `json:"detectPet"`      // 宠物检测
	LocalModelName string  `json:"localModelName"` // 模型显示名
	ConfSlider     float64 `json:"confSlider"`     // 阈值滑块值

	// ---- 分析策略（省钱关键）----
	Rules        map[string]bool `json:"rules"`        // 规则开关
	Dedupe       bool            `json:"dedupe"`       // 去重
	CacheEnabled bool            `json:"cacheEnabled"` // 缓存
	BudgetAction string          `json:"budgetAction"` // stop / alert
}

func defaultaiConfig() aiConfig {
	return aiConfig{
		Enabled:        false,
		Mode:           "local",
		CloudEnabled:   false,
		CloudImportant: true,
		DailyBudgetCNY: 5,
		DetectPerson:   true,
		DetectVehicle:  true,
		DetectPackage:  true,
		EmbedEnabled:   false,
		UploadMode:     "snapshot",
		PrivacyCameras: map[string]bool{},
		APIBaseURL:     "https://api.deepseek.com",
		APITimeout:     30,
		MaxConcurrent:  3,
		FrameInterval:  "realtime",
		ResultKeepDays: 30,
		DetectPet:      false,
		LocalModelName: "yolov8n",
		ConfSlider:     0.5,
		Rules: map[string]bool{
			"door_person":  true,  // 有人出现在门口
			"night_person": true,  // 夜间任何人员活动
			"linger":       true,  // 可疑停留超过 30 秒
			"repeat_car":   true,  // 重复出现未知车辆
			"day_car":      false, // 白天车辆经过（默认禁用）
		},
		Dedupe:        true,
		CacheEnabled:  true,
		BudgetAction:  "alert",
		Model:         "yolov8n",
		ConfThreshold: 0.45,
		Classes:       []string{"person", "vehicle"},
		MaxPerMinute:  12,
		Threads:       4,
	}
}

// aiObject 一个检测结果
type aiObject struct {
	Cls  string  `json:"cls"`
	Type string  `json:"type"` // person / vehicle / animal / package
	Conf float64 `json:"conf"`
	Box  []int   `json:"box"`
}

// aiResult 一次推理的结果
type aiResult struct {
	Objects []aiObject
	MS      int
	InferMS int
	Size    []int
}

// aiClient 管理推理子进程
type aiClient struct {
	mu       sync.Mutex
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	reader   *bufio.Reader
	seq      int
	cfg      aiConfig
	ready    bool
	lastErr  string
	status   string // "off" / "ready" / "unavailable" / "error"
	restarts int
	calls    []time.Time // 限流用

	// 外部 Provider（第二阶段）
	prov       *aiProvider
	costCNY    float64
	costDay    string
	cloudCount int
	imgCount   int
	modelDir   string
	scriptPath string
}

// ---- 外部 Provider（第二阶段接入）----
//
// 现在只保存配置和记账，实际调用等拿到 API Key 后实现。

// aiProvider 一个外部 AI 服务配置
type aiProvider struct {
	Name    string `json:"name"` // deepseek / openai / gemini / qwen / ollama
	BaseURL string `json:"baseUrl"`
	APIKey  string `json:"-""`     // **绝不序列化给前端**
	HasKey  bool   `json:"hasKey"` // 前端只看「有没有配」
	Model   string `json:"model"`
	Vision  bool   `json:"vision"`
	Enabled bool   `json:"enabled"`
}

// applyConfig 把新配置同步进客户端（不重启进程时用）
func (a *aiClient) applyConfig(c aiConfig) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg = c
}

// getConfig 取当前配置副本（锁保护）
func (a *aiClient) getConfig() aiConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

// hasProvider 是否配了可用的外部 Provider
func (a *aiClient) hasProvider() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.CloudEnabled && a.providerReady()
}

func (a *aiClient) providerReady() bool {
	return a.prov != nil && a.prov.HasKey && a.prov.Enabled
}

// costToday 今日累计费用（元）
func (a *aiClient) costToday() float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	day := time.Now().Format("2006-01-02")
	if a.costDay != day {
		return 0
	}
	return a.costCNY
}

// imgToday 今日上传的图片数
func (a *aiClient) imgToday() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.costDay != time.Now().Format("2006-01-02") {
		return 0
	}
	return a.imgCount
}

// addImages 记图片数。
//
// 锁内只更新内存并返回快照，写盘在锁外做（saveCostState）。
// 原实现是 Lock -> 手动 Unlock -> 写盘 -> 再 Lock，与 defer Unlock 混用：
// panic 路径下锁状态不一致，且后来者极易改错。
func (a *aiClient) addImages(n int) {
	day, cny, cloud, img := a.addImagesLocked(n)
	saveCostState(day, cny, cloud, img)
}

// addImagesLocked 锁内更新图片计数，返回供落盘用的快照。
func (a *aiClient) addImagesLocked(n int) (string, float64, int, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	day := time.Now().Format("2006-01-02")
	if a.costDay != day {
		a.costDay = day
		a.costCNY = 0
		a.cloudCount = 0
		a.imgCount = 0
	}
	a.imgCount += n
	return a.costDay, a.costCNY, a.cloudCount, a.imgCount
}

// ---- 费用记账持久化 ----
//
// 记账只在内存里的话，重启就清零 —— 那样「每日预算」形同虚设
// （重启一下就能绕过上限）。所以落盘。

type costState struct {
	Day     string  `json:"day"`
	CostCNY float64 `json:"costCny"`
	Cloud   int     `json:"cloud"`
	Images  int     `json:"images"`
}

func costStatePath() string {
	dir := dataDirPath
	if dir == "" {
		dir = "."
	}
	return filepath.Join(dir, "ai-cost.json")
}

func loadCostState(a *aiClient) {
	if a == nil {
		return
	}
	raw, err := os.ReadFile(costStatePath())
	if err != nil {
		return
	}
	var c costState
	if json.Unmarshal(raw, &c) != nil {
		return
	}
	a.costDay = c.Day
	a.costCNY = c.CostCNY
	a.cloudCount = c.Cloud
	a.imgCount = c.Images
}

func saveCostState(day string, cny float64, cloud, images int) {
	raw, err := json.Marshal(costState{Day: day, CostCNY: cny, Cloud: cloud, Images: images})
	if err != nil {
		logf("[ai] 序列化费用状态失败: %v", err)
		return
	}
	if err := os.WriteFile(costStatePath(), raw, 0o644); err != nil {
		logf("[ai] 写入费用文件 %s 失败: %v", costStatePath(), err)
	}
}

// addCost 记一笔费用。锁内算好快照，写盘在锁外。
func (a *aiClient) addCost(cny float64) {
	day, total, cloud, img := a.addCostLocked(cny)
	saveCostState(day, total, cloud, img)
}

// addCostLocked 锁内累加费用，返回供落盘用的快照。
func (a *aiClient) addCostLocked(cny float64) (string, float64, int, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	day := time.Now().Format("2006-01-02")
	if a.costDay != day {
		a.costDay = day
		a.costCNY = 0
		a.cloudCount = 0
		a.imgCount = 0
	}
	a.costCNY += cny
	a.cloudCount++
	return a.costDay, a.costCNY, a.cloudCount, a.imgCount
}

var ai *aiClient

// newAIClient 创建客户端（不启动进程）
func newAIClient(dataDir, scriptPath string) *aiClient {
	return &aiClient{
		status:     "off",
		cfg:        defaultaiConfig(),
		modelDir:   filepath.Join(dataDir, "models"),
		scriptPath: scriptPath,
	}
}

// findPythonPath 自动探测飞牛 AI 运行时。
//
// 飞牛自带 AI 应用会装 onnxruntime 到 @sysappmeta，
// 但那个 python3 指向系统解释器，所以要靠 PYTHONPATH 指到 site-packages。
func findPythonPath() (pythonExe string, pythonPath string, err error) {
	// 候选 site-packages（按版本号从新到旧）
	candidates := []string{
		"/vol1/@sysappmeta/ai-manager/python/3_11_x86/lib/python3.11/site-packages",
		"/vol1/@sysappmeta/ai-manager/python/3_12_x86/lib/python3.12/site-packages",
		"/vol1/@sysappmeta/ai-manager/python/3_10_x86/lib/python3.10/site-packages",
	}
	for _, sp := range candidates {
		if _, e := os.Stat(filepath.Join(sp, "onnxruntime", "__init__.py")); e == nil {
			pythonPath = sp
			break
		}
	}
	if pythonPath == "" {
		// 兜底：扫一遍 ai-manager/python/*
		base := "/vol1/@sysappmeta/ai-manager/python"
		if ents, e := os.ReadDir(base); e == nil {
			for _, ent := range ents {
				p := filepath.Join(base, ent.Name(), "lib")
				if subs, e2 := os.ReadDir(p); e2 == nil {
					for _, sub := range subs {
						sp := filepath.Join(p, sub.Name(), "site-packages")
						if _, e3 := os.Stat(filepath.Join(sp, "onnxruntime", "__init__.py")); e3 == nil {
							pythonPath = sp
							break
						}
					}
				}
				if pythonPath != "" {
					break
				}
			}
		}
	}
	if pythonPath == "" {
		return "", "", fmt.Errorf("未找到 onnxruntime（需要飞牛「AI」应用）")
	}

	pythonExe = "/usr/bin/python3"
	if _, e := os.Stat(pythonExe); e != nil {
		if p, e2 := exec.LookPath("python3"); e2 == nil {
			pythonExe = p
		} else {
			return "", "", fmt.Errorf("未找到 python3")
		}
	}
	return pythonExe, pythonPath, nil
}

// Start 启动推理子进程
func (a *aiClient) Start(cfg aiConfig) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.startLocked(cfg)
}

func (a *aiClient) startLocked(cfg aiConfig) error {
	a.cfg = cfg

	if !cfg.Enabled {
		a.stopLocked()
		a.status = "off"
		return nil
	}
	a.cfg.Enabled = true

	model := filepath.Join(a.modelDir, cfg.Model+".onnx")
	if _, err := os.Stat(model); err != nil {
		a.status = "unavailable"
		a.lastErr = fmt.Sprintf("模型文件不存在：%s（请在设置里下载）", model)
		return fmt.Errorf("%s", a.lastErr)
	}

	pythonExe := "/usr/bin/python3"
	pythonPath := cfg.PythonPath
	if pythonPath == "" {
		var err error
		pythonExe, pythonPath, err = findPythonPath()
		if err != nil {
			a.status = "unavailable"
			a.lastErr = err.Error()
			return err
		}
	}

	threads := cfg.Threads
	if threads <= 0 {
		threads = 4
	}

	cmd := exec.Command(pythonExe, a.scriptPath, cfg.Model, a.modelDir)
	cmd.Env = append(os.Environ(),
		"PYTHONPATH="+pythonPath,
		fmt.Sprintf("MICAM_MODEL_DIR=%s", a.modelDir),
		fmt.Sprintf("OMP_NUM_THREADS=%d", threads),
	)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = os.Stderr // 推理日志直接进主日志

	if err := cmd.Start(); err != nil {
		a.status = "error"
		a.lastErr = "启动推理进程失败: " + err.Error()
		return err
	}

	a.cmd = cmd
	a.stdin = stdin
	a.reader = bufio.NewReaderSize(stdout, 1<<20)

	// 等 ready
	type readyMsg struct {
		Ready bool   `json:"ready"`
		Model string `json:"model"`
		Error string `json:"error"`
	}
	done := make(chan struct{})
	var rmsg readyMsg
	var rerr error
	go func() {
		line, e := a.reader.ReadString('\n')
		if e != nil {
			rerr = e
		} else {
			_ = json.Unmarshal([]byte(strings.TrimSpace(line)), &rmsg)
		}
		close(done)
	}()

	select {
	case <-done:
		if rerr != nil {
			a.status = "error"
			a.lastErr = "推理进程无响应: " + rerr.Error()
			a.stopLocked()
			return rerr
		}
	case <-time.After(30 * time.Second):
		a.status = "error"
		a.lastErr = "推理进程启动超时（30 秒）"
		a.stopLocked()
		return fmt.Errorf("%s", a.lastErr)
	}

	a.ready = true
	a.status = "ready"
	a.lastErr = ""
	logf("[ai] 推理服务已启动: %s", cfg.Model)
	go a.watch()
	return nil
}

// watch 子进程退出监控
func (a *aiClient) watch() {
	a.mu.Lock()
	cmd := a.cmd
	a.mu.Unlock()
	if cmd == nil {
		return
	}
	_ = cmd.Wait()

	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.ready {
		return // 已经被主动停了
	}
	a.ready = false
	a.restarts++
	if a.restarts > 3 {
		a.status = "error"
		a.lastErr = "推理进程反复退出，已停用 AI"
		logf("[ai] 重启超过 3 次，停用")
		return
	}
	logf("[ai] 推理进程退出，第 %d 次重启", a.restarts)
	go func(c aiConfig) {
		time.Sleep(2 * time.Second)
		a.mu.Lock()
		defer a.mu.Unlock()
		if err := a.startLocked(c); err != nil {
			logf("[ai] 自动重启 AI 服务失败: %v", err)
		}
	}(a.cfg)
}

func (a *aiClient) stopLocked() {
	a.ready = false
	if a.stdin != nil {
		_, _ = a.stdin.Write([]byte("{\"id\":0,\"cmd\":\"exit\"}\n"))
		_ = a.stdin.Close()
		a.stdin = nil
	}
	if a.cmd != nil && a.cmd.Process != nil {
		_ = a.cmd.Process.Kill()
		a.cmd = nil
	}
}

// Stop 停止
func (a *aiClient) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopLocked()
	a.status = "off"
	a.cfg.Enabled = false // 状态要和配置一致，否则前端显示「已启用但已停止」
}

// IsReady 推理服务是否可用（无锁快速判断）
func (a *aiClient) IsReady() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ready && a.status == "ready"
}

// Status 当前状态（给设置页显示）
func (a *aiClient) Status() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return map[string]any{
		"status":   a.status,
		"model":    a.cfg.Model,
		"ready":    a.ready,
		"error":    a.lastErr,
		"restarts": a.restarts,
		"enabled":  a.cfg.Enabled,
	}
}

// Detect 推理一张图（串行，带限流）
func (a *aiClient) Detect(jpeg []byte) (*aiResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.ready || a.stdin == nil {
		return nil, fmt.Errorf("AI 未就绪（%s）", a.status)
	}

	// 限流
	now := time.Now()
	cut := now.Add(-time.Minute)
	var keep []time.Time
	for _, t := range a.calls {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	a.calls = keep
	if a.cfg.MaxPerMinute > 0 && len(a.calls) >= a.cfg.MaxPerMinute {
		return nil, fmt.Errorf("触发限流（每分钟最多 %d 次）", a.cfg.MaxPerMinute)
	}
	a.calls = append(a.calls, now)

	// 发请求
	a.seq++
	id := a.seq
	req := map[string]any{
		"id":    id,
		"cmd":   "detect",
		"image": base64.StdEncoding.EncodeToString(jpeg),
		"conf":  a.cfg.ConfThreshold,
	}
	if len(a.cfg.Classes) > 0 {
		req["classes"] = a.cfg.Classes
	}
	raw, _ := json.Marshal(req)
	if _, err := a.stdin.Write(append(raw, '\n')); err != nil {
		return nil, fmt.Errorf("发送失败: %w", err)
	}

	// 读响应（带超时）
	type respMsg struct {
		ID      int        `json:"id"`
		OK      bool       `json:"ok"`
		Error   string     `json:"error"`
		Objects []aiObject `json:"objects"`
		MS      int        `json:"ms"`
		InferMS int        `json:"inferMs"`
		Size    []int      `json:"size"`
	}
	ch := make(chan respMsg, 1)
	ech := make(chan error, 1)
	go func() {
		line, err := a.reader.ReadString('\n')
		if err != nil {
			ech <- err
			return
		}
		var m respMsg
		if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &m); err != nil {
			ech <- err
			return
		}
		ch <- m
	}()

	select {
	case m := <-ch:
		if !m.OK {
			return nil, fmt.Errorf("推理失败: %s", m.Error)
		}
		return &aiResult{Objects: m.Objects, MS: m.MS, InferMS: m.InferMS, Size: m.Size}, nil
	case err := <-ech:
		return nil, fmt.Errorf("读取响应失败: %w", err)
	case <-time.After(60 * time.Second):
		return nil, fmt.Errorf("推理超时（60 秒）")
	}
}
