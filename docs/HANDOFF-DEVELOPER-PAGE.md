# 移交：开发者页 + 容器类确认 + 发版

> 复制「--- 提示词开始 ---」到「--- 提示词结束 ---」之间的内容到新窗口。

--- 提示词开始 ---

# 任务：MiCam 三件事（开发者页 / 容器类确认 / 发版）

## 项目背景

MiCam 是**飞牛 NAS（fnOS）的小米摄像头插件**，开源在 `https://github.com/AmigaMeow/micam`。

```
代码位置   /root/Deepseek/micam/
分支       master（公开仓库）
当前版本   0.39.0
插件地址   http://192.168.2.100:8900/   （无鉴权，总是最新）
运行用户   micam
```

### 架构

```
后端   Go（cmd/micamd/ + internal/），单二进制，内置 HTTP（8900）
       CGO_ENABLED=0
前端   单文件 JS：platform/fnos/app/www/app.js
       组装进 index.html（**无构建步骤**是刻意选择：
       飞牛网关会重写相对路径，所以 CSS/JS 全部内联）
       /tmp/final-js.txt 是它的软链接（兼容旧工作流）
AI     本地 YOLO（onnxruntime）+ 本地 LPR（HyperLPR3）
       + 云端视觉大模型（DeepSeek）
```

### 硬约束

```
1. 不要引入构建步骤（webpack/vite/tsc）
2. 不要引入 cgo
3. 不要引入第三方 CSS 框架
   教训：曾引入 Basecoat -> 17 个类名冲突、弹窗透明、开关不可见
4. 不要提交密钥/凭据
```

---

## 环境与工作流（必须先看）

### 编译

```bash
export PATH=/usr/local/go/bin:$PATH
export GOPROXY=https://goproxy.cn,direct
cd /root/Deepseek/micam
go build -o /tmp/micamd ./cmd/micamd
gofmt -w cmd/micamd/*.go internal/*/*.go
```

### 前端改动流程

```bash
# 1) 编辑 platform/fnos/app/www/app.js（前端唯一源文件）
# 2) 组装 + 校验
cd /root/Deepseek/micam
node build/assemble-web.js      # 内联进 index.html
bash build/validate-web.sh      # HTML 结构 + JS 语法 + 类名检查
node build/check-classes.js --js platform/fnos/app/www/app.js   # 类名一致性
```

### 部署（**必须后台任务**）

```bash
bash /tmp/deploy-step.sh
```

**⚠️ 前台运行会在 120 秒被截断，导致部署静默失败。**
用后台任务（run_in_background: true），等 55~60 秒收结果。

### 版本号（发版时 3 处必须同步）

```
platform/fnos/manifest          version = 0.39.0
cmd/micamd/main.go              var appVersion = "0.39.0"
/tmp/deploy-step.sh             main.appVersion=0.39.0
```

### 视觉验证工具

```bash
export PATH=/var/apps/nodejs_v24/target/bin:$PATH
cd /root/shot
node shot.js <页面> /tmp/shots/x.png 13000
# 页面: - (首页) / devices / live / grid / events / settings / recordings / ai
node audit-all2.js      # 全部页面几何审计（溢出/零高/失败图片）
```

Playwright + Chrome：`/root/shot/chrome/chrome-linux64/chrome`

**验证纪律（重要）**：

```
· 改前端后必须截图并**用 read_image 实际查看图片**
  不能只看 DOM 或 curl 结果
· 不要凭印象说「改好了」——要有截图或量化数据
· 历史教训：多次说改好了但实际没生效
```

### 并发警告

```
可能同时有另一个会话在改同一个仓库。
每次改完【立刻 commit】，避免未提交改动被覆盖。
改之前 git status 看一眼有没有别人的未提交改动。
```

---

# 任务 1：开发者页（效果图已给）

页面路径：`renderDev` 函数（在 app.js 里搜 `function renderDev`）

## 效果图结构（详细）

