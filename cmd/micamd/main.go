package main

// MiCam 飞牛插件后端服务。
//
// 运行模式：
//   - 网关模式（生产）：监听 Unix Socket，由飞牛统一网关反代
//   - 独立模式（调试）：监听 TCP 端口，方便本机调试
//
// 所有飞牛开放 API 调用都通过 trimapi 包（Unix Socket），token 不落地。

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/micam/micam/internal/config"
	"github.com/micam/micam/internal/record"
	"github.com/micam/micam/internal/store"
	"github.com/micam/micam/internal/trimapi"
	"github.com/micam/micam/internal/xiaomi"
)

const appName = "micam"

func main() {
	var (
		socketPath = flag.String("socket", "", "Unix Socket 路径（网关模式）")
		httpAddr   = flag.String("http", "", "TCP 地址（独立调试模式），如 :8899")
		configPath = flag.String("config", "", "配置文件路径")
		dataDir    = flag.String("data", "", "数据目录（存 token/配置）")
		devMode    = flag.Bool("dev", false, "开发模式：额外开放调试接口")
		rtspPort   = flag.Int("rtsp", 18554, "内置 RTSP 中转端口（供 ffmpeg 使用）")
		ffmpegPath = flag.String("ffmpeg", "ffmpeg", "ffmpeg 可执行文件路径")
		daemonMode = flag.Bool("daemon", false, "后台运行（自己 daemon 化）")
	)
	flag.Parse()

	// daemon 化：重新 exec 自己并脱离父进程
	//
	// 为什么需要：shell 的 nohup/setsid 在 systemd 或 sudo 环境下
	// 不可靠（父会话退出时子进程仍会被杀）。
	// 自己 fork + setsid 最可靠。
	if *daemonMode && os.Getenv("MICAM_DAEMONIZED") == "" {
		daemonize()
		return
	}

	// 数据目录：优先命令行，其次 TRIM_PKGVAR，最后当前目录
	if *dataDir == "" {
		if v := os.Getenv("TRIM_PKGVAR"); v != "" {
			*dataDir = v
		} else {
			*dataDir = "."
		}
	}
	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		log.Fatalf("创建数据目录失败: %v", err)
	}

	// 默认配置路径
	if *configPath == "" {
		if v := os.Getenv("TRIM_PKGETC"); v != "" {
			*configPath = filepath.Join(v, "config.json")
		}
	}

	cfg := config.Load(*configPath)
	st := store.New(*dataDir)

	// 首次启动时把前端从应用目录部署到数据目录
	deployWeb(*dataDir)

	srv := &Server{cfg: cfg, store: st, dataDir: *dataDir, dev: *devMode, rtspPort: *rtspPort, ffm: *ffmpegPath}

	// 初始化录像模块
	recDir := cfg.RecordDir
	if recDir == "" {
		recDir = filepath.Join(*dataDir, "recordings")
	}
	srv.rec = record.NewManager(record.Config{
		Enabled:    cfg.RecordEnabled,
		Dir:        recDir,
		Segment:    time.Duration(cfg.RecordSegment) * time.Second,
		KeepDays:   7,
		MinFreeGB:  10,
		FFmpegPath: *ffmpegPath,
	})
	log.Printf("录像目录: %s (启用=%v)", recDir, cfg.RecordEnabled)

	_ = rtspPort // 保留参数，当前用管道方案，不需要 RTSP 中转

	mux := http.NewServeMux()
	srv.routes(mux)

	// 网关会带 /app/micam 前缀转发过来，需要剥掉
	handler := stripPrefixMiddleware(mux, "/app/micam")

	httpSrv := &http.Server{
		Handler:           logMiddleware(handler),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// 监听：Unix socket（飞牛网关）+ TCP（局域网/调试）
	//
	// 两个都要：
	//   - socket 给飞牛界面用（网关反代）
	//   - tcp 给局域网客户端用（Mac App、调试）
	var listeners []net.Listener

	if *socketPath != "" {
		_ = os.Remove(*socketPath)
		ln, err := net.Listen("unix", *socketPath)
		if err != nil {
			log.Fatalf("监听 unix socket 失败: %v", err)
		}
		_ = os.Chmod(*socketPath, 0o660)
		listeners = append(listeners, ln)
		log.Printf("监听 Unix socket: %s", *socketPath)
	}

	if *httpAddr != "" {
		ln, err := net.Listen("tcp", *httpAddr)
		if err != nil {
			log.Fatalf("监听 TCP 失败: %v", err)
		}
		listeners = append(listeners, ln)
		log.Printf("监听 TCP: %s", *httpAddr)
	}

	if len(listeners) == 0 {
		log.Fatal("必须指定 -socket 或 -http（或两者）")
	}

	// 多个 listener 同时服务
	for i := 1; i < len(listeners); i++ {
		go func(l net.Listener) {
			if err := httpSrv.Serve(l); err != nil && err != http.ErrServerClosed {
				log.Printf("listener 退出: %v", err)
			}
		}(listeners[i])
	}
	ln := listeners[0]

	// 优雅关闭
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Println("收到退出信号，正在关闭...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	}()

	if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Fatalf("服务异常退出: %v", err)
	}
	log.Println("已退出")
}

