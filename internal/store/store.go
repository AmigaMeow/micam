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

// CredSet 一组设备凭据。
//
// 为什么保存历史而不是只留最新：
//
//	miss_get_vendor 有严格限流（实测可封 24 小时+）。
//	如果只留最新一份，一旦刷新失败就彻底没得用。
//	保留多份历史 -> 可以逐个试，只有全部失效才需要调接口。
//
// 这也大幅降低调用频率：旧的往往还能用。
type CredSet struct {
	Src  string    `json:"src"`
	Time time.Time `json:"time"`
}

// maxCredSets 每台设备保留几组凭据。
const maxCredSets = 5

// Store 内存 + 文件存储。
type Store struct {
	mu       sync.Mutex
	dataDir  string
	accounts []Account
	sessions map[string]*xiaomi.Session // userId -> session
	qr       map[string]*QRSession
	devices  map[string]*xiaomi.Device // did -> device（设备发现缓存）
	devMu    sync.RWMutex
	sources  map[string][]CredSet // did -> 凭据历史（最新在前）

	srcMu    sync.RWMutex
}

// New 创建 store 并加载已有账号。
func New(dataDir string) *Store {
	s := &Store{
		dataDir:  dataDir,
		sessions: map[string]*xiaomi.Session{},
		qr:       map[string]*QRSession{},
		devices:  map[string]*xiaomi.Device{},
		sources:  map[string][]CredSet{},
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

// loadSources 加载凭据缓存（兼容三种历史格式）。
func (s *Store) loadSources() {
	raw, err := os.ReadFile(s.sourcesFile())
	if err != nil {
		return
	}

	// 格式 3（当前）：{did: [{src,time}, ...]}
	var m3 map[string][]CredSet
	if json.Unmarshal(raw, &m3) == nil && len(m3) > 0 {
		for did, list := range m3 {
			if len(list) > 0 {
				s.sources[did] = list
			}
		}
		if len(s.sources) > 0 {
			return
		}
	}

	// 格式 2：{did: {src, time}}
	var m2 map[string]struct {
		Src  string    `json:"src"`
		Time time.Time `json:"time"`
	}
	if json.Unmarshal(raw, &m2) == nil && len(m2) > 0 {
		for did, v := range m2 {
			s.sources[did] = []CredSet{{Src: v.Src, Time: v.Time}}
		}
		if len(s.sources) > 0 {
			return
		}
	}

	// 格式 1：{did: src}
	var m1 map[string]string
	if json.Unmarshal(raw, &m1) == nil {
		for did, src := range m1 {
			s.sources[did] = []CredSet{{Src: src}}
		}
	}
}

// saveSources 保存凭据缓存。
func (s *Store) saveSources() error {
	s.srcMu.RLock()
	raw, err := json.MarshalIndent(s.sources, "", "  ")
	s.srcMu.RUnlock()
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

// SessionForDevice 找能访问该设备的账号会话。
//
// 关键：不同设备属于不同账号，用错账号会返回 -704010000。
func (s *Store) SessionForDevice(did string) *xiaomi.Session {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 遍历所有账号，找到包含该设备的
	for _, a := range s.accounts {
		sess := s.sessions[a.UserID]
		if sess == nil {
			continue
		}
		// 查自有设备
		if devs, err := sess.Devices(); err == nil {
			for _, d := range devs {
				if d.Did == did {
					return sess
				}
			}
		}
		// 查共享设备
		if homes, err := sess.Homes(); err == nil {
			for _, h := range homes {
				if h.ShareFlag == 0 {
					continue
				}
				if devs, err := sess.SharedDevices(h.ID, h.OwnerID()); err == nil {
					for _, d := range devs {
						if d.Did == did {
							return sess
						}
					}
				}
			}
		}
	}
	// 找不到就返回第一个
	for _, a := range s.accounts {
		if sess := s.sessions[a.UserID]; sess != nil {
			return sess
		}
	}
	return nil
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

// SaveSource 缓存一组设备凭据（插到最前），并落盘。
//
// 已存在相同 src 时更新时间并提到最前（不重复保存）。
func (s *Store) SaveSource(did, src string) {
	s.srcMu.Lock()
	defer s.srcMu.Unlock()

	list := s.sources[did]
	out := make([]CredSet, 0, len(list)+1)
	out = append(out, CredSet{Src: src, Time: time.Now()})
	for _, c := range list {
		if c.Src == src {
			continue
		}
		out = append(out, c)
		if len(out) >= maxCredSets {
			break
		}
	}
	s.sources[did] = out
	go s.saveSources()
}

// Sources 返回该设备的所有凭据（最新在前）。
func (s *Store) Sources(did string) []CredSet {
	s.srcMu.RLock()
	defer s.srcMu.RUnlock()
	list := s.sources[did]
	out := make([]CredSet, len(list))
	copy(out, list)
	return out
}

// PromoteSource 把某个凭据提到最前（验证可用后调用）。
func (s *Store) PromoteSource(did, src string) {
	s.srcMu.Lock()
	defer s.srcMu.Unlock()

	list := s.sources[did]
	if len(list) == 0 || list[0].Src == src {
		return
	}

	var hit *CredSet
	out := make([]CredSet, 0, len(list))
	for i := range list {
		if list[i].Src == src {
			c := list[i]
			c.Time = time.Now()
			hit = &c
			continue
		}
		out = append(out, list[i])
	}
	if hit == nil {
		return
	}
	ns := make([]CredSet, 0, len(list))
	ns = append(ns, *hit)
	ns = append(ns, out...)
	s.sources[did] = ns
	go s.saveSources()
}

// DemoteSource 把某个凭据移到列表末尾（失败但暂不剔除）。
func (s *Store) DemoteSource(did, src string) {
	s.srcMu.Lock()
	defer s.srcMu.Unlock()

	list := s.sources[did]
	if len(list) <= 1 {
		return
	}
	// 已经在末尾就不动
	if list[len(list)-1].Src == src {
		return
	}

	var hit *CredSet
	out := make([]CredSet, 0, len(list))
	for i := range list {
		if list[i].Src == src {
			c := list[i]
			hit = &c
			continue
		}
		out = append(out, list[i])
	}
	if hit == nil {
		return
	}
	s.sources[did] = append(out, *hit)
	go s.saveSources()
}

// DropSourceAt 删除某个具体凭据（确认失效）。
func (s *Store) DropSourceAt(did, src string) {
	s.srcMu.Lock()
	defer s.srcMu.Unlock()

	list := s.sources[did]
	out := make([]CredSet, 0, len(list))
	for _, c := range list {
		if c.Src == src {
			continue
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		delete(s.sources, did)
	} else {
		s.sources[did] = out
	}
	go s.saveSources()
}

// SourceAge 返回最新凭据的年龄。
func (s *Store) SourceAge(did string) time.Duration {
	s.srcMu.RLock()
	defer s.srcMu.RUnlock()
	list := s.sources[did]
	if len(list) == 0 || list[0].Time.IsZero() {
		return 24 * time.Hour
	}
	return time.Since(list[0].Time)
}

// DropSource 删除该设备的全部凭据。
func (s *Store) DropSource(did string) {
	s.srcMu.Lock()
	delete(s.sources, did)
	s.srcMu.Unlock()
	go s.saveSources()
}

// Source 取最新的一组凭据。
func (s *Store) Source(did string) (string, bool) {
	s.srcMu.RLock()
	defer s.srcMu.RUnlock()
	list := s.sources[did]
	if len(list) == 0 {
		return "", false
	}
	return list[0].Src, true
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
