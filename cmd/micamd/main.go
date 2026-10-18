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
	"github.com/micam/micam/internal/miot"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/micam/micam/internal/config"
	"github.com/micam/micam/internal/event"
	"github.com/micam/micam/internal/notify"
	"github.com/micam/micam/internal/record"
	"github.com/micam/micam/internal/store"
	"github.com/micam/micam/internal/stream"
	"github.com/micam/micam/internal/trimapi"
	"github.com/micam/micam/internal/xiaomi"
)

const appName = "micam"

// appVersion 用 var 而非 const：
// 允许构建时用 -ldflags "-X main.appVersion=x.y.z" 注入，
// 确保二进制里的版本与 manifest 严格一致（构建脚本会校验）。
// dataDirPath 数据目录（包级，供其它文件使用）
var dataDirPath string

// appDestDir 应用安装目录（包级，供其它文件使用）
var appDestDir string

// startedAt 进程启动时间（状态条显示「运行时间」用）
var startedAt = time.Now()

var appVersion = "0.41.31"

func main() {
	checkMiotLib()

	// 启动时清理上一代遗留的 sidecar（2026-10-13）
	//
	// 教训：micamd 被 kill 后，它起的 miot_mjpeg.py 会变成孤儿进程，
	// 继续占着摄像头唯一的 PPCS 连接槽 —— 用户会看到
	// 「超过最大同时连接数量」，连米家 App 都被挤掉。
	// 实测残留了 18 小时都没释放。
	reapOrphanSidecars()

	var (
		socketPath = flag.String("socket", "", "Unix Socket 路径（网关模式）")
		httpAddr   = flag.String("http", "", "TCP 地址（独立调试模式），如 :8899")
		configPath = flag.String("config", "", "配置文件路径")
		dataDir    = flag.String("data", "", "数据目录（存 token/配置）")
		devMode    = flag.Bool("dev", false, "开发模式：额外开放调试接口")
		rtspPort   = flag.Int("rtsp", 18554, "内置 RTSP 中转端口（供 ffmpeg 使用）")
		ffmpegPath = flag.String("ffmpeg", "ffmpeg", "ffmpeg 可执行文件路径")
		appDest    = flag.String("appdest", os.Getenv("TRIM_APPDEST"), "应用安装目录（找 AI 脚本用）")
		daemonMode = flag.Bool("daemon", false, "后台运行（自己 daemon 化）")
	)
	showVersion := flag.Bool("version", false, "打印版本后退出")
	flag.Parse()

	appDestDir = *appDest
	dataDirPath = *dataDir

	// --version：打印版本后退出
	//
	// 用途：构建脚本用 `micamd --version` 校验二进制版本，
	// 比在二进制里 grep 版本号可靠（避免匹配到依赖的版本）。
	if *showVersion {
		fmt.Printf("%s %s\n", appName, appVersion)
		return
	}

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

	// 结果缓存开关（AI 配置里可关）
	globalCacheEnabled = cfg.AI.CacheEnabled
	st := store.New(*dataDir)

	// 初始化日志文件（daemon 化后 stdout 会丢）
	initLogFile(*dataDir)

	// 清理上次残留的 HLS 缓存
	srvCleanHLS(*dataDir)

	// 首次启动时把前端从应用目录部署到数据目录
	deployWeb(*dataDir)

	srv := &Server{cfg: cfg, store: st, dataDir: *dataDir, dev: *devMode, rtspPort: *rtspPort, ffm: *ffmpegPath}

	// 开发者页的两个内存态存储（Token 落盘、请求日志只在内存）
	srv.tokens = newTokenStore(*dataDir)
	srv.reqlog = newReqLogStore(*dataDir)
	initPersonStore(*dataDir)
	initWhitelistStore(*dataDir)

	// 初始化录像模块
	recDir := cfg.RecordDir
	if recDir == "" {
		recDir = filepath.Join(*dataDir, "recordings")
	}
	srv.rec = record.NewManager(record.Config{
		Enabled:    cfg.RecordEnabled,
		Dir:        recDir,
		Segment:    time.Duration(cfg.RecordSegment) * time.Second,
		KeepDays:   cfg.RecordKeepDays,
		MinFreeGB:  cfg.RecordMinFreeGB,
		FFmpegPath: srv.ffmpegPath(),
	})
	log.Printf("录像目录: %s (启用=%v)", recDir, cfg.RecordEnabled)

	// 事件通知
	srv.notify = notify.New(notify.Config{
		Enabled:   cfg.NotifyEnabled,
		Channel:   cfg.NotifyChannel,
		Webhook:   cfg.NotifyWebhook,
		Params:    cfg.NotifyParams,
		Events:    cfg.NotifyEvents,
		QuietFrom: cfg.UI.QuietFrom,
		QuietTo:   cfg.UI.QuietTo,
	})

	// 清理过期的片段导出文件
	srv.cleanExportsOnStart()

	// 存储守护：定时检查磁盘，超阈值自动清理旧录像
	srv.startStorageGuard()
	srv.initMotionRec()

	// 凭据守护：提前续期，避免过期后被动刷新撞限流
	srv.startCredentialKeeper()

	// 账号会话守护：提前续期 serviceToken（用户无感）
	srv.startAccountKeeper()

	// 缩略图预热 + 定时刷新（设备列表页秒开）
	srv.startThumbWarmer()

	// 事件库 + 设备别名
	srv.eventStore = event.NewStore(filepath.Join(*dataDir, "events"), 90)
	srv.meta = newMetaStore(*dataDir)
	// 载入限流静默状态（避免重启后立刻又去撞限流）
	loadThrottleState()
	loadAPIBase()            // 载入已选的 API 域名
	loadDeviceID()           // 载入（或首次生成）持久化设备 ID
	srv.loadAccessCode()     // 敏感接口访问码（无则生成）
	loadMiotAuth()           // 官方 OAuth2 通道授权
	srv.startMiotRefresher() // 令牌自动续期守护
	srv.presets = newPresetStore(*dataDir)
	srv.marks = newMarkStore(*dataDir)
	srv.aiw = newAIWorker(srv)
	srv.finds = newFindStore()
	srv.poses = newPoseStore(*dataDir)

	// 云端已配置就启动自动分析工作器
	if cfg.AI.CloudEnabled && cfg.AI.Mode != "local" {
		srv.aiw.Start()
	}

	// AI 推理客户端（默认关闭；开启且环境可用时才启动子进程）
	aiScript := findAIScript("infer.py")
	if aiScript == "" {
		aiScript = filepath.Join(*appDest, "ai", "infer.py")
	}
	srv.ai = newAIClient(*dataDir, aiScript)
	loadCostState(srv.ai) // 恢复今日费用记账（否则重启就绕过预算上限）
	if cfg.AI.Enabled {
		if err := srv.ai.Start(aiConfigFromUser(cfg.AI)); err != nil {
			logf("[ai] 启动失败（继续用帧差模式）: %v", err)
		}
	}

	// 移动侦测（复用缩略图缓存，几乎零成本）
	srv.startMotionDetector()
	srv.startRecordingAIScanner()

	// 设备在线状态守护：离线/上线发通知
	srv.startDeviceWatcher()
	srv.startDailyDigestScheduler()

	// 自动录像：只有配置里明确打开了才恢复
	//
	// 用户明确要求「同意才开始录制」，所以默认是关的。
	if cfg.RecordEnabled && (cfg.RecordMode == "" || cfg.RecordMode == "always") {
		go func() {
			time.Sleep(20 * time.Second) // 等设备列表就绪
			srv.startAllRecordings()
		}()
	}

	_ = rtspPort // 保留参数，当前用管道方案，不需要 RTSP 中转

	mux := http.NewServeMux()
	srv.routes(mux)
	srv.startRecCacheWorker()

	// 网关会带 /app/micam 前缀转发过来，需要剥掉
	handler := stripPrefixMiddleware(mux, "/app/micam")

	// CORS（开放模式开关控制）
	handler = srv.corsMiddleware(handler)

	// API Token 鉴权（**只对带 Authorization/?token= 的请求生效**，
	// 不带 token 的现有 UI / 局域网直连行为完全不变）
	handler = srv.devAuthMiddleware(handler)

	// 请求日志 + 调用统计（在鉴权外层，401/403 也会被记下来）
	handler = srv.devLogMiddleware(handler)

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
		if err := os.Chmod(*socketPath, 0o660); err != nil {
			logf("[main] 设置 unix socket 权限失败 %s: %v", *socketPath, err)
		}
		listeners = append(listeners, ln)
		log.Printf("监听 Unix socket: %s", *socketPath)

		// socket 自愈守护
		//
		// 为什么需要：飞牛应用中心安装/升级插件时会替换应用目录，
		// socket 文件会被删除，但进程仍在运行（日志显示 already running）。
		// 网关连不上 socket -> Bad Gateway。
		//
		// 这里定期检查 socket 文件是否存在，丢失就重建。
		go watchSocket(*socketPath, httpSrv)
	}

	if *httpAddr != "" {
		ln, err := net.Listen("tcp", *httpAddr)
		if err != nil {
			if len(listeners) > 0 {
				log.Printf("⚠️ 监听 TCP %s 失败 (%v)，降级为仅监听 Unix socket", *httpAddr, err)
			} else {
				log.Fatalf("监听 TCP 失败: %v", err)
			}
		} else {
			listeners = append(listeners, ln)
			log.Printf("监听 TCP: %s", *httpAddr)
		}
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

	// 开发者页（v0.40）：API Token 与请求日志/统计
	tokens *tokenStore
	reqlog *reqLogStore

	rtspPort int
	ffm      string
	rec      *record.Manager
	notify   *notify.Manager

	eventStore *event.Store
	meta       *metaStore
	presets    *presetStore
	marks      *markStore
	aiw        *aiWorker
	finds      *findStore
	poses      *poseStore
	ai         *aiClient
}

