# AI 模型选型 —— 三档对比与扩展设计

> 回答用户的三个问题：模型来源、能否选更大的、多模型会不会难适配

---

## 一、这个模型是小米自带的吗？

**不是。这点必须讲清楚。**

| | 来源 | 我们能拿到吗 |
|---|---|---|
| **摄像机内置 AI** | 小米自研，产生 `people-motion` / `detected-car` 等 19 个事件 | ❌ **拿不到**（实测云端 API 全封） |
| **我们用的 AI** | Ultralytics **YOLOv8**，开源 COCO 80 类模型 | ✅ 在 NAS 上跑 |

```
两套 AI 完全独立：
  小米的  -> 在摄像机芯片上跑 -> 结果锁在小米云 -> 我们拿不到
  我们的  -> 在 NAS CPU 上跑   -> 结果进我们的事件库
```

所以**不是「调用小米的 AI」，而是「自己装一套」**。

---

## 二、更大的模型实测（都是本机数据）

### 2.1 速度对比

```
模型        大小     加载     推理      相对速度
yolov8n    12.3 MB   0.12s   255.7 ms   1.0x
yolov8s    42.8 MB   0.25s   751.5 ms   2.9x 慢
```

### 2.2 CPU 占用（3 台摄像头，每小时各触发 4 次）

```
yolov8n:   0.09%
yolov8s:   0.25%
```

**结论：即使 yolov8s 也才 0.25%，CPU 完全不是瓶颈。**

### 2.3 准确度差异（官方 COCO mAP）

| 模型 | mAP@50-95 | 相对 |
|---|---|---|
| yolov8n | 37.3 | 基准 |
| yolov8s | 44.9 | **+20%** |
| yolov8m | 50.2 | +35% |

> yolov8s 的准确度提升是**实质性的**（+20%），不是宣传话术。
> 在监控场景（远景小目标）尤其明显。

---

## 三、多模型会不会难适配？

**如果硬编码几个模型 —— 会很难。**

```
❌ 错误做法：
   if model == "yolov8n" { ... }
   else if model == "yolov8s" { ... }      每个都要改代码
   else if model == "yolov8m" { ... }

问题：
  · 用户想用自己训练的模型 -> 加不进去
  · 版本升级 -> 到处改
  · 开源后各种模型需求 -> 永远追不完
```

```
✅ 正确做法：模型注册表 + 统一接口
```

### 3.1 核心思路

**所有 YOLO 系列（v5/v8/v11）的输出格式几乎一样**：

```
  yolov8/v11:  [1, 84, 8400]   (4 个坐标 + 80 类分数)
  yolov5:      [1, 25200, 85]  (5 个框参数 + 80 类分数，需转置)
```

只要**自动识别输出格式**，就能支持一大类模型：

```python
# 伪代码
def normalize_output(raw, input_size):
    if raw.shape[1] < raw.shape[2]:
        raw = raw.transpose(0, 2, 1)   # yolov5 风格 -> 统一成 [N, 84]
    # 现在统一是 [N, 4+num_classes]
    boxes = raw[:, :4]
    scores = raw[:, 4:]
    return boxes, scores
```

### 3.2 配置驱动的模型注册表

```json
{
  "ai": {
    "enabled": false,
    "activeModel": "yolov8n",
    "models": {
      "yolov8n": {
        "file": "yolov8n.onnx",
        "inputSize": 640,
        "classes": ["person","bicycle","car","motorbike","bus","truck"],
        "builtin": true
      },
      "yolov8s": {
        "file": "yolov8s.onnx",
        "inputSize": 640,
        "classes": ["person","bicycle","car","motorbike","bus","truck"],
        "builtin": true
      },
      "custom-mypet": {
        "file": "mypet.onnx",
        "inputSize": 416,
        "classes": ["cat","dog"],
        "builtin": false
      }
    }
  }
}

```

**用户放一个 onnx 文件 + 写几行配置就能用** —— 我们不用改代码。

### 3.3 类名从哪来

```
优先级：
  1. 配置文件里显式写的 classes
  2. 从 ONNX 元数据读（有些模型会内嵌 names）
  3. 回退到 COCO 80 类默认表
```

---

## 四、我的建议：内置 2 档，开放自定义

| 档位 | 模型 | 大小 | 推理 | 适合 |
|---|---|---|---|---|
| **轻量（默认）** | yolov8n | 12 MB | 256ms | 树莓派级设备、追求省资源 |
| **均衡（推荐）** | yolov8s | 43 MB | 752ms | 主流 NAS，准确度 +20% |
| 自定义 | 用户自己的 | - | - | 特殊需求（如专门识别包裹） |

**不内置 yolov8m/l/x** —— 理由：

```
· 85MB / 260ms -> 1.3s，性价比开始下降
· 大多数家庭监控场景 yolov8s 足够
· 用户真需要可以在「自定义」里加
```

---

## 五、模型下载策略

```
不进安装包（避免 fpk 膨胀到 50MB+）

首次启用 AI 时：
  1. 提示「需要下载模型（12MB / 43MB）」
  2. 用户确认后才下载
  3. 带多个镜像源（hf-mirror / 官方）
  4. 下载失败 -> 明确提示 + 保持帧差模式
```

配置里可关：

```json
{
  "ai": {
    "allowDownload": true,     // 关掉则只用手动放置的模型
    "downloadMirror": "auto"   // auto / hf-mirror / direct
  }
}
```

---

## 六、结论

| 问题 | 回答 |
|---|---|
| 是小米自带的模型吗 | **不是**。小米的拿不到，我们自己跑 YOLOv8 |
| 能有更大的吗 | **能**。yolov8s 实测 752ms、CPU 0.25%，准确度 +20% |
| 多模型会不会难适配 | **用注册表就不难**，且用户能加自己的模型 |

### 落地计划

```
1. 内置 yolov8n + yolov8s 两档，设置页可选
2. 模型注册表（配置驱动，不硬编码）
3. 自动识别输出格式（兼容 yolov5/v8/v11）
4. 开放「自定义模型」入口
5. 默认关闭，显式开启
```
