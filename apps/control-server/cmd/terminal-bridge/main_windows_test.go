//go:build windows

package main

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestBridgeTCPTransportRunsShell 验证 bridge 的提权传输通道：
// 以 TCP + 一次性令牌连接，完成 auth→open→ready 握手，再向 Shell 写入命令并
// 读到回显。真正的 UAC 提权无法自动化，这里覆盖的是除 runas 以外的全部链路。
func TestBridgeTCPTransportRunsShell(t *testing.T) {
	runBridgeTCPTransport(t, "cmd", "echo bridge-ready-check")
}

func TestBridgeTCPTransportRunsPowerShell(t *testing.T) {
	runBridgeTCPTransport(t, "powershell", "Write-Output bridge-ready-check")
}

func runBridgeTCPTransport(t *testing.T, shell, probe string) {
	if os.Getenv("MILEVIA_TERMINAL_BRIDGE_TEST_CHILD") == "1" {
		// 子进程模式：注入命令行后跑真实 main()，避免 go test 拦截 -tcp 参数。
		os.Args = []string{os.Args[0], "-tcp", os.Getenv("MILEVIA_TERMINAL_BRIDGE_TEST_CHILD_ADDR"), "-token", os.Getenv("MILEVIA_TERMINAL_BRIDGE_TEST_CHILD_TOKEN")}
		main()
		os.Exit(0)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	token := "test-token-" + t.Name()
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$")
	cmd.Env = append(os.Environ(),
		"MILEVIA_TERMINAL_BRIDGE_TEST_CHILD=1",
		"MILEVIA_TERMINAL_BRIDGE_TEST_CHILD_ADDR="+listener.Addr().String(),
		"MILEVIA_TERMINAL_BRIDGE_TEST_CHILD_TOKEN="+token,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start bridge child: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	accepted := make(chan net.Conn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- conn
	}()
	var conn net.Conn
	select {
	case conn = <-accepted:
	case err := <-acceptErr:
		t.Fatalf("accept: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("bridge child did not connect")
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))

	typ, data, err := readFrame(conn)
	if err != nil || typ != authFrame || string(data) != token {
		t.Fatalf("unexpected auth frame: type=%d err=%v data=%q", typ, err, data)
	}
	payload, err := json.Marshal(openRequest{ProtocolVersion: protocolVersion, WorkDir: t.TempDir(), Shell: shell, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("marshal open: %v", err)
	}
	if err := writeFrame(conn, openFrame, payload); err != nil {
		t.Fatalf("write open: %v", err)
	}

	var output bytes.Buffer
	ready := false
	for !ready {
		typ, data, err = readFrame(conn)
		if err != nil {
			t.Fatalf("read before ready: %v", err)
		}
		switch typ {
		case outputFrame:
			output.Write(data)
		case readyFrame:
			ready = true
		case errorFrame:
			t.Fatalf("bridge error before ready: %s", data)
		default:
			t.Fatalf("unexpected frame %d before ready", typ)
		}
	}

	if err := writeFrame(conn, inputFrame, []byte(probe+"\r\n")); err != nil {
		t.Fatalf("write probe: %v", err)
	}
	for {
		typ, data, err = readFrame(conn)
		if err != nil {
			t.Fatalf("read probe output: %v", err)
		}
		if typ == errorFrame {
			t.Fatalf("bridge error during probe: %s", data)
		}
		if typ != outputFrame {
			continue
		}
		output.Write(data)
		if strings.Contains(output.String(), "bridge-ready-check") {
			break
		}
	}
	_ = writeFrame(conn, closeFrame, nil)
}
