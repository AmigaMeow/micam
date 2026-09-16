package main

// 移动侦测 + 事件记录 + 设备别名 + 首页聚合接口。

// ===== 移动侦测原理 ====
//
// 复用缩略图缓存：每台在线设备每 N 秒有一张 640px 的图。
// 拿相邻两张做**灰度降采样 + 平均绝对差**，超阈值即为「移动」。
//
// 为什么不用 ffmpeg 场景检测：那要持续解码 2K 流，
// 两路就约 130% CPU。帧差法几乎零成本（图已在内存）。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/micam/micam/internal/event"
	"github.com/micam/micam/internal/record"
)

// ---- 设备别名 / 分组 ----

type devMeta struct {
	Alias    string `json:"alias,omitempty"`    // 中文别名（门口）
	AliasEn  string `json:"aliasEn,omitempty"`  // 英文名（Front Door）
	Group    string `json:"group,omitempty"`    // 分组
	Favorite bool   `json:"favorite,omitempty"` // 收藏
	Hidden   bool   `json:"hidden,omitempty"`   // 从列表隐藏（清理已拆掉的离线设备）
	DevName  string `json:"devName,omitempty"`  // 设备原名（别名反查用）
}

type metaStore struct {
	mu   sync.Mutex
	path string
	data map[string]devMeta
}

func newMetaStore(dataDir string) *metaStore {
	m := &metaStore{path: filepath.Join(dataDir, "devices.json"), data: map[string]devMeta{}}
	if raw, err := os.ReadFile(m.path); err == nil {
		_ = json.Unmarshal(raw, &m.data)
	}
	return m
}

func (m *metaStore) Get(did string) devMeta {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.data[did]
}

func (m *metaStore) Set(did string, v devMeta) error {
	m.mu.Lock()
	m.data[did] = v
	raw, _ := json.MarshalIndent(m.data, "", "  ")
	err := os.WriteFile(m.path, raw, 0o644)
	m.mu.Unlock()
	return err
}

// GetByDevName 按设备名找别名。
//
// 搜索时用户说的是别名（「门口」），而事件里存的是设备名，
// 需要反查。
func (m *metaStore) GetByDevName(devName string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range m.data {
		if v.DevName == devName && v.Alias != "" {
			return v.Alias
		}
	}
	return ""
}

// ---- 侦测状态 ----

type motionState struct {
	mu      sync.Mutex
	prev    map[string][]uint8    // did -> 上一帧灰度
	prevAt   map[string]time.Time  // did -> 上次采样的缓存时间戳（用于判断缓存是否更新）
	lastEvt  map[string]time.Time  // did -> 上次记录事件的时间
	streak   map[string]int        // 连续触发次数
	segStart map[string]segInfo    // did -> 当前活动段（用于累计时长）
}

// segInfo 一段连续活动的累积信息
type segInfo struct {
	Duration int
	Peak     float64
}

var motion = &motionState{
	prev:     map[string][]uint8{},
	prevAt:   map[string]time.Time{},
	lastEvt:  map[string]time.Time{},
	streak:   map[string]int{},
	segStart: map[string]segInfo{},
}

const (
	motionInterval = 8 * time.Second  // 采样间隔（抓帧本身要 3-9 秒）
	motionSilence  = 30 * time.Second // 同一设备的静默期（合并连续触发）
	motionThresh   = 6.0             // 平均绝对差阈值（0-255）
	motionW        = 160             // 降采样宽度
	motionH        = 90
)

// startMotionDetector 启动移动侦测。
func (s *Server) startMotionDetector() {
	go func() {
		time.Sleep(45 * time.Second) // 等缩略图预热完
		t := time.NewTicker(motionInterval)
		defer t.Stop()
		for range t.C {
			s.detectOnce()
		}
	}()
}

