package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// 日志文件句柄（daemon 化后 stdout 可能丢失，这里双写）
var (
	logFileMu sync.Mutex
	logFile   *os.File
)

// initLogFile 打开日志文件。
func initLogFile(dir string) {
	if dir == "" {
		return
	}
	_ = os.MkdirAll(dir, 0o755)
	f, err := os.OpenFile(filepath.Join(dir, "micamd.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err == nil {
		logFileMu.Lock()
		logFile = f
		logFileMu.Unlock()
	}
}

func logf(f string, a ...any) {
	msg := fmt.Sprintf(f, a...)
	log.Print(msg)

	logFileMu.Lock()
	if logFile != nil {
		_, _ = fmt.Fprintf(logFile, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), msg)
		_ = logFile.Sync()
	}
	logFileMu.Unlock()
}

func newPipe() (*io.PipeReader, *io.PipeWriter) {
	return io.Pipe()
}

// readJPEG 从流里读出第一个完整 JPEG 帧。
func readJPEG(r io.Reader, timeout time.Duration) ([]byte, error) {
	type res struct {
		b   []byte
		err error
	}
	ch := make(chan res, 1)
	go func() {
		var buf bytes.Buffer
		tmp := make([]byte, 8192)
		started := false
		for {
			n, err := r.Read(tmp)
			if n > 0 {
				buf.Write(tmp[:n])
				b := buf.Bytes()
				if !started {
					if i := bytes.Index(b, []byte{0xFF, 0xD8}); i >= 0 {
						started = true
						nb := make([]byte, len(b)-i)
						copy(nb, b[i:])
						buf.Reset()
						buf.Write(nb)
						b = buf.Bytes()
					} else if buf.Len() > 1<<20 {
						buf.Reset()
					}
				}
				if started {
					if i := bytes.Index(b, []byte{0xFF, 0xD9}); i >= 0 {
						frame := make([]byte, i+2)
						copy(frame, b[:i+2])
						ch <- res{frame, nil}
						return
					}
					if buf.Len() > 16<<20 {
						ch <- res{nil, fmt.Errorf("帧过大")}
						return
					}
				}
			}
			if err != nil {
				ch <- res{nil, err}
				return
			}
		}
	}()

	select {
	case r := <-ch:
		return r.b, r.err
	case <-time.After(timeout):
		return nil, fmt.Errorf("等待画面超时（摄像头可能被米家 App 或 EasyNVR 占用）")
	}
}