// ---- Server ----

type Server struct {
	cfg     *config.Config
	store   *store.Store
	dataDir string
	dev     bool

	rtspPort int
	ffm      string
	rec      *record.Manager
}

// ffmpegPath 返回 ffmpeg 路径。
func (s *Server) ffmpegPath() string {
	if s.ffm != "" {
		return s.ffm
	}
	return "ffmpeg"
}

// daemonize 重新 exec 自己并脱离父进程。
func daemonize() {
	exe, err := os.Executable()
	if err != nil {
		log.Fatalf("获取自身路径失败: %v", err)
	}

	args := os.Args[1:]
	// 去掉 --daemon，避免无限递归
	var filtered []string
	for _, a := range args {
		if a != "--daemon" && a != "-daemon" {
			filtered = append(filtered, a)
		}
	}

	cmd := exec.Command(exe, filtered...)
	cmd.Env = append(os.Environ(), "MICAM_DAEMONIZED=1")
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		log.Fatalf("daemon 化失败: %v", err)
	}
	// 父进程退出，子进程继续
	os.Exit(0)
}

// deployWeb 把前端页面从应用目录复制到数据目录（首次启动时）。
//
// 为什么需要：静态文件在 TRIM_APPDEST/www，
// 但服务以 package 用户运行时读应用目录不方便，
// 复制到 TRIM_PKGVAR/web 后统一从数据目录提供。
func deployWeb(dataDir string) {
	src := filepath.Join(os.Getenv("TRIM_APPDEST"), "www", "index.html")
	if src == "/www/index.html" {
		return
	}
	dstDir := filepath.Join(dataDir, "web")
	dst := filepath.Join(dstDir, "index.html")

	// 源不存在则跳过
	info, err := os.Stat(src)
	if err != nil {
		return
	}
	// 目标已存在且不比源旧则跳过
	if di, err := os.Stat(dst); err == nil && !di.ModTime().Before(info.ModTime()) {
		return
	}
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		log.Printf("创建前端目录失败: %v", err)
		return
	}
	data, err := os.ReadFile(src)
	if err != nil {
		log.Printf("读取前端失败: %v", err)
		return
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		log.Printf("写入前端失败: %v", err)
		return
	}
	log.Printf("已部署前端页面: %s", dst)
}

// srcOverride 取注入的源地址（调试用）。
func (s *Server) srcOverride(did string) (string, bool) {
	return s.store.Source(did)
}

func (s *Server) routes(mux *http.ServeMux) {
	// 健康检查
	mux.HandleFunc("/api/health", s.handleHealth)

	// 平台信息
	mux.HandleFunc("/api/platform", s.handlePlatform)

	// 小米账号
	mux.HandleFunc("/api/accounts", s.handleAccounts)          // GET 列表 / POST 添加
	mux.HandleFunc("/api/accounts/qr", s.handleQRStart)        // POST 发起扫码
	mux.HandleFunc("/api/accounts/qr/status", s.handleQRStatus) // GET 轮询扫码状态

	// 设备
	mux.HandleFunc("/api/devices", s.handleDevices)

	// 播放
	mux.HandleFunc("/api/live/mjpeg", s.handleMJPEG)   // MJPEG 流（<img> 直接显示）
	mux.HandleFunc("/api/live/snapshot", s.handleSnap)
	mux.HandleFunc("/api/live/inject", s.handleInjectSource) // 单帧 JPEG

	// 云台
	mux.HandleFunc("/api/ptz/status", s.handlePTZStatus)
	mux.HandleFunc("/api/ptz/move", s.handlePTZMove)

	// 录像
	mux.HandleFunc("/api/recordings", s.handleRecordings)       // GET 列表 / POST 开始
	mux.HandleFunc("/api/recordings/stop", s.handleRecordStop)  // POST 停止
	mux.HandleFunc("/api/recordings/files", s.handleRecordFiles) // GET 文件列表
	mux.HandleFunc("/api/recordings/cleanup", s.handleRecordCleanup) // POST 清理

	// 静态前端（同目录下的 web/）
	mux.Handle("/", s.staticHandler())
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"ok":      true,
		"app":     appName,
		"version": "0.3.7",
	})
}

func (s *Server) handlePlatform(w http.ResponseWriter, r *http.Request) {
	// 通过飞牛开放 API 读系统语言/版本
	res, err := trimapi.Call(appName, "trim.system.getPlatformConfig", map[string]any{})
	if err != nil {
		// 独立模式下可能没有飞牛环境，降级返回默认值
		writeJSON(w, 200, map[string]any{
			"systemLanguage": "zh-CN",
			"systemVersion":  "unknown",
			"fallback":       true,
			"error":          err.Error(),
		})
		return
	}
	writeJSON(w, 200, res)
}

