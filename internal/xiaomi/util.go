package xiaomi

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func nowMillis() int64 {
	return time.Now().UnixNano() / int64(time.Millisecond)
}