// ffmpegPath 返回 ffmpeg 路径。
func (s *Server) ffmpegPath() string {
	if s.ffm != "" {
		if _, err := os.Stat(s.ffm); err == nil {
			return s.ffm
		}
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	for _, cand := range []string{"/usr/bin/ffmpeg", "/usr/local/bin/ffmpeg", "/bin/ffmpeg"} {
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
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

	// 关键：把 stdout/stderr 重定向到日志文件
	// 否则 daemon 化后所有日志都进 /dev/null
	logPath := os.Getenv("TRIM_PKGVAR")
	if logPath == "" {
		logPath = "."
	}
	logPath = filepath.Join(logPath, "micamd.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		logFile = nil
	}

	cmd := exec.Command(exe, filtered...)
	cmd.Env = append(os.Environ(), "MICAM_DAEMONIZED=1")
	cmd.Stdin = nil
	if logFile != nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		log.Fatalf("daemon 化失败: %v", err)
	}
	// 父进程退出，子进程继续
	os.Exit(0)
}

// srvCleanHLS 清理 HLS 缓存目录。
func srvCleanHLS(dataDir string) {
	dir := filepath.Join(dataDir, "hls")
	if _, err := os.Stat(dir); err == nil {
		_ = os.RemoveAll(dir)
		log.Printf("已清理 HLS 缓存: %s", dir)
	}
}

// deployWeb 把前端页面从应用目录复制到数据目录（首次启动时）。
//
// 为什么需要：静态文件在 TRIM_APPDEST/www，
// 但服务以 package 用户运行时读应用目录不方便，
// 复制到 TRIM_PKGVAR/web 后统一从数据目录提供。
func deployWeb(dataDir string) {
	var candDirs []string
	if dest := os.Getenv("TRIM_APPDEST"); dest != "" {
		candDirs = append(candDirs, filepath.Join(dest, "www"))
	}
	if appDestDir != "" {
		candDirs = append(candDirs, filepath.Join(appDestDir, "www"))
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candDirs = append(candDirs,
			filepath.Join(dir, "www"),
			filepath.Join(dir, "..", "www"),
			filepath.Join(filepath.Dir(dir), "www"),
		)
	}
	if matches, err := filepath.Glob("/vol*/@appcenter/micam/www"); err == nil {
		candDirs = append(candDirs, matches...)
	}
	candDirs = append(candDirs, filepath.Join("platform", "fnos", "app", "www"), "www")

	var src string
	for _, cd := range candDirs {
		p := filepath.Join(cd, "index.html")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			src = p
			break
		}
	}
	if src == "" {
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
		// index.html 没变，但仍要补齐其他静态文件（如 hls.min.js）
		copyWebAssets(filepath.Dir(src), dstDir)
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

	// 安全校验：拒绝部署结构损坏的 HTML
	//
	// 曾经因为一次字符串替换把整段 JS 插到了文件开头，
	// 结果页面把源码当文本显示。这里做基本检查。
	if !looksLikeHTML(data) {
		log.Printf("前端页面结构异常，跳过部署: %s", src)
		return
	}

	if err := os.WriteFile(dst, data, 0o644); err != nil {
		log.Printf("写入前端失败: %v", err)
		return
	}
	log.Printf("已部署前端页面: %s", dst)

	// 一并部署同目录的其他静态资源（hls.min.js 等）
	//
	// 漏了这步的表现：hls.js 404 -> HLS 播放黑屏。
	copyWebAssets(filepath.Dir(src), dstDir)
}

// looksLikeHTML 基本结构检查。
func looksLikeHTML(data []byte) bool {
	if len(data) < 200 {
		return false
	}
	// 开头必须是 DOCTYPE（允许 BOM/空白）
	head := strings.TrimLeft(string(data[:min(200, len(data))]), " \t\r\n\ufeff")
	if !strings.HasPrefix(strings.ToUpper(head), "<!DOCTYPE") {
		return false
	}
	s := string(data)
	// script 标签必须配对
	if strings.Count(s, "<script") != strings.Count(s, "</script>") {
		return false
	}
	// 必须有 body
	return strings.Contains(s, "<body")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// srcOverride 取注入的源地址（调试用）。
func (s *Server) srcOverride(did string) (string, bool) {
	return s.store.Source(did)
}

func (s *Server) routes(mux *http.ServeMux) {
	// 健康检查
	mux.HandleFunc("/api/health", s.handleHealth)

	// 运行时与闭源组件自愈
	mux.HandleFunc("/api/system/runtime", s.handleSystemRuntime)
	mux.HandleFunc("/api/system/runtime/fetch-miot-lib", s.handleMiotLibFetch)
	mux.HandleFunc("/api/system/runtime/upload-miot-lib", s.handleMiotLibUpload)
	mux.HandleFunc("/api/system/update", s.handleSystemUpdate)

	// 平台信息
	mux.HandleFunc("/api/platform", s.handlePlatform)

	// 小米账号
	mux.HandleFunc("/api/accounts", s.handleAccounts)             // GET 列表 / POST 添加
	mux.HandleFunc("/api/accounts/qr", s.handleQRStart)           // POST 发起扫码
	mux.HandleFunc("/api/accounts/qr/status", s.handleQRStatus)   // GET 轮询扫码状态
	mux.HandleFunc("/api/accounts/login", s.handleAccountLogin)   // POST 账号密码登录
	mux.HandleFunc("/api/accounts/verify", s.handleAccountVerify) // POST 提交二次验证码

	// 凭据备份 / 恢复 / 状态
	mux.HandleFunc("/api/accounts/export", s.handleCredExport)
	mux.HandleFunc("/api/accounts/import", s.handleCredImport)
	mux.HandleFunc("/api/accounts/remove", s.handleAccountRemove)
	mux.HandleFunc("/api/accounts/backups", s.handleCredBackup)
	mux.HandleFunc("/api/accounts/status", s.handleAccountStatus)
	// ===== MiOT 官方通道（OAuth2 + PPCS，绕开 miss_get_vendor 限流）=====
	mux.HandleFunc("/api/miot/bind", s.handleMiotBind)
	mux.HandleFunc("/api/miot/authorize", s.handleMiotAuthorize)
	mux.HandleFunc("/api/miot/auto-auth", s.handleMiotAutoAuth)
	mux.HandleFunc("/api/miot/status", s.handleMiotStatus)
	mux.HandleFunc("/api/miot/unbind", s.handleMiotUnbind)
	mux.HandleFunc("/api/miot/refresh", s.handleMiotRefresh)
	mux.HandleFunc("/api/miot/live", s.handleMiotLive)
	mux.HandleFunc("/api/miot/live/stats", s.handleMiotLiveStats)
	mux.HandleFunc("/api/miot/cameras", s.handleMiotCameras)
	mux.HandleFunc("/api/miot/qr", s.handleMiotQRStart)
	mux.HandleFunc("/api/miot/qr/status", s.handleMiotQRStatus)
	mux.HandleFunc("/api/miot/qr/cancel", s.handleMiotQRCancel)
	mux.HandleFunc("/api/accounts/detail", s.handleAccountsDetail)
	mux.HandleFunc("/api/system/backup", s.handleSystemBackup)

	// 设备
	mux.HandleFunc("/api/devices", s.handleDevices)
	mux.HandleFunc("/api/devices/all", s.handleAllDevices)
	mux.HandleFunc("/api/devices/cloud", s.handleDevicesCloud)  // 云端清单（含 managed 标记）
	mux.HandleFunc("/api/devices/manage", s.handleDeviceManage) // 纳入 / 移出管理
	mux.HandleFunc("/api/devices/rtsp", s.handleDevicesRTSP)   // 通用 RTSP 摄像机流接入
	mux.HandleFunc("/api/homes", s.handleHomes)

	// 首页聚合 / 事件 / 设备别名
	mux.HandleFunc("/api/home", s.handleHome)
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/api/events/thumb", s.handleEventThumb)
	mux.HandleFunc("/api/events/marks", s.handleEventMarks)
	mux.HandleFunc("/api/events/bulk", s.handleEventBulk)
	mux.HandleFunc("/api/events/clip", s.handleEventClip)
	mux.HandleFunc("/api/events/heatmap", s.handleEventsHeatmap)
	mux.HandleFunc("/api/people", s.handlePeople)
	mux.HandleFunc("/api/people/link", s.handlePeopleLink)
	mux.HandleFunc("/api/people/events", s.handlePeopleEvents)
	mux.HandleFunc("/manifest.json", s.handleManifest)
	mux.HandleFunc("/api/devices/meta", s.handleDevMeta)
	mux.HandleFunc("/api/devices/hide", s.handleDevHide)

	// AI 推理
	mux.HandleFunc("/api/ai/config", s.handleAIConfig)
	mux.HandleFunc("/api/ui/config", s.handleUIConfig)
	mux.HandleFunc("/api/ai/status", s.handleAIStatus)
	mux.HandleFunc("/api/ai/model", s.handleAIModel)
	mux.HandleFunc("/api/ai/test", s.handleAITest)
	mux.HandleFunc("/api/ai/search", s.handleAISearch)
	mux.HandleFunc("/api/ai/summarize", s.handleAISummarizeEvent)
	mux.HandleFunc("/api/ai/ask", s.handleAISearch)
	mux.HandleFunc("/api/ai/scan", s.handleAIScan)
	mux.HandleFunc("/api/ai/overview", s.handleAIOverview)
	mux.HandleFunc("/api/ai/usage", s.handleAIUsage)
	mux.HandleFunc("/api/ai/embed", s.handleAIEmbed)
	mux.HandleFunc("/api/ai/provider/test", s.handleAIProviderTest)
	mux.HandleFunc("/api/ai/analyze", s.handleAIAnalyze)
	mux.HandleFunc("/api/ai/find/estimate", s.handleFindEstimate)
	mux.HandleFunc("/api/ai/find/run", s.handleFindRun)
	mux.HandleFunc("/api/ai/find/task", s.handleFindTask)
	mux.HandleFunc("/api/ai/find/tasks", s.handleFindTasks)
	mux.HandleFunc("/api/ai/plate/read", s.handlePlateRead)
	mux.HandleFunc("/api/ai/plate/search", s.handlePlateSearch)
	mux.HandleFunc("/api/ai/plate/status", s.handlePlateStatus)
	mux.HandleFunc("/api/ai/plate/install", s.handlePlateInstall)
	mux.HandleFunc("/api/camera/pose", s.handlePoseList)
	mux.HandleFunc("/api/camera/pose/capture", s.handlePoseCapture)
	mux.HandleFunc("/api/ai/jobs", s.handleAIJobs)
	mux.HandleFunc("/api/ai/digest", s.handleAIDigest)
	mux.HandleFunc("/api/ai/digest/trigger", s.handleAIDigestTrigger)
	mux.HandleFunc("/api/ai/whitelist", s.handleAIWhitelist)
	mux.HandleFunc("/api/ai/whitelist/delete", s.handleAIWhitelistDelete)

	// 播放
	mux.HandleFunc("/api/live/source-status", s.handleSourceStatus)
	mux.HandleFunc("/api/live/cred-status", s.handleCredStatus)
	mux.HandleFunc("/api/cred/apibase", s.handleAPIBase)
	mux.HandleFunc("/api/dev/debug-vendor", s.handleDebugVendor)
	mux.HandleFunc("/api/dev/vendor-probe", s.handleVendorProbe)
	mux.HandleFunc("/api/dev/access-code", s.handleAccessCodeStatus)
	mux.HandleFunc("/api/live/renew", s.handleCredRenew)
	mux.HandleFunc("/api/live/hls", s.handleHLS)          // HLS 播放（2K 流畅）
	mux.HandleFunc("/api/live/hls/info", s.handleHLSInfo) // HLS 能力信息
	mux.HandleFunc("/api/live/mjpeg", s.handleMJPEG)      // MJPEG 流（<img> 直接显示）
	mux.HandleFunc("/api/live/snapshot", s.handleSnap)
	mux.HandleFunc("/api/live/thumb/meta", s.handleThumbMeta)
	mux.HandleFunc("/api/live/thumb", s.handleLiveThumb)
	mux.HandleFunc("/api/live/stats", s.handleLiveStats)
	mux.HandleFunc("/api/live/inject", s.handleInjectSource) // 单帧 JPEG

	// 云台
	// 事件通知
	mux.HandleFunc("/api/storage/locations", s.handleStorageLocations)
	mux.HandleFunc("/api/storage/record-dir", s.handleSetRecordDir)

	mux.HandleFunc("/api/notify/config", s.handleNotifyConfig)
	mux.HandleFunc("/api/notify/events", s.handleNotifyEvents)
	mux.HandleFunc("/api/notify/test", s.handleNotifyTest)

	// 诊断信息导出（方便提交问题）
	mux.HandleFunc("/api/diag/report", s.handleDiagReport)
	mux.HandleFunc("/api/diag/bundle", s.handleDiagBundle)

	// 开放模式（开发者选项）
	mux.HandleFunc("/api/dev/status", s.handleDevStatus)
	mux.HandleFunc("/api/dev/tap", s.handleDevTap)
	mux.HandleFunc("/api/dev/config", s.handleDevConfig)
	mux.HandleFunc("/api/dev/reset", s.handleDevReset)
	mux.HandleFunc("/api/dev/openapi", s.handleDevOpenAPI)
	mux.HandleFunc("/api/dev/docs", s.handleDevDocs)
	mux.HandleFunc("/api/dev/docs/meta", s.handleDevDocsMeta)
	mux.HandleFunc("/api/dev/capabilities", s.handleDevCapabilities)

	// 开发者页（v0.40）：Token 管理 / 请求日志 / 调用统计 / Webhook 测试 / 重置配置
	mux.HandleFunc("/api/dev/tokens", s.handleDevTokens)
	mux.HandleFunc("/api/dev/logs", s.handleDevLogs)
	mux.HandleFunc("/api/dev/stats", s.handleDevStats)
	mux.HandleFunc("/api/dev/webhook/test", s.handleDevWebhookTest)
	mux.HandleFunc("/api/dev/reset-config", s.handleDevResetConfig)

	// 安装向导 / 自检
	mux.HandleFunc("/api/setup/check", s.handleSetupCheck)
	mux.HandleFunc("/api/setup/repair", s.handleSetupRepair)

	mux.HandleFunc("/api/ptz/status", s.handlePTZStatus)
	mux.HandleFunc("/api/ptz/move", s.handlePTZMove)
	mux.HandleFunc("/api/ptz/preset", s.handlePTZPreset)
	mux.HandleFunc("/api/ptz/goto", s.handlePTZGoto)
	// ===== 云台（MIoT-Spec，2026-10-11）=====
	// 旧接口走被限流的 cs2，这些走小米云 RPC（实测可用，快 10-40 倍）
	mux.HandleFunc("/api/ptz/spec", s.handlePTZSpecStatus)
	mux.HandleFunc("/api/ptz/reset", s.handlePTZReset)
	mux.HandleFunc("/api/ptz/toggle", s.handlePTZToggle)
	mux.HandleFunc("/api/ptz/guard", s.handlePTZGuard)

	// 录像
	// 存储监控
	mux.HandleFunc("/api/storage", s.handleStorage)
	mux.HandleFunc("/api/storage/detail", s.handleStorageDetail)
	mux.HandleFunc("/api/storage/cleanup-preview", s.handleCleanupPreview)
	mux.HandleFunc("/api/storage/migrate/plan", s.handleStorageMigratePlan)
	mux.HandleFunc("/api/storage/migrate/status", s.handleStorageMigrateStatus)
	mux.HandleFunc("/api/storage/migrate/cancel", s.handleStorageMigrateCancel)
	mux.HandleFunc("/api/storage/migrate", s.handleStorageMigrateStart)

	mux.HandleFunc("/api/recordings", s.handleRecordings)            // GET 列表 / POST 开始
	mux.HandleFunc("/api/recordings/stop", s.handleRecordStop)       // POST 停止
	mux.HandleFunc("/api/recordings/files", s.handleRecordFiles)     // GET 文件列表
	mux.HandleFunc("/api/recordings/cleanup", s.handleRecordCleanup) // POST 清理

	// 录像回放
	mux.HandleFunc("/api/recordings/dates", s.handleRecDates)
	mux.HandleFunc("/api/recordings/list", s.handleRecList)
	mux.HandleFunc("/api/recordings/play", s.handleRecPlay)
	mux.HandleFunc("/api/recordings/cache-status", s.handleRecCacheStatus)
	mux.HandleFunc("/api/recordings/thumb", s.handleRecThumb)
	mux.HandleFunc("/api/recordings/frame", s.handleRecFrame)
	mux.HandleFunc("/api/recordings/frame-strip", s.handleRecFrameStrip)
	mux.HandleFunc("/api/recordings/download", s.handleRecDownload)
	mux.HandleFunc("/api/recordings/delete", s.handleRecDelete)
	mux.HandleFunc("/api/recordings/cache", s.handleCacheStats)
	mux.HandleFunc("/api/recordings/cache/clear", s.handleCacheClear)
	mux.HandleFunc("/api/recordings/config", s.handleRecConfig)
	mux.HandleFunc("/api/recordings/batch-delete", s.handleRecBatchDelete)

	// 片段导出
	mux.HandleFunc("/api/recordings/export", s.handleRecExport)
	mux.HandleFunc("/api/recordings/export/status", s.handleRecExportStatus)
	mux.HandleFunc("/api/recordings/export/download", s.handleRecExportDownload)
	mux.HandleFunc("/api/recordings/export/delete", s.handleRecExportDelete)
	mux.HandleFunc("/api/recordings/export/list", s.handleRecExportList)
	mux.HandleFunc("/api/recordings/export/clear", s.handleRecExportClear)

	// 静态前端（同目录下的 web/）
	mux.Handle("/", s.staticHandler())
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"ok":        true,
		"app":       appName,
		"version":   appVersion,
		"uptimeSec": int(time.Since(startedAt).Seconds()),
		"startedAt": startedAt.Format("2006-01-02 15:04:05"),
		// 闭源库状态（前端据此提示用户；缺失时仅视频类功能降级）
		"miotLib":     miotLibPath != "",
		"miotLibPath": miotLibPath,
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
			"msg": "账号密码登录请走 POST /api/accounts/login；扫码走 POST /api/accounts/qr",
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
	if err := s.store.SaveQRImage(id, img); err != nil {
		logf("[qr] 保存二维码图片失败 id=%s: %v", id, err)
	}

	writeJSON(w, 200, map[string]any{
		"id":       id,
		"qrBase64": "data:image/png;base64," + base64Encode(img),
		"timeout":  qr.Timeout,
		"loginUrl": qr.LoginURL,
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
// handleDevices 设备列表。
//
// ⚠️ 重要设计（2026-09-18 用户反馈后确立）
//
//	默认【只读缓存】，不拉云端！
//	只有显式带 ?refresh=1 才去小米云端拉。
//
// 原因：这个接口内部会调 AllCameras()，
//
//	那会向小米云端发真实请求（设备列表 + 家庭 + 共享设备）。
//	之前前端「打开账号页/实时预览页」都会调它，
//	等于用户什么都没点就自动获取了设备 ——
//	用户明确反馈这是错的（「我都没同意」）。
//
//	而且小米对密集调用会限流（throttle deny），
//	无意义的自动拉取还会增加被限风险。
//
// 现在：读缓存很快、无副作用；要更新必须用户明确点「获取设备」。
func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	// ===== 契约：本接口只返回【已纳入管理】的设备 =====
	//
	// 账号↔设备模型（2026-09-18 设计确认）：
	//   云端设备 --获取设备(只读)--> 清单
	//   清单     --用户勾选-----> 已纳入管理（本接口返回的）
	//
	// 所以这里要：
	//   1. 默认只读缓存，不带 refresh=1 绝不拉云端
	//   2. 过滤掉未纳入管理的设备
	//   3. 云端全量清单请用 /api/devices/cloud
	//
	// ⚠️ 之前的 bug（用户反馈「扫完背景就有设备了」）：
	//   我第一版写成「缓存非空就返回，空了就往下走去云端拉」——
	//   这等于每次都拉！因为新用户缓存本来就是空的。
	//   用户刚扫码完，前端随便调一次就触发了真拉取。

	force := r.URL.Query().Get("refresh") == "1"
	if !force {
		// 缓存检查放在 session 检查之前 ——
		// 即使没有 session，也应该能返回缓存内容
		cached := s.store.Devices()
		out := make([]any, 0, len(cached))
		for _, d := range cached {
			if !s.meta.IsManaged(d.Did) {
				continue // 未纳入管理的不返回给业务页面
			}
			// ⚠️ lenses 用 LensCount(型号) 实时算，不要用缓存里存的 d.Lenses。
			//
			// 因为 device-cache.json 是【旧数据】（当时 Lenses 写死 1），
			// 直接返回它会显示「单摄」，即使型号其实是双摄。
			// 2026-10-11 用户反馈"我的摄像头是双摄的，你怎么没识别到"，
			// 一部分原因就在这里。
			out = append(out, map[string]any{
				"did": d.Did, "name": d.Name, "isOnline": d.IsOnline,
				"shared": d.Shared, "lenses": stream.LensCount(d.Model), "hasPTZ": IsPTZCapable(d.Model),
				"accountId": d.AccountID,
				"model":     d.Model, "localIp": d.LocalIP,
			})
		}
		writeJSON(w, 200, map[string]any{
			"devices": out, "cached": true, "count": len(out),
			"fetched": len(cached) > 0, // 区分「没获取过」和「获取了但没纳入」
		})
		return
	}

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
			// 标记归属账号（前端可按账号筛选）
			d.AccountID = sess.UserID
			// lenses = **实际能提供几路**，不是「型号声称几路」。
			//
			// ===== 双摄检测（2026-10-11 重做）=====
			//
			// 【推翻 2026-09-16 的旧结论】
			//
			// 旧结论说「channel=2 与 channel=1 是同一视场」，那是基于
			// cs2 旧通道测的，而那条通道本身有问题，结论不可靠。
			//
			// 用官方通道（OAuth2+PPCS）重测，结论完全相反：
			//   register_raw_data 的第 3 个参数是【通道号】，
			//   我们原来只注册了 channel=0 —— 所以第二路的数据根本没回调！
			//
			//   逐通道注册后实测（isa.camera.hlmax）：
			//     ch0 -> 247 帧   ch1 -> 232 帧
			//     两路都是 2560x1440，内容完全不同：
			//       channel 1 = 球机（PTZ 可转）
			//       channel 2 = 枪机（固定广角）
			//
			// 所以 CW500 确实是「枪球一体机」= 两个物理镜头。
			d.Lenses = stream.LensCount(d.Model)
			d.HasPTZ = IsPTZCapable(d.Model)
			if d.Lenses >= 2 {
				d.SecondLensClaimed = true
			}
			// 别名 + 最后活动（前端卡片要用）
			meta := s.meta.Get(d.Did)
			if meta.Hidden {
				continue // 用户主动隐藏的设备（通常是已拆掉的离线机）
			}
			d.Alias = meta.Alias
			d.AliasEn = meta.AliasEn
			d.Favorite = meta.Favorite
			d.LastActivity = s.lastSeen(d.Did)
			all = append(all, d)
		}
	}

	// 全部失败才算错误
	if len(all) == 0 && lastErr != nil {
		writeErr(w, 500, "获取设备失败: "+lastErr.Error())
		return
	}

	// 拉到新数据 -> 写入缓存（内存 + 落盘），
	// 下次默认读缓存就不用再麻烦小米云端了。
	if len(all) > 0 {
		s.store.CacheDevices(all)
	}

	// 附带账号列表（供前端做筛选下拉）
	accounts := s.store.Accounts()

	// 注意：这个接口按契约【只返回已纳入管理的设备】。
	// 云端全量清单请用 /api/devices/cloud。
	managedOnly := make([]xiaomi.Device, 0, len(all))
	for _, d := range all {
		if s.meta.IsManaged(d.Did) {
			managedOnly = append(managedOnly, d)
		}
	}
	writeJSON(w, 200, map[string]any{
		"devices":   managedOnly,
		"accounts":  accounts,
		"needLogin": false,
		"cached":    false,
		"count":     len(managedOnly),
	})
}

func (s *Server) staticHandler() http.Handler {
	dir := filepath.Join(s.dataDir, "web")
	if st, err := os.Stat(filepath.Join(dir, "index.html")); err != nil || st.IsDir() {
		for _, cand := range []string{
			filepath.Join(os.Getenv("TRIM_APPDEST"), "www"),
			filepath.Join(appDestDir, "www"),
		} {
			if cand != "" {
				if st2, err2 := os.Stat(filepath.Join(cand, "index.html")); err2 == nil && !st2.IsDir() {
					dir = cand
					break
				}
			}
		}
		if st, err := os.Stat(filepath.Join(dir, "index.html")); err != nil || st.IsDir() {
			if matches, err := filepath.Glob("/vol*/@appcenter/micam/www/index.html"); err == nil && len(matches) > 0 {
				dir = filepath.Dir(matches[0])
			} else {
				dir = "web"
			}
		}
	}
	fs := http.FileServer(http.Dir(dir))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// HTML 必须每次校验，不能强缓存。
		//
		// 踩过的坑（2026-09-16）：
		//   浏览器缓存了旧版 index.html，新版部署后用户仍加载旧 JS，
		//   结果实时预览卡在「加载中…」（旧版有 ReferenceError）。
		//   用户看到的是"服务端坏了"，其实是缓存。
		//
		// no-cache 不是"不缓存"，而是"每次都向服务器校验"，
		// 配合 Last-Modified / ETag 走 304，既正确又省流量。
		p := r.URL.Path
		if p == "/" || p == "/index.html" || strings.HasSuffix(p, ".html") {
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate, max-age=0")
			w.Header().Set("Pragma", "no-cache")
			w.Header().Set("Expires", "0")
		} else {
			// 静态资源（现在是：仅 hls.min.js / 图标，其余都内联在 HTML 里）
			w.Header().Set("Cache-Control", "public, max-age=86400")
		}
		fs.ServeHTTP(w, r)
	})
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

// corsMiddleware 按开放模式配置决定是否允许跨域。
//
// 默认关闭；开发者模式下可打开，方便网页端 / Electron 调试。
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Dev.CORS {
			origin := r.Header.Get("Origin")
			if origin == "" {
				origin = "*"
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			w.Header().Set("Access-Control-Max-Age", "86400")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// watchSocket 监控 socket 文件，丢失时自动重建。
//
// 场景：飞牛应用中心替换应用目录时删掉了 app.sock，
// 但进程还活着，网关就再也连不上了（Bad Gateway）。
func watchSocket(path string, srv *http.Server) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()

	for range t.C {
		if _, err := os.Stat(path); err == nil {
			continue
		}

		// socket 文件没了 —— 重建
		log.Printf("检测到 socket 丢失，正在重建: %s", path)
		_ = os.Remove(path)

		ln, err := net.Listen("unix", path)
		if err != nil {
			log.Printf("重建 socket 失败: %v", err)
			continue
		}
		if err := os.Chmod(path, 0o660); err != nil {
			logf("[main] socket 重建后设置权限失败 %s: %v", path, err)
		}

		go func(l net.Listener) {
			if err := srv.Serve(l); err != nil && err != http.ErrServerClosed {
				log.Printf("socket listener 退出: %v", err)
			}
		}(ln)

		log.Printf("socket 已重建: %s", path)
	}
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

// copyWebAssets 把 www 目录下的静态资源复制到部署目录。
//
// 为什么需要：前端原来只有 index.html，后来加了 hls.min.js。
// 只复制 index.html 的话，播放库会 404 -> HLS 播放黑屏。
func copyWebAssets(srcDir, dstDir string) {
	// 递归复制（assets/ 子目录有 css/js/ui/vendor/img 多层）
	n := copyTree(srcDir, dstDir, "")
	if n > 0 {
		log.Printf("已部署静态资源: %d 个文件", n)
	}
}

// copyTree 递归复制目录，跳过 index.html（单独处理）。
func copyTree(srcDir, dstDir, rel string) int {
	src := filepath.Join(srcDir, rel)
	dst := filepath.Join(dstDir, rel)

	entries, err := os.ReadDir(src)
	if err != nil {
		return 0
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return 0
	}

	var n int
	for _, ent := range entries {
		name := ent.Name()
		childRel := filepath.Join(rel, name)

		if ent.IsDir() {
			n += copyTree(srcDir, dstDir, childRel)
			continue
		}
		// index.html 由 deployWeb 单独写，且要经过 HTML 结构校验
		if rel == "" && name == "index.html" {
			continue
		}

		s := filepath.Join(srcDir, childRel)
		d := filepath.Join(dstDir, childRel)

		si, err := os.Stat(s)
		if err != nil {
			continue
		}
		if di, err := os.Stat(d); err == nil && !di.ModTime().Before(si.ModTime()) {
			continue
		}
		raw, err := os.ReadFile(s)
		if err != nil {
			continue
		}
		if err := os.WriteFile(d, raw, 0o644); err != nil {
			log.Printf("复制静态资源 %s 失败: %v", childRel, err)
			continue
		}
		n++
	}
	return n
}

// reapOrphanSidecars 杀掉上一代遗留的 sidecar（2026-10-13）。
//
// 只杀 cmdline 含 miot_mjpeg.py / miot_camera.py 且是 python 解释器的进程。
func reapOrphanSidecars() {
	me := os.Getpid()
	ents, err := os.ReadDir("/proc")
	if err != nil {
		logf("[miot] 清理遗留 sidecar: 读 /proc 失败 %v", err)
		return
	}
	killed := 0
	for _, e := range ents {
		pid, aerr := strconv.Atoi(e.Name())
		if aerr != nil || pid == me {
			continue
		}
		raw, rerr := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if rerr != nil || len(raw) == 0 {
			continue
		}
		// 逐字节拼成字符串（NUL 换成空格）
		var sb strings.Builder
		for _, b := range raw {
			if b == 0 {
				sb.WriteByte(32)
			} else {
				sb.WriteByte(b)
			}
		}
		cmd := sb.String()
		if !strings.Contains(cmd, "miot_mjpeg.py") && !strings.Contains(cmd, "miot_camera.py") {
			continue
		}
		if !strings.Contains(cmd, "python") {
			continue
		}
		if p, ferr := os.FindProcess(pid); ferr == nil {
			_ = p.Kill()
			killed++
		}
	}
	if killed > 0 {
		logf("[miot] 已清理 %d 个上一代遗留的 sidecar（释放摄像头连接槽）", killed)
	}
}

// checkMiotLib 启动时检查小米闭源库是否存在（2026-10-13 为开源做准备）。
//
// 该库（libmiot_camera_lite.so）是小米官方预编译闭源库，
// 本仓库【不随包分发】，需要用户自行获取（见 scripts/fetch-miot-lib.sh）。
//
// 缺失时不阻断启动，只是【视频流、录像、云台方向】不可用；
// 设备管理、miIO 通信、云台位置查询仍然可用。
// miotLibPath 启动时探到的闭源库路径，空串表示未找到。
var miotLibPath string

func checkMiotLib() {
	p := miot.FindLib("")
	if p != "" {
		miotLibPath = p
		logf("[miot] 闭源库已就绪: %s", p)
		return
	}
	logf("[miot] ⚠️ 未找到 libmiot_camera_lite.so")
	logf("[miot]    受影响的仅: 视频流 / 录像 / 云台方向控制")
	logf("[miot]    仍可用: 设备管理、miIO 本地通信、云台位置查询")
	logf("[miot]    获取方式: bash scripts/fetch-miot-lib.sh")
	logf("[miot]    或设置环境变量 MICAM_MIOT_LIB=/path/to/libmiot_camera_lite.so")
}
