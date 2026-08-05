package app

import (
	"net"
	"path/filepath"
	"testing"
)

func TestNativeApprovalHookCommandUsesExecutablePath(t *testing.T) {
	runner := &claudeCLIRunner{config: Config{
		ApprovalHook:       `C:\\Program Files\\Milevia\\milevia-approval.exe`,
		NativeApprovalHook: true,
	}}
	if got, want := runner.approvalHookCommand(), `"C:\\Program Files\\Milevia\\milevia-approval.exe"`; got != want {
		t.Fatalf("native approval command: got %q want %q", got, want)
	}

	runner.config.NativeApprovalHook = false
	if got, want := runner.approvalHookCommand(), `sh 'C:\\Program Files\\Milevia\\milevia-approval.exe'`; got != want {
		t.Fatalf("script approval command: got %q want %q", got, want)
	}
}

func TestServeListenerUsesPreboundRandomPort(t *testing.T) {
	server := newDesktopTestServer(t, Config{
		DatabasePath: filepath.Join(t.TempDir(), "milevia.db"),
		AllowedRoot:  t.TempDir(),
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- server.ServeListener(listener, func(url string) { ready <- url }) }()
	url := <-ready
	if url != "http://"+listener.Addr().String() {
		t.Fatalf("ready URL: got %q want listener address", url)
	}
	server.Close()
	if err := <-done; err != nil {
		t.Fatalf("serve listener: %v", err)
	}
}
