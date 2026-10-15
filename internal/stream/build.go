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

// BuildSourceChannel 构造指定镜头的源地址。
//
// ===== 双摄摄像头（如 CW500 双摄版）=====
//
// 小米云 API 只返回**一个**设备，第二镜头靠 P2P 命令区分：
//
//	channel 为空   -> {"videoquality": 3}                        第一镜头
//	channel 非空   -> {"videoquality": -1, "videoquality2": 3}   第二镜头
//
// 注意 channel 的值本身不重要（go2rtc 只判断是否为空），
// 但为了可读性我们约定 channel=2 表示第二镜头。
//
// 这个机制是读 go2rtc 的 StartMedia 源码确认的。
func BuildSourceChannel(s *xiaomi.Session, d *xiaomi.Device, region string, channel int) (string, error) {
	var extra []string
	if channel >= 2 {
		extra = append(extra, "channel=2")
	}
	return BuildSource(s, d, region, extra...)
}

// HasSecondLens 判断设备是否有第二镜头。
//
// 目前按型号白名单。已知双摄：
//
//	isa.camera.hlmax  CW500 双摄版
//
// 其他型号如果用户反馈，再加。
func HasSecondLens(model string) bool {
	return LensCount(model) >= 2
}

// LensCount 返回设备的镜头数量（1 或 2）。
//
// ===== 为什么需要（2026-10-11）=====
//
// 用户反馈「我的摄像头是双摄的，你怎么没识别到」。
//
// 根因不是型号识别问题，而是【官方通道的通道注册】：
//
//	register_raw_data(inst, cb, channel) 的第 3 个参数是通道号，
//	我们原来只注册了 channel=0，第二路数据根本没回调。
//
// 逐通道注册后实测（isa.camera.hlmax）：
//
//	ch0 -> 247 帧   ch1 -> 232 帧
//	两路都是 2560x1440，内容完全不同：
//	  channel 1 = 球机（PTZ 可转）
//	  channel 2 = 枪机（固定广角）
//
// 所以「枪球一体机」确实是两个物理镜头。
//
// ⚠️ 这推翻了 2026-09-16 的旧结论（那时用 cs2 旧通道测，
//
//	误判为「同一视场」—— 旧通道本身就有问题）。
func LensCount(model string) int {
	switch model {
	// 枪球一体机（球机 + 枪机）
	case "isa.camera.hlmax": // CW500 双摄版
		return 2
	}
	return 1
}
