package main

// 查找任务的 HTTP 接口。
//
//	POST /api/ai/find/estimate   {question, maxFrames?}
//	     -> 本地规划 + 成本预估（不花钱）
//
//	POST /api/ai/find/run        {question, maxFrames?, confirmed:true}
//	     -> 创建并执行任务
//
//	GET  /api/ai/find/task?id=   -> 进度与结果
//	GET  /api/ai/find/tasks      -> 任务列表

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// handleFindEstimate 本地规划 + 成本预估。
//
// **这一步完全不调用云端**，所以永远不花钱。
func (s *Server) handleFindEstimate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Question  string `json:"question"`
		MaxFrames int    `json:"maxFrames"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Question == "" {
		writeErr(w, 400, "缺少 question")
		return
	}

	plan := s.planFind(req.Question, req.MaxFrames)
	cov := s.buildCoverage(plan)

	// 云端可用性
	cfg := aiConfigFromUser(s.cfg.AI)
	ready := providerFromConfig(cfg) != nil

	writeJSON(w, 200, map[string]any{
		"ok":       true,
		"plan":     plan,
		"coverage": cov,
		"ready":    ready,
		"limits": map[string]any{
			"hardMaxFrames":  findHardMaxFrames,
			"hardMaxCostCny": findHardMaxCostCNY,
			"defaultFrames":  findDefaultMaxFrames,
			"pricePerFrame":  estCostPerFrameCNY,
		},
		"notice": findEstimateNotice(plan, cov, ready),
	})
}

// findEstimateNotice 给用户看的一句提示
func findEstimateNotice(plan findPlan, cov findCoverage, ready bool) string {
	if !ready {
		return "还没有配置云端 Provider，无法执行查找（可以先用本地搜索）"
	}
	if plan.Cands == 0 {
		return "该时间段没有候选事件，不需要调用云端（不产生费用）"
	}
	msg := ""
	if plan.Cands > plan.EstFrame {
		msg = "候选较多，将只检查前 " + strconv.Itoa(plan.EstFrame) + " 个重要事件"
	} else {
		msg = "将检查 " + strconv.Itoa(plan.Cands) + " 个候选事件"
	}
	if !cov.Complete {
		msg += "；" + cov.Note
	}
	return msg
}

// handleFindRun 创建并执行查找任务。
func (s *Server) handleFindRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Question  string `json:"question"`
		MaxFrames int    `json:"maxFrames"`
		Confirmed bool   `json:"confirmed"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Question == "" {
		writeErr(w, 400, "缺少 question")
		return
	}
	// 必须显式确认（成本预估后用户点了「继续」）
	if !req.Confirmed {
		writeErr(w, 400, "需要先确认成本预估（confirmed: true）")
		return
	}

	cfg := aiConfigFromUser(s.cfg.AI)
	c := providerFromConfig(cfg)
	if c == nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "还没有配置云端 Provider"})
		return
	}

	// 预算闸
	if cfg.DailyBudgetCNY > 0 && s.ai.costToday() >= cfg.DailyBudgetCNY {
		writeJSON(w, 200, map[string]any{
			"ok":    false,
			"error": "今日云端预算已用完，请明天再试或上调预算",
		})
		return
	}

	plan := s.planFind(req.Question, req.MaxFrames)
	t := &findTask{
		ID:        s.finds.nextID(),
		Question:  req.Question,
		Plan:      plan,
		Status:    "running",
		Coverage:  s.buildCoverage(plan),
		CreatedAt: time.Now().Format("2006-01-02 15:04:05"),
	}
	s.finds.put(t)

	// 后台跑，前端轮询进度
	go func() {
		defer func() { _ = recover() }()
		s.runFindTask(t, c, req.MaxFrames)
	}()

	writeJSON(w, 200, map[string]any{"ok": true, "taskId": t.ID, "task": t})
}

// handleFindTask 查询任务进度
func (s *Server) handleFindTask(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeErr(w, 400, "缺少 id")
		return
	}
	t := s.finds.get(id)
	if t == nil {
		writeErr(w, 404, "任务不存在")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "task": t})
}

// handleFindTasks 任务列表
func (s *Server) handleFindTasks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"ok": true, "tasks": s.finds.list()})
}
