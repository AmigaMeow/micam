package store

// 账号凭据与扫码会话的存储。
//
// 安全：凭据文件权限 0600，只存 userId + passToken（不存密码）。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/micam/micam/internal/xiaomi"
)

// Account 一个已登录的小米账号。
type Account struct {
	UserID    string `json:"userId"`
	PassToken string `json:"passToken"`
	Nickname  string `json:"nickname,omitempty"`
	AddedAt   string `json:"addedAt"`
}

// QRState 扫码状态。
type QRState string

const (
	QRWaiting QRState = "waiting"
	QRScanned QRState = "scanned"
	QRSuccess QRState = "success"
	QRFailed  QRState = "failed"
)

// QRSession 一次扫码会话。
type QRSession struct {
	ID        string
	State     QRState
	UserID    string
	Error     string
	CreatedAt time.Time
	sess      *xiaomi.Session
	qr        *xiaomi.QRLogin
	done      chan struct{}
}

// Store 内存 + 文件存储。
type Store struct {
	mu       sync.Mutex
	dataDir  string
	accounts []Account
	sessions map[string]*xiaomi.Session // userId -> session
	qr       map[string]*QRSession
	devices  map[string]*xiaomi.Device // did -> device（设备发现缓存）
	devMu    sync.RWMutex
	sources  map[string]string // did -> 完整 xiaomi:// 源（缓存，减少云端调用）
	srcMu    sync.RWMutex
}

// New 创建 store 并加载已有账号。
func New(dataDir string) *Store {
	s := &Store{
		dataDir:  dataDir,
		sessions: map[string]*xiaomi.Session{},
		qr:       map[string]*QRSession{},
		devices:  map[string]*xiaomi.Device{},
		sources:  map[string]string{},
	}
	s.loadAccounts()
	s.loadSources()
	return s
}

func (s *Store) accountsFile() string {
	return filepath.Join(s.dataDir, "accounts.json")
}

// sourcesFile 源缓存文件。
func (s *Store) sourcesFile() string {
	return filepath.Join(s.dataDir, "sources.json")
}

// loadSources 加载源缓存。
func (s *Store) loadSources() {
	raw, err := os.ReadFile(s.sourcesFile())
	if err != nil {
		return
	}
	var m map[string]string
	if json.Unmarshal(raw, &m) == nil {
		s.sources = m
	}
}

// saveSources 保存源缓存。
func (s *Store) saveSources() error {
	raw, err := json.MarshalIndent(s.sources, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.sourcesFile(), raw, 0o600)
}

func (s *Store) loadAccounts() {
	raw, err := os.ReadFile(s.accountsFile())
	if err != nil {
		return
	}
	var list []Account
	if json.Unmarshal(raw, &list) != nil {
		return
	}
	s.accounts = list

	// 自动恢复会话
	for _, a := range list {
		sess := xiaomi.NewSession()
		if err := sess.SetTokens(a.UserID, a.PassToken); err == nil {
			s.sessions[a.UserID] = sess
		}
	}
}

func (s *Store) saveAccounts() error {
	raw, err := json.MarshalIndent(s.accounts, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.accountsFile(), raw, 0o600)
}

// ReloadAccounts 重新从磁盘加载账号（修复用）。
func (s *Store) ReloadAccounts() {
	s.mu.Lock()
	s.accounts = nil
	s.sessions = map[string]*xiaomi.Session{}
	s.mu.Unlock()
	s.loadAccounts()
}

// Accounts 返回账号列表（不含敏感字段）。
func (s *Store) Accounts() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]any, 0, len(s.accounts))
	for _, a := range s.accounts {
		out = append(out, map[string]any{
			"userId":  a.UserID,
			"addedAt": a.AddedAt,
			"online":  s.sessions[a.UserID] != nil,
		})
	}
	return out
}

// FirstSession 取第一个可用会话（按账号顺序，保证稳定）。
func (s *Store) FirstSession() *xiaomi.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.accounts {
		if sess := s.sessions[a.UserID]; sess != nil {
			return sess
		}
	}
	return nil
}

