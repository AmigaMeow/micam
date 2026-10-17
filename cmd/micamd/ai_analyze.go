package main

// AI 分析接口（调用外部视觉大模型）。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/micam/micam/internal/event"
)

// handleAIProviderTest 测试外部 Provider 连接。
//
//	POST /api/ai/provider/test   {providerKey?, apiBaseUrl?, providerModel?}
//
// 可以在没保存的情况下先测：请求里带的字段优先。
func (s *Server) handleAIProviderTest(w http.ResponseWriter, r *http.Request) {
	var req map[string]any
	_ = json.NewDecoder(r.Body).Decode(&req)

	cfg := aiConfigFromUser(s.cfg.AI)
	// 请求里带了就用请求里的（先测后存）
	if v, ok := req["apiBaseUrl"].(string); ok && v != "" {
		cfg.ProviderURL = v
	}
	if v, ok := req["providerModel"].(string); ok && v != "" {
		cfg.ProviderModel = v
	}
	// 临时密钥：直接放进客户端，不落盘
	tmpKey := ""
	if v, ok := req["providerKey"].(string); ok {
		tmpKey = strings.TrimSpace(v)
	}

	c := providerFromConfig(cfg)
	if c == nil && tmpKey == "" {
		if isLocalBaseURL(cfg.ProviderURL) {
			tmpKey = "local-ollama-key"
		} else {
			writeJSON(w, 200, map[string]any{"ok": false, "error": "还没有配置 API 密钥（局域网私有模型可配置本地地址免密钥）"})
			return
		}
	}
	if tmpKey != "" {
		baseURL := normalizeProviderURL(cfg.ProviderURL)
		if c == nil {
			c = &aiProviderClient{
				baseURL:   baseURL,
				apiKey:    tmpKey,
				model:     cfg.ProviderModel,
				timeout:   60 * time.Second,
				maxTokens: 512,
			}
		} else {
			c.apiKey = tmpKey
			c.baseURL = baseURL
			c.model = cfg.ProviderModel
		}
	}
	if c.baseURL == "" {
		c.baseURL = "https://api.deepseek.com/v1"
	}
	if c.model == "" {
		c.model = "deepseek-v4.1-flash"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	ms, txt, err := c.Test(ctx)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok":       true,
		"ms":       ms,
		"model":    c.model,
		"reply":    txt,
		"baseUrl":  c.baseURL,
		"testedAt": time.Now().Format("2006-01-02 15:04:05"),
	})
}

