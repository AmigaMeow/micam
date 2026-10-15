package main

// Provider 用的小工具。

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"time"
)

func newRequest(ctx context.Context, url string, body []byte, key string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	return req, nil
}

func httpClientFor(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout}
}

func b64(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

func cacheEnabled() bool { return globalCacheEnabled }

// globalCacheEnabled 结果缓存开关（Server 启动时从配置注入）
var globalCacheEnabled = true
