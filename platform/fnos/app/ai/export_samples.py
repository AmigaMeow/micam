#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
阶段 3 比对任务的样本导出工具。

用途：从录像里自动抽取候选的「变化对比对」，生成待人工标注的清单。

为什么要人工标注：
  实测证明合成样本无效（模型能识别出人造痕迹），
  所以必须用真实发生的画面，而且需要人工给出"真实答案"作为基准。

用法：
  python3 export_samples.py --out /tmp/samples --days 7
  python3 export_samples.py --out /tmp/samples --pairs-per-cam 12

产出：
  /tmp/samples/frames/*.jpg        抽出的帧
  /tmp/samples/manifest.json       待标注清单（officialChange 为空）
  /tmp/samples/annotate.html       标注界面（打开就能标）
"""

import argparse, json, os, subprocess, sys, glob
from datetime import datetime, timedelta

REC_ROOT = os.environ.get("MICAM_REC_ROOT", "/vol1/@appdata/micam/recordings")
POSE_FILE = os.environ.get("MICAM_POSE_FILE", "/vol1/@appdata/micam/camera-poses.json")


def log(*a):
    print(*a, file=sys.stderr, flush=True)


def ffprobe_duration(path):
    try:
        out = subprocess.run(
            ["ffprobe", "-v", "error", "-show_entries", "format=duration",
             "-of", "csv=p=0", path],
            capture_output=True, text=True, timeout=30).stdout.strip()
        return float(out) if out else 0.0
    except Exception:
        return 0.0


def extract_frame(src, t, dst, width=1600):
    """从录像抽一帧到 dst"""
    try:
        r = subprocess.run(
            ["ffmpeg", "-hide_banner", "-loglevel", "error",
             "-ss", "%.2f" % t, "-i", src, "-frames:v", "1",
             "-vf", "scale=%d:-2" % width, "-q:v", "2", "-y", dst],
            capture_output=True, timeout=90)
        return r.returncode == 0 and os.path.exists(dst) and os.path.getsize(dst) > 4096
    except Exception:
        return False


def list_recordings(days):
    """列出最近 N 天的录像文件"""
    out = []
    cutoff = (datetime.now() - timedelta(days=days)).strftime("%Y-%m-%d")
    for dev_dir in sorted(glob.glob(os.path.join(REC_ROOT, "*"))):
        if not os.path.isdir(dev_dir) or os.path.basename(dev_dir).startswith("."):
            continue
        dev = os.path.basename(dev_dir)
        for day_dir in sorted(glob.glob(os.path.join(dev_dir, "*"))):
            if not os.path.isdir(day_dir):
                continue
            day = os.path.basename(day_dir)
            if len(day) != 10 or day < cutoff:
                continue
            for f in sorted(glob.glob(os.path.join(day_dir, "*.mkv"))):
                out.append({"device": dev, "date": day, "file": f,
                            "name": os.path.basename(f)})
    return out


def load_poses():
    try:
        with open(POSE_FILE) as f:
            return json.load(f)
    except Exception:
        return {}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True, help="输出目录")
    ap.add_argument("--days", type=int, default=7, help="回溯天数")
    ap.add_argument("--pairs-per-cam", type=int, default=10, help="每台设备生成多少对")
    ap.add_argument("--gap", type=int, default=60, help="一对之间的最小间隔（秒）")
    args = ap.parse_args()

    os.makedirs(args.out, exist_ok=True)
    frames_dir = os.path.join(args.out, "frames")
    os.makedirs(frames_dir, exist_ok=True)

    recs = list_recordings(args.days)
    log("[导出] 找到 %d 个录像文件" % len(recs))
    if not recs:
        log("[导出] 没有录像。请先在插件里打开录像，等积累几天。")
        json.dump({"error": "no recordings", "samples": []},
                  open(os.path.join(args.out, "manifest.json"), "w"), ensure_ascii=False, indent=1)
        return

    poses = load_poses()

    # 按设备分组
    by_dev = {}
    for r in recs:
        by_dev.setdefault(r["device"], []).append(r)

    samples = []
    sid = 0

    for dev, files in by_dev.items():
        # 只从一个镜头的画面取（双摄要注意）
        log("[导出] %s: %d 个文件" % (dev, len(files)))

        # 在文件里等间隔抽帧
        pool = []
        for r in files:
            dur = ffprobe_duration(r["file"])
            if dur < 5:
                continue
            # 每个文件抽 3 帧（首/中/尾）
            for frac in (0.15, 0.5, 0.85):
                pool.append((r, dur * frac))

        if len(pool) < 2:
            continue
        # 按 (文件, 时间) 排序 —— 乱序配对会产出「同文件相隔 0.5 秒」这种无效对
        pool.sort(key=lambda x: (x[0]["file"], x[1]))

        made = 0
        i = 0
        while made < args.pairs_per_cam and i + 1 < len(pool):
            # 找一对时间上相隔足够远的
            (ra, ta) = pool[i]
            (rb, tb) = pool[i + 1]
            i += 1

            gap_sec = abs(ta - tb)
            same_file = (ra["file"] == rb["file"])
            # 同一文件要求时间差够；不同文件跨天也算有意义的对
            if same_file and gap_sec < args.gap:
                continue
            # 排除「相邻两帧几乎一样」的无效对（少于 10 秒且同文件）
            if same_file and gap_sec < 10:
                continue

            sid += 1
            base = "s%03d" % sid
            pa = os.path.join(frames_dir, base + "_before.jpg")
            pb = os.path.join(frames_dir, base + "_after.jpg")

            if not extract_frame(ra["file"], ta, pa):
                continue
            if not extract_frame(rb["file"], tb, pb):
                os.remove(pa)
                continue

            pose = poses.get("", {})
            # 机位信息（如果有记录，按设备名匹配）
            for k, v in poses.items():
                if v.get("device") == dev:
                    pose = v
                    break

            samples.append({
                "id": base,
                "kind": "compare",
                "device": dev,
                "before": {"file": os.path.relpath(pa, args.out), "date": ra["date"],
                           "src": ra["name"], "t": round(ta, 2)},
                "after": {"file": os.path.relpath(pb, args.out), "date": rb["date"],
                          "src": rb["name"], "t": round(tb, 2)},
                "gapHours": round(abs(
                    (datetime.strptime(rb["date"], "%Y-%m-%d") - datetime.strptime(ra["date"], "%Y-%m-%d")).total_seconds()) / 3600, 1),
                "pose": {"lens": pose.get("lens", 1),
                         "angle": pose.get("angle"),
                         "elevation": pose.get("elevation")},
                # ↓↓↓ 人工标注这三个字段 ↓↓↓
                "officialChange": None,   # true / false
                "changeType": "",         # none / vehicle / environment / other
                "note": "",
            })
            made += 1

    manifest = {
        "generatedAt": datetime.now().strftime("%Y-%m-%d %H:%M:%S"),
        "total": len(samples),
        "labeled": 0,
        "instructions": "打开 annotate.html 逐条标注 officialChange（真实答案）",
        "samples": samples,
    }
    mpath = os.path.join(args.out, "manifest.json")
    with open(mpath, "w") as f:
        json.dump(manifest, f, ensure_ascii=False, indent=1)

    write_annotator(args.out, samples)

    log("[导出] 完成：%d 对样本" % len(samples))
    log("[导出] 清单: %s" % mpath)
    log("[导出] 标注界面: %s" % os.path.join(args.out, "annotate.html"))
    print(json.dumps({"ok": True, "count": len(samples), "manifest": mpath}))


def write_annotator(out_dir, samples):
    """生成标注界面（打开就能看图打标，导出 JSON）"""
    html = """<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8">
<title>比对样本标注</title>
<style>
 body{background:#0e1116;color:#e6edf3;font-family:system-ui,-apple-system,"PingFang SC",sans-serif;margin:0;padding:20px}
 h1{font-size:18px;margin:0 0 6px}
 .sub{color:#6e7681;font-size:13px;margin-bottom:16px}
 .card{background:#161b22;border:1px solid #262c36;border-radius:10px;padding:14px;margin-bottom:14px}
 .pair{display:grid;grid-template-columns:1fr 1fr;gap:12px;margin-bottom:10px}
 .pair img{width:100%;border-radius:8px;display:block;background:#000}
 .cap{font-size:12px;color:#8b949e;margin-top:4px}
 .row{display:flex;align-items:center;gap:12px;flex-wrap:wrap}
 button{background:#21262d;border:1px solid #30363d;color:#e6edf3;padding:6px 14px;border-radius:6px;cursor:pointer;font-size:13px}
 button:hover{border-color:#8b949e}
 button.on{background:#1d4ed8;border-color:#1d4ed8;color:#fff}
 button.n{background:#b91c1c;border-color:#b91c1c;color:#fff}
 .note{flex:1;min-width:200px;background:#0d1117;border:1px solid #30363d;color:#e6edf3;padding:6px 10px;border-radius:6px;font-size:13px}
 .meta{font-size:12px;color:#6e7681;margin-top:8px}
 .bar{position:sticky;top:0;background:#0e1116;padding:10px 0;border-bottom:1px solid #262c36;margin-bottom:16px;z-index:9}
 .done{color:#4ade80}
</style></head><body>
<h1>比对样本标注</h1>
<div class="sub">判断「车辆本身」是否发生变化。光线/影子/狗走动不算车辆变化。</div>
<div class="bar">
  <div class="row">
    <b id="prog">0 / 0</b>
    <button onclick="exportJson()">导出 JSON</button>
    <button onclick="window.scrollTo(0,document.body.scrollHeight)">到底部</button>
    <span class="sub" style="margin:0">标注会存在浏览器本地，导出后覆盖 manifest.json</span>
  </div>
</div>
<div id="list"></div>
<script>
const DATA = __SAMPLES__;
const KEY = 'micam.compare.labels';
let labels = {};
try { labels = JSON.parse(localStorage.getItem(KEY) || '{}'); } catch(e) {}

function render(){
  const box = document.getElementById('list');
  box.innerHTML = DATA.map(s => {
    const L = labels[s.id] || {};
    const bOn = L.officialChange === true, nOn = L.officialChange === false;
    return '<div class="card" id="c-' + s.id + '">'
      + '<div class="pair">'
      +   '<div><img loading="lazy" src="' + s.before.file + '" alt=""><div class="cap">BEFORE · ' + s.before.date + ' · ' + s.before.src + ' @' + s.before.t + 's</div></div>'
      +   '<div><img loading="lazy" src="' + s.after.file + '" alt=""><div class="cap">AFTER · ' + s.after.date + ' · ' + s.after.src + ' @' + s.after.t + 's</div></div>'
      + '</div>'
      + '<div class="row">'
      +   '<b>' + s.id + '</b>'
      +   '<span class="sub" style="margin:0">' + s.device + '</span>'
      +   '<button class="' + (bOn?'on':'') + '" onclick="mark(\'' + s.id + '\',true)">有变化</button>'
      +   '<button class="' + (nOn?'n':'') + '" onclick="mark(\'' + s.id + '\',false)">无变化</button>'
      +   '<input class="note" placeholder="备注（例如：车尾多了划痕 / 光线变化）" value="' + (L.note||'') + '" oninput="note(\'' + s.id + '\',this.value)">'
      + '</div>'
      + '<div class="meta">间隔 ' + s.gapHours + ' 小时 · 镜头 ' + (s.pose&&s.pose.lens||1) + ' · 云台 ' + (s.pose&&s.pose.angle!=null?s.pose.angle:'-') + '°</div>'
      + '</div>';
  }).join('');
  updateProg();
}
function mark(id, v){ labels[id] = labels[id] || {}; labels[id].officialChange = v; save(); render(); }
function note(id, v){ labels[id] = labels[id] || {}; labels[id].note = v; save(); }
function save(){ localStorage.setItem(KEY, JSON.stringify(labels)); }
function updateProg(){
  const done = Object.values(labels).filter(x => x.officialChange !== undefined).length;
  const el = document.getElementById('prog');
  el.textContent = done + ' / ' + DATA.length;
  el.className = done === DATA.length ? 'done' : '';
}
function exportJson(){
  const out = DATA.map(s => Object.assign({}, s, labels[s.id] || {}));
  const blob = new Blob([JSON.stringify({samples: out}, null, 1)], {type:'application/json'});
  const a = document.createElement('a');
  a.href = URL.createObjectURL(blob);
  a.download = 'manifest.json';
  a.click();
}
render();
</script></body></html>"""
    html = html.replace("__SAMPLES__", json.dumps(samples, ensure_ascii=False))
    with open(os.path.join(out_dir, "annotate.html"), "w") as f:
        f.write(html)


if __name__ == "__main__":
    main()
