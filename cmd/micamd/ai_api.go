package main

// AI 相关的 HTTP 接口。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/micam/micam/internal/config"
)

// aiConfigFromUser 把 config 包的类型转成主包类型
func aiConfigFromUser(c config.AIConfig) aiConfig {
	return aiConfig{
		Enabled:       c.Enabled,
		Model:         c.Model,
		ConfThreshold: c.ConfThreshold,
		Classes:       c.Classes,
		MaxPerMinute:  c.MaxPerMinute,
		PythonPath:    c.PythonPath,
		Threads:       c.Threads,

		Mode:           c.Mode,
		DetectPerson:   c.DetectPerson,
		DetectVehicle:  c.DetectVehicle,
		DetectPackage:  c.DetectPackage,
		DetectPet:      c.DetectPet,
		EmbedEnabled:   c.EmbedEnabled,
		FrameInterval:  c.FrameInterval,
		ResultKeepDays: c.ResultKeepDays,

		CloudEnabled:   c.CloudEnabled,
		CloudImportant: c.CloudImportant,
		DailyBudgetCNY: c.DailyBudgetCNY,
		BudgetAction:   c.BudgetAction,
		ProviderName:   c.ProviderName,
		ProviderModel:  c.ProviderModel,
		ProviderURL:    c.ProviderURL,
		APITimeout:     c.APITimeout,
		MaxConcurrent:  c.MaxConcurrent,
		MaxTokens:      c.MaxTokens,
		UploadMode:     c.UploadMode,

		Rules:        c.Rules,
		Dedupe:       c.Dedupe,
		CacheEnabled: c.CacheEnabled,

		PrivacyCameras: c.PrivacyCameras,
		ConfSlider:     c.ConfThreshold,
	}
}

// aiConfigToUser 反过来
func aiConfigToUser(a aiConfig) config.AIConfig {
	return config.AIConfig{
		Enabled:       a.Enabled,
		Model:         a.Model,
		ConfThreshold: a.ConfThreshold,
		Classes:       a.Classes,
		MaxPerMinute:  a.MaxPerMinute,
		PythonPath:    a.PythonPath,
		Threads:       a.Threads,

		Mode:           a.Mode,
		DetectPerson:   a.DetectPerson,
		DetectVehicle:  a.DetectVehicle,
		DetectPackage:  a.DetectPackage,
		DetectPet:      a.DetectPet,
		EmbedEnabled:   a.EmbedEnabled,
		FrameInterval:  a.FrameInterval,
		ResultKeepDays: a.ResultKeepDays,

		CloudEnabled:   a.CloudEnabled,
		CloudImportant: a.CloudImportant,
		DailyBudgetCNY: a.DailyBudgetCNY,
		BudgetAction:   a.BudgetAction,
		ProviderName:   a.ProviderName,
		ProviderModel:  a.ProviderModel,
		ProviderURL:    a.ProviderURL,
		APITimeout:     a.APITimeout,
		MaxConcurrent:  a.MaxConcurrent,
		MaxTokens:      a.MaxTokens,
		UploadMode:     a.UploadMode,

		Rules:        a.Rules,
		Dedupe:       a.Dedupe,
		CacheEnabled: a.CacheEnabled,

		PrivacyCameras: a.PrivacyCameras,
	}
}

