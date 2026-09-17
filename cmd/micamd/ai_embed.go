package main

// AI 相似图搜索 + Provider 测试。
//
// 相似图搜索用飞牛自带的 img2vec（2.3GB，输出 1024 维向量）+ faiss。
// 注意：单张向量化约 3.7 秒，所以**只在用户主动搜索时跑**，
// 不做后台批量处理。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/micam/micam/internal/event"
)

// handleAIEmbed 上传一张图，在历史事件里找相似的。
func (s *Server) handleAIEmbed(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 POST")
		return
	}
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		writeErr(w, 400, "解析上传失败: "+err.Error())
		return
	}
	f, _, err := r.FormFile("image")
	if err != nil {
		writeErr(w, 400, "缺少 image 字段")
		return
	}
	defer f.Close()
	img, err := io.ReadAll(f)
	if err != nil {
		writeErr(w, 400, "读取图片失败")
		return
	}
	if len(img) < 128 {
		writeErr(w, 400, "图片太小")
		return
	}

	// 写临时文件（python 侧读文件比传 base64 简单）
	tmp, err := os.CreateTemp("", "micam-sim-*.jpg")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	tmpPath := tmp.Name()
	_, _ = tmp.Write(img)
	tmp.Close()
	defer os.Remove(tmpPath)

	// 收集候选事件缩略图
	type cand struct {
		File  string
		Event event.Event
	}
	var cands []cand
	for _, e := range s.eventStore.Query(event.Query{Days: 90, Limit: 500}) {
		if e.Thumb == "" {
			continue
		}
		p := s.eventStore.ThumbPath(e.Thumb)
		if _, err := os.Stat(p); err == nil {
			cands = append(cands, cand{p, e})
		}
	}
	if len(cands) == 0 {
		writeJSON(w, 200, map[string]any{"ok": true, "matches": []any{}, "note": "没有可比较的历史缩略图"})
		return
	}

	// 交给 python 批量算相似度
	paths := make([]string, 0, len(cands))
	for _, c := range cands {
		paths = append(paths, c.File)
	}
	res, err := s.embedCompare(tmpPath, paths)
	if err != nil {
		writeErr(w, 504, "相似度计算失败: "+err.Error())
		return
	}

	type match struct {
		Did    string  `json:"did"`
		Device string  `json:"device"`
		Time   string  `json:"time"`
		Thumb  string  `json:"thumb"`
		Score  float64 `json:"score"`
	}
	var out []match
	for i, sc := range res.Scores {
		if i >= len(cands) {
			break
		}
		if sc < 0.6 { // 相似度阈值
			continue
		}
		e := cands[i].Event
		out = append(out, match{e.Did, e.Device, e.Time, e.Thumb, sc})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > 30 {
		out = out[:30]
	}

	writeJSON(w, 200, map[string]any{
		"ok":      true,
		"matches": out,
		"ms":      res.MS,
		"total":   len(cands),
	})
}

type embedResult struct {
	Scores []float64
	MS     int
}

// embedCompare 调 python 算「查询图 vs 候选图」的相似度。
func (s *Server) embedCompare(query string, cands []string) (*embedResult, error) {
	script := filepath.Join(appDestDir, "ai", "embed.py")
	if _, err := os.Stat(script); err != nil {
		script = filepath.Join("platform", "fnos", "app", "ai", "embed.py")
	}
	pyExe := "/usr/bin/python3"
	pyPath := ""
	if _, p, err := findPythonPath(); err == nil {
		pyPath = p
	}

	args := append([]string{script, query}, cands...)
	cmd := exec.Command(pyExe, args...)
	cmd.Env = append(os.Environ(), "PYTHONPATH="+pyPath, "OMP_NUM_THREADS=4")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("%s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}

	var r struct {
		Scores []float64 `json:"scores"`
		MS     int       `json:"ms"`
		Error  string    `json:"error"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return nil, fmt.Errorf("解析 python 输出失败")
	}
	if r.Error != "" {
		return nil, fmt.Errorf("%s", r.Error)
	}
	return &embedResult{Scores: r.Scores, MS: r.MS}, nil
}
