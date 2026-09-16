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
	}
}

// aiConfigFromReq 把请求转成配置
func aiConfigFromReq(req map[string]any, cur aiConfig) aiConfig {
	out := cur
	if v, ok := req["enabled"].(bool); ok {
		out.Enabled = v
	}
	if v, ok := req["model"].(string); ok && v != "" {
		out.Model = v
	}
	if v, ok := req["confThreshold"].(float64); ok && v > 0 && v <= 1 {
		out.ConfThreshold = v
	}
	if v, ok := req["maxPerMinute"].(float64); ok && v >= 0 {
		out.MaxPerMinute = int(v)
	}
	if v, ok := req["threads"].(float64); ok && v > 0 {
		out.Threads = int(v)
	}
	if v, ok := req["pythonPath"].(string); ok {
		out.PythonPath = v
	}
	if v, ok := req["classes"].([]any); ok {
		var cs []string
		for _, x := range v {
			if s, ok := x.(string); ok && s != "" {
				cs = append(cs, s)
			}
		}
		out.Classes = cs
	}
	return out
}

// handleAIConfig 读写 AI 配置。
//
//	GET  /api/ai/config
//	POST /api/ai/config  {enabled, model, confThreshold, classes, maxPerMinute, ...}
func (s *Server) handleAIConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]any{
			"config": s.cfg.AI,
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
