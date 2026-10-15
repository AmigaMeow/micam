package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

type backupInfo struct {
	CreatedAt   string   `json:"createdAt"`
	Version     string   `json:"version"`
	Files       []string `json:"files"`
	SizeMB      float64  `json:"sizeMB"`
	RestorePath string   `json:"restorePath"`
}

func copyBackupFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

func (s *Server) handleSystemBackup(w http.ResponseWriter, r *http.Request) {
	backupDir := filepath.Join(s.dataDir, "backup")

	if r.Method == http.MethodGet {
		infoPath := filepath.Join(backupDir, "backup_info.json")
		data, err := os.ReadFile(infoPath)
		if err != nil {
			writeJSON(w, 200, map[string]any{"hasBackup": false})
			return
		}
		var info backupInfo
		if err := json.Unmarshal(data, &info); err != nil {
			writeJSON(w, 200, map[string]any{"hasBackup": false})
			return
		}
		writeJSON(w, 200, map[string]any{"hasBackup": true, "info": info})
		return
	}

	if r.Method != http.MethodPost {
		writeErr(w, 405, "只支持 GET/POST")
		return
	}

	_ = os.MkdirAll(filepath.Join(backupDir, "bin"), 0755)
	_ = os.MkdirAll(filepath.Join(backupDir, "web"), 0755)
	_ = os.MkdirAll(filepath.Join(backupDir, "configs"), 0755)

	var backedFiles []string
	var totalBytes int64

	// 1. 备份核心二进制
	binSrc := "/vol1/@appcenter/micam/server/micamd"
	binDst := filepath.Join(backupDir, "bin", "micamd")
	if fi, err := os.Stat(binSrc); err == nil {
		if err := copyBackupFile(binSrc, binDst); err == nil {
			backedFiles = append(backedFiles, "bin/micamd")
			totalBytes += fi.Size()
		}
	}

	// 2. 备份前端完整包
	webSrc := filepath.Join(s.dataDir, "web", "index.html")
	webDst := filepath.Join(backupDir, "web", "index.html")
	if fi, err := os.Stat(webSrc); err == nil {
		if err := copyBackupFile(webSrc, webDst); err == nil {
			backedFiles = append(backedFiles, "web/index.html")
			totalBytes += fi.Size()
		}
	}

	// 3. 备份核心凭据与配置
	configFiles := []string{
		"config.json",
		"sources.json",
		"camera-poses.json",
		"miot-oauth.json",
		"accounts.json",
	}
	for _, cf := range configFiles {
		src := filepath.Join(s.dataDir, cf)
		dst := filepath.Join(backupDir, "configs", cf)
		if fi, err := os.Stat(src); err == nil {
			if err := copyBackupFile(src, dst); err == nil {
				backedFiles = append(backedFiles, "configs/"+cf)
				totalBytes += fi.Size()
			}
		}
	}

	// 4. 生成一键还原脚本
	restoreScript := `#!/bin/bash
# ==========================================
#   MiCam 小米摄像机插件一键系统还原工具
# ==========================================
set -e
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
echo "正在开始还原 MiCam 系统..."

# 1. 恢复核心二进制
if [ -f "$DIR/bin/micamd" ]; then
    echo "  [1/4] 还原后端二进制..."
    cp -f "$DIR/bin/micamd" /vol1/@appcenter/micam/server/micamd
    chown micam:AppUsers /vol1/@appcenter/micam/server/micamd 2>/dev/null || true
    chmod +x /vol1/@appcenter/micam/server/micamd
fi

# 2. 恢复前端网页
if [ -f "$DIR/web/index.html" ]; then
    echo "  [2/4] 还原前端页面..."
    cp -f "$DIR/web/index.html" /vol1/@appcenter/micam/www/index.html 2>/dev/null || true
    cp -f "$DIR/web/index.html" /vol1/@appdata/micam/web/index.html 2>/dev/null || true
fi

# 3. 恢复核心配置（若原配置不存在时兜底）
echo "  [3/4] 检查核心配置..."
for cfg in "$DIR/configs"/*.json; do
    fname="$(basename "$cfg")"
    if [ ! -f "/vol1/@appdata/micam/$fname" ]; then
        cp -n "$cfg" "/vol1/@appdata/micam/$fname" 2>/dev/null || true
    fi
done

# 4. 重启 Docker 服务
echo "  [4/4] 重启服务容器..."
docker restart micam-srv || true

echo "=========================================="
echo "✅ 还原完成！服务已恢复就绪。"
echo "=========================================="
`
	restorePath := filepath.Join(backupDir, "restore.sh")
	_ = os.WriteFile(restorePath, []byte(restoreScript), 0755)

	info := backupInfo{
		CreatedAt:   time.Now().Format("2006-01-02 15:04:05"),
		Version:     "v0.40.1",
		Files:       backedFiles,
		SizeMB:      float64(totalBytes) / (1024 * 1024),
		RestorePath: restorePath,
	}

	infoBytes, _ := json.MarshalIndent(info, "", "  ")
	_ = os.WriteFile(filepath.Join(backupDir, "backup_info.json"), infoBytes, 0644)

	logf("[backup] ✅ 系统一键备份完成，共 %d 个文件，%.2f MB -> %s", len(backedFiles), info.SizeMB, backupDir)
	writeJSON(w, 200, map[string]any{
		"ok":   true,
		"info": info,
		"msg":  fmt.Sprintf("备份完成（共 %d 个文件，%.2f MB）", len(backedFiles), info.SizeMB),
	})
}
