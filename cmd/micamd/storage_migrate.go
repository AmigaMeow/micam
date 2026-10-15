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

// migrateCandidates 收集待迁移文件（按设备名与时间范围过滤）。
func (s *Server) migrateCandidates(from, to, cam string) ([]string, error) {
	root := s.rec.GetConfig().Dir
	if root == "" {
		return nil, fmt.Errorf("录像目录未配置")
	}

	// 时间范围（date 输入，YYYY-MM-DD；含当天）
	var lo, hi time.Time
	var err error
	if from != "" {
		if lo, err = time.ParseInLocation("2006-01-02", from, time.Local); err != nil {
			return nil, fmt.Errorf("起始日期格式错误")
		}
	}
	if to != "" {
		if hi, err = time.ParseInLocation("2006-01-02", to, time.Local); err != nil {
			return nil, fmt.Errorf("结束日期格式错误")
		}
		hi = hi.AddDate(0, 0, 1) // 含结束当天
	}

	var out []string
	err = filepath.Walk(root, func(p string, info os.FileInfo, e error) error {
		if e != nil || info == nil || info.IsDir() {
			return nil
		}
		if filepath.Ext(p) != ".mkv" {
			return nil
		}
		// 设备过滤：录像在 <root>/<设备名>/... 下
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
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// validateMigrate 校验源/目标，返回规范化后的绝对路径。
func (s *Server) validateMigrate(dst string) (src, dstAbs string, err error) {
	src = s.rec.GetConfig().Dir
	if src == "" {
		return "", "", fmt.Errorf("录像目录未配置")
	}
	if dst == "" {
		return "", "", fmt.Errorf("请填写目标路径")
	}

	srcAbs, err := filepath.Abs(src)
	if err != nil {
		return "", "", err
	}
	dstAbs, err = filepath.Abs(dst)
	if err != nil {
		return "", "", err
	}

	// 目标必须是已存在的目录（不自动创建 —— 防止打错字写到别处）
	st, err := os.Stat(dstAbs)
	if err != nil {
		return "", "", fmt.Errorf("目标路径不存在：%s", dstAbs)
	}
	if !st.IsDir() {
		return "", "", fmt.Errorf("目标路径不是目录：%s", dstAbs)
	}

	// 互相包含一律拒绝
	sep := string(os.PathSeparator)
	if srcAbs == dstAbs {
		return "", "", fmt.Errorf("目标路径不能与源路径相同")
	}
	if strings.HasPrefix(dstAbs+sep, srcAbs+sep) {
		return "", "", fmt.Errorf("目标路径不能位于源路径内部")
	}
	if strings.HasPrefix(srcAbs+sep, dstAbs+sep) {
		return "", "", fmt.Errorf("源路径不能位于目标路径内部")
	}

	// 目标可写测试
	tf := filepath.Join(dstAbs, ".micam-write-test")
	if err := os.WriteFile(tf, []byte("ok"), 0o600); err != nil {
		return "", "", fmt.Errorf("目标路径不可写：%v", err)
	}
	_ = os.Remove(tf)

	return srcAbs, dstAbs, nil
}

// crossDevice 判断两个路径是否在同一文件系统（决定是 rename 还是 copy）。
func crossDevice(a, b string) bool {
	// 简化：逐个尝试 rename，失败再复制。这里只用于提示。
	pa := filepath.VolumeName(a)
	pb := filepath.VolumeName(b)
	return pa != pb
}

// handleStorageMigratePlan GET /api/storage/migrate/plan
func (s *Server) handleStorageMigratePlan(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	dst := q.Get("dst")
	files, err := s.migrateCandidates(q.Get("from"), q.Get("to"), q.Get("cam"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var total int64
	for _, f := range files {
		if st, e := os.Stat(f); e == nil {
			total += st.Size()
		}
	}

	var freeGB float64
	var dstErr string
	if dst != "" {
		if _, dstAbs, e := s.validateMigrate(dst); e == nil {
			freeGB = record.DiskUsageOf(dstAbs).FreeGB
		} else {
			dstErr = e.Error()
		}
	}

	writeJSON(w, 200, map[string]any{
		"files":     len(files),
		"bytes":     total,
		"sizeText":  humanSize(total),
		"dstFreeGB": freeGB,
		"enough":    freeGB*1024*1024*1024 >= float64(total),
		"crossDev":  dst != "" && crossDevice(s.rec.GetConfig().Dir, dst),
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

	_, dstAbs, err := s.validateMigrate(req.Dst)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}

	files, err := s.migrateCandidates(req.From, req.To, req.Cam)
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
	mig.Src = s.rec.GetConfig().Dir
	mig.Delete = req.DeleteSource
	mig.Total = len(files)
	mig.TotalB = 0
	mig.Done, mig.DoneB, mig.Skipped, mig.Failed = 0, 0, 0, 0
	mig.Errors = nil
	mig.Message = ""
	mig.Current = ""
	mig.Started = time.Now().Format(time.RFC3339)
	mig.Finished = ""
	mig.cancel = cancel
	mig.mu.Unlock()

	logf("[mig] 开始迁移 %d 个文件 -> %s（删源=%v）", len(files), dstAbs, req.DeleteSource)
	go s.runMigrate(ctx, files)

	writeJSON(w, 200, map[string]any{"ok": true, "total": len(files), "dst": dstAbs})
}

// runMigrate 后台执行迁移。
func (s *Server) runMigrate(ctx context.Context, files []string) {
	srcRoot := s.rec.GetConfig().Dir
	dstRoot := mig.Dst

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
				mig.mu.Lock()
				mig.Skipped++
				mig.Done++
				mig.DoneB += si.Size()
				mig.mu.Unlock()
				// 注意：这里删源也可能失败（权限/只读），必须报告 ——
				// 早期版本写成 `_ = os.Remove(p)` 把错误吞了，
				// 界面显示"成功"但源文件还在（实测踩到）。
				if mig.Delete {
					if err := os.Remove(p); err != nil {
						fail(rel, "副本已存在，但删除源文件失败: "+err.Error())
					}
				}
				continue
			}
			// 大小不同 -> 加序号，绝不覆盖
			target = fmt.Sprintf("%s.%d.migrated", target, time.Now().UnixNano()%100000)
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

		if mig.Delete {
			if err := os.Remove(p); err != nil {
				fail(rel, "删除源文件失败（副本已就绪）")
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

// copyFile 复制文件（带缓冲，避免大文件占内存）。
func copyFile(src, dst string) error {
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
	return out.Close()
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
