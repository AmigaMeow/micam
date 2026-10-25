package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/micam/micam/internal/config"
)

func TestIsTutkCamera(t *testing.T) {
	cases := map[string]bool{
		"chuangmi.camera.ipc009": true,
		"chuangmi.camera.ipc019": true,
		"chuangmi.camera.ipc013": true,
		"chuangmi.camera.019a01": true,
		"isa.camera.hlmax":       false,
		"chuangmi.camera.029a02": false,
		"chuangmi.camera.060a02": false,
		"mxiang.camera.moc001":   false,
	}

	for model, expected := range cases {
		actual := IsTutkCamera("", model)
		if actual != expected {
			t.Errorf("IsTutkCamera('', %q) = %v; want %v", model, actual, expected)
		}
	}
}

func TestParseStreamURLFromActionResponse(t *testing.T) {
	// Case 1: string out
	res1 := []byte(`{"code": 0, "out": ["https://stream.example.com/live/playlist.m3u8?token=xyz"]}`)
	url1 := parseStreamURLFromActionResponse(res1)
	if url1 != "https://stream.example.com/live/playlist.m3u8?token=xyz" {
		t.Errorf("expected parsed url1, got %q", url1)
	}

	// Case 2: map out
	res2 := []byte(`{"code": 0, "out": [{"stream_url": "https://hlstranscoder.example.com/stream.m3u8"}]}`)
	url2 := parseStreamURLFromActionResponse(res2)
	if url2 != "https://hlstranscoder.example.com/stream.m3u8" {
		t.Errorf("expected parsed url2, got %q", url2)
	}

	// Case 3: error code
	res3 := []byte(`{"code": -1, "out": []}`)
	if url3 := parseStreamURLFromActionResponse(res3); url3 != "" {
		t.Errorf("expected empty on code != 0, got %q", url3)
	}
}

func TestReplayFindFileWithSanitize(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "micam-rec-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Create directory with Chinese name and special characters
	camName := "客厅监控（北京）"
	sName := sanitizeName(camName)
	date := "2026-10-21"
	recDir := filepath.Join(tmpDir, sName, date)
	if err := os.MkdirAll(recDir, 0o755); err != nil {
		t.Fatal(err)
	}

	testFile := "20261021_120000_0.mkv"
	if err := os.WriteFile(filepath.Join(recDir, testFile), []byte("dummy-video-data"), 0o644); err != nil {
		t.Fatal(err)
	}

	recRootsMu.Lock()
	cachedRoots = nil
	recRootsTime = time.Time{}
	recRootsMu.Unlock()

	s := &Server{
		cfg: &config.Config{RecordDir: tmpDir},
	}

	// Test finding file using original unsanitized name
	found, err := s.findRecFile(camName, date, testFile)
	if err != nil {
		t.Fatalf("findRecFile with original name failed: %v", err)
	}
	if filepath.Base(found) != testFile {
		t.Errorf("expected file %q, got %q", testFile, found)
	}

	// Test finding file without date parameter
	foundNoDate, err := s.findRecFile(camName, "", testFile)
	if err != nil {
		t.Fatalf("findRecFile without date failed: %v", err)
	}
	if filepath.Base(foundNoDate) != testFile {
		t.Errorf("expected file %q, got %q", testFile, foundNoDate)
	}
}
