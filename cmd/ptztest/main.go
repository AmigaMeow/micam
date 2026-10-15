package main

// 独立云台测试：直接用 sources.json 里的缓存凭据发 0x112，绕过插件。

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/xiaomi/crypto"
	"github.com/AlexxIT/go2rtc/pkg/xiaomi/miss/cs2"
)

func main() {
	did := os.Args[1]
	op := 1
	if len(os.Args) > 2 {
		op, _ = strconv.Atoi(os.Args[2])
	}

	raw, err := os.ReadFile("/vol1/@appdata/micam/sources.json")
	if err != nil {
		fmt.Println("读 sources.json:", err)
		return
	}
	var m map[string][]struct {
		Src string `json:"src"`
	}
	if json.Unmarshal(raw, &m) != nil {
		fmt.Println("解析失败")
		return
	}
	list := m[did]
	if len(list) == 0 {
		fmt.Println("该设备无缓存凭据")
		return
	}
	src := list[0].Src
	u, err := url.Parse(src)
	if err != nil {
		fmt.Println("URL 非法:", err)
		return
	}
	q := u.Query()
	fmt.Printf("  host          = %s\n", u.Host)
	fmt.Printf("  device_public = %s\n", q.Get("device_public"))
	fmt.Printf("  client_public = %s\n", q.Get("client_public"))

	key, err := crypto.CalcSharedKey(q.Get("device_public"), q.Get("client_private"))
	if err != nil {
		fmt.Println("算密钥失败:", err)
		return
	}
	fmt.Printf("  shared key    = %x...\n", key[:8])

	conn, err := cs2.Dial(u.Host, q.Get("transport"))
	if err != nil {
		fmt.Println("连接摄像头失败:", err)
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(12 * time.Second))

	auth := `{"public_key":"` + q.Get("client_public") + `","sign":"` + q.Get("sign") + `","uuid":"","support_encrypt":0}`
	if err := conn.WriteCommand(0x100, []byte(auth)); err != nil {
		fmt.Println("认证发送失败:", err)
		return
	}
	_, d, err := conn.ReadCommand()
	if err != nil {
		fmt.Println("认证响应失败:", err)
		return
	}
	fmt.Printf("  认证响应      = %s\n", string(d))

	payload := map[string]any{"operation": op}
	if len(os.Args) > 3 {
		if v, e := strconv.Atoi(os.Args[3]); e == nil {
			payload["angle"] = v
		}
	}
	if len(os.Args) > 4 {
		if v, e := strconv.Atoi(os.Args[4]); e == nil {
			payload["elevation"] = v
		}
	}
	body, _ := json.Marshal(payload)
	buf := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(buf, 0x112)
	copy(buf[4:], body)
	enc, err := crypto.Encode(buf, key)
	if err != nil {
		fmt.Println("加密失败:", err)
		return
	}
	if err := conn.WriteCommand(0x1001, enc); err != nil {
		fmt.Println("发送失败:", err)
		return
	}
	_, d2, err := conn.ReadCommand()
	if err != nil {
		fmt.Println("读响应失败:", err)
		return
	}
	plain, err := crypto.Decode(d2, key)
	if err != nil {
		fmt.Println("解密失败:", err)
		return
	}
	fmt.Printf("  云台响应      = %s\n", string(plain))
}