func (s *Server) detectOnce() {
	for _, d := range s.allCameras() {
		if !d.IsCamera() || !d.IsOnline {
			continue
		}
		// 关键：侦测器必须自己触发刷新，否则缓存是静止的，
		// 两张图永远一样，score 恒为 0。
		s.refreshThumbAsync(d.Did, motionW*2)

		data, at := s.getThumbAt(d.Did)
		if len(data) == 0 {
			continue
		}

		// 只在缓存「更新过」时才比较，避免拿同一张图对比
		motion.mu.Lock()
		lastAt := motion.prevAt[d.Did]
		motion.mu.Unlock()
		if !lastAt.IsZero() && at.Equal(lastAt) {
			continue // 缓存没变（还在抓），本轮跳过
		}

		gray := s.downscaleGray(data, motionW, motionH)
		if gray == nil {
			continue
		}

		motion.mu.Lock()
		prev := motion.prev[d.Did]
		motion.prev[d.Did] = gray
		motion.prevAt[d.Did] = at
		motion.mu.Unlock()

		if prev == nil {
			continue // 基准帧
		}

		score := meanAbsDiff(prev, gray)
		if score < motionThresh {
			continue
		}
		if score < motionThresh {
			motion.mu.Lock(); motion.streak[d.Did] = 0; motion.mu.Unlock()
			continue
		}

		// 触发：静默期内不重复记，但要累计时长
		motion.mu.Lock()
		last := motion.lastEvt[d.Did]
		if time.Since(last) < motionSilence {
			// 同一段活动的延续：累加时长
			if seg, ok := motion.segStart[d.Did]; ok {
				seg.Duration += int(motionInterval / time.Second)
				if score > seg.Peak {
					seg.Peak = score
				}
				motion.segStart[d.Did] = seg
			}
			motion.mu.Unlock()
			continue
		}
		motion.lastEvt[d.Did] = time.Now()
		motion.streak[d.Did]++
		motion.mu.Unlock()

		thumbName := s.eventStore.SaveThumb(data)
		name := d.Name
		if m := s.meta.Get(d.Did); m.Alias != "" {
			name = m.Alias
		}

		// 置信度：把帧差分数映射到 78~99 区间
		// （帧差不是概率，这只是给 UI 一个直观的强弱指示）
		conf := 78 + score*0.45
		if conf > 99 {
			conf = 99
		}

		// ---- AI 分类（可选）----
		//
		// 帧差只能告诉我们「有变化」，AI 告诉我们「是什么」。
		// AI 未启用/不可用时静默跳过，保持 motion 类型 —— 不阻塞、不报错。
		evType := event.TypeMotion
		evDesc := "检测到画面变化"
		evConf := conf
		var aiBox []int
		var aiCls string

		if s.ai != nil && s.ai.IsReady() {
			if res, err := s.ai.Detect(data); err == nil && res != nil {
				for _, o := range res.Objects {
					if o.Type == "person" {
						evType, evDesc, evConf = "person", "检测到有人", o.Conf*100
						aiBox, aiCls = o.Box, o.Cls
						break
					}
					if o.Type == "vehicle" && evType != "person" {
						evType, evDesc, evConf = "vehicle", "检测到车辆", o.Conf*100
						aiBox, aiCls = o.Box, o.Cls
					}
					if o.Type == "package" && evType == event.TypeMotion {
						evType, evDesc, evConf = "package", "检测到包裹", o.Conf*100
						aiBox, aiCls = o.Box, o.Cls
					}
				}
				if aiCls != "" {
					logf("[ai] %s 识别为 %s (%.0f%%, %dms)", d.Name, aiCls, evConf, res.MS)
				}
			} else if err != nil {
				if !strings.Contains(err.Error(), "限流") {
					logf("[ai] %s 推理失败: %v", d.Name, err)
				}
			}
		}

		s.eventStore.Add(event.Event{
			Did:      d.Did,
			Device:   name,
			Type:     evType,
			Desc:     evDesc,
			Thumb:    thumbName,
			Score:    score,
			Duration: int(motionInterval / time.Second), // 初值，后续延续会累加
			Conf:     evConf,
			AICls:    aiCls,
			AIBox:    aiBox,
		});
		logf("[motion] %s %s (score=%.1f)", d.Name, evDesc, score);
	}
}

// downscaleGray 把 JPEG 降采样成灰度（用 ffmpeg，几乎瞬时）。
func (s *Server) downscaleGray(jpeg []byte, w, h int) []uint8 {
	cmd := exec.Command(s.ffmpegPath(), "-hide_banner", "-loglevel", "error",
		"-i", "pipe:0", "-vf", fmt.Sprintf("scale=%d:%d,format=gray", w, h),
		"-f", "rawvideo", "pipe:1")
	cmd.Stdin = bytes.NewReader(jpeg)
	out, err := cmd.Output()
	if err != nil || len(out) != w*h {
		return nil
	}
	return out
}

// meanAbsDiff 平均绝对差。
func meanAbsDiff(a, b []uint8) float64 {
	n := len(a)
	if n == 0 || n != len(b) {
		return 0
	}
	var sum int
	for i := 0; i < n; i++ {
		d := int(a[i]) - int(b[i])
		if d < 0 {
			d = -d
		}
		sum += d
	}
	return float64(sum) / float64(n)
}

// getThumbAt 取缩略图缓存内容和它的时间戳。
func (s *Server) getThumbAt(did string) ([]byte, time.Time) {
	thumbMu.Lock()
	defer thumbMu.Unlock()
	if e, ok := thumbCache[did]; ok && len(e.Data) > 0 {
		return e.Data, e.Time
	}
	return nil, time.Time{}
}

// ---- HTTP 接口 ----

