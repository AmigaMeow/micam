package main

// 事件标记与批量操作（对齐效果图 P0-3）。
//
// ===== 为什么需要单独存 =====
//
// 事件本身是**只追加**的（events.jsonl），
// 但「标记」「标签」是**可变**的用户数据 —— 混在事件里会导致
// 重写整个文件。所以分开存：
//
//   events/           只追加的事件（不动）
//   event_marks.json  可变的标记/标签（小文件，整体重写）
//
// ===== 标记类型 =====
//
//   bookmark     书签（卡片上的书签图标）
//   watch        需关注（红色徽章）
//   label        自定义标签

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// eventMark 一个事件的用户标记
type eventMark struct {
	Bookmark  bool     `json:"bookmark,omitempty"`
	Watch     bool     `json:"watch,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	UpdatedAt string   `json:"updatedAt,omitempty"`
}

type markStore struct {
	mu   sync.Mutex
	path string
	data map[string]eventMark // event id -> mark
}

func newMarkStore(dataDir string) *markStore {
	s := &markStore{
		path: filepath.Join(dataDir, "event_marks.json"),
		data: map[string]eventMark{},
	}
	if raw, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(raw, &s.data)
	}
	return s
}

func (s *markStore) get(id string) eventMark {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data[id]
}

// list 返回全部标记（前端一次性拉，本地量小）
func (s *markStore) list() map[string]eventMark {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]eventMark, len(s.data))
	for k, v := range s.data {
		out[k] = v
	}
	return out
}

// set 更新一条标记。空标记会被删除（保持文件小）。
func (s *markStore) set(id string, m eventMark) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m.UpdatedAt = time.Now().Format("2006-01-02 15:04:05")
	if !m.Bookmark && !m.Watch && len(m.Tags) == 0 {
		delete(s.data, id)
	} else {
		s.data[id] = m
	}
	return s.saveLocked()
}

// bulk 批量设置
func (s *markStore) bulk(ids []string, fn func(eventMark) eventMark) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Format("2006-01-02 15:04:05")
	for _, id := range ids {
		m := fn(s.data[id])
		m.UpdatedAt = now
		if !m.Bookmark && !m.Watch && len(m.Tags) == 0 {
			delete(s.data, id)
		} else {
			s.data[id] = m
		}
	}
	return s.saveLocked()
}

// deleteMarks 删除若干事件的标记
func (s *markStore) deleteMarks(ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		delete(s.data, id)
	}
	return s.saveLocked()
}

func (s *markStore) saveLocked() error {
	raw, _ := json.MarshalIndent(s.data, "", "  ")
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// ---- HTTP ----

// handleEventMarks 读写事件标记。
//
//	GET  /api/events/marks                       全部标记
//	POST /api/events/marks  {ids:[], mark:{...}}  设置
//	POST /api/events/marks  {ids:[], remove:true} 清除
func (s *Server) handleEventMarks(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]any{"marks": s.marks.list()})
		return
	}

	// 用 RawMessage 区分「没传」和「传了 false」
	var req struct {
		ID       string          `json:"id"`
		IDs      []string        `json:"ids"`
		Remove   bool            `json:"remove"`
		Bookmark *bool           `json:"bookmark"`
		Watch    *bool           `json:"watch"`
		Tags     []string        `json:"tags"`
		AddTags  []string        `json:"addTags"`
		Mark     json.RawMessage `json:"mark"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "参数错误")
		return
	}

	ids := req.IDs
	if req.ID == "" && len(ids) == 0 {
		writeErr(w, 400, "缺少 id 或 ids")
		return
	}
	if len(ids) == 0 {
		ids = []string{req.ID}
	}

	if req.Remove {
		if err := s.marks.deleteMarks(ids); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "count": len(ids)})
		return
	}

	err := s.marks.bulk(ids, func(cur eventMark) eventMark {
		if req.Bookmark != nil {
			cur.Bookmark = *req.Bookmark
		}
		if req.Watch != nil {
			cur.Watch = *req.Watch
		}
		if req.Tags != nil {
			cur.Tags = req.Tags
		}
		// addTags 是追加（批量打标签用），去重
		for _, t := range req.AddTags {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			found := false
			for _, x := range cur.Tags {
				if x == t {
					found = true
					break
				}
			}
			if !found {
				cur.Tags = append(cur.Tags, t)
			}
		}
		return cur
	})
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "count": len(ids)})
}

// handleEventBulk 批量操作：删除事件 / 导出。
//
//	POST /api/events/bulk  {ids:[], action:"delete"|"archive"}
func (s *Server) handleEventBulk(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs    []string `json:"ids"`
		Action string   `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.IDs) == 0 {
		writeErr(w, 400, "参数错误")
		return
	}

	switch req.Action {
	case "delete":
		// 删标记 + 缩略图 + 事件行
		n, err := s.eventStore.DeleteEvents(req.IDs)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		_ = s.marks.deleteMarks(req.IDs)
		writeJSON(w, 200, map[string]any{"ok": true, "deleted": n})

	case "exports":
		writeJSON(w, 200, map[string]any{"ok": true, "note": "导出功能在录像管理里"})

	default:
		writeErr(w, 400, "未知 action")
	}
}
