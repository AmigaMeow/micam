# 小米云凭据与限流（实战记录）

> 2026-09-15 | 插件版本 0.8.4

---

## 一、现象

两台摄像头同时无法播放，日志：

~~~
凭据失效（miss: auth: {"result":"failed","code":3}），尝试刷新
刷新失败: xiaomi: code=-12 msg=throttle deny
~~~

界面上表现为**一直黑屏**，用户不知道原因。

---

## 二、诊断过程（如何排除网络问题）

### 2.1 先排除网络

~~~bash
ping 192.168.1.10        # 2ms，通
ping 192.168.1.11        # 2ms，通
~~~

### 2.2 再排除「摄像头不响应」

裸 UDP 探测（cs2 发现包 `f1 30 00 00`）：

~~~python
s.sendto(bytes([0xf1,0x30,0x00,0x00]), ('192.168.1.10', 32108))
d, a = s.recvfrom(1024)
# 收到 24 字节 -> 摄像头是活的
~~~

**关键**：不要只看插件日志就下结论。
裸探测能区分「网络问题」「摄像头问题」「凭据问题」。

### 2.3 最后确认是凭据

写了 `cmd/vendorcheck`，直接调小米云：

~~~
=== CW300 (1000000102) ===
  [vendor] 限流重试 1/5，等待 5s...
  [vendor] 限流重试 2/5，等待 10s...
  [vendor] 限流重试 3/5，等待 20s...
  [vendor] 限流重试 4/5，等待 40s...
  账号 1000000002: 失败 xiaomi: code=-12 msg=throttle deny
  账号 1000000001: 失败 xiaomi: code=-12 msg=throttle deny
  ❌ 所有账号都失败
~~~

**两个账号都限流 → 账号级限流，不是设备级、不是网络问题。**

---

## 三、cs2 凭据的生命周期

~~~
小米云                  摄像头
  |                       |
  |  miss_get_vendor      |
  |  （拿 sign/公钥）     |
  |---------------------->|
  |                       |
  |  返回:                |
  |   - vendor（cs2）     |
  |   - p2p_id            |
  |   - device_public     |
  |   - sign              |
  |                       |
  |  有效期：实测 8 小时  |
~~~

过期后必须重新申请，而 `miss_get_vendor` **有严格限流**。

---

## 四、旧策略的问题

~~~
旧：过期后才刷新

  用户播放 -> 凭据失效 -> 触发刷新 -> 撞限流
           -> 冷却 30 分钟 -> 用户干等
           -> 30 分钟后重试 -> 还限流 -> ...
~~~

**问题**：
1. 刷新发生在用户等待时（阻塞）
2. 一撞限流就没有退路
3. 用户看到黑屏，不知道原因

---

## 五、新策略：提前续期

~~~
新：凭据年龄 > 4 小时就开始后台续期

  有效期 8 小时，4 小时续期 -> 留一半余量
  续期在后台进行，不阻塞用户
  （用户用的是仍然有效的旧凭据）

  续期失败（限流）-> 保留旧凭据 -> 15 分钟后再试
  即使连续撞 4 次限流（2 小时），凭据还没过期
~~~

### 5.1 实现

`cmd/micamd/credential.go`：

~~~go
const credRenewAfter    = 4 * time.Hour   // 续期阈值
const credKeeperInterval = 15 * time.Minute // 检查间隔

func (s *Server) startCredentialKeeper() {
    // 启动后等 2 分钟，然后每 15 分钟检查
}

func (s *Server) renewStaleCredentials() {
    for _, d := range s.store.Devices() {
        age := s.store.SourceAge(d.Did)
        if age < credRenewAfter { continue }
        if !s.canRefresh(d.Did) { continue }  // 冷却中
        // 申请成功才替换缓存（失败保留旧凭据）
    }
}
~~~

### 5.2 关键原则（再次强调）

~~~
申请成功才替换缓存，失败保留旧值
~~~

反过来做（先删再申请）在限流时会**彻底不可用** —— 这是第一版的坑。

---

## 六、新增接口

### 6.1 凭据状态

~~~bash
GET /api/live/cred-status
~~~

~~~json
{
  "renewAfterH": 4,
  "validForH": 8,
  "devices": [{
    "did": "1000000102",
    "name": "小米室外摄像机CW300",
    "hasCred": true,
    "ageMinutes": 1440,
    "expiresInMinutes": 0,
    "cooldownMinutes": 29,
    "renewHint": "已超过续期阈值，将自动续期"
  }]
}
~~~

### 6.2 手动续期

~~~bash
POST /api/live/renew?did=1000000102   # 指定设备
POST /api/live/renew                  # 全部设备
~~~

~~~json
{"results":[{"did":"...","ok":false,"error":"冷却中，剩余 28m0s"}]}
~~~

---

## 七、界面提示

实时预览页新增凭据状态条（只在异常时显示）：

~~~
限流中  -> 红色：
  ⚠️ 摄像头凭据需要刷新，但小米云正在限流。
     约 29 分钟后自动重试。
     期间可能无法播放。可稍后再试，或重启路由器换个出口 IP。
  [立即重试刷新]

快过期  -> 黄色：
  ⚠️ 摄像头凭据约 45 分钟后过期，到期会自动续期。
~~~

**播放失败时也会自动检查**，让用户知道是凭据问题而不是设备问题。

---

## 八、限流的特点（实测）

| 项 | 实测 |
|---|---|
| 触发原因 | 短时间内多次调用 `miss_get_vendor` |
| 范围 | **账号级**（同一账号下所有设备都受影响） |
| 错误码 | `code=-12 msg=throttle deny` |
| 恢复时间 | 几十分钟到几小时（不确定） |
| 退避重试 | 内置 5 次，5/10/20/40 秒，仍可能全失败 |

### 8.1 如何减少触发

1. **不要频繁手动刷新** —— 用提前续期代替
2. **不要重复 Dial** —— 用连接共享（0.8.1+）
3. **测试时注意节奏** —— 我这次就是因为云台测试太频繁触发的

### 8.2 恢复手段

1. 等（最可靠）
2. 换出口 IP（重启路由器）—— 限流可能绑 IP
3. 换账号

---

## 九、备用凭据（延长可用时间）

当前实现是「一个设备一份凭据」。理论上可以：

~~~
提前申请 N 份凭据轮换使用 -> 把 8 小时延长到 N*8 小时
~~~

**但**：每次申请都有触发限流的风险，收益不确定。

**暂不实现**，先观察提前续期的效果。

---

## 十、诊断工具

### 10.1 cmd/vendorcheck

直接测小米云凭据接口：

~~~bash
go build -o /tmp/vendorcheck ./cmd/vendorcheck
/tmp/vendorcheck
~~~

输出每个账号的成功/失败，用于判断限流是否恢复。

### 10.2 裸 UDP 探测

判断摄像头是否响应 P2P：

~~~python
import socket
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.settimeout(3)
s.sendto(bytes([0xf1,0x30,0x00,0x00]), ('192.168.1.10', 32108))
d, a = s.recvfrom(1024)
print('响应', len(d), '字节')
~~~

### 10.3 分阶段日志

~~~bash
grep -E '⏱|cred|vendor' /vol1/@appdata/micam/micamd.log
~~~

---

## 十一、给客户端的建议

1. **播放失败先查 `/api/live/cred-status`**
2. **看到 `cooldownMinutes > 0` 就提示用户等待**，不要反复重试
3. **不要自己实现凭据刷新** —— 服务端已做提前续期
4. `504` 可以重试；凭据错误不要立刻重试（会加重限流）
