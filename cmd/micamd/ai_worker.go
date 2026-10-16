package main

// 自动分析工作器。
//
// 每 30 秒扫描一次最近的事件，按策略挑出值得花钱的，
// 依次送云端分析，把结果写回事件。
//
// ===== 不重复花钱的四道闸 =====
//
//   1. 规则     不符合策略的直接跳过（省 ¥0）
//   2. 去重     同设备同类型 5 分钟内只分析第一条
//   3. 预算     今日花费超过上限就停
//   4. 重试上限 同一条事件最多试 2 次（失败不无限重试烧钱）
//
// 另外每条事件分析前都检查：是否已有结果（有就不重复分析）。

import (
	"context"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/micam/micam/internal/event"
)

// aiJob 一次分析任务
type aiJob struct {
	Seq       int         `json:"seq"`
	EventID   string      `json:"eventId"`
	Device    string      `json:"device"`
	Time      string      `json:"time"`
	Status    string      `json:"status"` // queued / running / done / failed / skipped
	Reason    string      `json:"reason"`
	Error     string      `json:"error,omitempty"`
	CreatedAt string      `json:"createdAt"`
	DoneAt    string      `json:"doneAt,omitempty"`
	CostCNY   float64     `json:"costCny,omitempty"`
	ElapsedMS int         `json:"elapsedMs,omitempty"`
	Result    *aiAnalysis `json:"result,omitempty"`
}

// aiWorker 后台自动分析
type aiWorker struct {
	srv     *Server
	mu      sync.Mutex
	jobs    []*aiJob
	seq     int
	stop    chan struct{}
	running bool

	// 统计
	statAnalyzed int
	statSkipped  int
	statFailed   int
	statSavedCNY float64

	// ⚠️ 跳过要**按原因分开计数**（2026-10-13）。
	//
	// 原来只有 statSkipped，被两处不同原因共用：
	//   ① 策略规则判定不值得上云  ② 去重命中
	// 于是界面只能显示一个笼统的「队列跳过」——
	// 用户在「去重」开关旁看不到它到底省了多少。
	// 拆开后该开关才有**真实数据**可展示（而不是编一个「预计减少 60%」）。
	statSkipRules  int
	statSkipDedupe int

	attempts map[string]int  // 事件 -> 尝试次数
	seen     map[string]bool // 去重键
	seenDay  string
}

func newAIWorker(s *Server) *aiWorker {
	return &aiWorker{
		srv:      s,
		stop:     make(chan struct{}),
		attempts: map[string]int{},
		seen:     map[string]bool{},
		seenDay:  time.Now().Format("2006-01-02"),
	}
}

// Start 启动后台循环
func (w *aiWorker) Start() {
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return
	}
	w.running = true
	w.mu.Unlock()

	go func() {
		// 启动后先等一会，避免和开机流程抢资源
		select {
		case <-time.After(45 * time.Second):
		case <-w.stop:
			return
		}
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-w.stop:
				return
			case <-tick.C:
				func() {
					defer func() {
						// 单次 panic 不能打死循环
						_ = recover()
					}()
					w.tick()
				}()
			}
		}
	}()
}

// Stop 停止
func (w *aiWorker) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.running {
		return
	}
	w.running = false
	close(w.stop)
	w.stop = make(chan struct{})
}

func (w *aiWorker) isRunning() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.running
}

