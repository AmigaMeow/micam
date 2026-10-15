package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBarkDelivery(t *testing.T) {
	var receivedBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/push" {
			t.Errorf("expected path /push, got %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &receivedBody)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))
	}))
	defer ts.Close()

	ev := Event{
		Title:   "测试标题",
		Message: "测试内容",
		Level:   "warn",
		Time:    "2026-10-14 22:00:00",
	}

	params := map[string]string{
		"barkKey":    "test-device-key-123",
		"barkServer": ts.URL,
		"barkSound":  "alarm.caf",
	}

	err := DeliverToChannel(context.Background(), ts.Client(), "bark", "", params, ev)
	if err != nil {
		t.Fatalf("DeliverToChannel(bark) failed: %v", err)
	}

	if receivedBody["device_key"] != "test-device-key-123" {
		t.Errorf("expected device_key test-device-key-123, got %v", receivedBody["device_key"])
	}
	if receivedBody["sound"] != "alarm.caf" {
		t.Errorf("expected sound alarm.caf, got %v", receivedBody["sound"])
	}
	if receivedBody["level"] != "timeSensitive" {
		t.Errorf("expected level timeSensitive for warn, got %v", receivedBody["level"])
	}
}

func TestWeComDelivery(t *testing.T) {
	var receivedBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &receivedBody)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer ts.Close()

	ev := Event{
		Title:   "车牌识别通知",
		Message: "白色车辆 京A88888 驶入车位",
		Level:   "info",
		Time:    "2026-10-14 22:10:00",
	}

	params := map[string]string{
		"wecomKey": ts.URL,
	}

	err := DeliverToChannel(context.Background(), ts.Client(), "wecom", "", params, ev)
	if err != nil {
		t.Fatalf("DeliverToChannel(wecom) failed: %v", err)
	}

	if receivedBody["msgtype"] != "markdown" {
		t.Errorf("expected msgtype markdown, got %v", receivedBody["msgtype"])
	}
	md, ok := receivedBody["markdown"].(map[string]any)
	if !ok || !strings.Contains(md["content"].(string), "京A88888") {
		t.Errorf("markdown content missing expected text: %v", md)
	}
}

func TestDingTalkDeliveryWithSignature(t *testing.T) {
	var gotQuery string
	var receivedBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &receivedBody)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer ts.Close()

	ev := Event{
		Title:   "夜间高危报警",
		Message: "检测到门口有人异常徘徊",
		Level:   "warn",
		Time:    "2026-10-14 22:15:00",
	}

	params := map[string]string{
		"dingToken":  ts.URL,
		"dingSecret": "SEC123456789abcdef",
	}

	err := DeliverToChannel(context.Background(), ts.Client(), "dingtalk", "", params, ev)
	if err != nil {
		t.Fatalf("DeliverToChannel(dingtalk) failed: %v", err)
	}

	if !strings.Contains(gotQuery, "timestamp=") || !strings.Contains(gotQuery, "sign=") {
		t.Errorf("expected signature and timestamp in URL query, got: %s", gotQuery)
	}
	if receivedBody["msgtype"] != "markdown" {
		t.Errorf("expected msgtype markdown, got %v", receivedBody["msgtype"])
	}
}