// handleAccounts：GET 列出，POST 添加（账号密码）
func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, s.store.Accounts())
	case http.MethodPost:
		var req struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Verify   string `json:"verify"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, "参数错误")
			return
		}
		writeJSON(w, 501, map[string]any{
			"msg": "账号密码登录请走 /api/accounts/login（未实现），推荐用扫码",
		})
	default:
		writeErr(w, 405, "方法不允许")
	}
}

// handleQRStart：发起扫码登录
func (s *Server) handleQRStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "方法不允许")
		return
	}
	sess := xiaomi.NewSession()
	qr, err := sess.StartQRLogin()
	if err != nil {
		writeErr(w, 500, "发起扫码失败: "+err.Error())
		return
	}
	img, err := qr.FetchQRImage()
	if err != nil {
		writeErr(w, 500, "获取二维码失败: "+err.Error())
		return
	}
	id := s.store.SaveQR(sess, qr)
	_ = s.store.SaveQRImage(id, img)

	writeJSON(w, 200, map[string]any{
		"id":        id,
		"qrBase64":  "data:image/png;base64," + base64Encode(img),
		"timeout":   qr.Timeout,
		"loginUrl":  qr.LoginURL,
	})
}

// handleQRStatus：轮询扫码状态
func (s *Server) handleQRStatus(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeErr(w, 400, "缺少 id")
		return
	}
	st, ok := s.store.GetQR(id)
	if !ok {
		writeErr(w, 404, "会话不存在或已过期")
		return
	}
	switch st.State {
	case store.QRWaiting:
		writeJSON(w, 200, map[string]any{"state": "waiting"})
	case store.QRScanned:
		writeJSON(w, 200, map[string]any{"state": "scanned"})
	case store.QRSuccess:
		writeJSON(w, 200, map[string]any{
			"state":  "success",
			"userId": st.UserID,
		})
	case store.QRFailed:
		writeJSON(w, 200, map[string]any{"state": "failed", "error": st.Error})
	}
}

// handleDevices：聚合所有账号的摄像头（含共享家庭）
func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	sessions := s.store.AllSessions()
	if len(sessions) == 0 {
		writeJSON(w, 200, map[string]any{"devices": []any{}, "needLogin": true})
		return
	}

	all := []xiaomi.Device{}
	seen := map[string]bool{}
	var lastErr error

	for _, sess := range sessions {
		devs, err := sess.AllCameras()
		if err != nil {
			lastErr = err
			continue
		}
		for _, d := range devs {
			if seen[d.Did] {
				continue
			}
			seen[d.Did] = true
			all = append(all, d)
		}
	}

	// 全部失败才算错误
	if len(all) == 0 && lastErr != nil {
		writeErr(w, 500, "获取设备失败: "+lastErr.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"devices": all, "needLogin": false})
}

func (s *Server) staticHandler() http.Handler {
	dir := filepath.Join(s.dataDir, "web")
	if _, err := os.Stat(dir); err != nil {
		dir = "web"
	}
	return http.FileServer(http.Dir(dir))
}

// ---- 工具 ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"msg": msg})
}

// stripPrefixMiddleware 剥掉网关前缀。
//
// 飞牛网关按 gatewayPrefix 转发时会保留前缀，
// 例如请求 /app/micam/api/health 实际是 /api/health。
func stripPrefixMiddleware(next http.Handler, prefix string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if prefix != "" && strings.HasPrefix(r.URL.Path, prefix) {
			p := strings.TrimPrefix(r.URL.Path, prefix)
			if p == "" {
				p = "/"
			}
			r2 := r.Clone(r.Context())
			r2.URL.Path = p
			next.ServeHTTP(w, r2)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("PANIC %s %s: %v", r.Method, r.URL.Path, rec)
				// 打印堆栈便于定位
				buf := make([]byte, 8192)
				n := runtime.Stack(buf, false)
				log.Printf("堆栈: %s", string(buf[:n]))
			}
		}()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %v", r.Method, r.URL.Path, time.Since(start))
	})
}

// recoverPanic 兜底：任何 goroutine 的 panic 都不应该杀掉整个进程。
func recoverPanic(name string) {
	if rec := recover(); rec != nil {
		buf := make([]byte, 8192)
		n := runtime.Stack(buf, false)
		log.Printf("PANIC in %s: %v | %s", name, rec, string(buf[:n]))
	}
}

func base64Encode(b []byte) string {
	const table = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var out []byte
	for i := 0; i < len(b); i += 3 {
		var n, pad int
		n = int(b[i]) << 16
		if i+1 < len(b) {
			n |= int(b[i+1]) << 8
		} else {
			pad++
		}
		if i+2 < len(b) {
			n |= int(b[i+2])
		} else {
			pad++
		}
		out = append(out, table[(n>>18)&63], table[(n>>12)&63])
		if pad < 2 {
			out = append(out, table[(n>>6)&63])
		} else {
			out = append(out, '=')
		}
		if pad < 1 {
			out = append(out, table[n&63])
		} else {
			out = append(out, '=')
		}
	}
	return string(out)
}

var _ = fmt.Sprintf
