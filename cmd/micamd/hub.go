package main

// 摄像头连接管理。
//
// ===== 核心约束（踩了很多坑才确立）=====
//
// 1. cs2 协议同一设备只允许【一路】连接，重复 Dial 会 "pop buffer is full"
// 2. Producer 是推模式：consumer 必须持续读，读慢了会撑爆内部缓冲
// 3. **客户端断开后必须主动释放**，否则 consumer.WriteTo 永不返回，
//    连接就永远占着，下次请求必然失败
// 4. 同一设备的请求必须串行（前一个完全释放后才能建下一个）
//
// ===== 设计 =====
//
//   每台设备一把锁（serialize 请求）
//   Dial -> 建 consumer -> 读 -> Stop consumer -> Stop producer -> 解锁
//
//   不用全局锁：全局锁在并发场景下容易双重解锁（panic: unlock of unlocked mutex）

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	gxiaomi "github.com/AlexxIT/go2rtc/pkg/xiaomi"
	"github.com/micam/micam/internal/stream"
	"github.com/micam/micam/internal/xiaomi"
)

// camConn 一台摄像头的连接。
type camConn struct {
	did  string
	prod core.Producer
}

var (
	connMu    sync.Mutex
	conns     = map[string]*camConn{}
	connLocks = map[string]*sync.Mutex{}
)

// didLock 取该 did 的串行锁。
func didLock(did string) *sync.Mutex {
	connMu.Lock()
	defer connMu.Unlock()
	l, ok := connLocks[did]
	if !ok {
		l = &sync.Mutex{}
		connLocks[did] = l
	}
	return l
}

// connTimeout 连接超时：超过这个时间没数据就视为坏连接。
const connTimeout = 12 * time.Second

// useConn 独占使用某设备的连接。
//
// 返回的 release 必须在用完时调用（建议 defer）。
// release 会停止 consumer 和 producer，并让出串行锁。
func (s *Server) useConn(dev *xiaomi.Device) (*camConn, *mpegtsConsumer, func(), error) {
	lock := didLock(dev.Did)
	lock.Lock()

	// 关掉可能残留的旧连接
	//
	// 关键：卡死的连接必须强制清理，否则新连接拿不到数据
	connMu.Lock()
	if old, ok := conns[dev.Did]; ok {
		logf("[%s] 发现残留连接，强制清理", dev.Did)
		_ = old.prod.Stop()
		delete(conns, dev.Did)
		// 等摄像头端释放会话
		connMu.Unlock()
		time.Sleep(1500 * time.Millisecond)
		connMu.Lock()
	}
	connMu.Unlock()

	src, err := s.resolveSource(dev)
	if err != nil {
		lock.Unlock()
		return nil, nil, nil, err
	}
	prod, err := gxiaomi.Dial(src)

	// 凭据失效时的处理策略：
	//   1. 先清缓存，重新申请
	//   2. 刷新失败（如小米云限流）时，如果还有旧凭据就继续用
	//   3. 记录冷却时间，避免频繁刷新触发限流
	if err != nil && isAuthError(err) {
		logf("[%s] 凭据失效（%v），尝试刷新", dev.Did, err)

		if !s.canRefresh(dev.Did) {
			logf("[%s] 刷新冷却中，跳过（上次已尝试）", dev.Did)
		} else {
			s.markRefresh(dev.Did)

			// 关键：先申请新凭据，成功才替换。
			// 不要先删缓存 —— 否则申请失败（如限流）会导致彻底不可用。
			newSrc, e2 := s.requestFreshSource(dev)
			if e2 != nil {
				logf("[%s] 刷新失败（保留旧凭据）: %v", dev.Did, e2)
			} else if prod2, e3 := gxiaomi.Dial(newSrc); e3 != nil {
				logf("[%s] 新凭据连接失败（保留旧凭据）: %v", dev.Did, e3)
			} else {
				prod, err = prod2, nil
				s.store.SaveSource(dev.Did, newSrc)
				logf("[%s] 凭据刷新成功", dev.Did)
			}
		}
	}
	if err != nil {
		lock.Unlock()
		return nil, nil, nil, err
	}

	c := &camConn{did: dev.Did, prod: prod}
	connMu.Lock()
	conns[dev.Did] = c
	connMu.Unlock()

	// 建 consumer
	mc, err := newConsumerFor(prod)
	if err != nil {
		_ = prod.Stop()
		connMu.Lock()
		delete(conns, dev.Did)
		connMu.Unlock()
		lock.Unlock()
		return nil, nil, nil, err
	}

	logf("已连接摄像头: %s (%s)", dev.Name, dev.Did)

	// 启动 producer 读循环（加 recover，避免 panic 杀进程）
	prodDone := make(chan struct{})
	go func() {
		defer close(prodDone)
		defer recoverPanic("producer " + dev.Did)
		_ = prod.Start()
	}()

	var once sync.Once
	release := func() {
		once.Do(func() {
			// 1) 停 consumer -> WriteTo 返回
			mc.stop()

			// 2) 停 producer -> 释放摄像头会话
			_ = prod.Stop()

			// 3) 等读循环真正退出
			select {
			case <-prodDone:
			case <-time.After(2 * time.Second):
			}

			// 4) 清理注册表
			connMu.Lock()
			if conns[dev.Did] == c {
				delete(conns, dev.Did)
			}
			connMu.Unlock()

			// 5) 让摄像头端有时间清理会话
			time.Sleep(500 * time.Millisecond)

			logf("[%s] 连接已释放", dev.Did)
			lock.Unlock()
		})
	}

	return c, mc, release, nil
}

