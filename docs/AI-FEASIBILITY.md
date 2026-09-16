# 本地 AI 人形识别 —— 可行性与架构设计

> 2026-09-16 · 全部数据来自本机实测

---

## 零、结论

```
✅ 可行，而且比预想的好得多
   飞牛自带 onnxruntime 1.19 + opencv 4.10
   YOLOv8n 推理实测 260ms（4 核 CPU）

✅ 完全支持开关
   关闭后零开销、零依赖，退化为纯帧差
```

---

## 一、关键发现：飞牛自带 AI 运行时

探路过程中发现的：

```
/vol1/@sysappmeta/ai-manager/python/3_11_x86/
  lib/python3.11/site-packages/
    onnxruntime      1.19.0   <- 含 TensorRT/CUDA/CPU provider
    opencv_python    4.10.0
    numpy            2.0.2
    openvino / faiss / ...
  models/
    face-model / img2vec / txt2vec
```

**权限验证通过**：

```
micam 用户可读 onnxruntime      ✅
micam 用户可执行 python3        ✅
```

### 这意味着什么

| 原以为 | 实际 |
|---|---|
| 要自己写推理引擎 | ❌ 不需要 |
| 要 cgo + gcc | ❌ 不需要 |
| 要打包运行时（几十 MB） | ❌ 不需要 |
| 要用户装 Python 包 | ❌ 不需要 |
| **只需设 PYTHONPATH 调现成的** | ✅ |

---

## 二、实测数据

### 2.1 YOLOv8n 推理速度

```
模型:     yolov8n.onnx  12.3 MB（COCO 80 类，含 person/car/truck）
输入:     [1, 3, 640, 640]
输出:     [1, 84, 8400]
provider: CPUExecutionProvider（4 线程）

加载:     0.14 秒（只加载一次，常驻内存）
推理:     257 / 269 / 267 ms   <- 稳定
```

**260ms 完全可以接受** —— 只在帧差触发时才跑，不是每帧都跑。

### 2.2 我们自己写推理引擎的对比（这条路已放弃）

```
朴素卷积:       0.73 GFLOP/s  ->  YOLOv8n 要 11.9 秒  ❌
im2col + 4 核:  2.25 GFLOP/s  ->  YOLOv8n 要 3.9 秒   ❌
```

**结论：不该重复造轮子。**

### 2.3 onnx-go（纯 Go 库）也不可行

```
解析 yolov8n.onnx -> No data found
原因：onnx-go 对 YOLOv8 的算子支持不足
```

---

## 三、架构设计

### 3.1 三层降级（保证任何环境都能用）

```
第 1 层：帧差侦测（永远可用）
  复用缩略图缓存，零额外开销
  输出：type=motion

        ↓ 如果 AI 开启且环境可用

第 2 层：AI 分类（可选）
  触发时把截图送 YOLOv8n
  输出：type=person / vehicle
        + conf（真实置信度）+ bbox（可画框）

        ↓ 如果 AI 不可用

第 3 层：自动降级
  记录一条「AI 不可用」的原因，继续用第 1 层
  前端显示「AI 未启用/不可用」，不报错
```

### 3.2 开关设计

```json
{
  "ai": {
    "enabled": false,
    "mode": "detect",
    "model": "yolov8n",
    "inputSize": 640,
    "confThreshold": 0.45,
    "classes": ["person","car","truck","bicycle","motorbike"],
    "maxPerMinute": 12,
    "pythonPath": "auto"
  }
}
```

| 字段 | 说明 |
|---|---|
| `enabled` | 总开关，**默认关闭** |
| `mode` | detect=目标检测 / classify=仅分类 |
| `classes` | 关心的类别（过滤掉其它） |
| `maxPerMinute` | 限流，防 CPU 打满 |
| `pythonPath` | auto=自动探测飞牛 AI 目录 |

### 3.3 关闭时的行为

```
· 不加载模型（省 12MB 内存 + 0.14s 启动时间）
· 不启动 Python 进程
· 事件类型只有 motion
· 对主功能零影响
```

---

## 四、实现方案

### 4.1 为什么用「常驻 Python 子进程」

```
每次调用 python3:  启动解释器 ~300ms + 加载模型 140ms = 440ms  太慢
常驻子进程:        启动一次，之后每次 260ms                 可接受
```

### 4.2 通信协议

```
Go 主进程 <--stdin/stdout JSON 行--> Python 推理进程

请求: {"id":1,"image":"<base64 jpeg>"}
响应: {"id":1,"objects":[{"cls":"person","conf":0.87,"box":[x,y,w,h]}]}
```

**为什么不用 HTTP**：本地进程通信用 stdin/stdout 更简单、无端口冲突。

### 4.3 目录结构

```
平台侧（随插件分发）：
  /vol1/@appcenter/micam/ai/
    infer.py      推理服务
    detect.py     YOLOv8 后处理（NMS 等）

数据侧（用户数据）：
  /vol1/@appdata/micam/models/
    yolov8n.onnx  模型（可选下载）
```

**模型不进安装包** —— 12.3MB 会让 fpk 变大，改成首次启用时下载（带镜像源）。

---

## 五、风险和应对

| 风险 | 应对 |
|---|---|
| 用户没装飞牛 AI 应用 | 自动探测路径；找不到就提示「AI 不可用」，主功能不受影响 |
| 飞牛升级改路径 | 多路径探测 + 允许手动指定 `pythonPath` |
| CPU 被 AI 打满 | 限流 + 单进程串行 + 可调频率 |
| 模型下载失败 | 内置多个镜像源；失败就保持帧差模式 |
| 推理进程崩溃 | 自动重启（最多 3 次）；连续失败就关闭 AI 并通知 |
| 开源用户环境各异 | **默认关闭**，用户显式开启 |

---

## 六、能识别什么

YOLOv8n 是 COCO 80 类模型，与本项目相关的：

| COCO 类名 | 映射到我们的事件类型 |
|---|---|
| person | **person（有人）** |
| car / truck / bus | **vehicle（车辆）** |
| bicycle / motorbike | vehicle |
| cat / dog | animal（可选） |
| backpack / handbag / suitcase | package（近似） |

> 注意：**COCO 没有「包裹」类**。快递盒会被识别成 suitcase 或忽略。
> 要准确识别包裹需要专门模型（未来可选）。

---

## 七、性能预算（4 核 NAS，3 台摄像头）

```
场景：3 台摄像头，每台平均每小时触发 4 次 AI

AI 调用:  3 * 4 = 12 次/小时
单次耗时: 0.26 秒
总 CPU:   12 * 0.26 / 3600 = 0.09%     几乎无感

极端场景（一直有人走动）:
  3 * 60 = 180 次/小时  ->  CPU 1.3%
  且有限流保护（maxPerMinute=12）
```

**CPU 不是问题。**

---

## 八、下一步

```
1. 写 infer.py（推理服务）
2. 写 Go 侧的进程管理 + JSON 通信
3. 接入事件系统（type 从 motion 升级）
4. 加配置项 + 设置页开关 + AI 状态显示
5. 实测准确度（用真实截图验证）
```
