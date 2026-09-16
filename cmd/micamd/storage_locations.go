package main

// 录像存储位置。
//
// ===== 飞牛的存储权限模型（实测结论）=====
//
// /vol1            d--------- root     插件用户**连列出都不行**
// /vol1/1000       d--------- 用户     插件的用户账户无权访问
// /vol1/@appdata/<app>/   插件私有数据（卸载可能清理）
// /vol1/@appshare/<app>/  应用共享目录（属主=插件用户）
//
// 所以插件**不能**随便往用户的共享文件夹里写。
// 正确做法是用 @appshare —— 它是飞牛给应用分配的共享空间，
// 用户在飞牛文件管理器里能看到，但权限由飞牛管理。
//
// 高级用户可以手填绝对路径（前提是自己在飞牛里放过权限）。

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"syscall"
)

type storageLocation struct {
	Path     string  `json:"path"`
	Name     string  `json:"name"`
	Kind     string  `json:"kind"` // appshare / appdata / custom
	Desc     string  `json:"desc"`
	FreeGB   float64 `json:"freeGB"`
	TotalGB  float64 `json:"totalGB"`
	Writable bool    `json:"writable"`
	Current  bool    `json:"current"`
	Badge    string  `json:"badge"` // 推荐 / 默认
}

// handleStorageLocations 列出可选的录像存储位置。
func (s *Server) handleStorageLocations(w http.ResponseWriter, r *http.Request) {
	cur := s.rec.GetConfig().Dir
	if cur == "" {
		cur = filepath.Join(s.dataDir, "recordings")
	}

	var out []storageLocation
	seen := map[string]bool{}

	add := func(path, name, kind, desc, badge string) {
		if path == "" || seen[path] {
			return
		}
		seen[path] = true

		st := syscall.Statfs_t{}
		var freeGB, totalGB float64
		if syscall.Statfs(path, &st) == nil {
			freeGB = float64(st.Bavail) * float64(st.Bsize) / (1 << 30)
			totalGB = float64(st.Blocks) * float64(st.Bsize) / (1 << 30)
		}

		out = append(out, storageLocation{
			Path: path, Name: name, Kind: kind, Desc: desc, Badge: badge,
			FreeGB: freeGB, TotalGB: totalGB,
			Writable: isWritable(path),
			Current:  path == cur,
		})
	}

	// 应用共享目录（推荐）：用户能在飞牛文件管理器看到
	appshare := "/vol1/@appshare/micam/data/recordings"
	if v := os.Getenv("TRIM_APPSHARE"); v != "" {
		appshare = filepath.Join(v, "data", "recordings")
	}
	add(appshare, "应用共享目录", "appshare",
		"飞牛文件管理器可见，卸载插件也不会删", "推荐")

	// 插件数据目录（默认）
	add(filepath.Join(s.dataDir, "recordings"), "插件数据目录", "appdata",
		"跟随插件，卸载时可能被清理", "");

	// 外接/网络存储（如果有）
	if ents, err := os.ReadDir("/vol02"); err == nil {
		for _, ent := range ents {
			if !ent.IsDir() {
				continue
			}
			full := filepath.Join("/vol02", ent.Name())
			add(full, "外接存储 "+ent.Name(), "external", "外接盘或网络存储", "")
		}
	}

	// 当前值不在候选里也补上
	add(cur, "当前使用", "custom", "手动配置的路径", "")

	sort.Slice(out, func(i, j int) bool {
		if out[i].Current != out[j].Current {
			return out[i].Current
		}
		// 推荐的排前面
		r := func(x storageLocation) int {
			if x.Badge == "推荐" {
				return 0
			}
			if x.Kind == "appdata" {
				return 1
			}
			return 2
		}
		if r(out[i]) != r(out[j]) {
			return r(out[i]) < r(out[j])
		}
		return out[i].Path < out[j].Path
	});

	writeJSON(w, 200, map[string]any{
		"locations": out,
		"current":   cur,
		"note": "飞牛限制：插件只能写入自己的应用目录。" +
			"如需录到其他位置，请先在飞牛「文件管理」中给 micam 应用授权。",
	})
}

// isWritable 测试目录是否可写（实际创建临时文件）。
func isWritable(dir string) bool {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	f, err := os.CreateTemp(dir, ".micam-wtest-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

// handleSetRecordDir 切换录像目录。
func (s *Server) handleSetRecordDir(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 POST")
		return
	}

	var req struct {
		Dir string `json:"dir"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Dir == "" {
		writeErr(w, 400, "参数错误")
		return
	}

	if !filepath.IsAbs(req.Dir) {
		writeErr(w, 400, "必须是绝对路径")
		return
	}
	clean := filepath.Clean(req.Dir)
	for _, bad := range []string{"/", "/etc", "/usr", "/bin", "/sbin", "/proc", "/sys", "/dev", "/boot", "/var", "/vol1", "/vol02"} {
		if clean == bad {
			writeErr(w, 400, "不能使用系统目录: "+clean)
			return
		}
	}

	if err := os.MkdirAll(clean, 0o755); err != nil {
		writeErr(w, 400, "无法创建目录（飞牛可能未给 micam 应用授权该目录）: "+err.Error())
		return
	}
	if !isWritable(clean) {
		writeErr(w, 400, "目录不可写 —— 请在飞牛「文件管理」中给 micam 应用授权该目录")
		return
	}

	s.cfg.SetRecordDir(clean)
	cur := s.rec.GetConfig()
	cur.Dir = clean
	s.rec.SetConfig(cur)

	logf("[storage] 录像目录已切换: %s", clean)
	writeJSON(w, 200, map[string]any{"ok": true, "dir": clean})
}
