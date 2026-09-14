# 贡献指南

感谢你愿意帮助改进 MiCam！

由于不同 NAS 的 CPU / 核显差异很大，**硬件适配**是这个项目最需要社区帮助的部分。

---

## 一、最简单的贡献：提交硬件适配信息

### 1. 导出诊断包

打开 MiCam → **系统设置** → **导出诊断包**

诊断包包含：
- 硬件能力（CPU / 核显 / 编码器）
- 应用状态
- 日志（最后 500 行）

**所有敏感信息已自动脱敏**：
- 小米账号 ID
- 摄像头 P2P 凭据
- IP 地址后两段
- 设备 did

### 2. 提交 Issue

到 [Issues](https://github.com/AmigaMeow/micam/issues) 新建，选「硬件适配」模板，附上诊断包。

### 3. 也可以直接用命令行

    curl -o micam-diag.tar.gz http://<你的NAS>:8900/api/diag/bundle

或者只要 JSON 报告：

    curl http://<NAS>:8900/api/diag/report | python3 -m json.tool

---

## 二、硬件适配需要什么信息

诊断包里的 hardware.json 是重点：

    {
      "cpuModel": "Intel(R) Celeron(R) J4125 CPU @ 2.00GHz",
      "cpuCores": 4,
      "arch": "amd64",
      "hasDRI": true,
      "gpuInfo": "Intel Corporation GeminiLake [UHD Graphics 600]",
      "preferred": "h264_qsv",
      "encoders": [
        {"name": "h264_qsv", "kind": "qsv", "hardware": true},
        {"name": "h264_nvenc", "kind": "nvenc", "hardware": true},
        {"name": "libx264", "kind": "software"}
      ]
    }

**我们会据此判断**：
- 你的硬件支持哪种编码器
- 是否需要调整参数（如 QSV 初始化失败的情况）
- 是否需要禁用某些优化

---

## 三、已知的硬件兼容问题

| 硬件 | 问题 | 状态 |
|---|---|---|
| Intel J4125 (UHD 600) | QSV 硬件编码在实时流下不稳定 | 已绕开（HLS 用 libx264） |
| Intel 核显 + VAAPI | hwdownload 开销大，反而更慢 | 已绕开 |
| 无核显 | 只能用软件编码 | 支持 |

**如果你遇到新的兼容问题，欢迎提交 Issue。**

---

## 四、代码贡献

### 开发环境

    git clone https://github.com/AmigaMeow/micam.git
    cd micam

    # 需要 Go 1.21+
    go build ./cmd/micamd

    # 构建 fpk
    bash build/build-fnos.sh

### 目录结构

    cmd/micamd/          主程序
      main.go            入口 + HTTP 路由
      live.go            播放（MJPEG/快照）
      hls.go             HLS 播放
      ptz.go             云台控制
      hub.go             摄像头连接管理
      setup.go           安装向导 + 自检
      diag.go            诊断导出

    internal/
      xiaomi/            小米云 API（登录/设备/凭据）
      stream/            流构建
      hardware/          硬件检测
      record/            录像
      store/             存储（账号/源缓存）

    platform/fnos/       飞牛插件配置
      manifest           应用元数据
      config/            权限与资源声明
      cmd/main           生命周期脚本
      app/www/           前端页面

### 提交规范

    feat: 新功能
    fix: 修 bug
    docs: 文档
    perf: 性能优化
    refactor: 重构
    chore: 杂项

---

## 五、硬件适配的开发接口

### 5.1 硬件检测

internal/hardware/detect.go：

    cap := hardware.Detect(ffmpegPath)

    // 可用的编码器
    for _, e := range cap.Encoders {
        fmt.Println(e.Name, e.Hardware)
    }

    // 推荐编码器
    fmt.Println(cap.Preferred)  // h264_qsv / h264_nvenc / libx264 ...

**要新增硬件支持**，只需在 pickEncoder 里加候选：

    candidates := []Encoder{
        {Name: "h264_qsv", Kind: "qsv", Hardware: true},
        // 加你的
        {Name: "h264_videotoolbox", Kind: "videotoolbox", Hardware: true},
    }

### 5.2 编码参数

BuildEncodeArgs() 返回编码器参数，按 kind 分支：

    case strings.HasSuffix(c.Preferred, "_qsv"):
        return []string{"-c:v", "h264_qsv", "-preset", "veryfast"}, true

---

## 六、常见问题

### Q: 诊断包里会有我的隐私吗？

**不会。** 所有敏感信息在导出时自动脱敏：

| 类型 | 处理 |
|---|---|
| IP 地址 | 192.168.1.100 → 192.168.*.* |
| 设备 did | 1000000101 → ***ID*** |
| 账号 userId | 同上 |
| token / 凭据 | ***TOKEN*** |

你可以先解压检查再提交。

### Q: 我不想公开日志怎么办？

只提交 report.json 里的 hardware 部分即可：

    curl -s http://<NAS>:8900/api/diag/report | python3 -c "
    import json, sys
    d = json.load(sys.stdin)
    print(json.dumps(d['hardware'], indent=2, ensure_ascii=False))
    "

### Q: 我的编码器不在支持列表里？

提交 Issue 附诊断包，我们会加上适配。
