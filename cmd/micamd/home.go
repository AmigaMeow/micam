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
	"math"
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

	// Managed 该设备是否已「纳入管理」。
	//
	// ===== 这是账号↔设备模型的核心字段（2026-09-18 设计确认）=====
	//
	// 「获取设备」只拉云端清单，**不等于**纳入管理。
	// 用户需要明确勾选，把云端设备【纳入】到插件里管理。
	//
	// 语义：
	//   Managed == false  云端有这个设备，但我们不管理它
	//                     （不在设备页/实时预览显示，不录像，不跑 AI）
	//   Managed == true   真正在管理（正常显示、可录像、可跑 AI）
	//
	// 为什么需要区分：
	//   1. 用户可能只想管其中几台（共享设备尤其常见）
	//   2. 「获取」是只读操作，「纳入」是用户的决定 —— 不能混为一谈
	//   3. 移出管理不能影响米家（设备还在云端，只是我们不管了）
	Managed bool `json:"managed,omitempty"`
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

// SetManaged 设置「是否纳入管理」。
func (m *metaStore) SetManaged(did string, managed bool) error {
	m.mu.Lock()
	v := m.data[did]
	v.Managed = managed
	if !managed {
		// 移出管理时保留别名（用户可能以后再加回来）
		v.Hidden = true
	} else {
		v.Hidden = false
	}
	m.data[did] = v
	raw, _ := json.MarshalIndent(m.data, "", "  ")
	err := os.WriteFile(m.path, raw, 0o644)
	m.mu.Unlock()
	return err
}

// IsManaged 该设备是否已纳入管理。
//
// 注意「向后兼容」：老用户的数据里没有 managed 字段，
// 默认值 false 会导致他们突然什么都看不见。
// 所以这里把「没有任何元数据记录」视为已管理（老数据），
// 只有显式设过 false 才算移出。
func (m *metaStore) IsManaged(did string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[did]
	if !ok {
		return true // 从没记录过 -> 老数据，视为已管理
	}
	if v.Managed {
		return true
	}
	// 显式移出过：只有 Hidden 也为真才算移出
	// （避免「只设过别名、没设过 managed」的设备被误判为未管理）
	return !v.Hidden
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
	mu       sync.Mutex
	prev     map[string][]uint8   // did -> 上一帧灰度
	prevAt   map[string]time.Time // did -> 上次采样的缓存时间戳（用于判断缓存是否更新）
	lastEvt  map[string]time.Time // did -> 上次记录事件的时间
	streak   map[string]int       // 连续触发次数
	segStart map[string]segInfo   // did -> 当前活动段（用于累计时长）
}

// segInfo 一段连续活动的累积信息
type segInfo struct {
	Duration int
	Peak     float64
}

type homeStationaryCar struct {
	box      []int
	lastSeen time.Time
	emitted  time.Time
}

var (
	homeStationaryMu   sync.Mutex
	homeStationaryCars = make(map[string]*homeStationaryCar) // key: did
)

var motion = &motionState{
	prev:     map[string][]uint8{},
	prevAt:   map[string]time.Time{},
	lastEvt:  map[string]time.Time{},
	streak:   map[string]int{},
	segStart: map[string]segInfo{},
}