```
页头: 开发者
      开放能力，让更多应用与 Haven 连接。

横向 Tab（7 个）:
  [概览] [接口文档] [API Token] [请求日志] [调用统计] [Webhook 测试] [诊断包]

┌─ 左列（宽）──────────────────────────────┬─ 右列 ────────────────┐
│                                          │                       │
│ ┌ 开放模式 ────────────────────────────┐ │ ┌ API 访问信息 ──────┐ │
│ │ 全局开关              [大开关 ON]     │ │ │ 在局域网内使用以下  │ │
│ │                                      │ │ │ 地址访问 Haven API  │ │
│ │ 🖥 本地 API (REST)   [ON] 已开启  >  │ │ │         [在浏览器中打开]│
│ │    访问设备、事件、回放等基础接口     │ │ │                    │ │
│ │ ▶ 实时流 (HLS)      [ON] 已开启  >  │ │ │ 基础地址            │ │
│ │    获取实时视频流地址                 │ │ │ http://192.168.1.100:8080 [复制]│
│ │ 🔔 事件推送 (Webhook) [ON] 已开启 > │ │ │ WebSocket           │ │
│ │    向第三方推送事件通知               │ │ │ ws://...:8080/ws   [复制]│
│ │ 📁 本地文件访问      [OFF] 已关闭 >  │ │ │ 当前版本            │ │
│ │    访问录像文件（需安全控制）          │ │ │ v1                 [复制]│
│ │ 📡 WebSocket        [ON] 已开启  >  │ │ │                    │ │
│ │    实时事件流                        │ │ │ 快速开始            │ │
│ └──────────────────────────────────────┘ │ │ [cURL][Python][JS] │ │
│                                          │ │ ┌────────────────┐ │ │
│ ┌ API Token ───────────────────────────┐ │ │ │ curl -H "Auth… │ │ │
│ │ 为第三方应用创建访问令牌，更安全地     │ │ │ └────────────────┘ │ │
│ │ 控制访问权限。        [+ 创建 Token]  │ │ │            [复制]  │ │
│ │                                      │ │ └────────────────────┘ │
│ │ 名称 | 权限 | 创建时间 | 最后使用 | 状态 | 操作│              │
│ │ HomeAssistant | 读写 | 2024-03-01 | 2小时前 | ●启用 | ···│     │
│ │ iOS App       | 只读 | 2024-02-18 | 1天前  | ●启用 | ···│     │
│ │ 测试工具       | 读写 | 2024-01-10 | 7天前  | ●禁用 | ···│     │
│ └──────────────────────────────────────┘ │ ┌ 最近请求日志 ──────┐ │
│                                          │ │        [查看全部]   │ │
│ ┌ 调用统计（近 7 天）──── [近7天 ▾] ────┐ │ │ 时间|方法|路径|状态|耗时|IP│
│ │ 总请求    成功    失败    平均耗时     │ │ │14:32:11 GET /api/v1/events 200 120ms 192.168.1.50│
│ │ 12,438    12,102    336     120 ms    │ │ │14:32:10 GET /api/v1/devices 200 85ms ...│
│ │ ↑12%      ↑13%      ↓28%     ↓8%      │ │ │14:32:08 GET /api/v1/stream/1 206 32ms ...│
│ │                                       │ │ │14:32:05 POST /api/v1/webhook/test 200 310ms│
│ │ [折线图 3K/2K/1K/0]                   │ │ │14:32:01 GET /api/v1/recordings 200 98ms│
│ │  3/10 —— 3/16                         │ │ └────────────────────┘ │
│ └───────────────────────────────────────┘ │                       │
│                                          │ ┌ 工具与资源 ────────┐ │
│                                          │ │ [📄 接口文档]  [⬇ 下载 OpenAPI]│
│                                          │ │ [🔍 能力探测]  [🔄 Webhook 测试]│
│                                          │ │ [🗄 诊断包]   [⏱ 重置配置]（红）│
│                                          │ └────────────────────┘ │
└──────────────────────────────────────────┴───────────────────────┘
```

## 现状 vs 效果图

| 元素 | 现状 | 需要做 |
|---|---|---|
| 5 个开放模式开关 | ✅ 已有 | 改成效果图的卡片式（图标+标题+说明+开关+箭头） |
| 4 个工具按钮 | ✅ 已有 | 改成 2×3 操作卡网格 |
| 横向 Tab（7 个） | ❌ | 新增 |
| **API Token 管理** | ❌ | **新后端**（数据结构 + CRUD + 鉴权） |
| **请求日志** | ❌ | **新后端**（中间件 + 环形缓冲 + 接口） |
| **调用统计** | ❌ | **新后端**（从日志聚合 + 趋势） |
| API 访问信息 + 快速开始 | ❌ | 新前端（后端只需暴露地址） |

## 后端要新增

```
1. API Token
   · 数据结构：{id, name, scopes(读写/只读), createdAt, lastUsedAt, enabled, token}
   · 存 <dataDir>/api-tokens.json（token 建议只存哈希）
   · CRUD 接口：GET/POST/PATCH/DELETE /api/dev/tokens
   · **鉴权中间件**：如果将来要给第三方用，请求带 Authorization 头校验
     （注意：现在局域网直连是无鉴权的，加鉴权要考虑向后兼容）

2. 请求日志
   · 中间件记录：时间/方法/路径/状态/耗时/来源IP
   · 内存环形缓冲（建议留最近 200 条，不要写盘）
   · GET /api/dev/logs

3. 调用统计
   · 从日志聚合近 7 天：总数/成功/失败/平均耗时
   · GET /api/dev/stats?days=7

4. 开发能力开关
   · 现有 5 个开关扩展成效果图的 5 项（本地API/实时流/Webhook/文件访问/WebSocket）
   · 现有配置在 /api/dev/config
```

## 注意

```
· WebSocket 是新能力，工作量最大 —— 可以先用「未实现」占位
· 「本地文件访问」是安全敏感的（暴露录像文件），默认必须关闭
· 重置配置按钮是危险操作（红色），要二次确认
· 代码块要有复制按钮
```

---

# 任务 2：逐个确认剩下 14 个容器类

## 背景

