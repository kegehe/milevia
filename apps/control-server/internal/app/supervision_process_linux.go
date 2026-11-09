//go:build linux

package app

import (
	"context"
	"debug/elf"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const supervisionJobPrefix = "milevia-supervision-"
const supervisionSystemdDescription = "Milevia controlled process v1"

// The trusted main process outlives the CLI and watches its owning supervisor.
// systemd owns the CLI's whole cgroup, so an abrupt owner death also stops setsid
// descendants without relying on the dead supervisor to send a final signal.
// This script is fixed code; executable and arguments remain positional data.
const supervisionLinuxOwnerGate = `set -f
owner_pid=$1
owner_birth=$2
manager=$3
unit=$4
shift 4
owner_alive() {
  IFS= read -r owner_stat < "/proc/$owner_pid/stat" || return 1
  set -- ${owner_stat##*) }
  [ "$1" != Z ] && [ "${20}" = "$owner_birth" ]
}
# This witness remains runnable while the trusted main gate is SIGSTOP-held.
# Owner death before resume must stop the named unit as well; no PID-only kill
# is used, and the service's cgroup lifetime contains this witness too.
(
  while owner_alive; do sleep 0.1; done
  "$manager" --user --no-block stop "$unit"
) </dev/null >/dev/null 2>&1 &
kill -STOP $$
owner_alive || exit 125
exec 3<&0
"$@" <&3 3<&- &
child=$!
exec 3<&-
while kill -0 "$child" 2>/dev/null; do
  owner_alive || exit 125
  sleep 0.1
done
wait "$child"`

// A process group alone does not contain setsid/double-fork descendants. Linux
// uses a transient user service with a cgroup lifetime. Unsupported managers
// remain unavailable instead of silently falling back to kill(-pid).
func supervisionLocalProcessCapability() error {
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		return errors.New("Linux 托管执行需要 cgroup v2 和 systemd 用户服务")
	}
	for _, binary := range []string{"systemctl", "systemd-run", "/bin/sh"} {
		if _, err := exec.LookPath(binary); err != nil {
			return errors.New("Linux 托管执行需要 systemd-run、systemctl 和 /bin/sh")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	help, err := exec.CommandContext(ctx, "systemd-run", "--help").Output()
	if err != nil || !strings.Contains(string(help), "--expand-environment") {
		return errors.New("Linux 托管执行需要支持 --expand-environment=no 的 systemd-run")
	}
	_, err = supervisionSystemctl("show-environment")
	if err != nil {
		return errors.New("目标环境的 systemd 用户服务不可用，无法核对托管进程及其后代")
	}
	return nil
}

func supervisionSystemctl(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemctl", append([]string{"--user", "--no-pager"}, args...)...)
	cmd.Env = managedCLIEnvironment(nil, os.Environ(), "LC_ALL=C", "SYSTEMD_PAGER=cat")
	raw, err := cmd.Output()
	if len(raw) > 64<<10 {
		return nil, errors.New("systemd 返回超过托管协议上限")
	}
	return raw, err
}

func supervisionSystemdUnit(container string) string { return container + ".service" }

func supervisionSystemdFacts(container string) (map[string]string, error) {
	if !validSupervisionContainer(container) {
		return nil, errors.New("托管容器身份无效")
	}
	raw, err := supervisionSystemctl("show", supervisionSystemdUnit(container), "--property=LoadState,ActiveState,MainPID,Description,InvocationID,ControlGroup,Result,ExecMainStatus")
	facts := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			facts[key] = value
		}
	}
	if facts["LoadState"] == "not-found" {
		return facts, nil
	}
	if err != nil {
		return nil, err
	}
	if facts["LoadState"] != "loaded" || facts["Description"] != supervisionSystemdDescription {
		return nil, errors.New("目标 systemd 服务不属于托管执行协议")
	}
	return facts, nil
}

func supervisionContainerQuiescent(container string) (bool, error) {
	facts, err := supervisionSystemdFacts(container)
	if err != nil {
		return false, err
	}
	if facts["LoadState"] == "not-found" {
		return true, nil
	}
	// KillMode=control-group stops descendants when the trusted main gate exits.
	// Check populated at the kernel boundary before declaring resources quiet.
	if facts["ActiveState"] != "inactive" && facts["ActiveState"] != "failed" {
		return false, nil
	}
	if group := facts["ControlGroup"]; group != "" {
		if !strings.HasPrefix(group, "/") || strings.Contains(group, "..") {
			return false, errors.New("无效的托管 cgroup 路径")
		}
		raw, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", group, "cgroup.events"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		if err == nil && !strings.Contains(string(raw), "populated 0") {
			return false, nil
		}
	}
	return true, nil
}

