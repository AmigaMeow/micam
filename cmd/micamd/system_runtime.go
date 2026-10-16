package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/micam/micam/internal/miot"
)

var (
	fetchMu       sync.Mutex
	isFetchingLib bool
	fetchError    string
	fetchProgress string
)

func (s *Server) handleSystemRuntime(w http.ResponseWriter, r *http.Request) {
	enc := s.activeEncoderName()
	isHw := strings.Contains(enc, "_qsv") || strings.Contains(enc, "_nvenc") || strings.Contains(enc, "_vaapi") || strings.Contains(enc, "_amf")

	var hwDesc string
	switch {
	case strings.Contains(enc, "_qsv"):
		hwDesc = "Intel QuickSync (QSV) 核显硬件加速"
	case strings.Contains(enc, "_nvenc"):
		hwDesc = "NVIDIA NVENC 硬件加速"
	case strings.Contains(enc, "_vaapi"):
		hwDesc = "VAAPI 通用硬件加速"
	default:
		hwDesc = "CPU 软件自适应编码模式 (低功耗超快预设)"
	}

	p := miot.FindLib("")
	installed := (p != "")
	var sizeBytes int64
	if installed {
		if fi, err := os.Stat(p); err == nil {
			sizeBytes = fi.Size()
		}
	}

	fetchMu.Lock()
	fetching := isFetchingLib
	errMsg := fetchError
	progMsg := fetchProgress
	fetchMu.Unlock()

	writeJSON(w, 200, map[string]any{
		"ok": true,
		"miotLib": map[string]any{
			"installed": installed,
			"path":      p,
			"sizeBytes": sizeBytes,
			"source":    "XiaoMi/xiaomi-miloco (官方开源仓库)",
			"fetching":  fetching,
			"progress":  progMsg,
			"error":     errMsg,
		},
		"hardware": map[string]any{
			"encoder":     enc,
			"isHardware":  isHw,
			"description": hwDesc,
			"arch":        runtime.GOARCH,
			"os":          runtime.GOOS,
		},
		"storage": map[string]any{
			"cacheMaxGB": s.cfg.TranscodeCacheGB,
		},
	})
}

func (s *Server) activeEncoderName() string {
	enc, _ := s.pickFileEncoder("")
	return enc
}

func (s *Server) handleMiotLibFetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "Method Not Allowed")
		return
	}

	fetchMu.Lock()
	if isFetchingLib {
		fetchMu.Unlock()
		writeJSON(w, 200, map[string]any{"ok": true, "msg": "正在下载中，请稍候..."})
		return
	}
	isFetchingLib = true
	fetchError = ""
	fetchProgress = "正在连接官方开源组件源..."
	fetchMu.Unlock()

	go s.performLibFetch()

	writeJSON(w, 200, map[string]any{
		"ok":  true,
		"msg": "已触发从小米官方开源仓库自动拉取组件",
	})
}

