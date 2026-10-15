package main

// 官方通道推流统计（2026-10-11）。
//
// 旧通道有 /api/live/stats（从 CS2 连接读字节数），
// 但官方通道走的是 sidecar + ffmpeg，没有那个统计数据。
//
// 这里记录每次 /api/miot/live 推了多少帧、多少字节、耗时多久，
// 供前端算实际码率（用户看到的"8.4 Mbps"要真实）。

import (
	"net/http"
	"sync"
	"time"
)

type miotLiveStat struct {
	Frames   int64
	Bytes    int64
	Started  time.Time
	LastPush time.Time
}

var (
	miotStatMu sync.Mutex
	miotStats  = map[string]*miotLiveStat{}
)

// recordMiotPush 记录推流进度（由 handleMiotLive 调用）。
//
// ⚠️ 2026-10-11 修正：原来用 += 累加，导致：
//
//	· 前一次推流的字节数被带进下一次 -> 码率虚高（实测 400 Mbps，实际 28）
//	· 多次并发流互相污染
//
// 现在改为【每次调用都传累计值】，直接覆盖而不是累加。
//
// 参数：
//
//	did      设备
//	started  本次推流的开始时刻（用于算时长）
//	frames   本次推流【至今累计】的帧数
//	bytes    本次推流【至今累计】的字节数
func recordMiotPush(did string, started time.Time, frames, bytes int64) {
	miotStatMu.Lock()
	defer miotStatMu.Unlock()
	s := miotStats[did]
	if s == nil || !s.Started.Equal(started) {
		// 新的一次推流（开始时刻不同）-> 重建
		s = &miotLiveStat{Started: started}
		miotStats[did] = s
	}
	s.Frames = frames
	s.Bytes = bytes
	s.LastPush = time.Now()
}

// handleMiotLiveStats GET /api/miot/live/stats?did=
//
// 返回最近一次推流的平均码率（Mbps）。
func (s *Server) handleMiotLiveStats(w http.ResponseWriter, r *http.Request) {
	did := r.URL.Query().Get("did")
	miotStatMu.Lock()
	st := miotStats[did]
	miotStatMu.Unlock()

	out := map[string]any{"did": did, "mbps": 0.0}
	if st != nil {
		dur := time.Since(st.Started).Seconds()
		if dur > 1 {
			out["mbps"] = float64(st.Bytes*8) / dur / 1e6
		}
		// 帧率也算出来（前端/诊断都用得上）
		if dur > 1 {
			out["fps"] = float64(st.Frames) / dur
		}
		out["frames"] = st.Frames
		out["bytes"] = st.Bytes
		out["seconds"] = int(dur)
		// 推流结束后（超过 10 秒没更新）就清零，避免一直显示旧值
		if !st.LastPush.IsZero() && time.Since(st.LastPush) > 10*time.Second {
			miotStatMu.Lock()
			delete(miotStats, did)
			miotStatMu.Unlock()
		}
	}
	writeJSON(w, 200, out)
}
