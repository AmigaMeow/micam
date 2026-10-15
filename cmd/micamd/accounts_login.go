package main

// 账号密码登录（含二次验证）。
//
// 背景：internal/xiaomi 里 Login / VerifyError.StartVerify / Submit 早就实现了，
// 只是从来没接出 HTTP 接口 —— /api/accounts POST 一直返回「账号密码登录…（未实现）」。
//
// 流程：
//   POST /api/accounts/login  {username, password}
//     -> {"ok":true}                                  直接登录成功（已落库）
//     -> {"needVerify":true,"id":"...","maskedPhone":...,"maskedEmail":...,"method":"手机/邮箱"}
//        小米对陌生设备通常要求二次验证，此时已把验证码发到手机/邮箱
//     -> {"needCaptcha":true,"url":"..."}              图形验证码（少见，需要人眼识别）
//   POST /api/accounts/verify {id, code}
//     -> {"ok":true} 验证通过并落库

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/micam/micam/internal/xiaomi"
)

type pendingLogin struct {
	sess    *xiaomi.Session
	verify  *xiaomi.VerifyError
	created time.Time
}

var (
	pendingLoginsMu sync.Mutex
	pendingLogins   = map[string]*pendingLogin{}
	pendingLoginSeq int64
)

// gcPendingLogins 清掉超过 5 分钟的待验证会话。
func gcPendingLogins() {
	cut := time.Now().Add(-5 * time.Minute)
	pendingLoginsMu.Lock()
	defer pendingLoginsMu.Unlock()
	for k, v := range pendingLogins {
		if v.created.Before(cut) {
			delete(pendingLogins, k)
		}
	}
}

func putPendingLogin(p *pendingLogin) string {
	pendingLoginsMu.Lock()
	defer pendingLoginsMu.Unlock()
	pendingLoginSeq++
	id := "pl" + itoa(int(pendingLoginSeq)) + "-" + itoa(int(time.Now().Unix()%100000))
	pendingLogins[id] = p
	return id
}

func getPendingLogin(id string) *pendingLogin {
	pendingLoginsMu.Lock()
	defer pendingLoginsMu.Unlock()
	p := pendingLogins[id]
	return p
}

func dropPendingLogin(id string) {
	pendingLoginsMu.Lock()
	defer pendingLoginsMu.Unlock()
	delete(pendingLogins, id)
}

// handleAccountLogin 账号密码登录。
//
// POST /api/accounts/login  {"username":"手机号/邮箱","password":"..."}
func (s *Server) handleAccountLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 POST")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "参数错误")
		return
	}
	if req.Username == "" || req.Password == "" {
		writeErr(w, 400, "小米账号和密码都要填")
		return
	}

	sess := xiaomi.NewSession()
	err := sess.Login(req.Username, req.Password)
	if err == nil {
		if err := s.store.AddAccount(sess); err != nil {
			writeErr(w, 500, "登录成功但保存账号失败: "+err.Error())
			return
		}
		logf("账号密码登录成功: %s", req.Username)
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}

	// 二次验证：验证码已发到手机/邮箱
	var ve *xiaomi.VerifyError
	if errors.As(err, &ve) {
		if e2 := ve.StartVerify(); e2 != nil {
			writeErr(w, 500, "发送验证码失败: "+e2.Error())
			return
		}
		gcPendingLogins()
		id := putPendingLogin(&pendingLogin{sess: sess, verify: ve, created: time.Now()})
		method := "手机"
		if ve.MaskedEmail != "" && ve.MaskedPhone == "" {
			method = "邮箱"
		}
		logf("账号密码登录需要二次验证: %s（%s）", req.Username, method)
		writeJSON(w, 200, map[string]any{
			"needVerify":  true,
			"id":          id,
			"maskedPhone": ve.MaskedPhone,
			"maskedEmail": ve.MaskedEmail,
			"method":      method,
		})
		return
	}

	// 图形验证码（少见）
	var ce *xiaomi.CaptchaError
	if errors.As(err, &ce) {
		writeJSON(w, 200, map[string]any{
			"needCaptcha": true,
			"url":         ce.URL,
			"msg":         "小米要求图形验证码，请改用扫码登录",
		})
		return
	}

	writeErr(w, 400, "登录失败："+err.Error())
}

// handleAccountVerify 提交二次验证码。
//
// POST /api/accounts/verify  {"id":"...","code":"123456"}
func (s *Server) handleAccountVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 POST")
		return
	}
	var req struct {
		ID   string `json:"id"`
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "参数错误")
		return
	}
	if req.ID == "" || req.Code == "" {
		writeErr(w, 400, "缺少验证码")
		return
	}
	p := getPendingLogin(req.ID)
	if p == nil {
		writeErr(w, 404, "验证会话不存在或已过期（5 分钟内有效），请重新登录")
		return
	}
	if err := p.verify.Submit(req.Code); err != nil {
		writeErr(w, 400, "验证失败："+err.Error())
		return
	}
	if err := s.store.AddAccount(p.sess); err != nil {
		writeErr(w, 500, "验证通过但保存账号失败: "+err.Error())
		return
	}
	dropPendingLogin(req.ID)
	logf("账号密码登录：二次验证通过")
	writeJSON(w, 200, map[string]any{"ok": true})
}
