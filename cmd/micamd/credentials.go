package main

// 凭据管理：备份 / 恢复 / 状态。
//
// ===== 两层凭据（重要，别混淆）=====
//
// 第一层：passToken（扫码登录拿到）
//   长期有效（实测 >= 24 小时，实际可达数月）
//   只在换设备 / 改密码 / 主动登出时失效
//   **用户扫一次码就够了，不需要反复扫**
//
// 第二层：serviceToken + ssecurity（由 passToken 换取）
//   短期有效（几小时到几天）
//   过期后自动用 passToken 重新换（客户端无感）
//
// 第三层：设备 P2P 凭据（sign / device_public）
//   按设备申请，有效期 >= 24 小时
//   到期前自动续期（12 小时阈值）
//   这层会撞小米云限流 —— 所以要尽量少调用
//
// ===== 为什么需要备份 =====
//
// 卸载插件时 @appdata 可能被清掉 -> passToken 丢失 -> 要重新扫码。
// 对监控产品来说这不可接受（可能人不在现场）。
//
// 所以提供导出/导入：
//   导出 -> 一个 JSON 文件（含凭据，敏感！）
//   导入 -> 恢复登录状态

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// handleCredExport 导出账号凭据（含 passToken，敏感）。
//
// GET /api/accounts/export
//
// 返回一个可以直接保存的 JSON。
// **提醒用户妥善保管** —— 拿到这个文件等于拿到账号登录态。
func (s *Server) handleCredExport(w http.ResponseWriter, r *http.Request) {
	path := filepath.Join(s.dataDir, "accounts.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		writeErr(w, 404, "没有账号凭据可导出")
		return
	}

	// 记录导出时间（便于判断新旧）
	var accs []map[string]any
	_ = json.Unmarshal(raw, &accs)
	out := map[string]any{
		"app":        appName,
		"version":    appVersion,
		"exportedAt": time.Now().Format(time.RFC3339),
		"note":       "含小米账号登录凭据，请妥善保管。重新安装插件后可导入恢复，无需重新扫码。",
		"accounts":   accs,
	}

	data, _ := json.MarshalIndent(out, "", "  ")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=micam-accounts.json")
	_, _ = w.Write(data)

	logf("[cred] 已导出 %d 个账号凭据", len(accs))
}

// handleCredImport 导入账号凭据。
//
// POST /api/accounts/import  （body = export 出来的 JSON）
func (s *Server) handleCredImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 POST")
		return
	}

	var in struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, "解析失败: "+err.Error())
		return
	}
	if len(in.Accounts) == 0 {
		writeErr(w, 400, "文件里没有账号")
		return
	}

	// 校验字段
	for _, a := range in.Accounts {
		uid, _ := a["userId"].(string)
		tok, _ := a["passToken"].(string)
		if uid == "" || tok == "" {
			writeErr(w, 400, "账号数据缺少 userId 或 passToken")
			return
		}
	}

	// 备份现有文件
	path := filepath.Join(s.dataDir, "accounts.json")
	if old, err := os.ReadFile(path); err == nil {
		if err := os.WriteFile(path+".bak", old, 0o600); err != nil {
			logf("[cred] 备份凭据到 %s.bak 失败: %v", path, err)
		}
	}

	data, _ := json.MarshalIndent(in.Accounts, "", "  ")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		writeErr(w, 500, "写入失败: "+err.Error())
		return
	}

	// 重新加载
	s.store.ReloadAccounts()

	logf("[cred] 已导入 %d 个账号凭据", len(in.Accounts))
	writeJSON(w, 200, map[string]any{
		"ok":    true,
		"count": len(in.Accounts),
		"msg":   "已导入，无需重新扫码。请刷新页面。",
	})
}

// handleCredBackup 列出备份文件。
//
// GET /api/accounts/backups
func (s *Server) handleCredBackup(w http.ResponseWriter, r *http.Request) {
	type item struct {
		Name    string `json:"name"`
		Size    int64  `json:"size"`
		ModTime string `json:"modTime"`
	}
	var out []item

	entries, _ := os.ReadDir(s.dataDir)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if n != "accounts.json" && n != "accounts.json.bak" {
			continue
		}
		if info, err := e.Info(); err == nil {
			out = append(out, item{
				Name:    n,
				Size:    info.Size(),
				ModTime: info.ModTime().Format("2006-01-02 15:04:05"),
			})
		}
	}

	writeJSON(w, 200, map[string]any{"backups": out})
}

// handleAccountStatus 账号凭据状态（区分两层凭据，给用户看清楚）。
//
// GET /api/accounts/status
func (s *Server) handleAccountStatus(w http.ResponseWriter, r *http.Request) {
	type acct struct {
		UserID       string `json:"userId"`
		HasPassToken bool   `json:"hasPassToken"`
		SessionOK    bool   `json:"sessionOk"`
		LastExchange string `json:"lastExchange,omitempty"`
		SessionAgeM  int    `json:"sessionAgeMinutes"`
	}

	var out []acct
	for _, sess := range s.store.AllSessions() {
		a := acct{
			UserID:       sess.UserID,
			HasPassToken: sess.PassToken != "",
			SessionOK:    sess.TokenValid(),
		}
		if t := sess.LastExchange(); !t.IsZero() {
			a.LastExchange = t.Format("2006-01-02 15:04:05")
			a.SessionAgeM = int(time.Since(t).Minutes())
		}
		out = append(out, a)
	}

	writeJSON(w, 200, map[string]any{
		"accounts": out,
		"explain": map[string]any{
			"passToken":    "扫码登录拿到，长期有效（无需反复扫码）",
			"serviceToken": "由 passToken 换取，短期有效，过期自动续期",
			"deviceCred":   "每台摄像头的 P2P 凭据，有效期 >= 24 小时，到期前自动续期",
		},
	})
}
