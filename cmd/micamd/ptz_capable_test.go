package main

import "testing"

func TestIsPTZCapable(t *testing.T) {
	cases := map[string]bool{
		"chuangmi.camera.060a02": true,
		"chuangmi.camera.029a02": false,
		"chuangmi.camera.ipc009": false,
		"chuangmi.camera.070a02": true,
		"isa.camera.hlmax":       true,
		"mxiang.camera.moc001":   true,
		"isa.camera.cw500":       true,
		"xiaomi.camera.c300":     true,
		"xiaomi.camera.c100":     false,
		"chuangmi.camera.046c01": false,
		"xiaomi.cateye.m1":       false,
		"xiaomi.feeder.f1":       false,
	}
	for model, expected := range cases {
		actual := IsPTZCapable(model)
		if actual != expected {
			t.Errorf("model %s expected %v, got %v", model, expected, actual)
		}
	}
}
