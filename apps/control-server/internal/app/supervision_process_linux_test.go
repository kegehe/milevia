//go:build linux

package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func linuxSupervisionCommand(t *testing.T, script string, args ...string) *exec.Cmd {
	t.Helper()
	if os.Getenv("MILEVIA_RUN_SUPERVISION_LINUX_PROCESS") != "1" {
		t.Skip("set MILEVIA_RUN_SUPERVISION_LINUX_PROCESS=1 for real cgroup execution")
	}
	if err := supervisionLocalProcessCapability(); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", append([]string{"-c", script, "process-test"}, args...)...)
	cmd.Dir = t.TempDir()
	cmd.WaitDelay = time.Second
	return cmd
}

func TestSupervisionLinuxNativeCLIRejectsInteropAndArbitraryScripts(t *testing.T) {
	if _, err := supervisionNativeClaudeBinary("/bin/sh"); err != nil {
		t.Fatal("native target ELF rejected", err)
	}
	for name, body := range map[string]string{"windows.exe": "MZfake Windows executable", "wrapper": "#!/bin/sh\nexit 0\n"} {
		binary := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(binary, []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
		if _, err := supervisionNativeClaudeBinary(binary); err == nil {
			t.Fatal("non-native CLI could escape Linux process containment", name)
		}
		if _, err := supervisionNativeCodexBinary(binary); err == nil {
			t.Fatal("unrecognized Codex wrapper accepted", name)
		}
	}
}

func TestSupervisionLinuxVerificationRejectsInteropAndResolvesNativeNpm(t *testing.T) {
	for _, body := range []string{"MZfake Windows executable", "#!/bin/sh\nexit 0\n"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "node"), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
		if _, _, err := supervisionNativeVerificationCommand("node", []string{"--version"}); err == nil {
			t.Fatal("verification selected a non-native runtime")
		}
	}
	// Use a real native runtime but synthetic package metadata; the wrapper is
	// never executed. This covers Debian and npm-global symlink entry layouts.
	t.Setenv("PATH", "/usr/bin:/bin")
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("native Node unavailable")
	}
	dir := t.TempDir()
	entry := filepath.Join(dir, "npm", "bin", "npm-cli.js")
	if err := os.MkdirAll(filepath.Dir(entry), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry, []byte("#!/usr/bin/env node\nconsole.log('fixture')"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "npm", "package.json"), []byte(`{"name":"npm","bin":{"npm":"bin/npm-cli.js"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(entry, filepath.Join(dir, "npm-entry")); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(entry, filepath.Join(binDir, "npm")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(node, filepath.Join(binDir, "node")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	binary, args, err := supervisionNativeVerificationCommand("npm", []string{"test"})
	if err != nil || binary == "" || len(args) != 2 || args[0] != entry || args[1] != "test" {
		t.Fatal("standard Linux npm entry was not resolved", binary, args, err)
	}
}

func TestSupervisionLinuxCodexResolvesHoistedAndNestedNativePackages(t *testing.T) {
	arch, triple := "x64", "x86_64-unknown-linux-musl"
	if runtime.GOARCH == "arm64" {
		arch, triple = "arm64", "aarch64-unknown-linux-musl"
	}
	for _, layout := range []string{"hoisted", "nested", "legacy"} {
		t.Run(layout, func(t *testing.T) {
			prefix := t.TempDir()
			root := filepath.Join(prefix, "node_modules", "@openai", "codex")
			entry := filepath.Join(root, "bin", "codex.js")
			if err := os.MkdirAll(filepath.Dir(entry), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(entry, []byte("#!/usr/bin/env node\nthrow Error('never execute npm wrapper')"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"name":"@openai/codex","version":"0.166.0","bin":{"codex":"bin/codex.js"}}`), 0600); err != nil {
				t.Fatal(err)
			}
			var native string
			switch layout {
			case "hoisted":
				native = filepath.Join(filepath.Dir(root), "codex-linux-"+arch, "vendor", triple, "bin", "codex")
			case "nested":
				native = filepath.Join(root, "node_modules", "@openai", "codex-linux-"+arch, "vendor", triple, "bin", "codex")
			case "legacy":
				native = filepath.Join(root, "vendor", triple, "codex", "codex")
			}
			if err := os.MkdirAll(filepath.Dir(native), 0700); err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile("/bin/sh")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(native, body, 0700); err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(prefix, "codex")
			if err := os.Symlink(entry, bin); err != nil {
				t.Fatal(err)
			}
			resolved, err := supervisionNativeCodexBinary(bin)
			if err != nil || resolved != native {
				t.Fatal("standard Linux npm install rejected", layout, resolved, err)
			}
			if err := os.WriteFile(native, []byte("MZforeign interoperability binary"), 0700); err != nil {
				t.Fatal(err)
			}
			if _, err := supervisionNativeCodexBinary(bin); err == nil {
				t.Fatal("foreign platform package accepted")
			}
		})
	}
}

