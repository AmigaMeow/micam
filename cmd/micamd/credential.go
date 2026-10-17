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
	"fmt"
	"net/http"
	"time"
)

// credRenewAfter 凭据年龄超过这个值就试着续期。
//
// ===== 实测修正（2026-09-15）=====
//
// 早期估计「8 小时失效」是错的。
// 实测：CW300 的凭据已经 24 小时（1440 分钟）仍在正常工作
// （当天上午还能抓拍、发云台指令）。
//
// 所以有效期 >= 24 小时。
//
// 阈值从 4 小时改成 12 小时：
//   - 留一半余量（12 小时重叠）
//   - 调用次数从 6 次/天降到 2 次/天（减少 3 倍）
//   - 限流风险大幅下降
//
// 如果将来发现某些型号有效期更短，可以用 /api/live/renew 手动刷。
const credRenewAfter = 12 * time.Hour

// credKeeperInterval 检查间隔。
//
// 15 分钟一次：冷却期一过就可能安排重试，
// 但又不会太频繁（避免自己把自己限流）。
// credKeeperInterval 凭据守护的检查间隔。
//
// 原来 15 分钟（和 cooldownBase 相等，曾导致死锁）。
// 现在 cooldownBase = 30 分钟 > 15 分钟，
// 保证每个周期都能重试一次，同时不至于太频繁。
const credKeeperInterval = 15 * time.Minute

// startCredentialKeeper 启动凭据守护。
func (s *Server) startCredentialKeeper() {
	go func() {
		// 启动后等 2 分钟（避开启动高峰）
		time.Sleep(2 * time.Minute)

		t := time.NewTicker(credKeeperInterval)
		defer t.Stop()

		// ⚠️ 限流期间暂停自动重试（2026-09-19 用户决定）
		//
		// 背景：小米限流持续 20+ 小时未解除。
		// 持续重试可能让风控认为这是「持续异常流量」，反而延长限流。
		//
		// 所以：检测到 throttle deny 后，进入【长时间静默期】，
		// 不再自动重试；用户可在界面上点「立即重试」手动触发。
		//
		// 静默期结束后恢复正常的定时检查（万一已经解除）。
		for {
			if _, ok := miotBound(); ok {
				<-t.C
				continue
			}
			if s.throttleSilenced() {
				<-t.C
				continue
			}
			s.renewStaleCredentials()
			<-t.C
		}
	}()
}

// renewStaleCredentials 检查所有设备，给即将过期的凭据续期。
func (s *Server) renewStaleCredentials() {
	if _, ok := miotBound(); ok {
		// 官方通道由 miot_refresh 维护长期令牌，无需向 miss_get_vendor 请求传统凭据
		return
	}
	for _, d := range s.allCameras() {
		if !d.IsCamera() {
			continue
		}

		age := s.store.SourceAge(d.Did)
		if age < credRenewAfter {
			continue
		}

		// 已有多个凭据时不必急着刷（轮换机制会兜住）
		//
		// 理由：miss_get_vendor 限流很重，能少调就少调。
		// 保留 2 组以上时，即使最新的一组失效，还有旧的可用。
		sets := s.store.Sources(d.Did)
		if len(sets) >= 3 {
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
			s.markRefreshResult(d.Did, false)
			// 不删旧凭据 —— 旧的还能用
			fails := s.RefreshFailCount(d.Did)
			logf("[cred] %s 续期失败（连续 %d 次，下次 %v 后重试）: %v",
				d.Did, fails, cooldownFor(fails).Round(time.Minute), err)

			// ⚠️ 检测到小米限流 -> 进入静默期，停止自动重试
			//
			// 理由（2026-09-19 实测）：限流持续 20+ 小时，
			// 而持续重试可能被风控判定为「持续异常流量」，反而延长限流。
			// 静默一段时间更利于恢复。
			if isThrottleError(err) {
				s.markThrottled()
				return // 整轮都跳过，不用再试其它设备
			}
			continue
		}

		// 成功才替换
		s.store.SaveSource(d.Did, newSrc)
		s.markRefreshResult(d.Did, true)
		logf("[cred] %s 凭据已续期", d.Did)
	}
}

