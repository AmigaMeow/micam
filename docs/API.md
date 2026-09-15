# MiCam API 接入文档

版本: 0.8.0

---

## 一、快速开始

### 1.1 两种接入方式

方式 A：局域网直连（推荐给桌面客户端）

    http://<nas-ip>:8900

方式 B：飞牛网关（插件内页面用）

    http://<nas-ip>/app/micam

### 1.2 第一步：探测能力

    GET /api/health
    -> {"app":"micam","ok":true,"version":"x.y.z"}

    GET /api/dev/capabilities
    -> 硬件、支持的功能、限制（fps/分辨率）

客户端应据此决定：
- 用 HLS 还是 MJPEG
- 最高分辨率取多少
- 哪些功能可显示

### 1.3 第二步：拿到设备

    GET /api/devices
    -> {"devices":[{"did":"...","name":"...","isOnline":true,"shared":false}]}

共享设备字段 shared=true（来自别人分享的家庭）。

### 1.4 第三步：播放

首选 HLS：

    <video src="http://<nas>:8900/api/live/hls?did=XXX&fps=15&w=0">

Safari 原生支持；其他浏览器用 hls.js。
重要：m3u8 里的分片行是相对路径，交给播放器解析即可，不要手工拼接。

兜底 MJPEG：

    <img src="http://<nas>:8900/api/live/mjpeg?did=XXX&fps=12&w=1280">

---

## 二、摄像头 vs 其他设备

本插件只做摄像头和存储监控。判断方法：

    model 含 .camera. / .cateye. / .feeder.  -> 摄像头

摄像头的控制走 cs2 P2P 私有协议（不是小米云 MIoT）：

    0x100  auth      认证
    0x102  video     视频流
    0x106  speaker   对讲（命令已知，未实现）
    0x112  motor     云台（已实现）
    0x1001 encoded   编码包装

---

## 三、播放接口详解

### 3.1 HLS（推荐）

    GET /api/live/hls?did=XXX&fps=15&w=0

参数：

    did   设备 DID（必填）
    fps   帧率，默认 15，上限 15
    w     宽度，0=原画（2K 2560），可填 1280 / 640
    seg   分片文件名（内部使用，客户端不用传）

返回 m3u8：

    #EXTM3U
    #EXT-X-VERSION:3
    #EXT-X-TARGETDURATION:2
    #EXT-X-PLAYLIST-TYPE:EVENT
    #EXTINF:2.000000,
    hls?did=XXX&seg=seg0.ts

**注意最后一行**：是相对路径且保留了 hls 段。
播放器按标准相对路径解析会得到正确地址。
如果手工拼接，必须拼成 /api/live/hls?did=XXX&seg=seg0.ts。

性能（Intel J4125 实测）：

    方案          CPU     首帧     2K
    MJPEG        335%!-(MISSING)19s    卡
    HLS           94%!s(MISSING)      流畅

### 3.2 MJPEG（兼容）

    GET /api/live/mjpeg?did=XXX&fps=12&w=1280

Content-Type: multipart/x-mixed-replace; boundary=mjpegframe

每帧格式：

    --mjpegframe

    Content-Type: image/jpeg

    Content-Length: N

    

    <N 字节 JPEG>


**重要**：macOS/iOS 的 CFNetwork 读不了 multipart/x-mixed-replace
（连 didReceiveResponse 都不回调）。需用裸 socket 读取。
可以按 Content-Length 切帧，也可以按 FFD8FF / FFD9 标记切。

### 3.3 抓拍

    GET /api/live/snapshot?did=XXX&w=0
    -> image/jpeg

---

## 四、云台

    GET  /api/ptz/status?did=XXX
    POST /api/ptz/move   {"did":"XXX","operation":N}

operation：

    1 = 查询位置
    2 = 左
    3 = 右
    4 = 下
    5 = 上

响应：

    {"ok":true,"ret":0,"angle":50,"elevation":73}

**注意**：同一设备同时只能有一路 cs2 连接。
云台操作会短暂占用视频通道，客户端应串行化云台请求。

---

## 五、录像与存储

    GET  /api/storage                       存储总览
    GET  /api/storage/cleanup-preview       清理预览
    GET  /api/recordings                    录像任务
    POST /api/recordings                    开始录像
    POST /api/recordings/stop               停止录像
    GET  /api/recordings/files?name=XXX     文件列表
    POST /api/recordings/cleanup            清理过期

存储总览返回：

    disk         磁盘容量（totalGB/usedGB/freeGB/usedPercent）
    recordings   录像占用（totalGB/fileCount/keepDays/minFreeGB）
    cameras      各摄像头明细（files/sizeMB/lastTime）
    estimateHours 按码率估算可录时长

---

## 六、错误处理

### 6.1 HTTP 状态码

    200  成功
    400  参数错误
    404  设备不存在
    405  方法不允许
    500  内部错误
    502  上游失败（摄像头/小米云）
    504  超时（HLS 播放列表未就绪，可重试）

### 6.2 业务错误

错误响应格式：

    {"msg":"错误描述"}

常见情况：

    设备离线（<name>），无法播放      设备不在线
    miss: auth: {"result":"failed","code":3}   凭据失效，稍后重试
    throttle deny                    小米云限流，等 30 分钟
    播放列表尚未生成，请稍候重试     HLS 转码启动中，重试即可

### 6.3 重试建议

    504          -> 2 秒后重试，最多 3 次
    auth failed  -> 3-5 秒后重试（服务端会自动刷新凭据）
    设备离线     -> 不要重试，提示用户

---

## 七、凭据与限流

### 7.1 cs2 凭据有有效期

实测 8 小时后可能失效。服务端会自动刷新，但小米云有限流：

    throttle deny（code=-12）

刷新失败时会保留旧凭据，并有 30 分钟冷却。
客户端只需重试，不要频繁请求。

### 7.2 查询凭据状态

    GET /api/live/source-status
    -> {"devices":[{"did":"...","hasSource":true,"ageMinutes":120,"cooldownMinutes":0}]}

---

## 八、开放模式（开发者选项）

连续点击侧边栏底部版本号 7 次解锁。

    GET  /api/dev/status        解锁状态与开关
    POST /api/dev/tap           点击计数（解锁用）
    POST /api/dev/config        修改开关
    GET  /api/dev/capabilities  能力探测
    GET  /api/dev/openapi       OpenAPI 3.0 规格
    GET  /api/dev/docs          本文档

开关说明：

    cors        允许跨域（网页端 / Electron 调试用）
    allowLan    允许局域网直连（默认开）
    verboseLog  详细日志
    mockMode    模拟数据（不连真设备，UI 开发用）
    exposeSpec  暴露规格文档接口

---

## 九、接口自测

命令行：

    # 健康检查
    curl http://<nas>:8900/api/health

    # 设备列表
    curl http://<nas>:8900/api/devices

    # HLS 播放列表
    curl http://<nas>:8900/api/live/hls?did=XXX

    # 抓拍存文件
    curl -o snap.jpg http://<nas>:8900/api/live/snapshot?did=XXX

    # OpenAPI 规格（可导入 Postman / Insomnia）
    curl -o micam.json http://<nas>:8900/api/dev/openapi

---

## 十、版本兼容

    0.7.x   当前。HLS + MJPEG + 云台 + 录像 + 存储监控
    0.6.x   加入 HLS
    0.5.x   诊断导出
    0.3.x   早期 MJPEG 版本

接口变更遵循：
    新增接口       -> patch 版本
    修改响应字段   -> minor 版本，保留旧字段
    删除接口       -> major 版本

