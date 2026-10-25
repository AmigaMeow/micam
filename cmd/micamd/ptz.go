package main

// 云台（PTZ）控制。
//
// 协议（已实测验证）：
//   payload = [4字节大端命令号 0x112] + JSON {"operation": N}
//   外层用 cmdEncoded(0x1001) 包装
//   用 X25519 共享密钥 ChaCha20 加密
//
// 响应：{"ret":0, "angle":50, "elevation":73}
//
// 注意：云台走 P2P 连接（和视频同一条通道），
// 所以同一时刻只能有一个操作。用 didLock 串行化。

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/xiaomi/crypto"
	"github.com/AlexxIT/go2rtc/pkg/xiaomi/miss/cs2"
	"github.com/micam/micam/internal/xiaomi"
)

// IsPTZCapable 判断摄像机型号是否配备物理云台电机。
//
// 硬件能力边界与安全防护（Issue #14/#15）：
// 1. 固定视角摄像机（小白卡片机/青春版、室外固定枪机、电池版相机、门铃、猫眼）物理上没有电机。
//    若向其发送 CS2 P2P 电机指令 (0x112)，不仅无法转动，还会导致低端机型唯一的 P2P 连接槽位被挂起，
//    造成米家 App 报错 36（设备超过最大同时连接数量）。
// 2. 只有带物理电机的机型（CW300、CW400、CW500、070a02、C200-C700等）才支持云台。
func IsPTZCapable(model string) bool {
	m := strings.ToLower(model)
	if m == "" {
		return false
	}

	// 1. 明确无物理电机的型号黑名单
	switch m {
	case "chuangmi.camera.021a04",  // 小白智能摄像机电池版
		"chuangmi.camera.026c02",  // 小白室外摄像机电池版
		"chuangmi.camera.046c01",  // 小白室外摄像机电池版 EC3
		"chuangmi.camera.049g01",  // 小白智能摄像机卡片机
		"chuangmi.camera.051a01",  // 小白智能摄像机室内卡片版
		"chuangmi.camera.069a01",  // 小白室外枪机 CW400枪机/EC4
		"chuangmi.camera.ipc009",  // 小白大众版 1080P
		"chuangmi.camera.ipc019",  // 小白大众版 1080P
		"chuangmi.camera.ipc020",
		"xiaomi.camera.c100",      // 小白/小米智能摄像机标准版 2K
		"xiaomi.camera.c101",
		"xiaomi.camera.c102",
		"xiaomi.camera.mcv01",
		"xiaomi.camera.mcv02",
		"xiaomi.camera.mcv03",
		"xiaomi.camera.mcv04",
		"xiaomi.camera.mcv05",
		"xiaomi.camera.mcv06",
		"xiaomi.camera.mcv07":
		return false
	}

	// 2. 类型特征过滤：非摄像机类（猫眼、门铃、喂食器）或带 card/battery
	if strings.Contains(m, ".cateye.") ||
		strings.Contains(m, ".doorbell.") ||
		strings.Contains(m, ".feeder.") ||
		strings.Contains(m, "card") ||
		strings.Contains(m, "battery") {
		return false
	}

	// 3. 明确配备物理云台的型号或系列
	// CW 系列户外球机 / 枪球一体机
	if strings.Contains(m, "cw300") ||
		strings.Contains(m, "cw400") ||
		strings.Contains(m, "cw500") ||
		strings.Contains(m, "hlmax") || // CW500 双摄版
		strings.Contains(m, "moc001") { // CW300
		return true
	}

	// C 系列室内云台机
	for _, p := range []string{"c200", "c300", "c400", "c500", "c700"} {
		if strings.Contains(m, p) {
			return true
		}
	}

	// 创米云台机系列
	switch m {
	case "chuangmi.camera.029a02", // 小米智能摄像机 云台版 2K
		"chuangmi.camera.060a02", // 小白智能摄像机云台版 2K
		"chuangmi.camera.070a02", // 小白智能摄像机云台版
		"chuangmi.camera.021a02",
		"chuangmi.camera.025a01",
		"chuangmi.camera.030a01",
		"chuangmi.camera.036a01",
		"chuangmi.camera.040a01",
		"chuangmi.camera.045a01",
		"chuangmi.camera.055a01",
		"chuangmi.camera.071a01",
		"chuangmi.camera.072a01",
		"chuangmi.camera.ipc013",
		"chuangmi.camera.ipc022":
		return true
	}

	if strings.Contains(m, "ptz") {
		return true
	}

	return false
}


