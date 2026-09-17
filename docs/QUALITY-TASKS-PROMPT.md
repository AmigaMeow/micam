# 质量改进任务提示词（可直接在独立窗口使用）

> 复制下面的「--- 提示词开始 ---」到「--- 提示词结束 ---」之间的全部内容，
> 粘贴到新窗口。

--- 提示词开始 ---

# 任务：MiCam 插件质量改进（5 项）

## 项目背景

MiCam 是一个**飞牛 NAS（fnOS）的小米摄像头插件**，开源在 `https://github.com/AmigaMeow/micam`。

```
代码位置   /root/Deepseek/micam/
分支       master（公开仓库）
当前版本   0.38.0
```

### 架构

```
后端   Go（cmd/micamd/ 主程序，internal/ 子包）
       · 单个二进制 micamd，内置 HTTP 服务（端口 8900）
       · 运行用户 micam
       · CGO_ENABLED=0（不用 cgo）

前端   单文件 JS（/tmp/final-js.txt，约 6800 行）
       组装进 platform/fnos/app/www/index.html
       · **无构建步骤**（刻意选择：飞牛网关会重写相对路径，
         所以 CSS/JS 全部内联）
       · 不使用框架，原生 DOM 操作
       · 设计语言：深色 + shadcn 风格

AI     本地 YOLO（onnxruntime + Python）+ 云端视觉大模型（DeepSeek）
       · platform/fnos/app/ai/*.py
```

### 关键约束（务必遵守）

```
1. **不要引入构建步骤**（webpack/vite/tsc 都不行）
2. **不要引入 cgo**（CGO_ENABLED=0 是既定约束）
3. **不要引入第三方 CSS 框架**
   教训：曾经引入 Basecoat，导致 17 个类名冲突、弹窗透明、
         开关不可见。已全部移除。
4. **不要改 UI 视觉**（这些任务只做质量改进，不改设计）
5. **不要提交任何密钥/凭据**
```

---

## 环境与工作流

### 编译

```bash
export PATH=/usr/local/go/bin:$PATH
export GOPROXY=https://goproxy.cn,direct
cd /root/Deepseek/micam
go build -o /tmp/micamd ./cmd/micamd
gofmt -w cmd/micamd/*.go internal/*/*.go
```

### 前端改动流程（重要）

```bash
# 1) 改 /tmp/final-js.txt（这是前端 JS 的唯一源文件）
# 2) 组装 + 校验
cd /root/Deepseek/micam
node build/assemble-web.js      # 把 JS/CSS 内联进 index.html
bash build/validate-web.sh      # 校验 HTML 结构 + JS 语法 + 无外部 CSS
```

### 部署（关键）

```bash
bash /tmp/deploy-step.sh
```

**⚠️ 部署必须作为后台任务运行**：

```
前台运行会在约 120 秒被截断，导致部署静默失败。
用 run_in_background: true，然后等 55~60 秒收结果。
```

### 版本号（3 处必须同步）

```
platform/fnos/manifest          version = 0.38.0
cmd/micamd/main.go              var appVersion = "0.38.0"
/tmp/deploy-step.sh             main.appVersion=0.38.0
```

### 插件访问地址

```
http://192.168.2.100:8900/      （无鉴权，总是最新）
```

### 视觉验证工具（改前端时用）

```bash
export PATH=/var/apps/nodejs_v24/target/bin:$PATH
cd /root/shot
node shot.js <页面> /tmp/shots/x.png 13000
# 页面: - (首页) / devices / live / grid / events / settings / recordings / ai

node audit-all2.js    # 全部页面几何审计（溢出/零高/失败图片）
```

工具用 Playwright + Chrome for Testing，路径 `/root/shot/chrome/chrome-linux64/chrome`。

**验证要求**：

```
· 改前端后必须截图并**实际查看图片**（用 read_image），
  不能只看 DOM 或 curl 结果
· 几何改动要跑 audit 脚本量化确认
· 不要凭印象说「改好了」——要有数据或截图证据
```

---

## 任务清单（5 项）

### 任务 1：给关键路径的 `_ = xxx` 加日志

**现状**：

```bash
grep -rn '_ = ' cmd/micamd/*.go internal/*/*.go | wc -l
```

有大量忽略返回值的调用。其中一部分是合理的（如 `_ = f.Close()`），
但**关键路径**的静默失败会导致难以排查的问题。

**要做的**：

```
1. 先分类列出所有 `_ =` 调用（脚本输出到 /tmp/ignored-calls.txt）
2. 按「关键 / 非关键」分类：

   关键（必须加日志或处理）：
     · 数据持久化（写文件、重命名、删文件）
     · 状态变更（启动/停止服务、保存配置）
     · 网络/外部调用
     · 事件写入（eventStore.*）
     · 计费/记账

   非关键（保持现状即可）：
     · f.Close() / resp.Body.Close()
     · os.Remove 临时文件
     · 清理类操作（失败也无妨）

3. 给关键路径的忽略调用加上日志，例如：

   // 改前
   _ = os.WriteFile(path, data, 0o644)

   // 改后
   if err := os.WriteFile(path, data, 0o644); err != nil {
       logf("[store] 写入 %s 失败: %v", path, err)
   }

4. 保持改动最小 —— 不要顺手重构周边代码
```

