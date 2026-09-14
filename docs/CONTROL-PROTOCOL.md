# 小米设备控制协议（给 Mac 开发用）

> 实测时间：2026-09-15
> 测试环境：飞牛 NAS + 小米账号 1000000001

---

## 一、核心结论（先看这个）

**小米设备有两套控制通道，用途完全不同：**

| 设备类型 | 控制通道 | 实测结果 |
|---|---|---|
| **摄像头** | cs2 P2P 私有协议 | MIoT 全部失败（-704010000） |
| **其他设备**（空调/灯/开关/音箱） | 小米云 MIoT | **全部成功** |

**实测证据**：

~~~
# 摄像头（CW300）
POST /miotspec/prop/get  {"did":"1000000102","siid":2,"piid":1}
-> {"code":-704010000}      属性不存在

# 空调（同一账号）
POST /miotspec/prop/get  {"did":"1000000104","siid":2,"piid":1}
-> {"code":0,"value":false}  成功
~~~

---

## 二、MIoT 协议（非摄像头设备）

### 2.1 接口

~~~
POST https://api.io.mi.com/app/miotspec/prop/get    读属性
POST https://api.io.mi.com/app/miotspec/prop/set    写属性
POST https://api.io.mi.com/app/miotspec/action      调用动作
~~~

### 2.2 请求格式（关键！）

**did 必须放在 params 数组的每一项里，不能放顶层。**

正确：

~~~json
{"params":[{"did":"1000000104","siid":2,"piid":1}]}
~~~

错误（会返回 -704090001）：

~~~json
{"did":"1000000104","params":[{"siid":2,"piid":1}]}
~~~

### 2.3 响应格式

~~~json
[
  {"did":"1000000104","siid":2,"piid":1,"code":0,"value":false,"exe_time":0}
]
~~~

### 2.4 错误码

| 错误码 | 含义 | 排查 |
|---|---|---|
| 0 | 成功 | - |
| -704090001 | did 格式错误 | 检查 did 是否在 params 里 |
| -704040002 | 服务（siid）不存在 | 该设备没有这个服务 |
| -704040003 | 属性（piid）不存在 | 服务有但属性 ID 不对 |
| -704010000 | 属性不存在 | 同上，另一种返回 |
| -704220043 | 设备离线 | 检查设备在线状态 |

### 2.5 写属性

~~~json
{"params":[{"did":"1000000104","siid":2,"piid":1,"value":true}]}
~~~

响应：

~~~json
[{"did":"...","siid":2,"piid":1,"code":0}]
~~~

### 2.6 调用动作

~~~json
{"params":[{"did":"...","siid":5,"aiid":1,"in":[]}]}
~~~

---

## 三、属性 ID 怎么查（关键）

**不同型号的 siid/piid 完全不同**，必须查 miot-spec。

### 3.1 查全量列表（推荐）

~~~bash
curl 'https://miot-spec.org/miot-spec-v2/instances?status=all' > all.json
# 在 all.json 里按 model 搜索，拿到 urn
# 例："urn:miot-spec-v2:device:camera:0000A01C:mxiang-moc001:1"

# 再用 urn 拉详情
curl 'https://miot-spec.org/miot-spec-v2/instance?type=<urn>'
~~~

### 3.2 常见类型 ID

| 类型 | ID |
|---|---|
| camera | 0000A01C |
| air-conditioner | 0000A004 |
| light | 0000A002 |
| switch | 0000A005 |
| speaker | 0000A007 |
| hood（油烟机） | 0000A00F |
| sensor_ht（温湿度） | 0000A00A |

### 3.3 spec 结构

~~~json
{
  "services": [
    {
      "iid": 2,                       <- siid
      "description": "Air Conditioner",
      "properties": [
        {
          "iid": 1,                   <- piid
          "description": "Switch Status",
          "format": "bool",
          "access": ["read","write","notify"]
        }
      ],
      "actions": [
        {"iid": 1, "description": "Toggle", "in": [], "out": []}
      ]
    }
  ]
}
~~~