const (
	ptzCmdAuthReq = 0x100
	ptzCmdMotor   = 0x112
	ptzCmdEncoded = 0x1001
)

// ptzResp 云台响应。
type ptzResp struct {
	Ret       int `json:"ret"`
	Angle     int `json:"angle"`
	Elevation int `json:"elevation"`
}

// ptzMu 云台专用锁：只串行化云台操作，不影响视频流。
var ptzMu sync.Mutex

// ptzSession 一次云台操作会话。
type ptzSession struct {
	conn *cs2.Conn
	key  []byte
	mu   sync.Mutex
}

// openPTZ 建立云台连接。
// ===== PTZ 连接复用 =====
//
// ⚠️ 为什么需要（2026-09-18 用户反馈「云台很卡」）
//
// 原实现每条命令都：
//   resolveSource -> cs2.Dial（完整 P2P 握手，最贵的一步）
//   -> CalcSharedKey -> 认证 -> 发一条命令 -> 关闭
//
// 连点 6 次方向键 = 6 次完整握手，每次几百毫秒到数秒。
// 前端虽然已经改成不断流，但后端这边还是「一条命令一次拨号」。
//
// 现在：复用连接，空闲 30 秒后自动关闭。
//   · 连点场景：1 次握手 + N 条命令  <- 快很多
//   · 空闲后正常释放，不会长期占用 P2P 槽位
//
// 注意：cs2 单设备只允许一路连接，所以这里必须串行管理，
// 不能ping-pong 建多路（那会导致 "pop buffer is full"）。

var (
	ptzCacheMu sync.Mutex
	ptzCache   = map[string]*ptzCached{}
)

type ptzCached struct {
	sess     *ptzSession
	lastUsed time.Time
	closed   bool
}

const ptzCacheIdle = 8 * time.Second

// ptzSend 发送命令，**任何错误都自动丢弃连接**。
//
// ⚠️ 为什么需要（2026-09-18 自己踩的坑）：
//
//	我加了连接缓存后，只在 handlePTZMove 里记得 dropPTZ，
//	handlePTZStatus 忘了写。结果：
//	  连接变坏（i/o timeout）-> 缓存里留着 ->
//	  后续所有命令都超时 -> 直到 30 秒空闲才回收。
//	  用户看到云台「突然完全不动了」。
//
// 所以改成：所有发送都走这个包装，出错就丢。
// 宁可多握一次手，也不能让坏连接卡住用户。
func (s *Server) ptzSend(dev *xiaomi.Device, sess *ptzSession, op int) (*ptzResp, error) {
	resp, err := sess.send(op)
	if err != nil {
		dropPTZ(dev.Did)
	}
	return resp, err
}

// acquirePTZ 取一条可用的 PTZ 连接（优先复用）。
func (s *Server) acquirePTZ(dev *xiaomi.Device) (*ptzSession, error) {
	ptzCacheMu.Lock()
	c := ptzCache[dev.Did]
	if c != nil && !c.closed && time.Since(c.lastUsed) < ptzCacheIdle {
		c.lastUsed = time.Now()
		sess := c.sess
		ptzCacheMu.Unlock()
		return sess, nil
	}
	// 过期/不存在：清掉旧的
	if c != nil {
		c.closed = true
		delete(ptzCache, dev.Did)
	}
	ptzCacheMu.Unlock()

	sess, err := s.openPTZ(dev)
	if err != nil {
		return nil, err
	}

	ptzCacheMu.Lock()
	ptzCache[dev.Did] = &ptzCached{sess: sess, lastUsed: time.Now()}
	ptzCacheMu.Unlock()

	// 安排空闲回收
	go s.reapPTZ(dev.Did, sess)
	return sess, nil
}

