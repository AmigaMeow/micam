package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func encodeTestJPEG(t *testing.T, w, h int, luma uint8) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	c := color.RGBA{R: luma, G: luma, B: luma, A: 255}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("jpeg.Encode failed: %v", err)
	}
	return buf.Bytes()
}

func encodeTestGrayJPEG(t *testing.T, w, h int, luma uint8) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, w, h))
	c := color.Gray{Y: luma}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetGray(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("jpeg.Encode gray failed: %v", err)
	}
	return buf.Bytes()
}

func TestDownscaleGrayPureGoAndMotionDiff(t *testing.T) {
	s, _ := newTestServer(t)

	if got := s.downscaleGray(nil, 160, 90); got != nil {
		t.Fatalf("expected nil on empty input")
	}
	if got := s.downscaleGray([]byte("not-a-jpeg"), 160, 90); got != nil {
		t.Fatalf("expected nil on invalid jpeg")
	}
	if got := s.downscaleGray(encodeTestJPEG(t, 64, 36, 128), 0, 90); got != nil {
		t.Fatalf("expected nil on zero width")
	}

	dark := encodeTestJPEG(t, 320, 180, 20)
	bright := encodeTestJPEG(t, 320, 180, 200)
	grayImg := encodeTestGrayJPEG(t, 320, 180, 110)

	g1 := s.downscaleGray(dark, 160, 90)
	g2 := s.downscaleGray(bright, 160, 90)
	g3 := s.downscaleGray(grayImg, 160, 90)
	if len(g1) != 160*90 || len(g2) != 160*90 || len(g3) != 160*90 {
		t.Fatalf("unexpected gray buffer sizes: %d, %d, %d", len(g1), len(g2), len(g3))
	}

	sameDiff := meanAbsDiff(g1, s.downscaleGray(dark, 160, 90))
	if sameDiff != 0 {
		t.Fatalf("expected identical frame diff=0, got %v", sameDiff)
	}
	contrastDiff := meanAbsDiff(g1, g2)
	if contrastDiff < 150 {
		t.Fatalf("expected high diff for dark vs bright frame, got %v", contrastDiff)
	}
}

func TestRecScannedPersistenceAndActiveSkip(t *testing.T) {
	origPace := recScanPaceDelay
	recScanPaceDelay = 0
	defer func() { recScanPaceDelay = origPace }()

	s, _ := newTestServer(t)
	today := time.Now().Format("2006-01-02")

	// 预置持久化索引：1 条超过 4 天的过期记录 + 1 条有效记录
	expiredKey := filepath.Join("camA", "2020-01-01", "old.mkv")
	validKey := filepath.Join("camA", today, "pre_scanned.mkv")
	if err := saveRecScannedSnapshot(s.recScannedFilePath(), map[string]int64{
		expiredKey: time.Now().Add(-5 * 24 * time.Hour).Unix(),
		validKey:   time.Now().Add(-1 * time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("saveRecScannedSnapshot: %v", err)
	}

	// 磁盘 3 个分片：已缓存、已完成无事件、仍在写入（<25s）
	makeRecFile(t, s.recRoot(), "camA", today, "pre_scanned.mkv", 200*1024, time.Hour)
	finishedPath := makeRecFile(t, s.recRoot(), "camA", today, "finished_zero.mkv", 200*1024, time.Hour)
	activePath := makeRecFile(t, s.recRoot(), "camA", today, "active_writing.mkv", 200*1024, 2*time.Second)

	// 重置内存缓存模拟重启
	recScanMu.Lock()
	recScannedLoadedPath = ""
	recScannedMap = make(map[string]int64)
	recScanMu.Unlock()

	scanned, _, err := s.scanAllRecordings(25)
	if err != nil {
		t.Fatalf("scanAllRecordings: %v", err)
	}
	if scanned != 1 {
		t.Fatalf("expected only finished_zero.mkv scanned (count=1), got %d", scanned)
	}

	recScanMu.Lock()
	_, hasExpired := recScannedMap[expiredKey]
	_, hasValid := recScannedMap[validKey]
	_, hasFinished := recScannedMap[filepath.Join("camA", today, filepath.Base(finishedPath))]
	_, hasActive := recScannedMap[filepath.Join("camA", today, filepath.Base(activePath))]
	recScanMu.Unlock()

	if hasExpired {
		t.Fatalf("expected expired key %q pruned", expiredKey)
	}
	if !hasValid || !hasFinished {
		t.Fatalf("expected valid (%v) and finished (%v) keys persisted", hasValid, hasFinished)
	}
	if hasActive {
		t.Fatalf("active file %q must not be marked scanned", activePath)
	}

	// 再次模拟重启：从磁盘加载后应完全跳过 finished_zero.mkv
	recScanMu.Lock()
	recScannedLoadedPath = ""
	recScannedMap = make(map[string]int64)
	recScanMu.Unlock()

	scannedAgain, _, err := s.scanAllRecordings(25)
	if err != nil {
		t.Fatalf("second scanAllRecordings: %v", err)
	}
	if scannedAgain != 0 {
		t.Fatalf("expected 0 scanned after persistence reload, got %d", scannedAgain)
	}
}

func TestResolveAIThreadsBounds(t *testing.T) {
	def := defaultAIThreads()
	if def < 1 || def > 2 {
		t.Fatalf("defaultAIThreads=%d out of [1,2]", def)
	}
	if got := resolveAIThreads(0); got != def {
		t.Fatalf("resolveAIThreads(0)=%d, want %d", got, def)
	}
	if got := resolveAIThreads(-3); got != def {
		t.Fatalf("resolveAIThreads(-3)=%d, want %d", got, def)
	}
	if got := resolveAIThreads(1); got != 1 {
		t.Fatalf("resolveAIThreads(1)=%d, want 1", got)
	}
	if got := resolveAIThreads(999); got != maxInt(1, runtime.NumCPU()) {
		t.Fatalf("resolveAIThreads(999)=%d, want <= NumCPU (%d)", got, runtime.NumCPU())
	}
}

func TestSaveRecMetaConcurrentAtomic(t *testing.T) {
	origMetaFile := metaFile
	metaMu.Lock()
	origCache := metaCache
	metaCache = map[string]recMeta{}
	metaMu.Unlock()
	defer func() {
		metaMu.Lock()
		metaCache = origCache
		metaFile = origMetaFile
		metaMu.Unlock()
	}()

	tmp := t.TempDir()
	path := filepath.Join(tmp, "nested", "rec_meta.json")
	metaMu.Lock()
	metaFile = path
	metaCache["/vol1/test/a.mkv"] = recMeta{Duration: 300.5, Size: 1024, ModTime: 1700000000}
	metaMu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			saveRecMeta()
		}()
	}
	wg.Wait()

	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		t.Fatalf("expected saved rec_meta.json, err=%v len=%d", err, len(raw))
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("expected temp file cleaned up after atomic rename")
	}
}

