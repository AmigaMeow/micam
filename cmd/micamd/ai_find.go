package main

// 阶段 1a：自然语言「查找」任务（完整最小闭环）
//
//   提问 -> 本地筛候选 -> 成本预估 -> 用户确认 -> 云端判断
//        -> 证据列表（后端绑定）-> 定位回放
//
// ===== 两条铁律 =====
//
// 1. 【证据零编造】
//    模型**只输出** {match, note, confidence}。
//    时间、设备、缩略图路径、事件 ID **全部由后端从事件对象绑定**。
//    模型根本没有机会生成时间或位置 —— 从数据结构上杜绝编造。
//
// 2. 【不能把「没找到」说成「没发生」】
//    返回结果必须同时给出：
//      · 录像覆盖率（这段时间录像实际存在多少）
//      · 事件索引完整性（索引有无断档）
//    结论措辞只能是「候选中未发现」，不能是「没有发生」。
//
// ===== 成本控制 =====
//    · 预估基于本地候选数（不花钱）
//    · 用户确认后才执行
//    · 单任务帧数硬上限 + 金额硬上限
//    · 按供应商实际 usage 记账（失败也可能计费）

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/micam/micam/internal/event"
)

// ---- 常数（成本与上限）----

const (
	// 单帧平均成本（元）。实测单帧分析约 600 prompt + 150 completion token，
	// 含 JSON 与推理大约 ¥0.002~0.003。取保守值用于预估。
	estCostPerFrameCNY = 0.003

	// 硬上限：单任务最多检查多少帧（无论用户怎么设）
	findHardMaxFrames = 60

	// 默认帧数上限
	findDefaultMaxFrames = 20

	// 硬上限：单任务金额（元）
	findHardMaxCostCNY = 1.0
)

// findEvidence 一条证据
//
// **注意**：除了 Note / Match / Conf 三个字段，其余全部由后端绑定，
// 模型不参与生成。
type findEvidence struct {
	EventID string  `json:"eventId"`    // 后端绑定
	Time    string  `json:"time"`       // 后端绑定（来自事件）
	Device  string  `json:"device"`     // 后端绑定
	Did     string  `json:"did"`        // 用于跳转回放
	Thumb   string  `json:"thumb"`      // 后端绑定
	EventTy string  `json:"eventType"`  // 本地类型
	Match   bool    `json:"match"`      // 模型判断
	Note    string  `json:"note"`       // 模型说明（仅文字）
	Conf    float64 `json:"confidence"` // 模型自评
}

// findCoverage 覆盖率与索引完整性
//
// 用于避免把「没找到」误解为「没发生」。
type findCoverage struct {
	IndexFrom  string   `json:"indexFrom"`  // 事件索引最早时间
	IndexTo    string   `json:"indexTo"`    // 事件索引最晚时间
	IndexCount int      `json:"indexCount"` // 索引内事件总数
	RecFrom    string   `json:"recFrom"`    // 录像覆盖最早
	RecTo      string   `json:"recTo"`      // 录像覆盖最晚
	RecDays    int      `json:"recDays"`    // 有录像的天数
	Gaps       []string `json:"gaps"`       // 断档提示
	Complete   bool     `json:"complete"`   // 是否完整覆盖
	Note       string   `json:"note"`       // 给用户看的一句话
}

// findTask 一个查找任务
type findTask struct {
	ID        string         `json:"id"`
	Question  string         `json:"question"`
	Plan      findPlan       `json:"plan"`
	Status    string         `json:"status"` // estimating/running/done/failed/cancelled
	Checked   int            `json:"checked"`
	Total     int            `json:"total"`
	Evidence  []findEvidence `json:"evidence"`
	Answer    string         `json:"answer"`
	Error     string         `json:"error"`
	CostCNY   float64        `json:"costCny"`
	Coverage  findCoverage   `json:"coverage"`
	CreatedAt string         `json:"createdAt"`
	DoneAt    string         `json:"doneAt"`
	Log       []string       `json:"log"`
}

// findPlan 本地解析出的执行计划（免费）
type findPlan struct {
	Days     int      `json:"days"`
	Device   string   `json:"device"` // 设备名（空=全部）
	Did      string   `json:"did"`
	Target   string   `json:"target"`     // 要找的东西（人/车/包裹）
	TimeHint string   `json:"timeHint"`   // 时段描述
	Cands    int      `json:"candidates"` // 本地候选数（不花钱）
	EstCost  float64  `json:"estCostCny"`
	EstFrame int      `json:"estFrames"`
	Types    []string `json:"types"`
}

type findStore struct {
	mu    sync.Mutex
	tasks map[string]*findTask
	seq   int
}

func newFindStore() *findStore {
	return &findStore{tasks: map[string]*findTask{}}
}

func (s *findStore) put(t *findTask) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[t.ID] = t
}