// reapPTZ 空闲超时后关闭这条连接。
func (s *Server) reapPTZ(did string, sess *ptzSession) {
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	for range t.C {
		ptzCacheMu.Lock()
		c := ptzCache[did]
		if c == nil || c.sess != sess || c.closed {
			ptzCacheMu.Unlock()
			return
		}
		idle := time.Since(c.lastUsed)
		if idle >= ptzCacheIdle {
			c.closed = true
			delete(ptzCache, did)
			ptzCacheMu.Unlock()
			logf("[ptz] %s 空闲 %v，释放云台连接", did, idle.Round(time.Second))
			sess.Close()
			return
		}
		ptzCacheMu.Unlock()
	}
}

// dropPTZ 主动丢弃某设备的 PTZ 连接（出错时调用）。
func dropPTZ(did string) {
	ptzCacheMu.Lock()
	c := ptzCache[did]
	if c != nil {
		c.closed = true
		delete(ptzCache, did)
	}
	ptzCacheMu.Unlock()
	if c != nil {
		c.sess.Close()
	}
}

func (s *Server) openPTZ(dev *xiaomi.Device) (*ptzSession, error) {
	src, err := s.resolveSource(dev)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(src)
	if err != nil {
		return nil, fmt.Errorf("源地址非法: %w", err)
	}
	q := u.Query()

	conn, err := cs2.Dial(u.Host, q.Get("transport"))
	if err != nil {
		return nil, fmt.Errorf("连接摄像头失败（可能被视频占用）: %w", err)
	}
	// 超时：云台是交互操作，不能让用户等太久；
	// 但 6 秒对"预览正在抢连接"的场景太紧 —— 摄像头握手本身就可能
	// 花掉 1~2 秒，中途被抢就只剩 EOF。放宽到 10 秒，配合上层重试。
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	closeConn := func() {
		_, _ = conn.Write([]byte{0xF1, 0xF1, 0, 0})
		_ = conn.Close()
	}

	key, err := crypto.CalcSharedKey(q.Get("device_public"), q.Get("client_private"))
	if err != nil {
		closeConn()
		return nil, fmt.Errorf("计算密钥失败: %w", err)
	}

	// 认证
	auth := fmt.Sprintf("{\"public_key\":\"%s\",\"sign\":\"%s\",\"uuid\":\"\",\"support_encrypt\":0}",
		q.Get("client_public"), q.Get("sign"))
	if err := conn.WriteCommand(ptzCmdAuthReq, []byte(auth)); err != nil {
		closeConn()
		return nil, fmt.Errorf("认证发送失败: %w", err)
	}
	if _, _, err := conn.ReadCommand(); err != nil {
		closeConn()
		return nil, fmt.Errorf("认证响应失败: %w", err)
	}

	return &ptzSession{conn: conn, key: key}, nil
}

// Close 关闭会话并优雅释放固件连接槽位。
func (p *ptzSession) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conn != nil {
		// 发送 CS2 msgClose (0xF1, 0xF1, 0x00, 0x00) 优雅断开通知固件
		// 避免摄像头固件因等待心跳超时挂起会话槽位，引发米家 App 报错 36（设备超过最大同时连接数量）
		_, _ = p.conn.Write([]byte{0xF1, 0xF1, 0, 0})
		_ = p.conn.Close()
		p.conn = nil
	}
}

// send 发送云台命令并读响应。
func (p *ptzSession) send(op int) (*ptzResp, error) {
	return p.sendExtra(op, nil)
}

