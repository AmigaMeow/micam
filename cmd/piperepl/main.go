package main

// 完整复刻插件的 MJPEG 路径

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/mpegts"
	gxiaomi "github.com/AlexxIT/go2rtc/pkg/xiaomi"
)

func main() {
	src := os.Args[1]
	fmt.Println("1) Dial...")
	prod, err := gxiaomi.Dial(src)
	if err != nil {
		fmt.Println("   失败:", err)
		os.Exit(1)
	}
	fmt.Println("   OK")

	fmt.Println("2) 绑定 mpegts consumer...")
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

	fmt.Println("3) 启动 ffmpeg...")
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-analyzeduration", "10M",
		"-probesize", "10M",
		"-fflags", "+discardcorrupt+genpts",
		"-err_detect", "ignore_err",
		"-f", "mpegts", "-i", "pipe:0",
		"-vf", "fps=5,scale=640:-2",
		"-c:v", "mjpeg", "-q:v", "7",
		"-f", "image2pipe",
		"pipe:1",
	}
	cmd := exec.Command("/usr/bin/ffmpeg", args...)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		fmt.Println("   失败:", err)
		os.Exit(1)
	}
	fmt.Println("   OK")

	fmt.Println("4) 启动 producer 读循环...")
	pr, pw := newPipe()
	go func() {
		defer pw.Close()
		cons.WriteTo(pw)
	}()
	go func() { prod.Start() }()

	// 把 pipe 数据喂给 ffmpeg
	go func() {
		defer stdin.Close()
		buf := make([]byte, 64*1024)
		for {
			n, err := pr.Read(buf)
			if n > 0 {
				if _, werr := stdin.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	fmt.Println("5) 读 ffmpeg 输出 20 秒...")
	var out bytes.Buffer
	done := make(chan struct{})
	go func() {
		buf := make([]byte, 64*1024)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				out.Write(buf[:n])
			}
			if err != nil {
				close(done)
				return
			}
		}
	}()

	select {
	case <-done:
		fmt.Println("   ffmpeg 退出")
	case <-time.After(20 * time.Second):
		fmt.Println("   20秒超时")
	}

	d := out.Bytes()
	fmt.Printf("   输出 %d 字节\n", len(d))
	fmt.Printf("   JPEG 帧数: %d\n", bytes.Count(d, []byte{0xFF, 0xD8, 0xFF}))
	if errBuf.Len() > 0 {
		fmt.Println("   ffmpeg stderr:")
		e := errBuf.String()
		if len(e) > 600 {
			e = e[:600]
		}
		fmt.Println(e)
	}
	_ = cmd.Process.Kill()
}

func newPipe() (*os.File, *os.File) {
	r, w, _ := os.Pipe()
	return r, w
}