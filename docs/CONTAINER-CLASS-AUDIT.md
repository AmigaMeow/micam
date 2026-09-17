# 容器类确认（check-classes 剩余 10 项）

> 2026-09-17 · v0.40.0 · 对应 `docs/CODE-REVIEW.md` 第七节的存量清单

## 背景

`build/check-classes.js` 报「JS 用了但 CSS 未定义」的类名。本轮开始时是 14 个，
现在收敛到 **10 个**，减少的 4 个是：

| 原类名 | 变化 |
|---|---|
| `tag` / `tag-off` / `tag-on` | 开发者页重写（7 Tab）后旧特性表取消，**不再使用** |
| `open` | **真 bug，已修**：`openSider()` 加的是 `is-open`，左滑手势却判断 `contains("open")`，恒为 false -> 左滑永远关不掉侧栏。已改为 `is-open` |

## 方法

不只看类名列表（这正是历史上误判的来源）。对每个类：

1. `grep -n` 源文件确认它在**哪个页面、哪种状态**出现；
2. Playwright 打开真实页面，取该元素的
   `getBoundingClientRect()` + `getComputedStyle()`，**同时取父元素**的
   `display / gap / padding` 与父元素尺寸；
3. 截图并用 `read_image` 实际查看；
4. 只有「宽高为 0 / 溢出 / 不可见 / 内容挤在一起」才算需要补样式。

实测脚本：`/root/shot/audit-classes.js`（不在仓库内）。

## 逐条结论

| 类名 | 出现位置 | 实测 | 父类（决定布局的那个） | 判定 |
|---|---|---|---|---|
| `empty__action` | `errorCard()` 空状态里的按钮容器（L1275） | 60x34，按钮与描述间距 **12px** | `.empty` = flex column, `gap:12px`, `align:center` | **纯容器**。间距由父类 gap 提供 |
| `ev-list` | 事件页列表容器 `#evList`（L1716） | 空数据时页面走 empty 分支，未出现在 DOM | 子元素 `.ev-group`（`margin-bottom:space-6`）、`.ev-group__items`（flex, gap）、`.ev-card2`（完整卡片样式） | **纯容器**。样式全在子类上 |
| `ai-examples__label` | AI 页「试试这些问题：」（L2579） | 98x21，可见 | `.ai-examples` = flex, `gap:8px` | **纯容器** |
| `ai-evidence__main` | AI 搜索答案「主要证据」列（L2627） | 有结果时出现 | `.ai-evidence` = **grid 两列** `1.5fr / 1fr` | **纯容器**（grid 子项） |
| `ai-evidence__side` | 同上「关键时刻」列（L2648） | 同上 | 同上 | **纯容器**（grid 子项） |
| `ai-find` | AI 页「问 AI 一个问题」卡本体（L2728） | 调查 Tab 实测 **1392x176**，内容完整 | 内部 `.ai-find__*` 一族（input/plan/answer/evidence）自带样式 | **纯容器**。本体只是语义标记 |
| `acct-guide__text` | 账号页扫码引导右列文字（L5393） | 折叠态父级 `display:none` -> 0x0（**预期**） | `.acct-guide__body` 展开时 = grid（`auto / minmax(0,1fr)`, `gap:space-6`） | **纯容器**（grid 子项）。子元素 `__title/__desc` 有样式 |
| `wizard` | 安装向导根容器（L5508） | 已配置环境不可达（需未配置状态） | 子元素 `.wizard__hero/__logo/__steps/__actions` 均有样式 | **纯容器**（CSS 侧证据） |
| `set3__row-label` | 设置页每行左侧标签（L6786） | 570~756 x 42，可见 | `.set3__row` = grid, `gap:16px` | **纯容器** |
| `dev-preview` | **死选择器**（L1122） | 设备页 **0 个元素**带此类；全仓库只有一句 `querySelectorAll(".dev-preview")` | — | **无元素 -> 不是样式问题**，是绑定不到任何元素的死代码 |

## 结论

**10 个全部不需要补样式**：8 个是父类已提供布局的语义容器（grid/flex 子项），
1 个是折叠态下的正常 0 尺寸（`acct-guide__text`），1 个是死选择器（`dev-preview`）。

没有做任何「加样式」的改动 —— 加样式等于改视觉，超出「质量改进」范围。
唯一改动的视觉相关代码是 `open` -> `is-open`（功能 bug，与样式无关）。

## 后续建议（未做）

```
· dev-preview：可删掉 bindDeviceActions() 里那段死绑定；
  但删了没有视觉变化，也可能将来复用时被误删，暂留并在此记录。
· validate-web.sh 现在还是「只警告不阻塞」。存量 10 个已确认无功能影响，
  想改成硬门禁的话，建议把这份清单做成白名单，而不是把 10 个类名硬写进 CSS。
```
