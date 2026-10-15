#!/usr/bin/env python3
# 相似图搜索：把查询图和候选图都转成向量，算余弦相似度。
#
# 用法：python3 embed.py <查询图> <候选1> <候选2> ...
# 输出：{"scores":[0.98, 0.87, ...], "ms": 1234}

import sys, os, json, time
import numpy as np
import cv2

MODEL_DIR = os.environ.get("MICAM_MODEL_DIR", "/vol1/@appdata/micam/models")
# 飞牛自带的 img2vec
SYS_MODEL = "/vol1/@sysappmeta/ai-manager/models/img2vec-large-model-1.0.0/img2vec.onnx"


def load():
    import onnxruntime as ort
    path = SYS_MODEL
    if not os.path.exists(path):
        # 兜底：用户自己放的
        alt = os.path.join(MODEL_DIR, "img2vec.onnx")
        if os.path.exists(alt):
            path = alt
        else:
            raise FileNotFoundError("未找到 img2vec 模型")
    so = ort.SessionOptions()
    so.intra_op_num_threads = 4
    so.graph_optimization_level = ort.GraphOptimizationLevel.ORT_ENABLE_ALL
    return ort.InferenceSession(path, so, providers=["CPUExecutionProvider"])


def embed(sess, inp_name, path):
    img = cv2.imread(path)
    if img is None:
        return None
    img = cv2.resize(img, (224, 224))
    img = cv2.cvtColor(img, cv2.COLOR_BGR2RGB)
    x = (img.astype(np.float32) / 255.0)
    x = np.transpose(x, (2, 0, 1))[None, ...]
    v = sess.run(None, {inp_name: x})[0][0]
    n = np.linalg.norm(v)
    return v / n if n > 0 else v


def main():
    if len(sys.argv) < 3:
        print(json.dumps({"error": "用法: embed.py <查询图> <候选...>"}))
        return
    t0 = time.time()
    try:
        sess = load()
        name = sess.get_inputs()[0].name
        q = embed(sess, name, sys.argv[1])
        if q is None:
            print(json.dumps({"error": "查询图无法解码"}))
            return
        scores = []
        for p in sys.argv[2:]:
            v = embed(sess, name, p)
            scores.append(float(np.dot(q, v)) if v is not None else 0.0)
        print(json.dumps({"scores": scores, "ms": int((time.time() - t0) * 1000)}))
    except Exception as e:
        print(json.dumps({"error": "%s: %s" % (type(e).__name__, e)}))


if __name__ == "__main__":
    main()
