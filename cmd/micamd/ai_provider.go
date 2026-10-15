package main

// 外部视觉大模型 Provider（OpenAI 兼容接口）。
//
// ===== 实测得到的关键点（见 docs/DEEPSEEK-INTEGRATION.md）=====
//
// 1. **推理模型**：deepseek-v4.1-flash 会把思考放在 reasoning_content，
//    答案放在 content。max_tokens 给少了会出现 content=null（思考没结束就被截断）。
//    -> 默认 1500，并在 content 为空时回退读 reasoning_content。
//
// 2. **JSON 会被代码围栏包裹**：即使设了 response_format=json_object，
//    返回仍是 ```json {...} ```。 -> 解析前必须剥围栏。
//
// 3. 图片用 base64 data URL。1280px 约 590 token，建议 1024 以内。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// ---- 云端请求缓存命中计数（2026-10-13）----
//
// 为什么用包级原子计数而不是挂在 aiProviderClient 上：
// worker 每个 tick 都用 providerFromConfig(cfg) **新建**一个 client，
// 挂在实例上的计数会被立刻丢弃。缓存目录本身是全局的，计数也放全局。
//
// 用途：「缓存」这个成本开关需要**真实数据**说明它省了多少次云端调用，
// 而不是编一个「预计减少 X%」。命中数 == 被省掉的调用次数。
var gCacheHits int64

// cacheHitCount 当前进程内的缓存命中次数
func cacheHitCount() int64 { return atomic.LoadInt64(&gCacheHits) }

// ---- 统一的「AI 事件包」与「分析结果」----
//
// 换任何 Provider，输入输出契约都不变。

// aiEventPacket 送给模型的输入
type aiEventPacket struct {
	Device    string   `json:"device"`
	Time      string   `json:"time"`
	LocalCls  string   `json:"localCls"` // 本地 YOLO 的结论（car/person）
	LocalConf float64  `json:"localConf"`
	Desc      string   `json:"desc"`
	Images    [][]byte `json:"-"` // 关键帧（JPEG 字节）
}

// aiAnalysis 模型返回的结构化结果
type aiAnalysis struct {
	Summary    string   `json:"summary"`    // 一句话描述
	Title      string   `json:"title"`      // 短标题（包裹送达）
	Objects    []string `json:"objects"`    // 关键对象
	Actions    []string `json:"actions"`    // 行为序列
	KeyMoments []string `json:"keyMoments"` // 关键时刻
	Tags       []string `json:"tags"`       // 标签
	Importance float64  `json:"importance"` // 重要度 0~1
	Plate      string   `json:"plate"`      // 车牌（如识别到）
	Confidence float64  `json:"confidence"`

	// 记账
	PromptTokens     int     `json:"promptTokens"`
	CompletionTokens int     `json:"completionTokens"`
	CostCNY          float64 `json:"costCny"`
	Provider         string  `json:"provider"`
	Model            string  `json:"model"`
	Cached           bool    `json:"cached"`
	ElapsedMS        int     `json:"elapsedMs"`
	Raw              string  `json:"raw,omitempty"`
}

// aiProviderClient 一个 OpenAI 兼容的 Provider 连接
type aiProviderClient struct {
	baseURL   string
	apiKey    string
	model     string
	timeout   time.Duration
	maxTokens int
	maxConc   int
	cacheDir  string
}

// providerFromConfig 按当前配置构造客户端（未配置返回 nil）
func providerFromConfig(cfg aiConfig) *aiProviderClient {
	key := loadProviderKey()
	if strings.TrimSpace(key) == "" {
		return nil
	}
	url := strings.TrimRight(strings.TrimSpace(cfg.ProviderURL), "/")
	if url == "" {
		url = "https://api.deepseek.com/v1"
	}
	to := cfg.APITimeout
	if to <= 0 {
		to = 60
	}
	mt := cfg.MaxTokens
	if mt <= 0 {
		mt = 1500
	}
	mc := cfg.MaxConcurrent
	if mc <= 0 {
		mc = 2
	}
	cd := filepath.Join(dataDirPath, "ai-cache")
	_ = os.MkdirAll(cd, 0o755)

	model := cfg.ProviderModel
	if model == "" {
		model = "deepseek-v4.1-flash"
	}

	return &aiProviderClient{
		baseURL:   url,
		apiKey:    key,
		model:     model,
		timeout:   time.Duration(to) * time.Second,
		maxTokens: mt,
		maxConc:   mc,
		cacheDir:  cd,
	}
}