const (
	motionInterval = 20 * time.Second // 采样间隔：从 8s 调大到 20s，减少 P2P 连接频率，避免触发小米限流
	motionSilence  = 60 * time.Second // 同一设备静默期：从 30s 调大到 60s，更好地合并连续触发
	motionThresh   = 6.0              // 平均绝对差阈值（0-255）
	motionW        = 160              // 降采样宽度
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
	// ⚠️ 限流静默期内完全停止动检（2026-09-20 优化）
	//
	// 动检每 8 秒抓一帧，会把 P2P 连接一直「焐热」（connIdle = 25 秒，
	// 所以 8 秒的间隔让连接永远不释放）。
	//
	// 这有两个后果：
	//   1. 摄像头被持续占用，用户想看实时预览时可能抢不到连接
	//   2. 一旦凭据失效，每 8 秒都可能触发一次云端申请 -> 撞限流
	//
	// 静默期仅限制依赖 miss_get_vendor 的传统未绑定通道。
	// 官方通道（OAuth2+PPCS）使用独立局域网直连，不受传统限流影响。
	if _, ok := miotBound(); !ok && s.throttleSilenced() {
		return
	}

	// 另外：没有凭据时也别试（抓不到，只会触发申请）
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
			motion.mu.Lock()
			motion.streak[d.Did] = 0
			motion.mu.Unlock()
			continue
		}

		// 触发：静默期内不重复记，但要累计时长
		motion.mu.Lock()
		last := motion.lastEvt[d.Did]
		if time.Since(last) < motionSilence {
			// 同一段活动的延续：累加时长并顺延录像
			if seg, ok := motion.segStart[d.Did]; ok {
				seg.Duration += int(motionInterval / time.Second)
				if score > seg.Peak {
					seg.Peak = score
				}
				motion.segStart[d.Did] = seg
			}
			motion.mu.Unlock()
			s.renewMotionRecording(d)
			continue
		}
		motion.lastEvt[d.Did] = time.Now()
		motion.streak[d.Did]++
		motion.mu.Unlock()

		s.triggerMotionRecording(d, false)
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

		var foundPerson *aiObject
		var foundAnimal *aiObject
		var foundPackage *aiObject
		var foundVehicle *aiObject

		if s.ai != nil && s.ai.IsReady() {
			if res, err := s.ai.Detect(data); err == nil && res != nil {
				for i := range res.Objects {
					o := &res.Objects[i]
					if o.Type == "person" {
						if foundPerson == nil || o.Conf > foundPerson.Conf {
							foundPerson = o
						}
					} else if o.Type == "animal" || o.Cls == "dog" || o.Cls == "cat" {
						if foundAnimal == nil || o.Conf > foundAnimal.Conf {
							foundAnimal = o
						}
					} else if o.Type == "package" {
						if foundPackage == nil || o.Conf > foundPackage.Conf {
							foundPackage = o
						}
					} else if o.Type == "vehicle" || o.Cls == "car" || o.Cls == "truck" || o.Cls == "bus" {
						if foundVehicle == nil || o.Conf > foundVehicle.Conf {
							foundVehicle = o
						}
					}
				}
			} else if err != nil {
				if !strings.Contains(err.Error(), "限流") {
					logf("[ai] %s 推理失败: %v", d.Name, err)
				}
			}
		}

		if foundPerson != nil {
			evType, evDesc, evConf = "person", "检测到有人", foundPerson.Conf*100
			aiBox, aiCls = foundPerson.Box, foundPerson.Cls
		} else if foundAnimal != nil {
			evType = "animal"
			if foundAnimal.Cls == "dog" {
				evDesc = "检测到小狗"
			} else if foundAnimal.Cls == "cat" {
				evDesc = "检测到猫咪"
			} else {
				evDesc = "检测到宠物活动"
			}
			evConf = foundAnimal.Conf * 100
			aiBox, aiCls = foundAnimal.Box, foundAnimal.Cls
		} else if foundPackage != nil {
			evType, evDesc, evConf = "package", "检测到包裹", foundPackage.Conf*100
			aiBox, aiCls = foundPackage.Box, foundPackage.Cls
		} else if foundVehicle != nil {
			homeStationaryMu.Lock()
			st := homeStationaryCars[d.Did]
			now := time.Now()
			isStationary := false
			if st != nil && len(st.box) >= 4 && len(foundVehicle.Box) >= 4 {
				iou := boxIoU(foundVehicle.Box, st.box)
				cx1, cy1 := (foundVehicle.Box[0]+foundVehicle.Box[2])/2, (foundVehicle.Box[1]+foundVehicle.Box[3])/2
				cx2, cy2 := (st.box[0]+st.box[2])/2, (st.box[1]+st.box[3])/2
				dist2 := (cx1-cx2)*(cx1-cx2) + (cy1-cy2)*(cy1-cy2)
				if iou > 0.35 || dist2 < 2500 {
					isStationary = true
					st.lastSeen = now
				}
			}

			if !isStationary {
				homeStationaryCars[d.Did] = &homeStationaryCar{
					box:      foundVehicle.Box,
					lastSeen: now,
					emitted:  now,
				}
				homeStationaryMu.Unlock()
				evType, evDesc, evConf = "vehicle", "检测到车辆", foundVehicle.Conf*100
				aiBox, aiCls = foundVehicle.Box, foundVehicle.Cls
			} else {
				if now.Sub(st.emitted) < 30*time.Minute {
					homeStationaryMu.Unlock()
					logf("[motion] %s 忽略静止停放车辆重复触发 (坐标未变)", d.Name)
					continue
				}
				st.emitted = now
				homeStationaryMu.Unlock()
				evType, evDesc, evConf = "vehicle", "车辆静止停放", foundVehicle.Conf*100
				aiBox, aiCls = foundVehicle.Box, foundVehicle.Cls
			}
		}

		if aiCls != "" {
			logf("[ai] %s 识别为 %s (%.0f%%)", d.Name, aiCls, evConf)
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
		})
		logf("[motion] %s %s (score=%.1f)", d.Name, evDesc, score)
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
		Did      string `json:"did"`
		Alias    string `json:"alias"`
		AliasEn  string `json:"aliasEn"`
		Group    string `json:"group"`
		Favorite *bool  `json:"favorite"`
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

