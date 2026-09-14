package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/xiaomi/crypto"
	"github.com/AlexxIT/go2rtc/pkg/xiaomi/miss/cs2"
)

const (
	cmdAuthReq = 0x100
	cmdMotor   = 0x112
	cmdEncoded = 0x1001
)

type resp struct {
	Ret       int `json:"ret"`
	Angle     int `json:"angle"`
	Elevation int `json:"elevation"`
}

var (
	conn *cs2.Conn
	key  []byte
)

func query() (*resp, error) {
	j, _ := json.Marshal(map[string]any{"operation": 1})
	buf := make([]byte, 4+len(j))
	binary.BigEndian.PutUint32(buf, cmdMotor)
	copy(buf[4:], j)
	enc, _ := crypto.Encode(buf, key)
	if err := conn.WriteCommand(cmdEncoded, enc); err != nil {
		return nil, err
	}
	type result struct {
		r   *resp
		err error
	}
	ch := make(chan result, 1)
	go func() {
		_, d, err := conn.ReadCommand()
		if err != nil {
			ch <- result{nil, err}
			return
		}
		plain, err := crypto.Decode(d, key)
		if err != nil {
			ch <- result{nil, err}
			return
		}
		var r resp
		if len(plain) > 4 {
			json.Unmarshal(plain[4:], &r)
		}
		ch <- result{&r, nil}
	}()
	select {
	case res := <-ch:
		return res.r, res.err
	case <-time.After(6 * time.Second):
		return nil, fmt.Errorf("超时")
	}
}

func move(op int) (*resp, error) {
	j, _ := json.Marshal(map[string]any{"operation": op})
	buf := make([]byte, 4+len(j))
	binary.BigEndian.PutUint32(buf, cmdMotor)
	copy(buf[4:], j)
	enc, _ := crypto.Encode(buf, key)
	if err := conn.WriteCommand(cmdEncoded, enc); err != nil {
		return nil, err
	}
	type result struct {
		r   *resp
		err error
	}
	ch := make(chan result, 1)
	go func() {
		_, d, err := conn.ReadCommand()
		if err != nil {
			ch <- result{nil, err}
			return
		}
		plain, err := crypto.Decode(d, key)
		if err != nil {
			ch <- result{nil, err}
			return
		}
		var r resp
		if len(plain) > 4 {
			json.Unmarshal(plain[4:], &r)
		}
		ch <- result{&r, nil}
	}()
	select {
	case res := <-ch:
		return res.r, res.err
	case <-time.After(6 * time.Second):
		return nil, fmt.Errorf("超时")
	}
}

func main() {
	rawURL := flag.String("url", "", "URL")
	ops := flag.String("ops", "2,3,4,5", "ops")
	settle := flag.Duration("settle", 6*time.Second, "settle")
	flag.Parse()

	u, _ := url.Parse(*rawURL)
	q := u.Query()
	var err error
	conn, err = cs2.Dial(u.Host, q.Get("transport"))
	if err != nil {
		fmt.Println("连接失败:", err)
		os.Exit(1)
	}
	defer conn.Close()
	key, _ = crypto.CalcSharedKey(q.Get("device_public"), q.Get("client_private"))

	auth := "{\"public_key\":\"" + q.Get("client_public") + "\",\"sign\":\"" + q.Get("sign") + "\",\"uuid\":\"\",\"support_encrypt\":0}"
	conn.WriteCommand(cmdAuthReq, []byte(auth))
	conn.ReadCommand()
	fmt.Println("已认证:", q.Get("model"))
	fmt.Println()

	var list []int
	for _, s := range strings.Split(*ops, ",") {
		if n, e := strconv.Atoi(strings.TrimSpace(s)); e == nil {
			list = append(list, n)
		}
	}

	fmt.Println("严格测试：query -> op -> settle -> query")
	fmt.Println()
	for _, op := range list {
		before, err := query()
		if err != nil {
			fmt.Printf("op=%-3d 前置查询失败: %v\n", op, err)
			continue
		}
		time.Sleep(1 * time.Second)

		ack, err := move(op)
		if err != nil {
			fmt.Printf("op=%-3d 命令失败: %v\n", op, err)
			continue
		}

		time.Sleep(*settle)

		after, err := query()
		if err != nil {
			fmt.Printf("op=%-3d 后置查询失败: %v\n", op, err)
			continue
		}

		fmt.Printf("op=%-3d  ack.ret=%d  |  before(angle=%d elev=%d) -> after(angle=%d elev=%d)  |  dA=%+d dE=%+d\n",
			op, ack.Ret, before.Angle, before.Elevation, after.Angle, after.Elevation,
			after.Angle-before.Angle, after.Elevation-before.Elevation)
		time.Sleep(2 * time.Second)
	}
	fmt.Println()
	fmt.Println("=== 完成 ===")
}
