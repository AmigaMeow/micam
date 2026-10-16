package main

// 本地车牌识别客户端（常驻 lpr.py 子进程）。
//
// 设计要点：
//   · HyperLPR 初始化要加载 8.4MB 模型（约 1 秒），所以常驻，不每帧重启
//   · 多帧投票 + 格式校验在 Python 侧完成（那里有实测数据支撑）
//   · 读不到就返回空，绝不猜

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// lprResult 一次识别结果
type lprResult struct {
	OK         bool           `json:"ok"`
	Plate      string         `json:"plate"`
	Conf       float64        `json:"conf"`
	Votes      map[string]int `json:"votes"`
	Candidates []lprCandidate `json:"candidates"`
	Frames     int            `json:"frames"`
	MS         int            `json:"ms"`
	Error      string         `json:"error"`
}

type lprCandidate struct {
	Code string  `json:"code"`
	Conf float64 `json:"conf"`
	Src  string  `json:"src"`
}

type lprClient struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	reader  *bufio.Reader
	ready   bool
	lastErr string
	seq     int
}

var lprc *lprClient

func newLPRClient() *lprClient { return &lprClient{} }

// lprPaths 返回 (lib路径, 模型目录, 脚本路径, 是否齐备)
func lprPaths() (string, string, string, bool) {
	base := filepath.Join(dataDirPath, "lpr")
	lib := filepath.Join(base, "lib", "libhyperlpr3.so")
	models := filepath.Join(base, "models")
	script := findAIScript("lpr.py")
	_, e1 := os.Stat(lib)
	_, e2 := os.Stat(models)
	_, e3 := os.Stat(script)
	return lib, models, script, e1 == nil && e2 == nil && e3 == nil
}

// lprAvailable 本地 LPR 是否可用
func lprAvailable() bool {
	_, _, _, ok := lprPaths()
	return ok
}

// start 启动常驻进程
func (c *lprClient) start() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ready && c.cmd != nil && c.cmd.Process != nil {
		return nil
	}

	lib, models, script, ok := lprPaths()
	if !ok {
		return fmt.Errorf("本地 LPR 资源未安装")
	}
	pyExe := "/usr/bin/python3"
	pyPath := ""
	if _, p, err := findPythonPath(); err == nil {
		pyPath = p
	}

	cmd := exec.Command(pyExe, script)
	cmd.Env = append(os.Environ(),
		"PYTHONPATH="+pyPath,
		"MICAM_LPR_LIB="+lib,
		"MICAM_LPR_MODELS="+models,
		"OMP_NUM_THREADS=2",
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = io.Discard // .so 的日志已经在 Python 侧隔离，这里再兜一层

	if err := cmd.Start(); err != nil {
		return err
	}
	c.cmd = cmd
	c.stdin = stdin
	c.reader = bufio.NewReader(stdout)
	c.ready = true
	c.lastErr = ""

	// 第一行是就绪消息
	if line, err := c.reader.ReadString('\n'); err == nil {
		var r lprResult
		if json.Unmarshal([]byte(line), &r) == nil && !r.OK {
			c.lastErr = r.Error
			return fmt.Errorf("%s", r.Error)
		}
	}
	return nil
}

// stop 停掉进程
func (c *lprClient) stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd != nil && c.cmd.Process != nil {
		_, _ = c.stdin.Write([]byte("{\"cmd\":\"quit\"}\n"))
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait()
	}
	c.ready = false
	c.cmd = nil
}

// Read 对若干张图做识别 + 投票。
func (c *lprClient) Read(paths []string, minConf float64, box []int) (*lprResult, error) {
	if len(paths) == 0 {
		return &lprResult{OK: true}, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.ready {
		c.mu.Unlock()
		err := c.start()
		c.mu.Lock()
		if err != nil {
			return nil, err
		}
	}

	reqData := map[string]any{"images": paths, "minConf": minConf}
	if len(box) >= 4 {
		reqData["box"] = box
	}
	req, _ := json.Marshal(reqData)
	if _, err := c.stdin.Write(append(req, '\n')); err != nil {
		c.ready = false
		return nil, err
	}

	// 读结果（带超时保护）
	type res struct {
		line string
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		l, e := c.reader.ReadString('\n')
		ch <- res{l, e}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			c.ready = false
			return nil, r.err
		}
		var out lprResult
		if err := json.Unmarshal([]byte(r.line), &out); err != nil {
			return nil, fmt.Errorf("解析失败: %w", err)
		}
		if !out.OK {
			return &out, fmt.Errorf("%s", out.Error)
		}
		return &out, nil
	case <-time.After(60 * time.Second):
		c.ready = false
		return nil, fmt.Errorf("LPR 超时")
	}
}
