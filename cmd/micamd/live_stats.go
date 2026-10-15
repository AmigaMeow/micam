package main

// 直播流统计（状态条显示码率用）。
//
// 原理：HLS 分片是固定时长的（默认 1 秒），所以
//       最近几个分片的平均字节数 ≈ 码率。
//       这样不用额外采集，读文件大小即可。

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// handleLiveStats 返回当前设备直播流的估算码率。
func (s *Server) handleLiveStats(w http.ResponseWriter, r *http.Request) {
	did := r.URL.Query().Get("did")
	if did == "" {
		writeErr(w, 400, "缺少 did")
		return
	}

	dir := filepath.Join(s.dataDir, "hls", did)
	ents, err := os.ReadDir(dir)
	if err != nil {
		writeJSON(w, 200, map[string]any{"mbps": 0, "note": "无流"})
		return
	}

	// 取最近 5 个分片
	type seg struct {
		name string
		size int64
		mod  time.Time
	}
	var segs []seg
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".ts") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		// 只统计 10 秒内的新鲜分片
		if time.Since(info.ModTime()) > 10*time.Second {
			continue
		}
		segs = append(segs, seg{e.Name(), info.Size(), info.ModTime()})
	}
	if len(segs) == 0 {
		writeJSON(w, 200, map[string]any{"mbps": 0, "note": "流未启动"})
		return
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i].mod.After(segs[j].mod) })
	if len(segs) > 5 {
		segs = segs[:5]
	}

	var total int64
	for _, sg := range segs {
		total += sg.size
	}
	// HLS 分片固定 1 秒（见 hls.go 的 -hls_time 1）
	const segDur = 1.0
	mbps := float64(total) / float64(len(segs)) * 8 / segDur / 1e6

	writeJSON(w, 200, map[string]any{
		"mbps":     mbps,
		"segments": len(segs),
		"avgKB":    float64(total) / float64(len(segs)) / 1024,
	})
}
