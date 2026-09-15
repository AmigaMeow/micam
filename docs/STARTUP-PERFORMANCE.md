# 启动速度优化（实测数据）

> 2026-09-15 实测 | 硬件 Intel J4125 | 设备 CW300 / CW500

---

## 一、优化前后对比

| 场景 | 优化前 | 优化后 | 提升 |
|---|---|---|---|
| **首次 HLS 起播** | 10.2s | **5.4s** | 1.9x |
| **二次播放（热连接）** | 10.2s | **0.01s** | **1000x** |
| **并发抓拍** | 60s 超时 | **2.4s** | - |
| **并发 MJPEG** | 35s 超时 | **2.3s** | 15x |
| **抓拍（单独）** | 6.7s | **2.4s** | 2.8x |

---

## 二、瓶颈定位过程

### 2.1 分阶段计时

给每个阶段加了 `⏱` 日志：

~~~
[1] 等锁        0s
[2] 取凭据      0s
[3] CS2 握手    445ms
[4] 建 consumer 465ms
[5] useConn     502ms      <- 连摄像头只要半秒
[6] 等 m3u8     10.239s    <- 瓶颈！占 91%
~~~

**结论：连摄像头很快，慢在等 ffmpeg 出第一个分片。**

### 2.2 摄像头本身有多快

写了个 `cmd/livetest` 工具，直接把 cs2 流输出到 stdout，测关键帧到达时间：

~~~
dial 完成:      0.27s
首个字节:       0.60s
首个 IDR 帧:    2.31s     <- 关键帧很快
IDR 间隔:       1.0-2.0s  <- 很密集
~~~

**摄像头 2.3 秒就给关键帧，排除摄像头因素。**

### 2.3 ffmpeg 参数才是元凶

对比不同 `-analyzeduration`：

| analyzeduration | 首分片时间 |
|---|---|
| **5M（旧）** | **9.31s** |
| 1M | 4.81s |
| **100k（采用）** | **4.61s** |
| 无参数（默认 5s） | 9.22s |

**`-analyzeduration 5M` 意思是「等 5 秒流时长再分析」，白白浪费 4.7 秒。**

同时发现 `-use_wallclock_as_timestamps 1` 有害（强制等真实时间流逝），已移除。

---

## 三、架构缺陷：「独占连接」导致互斥

### 3.1 现象

~~~
08:47:14 [snap] 抓拍请求开始
08:47:14 HLS 转码已启动          <- HLS 抢占了摄像头连接
08:48:15 ⏱ [1] 等锁 1m0.624s    <- 抓拍等了 60 秒！
08:48:16 [snap] 失败: context deadline exceeded
~~~

**cs2 协议同一设备只允许一路连接**，旧的「独占」架构下一个请求占住连接，
其他请求（抓拍/MJPEG/别的客户端）全部阻塞。

### 3.2 修复：连接共享

改成「一个 producer + 多个 consumer」：

~~~
每台设备一个 producer（共享）
  +-- consumer A (HLS)
  +-- consumer B (MJPEG)
  +-- consumer C (抓拍)
  +-- consumer D (Mac App)
~~~

最后一个消费者离开后，连接保持 25 秒（`connIdle`）再释放。
这样二次观看**无需重新握手**。

### 3.3 效果

~~~
首次 HLS:        5.418s
立刻再请求:      0.008s     <- 复用热连接
并发抓拍:        2.447s     <- 原来 60s 超时
并发 MJPEG:      2.320s
并发 HLS:        0.015s
~~~

日志确认：`⏱ [0] 复用热连接 0s`

---

## 四、最终的参数

### HLS 转码

~~~
-analyzeduration 100000     # 关键！原 5M
-probesize       100000     # 关键！原 5M
-hls_time        1          # 原 2
-g               15         # 1 秒 GOP（15fps）
-keyint_min      15
-sc_threshold    0          # 禁用场景切换检测
-force_key_frames expr:gte(t,n_forced*1)
-c:v libx264 -preset veryfast -tune zerolatency -crf 26
~~~

### 抓拍

~~~
-analyzeduration 100000     # 原 3M
-probesize       100000     # 原 5M
-skip_frame nokey           # 只解关键帧
-frames:v 1
~~~

### MJPEG

~~~
-analyzeduration 100000     # 原 10M（这就是它慢的原因）
-probesize       100000     # 原 10M
-flags +low_delay
~~~

### 前端播放器

~~~javascript
new Hls({
  lowLatencyMode: true,
  liveSyncDurationCount: 1,      // 原 2
  liveMaxLatencyDurationCount: 3,
  maxBufferLength: 4,
  manifestLoadingMaxRetry: 3,
  manifestLoadingRetryDelay: 500,
})
~~~

---

## 五、时间预算（理论下限）

~~~
CS2 握手            0.3s   （协议决定，无法再快）
等摄像头关键帧      2.3s   （GOP 决定，除非能主动请求 I 帧）
ffmpeg 出首个分片   1.0s   （hls_time 决定）
                  -----
合计约              3.6s

实测冷启动          5.4s   （含 ffmpeg 启动/探测开销）
实测热连接          0.01s  （复用连接，无需重新握手）
~~~

**要再快只能**：
1. 主动向摄像头请求 I 帧（需要逆向 cs2 命令）
2. 一直保持热连接（CPU 代价）
3. 用 WebRTC 替代 HLS（无分片等待）

---

## 六、对比 EasyNVR

实测观察：

~~~
EasyNVR 的 xiaomi 插件：常驻进程，无 ffmpeg 运行
我们的 micamd：          按需启动 ffmpeg（145% CPU）
~~~

**EasyNVR 不转码**，所以启动快。它的方案可能是：
- 直接转发原始 HEVC（浏览器用 WASM 解码），或
- 按需拉起 ffmpeg 但参数调得好，或
- 维护常驻热连接

我们选择了「转码 + 连接共享」，因为：
- 浏览器原生不支持 HEVC（必须转 H264）
- 连接共享已经解决了互斥问题
- 热连接把二次观看降到 0.01s

**后续如果要进一步提速，可以研究「按需转发原始流 + 前端 WASM 解码」。**

---

## 七、工具

### cmd/livetest

把 cs2 流实时输出到 stdout，用于测量关键帧时间：

~~~bash
go build -o /tmp/livetest ./cmd/livetest
/tmp/livetest "xiaomi://..." > /tmp/stream.ts 2>/tmp/livetest.log
cat /tmp/livetest.log   # 看 dial 耗时
~~~

### 分阶段计时日志

生产代码里有 `⏱` 前缀的日志：

~~~bash
grep '⏱' /vol1/@appdata/micam/micamd.log
~~~

---

## 八、踩坑记录

| 坑 | 现象 | 解法 |
|---|---|---|
| analyzeduration 过大 | 首分片 9-10 秒 | 降到 100k |
| use_wallclock_as_timestamps | 等真实时间流逝 | 移除 |
| 独占连接 | 并发请求等 60 秒 | 连接共享 |
| 抓拍占位抢连接 | 抓拍和 HLS 互斥 | 去掉占位（热连接已够快） |
| hls_time 2 | 起播慢 1 秒 | 改成 1 |
| 播放器缓冲 2 分片 | 多等 2 秒 | liveSyncDurationCount 1 |