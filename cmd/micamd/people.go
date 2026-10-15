package main

// 人员库接口。
//
// 存储已知人员信息，并支持把事件关联到具体人员。
// 数据持久化在 /vol1/@appdata/micam/configs/people.json。
//
// API:
//   GET  /api/people           → 人员列表
//   POST /api/people           → 新建人员 {name,alias,color,note}
//   PUT  /api/people           → 更新人员 {id,name,alias,color,note}
//   DELETE /api/people?id=xxx  → 删除人员
//   POST /api/people/link      → 关联事件 {personId,eventId}
//   DELETE /api/people/link    → 解除关联 {personId,eventId}
//   GET  /api/people/events?id=xxx → 该人员的关联事件列表

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/micam/micam/internal/event"
)

// Person 一个已知人员
type Person struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Alias    string   `json:"alias,omitempty"`
	Color    string   `json:"color"`   // hex color for avatar
	Note     string   `json:"note,omitempty"`
	EventIDs []string `json:"eventIds,omitempty"` // 关联事件 ID 列表
	LastSeen string   `json:"lastSeen,omitempty"`
	CreatedAt string  `json:"createdAt"`
}

// personStore 人员库（内存 + JSON 文件持久化）
type personStore struct {
	mu      sync.RWMutex
	path    string
	persons []*Person
}

var gPersonStore *personStore

func initPersonStore(dataDir string) {
	gPersonStore = &personStore{
		path: filepath.Join(dataDir, "configs", "people.json"),
	}
	gPersonStore.load()
}

func (ps *personStore) load() {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	data, err := os.ReadFile(ps.path)
	if err != nil {
		ps.persons = []*Person{}
		return
	}
	if err := json.Unmarshal(data, &ps.persons); err != nil {
		ps.persons = []*Person{}
	}
}

func (ps *personStore) save() error {
	data, err := json.MarshalIndent(ps.persons, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(ps.path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(ps.path, data, 0o644)
}

func (ps *personStore) all() []*Person {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	out := make([]*Person, len(ps.persons))
	copy(out, ps.persons)
	return out
}

func (ps *personStore) byID(id string) *Person {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	for _, p := range ps.persons {
		if p.ID == id {
			return p
		}
	}
	return nil
}

func newPersonID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

var personColors = []string{
	"#ef4444", "#f97316", "#eab308", "#22c55e",
	"#3b82f6", "#8b5cf6", "#ec4899", "#14b8a6",
}

// handlePeople GET/POST/PUT/DELETE /api/people
func (s *Server) handlePeople(w http.ResponseWriter, r *http.Request) {
	if gPersonStore == nil {
		writeErr(w, 503, "人员库未初始化")
		return
	}
	switch r.Method {
	case http.MethodGet:
		persons := gPersonStore.all()
		writeJSON(w, 200, map[string]any{"ok": true, "persons": persons})

	case http.MethodPost:
		var req struct {
			Name  string `json:"name"`
			Alias string `json:"alias"`
			Color string `json:"color"`
			Note  string `json:"note"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
			writeErr(w, 400, "参数错误：name 必填")
			return
		}
		gPersonStore.mu.Lock()
		// 分配默认颜色
		color := req.Color
		if color == "" {
			color = personColors[len(gPersonStore.persons)%len(personColors)]
		}
		p := &Person{
			ID:        newPersonID(),
			Name:      req.Name,
			Alias:     req.Alias,
			Color:     color,
			Note:      req.Note,
			EventIDs:  []string{},
			CreatedAt: time.Now().Format("2006-01-02 15:04:05"),
		}
		gPersonStore.persons = append(gPersonStore.persons, p)
		gPersonStore.save()
		gPersonStore.mu.Unlock()
		writeJSON(w, 200, map[string]any{"ok": true, "person": p})

	case http.MethodPut:
		var req struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			Alias string `json:"alias"`
			Color string `json:"color"`
			Note  string `json:"note"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
			writeErr(w, 400, "参数错误：id 必填")
			return
		}
		gPersonStore.mu.Lock()
		var found *Person
		for _, p := range gPersonStore.persons {
			if p.ID == req.ID {
				found = p
				break
			}
		}
		if found == nil {
			gPersonStore.mu.Unlock()
			writeErr(w, 404, "人员不存在")
			return
		}
		if req.Name != "" {
			found.Name = req.Name
		}
		found.Alias = req.Alias
		if req.Color != "" {
			found.Color = req.Color
		}
		found.Note = req.Note
		gPersonStore.save()
		gPersonStore.mu.Unlock()
		writeJSON(w, 200, map[string]any{"ok": true, "person": found})

	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if id == "" {
			writeErr(w, 400, "id 必填")
			return
		}
		gPersonStore.mu.Lock()
		newList := make([]*Person, 0, len(gPersonStore.persons))
		found := false
		for _, p := range gPersonStore.persons {
			if p.ID == id {
				found = true
				continue
			}
			newList = append(newList, p)
		}
		gPersonStore.persons = newList
		gPersonStore.save()
		gPersonStore.mu.Unlock()
		if !found {
			writeErr(w, 404, "人员不存在")
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})

	default:
		writeErr(w, 405, "method not allowed")
	}
}

// handlePeopleLink POST/DELETE /api/people/link
func (s *Server) handlePeopleLink(w http.ResponseWriter, r *http.Request) {
	if gPersonStore == nil {
		writeErr(w, 503, "人员库未初始化")
		return
	}
	var req struct {
		PersonID string `json:"personId"`
		EventID  string `json:"eventId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.PersonID == "" || req.EventID == "" {
		writeErr(w, 400, "参数错误：personId 和 eventId 必填")
		return
	}
	gPersonStore.mu.Lock()
	defer gPersonStore.mu.Unlock()
	var p *Person
	for _, pp := range gPersonStore.persons {
		if pp.ID == req.PersonID {
			p = pp
			break
		}
	}
	if p == nil {
		writeErr(w, 404, "人员不存在")
		return
	}
	if r.Method == http.MethodDelete {
		// Unlink
		newIDs := make([]string, 0, len(p.EventIDs))
		for _, eid := range p.EventIDs {
			if eid != req.EventID {
				newIDs = append(newIDs, eid)
			}
		}
		p.EventIDs = newIDs
	} else {
		// Link — avoid duplicate
		exists := false
		for _, eid := range p.EventIDs {
			if eid == req.EventID {
				exists = true
				break
			}
		}
		if !exists {
			p.EventIDs = append(p.EventIDs, req.EventID)
			p.LastSeen = time.Now().Format("2006-01-02 15:04:05")
		}
	}
	gPersonStore.save()
	writeJSON(w, 200, map[string]any{"ok": true, "person": p})
}

// handlePeopleEvents GET /api/people/events?id=xxx
func (s *Server) handlePeopleEvents(w http.ResponseWriter, r *http.Request) {
	if gPersonStore == nil {
		writeErr(w, 503, "人员库未初始化")
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writeErr(w, 400, "id 必填")
		return
	}
	p := gPersonStore.byID(id)
	if p == nil {
		writeErr(w, 404, "人员不存在")
		return
	}
	// 把 eventIDs 转成 set，然后从 eventStore 捞出来
	idSet := make(map[string]bool)
	for _, eid := range p.EventIDs {
		idSet[eid] = true
	}
	allEvs := s.eventStore.Query(event.Query{Days: 90, Limit: 10000})
	var matched []event.Event
	for _, ev := range allEvs {
		if idSet[ev.ID] {
			matched = append(matched, ev)
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true, "events": matched, "person": p})
}
