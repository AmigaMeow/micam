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

// isLocalBaseURL 检查是否为局域网或本地私有大模型地址（如 Ollama / vLLM）
func isLocalBaseURL(url string) bool {
	u := strings.ToLower(strings.TrimSpace(url))
	return strings.Contains(u, "localhost") || strings.Contains(u, "127.0.0.1") ||
		strings.Contains(u, "192.168.") || strings.Contains(u, "10.") ||
		strings.Contains(u, "172.16.") || strings.Contains(u, "172.17.") ||
		strings.Contains(u, "172.18.") || strings.Contains(u, "172.19.") ||
		strings.Contains(u, "172.2") || strings.Contains(u, "172.3") ||
		strings.Contains(u, ":11434") || strings.Contains(u, ":8000") ||
		strings.Contains(u, ":5000") || strings.Contains(u, ":8080")
}

// normalizeProviderURL 规范化 Provider API 地址，自动纠正局域网 Ollama 缺失 /v1 的情况
func normalizeProviderURL(url string) string {
	u := strings.TrimRight(strings.TrimSpace(url), "/")
	if u == "" {
		return "https://api.deepseek.com/v1"
	}
	if isLocalBaseURL(u) && !strings.HasSuffix(u, "/v1") {
		u = u + "/v1"
	}
	return u
}