func (s *Server) performLibFetch() {
	defer func() {
		fetchMu.Lock()
		isFetchingLib = false
		fetchMu.Unlock()
	}()

	arch := runtime.GOARCH
	var subPath string
	var fileName string
	switch arch {
	case "amd64":
		subPath = "linux/x86_64"
		fileName = "libmiot_camera_lite.so"
	case "arm64":
		subPath = "linux/arm64"
		fileName = "libmiot_camera_lite.so"
	default:
		subPath = "linux/x86_64"
		fileName = "libmiot_camera_lite.so"
	}

	rawUrl := fmt.Sprintf("https://raw.githubusercontent.com/XiaoMi/xiaomi-miloco/main/backend/miot/src/miot/libs/%s/%s", subPath, fileName)
	mirrors := []string{
		fmt.Sprintf("https://ghproxy.net/%s", rawUrl),
		fmt.Sprintf("https://mirror.ghproxy.com/%s", rawUrl),
		rawUrl,
		fmt.Sprintf("https://gh-proxy.com/%s", rawUrl),
	}

	targetDir := filepath.Join(s.dataDir, "lib")
	_ = os.MkdirAll(targetDir, 0755)
	targetFile := filepath.Join(targetDir, fileName)

	client := &http.Client{Timeout: 60 * time.Second}
	var success bool

	for idx, u := range mirrors {
		fetchMu.Lock()
		fetchProgress = fmt.Sprintf("正在从节点 %d/4 下载组件...", idx+1)
		fetchMu.Unlock()
		logf("[miot] 正在下载闭源组件: %s", u)

		resp, err := client.Get(u)
		if err != nil {
			logf("[miot] 节点 %s 连接失败: %v", u, err)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			logf("[miot] 节点 %s 响应异常码: %d", u, resp.StatusCode)
			continue
		}

		tmpFile := targetFile + ".tmp"
		f, err := os.Create(tmpFile)
		if err != nil {
			resp.Body.Close()
			logf("[miot] 无法创建临时文件: %v", err)
			continue
		}

		n, err := io.Copy(f, resp.Body)
		f.Close()
		resp.Body.Close()

		if err != nil || n < 1000000 {
			_ = os.Remove(tmpFile)
			logf("[miot] 下载内容异常（大小 %d 字节）: %v", n, err)
			continue
		}

		_ = os.Chmod(tmpFile, 0755)
		if err := os.Rename(tmpFile, targetFile); err != nil {
			_ = os.Remove(tmpFile)
			logf("[miot] 文件重命名失败: %v", err)
			continue
		}

		// 同步到 @appcenter 目录（若存在）
		appcenterLib := filepath.Join("/vol1/@appcenter/micam/lib", fileName)
		_ = os.MkdirAll(filepath.Dir(appcenterLib), 0755)
		if copyBackupFile(targetFile, appcenterLib) == nil {
			_ = os.Chmod(appcenterLib, 0755)
		}

		success = true
		logf("[miot] 成功从官方开源源就绪组件: %s (大小 %.1fMB)", targetFile, float64(n)/(1<<20))
		break
	}

	fetchMu.Lock()
	if success {
		fetchProgress = "下载完成，组件已就绪"
		checkMiotLib()
	} else {
		fetchError = "所有镜像源下载超时，请检查 NAS 外网连接或通过界面手动上传"
		fetchProgress = ""
	}
	fetchMu.Unlock()
}

func (s *Server) handleMiotLibUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "Method Not Allowed")
		return
	}

	// 限制 32MB
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, 400, "解析文件失败: "+err.Error())
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeErr(w, 400, "缺少上传文件")
		return
	}
	defer file.Close()

	if !strings.HasSuffix(header.Filename, ".so") && !strings.HasSuffix(header.Filename, ".dylib") {
		writeErr(w, 400, "仅支持 .so 或 .dylib 库文件")
		return
	}

	targetDir := filepath.Join(s.dataDir, "lib")
	_ = os.MkdirAll(targetDir, 0755)
	targetFile := filepath.Join(targetDir, "libmiot_camera_lite.so")

	tmp := targetFile + ".upload"
	dst, err := os.Create(tmp)
	if err != nil {
		writeErr(w, 500, "创建文件失败: "+err.Error())
		return
	}

	n, err := io.Copy(dst, file)
	dst.Close()
	if err != nil || n < 1000000 {
		_ = os.Remove(tmp)
		writeErr(w, 400, "文件过小或损坏")
		return
	}

	_ = os.Chmod(tmp, 0755)
	if err := os.Rename(tmp, targetFile); err != nil {
		_ = os.Remove(tmp)
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}

	appcenterLib := filepath.Join("/vol1/@appcenter/micam/lib", "libmiot_camera_lite.so")
	_ = os.MkdirAll(filepath.Dir(appcenterLib), 0755)
	if copyBackupFile(targetFile, appcenterLib) == nil {
		_ = os.Chmod(appcenterLib, 0755)
	}

	checkMiotLib()
	logf("[miot] 用户手动上传组件已激活: %s", targetFile)

	writeJSON(w, 200, map[string]any{
		"ok":   true,
		"msg":  "组件上传并激活成功",
		"path": targetFile,
	})
}


