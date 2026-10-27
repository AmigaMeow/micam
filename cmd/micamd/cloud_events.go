package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/micam/micam/internal/event"
	"github.com/micam/micam/internal/xiaomi"
)

// 云端硬件 AI 事件同步器。
//
// 小米智能摄像机（CW300, CW500, C系列等）内置 NPU 硬件算法，
// 会在侦测到人员、车辆、宠物、声音、画面变化时向小米云端实时上报硬件级事件。
// 本模块定时同步小米云端硬件事件，抓取高清抓拍快照并解密落库，
// 联动本地录像文件与外部 AI 大模型分析，解决本地单帧抓拍误报或漏检的问题。

type cloudEventUnit struct {
	EventType       string  `json:"eventType"`
	CreateTime      int64   `json:"createTime"`
	Duration        float64 `json:"duration"`
	Channel         string  `json:"channel"`
	ImgStoreID      string  `json:"imgStoreId"`
	VideoStoreID    string  `json:"videoStoreId"`
	IsAlarm         bool    `json:"isAlarm"`
	EventTypeDetail []struct {
		Event      string `json:"event"`
		ImgStoreID string `json:"imgStoreId"`
	} `json:"eventTypeDetail"`
}

type cloudEventListResp struct {
	Code        int    `json:"code"`
	Description string `json:"description"`
	Data        struct {
		Units      []cloudEventUnit `json:"thirdPartPlayUnits"`
		NextTime   int64            `json:"nextTime"`
		IsContinue bool             `json:"isContinue"`
	} `json:"data"`
}

type cloudEventSyncer struct {
	srv      *Server
	mu       sync.Mutex
	running  bool
	stop     chan struct{}
	syncing  bool
	lastSync time.Time
}

var cloudSyncer = &cloudEventSyncer{
	stop: make(chan struct{}),
}

func signSmartCameraGet(ssecurity []byte, path, data string) (url.Values, []byte, error) {
	nonce := xiaomi.Nonce()
	sn := xiaomi.SignedNonce(ssecurity, nonce)
	form := url.Values{"data": {data}}

	hashStr := "GET&" + path + "&data=" + form.Get("data") + "&" + base64.StdEncoding.EncodeToString(sn)
	h := sha1.New()
	h.Write([]byte(hashStr))
	form.Set("rc4_hash__", base64.StdEncoding.EncodeToString(h.Sum(nil)))

	for _, k := range []string{"data", "rc4_hash__"} {
		ct, err := xiaomi.RC4Crypt(sn, []byte(form.Get(k)))
		if err != nil {
			return nil, nil, err
		}
		form.Set(k, base64.StdEncoding.EncodeToString(ct))
	}

	sigStr := "GET&" + path + "&data=" + form.Get("data") + "&rc4_hash__=" + form.Get("rc4_hash__") + "&" + base64.StdEncoding.EncodeToString(sn)
	h2 := sha1.New()
	h2.Write([]byte(sigStr))
	form.Set("signature", base64.StdEncoding.EncodeToString(h2.Sum(nil)))
	form.Set("_nonce", base64.StdEncoding.EncodeToString(nonce))

	return form, sn, nil
}

func decryptCloudSnapshot(ssecurity, iv, encBytes []byte) ([]byte, error) {
	if len(encBytes) == 0 || len(encBytes)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("invalid encrypted length %d", len(encBytes))
	}
	block, err := aes.NewCipher(ssecurity)
	if err != nil {
		return nil, err
	}
	mode := cipher.NewCBCDecrypter(block, iv)
	dec := make([]byte, len(encBytes))
	mode.CryptBlocks(dec, encBytes)

	// PKCS7 unpad
	padLen := int(dec[len(dec)-1])
	if padLen > 0 && padLen <= aes.BlockSize {
		dec = dec[:len(dec)-padLen]
	}
	return dec, nil
}

func (s *Server) fetchCloudSnapshot(sess *xiaomi.Session, did, videoStoreID, imgStoreID string) []byte {
	if sess == nil || did == "" || imgStoreID == "" {
		return nil
	}
	iv := make([]byte, 16)
	if _, err := rand.Read(iv); err != nil {
		return nil
	}
	segIvB64 := base64.StdEncoding.EncodeToString(iv)

	imgPath := "/miot/camera/app/v1/img"
	imgPayload := map[string]any{
		"did":       did,
		"fileId":    videoStoreID,
		"stoId":     imgStoreID,
		"segmentIv": segIvB64,
	}
	raw, err := json.Marshal(imgPayload)
	if err != nil {
		return nil
	}

	form, _, err := signSmartCameraGet(sess.SsecurityBytes(), imgPath, string(raw))
	if err != nil {
		return nil
	}
	form.Set("yetAnotherServiceToken", sess.ServiceToken)

	reqURL := "https://processor.smartcamera.api.io.mi.com" + imgPath + "?" + form.Encode()
	req, err := http.NewRequestWithContext(context.Background(), "GET", reqURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", sess.UserAgent())
	req.Header.Set("MIOT-ENCRYPT-ALGORITHM", "ENCRYPT-RC4")
	req.Header.Set("Cookie", fmt.Sprintf("userId=%s; serviceToken=%s; deviceId=%s",
		sess.UserID, sess.ServiceToken, sess.DeviceID()))

	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil
	}
	encBytes, err := io.ReadAll(resp.Body)
	if err != nil || len(encBytes) == 0 {
		return nil
	}

	dec, err := decryptCloudSnapshot(sess.SsecurityBytes(), iv, encBytes)
	if err != nil {
		return nil
	}
	if len(dec) >= 2 && dec[0] == 0xFF && dec[1] == 0xD8 {
		return dec
	}
	return nil
}