// providerFromConfig 按当前配置构造客户端（未配置返回 nil）
func providerFromConfig(cfg aiConfig) *aiProviderClient {
	url := normalizeProviderURL(cfg.ProviderURL)
	key := loadProviderKey()
	if strings.TrimSpace(key) == "" {
		if isLocalBaseURL(url) {
			key = "local-ollama-key"
		} else {
			return nil
		}
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
请严格以画面中清晰可见的事实为准，用简洁客观的中文描述"发生了什么"，并输出 JSON。

【反幻觉与环境噪点识别准则】
1. 夜间环境噪点与误报：若画面为夜间黑白夜视，画面变化主要为飞虫掠过镜头、蜘蛛丝反光、微弱雨丝或车灯反光，且无人员、动物或车辆移动，请直接将 title 定为"环境噪点"或"飞虫掠过"，summary 说明"夜间飞虫或光线微弱变化，现场无异常"，importance 评为 0.05 以下，并在 tags 中加入"误报"、"飞虫"或"光影"。
2. 常驻背景分离：注意区分画面中长期静止停放的车辆、雨棚、椅子等常驻背景物体与真正发生移动的主体。如果车辆始终静止，不要将事件描述为车辆进出或车辆异动。
3. 物种谨慎识别：如果动物趴卧、距离远或夜视模糊无法看清面部与尾巴特征，请谨慎标记为"宠物活动"或"疑似家犬"，严禁在特征不明时武断强行定为"猫咪"。
4. 严禁任何 Emoji 表情。

字段要求：
- summary: 一句话说清发生了什么（客观真实，不超过 40 字）
- title: 4~8 字短标题，如"包裹送达""人员走动""环境噪点""飞虫掠过"
- objects: 画面里真实可见的关键对象
- actions: 画面中实际发生的行为（静止则写静止，严禁脑补连续剧情）
- keyMoments: 关键画面描述（2~3 条）
- tags: 检索标签（如 快递/夜间/飞虫/误报）
- importance: 重要程度 0~1（飞虫/微弱光影 < 0.05，正常静止 < 0.1，人员/夜间活动 = 高）
- plate: 仅在能看清车牌时填写，否则留空
- confidence: 你对画面判断的把握 0~1

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

// AnswerVisualQuery 用外部视觉大模型回答关于监控画面的自然语言提问（多模态视觉问答）。
func (c *aiProviderClient) AnswerVisualQuery(ctx context.Context, question string, images [][]byte, contextInfo string) (string, tokenUsage, error) {
	var u tokenUsage
	if len(images) == 0 {
		return "", u, fmt.Errorf("无可用画面可供多模态分析")
	}

	var parts []oaiContentPart
	promptText := "用户查询问题：" + question
	if contextInfo != "" {
		promptText += "\n\n【监控系统初筛候选记录（仅供参考线索，可能含误报或噪点）】\n" + contextInfo
	}
	promptText += "\n\n请严格以所附真实监控抓拍截图为最高依据，实事求是、客观严谨地回答用户问题。"
	promptText += "\n核心要求："
	promptText += "\n1. 开头直接给出客观明确的结论（发现或未发现，绝不迎合猜测）；"
	promptText += "\n2. 严格以肉眼可见的画面为准，若初筛文字记录与画面不符（如把狗记成猫、把飞虫记成移动），必须明确纠正；"
	promptText += "\n3. 若画面目标受黑白夜视、距离远、遮挡影响导致特征模糊，必须如实说明看不清或无法确认为某物种，严禁脑补连续动作过程；"
	promptText += "\n4. 严禁任何 Emoji 表情，语言精炼专业。"
	parts = append(parts, oaiContentPart{Type: "text", Text: promptText})

	for _, img := range images {
		parts = append(parts, oaiContentPart{
			Type:     "image_url",
			ImageURL: &oaiImg{URL: "data:image/jpeg;base64," + b64(img)},
		})
	}

	const visualQASystemPrompt = `你是一个严谨客观的家庭监控智能管家。
你正在协助用户查证监控画面事实。系统会提供事件候选检索记录以及关键画面截图。

【反幻觉核心准则与判决铁律】
1. 视觉画面是唯一的绝对事实依据（Visual Grounding）：
   - 系统提供的检索记录仅为端侧像素变化或初步标签线索，可能受飞虫、光影变化或初筛分类误差影响，绝非绝对事实。
   - 若检索记录文字提及某目标（如“猫”、“人员”），但在所附真实画面中并未看到对应实体或特征完全不符，必须以画面为准，严禁顺着文字迎合或默认其存在！必须明确指出画面实况。
2. 诚实披露不确定性与画面受限（Honest Uncertainty）：
   - 当画面处于夜视黑白红外、距离过远、反光严重、或动物/人物处于蜷缩、遮挡状态时，严禁武断断定物种或身份（如不可将趴卧的狗武断定为猫，不可将树影/飞虫断定为人员）。
   - 必须实事求是说明“画面中仅见模糊趴卧轮廓，无法确认具体物种”或“实为夜间飞虫反光/环境噪点”。
3. 严禁臆测连贯动作与虚构过程（No Fabricated Dynamics）：
   - 仅凭单帧或离散截图，严禁脑补“走到门口趴下”、“沿墙边走动”、“站立张望”等连续动态小说式叙事；只能客观陈述画面中截取到的静态形态与位置。
4. 正面直接结论与不见即答无（Negative Grounding）：
   - 回答开头必须给出直接清晰的结论。如果画面中并未发现用户所问目标（如用户问“有没有猫”，而画面中只有狗或无人），直接明确回答“未发现猫的活动，画面中记录到的主要是...”，坚决杜绝“今天确实有猫来过”等无中生有的虚假肯定。
5. 排版规范：
   - 语言精练克制、客观求真（严禁任何 Emoji 表情），以 Markdown 格式清晰陈述。`

	r, err := c.do(ctx, oaiReq{
		Model: c.model,
		Messages: []oaiMessage{
			{Role: "system", Content: visualQASystemPrompt},
			{Role: "user", Content: parts},
		},
		MaxTokens: func() int { if c.maxTokens < 4096 { return 4096 }; return c.maxTokens }(),
	})
	if err != nil {
		return "", u, err
	}

	u.PromptTokens = r.Usage.PromptTokens
	u.CompletionTokens = r.Usage.CompletionTokens

	choice := r.Choices[0]
	txt := strings.TrimSpace(choice.Message.Content)
	if txt == "" {
		return "", u, fmt.Errorf("云端模型未在时限内生成有效答复 (finish_reason=%s)", choice.FinishReason)
	}
	return txt, u, nil
}

var _ = bytes.NewReader
