package main

import (
	"context"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/micam/micam/internal/config"
	"github.com/micam/micam/internal/record"
)

// TestRecordPolicyConfig 验证录像策略配置（保留天数0为永久保留、存储上限、模式切换）
func TestRecordPolicyConfig(t *testing.T) {
	tmpDir, err := ioutil.TempDir("", "micam_cfg_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	cfgPath := filepath.Join(tmpDir, "config.json")
	cfg := config.Load(cfgPath)

	// 1. 测试设置「永久保留（0天）」与「存储上限 200GB」与「动态侦测模式」
	enabled := true
	mode := "motion"
	seg := 300
	keepZero := 0 // 永久保留
	maxGB := 200
	minFree := 15
	postRoll := 45
	cache := 8
	autoClean := true
	lowSpace := "protectAI"
	sens := "high"

	err = cfg.SetRecordPolicy(&enabled, &mode, &seg, &keepZero, &maxGB, &minFree, &postRoll, &cache, &autoClean, &lowSpace, &sens)
	if err != nil {
		t.Fatalf("SetRecordPolicy failed: %v", err)
	}

	if cfg.RecordKeepDays != 0 {
		t.Errorf("Expected RecordKeepDays 0 (forever), got %d", cfg.RecordKeepDays)
	}
	if cfg.RecordMaxStorageGB != 200 {
		t.Errorf("Expected RecordMaxStorageGB 200, got %d", cfg.RecordMaxStorageGB)
	}
	if cfg.RecordMode != "motion" {
		t.Errorf("Expected RecordMode 'motion', got %s", cfg.RecordMode)
	}
	if cfg.RecordPostRollSec != 45 {
		t.Errorf("Expected RecordPostRollSec 45, got %d", cfg.RecordPostRollSec)
	}
	if cfg.RecordLowSpaceMode != "protectAI" {
		t.Errorf("Expected RecordLowSpaceMode 'protectAI', got %s", cfg.RecordLowSpaceMode)
	}

	// 2. 重新加载，验证落盘持久化
	reloaded := config.Load(cfgPath)
	if reloaded.RecordKeepDays != 0 {
		t.Errorf("Reloaded RecordKeepDays expected 0, got %d", reloaded.RecordKeepDays)
	}
	if reloaded.RecordMaxStorageGB != 200 {
		t.Errorf("Reloaded RecordMaxStorageGB expected 200, got %d", reloaded.RecordMaxStorageGB)
	}
	if reloaded.RecordMode != "motion" {
		t.Errorf("Reloaded RecordMode expected 'motion', got %s", reloaded.RecordMode)
	}
}

// TestCleanupToTargetSize 验证配额超限清理（按时间倒序删除最旧文件，达标即止）
func TestCleanupToTargetSize(t *testing.T) {
	tmpDir, err := ioutil.TempDir("", "micam_clean_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// 创建 3 个假视频文件，大小各 1000 字节，时间由旧到新
	f1 := filepath.Join(tmpDir, "20261010_010000.mkv")
	f2 := filepath.Join(tmpDir, "20261011_010000.mkv")
	f3 := filepath.Join(tmpDir, "20261012_010000.mkv")

	buf := make([]byte, 1000)
	_ = os.WriteFile(f1, buf, 0644)
	_ = os.WriteFile(f2, buf, 0644)
	_ = os.WriteFile(f3, buf, 0644)

	t1 := time.Now().Add(-48 * time.Hour)
	t2 := time.Now().Add(-24 * time.Hour)
	t3 := time.Now()

	_ = os.Chtimes(f1, t1, t1)
	_ = os.Chtimes(f2, t2, t2)
	_ = os.Chtimes(f3, t3, t3)

	// 目标保留大小：2000 字节（应删掉最旧的 f1，释放 1000 字节，剩余 3000-1000 = 2000 <= 2000）
	srv := &Server{}
	deleted, freed, err := cleanupToTargetSize(tmpDir, 2000, false, srv)
	if err != nil {
		t.Fatalf("cleanupToTargetSize failed: %v", err)
	}

	if deleted != 1 {
		t.Errorf("Expected 1 file deleted, got %d", deleted)
	}
	if freed != 1000 {
		t.Errorf("Expected 1000 bytes freed, got %d", freed)
	}

	// 检查 f1 被删，f2 和 f3 仍在
	if _, err := os.Stat(f1); !os.IsNotExist(err) {
		t.Errorf("f1 should have been deleted")
	}
	if _, err := os.Stat(f2); err != nil {
		t.Errorf("f2 should still exist")
	}
	if _, err := os.Stat(f3); err != nil {
		t.Errorf("f3 should still exist")
	}
}

// TestMotionRecTimeout 验证动体录像超时停止逻辑
func TestMotionRecTimeout(t *testing.T) {
	tracker := &motionRecManager{
		activeUntil: make(map[string]time.Time),
	}

	now := time.Now()
	// 设备 1：超时了（应被剔除）
	tracker.activeUntil["cam1"] = now.Add(-10 * time.Second)
	// 设备 2：还在延录期内（应保留）
	tracker.activeUntil["cam2"] = now.Add(20 * time.Second)

	tracker.mu.Lock()
	var toStop []string
	for did, until := range tracker.activeUntil {
		if now.After(until) {
			toStop = append(toStop, did)
			delete(tracker.activeUntil, did)
		}
	}
	tracker.mu.Unlock()

	if len(toStop) != 1 || toStop[0] != "cam1" {
		t.Errorf("Expected cam1 to stop, got %v", toStop)
	}
	if _, ok := tracker.activeUntil["cam2"]; !ok {
		t.Errorf("Expected cam2 to remain active")
	}
}

var _ = context.Background
var _ = record.DirUsage