func stopSupervisionContainer(container string) error {
	facts, err := supervisionSystemdFacts(container)
	if err != nil || facts["LoadState"] == "not-found" {
		return err
	}
	_, err = supervisionSystemctl("stop", "--no-block", supervisionSystemdUnit(container))
	if err != nil {
		// --collect may unload a completed service between show and stop. Only
		// a fresh, successful containment probe can turn that race into success;
		// a missing main PID alone still cannot prove descendants are quiet.
		quiet, probeErr := supervisionContainerQuiescent(container)
		if probeErr == nil && quiet {
			return nil
		}
		return errors.Join(err, probeErr)
	}
	return err
}

func supervisionNativeClaudeBinary(binary string) (string, error) {
	resolved, err := filepath.EvalSymlinks(binary)
	if err != nil {
		return "", err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	file, err := elf.Open(resolved)
	if err != nil {
		return "", errors.New("Linux 托管 CLI 需要当前环境的原生 ELF 程序，不能使用 Windows 互操作入口或未识别脚本")
	}
	defer file.Close()
	machine := elf.EM_X86_64
	if runtime.GOARCH == "arm64" {
		machine = elf.EM_AARCH64
	} else if runtime.GOARCH != "amd64" {
		return "", errSupervisionProcessUnsupported
	}
	if file.Machine != machine || file.Class != elf.ELFCLASS64 || file.Data != elf.ELFDATA2LSB || file.Type != elf.ET_EXEC && file.Type != elf.ET_DYN {
		return "", errors.New("Linux 托管 CLI 的原生程序架构与目标环境不符")
	}
	return resolved, nil
}

type supervisionLinuxProcess struct {
	mu              sync.Mutex
	id              supervisionProcessIdentity
	cmd             *exec.Cmd
	waitDone        chan struct{}
	waitErr         error
	environmentFile string
	resumed         bool
}

func (p *supervisionLinuxProcess) identity() supervisionProcessIdentity { return p.id }

func supervisionLinuxProcessBirth(pid int) (birth, state string, resultErr error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", "", err
	}
	end := strings.LastIndexByte(string(raw), ')')
	if end < 0 {
		return "", "", errors.New("无法核对托管进程出生身份")
	}
	fields := strings.Fields(string(raw)[end+1:])
	if len(fields) < 20 {
		return "", "", errors.New("托管进程身份记录不完整")
	}
	return fields[19], fields[0], nil
}

func (p *supervisionLinuxProcess) resume() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.resumed {
		return errors.New("托管进程已经恢复")
	}
	birth, state, err := supervisionLinuxProcessBirth(p.id.PID)
	if err != nil || birth != p.id.Birth || state != "T" {
		return errors.New("托管进程不再处于已登记的挂起状态")
	}
	_, err = supervisionSystemctl("kill", "--kill-whom=main", "--signal=CONT", supervisionSystemdUnit(p.id.Container))
	if err == nil {
		p.resumed = true
	}
	return err
}

func (p *supervisionLinuxProcess) terminate() error { return stopSupervisionContainer(p.id.Container) }
func (p *supervisionLinuxProcess) quiescent() (bool, error) {
	return supervisionContainerQuiescent(p.id.Container)
}

func (p *supervisionLinuxProcess) wait() error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-p.waitDone:
			return supervisionLinuxExitError(p.waitErr)
		case <-ticker.C:
			birth, state, err := supervisionLinuxProcessBirth(p.id.PID)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err == nil && birth == p.id.Birth && state != "Z" {
				continue
			}
			// Main exited while a detached child may still own output. Stop the
			// entire cgroup, then wait for the pipe-forwarding process as well.
			stopErr := p.terminate()
			select {
			case <-p.waitDone:
				return errors.Join(supervisionLinuxExitError(p.waitErr), stopErr)
			case <-time.After(5 * time.Second):
				return errors.Join(stopErr, errSupervisionProcessIO)
			}
		}
	}
}

func supervisionLinuxExitError(err error) error {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return &supervisionExitError{Code: exit.ExitCode()}
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		return errors.Join(err, errSupervisionProcessIO)
	}
	return err
}

func (p *supervisionLinuxProcess) close() {
	select {
	case <-p.waitDone:
		os.Remove(p.environmentFile)
	default: /* preserve the file until the service is known quiet */
	}
}