// tick 一次扫描
func (w *aiWorker) tick() {
	// 云端 AI 分析的是已落库在 eventStore 中的本地事件，走的是公网模型 API，
	// 绝不受小米传统凭据限流影响。
	cfg := aiConfigFromUser(w.srv.cfg.AI)

	// 云端没开 / 模式是纯本地 -> 不动
	if !cfg.CloudEnabled || cfg.Mode == "local" {
		return
	}
	c := providerFromConfig(cfg)
	if c == nil {
		return
	}

	// 预算闸
	spent := w.srv.ai.costToday()
	if cfg.DailyBudgetCNY > 0 && spent >= cfg.DailyBudgetCNY {
		return
	}

	// 取最近 20 分钟的事件（够覆盖两次扫描间隔）
	list := w.srv.eventStore.Query(event.Query{Days: 1, Limit: 400})

	// 统计同设备同类型出现次数（给「重复出现的车辆」规则用）
	sameCount := map[string]int{}
	for i := range list {
		k := list[i].Did + "|" + list[i].Type
		sameCount[k]++
	}

	now := time.Now()
	type cand struct {
		e     *event.Event
		score float64
		why   string
	}
	var cands []cand

	for i := range list {
		e := &list[i]

		// 已经有结果 -> 跳过（不重复花钱）
		if e.AISummary != "" {
			continue
		}
		// 太老的不处理（避免开机后把历史全刷一遍）
		if !w.isRecent(e.Time, 20) {
			continue
		}
		// 隐私：这台设备禁止上云
		if allowed, ok := cfg.PrivacyCameras[e.Device]; ok && !allowed {
			continue
		}
		// 重试上限
		if w.attempts[e.ID] >= 2 {
			continue
		}

		// 规则
		res := evalRules(e, cfg, sameCount[e.Did+"|"+e.Type])
		if !res.Analyze && cfg.Mode != "full" {
			w.bumpSkip("rules")
			continue
		}
		why := res.Reason
		if cfg.Mode == "full" && !res.Analyze {
			why = "完整智能模式（全部事件）"
		}

		// 去重
		if cfg.Dedupe {
			key := dedupeKey(e, 5)
			if w.markSeen(key) {
				w.bumpSkip("dedupe")
				continue
			}
		}

		cands = append(cands, cand{e, res.Score, why})
	}

	// 重要的先分析
	sort.Slice(cands, func(i, j int) bool { return cands[i].score > cands[j].score })

	// 一次最多分析 3 条（避免长时间占用）
	max := 3
	if cfg.MaxConcurrent > 0 && cfg.MaxConcurrent < max {
		max = cfg.MaxConcurrent
	}
	for i, cd := range cands {
		if i >= max {
			break
		}
		// 每分析一条都再查一次预算
		if cfg.DailyBudgetCNY > 0 && w.srv.ai.costToday() >= cfg.DailyBudgetCNY {
			break
		}
		w.analyzeOne(c, cd.e, cd.why)
	}
	_ = now
}

