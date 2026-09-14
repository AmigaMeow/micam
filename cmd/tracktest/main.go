package main

import (
	"bytes"
	"fmt"
	"os"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/mpegts"
	gxiaomi "github.com/AlexxIT/go2rtc/pkg/xiaomi"
)

func main() {
	src := os.Args[1]
	fmt.Println("=== Dial ===")
	prod, err := gxiaomi.Dial(src)
	if err != nil {
		fmt.Println("失败:", err)
		os.Exit(1)
	}
	fmt.Println("成功")
	fmt.Println()

	fmt.Println("=== 轨道结构 ===")
	for i, m := range prod.GetMedias() {
		fmt.Printf("轨道 %d: kind=%s dir=%d codecs=%d\n", i, m.Kind, m.Direction, len(m.Codecs))
		for _, c := range m.Codecs {
			fmt.Printf("    codec: %s/%d payload=%d\n", c.Name, c.ClockRate, c.PayloadType)
		}
	}
	fmt.Println()

	fmt.Println("=== 绑定 mpegts consumer ===")
	cons := mpegts.NewConsumer()
	bound := 0
	for _, pm := range prod.GetMedias() {
		if pm.Direction != core.DirectionRecvonly {
			continue
		}
		for _, cm := range cons.GetMedias() {
			pc, cc := pm.MatchMedia(cm)
			if pc == nil {
				continue
			}
			tr, err := prod.GetTrack(pm, pc)
			if err != nil {
				fmt.Println("  GetTrack 失败:", err)
				continue
			}
			if err := cons.AddTrack(cm, cc, tr); err != nil {
				fmt.Println("  AddTrack 失败:", err)
				continue
			}
			fmt.Printf("  绑定 %s/%s\n", cm.Kind, cc.Name)
			bound++
		}
	}
	if bound == 0 {
		fmt.Println("  未绑定任何轨道")
		os.Exit(1)
	}
	fmt.Println()

	fmt.Println("=== 读取 5 秒 ===")
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		cons.WriteTo(&buf)
		close(done)
	}()
	go func() { prod.Start() }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}

	fmt.Printf("  收到 %d 字节\n", buf.Len())
	if buf.Len() > 0 {
		os.WriteFile("/tmp/cw500.ts", buf.Bytes(), 0644)
		fmt.Println("  已保存 /tmp/cw500.ts")
	}
	cons.Stop()
	prod.Stop()
}