**只对 access 含 "write" 的属性做控制。**

---

## 四、摄像头协议（cs2 P2P）

### 4.1 为什么不用 MIoT

摄像头固件**不实现标准 MIoT 属性表**：

~~~
siid=2 piid=1 -> -704010000（属性不存在）
即使属性 ID 来自官方 miot-spec，设备仍拒绝
~~~

### 4.2 走 P2P 私有协议

已知命令：

| 命令 | 值 | 用途 | 状态 |
|---|---|---|---|
| cmdAuthReq | 0x100 | 认证 | 已实现 |
| cmdAuthRes | 0x101 | 认证响应 | 已实现 |
| cmdVideoStart | 0x102 | 开始视频 | 已实现 |
| cmdVideoStop | 0x103 | 停止视频 | 已实现 |
| cmdAudioStart | 0x104 | 开始音频 | 已实现 |
| cmdAudioStop | 0x105 | 停止音频 | 已实现 |
| cmdSpeakerStart | 0x106 | **对讲开始** | 命令已知，未实现 |
| cmdSpeakerStop | 0x108 | 对讲停止 | 命令已知，未实现 |
| cmdStreamCtrl | 0x109 | 流控制 | 未实现 |
| cmdDevInfo | 0x110 | 设备信息 | 未实现 |
| **cmdMotor** | **0x112** | **云台电机** | **已实现** |
| cmdMotorRes | 0x113 | 云台响应 | 已实现 |
| cmdEncoded | 0x1001 | 编码包装 | 已实现 |

### 4.3 命令格式

~~~
payload = [4字节大端命令号] + JSON
外层用 cmdEncoded(0x1001) 包装
用 X25519 共享密钥 ChaCha20 加密

例（云台）：
  buf = 00 00 01 12 || {"operation":2}
  enc = chacha20_encode(buf, sharedKey)
  conn.WriteCommand(0x1001, enc)
~~~

### 4.4 认证

~~~
认证后才发其他命令：
cmdAuthReq(0x100) + {"public_key":"...","sign":"...","uuid":"","support_encrypt":0}
~~~

### 4.5 云台操作码（实测）

| operation | 方向 | 验证 |
|---|---|---|
| 1 | 查询位置 | 返回 angle/elevation |
| 2 | 左 | 已验证 |
| 3 | 右 | 已验证 |
| 4 | 下 | 已验证 |
| 5 | 上 | 已验证 |

响应：

~~~json
{"ret":0, "angle":50, "elevation":73}
~~~

### 4.6 待逆向的命令

要实现**对讲、夜视切换**等，需要：
1. 用米家 App 操作，同时抓 P2P 流量
2. 参考 go2rtc 的 pkg/xiaomi/miss/client.go
3. 命令号可能在 0x114+ 的未使用段

---

## 五、设备发现

### 5.1 自有设备

~~~
POST /v2/home/device_list
{}
~~~

返回 list 数组。

### 5.2 共享家庭设备（关键！）

**必须用 home_device_list，device_list_page 拿不到共享设备。**

~~~json
POST /v2/home/home_device_list
{
  "home_owner": <共享家庭 owner uid>,
  "home_id": <家庭 id>,
  "limit": 200,
  "get_split_device": true,
  "support_smart_home": true
}
~~~

### 5.3 家庭列表

~~~json
POST /v2/homeroom/gethome
{"fg":true, "fetch_share":true, "fetch_share_dev":true, "limit":300, "app_ver":7}
~~~

返回自有家庭 + 共享家庭（shareFlag=1 的是共享）。

### 5.4 实测数据

某账号的完整清单：