func (s *Server) sessionForDid(did string) *xiaomi.Session {
	if s == nil || s.store == nil {
		return nil
	}
	sessions := s.store.AllSessions()
	if len(sessions) == 0 {
		return nil
	}
	if len(sessions) == 1 {
		return sessions[0]
	}
	if sess := s.store.SessionForDevice(did); sess != nil {
		return sess
	}
	return sessions[0]
}

// syncCameraCloudEvents 拉取单台相机的云端硬件 AI 事件
func (s *Server) syncCameraCloudEvents(dev *xiaomi.Device, beginTime time.Time) (int, error) {
	if dev == nil || dev.Did == "" {
		return 0, nil
	}
	sess := s.sessionForDid(dev.Did)
	if sess == nil {
		return 0, fmt.Errorf("未找到设备 %s 的有效账号会话", dev.Did)
	}

	path := "/common/app/get/eventlist"
	payload := map[string]any{
		"did":       dev.Did,
		"model":     dev.Model,
		"beginTime": beginTime.UnixMilli(),
		"endTime":   time.Now().UnixMilli(),
		"limit":     50,
		"eventType": "Default",
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}

	form, sn, err := signSmartCameraGet(sess.SsecurityBytes(), path, string(raw))
	if err != nil {
		return 0, err
	}

	reqURL := "https://business.smartcamera.api.io.mi.com" + path + "?" + form.Encode()
	req, err := http.NewRequestWithContext(context.Background(), "GET", reqURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", sess.UserAgent())
	req.Header.Set("MIOT-ENCRYPT-ALGORITHM", "ENCRYPT-RC4")
	req.Header.Set("Cookie", fmt.Sprintf("userId=%s; serviceToken=%s; deviceId=%s",
		sess.UserID, sess.ServiceToken, sess.DeviceID()))

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}

	dec, err := xiaomi.DecryptResponse(sn, body)
	if err != nil {
		return 0, fmt.Errorf("解密云端事件响应失败: %w", err)
	}

	var res cloudEventListResp
	if err := json.Unmarshal(dec, &res); err != nil {
		return 0, fmt.Errorf("解析云端事件 JSON 失败: %w", err)
	}

	imported := 0
	for i := len(res.Data.Units) - 1; i >= 0; i-- {
		u := res.Data.Units[i]
		if u.EventType == "IgnoreEvent" {
			continue
		}

		t := time.UnixMilli(u.CreateTime)
		evTime := t.Format("2006-01-02 15:04:05")
		day := evTime[:10]
		evID := fmt.Sprintf("cloud_%s_%d", dev.Did, u.CreateTime)

		if s.eventStore.HasEventID(evID, day) {
			continue
		}

		evType := event.TypeMotion
		evDesc := "检测到画面变化"
		aiCls := ""
		conf := 90.0

		// 解析事件类型 (支持复合类型 如 PeopleMotion:IgnoreEvent)
		lowerType := strings.ToLower(u.EventType)
		if strings.Contains(lowerType, "people") || strings.Contains(lowerType, "person") || strings.Contains(lowerType, "human") {
			evType = "person"
			evDesc = "检测到人员活动"
			aiCls = "person"
			conf = 96.0
		} else if strings.Contains(lowerType, "vehicle") || strings.Contains(lowerType, "car") {
			evType = "vehicle"
			evDesc = "检测到车辆活动"
			aiCls = "car"
			conf = 95.0
		} else if strings.Contains(lowerType, "pet") || strings.Contains(lowerType, "animal") {
			evType = "animal"
			evDesc = "检测到宠物活动"
			aiCls = "pet"
			conf = 93.0
		} else if strings.Contains(lowerType, "baby") || strings.Contains(lowerType, "cry") {
			evType = "motion"
			evDesc = "检测到声音异响"
			aiCls = "sound"
			conf = 90.0
		}

		// 镜头通道标记
		chanSuffix := ""
		if u.Channel == "10" {
			chanSuffix = " (副摄镜头)"
		}

		// 抓取并缓存快照
		thumbName := ""
		imgStoreID := u.ImgStoreID
		if imgStoreID == "" && len(u.EventTypeDetail) > 0 {
			for _, d := range u.EventTypeDetail {
				if d.ImgStoreID != "" {
					imgStoreID = d.ImgStoreID
					break
				}
			}
		}

		if imgStoreID != "" {
			if imgData := s.fetchCloudSnapshot(sess, dev.Did, u.VideoStoreID, imgStoreID); len(imgData) > 0 {
				thumbName = s.eventStore.SaveThumb(imgData)
			}
		}

		dur := int(math.Round(u.Duration))
		if dur <= 0 {
			dur = 10
		}

		devName := dev.Name
		if m := s.meta.Get(dev.Did); m.Alias != "" {
			devName = m.Alias
		}

		// 关联本地录像分片
		recFile, recOff := s.findEventRecording(dev.Name, dev.Did, evTime)

		ev := event.Event{
			ID:       evID,
			Time:     evTime,
			Did:      dev.Did,
			Device:   devName,
			Type:     evType,
			Desc:     evDesc + chanSuffix,
			Thumb:    thumbName,
			Duration: dur,
			Score:    conf,
			Conf:     conf,
			AICls:    aiCls,
			RecFile:  recFile,
			RecOff:   recOff,
		}

		s.eventStore.Add(ev)
		imported++

		// 如果事件是最近 3 分钟内发生的，且开启了动态录像，触发本地录像启动
		if time.Since(t) < 3*time.Minute && s.cfg.RecordEnabled && (s.cfg.RecordMode == "motion" || s.cfg.RecordMode == "ai_event") {
			s.triggerMotionRecording(dev, true)
		}
	}

	return imported, nil
}

