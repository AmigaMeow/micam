package xiaomi

import (
	"crypto/hmac"
	"crypto/rc4"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/url"
	"time"
)

// Nonce 生成小米云 API 要求的 nonce。
// 关键：时间戳取的是「分钟」（Unix()/60），不是秒也不是毫秒。
func Nonce() []byte {
	n := make([]byte, 12)
	binary.BigEndian.PutUint32(n[8:], uint32(time.Now().Unix()/60))
	// 前 8 字节随机；这里用时间纳秒做熵，避免引入额外依赖
	binary.BigEndian.PutUint64(n[:8], uint64(time.Now().UnixNano()))
	return n
}

// SignedNonce = SHA256(ssecurity + nonce)
func SignedNonce(ssecurity []byte, nonce []byte) []byte {
	h := sha256.New()
	h.Write(ssecurity)
	h.Write(nonce)
	return h.Sum(nil)
}

// RC4Crypt 小米云用的 RC4。
// 关键：必须先丢弃前 1024 字节密钥流，否则签名/解密全错。
func RC4Crypt(key, plaintext []byte) ([]byte, error) {
	c, err := rc4.NewCipher(key)
	if err != nil {
		return nil, err
	}
	drop := make([]byte, 1024)
	c.XORKeyStream(drop, drop)
	out := make([]byte, len(plaintext))
	c.XORKeyStream(out, plaintext)
	return out, nil
}

// Signature 生成签名串。
// 顺序固定：method & path & data=... [& rc4_hash__=...] & base64(signedNonce)
func Signature(method, path string, values url.Values, signedNonce []byte) string {
	s := method + "&" + path + "&data=" + values.Get("data")
	if values.Has("rc4_hash__") {
		s += "&rc4_hash__=" + values.Get("rc4_hash__")
	}
	s += "&" + base64.StdEncoding.EncodeToString(signedNonce)

	h := sha1.New()
	h.Write([]byte(s))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// MD5Upper 登录时密码要转成大写 MD5。
func MD5Upper(s string) string {
	return fmt.Sprintf("%X", md5sum([]byte(s)))
}

// SignRequest 把 data 按小米云要求加密并签名，返回可直接 POST 的表单。
func SignRequest(ssecurity []byte, path, data string) (url.Values, []byte, error) {
	nonce := Nonce()
	sn := SignedNonce(ssecurity, nonce)

	form := url.Values{"data": {data}}
	form.Set("rc4_hash__", Signature("POST", path, form, sn))

	for _, k := range []string{"data", "rc4_hash__"} {
		ct, err := RC4Crypt(sn, []byte(form.Get(k)))
		if err != nil {
			return nil, nil, err
		}
		form.Set(k, base64.StdEncoding.EncodeToString(ct))
	}

	form.Set("signature", Signature("POST", path, form, sn))
	form.Set("_nonce", base64.StdEncoding.EncodeToString(nonce))
	return form, sn, nil
}

// DecryptResponse 解密小米云返回体。
func DecryptResponse(sn []byte, body []byte) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(string(body))
	if err != nil {
		return nil, err
	}
	return RC4Crypt(sn, raw)
}

// hmacSHA256 预留（部分接口用）。
func hmacSHA256(key, msg []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(msg)
	return h.Sum(nil)
}