// handleDevMeta 读写设备别名。
func (s *Server) handleDevMeta(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		did := r.URL.Query().Get("did")
		if did != "" {
			writeJSON(w, 200, s.meta.Get(did))
			return
		}
		writeJSON(w, 200, s.meta.data)
		return
	}

	var req struct {
		Did     string `json:"did"`
		Alias   string `json:"alias"`
		AliasEn string `json:"aliasEn"`
		Group   string `json:"group"`
		Favorite *bool `json:"favorite"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Did == "" {
		writeErr(w, 400, "参数错误")
		return
	}
	cur := s.meta.Get(req.Did)
	cur.Alias = req.Alias
	cur.AliasEn = req.AliasEn
	cur.Group = req.Group
	// 顺便存设备原名，便于搜索时按别名反查
	if dev, err := s.ensureDevice(req.Did); err == nil && dev != nil {
		cur.DevName = dev.Name
	}
	if req.Favorite != nil {
		cur.Favorite = *req.Favorite
	}
	if err := s.meta.Set(req.Did, cur); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleDevHide 隐藏/恢复设备（本机忽略名单）。
//
// 用途：用户不想要的离线设备（例如已经拆掉的摄像头）可以从列表移除。
// 只写本机名单，**不动小米账号**；重新同步设备时按名单过滤掉。
func (s *Server) handleDevHide(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Did    string `json:"did"`
		Hidden bool   `json:"hidden"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Did == "" {
		writeErr(w, 400, "参数错误")
		return
	}
	cur := s.meta.Get(req.Did)
	cur.Hidden = req.Hidden
	if err := s.meta.Set(req.Did, cur); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "did": req.Did, "hidden": req.Hidden})
}

// handleEvents 查询事件。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	q := event.Query{
		Days:  days,
		Did:   r.URL.Query().Get("did"),
		Type:  r.URL.Query().Get("type"),
		Limit: limit,
	}
	list := s.eventStore.Query(q)
	writeJSON(w, 200, map[string]any{"events": list, "today": s.eventStore.CountToday()})
}

// handleEventThumb 事件缩略图。
func (s *Server) handleEventThumb(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(r.URL.Query().Get("f"))
	if name == "" || name == "." {
		writeErr(w, 400, "缺少文件名")
		return
	}
	serveJPEG(w, s.eventStore.ThumbPath(name))
}

// ---- 首页聚合 ----

// handleHome 首页所需的全部数据（一次请求拿齐，减少往返）。
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	cams := s.allCameras()
	var online int
	for _, d := range cams {
		if d.IsOnline {
			online++
		}
	}

	// 摄像头（含别名 + 缩略图版本号，便于前端判断要不要重取）
	type camOut struct {
		Did       string `json:"did"`
		Name      string `json:"name"`
		Alias     string `json:"alias,omitempty"`
		AliasEn   string `json:"aliasEn,omitempty"`
		Online    bool   `json:"online"`
		Shared    bool   `json:"shared,omitempty"`
		Lenses    int    `json:"lenses,omitempty"`
		LastSeen  string `json:"lastSeen,omitempty"`
		ThumbAt   int64  `json:"thumbAt,omitempty"`
	}
	var camList []camOut
	for _, d := range cams {
		if !d.IsCamera() {
			continue
		}
		m := s.meta.Get(d.Did)
		var thumbAt int64
		thumbMu.Lock()
		if e, ok := thumbCache[d.Did]; ok {
			thumbAt = e.Time.Unix()
		}
		thumbMu.Unlock()
		camList = append(camList, camOut{
			Did: d.Did, Name: d.Name, Alias: m.Alias, AliasEn: m.AliasEn,
			Online: d.IsOnline, Shared: d.Shared, Lenses: d.Lenses,
			LastSeen: s.lastSeen(d.Did), ThumbAt: thumbAt,
		})
	}

	// 系统状态文案
	statusText := "一切正常，家中当前安全。"
	if online == 0 {
		statusText = "所有摄像头离线，请检查网络或电源。"
	} else if online < len(camList) {
		statusText = fmt.Sprintf("有 %d 台摄像头离线。", len(camList)-online)
	}

	// 存储占比（首页概览卡要用）
	var storagePct float64
	if du := record.DiskUsageOf(s.rec.GetConfig().Dir); du.TotalGB > 0 {
		storagePct = (du.TotalGB - du.FreeGB) / du.TotalGB * 100
	}

	writeJSON(w, 200, map[string]any{
		"online":     online,
		"total":      len(camList),
		"storagePct": storagePct,
		"todayEvents": s.eventStore.CountToday(),
		"statusText":  statusText,
		"now":      time.Now().Format("2006-01-02 15:04:05"),
		"cameras":  camList,
		"events":   s.eventStore.Query(event.Query{Days: 3, Limit: 5}),
	})
}

// lastSeen 从数据目录的文件推断设备最后活动时间（录像/事件）。
func (s *Server) lastSeen(did string) string {
	// 简化：从事件库找最近一条该设备的事件
	for _, e := range s.eventStore.Query(event.Query{Days: 7, Did: did, Limit: 1}) {
		return e.Time
	}
	return ""
}
