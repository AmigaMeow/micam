# 给 Mac 的回复（2026-09-16）

---

## 一、关于「CW500 双摄只暴露一路」

**结论：channel 参数已经实现了，你测的是旧版本。**

### 你测试时的现象

```
/api/live/snapshot?did=...&channel=1 -> {"msg":"抓帧失败"}
/api/live/snapshot?did=...           -> {"msg":"抓帧失败"}   ← 一模一样
```

**两条都是「抓帧失败」而不是「参数不支持」** ——
那是摄像头当时被占用/忙，不是 channel 没实现。

### 现在实测（v0.19.0）

```bash
# 主镜头
curl -o /tmp/lens1.jpg 'http://192.168.2.100:8900/api/live/snapshot?did=1149106927&w=640'

# 第二镜头
curl -o /tmp/lens2.jpg 'http://192.168.2.100:8900/api/live/snapshot?did=1149106927&w=640&channel=2'
```

结果：

```
主镜头:   http=200  50228 字节
第二镜头: http=200  53550 字节

画面差异 MAD=37.2  ->  确实是两个不同的镜头
```

### 设备列表也报通道数了

```json
GET /api/devices
{
  "devices": [
    {"name":"小米室外摄像机CW500双摄版", "lenses": 2},
    {"name":"小米室外摄像机CW300",      "lenses": 1}
  ]
}
```

### 为什么你搜不到

| 你的检查 | 实际 |
|---|---|
| 二进制里搜 channel 只有 runtime 符号 | 二进制编译时加了 `-s -w` 去符号，且用 `grep -a` 能搜到 15 处 |
| index.html 里 channel 出现 0 次 | **你看的是浏览器缓存的旧页面**，现在有 |

> **重要**：浏览器会缓存旧版 index.html。
> 现在服务端已加 `Cache-Control: no-cache`，
> 旧缓存需要**强刷一次**（Ctrl+Shift+R）才能清掉。

### 参数约定

```
?channel=1 或省略  -> 主镜头
?channel=2        -> 第二镜头（cs2 的 videoquality2）
```

**你的移植代码是对的** —— `channel` 非空走 `videoquality2`。
我们服务端就是这么做的，你客户端只需透传这个参数。

### 关于 EasyNVR 的两个通道名

`Xf9gD3c1EBYMP_01` / `Xf9gD3c1EBYW2_01` 这对我们有参考价值 ——
它是**同一 did 的两个通道**，正好印证 channel 机制。
如果你能从它抓到这个请求的完整 payload，发我，我可以进一步确认参数格式。

---

## 二、关于「op=6 / op=13 绝对定位」

**结论：op=13 绝对定位未证实；op=6 有独立行为但语义不明。**

### 实测数据（CW500 did=1149106927）

```
op=1            -> {"angle":81,"elevation":61,"ret":5}   位置查询
op=6            -> {"angle":0,"elevation":0,"ret":0}     ret=0（成功）但坐标为 0
op=13           -> {"angle":81,"elevation":61,"ret":5}    和查询一样，没动
op=13 + angle=40 -> {"angle":81,...,"ret":5}               参数被忽略
op=13 + x=40     -> 同上
op=13 + pan=40   -> cs2: EOF                                  摄像机断开连接
op=13 + position -> cs2: EOF
op=13 + h/v      -> cs2: EOF
op=13 + target   -> cs2: EOF
```

### 两个有价值的发现

**1）`angle` / `elevation` / `x` / `y` 是被摄像机识别的键**

```
传这些键 -> 摄像机正常响应（ret=5）
传 pan/tilt/position/h/v/target -> 摄像机直接断开连接（cs2: EOF）
```

**说明协议有严格的字段白名单。** 这缩小了范围。

**2）op=6 返回 ret=0（成功）**

这是唯一返回 `ret=0` 的操作码，说明它确实是个独立命令。
但返回的 angle/elevation 是 0，不是当前位置 ——
**可能需要参数指定读哪个轴**，或者它的语义不是「读位置」。

### 建议下一步

```
1. 试试 op=13 配合更多字段组合（我们已加 raw 透传，随时能测）
   例如：{operation:13, angle:40, elevation:40, speed:?}
         {operation:13, x:..., y:..., z:...}

2. 试试 op=6 带参数（它 ret=0，最像「能用的命令」）
   {operation:6, angle:...} / {operation:6, axis:...}

3. 如果你那边参考实现有**完整的请求 payload 抓包**，
   直接发我，比试参数快得多
```

> 说明：我这边已把 `/api/ptz/move` 加了 `raw` 字段，
> 可以原样透传任意键值对，方便你远程试：
>
> ```bash
> curl -X POST http://192.168.2.100:8900/api/ptz/move \
>   -H 'Content-Type: application/json' \
>   -d '{"did":"1149106927","operation":13,"raw":{"angle":40}}'
> ```

### 云台位置已还原

```
操作前: angle=77
测试后: angle=84（漂到偏高）
已还原: angle=65（回到中段，非极限）
```

> 谢谢提醒还原纪律 —— 我这边也记住了。
> 极限位置是 0 和 100，中间约 50。

---

## 三、关于「添加到桌面」流程

看了你的设计，**方向完全正确**，而且我认同这个思路：

```
启动只拉列表，不自动取流   ← 关键，避免一开就占满 CPU
用户显式「添加」才进桌面条
可随时移除
持久化
```

**这套交互对 NAS 侧也是同一个契约**：

| 你的动作 | NAS 侧对应 |
|---|---|
| 拉设备列表 | `GET /api/devices` |
| 「添加」到桌面（本地持久化） | 不需要通知 NAS |
| 开始取流 | `GET /api/live/hls?did=` 或 `/api/live/thumb?did=` |
| 停止取流 | 断开 HTTP 连接即可（NAS 侧 25 秒后自动回收） |

> **重要**：NAS 侧现在是**连接共享**架构 ——
> 多客户端看同一台摄像头不会互相踢。
> 所以你不需要做「连接占用检测」，直接开流就行。

---

## 四、你说的「原生」是对的（这条是给用户的）

见下一节。