// aiConfigFromReq 把请求转成配置（只覆盖请求里出现的字段）
func aiConfigFromReq(req map[string]any, cur aiConfig) aiConfig {
	out := cur

	str := func(k string, dst *string) {
		if v, ok := req[k].(string); ok {
			*dst = v
		}
	}
	bl := func(k string, dst *bool) {
		if v, ok := req[k].(bool); ok {
			*dst = v
		}
	}
	num := func(k string, dst *float64) {
		if v, ok := req[k].(float64); ok {
			*dst = v
		}
	}
	it := func(k string, dst *int) {
		if v, ok := req[k].(float64); ok {
			*dst = int(v)
		}
	}

	bl("enabled", &out.Enabled)
	str("model", &out.Model)
	num("confThreshold", &out.ConfThreshold)
	num("confSlider", &out.ConfSlider)
	it("maxPerMinute", &out.MaxPerMinute)
	it("threads", &out.Threads)
	str("pythonPath", &out.PythonPath)

	str("mode", &out.Mode)
	bl("detectPerson", &out.DetectPerson)
	bl("detectVehicle", &out.DetectVehicle)
	bl("detectPackage", &out.DetectPackage)
	bl("detectPet", &out.DetectPet)
	bl("embedEnabled", &out.EmbedEnabled)
	str("frameInterval", &out.FrameInterval)
	it("resultKeepDays", &out.ResultKeepDays)

	bl("cloudEnabled", &out.CloudEnabled)
	bl("cloudImportant", &out.CloudImportant)
	num("dailyBudgetCny", &out.DailyBudgetCNY)
	str("budgetAction", &out.BudgetAction)
	str("providerName", &out.ProviderName)
	str("providerModel", &out.ProviderModel)
	str("apiBaseUrl", &out.ProviderURL)
	str("providerUrl", &out.ProviderURL)
	it("apiTimeout", &out.APITimeout)
	it("maxConcurrent", &out.MaxConcurrent)
	it("maxTokens", &out.MaxTokens)
	str("uploadMode", &out.UploadMode)

	bl("dedupe", &out.Dedupe)
	bl("cacheEnabled", &out.CacheEnabled)

	if v, ok := req["classes"].([]any); ok {
		var cs []string
		for _, x := range v {
			if s2, ok := x.(string); ok && s2 != "" {
				cs = append(cs, s2)
			}
		}
		out.Classes = cs
	}
	if v, ok := req["rules"].(map[string]any); ok {
		m := map[string]bool{}
		for k, x := range v {
			if b, ok := x.(bool); ok {
				m[k] = b
			}
		}
		out.Rules = m
	}
	if v, ok := req["privacyCameras"].(map[string]any); ok {
		m := map[string]bool{}
		for k, x := range v {
			if b, ok := x.(bool); ok {
				m[k] = b
			}
		}
		out.PrivacyCameras = m
	}

	// ---- API Key 单独存（不进 config.json）----
	if v, ok := req["providerKey"].(string); ok && strings.TrimSpace(v) != "" {
		if err := saveProviderKey(strings.TrimSpace(v)); err == nil {
			out.ProviderReady = true
		}
	}
	if v, ok := req["clearProviderKey"].(bool); ok && v {
		_ = saveProviderKey("")
		out.ProviderReady = false
	}

	return out
}

// ---- API Key 安全存储 ----
//
// **故意不放在 config.json 里**：
// config.json 可能被导出/分享给他人排障，密钥混在里面会泄露。
// 单独一个 0600 的文件，只有 micam 用户能读。

func providerKeyPath() string {
	dir := dataDirPath
	if dir == "" {
		dir = "."
	}
	return filepath.Join(dir, "ai-key")
}

func saveProviderKey(key string) error {
	p := providerKeyPath()
	if key == "" {
		_ = os.Remove(p)
		return nil
	}
	// 0600：只有属主可读写
	return os.WriteFile(p, []byte(key), 0o600)
}

func loadProviderKey() string {
	raw, err := os.ReadFile(providerKeyPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// handleAIConfig 读写 AI 配置。
//
//	GET  /api/ai/config
//	POST /api/ai/config  {enabled, model, ..., providerKey}
func (s *Server) handleAIConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		cfg := s.cfg.AI
		writeJSON(w, 200, map[string]any{
			"config": cfg,
			// 前端只看「配没配」，密钥本身不下发
			"hasKey": loadProviderKey() != "",
			"status": s.ai.Status(),
		})
		return
	}

	var req map[string]any
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "参数错误")
		return
	}

	cur := aiConfigFromUser(s.cfg.AI)
	neu := aiConfigFromReq(req, cur)

	if err := s.cfg.SetAI(aiConfigToUser(neu)); err != nil {
		writeErr(w, 500, "保存配置失败: "+err.Error())
		return
	}

	// 同步到运行中的客户端
	s.ai.applyConfig(neu)

	// 重启推理进程以应用新配置
	if neu.Enabled {
		if err := s.ai.Start(neu); err != nil {
			writeJSON(w, 200, map[string]any{
				"ok":     true,
				"config": neu,
				"status": s.ai.Status(),
				"warn":   "配置已保存，但 AI 启动失败：" + err.Error(),
			})
			return
		}
	} else {
		s.ai.Stop()
	}

	writeJSON(w, 200, map[string]any{"ok": true, "config": neu, "status": s.ai.Status()})
}

