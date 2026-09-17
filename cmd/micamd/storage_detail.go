package main

// 存储页增强接口（对齐效果图）。
//
//   GET /api/storage/detail
//     返回：磁盘 / 统计卡 / 按类型统计 / 按摄像头 / 缓存 / 趋势 / 底部状态栏
//
// ===== 4 类统计怎么算的（诚实说明）=====
//
// 效果图里的四类是：连续录像 / 事件录像 / 缓存 / AI 元数据。
// 我们的数据模型里**没有区分「连续」和「事件」录像**（都是连续录），
// 所以映射成：
//
//   录像数据   -> recordings 根目录（真实）
//   转码缓存   -> .cache 目录（真实）
//   事件数据   -> events 目录（事件 JSONL + 缩略图，真实）
//   AI 模型    -> models + lpr 目录（真实）
//
// 前端分类名会如实标注，不假装我们区分了录像类型。

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/micam/micam/internal/record"
)

// storageType 一类占用
type storageType struct {
	Key    string  `json:"key"`
	Name   string  `json:"name"`
	SizeGB float64 `json:"sizeGB"`
	Files  int     `json:"files"`
	Pct    float64 `json:"pct"`
}

// storageTrendPoint 趋势图一个点
type storageTrendPoint struct {
	Date    string  `json:"date"`
	Label   string  `json:"label"`
	UsedGB  float64 `json:"usedGB"`
	TotalGB float64 `json:"totalGB"`
	RecGB   float64 `json:"recGB"`
}

// dirUsage 统计目录大小与文件数
func dirUsage(root string) (int64, int) {
	var bytes int64
	var count int
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		bytes += info.Size()
		count++
		return nil
	})
	return bytes, count
}

// handleStorageDetail 存储页明细
func (s *Server) handleStorageDetail(w http.ResponseWriter, r *http.Request) {
	base := dataDirPath

	// ---- 磁盘 ----
	type diskInfo struct {
		Path    string  `json:"path"`
		TotalGB float64 `json:"totalGB"`
		UsedGB  float64 `json:"usedGB"`
		FreeGB  float64 `json:"freeGB"`
		UsedPct float64 `json:"usedPct"`
	}
	disk := diskInfo{}
	if du := record.DiskUsageOf(base); du.Err == "" {
		disk.Path = du.Path
		disk.TotalGB = du.TotalGB
		disk.UsedGB = du.UsedGB
		disk.FreeGB = du.FreeGB
		disk.UsedPct = du.UsedPct
	}

	// ---- 按类型统计（真实目录）----
	recRoot := s.recRoot()
	cacheDir := filepath.Join(recRoot, ".cache")
	eventsDir := filepath.Join(base, "events")
	modelsDir := filepath.Join(base, "models")
	lprDir := filepath.Join(base, "lpr")

	types := []storageType{}
	add := func(key, name, dir string) {
		b, n := dirUsage(dir)
		if b == 0 && n == 0 {
			return
		}
		types = append(types, storageType{Key: key, Name: name, SizeGB: float64(b) / 1073741824, Files: n})
	}
	add("recordings", "录像数据", recRoot)
	add("cache", "转码缓存", cacheDir)
	add("events", "事件数据", eventsDir)
	add("models", "AI 模型", modelsDir)
	add("lpr", "车牌模型", lprDir)

	var totalGB float64
	for _, t := range types {
		totalGB += t.SizeGB
	}
	for i := range types {
		if totalGB > 0 {
			types[i].Pct = types[i].SizeGB / totalGB * 100
		}
	}
	sort.Slice(types, func(i, j int) bool { return types[i].SizeGB > types[j].SizeGB })

	// ---- 按摄像头 ----
	type camStat struct {
		Did      string  `json:"did"`
		Name     string  `json:"name"`
		Online   bool    `json:"online"`
		SizeGB   float64 `json:"sizeGB"`
		Files    int     `json:"files"`
		KeepDays int     `json:"keepDays"`
		EstDays  float64 `json:"estDays"` // 按当前占用速度还能录多久
		Thumb    string  `json:"thumb,omitempty"`
	}
	cams := []camStat{}
	entries, _ := os.ReadDir(recRoot)
	for _, e := range entries {
		if !e.IsDir() || len(e.Name()) > 0 && e.Name()[0] == '.' {
			continue
		}
		n := e.Name()
		b, cnt := dirUsage(filepath.Join(recRoot, n))
		cs := camStat{Name: n, SizeGB: float64(b) / 1073741824, Files: cnt, KeepDays: s.cfg.RecordKeepDays}
		// 关联 did / 在线状态 / 缩略图
		for _, d := range s.allCameras() {
			if d.Name == n {
				cs.Did = d.Did
				cs.Online = d.IsOnline
				break
			}
		}
		// 预计可录天数：总可用 / 每天写入（用录像总时长估算）
		if cs.SizeGB > 0 && disk.FreeGB > 0 {
			// 粗略：假设已录时间 = 文件数 × 分片时长
			hours := float64(cnt) * float64(s.cfg.RecordSegment) / 3600.0
			if hours > 0 {
				perHour := cs.SizeGB / hours
				if perHour > 0 {
					cs.EstDays = disk.FreeGB / perHour / 24
				}
			}
		}
		if cs.EstDays > 999 {
			cs.EstDays = 999
		}
		cams = append(cams, cs)
	}
	sort.Slice(cams, func(i, j int) bool { return cams[i].SizeGB > cams[j].SizeGB })

	// ---- 缓存 ----
	cacheBytes, cacheCount := dirUsage(cacheDir)
	cache := map[string]any{
		"dir":    cacheDir,
		"sizeGB": float64(cacheBytes) / 1073741824,
		"files":  cacheCount,
		"maxGB":  float64(s.cfg.TranscodeCacheGB),
		"lastAt": s.lastCacheClean(),
	}

	// ---- 趋势（读每日采样）----
	trend := loadTrend(7)

	// ---- 底部状态栏 ----
	status := map[string]any{
		"recPath":  recRoot,
		"dataPath": base,
		"lastScan": s.lastScanTime(),
		"healthy":  disk.UsedPct < 90,
	}

	writeJSON(w, 200, map[string]any{
		"ok":      true,
		"disk":    disk,
		"types":   types,
		"cameras": cams,
		"cache":   cache,
		"trend":   trend,
		"status":  status,
		"policy": map[string]any{
			"segmentSec":   s.cfg.RecordSegment,
			"keepDays":     s.cfg.RecordKeepDays,
			"minFreeGB":    s.cfg.RecordMinFreeGB,
			"thresholdPct": diskThresholdPct(float64(s.cfg.RecordMinFreeGB), disk.TotalGB),
			"autoClean":    s.cfg.RecordAutoClean,
			"lowSpaceMode": s.cfg.RecordLowSpaceMode,
			"cacheMaxGB":   s.cfg.TranscodeCacheGB,
		},
	})
}

