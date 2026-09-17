package main

// 设备在线判断 + 抓高分辨率帧（给云端视觉分析用）。

import (
	"io"
	"net/http"
	"time"
)

// camOnline 设备是否在线
func (s *Server) camOnline(did string) bool {
	for _, d := range s.allCameras() {
		if d.Did == did {
			return d.IsOnline
		}
	}
	return false
}

// captureHiRes 抓一张较高分辨率的当前帧。
//
// 说明：云端视觉模型看细节（车牌/门牌）需要至少 720p，
// 而事件缩略图只有 320x180 —— 手机上根本看不清。
// 这里直接用本地 P2P 抓一帧，走内网，不额外花钱。
func (s *Server) captureHiRes(did string) []byte {
	// 走本地自身的抓图接口，避免重复实现 P2P 逻辑
	url := "http://127.0.0.1:8900/api/live/snapshot?did=" + did + "&w=1280"
	cli := &http.Client{Timeout: 25 * time.Second}
	resp, err := cli.Get(url)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil || len(raw) < 1024 {
		return nil
	}
	return raw
}