func supervisionSystemdEnvironment(env []string) (string, error) {
	f, err := os.CreateTemp("", "milevia-supervision-env-*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	cleanup := func(err error) (string, error) { f.Close(); os.Remove(name); return "", err }
	for _, value := range env {
		key, value, ok := strings.Cut(value, "=")
		if !ok || key == "" || strings.Trim(key, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_") != "" || strings.ContainsRune(value, 0) {
			return cleanup(errors.New("无效的托管进程环境"))
		}
		// EnvironmentFile quotes are not shell code; escape its own quoting
		// grammar, preserving actual newlines inside double-quoted values.
		value = strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "`", "\\`", "$", "\\$").Replace(value)
		if _, err := fmt.Fprintf(f, "%s=\"%s\"\n", key, value); err != nil {
			return cleanup(err)
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

func startSupervisionContainedProcess(command *exec.Cmd, container string) (supervisionContainedProcess, error) {
	return startLinuxContainedProcess(command, container, true)
}

func startOrdinaryContainedProcess(command *exec.Cmd, container string) (supervisionContainedProcess, error) {
	return startLinuxContainedProcess(command, container, false)
}

func startLinuxContainedProcess(command *exec.Cmd, container string, bounded bool) (supervisionContainedProcess, error) {
	if !validSupervisionContainer(container) || command == nil || !filepath.IsAbs(command.Path) || command.Dir == "" || !filepath.IsAbs(command.Dir) || len(command.Args) == 0 {
		return nil, errors.New("托管进程缺少绝对路径、目录或有效容器")
	}
	if err := supervisionLocalProcessCapability(); err != nil {
		return nil, err
	}
	facts, err := supervisionSystemdFacts(container)
	if err != nil {
		return nil, err
	}
	if facts["LoadState"] != "not-found" {
		return nil, errors.New("托管容器已经存在，禁止重复启动")
	}
	env := command.Env
	if env == nil {
		env = os.Environ()
	}
	envFile, err := supervisionSystemdEnvironment(env)
	if err != nil {
		return nil, err
	}
	ownerPID := os.Getpid()
	manager, err := exec.LookPath("systemctl")
	if err != nil || !filepath.IsAbs(manager) {
		os.Remove(envFile)
		return nil, errors.New("无法解析目标 systemctl 原生管理入口")
	}
	ownerBirth, _, err := supervisionLinuxProcessBirth(ownerPID)
	if err != nil {
		os.Remove(envFile)
		return nil, err
	}
	// Only this fixed positional-argument gate runs before registration. No CLI
	// code executes until the owner persists its identity and calls resume.
	args := []string{"--user", "--quiet", "--pipe", "--wait", "--collect", "--expand-environment=no", "--service-type=exec", "--unit=" + supervisionSystemdUnit(container), "--description=" + supervisionSystemdDescription,
		"--property=KillMode=control-group", "--property=ExitType=main", "--property=TimeoutStopSec=3s", "--property=NoNewPrivileges=yes", "--property=ProtectControlGroups=yes", "--property=EnvironmentFile=" + envFile, "--working-directory=" + command.Dir}
	if bounded {
		args = append(args, "--property=RuntimeMaxSec=1800")
	}
	args = append(args, "--", "/bin/sh", "-c", supervisionLinuxOwnerGate, "milevia-exec-gate", strconv.Itoa(ownerPID), ownerBirth, manager, supervisionSystemdUnit(container), command.Path)
	args = append(args, command.Args[1:]...)
	cmd := exec.Command("systemd-run", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr, cmd.WaitDelay = command.Stdin, command.Stdout, command.Stderr, command.WaitDelay
	cmd.Env = managedCLIEnvironment(nil, os.Environ(), "LC_ALL=C", "SYSTEMD_PAGER=cat")
	p := &supervisionLinuxProcess{id: supervisionProcessIdentity{Container: container, Executable: command.Path, Directory: command.Dir}, cmd: cmd, waitDone: make(chan struct{}), environmentFile: envFile}
	if err := cmd.Start(); err != nil {
		os.Remove(envFile)
		return nil, err
	}
	go func() { p.waitErr = cmd.Wait(); close(p.waitDone) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		facts, err = supervisionSystemdFacts(container)
		if err != nil {
			return p, err
		}
		pid, _ := strconv.Atoi(facts["MainPID"])
		if pid > 0 {
			birth, state, err := supervisionLinuxProcessBirth(pid)
			if err == nil && state == "T" {
				p.id.PID, p.id.Birth = pid, birth
				// systemd already loaded the environment before launching this
				// held gate. It has no restart policy; do not leave credentials on
				// disk for the rest of the model call or after an owner crash.
				os.Remove(envFile)
				return p, nil
			}
		}
		select {
		case <-p.waitDone:
			return p, errors.Join(errors.New("托管服务未进入挂起状态"), p.waitErr)
		case <-time.After(10 * time.Millisecond):
		}
	}
	return p, errors.New("等待托管服务挂起超时")
}
