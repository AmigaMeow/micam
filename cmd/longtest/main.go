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
	prod, err := gxiaomi.Dial(src)
	if err != nil {
		fmt.Println("Dial 失败:", err)
		os.Exit(1)
	}

	cons := mpegts.NewConsumer()
	for _, pm := range prod.GetMedias() {
		if pm.Direction != core.DirectionRecvonly {
			continue
		}
		for _, cm := range cons.GetMedias() {
			pc, cc := pm.MatchMedia(cm)
			if pc == nil {
				continue
			}
			tr, _ := prod.GetTrack(pm, pc)
			cons.AddTrack(cm, cc, tr)
		}
	}

	var buf bytes.Buffer
	go func() { cons.WriteTo(&buf) }()
	go func() { prod.Start() }()

	// 每 5 秒报告一次，共 30 秒
	for i := 1; i <= 6; i++ {
		time.Sleep(5 * time.Second)
		n := buf.Len()
		fmt.Printf("  t=%2ds: %8d 字节 (%.1f KB/s)\n", i*5, n, float64(n)/float64(i*5)/1024)
	}

	data := buf.Bytes()
	os.WriteFile("/tmp/long.ts", data, 0644)
	fmt.Printf("  已保存 %d 字节\n", len(data))
	cons.Stop()
	prod.Stop()
}