// analyzeOne 分析一条事件并写回
func (w *aiWorker) analyzeOne(c *aiProviderClient, e *event.Event, why string) {
	job := w.newJob(e, why, "running")
	w.attempts[e.ID]++

	imgs := w.srv.gatherEventImages(e, "")
	if len(imgs) == 0 {
		job.Status = "skipped"
		job.Error = "没有可用的画面"
		w.finish(job)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	res, err := c.Analyze(ctx, &aiEventPacket{
		Device:    e.Device,
		Time:      e.Time,
		LocalCls:  e.AICls,
		LocalConf: e.Conf,
		Desc:      e.Desc,
		Images:    imgs,
	})
	if err != nil {
		job.Status = "failed"
		job.Error = err.Error()
		w.mu.Lock()
		w.statFailed++
		w.mu.Unlock()
		w.finish(job)
		return
	}

	if !res.Cached {
		w.srv.ai.addCost(res.CostCNY)
		w.srv.ai.addImages(len(imgs))
	}

	enr := event.Enrichment{
		AISummary:   res.Summary,
		AITitle:     res.Title,
		AITags:      res.Tags,
		AIEntities:  res.Objects,
		AIActions:   res.Actions,
		KeyMoments:  res.KeyMoments,
		Importance:  res.Importance,
		AIProvider:  res.Provider,
		AIModel:     res.Model,
		AIPromptVer: "v1",
	}
	if res.Plate != "" {
		enr.AIPlate = res.Plate
		enr.AIPlateConf = res.Confidence
		if enr.AIPlateConf <= 0 {
			enr.AIPlateConf = 0.95
		}
		enr.AIPlateSrc = "cloud"
	}
	if err := w.srv.eventStore.Enrich(e.ID, enr); err != nil {
		job.Status = "failed"
		job.Error = "写回失败: " + err.Error()
		w.finish(job)
		return
	}

	// 车辆事件顺带读车牌（本地 LPR，免费）
	if e.Type == "vehicle" && lprAvailable() {
		if pr, perr := w.srv.enrichEventPlate(e); perr == nil && pr != nil && pr.Plate != "" {
			// 车牌结果已写进事件，任务记录里通过 job.Result 展示
			_ = pr
		}
	}

	// 触发全渠道事件推送（若已订阅）
	w.srv.dispatchAINotify(e, res)

	// AI 确认事件触发录像
	for _, d := range w.srv.allCameras() {
		if d.Did == e.Did {
			w.srv.triggerMotionRecording(d, true)
			break
		}
	}

	job.Status = "done"
	job.CostCNY = res.CostCNY
	job.ElapsedMS = res.ElapsedMS
	job.Result = res
	w.mu.Lock()
	w.statAnalyzed++
	w.mu.Unlock()
	w.finish(job)
}

func (w *aiWorker) newJob(e *event.Event, why, status string) *aiJob {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seq++
	j := &aiJob{
		Seq:       w.seq,
		EventID:   e.ID,
		Device:    e.Device,
		Time:      e.Time,
		Status:    status,
		Reason:    why,
		CreatedAt: time.Now().Format("15:04:05"),
	}
	w.jobs = append(w.jobs, j)
	if len(w.jobs) > 200 {
		w.jobs = w.jobs[len(w.jobs)-200:]
	}
	return j
}

func (w *aiWorker) finish(j *aiJob) {
	w.mu.Lock()
	defer w.mu.Unlock()
	j.DoneAt = time.Now().Format("15:04:05")
}

// bumpSkip 记一次跳过。reason: "rules"（策略判定不值）| "dedupe"（去重命中）
// statSkipped 仍是两者之和，保持旧字段语义不变（向后兼容）。
func (w *aiWorker) bumpSkip(reason string) {
	w.mu.Lock()
	w.statSkipped++
	switch reason {
	case "rules":
		w.statSkipRules++
	case "dedupe":
		w.statSkipDedupe++
	}
	w.mu.Unlock()
}

// markSeen 返回 true 表示这条已经见过了（应该跳过）
func (w *aiWorker) markSeen(key string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	day := time.Now().Format("2006-01-02")
	if day != w.seenDay {
		w.seen = map[string]bool{}
		w.seenDay = day
	}
	if w.seen[key] {
		return true
	}
	w.seen[key] = true
	return false
}

// isRecent 事件时间是否在最近 n 分钟内
func (w *aiWorker) isRecent(t string, minutes int) bool {
	layouts := []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05"}
	for _, l := range layouts {
		if tt, err := time.ParseInLocation(l, t, time.Local); err == nil {
			return time.Since(tt) < time.Duration(minutes)*time.Minute
		}
	}
	return false
}

// Snapshot 给接口用的状态快照
type aiWorkerSnapshot struct {
	Running      bool     `json:"running"`
	Analyzed     int      `json:"analyzed"`
	Skipped      int      `json:"skipped"`
	Failed       int      `json:"failed"`
	CostCNY      float64  `json:"costCny"`
	RecentJobs   []*aiJob `json:"recentJobs"`
	QueueWaiting int      `json:"queueWaiting"`
	// 跳过按原因拆分（2026-10-13）：让「去重」这个成本开关有真实数据可展示
	SkippedRules  int `json:"skippedRules"`
	SkippedDedupe int `json:"skippedDedupe"`
	// 云端请求缓存命中次数（同类事件重复分析被省掉的调用）
	CacheHits int64 `json:"cacheHits"`
}

func (w *aiWorker) Snapshot() aiWorkerSnapshot {
	w.mu.Lock()
	defer w.mu.Unlock()

	// 最近 30 条，倒序
	n := len(w.jobs)
	start := 0
	if n > 30 {
		start = n - 30
	}
	out := make([]*aiJob, 0, n-start)
	for i := n - 1; i >= start; i-- {
		out = append(out, w.jobs[i])
	}

	return aiWorkerSnapshot{
		Running:      w.running,
		Analyzed:     w.statAnalyzed,
		Skipped:      w.statSkipped,
		Failed:       w.statFailed,
		CostCNY:      w.srv.ai.costToday(),
		RecentJobs:   out,
		QueueWaiting: w.pendingCount(),
		// 拆分后的跳过计数（两者之和 == Skipped）
		SkippedRules:  w.statSkipRules,
		SkippedDedupe: w.statSkipDedupe,
		CacheHits:     cacheHitCount(),
	}
}

// pendingCount 最近 20 分钟里还没分析、且符合策略的事件数
func (w *aiWorker) pendingCount() int {
	cfg := aiConfigFromUser(w.srv.cfg.AI)
	if !cfg.CloudEnabled || cfg.Mode == "local" {
		return 0
	}
	list := w.srv.eventStore.Query(event.Query{Days: 1, Limit: 400})
	sameCount := map[string]int{}
	for i := range list {
		sameCount[list[i].Did+"|"+list[i].Type]++
	}
	n := 0
	for i := range list {
		e := &list[i]
		if e.AISummary != "" || !w.isRecent(e.Time, 20) {
			continue
		}
		if w.attempts[e.ID] >= 2 {
			continue
		}
		if allowed, ok := cfg.PrivacyCameras[e.Device]; ok && !allowed {
			continue
		}
		if r := evalRules(e, cfg, sameCount[e.Did+"|"+e.Type]); r.Analyze {
			n++
		}
	}
	return n
}

var _ = strconv.Itoa
