
package event

// 事件存储与移动侦测。
//
// ===== 为什么需要 =====
//
// 小米 API **不提供事件流**（已验证：摄像头的 MIoT 属性读写不支持）。
// 所以「有动静」「有人经过」这类信息必须自己产生。
//
// ===== 实现方式 =====
//
// 复用已有的缩略图缓存：每台在线摄像头每 N 秒本来就有一张 640px 的图，
// 拿相邻两张做灰度帧差，超过阈值就是「移动」。
//
// 成本：几乎为零（图已经在内存里，只多一次缩放 + 逐像素比较）。
//
// ===== 局限 =====
//
// 只能判「有没有变化」，不能判「是不是人」。
// 人形/车辆/包裹需要图像模型（未来接入）。
// 夜间红外切换、树影、雨雪会产生误报，靠灵敏度和静默期抑制。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Event 一条事件。
type Event struct {
	ID       string  `json:"id"`
	Time     string  `json:"time"`          // 2006-01-02 15:04:05
	Did      string  `json:"did"`
	Device   string  `json:"device"`        // 设备名（快照，避免改名后历史错乱）
	Type     string  `json:"type"`          // motion / offline / online
	Desc     string  `json:"desc"`
	Thumb    string  `json:"thumb,omitempty"` // 缩略图文件名
	Duration int     `json:"duration,omitempty"` // 持续秒数
	Score    float64 `json:"score,omitempty"`    // 变化强度
	// 便于「点事件跳回放」
	RecFile string  `json:"recFile,omitempty"`
	RecOff  float64 `json:"recOff,omitempty"`
}

// 事件类型
const (
	TypeMotion  = "motion"
	TypeOffline = "offline"
	TypeOnline  = "online"
)

// Store 事件库（按天分文件，追加写）。
type Store struct {
	mu      sync.Mutex
	dir     string
	thumbDir string
	keepDays int
}

// NewStore 创建事件库。
func NewStore(dir string, keepDays int) *Store {
	if keepDays <= 0 {
		keepDays = 90
	}
	s := &Store{
		dir:      dir,
		thumbDir: filepath.Join(dir, "thumbs"),
		keepDays: keepDays,
	}
	_ = os.MkdirAll(s.thumbDir, 0o755)
	return s
}

// Add 追加一条事件。
func (s *Store) Add(ev Event) {
	if ev.ID == "" {
		ev.ID = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	if ev.Time == "" {
		ev.Time = time.Now().Format("2006-01-02 15:04:05")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.appendLocked(ev)
}

func (s *Store) appendLocked(ev Event) error {
	day := ev.Time[:10]
	p := filepath.Join(s.dir, day+".jsonl")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	raw, _ := json.Marshal(ev)
	_, err = f.Write(append(raw, '\n'))
	return err
}

// Query 查询事件（时间倒序）。
type Query struct {
	Days  int    // 最近几天（0 = 今天）
	Did   string // 设备过滤
	Type  string // 类型过滤
	Limit int
}

func (s *Store) Query(q Query) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()

	if q.Limit <= 0 {
		q.Limit = 100
	}
	days := q.Days
	if days <= 0 {
		days = 1
	}

	var out []Event
	now := time.Now()
	for i := 0; i < days && len(out) < q.Limit; i++ {
		day := now.AddDate(0, 0, -i).Format("2006-01-02")
		evs := s.readDayLocked(day)
		for j := len(evs) - 1; j >= 0; j-- {
			e := evs[j]
			if q.Did != "" && e.Did != q.Did {
				continue
			}
			if q.Type != "" && e.Type != q.Type {
				continue
			}
			out = append(out, e)
			if len(out) >= q.Limit {
				break
			}
		}
	}
	return out
}

// CountToday 今日事件数。
func (s *Store) CountToday() int {
	return len(s.Query(Query{Days: 1, Limit: 100000}))
}

func (s *Store) readDayLocked(day string) []Event {
	raw, err := os.ReadFile(filepath.Join(s.dir, day+".jsonl"))
	if err != nil {
		return nil
	}
	var out []Event
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e Event
		if json.Unmarshal([]byte(line), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

// SaveThumb 保存事件缩略图，返回文件名。
func (s *Store) SaveThumb(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	name := fmt.Sprintf("%d.jpg", time.Now().UnixNano())
	_ = os.WriteFile(filepath.Join(s.thumbDir, name), data, 0o644)
	return name
}

// ThumbPath 返回缩略图路径。
func (s *Store) ThumbPath(name string) string {
	return filepath.Join(s.thumbDir, filepath.Base(name))
}

// Cleanup 清理过期事件文件。
func (s *Store) Cleanup() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	deadline := time.Now().AddDate(0, 0, -s.keepDays)
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, err
	}
	var n int
	for _, ent := range ents {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".jsonl") {
			continue
		}
		day := strings.TrimSuffix(ent.Name(), ".jsonl")
		t, err := time.Parse("2006-01-02", day)
		if err != nil || !t.Before(deadline) {
			continue
		}
		if os.Remove(filepath.Join(s.dir, ent.Name())) == nil {
			n++
		}
	}
	return n, nil
}

// Days 返回有事件的日期（倒序）。
func (s *Store) Days(limit int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, ent := range ents {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".jsonl") {
			continue
		}
		out = append(out, strings.TrimSuffix(ent.Name(), ".jsonl"))
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}
