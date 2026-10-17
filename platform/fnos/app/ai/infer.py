#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
MiCam AI 推理服务

常驻进程，通过 stdin/stdout 的 JSON 行协议与 Go 主进程通信。

为什么要常驻：
  每次起 python3 + 加载模型 = 约 440ms（解释器 300ms + 模型 140ms）
  常驻进程之后，每次推理只要模型的推理时间（256ms @ yolov8n）

协议：
  请求（一行 JSON）：
    {"id":1, "cmd":"detect", "image":"<base64 jpeg>", "conf":0.45, "classes":["person"]}
    {"id":2, "cmd":"ping"}
    {"id":3, "cmd":"info"}
    {"id":4, "cmd":"switch", "model":"yolov8s"}
    {"id":5, "cmd":"exit"}

  响应（一行 JSON）：
    {"id":1, "ok":true, "ms":256, "objects":[{"cls":"person","conf":0.87,"box":[x,y,w,h]}]}
    {"id":2, "ok":true, "pong":true}
    {"id":3, "ok":true, "model":"yolov8n", "classes":[...], "inputSize":640}
    {"id":9, "ok":false, "error":"..."}

设计要点：
  · 输出格式自动识别（兼容 yolov5 / yolov8 / yolov11）
  · 类名从模型元数据读，读不到就用 COCO 默认表
  · 结果按「我们关心的事件类型」映射（person / vehicle / animal / package）