// sendExtra 发送命令，可带额外参数（如绝对定位的目标坐标）。
//
// 绝对定位（Mac 侧调研：operation=13）如果只传 operation 是不完整的，
// 必须带目标角度/仰角，所以这里支持透传。
func (p *ptzSession) sendExtra(op int, extra map[string]any) (*ptzResp, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	payload := map[string]any{"operation": op}
	for k, v := range extra {
		payload[k] = v
	}
	body, _ := json.Marshal(payload)
	buf := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(buf, ptzCmdMotor)
	copy(buf[4:], body)

	enc, err := crypto.Encode(buf, p.key)
	if err != nil {
		return nil, fmt.Errorf("加密失败: %w", err)
	}
	_ = p.conn.SetDeadline(time.Now().Add(8 * time.Second))
	if err := p.conn.WriteCommand(ptzCmdEncoded, enc); err != nil {
		return nil, fmt.Errorf("发送失败: %w", err)
	}

	type result struct {
		r   *ptzResp
		err error
	}
	ch := make(chan result, 1)
	go func() {
		_, d, err := p.conn.ReadCommand()
		if err != nil {
			ch <- result{nil, err}
			return
		}
		plain, err := crypto.Decode(d, p.key)
		if err != nil {
			ch <- result{nil, err}
			return
		}
		var r ptzResp
		if len(plain) > 4 {
			_ = json.Unmarshal(plain[4:], &r)
		}
		ch <- result{&r, nil}
	}()

	select {
	case res := <-ch:
		return res.r, res.err
	case <-time.After(8 * time.Second):
		return nil, fmt.Errorf("等待响应超时")
	}
}

// ---- HTTP API ----

// handlePTZStatus 查询云台当前角度。
// GET /api/ptz/status?did=xxx
func (s *Server) handlePTZStatus(w http.ResponseWriter, r *http.Request) {
	did := r.URL.Query().Get("did")
	if did == "" {
		writeErr(w, 400, "缺少 did")
		return
	}
	if dev, err := s.ensureDevice(did); err == nil && !IsPTZCapable(dev.Model) {
		writeJSON(w, 200, map[string]any{
			"ok":      true,
			"ret":     0,
			"hasPTZ":  false,
			"isFixed": true,
			"hasPos":  false,
			"source":  "fixed",
			"msg":     "固定视角摄像机，未配备物理云台",
		})
		return
	}
	// ===== 优先用 miIO 读位置（2026-10-13 实测修正）=====
	//
	// ⚠️ 重要教训：cs2 的 operation=1 在这台设备上【不是查询】——
	//    实测连续发 op=1 会让 angle 每次 +2~3 度（镜头持续转动）。
	//    开发文档里写的"1=查询位置"与实测不符。
	//
	// 真正的查询走 miIO 本地读 siid=6 piid=12（readPTZState），
	// 只读属性、不碰电机，13~30ms 返回。
	// ⚠️ 坐标系注意：miIO(siid=6 piid=12) 返回的是
	//    {"x":1326,"y":49} —— 步进电机计数（可到几千），
	//    而 cs2 的 operation=1 返回的是 {"angle":0..360,"elevation":0..180} 角度。
	//    两者【不是同一套坐标】，不能混用！
	//
	//    前端显示的"水平/俯仰 x°"用的是 cs2 角度，
	//    所以这里继续用 cs2 拿角度；miIO 作为附加信息一并返回，
	//    供需要原始步进值的场景使用。
	if st := s.readPTZState(did); st != nil && st.HasPos {
		// 用 miIO 回答这次查询 ——【不碰电机】。
		//
		// 为什么必须这样：cs2 的 operation=1 实测会让 angle 每次 +2~3 度，
		// 它不是查询而是电机命令。前端每查一次位置，镜头就转一下。
		//
		// 坐标系：miIO 给的是步进计数（x 可到几千），
		//        cs2 给的是角度（angle 0~360 / elevation 0~180）。
		//        两者单位不同，这里如实分别返回，不互相冒充。
		writeJSON(w, 200, map[string]any{
			"ok":      true,
			"ret":     0,
			"atLimit": false,
			// 角度坐标（本接口暂不提供，用步进值 + 提示）
			"angle":     nil,
			"elevation": nil,
			// 步进坐标（miIO，权威且不驱动电机）
			"x":      st.X,
			"y":      st.Y,
			"hasPos": true,
			"source": "miio",
		})
		return
	} else if st != nil && st.Error != "" {
		logf("[ptz] %s miIO 读位置失败（回退基准缓存，不触碰电机）: %s", did, st.Error)
	}

	// ⚠️ 严禁在此调用 ptzSend(dev, sess, 1)！
	// operation=1 是硬件自检校准(Calibration)，会导致电机全行程前后左右自检乱跑！
	if s.poses != nil {
		if p, ok := s.poses.get(did); ok {
			writeJSON(w, 200, map[string]any{
				"ok":        true,
				"ret":       0,
				"atLimit":   false,
				"angle":     p.Angle,
				"elevation": p.Elevation,
				"hasPos":    p.HasPTZ,
				"source":    "cache",
			})
			return
		}
	}
	writeJSON(w, 200, map[string]any{
		"ok":      true,
		"ret":     0,
		"atLimit": false,
		"hasPos":  false,
		"source":  "none",
	})
}

