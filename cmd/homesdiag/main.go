package main

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

	for _, a := range accs {
		fmt.Printf("\n========== 账号 %s ==========\n", a.UserID)
		s := xiaomi.NewSession()
		if err := s.SetTokens(a.UserID, a.PassToken); err != nil {
			fmt.Println("  登录失败:", err)
			continue
		}

		devs, err := s.Devices()
		if err != nil {
			fmt.Println(" 自有设备失败:", err)
		} else {
			fmt.Printf("\n--- 自有设备 (%d) ---\n", len(devs))
			for i, d := range devs {
				cam := ""
				if d.IsCamera() {
					cam = " [摄像头]"
				}
				fmt.Printf("  %2d. %-28s did=%-12s model=%s%s\n", i+1, d.Name, d.Did, d.Model, cam)
			}
		}

		homes, _ := s.Homes()
		for _, h := range homes {
			if h.ShareFlag == 0 {
				continue
			}
			fmt.Printf("\n--- 共享家庭 %s 的设备 ---\n", h.ID)
			shared, err := s.SharedDevices(h.ID, h.OwnerID())
			if err != nil {
				fmt.Println("  失败:", err)
				continue
			}
			for i, d := range shared {
				cam := ""
				if d.IsCamera() {
					cam = " [摄像头]"
				}
				fmt.Printf("  %2d. %-28s did=%-12s model=%s%s\n", i+1, d.Name, d.Did, d.Model, cam)
			}
		}
	}
}