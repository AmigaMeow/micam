package main

// 自动分析任务状态接口。

import "net/http"

// handleAIJobs 返回自动分析的状态与最近任务。
//
//	GET /api/ai/jobs
func (s *Server) handleAIJobs(w http.ResponseWriter, r *http.Request) {
	if s.aiw == nil {
		writeJSON(w, 200, map[string]any{"running": false, "recentJobs": []any{}})
		return
	}
	cfg := s.cfg.AI
	snap := s.aiw.Snapshot()

	mode := cfg.Mode
	if mode == "" {
		mode = "local"
	}
	writeJSON(w, 200, map[string]any{
		"running":       snap.Running,
		"analyzed":      snap.Analyzed,
		"skipped":       snap.Skipped,
		"failed":        snap.Failed,
		"costCny":       snap.CostCNY,
		"queueWaiting":  snap.QueueWaiting,
		"recentJobs":    snap.RecentJobs,
		"cloudEnabled":  cfg.CloudEnabled,
		"mode":          mode,
		"providerReady": loadProviderKey() != "",
		"rules":         cfg.Rules,
		"budget":        cfg.DailyBudgetCNY,
	})
}
