package main

// 凭据守护：主动提前续期。
//
// ===== 为什么需要 =====
//
// cs2 凭据（sign/device_public）实测 8 小时后失效。
// 旧策略是「过期后才刷新」，问题：
//
//   1. 一撞上小米云限流（throttle deny）就彻底停摆
//   2. 刷新失败后有 30 分钟冷却，期间完全不可用
//   3. 用户看不了画面，只能干等
//
// 新策略：**提前续期**
//
//   凭据年龄 > 阈值（默认 4 小时）时，在后台主动刷新。
//   提前续期不阻塞用户（用户在用的是旧凭据，仍然有效）。
//   如果限流，下次再试 —— 反正还有 4 小时余量。
//
// 这样把「限流」从「致命」降级成「重试」。

import (
	"net/http"
	"time"
)

// credRenewAfter 凭据年龄超过这个值就尝试续期。
//
// 凭据实测 8 小时失效，4 小时留一半余量：
// 即使连续撞 3 次限流（每次冷却 30 分钟），也还来得及。
const credRenewAfter = 4 * time.Hour

// credKeeperInterval 检查间隔。
//
// 15 分钟一次：冷却期一过就可能安排重试，
// 但又不会太频繁（避免自己把自己限流）。
const credKeeperInterval = 15 * time.Minute

// startCredentialKeeper 启动凭据守护。
func (s *Server) startCredentialKeeper() {
	go func() {
		// 启动后等 2 分钟（避开启动高峰）
		time.Sleep(2 * time.Minute)

		t := time.NewTicker(credKeeperInterval)
		defer t.Stop()

		for {
			s.renewStaleCredentials()
			<-t.C
		}
	}()
}

// renewStaleCredentials 检查所有设备，给即将过期的凭据续期。
func (s *Server) renewStaleCredentials() {
	for _, d := range s.store.Devices() {
		if !d.IsCamera() {
			continue
		}

		age := s.store.SourceAge(d.Did)
		if age < credRenewAfter {
			continue
		}

		// 冷却中（上次刷新失败）就跳过
		//
		// 注意：冷却结束后会自然重试 —— 相当于「限流恢复探测」，
		// 不需要额外的探测逻辑。
		if !s.canRefresh(d.Did) {
			left := s.RefreshCooldownLeft(d.Did)
			logf("[cred] %s 凭据已 %v，但刷新冷却中（剩 %v）",
				d.Did, age.Round(time.Minute), left.Round(time.Minute))
			continue
		}

		logf("[cred] %s 凭据已 %v，提前续期", d.Did, age.Round(time.Minute))
		s.markRefresh(d.Did)

		newSrc, err := s.requestFreshSource(d)
		if err != nil {
			// 不删旧凭据 —— 旧的还能用
			logf("[cred] %s 续期失败（保留旧凭据）: %v", d.Did, err)
			continue
		}

		// 成功才替换
		s.store.SaveSource(d.Did, newSrc)
		logf("[cred] %s 凭据已续期", d.Did)
	}
}

// handleCredRenew 手动触发凭据续期（前端/开发者用）。
//
// POST /api/live/renew?did=xxx   指定设备
// POST /api/live/renew           全部设备
func (s *Server) handleCredRenew(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 POST")
		return
	}

	did := r.URL.Query().Get("did")

	type result struct {
		Did   string `json:"did"`
		Name  string `json:"name"`
		OK    bool   `json:"ok"`
		Age   int    `json:"ageMinutes"`
		Error string `json:"error,omitempty"`
	}
	var out []result

	for _, d := range s.store.Devices() {
		if !d.IsCamera() {
			continue
		}
		if did != "" && d.Did != did {
			continue
		}

		age := s.store.SourceAge(d.Did)
		res := result{
			Did:  d.Did,
			Name: d.Name,
			Age:  int(age.Minutes()),
		}

		if !s.canRefresh(d.Did) {
			left := s.RefreshCooldownLeft(d.Did)
			res.Error = "冷却中，剩余 " + left.Round(time.Minute).String()
			out = append(out, res)
			continue
		}

		s.markRefresh(d.Did)
		newSrc, err := s.requestFreshSource(d)
		if err != nil {
			res.Error = err.Error()
			out = append(out, res)
			continue
		}
		s.store.SaveSource(d.Did, newSrc)
		res.OK = true
		out = append(out, res)
	}

	writeJSON(w, 200, map[string]any{"results": out})
}

// handleCredStatus 凭据状态（含剩余有效期估算）。
//
// GET /api/live/cred-status
func (s *Server) handleCredStatus(w http.ResponseWriter, r *http.Request) {
	type item struct {
		Did          string `json:"did"`
		Name         string `json:"name"`
		HasCred      bool   `json:"hasCred"`
		AgeMinutes   int    `json:"ageMinutes"`
		ExpiresInMin int    `json:"expiresInMinutes"`
		CooldownMin  int    `json:"cooldownMinutes"`
		RenewHint    string `json:"renewHint,omitempty"`
	}
	var out []item

	for _, d := range s.store.Devices() {
		if !d.IsCamera() {
			continue
		}
		it := item{Did: d.Did, Name: d.Name}
		if _, ok := s.store.Source(d.Did); ok {
			it.HasCred = true
			age := s.store.SourceAge(d.Did)
			it.AgeMinutes = int(age.Minutes())
			// 实测 8 小时（480 分钟）失效
			it.ExpiresInMin = 480 - it.AgeMinutes
			if it.ExpiresInMin < 0 {
				it.ExpiresInMin = 0
			}
			if age > credRenewAfter {
				it.RenewHint = "已超过续期阈值，将自动续期"
			}
		} else {
			it.RenewHint = "无缓存凭据"
		}
		it.CooldownMin = int(s.RefreshCooldownLeft(d.Did).Minutes())
		out = append(out, it)
	}

	writeJSON(w, 200, map[string]any{
		"devices":     out,
		"renewAfterH": int(credRenewAfter.Hours()),
		"validForH":   8,
	})
}
