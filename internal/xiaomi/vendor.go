package xiaomi

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/xiaomi/crypto"
)

// Vendor 是 miss_get_vendor 返回的通道信息。
type Vendor struct {
	ID     byte
	Name   string
	UID    string
	PubKey string
	Sign   string
}

func vendorName(i byte) string {
	switch i {
	case 1:
		return "tutk"
	case 3:
		return "agora"
	case 4:
		return "cs2"
	case 6:
		return "mtp"
	}
	return fmt.Sprint(i)
}

// GetVendorWithKey 调 /v2/device/miss_get_vendor 拿 P2P 凭据。
//
// ===== 限流时的策略（重要教训，2026-09-15）=====
//
// 早期版本遇到限流会重试 5 次（退避 5/10/20/40 秒，共 75 秒），
// 以为「多试几次总能成功」。
//
// **实测证明这是错的**：小米的限流是滑动窗口，
// 重试的每一次调用都算数，反而让窗口一直不重置。
//
// 结果：冷却结束 -> 重试 75 秒 -> 还是限流 -> 又等冷却
// 限流永远不过去（实测持续 2 小时+）。
//
// 现在：**遇到限流立刻失败，不重试**。
// 让接口安静下来，窗口才有机会重置。
// 重试节奏交给上层（credential.go 的指数退避）。
func (s *Session) GetVendorWithKey(did string) (*Vendor, string, string, error) {
	res, err := s.getVendorOnce(did)
	if err == nil {
		return res.v, res.pub, res.priv, nil
	}

	// 限流：快速失败，绝不重试
	if strings.Contains(err.Error(), "throttle") {
		return nil, "", "", err
	}

	// 其他错误（网络抖动等）重试一次
	time.Sleep(2 * time.Second)
	res, err = s.getVendorOnce(did)
	if err != nil {
		return nil, "", "", err
	}
	return res.v, res.pub, res.priv, nil
}

type vendorResult struct {
	v    *Vendor
	pub  string
	priv string
}

func (s *Session) getVendorOnce(did string) (*vendorResult, error) {
	pub, priv, err := crypto.GenerateKey()
	if err != nil {
		return nil, err
	}
	pubHex := hex.EncodeToString(pub)
	privHex := hex.EncodeToString(priv)

	params, _ := json.Marshal(map[string]any{
		"app_pubkey":      pubHex,
		"did":             did,
		"support_vendors": "TUTK_CS2_MTP",
	})
	raw, err := s.Request("/v2/device/miss_get_vendor", string(params))
	if err != nil {
		return nil, err
	}
	var v struct {
		Vendor struct {
			ID     byte `json:"vendor"`
			Params struct {
				UID string `json:"p2p_id"`
			} `json:"vendor_params"`
		} `json:"vendor"`
		PublicKey string `json:"public_key"`
		Sign      string `json:"sign"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return &vendorResult{
		v: &Vendor{
			ID:     v.Vendor.ID,
			Name:   vendorName(v.Vendor.ID),
			UID:    v.Vendor.Params.UID,
			PubKey: v.PublicKey,
			Sign:   v.Sign,
		},
		pub:  pubHex,
		priv: privHex,
	}, nil
}

// LegacyPass 老机型走 /device/devicepass。
func (s *Session) LegacyPass(did string) (uid, password, pubKey, sign string, err error) {
	pub, _, e := crypto.GenerateKey()
	if e != nil {
		err = e
		return
	}
	params, _ := json.Marshal(map[string]any{
		"did":           did,
		"toSignAppData": hex.EncodeToString(pub),
	})
	raw, e := s.Request("/device/devicepass", string(params))
	if e != nil {
		err = e
		return
	}
	var v struct {
		P2PID     string `json:"p2p_id"`
		Password  string `json:"password"`
		PublicKey string `json:"p2p_dev_public_key"`
		Sign      string `json:"signForAppData"`
	}
	if e = json.Unmarshal(raw, &v); e != nil {
		err = e
		return
	}
	return v.P2PID, v.Password, v.PublicKey, v.Sign, nil
}
