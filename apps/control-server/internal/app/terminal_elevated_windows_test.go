//go:build windows

package app

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// TestOpenElevatedTerminalRejectsMissingBridge 确保提权会话在找不到 bridge 时快速
// 失败并给出可定位的错误，而不是触发 UAC 后卡住。测试二进制位于临时目录，同目录
// 无 milevia-terminal-bridge.exe，且显式清空 MILEVIA_TERMINAL_BRIDGE，不会走到
// ShellExecuteEx(runas)。
func TestOpenElevatedTerminalRejectsMissingBridge(t *testing.T) {
	t.Setenv("MILEVIA_TERMINAL_BRIDGE", "")
	_, err := openElevatedWindowsTerminal(context.Background(), TerminalSpec{
		RunnerID: "windows-local",
		WorkDir:  t.TempDir(),
		Shell:    "cmd",
		Cols:     80,
		Rows:     24,
	})
	if err == nil {
		t.Fatal("expected an error when the elevated bridge executable is missing")
	}
	if !strings.Contains(err.Error(), "milevia-terminal-bridge.exe") {
		t.Fatalf("error should point at the bridge executable, got: %v", err)
	}
}

// TestOpenElevatedTerminalRejectsWSLProject 提权只对 Windows 目标有意义，WSL 项目
// 直接拒绝。
func TestOpenElevatedTerminalRejectsWSLProject(t *testing.T) {
	_, err := openElevatedWindowsTerminal(context.Background(), TerminalSpec{
		RunnerID:  "wsl-local",
		WorkDir:   `\\wsl$\Ubuntu\home\dev\project`,
		WSLDistro: "Ubuntu",
		Shell:     "",
		Cols:      80,
		Rows:      24,
	})
	if err == nil || !strings.Contains(err.Error(), "WSL") {
		t.Fatalf("expected a WSL rejection, got: %v", err)
	}
}

// TestAcceptElevatedBridgeValidatesToken 校验回环握手：错误令牌的连接会被丢弃，
// 只有携带正确一次性令牌的连接才会被 accept 返回。
func TestAcceptElevatedBridgeValidatesToken(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	token := "token-" + t.Name()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type acceptOutcome struct {
		conn net.Conn
		err  error
	}
	outcome := make(chan acceptOutcome, 1)
	go func() {
		conn, err := acceptElevatedBridge(ctx, listener, token)
		outcome <- acceptOutcome{conn: conn, err: err}
	}()

	// 先来一个错误令牌的连接，验证它不会被当成合法 bridge 交付。
	bad, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial bad: %v", err)
	}
	if err := writeElevatedBridgeFrame(bad, elevatedBridgeAuthFrame, []byte("wrong-token")); err != nil {
		t.Fatalf("write bad auth: %v", err)
	}
	_ = bad.Close()

	// 正确的令牌连接应被 accept 返回。
	good, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial good: %v", err)
	}
	defer good.Close()
	if err := writeElevatedBridgeFrame(good, elevatedBridgeAuthFrame, []byte(token)); err != nil {
		t.Fatalf("write good auth: %v", err)
	}
	select {
	case result := <-outcome:
		if result.err != nil {
			t.Fatalf("accept elevated bridge: %v", result.err)
		}
		defer result.conn.Close()
		if result.conn.RemoteAddr().String() != good.LocalAddr().String() {
			t.Fatalf("accepted the wrong connection: got %s, want %s", result.conn.RemoteAddr(), good.LocalAddr())
		}
	case <-ctx.Done():
		t.Fatal("accept timed out")
	}
}