// findEventRecording 根据设备名和时间定位录像文件与秒级偏移
func (s *Server) findEventRecording(devName, did, eventTime string) (string, float64) {
	if eventTime == "" {
		return "", 0
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", eventTime, time.Local)
	if err != nil {
		if t2, err2 := time.Parse(time.RFC3339, eventTime); err2 == nil {
			t = t2.In(time.Local)
		} else {
			return "", 0
		}
	}

	dateStr := t.Format("2006-01-02")
	roots := s.recRoots()

	names := []string{devName, sanitizeName(devName)}
	if did != "" {
		for _, c := range s.allCameras() {
			if c.Did == did {
				names = append(names, c.Name, sanitizeName(c.Name))
			}
		}
		names = append(names, did)
	}

	type matchCandidate struct {
		file    string
		offset  float64
		diffSec float64
	}
	var best *matchCandidate

	seenNames := make(map[string]bool)
	var uniqNames []string
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n != "" && !seenNames[n] {
			seenNames[n] = true
			uniqNames = append(uniqNames, n)
		}
	}

	for _, n := range uniqNames {
		for _, root := range roots {
			files := s.collectRecFiles(root, n, dateStr)
			for _, f := range files {
				fStart, err := time.ParseInLocation("2006-01-02 15:04:05", f.StartTime, time.Local)
				if err != nil {
					continue
				}
				dur := f.Duration
				if dur <= 0 {
					dur = 300
				}
				offset := t.Sub(fStart).Seconds()
				if offset >= -5 && offset <= dur+10 {
					realOff := offset
					if realOff < 0 {
						realOff = 0
					}
					return f.Name, realOff
				}

				diff := math.Abs(offset)
				if diff < 600 && (best == nil || diff < best.diffSec) {
					best = &matchCandidate{
						file:    f.Name,
						offset:  math.Max(0, offset),
						diffSec: diff,
					}
				}
			}
		}
	}

	if best != nil {
		return best.file, best.offset
	}
	return "", 0
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

	// 动态关联录像片段（修复未显式绑定 recFile 的实时告警事件）
	for i := range list {
		if list[i].RecFile == "" && list[i].Time != "" {
			f, off := s.findEventRecording(list[i].Device, list[i].Did, list[i].Time)
			if f != "" {
				list[i].RecFile = f
				list[i].RecOff = off
			}
		}
	}

	// 车牌过滤（阶段 2 B：按车牌检索）
	if plate := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("plate"))); plate != "" {
		var only []event.Event
		for _, e := range list {
			if strings.ToUpper(e.AIPlate) == plate {
				only = append(only, e)
			}
		}
		list = only
	}

	// 附上用户标记（书签/需关注/标签）与自用白名单
	marks := s.marks.list()
	var wl []*WhitelistItem
	if gWhitelistStore != nil {
		wl = gWhitelistStore.List()
	}

	writeJSON(w, 200, map[string]any{
		"events":    list,
		"today":     s.eventStore.CountToday(),
		"marks":     marks,
		"whitelist": wl,
	})
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
		Did      string `json:"did"`
		Name     string `json:"name"`
		Alias    string `json:"alias,omitempty"`
		AliasEn  string `json:"aliasEn,omitempty"`
		Online   bool   `json:"online"`
		Shared   bool   `json:"shared,omitempty"`
		Lenses   int    `json:"lenses,omitempty"`
		LastSeen string `json:"lastSeen,omitempty"`
		ThumbAt  int64  `json:"thumbAt,omitempty"`
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
	//
	// ⚠️ 之前没区分「没有设备」和「设备都离线」（2026-09-18 实测发现）：
	//    全新用户一台摄像头都没有时，首页显示
	//    「所有摄像头离线，请检查网络或电源」——
	//    这会让人以为设备坏了，而不是「还没添加」。
	statusText := "一切正常，家中当前安全。"
	if len(camList) == 0 {
		// 一台设备都没有：区分「没登录」和「登录了但账号下没摄像头」
		if len(s.store.AllSessions()) == 0 {
			statusText = "还没有添加摄像头，请先到「账号」页扫码登录。"
		} else {
			statusText = "当前账号下没有摄像头，可到「账号」页重新登录或获取设备。"
		}
	} else if online == 0 {
		statusText = "所有摄像头离线，请检查网络或电源。"
	} else if online < len(camList) {
		statusText = fmt.Sprintf("有 %d 台摄像头离线。", len(camList)-online)
	}

	// 存储用量（首页概览卡要用）
	//
	// ⚠️ 这里必须**整套**取自 DiskUsageOf，不能只取一个百分比、再让前端
	//    去 /api/storage 取另一套 GB 数拼在一起 —— 那正是两个数打架的来源。
	//
	// 历史 bug（2026-10-12 实测）：
	//   本文件原来自己算 (TotalGB - FreeGB) / TotalGB = 68.94%，
	//   而 /api/storage 用 DiskUsage.UsedPct = UsedGB / TotalGB = 67.53%。
	//   两者不等价：FreeGB = Bavail（普通用户可用块），
	//   UsedGB = Total - Bfree（含文件系统保留块）。
	//   结果同一个磁盘在首页显示 68.9%、在存储面板显示 67.5%，
	//   而且首页那个数还和面板里的「已用 278 GB / 412 GB」自相矛盾。
	//
	// 现在只暴露一套数，前端两处都用它渲染，结构上不可能再分叉。
	du := record.DiskUsageOf(s.rec.GetConfig().Dir)

	// 录像明细：每台设备的段数与占用。
	// 与 /api/storage 共用 collectCamStorage，避免首页和存储页各算一遍
	// 导致「首页 17 段、存储页 18 段」这类新分叉。
	recDir := s.rec.GetConfig().Dir
	if recDir == "" {
		recDir = filepath.Join(s.dataDir, "recordings")
	}
	camRecs, recFiles, recBytes := s.collectCamStorage(recDir)
	camRecsOut := make([]map[string]any, 0, len(camRecs))
	for _, c := range camRecs {
		camRecsOut = append(camRecsOut, map[string]any{
			"did": c.Did, "name": c.Name, "files": c.Files, "sizeMB": c.SizeMB,
		})
	}

	silenceLeft := s.throttleSilenceLeft()
	if _, ok := miotBound(); ok {
		silenceLeft = 0 // 官方通道不受传统凭据限流影响
	}
	silenceMin := int(silenceLeft.Minutes())

	writeJSON(w, 200, map[string]any{
		"online":       online,
		"total":        len(camList),
		"offline":      len(camList) - online,
		"silenced":     silenceLeft > 0,
		"silenceMin":   silenceMin,
		"storagePct":   du.UsedPct,
		"storageUsed":  du.UsedGB,
		"storageFree":  du.FreeGB,
		"storageTotal": du.TotalGB,
		// 与 /api/storage 的 estimateHours 同源（同一个函数），
		// 否则首页说 6 天、存储页说 390 天。
		"storageEstHours": estimateRecordHours(du.FreeGB),
		// 首页存储明细卡用（顶部概览卡不重复展示这些）
		"recCams":     camRecsOut,
		"recFiles":    recFiles,
		"recMB":       float64(recBytes) / (1 << 20),
		"todayEvents": s.eventStore.CountToday(),
		"statusText":  statusText,
		"now":         time.Now().Format("2006-01-02 15:04:05"),
		"cameras":     camList,
		"events":      s.eventStore.Query(event.Query{Days: 3, Limit: 5}),
		"ui":          s.cfg.UI,
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
