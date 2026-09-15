# 给 Mac 开发的文档索引

> 版本：0.9.2 | 更新：2026-09-15

这份文档告诉你**该读哪些、按什么顺序读**。

---

## 一、先读这三个（30 分钟）

| 顺序 | 文档 | 作用 |
|---|---|---|
| 1 | **FOR-MAC.md** | 适配说明：连接共享、启动耗时、接口选择、重试策略 |
| 2 | **API.md** | 完整接口文档（31 个接口） |
| 3 | **openapi.json** | 机器可读规格，可导入 Postman / 自动生成 SDK |

**读完这三个就能开始写代码了。**

---

## 二、按需查阅

| 文档 | 什么时候看 |
|---|---|
| **CREDENTIALS.md** | 搞不清「要不要反复扫码」时 → **三层凭据说明** |
| **STARTUP-PERFORMANCE.md** | 调 UI 加载态、想知道为什么冷启动 5 秒 |
| **REPLAY.md** | 做录像回放功能 |
| **PTZ-VERIFICATION.md** | 做云台时 → 方向实测结论（含未解之谜） |
| **CONTROL-PROTOCOL.md** | 要搞定摄像头控制协议（cs2 P2P 命令表） |
| **cs2-handshake-capture.md** | 自己实现 cs2 握手时 → 抓包对照 |
| **COMMUNITY-ADAPT.md** | 想知道不同硬件上的差异 |
| **REPLY-PTZ-TO-MAC.md** | **需要你回复的 4 个问题**（云台符号约定） |

---

## 三、重点提醒（先说结论）

### 3.1 服务端已经改了架构（0.8.1+）

**连接共享**：每台摄像头一条 cs2 连接，多客户端共享。

~~~
每台设备一个 producer
  +-- 网页 HLS
  +-- Mac App
  +-- 抓拍
~~~

**你可以删掉这些 workaround：**
- 「连接被占用」检测
- 抓拍前先停流
- 云台前等工作流让出连接

实测：并发抓拍从 60 秒超时降到 1.6 秒。

### 3.2 启动耗时（别误判为失败）

~~~
HLS 首次    5.4-6.2s
HLS 二次    0.01-0.03s   （复用热连接）
抓拍         2.4s / 1.6s（热）
云台         0.2-0.6s
~~~

**UI 建议**：0s 转圈 → 2s「建立视频流」→ 5s 播放 → 12s 才提示失败。

**不要 1-2 秒就判失败。**

### 3.3 m3u8 分片路径（容易踩）

~~~
m3u8 里写的是：hls?did=X&seg=seg0.ts

交给播放器按标准相对路径解析即可，不要手工拼接。

错误拼法：.../api/live/seg?...   -> 404
~~~

### 3.4 凭据分三层（别混淆）

~~~
passToken      扫码登录拿到      长期（数月）    <- 用户只需关心这层
serviceToken   由 passToken 换取  短期，自动续期
设备 P2P 凭据   每台摄像头一份    >= 24 小时，自动续期
~~~

**客户端不需要自己刷凭据**，服务端全包了。

---

## 四、接口速查

### 4.1 启动流程

~~~
1. GET /api/health              确认服务在、拿版本
2. GET /api/dev/capabilities    拿能力与限制
3. GET /api/devices             拿设备列表
4. 按 capabilities 决定播放方式
~~~

### 4.2 常用接口

| 用途 | 接口 |
|---|---|
| 摄像头列表 | `GET /api/devices` |
| 家庭列表（含共享） | `GET /api/homes` |
| HLS 播放 | `GET /api/live/hls?did=&fps=15&w=0` |
| MJPEG 播放 | `GET /api/live/mjpeg?did=&fps=12&w=1280` |
| 抓拍 | `GET /api/live/snapshot?did=&w=0` |
| 云台查询 | `GET /api/ptz/status?did=` |
| 云台移动 | `POST /api/ptz/move` `{did,operation}` |
| 录像列表 | `GET /api/recordings/list?name=&date=` |
| 录像回放 | `GET /api/recordings/play?name=&file=&t=` |
| 录像缩略图 | `GET /api/recordings/thumb?name=&file=` |
| 存储总览 | `GET /api/storage` |
| 凭据状态 | `GET /api/accounts/status` |