func TestProbeDurationBoundedConcurrencyAndDebouncedSave(t *testing.T) {
	origMetaFile := metaFile
	origDebounce := metaSaveDebounce
	origEligible := probePathEligible

	metaMu.Lock()
	origCache := metaCache
	metaCache = map[string]recMeta{}
	metaMu.Unlock()
	defer func() {
		metaSaveMu.Lock()
		if metaSaveTimer != nil {
			metaSaveTimer.Stop()
		}
		metaSaveMu.Unlock()
		metaSaveDebounce = origDebounce
		probePathEligible = origEligible
		metaMu.Lock()
		metaCache = origCache
		metaFile = origMetaFile
		metaMu.Unlock()
	}()

	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	runDir := filepath.Join(tmp, "active")
	logFile := filepath.Join(tmp, "calls.log")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// 构造轻量 fake ffmpeg / ffprobe：记录瞬时并发标记、延迟 50ms 后返回精确时长
	fakeFFmpeg := filepath.Join(binDir, "ffmpeg")
	if err := os.WriteFile(fakeFFmpeg, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeProbe := filepath.Join(binDir, "ffprobe")
	script := fmt.Sprintf(`#!/bin/sh
f="%s/pid_$$"
touch "$f"
cur=$(ls -1 "%s" | wc -l)
echo "CUR=$cur" >> "%s"
sleep 0.05
rm -f "$f"
echo "123.45"
`, runDir, runDir, logFile)
	if err := os.WriteFile(fakeProbe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	s, _ := newTestServer(t)
	s.ffm = fakeFFmpeg
	metaSaveDebounce = 80 * time.Millisecond
	probePathEligible = func(path string) bool { return true }

	metaMu.Lock()
	metaFile = s.metaFilePath()
	metaMu.Unlock()

	recDir := filepath.Join(tmp, "recs")
	if err := os.MkdirAll(recDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// 创建 24 个未缓存文件并一次性触发 probeDuration（还额外对第 0 个文件重复触发 5 次）
	var files []string
	for i := 0; i < 24; i++ {
		p := filepath.Join(recDir, fmt.Sprintf("20261116_010%02d00_0.mkv", i))
		if err := os.WriteFile(p, make([]byte, 512*1024), 0o644); err != nil {
			t.Fatal(err)
		}
		files = append(files, p)
	}

	for _, p := range files {
		s.probeDuration(p)
	}
	for i := 0; i < 5; i++ {
		s.probeDuration(files[0])
	}

	// 等待已排队的 probe（最多 16 个，每轮 2 个并发 * 50ms ≈ 400ms）与防抖保存完成
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		probePending.Lock()
		pending := len(probeInFlight)
		probePending.Unlock()
		if pending == 0 {
			if _, err := os.Stat(s.metaFilePath()); err == nil {
				raw, _ := os.ReadFile(s.metaFilePath())
				if len(raw) > 2 {
					var saved map[string]recMeta
					if json.Unmarshal(raw, &saved) == nil {
						if m, ok := saved[files[0]]; ok && m.Duration == 123.45 {
							break
						}
					}
				}
			}
		}
		time.Sleep(30 * time.Millisecond)
	}

	probePending.Lock()
	leftPending := len(probeInFlight)
	probePending.Unlock()
	if leftPending != 0 {
		t.Fatalf("expected all probes finished, %d left", leftPending)
	}

	rawLog, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("expected ffprobe log written: %v", err)
	}
	lines := bytes.Split(bytes.TrimSpace(rawLog), []byte("\n"))
	if len(lines) > probeMaxQueued {
		t.Fatalf("expected <= %d ffprobe invocations due to queue cap, got %d", probeMaxQueued, len(lines))
	}
	if len(lines) == 0 {
		t.Fatalf("expected at least 1 ffprobe invocation")
	}
	for _, ln := range lines {
		var cur int
		if _, err := fmt.Sscanf(string(ln), "CUR=%d", &cur); err == nil && cur > probeMaxConcurrent {
			t.Fatalf("concurrent ffprobe=%d exceeded limit=%d", cur, probeMaxConcurrent)
		}
	}
}
