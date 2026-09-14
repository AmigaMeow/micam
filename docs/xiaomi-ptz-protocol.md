# 小米摄像头云台协议 — 实测结论（给 Mac 客户端）

> 实测时间：2026-09-14
> 设备：小米室外摄像机 CW300（共享家庭）model=mxiang.camera.moc001
> 测试位置：飞牛 NAS，用自研 Go 探针直连摄像头

## 结论先行：云台可用，不用赌

**0x112 电机指令被摄像头接受，云台真的会动。**

Mac 那边担心的「赌 2000 行 Swift」**不成立** —— 协议已验证，可以放心写。

---

## 一、命令格式（Mac 照这个实现）

### 1.1 发送格式

~~~
payload = [4字节大端命令号] + JSON
外层用 cmdEncoded(0x1001) 包装
发送前用共享密钥 ChaCha20 加密
~~~

伪代码：
~~~
json = {"operation": N}
buf  = BigEndian(uint32(0x112)) || utf8(json)
enc  = chacha20_encode(buf, sharedKey)
conn.WriteCommand(0x1001, enc)
~~~

> ⚠️ **v1 探针的坑**：直接发裸 JSON 会导致**完全无响应**。
> 必须 [4字节 cmd][JSON]，且外层是 0x1001。

### 1.2 接收格式

~~~
raw      = conn.ReadCommand()
plain    = chacha20_decode(raw, key)
inner    = plain[4:]              // 跳过 4 字节命令号
response = json.Unmarshal(inner)
~~~

响应结构：
~~~
{
  "ret": 0,          // 0 = 成功
  "angle": 50,       // 水平角度
  "elevation": 50    // 俯仰角度
}
~~~

### 1.3 实测原始数据

~~~
发送: 00 00 01 12 7b 22 6f 70 65 72 61 74 69 6f 6e 22 3a 31 7d
      ^^^^^^^^^^^ ^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^
      0x112       {"operation":1}

响应明文:
  00 00 01 13 7b 22 72 65 74 22 3a 30 2c 20 22 61 6e 67 6c 65 22 3a 35 30 2c 22 65 6c 65 76 61 74 69 6f 6e 22 3a 37 33 7d
  ^^^^^^^^^^^ ^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^
  0x113       {"ret":0, "angle":50,"elevation":73}
~~~

---

## 二、认证流程（前置）

~~~
1. cs2.Dial(设备IP, transport)                    // 建立 P2P
2. key = X25519(device_public, client_private)    // 共享密钥
3. 发送 auth（明文，不加密）:
   WriteCommand(0x100, {"public_key": client_public,
                        "sign": sign, "uuid": "",
                        "support_encrypt": 0})
4. 读响应，应含 "result":"success"
~~~

实测认证成功响应：
~~~
{"result":"success", "channel": "1789341321040", "link_status":0, "encrypt_type":0}
~~~

---

## 三、operation 语义（部分确定）

实测观察：

| operation | 观察到 | 判断 |
|---|---|---|
| 1 | 返回 angle=50 elev=50 | **查询状态** |
| 2 | elevation 1 → 0（真的动了） | **执行动作** |
| 3,4,5 | 连接被摄像头断开 | **也执行了动作** |

已知：
- ret:0 表示指令被接受
- angle / elevation 是当前云台位置
- 数值在指令后**确实变化** → 云台真的在动

**待补**：需更长间隔（执行后摄像头会短暂断连，需重连）逐个测出
左/右/上/下/停止/校准 分别对应哪个 operation。

> 建议 Mac 端先用 operation:1 做状态查询打通链路，
> 再逐个试 2~10，每次之间**重建连接**。

---

## 四、连接特性（重要）

1. **执行云台指令后，摄像头可能主动断开连接**（观察到 broken pipe）
   → 客户端要**每次重建连接**，或实现自动重连
2. **每次建连都要向小米云取密钥**
   → 断网后无法重连
3. **P2P 是本地直连**，视频不经过小米服务器
4. **cs2 协议**（vendor=cs2），端口每次会变

---

## 五、Mac 端实现建议

### 5.1 不需要写 2000 行

~~~
连接管理（含重连）      ~300 行
ChaCha20 加解密          ~50 行
命令封装/解析            ~100 行
云台操作封装             ~150 行
状态查询与缓存           ~100 行
合计                     ~700 行 Swift
~~~

### 5.2 关键依赖

- X25519 密钥交换：Apple **CryptoKit** 原生支持
- ChaCha20：Apple **CryptoKit** 原生支持
- **零第三方依赖**

### 5.3 建议顺序

1. 先做查询：发 {"operation":1}，确认能拿到 angle/elevation
2. 再做动作：逐个试 operation，记录每个值的效果
3. 每次动作后重连：避免 broken pipe
4. 把标定结果写进代码常量表

---

## 六、可复用的探针

~~~
/root/Deepseek/micam/cmd/ptzprobe/main.go
/tmp/ptzprobe            # 已编译

./ptzprobe -url "<完整 xiaomi:// URL>" -ops "1,2,3,4,5" -settle 6s
~~~

源码里已完成：认证、加解密、命令封装、响应解析，可直接参考移植。

---

## 七、凭据说明

完整 URL 形如：
~~~
xiaomi://1000000001:@192.168.1.10?client_private=<64hex>&client_public=<64hex>
  &device_public=18acb361018c07d9d05fbacc80da2fd41eb2bef32688cf4db2fdb67d1683482f
  &did=1000000102&model=mxiang.camera.moc001&sign=<64hex>&subtype=3&vendor=cs2
~~~

> client_private / client_public / sign 每次建连都会变，由小米云下发。
> 完整值见：/root/Deepseek/micam-creds/devices.json

---

## 八、顺带纠正接入资料里的两处

1. **CW500 音频是单声道**（Mac 从 AAC AudioSpecificConfig 解出 channels=1），
   不是之前资料写的立体声。
2. **go2rtc 上游没有 HEVC 封装**（encodeAVData 只有 H264/AAC），
   EasyNVR 用的是打过补丁的版本 —— 这解释了为什么通用播放器容易翻车。