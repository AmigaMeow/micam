# MiCam 前端设计规范

> 基线版本：v0.15.0 · 2026-09-16
>
> 本文档定义界面设计令牌（design tokens）与组件规范。
> 改版时**只改本文件定义的令牌**，组件样式自动跟随。

---

## 一、技术栈

| 项 | 选择 | 说明 |
|---|---|---|
| 框架 | **无**（原生 JS） | 单文件，无构建步骤 |
| 文件 | `platform/fnos/app/www/index.html` | 单文件约 2900 行 |
| 样式 | 内联 CSS + CSS 变量 | 769 条规则 |
| 运行时库 | hls.js（本地内置） | 619KB，HLS 播放 |
| 宿主 SDK | `@trimjs/web-app` | 飞牛提供，取主题/语言 |

### 为什么不用框架

- 飞牛插件**没有**官方 UI 框架要求（已核实其他插件）
- 单文件便于 fpk 打包和部署，改完即生效
- **但**：2900 行已接近单文件可维护上限

### 约束（重要）

**不能依赖外部 CDN。** 国内网络对 jsdelivr/unpkg 不稳定，
实测 hls.js 从 CDN 加载失败会导致**播放黑屏**。
所有第三方库必须**本地内置**。

---

## 二、飞牛宿主环境

### 2.1 两种接入方式

| 方式 | 配置 | 说明 |
|---|---|---|
| **内嵌应用**（我们用） | `micro_app = true` + `www/index.html` | 自己的页面跑在飞牛窗口里 |
| 外部链接 | `ui/config` 里声明 `.url` + 端口 | 飞牛只显示图标，点开跳转 |

其他插件（AList / qBittorrent / 1Panel）都用「外部链接」方式。

### 2.2 宿主 SDK 能力

`@trimjs/web-app` 提供：

```javascript
const app = new TrimApp();
const cfg = await app.getPlatformConfig();
// cfg.theme          -> "dark" | "light"
// cfg.systemVersion  -> 飞牛版本
// cfg.language       -> 语言

await app.setTitle("小米摄像头");
app.$on("os/theme", t => { /* 主题变化 */ });
```

**没有 UI 组件库、没有样式注入** —— 界面完全由我们自己设计。

### 2.3 降级行为

SDK 从 unpkg 加载，加载失败时降级为「独立模式」
（TCP 8900 直接访问），不影响功能。

---

## 三、设计令牌（Design Tokens）

### 3.1 品牌色

```css
--primary:        #1677ff    /* 主色（Ant Design 蓝） */
--primary-hover:  #4096ff
--primary-active: #0958d9
```

### 3.2 语义色

```css
--success: #52c41a    /* 在线、成功 */
--warning: #faad14    /* 告警、注意 */
--error:   #ff4d4f    /* 错误、离线 */
```

### 3.3 文字层级

```css
--text:   rgba(0,0,0,.88)   /* 正文 */
--text-2: rgba(0,0,0,.65)   /* 次要 */
--text-3: rgba(0,0,0,.45)   /* 辅助、说明 */
```

深色模式：`rgba(255,255,255,.85/.65/.45)`

### 3.4 背景与边框

```css
--bg:       #f5f5f5              /* 页面底色 */
--card:     #fff                 /* 卡片 */
--border:   #f0f0f0              /* 常规边框 */
--border-2: #d9d9d9              /* 强调边框 */
--split:    rgba(5,5,5,.06)      /* 分隔线 */
--hover:    rgba(0,0,0,.04)      /* 悬停底色 */
```

深色模式：`--bg: #141414`、`--card: #1f1f1f`、`--border: #303030`

### 3.5 侧边栏

```css
--sider:        #001529                 /* 深色导航 */
--sider-text:   rgba(255,255,255,.65)
--sider-active: #fff
--sider-hover:  rgba(255,255,255,.08)
```

### 3.6 圆角

