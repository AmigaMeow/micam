package main

// 官方通道的每设备串行化（2026-10-11）。
//
// ===== 问题 =====
//
// 摄像头同一时刻只接受【一条 PPCS 连接】。切画质档位时前端会立刻
// 发起新流，而旧流的 HTTP 请求还没断 -> 两个 sidecar 抢同一条连接
// -> 新流建不上（实测：切档后 40 秒无画面，日志显示旧流 0 帧被挤掉）。
//
// ===== 方案 =====
//
// 每设备维护"当前活跃流"的取消函数：
//   · 新流开始时，先取消同设备的上一条流，并等它退出
//   · 保证任意时刻每设备只有一条 PPCS 连接
//
// 为什么不在前端做：前端 <img src> 换地址时旧连接不一定立刻断
// （浏览器行为不可控），必须在服务端强制。

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

type miotActiveStream struct {
	did     string
	channel string
	stdin   io.WriteCloser
	done    chan struct{}
}

var (
	miotStreamMu      sync.Mutex
	miotStreamCtx     = map[string]context.CancelFunc{} // did -> 取消函数
	miotStreamWG      = map[string]chan struct{}{}      // did -> 流退出信号
	miotStreamEnd     = map[string]time.Time{}          // did -> 上一条流结束时刻
	miotStreamingList = map[string]bool{}               // did -> 正在进行实时预览
	miotActiveStreams = map[string]*miotActiveStream{}  // did -> 当前活跃拉流会话
	tutkDevicesMu     sync.RWMutex
	tutkDevices       = map[string]bool{}
)

func setMiotStreaming(did string, active bool) {
	miotStreamMu.Lock()
	defer miotStreamMu.Unlock()
	if active {
		miotStreamingList[did] = true
	} else {
		delete(miotStreamingList, did)
	}
}

func isMiotStreaming(did string) bool {
	miotStreamMu.Lock()
	defer miotStreamMu.Unlock()
	return miotStreamingList[did]
}

func isTUTKDevice(did, model string) bool {
	if strings.Contains(strings.ToLower(model), "ipc009") {
		return true
	}
	tutkDevicesMu.RLock()
	defer tutkDevicesMu.RUnlock()
	return tutkDevices[did]
}

// streamCooldown 两条流之间的最小间隔。
// 现已支持优雅 SIGTERM 释放 PPCS 会话，冷却缩短为 300ms 兜底保护。
const streamCooldown = 300 * time.Millisecond

func registerMiotStreamStdin(did, channel string, stdin io.WriteCloser) {
	miotStreamMu.Lock()
	defer miotStreamMu.Unlock()
	if st, ok := miotActiveStreams[did]; ok && st != nil {
		st.stdin = stdin
		st.channel = channel
	}
}

func switchMiotStreamChannel(did, channel string) bool {
	miotStreamMu.Lock()
	defer miotStreamMu.Unlock()
	st, ok := miotActiveStreams[did]
	if !ok || st == nil || st.stdin == nil {
		return false
	}
	if st.channel == channel {
		return true
	}
	cmd := fmt.Sprintf("channel %s\n", channel)
	if _, err := st.stdin.Write([]byte(cmd)); err != nil {
		logf("[miot] 设备 %s 发送通道切换命令失败: %v", did, err)
		return false
	}
	st.channel = channel
	logf("[miot] 设备 %s 成功无缝切换到通道 %s（零重连延迟）", did, channel)
	return true
}

// beginMiotStream 开始一条新流：
//   - 取消该设备上一条流并等它退出
//   - 返回本次流的 context 和"结束"函数
func beginMiotStream(parent context.Context, did string, initialChannel ...string) (context.Context, context.CancelFunc, func()) {
	initCh := "1"
	if len(initialChannel) > 0 && initialChannel[0] != "" {
		initCh = initialChannel[0]
	}

	// 1. 取出上一条流的控制句柄并立即释放互斥锁，严禁持锁等待（否则上一条流的 finish() 会死锁等待该锁，导致必卡）
	miotStreamMu.Lock()
	cancelOld := miotStreamCtx[did]
	doneOld := miotStreamWG[did]
	miotStreamMu.Unlock()

	// 2. 触发取消并等待上一条流彻底退出与释放连接槽
	if cancelOld != nil {
		cancelOld()
	}
	if doneOld != nil {
		select {
		case <-doneOld:
		case <-time.After(2 * time.Second):
			logf("[miot] %s 的上一条流未在 2 秒内退出，继续", did)
		}
	}

	miotStreamMu.Lock()
	// 3. 冷却：等上一条流结束满 streamCooldown 再建新流。
	if t, ok := miotStreamEnd[did]; ok {
		if wait := streamCooldown - time.Since(t); wait > 0 {
			miotStreamMu.Unlock()
			logf("[miot] %s 冷却 %.1fs（保护摄像头会话表）", did, wait.Seconds())
			time.Sleep(wait)
			miotStreamMu.Lock()
		}
	}

	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	miotStreamCtx[did] = cancel
	miotStreamWG[did] = done
	miotStreamingList[did] = true
	miotActiveStreams[did] = &miotActiveStream{
		did:     did,
		channel: initCh,
		done:    done,
	}
	miotStreamMu.Unlock()

	var finishOnce sync.Once
	finish := func() {
		finishOnce.Do(func() {
			miotStreamMu.Lock()
			// 只有自己还是"当前流"时才清理，避免误删新流
			if c, ok := miotStreamCtx[did]; ok && c != nil {
				if d, ok2 := miotStreamWG[did]; ok2 && d == done {
					if a, ok3 := miotActiveStreams[did]; ok3 && a != nil && a.done == done {
						if a.stdin != nil {
							_ = a.stdin.Close()
						}
						delete(miotActiveStreams, did)
					}
					delete(miotStreamCtx, did)
					delete(miotStreamWG, did)
					delete(miotStreamingList, did)
					miotStreamEnd[did] = time.Now()
				}
			}
			miotStreamMu.Unlock()
			close(done)
		})
	}
	return ctx, cancel, finish
}
