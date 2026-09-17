package main

// API Token 管理（开发者页「API Token」Tab）。
//
// 设计要点
//   · 明文 token **只在创建时返回一次**，磁盘只留 sha256 哈希
//     （api-tokens.json 泄露 != token 泄露）
//   · 存 <dataDir>/api-tokens.json，权限 0600
//   · **向后兼容**：不带 Authorization 头的请求不进校验分支，
//     所以现有 UI / 局域网直连的行为完全不变
//
// 接口
//   GET    /api/dev/tokens            列表（不含哈希与明文）
//   POST   /api/dev/tokens            新建 {name, scopes}
//   PATCH  /api/dev/tokens            {id, name?, scopes?, enabled?}
//   DELETE /api/dev/tokens?id=xxx

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// tokenScheme token 前缀，便于在日志/配置里一眼认出来
	tokenScheme = "mcam_"
	// maxTokens 上限：防止误点刷出几千条
	maxTokens = 50
	// lastUsedWriteGap 两次落盘 lastUsedAt 的最小间隔
	lastUsedWriteGap = 5 * time.Minute
)

var errTooManyTokens = errors.New("Token 数量已达上限（" + strconv.Itoa(maxTokens) + "）")

// apiToken 一条 API Token。Hash 绝不下发前端。
type apiToken struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Scopes     string     `json:"scopes"` // rw=读写 ro=只读
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	Enabled    bool       `json:"enabled"`
	Hash       string     `json:"hash"`
	Prefix     string     `json:"prefix"`
}

// public 去掉哈希后的视图（给前端）。
func (t *apiToken) public() map[string]any {
	m := map[string]any{
		"id":        t.ID,
		"name":      t.Name,
		"scopes":    t.Scopes,
		"createdAt": t.CreatedAt,
		"enabled":   t.Enabled,
		"prefix":    t.Prefix,
	}
	if t.LastUsedAt != nil {
		m["lastUsedAt"] = t.LastUsedAt
	}
	return m
}

type tokenStore struct {
	mu        sync.Mutex
	path      string
	items     []*apiToken
	lastWrite map[string]time.Time // id -> 上次为 lastUsedAt 落盘的时间
}

func newTokenStore(dataDir string) *tokenStore {
	s := &tokenStore{
		path:      filepath.Join(dataDir, "api-tokens.json"),
		lastWrite: map[string]time.Time{},
	}
	s.load()
	return s
}

func (s *tokenStore) load() {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return // 文件不存在 = 还没建过 token
	}
	var items []*apiToken
	if err := json.Unmarshal(raw, &items); err != nil {
		logf("[dev] 读取 api-tokens.json 失败（已忽略）: %v", err)
		return
	}
	s.items = items
}

func (s *tokenStore) saveLocked() error {
	raw, err := json.MarshalIndent(s.items, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, raw, 0o600)
}

func hashToken(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

func newTokenValue() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return tokenScheme + hex.EncodeToString(buf), nil
}

func newTokenID() string {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(buf)
}

// create 新建 token，返回**明文**（此后无法再取回）。
func (s *tokenStore) create(name, scopes string) (string, *apiToken, error) {
	val, err := newTokenValue()
	if err != nil {
		return "", nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.items) >= maxTokens {
		return "", nil, errTooManyTokens
	}
	it := &apiToken{
		ID:        newTokenID(),
		Name:      name,
		Scopes:    scopes,
		CreatedAt: time.Now(),
		Enabled:   true,
		Hash:      hashToken(val),
		Prefix:    val[:len(tokenScheme)+8],
	}
	s.items = append(s.items, it)
	if err := s.saveLocked(); err != nil {
		return "", nil, err
	}
	return val, it, nil
}

// update 改名 / 改权限 / 启停。
func (s *tokenStore) update(id string, name, scopes *string, enabled *bool) (*apiToken, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range s.items {
		if it.ID != id {
			continue
		}
		if name != nil {
			it.Name = *name
		}
		if scopes != nil {
			it.Scopes = *scopes
		}
		if enabled != nil {
			it.Enabled = *enabled
		}
		if err := s.saveLocked(); err != nil {
			logf("[dev] 保存 api-tokens.json 失败: %v", err)
		}
		cp := *it
		return &cp, true
	}
	return nil, false
}