```css
--radius-sm: 6px    /* 按钮、标签 */
--radius:    8px    /* 卡片、输入框 */
--radius-lg: 12px   /* 弹窗 */
```

### 3.7 阴影

```css
--shadow:       0 1px 2px rgba(0,0,0,.03), 0 1px 6px -1px rgba(0,0,0,.02), 0 2px 4px rgba(0,0,0,.02)
--shadow-hover: 0 6px 16px rgba(0,0,0,.08), 0 3px 6px -4px rgba(0,0,0,.12), 0 9px 28px 8px rgba(0,0,0,.05)
```

### 3.8 字体

```css
--font: -apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC",
        "Microsoft YaHei", Helvetica, Arial, sans-serif;
```

**当前字号使用情况**（需优化 —— 12px 用了 60 次，偏小）：

| 字号 | 次数 | 用途 |
|---|---|---|
| 11px | 8 | 极小标注 |
| **12px** | **60** | 说明文字、表格 |
| 13px | 27 | 正文 |
| 14px | 12 | 按钮、强调 |
| 15-16px | 8 | 卡片标题 |
| 22-26px | 3 | 大数字 |

---

## 四、组件清单

### 4.1 布局

| 组件 | 类名 | 说明 |
|---|---|---|
| 侧边导航 | `.sider` / `.menu-item` | 左侧固定，深色 |
| 卡片 | `.card` / `.card-head` / `.card-title` | 内容容器 |
| 网格 | `.grid` | 设备卡片自适应排列 |
| 统计块 | `.stat-box` | 大数字 + 标签 |

### 4.2 交互

| 组件 | 类名 | 变体 |
|---|---|---|
| 按钮 | `.btn` | `.btn-primary` `.btn-sm` `.btn-danger` |
| 开关 | `.switch` | 滑动开关 |
| 标签页 | `.tabs` | 横排切换 |
| 下拉 | 原生 `<select class="btn">` | 复用按钮样式 |
| 弹窗 | `.mask` / `.modal` | 遮罩 + 居中卡片 |
| 提示 | `toast()` | 右上角浮层 |

### 4.3 业务组件

| 组件 | 类名 | 说明 |
|---|---|---|
| 播放器 | `.player` / `.loading-overlay` | 视频 + 加载进度 |
| 云台按钮 | `.ptz-btn` / `.ptz-grid` | 十字方向盘 |
| 状态标签 | `.tag` | `.tag-on` `.tag-off` `.tag-warn` `.tag-share` |
| 多路网格 | `.grid-view` / `.grid-tile` | 摄像头平铺 |
| 录像网格 | `.rec-grid` | 缩略图 + 时长 |
| 时间轴 | `.tl-strip` | 缩略图带 + 点击热区 |
| 配置行 | `.cfg-row` / `.cfg-label` / `.cfg-hint` | 左标签 + 右控件 |
| 检查项 | `.check-item` | 图标 + 标题 + 说明 |

### 4.4 页面

共 **10 个页面**：

```
renderDevices     设备列表
renderLive        实时预览（含云台）
renderGrid        多路预览
renderStorage     存储监控（含策略）
renderRecordings  录像管理（含回放/导出）
renderAccounts    账号管理
renderWizard      安装向导
renderSettings    系统设置（通知/诊断）
renderDev         开发者（开放模式）
```

---

## 五、响应式

```css
@media (max-width: 640px) {
  /* 侧边栏收起、网格单列、字号微调 */
}
```

断点只有一个（640px），平板/桌面共用一套。

---

## 六、改版指南

### 如果要改配色

只改第 3 章的令牌值即可，组件自动跟随。

### 如果要改组件形态

定位到 `.card` / `.btn` 等类名，改样式规则。

### 如果要换技术栈（Vue/React）

**可行**，但需要：

1. 加构建步骤（Vite），产物输出到 `platform/fnos/app/www/`
2. `build-fnos.sh` 增加「前端构建」阶段
3. 所有依赖打包进产物（**不能留 CDN 引用**）

