//go:build windows

package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestACPAuthProbeReapsWindowsTree(t *testing.T) {
	for _, mode := range []string{"descendant", "hang-descendant"} {
		t.Run(mode, func(t *testing.T) {
			isolateACPAuthFixture(t)
			t.Setenv("MILEVIA_AUTH_FIXTURE", mode)
			pidFile := filepath.Join(t.TempDir(), "pids.json")
			t.Setenv("MILEVIA_AUTH_PID_FILE", pidFile)
			fixture := filepath.Join(t.TempDir(), "fake cli.cjs")
			if err := os.WriteFile(fixture, []byte(acpAuthFixture), 0600); err != nil {
				t.Fatal(err)
			}
			runner := &codebuddyCLIRunner{paths: &agentPathResolver{override: map[string]string{"codebuddy": fixture}}}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			done := make(chan struct{})
			var state agentAuthState
			var err error
			go func() { state, err = runner.LoginStatus(ctx); close(done) }()
			var pids []uint32
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if data, readErr := os.ReadFile(pidFile); readErr == nil && json.Unmarshal(data, &pids) == nil {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if len(pids) != 2 {
				t.Fatal("fixture process tree did not start")
			}
			if mode == "hang-descendant" {
				cancel()
			}
			select {
			case <-done:
			case <-time.After(4 * time.Second):
				t.Fatal("probe did not stop within grace period")
			}
			if mode == "descendant" && (state != agentAuthLoggedIn || err != nil) {
				t.Fatalf("state=%s err=%v", state, err)
			}
			if mode == "hang-descendant" && (state != agentAuthUnreadable || err == nil) {
				t.Fatalf("cancelled state=%s err=%v", state, err)
			}
			for _, pid := range pids {
				handle, openErr := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, pid)
				if openErr != nil {
					continue
				}
				status, waitErr := windows.WaitForSingleObject(handle, 2000)
				if waitErr != nil || status != windows.WAIT_OBJECT_0 {
					_ = windows.TerminateProcess(handle, 1)
					windows.CloseHandle(handle)
					t.Fatalf("probe left process %d alive", pid)
				}
				windows.CloseHandle(handle)
			}
		})
	}
}