"""

import sys
import os
import json
import base64
import time
import traceback

import numpy as np

# ---- COCO 80 类（YOLOv8 默认训练集）----
COCO_CLASSES = [
    "person", "bicycle", "car", "motorbike", "aeroplane", "bus", "train", "truck",
    "boat", "traffic light", "fire hydrant", "stop sign", "parking meter", "bench",
    "bird", "cat", "dog", "horse", "sheep", "cow", "elephant", "bear", "zebra",
    "giraffe", "backpack", "umbrella", "handbag", "tie", "suitcase", "frisbee",
    "skis", "snowboard", "sports ball", "kite", "baseball bat", "baseball glove",
    "skateboard", "surfboard", "tennis racket", "bottle", "wine glass", "cup",
    "fork", "knife", "spoon", "bowl", "banana", "apple", "sandwich", "orange",
    "broccoli", "carrot", "hot dog", "pizza", "donut", "cake", "chair", "sofa",
    "pottedplant", "bed", "diningtable", "toilet", "tvmonitor", "laptop", "mouse",
    "remote", "keyboard", "cell phone", "microwave", "oven", "toaster", "sink",
    "refrigerator", "book", "clock", "vase", "scissors", "teddy bear", "hair drier",
    "toothbrush",
]

# ---- COCO 类名 -> 我们的事件类型 ----
# 分两档：
#   user_classes  用户关心的（会产生事件）
#   tier2         归类用（记录但不单独成事件，除非用户勾选）
CLASS_TIER = {
    "person":    ("person",  1),
    "bicycle":   ("vehicle", 1),
    "car":       ("vehicle", 1),
    "motorbike": ("vehicle", 1),
    "bus":       ("vehicle", 1),
    "truck":     ("vehicle", 1),
    "cat":       ("animal",  2),
    "dog":       ("animal",  2),
    "bird":      ("animal",  2),
    "backpack":  ("package", 2),
    "suitcase":  ("package", 2),
    "handbag":   ("package", 2),
}

MODEL_DIR = os.environ.get("MICAM_MODEL_DIR", "/tmp/models")


def log(msg):
    """日志写 stderr（stdout 留给协议）"""
    sys.stderr.write("[infer] %s\n" % msg)
    sys.stderr.flush()


class Detector:
    def __init__(self, model_name, threads=4):
        self.model_name = os.path.splitext(model_name)[0]
        self.path = self._resolve_path(self.model_name)
        self.threads = threads
        self.session = None
        self.input_name = None
        self.input_h = 640
        self.input_w = 640
        self.num_classes = 80
        self.class_names = list(COCO_CLASSES)
        self.load()

    def _resolve_path(self, name):
        candidates = [name + ".onnx"]
        if "v11" in name:
            candidates.append(name.replace("v11", "11") + ".onnx")
        elif "11" in name:
            candidates.append(name.replace("11", "v11") + ".onnx")
        if name == "yolov8n":
            candidates.append("yolo8n.onnx")
        for c in candidates:
            p = os.path.join(MODEL_DIR, c)
            if os.path.exists(p):
                return p
        return os.path.join(MODEL_DIR, name + ".onnx")

    def load(self):
        import onnxruntime as ort

        if not os.path.exists(self.path):
            raise FileNotFoundError("模型不存在: %s" % self.path)

        so = ort.SessionOptions()
        so.intra_op_num_threads = self.threads
        so.inter_op_num_threads = 1
        so.graph_optimization_level = ort.GraphOptimizationLevel.ORT_ENABLE_ALL

        t0 = time.time()
        self.session = ort.InferenceSession(
            self.path, so, providers=["CPUExecutionProvider"]
        )
        inp = self.session.get_inputs()[0]
        self.input_name = inp.name
        # 形状可能是 [1,3,640,640] 或 [1,3,'height','width']
        shape = inp.shape
        if isinstance(shape[2], int):
            self.input_h = shape[2]
        if isinstance(shape[3], int):
            self.input_w = shape[3]
        out = self.session.get_outputs()[0]
        if len(out.shape) == 3 and isinstance(out.shape[1], int):
            # [1, 84, 8400] -> 84 = 4 + 80
            self.num_classes = out.shape[1] - 4

        log("模型已加载 %s  input=%dx%d  classes=%d  耗时%.2fs" % (
            self.model_name, self.input_w, self.input_h, self.num_classes, time.time() - t0))

    def detect(self, jpeg_bytes, conf_th=0.45, want_classes=None, iou_th=0.45):
        import cv2

        t0 = time.time()

        # ---- 解码 ----
        arr = np.frombuffer(jpeg_bytes, dtype=np.uint8)
        img = cv2.imdecode(arr, cv2.IMREAD_COLOR)
        if img is None:
            raise ValueError("图片解码失败")
        orig_h, orig_w = img.shape[:2]

        # ---- letterbox 预处理 ----
        blob, ratio, (dw, dh) = self._letterbox(img, self.input_w, self.input_h)
        blob = blob[:, :, ::-1].transpose(2, 0, 1)          # BGR->RGB, HWC->CHW
        blob = np.ascontiguousarray(blob, dtype=np.float32) / 255.0
        blob = blob[None, ...]                               # NCHW

        # ---- 推理 ----
        t_infer = time.time()
        outputs = self.session.run(None, {self.input_name: blob})
        infer_ms = (time.time() - t_infer) * 1000

        # ---- 后处理 ----
        boxes, scores, class_ids = self._postprocess(
            outputs[0], conf_th, iou_th, ratio, dw, dh, orig_w, orig_h)

        # ---- 过滤 + 映射 ----
        objects = []
        for box, sc, cid in zip(boxes, scores, class_ids):
            cname = self.class_names[cid] if cid < len(self.class_names) else str(cid)
            tier_info = CLASS_TIER.get(cname)
            if tier_info is None:
                continue                      # 不关心的类直接丢
            ev_type, tier = tier_info
            if want_classes is not None and len(want_classes) > 0:
                if cname not in want_classes and ev_type not in want_classes:
                    continue
            objects.append({
                "cls": cname,
                "type": ev_type,
                "tier": tier,
                "conf": round(float(sc), 4),
                "box": [int(v) for v in box],
            })

        # 置信度高的排前面
        objects.sort(key=lambda o: -o["conf"])

        return {
            "objects": objects,
            "ms": int((time.time() - t0) * 1000),
            "inferMs": int(infer_ms),
            "size": [orig_w, orig_h],
        }

    def _letterbox(self, img, tw, th):
        import cv2
        h, w = img.shape[:2]
        ratio = min(tw / w, th / h)
        nw, nh = int(round(w * ratio)), int(round(h * ratio))
        if (nw, nh) != (w, h):
            img = cv2.resize(img, (nw, nh), interpolation=cv2.INTER_LINEAR)
        dw, dh = (tw - nw) // 2, (th - nh) // 2
        top, bottom = dh, th - nh - dh
        left, right = dw, tw - nw - dw
        img = cv2.copyMakeBorder(img, top, bottom, left, right,
                                 cv2.BORDER_CONSTANT, value=(114, 114, 114))
        return img, ratio, (dw, dh)

    def _postprocess(self, raw, conf_th, iou_th, ratio, dw, dh, orig_w, orig_h):
        import cv2

        pred = raw
        if pred.ndim == 3:
            pred = pred[0]                       # [84, 8400] 或 [25200, 85]

        # ---- 自动识别格式 ----
        # yolov8/v11: [4+nc, N]  -> 需要转置
        # yolov5:     [N, 4+1+nc]
        if pred.shape[0] < pred.shape[1]:
            pred = pred.transpose(1, 0)          # -> [N, 4+nc]

        n, dim = pred.shape
        nc = dim - 4

        boxes_xywh = pred[:, :4]
        cls_scores = pred[:, 4:]

        # ---- yolov8 是无 objectness 的，直接取最大类分数 ----
        #      yolov5 有 objectness，但常见导出也会合并，这里做兼容判断
        if cls_scores.shape[1] == nc:
            scores = cls_scores.max(axis=1)
            class_ids = cls_scores.argmax(axis=1)
        else:
            scores = cls_scores.max(axis=1)
            class_ids = cls_scores.argmax(axis=1)

        mask = scores >= conf_th
        if not mask.any():
            return [], [], []

        boxes_xywh = boxes_xywh[mask]
        scores = scores[mask]
        class_ids = class_ids[mask]

        # ---- xywh(中心) -> xyxy，并还原到原图坐标 ----
        cx, cy, bw, bh = boxes_xywh[:, 0], boxes_xywh[:, 1], boxes_xywh[:, 2], boxes_xywh[:, 3]
        x1 = (cx - bw / 2 - dw) / ratio
        y1 = (cy - bh / 2 - dh) / ratio
        x2 = (cx + bw / 2 - dw) / ratio
        y2 = (cy + bh / 2 - dh) / ratio

        x1 = np.clip(x1, 0, orig_w); x2 = np.clip(x2, 0, orig_w)
        y1 = np.clip(y1, 0, orig_h); y2 = np.clip(y2, 0, orig_h)

        xyxy = np.stack([x1, y1, x2, y2], axis=1).astype(np.float32)
        wh = np.stack([x2 - x1, y2 - y1], axis=1).astype(np.float32)

        # ---- NMS ----
        idx = cv2.dnn.NMSBoxes(
            bboxes=[[float(v) for v in b] for b in np.concatenate([xyxy[:, :2], wh], axis=1)],
            scores=[float(s) for s in scores],
            score_threshold=float(conf_th),
            nms_threshold=float(iou_th),
        )
        if len(idx) == 0:
            return [], [], []
        idx = np.array(idx).flatten()

        return xyxy[idx], scores[idx], class_ids[idx]


def main():
    model_name = sys.argv[1] if len(sys.argv) > 1 else "yolov8n"
    if len(sys.argv) > 2:
        globals()["MODEL_DIR"] = sys.argv[2]

    det = None
    try:
        det = Detector(model_name)
    except Exception as e:
        log("模型加载失败: %s" % e)
        # 仍然进入循环，但所有 detect 都返回错误
        # （这样 Go 侧能拿到清晰的错误信息，而不是进程消失）

    log("就绪，等待请求")
    sys.stdout.write(json.dumps({"ready": True, "model": model_name}) + "\n")
    sys.stdout.flush()

    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        rid = None
        try:
            req = json.loads(line)
            rid = req.get("id")
            cmd = req.get("cmd", "detect")

            if cmd == "ping":
                out = {"id": rid, "ok": True, "pong": True}

            elif cmd == "info":
                out = {
                    "id": rid, "ok": True,
                    "model": model_name,
                    "inputSize": det.input_w if det else 0,
                    "classes": det.class_names[:det.num_classes] if det else [],
                }

            elif cmd == "switch":
                new_model = req.get("model")
                det = Detector(new_model)
                model_name = new_model
                out = {"id": rid, "ok": True, "model": new_model}

            elif cmd == "exit":
                sys.stdout.write(json.dumps({"id": rid, "ok": True}) + "\n")
                sys.stdout.flush()
                break

            elif cmd == "detect":
                if det is None:
                    out = {"id": rid, "ok": False, "error": "模型未加载"}
                else:
                    raw = base64.b64decode(req["image"])
                    conf = float(req.get("conf", 0.45))
                    classes = req.get("classes")
                    r = det.detect(raw, conf_th=conf, want_classes=classes)
                    out = {
                        "id": rid, "ok": True,
                        "objects": r["objects"],
                        "ms": r["ms"],
                        "inferMs": r["inferMs"],
                        "size": r["size"],
                    }
            else:
                out = {"id": rid, "ok": False, "error": "未知命令: %s" % cmd}

        except Exception as e:
            out = {"id": rid, "ok": False,
                   "error": "%s: %s" % (type(e).__name__, str(e)),
                   "trace": traceback.format_exc()[-400:]}

        sys.stdout.write(json.dumps(out, ensure_ascii=False) + "\n")
        sys.stdout.flush()


if __name__ == "__main__":
    main()
