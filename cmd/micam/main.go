package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/micam/micam/internal/stream"
	"github.com/micam/micam/internal/xiaomi"
)

// redact 隐藏源地址里的敏感参数
func redact(s string) string {
	out := s
	for _, k := range []string{"client_private=", "client_public=", "sign="} {
		_ = k
	}
	return out
}

func main() {
	var (
		cmd     = flag.String("cmd", "devices", "命令: qr | login | devices | cameras | export | probe")
		did     = flag.String("did", "", "probe 用：设备 did")
		model   = flag.String("model", "", "probe 用：设备 model")
		ip      = flag.String("ip", "", "probe 用：设备内网 IP")
		user    = flag.String("user", "", "小米账号（邮箱/手机/ID）")
		pass    = flag.String("pass", "", "密码（不推荐命令行传，可用 -passfile）")
		passF   = flag.String("passfile", "", "从文件读密码")
		userID  = flag.String("uid", "", "已登录的 userId")
		pToken  = flag.String("passtoken", "", "已登录的 passToken")
		tokenF  = flag.String("tokenfile", "", "凭据文件（export 输出）")
		qrOut   = flag.String("qrout", "", "二维码图片保存路径")
		asJSON  = flag.Bool("json", false, "以 JSON 输出")
	)
	flag.Parse()

	s := xiaomi.NewSession()

	// 恢复已有凭据
	if *tokenF != "" {
		raw, err := os.ReadFile(*tokenF)
		if err == nil {
			var t map[string]string
			if json.Unmarshal(raw, &t) == nil && t["userId"] != "" {
				*userID, *pToken = t["userId"], t["passToken"]
			}
		}
	}

	switch *cmd {
	case "qr":
		q, err := s.StartQRLogin()
		must(err)
		img, err := q.FetchQRImage()
		must(err)
		out := *qrOut
		if out == "" {
			out = filepath.Join(os.TempDir(), "xiaomi-qr.png")
		}
		must(os.WriteFile(out, img, 0o600))
		fmt.Printf("二维码已保存: %s\n", out)
		fmt.Printf("有效期: %d 秒\n", q.Timeout)
		fmt.Println("请用米家 App 扫码，等待中...")
		must(q.Wait())
		fmt.Printf("登录成功 userId=%s\n", s.UserID)
		printToken(s)

	case "login":
		if *passF != "" {
			b, err := os.ReadFile(*passF)
			must(err)
			*pass = string(b)
		}
		err := s.Login(*user, *pass)
		if err != nil {
			if ve, ok := err.(*xiaomi.VerifyError); ok {
				must(ve.StartVerify())
				fmt.Printf("验证码已发送 (%s)\n", ve.Error())
				fmt.Print("请输入验证码: ")
				var code string
				fmt.Scanln(&code)
				must(ve.Submit(code))
				fmt.Printf("登录成功 userId=%s\n", s.UserID)
				printToken(s)
				return
			}
			must(err)
		}
		fmt.Printf("登录成功 userId=%s\n", s.UserID)
		printToken(s)

	case "devices", "cameras":
		if *userID != "" && *pToken != "" {
			must(s.SetTokens(*userID, *pToken))
		}
		devs, err := s.AllCameras()
		must(err)
		if *asJSON {
			b, _ := json.MarshalIndent(devs, "", "  ")
			fmt.Println(string(b))
			return
		}
		fmt.Printf("发现 %d 台摄像头:\n", len(devs))
		for i, d := range devs {
			status := "离线"
			if d.IsOnline {
				status = "在线"
			}
			fmt.Printf("  %d) %s\n", i+1, d.Name)
			fmt.Printf("     did=%s model=%s ip=%s [%s]\n", d.Did, d.Model, d.LocalIP, status)
		}

	case "probe":
		if *userID != "" && *pToken != "" {
			must(s.SetTokens(*userID, *pToken))
		}
		dev := &xiaomi.Device{Did: *did, Model: *model, LocalIP: *ip}
		src, err := stream.BuildSource(s, dev, "cn")
		must(err)
		fmt.Println("源地址已构造（含 P2P 凭据）")
		mgr := stream.NewManager()
		st := mgr.Get("probe", src)
		info, err := st.Info()
		must(err)
		b, _ := json.MarshalIndent(info, "", "  ")
		fmt.Println(string(b))

		cons := stream.NewTS()
		if err := st.AddConsumer(cons); err != nil {
			must(err)
		}
		data, err := st.ReadBytes(cons, 188*50)
		must(err)
		fmt.Printf("成功读取 %d 字节（流有数据 → 出流正常）\n", len(data))
		st.Close()

	case "export":
		if *userID != "" && *pToken != "" {
			must(s.SetTokens(*userID, *pToken))
		}
		b, err := s.MarshalToken()
		must(err)
		fmt.Println(string(b))
	}
}

func printToken(s *xiaomi.Session) {
	b, _ := s.MarshalToken()
	fmt.Println("凭据（请妥善保存）:")
	fmt.Println(string(b))
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}
