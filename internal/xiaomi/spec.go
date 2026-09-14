package xiaomi

// MIoT spec 查询与缓存。
//
// miot-spec 描述设备的属性表（siid/piid 含义），是静态的。
// 通过型号查询：https://miot-spec.org/miot-spec-v2/instance?type=<urn>
//
// 缓存策略：model -> spec，内存 + 磁盘双缓存。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SpecProperty 一个属性定义。
type SpecProperty struct {
	IID         int      `json:"iid"`
	Description string   `json:"description"`
	Format      string   `json:"format"`
	Access      []string `json:"access"`
	Unit        string   `json:"unit,omitempty"`
	ValueList   []struct {
		Value       int    `json:"value"`
		Description string `json:"description"`
	} `json:"value-list,omitempty"`
	ValueRange []int `json:"value-range,omitempty"`
}

// SpecAction 动作定义。
type SpecAction struct {
	IID         int    `json:"iid"`
	Description string `json:"description"`
}

// SpecService 服务定义。
type SpecService struct {
	IID         int            `json:"iid"`
	Type        string         `json:"type"`
	Description string         `json:"description"`
	Properties  []SpecProperty `json:"properties"`
	Actions     []SpecAction   `json:"actions"`
}

// Spec 设备规格。
type Spec struct {
	Type        string        `json:"type"`
	Description string        `json:"description"`
	Services    []SpecService `json:"services"`
}

// CanWrite 是否可写。
func (p *SpecProperty) CanWrite() bool {
	for _, a := range p.Access {
		if a == "write" {
			return true
		}
	}
	return false
}

// CanRead 是否可读。
func (p *SpecProperty) CanRead() bool {
	for _, a := range p.Access {
		if a == "read" {
			return true
		}
	}
	return false
}

var (
	specMu    sync.RWMutex
	specCache = map[string]*Spec{}  // model -> spec
	specDir   string                // 磁盘缓存目录
)

// InitSpecCache 初始化 spec 缓存目录。
func InitSpecCache(dir string) {
	specDir = dir
	_ = os.MkdirAll(dir, 0o755)
}

// GetSpec 按型号获取 spec（带缓存）。
func GetSpec(model string) (*Spec, error) {
	// 内存缓存
	specMu.RLock()
	if s, ok := specCache[model]; ok {
		specMu.RUnlock()
		return s, nil
	}
	specMu.RUnlock()

	// 磁盘缓存
	if specDir != "" {
		path := filepath.Join(specDir, model+".json")
		if raw, err := os.ReadFile(path); err == nil {
			var s Spec
			if json.Unmarshal(raw, &s) == nil {
				specMu.Lock()
				specCache[model] = &s
				specMu.Unlock()
				return &s, nil
			}
		}
	}

	// 从网络查
	s, urn, err := fetchSpec(model)
	if err != nil {
		return nil, err
	}

	specMu.Lock()
	specCache[model] = s
	specMu.Unlock()

	// 写磁盘缓存
	if specDir != "" {
		if raw, err := json.Marshal(s); err == nil {
			_ = os.WriteFile(filepath.Join(specDir, model+".json"), raw, 0o644)
		}
	}
	_ = urn
	return s, nil
}

// fetchSpec 从 miot-spec.org 拉取。
func fetchSpec(model string) (*Spec, string, error) {
	// 1) 找 urn
	urn, err := findURN(model)
	if err != nil {
		return nil, "", err
	}

	// 2) 拉详情
	url := "https://miot-spec.org/miot-spec-v2/instance?type=" + urn
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, "", fmt.Errorf("拉取 spec 失败: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, "", err
	}

	var s Spec
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, "", fmt.Errorf("解析 spec 失败: %w", err)
	}
	return &s, urn, nil
}

// findURN 在实例列表里按 model 找 urn。
func findURN(model string) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get("https://miot-spec.org/miot-spec-v2/instances?status=all")
	if err != nil {
		return "", fmt.Errorf("拉取实例列表失败: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return "", err
	}

	var d struct {
		Instances []struct {
			Type  string `json:"type"`
			Model string `json:"model"`
		} `json:"instances"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return "", fmt.Errorf("解析实例列表失败: %w", err)
	}

	// 精确匹配 model
	base := model
	if i := strings.LastIndex(model, ".v"); i > 0 {
		base = model[:i]
	}
	for _, inst := range d.Instances {
		if inst.Model == model || inst.Model == base {
			return inst.Type, nil
		}
	}
	// 模糊匹配
	for _, inst := range d.Instances {
		if strings.Contains(inst.Model, base) || strings.Contains(base, inst.Model) {
			return inst.Type, nil
		}
	}
	return "", fmt.Errorf("未找到型号 %s 的 spec", model)
}
