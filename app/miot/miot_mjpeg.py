#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""官方通道 → MJPEG（给插件直接消费）。

链路:
  libmiot_camera_lite（PPCS 局域网 P2P）
    → H264/H265 原始码流（Annex-B）
    → 动态识别编码格式 (H264 / HEVC)
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
import argparse, os, subprocess, sys, threading, time
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
    ap.add_argument("--infmt", default="auto", help="输入码流格式: auto / h264 / hevc")
    args = ap.parse_args()

    lib = find_lib(args.lib)
    if not lib:
        sys.stderr.write("找不到 libmiot_camera_lite.so\n"); return 2

    try:
        cam = MiotCamera(lib, args.host, args.client_id, args.token)
        cam.open(args.did, args.model, args.ip, max(1, int(args.channels)), [])
    except Exception as e:
        sys.stderr.write("初始化失败: %s\n" % e); return 3

    ff = None
    ff_lock = threading.Lock()
    stop = {"v": False}
    want = max(1, int(args.channel)) - 1
    started_event = threading.Event()

    def get_or_start_ff(codec):
        nonlocal ff
        if ff is not None:
            return ff
        with ff_lock:
            if ff is not None:
                return ff
            if args.infmt and args.infmt != "auto":
                fmt = args.infmt
            elif codec == 4:
                fmt = "h264"
            else:
                fmt = "hevc"

            if args.width > 0:
                vf = "scale='min(%d,iw)':-2" % args.width
            else:
                vf = "scale='min(2560,iw)':-2"
            cmd = [
                args.ffmpeg, "-hide_banner", "-loglevel", "warning",
                "-fflags", "nobuffer+flush_packets+fastseek",
                "-flags", "low_delay",
                "-probesize", "65536",
                "-analyzeduration", "100000",
                "-threads", "2",
                "-f", fmt, "-i", "pipe:0",
                "-fps_mode", "passthrough",
                "-vf", vf,
                "-c:v", "mjpeg", "-q:v", str(args.qv),
                "-f", "mjpeg", "-",
            ]
            sys.stderr.write("[ffmpeg] 动态启动转码: input=%s (codec=%d) qv=%d\n" % (fmt, codec, args.qv))
            sys.stderr.flush()
            ff = subprocess.Popen(cmd, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                  stderr=subprocess.DEVNULL, bufsize=0)
            started_event.set()
            return ff

    def writer(codec, channel, ts, data):
        # 只转发请求的镜头（单摄直接转发所有视频帧，双摄严格匹配）
        if int(args.channels) > 1 and channel != want:
            return
        if stop["v"]:
            return
        p = get_or_start_ff(codec)
        if not p or not p.stdin:
            return
        try:
            p.stdin.write(data)
        except Exception:
            stop["v"] = True
    cam._on_frame = writer

    # 按官方格式：每通道一个 quality + 终止 0
    _q = [int(x) for x in args.quality.split(",") if x.strip()] or [3]
    _nch = max(1, int(args.channels))
    rc = cam.start((_q * _nch)[:_nch], channel_count=_nch)
    sys.stderr.write("[start] rc=%d\n" % rc)
    if rc != 0:
        cam.stop(); cam.free()
        if ff: ff.kill()
        return 5

    out = sys.stdout.buffer if args.out == "-" else open(args.out, "wb")

    # 等待第一帧启动 ffmpeg（最多等 10 秒）
    if not started_event.wait(10.0):
        sys.stderr.write("等待首帧数据超时\n")
        stop["v"] = True
        cam.stop(); cam.free()
        return 6

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
        try:
            if ff: ff.kill()
        except Exception: pass
        cam.stop(); cam.free()
    return 0


if __name__ == "__main__":
    sys.exit(main())
