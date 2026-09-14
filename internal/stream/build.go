package stream

import (
	"fmt"
	"net/url"

	"github.com/micam/micam/internal/xiaomi"
)

// BuildSource 为设备构造完整的 xiaomi:// 源地址（含 P2P 凭据）。
// go2rtc 的 Dial 需要 vendor / public_key / sign 这些参数才会走 miss 分支。
func BuildSource(s *xiaomi.Session, d *xiaomi.Device, region string, extra ...string) (string, error) {
	if region == "" {
		region = "cn"
	}

	if xiaomi.IsLegacyModel(d.Model) {
		uid, pwd, pub, sign, err := s.LegacyPass(d.Did)
		if err != nil {
			return "", err
		}
		base := fmt.Sprintf("xiaomi://%s:%s@%s?did=%s&model=%s",
			s.UserID, region, d.LocalIP, d.Did, d.Model)
		q := url.Values{}
		if sign != "" {
			q.Set("uid", uid)
			q.Set("device_public", pub)
			q.Set("sign", sign)
		} else {
			q.Set("password", pwd)
		}
		return base + "&" + q.Encode(), nil
	}

	v, pubHex, privHex, err := s.GetVendorWithKey(d.Did)
	if err != nil {
		return "", err
	}
	base := fmt.Sprintf("xiaomi://%s:%s@%s?did=%s&model=%s",
		s.UserID, region, d.LocalIP, d.Did, d.Model)
	q := url.Values{}
	q.Set("client_public", pubHex)
	q.Set("client_private", privHex)
	q.Set("device_public", v.PubKey)
	q.Set("sign", v.Sign)
	q.Set("vendor", v.Name)
	if v.ID == 1 && v.UID != "" {
		q.Set("uid", v.UID)
	}
	q.Set("subtype", "3") // 2K 画质
	for _, e := range extra {
		for i := 0; i < len(e); i++ {
			if e[i] == '=' {
				q.Set(e[:i], e[i+1:])
				break
			}
		}
	}
	return base + "&" + q.Encode(), nil
}
