# 移交：实时预览黑屏 + 云台卡顿 + 控制不同步

> 把「--- 提示词开始 ---」到「--- 提示词结束 ---」之间的内容复制到新窗口。

--- 提示词开始 ---

# 任务：修复实时预览黑屏 + 云台性能（含全部诊断数据）

## 项目背景

MiCam 是**飞牛 NAS（fnOS）的小米摄像头插件**，开源在 `https://github.com/AmigaMeow/micam`。

```
代码位置   /root/Deepseek/micam/
分支       master
当前版本   0.40.2
插件地址   http://192.168.2.100:8900/
运行用户   micam
测试设备   小米室外摄像机CW500双摄版  did=1149106927  （真云台）
           小米室外摄像机CW300         did=1105978925
```

### 架构

```
后端   Go（cmd/micamd/ + internal/），单二进制，内置 HTTP（8900）
       CGO_ENABLED=0
前端   单文件 JS：platform/fnos/app/www/app.js
       组装进 index.html（无构建步骤是刻意选择：飞牛网关会重写相对路径）
       /tmp/final-js.txt 是指向它的软链接
视频   小米 cs2 P2P 协议 -> ffmpeg -> HLS 分片
```

### 硬约束

```
1. 不要引入构建步骤（webpack/vite/tsc）
2. 不要引入 cgo
3. 不要引入第三方 CSS 框架（曾引入 Basecoat 导致 17 个类名冲突）
4. 前端追加代码必须【自包含】—— 见下方「重要陷阱」
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

### 前端改动

```bash
# 1) 编辑 platform/fnos/app/www/app.js
# 2)
cd /root/Deepseek/micam
node build/assemble-web.js      # 内联进 index.html
bash build/validate-web.sh       # 校验
```

### 部署（**必须后台任务**）

```bash
bash /tmp/deploy-step.sh
```

**⚠️ 前台运行会在 120 秒被截断，导致部署静默失败。**
用 run_in_background，等 55~60 秒收结果。

### 版本号（发版时 3 处同步）

```
platform/fnos/manifest          version = 0.40.2
cmd/micamd/main.go              var appVersion = "0.40.2"
/tmp/deploy-step.sh             main.appVersion=0.40.2
```

### 视觉验证

```bash
export PATH=/var/apps/nodejs_v24/target/bin:$PATH
cd /root/shot
node shot.js live /tmp/shots/x.png 16000     # 实时预览页
# 页面: - / devices / live / grid / events / settings / recordings / ai
```

Playwright + Chrome：`/root/shot/chrome/chrome-linux64/chrome`

**验证纪律**：改完必须截图并**用 read_image 实际查看图片**，不能只看 API 返回。

### ⚠️ 重要陷阱：前端追加代码必须自包含

实测发现：**应用主体脚本的作用域被包裹**，`window` 上只有新加的函数，
`esc` / `api` / `icon` / `render` **全部不在 window 上**。

```
验证方式（在浏览器里）：
  Object.getOwnPropertyNames(window).filter(k=>/^(esc|api|render)$/.test(k))
  -> []  （空！）
```

**所以追加到 app.js 末尾的代码不能直接调 `api()` / `esc()` / `icon()`，
否则 ReferenceError。** 要么写在 app 作用域内，要么自包含。

参考：`initTopNav` 那一段就是自包含的（自己实现 `_tnEsc` / `_tnApi` / `_tnIcon`）。

### 并发警告

```
可能同时有另一个会话在改同一仓库。改完【立刻 commit】。
```

---

# 问题 1：实时预览黑屏（P0，会复发）

## 现象

```
打开实时预览 -> 播放器区域全黑，但显示 LIVE 角标
状态面板显示「模式 MJPEG」，角标显示 HLS（不一致）
多等一会也不出画面
```

## 诊断数据（实测日志）

### 时间线

```
06:55:38  [feed] 1105978925 已写入 9 MB       <- 正常
06:55:44  [feed] 1105978925 已写入 10 MB
06:55:50  [feed] 1105978925 已写入 11 MB      <- 正常写入中
06:55:57  [enc] h264_amf 不可用: DLL libamfrt64.so.1 failed to open
          [h264_amf] Failed to create hardware device context (AMF)
          Device creation failed: -22.
          Failed to set value '/dev/dri/renderD128' for option 'qsv_device'