### 4.3 错误重试

| 状态码 | 含义 | 动作 |
|---|---|---|
| 504 | 播放列表未就绪 | **2 秒后重试，最多 3 次** |
| 503 | 设备离线 | **不要重试** |
| 502 | 上游失败 | 看 msg |

~~~
auth failed  -> 3-5 秒后重试（服务端会自动续期）
throttle deny -> 等 30 分钟，别反复试
~~~

---

## 五、待你回复的问题

### 5.1 相位相关符号约定（重要）

我们的结论**正好相反**：

~~~
你:  op=2 -> 画面内容左移 240px -> 视野向右
我:  op=2 -> 画面内容右移  21px -> 视野向左
~~~

**必有一方写反了。** 建议加这个自检：

~~~python
shifted = np.roll(base, 25, axis=1)   # 内容右移 25px
assert phase_shift(base, shifted)[0] == +25, '符号反了！'
~~~

Swift 版本等价：构造已知位移的图，验证输出符号。

### 5.2 其他三个问题

1. **240px 是在多大分辨率上测的？**
2. **测 op=2 时的起始 angle 是多少？**（我这边 A/B 两次方向相反）
3. **CW500 的 MIoT `{x,y}` 读到过什么值？**

详见 **REPLY-PTZ-TO-MAC.md**。

---

## 六、云台方向的当前状态

### 已确定（三次运行一致）

| op | 实测 | 结论 |
|---|---|---|
| 1 | 位置不变，ret=-2 | 查询 |
| 3 | elevation +6，内容下移 | **上** |
| 4 | elevation -6，内容上移 | **下** |

**注意**：`op=3` 是「上」不是「右」—— 我们插件之前也标错了，已修。

### 未确定

| op | 情况 |
|---|---|
| 2 | 方向在两次运行中相反（+21 vs -32），标准差仅 1.5-2 |
| 5 | 不稳定（+44 / -147） |
| 6-10 | 无数据（测试中摄像头断连） |

**可能原因**：CW300 不是真机械云台（它不支持 MIoT `6.12`，CW500 支持）。

### 建议

1. **上/下 绑 op=3 / op=4**（已确认，可放心）
2. **左右留翻转开关**（别写死）
3. **用 CW500 定标准映射**（真机械云台 + MIoT 坐标可交叉验证）

---

## 七、自测命令

~~~bash
# 1. 服务在吗
curl http://<nas>:8900/api/health

# 2. 能力探测（含硬件编码器实测）
curl http://<nas>:8900/api/dev/capabilities | python3 -m json.tool

# 3. 设备列表
curl http://<nas>:8900/api/devices

# 4. HLS 播放列表（首次约 5 秒）
time curl 'http://<nas>:8900/api/live/hls?did=<did>&fps=15&w=0'

# 5. 再来一次（应该 0.01 秒）
time curl 'http://<nas>:8900/api/live/hls?did=<did>&fps=15&w=0'

# 6. 抓拍
curl -o snap.jpg 'http://<nas>:8900/api/live/snapshot?did=<did>&w=0'

# 7. 云台（op=3 = 上）
curl -X POST http://<nas>:8900/api/ptz/move \
  -H 'Content-Type: application/json' \
  -d '{"did":"<did>","operation":3}'
~~~

---

## 八、文档清单（本包内容）

~~~
MAC-HANDOFF.md               本文档（索引）
FOR-MAC.md                   ★ 适配说明（先读）
API.md                       ★ 接口文档
openapi.json                 ★ 机器可读规格
CREDENTIALS.md               三层凭据
STARTUP-PERFORMANCE.md       启动性能
REPLAY.md                    录像回放
PTZ-VERIFICATION.md          云台方向实测
REPLY-PTZ-TO-MAC.md          待回复的问题
CONTROL-PROTOCOL.md          设备控制协议
cs2-handshake-capture.md     cs2 握手抓包
COMMUNITY-ADAPT.md           硬件差异
~~~

**服务端随时可拉最新版：**

~~~bash
curl http://<nas>:8900/api/dev/docs      # Markdown（含全部章节）
curl http://<nas>:8900/api/dev/openapi   # OpenAPI JSON
~~~
