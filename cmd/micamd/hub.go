package main

// 摄像头连接管理（共享模式）。
//
// ===== 核心约束 =====
//
// 1. cs2 协议同一设备只允许【一路】连接，重复 Dial 会 "pop buffer is full"
// 2. Producer 是推模式：consumer 必须持续读，读慢了会撑爆内部缓冲
// 3. 客户端断开后必须主动释放 consumer，否则 WriteTo 永不返回
//
// ===== 设计：一个连接，多个消费者 =====
//
// 之前是「独占」模式：每个请求 Dial 一次，用完拆除。
// 问题：
//   - HLS 占住连接时，抓拍/MJPEG 要等最多 60 秒（实测 1m0s）
//   - 网页和 Mac App 不能同时看同一台摄像头
//   - 重复观看每次都重新握手（多花 0.5s）
//
// 改成共享模式：
//
//   每台设备一个 producer（共享）
//     +-- consumer A (HLS)
//     +-- consumer B (MJPEG)
//     +-- consumer C (抓拍)
//
//   最后一个消费者离开后再过 idle 时间才停 producer。
//
// 好处：
//   - 多个客户端同时观看（不互斥）
//   - 二次观看立即出画（连接已热）
//   - 抓拍可以从热连接上直接取

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

// camConn 一台摄像头的共享连接。
type camConn struct {
	did  string
	prod core.Producer

	mu        sync.Mutex
	refs      int       // 活跃消费者数
	lastUsed  time.Time // 最后使用时间
	createdAt time.Time
	prodDone  chan struct{}
	stopped   bool
}

var (
	connMu = sync.Mutex{}
	conns  = map[string]*camConn{}
	// dialLocks 保证同一设备同时只有一次 Dial
	dialLocks = map[string]*sync.Mutex{}
)

// allCameras 取所有摄像头（优先缓存，为空则从云端拉并回填缓存）。
//
// 为什么需要：服务刚重启时本地设备缓存是空的，
// 依赖 s.store.Devices() 的接口会返回空列表。
func (s *Server) allCameras() []*xiaomi.Device {
	if devs := s.store.Devices(); len(devs) > 0 {
		return devs
	}

	// 缓存为空 -> 从云端拉
	var vals []xiaomi.Device
	var out []*xiaomi.Device
	seen := map[string]bool{}
	for _, sess := range s.store.AllSessions() {
		devs, err := sess.AllCameras()
		if err != nil {
			continue
		}
		for i := range devs {
			if seen[devs[i].Did] {
				continue
			}
			seen[devs[i].Did] = true
			d := devs[i]
			vals = append(vals, d)
			out = append(out, &d)
		}
	}
	if len(vals) > 0 {
		s.store.CacheDevices(vals)
	}
	return out
}

// didLock 取该 did 的建连锁。
func didLock(did string) *sync.Mutex {
	connMu.Lock()
	defer connMu.Unlock()
	l, ok := dialLocks[did]
	if !ok {
		l = &sync.Mutex{}
		dialLocks[did] = l
	}
	return l
}

// connTimeout 建连超时。
const connTimeout = 12 * time.Second

// connIdle 最后一个消费者离开后，连接保持多久再释放。
//
// 保持一段时间的好处：用户切换页面/重连时无需重新握手（立即出画）。
const connIdle = 25 * time.Second

// addRef 增加引用。
func (c *camConn) addRef() {
	c.mu.Lock()
	c.refs++
	c.lastUsed = time.Now()
	c.mu.Unlock()
}

// releaseRef 减少引用，并在归零时安排回收。
func (c *camConn) releaseRef() {
	c.mu.Lock()
	c.refs--
	c.lastUsed = time.Now()
	refs := c.refs
	c.mu.Unlock()

	if refs <= 0 {
		go c.reapWhenIdle()
	}
}

// reapWhenIdle 空闲一段时间后停止连接。
func (c *camConn) reapWhenIdle() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for range t.C {
		c.mu.Lock()
		if c.refs > 0 || c.stopped {
			c.mu.Unlock()
			return
		}
		idle := time.Since(c.lastUsed)
		c.mu.Unlock()

		if idle >= connIdle {
			logf("[%s] 空闲 %v，释放连接", c.did, idle.Round(time.Second))
			c.stop()
			return
		}
	}
}

