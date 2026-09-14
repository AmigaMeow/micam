package main

// 测试 MIoT 对非摄像头设备是否有效

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/micam/micam/internal/xiaomi"
)

func main() {
	raw, _ := os.ReadFile("/vol1/@appdata/micam/accounts.json")
	var accs []struct {
		UserID    string `json:"userId"`
		PassToken string `json:"passToken"`
	}
	json.Unmarshal(raw, &accs)

	s := xiaomi.NewSession()
	for _, a := range accs {
		if a.UserID == "1000000001" {
			s.SetTokens(a.UserID, a.PassToken)
		}
	}

	// 测试几个非摄像头设备
	tests := []struct {
		Name  string
		Did   string
		Siid  int
		Piid  int
	}{
		{"一楼空调-开关", "1000000104", 2, 1},
		{"一楼空调-模式", "1000000104", 2, 2},
		{"抽油烟机-开关", "1000000105", 2, 1},
		{"大灯-开关", "1125552783", 2, 1},
		{"水泵开关-开关", "1128137626", 2, 1},
		{"小爱音箱-音量", "502390898", 2, 1},
		{"净烟机-开关", "2049389602", 2, 1},
	}

	for _, t := range tests {
		res, err := s.GetProps(t.Did, []xiaomi.MIoTProp{{Siid: t.Siid, Piid: t.Piid}})
		if err != nil {
			fmt.Printf("  %-18s 错误: %v\n", t.Name, err)
			continue
		}
		if len(res) == 0 {
			fmt.Printf("  %-18s 无返回\n", t.Name)
			continue
		}
		r := res[0]
		status := "OK"
		if r.Code != 0 {
			status = fmt.Sprintf("code=%d", r.Code)
		}
		fmt.Printf("  %-18s siid=%d piid=%d -> %s 值=%s\n", t.Name, t.Siid, t.Piid, status, string(r.Value))
	}
}