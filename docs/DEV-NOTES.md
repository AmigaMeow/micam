# MiCam 开发笔记（cs2 直连踩坑全记录）

> 更新：2026-09-14 | 版本 0.2.0
> 用途：记录实现过程中踩过的所有坑，供后续维护和 Mac 端参考

---

## 一、go2rtc 使用要点

### 1.1 Dial 内部已完成握手

~~~go
prod, _ := xiaomi.Dial(src)   // 内部完成 cs2 握手 + probe
// prod.Start()  <-- 千万别在这里调！会 'pop buffer is full'
~~~

### 1.2 Producer 是推模式，必须有人读

~~~
正确顺序：
  1. Dial
  2. 建 consumer 并绑定轨道
  3. 起 goroutine 读（consumer.WriteTo）
  4. 起 goroutine 跑 prod.Start()  <- 这是读循环，会阻塞
~~~

**顺序错了会怎样**：先 Start 后绑 consumer，producer 内部缓冲撑爆，
报 `miss: read media: cs2: pop buffer is full`。

### 1.3 可用的 consumer

| consumer | 接受编码 | 说明 |
|---|---|---|
| `mpegts.Consumer` | H264/H265/AAC/OPUS | ✅ 我们用这个 |
| `mjpeg.Consumer` | **只接受 JPEG/RAW** | ❌ 不匹配 H265 |
| `flv.Consumer` | H264/AAC | ❌ 上游不支持 HEVC |
| `hls.Consumer` | 需要转码 | |

**关键**：`MatchMedia` 只做**同名编码**匹配，不做转码协商。
所以 H265 源只能用 `mpegts.Consumer`。

---

## 二、ffmpeg 转码要点

### 2.1 从流中途开始会失败

**症状**：
~~~
[hevc] PPS id out of range: 0
[hevc] Could not find ref with POC 114
[hevc] Error constructing the frame RPS
Output file does not contain any stream
~~~

**原因**：consumer 从流任意位置开始，第一个帧不是关键帧，缺少 SPS/PPS。

**解决**：加大探测缓冲，让 ffmpeg 自己等到参数集。

~~~bash
-analyzeduration 10M   # 分析 10 秒的数据
-probesize 10M         # 探测 10MB
-fflags +discardcorrupt+genpts
-err_detect ignore_err
~~~

### 2.2 快照加速

加 `-skip_frame nokey` 只解码关键帧，快照从 13 秒降到 **6 秒**：

~~~bash
-skip_frame nokey
-analyzeduration 3M
-probesize 5M
-frames:v 1
~~~

### 2.3 参数坑

| 参数 | 坑 |
|---|---|
| `-boundary xxx` | ❌ **不是有效选项**！用 `-f mpjpeg` 即可 |
| `-c copy` | 录像时用，不转码，CPU 占用极低 |
| `-f segment` | 分段录像，配 `-segment_atclocktime 1` 按整点切 |

---

## 三、HTTP 流式输出要点

### 3.1 必须主动 Flush 响应头

**症状**：响应头里出现 `Content-Length: 0`，浏览器不渲染。

**原因**：Go 的 http 包在 handler 返回时会补上 Content-Length。

**解决**：立即 WriteHeader + Flush，进入 chunked 模式。

~~~go
w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")
flusher, _ := w.(http.Flusher)
w.WriteHeader(200)
flusher.Flush()   // <- 关键！否则 Content-Length: 0
~~~

### 3.2 不要加 Connection: close

会导致浏览器把响应当一次性请求，`<img>` 不渲染流。

### 3.3 客户端断开必须主动释放

**这是最大的坑**，症状：第一次请求成功，第二次开始全部卡死。

**原因链**：
~~~
客户端断开
  -> handler 返回
  -> 但 consumer.WriteTo 还在跑（缓冲队列 Write 不阻塞，不会返回）
  -> mc.Release() 永不执行
  -> 摄像头连接永不释放
  -> 下次 Dial 失败（cs2 同一设备只允许一路）
~~~

**解决**：runFFmpeg 返回 cleanup 函数，handler defer 调用，
cleanup 里主动 `cons.Stop()` 让 WriteTo 返回。

~~~go
cleanup := func() {
    once.Do(func() {
        pr.Close()      // 让 ffmpeg 读到 EOF
        release()       // 停 consumer -> WriteTo 返回
        <-done          // 等 WriteTo 真的退出
    })
}
~~~

---

## 四、并发与锁

### 4.1 同一设备必须串行

cs2 只允许一路连接。用 `didLock`（每设备一把锁）串行化请求。

### 4.2 不要用全局锁

**踩坑**：用全局锁 + 双重解锁，导致 `fatal error: sync: unlock of unlocked mutex`，
整个进程崩溃。

**教训**：Release 一定要幂等（`sync.Once` 保护），
锁的粒度尽量小，能按设备分就按设备分。

### 4.3 io.Pipe 会反压

`io.Pipe` 是同步的：ffmpeg 读得慢会阻塞 consumer 读取，
进而撑爆 producer 内部缓冲。

**解决**：用带缓冲的队列（`bufferedPipe`）解耦。

---

## 五、小米云 API 要点

### 5.1 限流

`/v2/device/miss_get_vendor` 有严格限流，频繁调用会 `code=-12 msg=throttle deny`。

**解决**：
1. 拿到凭据后**落盘缓存**（`sources.json`）
2. 播放时优先用缓存，不重复申请
3. 限流时指数退避（5/10/20/40 秒）

### 5.2 共享家庭设备

