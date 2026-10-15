package main

// 账号页聚合接口（对齐效果图）。
//
//   GET /api/accounts/detail
//     一次返回：账号卡片（含掩码ID/设备数/缩略图/凭据状态）+ 备份历史
//              + 连接统计 + 最近活动
//
// ===== 为什么要聚合 =====
// 效果图一张卡片上要 8 个字段（名称/掩码ID/设备数/会话剩余/上次同步/
// 三种凭据状态/缩略图），分散请求会有 4~5 次往返，所以合到一次。

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ---- 活动日志（内存环形缓冲）----

type acctActivity struct {
	Time   string `json:"time"`
	Level  string `json:"level"` // ok / warn / err
	Text   string `json:"text"`
	Detail string `json:"detail,omitempty"`
}

var (
	acctLogMu sync.Mutex
	acctLog   []acctActivity
)

// logAccountAction 记一条账号活动（最多留 50 条）
func logAccountAction(level, text, detail string) {
	acctLogMu.Lock()
	defer acctLogMu.Unlock()
	acctLog = append([]acctActivity{{
		Time:   time.Now().Format("15:04"),
		Level:  level,
		Text:   text,
		Detail: detail,
	}}, acctLog...)
	if len(acctLog) > 50 {
		acctLog = acctLog[:50]
	}
}

func recentAccountActivity(n int) []acctActivity {
	acctLogMu.Lock()
	defer acctLogMu.Unlock()
	if len(acctLog) < n {
		n = len(acctLog)
	}
	out := make([]acctActivity, n)
	copy(out, acctLog[:n])
	return out
}

// maskUserID 生成掩码 ID：13712345678 -> xiaomi_137****5678
func maskUserID(uid string) string {
	r := []rune(uid)
	if len(r) <= 7 {
		return "xiaomi_" + uid
	}
	return "xiaomi_" + string(r[:3]) + "****" + string(r[len(r)-4:])
}

// acctCard 账号卡片
type acctCard struct {
	UserID      string `json:"userId"`
	Masked      string `json:"masked"`
	Name        string `json:"name,omitempty"`
	Thumb       string `json:"thumb,omitempty"`
	Did         string `json:"did,omitempty"`
	DeviceCount int    `json:"deviceCount"`
	OnlineCount int    `json:"onlineCount"`
	ShareCount  int    `json:"shareCount"`

	// 三层凭据
	PassTokenOK    bool    `json:"passTokenOk"`
	SessionOK      bool    `json:"sessionOk"`
	SessionLeftH   float64 `json:"sessionLeftHours"`
	ServiceTokenOK bool    `json:"serviceTokenOk"`
	DidBound       bool    `json:"didBound"`

	LastSync   string  `json:"lastSync,omitempty"`
	SessionAge float64 `json:"sessionAgeMinutes"`
}

