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
	"strings"
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

// dirUsage 统计目录大小与文件数。
//
// skipHidden=true 时跳过子目录中【以 . 开头】的目录（.cache / .thumbs / .frames）。
//
// 为什么需要这个开关（2026-09-23 实测发现的 bug）：
//
//	recordings 根目录下混着辅助数据：
//	  .thumbs  缩略图        （190 个文件）
//	  .cache   转码缓存      （465 MB）
//	  .frames  抽帧
//	而 .cache 已经被【单独列为「转码缓存」一类】。
//	早期实现无条件递归统计整个 recordings，于是 .cache 被算了两次
//	（既进「录像数据」又进「转码缓存」），
//	表现为「录像文件 0 个」却「录像数据 467 MB」。
func dirUsage(root string, skipHidden bool) (int64, int) {
	var bytes int64
	var count int
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if info.IsDir() {
			// 跳过隐藏目录，但 root 自身不算
			if skipHidden && p != root && strings.HasPrefix(info.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		// 隐藏文件也跳过（如 recordings/.meta.json —— 它是索引不是录像）
		if skipHidden && strings.HasPrefix(info.Name(), ".") {
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
	add := func(key, name, dir string, skipHidden bool) {
		b, n := dirUsage(dir, skipHidden)
		if b == 0 && n == 0 {
			return
		}
		types = append(types, storageType{Key: key, Name: name, SizeGB: float64(b) / 1073741824, Files: n})
	}
	// 录像数据要跳过隐藏目录 —— 否则 .cache 会同时被算进「转码缓存」
	add("recordings", "录像数据", recRoot, true)
	add("cache", "转码缓存", cacheDir, false)
	add("events", "事件数据", eventsDir, false)
	add("models", "AI 模型", modelsDir, false)
	add("lpr", "车牌模型", lprDir, false)

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
	//
	// ⚠️ 必须以**摄像头**为基准遍历，而不是遍历录像目录（2026-10-13 修）。
	//
	// 原实现是 os.ReadDir(recRoot) 拿到目录名、再反过来匹配摄像头 ——
	// 于是**没有录像目录的设备直接不出现**。实测：离线的「二楼房间摄像头」
	// 在存储页表格里整个消失，用户看到「只有 2 台」会以为统计不全。
	//
	// 现改用 s.collectCamStorage()（与首页存储明细卡**同一实现**），
	// 保证：① 每台摄像头都出现（无录像的显示 0）
	//       ② 两处口径不会再次分叉（本项目已因两套实现吃过两次亏）。
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
	rawCams, _, _ := s.collectCamStorage(recRoot)
	cams := []camStat{}
	for _, rc := range rawCams {
		cs := camStat{
			Did:      rc.Did,
			Name:     rc.Name,
			Online:   rc.Online,
			SizeGB:   rc.SizeMB / 1024,
			Files:    rc.Files,
			KeepDays: s.cfg.RecordKeepDays,
		}
		// 预计可录天数：总可用 / 每天写入（用录像总时长估算）
		if cs.SizeGB > 0 && disk.FreeGB > 0 {
			hours := float64(rc.Files) * float64(s.cfg.RecordSegment) / 3600.0
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
	// collectCamStorage 已按占用降序，这里保持同一顺序即可

	// ---- 孤儿录像目录（未归属）----
	//
	// ⚠️ 为什么需要这一步（2026-10-13）：
	// 上面改成**以摄像头为基准**遍历后，修复了「没有录像目录的设备不出现」，
	// 但也丢掉了原来目录遍历唯一的优点 —— **对账**。
	// 已移出/已删除设备遗留在磁盘上的录像目录，不会归属到任何一台摄像头，
	// 于是「各台之和 < 磁盘实际占用」，用户找不到这部分空间是谁的。
	//
	// 这里扫目录差集，把非空孤儿目录聚合成一行「未归属（已移出设备）」。
	// 既保留 3 台全量展示，又能对上账。
	known := map[string]bool{}
	for _, rc := range rawCams {
		known[safeDirName(rc.Name)] = true
	}
	var orphanBytes int64
	orphanFiles := 0
	orphanDirs := 0
	orphanNames := []string{}
	if entries, err := os.ReadDir(recRoot); err == nil {
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			if known[e.Name()] {
				continue
			}
			b, n := dirUsage(filepath.Join(recRoot, e.Name()), false)
			if b == 0 && n == 0 {
				continue // 空目录不算占用，不占一行
			}
			orphanBytes += b
			orphanFiles += n
			orphanDirs++
			orphanNames = append(orphanNames, e.Name())
		}
	}
	orphan := map[string]any{
		"dirs":   orphanDirs,
		"names":  orphanNames,
		"sizeGB": float64(orphanBytes) / 1073741824,
		"files":  orphanFiles,
	}

	// ---- 缓存 ----
	cacheBytes, cacheCount := dirUsage(cacheDir, false)
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
		// 未归属的录像目录（已移出/已删除设备遗留），用于对账。
		// 各台之和 + 本项 == 录像根目录实际占用。
		"orphan": orphan,
		// 可录时长估算。与 /api/storage 的 estimateHours 用**同一个函数**，
		// 避免同一件事在不同接口给出不同数字（曾经 6 天 vs 390 天）。
		"estimateHours": estimateRecordHours(disk.FreeGB),
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
	// 与「录像数据」口径一致：跳过隐藏目录（否则趋势里会混入 .cache）
	recB, _ := dirUsage(s.recRoot(), true)
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
