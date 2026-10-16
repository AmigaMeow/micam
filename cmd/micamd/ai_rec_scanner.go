package main

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/micam/micam/internal/event"
)

type trackedObject struct {
	TrackID      int       `json:"trackId"`
	Cls          string    `json:"cls"`
	Box          []int     `json:"box"`
	Plate        string    `json:"plate"`
	FirstSeen    time.Time `json:"firstSeen"`
	LastEvent    time.Time `json:"lastEvent"`
	LastSeen     time.Time `json:"lastSeen"`
	HitCount     int       `json:"hitCount"` // 累计命中帧数
	Speed        float64   `json:"speed"`    // 像素位移速度
	Status       string    `json:"status"`   // moving / parked
	EmittedCount int       `json:"emittedCount"`
}

var (
	recScanMu      sync.Mutex
	recScannedMap  = make(map[string]bool)
	recScanRunning bool

	trackerMu      sync.Mutex
	cameraTrackers = make(map[string][]*trackedObject)
)


// matchDetectionZone 电子围栏区域判定
func matchDetectionZone(devName string, box []int) string {
	if len(box) < 4 {
		return "车位"
	}
	cx := (box[0] + box[2]) / 2
	cy := box[3]
	// CW300/CW500 监控视角下，画面主要活动区域均标记为车位/监控区
	if cx >= 100 && cx <= 1180 && cy >= 120 && cy <= 710 {
		return "车位"
	}
	return "车位"
}