// handleAIStatus AI 运行状态（设置页显示用）
func (s *Server) handleAIStatus(w http.ResponseWriter, r *http.Request) {
	st := s.ai.Status()

	// 环境探测（即使没开启也告诉用户环境是否具备）
	pyExe, pyPath, envErr := findPythonPath()
	env := map[string]any{
		"python":     pyExe,
		"pythonPath": pyPath,
		"available":  envErr == nil,
	}
	if envErr != nil {
		env["error"] = envErr.Error()
	}

	// 模型文件情况
	modelDir := filepath.Join(s.dataDir, "models")
	var models []map[string]any
	for _, name := range []string{"yolov8n", "yolov8s"} {
		p := filepath.Join(modelDir, name+".onnx")
		info, err := os.Stat(p)
		m := map[string]any{"name": name, "downloaded": err == nil}
		if err == nil {
			m["sizeMB"] = float64(info.Size()) / 1024 / 1024
		}
		models = append(models, m)
	}

	writeJSON(w, 200, map[string]any{
		"status": st,
		"env":    env,
		"models": models,
		"dir":    modelDir,
	})
}

// handleAIModel 下载/删除模型。
//
//	POST /api/ai/model  {"action":"download","name":"yolov8n"}
//	POST /api/ai/model  {"action":"delete","name":"yolov8n"}
func (s *Server) handleAIModel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"`
		Name   string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		writeErr(w, 400, "参数错误")
		return
	}
	// 只允许已知模型名，防路径穿越
	if strings.ContainsAny(req.Name, "/\\..") {
		writeErr(w, 400, "非法模型名")
		return
	}

	modelDir := filepath.Join(s.dataDir, "models")
	_ = os.MkdirAll(modelDir, 0o755)
	dst := filepath.Join(modelDir, req.Name+".onnx")

	switch req.Action {
	case "delete":
		if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
		return

	case "download":
		// 异步下载，前端轮询 status
		go func(name, dst string) {
			if err := downloadModel(name, dst); err != nil {
				logf("[ai] 模型下载失败 %s: %v", name, err)
				return
			}
			logf("[ai] 模型下载完成: %s", name)
		}(req.Name, dst)

		writeJSON(w, 200, map[string]any{"ok": true, "msg": "开始下载"})
		return
	}

	writeErr(w, 400, "未知 action")
}

// handleAITest 用一张真实截图测试推理。
func (s *Server) handleAITest(w http.ResponseWriter, r *http.Request) {
	did := r.URL.Query().Get("did")

	// 优先用事件缩略图（一定有画面）
	var jpeg []byte
	if b, _ := s.getThumbAt(did); len(b) > 0 {
		jpeg = b
	}
	if len(jpeg) == 0 {
		writeErr(w, 400, "没有可用的测试图（请先在设备页加载出画面）")
		return
	}

	res, err := s.ai.Detect(jpeg)
	if err != nil {
		writeErr(w, 504, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok":      true,
		"objects": res.Objects,
		"ms":      res.MS,
		"inferMs": res.InferMS,
		"size":    res.Size,
	})
}

// downloadModel 从镜像下载模型。
func downloadModel(name, dst string) error {
	// 已知模型的 URL（hf-mirror 优先，国内可用）
	sources := map[string][]string{
		"yolov8n": {
			"https://hf-mirror.com/kshitijjjjjjjjjjjjjjjj/yolov8n-coco-onnx/resolve/main/yolov8n.onnx",
		},
		"yolov8s": {
			"https://hf-mirror.com/orirdx/yolov8s-coco-onnx/resolve/main/coco_yolov8s.onnx",
		},
	}
	urls, ok := sources[name]
	if !ok {
		return fmt.Errorf("未知模型: %s", name)
	}
	return downloadFile(urls, dst)
}
