package xiaomi

import "crypto/md5"

func md5sum(b []byte) []byte {
	s := md5.Sum(b)
	return s[:]
}