代价：fpk 构建需要 Node 环境；调试从「改完刷新」变成「改完重新构建」。

### 已知可优化点

| 问题 | 建议 |
|---|---|
| 12px 字号占主导，偏小 | 正文提到 13-14px，说明文字 12px |
| 单文件 2900 行 | 可拆分 CSS 到 `app.css`（无构建步骤也能用） |
| 无统一间距体系 | 引入 `--space-*` 令牌（4/8/12/16/24） |
| 无统一动效 | 引入 `--ease` / `--duration` 令牌 |
| 图标为内联 SVG 函数 | 可抽成图标集 |

---

## 七、实现现状（v0.16.0）

### 7.1 技术栈落地方案

~~~
Haven UI = Basecoat primitives + shadcn/Linear 视觉
         + Haven Tokens + Lucide + Vanilla JS
~~~

| 层 | 选型 | 位置 |
|---|---|---|
| 基础组件 | **Basecoat** 1.0.2（自托管，无构建） | `assets/ui/basecoat.css` + `assets/ui/js/` |
| 设计令牌 | **Haven**（本项目定义） | `assets/css/tokens.css` |
| 图标 | **Lucide** 1.46.0 子集（92 个，19KB sprite） | `assets/img/icons.svg` |
| 业务组件 | 自研 | `assets/css/components.css` |
| 播放库 | hls.js（自托管） | `assets/vendor/hls.min.js` |
| 交互 | Vanilla JS | `index.html`（组装产物） |

**为什么不用框架**：飞牛插件无官方 UI 要求；保持无构建能"改完即生效"。
**硬约束**：所有依赖必须本地化（CDN 在国内不可靠）。

### 7.2 目录结构

~~~
platform/fnos/app/
├── shell.html              外壳模板（占位符：<!--SPRITE--> <!--APP_JS-->）
├── www/
│   ├── index.html          组装产物
│   └── assets/
│       ├── css/            tokens / base / components / pages
│       ├── js/             （应用逻辑内联在 index.html，此目录预留）
│       ├── ui/             Basecoat CSS + 组件 JS
│       ├── img/            Lucide sprite
│       └── vendor/         hls.js
└── ...
~~~

组装：`node build/assemble-web.js`（外壳 + sprite + JS → index.html）

### 7.3 Basecoat 集成方式

Basecoat 用 shadcn 语义变量。**不修改它的 dist 文件**，
而是在 `tokens.css` 里把这些变量指向 Haven 令牌：

~~~css
--background: var(--bg);
--card:       var(--surface-1);
--primary:    var(--accent);
--muted:      var(--surface-2);
--border:     var(--border);
--radius-sm:  6px;   /* 覆盖 Basecoat 的 calc 派生 */
~~~

这样 Basecoat 组件自动套上 Haven 皮肤。

### 7.4 明暗模式

- 默认 `<html class="dark">`（对齐效果图）
- 飞牛宿主为浅色时，SDK 回调移除 `dark` 类
- 令牌按 `.dark` 作用域切换

### 7.5 浏览器实测

| 项 | 计算值 | 期望 | |
|---|---|---|---|
| 背景 | `rgb(14,17,22)` | `#0e1116` | ✅ |
| 侧边栏 | `rgb(18,23,30)` | `#12171e` | ✅ |
| 激活项 | `rgb(27,33,42)` | `#1b212a` | ✅ |
| 文字 | `rgb(230,237,243)` | `#e6edf3` | ✅ |
| 侧边栏宽 | `232px` | `--sider-w` | ✅ |

8 个页面全部渲染正常，无 JS 错误。

### 7.6 待优化

| 项 | 说明 |
|---|---|
| 页面级细节 | 各页可继续按效果图细化（当前是统一改造） |
| 图标按需加载 | 当前 sprite 92 个；可按页拆分 |
| 业务组件抽象 | camera-card / rec-item 可进一步参数化 |
