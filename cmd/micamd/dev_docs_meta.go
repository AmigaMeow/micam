package main

// 文档元信息（给第三方客户端判断「文档有没有更新」）。
//
//	GET /api/dev/docs/meta
//
// 返回版本号 + 内容哈希 + 更新时间。
// 客户端启动时拉一次，对比 hash 即可知道要不要重新读文档。

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
)

func (s *Server) handleDevDocsMeta(w http.ResponseWriter, r *http.Request) {
	body := s.markdownDocs()
	sum := sha256.Sum256([]byte(body))

	// 找实际使用的文件，取 mtime
	path := ""
	for _, p := range []string{
		filepath.Join(appDestDir, "docs", "API.md"),
		filepath.Join("docs", "API.md"),
	} {
		if _, err := os.Stat(p); err == nil {
			path = p
			break
		}
	}
	updated := ""
	if fi, err := os.Stat(path); err == nil {
		updated = fi.ModTime().Format("2006-01-02 15:04:05")
	}

	writeJSON(w, 200, map[string]any{
		"version":   appVersion,
		"hash":      hex.EncodeToString(sum[:])[:16],
		"updatedAt": updated,
		"source":    path,
		"bytes":     len(body),
		"urls": map[string]string{
			"markdown": "/api/dev/docs",
			"openapi":  "/api/dev/openapi",
		},
	})
}
