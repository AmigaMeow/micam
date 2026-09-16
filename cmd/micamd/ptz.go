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
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/xiaomi/crypto"
	"github.com/AlexxIT/go2rtc/pkg/xiaomi/miss/cs2"
	"github.com/micam/micam/internal/xiaomi"
)

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
	// 短超时：云台是交互操作，不能让用户等
	_ = conn.SetDeadline(time.Now().Add(6 * time.Second))

	key, err := crypto.CalcSharedKey(q.Get("device_public"), q.Get("client_private"))
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("计算密钥失败: %w", err)
	}

	// 认证
	auth := fmt.Sprintf("{\"public_key\":\"%s\",\"sign\":\"%s\",\"uuid\":\"\",\"support_encrypt\":0}",
		q.Get("client_public"), q.Get("sign"))
	if err := conn.WriteCommand(ptzCmdAuthReq, []byte(auth)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("认证发送失败: %w", err)
	}
	if _, _, err := conn.ReadCommand(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("认证响应失败: %w", err)
	}

	return &ptzSession{conn: conn, key: key}, nil
}

// Close 关闭会话。
func (p *ptzSession) Close() {
	if p.conn != nil {
		p.conn.Close()
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
	case <-time.After(5 * time.Second):
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
	dev, err := s.ensureDevice(did)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}

	// 云台专用锁（不阻塞视频流）
	ptzMu.Lock()
	defer ptzMu.Unlock()

	// 注意：不共用 MJPEG 的锁！
	// MJPEG 是长连接，持锁不放会导致云台永远等不到。
	// 云台自己建短连接，用完即关。
	sess, err := s.openPTZ(dev)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	defer sess.Close()

	resp, err := sess.send(1) // operation=1 是查询
	if err != nil {
		writeErr(w, 504, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok":        resp.Ret == 0,
		"ret":       resp.Ret,
		"angle":     resp.Angle,
		"elevation": resp.Elevation,
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
		Did       string `json:"did"`
		Operation int    `json:"operation"`
		// 绝对定位（operation=13）用的目标坐标；其他 operation 忽略
		Angle     *int `json:"angle"`
		Elevation *int `json:"elevation"`
		// raw 原样透传（协议探测用，任何键值对）
		Raw map[string]any `json:"raw"`
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

	dev, err := s.ensureDevice(req.Did)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}

	// 云台专用锁（不阻塞视频流）
	ptzMu.Lock()
	defer ptzMu.Unlock()

	sess, err := s.openPTZ(dev)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	defer sess.Close()

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

	resp, err := sess.sendExtra(req.Operation, extra)
	if err != nil {
		writeErr(w, 504, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok":        resp.Ret == 0,
		"ret":       resp.Ret,
		"angle":     resp.Angle,
		"elevation": resp.Elevation,
	})
}