func (s *findStore) get(id string) *findTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tasks[id]
}

func (s *findStore) list() []*findTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*findTask, 0, len(s.tasks))
	for _, t := range s.tasks {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	if len(out) > 30 {
		out = out[:30]
	}
	return out
}

func (s *findStore) nextID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	return fmt.Sprintf("find-%d-%d", time.Now().Unix(), s.seq)
}

// ---- 第一步：本地规划（不花钱）----

// planFind 用【本地规则】解析问题，并统计候选数。
//
// 复用已有的 parseNaturalQuery（自然语言搜索那套），所以完全免费。
func (s *Server) planFind(question string, maxFrames int) findPlan {
	var plan findPlan

	// 设备名列表（用于匹配）
	var devNames []string
	for _, d := range s.allCameras() {
		devNames = append(devNames, d.Name)
	}

	parsed := parseNaturalQuery(question, devNames, s.meta.GetByDevName)

	plan.Days = parsed.Days
	if plan.Days <= 0 {
		plan.Days = 1
	}
	plan.Types = parsed.Types
	plan.Device = parsed.Device
	plan.TimeHint = parsed.Parsed

	// 设备 -> did
	if plan.Device != "" {
		for _, d := range s.allCameras() {
			if d.Name == plan.Device {
				plan.Did = d.Did
				break
			}
		}
	}

	// 目标词（用于 prompt）
	target := "人员或车辆等活动"
	if len(parsed.Types) > 0 {
		var ts []string
		for _, t := range parsed.Types {
			switch t {
			case "person":
				ts = append(ts, "人")
			case "vehicle":
				ts = append(ts, "车")
			case "package":
				ts = append(ts, "包裹")
			case "motion":
				ts = append(ts, "移动")
			}
		}
		if len(ts) > 0 {
			target = strings.Join(ts, "、")
		}
	}
	plan.Target = target

	// 数候选（免费）
	cands := s.collectCandidates(plan)
	plan.Cands = len(cands)

	// 预估
	lim := maxFrames
	if lim <= 0 {
		lim = findDefaultMaxFrames
	}
	if lim > findHardMaxFrames {
		lim = findHardMaxFrames
	}
	frames := plan.Cands
	if frames > lim {
		frames = lim
	}
	plan.EstFrame = frames
	plan.EstCost = float64(frames) * estCostPerFrameCNY

	return plan
}

// collectCandidates 本地筛候选（不花钱）
func (s *Server) collectCandidates(plan findPlan) []event.Event {
	list := s.eventStore.Query(event.Query{Days: plan.Days, Limit: 3000})

	var out []event.Event
	for _, e := range list {
		// 设备过滤
		if plan.Did != "" && e.Did != plan.Did {
			continue
		}
		// 类型过滤
		if len(plan.Types) > 0 {
			hit := false
			for _, t := range plan.Types {
				if e.Type == t {
					hit = true
					break
				}
			}
			if !hit {
				continue
			}
		}
		// 没图的没法看
		if e.Thumb == "" {
			continue
		}
		out = append(out, e)
	}
	return out
}

// buildCoverage 统计覆盖情况（避免把「没找到」说成「没发生」）
func (s *Server) buildCoverage(plan findPlan) findCoverage {
	cov := findCoverage{}

	// 事件索引范围
	all := s.eventStore.Query(event.Query{Days: plan.Days, Limit: 5000})
	if len(all) > 0 {
		cov.IndexFrom = all[len(all)-1].Time
		cov.IndexTo = all[0].Time
		cov.IndexCount = len(all)
	}

	// 录像覆盖
	if days := s.listRecDateDirs(); len(days) > 0 {
		sort.Strings(days)
		cov.RecDays = len(days)
		cov.RecFrom = days[0]
		cov.RecTo = days[len(days)-1]
	}

	// 断档判断：事件索引覆盖的天数 vs 请求天数
	wantDays := plan.Days
	idxDays := map[string]bool{}
	for _, e := range all {
		if len(e.Time) >= 10 {
			idxDays[e.Time[:10]] = true
		}
	}
	if len(idxDays) < wantDays {
		cov.Gaps = append(cov.Gaps,
			fmt.Sprintf("请求的 %d 天里，事件索引只覆盖 %d 天", wantDays, len(idxDays)))
	}
	if cov.RecDays == 0 {
		cov.Gaps = append(cov.Gaps, "该时间段没有录像文件")
	}

	cov.Complete = len(cov.Gaps) == 0

	switch {
	case cov.IndexCount == 0:
		cov.Note = "该时间段事件索引为空，可能设备未运行或数据已清理"
	case cov.Complete:
		cov.Note = fmt.Sprintf("事件索引覆盖完整（%d 条事件，%d 天录像）", cov.IndexCount, cov.RecDays)
	default:
		cov.Note = strings.Join(cov.Gaps, "；") + "，结果可能不全"
	}
	return cov
}

