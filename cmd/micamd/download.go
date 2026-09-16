package main

// 通用文件下载（带多镜像重试）。

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// downloadFile 从多个 URL 依次尝试下载到 dst。
//
// 用「临时文件 + 原子改名」：下载中断不会留下半个文件，
// 也就不会出现「模型存在但加载失败」这种困惑状态。
func downloadFile(urls []string, dst string) error {
	var lastErr error
	for i, u := range urls {
		logf("[download] 尝试源 %d/%d: %s", i+1, len(urls), u)

		client := &http.Client{Timeout: 20 * time.Minute}
		res, err := client.Get(u)
		if err != nil {
			lastErr = err
			continue
		}
		if res.StatusCode != 200 {
			res.Body.Close()
			lastErr = fmt.Errorf("HTTP %d", res.StatusCode)
			continue
		}

		tmp := dst + ".part"
		f, err := os.Create(tmp)
		if err != nil {
			res.Body.Close()
			return err
		}

		n, err := io.Copy(f, res.Body)
		res.Body.Close()
		f.Close()

		if err != nil {
			os.Remove(tmp)
			lastErr = err
			continue
		}
		if n < 1024 {
			os.Remove(tmp)
			lastErr = fmt.Errorf("文件太小（%d 字节），可能不是模型", n)
			continue
		}

		_ = os.MkdirAll(filepath.Dir(dst), 0o755)
		if err := os.Rename(tmp, dst); err != nil {
			os.Remove(tmp)
			return err
		}
		logf("[download] 完成: %.1f MB -> %s", float64(n)/1024/1024, dst)
		return nil
	}
	return fmt.Errorf("所有下载源都失败: %v", lastErr)
}
