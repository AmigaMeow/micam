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
// 该接口容易被小米限流，这里内置退避重试。
func (s *Session) GetVendorWithKey(did string) (*Vendor, string, string, error) {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(5*(1<<uint(attempt-1))) * time.Second
			fmt.Printf("  [vendor] 限流重试 %d/5，等待 %v...\n", attempt, backoff)
			time.Sleep(backoff)
		}
		res, err := s.getVendorOnce(did)
		if err == nil {
			return res.v, res.pub, res.priv, nil
		}
		lastErr = err
		if !strings.Contains(err.Error(), "throttle") {
			return nil, "", "", err
		}
	}
	return nil, "", "", lastErr
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