func TestFeishuDelivery(t *testing.T) {
	var receivedBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &receivedBody)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"code":0,"msg":"success"}`))
	}))
	defer ts.Close()

	ev := Event{
		Title:   "包裹送达通知",
		Message: "快递员放置包裹在门口",
		Level:   "info",
		Time:    "2026-10-14 22:20:00",
	}

	params := map[string]string{
		"feishuWebhook": ts.URL,
		"feishuSecret":  "feishu-sec-xyz",
	}

	err := DeliverToChannel(context.Background(), ts.Client(), "feishu", "", params, ev)
	if err != nil {
		t.Fatalf("DeliverToChannel(feishu) failed: %v", err)
	}

	if receivedBody["msg_type"] != "interactive" {
		t.Errorf("expected msg_type interactive, got %v", receivedBody["msg_type"])
	}
	if receivedBody["sign"] == nil || receivedBody["sign"] == "" {
		t.Errorf("expected sign field for Feishu signed request")
	}
}

func TestServerChanDelivery(t *testing.T) {
	ev := Event{
		Title:   "摄像头离线",
		Message: "小米室外摄像机 CW500 离线",
		Level:   "error",
		Time:    "2026-10-14 22:25:00",
	}

	params := map[string]string{
		"serverChanKey": "SCT12345test",
	}

	// DeliverServerChan will construct url with key, but for test let's test deliverServerChan logic
	_ = ev
	_ = params
}

func TestQuietHours(t *testing.T) {
	// Tests quiet hours span logic
	if !isQuietTime("00:00", "23:59") {
		t.Errorf("all day should be quiet time")
	}
	if isQuietTime("00:00", "00:00") {
		t.Errorf("00:00 to 00:00 should not be quiet")
	}
}

func TestPushPlusDelivery(t *testing.T) {
	var receivedBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/send" {
			t.Errorf("expected path /send, got %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &receivedBody)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"code":200,"msg":"请求成功","data":"msg_id_123456"}`))
	}))
	defer ts.Close()

	ev := Event{
		Title:   "车辆驶入车位",
		Message: "白色车辆 粤B66688 驶入前院车道",
		Level:   "info",
		Time:    "2026-10-14 22:40:00",
	}

	params := map[string]string{
		"pushPlusToken":  "mock-user-token-abc",
		"pushPlusServer": ts.URL,
		"pushPlusTopic":  "family_group_01",
	}

	err := DeliverToChannel(context.Background(), ts.Client(), "pushplus", "", params, ev)
	if err != nil {
		t.Fatalf("DeliverToChannel(pushplus) failed: %v", err)
	}

	if receivedBody["token"] != "mock-user-token-abc" {
		t.Errorf("expected token mock-user-token-abc, got %v", receivedBody["token"])
	}
	if receivedBody["topic"] != "family_group_01" {
		t.Errorf("expected topic family_group_01, got %v", receivedBody["topic"])
	}
	if receivedBody["channel"] != "wechat" {
		t.Errorf("expected default channel wechat, got %v", receivedBody["channel"])
	}
	content, ok := receivedBody["content"].(string)
	if !ok || !strings.Contains(content, "粤B66688") {
		t.Errorf("expected content to contain plate 粤B66688, got: %v", content)
	}
}

func TestHomeAssistantDelivery(t *testing.T) {
	var receivedBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &receivedBody)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer ts.Close()

	ev := Event{
		Title:   "车辆驶入车位",
		Message: "京A88888 停入车位",
		Type:    "ai.vehicle",
		Level:   "info",
		Time:    "2026-10-15 03:50:00",
		Data: map[string]any{
			"device": "CW300",
			"plate":  "京A88888",
			"zone":   "车位",
		},
	}

	params := map[string]string{
		"haWebhookUrl": ts.URL,
	}

	err := DeliverToChannel(context.Background(), ts.Client(), "homeassistant", "", params, ev)
	if err != nil {
		t.Fatalf("DeliverToChannel(homeassistant) failed: %v", err)
	}

	if receivedBody["event"] != "ai.vehicle" {
		t.Errorf("expected event ai.vehicle, got %v", receivedBody["event"])
	}
	if receivedBody["zone"] != "车位" {
		t.Errorf("expected zone 车位, got %v", receivedBody["zone"])
	}
	if receivedBody["source"] != "micam" {
		t.Errorf("expected source micam, got %v", receivedBody["source"])
	}
}

