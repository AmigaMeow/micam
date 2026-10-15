#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
本地车牌识别常驻服务（HyperLPR3 via ctypes）。

协议（一行一个 JSON）：
  输入: {"images": ["/path/a.jpg", ...], "minConf": 0.85}
  输出: {"ok": true, "plate": "京A88888", "conf": 0.976,
         "votes": {"京A88888": 2, "京B88888": 1},
         "candidates": [{"code":"京A88888","conf":0.993,"src":"a.jpg"}],
         "raw": ["京A88888","京B88888"], "ms": 266}

为什么是常驻服务：
  HyperLPR 初始化要加载 8.4MB 模型（约 1 秒），每帧重新加载太浪费。
  常驻之后单帧只要 133ms。

校验机制（实测驱动 —— 省份汉字不稳定，数字部分 100% 稳定）：
  1. 多帧投票：同一车牌读 N 帧取多数
  2. 省简称白名单：必须是 31 个合法值
  3. 格式正则：^[省][A-Z][A-Z0-9]{5}$
  4. 排除易混字符 I / O
"""

import ctypes, json, os, sys, time
import numpy as np
import cv2

# ===== 先隔离 C 库的 stdout =====
#
# HyperLPR 的 .so 会往 fd 1 直接打印 [InferenceHelper] 之类日志，
# 会把我们的 JSON 协议弄脏。所以：
#   1. 复制一份 fd 1 给自己的输出（_OUT）
#   2. 把 fd 1 指向 /dev/null，让 C 库的日志沉掉
_OUT = None
if not os.environ.get("MICAM_LPR_KEEP_STDOUT"):
    _saved = os.dup(1)
    _devnull = os.open(os.devnull, os.O_WRONLY)
    os.dup2(_devnull, 1)
    _OUT = os.fdopen(_saved, "w", buffering=1)


def emit(obj):
    """输出一行 JSON（走未被污染的通道）"""
    s = json.dumps(obj, ensure_ascii=False)
    if _OUT:
        _OUT.write(s + "\n")
        _OUT.flush()
    else:
        print(s, flush=True)

LIB_PATH = os.environ.get("MICAM_LPR_LIB", "")
MODELS_PATH = os.environ.get("MICAM_LPR_MODELS", "")

# 31 个省级行政区简称（白名单）
PROVINCES = set("京津冀晋蒙辽吉黑沪苏浙皖闽赣鲁豫鄂湘粤桂琼渝川贵云藏陕甘青宁新")

PLATE_RE = None


def log(*a):
    print(*a, file=sys.stderr, flush=True)


# ---------------- ctypes 结构 ----------------

class Cfg(ctypes.Structure):
    _fields_ = [
        ("models_path", ctypes.c_char_p),
        ("max_num", ctypes.c_int),
        ("threads", ctypes.c_int),
        ("use_half", ctypes.c_bool),
        ("box_conf_threshold", ctypes.c_float),
        ("nms_threshold", ctypes.c_float),
        ("rec_confidence_threshold", ctypes.c_float),
        ("det_level", ctypes.c_int),
    ]


class ImageData(ctypes.Structure):
    _fields_ = [
        ("data", ctypes.POINTER(ctypes.c_ubyte)),
        ("width", ctypes.c_int),
        ("height", ctypes.c_int),
        ("format", ctypes.c_int),     # 1 = BGR
        ("rotation", ctypes.c_int),   # 0 = 不旋转
    ]


class Plate(ctypes.Structure):
    _fields_ = [
        ("x1", ctypes.c_float), ("y1", ctypes.c_float),
        ("x2", ctypes.c_float), ("y2", ctypes.c_float),
        ("ptype", ctypes.c_int),
        ("text_conf", ctypes.c_float),
        ("code", ctypes.c_char * 128),
    ]


class Plates(ctypes.Structure):
    _fields_ = [("plate_size", ctypes.c_ulong), ("plates", ctypes.POINTER(Plate))]


class LPR:
    def __init__(self, lib_path, models_path, threads=2):
        if not lib_path or not os.path.exists(lib_path):
            raise FileNotFoundError("找不到 libhyperlpr3.so: %s" % lib_path)
        if not models_path or not os.path.isdir(models_path):
            raise FileNotFoundError("找不到模型目录: %s" % models_path)

        lib = ctypes.CDLL(lib_path)
        lib.HLPR_CreateContext.argtypes = [ctypes.POINTER(Cfg)]
        lib.HLPR_CreateContext.restype = ctypes.c_void_p
        lib.HLPR_ContextQueryStatus.argtypes = [ctypes.c_void_p]
        lib.HLPR_ContextQueryStatus.restype = ctypes.c_int
        lib.HLPR_CreateDataBuffer.argtypes = [ctypes.POINTER(ImageData)]
        lib.HLPR_CreateDataBuffer.restype = ctypes.c_void_p
        lib.HLPR_ContextUpdateStream.argtypes = [ctypes.c_void_p, ctypes.c_void_p, ctypes.POINTER(Plates)]
        lib.HLPR_ContextUpdateStream.restype = ctypes.c_int
        lib.HLPR_ReleaseDataBuffer.argtypes = [ctypes.c_void_p]
        lib.HLPR_ReleaseContext.argtypes = [ctypes.c_void_p]
        self._lib = lib

        cfg = Cfg()
        cfg.models_path = models_path.encode()
        cfg.max_num = 5
        cfg.threads = threads
        cfg.use_half = False
        cfg.box_conf_threshold = 0.30
        cfg.nms_threshold = 0.50
        cfg.rec_confidence_threshold = 0.30
        cfg.det_level = 0

        self.ctx = lib.HLPR_CreateContext(ctypes.byref(cfg))
        if not self.ctx:
            raise RuntimeError("HLPR_CreateContext 返回空")
        st = lib.HLPR_ContextQueryStatus(self.ctx)
        if st != 0:
            raise RuntimeError("Context 状态异常: %d" % st)

    def read(self, path, box=None):
        """读一张图，优先在目标车辆包围盒内检测，避免多车并存时车牌混淆"""
        img = cv2.imread(path)
        if img is None:
            return []
        h, w = img.shape[:2]
        if box and len(box) >= 4:
            x1, y1, x2, y2 = box
            sx, sy = w / 1280.0, h / 720.0
            cx1 = max(0, int((x1 - 20) * sx))
            cy1 = max(0, int((y1 - 20) * sy))
            cx2 = min(w, int((x2 + 20) * sx))
            cy2 = min(h, int((y2 + 20) * sy))
            if cx2 > cx1 + 30 and cy2 > cy1 + 30:
                crop = img[cy1:cy2, cx1:cx2]
                res = self._read_mat(crop)
                if res:
                    return res
        return self._read_mat(img)

    def _read_mat(self, img):
        img = np.ascontiguousarray(img)
        h, w = img.shape[:2]
        d = ImageData()
        d.data = img.ctypes.data_as(ctypes.POINTER(ctypes.c_ubyte))
        d.width = w
        d.height = h
        d.format = 1
        d.rotation = 0
        buf = self._lib.HLPR_CreateDataBuffer(ctypes.byref(d))
        if not buf:
            return []
        try:
            res = Plates()
            self._lib.HLPR_ContextUpdateStream(self.ctx, buf, ctypes.byref(res))
            out = []
            for i in range(res.plate_size):
                p = res.plates[i]
                code = p.code.decode("utf-8", "ignore").strip()
                if code:
                    out.append((code, float(p.text_conf)))
            return out
        finally:
            self._lib.HLPR_ReleaseDataBuffer(buf)


# ---------------- 校验 ----------------

def valid_format(code):
    """车牌格式校验：省简称白名单 + 长度 + 字符集"""
    if not code or len(code) != 7:
        return False
    if code[0] not in PROVINCES:
        return False
    if not ("A" <= code[1] <= "Z"):
        return False
    for ch in code[2:]:
        if ch in ("I", "O"):
            return False
        if not (ch.isdigit() or ("A" <= ch <= "Z")):
            return False
    return True


def vote(reads, min_conf=0.85):
    """多帧投票 + 校验。

    实测：省份汉字不稳定（鄂/粤/粤H），数字部分 100% 稳定。
    所以投票能纠正省份错误。
    """
    tally = {}
    cands = []
    for code, conf, src in reads:
        cands.append({"code": code, "conf": round(conf, 4), "src": src})
        if conf < min_conf:
            continue
        if not valid_format(code):
            continue
        tally[code] = tally.get(code, 0) + 1

    if not tally:
        return None, 0.0, tally

    # 票数优先，票数相同看最高置信度
    best = None
    best_votes = -1
    best_conf = 0.0
    for code, n in tally.items():
        # 取该车牌在所有帧里的最高置信度（注意要取 cf 不是 c）
        top = max([cf for cc, cf in [(x[0], x[1]) for x in reads] if cc == code] or [0.0])
        if n > best_votes or (n == best_votes and top > best_conf):
            best, best_votes, best_conf = code, n, top
    return best, best_conf, tally


def main():
    lib = os.environ.get("MICAM_LPR_LIB", LIB_PATH)
    models = os.environ.get("MICAM_LPR_MODELS", MODELS_PATH)

    try:
        eng = LPR(lib, models)
    except Exception as e:
        # 启动失败：告诉上层，然后退出（上层会按需重启）
        emit({"ok": False, "error": "%s: %s" % (type(e).__name__, e)})
        return
    emit({"ok": True, "ready": True})
    log("[lpr] 就绪")

    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
        except Exception:
            continue

        if req.get("cmd") == "quit":
            break

        paths = req.get("images") or []
        min_conf = float(req.get("minConf", 0.85))
        box = req.get("box")
        t0 = time.time()

        reads = []
        for p in paths:
            for code, conf in eng.read(p, box):
                reads.append((code, conf, os.path.basename(p)))

        plate, conf, tally = vote(reads, min_conf)
        ms = int((time.time() - t0) * 1000)

        emit({
            "ok": True,
            "plate": plate or "",
            "conf": round(conf, 4),
            "votes": tally,
            "candidates": [{"code": c, "conf": cf, "src": s} for c, cf, s in reads],
            "raw": [c for c, _, _ in reads],
            "frames": len(paths),
            "ms": ms,
        })


if __name__ == "__main__":
    main()
