package main

// vendorcheck: 直接测小米云凭据接口的限流状态。

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/micam/micam/internal/xiaomi"
)

func main() {
	raw, _ := os.ReadFile("/vol1/@appdata/micam/accounts.json")
	var accs []struct {
		UserID    string `json:"userId"`
		PassToken string `json:"passToken"`
	}
	json.Unmarshal(raw, &accs)

	targets := []struct{ did, name string }{
		{"1000000102", "CW300"},
		{"1000000101", "CW500"},
	}

	for _, t := range targets {
		fmt.Printf("\n=== %s (%s) ===\n", t.name, t.did)
		ok := false
		for _, a := range accs {
			s := xiaomi.NewSession()
			if err := s.SetTokens(a.UserID, a.PassToken); err != nil {
				continue
			}
			t0 := time.Now()
			v, pub, priv, err := s.GetVendorWithKey(t.did)
			el := time.Since(t0).Round(time.Millisecond)
			if err != nil {
				fmt.Printf("  账号 %s: 失败 %v (%v)\n", a.UserID, err, el)
				continue
			}
			fmt.Printf("  账号 %s: ✅ 成功 (%v)\n", a.UserID, el)
			fmt.Printf("    vendor=%s uid=%s\n", v.Name, v.UID)
			fmt.Printf("    pub=%s... priv=%s...\n", pub[:16], priv[:16])
			ok = true
			break
		}
		if !ok {
			fmt.Println("  ❌ 所有账号都失败")
		}
	}
}