// handleCredRenew 手动触发凭据续期（前端/开发者用）。
//
// POST /api/live/renew?did=xxx   指定设备
// POST /api/live/renew           全部设备
func (s *Server) handleCredRenew(w http.ResponseWriter, r *http.Request) {
	// 恢复期实验锁定期间拒绝（否则 ResetThrottleSilence 会绕过静默）
	if blockIfExperiment(w, r) {
		return
	}
	// 用户主动重试 -> 解除静默（静默是为了避免自动重试加重限流，
	// 但用户明示要试，就让他试）
	s.ResetThrottleSilence()

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

	for _, d := range s.allCameras() {
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

		// ⚠️ 手动重试【绕过】冷却。
		//
		// 理由：冷却机制是为了避免【自动】重试太频繁而加重限流。
		//      但用户手动点「立即重试」是一个明确意图 ——
		//      如果还被冷却挡住（返回「冷却中，剩余 29 分钟」），
		//      用户会觉得按钮没用。
		//
		// 但仍然加一个最短间隔（10 秒），防止疯狂点击：
		//   狂点 = 高频请求 = 正是限流的诱因。
		if left := s.RefreshCooldownLeft(d.Did); left > manualRenewMinGap {
			// 只做一次「提前解锁」：把上次失败时间往前推，
			// 使冷却立即到期，但保留 failCount（退避仍会累积）
			s.forceRefreshNow(d.Did)
		}

		s.markRefresh(d.Did)
		newSrc, err := s.requestFreshSource(d)
		if err != nil {
			s.markRefreshResult(d.Did, false)
			res.Error = err.Error()
			out = append(out, res)
			// 撞限流 -> 进入静默（避免自动重试继续加重）
			if isThrottleError(err) {
				s.markThrottled()
			}
			continue
		}
		s.store.SaveSource(d.Did, newSrc)
		s.markRefreshResult(d.Did, true)
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
		FailCount    int    `json:"failCount"`
		RenewHint    string `json:"renewHint,omitempty"`
	}
	var out []item

	for _, d := range s.allCameras() {
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
		it.FailCount = s.RefreshFailCount(d.Did)
		out = append(out, it)
	}

	// ===== 汇总（供前端显示「凭据受限」状态）=====
	//
	// 为什么要汇总：用户看到的是「画面坏了」，
	// 但真正原因是「设备凭据申请被小米限流」——
	// 界面必须把这个说出来，否则用户以为摄像头坏了。
	limited := 0     // 没有凭据的设备数
	cooldownMin := 0 // 最大的冷却剩余
	fails := 0
	for _, it := range out {
		if !it.HasCred {
			limited++
		}
		if it.CooldownMin > cooldownMin {
			cooldownMin = it.CooldownMin
		}
		if it.FailCount > fails {
			fails = it.FailCount
		}
	}
	silenceLeft := s.throttleSilenceLeft()
	if _, ok := miotBound(); ok {
		silenceLeft = 0 // 官方通道不受传统凭据限流影响
	}
	summary := map[string]any{
		"total":       len(out),
		"ready":       len(out) - limited,
		"limited":     limited,
		"cooldownMin": cooldownMin,
		"failCount":   fails,
		"healthy":     limited == 0,
		"silenced":    silenceLeft > 0,
		"silenceMin":  int(silenceLeft.Minutes()),
	}
	if limited > 0 {
		if silenceLeft > 0 {
			summary["message"] = fmt.Sprintf(
				"%d 台设备的凭据已被小米限流。已暂停自动重试约 %d 小时，避免加重限制；可点「立即重试」手动尝试",
				limited, int(silenceLeft.Hours())+1)
		} else if cooldownMin > 0 {
			summary["message"] = fmt.Sprintf(
				"%d 台设备的凭据申请被小米限流，约 %d 分钟后自动重试", limited, cooldownMin)
		} else {
			summary["message"] = fmt.Sprintf("%d 台设备的凭据不可用，正在重试", limited)
		}
	}

	writeJSON(w, 200, map[string]any{
		"devices":     out,
		"summary":     summary,
		"renewAfterH": int(credRenewAfter.Hours()),
		"validForH":   24, // 实测值（保守）
	})
}

// ---- 账号会话守护 ----
//
// 为什么需要：serviceToken 短期有效，过期后接口会返回未登录错误。
// 虽然 Request 里有自动重试（见 internal/xiaomi/client.go），
// 但「先失败一次再续期」会让用户看到一次错误。
//
// 这里提前续期，用户完全无感。

// sessionRenewAfter serviceToken 用多久后主动续期。
//
// 小米的 serviceToken 时长不公开，保守取 8 小时。
// 即使续期失败，Request 里的自动重试仍能兜住。
const sessionRenewAfter = 8 * time.Hour

// startAccountKeeper 启动账号会话守护。
func (s *Server) startAccountKeeper() {
	go func() {
		time.Sleep(3 * time.Minute)

		t := time.NewTicker(30 * time.Minute)
		defer t.Stop()

		for {
			s.renewAccountSessions()
			<-t.C
		}
	}()
}

// renewAccountSessions 给快过期的账号会话续期。
func (s *Server) renewAccountSessions() {
	for _, sess := range s.store.AllSessions() {
		last := sess.LastExchange()
		if last.IsZero() {
			continue
		}
		age := time.Since(last)
		if age < sessionRenewAfter {
			continue
		}

		logf("[acct] %s 会话已 %v，提前续期", sess.UserID, age.Round(time.Minute))
		if err := sess.Reauth(); err != nil {
			// 续期失败不影响使用（旧 token 可能还有效），
			// 下次 Request 遇到未登录会自动重试
			logf("[acct] %s 续期失败（不影响当前使用）: %v", sess.UserID, err)
		} else {
			logf("[acct] %s 会话已续期", sess.UserID)
		}
	}
}
