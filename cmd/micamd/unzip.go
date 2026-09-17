package main

// 解压辅助（Go 标准库，不依赖外部 unzip 命令）。

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// unzipTo 把 zip 解压到 dstDir（保留内部目录结构，做了路径穿越防护）。
func unzipTo(zipPath, dstDir string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		name := filepath.Clean(f.Name)
		if strings.HasPrefix(name, "..") || filepath.IsAbs(name) {
			continue // 防路径穿越
		}
		dst := filepath.Join(dstDir, name)
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(dst, 0o755); err != nil {
				logf("[unzip] 创建目标目录 %s 失败: %v", dst, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		w, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(w, rc)
		rc.Close()
		w.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
