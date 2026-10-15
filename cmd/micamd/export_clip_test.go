package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestHandleManifest(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest("GET", "/manifest.json", nil)
	w := httptest.NewRecorder()
	srv.handleManifest(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var res map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("failed to parse manifest json: %v", err)
	}
	if res["name"] != "MiCam 智能监控与安全系统" {
		t.Errorf("expected MiCam name, got %v", res["name"])
	}
	if res["display"] != "standalone" {
		t.Errorf("expected standalone display, got %v", res["display"])
	}
}

func TestHandleEventClip_Validation(t *testing.T) {
	srv := &Server{}
	body := bytes.NewReader([]byte(`{"id":"non_existent_clip_test"}`))
	req := httptest.NewRequest("POST", "/api/events/clip", body)
	w := httptest.NewRecorder()
	srv.handleEventClip(w, req)

	if w.Code != 404 && w.Code != 400 {
		t.Errorf("expected 404 or 400 for non-existent clip, got %d", w.Code)
	}
}
