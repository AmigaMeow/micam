package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// WhitelistItem 熟悉目标与车辆白名单
type WhitelistItem struct {
	ID        string `json:"id"`
	Type      string `json:"type"`      // "vehicle" / "person" / "pet"
	Value     string `json:"value"`     // 车牌（如 "鄂W61J59"）或标识
	Name      string `json:"name"`      // 名称（如 "家庭自用车" / "家人"）
	Policy    string `json:"policy"`    // "silent" (免打扰/静默记录), "trusted" (可信任), "vip" (重点关注)
	Note      string `json:"note"`      // 备注
	CreatedAt string `json:"createdAt"`
}

type whitelistStore struct {
	mu    sync.RWMutex
	path  string
	items []*WhitelistItem
}

var gWhitelistStore *whitelistStore

func initWhitelistStore(dataDir string) {
	gWhitelistStore = &whitelistStore{
		path: filepath.Join(dataDir, "configs", "ai_whitelist.json"),
	}
	gWhitelistStore.load()
}

// 归一化车牌（去除中点、空格、横杠）
func normalizePlate(p string) string {
	p = strings.ToUpper(strings.TrimSpace(p))
	p = strings.ReplaceAll(p, "·", "")
	p = strings.ReplaceAll(p, "•", "")
	p = strings.ReplaceAll(p, " ", "")
	p = strings.ReplaceAll(p, "-", "")
	p = strings.ReplaceAll(p, "_", "")
	return p
}

// runeDiff 统计两个字符串的汉字/字母差异数（长度相同时）
func runeDiff(a, b string) int {
	ra := []rune(a)
	rb := []rune(b)
	if len(ra) != len(rb) {
		return 999
	}
	diff := 0
	for i := range ra {
		if ra[i] != rb[i] {
			diff++
		}
	}
	return diff
}

// isPlateFuzzyMatch 智能容错 1 位字符 OCR 抖动（如 粤W61J59 vs 鄂W61J59）或后5位字符完全相同
func isPlateFuzzyMatch(candidate, target string) bool {
	cNorm := normalizePlate(candidate)
	tNorm := normalizePlate(target)
	if cNorm == "" || tNorm == "" {
		return false
	}
	if cNorm == tNorm {
		return true
	}
	if runeDiff(cNorm, tNorm) <= 1 {
		return true
	}
	rc := []rune(cNorm)
	rt := []rune(tNorm)
	if len(rc) >= 6 && len(rt) >= 6 {
		suffixC := string(rc[len(rc)-5:])
		suffixT := string(rt[len(rt)-5:])
		if suffixC == suffixT {
			return true
		}
	}
	// 针对 OCR 漏识 1 位（如 湘W6J59 vs 鄂W61J59）
	if len(rc) >= 5 && len(rt) >= 6 {
		sC := string(rc[1:])
		sT := string(rt[1:])
		if strings.Contains(sT, sC) || strings.Contains(sC, sT) {
			return true
		}
	}
	return false
}

func (ws *whitelistStore) load() {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	ws.items = make([]*WhitelistItem, 0)
	data, err := os.ReadFile(ws.path)
	if err != nil {
		// 自动初始化默认家庭自用车白名单（基于已检测车牌 鄂W61J59）
		defaultItem := &WhitelistItem{
			ID:        "wl-car-ew61j59",
			Type:      "vehicle",
			Value:     "鄂W61J59",
			Name:      "家庭自用车",
			Policy:    "silent",
			Note:      "庭院停放自用车（静默记录归档，免打扰推送）",
			CreatedAt: time.Now().Format("2006-01-02 15:04:05"),
		}
		ws.items = append(ws.items, defaultItem)
		ws.saveLocked()
		return
	}

	var list []*WhitelistItem
	if err := json.Unmarshal(data, &list); err == nil {
		ws.items = list
	}
}

