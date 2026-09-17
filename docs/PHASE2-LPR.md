# 阶段 2：本地车牌识别（HyperLPR3）实测

> 2026-09-17 · 目标车牌（人工确认）：**鄂W·61J59**

---

## 一、结论速览

```
方案        耗时      费用    联网    同一车牌识别结果
本地 HyperLPR  133ms   免费    否      鄂W61J59 (0.993) / 鄂W61J59 (0.976) / 粤W61J59 / 粤H61J59
云端视觉模型   3~5s    ¥0.003  是      粤W·GL659 / 皖G·6J59 / 苏W·GLJ59 / 苏G·J59  全错
```

**本地完胜：快 25 倍、免费、离线，且准确率明显更高。**

---

## 二、实测数据

| 输入 | 尺寸 | 耗时 | 检出 | 结果 | 置信度 | 正确? |
|---|---|---|---|---|---|---|
| 车牌裁剪 | 260x110 | 139ms | 1 | 鄂W61J59 | 0.993 | ✅ |
| 整幅 | 1024x576 | 133ms | 1 | 鄂W61J59 | 0.976 | ✅ |
| 整幅 | 1280x720 | 132ms | 1 | 粤W61J59 | 0.954 | ❌ 省份错 |
| 划痕样本 | 1024x576 | 133ms | 1 | 粤H61J59 | 0.895 | ❌ 省份错 |
| 整幅 | 2560x1440 | 141ms | 0 | 未检出 | - | - |

### 关键观察：**只有省份汉字不稳定**

```
数字字母部分「61J59」  ->  5 次全部正确，100% 稳定
省份汉字              ->  鄂 / 粤 / 粤H 三个答案
```

**多数投票会选出「鄂W61J59」**（2 票 vs 1 票 vs 1 票）—— 正确答案。

这**实测验证了多帧投票的必要性**，不是过度设计。

### 分辨率的影响

```
2560px  车牌相对画面太小 -> 检不出（需要先裁剪放大）
1280px  能检出，省份错
1024px  能检出，完全正确
260px   直接裁车牌，最准（0.993）
```

**结论：送进 LPR 之前应该先做车牌区域裁剪**，而不是丢整幅大图。

---

## 三、部署要点（踩过的坑）

### 3.1 包与模型

```
库:   hyperlpr3.0-share-linux-x86.zip (9.76MB)
        -> lib/libhyperlpr3.so  (22.7MB, 内含 MNN 推理引擎)
        -> include/hyper_lpr_sdk.h

模型: resource/models/r2_mobile/*.mnn (共 8.4MB)
        b320_backbone_h.mnn      0.93MB
        b320_header_h.mnn        0.32MB
        b640x_backbone_h.mnn     0.93MB
        b640x_head_h.mnn         1.08MB
        litemodel_cls_96xh.mnn   0.78MB
        rpv3_mdict_160h.mnn      4.29MB
```

仓库: https://github.com/szad670401/HyperLPR

### 3.2 坑 1：模型文件名版本不匹配

```
发布的 .so 是 v3.0（2023），期望 rpv3_mdict_160_r3.mnn
但仓库 master 里叫 rpv3_mdict_160h.mnn

-> 直接复制改名即可（模型本身兼容）
   实测改名后 status=0，识别正常
```

### 3.3 坑 2：CreateDataBuffer 的签名

```c
// 错误理解（5 个参数）
HLPR_CreateDataBuffer(data, w, h, format, rotation)

// 实际签名（结构体指针）
HLPR_CreateDataBuffer(P_HLPR_ImageData data)

typedef struct HLPR_ImageData {
    uint8_t *data;
    int width;
    int height;
    HLPR_ImageFormat format;
    HLPR_Rotation rotation;
} HLPR_ImageData;
```

按 5 参数调用会导致 `cv::Exception: Insufficient memory` 崩溃
（结构体被当垃圾解析，算出了 9.6e17 字节的分配请求）。

### 3.4 调用方式（Go 不能用 cgo，走 Python ctypes）

```python
lib = ctypes.CDLL('libhyperlpr3.so')
ctx = lib.HLPR_CreateContext(byref(cfg))          # status=0 即成功
buf = lib.HLPR_CreateDataBuffer(byref(imgdata))
lib.HLPR_ContextUpdateStream(ctx, buf, byref(results))
```

---

## 四、集成设计（阶段 2 落地）

### 4.1 分层：本地优先，云端兜底

```
1. 本地 HyperLPR 读（133ms，免费）
     ↓ 有多帧结果
2. 多帧投票（同一车牌读 N 帧，取多数）
     ↓
3. 格式校验（省简称必须是 31 个合法值之一）
     ↓
4. 置信度门槛（低于阈值标「待确认」）
     ↓ 只有在本地读不出/冲突时
5. 云端视觉模型兜底（花钱，但适应性强）
```

### 4.2 必须做的校验（实测驱动）

```
✓ 多帧投票       <- 省份字不稳定，投票能纠正
✓ 省简称白名单   <- 31 个：京津冀晋蒙辽吉黑沪苏浙皖闽赣鲁豫鄂湘粤桂琼渝川贵云藏陕甘青宁新
✓ 车牌格式正则   <- ^[省][A-Z][A-Z0-9]{5}$
✓ 字符集排除     <- I / O 不用于车牌
✓ 置信度门槛     <- <0.9 标「待确认」，不当事实
✓ 保留原图       <- 允许人工复核
```

### 4.3 送图策略

```
不要丢整幅 2560px（车牌太小，检不出）
应该：
  1. 用 YOLO 或运动区域先定位可能的车牌区域
  2. 裁剪 + 放大到车牌宽度 200~400px
  3. 再送 LPR

或者直接用 1280px 整幅（实测能检出，质量可接受）
```

---

## 五、与阶段 1a 的关系

```
查找任务（已上线）
  证据里的车牌，可以由 LPR 在【分析时顺便读取】
  写入事件的 aiPlate 字段（带多帧投票结果）

比对任务（阶段 3）
  「我的车被谁碰了」-> 用 LPR 找同一车牌的出现记录
  这是最实用的车牌应用场景
```

---

## 六、体积与依赖

```
libhyperlpr3.so    22.7 MB  (含 MNN 引擎，内嵌部分逻辑)
6 个 .mnn 模型      8.4 MB
合计               31.1 MB

依赖：只需系统标准库（libstdc++ / libm / libc 等）
      实测 ldd 无缺失依赖

是否进 fpk：不进（和 YOLO 模型一样，首次启用时下载）
```
