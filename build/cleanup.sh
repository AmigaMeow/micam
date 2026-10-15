#!/bin/bash
# 清理调试残留（2026-10-13 教训）
#
# 背景：调试时反复建连，残留的 sidecar 会一直占着摄像头唯一的 PPCS 连接槽，
# 导致用户看到「超过最大同时连接数量」，连米家 App 都被挤掉。
# 另外 tcpdump 常驻会一直占着已删除 pcap 的文件句柄，把磁盘占满。
#
# 用法：bash build/cleanup.sh
set -u

echo "=== 清理 sidecar（会占摄像头连接槽）==="
for p in $(pgrep -f 'miot_mjpeg.py|miot_camera.py' 2>/dev/null); do
  c=$(tr '\0' ' ' < /proc/$p/cmdline 2>/dev/null)
  case "$c" in
    *python*) echo "  kill $p"; kill -9 "$p" 2>/dev/null ;;
  esac
done

echo "=== 清理抓包进程（会占磁盘句柄）==="
for p in $(pgrep -x tcpdump 2>/dev/null); do
  echo "  kill tcpdump $p"; kill -9 "$p" 2>/dev/null
done

echo "=== 清理调试工具进程 ==="
for p in $(pgrep -x gdb 2>/dev/null); do kill -9 "$p" 2>/dev/null; done
for p in $(pgrep -f 'mitmdump' 2>/dev/null); do
  c=$(tr '\0' ' ' < /proc/$p/cmdline 2>/dev/null)
  case "$c" in *mitmdump*) kill -9 "$p" 2>/dev/null ;; esac
done

echo "=== 清理调试临时文件 ==="
rm -f /tmp/*.pcap /tmp/*.mjpeg /tmp/*.jpg /tmp/lib_write.log \
      /tmp/all_raw*.jsonl /tmp/mitm_raw.json /tmp/plain*.bin 2>/dev/null

sleep 2
echo
echo "=== 结果 ==="
echo "  残留 sidecar : $(pgrep -fc 'miot_mjpeg|miot_camera' 2>/dev/null || echo 0)"
echo "  残留 tcpdump : $(pgrep -xc tcpdump 2>/dev/null || echo 0)"
echo "  到摄像头连接 : $(ss -tn 2>/dev/null | grep -c '192.168.1.')"
df -h / 2>/dev/null | tail -1 | awk '{print "  根分区       : " $5 " 已用（可用 " $4 "）"}'
echo
echo "提示：部署前/调试后都跑一次，避免残留占着摄像头连接槽。"
