# Mac 客户端 vs NAS 插件：职责边界

> 给用户看的沟通要点 + 给 Mac 开发的技术契约

---

## 一、一句话结论

```
Mac 端：用原生做（SwiftUI / AppKit）
NAS 端：提供 API（谁都能调）
两边共享：API 契约 + 数据模型   ← 只共享这个，不共享 UI
```

**不要试图把网页版 UI 搬到 Mac 上。**

---

## 二、为什么应该原生

| 维度 | 网页套壳 | 原生（推荐） |
|---|---|---|
| 启动速度 | 要起 WebView | 秒开 |
| 视频解码 | 靠浏览器 | **VideoToolbox 硬解**（Mac 的 GPU 很强） |
| 多路同屏 | 浏览器 4 路就吃力 | 原生可开 9-16 路 |
| 菜单栏 / 小组件 | 做不到 | 原生支持 |
| 通知 | 要绕 | 原生 UNUserNotification |
| 手感 | 一眼看出是网页 | 系统级体验 |
| 包体积 | 大 | 小 |

**关键**：Mac 的 VideoToolbox 能硬解 H.264/HEVC，
比浏览器软解省 5-10 倍电。

---

## 三、边界：什么共享、什么各自做

### 3.1 共享（必须一致）

| 项 | 说明 |
|---|---|
| **API 契约** | 端点、参数名、响应结构 —— 内容见 `docs/API.md` / `docs/openapi.json` |
| **数据模型** | 设备、事件、通道（lens）、别名 —— 语义要一致 |
| **参数约定** | 如 `channel=2` 表示第二镜头 |
| **性能预期** | 冷启动 5-6 秒、热连接 0.01 秒 —— 别误判为失败 |
| **错误处理** | 504=重试、503=离线不重试、throttle=退避 |

### 3.2 各自做（不要互相抄）

| 项 | NAS 网页版 | Mac 原生 |
|---|---|---|
| **配色** | Haven 暗色令牌 | macOS 系统色（或自己的） |
| **布局** | 顶栏导航 | macOS 侧边栏 / Toolbar |
| **控件** | 我们的 CSS 组件 | SwiftUI 原生控件 |
| **弹窗** | 我们的 dialog | Sheet / NSAlert |
| **图标** | Lucide | SF Symbols |
| **导航** | 顶栏 tab | NavigationSplitView |

**一句话**：Mac 端用 macOS 的方式实现**同样的功能**，而不是**同样的外观**。

---

## 四、Mac 端建议的信息架构

按 macOS 习惯（对应网页版的顶栏）：

```
┌──────────┬────────────────────────────────┐
│ Sidebar  │  Toolbar（搜索/通知/账号）      │
│          ├────────────────────────────────┤
│ 首页      │                                │
│ 摄像头    │         内容区                  │
│ 事件 ①    │                                │
│ 回放      │                                │
│ AI 智能   │                                │
│ 存储      │                                │
│ ─────    │                                │
│ 设置      │                                │
└──────────┴────────────────────────────────┘
```

**视觉参考**：你的 Haven 效果图可以直接用；
但控件形态改成 macOS 的（侧边栏、工具栏、Sheet）。

---

## 五、给 Mac 的技术要点

### 5.1 必读文档

```
docs/API.md              完整接口（51+ 个）
docs/openapi.json        机器可读规格
docs/FOR-MAC.md          适配说明
docs/STARTUP-PERFORMANCE.md  启动耗时（别误判）
docs/CREDENTIALS.md      三层凭据（客户端不用管刷新）
docs/REPLY-TO-MAC-V3.md  本次回复
```

**在线获取（永远最新）**：

```bash
curl http://192.168.2.100:8900/api/dev/docs      # Markdown
curl http://192.168.2.100:8900/api/dev/openapi   # OpenAPI
```

### 5.2 关键约束（我们踩过的坑，请转告）

**① 不要用 CDN**
```
国内 CDN 不可靠。所有依赖本地内置。
我们因为 hls.js 走 CDN 导致过一次「播放全黑」。
```

**② 别用浏览器内核判断 HLS 支持**
```swift
// ❌ 错误（Edge/Chrome 会说 support，但实际不支持）
if video.canPlayType("application/vnd.apple.mpegurl") { ... }
```

Mac 原生用 **AVPlayer 直接播 HLS** 就行，没这个问题。
但如果你用 WKWebView 嵌网页，会遇到同样的坑。

**③ 冷启动 5-6 秒是正常的**
```
CS2 握手 0.3s + 首帧 2.3s + ffmpeg 1.0s ≈ 3.6s 下限
UI 建议：0s 转圈 → 2s「建立视频流」→ 5s 播放 → 12s 才提示失败
```

**④ 连接共享，不用做占用检测**
```
NAS 侧每台摄像头一条连接，多客户端共享
你直接开流，不会踢掉别的客户端
停止时断开 HTTP 连接即可（25 秒后自动回收）
```

**⑤ 云台同一时刻只能一个操作**
```
云台和视频共用 P2P 通道
不要并发发多条云台指令
```

### 5.3 建议的实现顺序

```
阶段 1：设备列表 + 单路实时预览（AVPlayer 播 HLS）
阶段 2：多路同屏（VideoToolbox 硬解）
阶段 3：录像回放（用 /api/recordings/play）
阶段 4：事件列表（用 /api/events）
阶段 5：云台控制（等绝对定位确认）
阶段 6：菜单栏 / 桌面小组件
```

---

## 六、你可以直接这样跟他说

```
「Mac 端走原生，别套网页 —— 网页那套是给 NAS 面板用的。

  但 API 契约两边一致，你直接调 NAS 的接口：
    http://192.168.2.100:8900/api/...
  在线文档：
    curl http://192.168.2.100:8900/api/dev/docs

  你要复用的是「接口 + 数据模型」，不是「外观」。
  界面按 macOS 的习惯做（侧边栏/工具栏/Sheet/SF Symbols），
  视觉风格可以沿用 Haven 那套配色。

  三件事先说清楚：
  1. channel 参数早就实现了，你测的是旧版；
     旧缓存要强刷一次（Ctrl+Shift+R）。
  2. op=13 绝对定位我这边没测出来（参数被忽略），
     麻烦把你参考实现的**完整 payload 抓包**发我。
  3. 云台同一时刻只能一个操作，别并发发指令。」
```

---

## 七、附：两边接口对照

| 功能 | 接口 | Mac 端用什么 |
|---|---|---|
| 设备列表 | `GET /api/devices` | 侧边栏列表 |
| 首页聚合 | `GET /api/home` | 首页 |
| 实时预览 | `GET /api/live/hls?did=` | **AVPlayer** |
| 第二镜头 | `...&channel=2` | AVPlayer |
| 缩略图 | `GET /api/live/thumb?did=` | 卡片预览 |
| 抓拍 | `GET /api/live/snapshot?did=` | 截图 |
| 事件 | `GET /api/events` | 事件列表 |
| 事件缩略图 | `GET /api/events/thumb?f=` | 事件行 |
| 录像回放 | `GET /api/recordings/play` | AVPlayer |
| 云台 | `POST /api/ptz/move` | 方向键 |
| 别名 | `GET/POST /api/devices/meta` | 设置页 |
| 存储 | `GET /api/storage` | 存储页 |
