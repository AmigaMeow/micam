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
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	gxiaomi "github.com/AlexxIT/go2rtc/pkg/xiaomi"
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
