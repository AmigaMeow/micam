package main

import (
	"bytes"
	"context"
	"sync"
	"testing"
)

type dummyWriteCloser struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	closed bool
}

func (d *dummyWriteCloser) Write(p []byte) (n int, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.buf.Write(p)
}

func (d *dummyWriteCloser) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	return nil
}

func TestMiotChannelSwitch_Seamless(t *testing.T) {
	did := "test_cw500_switch_did"
	ctx, cancel, finish := beginMiotStream(context.Background(), did, "1")
	defer cancel()
	defer finish()

	dummyStdin := &dummyWriteCloser{}
	registerMiotStreamStdin(did, "1", dummyStdin)

	// 1. 同通道无需重复切换，返回 true
	if !switchMiotStreamChannel(did, "1") {
		t.Fatalf("expected switch to current channel 1 to return true")
	}

	// 2. 切换到副摄通道 2
	if !switchMiotStreamChannel(did, "2") {
		t.Fatalf("expected switch to channel 2 to succeed")
	}

	dummyStdin.mu.Lock()
	output := dummyStdin.buf.String()
	dummyStdin.mu.Unlock()

	expectedCmd := "channel 2\n"
	if output != expectedCmd {
		t.Fatalf("expected command %q, got %q", expectedCmd, output)
	}

	// 3. 再次切回主摄通道 1
	if !switchMiotStreamChannel(did, "1") {
		t.Fatalf("expected switch to channel 1 to succeed")
	}

	dummyStdin.mu.Lock()
	output = dummyStdin.buf.String()
	dummyStdin.mu.Unlock()

	expectedCmd2 := "channel 2\nchannel 1\n"
	if output != expectedCmd2 {
		t.Fatalf("expected command %q, got %q", expectedCmd2, output)
	}

	// 4. 不存在的设备返回 false
	if switchMiotStreamChannel("non_existent_did", "2") {
		t.Fatalf("expected non-existent did switch to return false")
	}

	// 5. 校验 finish() 之后资源被清理
	finish()
	if switchMiotStreamChannel(did, "2") {
		t.Fatalf("expected switch after finish to return false")
	}

	dummyStdin.mu.Lock()
	isClosed := dummyStdin.closed
	dummyStdin.mu.Unlock()
	if !isClosed {
		t.Fatalf("expected stdin to be closed on finish")
	}
	_ = ctx
}
