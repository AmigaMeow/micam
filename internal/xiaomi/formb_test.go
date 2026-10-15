package xiaomi

import (
	"encoding/json"
	"strings"
	"testing"
)

// 实测响应（打码后）—— 2026-09-18 线上抓到的形态 B
const formB = `&&&START&&&{"psecurity":"bW9ja19wc2VjdXJpdHlfc3RyaW5nPT0=","nonce":3284657645227332608,"ssecurity":"bW9ja19zc2VjdXJpdHlfc3RyaW5nPT0=","passToken":"V1:MOCK_TOKEN_EXAMPLE_FOR_TESTING","userId":1234567890,"cUserId":"abc","captchaUrl":null}`

func TestParseFormB(t *testing.T) {
	var v struct {
		UserID    any    `json:"userId"`
		Ssecurity any    `json:"ssecurity"`
		CUserID   any    `json:"cUserId"`
		PassToken string `json:"passToken"`
		Location  string `json:"location"`
		Psecurity string `json:"psecurity"`
	}
	if _, err := decodeLoginRaw(strings.NewReader(formB), &v); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if v.PassToken == "" {
		t.Fatal("passToken 为空")
	}
	if v.Location != "" {
		t.Fatal("形态 B 不应有 location")
	}
	if got := anyToStr(v.UserID); got != "1234567890" {
		t.Fatalf("userId 转换失败: got %q", got)
	}
	if got := anyToBytes(v.Ssecurity); len(got) == 0 {
		t.Fatal("ssecurity 转换失败")
	}
	t.Logf("形态 B 解析 OK: userId=%s", anyToStr(v.UserID))
}

func TestAnyToStr(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{float64(123), "123"},
		{"456", "456"},
		{json.Number("789"), "789"},
		{nil, ""},
	}
	for _, c := range cases {
		if got := anyToStr(c.in); got != c.want {
			t.Errorf("anyToStr(%v) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}