// stop 停止 producer 并注销。
func (c *camConn) stop() {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	c.stopped = true
	c.mu.Unlock()

	connMu.Lock()
	if conns[c.did] == c {
		delete(conns, c.did)
	}
	connMu.Unlock()

	_ = c.prod.Stop()
	select {
	case <-c.prodDone:
	case <-time.After(2 * time.Second):
	}
	logf("[%s] 连接已释放", c.did)
}

// getConn 取现有的可用连接（不建连）。
func getConn(did string) *camConn {
	connMu.Lock()
	defer connMu.Unlock()
	c, ok := conns[did]
	if !ok {
		return nil
	}
	c.mu.Lock()
	stopped := c.stopped
	c.mu.Unlock()
	if stopped {
		return nil
	}
	return c
}

// useConn 使用某设备的连接（共享）。
//
// 返回的 release 必须在用完时调用（建议 defer）。
// release 只释放本次的 consumer，连接本身会在空闲后回收。
func (s *Server) useConn(dev *xiaomi.Device) (*camConn, *mpegtsConsumer, func(), error) {
	tAll := time.Now()

	// 快路径：复用已有连接（无需加锁，瞬间完成）
	if c := getConn(dev.Did); c != nil {
		mc, err := newConsumerFor(c.prod)
		if err == nil {
			c.addRef()
			logf("⏱ [0] 复用热连接 %v", time.Since(tAll).Round(time.Millisecond))
			return c, mc, func() {
				mc.stop()
				c.releaseRef()
			}, nil
		}
		// 建 consumer 失败，说明连接坏了，重建
		logf("[%s] 复用失败（%v），重建连接", dev.Did, err)
		c.stop()
	}

	// 慢路径：建新连接
	lock := didLock(dev.Did)
	tLock := time.Now()
	lock.Lock()
	defer lock.Unlock()
	logf("⏱ [1] 等建连锁 %v", time.Since(tLock).Round(time.Millisecond))

	// 双重检查：等锁期间可能已有人建好
	if c := getConn(dev.Did); c != nil {
		mc, err := newConsumerFor(c.prod)
		if err == nil {
			c.addRef()
			logf("⏱ [1.5] 等锁后复用 %v", time.Since(tAll).Round(time.Millisecond))
			return c, mc, func() {
				mc.stop()
				c.releaseRef()
			}, nil
		}
	}

	// 清理残留
	if c := getConn(dev.Did); c != nil {
		logf("[%s] 清理残留连接", dev.Did)
		c.stop()
	}

	tSrc := time.Now()
	src, err := s.resolveSource(dev)
	if err != nil {
		return nil, nil, nil, err
	}
	logf("⏱ [2] 取凭据 %v", time.Since(tSrc).Round(time.Millisecond))

	tDial := time.Now()
	prod, err := gxiaomi.Dial(src)

	// 凭据失效时的处理策略：
	//   1. 先清缓存，重新申请
	//   2. 刷新失败（如小米云限流）时保留旧凭据
	//   3. 记录冷却时间，避免频繁刷新触发限流
	if err != nil && isAuthError(err) {
		logf("[%s] 凭据失效（%v），尝试刷新", dev.Did, err)

		if !s.canRefresh(dev.Did) {
			logf("[%s] 刷新冷却中，跳过（上次已尝试）", dev.Did)
		} else {
			s.markRefresh(dev.Did)
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
		return nil, nil, nil, err
	}
	logf("⏱ [3] CS2 握手 %v", time.Since(tDial).Round(time.Millisecond))

	c := &camConn{
		did:       dev.Did,
		prod:      prod,
		createdAt: time.Now(),
		lastUsed:  time.Now(),
		prodDone:  make(chan struct{}),
	}

	connMu.Lock()
	conns[dev.Did] = c
	connMu.Unlock()

	// 每个消费者一份，互不影响
	mc, err := newConsumerFor(prod)
	if err != nil {
		_ = prod.Stop()
		connMu.Lock()
		delete(conns, dev.Did)
		connMu.Unlock()
		return nil, nil, nil, err
	}
	logf("⏱ [4] 建 consumer %v", time.Since(tAll).Round(time.Millisecond))

	logf("已连接摄像头: %s (%s)", dev.Name, dev.Did)

	// 启动 producer 读循环
	go func() {
		defer close(c.prodDone)
		defer recoverPanic("producer " + dev.Did)
		_ = prod.Start()
	}()

	c.addRef()
	logf("⏱ [5] useConn 总计 %v", time.Since(tAll).Round(time.Millisecond))

	return c, mc, func() {
		mc.stop()
		c.releaseRef()
	}, nil
}

// ===== 凭据刷新冷却：指数退避 =====
//
// 为什么不用固定 30 分钟：
//
// 小米的 miss_get_vendor 限流是**滑动窗口**。
// 固定间隔重试 = 每隔 30 分钟又去撞一次，窗口永远不重置。
//
// 实测教训（2026-09-15）：限流持续 2 小时+，
// 就是因为每 30 分钟重试一次，而且每次还重试 5 遍（共 75 秒）。
// 大量的失败调用让窗口一直无法重置。
//
// 改成指数退避：
//
//   连续失败 1 次 -> 等 15 分钟
//   连续失败 2 次 -> 等 30 分钟
//   连续失败 3 次 -> 等 1 小时
//   连续失败 4 次 -> 等 2 小时
//   连续失败 5 次 -> 等 4 小时（上限）
//
// 给接口大段安静时间，窗口才有机会重置。
var (
	refreshMu   sync.Mutex
	refreshLast = map[string]time.Time{}
	refreshFail = map[string]int{}
)

const (
	cooldownBase = 15 * time.Minute
	cooldownMax  = 4 * time.Hour
)

// cooldownFor 根据连续失败次数算冷却时长。
func cooldownFor(fails int) time.Duration {
	if fails <= 1 {
		return cooldownBase
	}
	d := cooldownBase
	for i := 1; i < fails; i++ {
		d *= 2
		if d >= cooldownMax {
			return cooldownMax
		}
	}
	return d
}

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

// canRefresh 判断是否可以尝试刷新。
func (s *Server) canRefresh(did string) bool {
	refreshMu.Lock()
	defer refreshMu.Unlock()
	t, ok := refreshLast[did]
	if !ok {
		return true
	}
	return time.Since(t) > cooldownFor(refreshFail[did])
}

// markRefresh 记录一次刷新尝试。
func (s *Server) markRefresh(did string) {
	refreshMu.Lock()
	refreshLast[did] = time.Now()
	refreshMu.Unlock()
}

// markRefreshResult 记录刷新结果（成功清零失败计数）。
func (s *Server) markRefreshResult(did string, ok bool) {
	refreshMu.Lock()
	defer refreshMu.Unlock()
	if ok {
		refreshFail[did] = 0
	} else {
		refreshFail[did]++
	}
}

// RefreshCooldownLeft 返回冷却剩余时间。
func (s *Server) RefreshCooldownLeft(did string) time.Duration {
	refreshMu.Lock()
	defer refreshMu.Unlock()
	t, ok := refreshLast[did]
	if !ok {
		return 0
	}
	left := cooldownFor(refreshFail[did]) - time.Since(t)
	if left < 0 {
		return 0
	}
	return left
}

// RefreshFailCount 连续失败次数。
func (s *Server) RefreshFailCount(did string) int {
	refreshMu.Lock()
	defer refreshMu.Unlock()
	return refreshFail[did]
}

// isAuthError 判断是否为凭据失效错误。
func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "auth") ||
		strings.Contains(msg, "failed") ||
		(strings.Contains(msg, "code") && strings.Contains(msg, "3"))
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

// stopAllConns 停止所有连接（服务退出时用）。
func stopAllConns() {
	connMu.Lock()
	list := make([]*camConn, 0, len(conns))
	for _, c := range conns {
		list = append(list, c)
	}
	connMu.Unlock()
	for _, c := range list {
		c.stop()
	}
}
