package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/micam/micam/internal/event"
)

func TestTieredStorage(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "micam_tier_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	s := &Server{
		marks:      newMarkStore(tmpDir),
		eventStore: event.NewStore(filepath.Join(tmpDir, "events"), 90),
	}

	// 1. L1 级：用户书签/关注锁定（文件维度直接锁定）
	bookmarkedFile := "20261017_100000_0.mkv"
	_ = s.marks.set(bookmarkedFile, eventMark{Bookmark: true})
	if !isFileProtected(filepath.Join(tmpDir, bookmarkedFile), s) {
		t.Errorf("Expected L1 bookmarked file to be protected")
	}

	// 2. L1 级：关联事件被用户标记锁定
	evL1File := "20261017_110000_0.mkv"
	evL1 := event.Event{
		ID:      "ev-l1-001",
		Time:    "2026-10-17 11:05:00",
		Type:    "motion",
		RecFile: evL1File,
	}
	s.eventStore.Add(evL1)
	_ = s.marks.set(evL1.ID, eventMark{Watch: true})
	if !isFileProtected(filepath.Join(tmpDir, evL1File), s) {
		t.Errorf("Expected L1 event-associated file to be protected")
	}

	// 3. L2 级：AI 识别高价值目标（人/车/包裹），自动免清理保护
	evL2PersonFile := "20261017_120000_0.mkv"
	evL2Person := event.Event{
		ID:      "ev-l2-person",
		Time:    "2026-10-17 12:05:00",
		Type:    "person",
		RecFile: evL2PersonFile,
	}
	s.eventStore.Add(evL2Person)
	if !isFileProtected(filepath.Join(tmpDir, evL2PersonFile), s) {
		t.Errorf("Expected L2 person event file to be protected")
	}

	evL2VehicleFile := "20261017_130000_0.mkv"
	evL2Vehicle := event.Event{
		ID:      "ev-l2-vehicle",
		Time:    "2026-10-17 13:05:00",
		Type:    "vehicle",
		RecFile: evL2VehicleFile,
	}
	s.eventStore.Add(evL2Vehicle)
	if !isFileProtected(filepath.Join(tmpDir, evL2VehicleFile), s) {
		t.Errorf("Expected L2 vehicle event file to be protected")
	}

	// 4. L3 级：普通画面变动/无标记无AI目标，不受保护（可正常按策略轮替清理）
	evL3MotionFile := "20261017_140000_0.mkv"
	evL3Motion := event.Event{
		ID:      "ev-l3-motion",
		Time:    "2026-10-17 14:05:00",
		Type:    "motion",
		RecFile: evL3MotionFile,
	}
	s.eventStore.Add(evL3Motion)
	if isFileProtected(filepath.Join(tmpDir, evL3MotionFile), s) {
		t.Errorf("Expected L3 normal motion file NOT to be protected")
	}

	// 5. L3 级：完全无事件关联的普通无意义录像，不受保护
	normalFile := "20261017_150000_0.mkv"
	if isFileProtected(filepath.Join(tmpDir, normalFile), s) {
		t.Errorf("Expected unassociated file NOT to be protected")
	}
}