**怎么算完成**：

```
· 关键路径的忽略调用都有日志或错误处理
· 非关键的保持不变（不要为了「干净」全部改，会引入风险）
· 编译通过
· 部署后功能正常（截图验证几个主要页面）
```

---

### 任务 2：AI 计费逻辑改为无中途解锁

**问题位置**：`cmd/micamd/ai.go` 的 `addCost` / `addImages`

**现状**（可读性差，且是有风险的写法）：

```go
func (a *aiClient) addCost(cny float64) {
    a.mu.Lock()
    defer a.mu.Unlock()
    // ... 更新计数 ...
    a.costCNY += cny
    a.cloudCount++

    // ↓ 为了写盘先解锁，写完再锁回来
    day2, cny2, cloud2, img2 := a.costDay, a.costCNY, a.cloudCount, a.imgCount
    a.mu.Unlock()
    saveCostState(day2, cny2, cloud2, img2)
    a.mu.Lock()
}
```

**为什么要改**：

```
· Lock/Unlock/Lock 交替极易出错（例如 panic 路径下锁状态不一致）
· 可读性差，后来者容易改错
· defer Unlock 和手动 Unlock 混用是反模式
```

**改成什么**：

```go
// 先算好要落盘的快照，函数返回后再写盘（或异步写）
func (a *aiClient) addCost(cny float64) {
    day, total, cloud, images := a.addCostLocked(cny)
    saveCostState(day, total, cloud, images)   // 锁外写盘
}

func (a *aiClient) addCostLocked(cny float64) (string, float64, int, int) {
    a.mu.Lock()
    defer a.mu.Unlock()
    // ... 更新 ...
    return a.costDay, a.costCNY, a.cloudCount, a.imgCount
}
```

**同样的模式也用在 `addImages`**。

**额外要求**：

```
· 写盘失败要记日志（结合任务 1）
· 如果写盘很频繁，可考虑加节流（例如同一秒内多次写只落一次盘）
  但不要引入 goroutine 泄漏风险
· 保持对外语义不变：调用 addCost 后 costToday() 能读到新值
```

**怎么验证**：

```bash
# 1) 分析一个事件，看费用是否正确累计
curl -s -X POST http://127.0.0.1:8900/api/ai/analyze \
  -H 'Content-Type: application/json' -d '{"id":"<某个事件ID>"}'

# 2) 检查费用文件
cat /vol1/@appdata/micam/ai-cost.json

# 3) 重启服务，确认费用仍在（持久化没坏）
# 4) 检查 /api/ai/usage 的数字一致
```

---

### 任务 3：CSS 类名与 JS 使用一致性检查脚本

**背景（这是本项目最大的坑）**：

```
历史上出现过的同类 bug：
  · JS 里用了 class="switch__track" 但漏写 -> 开关完全不可见
  · JS 里用了 CSS 从未定义的 class -> 样式静默失效
  · 引入第三方 CSS 导致 17 个同名类冲突（已移除该框架）
  · 48 处用了未定义的 CSS 变量 --text-3 -> 颜色静默失效
```

**要写的脚本**：`build/check-classes.js`

**检查项**：

```
1. JS 里用到的类名（class="..." 和 className="..."）
   -> 是否在 CSS 里有定义？
   未定义的要报告（可能是笔误或漏写 CSS）

2. CSS 变量：JS 内联 style 里用的 var(--xxx)
   -> 是否在 tokens.css 里有定义？
   未定义的要报告

3. CSS 里定义但 JS 从未使用的类（可选，仅统计不报错）
```

**输出格式**：

```
✅ MiCam 前端类名检查

JS 使用的类名:        762
CSS 定义的类名:       891

❌ JS 用了但 CSS 未定义（3）:
   ev-list        出现在 第 1234 行
   tag            出现在 第 2345 行
   wizard         出现在 第 3456 行

❌ 未定义的 CSS 变量（1）:
   --text-4       出现在 第 4567 行

ℹ CSS 定义但未使用: 129 个（不报错，仅提示）

✅ 检查通过 / ❌ 发现 N 个问题
```

**要求**：

```
· 纯 Node 脚本，零依赖（不要 npm install）
· 输入文件：
    /tmp/final-js.txt（前端 JS）
    platform/fnos/app/www/assets/css/*.css
· 退出码：有「未定义」问题返回 1，否则返回 0
· 误报要少 —— 注意排除：
    · 带拼接的 class（如 'ev-card2' + suffix）
    · 模板字符串里的类名
    · 注释里的类名
    · SVG 里的 class 属性
```

