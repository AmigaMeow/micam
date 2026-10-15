package main

// 敏感接口的访问码（access code）—— 2026-09-23 新增。
//
// ===== 要解决的问题 =====
//
// 实测确认：局域网内任意设备无需任何凭据即可拿到账号 passToken：
//
//	curl http://<NAS_IP>:8900/api/accounts/export
//	-> {"accounts":[{"passToken":"V1:J7rrshru..."}]}
//
// passToken 是小米账号的【长期登录凭据】，等于账号被接管。
//
// 根因：原鉴权是「可选」的 —— 不带 Authorization 头就直接放行（向后兼容），
// 这对导出、导入、删除账号、切换 API 域名这类【敏感操作】是错的。
//
// ===== 为什么用访问码而不是 API Token =====
//
// API Token 的创建接口本身无鉴权，攻击者可以自助创建一个再调用敏感接口，
// 等于没修。所以需要一个【只有能访问 NAS 的人才能拿到】的凭据。
//
// 访问码满足这一点：它写在 NAS 文件系统里（0600），
// 局域网攻击者既读不到文件、也看不到日志。
//
// ===== 设计 =====
//
//   · 首次运行生成 16 字符随机码，存 <dataDir>/access-code.txt（0600）
//   · 启动时写入日志（fnOS 日志查看器能看到）
//   · 敏感接口要求 X-Access-Code 头，或 ?code= 查询参数
//   · 比较用 crypto/subtle.ConstantTimeCompare（防时序侧信道）
//   · 失败限速：5 分钟内 10 次失败 -> 锁 5 分钟（防暴力）
//   · 若用户显式设置 cfg.Dev.AllowLANSensitive = true，可关闭该保护
//     （默认关闭 = 保护生效，即「安全默认」）

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	accessCodeLen      = 16 // 生成的码长度（字符）
	accessFailWindow   = 5 * time.Minute
	accessFailMax      = 10 // 窗口内允许的失败次数
	accessLockDuration = 5 * time.Minute
)

var (
	accessCodeMu   sync.RWMutex
	accessCodeVal  string
	accessFailMu   sync.Mutex
	accessFailures []time.Time
)

// sensitivePaths 需要访问码的接口前缀。
//
// 判断标准：泄露凭据 / 破坏数据 / 可被用来钓鱼。
var sensitivePaths = []string{
	"/api/accounts/export",    // 返回 passToken —— 最严重
	"/api/accounts/import",    // 接受 passToken（可覆写账号）
	"/api/accounts/remove",    // 破坏性
	"/api/credentials/export", // 旧路径别名
	"/api/cred/apibase",       // 可把 API 指向攻击者服务器 -> 钓 serviceToken
	"/api/dev/debug-vendor",   // 诊断接口，含设备/凭据信息
	"/api/dev/reset",          // 重置配置
	"/api/dev/logs/clear",     // 清日志（销毁痕迹）
}

func isSensitivePath(p string) bool {
	for _, pre := range sensitivePaths {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

// accessCodeFile 访问码存放路径。
func (s *Server) accessCodeFile() string {
	return filepath.Join(s.dataDir, "access-code.txt")
}

// loadAccessCode 载入或首次生成访问码。
func (s *Server) loadAccessCode() {
	path := s.accessCodeFile()

	if raw, err := os.ReadFile(path); err == nil {
		code := strings.TrimSpace(string(raw))
		if code != "" {
			accessCodeMu.Lock()
			accessCodeVal = code
			accessCodeMu.Unlock()
			logf("[sec] 已载入访问码（敏感接口需 X-Access-Code 头）")
			return
		}
	}

	// 生成新码
	b := make([]byte, accessCodeLen/2)
	if _, err := rand.Read(b); err != nil {
		logf("[sec] 生成访问码失败: %v（敏感接口将保持锁定）", err)
		return
	}
	code := hex.EncodeToString(b)

	if err := os.WriteFile(path, []byte(code+"\n"), 0o600); err != nil {
		logf("[sec] 保存访问码失败: %v", err)
	}
	accessCodeMu.Lock()
	accessCodeVal = code
	accessCodeMu.Unlock()

	logf("[sec] 已生成访问码: %s", code)
	logf("[sec] 存放于 %s（权限 0600）", path)
	logf("[sec] 导出凭据等敏感操作需要在界面上输入此码")
}

// currentAccessCode 返回当前访问码（供日志/界面提示）。
func currentAccessCode() string {
	accessCodeMu.RLock()
	defer accessCodeMu.RUnlock()
	return accessCodeVal
}

// requestAccessCode 从请求里取访问码。
func requestAccessCode(r *http.Request) string {
	if v := r.Header.Get("X-Access-Code"); v != "" {
		return strings.TrimSpace(v)
	}
	return strings.TrimSpace(r.URL.Query().Get("code"))
}

// isLocalRequest 判断请求是否来自本机（NAS 自身）。
func isLocalRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// accessLocked 判断是否因失败过多而锁定。
func accessLocked() bool {
	accessFailMu.Lock()
	defer accessFailMu.Unlock()
	cut := time.Now().Add(-accessFailWindow)
	kept := accessFailures[:0]
	for _, t := range accessFailures {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	accessFailures = kept
	return len(accessFailures) >= accessFailMax
}

// recordAccessFailure 记录一次失败。
func recordAccessFailure() {
	accessFailMu.Lock()
	defer accessFailMu.Unlock()
	accessFailures = append(accessFailures, time.Now())
}

// verifyAccessCode 校验访问码（常量时间比较）。
func verifyAccessCode(got string) bool {
	want := currentAccessCode()
	if want == "" || got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// handleAccessCodeStatus 返回保护状态（不返回码本身）。
//
// GET /api/dev/access-code
func (s *Server) handleAccessCodeStatus(w http.ResponseWriter, r *http.Request) {
	code := currentAccessCode()
	writeJSON(w, 200, map[string]any{
		"enabled":  code != "",
		"unlocked": verifyAccessCode(requestAccessCode(r)) || isLocalRequest(r),
		"hint":     "访问码在 NAS 上：" + s.accessCodeFile(),
		"note":     "密码本身不会通过接口返回（防局域网窃取）",
	})
}