// AllSessions 返回所有可用会话（按账号顺序）。
func (s *Store) AllSessions() []*xiaomi.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*xiaomi.Session, 0, len(s.accounts))
	for _, a := range s.accounts {
		if sess := s.sessions[a.UserID]; sess != nil {
			out = append(out, sess)
		}
	}
	return out
}

// HasAccounts 是否已有账号。
func (s *Store) HasAccounts() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.accounts) > 0
}

// CacheDevices 缓存设备（按 did）。
func (s *Store) CacheDevices(devs []xiaomi.Device) {
	s.devMu.Lock()
	defer s.devMu.Unlock()
	for i := range devs {
		d := devs[i]
		s.devices[d.Did] = &d
	}
}

// Device 按 did 取设备。
func (s *Store) Device(did string) (*xiaomi.Device, bool) {
	s.devMu.RLock()
	defer s.devMu.RUnlock()
	d, ok := s.devices[did]
	return d, ok
}

// SaveSource 缓存设备的完整源地址（带 P2P 凭据），并落盘。
func (s *Store) SaveSource(did, src string) {
	s.srcMu.Lock()
	s.sources[did] = src
	s.srcMu.Unlock()
	_ = s.saveSources()
}

// Source 取缓存的源地址。
func (s *Store) Source(did string) (string, bool) {
	s.srcMu.RLock()
	defer s.srcMu.RUnlock()
	v, ok := s.sources[did]
	return v, ok
}

// SourceDids 返回所有有缓存源的 did。
func (s *Store) SourceDids() []string {
	s.srcMu.RLock()
	defer s.srcMu.RUnlock()
	out := make([]string, 0, len(s.sources))
	for did := range s.sources {
		out = append(out, did)
	}
	return out
}

// Devices 返回缓存的全部设备。
func (s *Store) Devices() []*xiaomi.Device {
	s.devMu.RLock()
	defer s.devMu.RUnlock()
	out := make([]*xiaomi.Device, 0, len(s.devices))
	for _, d := range s.devices {
		out = append(out, d)
	}
	return out
}

// Session 取指定账号会话。
func (s *Store) Session(userID string) *xiaomi.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[userID]
}

// AddAccount 保存账号。
func (s *Store) AddAccount(sess *xiaomi.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := sess.Token()
	a := Account{
		UserID:    t["userId"],
		PassToken: t["passToken"],
		AddedAt:   time.Now().Format(time.RFC3339),
	}
	for i, old := range s.accounts {
		if old.UserID == a.UserID {
			s.accounts[i] = a
			s.sessions[a.UserID] = sess
			return s.saveAccounts()
		}
	}
	s.accounts = append(s.accounts, a)
	s.sessions[a.UserID] = sess
	return s.saveAccounts()
}

// ---- 扫码会话 ----

// SaveQR 注册一个扫码会话并后台等待。
func (s *Store) SaveQR(sess *xiaomi.Session, qr *xiaomi.QRLogin) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := sess.UserID
	// userId 还没拿到，用时间戳做临时 id
	id = time.Now().Format("20060102150405.000000")
	qs := &QRSession{
		ID:        id,
		State:     QRWaiting,
		CreatedAt: time.Now(),
		sess:      sess,
		qr:        qr,
		done:      make(chan struct{}),
	}
	s.qr[id] = qs

	go func() {
		qs.State = QRScanned
		err := qr.Wait()
		if err != nil {
			qs.State = QRFailed
			qs.Error = err.Error()
			return
		}
		_ = s.AddAccount(sess)
		qs.UserID = sess.UserID
		qs.State = QRSuccess
		close(qs.done)
	}()

	// 清理过期会话
	go func() {
		time.Sleep(time.Duration(qr.Timeout+30) * time.Second)
		s.mu.Lock()
		delete(s.qr, id)
		s.mu.Unlock()
	}()

	return id
}

// GetQR 取扫码会话。
func (s *Store) GetQR(id string) (*QRSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	qs, ok := s.qr[id]
	return qs, ok
}

// SaveQRImage 暂存二维码图片。
func (s *Store) SaveQRImage(id string, img []byte) error {
	return os.WriteFile(filepath.Join(s.dataDir, "qr_"+id+".png"), img, 0o600)
}