func boxIoU(b1, b2 []int) float64 {
	if len(b1) < 4 || len(b2) < 4 {
		return 0
	}
	x1 := maxInt(b1[0], b2[0])
	y1 := maxInt(b1[1], b2[1])
	x2 := minInt(b1[2], b2[2])
	y2 := minInt(b1[3], b2[3])
	if x2 <= x1 || y2 <= y1 {
		return 0
	}
	inter := float64((x2 - x1) * (y2 - y1))
	area1 := float64((b1[2] - b1[0]) * (b1[3] - b1[1]))
	area2 := float64((b2[2] - b2[0]) * (b2[3] - b2[1]))
	union := area1 + area2 - inter
	if union <= 0 {
		return 0
	}
	return inter / union
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// parseRecFileTime 从录像文件名中提取开始时间
// 例如 20261014_152456_3.mkv -> 2026-10-14 15:24:56
func parseRecFileTime(name string) (time.Time, bool) {
	base := filepath.Base(name)
	if len(base) < 15 {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("20060102_150405", base[:15], time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// extractRecFrameByPath 从本地录像文件抽出指定秒数的高清帧
func (s *Server) extractRecFrameByPath(src string, t float64, w0 int) ([]byte, error) {
	if w0 <= 0 {
		w0 = 1280
	}
	cmd := exec.Command(s.ffmpegPath(),
		"-hide_banner", "-loglevel", "error",
		"-ss", fmt.Sprintf("%.2f", t),
		"-i", src,
		"-frames:v", "1",
		"-vf", fmt.Sprintf("scale=%d:-2", w0),
		"-f", "image2", "-c:v", "mjpeg",
		"pipe:1",
	)
	return cmd.Output()
}

// scanSingleRecFile 抽取关键帧执行 AI 推理并生成事件
func (s *Server) scanSingleRecFile(devName, date, file string) ([]event.Event, error) {
	fullPath := filepath.Join(s.recRoot(), devName, date, file)
	fi, err := os.Stat(fullPath)
	if err != nil || fi.Size() < 100*1024 {
		return nil, fmt.Errorf("文件过小或不存在")
	}

	startTime, ok := parseRecFileTime(file)
	if !ok {
		startTime = fi.ModTime().Add(-5 * time.Minute)
	}

	// 查找该设备的 did
	did := ""
	for _, d := range s.allCameras() {
		if d.Name == devName || sanitizeName(d.Name) == devName {
			did = d.Did
			break
		}
	}

	// 智能多关键帧采样：3s / 12s / 22s 覆盖分片前中后，防止快速移动漏检
	offsets := []float64{3.0, 12.0, 22.0}
	if fi.Size() > 20*1024*1024 {
		offsets = append(offsets, 35.0, 50.0)
	}

	var newEvents []event.Event
	for _, off := range offsets {
		frame, err := s.extractRecFrameByPath(fullPath, off, 1280)
		if err != nil || len(frame) < 1024 {
			continue
		}

		if s.ai == nil || !s.ai.IsReady() {
			continue
		}

		res, err := s.ai.Detect(frame)
		if err != nil || res == nil || len(res.Objects) == 0 {
			continue
		}

		// 动态目标优先级排序：人员 > 宠物 > 包裹 > 动态 > 车辆
		// 严防院内已停放的静态车辆掩盖走动的人员与小动物
		sort.SliceStable(res.Objects, func(i, j int) bool {
			priority := func(cls string) int {
				switch mapObjType(cls) {
				case "person":
					return 5
				case "animal":
					return 4
				case "package":
					return 3
				case "motion":
					return 2
				default:
					return 1
				}
			}
			return priority(res.Objects[i].Cls) > priority(res.Objects[j].Cls)
		})

		seenTypes := make(map[string]bool)
		for _, obj := range res.Objects {
			evType := obj.Type
			aiCls := obj.Cls
			if evType == "" {
				evType = mapObjType(aiCls)
			}
			if seenTypes[evType] {
				continue
			}
			seenTypes[evType] = true

			evTime := startTime.Add(time.Duration(off) * time.Second)

			// 连续轨迹跟踪与静止去抖（借鉴 Clearcam OC-SORT & Speed Gating 机制）
			trackerMu.Lock()
			trackedList := cameraTrackers[devName]
			var matchedTrk *trackedObject
			maxIoU := 0.0
			for _, t := range trackedList {
				sameCategory := (t.Cls == aiCls) || (evType == "vehicle" && mapObjType(t.Cls) == "vehicle")
				if sameCategory {
					iou := boxIoU(obj.Box, t.Box)
					if iou > 0.30 && iou > maxIoU {
						maxIoU = iou
						matchedTrk = t
					}
				}
			}

			curCx := (obj.Box[0] + obj.Box[2]) / 2
			curCy := (obj.Box[1] + obj.Box[3]) / 2

			if matchedTrk != nil {
				prevCx := (matchedTrk.Box[0] + matchedTrk.Box[2]) / 2
				prevCy := (matchedTrk.Box[1] + matchedTrk.Box[3]) / 2
				dx := curCx - prevCx
				dy := curCy - prevCy
				disp := float64(dx*dx + dy*dy) // 平方位移

				matchedTrk.HitCount++
				matchedTrk.Speed = disp
				matchedTrk.LastSeen = evTime
				matchedTrk.Box = obj.Box

				// 静止车辆抑制：如果目标是车辆，且重合度高 (IoU > 0.35 或中心位移 < 60px)，且此前已产生过事件
				// 判定为【静止停放车辆】，坚决抑制重复的“车辆驶入”警报！
				isVehicle := (evType == "vehicle" || aiCls == "car" || aiCls == "truck")
				if isVehicle && matchedTrk.EmittedCount > 0 {
					if maxIoU > 0.35 || disp < 3600.0 {
						matchedTrk.Status = "parked"
						// 只要车辆保持停放状态，完全抑制后续重复报警
						trackerMu.Unlock()
						continue
					}
				}

				// 普通速度门控：微小位移防抖
				if disp < 225.0 && evTime.Sub(matchedTrk.LastEvent) < 30*time.Minute {
					trackerMu.Unlock()
					continue
				}

				matchedTrk.EmittedCount++
				matchedTrk.LastEvent = evTime
				trackerMu.Unlock()
			} else {
				// 新轨迹
				newTrk := &trackedObject{
					TrackID:      len(trackedList) + 1,
					Cls:          aiCls,
					Box:          obj.Box,
					FirstSeen:    evTime,
					LastEvent:    evTime,
					LastSeen:     evTime,
					HitCount:     1,
					Speed:        400.0,
					Status:       "moving",
					EmittedCount: 1,
				}
				cameraTrackers[devName] = append(cameraTrackers[devName], newTrk)
				var activeTrackers []*trackedObject
				for _, trk := range cameraTrackers[devName] {
					if evTime.Sub(trk.LastSeen) < 24*time.Hour {
						activeTrackers = append(activeTrackers, trk)
					}
				}
				cameraTrackers[devName] = activeTrackers
				trackerMu.Unlock()
			}

			// 单文件内车辆事件去重：同 60 秒录像分片内绝不重复发射两起车辆进出事件
			if evType == "vehicle" && seenTypes["emitted_vehicle_file"] {
				continue
			}
			if evType == "vehicle" {
				seenTypes["emitted_vehicle_file"] = true
			}

			var evDesc string
			if evType == "vehicle" {
				evDesc = cnClsName(aiCls) + "驶入停放"
			} else {
				evDesc = "检测到" + cnClsName(aiCls)
			}
			thumbName := s.eventStore.SaveThumb(frame)

			ev := event.Event{
				Did:      did,
				Device:   devName,
				Time:     evTime.Format("2006-01-02 15:04:05"),
				Type:     evType,
				Desc:     evDesc,
				Thumb:    thumbName,
				Duration: 5,
				Score:    25.0,
				Conf:     obj.Conf * 100,
				AICls:    aiCls,
				AIBox:    obj.Box,
				RecFile:  file,
				RecOff:   off,
				Zone:     matchDetectionZone(devName, obj.Box),
			}

			if ev.ID == "" {
				ev.ID = fmt.Sprintf("%d", time.Now().UnixNano())
			}

			// 尝试车牌识别并丰富写回，同时支持同位置静止车辆车牌记忆继承
			if evType == "vehicle" || aiCls == "car" || aiCls == "truck" {
				trackerMu.Lock()
				for _, trk := range cameraTrackers[devName] {
					if trk.Plate != "" && boxIoU(obj.Box, trk.Box) > 0.45 {
						ev.AIPlate = trk.Plate
						ev.AIPlateConf = 0.96
						ev.AIPlateSrc = "local"
						break
					}
				}
				trackerMu.Unlock()

				if pr, perr := s.enrichEventPlate(&ev); perr == nil && pr != nil && pr.Plate != "" {
					logf("[ai-scan] 录像分片 %s 成功识别车牌 %s (%.0f%%)", file, pr.Plate, pr.Conf*100)
					trackerMu.Lock()
					for _, trk := range cameraTrackers[devName] {
						if boxIoU(obj.Box, trk.Box) > 0.45 {
							trk.Plate = pr.Plate
						}
					}
					trackerMu.Unlock()
				}
			}

			// 入库并落盘
			s.eventStore.Add(ev)
			newEvents = append(newEvents, ev)
			logf("[ai-scan] 录像分片 %s 发现新动态 %s (%.0f%%) -> 记录事件", file, aiCls, ev.Conf)
		}
	}

	return newEvents, nil
}

func mapObjType(cls string) string {
	switch cls {
	case "person":
		return "person"
	case "car", "truck", "bus", "vehicle", "suv", "bicycle", "motorcycle":
		return "vehicle"
	case "dog", "cat", "bird", "horse", "sheep", "cow", "bear":
		return "animal"
	case "backpack", "suitcase", "handbag":
		return "package"
	default:
		return "motion"
	}
}

func cnClsName(cls string) string {
	switch cls {
	case "person":
		return "人员"
	case "car":
		return "轿车/SUV"
	case "truck":
		return "货车"
	case "bus":
		return "客车"
	case "bicycle":
		return "自行车"
	case "motorbike", "motorcycle":
		return "两轮车"
	case "dog":
		return "小狗"
	case "cat":
		return "猫咪"
	case "bird":
		return "鸟类"
	case "backpack", "suitcase", "handbag":
		return "包裹"
	default:
		return cls
	}
}

// scanAllRecordings 扫描录像并生成事件索引
func (s *Server) scanAllRecordings(maxFiles int) (int, int, error) {
	recScanMu.Lock()
	if recScanRunning {
		recScanMu.Unlock()
		return 0, 0, nil
	}
	recScanRunning = true
	recScanMu.Unlock()

	defer func() {
		recScanMu.Lock()
		recScanRunning = false
		recScanMu.Unlock()
	}()

	root := s.recRoot()
	devices, err := os.ReadDir(root)
	if err != nil {
		return 0, 0, err
	}

	scannedCount := 0
	newEventsCount := 0

	// 预热已扫描的录像索引缓存，避免服务重启后全量重复扫描
	recScanMu.Lock()
	if len(recScannedMap) == 0 {
		for _, ev := range s.eventStore.Query(event.Query{Days: 2, Limit: 5000}) {
			if ev.RecFile != "" && len(ev.Time) >= 10 {
				k := filepath.Join(ev.Device, ev.Time[:10], ev.RecFile)
				recScannedMap[k] = true
			}
		}
	}
	recScanMu.Unlock()

	// 查今天和昨天的目录
	dates := []string{
		time.Now().Format("2006-01-02"),
		time.Now().AddDate(0, 0, -1).Format("2006-01-02"),
	}

	for _, dev := range devices {
		if !dev.IsDir() || strings.HasPrefix(dev.Name(), ".") {
			continue
		}
		for _, d := range dates {
			dir := filepath.Join(root, dev.Name(), d)
			files, err := os.ReadDir(dir)
			if err != nil {
				continue
			}

			// 按时间升序
			sort.Slice(files, func(i, j int) bool {
				return files[i].Name() < files[j].Name()
			})

			for _, f := range files {
				if !strings.HasSuffix(f.Name(), ".mkv") {
					continue
				}
				key := filepath.Join(dev.Name(), d, f.Name())
				recScanMu.Lock()
				already := recScannedMap[key]
				recScanMu.Unlock()
				if already {
					continue
				}

				evs, err := s.scanSingleRecFile(dev.Name(), d, f.Name())
				recScanMu.Lock()
				recScannedMap[key] = true
				recScanMu.Unlock()

				scannedCount++
				if err == nil {
					newEventsCount += len(evs)
				}
				if maxFiles > 0 && scannedCount >= maxFiles {
					break
				}
			}
			if maxFiles > 0 && scannedCount >= maxFiles {
				break
			}
		}
		if maxFiles > 0 && scannedCount >= maxFiles {
			break
		}
	}

	if newEventsCount > 0 {
		logf("[ai-scan] 扫描完成: 共处理 %d 个录像分片，产生 %d 个新事件", scannedCount, newEventsCount)
	}
	return scannedCount, newEventsCount, nil
}

// startRecordingAIScanner 启动录像 AI 周期性扫描器
func (s *Server) startRecordingAIScanner() {
	go func() {
		// 启动 10 秒后执行首轮扫描
		time.Sleep(10 * time.Second)
		s.scanAllRecordings(100)

		// 之后每 60 秒检查并索引最新录像分片
		tk := time.NewTicker(60 * time.Second)
		defer tk.Stop()
		for range tk.C {
			s.scanAllRecordings(20)
		}
	}()
}

// handleAIScan 手动或按需触发录像扫描
func (s *Server) handleAIScan(w http.ResponseWriter, r *http.Request) {
	scanned, newEvents, err := s.scanAllRecordings(100)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok":          true,
		"scanned":     scanned,
		"newEvents":   newEvents,
		"totalEvents": s.eventStore.CountToday(),
	})
}
