# 社区适配指南（硬件差异）

> 面向：想让 MiCam 在自己 NAS 上跑得更好的用户
> 也面向：愿意帮我们适配新硬件的贡献者

---

## 一、为什么需要社区适配

不同 NAS 的 CPU / 核显差异极大，视频转码（直播 + 录像回放）
能不能用硬件编码，直接决定 CPU 占用和流畅度。

**同一个插件在不同机器上表现可能差 5 倍。**

举例（本机 Intel J4125）：

~~~
库上写着支持        实际能不能用
----------------    --------------
h264_qsv            ❌ Failed to create a VAAPI device
h264_nvenc          ❌ Cannot load libcuda.so.1
h264_vaapi          ❌ No VA display found
h264_amf            ❌ DLL libamfrt64.so.1 failed
libx264             ✅ 可用
~~~

**`ffmpeg -encoders` 说支持，不等于真的能用。**
所以我们改成「实测」。

---

## 二、插件怎么自动适配

### 2.1 启动时探测

读 `/proc/cpuinfo`、`/dev/dri`、`lspci`，列候选编码器：

~~~
候选顺序（按性能）：
  h264_qsv       Intel 核显
  h264_nvenc     NVIDIA
  h264_vaapi     AMD / Intel 通用
  h264_amf       AMD
  libx264        软件兜底
~~~

### 2.2 实测确认（关键）

**每个候选都要真编 1 秒视频，检查真的产出了数据。**

为什么不能只看退出码：`h264_v4l2m2m` 退出码 0 但一帧都没编出来
（实测踩过）。

### 2.3 结果缓存

~~~
进程内缓存 + 缓存文件名带编码器指纹
  -> 换硬件后自动失效，不会用错缓存
~~~

---

## 三、你该怎么做（普通用户）

### 3.1 先看插件自己怎么说

打开插件 → 连点版本号 7 次 → 开发者 → **能力探测**

会看到：

~~~
版本        0.9.1
CPU         Intel Celeron J4125 · 4 核
核显        Intel UHD Graphics 600
首选编码器  libx264
可用编码器  libx264
~~~

### 3.2 也可以命令行拉

~~~bash
curl http://<你的NAS>:8900/api/dev/capabilities | python3 -m json.tool
~~~

重点看 `encoderProbe`：

~~~json
{
  "encoderProbe": [
    {"name":"h264_qsv", "ok":false, "error":"Failed to create a VAAPI device"},
    {"name":"libx264",  "ok":true}
  ]
}
~~~

### 3.3 如果 qsv 报权限错误

说明服务进程拿不到 `/dev/dri`。可以试着把服务用户加进 `video` / `render` 组：

~~~bash
# 查看设备权限
ls -la /dev/dri/

# 查看服务用户
grep micam /etc/passwd

# 加组（具体组名看 ls 输出）
usermod -aG video micam
usermod -aG render micam

# 重启插件
~~~

> 注意：飞牛应用中心会重置权限，升级插件后可能要重新加。

---

## 四、你该怎么做（贡献者）

### 4.1 导出诊断包

插件 → 系统设置 → **导出诊断包**

会包含：

~~~
report.json           应用状态、设备列表、自检结果
hardware.json         CPU / 核显 / 编码器列表
encoder-probe.json    ★ 编码器实测结果（最关键）
transcode-cache.json  转码缓存状态
config.json           配置
logs/                 日志（最后 500 行）
README.txt            说明
~~~

**所有敏感信息自动脱敏**：

| 类型 | 处理 |
|---|---|
| IP 地址 | 192.168.1.100 → 192.168.*.* |
| 设备 did | 1000000101 → ***ID*** |
| 账号 userId | 同上 |
| token / 凭据 | ***TOKEN*** |

**提交前建议自己解压检查一遍。**

### 4.2 提交 Issue

到 [Issues](https://github.com/AmigaMeow/micam/issues) 新建，附上诊断包。

标题格式建议：

~~~
[硬件适配] <CPU型号> + <核显型号>

例：[硬件适配] Intel N100 + UHD Graphics
~~~

### 4.3 我们拿到后会做什么

1. 看 `encoder-probe.json` 判断你的硬件能用哪个编码器
2. 如果需要新编码器（比如 Apple VideoToolbox、Rockchip MPP）就加候选
3. 如果参数需要调（比如 VAAPI 的 `hwupload` 格式）就针对性改
4. 发一个版本，你在里面验证

---

## 五、支持的硬件与坑（持续更新）

| 硬件 | 情况 | 状态 |
|---|---|---|
| Intel J4125 (UHD 600) | qsv 报 VAAPI 设备失败 | 自动降级 libx264 |
| Intel 核显 + VAAPI | hwdownload 开销大，反而更慢 | 直播用 libx264 |
| NVIDIA | 需要 libcuda.so.1 + 驱动 | 待验证 |
| AMD | 需要 libamfrt64.so.1 | 待验证 |
| 无核显 | 只能软件编码 | 支持 |
| ARM（如 RK3588） | 需要 rockchip_mpp | **需要社区提供日志** |

### 5.1 已知的编码器陷阱

| 编码器 | 陷阱 |
|---|---|
| h264_qsv | 服务进程可能没有 /dev/dri 权限（shell 有） |
| h264_v4l2m2m | 探测通过但编不出数据 |
| h264_vaapi | 需要 `-vaapi_device` + `format=nv12,hwupload` |
| h264_qsv（直播） | pipe 输入不稳定，报告 `Could not open encoder before EOF` |

---

## 六、插件里的相关接口

~~~
GET  /api/dev/capabilities        能力探测（含编码器实测）
GET  /api/diag/bundle             诊断包（tar.gz）
GET  /api/diag/report             诊断报告（JSON）
GET  /api/recordings/cache        转码缓存状态
POST /api/recordings/cache/clear  清空转码缓存
~~~

---

## 七、转码缓存对硬件的影响

录像回放要转码（MKV/HEVC → MP4/H264）。

**有缓存之后，同一段录像只转码一次：**

~~~
首次播放   实时转码（同时后台生成缓存文件）
二次播放   直接发缓存（支持 Range，可拖动，秒开）
~~~

实测（33 秒 2K 素材）：

| | 首次 | 二次（缓存） |
|---|---|---|
| 耗时 | 21.6s | **0.023s** |
| 可拖动 | ❌ | **✅** |

**所以硬件好不好，只影响首次播放。**
缓存上限默认 5GB，超了自动删最旧的。

---

## 八、我们为什么不做「自动下载适配包」

考虑过：根据硬件自动下载对应的编码参数配置。

**不做，因为**：

1. 参数就那么几个，内置候选够了
2. 自动下载引入网络依赖和安全风险
3. 实测探测已经能覆盖 95% 的情况
4. 剩下的 5% 需要人工看日志，正是社区适配的价值

**更重要的是**：把「探测结果」暴露出来，
让用户和贡献者能看见问题，比黑盒自动下载更可靠。
