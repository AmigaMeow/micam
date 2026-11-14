package record

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestManagerKeepDaysZeroPreserved(t *testing.T) {
	mgr := NewManager(Config{Dir: t.TempDir(), KeepDays: 0})
	if mgr.GetConfig().KeepDays != 0 {
		t.Fatalf("KeepDays = %d, want 0", mgr.GetConfig().KeepDays)
	}
	n, freed, err := mgr.Cleanup()
	if err != nil || n != 0 || freed != 0 {
		t.Fatalf("Cleanup with KeepDays=0 returned n=%d freed=%d err=%v, want 0,0,nil", n, freed, err)
	}
}

func TestManagerCleanupRemovesExpiredVideosAndEmptyDirs(t *testing.T) {
	root := t.TempDir()
	oldDir := filepath.Join(root, "cam1", "2020-01-01")
	newDir := filepath.Join(root, "cam1", time.Now().Format("2006-01-02"))
	_ = os.MkdirAll(oldDir, 0o755)
	_ = os.MkdirAll(newDir, 0o755)

	oldMKV := filepath.Join(oldDir, "20200101_000000.mkv")
	oldMP4 := filepath.Join(oldDir, "20200101_000500.mp4")
	newMKV := filepath.Join(newDir, "now.mkv")
	_ = os.WriteFile(oldMKV, make([]byte, 1024), 0o644)
	_ = os.WriteFile(oldMP4, make([]byte, 2048), 0o644)
	_ = os.WriteFile(newMKV, make([]byte, 512), 0o644)

	oldTime := time.Now().AddDate(0, 0, -10)
	_ = os.Chtimes(oldMKV, oldTime, oldTime)
	_ = os.Chtimes(oldMP4, oldTime, oldTime)

	mgr := NewManager(Config{Dir: root, KeepDays: 7})
	n, freed, err := mgr.Cleanup()
	if err != nil {
		t.Fatalf("Cleanup failed: %v", err)
	}
	if n != 2 || freed != 3072 {
		t.Fatalf("Cleanup got n=%d freed=%d, want 2, 3072", n, freed)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Fatalf("expected empty expired dir to be removed")
	}
	if _, err := os.Stat(newMKV); err != nil {
		t.Fatalf("new file should still exist: %v", err)
	}
}

func TestDiskUsageOfReadOnlyDoesNotCreateDir(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "never-created", "subdir")
	du := DiskUsageOfReadOnly(missing)
	if du.Err != "" || du.TotalGB <= 0 {
		t.Fatalf("unexpected DiskUsageOfReadOnly result: %+v", du)
	}
	if _, err := os.Stat(filepath.Join(root, "never-created")); !os.IsNotExist(err) {
		t.Fatalf("DiskUsageOfReadOnly created missing directory")
	}
}