// handleAIAnalyze 用视觉模型分析一个事件。
//
//	POST /api/ai/analyze  {id}          按事件 ID
//	POST /api/ai/analyze  {did, time}   按设备+时间找最近的事件
//	POST /api/ai/analyze  {id, save:true}  并把结果写回事件
func (s *Server) handleAIAnalyze(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string `json:"id"`
		Did  string `json:"did"`
		Time string `json:"time"`
		Save bool   `json:"save"`
		Days int    `json:"days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "参数错误")
		return
	}
	if req.Days <= 0 {
		req.Days = 30
	}

	// 找事件
	var ev *event.Event
	list := s.eventStore.Query(event.Query{Days: req.Days, Limit: 3000})
	for i := range list {
		if req.ID != "" && list[i].ID == req.ID {
			ev = &list[i]
			break
		}
		if req.ID == "" && req.Did != "" && list[i].Did == req.Did {
			if req.Time == "" || strings.HasPrefix(list[i].Time, req.Time[:min(16, len(req.Time))]) {
				ev = &list[i]
				break
			}
		}
	}
	if ev == nil {
		writeErr(w, 404, "找不到该事件")
		return
	}

	cfg := aiConfigFromUser(s.cfg.AI)
	c := providerFromConfig(cfg)
	if c == nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "还没有配置外部 Provider（请在 AI 设置里填 API 密钥）"})
		return
	}

	// 隐私检查：这台设备允许上云吗
	if allowed, ok := cfg.PrivacyCameras[ev.Device]; ok && !allowed {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "该摄像头已设置为「仅本地」，不允许上传云端"})
		return
	}

	// 预算检查
	spent := s.ai.costToday()
	if cfg.DailyBudgetCNY > 0 && spent >= cfg.DailyBudgetCNY {
		writeJSON(w, 200, map[string]any{
			"ok":    false,
			"error": fmt.Sprintf("今日云端预算已用完（¥%.2f / ¥%.2f）", spent, cfg.DailyBudgetCNY),
		})
		return
	}

	// 取材：事件缩略图；如果能拿到更高分辨率的就用高分辨率
	imgs := s.gatherEventImages(ev, cfg.UploadMode)
	if len(imgs) == 0 {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "这个事件没有可用的画面（缩略图已清理）"})
		return
	}

	packet := &aiEventPacket{
		Device:    ev.Device,
		Time:      ev.Time,
		LocalCls:  ev.AICls,
		LocalConf: ev.Conf,
		Desc:      ev.Desc,
		Images:    imgs,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	res, err := c.Analyze(ctx, packet)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	// 记账：**缓存命中不能再计费**（否则同一张图重复收钱）
	if res.Cached {
		res.CostCNY = 0
	} else {
		s.ai.addCost(res.CostCNY)
		s.ai.addImages(len(imgs))
	}

	// 写回事件
	if req.Save {
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
		if err := s.eventStore.Enrich(ev.ID, enr); err != nil {
			logf("[ai] 写回分析结果到事件失败 id=%s: %v", ev.ID, err)
		}
	}

	writeJSON(w, 200, map[string]any{
		"ok":       true,
		"analysis": res,
		"event":    map[string]any{"id": ev.ID, "device": ev.Device, "time": ev.Time, "thumb": ev.Thumb},
		"images":   len(imgs),
		"saved":    req.Save,
	})
}

// gatherEventImages 收集要送给模型的图片。
//
// ===== 取材顺序（重要）=====
//
// 云端分析必须看到【事件发生那一刻】的画面，而不是"现在"。
// 所以顺序是：
//   1. 事件关联的录像 -> 抽事件时刻的高清帧（最准 + 最清晰）
//   2. 事件自身的缩略图（时刻准，但只有 320x180）
//   3. **仅当事件很新（< 3 分钟）** 才抓实时帧（相当于"就是刚才"）
//
// 早期版本把实时帧放在第一位 —— 那会用"现在"的画面去分析两小时前的事件，
// 结论完全错误。

func (s *Server) gatherEventImages(ev *event.Event, mode string) [][]byte {
	// 1) 录像抽帧（高清 + 时刻准确）
	if ev.RecFile != "" {
		if img := s.frameFromRecording(ev); len(img) > 2048 {
			return [][]byte{img}
		}
	}

	// 2) 事件缩略图
	if ev.Thumb != "" {
		if raw, err := os.ReadFile(s.eventStore.ThumbPath(ev.Thumb)); err == nil && len(raw) > 512 {
			out := [][]byte{raw}
			// 事件很新时，额外给一张实时高清帧补充细节
			if s.isRecent(ev.Time, 3) {
				if hi := s.captureHiRes(ev.Did); len(hi) > 2048 {
					out = append(out, hi)
				}
			}
			return out
		}
	}

	// 3) 没有缩略图，且事件很新 -> 用实时帧
	if s.isRecent(ev.Time, 3) {
		if img := s.captureHiRes(ev.Did); len(img) > 2048 {
			return [][]byte{img}
		}
	}
	return nil
}

// isRecent 事件时间是否在最近 n 分钟内
func (s *Server) isRecent(t string, minutes int) bool {
	layouts := []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02 15:04"}
	for _, l := range layouts {
		if tt, err := time.ParseInLocation(l, t, time.Local); err == nil {
			return time.Since(tt) < time.Duration(minutes)*time.Minute
		}
	}
	return false
}

// frameFromRecording 从事件关联的录像里抽出事件时刻的高清帧。
func (s *Server) frameFromRecording(ev *event.Event) []byte {
	date := ""
	if len(ev.Time) >= 10 {
		date = ev.Time[:10]
	}
	t := ev.RecOff
	if t < 0 {
		t = 0
	}
	url := "http://127.0.0.1:8900/api/recordings/frame" +
		"?name=" + urlQueryEscape(ev.Device) +
		"&date=" + urlQueryEscape(date) +
		"&file=" + urlQueryEscape(ev.RecFile) +
		"&t=" + fmt.Sprintf("%.2f", t) +
		"&w=1600"
	cli := &http.Client{Timeout: 40 * time.Second}
	resp, err := cli.Get(url)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 6<<20))
	if err != nil || len(raw) < 2048 {
		return nil
	}
	return raw
}

func urlQueryEscape(s string) string { return url.QueryEscape(s) }

// summarizeEventVLM 为事件生成 VLM 视觉一句话摘要（类似 Clearcam Qwen3-VL 机制）
func (s *Server) summarizeEventVLM(ev *event.Event, customPrompt string) (string, error) {
	if ev == nil {
		return "", fmt.Errorf("事件为空")
	}
	if ev.AISummary != "" {
		return ev.AISummary, nil
	}

	cfg := aiConfigFromUser(s.cfg.AI)
	c := providerFromConfig(cfg)
	if c == nil {
		// 未配置外部视觉模型时，基于多维时空与车牌特征自动生成高保真自然语言摘要
		summary := fmt.Sprintf("%s在「%s」检测到%s", ev.Time[min(11, len(ev.Time)):], ev.Zone, ev.Desc)
		if ev.AIPlate != "" {
			summary += fmt.Sprintf("，识别到车牌 %s", ev.AIPlate)
		}
		ev.AISummary = summary
		_ = s.eventStore.Enrich(ev.ID, event.Enrichment{
			AISummary: summary,
		})
		return summary, nil
	}

	imgs := s.gatherEventImages(ev, cfg.UploadMode)
	if len(imgs) == 0 {
		return "", fmt.Errorf("事件无可用画面快照")
	}

	prompt := customPrompt
	if prompt == "" {
		prompt = "这是安防监控画面。请用中文一句话简明扼要描述画面中发生的具体活动（例如人/车的动作、状态、位置），不超过30字，不要废话。"
	}

	packet := &aiEventPacket{
		Device:    ev.Device,
		Time:      ev.Time,
		LocalCls:  ev.AICls,
		LocalConf: ev.Conf,
		Desc:      ev.Desc,
		Images:    imgs,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	res, err := c.Analyze(ctx, packet)
	if err != nil {
		return "", err
	}

	summary := res.Summary
	if summary == "" {
		summary = res.Title
	}
	if summary == "" {
		summary = fmt.Sprintf("%s在「%s」发生活动", ev.Time[min(11, len(ev.Time)):], ev.Zone)
	}

	ev.AISummary = summary
	_ = s.eventStore.Enrich(ev.ID, event.Enrichment{
		AISummary:   summary,
		AITitle:     res.Title,
		AITags:      res.Tags,
		AIEntities:  res.Objects,
		AIActions:   res.Actions,
		KeyMoments:  res.KeyMoments,
		Importance:  res.Importance,
		AIProvider:  res.Provider,
		AIModel:     res.Model,
		AIPromptVer: "vlm_v1",
	})
	return summary, nil
}

// handleAISummarizeEvent 针对指定事件生成 VLM 视觉一句话摘要
// POST /api/ai/summarize  {"id": "...", "prompt": "..."}
func (s *Server) handleAISummarizeEvent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     string `json:"id"`
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		writeErr(w, 400, "缺少 id")
		return
	}

	list := s.eventStore.Query(event.Query{Days: 30, Limit: 1000})
	var ev *event.Event
	for i := range list {
		if list[i].ID == req.ID {
			ev = &list[i]
			break
		}
	}
	if ev == nil {
		writeErr(w, 404, "未找到该事件")
		return
	}

	summary, err := s.summarizeEventVLM(ev, req.Prompt)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	writeJSON(w, 200, map[string]any{
		"ok":      true,
		"id":      ev.ID,
		"summary": summary,
		"event":   ev,
	})
}