// ---- HTTP 结构 ----

type oaiReq struct {
	Model          string       `json:"model"`
	Messages       []oaiMessage `json:"messages"`
	MaxTokens      int          `json:"max_tokens,omitempty"`
	Temperature    float64      `json:"temperature,omitempty"`
	ResponseFormat *oaiRespFmt  `json:"response_format,omitempty"`
}

type oaiRespFmt struct {
	Type string `json:"type"`
}

type oaiMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type oaiContentPart struct {
	Type     string  `json:"type"`
	Text     string  `json:"text,omitempty"`
	ImageURL *oaiImg `json:"image_url,omitempty"`
}

type oaiImg struct {
	URL string `json:"url"`
}

type oaiResp struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// do 发一次请求
func (c *aiProviderClient) do(ctx context.Context, payload oaiReq) (*oaiResp, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := newRequest(ctx, c.baseURL+"/chat/completions", body, c.apiKey)
	if err != nil {
		return nil, err
	}
	httpc := httpClientFor(c.timeout)
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 260))
	}

	var out oaiResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("响应解析失败: %w", err)
	}
	if out.Error != nil && out.Error.Message != "" {
		return nil, fmt.Errorf("%s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("响应里没有 choices")
	}
	return &out, nil
}

// Test 测试连接（发一个极小的请求）
func (c *aiProviderClient) Test(ctx context.Context) (int, string, error) {
	t0 := time.Now()
	r, err := c.do(ctx, oaiReq{
		Model:       c.model,
		Messages:    []oaiMessage{{Role: "user", Content: "回复 ok"}},
		MaxTokens:   512,
		Temperature: 0,
	})
	if err != nil {
		return 0, "", err
	}
	ms := int(time.Since(t0).Milliseconds())
	txt := firstNonEmpty(r.Choices[0].Message.Content, r.Choices[0].Message.ReasoningContent)
	return ms, truncate(strings.TrimSpace(txt), 80), nil
}

// SystemPrompt 分析事件的系统提示
const aiSystemPrompt = `你是一个家庭监控事件分析助手。
你会收到摄像头的画面截图和本地检测结果。
请用简洁的中文描述"发生了什么"，并输出 JSON。

要求：
- summary: 一句话说清发生了什么（不超过 40 字）
- title: 4~8 字的短标题，如"包裹送达""人员停留""车辆进出"
- objects: 画面里的关键对象（如 人员/包裹/纸箱/汽车/宠物）
- actions: 按时间顺序的行为（如 走到门口/放下包裹/离开）
- keyMoments: 关键时间点描述（2~4 条）
- tags: 检索标签（如 快递/夜间/陌生车辆）
- importance: 重要程度 0~1（陌生人/夜间/停留久 = 高）
- plate: 如果能看清车牌就填车牌号，否则留空
- confidence: 你对结果的把握 0~1

只输出 JSON，不要任何解释。`

