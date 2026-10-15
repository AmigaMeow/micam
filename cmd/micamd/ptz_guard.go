package main

// 云台写操作的安全护栏（2026-10-11）。
//
// ===== 为什么需要 =====
//
// 我用 MIoT-Spec 写「常看位置」+「激活」测试云台时，
// 把用户的球机转到了墙上，之后复位/校准都转不回来。
// 用户明确警告过「测试结束之后画面都是乱的」。
//
// 这个文件加三道护栏：
//
//   1. 写操作必须先备份当前状态（可回滚）
//   2. 默认【禁止】写操作，需要显式开启（config 或环境变量）
//   3. 单次会话最多写 N 次，防止脚本失控（如循环激活）

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ptzWriteGuard PTZ 写操作护栏。
type ptzWriteGuard struct {
	mu       sync.Mutex
	enabled  bool
	count    int
	lastAt   time.Time
	backup   map[string]any
	backupAt time.Time
}

var ptzGuard = &ptzWriteGuard{}

const (
	// ptzMaxWritesPerHour 每小时最多写多少次（防失控）
	ptzMaxWritesPerHour = 30
	// ptzWriteCooldown 两次写之间的最小间隔
	ptzWriteCooldown = 1500 * time.Millisecond
)

// initPTZGuard 从配置/环境读取是否允许写。
//
// 默认【禁止】—— 必须显式开启。
// 开启方式（任一）：
//
//	· 环境变量 MICAM_PTZ_WRITE=1
//	· <dataDir>/ptz-write-enabled 文件存在
func initPTZGuard(dataDir string) {
	ptzGuard.enabled = os.Getenv("MICAM_PTZ_WRITE") == "1"
	if !ptzGuard.enabled {
		if _, err := os.Stat(filepath.Join(dataDir, "ptz-write-enabled")); err == nil {
			ptzGuard.enabled = true
		}
	}
	if ptzGuard.enabled {
		logf("[ptz] ⚠️ 云台写操作已启用（用了护栏：备份 + 限流 %d 次/小时）", ptzMaxWritesPerHour)
	} else {
		logf("[ptz] 云台写操作已禁用（只读模式）。启用：创建 %s 文件",
			filepath.Join(dataDir, "ptz-write-enabled"))
	}
}

// allowPTZWrite 检查是否允许写。
func allowPTZWrite() error {
	ptzGuard.mu.Lock()
	defer ptzGuard.mu.Unlock()

	if !ptzGuard.enabled {
		return fmt.Errorf("云台写操作已被禁用（安全模式）。" +
			"如需启用，在数据目录创建 ptz-write-enabled 文件。" +
			"注意：写操作可能改变镜头朝向且无法自动恢复")
	}
	if time.Since(ptzGuard.lastAt) < ptzWriteCooldown {
		return fmt.Errorf("操作过于频繁，请等 %.1f 秒",
			(ptzWriteCooldown - time.Since(ptzGuard.lastAt)).Seconds())
	}
	if time.Since(ptzGuard.backupAt) > time.Hour {
		ptzGuard.count = 0
	} else if ptzGuard.count >= ptzMaxWritesPerHour {
		return fmt.Errorf("已达每小时写操作上限（%d 次），防止失控", ptzMaxWritesPerHour)
	}
	ptzGuard.lastAt = time.Now()
	ptzGuard.count++
	return nil
}

// backupPTZState 写操作前备份云台状态。
func (s *Server) backupPTZState(did string) {
	st := s.readPTZState(did)
	b, _ := json.Marshal(st)
	ptzGuard.mu.Lock()
	ptzGuard.backup = map[string]any{"did": did, "state": string(b), "at": time.Now().Format(time.RFC3339)}
	ptzGuard.backupAt = time.Now()
	ptzGuard.mu.Unlock()

	// 落盘（方便人工查看）
	p := filepath.Join(dataDirPath, "ptz-last-backup.json")
	_ = os.WriteFile(p, b, 0o600)
	logf("[ptz] 已备份云台状态到 %s: %s", p, string(b)[:min(len(b), 160)])
}

// ptzWriteStatus 报告护栏状态（给前端显示）。
func ptzWriteStatus() map[string]any {
	ptzGuard.mu.Lock()
	defer ptzGuard.mu.Unlock()
	out := map[string]any{
		"enabled": ptzGuard.enabled,
		"count":   ptzGuard.count,
		"limit":   ptzMaxWritesPerHour,
	}
	if !ptzGuard.backupAt.IsZero() {
		out["lastBackupAt"] = ptzGuard.backupAt.Format(time.RFC3339)
	}
	if ptzGuard.backup != nil {
		out["lastBackupState"] = ptzGuard.backup["state"]
	}
	return out
}