func TestSupervisionLinuxProcessWaitsForDurableHandshake(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker")
	cmd := linuxSupervisionCommand(t, `printf '%s' "$MILEVIA_TEST_LITERAL" > "$1"`, marker)
	cmd.Env = append(os.Environ(), "MILEVIA_TEST_LITERAL=literal $x `value` \\\"\nsecond line")
	container := supervisionJobPrefix + uuid.NewString()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	started, waited, quiet, err := runSupervisionContainedCommand(ctx, cmd, container, func(identity supervisionProcessIdentity, resume func() error) error {
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Error("process ran before handshake")
		}
		if identity.PID <= 0 || identity.Birth == "" || identity.Executable != "/bin/sh" {
			t.Error("missing process identity", identity)
		}
		if quiet, err := supervisionContainerQuiescent(container); err != nil || quiet {
			t.Error("held process not protected", quiet, err)
		}
		return resume()
	})
	if err != nil || !started || !waited || !quiet {
		t.Fatal(started, waited, quiet, err)
	}
	raw, err := os.ReadFile(marker)
	if err != nil || string(raw) != "literal $x `value` \\\"\nsecond line" {
		t.Fatal("environment was interpreted or lost", string(raw), err)
	}
}

func TestSupervisionLinuxRejectedHandshakeDoesNotExecute(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "forbidden")
	cmd := linuxSupervisionCommand(t, `touch "$1"`, marker)
	denied := errors.New("revoked before resume")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	started, waited, quiet, err := runSupervisionContainedCommand(ctx, cmd, supervisionJobPrefix+uuid.NewString(), func(supervisionProcessIdentity, func() error) error { return denied })
	if !errors.Is(err, denied) || !started || !waited || !quiet {
		t.Fatal(started, waited, quiet, err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("revoked process wrote a file", err)
	}
}

func TestSupervisionLinuxReclaimsSetsidDescendantsAndOutput(t *testing.T) {
	cmd := linuxSupervisionCommand(t, `setsid /bin/sh -c 'sleep 60' & echo $!; exit 0`)
	var output bytes.Buffer
	cmd.Stdout = &output
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, waited, quiet, err := runSupervisionContainedCommand(ctx, cmd, supervisionJobPrefix+uuid.NewString(), func(_ supervisionProcessIdentity, resume func() error) error { return resume() })
	if !waited || !quiet {
		t.Fatal(waited, quiet, err)
	}
	pid, parseErr := strconv.Atoi(strings.TrimSpace(output.String()))
	if parseErr != nil {
		t.Fatal(output.String(), parseErr, err)
	}
	_, state, probeErr := supervisionLinuxProcessBirth(pid)
	if probeErr == nil && state != "Z" {
		t.Fatal("setsid descendant survived", pid, state)
	}
}

