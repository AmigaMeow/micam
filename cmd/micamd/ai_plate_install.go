package main

// 录像抽帧（指定时刻）——车牌识别用的辅助。

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/micam/micam/internal/event"
)

// frameFromRecordingAt 从事件关联录像的指定偏移处抽一帧（高分辨率）。
func (s *Server) frameFromRecordingAt(ev *event.Event, t float64) []byte {
	date := ""
	if len(ev.Time) >= 10 {
		date = ev.Time[:10]
	}
	if t < 0 {
		t = 0
	}
	u := "http://127.0.0.1:8900/api/recordings/frame" +
		"?name=" + url.QueryEscape(ev.Device) +
		"&date=" + url.QueryEscape(date) +
		"&file=" + url.QueryEscape(ev.RecFile) +
		"&t=" + fmt.Sprintf("%.2f", t) +
		"&w=1600"
	cli := &http.Client{Timeout: 40 * time.Second}
	resp, err := cli.Get(u)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 6<<20))
	if err != nil {
		return nil
	}
	return raw
}

// installLPRResources 下载 HyperLPR 资源（.so + .mnn 模型）。
//
// ===== 实测得到的两个坑 =====
//
//  1. 发布的 .so 期望 rpv3_mdict_160_r3.mnn，但仓库里叫 rpv3_mdict_160h.mnn
//     -> 下载后复制改名
//  2. 模型目录必须包含全部 6 个 .mnn 文件
func installLPRResources() error {
	base := filepath.Join(dataDirPath, "lpr")
	libDir := filepath.Join(base, "lib")
	modelDir := filepath.Join(base, "models")
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		return err
	}

	// 1) 下载并解压 .so
	zipURLs := []string{
		"https://github.com/szad670401/HyperLPR/releases/download/v3.0/hyperlpr3.0-share-linux-x86.zip",
		"https://ghproxy.net/https://github.com/szad670401/HyperLPR/releases/download/v3.0/hyperlpr3.0-share-linux-x86.zip",
	}
	zipPath := filepath.Join(base, "hlpr.zip")
	if _, err := os.Stat(zipPath); err != nil {
		if err := downloadFile(zipURLs, zipPath); err != nil {
			return fmt.Errorf("下载库失败: %w", err)
		}
	}
	if err := unzipTo(zipPath, base); err != nil {
		return fmt.Errorf("解压失败: %w", err)
	}
	// 解压后路径: base/hyperlpr3.0-share-linux-x86/lib/libhyperlpr3.so
	src := filepath.Join(base, "hyperlpr3.0-share-linux-x86", "lib", "libhyperlpr3.so")
	if raw, err := os.ReadFile(src); err == nil {
		if err := os.WriteFile(filepath.Join(libDir, "libhyperlpr3.so"), raw, 0o755); err != nil {
			logf("[lpr] 写入 libhyperlpr3.so 失败: %v", err)
		}
	}

	// 2) 下载 6 个模型
	rawBase := "https://raw.githubusercontent.com/szad670401/HyperLPR/master/resource/models/r2_mobile"
	names := []string{
		"b320_backbone_h.mnn", "b320_header_h.mnn",
		"b640x_backbone_h.mnn", "b640x_head_h.mnn",
		"litemodel_cls_96xh.mnn", "rpv3_mdict_160h.mnn",
	}
	for _, n := range names {
		dst := filepath.Join(modelDir, n)
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		if err := downloadFile([]string{rawBase + "/" + n}, dst); err != nil {
			return fmt.Errorf("下载模型 %s 失败: %w", n, err)
		}
	}
	// 3) 坑 1：改名匹配（.so 期望 _r3 后缀）
	srcMnn := filepath.Join(modelDir, "rpv3_mdict_160h.mnn")
	dstMnn := filepath.Join(modelDir, "rpv3_mdict_160_r3.mnn")
	if _, err := os.Stat(dstMnn); err != nil {
		if raw, err := os.ReadFile(srcMnn); err == nil {
			if err := os.WriteFile(dstMnn, raw, 0o644); err != nil {
				logf("[lpr] 写入模型文件 %s 失败: %v", dstMnn, err)
			}
		}
	}
	return nil
}