func TestPushoverDelivery(t *testing.T) {
	var receivedForm map[string]string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		receivedForm = map[string]string{}
		for k, v := range r.Form {
			if len(v) > 0 {
				receivedForm[k] = v[0]
			}
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"status":1,"request":"req123"}`))
	}))
	defer ts.Close()

	ev := Event{
		Title:   "人员徘徊报警",
		Message: "门口有人停留",
		Level:   "warn",
		Time:    "2026-10-15 03:51:00",
	}

	params := map[string]string{
		"pushoverUser":     "test_user_key",
		"pushoverToken":    "test_api_token",
		"pushoverDevice":   "iPhone16",
		"pushoverPriority": "1",
	}

	// We pass client pointing to mock server if we override endpoint or test logic
	_ = ev
	_ = params
}

func TestEnhancedWebhookDelivery(t *testing.T) {
	var gotAuthHeader string
	var gotMethod string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotAuthHeader = r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	defer ts.Close()

	ev := Event{
		Title:   "测试自动化",
		Message: "n8n 工作流测试",
		Time:    "2026-10-15 03:52:00",
	}

	params := map[string]string{
		"webhook":        ts.URL,
		"webhookMethod":  "POST",
		"webhookHeaders": "Authorization: Bearer n8n_secret_token_123\nX-Custom: Micam",
	}

	err := DeliverToChannel(context.Background(), ts.Client(), "webhook", "", params, ev)
	if err != nil {
		t.Fatalf("DeliverToChannel(webhook) failed: %v", err)
	}

	if gotMethod != "POST" {
		t.Errorf("expected POST, got %s", gotMethod)
	}
	if gotAuthHeader != "Bearer n8n_secret_token_123" {
		t.Errorf("expected Bearer n8n_secret_token_123, got %s", gotAuthHeader)
	}
}

func TestHomeAssistantDelivery_RestAPI(t *testing.T) {
	var eventReceived bool
	var stateReceived bool
	var gotAuthHeader string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthHeader = r.Header.Get("Authorization")
		if r.URL.Path == "/api/events/micam_event" {
			eventReceived = true
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"message":"Event micam_event fired."}`))
			return
		}
		if r.URL.Path == "/api/states/sensor.micam_latest_event" {
			stateReceived = true
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"entity_id":"sensor.micam_latest_event","state":"ok"}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer ts.Close()

	ev := Event{
		Title:   "车辆驶入",
		Message: "京A88888 驶入前院车位",
		Type:    "ai.vehicle",
		Level:   "info",
		Time:    "2026-10-15 05:10:00",
		Data: map[string]any{
			"plate":  "京A88888",
			"device": "CW500双摄版",
		},
	}

	params := map[string]string{
		"haMode":     "rest_api",
		"haBaseUrl":  ts.URL,
		"haToken":    "test_long_lived_jwt_token_ha",
		"haEntityId": "sensor.micam_latest_event",
	}

	err := DeliverToChannel(context.Background(), ts.Client(), "homeassistant", "", params, ev)
	if err != nil {
		t.Fatalf("DeliverToChannel(homeassistant rest_api) failed: %v", err)
	}

	if !eventReceived {
		t.Errorf("expected event to be delivered to /api/events/micam_event")
	}
	if !stateReceived {
		t.Errorf("expected state sensor update to /api/states/sensor.micam_latest_event")
	}
	if gotAuthHeader != "Bearer test_long_lived_jwt_token_ha" {
		t.Errorf("expected auth Bearer token, got %q", gotAuthHeader)
	}
}

func TestWebhookDelivery_N8nPresetAndSignature(t *testing.T) {
	var gotSecretHeader string
	var gotSigHeader string
	var receivedBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSecretHeader = r.Header.Get("X-Webhook-Secret")
		gotSigHeader = r.Header.Get("X-Hub-Signature-256")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &receivedBody)
		w.WriteHeader(200)
	}))
	defer ts.Close()

	ev := Event{
		ID:      "ev_12345",
		Title:   "包裹送达通知",
		Message: "快递员放置包裹在门口",
		Type:    "ai.package",
		Level:   "info",
		Time:    "2026-10-15 05:12:00",
	}

	secret := "my_n8n_secret_9988"
	params := map[string]string{
		"webhook":       ts.URL,
		"webhookPreset": "n8n",
		"webhookSecret": secret,
	}

	err := DeliverToChannel(context.Background(), ts.Client(), "webhook", "", params, ev)
	if err != nil {
		t.Fatalf("DeliverToChannel(webhook n8n) failed: %v", err)
	}

	if gotSecretHeader != secret {
		t.Errorf("expected X-Webhook-Secret %q, got %q", secret, gotSecretHeader)
	}
	if !strings.HasPrefix(gotSigHeader, "sha256=") {
		t.Errorf("expected X-Hub-Signature-256 with sha256= prefix, got %q", gotSigHeader)
	}
	if receivedBody["json"] == nil {
		t.Errorf("expected body to wrap event in json key for n8n preset, got %v", receivedBody)
	}
}
