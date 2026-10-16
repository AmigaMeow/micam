#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""官方通道 → MJPEG（给插件直接消费）。

链路:
  libmiot_camera_lite（PPCS 局域网 P2P）
    → H264/H265 原始码流（Annex-B）
    → ffmpeg 转 MJPEG
    → 写 stdout / 文件

为什么用 ffmpeg 而不是自己解:
  · 插件已有 ffmpeg（--ffmpeg 参数）
  · 官方用 PyAV（ffmpeg 的 Python 绑定）做同样的事
  · H265 必须转码，浏览器普遍不支持

用法:
  python3 miot_mjpeg.py --token T --did D --model M --ip IP --channel 1 \
                        --ffmpeg /usr/bin/ffmpeg --out -
"""
import argparse, os, subprocess, sys, threading
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from miot_camera import MiotCamera, find_lib   # noqa: E402


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--lib")
    ap.add_argument("--host", default="mico.api.mijia.tech")
    ap.add_argument("--client-id", default="2882303761520431603")
    ap.add_argument("--token", required=True)
    ap.add_argument("--did", required=True)
    ap.add_argument("--model", required=True)
    ap.add_argument("--ip", default="")
    ap.add_argument("--channel", type=int, default=1,
                    help="输出第几个镜头（1 起）")
    ap.add_argument("--channels", type=int, default=2,
                    help="设备总通道数（双摄填 2）")
    ap.add_argument("--quality", default="1")
    ap.add_argument("--ffmpeg", default="/usr/bin/ffmpeg")
    ap.add_argument("--fps", type=int, default=8)
    ap.add_argument("--width", "--w", type=int, default=640)
    ap.add_argument("--out", default="-")
    ap.add_argument("--qv", type=int, default=6,
                    help="JPEG 质量（2=最好最大，31=最差最小）。默认 6")
    ap.add_argument("--infmt", default="hevc", help="输入码流格式: h264 / hevc")
    args = ap.parse_args()

    lib = find_lib(args.lib)
    if not lib:
        sys.stderr.write("找不到 libmiot_camera_lite.so\n"); return 2

    try:
        cam = MiotCamera(lib, args.host, args.client_id, args.token)
        cam.open(args.did, args.model, args.ip, max(1, int(args.channels)), [])
    except Exception as e:
        sys.stderr.write("初始化失败: %s\n" % e); return 3

    # ffmpeg：从 stdin 读 Annex-B 码流，输出 MJPEG
    #
    # ⚠️ 摄像头可能发 H264 也可能 H265（我们实测 CW500 是 H265/codec=5）。
    # 写死 -f h264 会导致 ffmpeg 解不出任何帧 -> 输出 0 字节。
    # 用 probe 自动识别更稳：先让 ffmpeg 自己探测。
    infmt = args.infmt or "hevc"
    # ⚠️ 帧率处理（2026-10-11 实测调优）
    #
    # 摄像头原生约 12.8 真实图像帧/秒（另有一堆 26 字节碎片帧）。
    #
    # 用 "-vf fps=N" 会让 ffmpeg 按【输入 PTS】抽帧，而库给的 PTS
    # 不均匀（实测被压到 9 fps，比原生还低）。改用：
    #   -vsync passthrough  -> 每帧照原样输出，不抽不补
    #   -r N                -> 只在【编码时】打上稳定的输出帧率
    # 这样既不丢帧，播放器也知道该按多少 fps 播。
    # MJPEG 是逐帧独立编码，不需要固定输出帧率 —— 有帧就编，没有就等。
    # 加任何 fps 过滤都会按 PTS 抽帧，而库的 PTS 不均匀（实测被压到 9fps）。
    vf = "scale=%d:-1" % args.width if args.width > 0 else "null"
    cmd = [
        args.ffmpeg, "-hide_banner", "-loglevel", "warning",
        "-fflags", "nobuffer+flush_packets+fastseek",
        "-flags", "low_delay",
        "-probesize", "32768",
        "-analyzeduration", "0",
        "-threads", "2",
        "-f", infmt, "-i", "pipe:0",
        "-fps_mode", "passthrough",
        "-vf", vf,
        "-c:v", "mjpeg", "-q:v", str(args.qv),
        "-f", "mjpeg", "-",
    ]
    ff = subprocess.Popen(cmd, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                          stderr=subprocess.DEVNULL, bufsize=0)

    stop = {"v": False}
    want = max(1, int(args.channel)) - 1

    def writer(codec, channel, ts, data):
        # 只转发请求的镜头
        if channel != want:
            return
        if stop["v"] or not ff.stdin:
            return
        try:
            ff.stdin.write(data)
        except Exception:
            stop["v"] = True
    cam._on_frame = writer

    # 按官方格式：每通道一个 quality + 终止 0
    _q = [int(x) for x in args.quality.split(",") if x.strip()] or [3]
    _nch = max(1, int(args.channels))
    rc = cam.start((_q * _nch)[:_nch], channel_count=_nch)
    sys.stderr.write("[start] rc=%d\n" % rc)
    if rc != 0:
        cam.stop(); cam.free(); ff.kill()
        return 5

    out = sys.stdout.buffer if args.out == "-" else open(args.out, "wb")
    try:
        while True:
            chunk = ff.stdout.read(4096)
            if not chunk:
                break
            out.write(chunk)
            out.flush()
    except (BrokenPipeError, KeyboardInterrupt):
        pass
    finally:
        stop["v"] = True
        try: ff.kill()
        except Exception: pass
        cam.stop(); cam.free()
    return 0


if __name__ == "__main__":
    sys.exit(main())
