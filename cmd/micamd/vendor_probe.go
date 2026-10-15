package main

// 参数探测：用任意参数调 miss_get_vendor，看小米怎么回应。
//
// 目的（2026-09-23）：搞清"官方到底发了什么参数"。
// 我们当前发的是 {app_pubkey, did, support_vendors:"TUTK_CS2_MTP"}，
// 与 go2rtc 上游一致，但仍被限流。
//
//	GET /api/dev/vendor-probe?did=xxx&json={"support_vendors":"CS2"}
//	GET /api/dev/vendor-probe?did=xxx          （用默认参数做对照）
//
// 返回：HTTP 状态 + 解密后的响应 + 耗时。

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/xiaomi/crypto"
	"github.com/micam/micam/internal/xiaomi"
)

func (s *Server) handleVendorProbe(w http.ResponseWriter, r *http.Request) {
	if blockIfExperiment(w, r) {
		return
	}
	q := r.URL.Query()
	did := q.Get("did")
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

	// 构造参数：默认 = 我们当前用的
	params := map[string]any{
		"app_pubkey":      hex.EncodeToString(pub),
		"did":             did,
		"support_vendors": "TUTK_CS2_MTP",
	}
	if raw := q.Get("json"); raw != "" {
		var custom map[string]any
		if err := json.Unmarshal([]byte(raw), &custom); err != nil {
			writeErr(w, 400, "json 参数解析失败: "+err.Error())
			return
		}
		// 合并（custom 覆盖默认）
		for k, v := range custom {
			if v == nil {
				delete(params, k) // null = 删除该字段
				continue
			}
			params[k] = v
		}
		// did 兜底
		if _, ok := params["did"]; !ok {
			params["did"] = did
		}
	}

	// 可指定其它端点，用于对比"限流是否只针对 miss_get_vendor"
	path := q.Get("path")
	if path == "" {
		path = "/v2/device/miss_get_vendor"
	}

	body, _ := json.Marshal(params)
	form, sn, err := xiaomi.SignRequest(sess.SsecurityBytes(), path, string(body))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	url := "https://" + xiaomi.APIBase() + "/app" + path
	req, _ := http.NewRequest("POST", url, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", sess.UserAgent())
	req.Header.Set("Cookie", "userId="+sess.UserID+"; serviceToken="+sess.ServiceToken+"; deviceId="+sess.DeviceID())

	t0 := time.Now()
	res, err := http.DefaultClient.Do(req)
	ms := time.Since(t0).Milliseconds()
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)

	plain := ""
	if dec, e := xiaomi.DecryptResponse(sn, raw); e == nil {
		plain = string(dec)
	} else {
		plain = "DECRYPT_ERR: " + e.Error()
	}

	logf("[probe] %d %dms params=%s -> %.200s", res.StatusCode, ms, string(body), plain)

	writeJSON(w, 200, map[string]any{
		"path":          path,
		"sentParams":    params,
		"httpStatus":    res.StatusCode,
		"xiaomiStatus":  res.Header.Get("X-Xiaomi-Status-Code"),
		"ms":            ms,
		"responsePlain": plain,
	})
}
