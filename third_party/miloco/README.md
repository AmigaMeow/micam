# libmiot_camera_lite（小米官方 PPCS 摄像头库）

来源：[XiaoMi/xiaomi-miloco](https://github.com/XiaoMi/xiaomi-miloco)
路径：`backend/miot/src/miot/libs/linux/x86_64/libmiot_camera_lite.so`

**小米官方预编译的闭源库**，用于建立 PPCS P2P 连接拉取摄像头码流。
它**不经过** `miss_get_vendor`，因此不受该端点的限流影响。

## 为什么不提交进仓库

- 8.6 MB 二进制
- 版权属于小米（Miloco License Agreement）
- 用 `scripts/fetch-miot-lib.sh` 按需下载

## 架构

| 平台 | 架构 | 文件名 | 本机可用 |
|---|---|---|---|
| Linux | x86_64 | `libmiot_camera_lite.so` | ✅ NAS 正是此架构 |
| Linux | arm64 | `libmiot_camera_lite.so` | — |
| macOS | x86_64 / arm64 | `.dylib` | — |