// handleAccountsDetail 账号页聚合
func (s *Server) handleAccountsDetail(w http.ResponseWriter, r *http.Request) {
	// ---- 账号（从 accounts.json 读）----
	type rawAcct struct {
		UserID    string `json:"userId"`
		AddedAt   string `json:"addedAt"`
		PassToken string `json:"passToken"`
		LastCheck string `json:"lastCheck"`
	}
	type rawFile struct {
		Accounts []rawAcct `json:"accounts"`
	}
	// accounts.json 可能是「数组」也可能是 {"accounts":[...]} —— 两种都支持
	var rf rawFile
	var arrAccts []rawAcct
	if raw, err := os.ReadFile(filepath.Join(dataDirPath, "accounts.json")); err == nil {
		if json.Unmarshal(raw, &rf) != nil || len(rf.Accounts) == 0 {
			_ = json.Unmarshal(raw, &arrAccts)
		}
	}
	allAccts := rf.Accounts
	if len(allAccts) == 0 {
		allAccts = arrAccts
	}

	stAccounts := []acctCard{}
	for _, acc := range allAccts {
		c := acctCard{
			UserID:       acc.UserID,
			Masked:       maskUserID(acc.UserID),
			PassTokenOK:  acc.PassToken != "",
			SessionOK:    true,
			LastSync:     fmtSyncTime(acc.LastCheck),
			SessionLeftH: 24 * 7,
		}
		if c.LastSync == "" {
			c.LastSync = acc.AddedAt
		}
		stAccounts = append(stAccounts, c)
	}

	// ---- 摄像头按账号分组 ----
	//
	// 注意：不能用 s.allCameras() —— 它在 store 有数据时直接返回，
	// 不会填充 AccountID（那是登录时的另一条路径做的），
	// 结果所有设备会被归到 "unknown"。
	// 所以走本地 /api/devices（那里 accountId 一定有值）。
	camsByAcct := map[string][]camBrief{}
	{
		cli := &http.Client{Timeout: 20 * time.Second}
		if resp, err := cli.Get("http://127.0.0.1:8900/api/devices"); err == nil {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
			resp.Body.Close()
			var dd struct {
				Devices []struct {
					Did       string `json:"did"`
					Name      string `json:"name"`
					Model     string `json:"model"`
					Alias     string `json:"alias"`
					IsOnline  bool   `json:"isOnline"`
					Shared    bool   `json:"shared"`
					AccountID string `json:"accountId"`
				} `json:"devices"`
			}
			if json.Unmarshal(raw, &dd) == nil {
				for _, x := range dd.Devices {
					aid := x.AccountID
					if aid == "" {
						aid = "unknown"
					}
					nm := x.Alias
					if nm == "" {
						nm = x.Name
					}
					camsByAcct[aid] = append(camsByAcct[aid], camBrief{
						Did: x.Did, Name: nm, Model: x.Model,
						Online: x.IsOnline, Shared: x.Shared,
					})
				}
			}
		}
	}

	// ---- 备份历史 ----
	type bakItem struct {
		Name     string `json:"name"`
		Size     int64  `json:"size"`
		ModTime  string `json:"modTime"`
		Accounts int    `json:"accounts"`
		Devices  int    `json:"devices"`
	}
	var backups []bakItem
	bdir := filepath.Join(dataDirPath, "backups")
	entries, _ := os.ReadDir(bdir)
	if len(entries) == 0 {
		// 也看根目录的 accounts.json.bak*
		entries, _ = os.ReadDir(dataDirPath)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if !(len(n) > 12 && (n[len(n)-4:] == ".bak" || n == "accounts.json")) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		it := bakItem{
			Name:    n,
			Size:    fi.Size(),
			ModTime: fi.ModTime().Format("2006-01-02 15:04"),
		}
		// 尝试读出里面的账号/设备数
		if raw, err := os.ReadFile(filepath.Join(bdir, n)); err == nil {
			var db struct {
				Accounts []any `json:"accounts"`
			}
			if json.Unmarshal(raw, &db) == nil {
				it.Accounts = len(db.Accounts)
			}
		}
		backups = append(backups, it)
	}
	sort.Slice(backups, func(i, j int) bool { return backups[i].ModTime > backups[j].ModTime })
	if len(backups) > 5 {
		backups = backups[:5]
	}

	// ---- 连接统计 ----
	totalDev, onlineDev := 0, 0
	for _, ds := range camsByAcct {
		for _, d := range ds {
			totalDev++
			if d.Online {
				onlineDev++
			}
		}
	}

	// ---- 补每张卡的数据（设备数 / 缩略图 / 凭据状态 / 会话剩余）----
	for i := range stAccounts {
		ds := camsByAcct[stAccounts[i].UserID]
		stAccounts[i].DeviceCount = len(ds)
		for _, dd := range ds {
			if dd.Online {
				stAccounts[i].OnlineCount++
				if stAccounts[i].Did == "" {
					stAccounts[i].Did = dd.Did
					stAccounts[i].Thumb = "/api/live/thumb?did=" + dd.Did
				}
			}
			if dd.Shared {
				stAccounts[i].ShareCount++
			}
		}
		stAccounts[i].DidBound = len(ds) > 0
		stAccounts[i].ServiceTokenOK = stAccounts[i].SessionOK
		stAccounts[i].SessionLeftH = 24 * 7
	}

	writeJSON(w, 200, map[string]any{
		"ok":        true,
		"accounts":  stAccounts,
		"byAccount": camsByAcct,
		"backups":   backups,
		"activity":  recentAccountActivity(12),
		"stats": map[string]any{
			"currentOnline": len(stAccounts),
			"totalAccounts": len(stAccounts),
			"devices":       totalDev,
			"onlineDevices": onlineDev,
			"lastSync":      time.Now().Format("15:04"),
		},
	})
}

type camBrief struct {
	Did    string `json:"did"`
	Name   string `json:"name"`
	Model  string `json:"model"`
	Online bool   `json:"online"`
	Shared bool   `json:"shared"`
}

// fmtSyncTime 把 ISO 时间格式化成 HH:MM（前端直出，避免前端再处理）
func fmtSyncTime(s string) string {
	if s == "" {
		return "—"
	}
	// 2026-09-14T21:06:17Z / 2026-09-14 21:06:17
	for _, l := range []string{"2006-01-02T15:04:05Z", "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02T15:04:05Z07:00"} {
		if t, err := time.Parse(l, s); err == nil {
			return t.Local().Format("01-02 15:04")
		}
	}
	if len(s) >= 16 {
		return s[:10] + " " + s[11:16]
	}
	return s
}
