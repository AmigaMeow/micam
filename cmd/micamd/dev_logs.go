package main

// 请求日志 + 调用统计（开发者页「请求日志」「调用统计」Tab）。
//
// 三件容易踩的事，这里都处理了：
//   1. 只记 /api/ 下的请求，并且**跳过 /api/dev/logs 自己**
//      —— 前端轮询这个接口，记进去会把 200 条缓冲刷满，别的请求全被挤掉
//   2. 流式接口（MJPEG / HLS）**不包装 ResponseWriter**
//      —— 包装体如果不实现 http.Flusher，流会直接卡死
//   3. 按天聚合单独存 <dataDir>/dev-stats.json（30 秒脏时落盘一次）
//      —— 否则每次重启后「近 7 天」都只剩几分钟数据

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// reqLogCap 环形缓冲条数（效果图要求「最近 200 条」）
	reqLogCap = 200
	// statKeepDays 按天聚合保留天数（要能算 7 天环比，所以留足 30 天）
	statKeepDays = 30
	// flushInterval 聚合数据落盘间隔
	flushInterval = 30 * time.Second
)

// reqEntry 一条请求记录。
type reqEntry struct {
	Time   time.Time `json:"time"`
	Method string    `json:"method"`
	Path   string    `json:"path"`
	Status int       `json:"status"`
	Ms     int64     `json:"ms"`
	IP     string    `json:"ip"`
}

// dayStat 一天的聚合计数。
type dayStat struct {
	Total int   `json:"total"`
	OK    int   `json:"ok"`
	Fail  int   `json:"fail"`
	MsSum int64 `json:"msSum"`
}

type reqLogStore struct {
	mu    sync.Mutex
	buf   []reqEntry
	next  int
	full  bool
	daily map[string]*dayStat
	dirty bool
	path  string
}

func newReqLogStore(dataDir string) *reqLogStore {
	s := &reqLogStore{
		buf:   make([]reqEntry, reqLogCap),
		daily: map[string]*dayStat{},
		path:  filepath.Join(dataDir, "dev-stats.json"),
	}
	s.load()
	go s.flusher()
	return s
}

func (s *reqLogStore) load() {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var daily map[string]*dayStat
	if err := json.Unmarshal(raw, &daily); err != nil {
		logf("[dev] 读取 dev-stats.json 失败（已忽略）: %v", err)
		return
	}
	if daily != nil {
		s.daily = daily
	}
}

func (s *reqLogStore) flusher() {
	t := time.NewTicker(flushInterval)
	defer t.Stop()
	for range t.C {
		s.flush()
	}
}

func (s *reqLogStore) flush() {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return
	}
	raw, err := json.MarshalIndent(s.daily, "", "  ")
	if err == nil {
		s.dirty = false
	}
	s.mu.Unlock()

	if err != nil {
		logf("[dev] 序列化 dev-stats.json 失败: %v", err)
		return
	}
	if err := os.WriteFile(s.path, raw, 0o644); err != nil {
		logf("[dev] 写入 dev-stats.json 失败: %v", err)
		s.mu.Lock()
		s.dirty = true // 下次再试
		s.mu.Unlock()
	}
}

func (s *reqLogStore) add(e reqEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.buf[s.next] = e
	s.next = (s.next + 1) % reqLogCap
	if s.next == 0 {
		s.full = true
	}

	// 用**本地时区**的日期分桶（曾经用 UTC 导致「今天」查不到数据）
	key := e.Time.Format("2006-01-02")
	d := s.daily[key]
	if d == nil {
		d = &dayStat{}
		s.daily[key] = d
	}
	d.Total++
	if e.Status >= 200 && e.Status < 400 {
		d.OK++
	} else {
		d.Fail++
	}
	d.MsSum += e.Ms
	s.dirty = true

	cut := e.Time.AddDate(0, 0, -statKeepDays).Format("2006-01-02")
	for k := range s.daily {
		if k < cut {
			delete(s.daily, k)
		}
	}
}

// recent 返回最近 n 条（新的在前）。
func (s *reqLogStore) recent(n int) []reqEntry {
	s.mu.Lock()
	defer s.mu.Unlock()

	total := s.next
	if s.full {
		total = reqLogCap
	}
	if n <= 0 || n > total {
		n = total
	}
	out := make([]reqEntry, 0, n)
	for i := 1; i <= n; i++ {
		idx := (s.next - i + reqLogCap*2) % reqLogCap
		out = append(out, s.buf[idx])
	}
	return out
}

func (s *reqLogStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.full {
		return reqLogCap
	}
	return s.next
}

