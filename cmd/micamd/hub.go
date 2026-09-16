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
	"strconv"
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
			d.AccountID = sess.UserID
			if stream.HasSecondLens(d.Model) {
				d.Lenses = 2
			} else {
				d.Lenses = 1
			}
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
// useConn 取第一镜头的连接（最常用）。
func (s *Server) useConn(dev *xiaomi.Device) (*camConn, *mpegtsConsumer, func(), error) {
	return s.useConnKey(dev, 1)
}

// useConnKey 按「设备+镜头」取连接。
func (s *Server) useConnKey(dev *xiaomi.Device, lens int) (*camConn, *mpegtsConsumer, func(), error) {
	key := connKey(dev.Did, lens)
	_ = key
	tAll := time.Now()

	// 快路径：复用已有连接（无需加锁，瞬间完成）
	if c := getConn(key); c != nil {
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
	lock := didLock(key)
	tLock := time.Now()
	lock.Lock()
	defer lock.Unlock()
	logf("⏱ [1] 等建连锁 %v", time.Since(tLock).Round(time.Millisecond))

	// 双重检查：等锁期间可能已有人建好
	if c := getConn(key); c != nil {
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
	if c := getConn(key); c != nil {
		logf("[%s] 清理残留连接", dev.Did)
		c.stop()
	}

	tSrc := time.Now()

	// ===== 凭据轮换 =====
	//
	// 按「最新优先」逐个尝试历史凭据，只有全部失效才向小米云申请新的。
	//
	// 为什么要这样（2026-09-15 的教训）：
	//
	//   小米云的 miss_get_vendor 限流可以持续 24 小时+。
	//   如果只保留最新一份凭据，一旦它失效，就彻底没得用 ——
	//   只能干等限流解除。
	//
	//   保留多份历史凭据后：
	//     1. 旧的往往还能用 -> 大幅减少云端调用
	//     2. 限流期间仍有可用凭据 -> 可用性大幅提升
	//     3. 确认失效的会被剔除，不会反复试
	var prod core.Producer
	var err error

	sets := s.store.Sources(dev.Did)
	tDial := time.Now()

	for idx, c := range sets {
		p, e := gxiaomi.Dial(sourceForLens(c.Src, lens))
		if e == nil {
			prod = p
			credFailReset(c.Src)
			if idx > 0 {
				logf("[%s] 第 %d 组历史凭据可用，提到最前", dev.Did, idx+1)
				s.store.PromoteSource(dev.Did, c.Src)
			}
			break
		}
		err = e

		// 非凭据问题（网络抖动等）直接返回，不要误删凭据
		if !isAuthError(e) {
			return nil, nil, nil, e
		}

		// 容错：连续 2 次失败才剔除，单次失败只移到末尾
		n := credFailInc(c.Src)
		if n >= 2 {
			logf("[%s] 第 %d/%d 组凭据连续 %d 次失效，剔除", dev.Did, idx+1, len(sets), n)
			credFailReset(c.Src)
			s.store.DropSourceAt(dev.Did, c.Src)
		} else {
			logf("[%s] 第 %d/%d 组凭据失效（第 %d 次，暂保留）", dev.Did, idx+1, len(sets), n)
			// 移到末尾：下次优先试其他凭据
			s.store.DemoteSource(dev.Did, c.Src)
		}
	}

	// 全部失效（或从未申请过）-> 向小米云申请
	if prod == nil {
		if len(sets) > 0 {
			logf("[%s] %d 组凭据全部失效，申请新的", dev.Did, len(sets))
		}

		if !s.canRefresh(dev.Did) {
			left := s.RefreshCooldownLeft(dev.Did)
			fails := s.RefreshFailCount(dev.Did)
			return nil, nil, nil, fmt.Errorf(
				"摄像头凭据需要刷新，但小米云正在限流（已连续失败 %d 次，约 %v 后自动重试）",
				fails, left.Round(time.Minute))
		}

		s.markRefresh(dev.Did)
		newSrc, e2 := s.requestFreshSource(dev)
		if e2 != nil {
			s.markRefreshResult(dev.Did, false)
			return nil, nil, nil, fmt.Errorf("向小米云申请凭据失败: %w", e2)
		}

		p, e3 := gxiaomi.Dial(sourceForLens(newSrc, lens))
		if e3 != nil {
			s.markRefreshResult(dev.Did, false)
			return nil, nil, nil, fmt.Errorf("新凭据连接失败: %w", e3)
		}

		prod = p
		s.store.SaveSource(dev.Did, newSrc)
		s.markRefreshResult(dev.Did, true)
		logf("[%s] 凭据申请成功", dev.Did)
	}

	logf("⏱ [2] 取凭据 %v", time.Since(tSrc).Round(time.Millisecond))
	logf("⏱ [3] CS2 握手 %v", time.Since(tDial).Round(time.Millisecond))

	c := &camConn{
		did:       key,
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

// 凭据失败次数（内存，按 src 哈希计）。
//
// 为什么不失败一次就剔除：
//   摄像头可能偶发返回 auth 错误（忙、暂时抖动）。
//   直接剔除会丢掉本来可用的凭据 —— 而重新申请可能撞限流。
//
// 策略：连续 2 次失败才剔除；只失败 1 次的移到列表末尾（留作兜底）。
var (
	credFailMu sync.Mutex
	credFails  = map[string]int{}
)

func credFailInc(src string) int {
	credFailMu.Lock()
	defer credFailMu.Unlock()
	credFails[src]++
	return credFails[src]
}

func credFailReset(src string) {
	credFailMu.Lock()
	defer credFailMu.Unlock()
	delete(credFails, src)
}

// ===== 多镜头支持 =====
//
// 双摄摄像头（如 CW500 双摄版）在小米云里只有**一个** did，
// 第二镜头靠 P2P 命令参数区分：
//
//   channel 为空 -> 第一镜头
//   channel=2    -> 第二镜头（videoquality2）
//
// 所以连接缓存必须按「设备+镜头」分键，否则两路会互相顶掉。

// connKey 生成连接缓存键。
func connKey(did string, lens int) string {
	if lens >= 2 {
		return did + "#" + strconv.Itoa(lens)
	}
	return did
}

// sourceForLens 给源地址加上镜头参数。
func sourceForLens(src string, lens int) string {
	if lens < 2 {
		return src
	}
	// 已经有 channel 就先去掉，避免重复
	if i := strings.Index(src, "&channel="); i >= 0 {
		rest := src[i+1:]
		if j := strings.Index(rest, "&"); j >= 0 {
			src = src[:i] + rest[j:]
		} else {
			src = src[:i]
		}
	}
	return src + "&channel=2"
}

// useConnLens 按镜头取连接。
func (s *Server) useConnLens(dev *xiaomi.Device, lens int) (*camConn, *mpegtsConsumer, func(), error) {
	return s.useConnKey(dev, lens)
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
//   连续失败 5 次 -> 等 4 小时
//   连续失败 6+ 次 -> 等 24 小时（上限）
//
// 给接口大段安静时间，窗口才有机会重置。
//
// ===== 为什么要 24 小时（2026-09-15 实测）=====
//
// 限流持续了 10 小时+。期间我们只发了 8 次调用
// （间隔 16分→45分→75分→135分，退避有效），
// 但仍然不恢复。
//
// 说明小米的限流**可能不是简单的滑动窗口**，
// 而更像「每日配额」或「长时间封禁」。
// 这种情况下，任何尝试都可能是徒劳，还会延长封禁。
//
// 所以把上限提到 24 小时：给足恢复时间，也不再浪费调用。
var (
	refreshMu   sync.Mutex
	refreshLast = map[string]time.Time{}
	refreshFail = map[string]int{}
)

const (
	cooldownBase = 15 * time.Minute
	cooldownMax  = 24 * time.Hour
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

// cooldownHuman 可读的冷却描述（给界面用）。
func cooldownHuman(fails int) string {
	d := cooldownFor(fails)
	if d >= time.Hour {
		return fmt.Sprintf("%.0f 小时", d.Hours())
	}
	return fmt.Sprintf("%.0f 分钟", d.Minutes())
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