// diskThresholdPct 由「最低剩余 GB」换算成「已用百分比阈值」
func diskThresholdPct(minFreeGB, totalGB float64) int {
	if totalGB <= 0 {
		return 85
	}
	pct := (1 - minFreeGB/totalGB) * 100
	if pct < 50 {
		pct = 50
	}
	if pct > 99 {
		pct = 99
	}
	return int(pct)
}

// ---- 趋势采样 ----

type trendFile struct {
	Points []storageTrendPoint `json:"points"`
}

func trendPath() string { return filepath.Join(dataDirPath, "storage-trend.json") }

func loadTrend(days int) []storageTrendPoint {
	raw, err := os.ReadFile(trendPath())
	if err != nil {
		return nil
	}
	var tf trendFile
	if json.Unmarshal(raw, &tf) != nil {
		return nil
	}
	pts := tf.Points
	// 只保留最近 days 天
	cut := time.Now().AddDate(0, 0, -days).Format("2006-01-02")
	var out []storageTrendPoint
	for _, p := range pts {
		if p.Date >= cut {
			out = append(out, p)
		}
	}
	return out
}

// sampleStorageTrend 记一个采样点（每天一次即可，重复调用覆盖当天）
func (s *Server) sampleStorageTrend() {
	du := record.DiskUsageOf(dataDirPath)
	if du.Err != "" {
		return
	}
	total := du.TotalGB
	used := du.UsedGB
	recB, _ := dirUsage(s.recRoot())
	recGB := float64(recB) / 1073741824

	now := time.Now()
	p := storageTrendPoint{
		Date:    now.Format("2006-01-02"),
		Label:   now.Format("1/2"),
		UsedGB:  used,
		TotalGB: total,
		RecGB:   recGB,
	}

	var tf trendFile
	if raw, err := os.ReadFile(trendPath()); err == nil {
		_ = json.Unmarshal(raw, &tf)
	}
	replaced := false
	for i := range tf.Points {
		if tf.Points[i].Date == p.Date {
			tf.Points[i] = p
			replaced = true
			break
		}
	}
	if !replaced {
		tf.Points = append(tf.Points, p)
	}
	// 只留最近 30 天
	if len(tf.Points) > 30 {
		tf.Points = tf.Points[len(tf.Points)-30:]
	}
	raw, _ := json.MarshalIndent(tf, "", "  ")
	_ = os.WriteFile(trendPath()+".tmp", raw, 0o644)
	_ = os.Rename(trendPath()+".tmp", trendPath())
}

// lastScanTime 最近一次扫描/采样时间
func (s *Server) lastScanTime() string {
	if fi, err := os.Stat(trendPath()); err == nil {
		return fi.ModTime().Format("2006-01-02 15:04")
	}
	return "—"
}

func (s *Server) lastCacheClean() string {
	if fi, err := os.Stat(filepath.Join(s.recRoot(), ".cache")); err == nil {
		return fi.ModTime().Format("2006-01-02 15:04")
	}
	return "—"
}