// 凭据刷新冷却（避免小米云限流）
var (
	refreshMu   sync.Mutex
	refreshLast = map[string]time.Time{}
)

// requestFreshSource 向小米云申请新凭据（不动缓存）。
func (s *Server) requestFreshSource(dev *xiaomi.Device) (string, error) {
	sessions := s.store.AllSessions()
	if len(sessions) == 0 {
		return "", fmt.Errorf("未登录")
	}
	src, err := stream.BuildSource(sessions[0], dev, "cn")
	if err != nil {
		return "", err
	}
	return src, nil
}

// canRefresh 判断是否可以刷新。
//
// 冷却 30 分钟：小米云的 miss_get_vendor 限流很严，
// 实测频繁刷新会导致 code=-12 throttle deny，
// 而且限流恢复需要较长时间。
func (s *Server) canRefresh(did string) bool {
	refreshMu.Lock()
	defer refreshMu.Unlock()
	t, ok := refreshLast[did]
	if !ok {
		return true
	}
	return time.Since(t) > 30*time.Minute
}

// RefreshCooldownLeft 返回冷却剩余时间（供 API 提示用户）。
func (s *Server) RefreshCooldownLeft(did string) time.Duration {
	refreshMu.Lock()
	defer refreshMu.Unlock()
	t, ok := refreshLast[did]
	if !ok {
		return 0
	}
	left := 30*time.Minute - time.Since(t)
	if left < 0 {
		return 0
	}
	return left
}

// markRefresh 记录刷新时间。
func (s *Server) markRefresh(did string) {
	refreshMu.Lock()
	refreshLast[did] = time.Now()
	refreshMu.Unlock()
}

// isAuthError 判断是否为凭据失效错误。
func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "auth") ||
		strings.Contains(msg, "failed") ||
		strings.Contains(msg, "code") && strings.Contains(msg, "3")
}

// newConsumerFor 给 producer 建一个 mpegts consumer。
func newConsumerFor(prod core.Producer) (*mpegtsConsumer, error) {
	cons := newMpegtsConsumer()
	bound := 0
	for _, pm := range prod.GetMedias() {
		if pm.Direction != core.DirectionRecvonly {
			continue
		}
		for _, cm := range cons.GetMedias() {
			pc, cc := pm.MatchMedia(cm)
			if pc == nil {
				continue
			}
			tr, err := prod.GetTrack(pm, pc)
			if err != nil {
				continue
			}
			if err := cons.AddTrack(cm, cc, tr); err != nil {
				continue
			}
			bound++
		}
	}
	if bound == 0 {
		_ = cons.Stop()
		return nil, errNoTrack
	}
	return &mpegtsConsumer{cons: cons}, nil
}