**坑**：`/v2/home/device_list` 对共享家庭返回空 did 列表。

**解决**：必须用 `/v2/home/home_device_list`，带参数：
~~~json
{"home_owner": <uid>, "home_id": <id>, "limit": 200,
 "get_split_device": true, "support_smart_home": true}
~~~

### 5.3 签名

- nonce = 8 字节随机 + uint32(Unix()/60)（**分钟**级时间戳，不是秒！）
- RC4 要**丢弃前 1024 字节**密钥流
- 双重签名：先对明文算 `rc4_hash__`，再对密文算 `signature`

---

## 六、录像实现

~~~bash
ffmpeg -f mpegts -i pipe:0 \
  -c copy \                    # 不转码，原始画质
  -f segment \
  -segment_time 30 \           # 每 30 秒一段
  -segment_format matroska \   # MKV 断电不坏
  -segment_atclocktime 1 \     # 按整点切
  -reset_timestamps 1 \
  -strftime 1 \
  '/path/%Y%m%d_%H%M%S.mkv'
~~~

**设计要点**：
- `-c copy` 不转码 -> CPU 几乎为零，画质无损（实测 HEVC 2560x1440）
- MKV 容器 -> 断电不损坏整个文件
- 目录结构：`录像根/设备名/日期/时间.mkv`
- 磁盘水位保护：剩余 < 10GB 自动停止

---

## 七、飞牛网关集成（重要）

### 7.1 网关前缀必须自己剥

**症状**：应用中心装完后打开是 **404 page not found**。

**原因**：飞牛网关按 `gatewayPrefix` 转发时会**保留前缀**：

~~~
浏览器请求: /app/micam/api/health
网关转发:   /app/micam/api/health   <- 前缀还在！
我们的服务: 只注册了 /api/health -> 404
~~~

**解决**：加 middleware 剥掉前缀。

~~~go
func stripPrefixMiddleware(next http.Handler, prefix string) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if prefix != "" && strings.HasPrefix(r.URL.Path, prefix) {
            p := strings.TrimPrefix(r.URL.Path, prefix)
            if p == "" { p = "/" }
            r2 := r.Clone(r.Context())
            r2.URL.Path = p
            next.ServeHTTP(w, r2)
            return
        }
        next.ServeHTTP(w, r)
    })
}
~~~

### 7.2 网关的 socket 路径映射

网关读的是 **`/var/apps/<app>/target/app.sock`**，
而 `/var/apps/<app>/target` 是指向 `/vol1/@appcenter/<app>` 的符号链接。

所以 socket 建在 `${TRIM_APPDEST}/app.sock` 就是对的（路径会自动映射）。

**验证方法**：
~~~bash
# 看网关是否注册了你的应用
journalctl -u trim_http_cgi | grep "upstream register" | grep micam

# 期望看到：
# upstream register [/app/micam] micam.Application /var/apps/micam/target/app.sock
~~~

### 7.3 网关的鉴权层（别被误导）

直接 curl 网关会返回 **`invalid token`**，这是**正常的** —— 
网关有鉴权层，浏览器带着登录 cookie 才能通过。

**不要以为路由没通**。验证路由应该直接测 socket：

~~~bash
curl --unix-socket /vol1/@appcenter/micam/app.sock http://localhost/app/micam
~~~

### 7.4 双监听（必须）

**问题**：只监听 Unix socket 的话，**局域网客户端（如 Mac App）访问不了**。

**解决**：同时监听两个。

~~~bash
micamd --socket ${TRIM_APPDEST}/app.sock \   # 飞牛界面（网关）
       --http   :8900                        # 局域网 / Mac App
~~~

代码里用多个 listener 同时 Serve：

~~~go
for i := 1; i < len(listeners); i++ {
    go func(l net.Listener) {
        httpSrv.Serve(l)
    }(listeners[i])
}
ln := listeners[0]
httpSrv.Serve(ln)
~~~

### 7.5 网关何时重新加载配置

网关（`trim_http_cgi`）在**应用启动时**注册路由：

~~~
20:58:31 trim_http_cgi: upstream register [/app/micam] ... service register success.
~~~

**注意**：网关进程本身不需要重启，安装/启动应用时它会自动注册。
如果没看到注册日志，检查 `ui/config` 的 `gatewayPrefix` / `gatewaySocket`。

---

## 八、性能实测数据

| 指标 | 数值 |
|---|---|
| Dial 耗时 | 0.3 - 0.9 秒 |
| MJPEG 首帧 | **0.78 秒** |
| 快照耗时 | **6 秒**（优化前 13 秒） |
| MJPEG 码率 | ~150 KB/s（320px, 3fps） |
| 录像码率 | ~110 KB/s（原画质 HEVC） |
| 连续请求 | ✅ 稳定（修复连接泄漏后） |

---

## 九、诊断方法论

遇到「第一次成功、第二次失败」这类问题时，有效的排查步骤：

1. **隔离环境**：写独立程序复刻逻辑，确认是不是代码问题
   - 实测 go2rtc 原生连续 3 次 Dial 全部成功 -> 排除摄像头限制
2. **进程内诊断**：加 `/diag` 端点，在插件进程内跑测试程序逻辑
   - 实测内存 writer 连续 3 次成功 -> 定位到 ffmpeg 路径
3. **逐步逼近**：对比成功路径和失败路径的每一处差异
   - 内存 writer vs ffmpeg 子进程 -> 发现反压问题
   - 客户端主动断开 vs 超时断开 -> 发现连接泄漏
