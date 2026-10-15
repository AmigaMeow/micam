package main

// 临时诊断：dump miss_get_vendor 的原始 HTTP 响应（状态/头/体）。
//
// 目的：判断 "throttle deny" 是小米 API 的正常业务响应，
// 还是 CDN/WAF 的拦截（后者会有 Server / X-Cache 等特征头）。
//
//	GET /api/dev/debug-vendor?did=<设备ID>

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/AlexxIT/go2rtc/pkg/xiaomi/crypto"
	"github.com/micam/micam/internal/xiaomi"
)

func (s *Server) handleDebugVendor(w http.ResponseWriter, r *http.Request) {
	if blockIfExperiment(w, r) {
		return
	}
	did := r.URL.Query().Get("did")
	if did == "" {
		did = "" // 必填，不设默认值
	}
	sessions := s.store.AllSessions()
	if len(sessions) == 0 {
		writeErr(w, 400, "未登录")
		return
	}
	sess := sessions[0]

	pub, _, err := crypto.GenerateKey()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	params, _ := json.Marshal(map[string]any{
		"app_pubkey":      hex.EncodeToString(pub),
		"did":             did,
		"support_vendors": "TUTK_CS2_MTP",
	})

	form, sn, err := xiaomi.SignRequest(sess.SsecurityBytes(), "/v2/device/miss_get_vendor", string(params))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	url := "https://" + xiaomi.APIBase() + "/app/v2/device/miss_get_vendor"
	req, _ := http.NewRequest("POST", url, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", sess.UserAgent())
	req.Header.Set("Cookie", "userId="+sess.UserID+"; serviceToken="+sess.ServiceToken+"; deviceId="+sess.DeviceID())

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)

	hdrs := map[string]string{}
	for k, v := range res.Header {
		hdrs[k] = strings.Join(v, ", ")
	}

	// 解密响应体（看完整 JSON）
	plain := ""
	if dec, e := xiaomi.DecryptResponse(sn, body); e == nil {
		plain = string(dec)
	} else {
		plain = "DECRYPT_ERR: " + e.Error()
	}

	writeJSON(w, 200, map[string]any{
		"url":          url,
		"sid":          xiaomi.SID(),
		"host":         xiaomi.APIBase(),
		"status":       res.StatusCode,
		"xiaomiStatus": res.Header.Get("X-Xiaomi-Status-Code"),
		"headers":      hdrs,
		"bodyRaw":      string(body),
		"bodyLen":      len(body),
		"bodyPlain":    plain,
		"reqParams":    map[string]string{"did": did, "support_vendors": "TUTK_CS2_MTP"},
	})
}
