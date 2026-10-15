package main

// 实时设备 IP 查询（2026-10-11）。
//
// ===== 为什么必须实时查 =====
//
// 官方通道（PPCS）靠【局域网 IP】直连摄像头，
// 而摄像头 IP 会变（DHCP 续租、重启、换网）：
//
//   实测：小米云报的 IP 可能与实际不符，需以局域网探测为准
//   用旧 IP -> PPCS_Connect errorcode: -3（一直连不上）
//   用实时 IP -> 立刻连通并出流
//
// 而 /v2/home/device_list 是【只读接口，不受 miss_get_vendor 限流影响】，
// 可以放心频繁调用（实测 200 OK）。
//
// ===== 策略 =====
//
//   1. 优先用实时查询
//   2. 查询失败则退回缓存（至少不会更差）
//   3. 结果缓存 60 秒，避免每次拉流都打云端

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/micam/micam/internal/xiaomi"
)

var (
	ipCacheMu  sync.Mutex
	ipCacheAt  time.Time
	ipCacheMap map[string]string // did -> localip
	ipCacheTTL = 60 * time.Second
)

// liveDeviceIP 查某设备的实时局域网 IP。
//
// 失败时返回空串（调用方应退回缓存）。
func (s *Server) liveDeviceIP(did string) string {
	ipCacheMu.Lock()
	if time.Since(ipCacheAt) < ipCacheTTL && ipCacheMap != nil {
		ip := ipCacheMap[did]
		ipCacheMu.Unlock()
		return ip
	}
	ipCacheMu.Unlock()

	// 刷新整张表（一次查询覆盖所有设备）
	m := s.fetchDeviceIPs()
	if len(m) > 0 {
		ipCacheMu.Lock()
		ipCacheMap = m
		ipCacheAt = time.Now()
		ip := m[did]
		ipCacheMu.Unlock()
		return ip
	}
	return ""
}

// fetchDeviceIPs 调 device_list 拉取所有设备的实时 IP。
func (s *Server) fetchDeviceIPs() map[string]string {
	sessions := s.store.AllSessions()
	if len(sessions) == 0 {
		return nil
	}
	raw, err := sessions[0].Request("/v2/home/device_list", "{}")
	if err != nil {
		logf("[miot] 实时 IP 查询失败（退回缓存）: %v", err)
		return nil
	}
	var v struct {
		List []struct {
			Did     string `json:"did"`
			LocalIP string `json:"localip"`
		} `json:"list"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	out := map[string]string{}
	for _, d := range v.List {
		if d.LocalIP != "" && d.LocalIP != "0.0.0.0" && d.LocalIP != "127.0.0.1" {
			out[d.Did] = d.LocalIP
		}
	}
	logf("[miot] 实时 IP 已刷新（%d 台设备）", len(out))
	return out
}

// resolveDeviceIP 解析设备 IP：实时优先，失败退回缓存。
func (s *Server) resolveDeviceIP(dev *xiaomi.Device) (string, string) {
	if ip := s.liveDeviceIP(dev.Did); ip != "" && ip != "0.0.0.0" && ip != "127.0.0.1" {
		if ip != dev.LocalIP {
			logf("[miot] %s 的 IP 已变化: 缓存=%s 实时=%s", dev.Did, dev.LocalIP, ip)
		}
		return ip, "live"
	}
	return dev.LocalIP, "cache"
}

// 让编译器知道 fmt 被用到（保留扩展点）
var _ = fmt.Sprintf
