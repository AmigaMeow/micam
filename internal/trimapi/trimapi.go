package trimapi

// 飞牛开放 API 客户端。
//
// 关键约束（官方文档 + 安全审查）：
//   1. 只能通过 Unix Socket /var/run/trim_open_gateway_apiscope.socket 调用
//   2. token 从 TRIM_API_TOKEN 读，每次读、不缓存、不落盘、不下发前端
//   3. req 必须白名单，绝不接受前端传入的任意 req
//   4. code != 0 视为失败

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

const (
	SocketPath = "/var/run/trim_open_gateway_apiscope.socket"
	APIPath    = "/api/v1/trimapp"
	maxResp    = 1 << 20
)

var allowed = map[string]bool{
	"trim.system.getPlatformConfig":        true,
	"trim.file.getSharedAccessibleFolders": true,
	"trim.file.delSharedAccessibleFolder":  true,
	"trim.file.getUserAccessibleFolders":   true,
	"trim.file.delUserAccessibleFolder":    true,
	"trim.file.checkUserACL":               true,
	"trim.file.convertPath":                true,
}

// Error 飞牛 API 错误。
type Error struct {
	HTTPStatus int
	Code       int
	Msg        string
	ReqID      string
}

func (e *Error) Error() string {
	return fmt.Sprintf("trimapi: http=%d code=%d msg=%s", e.HTTPStatus, e.Code, e.Msg)
}

// Call 调用飞牛开放 API。
func Call(appName, req string, data map[string]any) (map[string]any, error) {
	if !allowed[req] {
		return nil, fmt.Errorf("trimapi: req 不在白名单: %s", req)
	}
	token := os.Getenv("TRIM_API_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("trimapi: TRIM_API_TOKEN 不可用")
	}
	if data == nil {
		data = map[string]any{}
	}

	body, err := json.Marshal(map[string]any{
		"reqId":   fmt.Sprint(time.Now().UnixNano()),
		"req":     req,
		"appName": appName,
		"data":    data,
	})
	if err != nil {
		return nil, err
	}

	client := &http.Client{
		Timeout: 8 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", SocketPath)
			},
		},
	}

	httpReq, err := http.NewRequest(http.MethodPost, "http://localhost"+APIPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResp))
	if err != nil {
		return nil, err
	}

	var out struct {
		ReqID string          `json:"reqId"`
		Code  int             `json:"code"`
		Msg   string          `json:"msg"`
		Data  json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &Error{HTTPStatus: resp.StatusCode, Msg: "返回非法 JSON"}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || out.Code != 0 {
		return nil, &Error{HTTPStatus: resp.StatusCode, Code: out.Code, Msg: out.Msg, ReqID: out.ReqID}
	}

	var result map[string]any
	if len(out.Data) > 0 {
		_ = json.Unmarshal(out.Data, &result)
	}
	return result, nil
}
