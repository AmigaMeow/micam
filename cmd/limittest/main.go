package main

// limittest: 对比两个凭据接口的限流情况
//   /v2/device/miss_get_vendor  (当前用)
//   /device/devicepass          (备用，可能不同限流策略)

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

	if len(accs) == 0 {
		fmt.Println("无账号")
		return
	}

	a := accs[1] // 1000000001
	s := xiaomi.NewSession()
	if err := s.SetTokens(a.UserID, a.PassToken); err != nil {
		fmt.Println("登录失败:", err)
		return
	}
	did := "1000000101"

	fmt.Println("=== 1) miss_get_vendor（当前用的）===")
	t0 := time.Now()
	v, _, _, err := s.GetVendorWithKey(did)
	if err != nil {
		fmt.Printf("  失败: %v (%v)\n", err, time.Since(t0).Round(time.Millisecond))
	} else {
		fmt.Printf("  成功: vendor=%s (%v)\n", v.Name, time.Since(t0).Round(time.Millisecond))
	}

	fmt.Println()
	fmt.Println("=== 2) /device/devicepass（备用）===")
	t0 = time.Now()
	uid, pw, pub, sign, err := s.LegacyPass(did)
	if err != nil {
		fmt.Printf("  失败: %v (%v)\n", err, time.Since(t0).Round(time.Millisecond))
	} else {
		fmt.Printf("  成功! (%v)\n", time.Since(t0).Round(time.Millisecond))
		fmt.Printf("  uid=%s\n", uid)
		fmt.Printf("  pw=%s...\n", trunc(pw, 20))
		fmt.Printf("  pub=%s...\n", trunc(pub, 20))
		fmt.Printf("  sign=%s...\n", trunc(sign, 20))
	}

	fmt.Println()
	fmt.Println("=== 3) 其他可能的接口 ===")
	for _, path := range []string{"/v2/device/miss_get_vendor", "/device/devicepass"} {
		fmt.Printf("  已测: %s\n", path)
	}
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
