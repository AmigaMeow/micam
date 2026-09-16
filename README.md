# MiCam — 飞牛 fnOS 小米摄像头插件

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.21+-00ADD8.svg)](https://go.dev)
[![fnOS](https://img.shields.io/badge/fnOS-1.2.0401+-green.svg)](https://www.fnnas.com)
[![Release](https://img.shields.io/github/v/release/AmigaMeow/micam)](../../releases)

在飞牛 NAS 上接入小米/米家摄像头，做成一个完整的家庭监控方案。

**扫码登录 · 2K 实时预览 · 多路同屏 · 录像回放 · 云台控制 · 存储监控**

直连小米云 P2P（cs2 协议），不经过任何中转服务器。

---

## 功能

### 播放

| 功能 | 说明 |
|---|---|
| **HLS 2K 实时预览** | 2560×1440 原画，起播 5-6 秒，CPU 仅 94% |
| **多路预览** | 一次看所有摄像头，低帧率省 CPU |
| MJPEG 兼容模式 | HLS 不可用时自动降级 |
| 快照 | 单帧 JPEG，用于缩略图/告警 |

### 录像

| 功能 | 说明 |
|---|---|
| 分段录像 | `-c copy` 原画质，不转码 |
| **录像回放** | 自动转码成浏览器可播的 H.264 MP4 |
| **转码缓存** | 二次播放快 940 倍，支持随意拖动 |
| **时间轴缩略图** | 12 帧画面带，点击跳转 |
| **片段导出** | 剪一段转成 MP4，发给别人能直接播 |
| 批量删除 / 自动清理 | 存储策略可配 |

### 控制与管理

| 功能 | 说明 |
|---|---|
| 云台控制 | 上下左右 + 位置查询（方向已实测标定） |
| **存储监控** | 磁盘、占用、各摄像头明细、可录时长估算 |
| **事件通知** | Webhook（钉钉/企业微信/Bark）+ 9 种事件 |
| 多账号管理 | 扫码登录、凭据备份、多账号筛选 |
| **凭据轮换** | 保留 5 组历史凭据，大幅降低云端调用 |

### 开发与诊断

| 功能 | 说明 |
|---|---|
| **开放模式** | 连点版本号 7 次解锁（同 Android 开发者选项） |
| **完整 API 文档** | 内置 OpenAPI 3.0，51 个接口 |
| **硬件自适应** | 自动探测可用编码器（QSV/NVENC/VAAPI/AMF） |
| 诊断包 | 一键导出环境信息（自动脱敏），便于提交适配 |

---

## 快速开始

### 安装

1. 从 [Releases](../../releases) 下载 `micam.fpk`
2. 飞牛 → **应用中心** → **手动安装** → 选择 fpk
3. 打开「小米摄像头」→ 用**米家 App 扫码登录**

> 支持 x86_64，fnOS 1.2.0401+

**扫码一次就够了** —— 登录凭据长期有效，不需要反复扫。
（设备凭据会自动轮换，详见 [凭据说明](docs/CREDENTIALS.md)）

### 从源码构建

```bash
git clone https://github.com/AmigaMeow/micam.git
cd micam

# 需要 Go 1.21+ 和 fnpack
bash build/build-fnos.sh
# 产物在 dist/
```

---

## 性能实测

硬件：Intel Celeron J4125（4 核 2.0GHz，UHD 600 核显）

### 起播速度

| 场景 | 耗时 |
|---|---|
| HLS 首次（冷连接） | **5.4-6.2 秒** |
| HLS 二次（热连接） | **0.01-0.03 秒** |
| 抓拍 | 2.4 秒 / 热 1.6 秒 |
| 云台操作 | 0.2-0.6 秒 |

### CPU 占用

| 场景 | CPU |
|---|---|
| 空闲（无观看） | ~0% |
| HLS 2K 播放 | 90-150% |
| 多路预览（2 路 @3fps） | ~130% |
| MJPEG 2K | 300-350% |

**为什么 HLS 比 MJPEG 省 3.5 倍**：
MJPEG 逐帧独立压缩，2K 下数据量放大 27 倍；HLS 有帧间预测。

详见 [启动性能优化](docs/STARTUP-PERFORMANCE.md)。

---

## 架构

```
摄像头 --cs2(TCP)--> go2rtc Producer（共享连接）
                          |
                          +-- consumer A --> ffmpeg --> HLS (2K 实时预览)
                          +-- consumer B --> ffmpeg --> MJPEG (兼容模式)
                          +-- consumer C --> ffmpeg --> JPEG (快照)
                          +-- consumer D --> ffmpeg --> MKV  (录像，-c copy)
```

### 关键设计

**1. 连接共享**

每台摄像头只维护**一条** cs2 连接，多消费者共享：

```
网页 + Mac App + 抓拍 + 录像 可以同时工作
最后一个消费者离开后连接保持 25 秒（二次观看秒开）
```

**为什么重要**：cs2 协议同一设备只允许一路连接。
早期「独占」模式下，第二个请求要等 60 秒 —— 实测踩过这个坑。

**2. 凭据轮换**

每台设备保留最近 5 组凭据，逐个尝试；只有全部失效才向小米云申请。

```
好处：
  - 旧凭据往往还能用 -> 云端调用大幅减少
  - 小米云限流期间仍有可用凭据
  - 实测：限流 21 小时期间靠缓存凭据仍能正常播放
```

**3. 硬件自适应**

自动探测可用编码器，**实测**而不是只看 `ffmpeg -encoders`：

```
候选：h264_qsv -> h264_nvenc -> h264_vaapi -> h264_amf -> libx264
每个都真编 1 秒视频，检查真的产出了数据
失败自动降级，并记录原因
```

**为什么需要实测**：服务进程可能拿不到 `/dev/dri` 权限，
`ffmpeg -encoders` 说支持但实际报 `Failed to create a VAAPI device`。

**4. go2rtc 编译期内嵌**

无外部依赖，不与其他 go2rtc 实例冲突。

---

## 支持的设备

| 型号 | model | 状态 |
|---|---|---|
| 小米室外摄像机 CW300 | `mxiang.camera.moc001` | ✅ 已验证 |
| 小米室外摄像机 CW500 双摄版 | `isa.camera.hlmax` | ✅ 已验证 |
| 小米室外摄像机 CW700S | `isa.camera.hlc7` | 未测试 |
| 米家摄像头系列 | `chuangmi.camera.*` | 未测试 |

**共享家庭的设备也支持**（会标「共享」标签）。

> 其他型号应该也能用（cs2 协议通用），但需要实测。
> 如果你试过，欢迎提交 [Issues](../../issues) 告诉我们结果。

---

## API

完整的 OpenAPI 3.0 规格（**51 个接口**）：

```bash
# 插件内置（推荐，始终最新）
curl http://<nas>:8900/api/dev/openapi   # OpenAPI JSON
curl http://<nas>:8900/api/dev/docs      # Markdown 文档
```

也可以直接看 [docs/openapi.json](docs/openapi.json)。

### 常用接口

| 用途 | 接口 |
|---|---|
| 健康检查 | `GET /api/health` |
| 摄像头列表 | `GET /api/devices` |
| HLS 播放 | `GET /api/live/hls?did=&fps=15&w=0` |
| MJPEG 播放 | `GET /api/live/mjpeg?did=&fps=12&w=1280` |
| 抓拍 | `GET /api/live/snapshot?did=&w=0` |
| 云台移动 | `POST /api/ptz/move` `{did,operation}` |
| 存储总览 | `GET /api/storage` |
| 录像回放 | `GET /api/recordings/play?name=&file=&t=` |

---

## 硬件适配

不同 NAS 的 CPU / 核显差异很大。插件会**自动探测**，但可能遇到特例。

### 遇到问题怎么办

1. 打开插件 → **系统设置** → **导出诊断包**
2. 诊断包含编码器实测结果（**所有敏感信息已自动脱敏**）
3. 提交到 [Issues](../../issues)，我们会针对性适配

标题格式：

```
[硬件适配] <CPU型号> + <核显型号>
```

详见 [社区适配指南](docs/COMMUNITY-ADAPT.md)。

---

## 文档

### 使用

- [安装/升级/卸载](docs/INSTALL-GUIDE.md)
- [凭据说明](docs/CREDENTIALS.md) —— 三层凭据，扫码一次就够
- [录像回放](docs/REPLAY.md)
- [移动端适配](docs/MOBILE.md)

### 技术

- [开发笔记](docs/DEV-NOTES.md) —— cs2 直连踩坑全记录
- [启动性能优化](docs/STARTUP-PERFORMANCE.md) —— 实测数据
- [cs2 协议抓包](docs/cs2-handshake-capture.md) —— 完整握手时序
- [云台协议](docs/xiaomi-ptz-confirmed.md) —— 方向实测标定
- [凭据与限流](docs/CREDENTIAL-LIMITS.md) —— 限流应对实战

### 开发

- [贡献指南](docs/CONTRIBUTING.md)
- [社区适配指南](docs/COMMUNITY-ADAPT.md)
- [开放模式说明](docs/OPEN-MODE.md)
- [API 文档](docs/API.md)

---

## 已知限制

| 限制 | 说明 |
|---|---|
| 同一摄像头只能一路连接 | cs2 协议限制；插件用连接共享缓解 |
| 小米云接口有限流 | `miss_get_vendor` 调用过频会被封（实测可达 24 小时）；插件用凭据轮换 + 指数退避规避 |
| 录像需转码才能回放 | 摄像头输出 HEVC，浏览器不支持；用转码缓存加速 |
| 画面延迟 2-5 秒 | HLS 固有 |

---

## 致谢

- [go2rtc](https://github.com/AlexxIT/go2rtc) —— cs2 协议实现（MIT）

---

## 免责声明

本项目仅供个人学习与自用。使用前请确认符合小米的服务条款。

摄像头 P2P 连接同一设备通常只允许一路，与米家 App 同时使用可能冲突。

---

## License

MIT
