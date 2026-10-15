package main

// 账号删除 / 重新登录相关接口。
//
//	POST /api/accounts/remove   {"userId":"..."}  删除账号及其设备
//
// 为什么需要：用户添加错了账号、或换了账号，没法删除 —— 这是基础能力缺失。

import (
	"encoding/json"
	"net/http"
)

func (s *Server) handleAccountRemove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 POST")
		return
	}
	var req struct {
		UserID string `json:"userId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.UserID == "" {
		writeErr(w, 400, "缺少 userId")
		return
	}

	if err := s.store.RemoveAccount(req.UserID); err != nil {
		writeErr(w, 500, "删除失败: "+err.Error())
		return
	}
	// 同时清掉该账号的设备缓存（否则设备列表会留孤儿）
	s.store.RemoveDevicesOfAccount(req.UserID)

	logf("[acct] 已删除账号 %s", req.UserID)
	writeJSON(w, 200, map[string]any{"ok": true, "userId": req.UserID})
}
