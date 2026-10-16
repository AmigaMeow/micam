package main

// 录像存储位置。
//
// ===== 飞牛的存储权限模型 =====
//
// /vol*            各存储池根目录（需管理员权限）
// /vol*/@appdata/<app>/   插件私有数据（卸载可能清理）
// /vol*/@appshare/<app>/  应用共享目录（属主=插件用户，推荐）
//
// 高级用户可手填绝对路径（如 /vol2/recordings 或自定义共享文件夹），
// 前提是在飞牛文件管理器中为 micam 应用赋予读写权限。

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

type storageLocation struct {
	Path     string  `json:"path"`
	Name     string  `json:"name"`
	Kind     string  `json:"kind"` // appshare / appdata / volume / custom
	Desc     string  `json:"desc"`
	FreeGB   float64 `json:"freeGB"`
	TotalGB  float64 `json:"totalGB"`
	Writable bool    `json:"writable"`
	Current  bool    `json:"current"`
	Badge    string  `json:"badge"` // 推荐 / 默认
}

func friendlyVolName(vol string) string {
	v := strings.ToLower(vol)
	if strings.HasPrefix(v, "vol0") {
		return "存储空间 " + strings.TrimPrefix(v, "vol0")
	}
	if strings.HasPrefix(v, "vol") {
		return "存储空间 " + strings.TrimPrefix(v, "vol")
	}
	return vol
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
		} else if parent := filepath.Dir(path); syscall.Statfs(parent, &st) == nil {
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

	// 1. 飞牛应用共享目录（TRIM_APPSHARE 环境变量）
	if v := os.Getenv("TRIM_APPSHARE"); v != "" {
		appshare := filepath.Join(v, "data", "recordings")
		add(appshare, "应用共享目录 (开箱即用)", "appshare",
			"飞牛文件管理器可见，卸载插件也不会删", "推荐")
	}

	// 2. 动态扫描所有存储卷下的 @appshare/micam (/vol1, /vol2, /vol3 等)
	if shares, err := filepath.Glob("/vol*/@appshare/micam"); err == nil {
		for _, sh := range shares {
			recPath := filepath.Join(sh, "data", "recordings")
			volName := friendlyVolName(filepath.Base(filepath.Dir(filepath.Dir(sh))))
			add(recPath, "应用共享目录 ("+volName+")", "appshare",
				"飞牛文件管理器可见，数据独立保存", "推荐")
		}
	}

	// 3. 插件数据目录（默认备选）
	add(filepath.Join(s.dataDir, "recordings"), "插件数据目录 (默认)", "appdata",
		"跟随插件，卸载时可能被清理", "")

	// 4. 重点：读取飞牛系统用户共享目录配置（/etc/samba/users/*.share.conf）
	// 这包含了用户在飞牛 NAS 中创建的所有共享文件夹（如「测试小米存储」、「摄像头」等）
	if confFiles, err := filepath.Glob("/etc/samba/users/*.share.conf"); err == nil {
		for _, cf := range confFiles {
			raw, err := os.ReadFile(cf)
			if err != nil {
				continue
			}
			lines := strings.Split(string(raw), "\n")
			currentSection := ""
			for _, l := range lines {
				l = strings.TrimSpace(l)
				if strings.HasPrefix(l, "[") && strings.HasSuffix(l, "]") {
					currentSection = l[1 : len(l)-1]
				} else if strings.HasPrefix(l, "path =") && currentSection != "" {
					p := strings.TrimSpace(strings.TrimPrefix(l, "path ="))
					if p != "" {
						vol := "存储空间"
						parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
						if len(parts) > 0 {
							vol = friendlyVolName(parts[0])
						}
						add(p, vol+" / "+currentSection, "share", "飞牛用户共享文件夹", "")
					}
				}
			}
		}
	}

	// 5. 扫描所有存储卷的用户文件夹与外接存储 (/vol*, /vol02 等)
	if volRoots, err := filepath.Glob("/vol*"); err == nil {
		for _, vr := range volRoots {
			ents, err := os.ReadDir(vr)
			if err != nil {
				continue
			}
			volName := friendlyVolName(filepath.Base(vr))
			for _, ent := range ents {
				if !ent.IsDir() {
					continue
				}
				name := ent.Name()
				if strings.HasPrefix(name, "@") || strings.HasPrefix(name, ".") {
					continue
				}

				// 若为用户 UID 目录（如 1000），扫描其下的实际文件夹
				subEnts, subErr := os.ReadDir(filepath.Join(vr, name))
				if subErr == nil && len(subEnts) > 0 {
					for _, se := range subEnts {
						if !se.IsDir() || strings.HasPrefix(se.Name(), ".") || strings.HasPrefix(se.Name(), "@") {
							continue
						}
						p := filepath.Join(vr, name, se.Name())
						add(p, volName+" / "+se.Name(), "share", "飞牛个人文件夹", "")
					}
				} else {
					cand := filepath.Join(vr, name, "recordings")
					add(cand, volName+" / "+name, "volume", "存储池 "+volName+" 文件夹", "")
				}
			}
		}
	}

	// 6. 当前配置的路径始终包含
	add(cur, "当前使用路径", "custom", "当前生效的录像存储目录", "")

	sort.Slice(out, func(i, j int) bool {
		if out[i].Current != out[j].Current {
			return out[i].Current
		}
		if out[i].Writable != out[j].Writable {
			return out[i].Writable
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
	})

	writeJSON(w, 200, map[string]any{
		"locations": out,
		"current":   cur,
		"note": "飞牛系统提示：插件支持保存到任意存储池。如需录到非 @appshare 共享目录，" +
			"请先在飞牛「文件管理」中为该目录分配 micam 应用的读写权限。",
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
	if clean == "/" || filepath.Dir(clean) == "/" {
		writeErr(w, 400, "不能直接使用根目录或存储卷根路径: "+clean)
		return
	}
	for _, bad := range []string{"/etc", "/usr", "/bin", "/sbin", "/proc", "/sys", "/dev", "/boot", "/var"} {
		if clean == bad || strings.HasPrefix(clean, bad+"/") {
			writeErr(w, 400, "不能使用系统核心目录: "+clean)
			return
		}
	}

	if err := os.MkdirAll(clean, 0o755); err != nil {
		writeErr(w, 400, "创建目录失败（无权限）：飞牛系统限制了应用跨目录写入。请在飞牛桌面打开「应用中心」→「小米摄像头」→「设置」→「访问权限」中点击「+ 添加」该文件夹并赋予读写权限，或在文件管理中右键赋予 micam 用户权限: "+err.Error())
		return
	}
	if !isWritable(clean) {
		writeErr(w, 400, "目录不可写（无写入权限）：请在飞牛桌面打开「应用中心」→「小米摄像头」→「设置」→「访问权限」中点击「+ 添加」该文件夹并赋予读写权限，或在文件管理中右键该文件夹属性为 micam 用户分配读写权限")
		return
	}

	s.cfg.SetRecordDir(clean)
	cur := s.rec.GetConfig()
	cur.Dir = clean
	s.rec.SetConfig(cur)

	logf("[storage] 录像目录已成功切换至: %s", clean)
	writeJSON(w, 200, map[string]any{"ok": true, "dir": clean})
}
