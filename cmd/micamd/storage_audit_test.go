package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/micam/micam/internal/config"
	"github.com/micam/micam/internal/event"
	"github.com/micam/micam/internal/record"
)

func newTestServer(t *testing.T, roots ...string) (*Server, string) {
	t.Helper()
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.json")
	cfg := config.Load(cfgPath)
	if len(roots) == 0 {
		recDir := filepath.Join(tmpDir, "recordings")
		_ = os.MkdirAll(recDir, 0o755)
		roots = []string{recDir}
	}
	_ = cfg.SetRecordDir(roots[0])
	rec := record.NewManager(record.Config{
		Enabled:   cfg.RecordEnabled,
		Dir:       roots[0],
		Segment:   time.Duration(cfg.RecordSegment) * time.Second,
		KeepDays:  cfg.RecordKeepDays,
		MinFreeGB: cfg.RecordMinFreeGB,
	})
	es := event.NewStore(filepath.Join(tmpDir, "events"), 30)
	s := &Server{
		cfg:          cfg,
		dataDir:      tmpDir,
		rec:          rec,
		eventStore:   es,
		marks:        newMarkStore(tmpDir),
		testRecRoots: roots,
	}
	return s, tmpDir
}

func makeRecFile(t *testing.T, root, dev, date, name string, size int, age time.Duration) string {
	t.Helper()
	p := filepath.Join(root, dev, date, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, bytes.Repeat([]byte("a"), size), 0o644); err != nil {
		t.Fatal(err)
	}
	mt := time.Now().Add(-age)
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFileProtectionCheckerUnlimitedEvents(t *testing.T) {
	s, _ := newTestServer(t)
	now := time.Now()
	const total = 1250
	for i := 0; i < total; i++ {
		ts := now.Add(-time.Duration(i) * time.Minute)
		recName := fmt.Sprintf("%s_0.mkv", ts.Format("20060102_150405"))
		s.eventStore.Add(event.Event{
			ID:      fmt.Sprintf("ev-%04d", i),
			Time:    ts.Format("2006-01-02 15:04:05"),
			Type:    "person",
			RecFile: recName,
		})
	}
	checker := newFileProtectionChecker(s)
	first := fmt.Sprintf("%s_0.mkv", now.Format("20060102_150405"))
	oldest := fmt.Sprintf("%s_0.mkv", now.Add(-time.Duration(total-1)*time.Minute).Format("20060102_150405"))
	if !checker.isProtected(filepath.Join(s.recRoot(), first)) {
		t.Fatalf("expected newest event recording %s to be protected", first)
	}
	if !checker.isProtected(filepath.Join(s.recRoot(), oldest)) {
		t.Fatalf("expected event #1250 recording %s to be protected (old Limit:1000 cap must be gone)", oldest)
	}
}

func TestCleanupPreviewMatchesExecution(t *testing.T) {
	s, tmpDir := newTestServer(t)
	root := s.recRoot()

	oldNormal := makeRecFile(t, root, "camA", "2026-10-01", "20261001_010000_0.mkv", 2048, 10*24*time.Hour)
	oldAI := makeRecFile(t, root, "camA", "2026-10-01", "20261001_020000_0.mkv", 2048, 10*24*time.Hour)
	oldMP4 := makeRecFile(t, root, "camA", "2026-10-02", "20261002_030000_0.mp4", 1024, 9*24*time.Hour)
	recent := makeRecFile(t, root, "camA", "2026-11-12", "20261112_040000_0.mkv", 1024, 24*time.Hour)

	s.eventStore.Add(event.Event{
		ID:      "ev-protect",
		Time:    time.Now().Add(-10 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
		Type:    "person",
		RecFile: filepath.Base(oldAI),
	})

	thumbOld := filepath.Join(tmpDir, "thumbs", "camA", filepath.Base(oldNormal)+".jpg")
	cacheOld := filepath.Join(tmpDir, "transcode_cache", "camA", "2026-10-01", filepath.Base(oldNormal)+".h264.mp4")
	_ = os.MkdirAll(filepath.Dir(thumbOld), 0o755)
	_ = os.MkdirAll(filepath.Dir(cacheOld), 0o755)
	_ = os.WriteFile(thumbOld, []byte("thumb"), 0o644)
	_ = os.WriteFile(cacheOld, []byte("cache"), 0o644)

	// 1. Preview
	req := httptest.NewRequest(http.MethodGet, "/api/storage/cleanup-preview?days=7&protectAI=1&cleanCache=1&allPools=1", nil)
	rec := httptest.NewRecorder()
	s.handleCleanupPreview(rec, req)
	if rec.Code != 200 {
		t.Fatalf("preview status=%d body=%s", rec.Code, rec.Body.String())
	}
	var prev struct {
		FileCount        int `json:"fileCount"`
		ProtectedAICount int `json:"protectedAICount"`
		CacheFiles       int `json:"cacheFiles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &prev); err != nil {
		t.Fatal(err)
	}
	if prev.FileCount != 2 || prev.ProtectedAICount != 1 || prev.CacheFiles != 1 {
		t.Fatalf("unexpected preview: %+v", prev)
	}

	// 2. Execute cleanup with identical params
	body := bytes.NewBufferString(`{"days":"7","protectAI":true,"cleanCache":true,"allPools":true}`)
	req2 := httptest.NewRequest(http.MethodPost, "/api/recordings/cleanup", body)
	rec2 := httptest.NewRecorder()
	s.handleRecordCleanup(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("cleanup status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	var res struct {
		Deleted          int `json:"deleted"`
		ProtectedAICount int `json:"protectedAICount"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Deleted != prev.FileCount || res.ProtectedAICount != prev.ProtectedAICount {
		t.Fatalf("preview/cleanup mismatch: preview=%+v cleanup=%+v", prev, res)
	}
	if _, err := os.Stat(oldNormal); !os.IsNotExist(err) {
		t.Fatalf("expected oldNormal removed")
	}
	if _, err := os.Stat(oldMP4); !os.IsNotExist(err) {
		t.Fatalf("expected oldMP4 removed")
	}
	if _, err := os.Stat(thumbOld); !os.IsNotExist(err) {
		t.Fatalf("expected cascaded thumb removed")
	}
	if _, err := os.Stat(cacheOld); !os.IsNotExist(err) {
		t.Fatalf("expected cache removed")
	}
	if _, err := os.Stat(oldAI); err != nil {
		t.Fatalf("expected AI protected file retained: %v", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatalf("expected recent file retained: %v", err)
	}
}

func TestStoragePolicyNoneNormalizedAndCheckStorageMultiPool(t *testing.T) {
	tmp := t.TempDir()
	pool1 := filepath.Join(tmp, "pool1")
	pool2 := filepath.Join(tmp, "pool2")
	_ = os.MkdirAll(pool1, 0o755)
	_ = os.MkdirAll(pool2, 0o755)

	s, _ := newTestServer(t, pool1, pool2)
	old1 := makeRecFile(t, pool1, "cam1", "2026-10-01", "20261001_000000_0.mkv", 1024, 15*24*time.Hour)
	old2 := makeRecFile(t, pool2, "cam1", "2026-10-02", "20261002_000000_0.mkv", 1024, 15*24*time.Hour)
	keep := makeRecFile(t, pool2, "cam1", "2026-11-13", "20261113_000000_0.mkv", 1024, 24*time.Hour)

	// POST "none" -> normalized to "neverDelete"
	body := bytes.NewBufferString(`{"keepDays":7,"minFreeGB":1,"maxStorageGB":100,"cacheGB":8,"autoClean":true,"lowSpaceMode":"none"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/storage/policy", body)
	rec := httptest.NewRecorder()
	s.handleStoragePolicy(rec, req)
	if rec.Code != 200 {
		t.Fatalf("policy save failed: %d %s", rec.Code, rec.Body.String())
	}
	if s.cfg.RecordLowSpaceMode != "neverDelete" {
		t.Fatalf("expected lowSpaceMode normalized to neverDelete, got %q", s.cfg.RecordLowSpaceMode)
	}
	if s.cfg.TranscodeCacheGB != 8 || s.cfg.RecordMaxStorageGB != 100 {
		t.Fatalf("expected cacheGB=8 maxStorageGB=100, got cache=%d max=%d", s.cfg.TranscodeCacheGB, s.cfg.RecordMaxStorageGB)
	}

	s.checkStorageOnce()
	if _, err := os.Stat(old1); err != nil {
		t.Fatalf("neverDelete should preserve old1: %v", err)
	}
	if _, err := os.Stat(old2); err != nil {
		t.Fatalf("neverDelete should preserve old2: %v", err)
	}

	// Switch to deleteOldest -> both pools' expired files (>7d) deleted cleanly
	mode := "deleteOldest"
	zero := 0
	if err := s.cfg.SetRecordPolicy(nil, nil, nil, nil, &zero, nil, nil, nil, nil, &mode, nil); err != nil {
		t.Fatal(err)
	}
	s.checkStorageOnce()
	if _, err := os.Stat(old1); !os.IsNotExist(err) {
		t.Fatalf("expected old1 deleted after switching to deleteOldest")
	}
	if _, err := os.Stat(old2); !os.IsNotExist(err) {
		t.Fatalf("expected old2 in pool2 deleted too")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("expected recent file kept: %v", err)
	}

	// Verify multi-pool quota trimming
	extra := makeRecFile(t, pool1, "cam1", "2026-11-12", "20261112_000000_0.mkv", 2048, 48*time.Hour)
	deleted, freed, err := s.cleanupPoolsToTargetSize([]string{pool1, pool2}, 1500, false)
	if err != nil || deleted != 1 || freed != 2048 {
		t.Fatalf("multi-pool quota cleanup failed: deleted=%d freed=%d err=%v", deleted, freed, err)
	}
	if _, err := os.Stat(extra); !os.IsNotExist(err) {
		t.Fatalf("expected older extra file removed for quota")
	}
}

func TestStopAllRecordingsStopsMiotJobsAndLowSpacePause(t *testing.T) {
	s, _ := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	miotRecJobsMu.Lock()
	miotRecJobs["mock-miot-did"] = &miotRecJob{did: "mock-miot-did", cancel: cancel}
	miotRecJobsMu.Unlock()

	s.stopAllRecordings()
	if ctx.Err() == nil {
		t.Fatalf("expected mock miotRecJob context to be cancelled by stopAllRecordings")
	}
	miotRecJobsMu.Lock()
	_, stillPresent := miotRecJobs["mock-miot-did"]
	miotRecJobsMu.Unlock()
	if stillPresent {
		t.Fatalf("expected mock-miot-did removed from miotRecJobs")
	}

	mode := "stopRecording"
	hugeMinFree := 9999999
	_ = s.cfg.SetRecordPolicy(nil, nil, nil, nil, nil, &hugeMinFree, nil, nil, nil, &mode, nil)
	s.setPausedForLowSpace(true)
	if err := s.checkLowSpacePauseBeforeStart(); err == nil {
		t.Fatalf("expected checkLowSpacePauseBeforeStart to block when low space pause active")
	}
	mode2 := "protectAI"
	_ = s.cfg.SetRecordPolicy(nil, nil, nil, nil, nil, nil, nil, nil, nil, &mode2, nil)
	if err := s.checkLowSpacePauseBeforeStart(); err != nil {
		t.Fatalf("expected switching away from stopRecording to clear pause, got %v", err)
	}
}

func TestMaintainAppDataNow(t *testing.T) {
	metaMu.Lock()
	origMetaFile := metaFile
	origMetaCache := metaCache
	metaCache = map[string]recMeta{}
	metaMu.Unlock()
	defer func() {
		metaMu.Lock()
		metaFile = origMetaFile
		metaCache = origMetaCache
		metaMu.Unlock()
	}()

	s, tmpDir := newTestServer(t)
	metaMu.Lock()
	metaFile = s.metaFilePath()
	metaMu.Unlock()
	root := s.recRoot()

	aliveRec := makeRecFile(t, root, "cam1", "2026-11-12", "alive.mkv", 512, 24*time.Hour)
	aliveThumb := filepath.Join(tmpDir, "thumbs", "cam1", "alive.mkv.jpg")
	orphanThumb := filepath.Join(tmpDir, "thumbs", "cam1", "gone.mkv.jpg")
	_ = os.MkdirAll(filepath.Dir(aliveThumb), 0o755)
	_ = os.WriteFile(aliveThumb, []byte("ok"), 0o644)
	_ = os.WriteFile(orphanThumb, []byte("orphan"), 0o644)
	oldTime := time.Now().Add(-time.Hour)
	_ = os.Chtimes(orphanThumb, oldTime, oldTime)

	qrOld := filepath.Join(tmpDir, "qr_old.png")
	qrNew := filepath.Join(tmpDir, "qr_new.png")
	_ = os.WriteFile(qrOld, []byte("qr"), 0o600)
	_ = os.WriteFile(qrNew, []byte("qr"), 0o600)
	_ = os.Chtimes(qrOld, oldTime, oldTime)

	frameOld := filepath.Join(tmpDir, "frames", "cam1", "f1.jpg")
	aiOld := filepath.Join(tmpDir, "ai-cache", "old.json")
	wtestOld := filepath.Join(root, ".micam-wtest-leftover")
	_ = os.MkdirAll(filepath.Dir(frameOld), 0o755)
	_ = os.MkdirAll(filepath.Dir(aiOld), 0o755)
	_ = os.WriteFile(frameOld, []byte("f"), 0o644)
	_ = os.WriteFile(aiOld, []byte("{}"), 0o644)
	_ = os.WriteFile(wtestOld, []byte{}, 0o600)
	old10d := time.Now().Add(-10 * 24 * time.Hour)
	_ = os.Chtimes(frameOld, old10d, old10d)
	_ = os.Chtimes(aiOld, old10d, old10d)
	_ = os.Chtimes(wtestOld, oldTime, oldTime)

	evThumbs := filepath.Join(tmpDir, "events", "thumbs")
	_ = os.MkdirAll(evThumbs, 0o755)
	evOrphan := filepath.Join(evThumbs, "ev-orphan.jpg")
	evMarked := filepath.Join(evThumbs, "ev-marked.jpg")
	_ = os.WriteFile(evOrphan, []byte("x"), 0o644)
	_ = os.WriteFile(evMarked, []byte("y"), 0o644)
	_ = os.Chtimes(evOrphan, oldTime, oldTime)
	_ = os.Chtimes(evMarked, oldTime, oldTime)
	s.eventStore.Add(event.Event{ID: "ev-marked", Time: time.Now().Format("2006-01-02 15:04:05"), Type: "person", Thumb: evMarked})
	_ = s.marks.set("ev-marked", eventMark{Bookmark: true})

	metaMu.Lock()
	metaCache[aliveRec] = recMeta{Duration: 300}
	metaCache[filepath.Join(root, "cam1", "2026-10-01", "missing.mkv")] = recMeta{Duration: 300}
	metaMu.Unlock()

	s.maintainAppDataNow()

	for _, shouldBeGone := range []string{qrOld, frameOld, aiOld, orphanThumb, evOrphan, wtestOld} {
		if _, err := os.Stat(shouldBeGone); !os.IsNotExist(err) {
			t.Fatalf("expected %s to be cleaned", shouldBeGone)
		}
	}
	for _, shouldStay := range []string{qrNew, aliveThumb, evMarked} {
		if _, err := os.Stat(shouldStay); err != nil {
			t.Fatalf("expected %s to be preserved: %v", shouldStay, err)
		}
	}
	metaMu.Lock()
	_, aliveOK := metaCache[aliveRec]
	_, missingOK := metaCache[filepath.Join(root, "cam1", "2026-10-01", "missing.mkv")]
	metaMu.Unlock()
	if !aliveOK || missingOK {
		t.Fatalf("metaCache pruning mismatch: aliveOK=%v missingOK=%v", aliveOK, missingOK)
	}
}

func TestMigrateModTimeAndNonDestructiveLocation(t *testing.T) {
	tmp := t.TempDir()
	missingCand := filepath.Join(tmp, "share1", "recordings")
	if !isLocationWritable(missingCand) {
		t.Fatalf("expected missing candidate inside writable tmp to pass")
	}
	if _, err := os.Stat(filepath.Join(tmp, "share1")); !os.IsNotExist(err) {
		t.Fatalf("isLocationWritable must NOT create non-existent parent dirs")
	}

	srcDir := filepath.Join(tmp, "srcPool")
	dstDir := filepath.Join(tmp, "dstPool")
	_ = os.MkdirAll(srcDir, 0o755)
	_ = os.MkdirAll(dstDir, 0o755)

	s, _ := newTestServer(t, srcDir)
	f1 := makeRecFile(t, srcDir, "cam1", "2026-10-01", "20261001_000000_0.mp4", 4096, 12*24*time.Hour)
	active := makeRecFile(t, srcDir, "cam1", "2026-11-14", "20261114_000000_0.mkv", 2048, 2*time.Second)

	files, totalB, err := s.migrateCandidates(srcDir, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != f1 || totalB != 4096 {
		t.Fatalf("expected only inactive f1 (4096B), got files=%v totalB=%d (active=%s)", files, totalB, active)
	}

	copied := filepath.Join(dstDir, "cam1", "2026-10-01", "20261001_000000_0.mp4")
	_ = os.MkdirAll(filepath.Dir(copied), 0o755)
	if err := copyFile(f1, copied); err != nil {
		t.Fatal(err)
	}
	si, _ := os.Stat(f1)
	di, _ := os.Stat(copied)
	if si.ModTime().Unix() != di.ModTime().Unix() {
		t.Fatalf("copyFile must preserve ModTime: src=%v dst=%v", si.ModTime(), di.ModTime())
	}
}

func TestLogRotationCap(t *testing.T) {
	logFileMu.Lock()
	origFile, origPath, origSize, origSame := logFile, logFilePath, logCurSize, logSameAsStderr
	logFile, logFilePath, logCurSize, logSameAsStderr = nil, "", 0, true
	logFileMu.Unlock()
	defer func() {
		logFileMu.Lock()
		if logFile != nil {
			_ = logFile.Close()
		}
		logFile, logFilePath, logCurSize, logSameAsStderr = origFile, origPath, origSize, origSame
		logFileMu.Unlock()
	}()

	tmp := t.TempDir()
	initLogFile(tmp)
	logFileMu.Lock()
	logCurSize = maxLogFileSize - 5
	logSameAsStderr = true
	logFileMu.Unlock()

	writeLogLine("[test] trigger log rotation line")

	if _, err := os.Stat(filepath.Join(tmp, "micamd.log.1")); err != nil {
		t.Fatalf("expected rotated backup micamd.log.1: %v", err)
	}
	fi, err := os.Stat(filepath.Join(tmp, "micamd.log"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() >= maxLogFileSize {
		t.Fatalf("expected active log size reset below maxLogFileSize, got %d", fi.Size())
	}
}
