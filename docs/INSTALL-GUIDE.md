# MiCam 插件：安装 / 升级 / 卸载指南

> 更新：2026-09-15 | 版本 0.3.7

---

## 一、升级的正确姿势

### 1.1 飞牛应用中心的限制

**手动安装的 fpk 不支持自动升级** —— 这是飞牛的限制，不是插件问题。

| 操作 | 是否支持 |
|---|---|
| 应用中心自动升级 | ❌ 手动装的包不支持 |
| 覆盖安装（同 appname） | ✅ 可以，会替换 |
| 卸载后重装 | ✅ 可以 |

### 1.2 推荐：覆盖安装

**不用卸载**，直接再装一次新的 fpk：

1. 飞牛 → 应用中心
2. 点「手动安装」
3. 选新的 micam.fpk
4. 提示"已安装，是否覆盖" → 确认

**好处**：@appdata/micam（登录凭据、录像）会保留。

---

## 二、⚠️ 卸载前必做

**如果之前配过 systemd 服务，卸载前必须先删掉**，否则会 Bad Gateway：

    systemctl stop micam
    systemctl disable micam
    rm -f /etc/systemd/system/micam.service
    systemctl daemon-reload

**为什么会 Bad Gateway**：
- 手动服务还在跑，但用的是被卸载删除的文件
- 它会删掉 app.sock
- 网关连不上 socket → Bad Gateway

---

## 三、常见问题

### 3.1 Bad Gateway

**原因**：网关连不上 app.sock。

**排查**：

    # 1. socket 在吗
    ls -la /vol1/@appcenter/micam/app.sock

    # 2. 进程在吗
    ps -eo user,cmd | grep micamd | grep -v grep

    # 3. 有冲突的服务吗（重点）
    systemctl status micam 2>/dev/null
    ls /etc/systemd/system/micam.service 2>/dev/null

**修复**：

    systemctl stop micam 2>/dev/null
    rm -f /etc/systemd/system/micam.service
    systemctl daemon-reload
    pkill -9 -f micamd
    # 然后应用中心里重启

### 3.2 404 page not found

**原因**：装的是旧版（< 0.3.1），没有网关前缀处理。

### 3.3 界面空白

**原因**：JS 语法错误（0.3.4 已修复）。

### 3.4 画面黑屏

| 机型 | 首帧时间 |
|---|---|
| CW300 | ~9 秒 |
| CW500 | **17-19 秒**（关键帧间隔长） |

超过 45 秒还没画面，看日志：

    tail -20 /vol1/@appdata/micam/info.log

---

## 四、包的位置

| 路径 | 说明 |
|---|---|
| /vol1/1000/micam.fpk | 用户目录（推荐） |
| /vol1/1000/分享/micam/micam.fpk | 分享目录 |
| /vol1/@appshare/micam/micam.fpk | 应用共享目录 |

三个位置内容相同。

---

## 五、目录说明

| 目录 | 内容 | 卸载时 |
|---|---|---|
| /vol1/@appcenter/micam | 程序文件 | 删除 |
| /vol1/@appconf/micam | 配置 | 保留 |
| /vol1/@appdata/micam | **登录凭据、录像** | **保留** |
| /vol1/@appmeta/micam | 元数据 | 删除 |
| /var/apps/micam | 符号链接 | 删除 |

**关键**：@appdata 里的 accounts.json 是登录凭据，删了要重新扫码。
