package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	maxLogFileSize = 10 << 20 // 单日志文件上限 10 MB
	maxLogBackups  = 2        // 最多保留 2 个历史备份（合计上限约 30 MB）
)

var (
	logFileMu       sync.Mutex
	logFile         *os.File
	logFilePath     string
	logCurSize      int64
	logSameAsStderr bool
)

// rotateLogLocked 在持锁状态下执行日志轮转，并原地清空当前主日志文件以兼容已有 stdout/stderr 重定向句柄。
func rotateLogLocked() {
	if logFilePath == "" {
		return
	}
	for i := maxLogBackups; i >= 1; i-- {
		src := logFilePath
		if i > 1 {
			src = fmt.Sprintf("%s.%d", logFilePath, i-1)
		}
		dst := fmt.Sprintf("%s.%d", logFilePath, i)
		if i == 1 {
			// 复制后截断当前文件，防止重定向 >> micamd.log 的外部 fd 写入丢失节点
			if data, err := os.Open(src); err == nil {
				if out, oerr := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); oerr == nil {
					_, _ = io.Copy(out, data)
					_ = out.Close()
				}
				_ = data.Close()
			}
			if logFile != nil {
				_ = logFile.Truncate(0)
				_, _ = logFile.Seek(0, io.SeekEnd)
			} else {
				_ = os.Truncate(src, 0)
			}
			logCurSize = 0
			continue
		}
		if _, err := os.Stat(src); err == nil {
			_ = os.Rename(src, dst)
		}
	}
}

type micamLogWriter struct{}

func (micamLogWriter) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\r\n")
	if line != "" {
		writeLogLine(line)
	}
	return len(p), nil
}

func writeLogLine(msg string) {
	line := time.Now().Format("2006-01-02 15:04:05") + " " + msg + "\n"

	logFileMu.Lock()
	if logFile != nil {
		if logCurSize+int64(len(line)) > maxLogFileSize {
			rotateLogLocked()
		}
		if n, err := logFile.WriteString(line); err == nil {
			logCurSize += int64(n)
		}
	}
	same := logSameAsStderr
	logFileMu.Unlock()

	if !same {
		_, _ = os.Stderr.WriteString(line)
	}
}

// initLogFile 打开并托管单个旋转日志文件，消除重复双写与无限膨胀。
func initLogFile(dir string) {
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("[log] 创建日志目录 %s 失败: %v", dir, err)
	}
	pth := filepath.Join(dir, "micamd.log")
	f, err := os.OpenFile(pth, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}

	var curSize int64
	if fi, serr := f.Stat(); serr == nil {
		curSize = fi.Size()
	}
	same := false
	if fi, serr := f.Stat(); serr == nil {
		if se, eerr := os.Stderr.Stat(); eerr == nil {
			same = os.SameFile(fi, se)
		}
	}

	logFileMu.Lock()
	logFile = f
	logFilePath = pth
	logCurSize = curSize
	logSameAsStderr = same
	if logCurSize >= maxLogFileSize {
		rotateLogLocked()
	}
	logFileMu.Unlock()

	// 让标准库 log.Print/Printf 也走同一个带时间戳、限体积的单通道 writer
	log.SetFlags(0)
	log.SetOutput(micamLogWriter{})
}

func logf(f string, a ...any) {
	writeLogLine(fmt.Sprintf(f, a...))
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
		return nil, fmt.Errorf("等待画面超时（摄像头可能被米家 App 占用）")
	}
}
