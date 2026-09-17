# 文档索引

> 40 个文档按主题归类，另有 6 个历史版本文档在 [`_archive/`](_archive/)。新人从「入门」开始看。

---

## 入门

| 文档 | 说明 |
|---|---|
| [INSTALL-GUIDE.md](INSTALL-GUIDE.md) | 安装指南 |
| [DESIGN.md](DESIGN.md) | 整体设计 |
| [PAGES-SPEC.md](PAGES-SPEC.md) | **页面·功能·设置规格书**（出效果图的依据，42KB） |
| [ROADMAP.md](ROADMAP.md) | 路线图 |
| [CONTRIBUTING.md](CONTRIBUTING.md) | 贡献指南 |
| [COMMUNITY-ADAPT.md](COMMUNITY-ADAPT.md) | 社区适配 |

---

## 小米协议（逆向）

| 文档 | 说明 |
|---|---|
| [CONTROL-PROTOCOL.md](CONTROL-PROTOCOL.md) | cs2 P2P 控制协议 |
| [cs2-handshake-capture.md](cs2-handshake-capture.md) | 握手抓包记录 |
| [xiaomi-ptz-confirmed.md](xiaomi-ptz-confirmed.md) | 云台协议（实测确认，**当前有效**） |
| [REPLY-PTZ-TO-MAC.md](REPLY-PTZ-TO-MAC.md) | 回复 Mac 的云台问题 |

**注意**：云台协议的初版与修订版（`xiaomi-ptz-protocol.md`、`xiaomi-ptz-protocol-v2.md`）
已移入 [`_archive/`](_archive/)，看 [xiaomi-ptz-confirmed.md](xiaomi-ptz-confirmed.md) 即可。

---

## 账号与凭据

| 文档 | 说明 |
|---|---|
| [CREDENTIALS.md](CREDENTIALS.md) | 三层凭据机制 |
| [CREDENTIAL-LIMITS.md](CREDENTIAL-LIMITS.md) | 凭据限制与失效场景 |

---

## 录像与播放

| 文档 | 说明 |
|---|---|
| [REPLAY.md](REPLAY.md) | 回放实现 |
| [PERFORMANCE.md](PERFORMANCE.md) | 性能数据 |
| [STARTUP-PERFORMANCE.md](STARTUP-PERFORMANCE.md) | 启动性能 |
| [MOBILE.md](MOBILE.md) | 移动端适配 |
| NVR 对接（早期草稿） | 未实施，已移入 [`_archive/easynvr-design-spec.md`](_archive/easynvr-design-spec.md) |

---

## AI 能力（重点）

### 可行性调研
| 文档 | 说明 |
|---|---|
| [AI-FEASIBILITY.md](AI-FEASIBILITY.md) | 整体可行性 |
| [AI-CAPABILITY-TIERS.md](AI-CAPABILITY-TIERS.md) | **能力分级**（按真实能力裁定 8 类功能） |
| [AI-MODELS.md](AI-MODELS.md) | 可用模型清单 |
| [MODEL-LIMITS.md](MODEL-LIMITS.md) | **模型边界**（从一次车牌误读说起） |

### 落地设计
| 文档 | 说明 |
|---|---|
| [AI-INVESTIGATION.md](AI-INVESTIGATION.md) | **AI 调查任务**设计（三种任务类型 + 成本控制） |
| [AI-PAGE-IMPL.md](AI-PAGE-IMPL.md) | AI 智能页实现清单 |
| [AI-SETTINGS-IMPL.md](AI-SETTINGS-IMPL.md) | AI 设置页实现清单 |
| [P0-3-IMPL.md](P0-3-IMPL.md) | 事件中心增强实现清单 |
| [DEEPSEEK-INTEGRATION.md](DEEPSEEK-INTEGRATION.md) | **DeepSeek 接入实测**（含 3 个技术坑） |

### 实测记录（重要）
| 文档 | 说明 |
|---|---|
| [AI-CHANGE-DETECTION-TEST.md](AI-CHANGE-DETECTION-TEST.md) | **变化检测实测**（合成样本无效 + 车牌不可靠） |
| [PHASE2-LPR.md](PHASE2-LPR.md) | **本地车牌识别实测**（HyperLPR3，含 2 个部署坑） |
| [PHASE3-POSE-BASELINE.md](PHASE3-POSE-BASELINE.md) | 机位一致性 + 基线测试框架 |
| [EVENT-CAPABILITY.md](EVENT-CAPABILITY.md) | 事件能力边界 |

---

## 开放接口

| 文档 | 说明 |
|---|---|
| [API.md](API.md) | API 文档 |
| [OPEN-MODE.md](OPEN-MODE.md) | 开放模式 |
| [openapi.json](openapi.json) | OpenAPI 3.0 规格 |

### 第三方客户端对接（历史版本）

`REPLY-TO-MAC*.md` / `reply-to-mac.md` / `MAC-*.md` / `FOR-MAC.md` 共 7 个，
是给 Mac 客户端作者的多轮回复。看最新的 `REPLY-TO-MAC-V4.md`。

---

## 开发记录

| 文档 | 说明 |
|---|---|
| [DEV-NOTES.md](DEV-NOTES.md) | 开发笔记（19KB，踩坑记录） |
| [CODE-REVIEW.md](CODE-REVIEW.md) | **代码审查报告**（规模 / 安全 / 质量观察，5 项质量任务的出处） |
| [QUALITY-TASKS-PROMPT.md](QUALITY-TASKS-PROMPT.md) | 质量改进任务的独立提示词（可整段贴到新窗口执行） |

---

## 历史归档

早期版本与已废弃的草稿统一放在 [`_archive/`](_archive/)，主目录只留当前有效文档 ——
**历史版本见 `_archive/`**。

| 归档文档 | 为什么归档 |
|---|---|
| reply-to-mac.md / REPLY-TO-MAC-V2.md / REPLY-TO-MAC-V3.md | 已被 [REPLY-TO-MAC-V4.md](REPLY-TO-MAC-V4.md) 取代 |
| xiaomi-ptz-protocol.md / xiaomi-ptz-protocol-v2.md | 已被 [xiaomi-ptz-confirmed.md](xiaomi-ptz-confirmed.md) 取代 |
| easynvr-design-spec.md | NVR 对接早期草稿，未实施 |

仍可合并的候选：AI-FEASIBILITY / AI-MODELS / MODEL-LIMITS 可合成「AI 能力与边界」，
但内容都还有效，暂不归档。