// listRecDateDirs 列出录像里出现过的日期（YYYY-MM-DD）。
//
// 用于覆盖率统计：告诉用户「这段时间到底有没有录像」。
func (s *Server) listRecDateDirs() []string {
	root := s.recRoot()
	set := map[string]bool{}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	for _, dev := range entries {
		if !dev.IsDir() || strings.HasPrefix(dev.Name(), ".") {
			continue
		}
		sub, err := os.ReadDir(filepath.Join(root, dev.Name()))
		if err != nil {
			continue
		}
		for _, d := range sub {
			if d.IsDir() && len(d.Name()) == 10 && d.Name()[4] == '-' {
				set[d.Name()] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}

// ---- 第二步：执行（花钱，需用户确认）----

// runFindTask 执行一个查找任务。
func (s *Server) runFindTask(t *findTask, c *aiProviderClient, maxFrames int) {
	cfg := aiConfigFromUser(s.cfg.AI)

	cands := s.collectCandidates(t.Plan)
	lim := maxFrames
	if lim <= 0 {
		lim = findDefaultMaxFrames
	}
	if lim > findHardMaxFrames {
		lim = findHardMaxFrames
	}
	if len(cands) > lim {
		cands = cands[:lim]
	}
	t.Total = len(cands)
	t.Status = "running"

	costCap := findHardMaxCostCNY
	if cfg.DailyBudgetCNY > 0 && cfg.DailyBudgetCNY < costCap {
		costCap = cfg.DailyBudgetCNY
	}

	for i := range cands {
		e := &cands[i]
		t.Checked = i + 1

		if t.CostCNY >= costCap {
			t.Log = append(t.Log, fmt.Sprintf("已达单任务金额上限 ¥%.2f，提前结束", costCap))
			break
		}
		if cfg.DailyBudgetCNY > 0 && s.ai.costToday() >= cfg.DailyBudgetCNY {
			t.Log = append(t.Log, "已达每日预算上限，提前结束")
			break
		}

		imgs := s.gatherEventImages(e, "snapshot")
		if len(imgs) == 0 {
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		verdict, usage, err := c.Judge(ctx, t.Question, t.Plan.Target, imgs)
		cancel()

		// 记账：按供应商实际返回的 usage（失败也可能已计费）
		if usage.PromptTokens > 0 || usage.CompletionTokens > 0 {
			fee := estimateCostCNY(usage.PromptTokens, usage.CompletionTokens)
			s.ai.addCost(fee)
			s.ai.addImages(len(imgs))
			t.CostCNY += fee
		}

		if err != nil {
			t.Log = append(t.Log, fmt.Sprintf("%s 判定失败: %s", e.Time, truncate(err.Error(), 60)))
			continue
		}

		// 证据由【后端绑定】—— 模型只提供 Match / Note / Conf
		if verdict.Match {
			t.Evidence = append(t.Evidence, findEvidence{
				EventID: e.ID,
				Time:    e.Time,
				Device:  e.Device,
				Did:     e.Did,
				Thumb:   e.Thumb,
				EventTy: e.Type,
				Match:   verdict.Match,
				Note:    verdict.Note,
				Conf:    verdict.Confidence,
			})
			t.Log = append(t.Log, fmt.Sprintf("%s 命中: %s", e.Time, truncate(verdict.Note, 50)))
		} else {
			t.Log = append(t.Log, fmt.Sprintf("%s 未命中", e.Time))
		}
	}

	t.Status = "done"
	t.DoneAt = time.Now().Format("15:04:05")
	t.Answer = buildFindAnswer(t)
}

// buildFindAnswer 生成结论文案。
//
// 措辞纪律：
//
//	有命中   -> 说明找到什么
//	无命中   -> 只能说「候选中未发现」，并附覆盖率
//	           **绝不能说「没有发生」**
func buildFindAnswer(t *findTask) string {
	n := len(t.Evidence)
	if t.Total == 0 {
		base := "该时间段的事件索引里没有符合条件的候选，未发起云端分析。"
		if !t.Coverage.Complete {
			base += "（" + t.Coverage.Note + "）"
		}
		return base
	}
	if n > 0 {
		s := fmt.Sprintf("在 %d 个候选中找到 %d 处符合描述的记录。", t.Total, n)
		if !t.Coverage.Complete {
			s += "注意：" + t.Coverage.Note + "，结果可能不全。"
		}
		return s
	}
	s := fmt.Sprintf("已检查 %d 个候选，均未发现符合描述的内容。", t.Total)
	if !t.Coverage.Complete {
		s += "需要说明：" + t.Coverage.Note + "，因此只能说明「候选中未发现」，不能确定该时段没有发生。"
	} else {
		s += "该时段事件索引覆盖完整，但这仍只代表「本地事件库筛出的候选中未发现」，不等于绝对没有发生。"
	}
	return s
}