06:56:54  [hub] 启动 producer 失败: miss: read media: cs2: pop buffer is full
07:02:28  GET /api/live/hls 24.143857015s      <- 请求卡 24 秒后失败
07:03:17  GET /api/live/hls 24.111242149s
07:05+    [feed] 1149106927 结束，共写入 376 字节   <- 只剩 376 字节
```

### 关键证据

```
1. 正常时：feed 写入 9~11 MB
   故障时：feed 只写 376 字节（约等于没有数据）

2. /vol1/@appdata/micam/hls/<did>/ffmpeg.log 是 0 字节
   （说明 ffmpeg 根本没拿到输入）

3. /api/live/hls 返回：{"msg":"播放列表尚未生成，请稍候重试"}
   而且要等 24 秒才返回（应该快速失败）

4. 重启服务后立即恢复：
     index.m3u8  126 字节（正常）
     seg0.ts     1,052,800 字节（1MB，正常分片）
     ffmpeg.log  1680 字节（有内容了）
```

## 根因推断

```
GPU 硬编（VAAPI /dev/dri/renderD128）失败
  -> 消费端（编码器）停了
    -> P2P 缓冲区填满（cs2: pop buffer is full）
      -> 生产者写不进去
        -> 整个管线卡死
          -> HLS 无法生成分片 -> 黑屏
```

## 要做的

### A. 管线自动恢复（最重要）

```
· 检测卡死：feed 连续 N 次写入 < 1KB  -> 判定卡死 -> 自动重建管线
· 编码器失败时自动降级：qsv/vaapi 失败 -> 回退 libx264 软编并重试
· 加冷却时间，避免无限重建
· 记日志说明「检测到管线卡死，正在重建」
```

### B. HLS 快速失败

```
现状：请求要等 24 秒才返回「播放列表尚未生成」
要求：3~5 秒内失败，并触发管线重建
      前端收到失败后显示「正在重连…」而不是一直转圈
```

### C. 硬件编码探测健壮性

```
现状：硬编失败后发现太晚（已经卡死）
要求：
  · 启动时探测一次可用编码器（探测失败就记下来，别每次试）
  · 探测要真正跑一次编码（不只是看 /dev/dri 存在）
  · 已实现的探测在 /api/live/hls/info（返回 encoders 列表）
```

### D. 模式显示一致性（前端）

```
现状：HLS 失败退回 MJPEG 时，只有部分 UI 更新
      状态面板显示 MJPEG，角标还是 HLS

相关代码（app.js）：
  4024 行  mode.textContent = liveMode === 'hls' ? 'HLS' : 'MJPEG'
  4090 行  lq.textContent = qv + ' · ' + (liveMode === 'hls' ? 'HLS' : 'MJPEG')
  4094 行  cm.textContent = liveMode === 'hls' ? 'HLS' : 'MJPEG'
  4291 行  liveMode = "mjpeg"   <- 失败退回时只改了变量

要求：加一个 refreshModeUI() 统一更新所有模式显示，
      退回时调用它。
```

---

# 问题 2：云台卡顿（P0）

## 现象

```
点方向键 -> 要等好久（2~3 秒）才动
连点几下 -> 反应非常慢，甚至像卡住
```

## 诊断：当前实现是「闭环绕路」

```
每条命令的流程：
  1. 读当前角度（P2P 命令 0x112 op=1）
  2. 计算与目标的角度差
  3. 发一步相对步进（ptzStepDeg = 2.7 度）
  4. 等 700ms（因为步进响应滞后一个命令）
  5. 再读一次角度（验证）
  6. 没到目标就回到第 3 步

实测：走 6~7 步，一条命令 2~3 秒
```

## 为什么这么设计

```
因为【绝对定位不可用】—— 实测过：
  · op=13（绝对定位）：参数被忽略，或直接 cs2: EOF 断开
    字段白名单：angle/elevation/x/y 能传，但似乎不生效
    传 pan/tilt/position/h/v/target -> 直接断开
  · op=6：返回 ret=0 但实际不停止/不复位，语义未知

所以只能靠相对步进逼近。
```

## 相关代码位置

```
cmd/micamd/ptz.go           云台命令实现
cmd/micamd/ptz_preset.go    预置位（闭环保存/跳转）
  tolDeg = 1                容差
  ptzStepDeg = 2.7          每步角度
  700ms                     步进后等待