// handlePTZMove 执行云台动作。
// POST /api/ptz/move  {"did":"xxx","operation":N}
func (s *Server) handlePTZMove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 POST")
		return
	}
	var req struct {
		Did       string         `json:"did"`
		Operation int            `json:"operation"`
		Lens      int            `json:"lens"`
		Angle     *int           `json:"angle"`
		Elevation *int           `json:"elevation"`
		Raw       map[string]any `json:"raw"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "参数错误")
		return
	}
	if req.Did == "" {
		writeErr(w, 400, "缺少 did")
		return
	}
	if req.Operation < 1 || req.Operation > 20 {
		writeErr(w, 400, "operation 超出范围（1-20）")
		return
	}
	if req.Operation == 5 {
		writeErr(w, 400, "operation 5 (硬件巡航/自动校准) 已禁用，单步微调请使用 1(右)/2(左)/3(上)/4(下)")
		return
	}

	dev, err := s.ensureDevice(req.Did)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}

	// 1. 物理云台能力检查：固定视角摄像机直接拦截，绝不向摄像头建立无效 CS2 连接
	if !IsPTZCapable(dev.Model) {
		writeErr(w, 400, "该设备型号（"+dev.Model+"）为固定视角摄像机，未配备物理云台电机")
		return
	}

	// 2. 双摄机型副摄检查（CW500 镜头2为固定广角，镜头1为可旋转球机）
	if dev.Model == "isa.camera.hlmax" && req.Lens == 2 {
		writeErr(w, 400, "CW500 副摄（镜头2）为固定广角镜头，仅主摄（镜头1）配备物理云台")
		return
	}

	// 云台专用锁（不阻塞视频流）
	ptzMu.Lock()
	defer ptzMu.Unlock()

	var sess *ptzSession
	for attempt := 1; attempt <= 2; attempt++ {
		sess, err = s.acquirePTZ(dev)
		if err == nil {
			break
		}
		if isThrottleError(err) || s.throttleSilenced() {
			break
		}
		if attempt < 2 && isPTZRetryable(err) {
			dropPTZ(dev.Did)
			time.Sleep(time.Duration(attempt*300) * time.Millisecond)
			logf("[ptz] %s 建连重试第 %d 次: %v", dev.Did, attempt, err)
			continue
		}
		break
	}
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}

	extra := map[string]any{}
	if req.Angle != nil {
		extra["angle"] = *req.Angle
	}
	if req.Elevation != nil {
		extra["elevation"] = *req.Elevation
	}
	for k, v := range req.Raw {
		if k == "operation" {
			continue
		}
		extra[k] = v
	}
	if len(extra) == 0 {
		extra = nil
	}

	// ===== 自动重试（2026-10-13）=====
	//
	// 现象：用户报"有时候失败，有时候正常"，错误是 cs2: EOF。
	//
	// 原因：摄像头同一时刻只允许一条 PPCS 连接。用户开着实时预览时，
	//      预览占着那条连接，云台新建的连接会被摄像头单方面断掉（EOF）。
	//      预览重连的间隙里云台就能成功 —— 所以表现为时好时坏。
	//
	// 对策：EOF/超时这类"连接被抢"的错误，丢掉连接重建后再试。
	//      最多 3 次，每次退避递增，总耗时控制在 2 秒内，
	//      避免用户在界面上感觉到明显卡顿。
	logf("[ptz] >>> MOVE REQ did=%s op=%d extra=%v", req.Did, req.Operation, extra)
	resp, err := sess.sendExtra(req.Operation, extra)
	if resp != nil {
		logf("[ptz] <<< MOVE RESP did=%s op=%d ret=%d angle=%d elev=%d err=%v", req.Did, req.Operation, resp.Ret, resp.Angle, resp.Elevation, err)
	} else {
		logf("[ptz] <<< MOVE RESP did=%s op=%d err=%v", req.Did, req.Operation, err)
	}
	if err != nil && isPTZRetryable(err) {
		dropPTZ(dev.Did)
		time.Sleep(300 * time.Millisecond)
		logf("[ptz] %s 发送失败，尝试重建连接重发: %v", dev.Did, err)
		if sess2, err2 := s.openPTZ(dev); err2 == nil {
			resp2, err2 := sess2.sendExtra(req.Operation, extra)
			if err2 == nil {
				err = nil
				resp = resp2
				ptzCacheMu.Lock()
				ptzCache[dev.Did] = &ptzCached{sess: sess2, lastUsed: time.Now()}
				ptzCacheMu.Unlock()
				go s.reapPTZ(dev.Did, sess2)
			} else {
				sess2.Close()
			}
		}
	}
	if err != nil {
		dropPTZ(dev.Did)
		if isThrottleError(err) {
			s.markThrottled()
			left := s.throttleSilenceLeft()
			writeErr(w, 429, fmt.Sprintf("云台方向控制需小米云下发动态P2P凭据，当前账号处于小米云限流冷却期 (throttle deny)。系统已自动进入静默保护（约剩余 %v），冷却后将自动恢复连线，视频与录像不受影响", left.Round(time.Minute)))
			return
		}
		writeErr(w, 504, err.Error())
		return
	}
	// ⚠️ 成功码因机型而异（2026-10-13 实测）：
	//   CW300 正常 ret = 0
	//   CW500 正常 ret = 5     ← 之前只认 0，导致成功的命令被判失败
	//   到行程极限 ret = -1001（不是失败，是正常边界）
	ok := resp.Ret == 0 || resp.Ret == 5
	atLimit := resp.Ret == -1001
	writeJSON(w, 200, map[string]any{
		"ok":        ok,
		"ret":       resp.Ret,
		"atLimit":   atLimit,
		"angle":     resp.Angle,
		"elevation": resp.Elevation,
	})
}

// isPTZRetryable 判断云台错误是否值得重试。
//
// 只重试「连接被抢 / 网络抖动」类错误 —— 凭据类错误重试没有意义，
// 反而会加重小米云的限流。
func isPTZRetryable(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	for _, k := range []string{
		"EOF",     // 连接被摄像头断开（最常见：被视频预览抢走）
		"timeout", // 读写超时
		"i/o timeout",
		"connection reset",
		"broken pipe",
		"connection refused", // 端口瞬时被预览占用拒绝
		"reset by peer",
		"等待响应超时",
		"超时",
	} {
		if strings.Contains(m, k) {
			return true
		}
	}
	return false
}
