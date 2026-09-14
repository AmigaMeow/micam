package main

// 带缓冲的管道：生产者全速写，消费者按自己节奏读。
//
// 为什么需要：
//   io.Pipe 是同步的——写和读必须配对，
//   ffmpeg 解码慢的时候会反压到 go2rtc 的 consumer，
//   最终撑爆 producer 内部缓冲。

import (
	"errors"
	"io"
	"sync"
)

type bufferedPipe struct {
	mu     sync.Mutex
	cond   *sync.Cond
	data   []byte
	max    int
	closed bool
	err    error
}

var errPipeClosed = errors.New("pipe closed")

func newBufferedPipe(max int) *bufferedPipe {
	p := &bufferedPipe{max: max}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// Write 追加数据（超过上限则丢弃最旧的）。
func (p *bufferedPipe) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, errPipeClosed
	}
	p.data = append(p.data, b...)
	// 超限丢弃最旧的（丢帧好过卡死）
	if len(p.data) > p.max {
		p.data = p.data[len(p.data)-p.max:]
	}
	p.cond.Signal()
	return len(b), nil
}

// Read 读数据（阻塞直到有数据或关闭）。
func (p *bufferedPipe) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.data) == 0 && !p.closed {
		p.cond.Wait()
	}
	if len(p.data) > 0 {
		n := copy(b, p.data)
		p.data = p.data[n:]
		return n, nil
	}
	if p.err != nil {
		return 0, p.err
	}
	return 0, io.EOF
}

// Close 关闭。
func (p *bufferedPipe) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	p.cond.Broadcast()
	return nil
}
