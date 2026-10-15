package main

// API 域名切换接口（调查用）。
//
// 背景（2026-09-20）：抓包发现官方米家 App 用 api.mijia.tech，
// 我们用 api.io.mi.com。若前者对第三方限流更宽松，换过去即可解决。
//
//	GET  /api/cred/apibase              查当前域名 + 候选列表
//	POST /api/cred/apibase {"host":""}  切换（落盘，重启保持）

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"

	"github.com/micam/micam/internal/xiaomi"
)

// apiBaseFile 保存已选域名。
func apiBaseFile() string {
	return filepath.Join(dataDirPath, "api-base.json")
}

func loadAPIBase() {
	raw, err := os.ReadFile(apiBaseFile())
	if err != nil {
		return
	}
	var v struct {
		Host string `json:"host"`
		SID  string `json:"sid"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return
	}
	if v.Host != "" {
		xiaomi.SetAPIBase(v.Host)
		logf("[api] 载入 API 域名: %s", v.Host)
	}
	if v.SID != "" {
		xiaomi.SetSID(v.SID)
		logf("[api] 载入 sid: %s", v.SID)
	}
}

func saveAPIBase(host, sid string) error {
	m := map[string]string{}
	if host != "" {
		m["host"] = host
	}
	if sid != "" {
		m["sid"] = sid
	}
	raw, _ := json.MarshalIndent(m, "", "  ")
	return os.WriteFile(apiBaseFile(), raw, 0o600)
}

func (s *Server) handleAPIBase(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]any{
			"host":       xiaomi.APIBase(),
			"default":    xiaomi.DefaultBase,
			"candidates": xiaomi.KnownAPIHosts(),
		})
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 GET / POST")
		return
	}

	var req struct {
		Host string `json:"host"`
		SID  string `json:"sid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "参数错误")
		return
	}

	// 白名单校验：只允许候选域名（避免误配导致完全不可用）
	if req.Host != "" {
		ok := false
		for _, h := range xiaomi.KnownAPIHosts() {
			if h == req.Host {
				ok = true
				break
			}
		}
		if !ok {
			writeErr(w, 400, "不支持的域名（仅限候选列表）")
			return
		}
	}
	if req.SID != "" {
		ok := false
		for _, s := range xiaomi.KnownSIDs() {
			if s == req.SID {
				ok = true
				break
			}
		}
		if !ok {
			writeErr(w, 400, "不支持的 sid（仅限候选列表）")
			return
		}
	}

	if req.Host != "" {
		xiaomi.SetAPIBase(req.Host)
	}
	if req.SID != "" {
		xiaomi.SetSID(req.SID)
	}
	_ = saveAPIBase(xiaomi.APIBase(), xiaomi.SID())
	logf("[api] 已切换 host=%s sid=%s", xiaomi.APIBase(), xiaomi.SID())

	writeJSON(w, 200, map[string]any{
		"ok":   true,
		"host": xiaomi.APIBase(),
		"sid":  xiaomi.SID(),
		"note": "已生效；切换 sid 后必须重新登录（token 体系不同）",
	})
}
