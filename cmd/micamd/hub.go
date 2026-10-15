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

	"encoding/json"
	"github.com/AlexxIT/go2rtc/pkg/core"
	gxiaomi "github.com/AlexxIT/go2rtc/pkg/xiaomi"
	"github.com/micam/micam/internal/stream"
	"github.com/micam/micam/internal/xiaomi"
	"os"
	"path/filepath"
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
// allCameras 返回【已纳入管理】的设备（纯读缓存）。
//
// ⚠️ 重要设计变更（2026-09-18 用户多次反馈「清空后设备又自动出现」）
//
// 原实现有一个「缓存为空就自动去云端拉」的回退分支：
//
//	if devs := s.store.Devices(); len(devs) > 0 { return devs }
//	// 缓存为空 -> 从云端拉          <-- 罪魁祸首
//	for _, sess := range s.store.AllSessions() { sess.AllCameras() }
//
// 后果：
//
//	清空数据后，任何页面（首页/存储/AI/通知…共 16 个调用点）
//	调一次就会触发真·云端拉取，设备又冒出来了。
//	用户看到的就是「我什么都没点，设备自己出现了」。
//
// 为什么原来这么写：服务刚重启时缓存是空的，
//
//	作者想「自动恢复」一下。但这违背了
//	「获取设备必须是用户的明确动作」这个产品原则。
//
// 现在：**只读缓存**。缓存空就返回空列表。
//
//	要拉设备，必须用户点「获取设备」（/api/devices/cloud?refresh=1）。
//
// 另外顺带做一件事：过滤掉【未纳入管理】的设备 ——
// 业务页面（首页/录像/AI）不应该看到用户没纳入的摄像头。
func (s *Server) allCameras() []*xiaomi.Device {
	if s == nil || s.store == nil {
		return nil
	}
	cached := s.store.Devices()
	out := make([]*xiaomi.Device, 0, len(cached))
	for _, d := range cached {
		if d == nil {
			continue
		}
		if !s.meta.IsManaged(d.Did) {
			continue // 未纳入管理，业务页面不显示
		}
		out = append(out, d)
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

	if err := c.prod.Stop(); err != nil {
		logf("[hub] 停止 producer 失败: %v", err)
	}
	select {
	case <-c.prodDone:
	case <-time.After(2 * time.Second):
	}
	logf("[%s] 连接已释放", c.did)
}

// getConn 取现有的可用连接（不建连）。
// stopSiblingLenses 停掉「同一台摄像机的其它镜头」连接。
//
// 为什么需要（Mac 侧实测发现）：
//
//	connKey(did, 2) = did#2，和主镜头是不同的 key，
//	于是切通道时会对**同一台摄像机**建第二条 P2P 会话。
//	但小米摄像机一次只接受一条会话 —— 第二条会 probe 失败：
//	  {"msg":"连接摄像头失败: xiaomi: probe: miss: read media: cs2: EOF"}
//	表现为「在两路之间来回切很容易失败」。
//
// 做法：建新连接前，先把同设备其它 lens 的连接停掉，并等一小会儿
//
//	让摄像机释放会话。
func stopSiblingLenses(did string, keepKey string) {
	prefix := did + "#"
	var victims []*camConn
	connMu.Lock()
	for k, c := range conns {
		if k == keepKey {
			continue
		}
		// 同设备：要么是主镜头 key（== did），要么是 did#N
		if k == did || strings.HasPrefix(k, prefix) {
			victims = append(victims, c)
			delete(conns, k)
		}
	}
	connMu.Unlock()

	if len(victims) == 0 {
		return
	}
	logf("[%s] 切换镜头：先停掉 %d 条同设备连接（摄像机只接受一条会话）", did, len(victims))
	for _, c := range victims {
		c.stop()
	}
	// 给摄像机一点时间释放 P2P 会话，否则新会话会 probe 失败
	time.Sleep(1200 * time.Millisecond)
}

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

	// 同一台摄像机一次只能有一条 P2P 会话，
	// 所以建新连接前必须先停掉它的另一路（否则 probe 会 cs2: EOF）
	stopSiblingLenses(dev.Did, key)
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

	// ⚠️ 历史凭据重试加总时长上限（2026-09-20）
	//
	// 原来会把【所有】历史凭据逐个 Dial 一遍，每组都可能等几秒。
	// 如果设备有 3 组历史凭据且都失效，每次建连要耗 10+ 秒，
	// 而且期间会反复触发 cs2 握手 —— 这本身就是调用量。
	//
	// 现在：整轮最多尝试 credDialBudget，超了就直接去申请新凭据。
	credDeadline := time.Now().Add(credDialBudget)

	for idx, c := range sets {
		if time.Now().After(credDeadline) {
			logf("[%s] 历史凭据尝试超时（已试 %d 组），直接申请新凭据", dev.Did, idx)
			break
		}
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
		if err := prod.Stop(); err != nil {
			logf("[hub] 停止旧 producer 失败: %v", err)
		}
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
		if err := prod.Start(); err != nil {
			logf("[hub] 启动 producer 失败: %v", err)
		}
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
//
//	摄像头可能偶发返回 auth 错误（忙、暂时抖动）。
//	直接剔除会丢掉本来可用的凭据 —— 而重新申请可能撞限流。
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
//	连续失败 1 次 -> 等 15 分钟
//	连续失败 2 次 -> 等 30 分钟
//	连续失败 3 次 -> 等 1 小时
//	连续失败 4 次 -> 等 2 小时
//	连续失败 5 次 -> 等 4 小时
//	连续失败 6+ 次 -> 等 24 小时（上限）
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
	// cooldownBase 凭据续期的冷却基数。
	//
	// 历史：
	//   最初 15 分钟 —— 和 credKeeperInterval 相等 + 用 > 判断
	//   -> 永远不重试 -> 凭据一直过期（死锁 bug）
	//   改成 12 分钟后修复了死锁。
	//
	// 再改（2026-09-18 晚，实测教训）：
	//   小米限流（throttle deny）会持续十几小时，
	//   而 12 分钟一次的持续重试可能被判定为「异常流量」，
	//   反而延长限流。
	//   改成 30 分钟起，连续失败翻倍退避（30m -> 1h -> 2h ... 上限 24h）：
	//   既能自动恢复，又不会激进重试。
	cooldownBase = 30 * time.Minute
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
	// 注意：这里必须是 >=，而且要保持「冷却 < keeper 间隔」。
	//
	// 曾经的 bug：cooldownBase 和 credKeeperInterval 都是 15 分钟，
	// 用 > 判断时每次 keeper 跑到都刚好等于边界 -> 永远 false
	// -> 永远不重试续期 -> 凭据一直过期 -> 摄像头连上但不发数据。
	return time.Since(t) >= cooldownFor(refreshFail[did])
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

// isThrottleError 判断是否为小米限流（throttle deny）。
//
// 这类错误不同于「凭据失效」：凭据本身没问题，
// 只是小米临时拒绝我们的申请。重试太频繁会让它更久。
func isThrottleError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "throttle") || strings.Contains(msg, "code=-12")
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

// ===== 管线卡死检测与自动恢复 =====
//
// 背景（2026-09-18 修正版）：
//
//   ⚠️ 第一版把根因写成「GPU 硬编失败 -> 消费端停 -> 缓冲满」——
//      这是【错的】，两者只是日志时间接近，是巧合。
//
//   真实根因（见 hub.go 顶部注释）：
//     cs2 协议同一设备只允许【一路】连接，
//     重复 Dial 就会报 "pop buffer is full"。
//
//   所以卡死的本质是【连接泄漏 / 重复建连】，不是编码器问题。
//   （HLS 本来就强制用 libx264 软编，见 hls.go 的说明）
//         -> 生产者写不进去
//           -> 整个管线卡死，feed 只写 376 字节
//             -> HLS 无法生成分片 -> 黑屏，且【自己不会恢复】
//
// 现象特征：feed 结束时 total 极小（正常应 100KB~几 MB）。
// 所以用「连续 N 次写入过小」作为卡死信号，主动丢掉缓存的连接，
// 让下一次请求强制重新 Dial。

const (
	stuckMinBytes  = 64 * 1024 // 小于 64KB 视为「几乎没数据」
	stuckThreshold = 3         // 连续 3 次就判定卡死
)

var (
	stuckMu    sync.Mutex
	stuckCount = map[string]int{}
)

// noteStuckFeed 记一次「几乎没数据」的 feed，返回连续次数。
func noteStuckFeed(did string) int {
	stuckMu.Lock()
	defer stuckMu.Unlock()
	stuckCount[did]++
	return stuckCount[did]
}

// clearStuckFeed 有一次正常写入就清零。
func clearStuckFeed(did string) {
	stuckMu.Lock()
	delete(stuckCount, did)
	stuckMu.Unlock()
}

// ===== 卡死恢复的冷却 =====
//
// ⚠️ 为什么需要（2026-09-18 实测发现）
//
// 加了自动恢复后，日志出现这种循环：
//
//	连续1次 -> 连续2次 -> 连续3次（丢弃连接）-> 连续1次 -> ... 无限循环
//
// 说明：丢掉连接 -> 立刻重建 -> 摄像头端还是「pop buffer is full」
//
//	-> 又只有 376 字节 -> 又丢 -> 又建 ...
//
// 这不是「重建不够快」，而是**重建太频繁** ——
// 摄像头端的发送缓冲需要时间平复，急着重建只会让它更忙。
//
// 所以加冷却：同一设备两次强制重建之间至少间隔 stuckRecoverCooldown。
// 冷却期内不再丢弃（让现有连接自己走完），避免空转。
const stuckRecoverCooldown = 45 * time.Second

var (
	lastRecoverMu sync.Mutex
	lastRecover   = map[string]time.Time{}
)

// recoverAllowed 判断该设备现在是否允许强制重建。
func recoverAllowed(did string) bool {
	lastRecoverMu.Lock()
	defer lastRecoverMu.Unlock()
	t, ok := lastRecover[did]
	if ok && time.Since(t) < stuckRecoverCooldown {
		return false
	}
	lastRecover[did] = time.Now()
	return true
}

// dropCamConn 丢弃某设备的缓存连接，让下次重新建立。
//
// ⚠️ 曾经写错过（2026-09-18 自查发现）：
//
//	第一版只做了「从 map 移除 + 标记 stopped」，**没有真正停 producer**。
//	结果：P2P 连接还活着，下次请求又 Dial 一路 ->
//	        同一设备两路连接 -> 正是 "pop buffer is full"！
//	        也就是说这个「修复」反而会制造它想解决的问题。
//
// 正确做法：调用 c.stop()（它会 prod.Stop() 并等 prodDone 确认释放）。
//
// 另外：有活跃消费者（refs > 0）时不能拆 —— 那会把正在看的画面掐掉。
// 这种情况返回 false，交给原来的 reapWhenIdle 空闲回收逻辑处理。
func dropCamConn(did string) bool {
	connMu.Lock()
	c, ok := conns[did]
	connMu.Unlock()
	if !ok {
		return false
	}

	c.mu.Lock()
	refs := c.refs
	c.mu.Unlock()
	if refs > 0 {
		// 有活跃消费者，不能拆（会被 reapWhenIdle 在空闲后正常回收）
		return false
	}

	// 真正关闭：stop() 内部会 prod.Stop()、等待 prodDone、从 map 注销
	c.stop()
	return true
}

// ===== 限流静默期 =====
//
// 小米返回 throttle deny 后，进入一段静默期：
//
//	· 不再自动重试凭据申请
//	· 手动「立即重试」不受限制（用户可以主动试）
//
// 为什么需要（2026-09-19）：
//
//	限流持续 20+ 小时。持续自动重试可能被风控判定为
//	「持续异常流量」，反而延长限流。静默一段时间更利于恢复。
//
// 静默时长：12 小时（第一次触发），之后仍限流则翻倍，
//
//	上限 24 小时。
const (
	throttleSilenceBase = 12 * time.Hour
	throttleSilenceMax  = 24 * time.Hour
)

var (
	throttleMu       sync.Mutex
	throttleSilenceT time.Time // 静默期结束时刻
	throttleSilenceN int       // 连续进入静默的次数
)

// ===== 静默状态持久化 =====
//
// ⚠️ 为什么必须持久化（2026-09-20 实测发现）
//
// 原实现把静默状态放在内存 —— 服务一重启就丢，
// 于是立刻又开始调 miss_get_vendor，把刚安静下来的窗口又撞开。
//
// 实测时间线：
//   09-19 06:51 静默 12 小时
//   09-19 18:51 到期 -> 自动重试 -> 又撞限流
//   09-20 06:38 累计失败 7 次 -> 静默 24 小时
//   限流持续 44 小时
//
// 中间只要重启一次，静默就白做了。
//
// 所以存到 <dataDir>/throttle.json。

type throttleState struct {
	SilenceUntil string `json:"silenceUntil"` // RFC3339
	Count        int    `json:"count"`
}

func throttleFile() string {
	return filepath.Join(dataDirPath, "throttle.json")
}

// loadThrottleState 启动时载入静默状态。
func loadThrottleState() {
	raw, err := os.ReadFile(throttleFile())
	if err != nil {
		return
	}
	var st throttleState
	if json.Unmarshal(raw, &st) != nil {
		return
	}
	t, err := time.Parse(time.RFC3339, st.SilenceUntil)
	if err != nil {
		return
	}
	throttleMu.Lock()
	throttleSilenceT = t
	throttleSilenceN = st.Count
	throttleMu.Unlock()
	if time.Now().Before(t) {
		logf("[cred] 载入静默状态：剩余 %v", time.Until(t).Round(time.Minute))
	}
}

// saveThrottleState 落盘（调用方需持有锁，或先取快照）。
func saveThrottleState(t time.Time, n int) {
	st := throttleState{SilenceUntil: t.Format(time.RFC3339), Count: n}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(throttleFile(), raw, 0o600)
}

// credDialBudget 一轮里尝试历史凭据的总时长上限。
//
// 超过就直接去申请新凭据，避免为了试旧凭据反复握手。
const credDialBudget = 12 * time.Second

// manualRenewMinGap 手动重试的最短间隔。
//
// 冷却机制对【自动】重试有意义，但对用户手动点击不该挡
// （否则按钮形同虚设）。但也不能让人狂点 —— 那正是限流的诱因。
const manualRenewMinGap = 10 * time.Second

// forceRefreshNow 让某设备的冷却立即到期（供手动重试用）。
//
// 只把「上次刷新时刻」往前推，保留 failCount ——
// 这样退避序列仍然正确累积，只是这一下不等冷却。
func (s *Server) forceRefreshNow(did string) {
	refreshMu.Lock()
	defer refreshMu.Unlock()
	if _, ok := refreshLast[did]; ok {
		refreshLast[did] = time.Now().Add(-cooldownMax)
	}
}

// markThrottled 检测到限流时调用，进入静默期。
func (s *Server) markThrottled() {
	throttleMu.Lock()
	defer throttleMu.Unlock()
	throttleSilenceN++
	d := throttleSilenceBase
	for i := 1; i < throttleSilenceN; i++ {
		d *= 2
		if d >= throttleSilenceMax {
			d = throttleSilenceMax
			break
		}
	}
	throttleSilenceT = time.Now().Add(d)
	until, cnt := throttleSilenceT, throttleSilenceN
	logf("[cred] 检测到小米限流 -> 静默 %v（第 %d 次），期间不自动重试",
		d.Round(time.Minute), throttleSilenceN)
	// 落盘：避免重启丢失（重启会立刻又去撞限流）
	saveThrottleState(until, cnt)
}

// throttleSilenced 当前是否处于静默期。
func (s *Server) throttleSilenced() bool {
	throttleMu.Lock()
	defer throttleMu.Unlock()
	return time.Now().Before(throttleSilenceT)
}

// throttleSilenceLeft 静默期剩余时长（给接口用）。
func (s *Server) throttleSilenceLeft() time.Duration {
	throttleMu.Lock()
	defer throttleMu.Unlock()
	left := time.Until(throttleSilenceT)
	if left < 0 {
		return 0
	}
	return left
}

// ResetThrottleSilence 手动解除静默（用户点「立即重试」时调用）。
func (s *Server) ResetThrottleSilence() {
	throttleMu.Lock()
	throttleSilenceT = time.Time{}
	// 不清 Count —— 保留退避级别（用户手动试失败多次说明确实被限）
	saveThrottleState(time.Time{}, throttleSilenceN)
	throttleMu.Unlock()
}