```

## 要做的（按性价比排序）

```
① 前端乐观更新（改动最小、体感提升最大）
   点击方向键立即移动指针 + 显示「移动中…」
   后台发命令，完成后校正指针
   用户感觉「立刻响应」

② 连点合并
   连点 5 次右 -> 合并成一个「走 5 步」命令
   而不是发 5 条独立命令（每条 2~3 秒）
   实现：前端加 200ms 防抖，累积步数后一次发

③ 减少闭环开销
   · 只在最后读一次验证，不要每步都读
   · 700ms 等待可以试 300~400ms（实测调）
   · 步进失败就提前退出，别硬撑到超时

④ 命令队列串行化
   保证同一设备同时只有一条 PTZ 命令在执行
   新命令排队而不是并发（并发会争 P2P 连接）
```

## ⚠️ 重要：P2P 单槽位约束

```
实测结论：摄像头只有【一个 P2P 连接槽】
  实时预览 / 动检侦测 / 云台命令 / LPR 抽帧 会互相抢占

表现：
  · 云台命令 i/o timeout
  · 转弯中途断开（实测走 7 步后断，差 31 度）
  · 保存预置位报「连接摄像头失败（可能被视频占用）」

这是设备/协议限制，不是软件 bug。
所以 PTZ 优化必须考虑「视频正在占用连接」这个前提。
```

---

# 问题 3：控制与画面不同步

## 现象

```
云台角度读数与实际画面不符
点方向键后画面变化与预期方向不一致
```

## 根因

```
① 角度读数有 1~2 个命令的延迟
   步进响应滞后：发命令后立即读，读到的是上一个状态
   这就是为什么原实现要等 700ms

② P2P 争用导致命令丢失/超时
   视频在占连接时，PTZ 命令可能超时
   超时后角度没变，但前端以为变了

③ ret 值语义不统一（实测）：
   CW300  ret=0
   CW500  ret=5
   到达限位 ret=-1001
   -> 客户端【不能】用 ret==0 判断成功
```

## 要做的

```
· 前端显示「移动中」状态，完成前不更新读数
· 命令超时明确提示「云台被视频占用，请稍后重试」
· 保存/跳转预置位前检查视频是否在播放，提示用户
· 若可能，PTZ 操作时短暂暂停视频（但注意用户体验）
```

---

# ⚠️ 测试后必须还原摄像头画面

```
测试 PTZ 后一定要把摄像头调回原来的视角！

当前位置（本次记录）：
  angle = 87    elevation = 69
  （读数有噪声，历史值：96 -> 84 -> 87，俯仰始终 69）

已保存的预置位：
  槽位 1「测试位」  angle=65  elevation=61

期望视角：正对门口（能看到对联、木门、福字，右上角有汽车和绿顶棚）
```

**测试前先记录位置，测试后还原。**

---

# 验收标准

```
问题 1：
  [ ] 打开实时预览 5 秒内出画面（不是黑屏）
  [ ] 人为制造编码器失败（或模拟）后，管线能自动恢复
  [ ] HLS 失败在 5 秒内返回，不是 24 秒
  [ ] 退回 MJPEG 时，面板/角标/工具栏三处模式显示一致

问题 2：
  [ ] 点方向键 < 500ms 内有视觉反馈（乐观更新）
  [ ] 连点 5 次不会发 5 条独立命令
  [ ] 一条 PTZ 命令总耗时明显低于现在的 2~3 秒

问题 3：
  [ ] 命令超时时有明确提示
  [ ] 读数与画面不再明显矛盾（或至少给用户解释）

回归：
  [ ] 8 个页面截图逐一确认无视觉回归
  [ ] node build/assemble-web.js && bash build/validate-web.sh 通过
  [ ] 摄像头画面已还原
```

---

# 参考文档

```
docs/README.md              文档索引
docs/DESIGN.md              整体设计
docs/PERFORMANCE.md         性能数据
docs/STARTUP-PERFORMANCE.md 启动性能
docs/xiaomi-ptz-confirmed.md 云台协议（实测确认版）
docs/MODEL-LIMITS.md        模型与能力边界（含实测教训）
```

--- 提示词结束 ---

## 使用说明

```
1. 复制标记之间的全部内容到新窗口
2. 建议顺序：问题 1（黑屏，最影响使用）-> 问题 2（云台）-> 问题 3
3. 每修完一项立刻 commit
4. 测试完记得还原摄像头画面
```
