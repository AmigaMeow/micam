#!/usr/bin/env python3
import ctypes, sys, time
import numpy as np, cv2

SO='/tmp/hlpr/hyperlpr3.0-share-linux-x86/lib/libhyperlpr3.so'
MODELS=b'/tmp/hlpr/models'
lib=ctypes.CDLL(SO)

class Cfg(ctypes.Structure):
    _fields_=[('models_path',ctypes.c_char_p),('max_num',ctypes.c_int),('threads',ctypes.c_int),
              ('use_half',ctypes.c_bool),('box_conf_threshold',ctypes.c_float),
              ('nms_threshold',ctypes.c_float),('rec_confidence_threshold',ctypes.c_float),
              ('det_level',ctypes.c_int)]
class ImageData(ctypes.Structure):
    _fields_=[('data',ctypes.POINTER(ctypes.c_ubyte)),('width',ctypes.c_int),('height',ctypes.c_int),
              ('format',ctypes.c_int),('rotation',ctypes.c_int)]
class Plate(ctypes.Structure):
    _fields_=[('x1',ctypes.c_float),('y1',ctypes.c_float),('x2',ctypes.c_float),('y2',ctypes.c_float),
              ('ptype',ctypes.c_int),('text_conf',ctypes.c_float),('code',ctypes.c_char*128)]
class Plates(ctypes.Structure):
    _fields_=[('plate_size',ctypes.c_ulong),('plates',ctypes.POINTER(Plate))]

lib.HLPR_CreateContext.argtypes=[ctypes.POINTER(Cfg)]; lib.HLPR_CreateContext.restype=ctypes.c_void_p
lib.HLPR_ContextQueryStatus.argtypes=[ctypes.c_void_p]; lib.HLPR_ContextQueryStatus.restype=ctypes.c_int
lib.HLPR_CreateDataBuffer.argtypes=[ctypes.POINTER(ImageData)]; lib.HLPR_CreateDataBuffer.restype=ctypes.c_void_p
lib.HLPR_ContextUpdateStream.argtypes=[ctypes.c_void_p,ctypes.c_void_p,ctypes.POINTER(Plates)]
lib.HLPR_ContextUpdateStream.restype=ctypes.c_int
lib.HLPR_ReleaseDataBuffer.argtypes=[ctypes.c_void_p]
lib.HLPR_ReleaseContext.argtypes=[ctypes.c_void_p]

cfg=Cfg(); cfg.models_path=MODELS; cfg.max_num=5; cfg.threads=2; cfg.use_half=False
cfg.box_conf_threshold=0.3; cfg.nms_threshold=0.5; cfg.rec_confidence_threshold=0.3; cfg.det_level=0
ctx=lib.HLPR_CreateContext(ctypes.byref(cfg))
print('Context status=%d (0=Ok)' % lib.HLPR_ContextQueryStatus(ctx), flush=True)
if not ctx: sys.exit(1)
print()
print('=== 目标（人工确认）: 京A·88888 ===')
print()

def run(path,label):
    img=cv2.imread(path)
    if img is None: print('  %-22s 读不到'%label); return
    img=np.ascontiguousarray(img)
    h,w=img.shape[:2]
    d=ImageData()
    d.data=img.ctypes.data_as(ctypes.POINTER(ctypes.c_ubyte))
    d.width=w; d.height=h; d.format=1; d.rotation=0   # 1=BGR, 0=rot0
    buf=lib.HLPR_CreateDataBuffer(ctypes.byref(d))
    if not buf:
        print('  %-22s CreateDataBuffer 失败'%label); return
    res=Plates()
    t0=time.time(); r=lib.HLPR_ContextUpdateStream(ctx,buf,ctypes.byref(res)); el=(time.time()-t0)*1000
    print('  %-22s %4dx%-5d ret=%d %.0fms 检出%d' % (label,w,h,r,el,res.plate_size), flush=True)
    for i in range(res.plate_size):
        p=res.plates[i]
        print('       >>> 【%s】 conf=%.3f' % (p.code.decode('utf-8','ignore').strip(), p.text_conf), flush=True)
    lib.HLPR_ReleaseDataBuffer(buf)
    print()

run('/tmp/plate-native.png','车牌裁剪 260x110')
run('/tmp/plate-frame.jpg','整幅 2560px')
run('/tmp/plate-1280.jpg','整幅 1280px')
run('/tmp/cmp-a.jpg','整幅 1024px')
run('/tmp/s-scratch.jpg','划痕样本 1024px')
lib.HLPR_ReleaseContext(ctx)
