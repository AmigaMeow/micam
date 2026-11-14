package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeLowSpaceMode(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "deleteOldest"},
		{"deleteOldest", "deleteOldest"},
		{"protectAI", "protectAI"},
		{"stopRecording", "stopRecording"},
		{"neverDelete", "neverDelete"},
		{"none", "neverDelete"},
		{"unknown", "deleteOldest"},
	}
	for _, tc := range cases {
		if got := NormalizeLowSpaceMode(tc.in); got != tc.want {
			t.Fatalf("NormalizeLowSpaceMode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestKeepDaysZeroPersistedOnReload(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	cfg := Default()
	if err := cfg.Save(p); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	zero := 0
	noneMode := "none"
	if err := cfg.SetRecordPolicy(nil, nil, nil, &zero, nil, nil, nil, nil, nil, &noneMode, nil); err != nil {
		t.Fatalf("SetRecordPolicy failed: %v", err)
	}
	loaded := Load(p)
	if loaded.RecordKeepDays != 0 {
		t.Fatalf("RecordKeepDays = %d, want 0", loaded.RecordKeepDays)
	}
	if loaded.RecordLowSpaceMode != "neverDelete" {
		t.Fatalf("RecordLowSpaceMode = %q, want neverDelete", loaded.RecordLowSpaceMode)
	}
	_ = os.Remove(p)
}