// SyncAllCloudEvents 同步所有摄像头的云端硬件事件
func (s *Server) SyncAllCloudEvents(beginTime time.Time) (int, error) {
	cams := s.allCameras()
	if len(cams) == 0 {
		return 0, nil
	}

	cloudSyncer.mu.Lock()
	if cloudSyncer.syncing {
		cloudSyncer.mu.Unlock()
		return 0, nil
	}
	cloudSyncer.syncing = true
	cloudSyncer.mu.Unlock()

	defer func() {
		cloudSyncer.mu.Lock()
		cloudSyncer.syncing = false
		cloudSyncer.lastSync = time.Now()
		cloudSyncer.mu.Unlock()
	}()

	totalImported := 0
	for _, cam := range cams {
		if !cam.IsCamera() {
			continue
		}
		n, err := s.syncCameraCloudEvents(cam, beginTime)
		if err != nil {
			logf("[cloud-event] 同步摄像头 %s (%s) 事件失败: %v", cam.Name, cam.Did, err)
			continue
		}
		if n > 0 {
			logf("[cloud-event] 成功同步摄像头 %s (%s) %d 条硬件 AI 事件", cam.Name, cam.Did, n)
			totalImported += n
		}
	}
	return totalImported, nil
}

// startCloudEventSyncer 启动后台云端硬件事件轮询守护协程
func (s *Server) startCloudEventSyncer() {
	cloudSyncer.mu.Lock()
	if cloudSyncer.running {
		cloudSyncer.mu.Unlock()
		return
	}
	cloudSyncer.srv = s
	cloudSyncer.running = true
	cloudSyncer.mu.Unlock()

	go func() {
		// 启动后等待 15 秒（让会话与设备列表就绪），回溯同步过去 24 小时的事件
		time.Sleep(15 * time.Second)
		logf("[cloud-event] 启动云端硬件事件同步服务，全量回溯过去 24 小时事件...")
		if n, err := s.SyncAllCloudEvents(time.Now().Add(-24 * time.Hour)); err == nil && n > 0 {
			logf("[cloud-event] 初始回溯完成，成功载入 %d 条硬件 AI 事件", n)
		}

		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-cloudSyncer.stop:
				return
			case <-ticker.C:
				// 每 30 秒轮询最近 5 分钟的事件
				_, _ = s.SyncAllCloudEvents(time.Now().Add(-5 * time.Minute))
			}
		}
	}()
}

// handleEventsSync POST /api/events/sync 手动触发全量同步过去 24 小时事件
func (s *Server) handleEventsSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	hours := 24
	if hStr := r.URL.Query().Get("hours"); hStr != "" {
		if h, err := time.ParseDuration(hStr + "h"); err == nil && h > 0 && h <= 168*time.Hour {
			hours = int(h.Hours())
		}
	}
	beginTime := time.Now().Add(-time.Duration(hours) * time.Hour)
	n, err := s.SyncAllCloudEvents(beginTime)
	if err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "synced": n, "hours": hours})
}