// Analyze 用视觉模型分析一个事件
func (c *aiProviderClient) Analyze(ctx context.Context, p *aiEventPacket) (*aiAnalysis, error) {
	// 缓存键：图片内容 + 模型 + prompt 版本
	if c.cacheDir != "" {
		key := c.cacheKey(p)
		if hit := c.cacheGet(key); hit != nil {
			hit.Cached = true
			return hit, nil
		}
	}

	// 组装用户消息
	var parts []oaiContentPart
	var sb strings.Builder
	sb.WriteString("本地检测结果：\n")
	if p.LocalCls != "" {
		fmt.Fprintf(&sb, "- 本地模型识别为：%s（置信度 %.0f%%）\n", p.LocalCls, p.LocalConf*100)
	}
	if p.Desc != "" {
		fmt.Fprintf(&sb, "- 本地描述：%s\n", p.Desc)
	}
	if p.Device != "" {
		fmt.Fprintf(&sb, "- 摄像头：%s\n", p.Device)
	}
	if p.Time != "" {
		fmt.Fprintf(&sb, "- 时间：%s\n", p.Time)
	}
	sb.WriteString("\n请分析下面这些画面截图。")
	parts = append(parts, oaiContentPart{Type: "text", Text: sb.String()})

	for _, img := range p.Images {
		parts = append(parts, oaiContentPart{
			Type:     "image_url",
			ImageURL: &oaiImg{URL: "data:image/jpeg;base64," + b64(img)},
		})
	}

	t0 := time.Now()
	r, err := c.do(ctx, oaiReq{
		Model: c.model,
		Messages: []oaiMessage{
			{Role: "system", Content: aiSystemPrompt},
			{Role: "user", Content: parts},
		},
		MaxTokens:      c.maxTokens,
		ResponseFormat: &oaiRespFmt{Type: "json_object"},
	})
	if err != nil {
		return nil, err
	}
	el := int(time.Since(t0).Milliseconds())

	msg := r.Choices[0].Message
	txt := msg.Content
	if strings.TrimSpace(txt) == "" {
		// 推理模型：content 空时回退到 reasoning_content 末尾
		txt = lastLines(msg.ReasoningContent, 8)
	}

	out := &aiAnalysis{
		Provider:         "cloud",
		Model:            r.Model,
		PromptTokens:     r.Usage.PromptTokens,
		CompletionTokens: r.Usage.CompletionTokens,
		ElapsedMS:        el,
		Raw:              truncate(txt, 400),
	}
	out.CostCNY = estimateCostCNY(r.Usage.PromptTokens, r.Usage.CompletionTokens)

	// 剥掉代码围栏后解析 JSON
	var parsed struct {
		Summary    string   `json:"summary"`
		Title      string   `json:"title"`
		Objects    []string `json:"objects"`
		Actions    []string `json:"actions"`
		KeyMoments []string `json:"keyMoments"`
		Tags       []string `json:"tags"`
		Importance float64  `json:"importance"`
		Plate      string   `json:"plate"`
		Confidence float64  `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(stripFence(txt)), &parsed); err != nil {
		// 解析失败就把原文当 summary（至少不丢信息）
		out.Summary = truncate(strings.TrimSpace(txt), 200)
		if out.Summary == "" {
			out.Summary = "（模型没有返回可解析内容）"
		}
		return out, nil
	}
	out.Summary = strings.TrimSpace(parsed.Summary)
	out.Title = strings.TrimSpace(parsed.Title)
	out.Objects = parsed.Objects
	out.Actions = parsed.Actions
	out.KeyMoments = parsed.KeyMoments
	out.Tags = parsed.Tags
	out.Importance = parsed.Importance
	out.Plate = strings.TrimSpace(parsed.Plate)
	out.Confidence = parsed.Confidence

	if c.cacheDir != "" {
		c.cachePut(c.cacheKey(p), out)
	}
	return out, nil
}

// ---- 工具 ----

// stripFence 剥掉 ```json ... ``` 代码围栏
func stripFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		// 也可能前后有说明文字，取第一个 { 到最后一个 }
		if i, j := strings.Index(s, "{"), strings.LastIndex(s, "}"); i >= 0 && j > i {
			return s[i : j+1]
		}
		return s
	}
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimPrefix(s, "json")
	s = strings.TrimPrefix(s, "JSON")
	if i := strings.LastIndex(s, "```"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// cacheKey 用图片内容 + 模型 + prompt 版本算缓存键
func (c *aiProviderClient) cacheKey(p *aiEventPacket) string {
	h := sha256.New()
	h.Write([]byte("v1|" + c.model + "|" + aiSystemPrompt))
	for _, img := range p.Images {
		h.Write(img)
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

func (c *aiProviderClient) cachePath(key string) string {
	return filepath.Join(c.cacheDir, key+".json")
}

func (c *aiProviderClient) cacheGet(key string) *aiAnalysis {
	if !cacheEnabled() {
		return nil
	}
	raw, err := os.ReadFile(c.cachePath(key))
	if err != nil {
		return nil
	}
	var a aiAnalysis
	if json.Unmarshal(raw, &a) != nil {
		return nil
	}
	// 命中 = 省掉一次云端调用（供「缓存」开关展示真实效果）
	atomic.AddInt64(&gCacheHits, 1)
	return &a
}

func (c *aiProviderClient) cachePut(key string, a *aiAnalysis) {
	if !cacheEnabled() {
		return
	}
	raw, _ := json.Marshal(a)
	_ = os.WriteFile(c.cachePath(key), raw, 0o644)
}

// 价格（元 / 1M token）。默认按 DeepSeek Flash 官方价折人民币。
var (
	priceInCNY  = 1.10 // $0.15 约 ¥1.10
	priceOutCNY = 4.35 // $0.60 约 ¥4.35
)

func estimateCostCNY(promptTok, compTok int) float64 {
	return (float64(promptTok)*priceInCNY + float64(compTok)*priceOutCNY) / 1e6
}

var _ = bytes.NewReader
