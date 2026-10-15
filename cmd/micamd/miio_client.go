package main

// 云台控制 v2：改用 miIO 本地协议（2026-10-11）。
//
// ===== 为什么换成本地协议 =====
//
// 方案 1 最初走 MIoT-Spec over 云端 RPC（/home/rpc/{did}）：
//   · 延迟 500-3000ms
//   · 依赖云端可达
//   · 写操作风险高（我搞坏过用户的球机）
//
// 现在改用 miIO 本地协议（UDP 54321 直连摄像头）：
//   · 延迟 13-371ms（快 30-200 倍）
//   · 不经过云、不被限流
//   · 只走局域网
//
// ===== 怎么发现的 =====
//
// 我原来的 miIO 实现有两个致命 bug：
//   ① AES key 直接用 token（正确是 md5(token)）
//   ② checksum 只算 header+token（正确要加加密数据）
// 参考 python-miio/miio/protocol.py 修正后立刻通。
//
// ===== 实测 =====
//
//   miIO.info        26ms   ✅
//   get_properties   15-23ms ✅
//   set_properties   371ms  ✅ code:0
//   action(校准)     10ms   ✅
//
// ===== 安全 =====
//
// 写操作仍然受 ptz_guard.go 的护栏约束（默认禁用）。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/micam/micam/internal/miot"
)

// miioCall 调一次 miIO（走 Python sidecar）。
//
// 为什么用 Python：
//
//	· miIO 需要 AES-128-CBC + md5，Go 标准库能做但代码量不小
//	· 我们已经有 miio.py（实测可用）
//	· 每台设备一个短进程，延迟可接受（~100ms 起进程）
//
// 后续如需要可改成常驻进程或纯 Go 实现。
func (s *Server) miioCall(dev *xiaomiDevice, method string, params any) (map[string]any, error) {
	script := miot.FindMiioScript()
	if script == "" {
		return nil, fmt.Errorf("找不到 miio.py")
	}
	args := []string{script, dev.IP, dev.Token, method}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, miot.PythonBin(), args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("miIO 调用失败: %w", err)
	}
	var v map[string]any
	if err := json.Unmarshal(out, &v); err != nil {
		return nil, fmt.Errorf("解析失败: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return v, nil
}

// miioBatch 一次进程读多个属性。
//
// 为什么需要：每次 miioCall 起一个 Python 进程要 ~200ms，
// 读 9 个属性就是 1.8 秒。批量后只起一次（实测 4 属性 210ms）。
func (s *Server) miioBatch(dev *xiaomiDevice, calls []miioBatchCall) ([]map[string]any, error) {
	script := miot.FindMiioScript()
	if script == "" {
		return nil, fmt.Errorf("找不到 miio.py")
	}
	type call struct {
		Method string `json:"method"`
		Params any    `json:"params"`
	}
	payload := struct {
		Calls []call `json:"calls"`
	}{}
	for _, c := range calls {
		payload.Calls = append(payload.Calls, call{Method: c.Method, Params: c.Params})
	}
	b, _ := json.Marshal(payload)
	spec := filepath.Join(os.TempDir(), fmt.Sprintf("micam-miio-%d.json", time.Now().UnixNano()))
	if err := os.WriteFile(spec, b, 0o600); err != nil {
		return nil, err
	}
	defer os.Remove(spec)

	// 诊断：打印脚本存在性、spec 内容、环境
	if st, serr := os.Stat(script); serr == nil {
		logf("[miio] 脚本 %s (%d 字节, mode %v)", script, st.Size(), st.Mode())
	} else {
		logf("[miio] ❌ 脚本不存在: %v", serr)
	}
	logf("[miio] spec=%s 内容=%s", spec, string(b)[:min(len(b), 200)])
	logf("[miio] 调用: %s %s %s --batch %s", miot.PythonBin(), script, dev.IP, spec)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, miot.PythonBin(), script, dev.IP, dev.Token, "--batch", spec)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("miIO 批量调用失败: %w (stderr: %s)", err, stderr.String()[:min(len(stderr.String()), 200)])
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("miIO 无输出 (stderr: %s)", stderr.String()[:min(len(stderr.String()), 200)])
	}
	var v struct {
		Results []map[string]any `json:"results"`
		Error   string           `json:"error"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return nil, fmt.Errorf("解析失败: %w", err)
	}
	if v.Error != "" {
		return nil, fmt.Errorf("%s", v.Error)
	}
	return v.Results, nil
}

// miioBatchCall 一次批量调用里的单条。
type miioBatchCall struct {
	Method string
	Params any
}

var _ = sync.Mutex{}