~~~
自有设备 (10 台)：
  1. 房间摄像头    did=1000000103    model=chuangmi.camera.029a02  [摄像头]
  2. 抽油烟机         did=1000000105    model=viomi.hood.c1
  3. 老房子一楼门口大灯  did=1125552783   model=bean.switch.blns1
  4. 一楼增压水泵开关    did=1128137626   model=lemesh.switch.sw3g01
  5. CW500 双摄版     did=1000000101   model=isa.camera.hlmax        [摄像头]
  6. 米家智能净烟机Pro   did=2049389602   model=xiaomi.hood.cyp10
  7. 一楼空调         did=1000000104   model=xiaomi.airc.h55h00
  8. 小爱音箱Play      did=502390898    model=xiaomi.wifispeaker.l05b
  9. 二楼空调         did=928874393    model=xiaomi.airc.h39h00
 10. 领普温湿度传感器    did=blt.3.1lvigtirgcg00  model=linp.sensor_ht.ks2bb

共享家庭 525001384320：
  1. CW300           did=1000000102   model=mxiang.camera.moc001    [摄像头]
~~~

---

## 五之二、写操作实测（重要）

### 读 vs 写

| 操作 | 结果 |
|---|---|
| 读属性 | ✅ 全部成功 |
| 写属性 | ✅ **成功**（设备在线时） |

**实测成功案例**：

~~~bash
# 抽油烟机：开
POST /api/device/control {"did":"1000000105","siid":2,"piid":1,"value":true}
# -> {"ok":true}

# 验证读取
# -> siid=2 piid=1 = True   ← 生效了

# 关
POST /api/device/control {"did":"1000000105","siid":2,"piid":1,"value":false}
# -> {"ok":true}
~~~

### 失败原因分类

| 错误码 | 原因 | 处理 |
|---|---|---|
| -704220043 | **设备离线** | 检查 isOnline |
| -704042011 | 权限/状态不允许 | 提示用户 |
| code=1 | 设备协议不同（如 blt 蓝牙设备） | 跳过 |

**关键：设备离线时写操作会失败**，必须检查 online 状态。

### 账号归属（重要！）

**不同设备属于不同账号，用错账号会返回 -704010000。**

某账号的实测：

~~~bash
# 空调 1000000104 属于账号 1000000001
# 如果用 1000000002 的会话去读 -> -704010000

# 排查方法：逐个账号尝试
for session in sessions:
    res = session.GetProps(did, ...)
    if res.Code == 0:
        return session   # 找到了
~~~

**实现建议**：缓存 did -> userId 映射，避免每次遍历。

---

## 六、给 Mac 的实现建议

### 6.1 架构

~~~
设备列表
  |
  +-- 是摄像头 (.camera.) --> cs2 P2P 通道
  |     - 视频流
  |     - 云台 (0x112)
  |     - 待实现：对讲 (0x106)
  |
  +-- 其他设备 ----------> MIoT 通道
        - 读属性 (prop/get)
        - 写属性 (prop/set)
        - 动作 (action)
~~~

### 6.2 判断是否摄像头

~~~swift
func isCamera(model: String) -> Bool {
    return model.contains(".camera.") ||
           model.contains(".cateye.") ||
           model.contains(".feeder.")
}
~~~

### 6.3 属性缓存

MIoT 的 miot-spec 是**静态的**（型号决定），可以缓存：

~~~
model -> spec 映射
~~~

第一次查询后缓存，后续直接用。

### 6.4 错误处理

| 错误 | 建议 |
|---|---|
| -704220043 设备离线 | 提示用户，不要重试 |
| -704040002/3 属性不存在 | 说明 spec 过期，重新拉取 |
| throttle deny | 等 30 分钟（小米云限流） |

---

## 七、参考资料

- [miot-spec.org](https://miot-spec.org/miot-spec-v2/instances?status=all) 全量设备规格
- [go2rtc](https://github.com/AlexxIT/go2rtc) cs2 协议实现（Go）
- 本项目代码：
  - internal/xiaomi/miot.go       MIoT 实现
  - internal/xiaomi/client.go     设备发现
  - cmd/micamd/ptz.go             云台实现
  - docs/specs/                   已下载的设备 spec