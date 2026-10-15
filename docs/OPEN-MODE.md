# 开放模式（开发者选项）

> v0.8.0 新增

## 一、怎么打开

**连续点击左下角版本号 7 次**（和 Android 开发者选项一样）。

每次点击会提示「再点击 N 次开启开发者模式」。

解锁后侧边栏出现「开发者」菜单。

## 二、里面有什么

### 2.1 开放模式开关

| 开关 | 说明 | 默认 |
|---|---|---|
| 允许局域网直连 | 开启 :8900，第三方客户端可直连 | 开 |
| 允许跨域 (CORS) | 网页端 / Electron 调试用 | 关 |
| 暴露接口规格 | 开放 /api/dev/openapi 和 /api/dev/docs | 开 |
| 详细日志 | 记录凭据申请、连接生命周期 | 关 |
| 模拟数据模式 | 不连真设备（未实现） | 关 |

### 2.2 接口文档

- **OpenAPI JSON**：可导入 Postman / Insomnia / 自动生成客户端
- **Markdown**：人类可读的完整接入文档

也有 HTTP 接口：

    GET /api/dev/openapi
    GET /api/dev/docs

### 2.3 接口自测

列出所有接口，点「调用」直接请求，显示响应时间和 JSON。

### 2.4 能力探测

    GET /api/dev/capabilities

返回：

    {
      "app":     {"name":"micam","version":"0.8.0"},
      "hardware":{"cpuModel":"...","preferred":"h264_qsv","encoders":[...]},
      "features":{"live.hls":true,"live.mjpeg":true,"ptz":true,...},
      "limits":  {"maxFPS":15,"maxWidth":2560,"hlsSegmentSec":2}
    }

**第三方客户端应该先调这个**，据此决定用 HLS 还是 MJPEG、
分辨率取多少、哪些功能可显示。

## 三、接口一览（31 个）

| 分类 | 接口 |
|---|---|
| 系统 | /api/health · /api/platform · /api/setup/check · /api/setup/repair |
| 账号 | /api/accounts · /api/accounts/qr · /api/accounts/qr/status |
| 设备 | /api/devices · /api/devices/all · /api/homes |
| 播放 | /api/live/hls · /api/live/hls/info · /api/live/mjpeg · /api/live/snapshot · /api/live/source-status |
| 云台 | /api/ptz/status · /api/ptz/move |
| 录像 | /api/recordings · /api/recordings/stop · /api/recordings/files · /api/recordings/cleanup |
| 存储 | /api/storage · /api/storage/cleanup-preview |
| 诊断 | /api/diag/report · /api/diag/bundle |
| 开发者 | /api/dev/status · /api/dev/tap · /api/dev/config · /api/dev/capabilities · /api/dev/openapi · /api/dev/docs |

## 四、给第三方的接入建议

### 4.1 启动流程

    1. GET /api/health          -> 确认服务在、拿版本
    2. GET /api/dev/capabilities -> 拿能力与限制
    3. GET /api/devices          -> 拿设备列表
    4. 根据 capabilities 决定播放方式

### 4.2 播放选择

    features["live.hls"] == true  -> 优先 HLS（2K 流畅，CPU 低）
    否则                          -> MJPEG（兼容性最好）

### 4.3 云台

云台请求必须**串行化**：同一设备同时只能有一路 cs2 连接。

### 4.4 错误重试

    504          -> 2 秒后重试（最多 3 次）
    auth failed  -> 3-5 秒后重试（服务端自动刷新凭据）
    设备离线     -> 不重试

## 五、文件位置

| 文件 | 说明 |
|---|---|
| docs/API.md | 完整接入文档（Markdown） |
| docs/openapi.json | OpenAPI 3.0 规格 |
| docs/CONTROL-PROTOCOL.md | 设备控制协议（P2P 命令表） |
| docs/DEV-NOTES.md | 开发笔记（踩坑记录） |

也可以直接从服务拉：

    curl http://<nas>:8900/api/dev/docs
    curl http://<nas>:8900/api/dev/openapi
