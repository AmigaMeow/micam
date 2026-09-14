package main

import (
	"sync"

	"github.com/AlexxIT/go2rtc/pkg/mpegts"
)

var errNoTrack = errStr("没有匹配的视频轨道")

type errStr string

func (e errStr) Error() string { return string(e) }

// mpegtsConsumer 包装 consumer，提供幂等的停止。
type mpegtsConsumer struct {
	cons *mpegts.Consumer
	once sync.Once
}

// stop 停止（幂等）。
func (m *mpegtsConsumer) stop() {
	m.once.Do(func() {
		_ = m.cons.Stop()
	})
}

// Consumer 返回底层 consumer。
func (m *mpegtsConsumer) Consumer() *mpegts.Consumer {
	return m.cons
}

func newMpegtsConsumer() *mpegts.Consumer {
	return mpegts.NewConsumer()
}
