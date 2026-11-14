package event

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreCleanupWithRetentionRemovesExpiredLogsAndOrphanThumbs(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir, 30)

	// Write today's referenced event & thumb
	keepThumb := "keep.jpg"
	_ = os.WriteFile(filepath.Join(dir, "thumbs", keepThumb), []byte("ok"), 0o644)
	dayToday := time.Now().Format("2006-01-02")
	st.Add(Event{ID: "ev-today", Time: dayToday + " 12:00:00", Type: "person", Thumb: keepThumb})

	// Write expired log + referenced thumb + orphan thumb
	oldDay := time.Now().AddDate(0, 0, -40).Format("2006-01-02")
	_ = os.WriteFile(filepath.Join(dir, oldDay+".jsonl"), []byte(`{"id":"ev-old","time":"`+oldDay+` 10:00:00","type":"motion","thumb":"old.jpg"}`+"\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, oldDay+".jsonl.bak"), []byte("bak"), 0o644)
	oldMod := time.Now().AddDate(0, 0, -40)
	_ = os.Chtimes(filepath.Join(dir, oldDay+".jsonl.bak"), oldMod, oldMod)

	oldThumbPath := filepath.Join(dir, "thumbs", "old.jpg")
	orphanThumbPath := filepath.Join(dir, "thumbs", "orphan.jpg")
	_ = os.WriteFile(oldThumbPath, make([]byte, 128), 0o644)
	_ = os.WriteFile(orphanThumbPath, make([]byte, 256), 0o644)
	_ = os.Chtimes(oldThumbPath, oldMod, oldMod)
	_ = os.Chtimes(orphanThumbPath, oldMod, oldMod)

	// Recent orphan within grace window should be kept
	recentOrphan := filepath.Join(dir, "thumbs", "recent_orphan.jpg")
	_ = os.WriteFile(recentOrphan, make([]byte, 64), 0o644)

	logsRemoved, thumbsRemoved, _, err := st.CleanupWithRetention(30, nil)
	if err != nil {
		t.Fatalf("CleanupWithRetention failed: %v", err)
	}
	if logsRemoved != 2 {
		t.Fatalf("logsRemoved = %d, want 2", logsRemoved)
	}
	if thumbsRemoved != 2 {
		t.Fatalf("thumbsRemoved = %d, want 2", thumbsRemoved)
	}
	if _, err := os.Stat(filepath.Join(dir, "thumbs", keepThumb)); err != nil {
		t.Fatalf("referenced thumb removed unexpectedly: %v", err)
	}
	if _, err := os.Stat(recentOrphan); err != nil {
		t.Fatalf("recent grace thumb removed unexpectedly: %v", err)
	}
}

func TestStoreQueryUnlimited(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir, 30)
	base := time.Date(time.Now().Year(), time.Now().Month(), time.Now().Day(), 0, 0, 0, 0, time.Local)
	for i := 0; i < 120; i++ {
		ts := base.Add(time.Duration(i) * time.Second).Format("2006-01-02 15:04:05")
		st.Add(Event{ID: ts, Time: ts, Type: TypeMotion})
	}
	if got := len(st.Query(Query{Days: 1, Limit: 0})); got != 100 {
		t.Fatalf("default Query limit = %d, want 100", got)
	}
	if got := len(st.Query(Query{Days: 1, Limit: -1})); got != 120 {
		t.Fatalf("unlimited Query got %d, want 120", got)
	}
}