type updateInfo struct {
	Ok             bool   `json:"ok"`
	CurrentVersion string `json:"currentVersion"`
	LatestVersion  string `json:"latestVersion"`
	HasUpdate      bool   `json:"hasUpdate"`
	ReleaseName    string `json:"releaseName"`
	ReleaseURL     string `json:"releaseUrl"`
	DownloadURL    string `json:"downloadUrl"`
	PublishedAt    string `json:"publishedAt"`
	Changelog      string `json:"changelog"`
	CheckedAt      string `json:"checkedAt"`
	Error          string `json:"error,omitempty"`
}

var (
	updateCacheMu    sync.Mutex
	lastUpdateCheck  time.Time
	cachedUpdateInfo *updateInfo
)

// handleSystemUpdate 检查 GitHub Releases 最新版本与更新包信息
func (s *Server) handleSystemUpdate(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force") == "1"

	updateCacheMu.Lock()
	if !force && cachedUpdateInfo != nil && time.Since(lastUpdateCheck) < 30*time.Minute {
		res := *cachedUpdateInfo
		updateCacheMu.Unlock()
		writeJSON(w, 200, res)
		return
	}
	updateCacheMu.Unlock()

	info, err := fetchLatestReleaseInfo()
	if err != nil {
		writeJSON(w, 200, map[string]any{
			"ok":             false,
			"currentVersion": appVersion,
			"latestVersion":  appVersion,
			"hasUpdate":      false,
			"error":          "检测更新失败: " + err.Error(),
			"checkedAt":      time.Now().Format("2006-01-02 15:04:05"),
		})
		return
	}

	updateCacheMu.Lock()
	cachedUpdateInfo = info
	lastUpdateCheck = time.Now()
	updateCacheMu.Unlock()

	writeJSON(w, 200, info)
}

type ghRelease struct {
	TagName     string `json:"tag_name"`
	Name        string `json:"name"`
	Body        string `json:"body"`
	HTMLURL     string `json:"html_url"`
	PublishedAt string `json:"published_at"`
	Assets      []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
		Size               int64  `json:"size"`
	} `json:"assets"`
}

func fetchLatestReleaseInfo() (*updateInfo, error) {
	req, err := http.NewRequest("GET", "https://api.github.com/repos/AmigaMeow/micam/releases/latest", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "MiCam-NAS/" + appVersion)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API 返回 HTTP %d", resp.StatusCode)
	}

	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}

	latestVer := strings.TrimPrefix(rel.TagName, "v")
	curVer := appVersion

	downloadURL := ""
	for _, a := range rel.Assets {
		if strings.HasSuffix(a.Name, ".fpk") {
			downloadURL = a.BrowserDownloadURL
			break
		}
	}
	if downloadURL == "" && rel.HTMLURL != "" {
		downloadURL = rel.HTMLURL
	}

	hasUpdate := isVersionNewer(latestVer, curVer)

	return &updateInfo{
		Ok:             true,
		CurrentVersion: curVer,
		LatestVersion:  latestVer,
		HasUpdate:      hasUpdate,
		ReleaseName:    rel.Name,
		ReleaseURL:     rel.HTMLURL,
		DownloadURL:    downloadURL,
		PublishedAt:    rel.PublishedAt,
		Changelog:      rel.Body,
		CheckedAt:      time.Now().Format("2006-01-02 15:04:05"),
	}, nil
}

func isVersionNewer(latest, cur string) bool {
	lParts := strings.Split(strings.TrimPrefix(latest, "v"), ".")
	cParts := strings.Split(strings.TrimPrefix(cur, "v"), ".")
	for i := 0; i < len(lParts) && i < len(cParts); i++ {
		var lNum, cNum int
		fmt.Sscanf(lParts[i], "%d", &lNum)
		fmt.Sscanf(cParts[i], "%d", &cNum)
		if lNum > cNum {
			return true
		}
		if lNum < cNum {
			return false
		}
	}
	return len(lParts) > len(cParts)
}
