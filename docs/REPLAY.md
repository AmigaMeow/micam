# 录像回放

> v0.9.0 新增

## 一、为什么需要转码

录像文件是 **MKV 容器 + HEVC 编码**（摄像头原生格式，`-c copy` 录制）。

浏览器兼容性：

| 浏览器 | MKV | HEVC |
|---|---|---|
| Chrome | 可以 | **不行** |
| Safari | **不行** | 可以 |
| Firefox | **不行** | **不行** |

**所以回放必须转成 H.264 + MP4。**

实测（10 秒 2K 素材）：

~~~
libx264   6.43s  CPU 262%
h264_qsv  3.33s  CPU 200%（手工跑）
~~~

## 二、硬件编码的坑

**服务进程里 h264_qsv 拿不到 /dev/dri 权限**：

~~~
[replay] ffmpeg: [h264_qsv] Failed to create a VAAPI device.
~~~

手工在 shell 里跑却没问题（shell 有 video 组权限，服务进程没有）。

**解法：提前探测 + 自动降级**

~~~
probeTranscode() 先试编码 1 秒
  成功 -> 用 h264_qsv
  失败 -> 用 libx264，并记日志
~~~

**为什么必须先探测**：如果已经发了 `200 OK` 才发现失败，
客户端只会收到空 body，看不到错误。

## 三、接口

~~~
GET  /api/recordings/dates?name=xxx          可用日期
GET  /api/recordings/list?name=&date=        文件列表（时长+缩略图）
GET  /api/recordings/play?name=&file=&t=     转码播放（t=起始秒）
GET  /api/recordings/thumb?name=&file=       缩略图
GET  /api/recordings/download?name=&file=    下载原文件
POST /api/recordings/delete                  删除
~~~

### 3.1 时长缓存

`ffprobe` 取时长较慢，结果缓存在 `recordings/.meta.json`：

~~~json
{
  "/path/to/x.mkv": {"duration": 32.95, "size": 3264544, "modTime": 1789391177}
}
~~~

**注意**：只缓存有效时长（d > 0），避免把失败结果缓存住。

`ffprobe` 找不到时会兜底解析 `ffmpeg -i` 的 stderr。

### 3.2 缩略图

- 取第 1 秒的帧（避开开头黑屏）
- 缩放 320 宽
- 缓存到 `recordings/.thumbs/<设备>/<文件>.jpg`
- 源文件更新时自动重新生成

### 3.3 拖动进度

`?t=<秒>` 参数，用 `-ss`（放在 `-i` 前面 = 快速定位到关键帧）。

前端在时间轴上点击时重新请求带 `t=` 的 URL。

## 四、前端界面

### 4.1 布局

~~~
回放卡片（点播放才显示）
  video 播放器 + 下载/关闭按钮

录像任务卡片
  设备 / 状态 / 开始时间 / 停止

录像回放卡片
  设备选择 + 日期筛选 + 清理过期
  按日期分组的缩略图网格
    缩略图上有播放按钮和时长
    悬停显示删除按钮
~~~

### 4.2 缩略图网格

~~~css
.rec-grid  auto-fill minmax(190px, 1fr)
手机       2 列
~~~

每个卡片：
- 缩略图（16:9）
- 时长角标
- 文件名 + 大小 + 时间
- 删除按钮（悬停显示，手机常显）

## 五、待做

| # | 事项 | 说明 |
|---|---|---|
| 1 | 转码缓存 | 首次播放后缓存 MP4，二次播放走 Range（秒开+可拖动） |
| 2 | 时间轴预览 | 拖动时显示缩略图 |
| 3 | 批量删除 | 按日期/设备批量 |
| 4 | 自动清理策略配置 | 界面上改保留天数 |
| 5 | 导出片段 | 剪辑指定时间段 |
