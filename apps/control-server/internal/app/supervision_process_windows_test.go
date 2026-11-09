//go:build windows

package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/windows"
)

func supervisionProcessCommandForTest(t *testing.T, script string) *exec.Cmd {
	t.Helper()
	binary, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Dir = t.TempDir()
	return cmd
}

func TestSupervisionProcessIsContainedAndSuspendedBeforeHandshake(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "executed.txt")
	cmd := supervisionProcessCommandForTest(t, "[IO.File]::WriteAllText('"+strings.ReplaceAll(marker, "'", "''")+"','executed')")
	container := supervisionJobPrefix + uuid.NewString()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	started, waited, quiet, err := runSupervisionContainedCommand(ctx, cmd, container, func(identity supervisionProcessIdentity, resume func() error) error {
		if identity.PID == 0 || len(identity.Birth) != 16 || identity.Container != container {
			t.Fatalf("invalid identity: %+v", identity)
		}
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("process executed before authorization")
		}
		if quiet, err := supervisionContainerQuiescent(container); err != nil || quiet {
			t.Fatalf("suspended process not in container: quiet=%v err=%v", quiet, err)
		}
		return resume()
	})
	if err != nil || !started || !waited || !quiet {
		t.Fatalf("start=%v wait=%v quiet=%v err=%v", started, waited, quiet, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal(err)
	}
	if quiet, err := supervisionContainerQuiescent(container); err != nil || !quiet {
		t.Fatalf("container remained: %v %v", quiet, err)
	}
}

func TestSupervisionProcessRejectedHandshakeCannotExecute(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "must-not-exist.txt")
	cmd := supervisionProcessCommandForTest(t, "[IO.File]::WriteAllText('"+strings.ReplaceAll(marker, "'", "''")+"','executed')")
	denied := errors.New("control already committed")
	started, waited, quiet, err := runSupervisionContainedCommand(context.Background(), cmd, supervisionJobPrefix+uuid.NewString(), func(supervisionProcessIdentity, func() error) error { return denied })
	if !errors.Is(err, denied) || !started || !waited || !quiet {
		t.Fatalf("start=%v wait=%v quiet=%v err=%v", started, waited, quiet, err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rejected process executed")
	}
}

func TestSupervisionProcessWaitReclaimsDescendants(t *testing.T) {
	cmd := supervisionProcessCommandForTest(t, "$info = New-Object System.Diagnostics.ProcessStartInfo; $info.FileName = 'powershell.exe'; $info.Arguments = '-NoLogo -NoProfile -NonInteractive -Command Start-Sleep -Seconds 30'; $info.UseShellExecute = $false; $info.CreateNoWindow = $true; $child = [Diagnostics.Process]::Start($info); [Console]::WriteLine($child.Id)")
	var output bytes.Buffer
	cmd.Stdout = &output
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, waited, quiet, err := runSupervisionContainedCommand(ctx, cmd, supervisionJobPrefix+uuid.NewString(), func(_ supervisionProcessIdentity, resume func() error) error { return resume() })
	if err != nil || !waited || !quiet {
		t.Fatalf("wait=%v quiet=%v err=%v", waited, quiet, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(output.String()))
	if err != nil {
		t.Fatalf("child pid=%q err=%v", output.String(), err)
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	state, err := windows.WaitForSingleObject(handle, 1000)
	if err != nil || state != windows.WAIT_OBJECT_0 {
		t.Fatalf("descendant still alive: state=%d err=%v", state, err)
	}
}

func TestSupervisionProcessCancelAndContainerCollision(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	container := supervisionJobPrefix + uuid.NewString()
	cmd := supervisionProcessCommandForTest(t, "Start-Sleep -Seconds 30")
	_, waited, quiet, err := runSupervisionContainedCommand(ctx, cmd, container, func(_ supervisionProcessIdentity, resume func() error) error {
		other, err := startSupervisionContainedProcess(supervisionProcessCommandForTest(t, "exit 0"), container)
		if err == nil || other != nil {
			if other != nil {
				_ = other.terminate()
				_ = other.wait()
				other.close()
			}
			t.Fatal("duplicate named container was borrowed")
		}
		if err := resume(); err != nil {
			return err
		}
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || !waited || !quiet {
		t.Fatalf("wait=%v quiet=%v err=%v", waited, quiet, err)
	}
}
