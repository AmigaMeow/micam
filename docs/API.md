# MiCam API 接入文档

版本: 0.9.3

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
    GET  /api/recordings/dates?name=XXX     可用日期
    GET  /api/recordings/list?name=&date=   文件列表（含时长/缩略图）
    GET  /api/recordings/play?name=&file=&t= 转码回放（t=起始秒）
    GET  /api/recordings/thumb?name=&file=   缩略图
    GET  /api/recordings/download?name=&file= 下载原文件
    POST /api/recordings/delete              删除单个文件
    GET  /api/recordings/cache               转码缓存状态

### 5.1 录像回放需要转码（重要）

录像文件是 **MKV 容器 + HEVC 编码**（摄像头原生格式）。

    浏览器兼容性：
      Chrome  -> MKV 可以，HEVC 不行
      Safari  -> MKV 不行，HEVC 可以
      Firefox -> 都不行

**所以回放必须转成 H.264 + MP4。**

转码缓存让二次播放快 940 倍：

    首次播放   21.6s（同时后台生成缓存）
    二次播放   0.023s（直接发文件，支持 Range 可拖动）

### 5.2 凭据管理（三层，别混淆）

    第 1 层  passToken        扫码登录拿到    长期（数月）
    第 2 层  serviceToken     由 passToken 换  短期，自动续期
    第 3 层  设备 P2P 凭据     每台摄像头一份  >= 24 小时，自动续期

**客户端不需要自己刷凭据，服务端全包了。**

    GET  /api/accounts/status    三层凭据说明 + 会话状态
    GET  /api/accounts/export    导出凭据（含 passToken，敏感）
    POST /api/accounts/import    导入凭据
    GET  /api/live/cred-status   设备凭据状态（年龄/剩余/冷却）
    POST /api/live/renew         手动续期设备凭据

**限流提示**：设备凭据接口有严格限流（code=-12 throttle deny），
客户端遇到时不要反复重试，看 cooldownMinutes。

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

---

## 十、连接共享（重要）

### 10.1 服务端是共享连接

每台摄像头只维护【一条】cs2 连接，多个客户端共享：

    每台设备一个 producer（共享）
      +-- consumer A (网页 HLS)
      +-- consumer B (Mac App MJPEG)
      +-- consumer C (抓拍)

**所以：网页和 Mac App 可以同时看同一台摄像头，不会互相踢掉。**

### 10.2 连接的保温与回收

    最后一个消费者离开后，连接保持 25 秒
    25 秒内再有人看 -> 直接复用（秒开）
    超过 25 秒    -> 释放连接（下次重新握手）

**客户端适配建议**：短时间内反复切换设备/重连不会重新握手。
不要为了「保持热连接」而空转请求——那会占着摄像头。

### 10.3 历史坑（已修复）

旧版本是「独占连接」，同一摄像头的第二个请求要等 60 秒。
如果你在旧版本遇到过「等了很久才出画」，升级到 0.8.1+ 即可。

---

## 十一、性能特征与客户端适配

### 11.1 启动耗时（实测）

    场景                     耗时        说明
    ----------------------  ---------  ------------------
    HLS 首次（冷连接）      5.4-6.2s   含握手+等关键帧+首分片
    HLS 二次（热连接）      0.01-0.03s 复用连接，几乎瞬间
    抓拍 首次               2.4s
    抓拍 热连接             1.6s
    MJPEG 首帧（热连接）    2.3s
    云台操作                0.2-0.6s
    设备列表                0.3-2s

### 11.2 为什么冷启动要 5 秒

    时间预算：
      CS2 握手            0.3s   协议决定
      等摄像头关键帧      2.3s   GOP 决定（摄像头每 1-2s 一个 I 帧）
      ffmpeg 出首个分片   1.0s   hls_time 决定
      ffmpeg 启动/探测    1.5s
                        -----
      合计                5.1s

**这是物理下限，不是 bug。** 摄像头不主动给 I 帧就没法更快解码。

### 11.3 客户端 UI 建议

    0.0s  发起请求，显示「连接中」+ 转圈
    2.0s  提示「正在建立视频流…」
    5.0s  开始播放
    12s   还没出画 -> 提示「设备可能被占用」并给重试按钮

**不要**在 1-2 秒内就判定失败。
**不要**并发发起多个播放请求（虽然服务端支持共享，但没必要）。

### 11.4 播放方式选择

    GET /api/dev/capabilities -> features.live.hls

    true  -> 优先 HLS（CPU 低、2K 流畅）
    false -> 用 MJPEG

macOS 提示：CFNetwork 读不了 multipart/x-mixed-replace，
MJPEG 需要用裸 socket 读取。HLS 没有这个问题（标准 HTTP 分片）。
**所以 Mac App 建议优先用 HLS。**

### 11.5 服务端资源占用

    空闲（无观看）：  ~0%!C(MISSING)PU（连接已释放）
    HLS 播放中：      90-150%!C(MISSING)PU（一路 2K 转码）
    MJPEG 播放中：    300-350%!C(MISSING)PU（数据量放大 27 倍）
    抓拍瞬时：        100-200%!C(MISSING)PU（1 秒内结束）

**建议客户端默认用 HLS**，MJPEG 只做兜底。

---

## 十二、版本兼容

    0.7.x   当前。HLS + MJPEG + 云台 + 录像 + 存储监控
    0.6.x   加入 HLS
    0.5.x   诊断导出
    0.3.x   早期 MJPEG 版本

接口变更遵循：
    新增接口       -> patch 版本
    修改响应字段   -> minor 版本，保留旧字段
    删除接口       -> major 版本

