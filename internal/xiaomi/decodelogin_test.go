package xiaomi

import (
	"strings"
	"testing"
)

func TestDecodeLoginRawWrappers(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"纯 JSON", `{"passToken":"V1:x","userId":"123"}`},
		{"只带 START", "&&&START&&&" + `{"passToken":"V1:x","userId":"123"}`},
		{"带 START 和 END", "&&&START&&&" + `{"passToken":"V1:x","userId":"123"}` + "&&&END&&&"},
		{"前后有杂物", "junk&&&START&&&" + `{"passToken":"V1:x","userId":"123"}` + "&&&END&&&trailing"},
	}
	for _, c := range cases {
		var v struct {
			PassToken string `json:"passToken"`
			UserID    string `json:"userId"`
		}
		_, err := decodeLoginRaw(strings.NewReader(c.in), &v)
		if err != nil {
			t.Errorf("%s: 解析失败: %v", c.name, err)
			continue
		}
		if v.PassToken != "V1:x" || v.UserID != "123" {
			t.Errorf("%s: 内容不对 passToken=%q userId=%q", c.name, v.PassToken, v.UserID)
			continue
		}
		t.Logf("%s: OK", c.name)
	}
}
