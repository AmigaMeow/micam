package main

// 录像迁移（2026-09-23）。
//
// 目的：把已有录像从当前录像目录搬到另一个位置（例如外挂盘/备份盘）。
//
// ===== 安全设计（重要）=====
//
// 迁移是【破坏性】操作（搬完之后原位置不再保留），所以：
//   1. 目标路径必须在源路径之外（互相包含一律拒绝）
//   2. 目标目录必须已存在（不自动创建 —— 打错一个字母不至于写到奇怪的地方）
//   3. 目标必须是可写目录，且不能是符号链接指向源
//   4. 【先复制、校验大小一致、再删源】；大小不一致则保留源并记错误
//   5. 目标已存在同名文件：大小相同视为已迁移（跳过）；不同则加序号，不覆盖
//   6. 逐个文件处理，支持取消；取消后已完成的部分保留（不留半截文件）
//   7. deleteSource=false 时只复制不删源（"另存一份"模式）
//
// ===== 接口 =====
//
//	GET  /api/storage/migrate/plan     预估（文件数/大小/目标可用空间）
//	POST /api/storage/migrate          开始迁移
//	GET  /api/storage/migrate/status   进度
//	POST /api/storage/migrate/cancel   取消

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/micam/micam/internal/record"
)

// migrateState 迁移任务的运行状态。
type migrateState struct {
	mu sync.Mutex

	State    string   `json:"state"` // idle | running | done | cancelled | error
	Dst      string   `json:"dst"`
	Src      string   `json:"src"`
	Delete   bool     `json:"deleteSource"`
	Total    int      `json:"totalFiles"`
	TotalB   int64    `json:"totalBytes"`
	Done     int      `json:"doneFiles"`
	DoneB    int64    `json:"doneBytes"`
	Skipped  int      `json:"skipped"`
	Failed   int      `json:"failed"`
	Current  string   `json:"current"`
	Message  string   `json:"message"`
	Errors   []string `json:"errors"`
	Started  string   `json:"startedAt"`
	Finished string   `json:"finishedAt"`

	cancel context.CancelFunc `json:"-"`
}

var mig = &migrateState{State: "idle"}

func (s *Server) resolveMigrateSrc(src string) string {
	if strings.TrimSpace(src) != "" {
		return filepath.Clean(src)
	}
	return s.recRoot()
}