func TestSupervisionLinuxCancellationAndContainerCollision(t *testing.T) {
	cmd := linuxSupervisionCommand(t, `sleep 60`)
	container := supervisionJobPrefix + uuid.NewString()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, waited, quiet, err := runSupervisionContainedCommand(ctx, cmd, container, func(_ supervisionProcessIdentity, resume func() error) error {
		other, err := startSupervisionContainedProcess(linuxSupervisionCommand(t, `exit 0`), container)
		if err == nil || other != nil {
			t.Error("duplicate container accepted", err)
			if other != nil {
				other.terminate()
				other.close()
			}
		}
		if err := resume(); err != nil {
			return err
		}
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || !waited || !quiet {
		t.Fatal(waited, quiet, err)
	}
}

func TestSupervisionLinuxFastExitKeepsActualStatusAfterCollection(t *testing.T) {
	for _, code := range []int{0, 7, 0, 0, 13, 0} {
		cmd := linuxSupervisionCommand(t, `exit "$1"`, strconv.Itoa(code))
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		_, waited, quiet, err := runSupervisionContainedCommand(ctx, cmd, supervisionJobPrefix+uuid.NewString(), func(_ supervisionProcessIdentity, resume func() error) error { return resume() })
		cancel()
		if !waited || !quiet {
			t.Fatal("process or output did not settle", code, waited, quiet, err)
		}
		if code == 0 {
			if err != nil {
				t.Fatal("collected successful service reported failure", err)
			}
			continue
		}
		var exit *supervisionExitError
		if !errors.As(err, &exit) || exit.Code != code {
			t.Fatal("lost actual target exit code", code, err)
		}
	}
}

func TestSupervisionLinuxGatePreservesStdin(t *testing.T) {
	cmd := linuxSupervisionCommand(t, `cat`)
	cmd.Stdin = strings.NewReader("immutable observation\nsecond line\n")
	var output bytes.Buffer
	cmd.Stdout = &output
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, waited, quiet, err := runSupervisionContainedCommand(ctx, cmd, supervisionJobPrefix+uuid.NewString(), func(_ supervisionProcessIdentity, resume func() error) error { return resume() })
	if err != nil || !waited || !quiet || output.String() != "immutable observation\nsecond line\n" {
		t.Fatal("gate lost model input", output.String(), waited, quiet, err)
	}
}

func TestSupervisionLinuxOwnerCrashStopsDetachedDescendants(t *testing.T) {
	if os.Getenv("MILEVIA_SUPERVISION_OWNER_CRASH_CHILD") == "1" {
		cmd := linuxSupervisionCommand(t, `setsid /bin/sh -c 'sleep 60' & echo "$!" > "$1"; sleep 60`, os.Getenv("MILEVIA_SUPERVISION_OWNER_MARKER"))
		_, _, _, err := runSupervisionContainedCommand(context.Background(), cmd, os.Getenv("MILEVIA_SUPERVISION_OWNER_CONTAINER"), func(_ supervisionProcessIdentity, resume func() error) error { return resume() })
		t.Fatal("owner experiment ended before parent killed it", err)
	}
	// Check the opt-in and cgroup capability before starting a real child test.
	_ = linuxSupervisionCommand(t, "exit 0")
	container := supervisionJobPrefix + uuid.NewString()
	marker := filepath.Join(t.TempDir(), "descendant-pid")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(binary, "-test.run=^TestSupervisionLinuxOwnerCrashStopsDetachedDescendants$", "-test.timeout=30s")
	child.Env = append(os.Environ(), "MILEVIA_SUPERVISION_OWNER_CRASH_CHILD=1", "MILEVIA_SUPERVISION_OWNER_CONTAINER="+container, "MILEVIA_SUPERVISION_OWNER_MARKER="+marker)
	var diagnostics bytes.Buffer
	child.Stdout, child.Stderr = &diagnostics, &diagnostics
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		child.Process.Kill()
		stopSupervisionContainer(container)
	}()
	deadline := time.Now().Add(15 * time.Second)
	var descendant int
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(marker); err == nil {
			descendant, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
			if descendant > 0 {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if descendant <= 0 {
		child.Process.Kill()
		child.Wait()
		t.Fatal("owner never started the real descendant", diagnostics.String())
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	// No controller is alive to stop this service. The trusted gate must react
	// to the original owner's death and systemd must drain its whole cgroup.
	deadline = time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		quiet, err := supervisionContainerQuiescent(container)
		if err != nil {
			t.Fatal(err)
		}
		if quiet {
			_, state, err := supervisionLinuxProcessBirth(descendant)
			if err == nil && state != "Z" {
				t.Fatal("detached descendant survived owner death", descendant, state)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("owner death did not stop the cgroup")
}
