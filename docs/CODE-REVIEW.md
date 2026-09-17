# 代码审查报告

> 2026-09-17 · v0.36.0

---

## 一、规模

```
Go        18,760 行   （后端 + 内置 Web 服务）
Python       740 行   （AI 推理服务 infer.py / lpr.py / embed.py）
前端 JS    6,774 行   （单文件，组装进 index.html）
CSS        4,289 行   （tokens / base / components / pages）
文档           41 个   （约 300KB）
```

---

## 二、安全检查 ✅

### 凭据文件权限

| 文件 | 权限 | 评价 |
|---|---|---|
| accounts.json | `-rwx------` | ✅ 只有属主可读 |
| sources.json | `-rwx------` | ✅ |
| **ai-key** | `-rw-------` | ✅ API 密钥单独存，0600 |
| ai-cost.json | `-rw-r--r--` | 🟡 费用记账，无敏感信息 |
| camera-poses.json | `-rw-r--r--` | 🟡 机位元数据，无敏感信息 |
| ptz-presets.json | `-rw-r--r--` | 🟡 预置位，无敏感信息 |

**结论**：敏感文件全部 0600/0700，其余为无敏感元数据。

### 密钥泄露检查

```
全仓库搜索 sk- / passToken= / password=  -> 无硬编码密钥
（唯一命中的是 fmt.Sprintf 的格式串，不是真实值）

API Key 隔离：
  · 存 <dataDir>/ai-key（0600），不进 config.json
  · struct tag 为 json:"-"，绝不下发前端
  · 前端只拿到 hasKey 布尔值
验证：config.json 里搜索密钥 -> 0 处匹配 ✅
```

---

## 三、代码质量观察

### 3.1 优点

```
✓ 分层清晰：本地模型筛选 / 云端理解 / 规则引擎决定何时花钱
✓ 证据零编造：模型只输出 match/note/confidence，
  时间/位置由后端绑定事件 ID 生成（数据结构层面保证）
✓ 结论措辞有纪律：不说「没有发生」，只说「候选中未发现」+ 覆盖率
✓ 成本控制完整：预估（免费）-> 确认 -> 硬上限 -> 按实际 usage 记账
✓ 模型能力边界有实测数据支撑（不是拍脑袋）
✓ 失败可观测：AI 任务有状态机 + 日志 + 重试上限
```

### 3.2 需改进

| # | 问题 | 影响 | 建议 |
|---|---|---|---|
| 1 | 前端 JS 单文件 6,774 行 | 难维护 | 已按页面分节，可再拆模块（但无构建步骤是刻意选择） |
| 2 | `_ = xxx` 忽略返回值的调用较多 | 潜在静默失败 | 关键路径应记日志 |
| 3 | 部分 store 直接整体重写 JSON | 数据量大时性能 | 当前规模（几百条）无问题，需监控 |
| 4 | AI 记账解锁/加锁模式（addCost）| 可读性差 | 可改为先算后写，避免中途解锁 |
| 5 | 文档同一主题多版本（7 个 Mac 相关）| 查找困难 | 建 docs/_archive/ |

### 3.3 已知技术债（有意为之）

```
· 前端无构建步骤 -> 换取「网关路径重写下仍可用」
  （Basecoat 移除后 CSS 143KB 内联，可接受）
· 不用 cgo -> LPR 走 Python ctypes 调用 .so
  （CGO_ENABLED=0 是既定约束）
· 模型不进 fpk -> 首次启用时下载（31MB LPR + 56MB YOLO）
```

---

## 四、本轮修复回顾（值得记录的 bug）

| Bug | 根因 | 教训 |
|---|---|---|
| 弹窗透明不可见 | Basecoat 定义了同名 `.dialog { opacity:0 }` | **同名类冲突是系统性风险**，已移除 Basecoat |
| 17 个类名冲突 | 同上 | 引入第三方 CSS 前必须做类名审计 |
| 开关完全不可见 | `<span>` 少了 `switch__track` 类 | HTML 与 CSS 约定要有检查 |
| 48 处颜色失效 | `--text-3` 变量从未定义 | 用不存在的变量不会报错，只会静默失效 |
| 服务崩溃 | `loadCostState(srv.ai)` 插在 `srv.ai` 创建之前 | 插入初始化代码必须确认依赖顺序 |
| 日期筛选查不到 | `toISOString()` 是 UTC | 本地日期必须用本地时区 |
| 用现在画面分析历史事件 | 取图顺序错误 | 时序语义要写清楚 |
| 缓存命中重复计费 | `addCost` 在缓存判断之前 | 计费逻辑的顺序敏感 |
| 车牌投票取错字段 | `max([c for c,cf in ...])` 取的是字符 | 推导式变量名要一致 |
| LPR 的 .so 污染 stdout | C 库直接写 fd 1 | 子进程协议要先隔离 fd |