// migrateCandidates 收集待迁移文件（支持指定源录像池、按设备名与时间范围过滤，自动跳过正在录制的活跃分片）。
func (s *Server) migrateCandidates(srcRoot, from, to, cam string) ([]string, int64, error) {
	root := s.resolveMigrateSrc(srcRoot)
	if root == "" {
		return nil, 0, fmt.Errorf("录像目录未配置")
	}

	var lo, hi time.Time
	var err error
	if from != "" {
		if lo, err = time.ParseInLocation("2006-01-02", from, time.Local); err != nil {
			return nil, 0, fmt.Errorf("起始日期格式错误")
		}
	}
	if to != "" {
		if hi, err = time.ParseInLocation("2006-01-02", to, time.Local); err != nil {
			return nil, 0, fmt.Errorf("结束日期格式错误")
		}
		hi = hi.AddDate(0, 0, 1)
	}

	now := time.Now()
	skipActive := (s != nil && s.rec != nil)
	var out []string
	var totalBytes int64

	err = filepath.Walk(root, func(p string, info os.FileInfo, e error) error {
		if e != nil || info == nil {
			return nil
		}
		if info.IsDir() {
			if p != root && strings.HasPrefix(info.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !isVideoFile(info.Name()) {
			return nil
		}
		if skipActive && now.Sub(info.ModTime()) < 30*time.Second {
			return nil
		}
		if cam != "" {
			rel, _ := filepath.Rel(root, p)
			if !strings.HasPrefix(rel, cam+string(os.PathSeparator)) {
				return nil
			}
		}
		mt := info.ModTime()
		if !lo.IsZero() && mt.Before(lo) {
			return nil
		}
		if !hi.IsZero() && !mt.Before(hi) {
			return nil
		}
		out = append(out, p)
		totalBytes += info.Size()
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	sort.Strings(out)
	return out, totalBytes, nil
}

// validateMigrate 校验源/目标，返回规范化后的绝对路径。
func (s *Server) validateMigrate(srcIn, dst string) (srcAbs, dstAbs string, err error) {
	src := s.resolveMigrateSrc(srcIn)
	if src == "" {
		return "", "", fmt.Errorf("源录像目录未配置")
	}
	if dst == "" {
		return "", "", fmt.Errorf("请填写目标路径")
	}

	srcAbs, err = filepath.Abs(src)
	if err != nil {
		return "", "", err
	}
	dstAbs, err = filepath.Abs(dst)
	if err != nil {
		return "", "", err
	}

	srcReal := srcAbs
	if r, e := filepath.EvalSymlinks(srcAbs); e == nil {
		srcReal = r
	}
	dstReal := dstAbs
	if r, e := filepath.EvalSymlinks(dstAbs); e == nil {
		dstReal = r
	}

	// 目标必须是已存在的目录（不自动创建 —— 防止打错字写到别处）
	st, err := os.Stat(dstAbs)
	if err != nil {
		return "", "", fmt.Errorf("目标路径不存在：%s", dstAbs)
	}
	if !st.IsDir() {
		return "", "", fmt.Errorf("目标路径不是目录：%s", dstAbs)
	}

	// 互相包含一律拒绝（基于真实路径防符号链接绕过）
	sep := string(os.PathSeparator)
	if srcReal == dstReal || srcAbs == dstAbs {
		return "", "", fmt.Errorf("目标路径不能与源路径相同")
	}
	if strings.HasPrefix(dstReal+sep, srcReal+sep) {
		return "", "", fmt.Errorf("目标路径不能位于源路径内部")
	}
	if strings.HasPrefix(srcReal+sep, dstReal+sep) {
		return "", "", fmt.Errorf("源路径不能位于目标路径内部")
	}

	tf := filepath.Join(dstAbs, ".micam-write-test")
	if err := os.WriteFile(tf, []byte("ok"), 0o600); err != nil {
		return "", "", fmt.Errorf("目标路径不可写：%v", err)
	}
	_ = os.Remove(tf)

	return srcAbs, dstAbs, nil
}

// crossDevice 判断两个路径是否跨文件系统（通过 Linux stat.Dev 识别）。
func crossDevice(a, b string) bool {
	var sa, sb syscall.Stat_t
	if syscall.Stat(filepath.Clean(a), &sa) != nil || syscall.Stat(filepath.Clean(b), &sb) != nil {
		return filepath.VolumeName(a) != filepath.VolumeName(b)
	}
	return sa.Dev != sb.Dev
}

// handleStorageMigratePlan GET /api/storage/migrate/plan
func (s *Server) handleStorageMigratePlan(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	src := s.resolveMigrateSrc(q.Get("src"))
	dst := q.Get("dst")
	files, total, err := s.migrateCandidates(src, q.Get("from"), q.Get("to"), q.Get("cam"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}

	var freeGB float64
	var dstErr string
	cross := false
	if dst != "" {
		if srcAbs, dstAbs, e := s.validateMigrate(src, dst); e == nil {
			freeGB = record.DiskUsageOfReadOnly(dstAbs).FreeGB
			cross = crossDevice(srcAbs, dstAbs)
		} else {
			dstErr = e.Error()
		}
	}

	writeJSON(w, 200, map[string]any{
		"src":       src,
		"files":     len(files),
		"bytes":     total,
		"sizeText":  humanSize(total),
		"dstFreeGB": freeGB,
		"enough":    freeGB*1024*1024*1024 >= float64(total),
		"crossDev":  cross,
		"dstErr":    dstErr,
	})
}

// handleStorageMigrateStart POST /api/storage/migrate
func (s *Server) handleStorageMigrateStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeErr(w, 405, "只支持 POST")
		return
	}
	var req struct {
		Src          string `json:"src"`
		Dst          string `json:"dst"`
		From         string `json:"from"`
		To           string `json:"to"`
		Cam          string `json:"cam"`
		DeleteSource bool   `json:"deleteSource"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "参数错误")
		return
	}

	mig.mu.Lock()
	if mig.State == "running" {
		mig.mu.Unlock()
		writeErr(w, 409, "已有迁移任务在进行")
		return
	}
	mig.mu.Unlock()

	srcAbs, dstAbs, err := s.validateMigrate(req.Src, req.Dst)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}

	files, totalB, err := s.migrateCandidates(srcAbs, req.From, req.To, req.Cam)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if len(files) == 0 {
		writeErr(w, 400, "所选条件下没有可迁移的录像")
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	mig.mu.Lock()
	mig.State = "running"
	mig.Dst = dstAbs
	mig.Src = srcAbs
	mig.Delete = req.DeleteSource
	mig.Total = len(files)
	mig.TotalB = totalB
	mig.Done, mig.DoneB, mig.Skipped, mig.Failed = 0, 0, 0, 0
	mig.Errors = nil
	mig.Message = ""
	mig.Current = ""
	mig.Started = time.Now().Format(time.RFC3339)
	mig.Finished = ""
	mig.cancel = cancel
	mig.mu.Unlock()

	logf("[mig] 开始迁移 %d 个文件 (%.2f GB): %s -> %s（删源=%v）", len(files), float64(totalB)/(1<<30), srcAbs, dstAbs, req.DeleteSource)
	go s.runMigrate(ctx, srcAbs, dstAbs, files)

	writeJSON(w, 200, map[string]any{"ok": true, "total": len(files), "totalBytes": totalB, "src": srcAbs, "dst": dstAbs})
}

func relocateRecMeta(oldPath, newPath string, removeOld bool) {
	metaMu.Lock()
	if m, ok := metaCache[oldPath]; ok {
		metaCache[newPath] = m
		if removeOld {
			delete(metaCache, oldPath)
		}
	}
	metaMu.Unlock()
}

// runMigrate 后台执行迁移。
func (s *Server) runMigrate(ctx context.Context, srcRoot, dstRoot string, files []string) {
	sameFS := !crossDevice(srcRoot, dstRoot)

	fail := func(p, msg string) {
		mig.mu.Lock()
		mig.Failed++
		if len(mig.Errors) < 50 {
			mig.Errors = append(mig.Errors, p+": "+msg)
		}
		mig.mu.Unlock()
	}

	for _, p := range files {
		select {
		case <-ctx.Done():
			mig.mu.Lock()
			mig.State = "cancelled"
			mig.Finished = time.Now().Format(time.RFC3339)
			mig.Message = "已取消"
			mig.mu.Unlock()
			logf("[mig] 已取消")
			return
		default:
		}

		rel, err := filepath.Rel(srcRoot, p)
		if err != nil {
			fail(p, "无法计算相对路径")
			continue
		}
		target := filepath.Join(dstRoot, rel)

		mig.mu.Lock()
		mig.Current = rel
		mig.mu.Unlock()

		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			fail(rel, "创建目录失败")
			continue
		}

		si, err := os.Stat(p)
		if err != nil {
			fail(rel, "读取源文件失败")
			continue
		}

		// 目标已存在：大小一致视为已迁移（跳过）
		if ti, e := os.Stat(target); e == nil {
			if ti.Size() == si.Size() {
				relocateRecMeta(p, target, mig.Delete)
				mig.mu.Lock()
				mig.Skipped++
				mig.Done++
				mig.DoneB += si.Size()
				mig.mu.Unlock()
				if mig.Delete {
					if err := os.Remove(p); err != nil {
						fail(rel, "副本已存在，但删除源文件失败: "+err.Error())
					} else {
						removeEmptyParentDirs(p, srcRoot)
					}
				}
				continue
			}
			// 大小不同 -> 加序号，绝不覆盖
			target = fmt.Sprintf("%s.%d.migrated", target, time.Now().UnixNano()%100000)
		}

		// 同文件系统且删源时优先使用零拷贝原子 Rename
		if sameFS && mig.Delete {
			if err := os.Rename(p, target); err == nil {
				relocateRecMeta(p, target, true)
				removeEmptyParentDirs(p, srcRoot)
				mig.mu.Lock()
				mig.Done++
				mig.DoneB += si.Size()
				mig.mu.Unlock()
				continue
			}
		}

		if err := copyFile(p, target); err != nil {
			_ = os.Remove(target) // 清掉半截文件
			fail(rel, "复制失败: "+err.Error())
			continue
		}

		// 校验大小一致才删源（关键安全步骤）
		ti, err := os.Stat(target)
		if err != nil || ti.Size() != si.Size() {
			fail(rel, "复制后大小不一致，已保留源文件")
			continue
		}

		relocateRecMeta(p, target, mig.Delete)
		if mig.Delete {
			if err := os.Remove(p); err != nil {
				fail(rel, "删除源文件失败（副本已就绪）")
			} else {
				removeEmptyParentDirs(p, srcRoot)
			}
		}

		mig.mu.Lock()
		mig.Done++
		mig.DoneB += si.Size()
		mig.mu.Unlock()
	}

	mig.mu.Lock()
	mig.State = "done"
	mig.Current = ""
	mig.Finished = time.Now().Format(time.RFC3339)
	msg := fmt.Sprintf("完成：%d 个文件", mig.Done)
	if mig.Failed > 0 {
		msg += fmt.Sprintf("，失败 %d 个", mig.Failed)
	}
	if mig.Skipped > 0 {
		msg += fmt.Sprintf("，跳过 %d 个（目标已存在）", mig.Skipped)
	}
	mig.Message = msg
	mig.mu.Unlock()
	saveRecMeta()
	invalidateRecRootsCache()
	go s.checkStorageOnce()
	logf("[mig] %s", msg)
}

// handleStorageMigrateStatus GET /api/storage/migrate/status
func (s *Server) handleStorageMigrateStatus(w http.ResponseWriter, r *http.Request) {
	mig.mu.Lock()
	defer mig.mu.Unlock()
	writeJSON(w, 200, map[string]any{
		"state":        mig.State,
		"src":          mig.Src,
		"dst":          mig.Dst,
		"deleteSource": mig.Delete,
		"totalFiles":   mig.Total,
		"totalBytes":   mig.TotalB,
		"doneFiles":    mig.Done,
		"doneBytes":    mig.DoneB,
		"doneText":     humanSize(mig.DoneB),
		"skipped":      mig.Skipped,
		"failed":       mig.Failed,
		"current":      mig.Current,
		"message":      mig.Message,
		"errors":       mig.Errors,
		"startedAt":    mig.Started,
		"finishedAt":   mig.Finished,
	})
}

// handleStorageMigrateCancel POST /api/storage/migrate/cancel
func (s *Server) handleStorageMigrateCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeErr(w, 405, "只支持 POST")
		return
	}
	mig.mu.Lock()
	c := mig.cancel
	state := mig.State
	mig.mu.Unlock()
	if state != "running" || c == nil {
		writeErr(w, 400, "没有正在进行的迁移")
		return
	}
	c()
	writeJSON(w, 200, map[string]any{"ok": true, "msg": "正在停止（当前文件完成后停止）"})
}

// copyFile 复制文件并保留原始修改时间（保证迁移后的录像仍按真实录制日期参与保留期限统计与清理）。
func copyFile(src, dst string) error {
	info, statErr := os.Stat(src)
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if statErr == nil {
		_ = os.Chtimes(dst, info.ModTime(), info.ModTime())
	}
	return nil
}

// humanSize 人类可读体积。
func humanSize(b int64) string {
	f := float64(b)
	switch {
	case f >= 1<<40:
		return fmt.Sprintf("%.2f TB", f/(1<<40))
	case f >= 1<<30:
		return fmt.Sprintf("%.2f GB", f/(1<<30))
	case f >= 1<<20:
		return fmt.Sprintf("%.1f MB", f/(1<<20))
	case f >= 1<<10:
		return fmt.Sprintf("%.1f KB", f/(1<<10))
	}
	return fmt.Sprintf("%d B", b)
}
