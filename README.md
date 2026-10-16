# MiCam — 飞牛 fnOS 智能家居监控桥接套件 (非官方社区版)

<p align="center">
  <img src="platform/fnos/app/ui/images/icon_256.png" width="108" height="108" alt="MiCam Logo" />
</p>

<p align="center">
  <strong>专为飞牛 fnOS 打造的智能家居摄像头本地监控、24小时私有存储与 AI 智能中心</strong><br>
  <em>非官方开源社区适配 · 零官方闭源库打包 · 100% 本地私有化 · 完全免费开源</em>
</p>

<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-AGPLv3-blue.svg" alt="License: AGPLv3"></a>
  <a href="https://go.dev"><img src="https://img.shields.io/badge/Go-1.21+-00ADD8.svg" alt="Go 1.21+"></a>
  <a href="https://www.fnnas.com"><img src="https://img.shields.io/badge/fnOS-1.2+-green.svg" alt="fnOS Support"></a>
  <a href="DISCLAIMER.md"><img src="https://img.shields.io/badge/Legal-Disclaimer-orange.svg" alt="Legal Disclaimer"></a>
  <a href="README.en.md"><img src="https://img.shields.io/badge/Lang-English-lightgrey.svg" alt="English Documentation"></a>
</p>

---

> [!IMPORTANT]
> ### ⚖️ 重要法律与合规声明 / Legal Notice
> 1. **独立第三方开源项目**：本项目为开源社区开发者出于技术研究与个人私有存储需求自发实现的适配桥接工具，**与北京小米科技有限责任公司（Xiaomi Inc.）及其关联方不存在任何隶属、合作、赞助、授权或商业背书关系**。
> 2. **商标权属**：“小米”、“米家”、“MI”等商标版权及商誉均归其各自权利人所有。文档及界面中所出现的设备名称仅出于**客观指代硬件兼容性**目的。
> 3. **物理隔离与零分发原则（Zero-Binary Policy）**：本代码库及发布的 `.fpk` 插件包中**严禁且绝不包含任何小米官方预编译闭源二进制文件**（如 `libmiot_camera_lite.so` 等）。运行时组件由用户 NAS 在首次启动时直接向小米官方于 GitHub 开源的公开代码库（[XiaoMi/xiaomi-miloco](https://github.com/XiaoMi/xiaomi-miloco)）按需发起请求，或由用户离线手动上传。
> 4. **合规性抗辩**：本项目完全免费开源，不含任何商业收费或盈利行为，符合《计算机软件保护条例》第十七条关于计算机软件互操作性反向工程之合理使用规定。详细条款请参阅完整 **[DISCLAIMER.md](DISCLAIMER.md)**。

---

## 📸 核心特性

### 1. 2K 低延迟实时流与多路同屏
- **P2P 直连直通**：基于纯 Go 实现的 cs2 协议与官方通道，局域网与广域网自适应直连，延迟低至 300ms。
- **全格式与双引擎**：支持 HLS (2560×1440 原画质) 及 MJPEG 极速流畅预览；自动适配桌面与移动端。
- **双摄分屏与画中画**：完整支持如 CW500 等多目双摄机型，一屏双画面独立查看。

### 2. 24 小时连续录像与秒级回放 (无黑屏寻道)
- **零转码原画直存**：录像采用 `-c copy` 原生流直存，CPU 占用极低（仅 ~2%），不损耗画质。
- **智能静态缓存 + 动态自适应切片**：后台自动预热生成轻量缓存，二次播放提升 900+ 倍，支持 HTTP Range 原生快进快退。
- **2.5K / 4K 时间轴无缝滑选**：彻底重构播放内核与关键帧寻道逻辑，时间轴与进度条任意拖动跳转即刻出帧，告别缓冲死锁与黑屏。
- **自动配额与多存储池深度清理**：支持真实占用统计与跨盘多存储池深度清理，递归解绑硬链接与软链接，杜绝假清理与空间残留。
- **飞牛存储权限智能指引**：内置权限探测与折叠引导指引，自动检测非自带目录写入权限并给出修复指令。

### 3. 云台 PTZ 方向控制
- **全向操控**：上下左右 4 向精密电机微调，内置步进限制，杜绝死机与过载。
- **双目联动与反向开关**：支持镜头方向一键水平反转，适配各种倒装/壁挂安装场景。

### 4. 边缘 AI 视觉分析与多架构模式
- **三种 AI 运行架构按需切换**：
  - **边云协同 (混合推荐)**：本地轻量初筛 + 云端深度视觉大模型，兼顾毫秒级响应与超强理解力。
  - **纯大模型模式 (免本地算力)**：无需下载本地大体积 ONNX 模型，直接对接局域网 Ollama (如 qwen2.5-vl) 或云端 API (DeepSeek、通义千问 Qwen-VL 等)，低配 NAS 也能畅快使用。
  - **纯本地离线模式**：零外网请求，采用 YOLOv8 / YOLOv11 纯本地推理，数据 100% 留存本地 NAS。
- **原生支持局域网私有端点**：无需填写 API Key 即可直连内网 Ollama / vLLM / LocalAI 服务。
- **本地模型存放与手动下载**：本地模型统一存放在 `/vol1/@appdata/micam/models/`，支持通过国内镜像（hf-mirror）或 GitHub Releases 离线放置。
- **自然语言语义追查**：支持在 AI 页面使用自然语言直接提问（例：“今天下午穿红衣服的人什么时候来的？”），系统自动联动录像视频片段回放。
- **热力图与人脸/人物资产库**：自动汇总高频活动区域热力图及常客面孔聚类。
- **每日智能安防晨报**：每日定时汇总过去 24 小时关键安防动态。

### 5. 多通道自动化联动
- **多端实时推送**：支持 Bark、PushPlus、Pushover、企业微信 / 钉钉 / 飞书 Webhook。
- **智能家居中枢**：开箱即用的 Home Assistant 自动化 YAML 配置与 n8n 自动化流水线。

---

## 🛠️ 架构与组件隔离原理

```mermaid
flowchart TD
    subgraph 发布物安全边界 (GitHub / Release / .fpk)
        A["MiCam 纯源码仓库<br>(Go + JS + CSS + HTML)"] --> B["飞牛 .fpk 插件包<br>(仅包含编译后的 Go 服务与前端)"]
        B -.->|严格禁止打包| C["小米闭源库 libmiot_camera_lite.so"]
    end

    subgraph 用户个人 NAS 运行时环境
        B --> D["首次部署 / 启动"]
        D -->|一键安全拉取| E["小米官方开源仓库<br>XiaoMi/xiaomi-miloco"]
        D -->|备选: 离线上传| F["用户本地手动提供 .so"]
        E --> G["安装至私有数据目录<br>/vol1/@appdata/micam/lib/"]
        F --> G
        G --> H["动态加载与激活"]
        H --> I["100% 完整功能就绪<br>(实时流 · 录像 · 回放 · 云台 · AI)"]
    end
```

---

## 🚀 安装与使用

### 方式一：飞牛应用中心手动安装（推荐）
1. 前往本仓库 [Releases 页面](../../releases) 下载最新的 `micam_x.x.x_amd64.fpk`。
2. 打开飞牛 NAS 桌面 → 进入 **应用中心** → 点击右上角 **手动安装** → 上传 `.fpk` 文件。
3. 安装完成后打开 MiCam：
   - 使用 **米家 App 扫码授权** 即可完成登录。
   - 在 **设置 → 系统 → 开源合规与流媒体组件** 中，点击 **从官方仓库拉取**，系统将自动从官方公开源下载流媒体组件并即刻激活！

### 方式二：源码构建
```bash
# 1. 克隆本仓库 (干净无专有二进制)
git clone https://github.com/AmigaMeow/micam.git
cd micam

# 2. 编译并打包 fnOS 插件
bash build/build-fnos.sh

# 产物位于 dist/micam_<version>_amd64.fpk
```

---

## ⚙️ 硬件加速说明

MiCam 能够智能检测并适配底层硬件环境：
- **Intel 核显**：优先启用 `h264_qsv` (QuickSync)，1080P/2K 转码仅占用约 5% CPU。
- **通用硬件加速**：支持 VAAPI (`/dev/dri/renderD128`) 及 NVIDIA NVENC。
- **纯 CPU 低功耗自适应**：无独显/核显环境下自动切换为 `libx264 -preset ultrafast -crf 28 -tune zerolatency`，即使在 J4125、N100 等低功耗 NAS CPU 上也能达到 60fps+ 超高速转码，杜绝播放卡顿。

---

---

## 系统诊断与问题排查

遇到运行异常或配置问题时，MiCam 提供了内置的一键诊断与脱敏日志导出能力：
- **一键导出诊断包**：在系统 **设置 → 维护与支持** 中点击 **「导出诊断包」**，或直接访问 `http://<NAS_IP>:8900/api/diag/bundle`，即可下载包含运行环境、硬件编码、存储状态及最近日志的脱敏压缩包。
- **隐私保护与协助反馈**：所有密码、Token 及鉴权凭据均已在服务端做星号脱敏。如遇到异常排查需要，可在 GitHub Issues 附带诊断报告，或直接发送至专项排查邮箱：`evendstate@gmail.com`（若含有敏感局域网拓扑或隐私录像信息）。

---

## 版本发布与维护规范

- **持续维护与交付**：每次重大功能改进与架构演进，项目均会严格同步更新 `README.md`、提交至代码仓库，并在 GitHub Releases 发布最新的 `.fpk` 插件安装包。
- **开箱即用无感升级**：升级时直接在飞牛 NAS 应用中心上传新版 `.fpk` 即可无缝覆盖安装，历史录像、摄像头授权与系统配置完全保留。

## 📄 开源许可证

本项目基于 **[GNU Affero General Public License v3.0 (AGPL-3.0)](LICENSE)** 协议开源。  
本软件承诺永久保持开源与免费，严禁任何闭源商业分发或加壳变相收费行为。