**共性：这些都不是逻辑错误，而是「约定/顺序/命名」问题。**

---

## 五、建议的下一步质量工作（2026-09-17 · v0.39.0 已全部完成）

```
[x] 建 docs/_archive/ 归置历史文档              -> 6 个历史文档已归档，见 docs/README.md「历史归档」
[x] 给关键路径的 `_ = xxx` 加日志               -> 37 处关键路径加日志，见第六节
[x] AI 计费逻辑改为无中途解锁                    -> addCost / addImages 改为「锁内算快照 + 锁外写盘」
[x] 补一个「CSS 类名与 JS 使用一致性」的检查脚本 -> build/check-classes.js（零依赖），已接入 validate-web.sh（只提示）
[x] 前端 JS 分节索引                            -> build/gen-js-index.js 自动生成，索引在 /tmp/final-js.txt 头部
```

---

## 六、`_ = xxx` 分类结果（2026-09-17）

改前 159 处，其中 **37 处属关键路径**，已加日志（编译通过、部署验证）：

| 类别 | 处数 | 代表 |
|---|---|---|
| 事件写入 | 5 | `eventStore.Enrich`、`appendLocked`、`SaveThumb`、`os.Rename` 原子替换 |
| 数据持久化 | 9 | 机位文件写+改名、凭据备份、LPR 资源/模型写入、二维码图片 |
| 状态变更 | 13 | `cfg.SaveSelf`、`saveProviderKey("")`、presets/marks 删除、producer/consumer Stop/Start |
| 持久化目录创建 | 5 | 模型目录、解压目录、下载目录、日志目录 |
| 外部命令 | 3 | `systemctl stop / disable / daemon-reload` |
| 计费 / 记账 | 1 | `saveCostState` 写盘失败 |

**剩余 122 处全部判为非关键并保持现状**：编译期占位 47、解析失败降级 22、临时文件清理 16、
资源释放 15、缓存目录 5、缓存写入 2、HTTP 响应写 2、其它 13。

原则：**不为「干净」全改** —— 把 159 处全改一遍，风险远大于收益。
完整清单：`grep -rn "_ = " --include='*.go' cmd internal`。

---

## 七、前端类名 / CSS 变量未定义清单（2026-09-17 扫描，**本轮未修**）

`node build/check-classes.js` 扫出 **18 个未定义项**（JS 用 611 个类名，CSS 定义 849 个）。
都是**存量问题**，本轮只做检测不改动 —— 修它们会改变视觉效果，超出「质量改进」范围，需要单独决策。

### 7.1 未定义类名（14）

| 类名 | 位置 | 备注 |
|---|---|---|
| `open` | L299 / L304 | 代码只 add/remove `is-open`，CSS 也只有 `.is-open` -> `contains("open")` 恒 false，侧栏滑动手势分支失效 |
| `spin` | L4175 | 实际用的类是 `.spinner` -> 「隐藏 loading」永不生效 |
| `ai-find` | L2726 | `.ai-find__answer` / `__cover` / `__est-*` 一族都在，独缺本体 |
| `ai-evidence__main` / `ai-evidence__side` | L2625 / L2646 | 同级 `__label` / `__box` / `__img` 都在 |
| `tag` / `tag-on` / `tag-off` | L6079 | CSS 里 `.tag*` 一个都没有（相近的只有 `.badge` / `.ev-tag`） |
| `dev-preview` `empty__action` `ev-list` `ai-examples__label` `tl-strip` `wizard` | L1120 / L1273 / L1714 / L2577 / L5434 / L5860 | 待确认是漏写 CSS 还是废弃代码 |

### 7.2 未定义 CSS 变量（4 个 / 18 处）

| 变量 | 处数 | 等价 token |
|---|---|---|
| `--error` | 11 | `--danger` |
| `--success` | 3 | `--online` |
| `--hover` | 2 | `--surface-hover` |
| `--split` | 2 | `--border` / `--border-subtle` |

> `--text-2` / `--text-3`（曾 49 处未定义）已在 tokens.css 加了别名，不再报警。

### 7.3 防回归

`build/validate-web.sh` 已接入 `node build/check-classes.js --js "$HTML"`，
**当前只警告不阻塞**（存量 18 个问题会立刻卡住部署）。
建议先把上面 18 个修完，再改成 `fail_msg` 硬门禁。