func (s *tokenStore) remove(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, it := range s.items {
		if it.ID != id {
			continue
		}
		s.items = append(s.items[:i], s.items[i+1:]...)
		if err := s.saveLocked(); err != nil {
			logf("[dev] 保存 api-tokens.json 失败: %v", err)
		}
		return true
	}
	return false
}

// list 返回全部 token 的前端视图（创建时间倒序）。
func (s *tokenStore) list() []map[string]any {
	s.mu.Lock()
	items := make([]*apiToken, len(s.items))
	copy(items, s.items)
	s.mu.Unlock()

	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		out = append(out, it.public())
	}
	return out
}

func (s *tokenStore) counts() (total, enabled int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range s.items {
		total++
		if it.Enabled {
			enabled++
		}
	}
	return total, enabled
}

// verify 校验明文 token；返回 nil 表示无效或已禁用。
//
// 返回的是**副本**：调用方解锁后再读字段不会有数据竞争。
func (s *tokenStore) verify(val string) *apiToken {
	h := hashToken(val)
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range s.items {
		if it.Hash != h {
			continue
		}
		if !it.Enabled {
			return nil
		}
		it.LastUsedAt = &now
		// lastUsedAt 不必每个请求都写盘：内存里一直是最新的，
		// 磁盘 5 分钟落一次足够（这个字段只用于界面展示）
		if last, ok := s.lastWrite[it.ID]; !ok || now.Sub(last) >= lastUsedWriteGap {
			s.lastWrite[it.ID] = now
			if err := s.saveLocked(); err != nil {
				logf("[dev] 保存 api-tokens.json 失败: %v", err)
			}
		}
		cp := *it
		return &cp
	}
	return nil
}

// normalizeScopes 把前端给的权限字符串收敛成 rw / ro。
func normalizeScopes(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "ro", "read", "readonly", "只读":
		return "ro"
	default:
		return "rw"
	}
}

// normalizeTokenName 清洗名称：去空白、限长、留空给默认值。
func normalizeTokenName(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "未命名 Token"
	}
	r := []rune(v)
	if len(r) > 40 {
		v = string(r[:40])
	}
	return v
}

// handleDevTokens 增删改查入口（按 HTTP 方法分派）。
func (s *Server) handleDevTokens(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Dev.Unlocked {
		writeErr(w, 403, "开发者模式未开启")
		return
	}

	switch r.Method {
	case http.MethodGet:
		items := s.tokens.list()
		writeJSON(w, 200, map[string]any{"items": items, "count": len(items), "limit": maxTokens})

	case http.MethodPost:
		var req struct {
			Name   string `json:"name"`
			Scopes string `json:"scopes"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, "参数错误")
			return
		}
		val, it, err := s.tokens.create(normalizeTokenName(req.Name), normalizeScopes(req.Scopes))
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		logf("开发者页新建 API Token: %s（权限 %s）", it.Name, it.Scopes)
		writeJSON(w, 200, map[string]any{
			"ok":    true,
			"token": val, // 仅此一次
			"item":  it.public(),
			"note":  "请立即保存：服务端只存哈希，关闭后无法再次查看完整 Token",
		})

	case http.MethodPatch:
		var req struct {
			ID      string  `json:"id"`
			Name    *string `json:"name"`
			Scopes  *string `json:"scopes"`
			Enabled *bool   `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, "参数错误")
			return
		}
		if req.ID == "" {
			writeErr(w, 400, "缺少 id")
			return
		}
		if req.Name != nil {
			n := normalizeTokenName(*req.Name)
			req.Name = &n
		}
		if req.Scopes != nil {
			sc := normalizeScopes(*req.Scopes)
			req.Scopes = &sc
		}
		it, ok := s.tokens.update(req.ID, req.Name, req.Scopes, req.Enabled)
		if !ok {
			writeErr(w, 404, "Token 不存在")
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "item": it.public()})

	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if id == "" {
			writeErr(w, 400, "缺少 id")
			return
		}
		if !s.tokens.remove(id) {
			writeErr(w, 404, "Token 不存在")
			return
		}
		logf("开发者页删除 API Token: %s", id)
		writeJSON(w, 200, map[string]any{"ok": true})

	default:
		writeErr(w, 405, "方法不允许")
	}
}
