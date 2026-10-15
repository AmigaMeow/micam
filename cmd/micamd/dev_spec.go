package main

// API 规格文档（OpenAPI 3.0）。
//
// 这是给第三方客户端（Mac App、Home Assistant、脚本等）的接入契约。
// 改接口时必须同步更新这里。

import (
	"net/http"
)

// openAPISpec 生成 OpenAPI 3.0 规格。
func (s *Server) openAPISpec(r *http.Request) map[string]any {
	okResp := func(desc string) map[string]any {
		return map[string]any{
			"200": map[string]any{
				"description": desc,
				"content": map[string]any{
					"application/json": map[string]any{"schema": map[string]any{"type": "object"}},
				},
			},
		}
	}

	strParam := func(name, desc string, required bool) map[string]any {
		return map[string]any{
			"name": name, "in": "query", "required": required,
			"description": desc,
			"schema":      map[string]any{"type": "string"},
		}
	}

	paths := map[string]any{
		"/health": map[string]any{
			"get": map[string]any{
				"summary":     "健康检查",
				"description": "返回应用名、状态与版本号。客户端启动时先调这个。",
				"tags":        []string{"系统"},
				"responses":   okResp(`{"app":"micam","ok":true,"version":"x.y.z"}`),
			},
		},
		"/platform": map[string]any{
			"get": map[string]any{
				"summary":   "平台信息",
				"tags":      []string{"系统"},
				"responses": okResp("运行平台信息"),
			},
		},
		"/accounts": map[string]any{
			"get": map[string]any{
				"summary":   "账号列表",
				"tags":      []string{"账号"},
				"responses": okResp("小米账号列表（不含凭据）"),
			},
		},
		"/accounts/qr": map[string]any{
			"post": map[string]any{
				"summary":     "发起扫码登录",
				"description": "返回二维码图片 URL 与登录 URL。",
				"tags":        []string{"账号"},
				"responses":   okResp("二维码信息"),
			},
		},
		"/accounts/qr/status": map[string]any{
			"get": map[string]any{
				"summary":     "轮询扫码状态",
				"description": "状态：waiting / scanned / confirmed / expired",
				"tags":        []string{"账号"},
				"responses":   okResp("扫码状态"),
			},
		},
		"/devices": map[string]any{
			"get": map[string]any{
				"summary":     "摄像头列表",
				"description": "只返回摄像头（含共享家庭的）。字段：did/name/model/isOnline/localip/shared/shareFlag",
				"tags":        []string{"设备"},
				"responses":   okResp("摄像头列表"),
			},
		},
		"/devices/all": map[string]any{
			"get": map[string]any{
				"summary":     "全部设备列表",
				"description": "包含非摄像头设备。字段含 type/isCamera。",
				"tags":        []string{"设备"},
				"responses":   okResp("全部设备"),
			},
		},
		"/homes": map[string]any{
			"get": map[string]any{
				"summary":     "家庭列表",
				"description": "含共享家庭（shared=true）。共享设备必须用该家庭的 ownerId 查询。",
				"tags":        []string{"设备"},
				"responses":   okResp("家庭列表"),
			},
		},
		"/live/hls": map[string]any{
			"get": map[string]any{
				"summary":     "HLS 播放（推荐）",
				"description": "返回 m3u8 播放列表。分片通过同一路径 + seg 参数获取。注意：m3u8 里的分片行是相对路径（hls?did=X&seg=seg0.ts），播放器按相对路径解析即可，不要手工拼接。",
				"tags":        []string{"播放"},
				"parameters": []any{
					strParam("did", "设备 DID", true),
					strParam("fps", "帧率，默认 15，上限 15", false),
					strParam("w", "宽度，0=原画（2K 2560），可选 1280/640", false),
					strParam("seg", "分片文件名（如 seg0.ts），留空返回播放列表", false),
				},
				"responses": okResp("m3u8 播放列表 或 video/mp2t 分片"),
			},
		},
		"/live/hls/info": map[string]any{
			"get": map[string]any{
				"summary":   "HLS 能力信息",
				"tags":      []string{"播放"},
				"responses": okResp("硬件能力与编码器"),
			},
		},
		"/live/mjpeg": map[string]any{
			"get": map[string]any{
				"summary":     "MJPEG 流（兼容模式）",
				"description": "multipart/x-mixed-replace，boundary=mjpegframe。每帧带准确 Content-Length。注意：CFNetwork 读不了这个类型，iOS/macOS 需用裸 socket 读取。",
				"tags":        []string{"播放"},
				"parameters": []any{
					strParam("did", "设备 DID", true),
					strParam("fps", "帧率，默认 12", false),
					strParam("w", "宽度，0=原画", false),
				},
				"responses": map[string]any{
					"200": map[string]any{
						"description": "MJPEG 流",
						"content": map[string]any{
							"multipart/x-mixed-replace": map[string]any{
								"schema": map[string]any{"type": "string", "format": "binary"},
							},
						},
					},
				},
			},
		},
		"/live/snapshot": map[string]any{
			"get": map[string]any{
				"summary": "抓拍单帧",
				"tags":    []string{"播放"},
				"parameters": []any{
					strParam("did", "设备 DID", true),
					strParam("w", "宽度，0=原画", false),
				},
				"responses": map[string]any{
					"200": map[string]any{
						"description": "JPEG 图片",
						"content": map[string]any{
							"image/jpeg": map[string]any{"schema": map[string]any{"type": "string", "format": "binary"}},
						},
					},
				},
			},
		},
		"/live/source-status": map[string]any{
			"get": map[string]any{
				"summary":     "凭据状态",
				"description": "查询各设备的 cs2 凭据缓存时间与刷新冷却。",
				"tags":        []string{"播放"},
				"responses":   okResp("凭据状态列表"),
			},
		},
		"/ptz/status": map[string]any{
			"get": map[string]any{
				"summary":    "云台位置查询",
				"tags":       []string{"云台"},
				"parameters": []any{strParam("did", "设备 DID", true)},
				"responses":  okResp(`{"ok":true,"ret":0,"angle":50,"elevation":73}`),
			},
		},
		"/ptz/move": map[string]any{
			"post": map[string]any{
				"summary":     "云台移动",
				"description": "operation: 1=右 2=左 3=上 4=下。注意：绝对禁止发 5（固件巡航/校准）。同一设备同时只能有一路连接，云台会短暂占用通道。",
				"tags":        []string{"云台"},
				"requestBody": map[string]any{
					"required": true,
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"did":       map[string]any{"type": "string"},
									"operation": map[string]any{"type": "integer", "minimum": 1, "maximum": 4},
								},
								"required": []string{"did", "operation"},
							},
						},
					},
				},
				"responses": okResp(`{"ok":true,"ret":0,"angle":50,"elevation":73}`),
			},
		},
		"/recordings": map[string]any{
			"get":  map[string]any{"summary": "录像任务列表", "tags": []string{"录像"}, "responses": okResp("正在录像的任务")},
			"post": map[string]any{"summary": "开始录像", "tags": []string{"录像"}, "responses": okResp("已启动")},
		},
		"/recordings/stop": map[string]any{
			"post": map[string]any{"summary": "停止录像", "tags": []string{"录像"}, "responses": okResp("已停止")},
		},
		"/recordings/files": map[string]any{
			"get": map[string]any{
				"summary":    "录像文件列表",
				"tags":       []string{"录像"},
				"parameters": []any{strParam("name", "设备名（目录名）", false)},
				"responses":  okResp("文件列表"),
			},
		},
		"/recordings/dates": map[string]any{
			"get": map[string]any{
				"summary":     "录像可用日期",
				"description": "按日期聚合，返回每天的录像数量和占用大小。",
				"tags":        []string{"录像"},
				"parameters":  []any{strParam("name", "设备名（目录名）", false)},
				"responses":   okResp("日期列表"),
			},
		},
		"/recordings/list": map[string]any{
			"get": map[string]any{
				"summary":     "录像文件列表",
				"description": "返回文件列表，含时长、大小、缩略图 URL、播放 URL。",
				"tags":        []string{"录像"},
				"parameters": []any{
					strParam("name", "设备名", true),
					strParam("date", "日期（可空 = 全部）", false),
				},
				"responses": okResp("文件列表"),
			},
		},
		"/recordings/play": map[string]any{
			"get": map[string]any{
				"summary": "录像回放（转码）",
				"description": "把录像转码成 H.264 MP4 流式输出。" +
					"首次播放会同时后台生成缓存；二次播放直接发缓存文件，" +
					"支持 Range，可随意拖动。" +
					"注意：源文件是 MKV+HEVC，浏览器都不支持，必须转码。",
				"tags": []string{"录像"},
				"parameters": []any{
					strParam("name", "设备名", true),
					strParam("date", "日期", false),
					strParam("file", "文件名（.mkv）", true),
					strParam("t", "起始秒数（拖动用）", false),
				},
				"responses": map[string]any{
					"200": map[string]any{
						"description": "MP4 流（缓存命中时支持 Range）",
						"content": map[string]any{
							"video/mp4": map[string]any{"schema": map[string]any{"type": "string", "format": "binary"}},
						},
					},
				},
			},
		},
		"/recordings/thumb": map[string]any{
			"get": map[string]any{
				"summary":     "录像缩略图",
				"description": "取第 1 秒的帧，320 宽，磁盘缓存。",
				"tags":        []string{"录像"},
				"parameters": []any{
					strParam("name", "设备名", true),
					strParam("date", "日期", false),
					strParam("file", "文件名", true),
				},
				"responses": map[string]any{
					"200": map[string]any{
						"description": "JPEG 缩略图",
						"content": map[string]any{
							"image/jpeg": map[string]any{"schema": map[string]any{"type": "string", "format": "binary"}},
						},
					},
				},
			},
		},
		"/recordings/download": map[string]any{
			"get": map[string]any{
				"summary": "下载录像原文件",
				"tags":    []string{"录像"},
				"parameters": []any{
					strParam("name", "设备名", true),
					strParam("date", "日期", false),
					strParam("file", "文件名", true),
				},
				"responses": map[string]any{"200": map[string]any{"description": "MKV 文件"}},
			},
		},
		"/recordings/delete": map[string]any{
			"post": map[string]any{
				"summary": "删除单个录像文件",
				"tags":    []string{"录像"},
				"requestBody": map[string]any{
					"required": true,
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"name": map[string]any{"type": "string"},
									"date": map[string]any{"type": "string"},
									"file": map[string]any{"type": "string"},
								},
								"required": []string{"name", "file"},
							},
						},
					},
				},
				"responses": okResp("ok + freedMB"),
			},
		},
		"/recordings/cache": map[string]any{
			"get": map[string]any{
				"summary":     "转码缓存状态",
				"description": "返回缓存文件数、占用、上限、当前编码器。",
				"tags":        []string{"录像"},
				"responses":   okResp("缓存统计"),
			},
		},
		"/recordings/cache/clear": map[string]any{
			"post": map[string]any{
				"summary":   "清空转码缓存",
				"tags":      []string{"录像"},
				"responses": okResp("ok + count + freedMB"),
			},
		},
		"/live/cred-status": map[string]any{
			"get": map[string]any{
				"summary":     "设备凭据状态",
				"description": "每台摄像头的 P2P 凭据年龄、估算剩余有效期、刷新冷却时间。",
				"tags":        []string{"播放"},
				"responses":   okResp("凭据状态列表"),
			},
		},
		"/live/renew": map[string]any{
			"post": map[string]any{
				"summary":     "手动续期设备凭据",
				"description": "不传 did 则续期全部。冷却期内会返回错误。",
				"tags":        []string{"播放"},
				"parameters":  []any{strParam("did", "设备 DID（可空）", false)},
				"responses":   okResp("每个设备的续期结果"),
			},
		},
		"/accounts/status": map[string]any{
			"get": map[string]any{
				"summary":     "账号凭据状态",
				"description": "三层凭据说明 + 每个账号的会话状态。",
				"tags":        []string{"账号"},
				"responses":   okResp("账号状态 + explain"),
			},
		},
		"/accounts/export": map[string]any{
			"get": map[string]any{
				"summary":     "导出账号凭据",
				"description": "含 passToken，敏感。用于重装后恢复，无需重新扫码。",
				"tags":        []string{"账号"},
				"responses": map[string]any{
					"200": map[string]any{
						"description": "JSON 文件",
						"content": map[string]any{
							"application/json": map[string]any{"schema": map[string]any{"type": "object"}},
						},
					},
				},
			},
		},
		"/accounts/import": map[string]any{
			"post": map[string]any{
				"summary":     "导入账号凭据",
				"description": "导入前自动把现有文件备份成 accounts.json.bak。",
				"tags":        []string{"账号"},
				"responses":   okResp("ok + count"),
			},
		},
		"/accounts/backups": map[string]any{
			"get": map[string]any{
				"summary":   "凭据备份文件列表",
				"tags":      []string{"账号"},
				"responses": okResp("备份列表"),
			},
		},
		"/recordings/config": map[string]any{
			"get": map[string]any{
				"summary":     "读取录像策略",
				"description": "分片时长、保留天数、磁盘阈值、缓存上限。",
				"tags":        []string{"录像"},
				"responses":   okResp("当前策略"),
			},
			"post": map[string]any{
				"summary":     "修改录像策略",
				"description": "所有字段可选。带范围校验（分片 10-3600 秒，保留 1-365 天）。",
				"tags":        []string{"录像"},
				"responses":   okResp("ok"),
			},
		},
		"/recordings/batch-delete": map[string]any{
			"post": map[string]any{
				"summary": "批量删除录像",
				"description": "三种模式：按日期、按日期之前、指定文件列表。" +
					"删除失败会返回 failed 列表（常见：文件属主不是服务用户）。",
				"tags":      []string{"录像"},
				"responses": okResp("deleted + total + freedMB"),
			},
		},
		"/recordings/export": map[string]any{
			"post": map[string]any{
				"summary": "创建片段导出任务",
				"description": "把指定时间段转码成 MP4（对方可直接播放）。" +
					"异步执行 —— 返回 id，用 status 轮询进度。",
				"tags":      []string{"录像"},
				"responses": okResp("ok + id"),
			},
		},
		"/recordings/export/status": map[string]any{
			"get": map[string]any{
				"summary":     "导出任务状态",
				"description": "status: running / done / failed。",
				"tags":        []string{"录像"},
				"parameters":  []any{strParam("id", "任务 ID（可空 = 全部）", false)},
				"responses":   okResp("任务详情"),
			},
		},
		"/recordings/export/download": map[string]any{
			"get": map[string]any{
				"summary":    "下载导出结果",
				"tags":       []string{"录像"},
				"parameters": []any{strParam("id", "任务 ID", true)},
				"responses":  map[string]any{"200": map[string]any{"description": "MP4 文件"}},
			},
		},
		"/recordings/export/delete": map[string]any{
			"post": map[string]any{
				"summary":   "删除导出任务",
				"tags":      []string{"录像"},
				"responses": okResp("ok"),
			},
		},
		"/recordings/cleanup": map[string]any{
			"post": map[string]any{
				"summary":   "清理过期录像",
				"tags":      []string{"录像"},
				"responses": okResp(`{"deleted":3,"freedMB":120.5}`),
			},
		},
		"/storage": map[string]any{
			"get": map[string]any{
				"summary":     "存储总览",
				"description": "磁盘容量、录像占用、各摄像头明细、预计可录时长。",
				"tags":        []string{"存储"},
				"responses":   okResp("存储总览"),
			},
		},
		"/storage/cleanup-preview": map[string]any{
			"get": map[string]any{
				"summary":    "清理预览",
				"tags":       []string{"存储"},
				"parameters": []any{strParam("keepDays", "保留天数，默认取配置", false)},
				"responses":  okResp("可清理文件列表"),
			},
		},
		"/setup/check": map[string]any{
			"get": map[string]any{"summary": "环境自检", "tags": []string{"系统"}, "responses": okResp("检查项列表")},
		},
		"/setup/repair": map[string]any{
			"post": map[string]any{"summary": "一键修复", "tags": []string{"系统"}, "responses": okResp("修复结果")},
		},
		"/diag/report": map[string]any{
			"get": map[string]any{"summary": "诊断报告（已脱敏）", "tags": []string{"诊断"}, "responses": okResp("诊断 JSON")},
		},
		"/diag/bundle": map[string]any{
			"get": map[string]any{"summary": "诊断包（tar.gz）", "tags": []string{"诊断"}, "responses": map[string]any{"200": map[string]any{"description": "tar.gz 文件"}}},
		},
		"/dev/status": map[string]any{
			"get": map[string]any{"summary": "开发者模式状态", "tags": []string{"开发者"}, "responses": okResp("解锁状态与开关")},
		},
		"/dev/tap": map[string]any{
			"post": map[string]any{"summary": "点击计数（7 次解锁）", "tags": []string{"开发者"}, "responses": okResp("解锁进度")},
		},
		"/dev/config": map[string]any{
			"post": map[string]any{"summary": "修改开放模式开关", "tags": []string{"开发者"}, "responses": okResp("已保存")},
		},
		"/dev/capabilities": map[string]any{
			"get": map[string]any{
				"summary":     "能力探测",
				"description": "客户端据此决定用 HLS 还是 MJPEG、支持哪些功能。",
				"tags":        []string{"开发者"},
				"responses":   okResp("能力清单"),
			},
		},
		"/dev/openapi": map[string]any{
			"get": map[string]any{"summary": "本规格文档（OpenAPI 3.0）", "tags": []string{"开发者"}, "responses": okResp("OpenAPI JSON")},
		},
		"/dev/docs": map[string]any{
			"get": map[string]any{"summary": "Markdown 文档", "tags": []string{"开发者"}, "responses": map[string]any{"200": map[string]any{"description": "Markdown 文本"}}},
		},
	}

	_ = r

	// 补齐其余公开接口。
	//
	// 上面手写的是核心接口（带说明/参数/响应示例）；这里按分组补齐其余对外接口，
	// 保证「参考手册」和真实路由一致。内部调试接口（/api/ui/config、/api/live/inject、
	// /api/dev/tap、/api/ai/* 的内部端点、/api/camera/pose*）不列。
	extra := []struct {
		method, path, summary, tag, resp string
	}{
		{"get", "/events", "事件列表（按日期/设备/类型筛选）", "事件", "事件数组"},
		{"get", "/events/thumb", "事件缩略图（图片）", "事件", "JPEG"},
		{"get", "/events/marks", "事件的标记/标签/收藏", "事件", "标记对象"},
		{"post", "/events/bulk", "批量标记 / 归档 / 删除事件", "事件", "{\"ok\":true}"},
		{"get", "/devices/meta", "设备别名与隐藏状态", "设备", "元数据"},
		{"post", "/devices/meta", "设置设备别名", "设备", "{\"ok\":true}"},
		{"post", "/devices/hide", "隐藏 / 显示设备", "设备", "{\"ok\":true}"},
		{"get", "/home", "默认家庭信息", "设备", "家庭对象"},
		{"get", "/live/thumb", "设备实时缩略图（图片）", "播放", "JPEG"},
		{"get", "/live/stats", "当前播放/推流统计", "播放", "统计对象"},
		{"get", "/ptz/preset", "云台预置位列表", "云台", "预置位数组"},
		{"post", "/ptz/preset", "保存 / 删除云台预置位", "云台", "{\"ok\":true}"},
		{"post", "/ptz/goto", "转到指定预置位", "云台", "{\"ok\":true}"},
		{"post", "/recordings/stop", "停止录像", "录像", "{\"ok\":true}"},
		{"get", "/recordings/dates", "有录像的日期列表", "录像", "日期数组"},
		{"get", "/recordings/list", "某天的录像片段列表", "录像", "片段数组"},
		{"get", "/recordings/play", "获取可播放地址（必要时转码）", "录像", "播放信息"},
		{"get", "/recordings/frame", "录像某一时刻的帧（图片）", "录像", "JPEG"},
		{"get", "/recordings/frame-strip", "录像关键帧连拍条", "录像", "JPEG"},
		{"post", "/recordings/delete", "删除单个录像片段", "录像", "{\"ok\":true}"},
		{"post", "/recordings/batch-delete", "批量删除录像", "录像", "{\"ok\":true}"},
		{"post", "/recordings/cleanup", "按策略清理录像", "录像", "{\"ok\":true}"},
		{"get", "/recordings/cache", "转码缓存占用", "录像", "缓存统计"},
		{"post", "/recordings/cache/clear", "清理转码缓存", "录像", "{\"ok\":true}"},
		{"post", "/recordings/export", "导出片段（异步任务）", "录像", "任务信息"},
		{"get", "/recordings/export/status", "导出任务状态", "录像", "任务状态"},
		{"get", "/recordings/export/list", "导出任务列表", "录像", "任务数组"},
		{"get", "/recordings/export/download", "下载导出结果", "录像", "文件流"},
		{"post", "/recordings/export/delete", "删除导出结果", "录像", "{\"ok\":true}"},
		{"post", "/recordings/export/clear", "清空导出结果", "录像", "{\"ok\":true}"},
		{"get", "/storage/detail", "存储明细（按摄像头/类型）", "存储", "明细对象"},
		{"get", "/storage/locations", "可用的存储位置", "存储", "位置数组"},
		{"post", "/storage/record-dir", "修改录像目录", "存储", "{\"ok\":true}"},
		{"get", "/notify/config", "通知配置", "通知", "配置对象"},
		{"post", "/notify/config", "保存通知配置", "通知", "{\"ok\":true}"},
		{"get", "/notify/events", "可订阅的事件类型", "通知", "事件类型数组"},
		{"post", "/notify/test", "发送测试通知", "通知", "{\"ok\":true}"},
		{"get", "/accounts/detail", "账号页聚合（账号 + 每账号设备 + 活动）", "账号", "聚合对象"},
		{"get", "/dev/tokens", "API Token 列表（不含明文）", "开发者", "Token 数组"},
		{"post", "/dev/tokens", "创建 API Token（明文仅返回一次）", "开发者", "{\"token\":\"mcam_…\"}"},
		{"patch", "/dev/tokens", "启用 / 禁用 / 改名 Token", "开发者", "{\"ok\":true}"},
		{"delete", "/dev/tokens", "删除 API Token", "开发者", "{\"ok\":true}"},
		{"get", "/dev/logs", "最近请求日志（环形缓冲 200 条）", "开发者", "日志数组"},
		{"get", "/dev/stats", "调用统计（近 N 天）", "开发者", "统计对象"},
		{"post", "/dev/webhook/test", "向指定地址发一条示例事件", "开发者", "发送结果"},
		{"post", "/dev/reset-config", "开放模式开关恢复默认", "开发者", "{\"ok\":true}"},
		{"get", "/setup/check", "环境自检（账号/设备/端口/编码器）", "安装", "自检结果"},
		{"post", "/setup/repair", "一键修复自检发现的问题", "安装", "修复结果"},
		{"get", "/ai/status", "本地 AI 运行状态", "AI", "状态对象"},
		{"get", "/ai/usage", "AI 用量与费用", "AI", "用量对象"},
		{"get", "/ai/search", "自然语言搜索事件", "AI", "搜索结果"},
		{"post", "/ai/analyze", "分析指定事件", "AI", "分析结果"},
		{"get", "/ai/plate/search", "按车牌搜索", "AI", "事件数组"},
		{"get", "/miot/live", "官方通道实时 MJPEG 流（OAuth2+PPCS，不受限流影响）", "播放", "MJPEG 流"},
		{"get", "/miot/cameras", "官方通道设备列表与实时局域网 IP", "设备", "设备数组"},
		{"post", "/miot/refresh", "手动刷新官方通道 OAuth2 令牌（有效期 72 小时）", "账号", "{\"ok\":true}"},
	}
	for _, e := range extra {
		item, ok := paths[e.path].(map[string]any)
		if !ok {
			item = map[string]any{}
			paths[e.path] = item
		}
		if _, exists := item[e.method]; exists {
			continue
		}
		item[e.method] = map[string]any{
			"summary":   e.summary,
			"tags":      []string{e.tag},
			"responses": okResp(e.resp),
		}
	}

	return map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":       "MiCam API",
			"version":     appVersion,
			"description": "MiCam 智能监控桥接 API。给第三方客户端（Mac App / Home Assistant / 脚本）接入用。",
		},
		"servers": []any{
			map[string]any{"url": "/app/micam", "description": "飞牛网关（插件内）"},
			map[string]any{"url": "http://<nas-ip>:8900", "description": "局域网直连"},
		},
		"tags": []any{
			map[string]any{"name": "系统"}, map[string]any{"name": "账号"},
			map[string]any{"name": "设备"}, map[string]any{"name": "播放"},
			map[string]any{"name": "云台"}, map[string]any{"name": "录像"},
			map[string]any{"name": "存储"}, map[string]any{"name": "诊断"},
			map[string]any{"name": "开发者"},
		},
		"paths": paths,
	}
}