func avgMsOf(d *dayStat) int {
	if d == nil || d.Total == 0 {
		return 0
	}
	return int(d.MsSum / int64(d.Total))
}

func statJSON(d *dayStat) map[string]any {
	return map[string]any{
		"total": d.Total,
		"ok":    d.OK,
		"fail":  d.Fail,
		"avgMs": avgMsOf(d),
	}
}

// pctDelta 环比百分比；没有基期数据时返回 nil（前端显示「—」）。
func pctDelta(cur, prev int) any {
	if prev <= 0 {
		return nil
	}
	return float64(cur-prev) / float64(prev) * 100
}

// stats 聚合近 days 天数据，并给出与上一个等长周期的环比。
func (s *reqLogStore) stats(days int, now time.Time) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()

	totals := &dayStat{}
	series := make([]map[string]any, 0, days)
	start := now.AddDate(0, 0, -(days - 1))
	for i := 0; i < days; i++ {
		key := start.AddDate(0, 0, i).Format("2006-01-02")
		cur := s.daily[key]
		item := map[string]any{"date": key, "total": 0, "ok": 0, "fail": 0, "avgMs": 0}
		if cur != nil {
			item["total"] = cur.Total
			item["ok"] = cur.OK
			item["fail"] = cur.Fail
			item["avgMs"] = avgMsOf(cur)
			totals.Total += cur.Total
			totals.OK += cur.OK
			totals.Fail += cur.Fail
			totals.MsSum += cur.MsSum
		}
		series = append(series, item)
	}

	prev := &dayStat{}
	for i := 1; i <= days; i++ {
		key := now.AddDate(0, 0, -days-(i-1)).Format("2006-01-02")
		if cur := s.daily[key]; cur != nil {
			prev.Total += cur.Total
			prev.OK += cur.OK
			prev.Fail += cur.Fail
			prev.MsSum += cur.MsSum
		}
	}

	out := map[string]any{
		"days":   days,
		"totals": statJSON(totals),
		"series": series,
		"since":  now.Format(time.RFC3339),
	}
	if prev.Total > 0 {
		out["delta"] = map[string]any{
			"total": pctDelta(totals.Total, prev.Total),
			"ok":    pctDelta(totals.OK, prev.OK),
			"fail":  pctDelta(totals.Fail, prev.Fail),
			"avgMs": pctDelta(avgMsOf(totals), avgMsOf(prev)),
		}
	}
	return out
}

// ---- 中间件 ----

// isStreamPath 长连接流式接口：不包装 ResponseWriter（包装体会丢 Flusher）。
func isStreamPath(p string) bool {
	return strings.HasPrefix(p, "/api/live/mjpeg") ||
		strings.HasPrefix(p, "/api/live/hls") ||
		strings.HasPrefix(p, "/api/live/inject")
}

// statusRecorder 记录状态码。
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
}

// Flush 透传给底层：否则 Flusher 断言失败，HLS / MJPEG 会卡住。
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 让 http.ResponseController 能找到底层 Writer。
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// devLogMiddleware 记录 API 请求（内存环形缓冲 + 按天聚合）。
func (s *Server) devLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") ||
			r.URL.Path == "/api/dev/logs" ||
			isStreamPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		s.reqlog.add(reqEntry{
			Time:   start,
			Method: r.Method,
			Path:   r.URL.Path,
			Status: rec.status,
			Ms:     time.Since(start).Milliseconds(),
			IP:     clientIP(r),
		})
	})
}

// clientIP 取来源 IP（网关走 unix socket 时 RemoteAddr 是 at 符号）。
func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		return strings.TrimSpace(strings.Split(v, ",")[0])
	}
	if v := r.Header.Get("X-Real-IP"); v != "" {
		return strings.TrimSpace(v)
	}
	if r.RemoteAddr == "" || r.RemoteAddr == "@" {
		return "本机"
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---- 接口 ----

// handleDevLogs 返回最近的请求日志。
//
// GET /api/dev/logs?limit=50
func (s *Server) handleDevLogs(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Dev.Unlocked {
		writeErr(w, 403, "开发者模式未开启")
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= reqLogCap {
			limit = n
		}
	}
	items := s.reqlog.recent(limit)
	writeJSON(w, 200, map[string]any{
		"items": items,
		"count": len(items),
		"cap":   reqLogCap,
	})
}

// handleDevStats 返回调用统计（近 N 天 + 环比）。
//
// GET /api/dev/stats?days=7
func (s *Server) handleDevStats(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Dev.Unlocked {
		writeErr(w, 403, "开发者模式未开启")
		return
	}
	days := 7
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= statKeepDays {
			days = n
		}
	}
	writeJSON(w, 200, s.reqlog.stats(days, time.Now()))
}