前端类名检查脚本（`build/check-classes.js`）扫出 17 个「JS 用了但 CSS 未定义」。
已修 3 个（2 个功能 bug + 8 处变量），**剩 14 个待逐个确认**。

## 剩余清单

```
dev-preview             空状态按钮容器
empty__action           事件列表容器
ev-list                 AI 示例标签
ai-examples__label      AI 证据左列
ai-evidence__main       AI 证据右列
ai-evidence__side       AI 查找卡本体
ai-find                 账号引导文字
acct-guide__text        向导本体
wizard                  特性表标签（3 个）
tag / tag-off / tag-on  设置页行标签
set3__row-label
```

## 怎么确认（不要凭猜）

对每一个类：

```bash
# 1) 在源文件里找到它，看上下文
grep -n "ev-list" platform/fnos/app/www/app.js

# 2) 看它在哪个页面，截图那个页面
export PATH=/var/apps/nodejs_v24/target/bin:$PATH
cd /root/shot && node shot.js events /tmp/shots/x.png 13000

# 3) **用浏览器量它的 computed style**（关键 —— 不要只看类名列表）
#    写个 Playwright 脚本，取该元素的 getBoundingClientRect + getComputedStyle
#    判断它是否真的需要样式：
#      · 宽高为 0 或异常     -> 需要补样式
#      · 父类已处理布局      -> 不需要，是纯容器
```

## 判定标准

```
需要修（功能性）：
  · 元素尺寸为 0 / 溢出 / 不可见
  · 缺少必要的间距导致内容挤在一起

不需要修（纯容器）：
  · 父类已提供布局，本类只是语义标记
  · 例：wizard 的 wizard__ 子类有 6 处样式，本体只是容器

只影响美观（可修可不修，需用户确认）：
  · tag / tag-off / tag-on 只影响开发者页特性表外观
```

## 注意

```
· 加样式 = 改视觉。如果有疑问，先问用户，不要擅自改
· 用户的原则是「质量改进不改视觉」，但功能性缺失（尺寸为 0）该修
```

---

# 任务 3：补版本号 + 发版

## 步骤

```bash
# 1) 三处同步（假设升到 0.40.0）
#    platform/fnos/manifest
#    cmd/micamd/main.go          var appVersion = "0.40.0"
#    /tmp/deploy-step.sh         main.appVersion=0.40.0

# 2) 编译 + 组装 + 校验
export PATH=/usr/local/go/bin:$PATH GOPROXY=https://goproxy.cn,direct
cd /root/Deepseek/micam
go build -o /tmp/micamd ./cmd/micamd
node build/assemble-web.js && bash build/validate-web.sh

# 3) 部署（后台任务！）
bash /tmp/deploy-step.sh

# 4) 验证
curl -s http://127.0.0.1:8900/api/health     # version 应为新版本
# 截图 8 个页面确认无回归

# 5) 打包 + 发版
cd platform/fnos && fnpack build
cd /root/Deepseek/micam
for d in /vol1/1000/micam.fpk '/vol1/1000/分享/micam/micam.fpk' /vol1/@appshare/micam/micam.fpk; do
  rm -f "$d"; cp platform/fnos/micam.fpk "$d" && chmod 644 "$d"
done
chown 1000:1000 /vol1/1000/micam.fpk '/vol1/1000/分享/micam/micam.fpk' 2>/dev/null

git add -A && git commit -m "..." && git push
gh release create v0.40.0 platform/fnos/micam.fpk \
  --title "v0.40.0 — ..." --notes-file /tmp/commit-msg.txt
```

## 安全检查（发版前必做）

```bash
# 仓库里绝不能有密钥
grep -rn 'sk-' . --include='*.go' --include='*.js' --include='*.json' 2>/dev/null | grep -v node_modules
# 敏感文件权限
ls -la /vol1/@appdata/micam/accounts.json /vol1/@appdata/micam/ai-key
#   accounts.json 应为 -rwx------
#   ai-key        应为 -rw-------
```

---

# 参考文档

```
docs/README.md                    文档索引（41 个文档按主题归类）
docs/CODE-REVIEW.md               代码审查报告（含 14 个类名的出处）
docs/DESIGN.md                    整体设计
docs/PAGES-SPEC.md                页面规格书（42KB，含开发者页）
docs/MOCKUP-REQUEST-4PAGES.md     四页效果图需求书
docs/ACCOUNTS-MOCKUP-GAP.md       账号页（已完成）
docs/STORAGE-MOCKUP-GAP.md        存储页（已完成）
```

# 其他已知情况

```
· 版本号可能不一致：commit 消息写过 v0.39.2 但代码是 0.39.0
  -> 发版时统一到 0.40.0
· 另一个会话可能同时在改 -> 改完立刻 commit
· /favicon.ico 404 是历史现象，非 bug
· 多路预览页 headless 截图 tile 全黑是历史现象（A/B 验证过非回归）
```

--- 提示词结束 ---

## 使用说明

```
1. 复制上面标记之间的全部内容
2. 粘贴到新窗口
3. 三个任务按顺序做：开发者页 -> 容器类 -> 发版
4. 每完成一项就 commit（避免并发覆盖）
```
