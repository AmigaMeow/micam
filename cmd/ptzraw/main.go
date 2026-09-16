
package main

// 云台协议探针：直接发各种 payload，看摄像机怎么响应。
//
// 目的：确认 Mac 说的「op=13 绝对定位」到底需要什么参数格式。

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/xiaomi/crypto"
	"github.com/AlexxIT/go2rtc/pkg/xiaomi/miss/cs2"
)

const (
	cmdMotor   = 0x112
	cmdEncoded = 0x1001
)

func main() {
	raw, _ := os.ReadFile("/vol1/@appdata/micam/sources.json")
	var m map[string][]struct {
		Src  string    `json:"src"`
		Time time.Time `json:"time"`
	}
	json.Unmarshal(raw, &m)

	var src string
	for did, list := range m {
		if len(list) > 0 && (did == "1149106927" || len(src) == 0) {
			src = list[0].Src
			if did == "1149106927" {
				break
			}
		}
	}
	if src == "" {
		fmt.Println("无凭据")
		return
	}

	u, _ := url.Parse(src)
	q := u.Query()
	key, err := crypto.CalcSharedKey(q.Get("device_public"), q.Get("client_private"))
	if err != nil {
		fmt.Println("共享密钥失败:", err)
		return
	}

	send := func(payload map[string]any) (string, error) {
		conn, err := cs2.Dial(u.Host, q.Get("transport"))
		if err != nil {
			return "", fmt.Errorf("dial: %w", err)
		}
		defer conn.Close()

		if err := login(conn, q.Get("client_public"), q.Get("sign"), key); err != nil {
			return "", fmt.Errorf("login: %w", err)
		}

		body, _ := json.Marshal(payload)
		buf := make([]byte, 4+len(body))
		binary.BigEndian.PutUint32(buf, cmdMotor)
		copy(buf[4:], body)

		enc, err := crypto.Encode(buf, key)
		if err != nil {
			return "", err
		}
		if err := conn.WriteCommand(cmdEncoded, enc); err != nil {
			return "", fmt.Errorf("write: %w", err)
		}

		conn.SetReadDeadline(time.Now().Add(8 * time.Second))
		_, d, err := conn.ReadCommand()
		if err != nil {
			return "", fmt.Errorf("read: %w", err)
		}
		dec, err := crypto.Decode(d, key)
		if err != nil {
			return "", err
		}
		s := string(dec)
		if len(s) > 120 {
			s = s[:120]
		}
		return s, nil
	}

	fmt.Println("=== 基准：只传 operation ===")
	for _, op := range []int{1, 6, 13} {
		r, err := send(map[string]any{"operation": op})
		fmt.Printf("  op=%-3d %s\n", op, out(r, err))
		time.Sleep(2 * time.Second)
	}

	fmt.Println()
	fmt.Println("=== op=13 试各种参数名 ===")
	variants := []map[string]any{
		{"operation": 13, "angle": 40, "elevation": 40},
		{"operation": 13, "x": 40, "y": 40},
		{"operation": 13, "pan": 40, "tilt": 40},
		{"operation": 13, "position": 40},
		{"operation": 13, "h": 40, "v": 40},
		{"operation": 13, "angle": 40},
		{"operation": 13, "horizontal": 40, "vertical": 40},
	}
	for _, v := range variants {
		keys := []string{}
		for k := range v {
			if k != "operation" {
				keys = append(keys, k)
			}
		}
		r, err := send(v)
		fmt.Printf("  %-28s -> %s\n", fmt.Sprint(keys), out(r, err))
		time.Sleep(2500 * time.Millisecond)
	}
}

func out(s string, err error) string {
	if err != nil {
		return "ERR " + err.Error()
	}
	return s
}

func login(conn *cs2.Conn, pub, sign string, key []byte) error {
	payload := fmt.Sprintf(`{"public_key":"%s","sign":"%s","uuid":"","support_encrypt":0}`, pub, sign)
	buf := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(buf, 0x100)
	copy(buf[4:], payload)
	enc, err := crypto.Encode(buf, key)
	if err != nil {
		return err
	}
	return conn.WriteCommand(cmdEncoded, enc)
}
