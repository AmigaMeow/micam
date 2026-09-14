# MiCam — 飞牛 fnOS 小米摄像头插件

在飞牛 NAS 上接入小米/米家摄像头：扫码登录、实时预览、录像、云台控制。

**不依赖 EasyNVR**，直连小米云 P2P（cs2 协议）。

## 功能

| 功能 | 状态 |
|---|---|
| 扫码登录（米家 App） | ✅ |
| 设备发现（含**共享家庭**） | ✅ |
| 实时预览（MJPEG） | ✅ 首帧 ~0.7s |
| 快照 | ✅ 3-6s |
| 录像（分段 MKV，原画质） | ✅ |
| 云台控制（PTZ） | ✅ |
| Web 界面 | ✅ |

## 快速开始

### 安装

1. 下载 `micam.fpk`
2. 飞牛 → 应用中心 → 手动安装
3. 打开「小米摄像头」→ 扫码登录

### 从源码构建

```bash
# 依赖 Go 1.21+
git clone https://github.com/<your-org>/micam.git
cd micam

# 构建 fpk
bash build/build-fnos.sh

# 产物在 dist/
```

## 架构

```
摄像头 --cs2(TCP)--> go2rtc Producer --> 内存管道 --> ffmpeg --> HTTP
                          |
                          +--> 录像（-c copy 不转码）
                          |
                          +--> 云台（0x112 电机指令）
```

## API

| 接口 | 说明 |
|---|---|
| `GET /api/health` | 健康检查 |
| `GET /api/accounts` | 账号列表 |
| `POST /api/accounts/qr` | 发起扫码登录 |
| `GET /api/accounts/qr/status?id=` | 轮询扫码状态 |
| `GET /api/devices` | 设备列表 |
| `GET /api/live/snapshot?did=&w=` | 单帧 JPEG |
| `GET /api/live/mjpeg?did=&fps=&w=` | MJPEG 流 |
| `GET /api/ptz/status?did=` | 查询云台角度 |
| `POST /api/ptz/move` | 云台移动 |
| `GET /api/recordings` | 录像状态 |
| `POST /api/recordings` | 开始录像 |
| `POST /api/recordings/stop` | 停止录像 |
| `GET /api/recordings/files?name=` | 录像文件列表 |
| `POST /api/recordings/cleanup` | 清理过期录像 |

## 文档

- [开发笔记](docs/DEV-NOTES.md) — cs2 直连踩坑全记录
- [cs2 协议抓包](docs/cs2-handshake-capture.md) — 完整握手时序 + 原始字节
- [云台协议](docs/xiaomi-ptz-protocol.md) — PTZ 命令格式
- [设计规范](docs/easynvr-design-spec.md) — 前端设计参考

## 致谢

- [go2rtc](https://github.com/AlexxIT/go2rtc) — cs2 协议实现（MIT）
- 小米云 API 为公开接口，本项目未使用任何私有 API

## 免责声明

本项目仅供个人学习与自用。使用前请确认符合小米的服务条款。
摄像头 P2P 连接同一设备通常只允许一路，与米家 App 同时使用可能冲突。

## License

MIT
