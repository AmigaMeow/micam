#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""MiCam 官方通道拉流 sidecar（2026-10-11）。

为什么是 Python 而不是 Go：
  · libmiot_camera_lite 是 C ABI 的 .so，Go 调它需要 cgo
  · 本项目【没有 C 编译器】，且文档明确「不要引入 cgo」
  · 官方 Miloco 本身就是 Python（ctypes 调同一个库）
  => 用 Python sidecar，输出 MJPEG 给 Go 主程序读取

用法:
  python3 miot_camera.py --token <access_token> --did <did> --model <model> \
                         --ip <lan_ip> --channel 1 --out -

输出: multipart/x-mixed-replace 的 MJPEG 到 stdout（--out -）
      或写到文件（--out /path/to.mjpeg）

退出码:
  0  正常结束
  2  库不可用
  3  初始化失败
  4  创建摄像头失败
  5  启动失败
"""
import argparse, ctypes, os, signal, subprocess, sys, time
from ctypes import (CDLL, CFUNCTYPE, POINTER, Structure, byref, c_bool,
                    c_char_p, c_int, c_uint8, c_uint32, c_uint64, c_void_p)

# ---------- C 结构（对齐官方 camera.py） ----------

class CameraInfo(Structure):
    _fields_ = [("did", c_char_p), ("model", c_char_p), ("ip", c_char_p),
                ("channel_count", c_uint8)]

class CameraConfig(Structure):
    _fields_ = [("video_qualities", POINTER(c_uint8)),
                ("enable_audio", c_bool), ("pin_code", c_char_p)]

class FrameHeader(Structure):
    _fields_ = [("codec_id", c_uint32), ("length", c_uint32), ("timestamp", c_uint64),
                ("sequence", c_uint32), ("frame_type", c_uint32), ("channel", c_uint8)]

LOG_CB  = CFUNCTYPE(None, c_int, c_char_p)
STAT_CB = CFUNCTYPE(None, c_int)
RAW_CB  = CFUNCTYPE(None, POINTER(FrameHeader), POINTER(c_uint8))

# ---------- 库加载 ----------

def find_lib(explicit=None):
    cands = []
    if explicit:
        cands.append(explicit)
    if os.environ.get("MICAM_MIOT_LIB"):
        cands.append(os.environ["MICAM_MIOT_LIB"])
    here = os.path.dirname(os.path.abspath(__file__))
    cands += [
        os.path.join(here, "libmiot_camera_lite.so"),
        os.path.join(here, "..", "third_party", "miloco", "libmiot_camera_lite.so"),
        "/vol1/@appcenter/micam/lib/libmiot_camera_lite.so",
        "/vol1/@appdata/micam/lib/libmiot_camera_lite.so",
        "/usr/local/lib/libmiot_camera_lite.so",
    ]
    for p in cands:
        if p and os.path.exists(p):
            return os.path.abspath(p)
    return None


class MiotCamera:
    """libmiot_camera_lite 的最小封装。"""

    # 帧率/尺寸由库决定，这里只负责把 NAL 数据吐出来
    def __init__(self, lib_path, host, client_id, token):
        self.lib = CDLL(lib_path)
        self._bind()
        self._logs = []
        self._logref = LOG_CB(self._on_log)
        self.lib.miot_camera_set_log_handler(self._logref)
        rc = self.lib.miot_camera_init(host.encode(), client_id.encode(), token.encode())
        if rc != 0:
            raise RuntimeError("miot_camera_init 返回 %d" % rc)

        self._on_frame = None
        self._statref = STAT_CB(self._on_status)
        self._rawref = RAW_CB(self._on_raw)

    def _bind(self):
        L = self.lib
        L.miot_camera_set_log_handler.argtypes = [LOG_CB]; L.miot_camera_set_log_handler.restype = None
        L.miot_camera_init.argtypes = [c_char_p, c_char_p, c_char_p]; L.miot_camera_init.restype = c_int
        L.miot_camera_new.argtypes = [POINTER(CameraInfo)]; L.miot_camera_new.restype = c_void_p
        L.miot_camera_free.argtypes = [c_void_p]; L.miot_camera_free.restype = None
        L.miot_camera_start.argtypes = [c_void_p, POINTER(CameraConfig)]; L.miot_camera_start.restype = c_int
        L.miot_camera_stop.argtypes = [c_void_p]; L.miot_camera_stop.restype = c_int
        L.miot_camera_status.argtypes = [c_void_p]; L.miot_camera_status.restype = c_int
        L.miot_camera_version.argtypes = []; L.miot_camera_version.restype = c_char_p
        L.miot_camera_register_status_changed.argtypes = [c_void_p, STAT_CB]
        L.miot_camera_register_status_changed.restype = c_int
        L.miot_camera_register_raw_data.argtypes = [c_void_p, RAW_CB, c_uint8]
        L.miot_camera_register_raw_data.restype = c_int

    def _on_log(self, level, msg):
        try:
            s = msg.decode("utf-8", "ignore")
            self._logs.append(s)
            sys.stderr.write("[lib] %s\n" % s)
            sys.stderr.flush()
        except Exception:
            pass

    def _on_status(self, st):
        sys.stderr.write("[status] %d\n" % st)
        sys.stderr.flush()

    def _on_raw(self, hdr, data):
        if not self._on_frame:
            return
        try:
            h = hdr.contents
            # 官方 types.py: VIDEO_H264=4, VIDEO_H265/HEVC=5
            # frame_type: FRAME_P=0, FRAME_I=1（关键帧）
            if h.codec_id not in (4, 5):
                return
            buf = ctypes.string_at(data, h.length)
            self._on_frame(h.codec_id, h.channel, h.timestamp, buf)
        except Exception as e:
            sys.stderr.write("[raw] %s\n" % str(e)[:80])

    def version(self):
        try:
            v = self.lib.miot_camera_version()
            return v.decode() if v else "?"
        except Exception:
            return "?"

    def open(self, did, model, ip, channel, qualities, audio=False):
        """创建并注册回调。

        ⚠️ 关键（2026-10-11 实测）：
            register_raw_data 的第 3 个参数是【通道号】，必须逐通道注册。

            官方 camera.py:508 __register_raw_data_async(channel) 就是这么做的：
                for ch in range(channel_count):
                    register_raw_data(inst, cb, ch)

            我们原来只注册 channel=0 -> 永远只收到一路，
            这正是「双摄识别不到」的根因。

            实测（isa.camera.hlmax）：
                注册 ch0 -> 247 帧
                注册 ch1 -> 232 帧
                两路都是 2560x1440，内容不同（一个是枪机、一个是球机）
        """
        self.info = CameraInfo(did.encode(), model.encode(), ip.encode() if ip else None, channel)
        self.inst = self.lib.miot_camera_new(byref(self.info))
        if not self.inst:
            raise RuntimeError("miot_camera_new 失败")
        self.lib.miot_camera_register_status_changed(self.inst, self._statref)

        # 逐通道注册（channel_count >= 1 时注册 0..channel-1）
        nch = max(1, int(channel or 1))
        self._rawrefs = []
        for ch in range(nch):
            def make_cb(channel_idx):
                def _cb(hdr, data):
                    return self._on_raw_ch(channel_idx, hdr, data)
                return RAW_CB(_cb)
            ref = make_cb(ch)
            # 必须保持引用，否则会被 GC 回收导致崩溃
            self._rawrefs.append(ref)
            self.lib.miot_camera_register_raw_data(self.inst, ref, ch)
        return self.inst

    def _on_raw_ch(self, reg_channel, hdr, data):
        """按注册通道分发回调。"""
        if not self._on_frame:
            return
        try:
            h = hdr.contents
            if h.codec_id not in (4, 5):
                return
            buf = ctypes.string_at(data, h.length)
            # 用帧自带的 channel 优先；为 0 时用注册时的通道号
            ch = h.channel if h.channel else reg_channel
            self._on_frame(h.codec_id, ch, h.timestamp, buf)
        except Exception as e:
            sys.stderr.write("[raw] %s\n" % str(e)[:80])

    def start(self, qualities, audio=False, channel_count=1):
        """启动。

        ⚠️ qualities 数组格式必须与官方一致（2026-10-11 修正）：
            官方 camera.py:211-217
                video_qualities = [q for _ in range(channel_count)]
                video_qualities.append(0)          # 终止符
                (c_uint8 * (channel_count + 1))(*video_qualities)

            我们原来只传一个值、没有终止符 -> 库按 channel_count+1 读越界。
        """
        nch = max(1, int(channel_count or 1))
        if len(qualities) == 1:
            vals = list(qualities) * nch
        else:
            vals = list(qualities)
        vals = vals[:nch] + [0]          # 每通道一个 + 终止 0
        q = (c_uint8 * len(vals))(*vals)
        cfg = CameraConfig(ctypes.cast(q, POINTER(c_uint8)), audio, None)
        return self.lib.miot_camera_start(self.inst, byref(cfg))

    def stop(self):
        try:
            self.lib.miot_camera_stop(self.inst)
        except Exception:
            pass

    def free(self):
        try:
            self.lib.miot_camera_free(self.inst)
        except Exception:
            pass


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
                    help="输出第几个镜头（1 起）。双摄机型 1=枪机/2=球机")
    ap.add_argument("--channels", type=int, default=2,
                    help="设备总通道数（注册多少个 raw_data 回调）。双摄填 2")
    ap.add_argument("--quality", default="1", help="逗号分隔的画质档位，官方默认 1(LOW)")
    ap.add_argument("--out", default="-", help="输出路径，- 表示 stdout")
    ap.add_argument("--segment", type=int, default=0,
                    help="按秒切分输出文件（>0 时生效，需配合 --out 目录）")
    ap.add_argument("--segment-format", default="mkv",
                    help="分片格式: mkv（内部调用 ffmpeg 转封装）或 raw（裸 .265）")
    ap.add_argument("--probe", action="store_true", help="只初始化并报告，不拉流")
    ap.add_argument("--ffmpeg", default="/usr/bin/ffmpeg", help="分片转封装用的 ffmpeg")
    args = ap.parse_args()

    lib_path = find_lib(args.lib)
    if not lib_path:
        sys.stderr.write("找不到 libmiot_camera_lite.so\n")
        return 2
    sys.stderr.write("[init] lib=%s\n" % lib_path)

    try:
        cam = MiotCamera(lib_path, args.host, args.client_id, args.token)
    except Exception as e:
        sys.stderr.write("[init] 失败: %s\n" % e)
        return 3

    sys.stderr.write("[init] 库版本 %s\n" % cam.version())

    try:
        nch = max(1, int(args.channels))
        cam.open(args.did, args.model, args.ip, nch, [])
    except Exception as e:
        sys.stderr.write("[open] 失败: %s\n" % e)
        return 4

    if args.probe:
        print("lib=%s version=%s init=ok open=ok" % (lib_path, cam.version()))
        cam.free()
        return 0

    _q = [int(x) for x in args.quality.split(",") if x.strip()] or [3]
    _nch = max(1, int(args.channels))
    rc = cam.start((_q * _nch)[:_nch], channel_count=_nch)
    sys.stderr.write("[start] rc=%d\n" % rc)
    if rc != 0:
        # 停止 + 报告
        cam.stop(); cam.free()
        return 5

    # ===== 输出：stdout / 单文件 / 按时间分片 =====
    #
    # ⚠️ 为什么分片要落【文件】而不是管道给 ffmpeg（2026-10-11 实测）：
    #
    #   ffmpeg 从管道读裸 H265 时无法 seek，探测不到 SPS/PPS：
    #     "Could not find codec parameters ... unspecified size"
    #   即使把 probesize/analyzeduration 加到 20M 也没用（管道不可 seek）。
    #
    #   先落文件再转封装则完全正常（实测 2MB .265 -> 2.08MB .mkv）。
    #
    #   所以：sidecar 自己写原始 .265 分片，写完一个就用 ffmpeg 转成 mkv，
    #   再删掉 .265。这是官方 Miloco 的做法（它也是落文件）。
    seg = {"n": 0, "t0": time.time(), "fh": None, "path": None}
    outdir = args.out
    if args.segment > 0:
        os.makedirs(outdir, exist_ok=True)

    def seg_open(now):
        ts = time.strftime("%Y%m%d_%H%M%S", time.localtime(now))
        seg["path"] = os.path.join(outdir, "%s_%d.265" % (ts, seg["n"]))
        seg["fh"] = open(seg["path"], "wb")
        seg["n"] += 1
        seg["t0"] = now

    def seg_roll(now):
        """关闭当前分片并转封装。"""
        if seg["fh"]:
            seg["fh"].close()
            raw = seg["path"]
            if os.path.getsize(raw) > 1024:
                dst = raw[:-4] + ".mkv"
                try:
                    subprocess.run(
                        [args.ffmpeg, "-hide_banner", "-loglevel", "error",
                         "-f", "hevc", "-i", raw, "-c", "copy", dst],
                        timeout=60)
                    if os.path.exists(dst) and os.path.getsize(dst) > 1024:
                        os.remove(raw)
                        print("[seg] %s (%d 字节)" % (os.path.basename(dst),
                                                      os.path.getsize(dst)),
                              file=sys.stderr, flush=True)
                    else:
                        os.remove(dst)
                except Exception as e:
                    print("[seg] 转封装失败: %s" % e, file=sys.stderr, flush=True)
            else:
                try: os.remove(raw)
                except Exception: pass
            seg["fh"] = None
        if args.segment > 0:
            seg_open(now)

    out = sys.stdout.buffer if args.out == "-" else None
    n = 0

    want = max(1, int(args.channel)) - 1   # 1 起 -> 0 起

    def on_frame(codec, channel, ts, data):
        nonlocal n
        # 只输出请求的镜头
        if channel != want:
            return
        n += 1
        if n <= 5 or n % 50 == 0:
            sys.stderr.write("[frame] #%d codec=%d ch=%d len=%d ts=%d\n"
                             % (n, codec, channel, len(data), ts))
            sys.stderr.flush()
        # 输出原始 H264/H265 码流（Annex-B）
        if args.segment > 0:
            now = time.time()
            if seg["fh"] is None:
                seg_open(now)
            try:
                seg["fh"].write(data)
            except Exception:
                pass
            # 到时间就换片
            if now - seg["t0"] >= args.segment:
                seg_roll(now)
        elif out is not None:
            out.write(data)
        return

    cam._on_frame = on_frame

    stop = {"v": False}
    def handler(sig, frm):
        stop["v"] = True
    signal.signal(signal.SIGTERM, handler)
    signal.signal(signal.SIGINT, handler)

    t0 = time.time()
    while not stop["v"]:
        time.sleep(0.5)
        if args.segment <= 0 and time.time() - t0 >= 30:
            break
    cam.stop()
    cam.free()
    if args.segment > 0:
        seg_roll(time.time())
    sys.stderr.write("[done] 累计 %d 帧\n" % n)
    return 0


if __name__ == "__main__":
    sys.exit(main())
