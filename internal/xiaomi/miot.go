package xiaomi

// 小米 MIoT 属性读写（米家物联协议）。
//
// 接口：
//   POST /miotspec/prop/get    读属性
//   POST /miotspec/prop/set    写属性
//   POST /miotspec/action      调用动作
//
// 关键：did 必须放在 params 数组的每一项里，不能放在 body 顶层。
// 错误格式会导致 code=-704090001。
//
// 正确格式：
//   {"params":[{"did":"xxx","siid":2,"piid":1}]}

import (
	"encoding/json"
	"fmt"
	"log"
)

// MIoTProp 属性引用。
type MIoTProp struct {
	Siid int `json:"siid"`
	Piid int `json:"piid"`
}

// MIoTResult 属性读写结果。
type MIoTResult struct {
	Did     string          `json:"did"`
	Siid    int             `json:"siid"`
	Piid    int             `json:"piid"`
	Code    int             `json:"code"`
	Value   json.RawMessage `json:"value"`
	ExeTime int             `json:"exe_time"`
}

// GetProps 读属性。
func (s *Session) GetProps(did string, props []MIoTProp) ([]MIoTResult, error) {
	// did 放在每个 param 里
	params := make([]map[string]any, 0, len(props))
	for _, p := range props {
		params = append(params, map[string]any{
			"did":  did,
			"siid": p.Siid,
			"piid": p.Piid,
		})
	}
	data, _ := json.Marshal(map[string]any{"params": params})

	raw, err := s.Request("/miotspec/prop/get", string(data))
	if err != nil {
		return nil, err
	}

	var out []MIoTResult
	if err := json.Unmarshal(raw, &out); err != nil {
		var wrap struct {
			Result []MIoTResult `json:"result"`
		}
		if e2 := json.Unmarshal(raw, &wrap); e2 != nil {
			return nil, fmt.Errorf("解析属性失败: %w", err)
		}
		out = wrap.Result
	}

	// 记录失败项
	failed := 0
	for _, r := range out {
		if r.Code != 0 {
			failed++
		}
	}
	if failed > 0 {
		log.Printf("[miot] prop/get: %d/%d 项失败 (raw=%s)", failed, len(out), truncate(string(raw), 200))
	}
	return out, nil
}

// SetProp 写属性。
func (s *Session) SetProp(did string, siid, piid int, value any) error {
	data, _ := json.Marshal(map[string]any{
		"params": []map[string]any{
			{"did": did, "siid": siid, "piid": piid, "value": value},
		},
	})

	raw, err := s.Request("/miotspec/prop/set", string(data))
	if err != nil {
		return err
	}

	var out []struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("解析响应失败: %w", err)
	}
	for _, r := range out {
		if r.Code != 0 {
			return fmt.Errorf("设置被拒绝 code=%d", r.Code)
		}
	}
	return nil
}

// CallAction 调用动作。
func (s *Session) CallAction(did string, siid, aiid int, in []any) (json.RawMessage, error) {
	if in == nil {
		in = []any{}
	}
	data, _ := json.Marshal(map[string]any{
		"params": []map[string]any{
			{"did": did, "siid": siid, "aiid": aiid, "in": in},
		},
	})

	return s.Request("/miotspec/action", string(data))
}

// truncate 截断字符串（日志用）。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ===== 摄像头常用常量 =====
const (
	SiidCamera    = 2
	SiidPTZ       = 5
	SiidNightShot = 6
	SiidMotion    = 7
)