**接入**：

```bash
# 加到 build/validate-web.sh 里，这样每次组装都会检查
node build/check-classes.js || exit 1
```

**怎么验证**：

```
· 故意在 JS 里加一个未定义的类，看脚本是否报出
· 跑一遍现有代码，把真实存在的问题列出来（这本身就是有价值的产出）
· 修掉真实问题（如果改动小且安全）
```

---

### 任务 4：前端 JS 分节索引

**背景**：`/tmp/final-js.txt` 约 6800 行，单文件无构建步骤（刻意选择）。
用注释分节（`// ===== 页面：XXX =====`），但没有总索引。

**要做的**：在文件**开头**加一个总索引注释，列出各部分的起始行号。

**格式示例**：

```javascript
// ============================================================
// MiCam 前端 — 分节索引
// ============================================================
//
//   工具函数            L    1 -  320
//   全局状态            L  321 -  420
//   路由与菜单          L  421 -  560
//   页面：首页          L  561 - 1100
//   页面：设备列表      L 1101 - 2200
//   页面：实时预览      L 2201 - 3100
//   页面：多路预览      L 3101 - 3800
//   页面：事件中心      L 3801 - 5400
//   页面：回放          L 5401 - 6300
//   页面：AI 智能       L 6301 - 8600
//   页面：设备详情      L 8601 - 9300
//   页面：存储          L 9301 - 9800
//   页面：账号          L 9801 - 10200
//   页面：设置          L 10201 - 10800
//   页面：开发者        L 10801 - 11300
//
// 改代码前先看这里，能快速定位。
// 行号会随改动漂移，用「查找注释」更可靠：搜 `// ===== 页面：XXX =====`
// ============================================================
```

**要求**：

```
· 行号必须准确（写个脚本自动生成，不要手算）
· 索引本身要说明「行号会漂移，建议搜注释」
· 不要改变任何代码逻辑 —— 只加注释
```

**加分项**：顺便写 `build/gen-js-index.js`，可重复生成索引。

---

### 任务 5：docs/_archive/ 归置历史文档（已完成，仅需确认）

**现状**：已经做过了。

```bash
ls /root/Deepseek/micam/docs/_archive/
```

应该能看到 6 个文件：

```
REPLY-TO-MAC-V2.md
REPLY-TO-MAC-V3.md
reply-to-mac.md
xiaomi-ptz-protocol.md
xiaomi-ptz-protocol-v2.md
easynvr-design-spec.md
```

**要做的**：

```
1. 确认这 6 个文件在 _archive/ 里
2. 在 docs/README.md 里确认已标注「历史版本见 _archive/」
3. 如有其他明显重复的历史文档，一并归档
```

---

## 交付要求

### 每项任务完成后

```bash
# 1) 编译
export PATH=/usr/local/go/bin:$PATH GOPROXY=https://goproxy.cn,direct
cd /root/Deepseek/micam && go build -o /tmp/micamd ./cmd/micamd

# 2) 前端校验（如果改了前端）
node build/assemble-web.js && bash build/validate-web.sh

# 3) 部署（后台任务！）
bash /tmp/deploy-step.sh

# 4) 验证（截图 + 实际看图）
```

### 提交

```bash
cd /root/Deepseek/micam
git add -A
git commit -m 'refactor: <任务说明>'
git push
```

**提交信息要求**：

```
· 说明改了什么、为什么改
· 如果修了 bug，写清根因
· 用中文（项目惯例）
```

### 版本号

如果这一批改动值得发版，同步更新三处版本号并升一个小版本（0.38.0 -> 0.39.0）：

```
platform/fnos/manifest
cmd/micamd/main.go
/tmp/deploy-step.sh
```

---

## 重要提醒

```
1. **不要扩大改动范围**
   这 5 项是质量改进，不是重构。只做要求的，不要顺手改周边。

2. **一定要验证，不能凭印象**
   改完前端要截图并实际查看图片（用 read_image 工具）。
   历史教训：多次说「改好了」但实际没生效或不符预期。

3. **部署用后台任务**
   前台部署会在 120 秒被截断，导致静默失败。

4. **遇到不确定的先问**
   特别是涉及 API 语义变化、数据格式变化时。

5. **保留证据**
   把脚本输出、截图路径写进提交信息或回复里。
```

--- 提示词结束 ---

## 使用说明

```
1. 复制上面两段标记之间的全部内容
2. 粘贴到新窗口
3. 新窗口会自己读仓库、编译、部署、验证
4. 完成后把结果发回
```

如果新窗口需要更多背景，让它读这几个文件：

```
/root/Deepseek/micam/docs/README.md          文档索引
/root/Deepseek/micam/docs/CODE-REVIEW.md     审查报告（这 5 项的出处）
/root/Deepseek/micam/docs/DESIGN.md          整体设计
/root/Deepseek/micam/docs/PAGES-SPEC.md      页面规格
```
