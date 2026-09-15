# 凭据体系（重要：别混淆三层）

> 常见误解：「登录一次能管多久？会不会过两天又要重新扫码？」
> 答案：**扫码一次就够，不会反复扫。** 但下面三层凭据要分清。

---

## 一、三层凭据

~~~
第 1 层  passToken        扫码登录拿到        长期（数月）
   |
   | 换取
   v
第 2 层  serviceToken     短期（几小时~几天）  过期自动续期
   ssecurity
   |
   | 调用小米云
   v
第 3 层  设备 P2P 凭据     每台摄像头一份      >= 24 小时
         sign / device_public             到期前自动续期
~~~

---

## 二、第 1 层：passToken（用户要关心的唯一一层）

### 2.1 什么时候拿到

**扫码登录小米账号**，只做一次。

### 2.2 有效期

~~~
小米的 passToken 是长期凭据（业界实践可达数月甚至更久）
实测：2026-09-14 扫码，2026-09-15 仍有效
~~~

### 2.3 什么时候会失效

- 用户在米家 App 里**主动登出**
- **修改小米账号密码**
- 账号被安全风控（异地登录等）
- 长时间完全不用（可能数月）

**正常使用不会失效。监控产品靠的就是这个特性。**

### 2.4 存在哪

~~~
/vol1/@appdata/micam/accounts.json
~~~

**关键**：`@appdata` 在卸载插件时通常**保留**，但为了保险，
插件提供导出/导入（见第五章）。

---

## 三、第 2 层：serviceToken（用户不用管）

### 3.1 怎么来的

~~~
passToken --[交换]--> serviceToken + ssecurity
~~~

每次调小米云 API 都要带上它。

### 3.2 有效期与续期

短期有效（小米不公开具体时长）。

**插件做了双重保障：**

~~~
1. 主动续期    每 8 小时检查，超龄就提前换
2. 被动兜底    请求遇到 401 -> 立刻用 passToken 重换 -> 重试原请求
~~~

**用户完全无感，监控不会因为 token 过期中断。**

### 3.3 实现细节（踩过的坑）

**坑 1：小米返回的是 HTTP 401，不是 JSON code**

~~~
一开始只检查 JSON 里的 code 字段 -> 漏掉了 401
修复：HTTP 401/403 也触发续期
~~~

**坑 2：用「时间防抖」判断并发会误伤**

~~~go
// 错的：刚登录时 lastExchange = now，
//       2 秒内遇到坏 token 会直接跳过续期
if time.Since(s.lastExchange) < 2*time.Second { return nil }

// 对的：用代际计数器
//   请求前记下 gen
//   拿锁后发现 gen 变了 -> 别人已经换好了，直接复用
//   gen 没变 -> 真的需要换
if genAt != 0 && s.gen != genAt { return nil }
~~~

---

## 四、第 3 层：设备 P2P 凭据

### 4.1 怎么来的

~~~
POST /v2/device/miss_get_vendor
  -> sign / device_public / p2p_id
~~~

**每台摄像头一份，按需申请。**

### 4.2 有效期

~~~
实测 CW300 的凭据 24 小时（1440 分钟）后仍可用
所以有效期 >= 24 小时（早期估的「8 小时」是错的）
~~~

### 4.3 这是唯一会撞限流的一层

`miss_get_vendor` 有**严格的调用频率限制**：

~~~
超限 -> code=-12 msg=throttle deny
恢复 -> 几十分钟到几小时（绑 IP + 账号）
~~~

**插件的应对：**

| 措施 | 说明 |
|---|---|
| 提前续期 | 阈值 12 小时（不是等过期才刷） |
| 失败保留旧凭据 | 刷新失败不清缓存，旧的继续用 |
| 30 分钟冷却 | 刷新失败后不反复打 |
| 调用量极低 | 3 台设备约 2 次/天/台 = 6 次/天 |

**对比**：如果不做优化，一天可能调用几百次，必然触发限流。

---

## 五、凭据备份（重要）

### 5.1 为什么需要

~~~
卸载插件时 @appdata 通常保留，但不是绝对保证
万一丢了 -> 要重新扫码 -> 人不在现场就麻烦了
~~~

### 5.2 导出

**插件界面**：账号管理 → 导出凭据

**命令行**：

~~~bash
curl -o micam-accounts.json \
  http://<nas>:8900/api/accounts/export
~~~

导出内容：

~~~json
{
  "app": "micam",
  "version": "0.9.2",
  "exportedAt": "2026-09-15T11:37:33+08:00",
  "note": "含小米账号登录凭据，请妥善保管",
  "accounts": [{"userId":"...","passToken":"V1:..."}]
}
~~~

### 5.3 ⚠️ 安全提醒

**这个文件等于账号登录态。**

- 不要上传到公开仓库
- 不要发给别人
- 建议存在加密盘或密码管理器里

### 5.4 导入

**界面**：账号管理 → 导入凭据

**命令行**：

~~~bash
curl -X POST -H 'Content-Type: application/json' \
  --data-binary @micam-accounts.json \
  http://<nas>:8900/api/accounts/import
~~~

导入前会自动把现有文件备份成 `accounts.json.bak`。

### 5.5 查看备份

~~~bash
curl http://<nas>:8900/api/accounts/backups
~~~

---

## 六、常见疑问

### Q1：过两天要重新扫码吗？

**不用。** `passToken` 长期有效，扫码一次就够。

### Q2：为什么日志里老看到「凭据失效」？

那是**第 3 层**（设备 P2P 凭据）或**第 2 层**（serviceToken），
不是登录凭据。插件会自动处理，用户不用管。

### Q3：限流了要重新登录吗？

**不用。** 限流只影响「申请设备 P2P 凭据」这一步，
登录状态完全不受影响。限流过去后自动恢复。

### Q4：怎么知道登录还有效？

~~~bash
curl http://<nas>:8900/api/accounts/status
~~~

~~~json
{
  "accounts": [{
    "userId": "1000000001",
    "hasPassToken": true,
    "sessionOk": true,
    "lastExchange": "2026-09-15 11:37:26",
    "sessionAgeMinutes": 0
  }],
  "explain": {
    "passToken": "扫码登录拿到，长期有效（无需反复扫码）",
    "serviceToken": "由 passToken 换取，短期有效，过期自动续期",
    "deviceCred": "每台摄像头的 P2P 凭据，有效期 >= 24 小时"
  }
}
~~~

### Q5：多久需要维护一次？

**理论上不需要。** 插件的自动续期覆盖了第 2、3 层。

建议：
- **偶尔导出一次凭据备份**（换设备/重装前必做）
- 如果换了小米账号密码，需要重新扫码

---

## 七、相关接口

~~~
GET  /api/accounts/status    三层凭据状态
GET  /api/accounts/export    导出凭据（含 passToken）
POST /api/accounts/import    导入凭据
GET  /api/accounts/backups   备份文件列表
GET  /api/live/cred-status   设备 P2P 凭据状态
POST /api/live/renew         手动续期设备凭据
~~~