func (ws *whitelistStore) saveLocked() {
	if ws.path == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(ws.path), 0755)
	data, err := json.MarshalIndent(ws.items, "", "  ")
	if err == nil {
		_ = os.WriteFile(ws.path, data, 0644)
	}
}

func (ws *whitelistStore) List() []*WhitelistItem {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	res := make([]*WhitelistItem, len(ws.items))
	copy(res, ws.items)
	return res
}

func (ws *whitelistStore) MatchPlate(plate string) *WhitelistItem {
	if plate == "" {
		return nil
	}
	norm := normalizePlate(plate)
	if norm == "" {
		return nil
	}

	ws.mu.RLock()
	defer ws.mu.RUnlock()

	for _, item := range ws.items {
		if item == nil || item.Type != "vehicle" {
			continue
		}
		itemNorm := normalizePlate(item.Value)
		if itemNorm == norm || isPlateFuzzyMatch(norm, itemNorm) {
			return item
		}
	}
	return nil
}

func (ws *whitelistStore) AddOrUpdate(item *WhitelistItem) error {
	if item == nil {
		return nil
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()

	if item.ID == "" {
		b := make([]byte, 8)
		_, _ = rand.Read(b)
		item.ID = "wl-" + hex.EncodeToString(b)
	}
	if item.CreatedAt == "" {
		item.CreatedAt = time.Now().Format("2006-01-02 15:04:05")
	}

	found := false
	for i, it := range ws.items {
		if it.ID == item.ID || (it.Type == item.Type && normalizePlate(it.Value) == normalizePlate(item.Value)) {
			ws.items[i] = item
			found = true
			break
		}
	}
	if !found {
		ws.items = append(ws.items, item)
	}
	ws.saveLocked()
	return nil
}

func (ws *whitelistStore) Delete(id string) bool {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	var next []*WhitelistItem
	found := false
	for _, it := range ws.items {
		if it.ID == id {
			found = true
			continue
		}
		next = append(next, it)
	}
	if found {
		ws.items = next
		ws.saveLocked()
	}
	return found
}

// handleAIWhitelist GET/POST /api/ai/whitelist
func (s *Server) handleAIWhitelist(w http.ResponseWriter, r *http.Request) {
	if gWhitelistStore == nil {
		initWhitelistStore(s.dataDir)
	}

	if r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]any{
			"ok":        true,
			"whitelist": gWhitelistStore.List(),
		})
		return
	}

	if r.Method == http.MethodPost {
		var item WhitelistItem
		if err := json.NewDecoder(r.Body).Decode(&item); err != nil {
			writeErr(w, 400, "参数格式错误: "+err.Error())
			return
		}
		if item.Value == "" {
			writeErr(w, 400, "缺少车牌或识别特征值")
			return
		}
		if item.Type == "" {
			item.Type = "vehicle"
		}
		if item.Name == "" {
			item.Name = "家庭自用车"
		}
		if item.Policy == "" {
			item.Policy = "silent"
		}
		_ = gWhitelistStore.AddOrUpdate(&item)
		logf("[whitelist] 添加/更新白名单: [%s] %s (%s, 策略:%s)", item.Type, item.Value, item.Name, item.Policy)
		writeJSON(w, 200, map[string]any{
			"ok":   true,
			"item": item,
		})
		return
	}

	writeErr(w, 405, "只支持 GET/POST")
}

// handleAIWhitelistDelete POST/DELETE /api/ai/whitelist/delete
func (s *Server) handleAIWhitelistDelete(w http.ResponseWriter, r *http.Request) {
	if gWhitelistStore == nil {
		initWhitelistStore(s.dataDir)
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		var body struct {
			ID string `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		id = body.ID
	}
	if id == "" {
		writeErr(w, 400, "缺少 id")
		return
	}

	ok := gWhitelistStore.Delete(id)
	logf("[whitelist] 删除白名单条目: id=%s (success=%v)", id, ok)
	writeJSON(w, 200, map[string]any{
		"ok":      true,
		"deleted": ok,
	})
}
