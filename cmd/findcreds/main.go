package main

// 从运行中的库进程里捞出 cs2 凭据：
//   收集所有 public_key 候选，逐个用 X25519 反查私钥（32 字节原始窗口）。

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"golang.org/x/crypto/curve25519"
)

func main() {
	pid := strings.TrimSpace(os.Getenv("LIBPID"))
	mm, _ := os.ReadFile("/proc/" + pid + "/maps")
	mem, err := os.Open("/proc/" + pid + "/mem")
	if err != nil {
		fmt.Println("  mem:", err)
		return
	}
	defer mem.Close()

	var blks [][]byte
	total := 0
	for _, line := range strings.Split(string(mm), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || !strings.Contains(f[1], "rw") {
			continue
		}
		var s, e int64
		if _, err := fmt.Sscanf(f[0], "%x-%x", &s, &e); err != nil {
			continue
		}
		if e-s <= 0 || e-s > 200*1024*1024 {
			continue
		}
		buf := make([]byte, e-s)
		n, err := mem.ReadAt(buf, s)
		if err != nil && n <= 0 {
			continue
		}
		blks = append(blks, buf[:n])
		total += n
	}
	fmt.Printf("  内存 %d MB / %d 块\n", total/1024/1024, len(blks))

	re := regexp.MustCompile("\"public_key\":\"([0-9a-f]{64})\"")
	seen := map[string]bool{}
	var cands []string
	for _, b := range blks {
		for _, m := range re.FindAllSubmatch(b, -1) {
			h := string(m[1])
			if !seen[h] {
				seen[h] = true
				cands = append(cands, h)
			}
		}
	}
	fmt.Printf("  public_key 候选: %d 个\n", len(cands))
	for _, c := range cands {
		fmt.Printf("    %s\n", c)
	}
	if len(cands) == 0 {
		fmt.Println("  ✗ 未找到任何 public_key")
		return
	}

	var out []string
	for _, c := range cands {
		want, _ := hex.DecodeString(c)
		if want == nil {
			continue
		}
		pr := brute(blks, want)
		if pr != "" {
			fmt.Printf("  ★ 配对 pub=%s\n         priv=%s\n", c, pr)
			out = append(out, c+" "+pr)
		}
	}
	if len(out) == 0 {
		fmt.Println("  ✗ 未找到任何配对")
		return
	}
	os.WriteFile("/tmp/pairs.txt", []byte(strings.Join(out, "\n")), 0600)
	fmt.Println("  已写 /tmp/pairs.txt")
}

func brute(blks [][]byte, want []byte) string {
	nw := runtime.NumCPU()
	if nw > 8 {
		nw = 8
	}
	jobs := make(chan []byte, 64)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var found string
	for w := 0; w < nw; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for d := range jobs {
				mu.Lock()
				stop := found != ""
				mu.Unlock()
				if stop {
					continue
				}
				for i := 0; i+32 <= len(d); i++ {
					var piv [32]byte
					copy(piv[:], d[i:i+32])
					pub, err := curve25519.X25519(piv[:], curve25519.Basepoint)
					if err != nil {
						continue
					}
					if bytes.Equal(pub, want) {
						mu.Lock()
						if found == "" {
							found = hex.EncodeToString(piv[:])
						}
						mu.Unlock()
						return
					}
				}
			}
		}()
	}
	for _, b := range blks {
		jobs <- b
	}
	close(jobs)
	wg.Wait()
	return found
}
