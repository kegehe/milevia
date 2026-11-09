package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
	"golang.org/x/crypto/ssh"
)

// Commands are fixed service code. Only a verified hex content hash is passed
// as argv; no source, role output, credentials or JSON are interpolated.
const supervisionTargetPlatformScript = `set -eu
test "$(uname -s)" = Linux
case "$(uname -m)" in x86_64) printf 'linux-amd64\n';; aarch64|arm64) printf 'linux-arm64\n';; *) exit 72;; esac`

const supervisionTargetInstallScript = `set -eu
umask 077
digest=$1
target_home=${2:-$HOME}
case "$digest" in *[!a-f0-9]*|'') exit 73;; esac
test "${#digest}" = 64
base="$target_home/.local/share/milevia/supervision-targets"
for dir in "$target_home" "$target_home/.local" "$target_home/.local/share" "$target_home/.local/share/milevia" "$base" "$base/$digest"; do
 test ! -L "$dir"
 mkdir -p "$dir"
done
file="$base/$digest/helper"
if test -e "$file"; then
 test ! -L "$file"
 test -f "$file"
 printf '%s  %s\n' "$digest" "$file" | sha256sum -c - >/dev/null
 cat >/dev/null
else
 temporary=$(mktemp "$base/$digest/upload-XXXXXX")
 trap 'rm -f -- "$temporary"' EXIT
 cat >"$temporary"
 printf '%s  %s\n' "$digest" "$temporary" | sha256sum -c - >/dev/null
 chmod 700 "$temporary"
 mv -n -- "$temporary" "$file"
 test ! -L "$file"
 printf '%s  %s\n' "$digest" "$file" | sha256sum -c - >/dev/null
fi
printf 'installed\n'`

const supervisionTargetLaunchScript = `set -eu
umask 077
digest=$1
target_home=${2:-$HOME}
case "$digest" in *[!a-f0-9]*|'') exit 73;; esac
test "${#digest}" = 64
base="$target_home/.local/share/milevia/supervision-targets"
state="$target_home/.local/state/milevia/supervision-execution"
for dir in "$target_home" "$target_home/.local" "$target_home/.local/share" "$target_home/.local/share/milevia" "$base" "$base/$digest" "$target_home/.local/state" "$target_home/.local/state/milevia" "$state"; do
 test ! -L "$dir"
 mkdir -p "$dir"
done
file="$base/$digest/helper"
test ! -L "$file"
printf '%s  %s\n' "$digest" "$file" | sha256sum -c - >/dev/null
export PATH="$HOME/.local/share/milevia/npm-global/bin:$HOME/.npm-global/bin:$HOME/.local/bin:$HOME/.opencode/bin:$PATH"
unset AUTO_APPROVAL_TOKEN AUTO_APPROVAL_RUN_ID AUTO_CONTROL_URL AUTO_DATABASE_PATH AUTO_CLAUDE_PATH AUTO_CODEX_PATH AUTO_OPENCODE_PATH AUTO_REMOTE_AGENT_TOKEN
mode=${3:-supervision-target}
case "$mode" in supervision-target|workspace-guard) ;; *) exit 73;; esac
exec "$file" --mode "$mode" --data-dir "$state"`

type supervisionTargetPipe struct {
	reader io.ReadCloser
	writer io.WriteCloser
	stop   func() error
	wait   func() error
	once   sync.Once
	done   chan struct{}
	err    error
}

func (p *supervisionTargetPipe) Read(raw []byte) (int, error)  { return p.reader.Read(raw) }
func (p *supervisionTargetPipe) Write(raw []byte) (int, error) { return p.writer.Write(raw) }
func (p *supervisionTargetPipe) Close() error {
	p.once.Do(func() {
		_ = p.writer.Close()
		// EOF revokes the target grants and starts trusted process settlement.
		// Give it a bounded grace before closing the transport wrapper. The
		// target's own owner watcher still reclaims CLI descendants on crash.
		done := make(chan error, 1)
		go func() { done <- p.wait() }()
		select {
		case p.err = <-done:
		case <-time.After(8 * time.Second):
			_ = p.stop()
			select {
			case p.err = <-done:
			case <-time.After(2 * time.Second):
				p.err = errors.New("目标连接关闭后尚未确认监督进程结束")
			}
		}
		_ = p.reader.Close()
		close(p.done)
	})
	return p.err
}

type supervisionTargetLink struct {
	Client   *targetexec.Client
	TargetID string
	Artifact targetexec.HelperArtifact
	close    func() error
	call     func(context.Context, targetexec.Request) (targetexec.Response, error)
}

func (link *supervisionTargetLink) Call(ctx context.Context, request targetexec.Request) (targetexec.Response, error) {
	if link.call != nil {
		return link.call(ctx, request)
	}
	return link.Client.Call(ctx, request)
}

func (link *supervisionTargetLink) Close() error {
	if link.close != nil {
		return link.close()
	}
	return link.Client.Close()
}

func supervisionHelperDirectory(config Config) string {
	if config.SupervisionTargetsDir != "" {
		return config.SupervisionTargetsDir
	}
	executable, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(executable), "supervision-targets")
}

// A connection is owned by the caller's runtime context, not an HTTP request.
// No reconnect retries a start: the durable control state must be queried first.
func openSupervisionTarget(ctx context.Context, config Config, runner AgentRunner, expectedTargetID string) (*supervisionTargetLink, error) {
	return openSupervisionTargetAt(ctx, config, runner, expectedTargetID, "")
}

// The optional target home is used by isolated deployment/recovery integration
// tests. Product calls use the authenticated target user's own home.
func openSupervisionTargetAt(ctx context.Context, config Config, runner AgentRunner, expectedTargetID, targetHome string) (*supervisionTargetLink, error) {
	return openSupervisionTargetMode(ctx, config, runner, expectedTargetID, targetHome, "supervision-target")
}

func openSupervisionTargetMode(ctx context.Context, config Config, runner AgentRunner, expectedTargetID, targetHome, mode string) (*supervisionTargetLink, error) {
	if mode != "supervision-target" && mode != "workspace-guard" {
		return nil, targetexec.ErrProtocol
	}
	if _, ok := runner.(*windowsAgentRunner); ok {
		return openSupervisionWindowsTargetMode(ctx, config, expectedTargetID, targetHome, mode)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var platform []byte
	var err error
	switch typed := runner.(type) {
	case *wslAgentRunner:
		platform, err = typed.supervisionTargetCommand(probeCtx, supervisionTargetPlatformScript, nil)
	case *sshRunner:
		platform, _, err = typed.client.execCommandSeparate(probeCtx, "/bin/sh -c "+shellQuote(supervisionTargetPlatformScript), 128)
	default:
		return nil, errors.New("当前 Runner 尚未提供跨端目标管道")
	}
	if err != nil {
		return nil, errors.New("无法核对目标执行环境的平台与架构")
	}
	targetOS, arch, ok := strings.Cut(strings.TrimSpace(string(platform)), "-")
	if !ok || targetOS != "linux" || arch != "amd64" && arch != "arm64" {
		return nil, targetexec.ErrProtocol
	}
	artifact, body, err := targetexec.LoadHelper(supervisionHelperDirectory(config), targetOS, arch)
	if err != nil {
		return nil, fmt.Errorf("目标辅助程序构建产物不可用：%w", err)
	}
	var channel *supervisionTargetPipe
	switch typed := runner.(type) {
	case *wslAgentRunner:
		_, err = typed.supervisionTargetCommand(probeCtx, supervisionTargetInstallScript, body, artifact.SHA256, targetHome)
		if err == nil {
			channel, err = typed.supervisionTargetChannel(ctx, artifact.SHA256, targetHome, mode)
		}
	case *sshRunner:
		_, _, err = typed.client.execCommandWithStdinSeparate(probeCtx, "/bin/sh -c "+shellQuote(supervisionTargetInstallScript)+" sh "+shellQuote(artifact.SHA256)+" "+shellQuote(targetHome), body, 128)
		if err == nil {
			channel, err = typed.supervisionTargetChannel(ctx, artifact.SHA256, targetHome, mode)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("目标辅助程序部署或连接失败：%w", err)
	}
	client := targetexec.NewClient(channel)
	hello, err := client.Call(probeCtx, targetexec.Request{ID: "connect", Method: "hello"})
	if err != nil {
		client.Close()
		return nil, err
	}
	if expectedTargetID != "" && hello.TargetID != expectedTargetID {
		client.Close()
		return nil, errors.New("目标持久身份已改变，必须核对原资源后重新建立映射")
	}
	health, err := client.Call(probeCtx, targetexec.Request{ID: "health", Method: "health"})
	var status struct {
		Protocol    int    `json:"protocol"`
		OS          string `json:"os"`
		Arch        string `json:"arch"`
		Containment bool   `json:"processContainment"`
		Guard       bool   `json:"workspaceGuard"`
	}
	if err != nil || json.Unmarshal(health.Data, &status) != nil || status.Protocol != targetexec.ProtocolVersion || (mode == "supervision-target" && (status.OS != artifact.OS || status.Arch != artifact.Arch || !status.Containment)) || (mode == "workspace-guard" && !status.Guard) {
		client.Close()
		return nil, errors.Join(errors.New("目标进程监督能力或实际平台尚未就绪"), err)
	}
	return &supervisionTargetLink{Client: client, TargetID: hello.TargetID, Artifact: artifact}, nil
}

func (r *wslAgentRunner) supervisionTargetCommand(ctx context.Context, script string, input []byte, args ...string) ([]byte, error) {
	if runtime.GOOS != "windows" || r.distro == "" {
		return nil, errors.New("WSL 目标连接缺少 Windows 宿主或发行版")
	}
	wsl, err := wslExePath()
	if err != nil {
		return nil, err
	}
	argv := append([]string{"-d", r.distro, "--exec", "/bin/sh", "-c", script, "sh"}, args...)
	cmd := exec.CommandContext(ctx, wsl, argv...)
	cmd.Stdin = bytes.NewReader(input)
	return runProbeCommand(ctx, cmd)
}

func (r *wslAgentRunner) supervisionTargetChannel(ctx context.Context, digest, targetHome string, modes ...string) (*supervisionTargetPipe, error) {
	mode := "supervision-target"
	if len(modes) == 1 {
		mode = modes[0]
	}
	wsl, err := wslExePath()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(wsl, "-d", r.distro, "--exec", "/bin/sh", "-c", supervisionTargetLaunchScript, "sh", digest, targetHome, mode)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}
	cmd.Stderr = &supervisionSchedulerStderr{}
	configureProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return nil, err
	}
	pipe := &supervisionTargetPipe{reader: stdout, writer: stdin, stop: cmd.Process.Kill, wait: cmd.Wait, done: make(chan struct{})}
	go func() {
		select {
		case <-ctx.Done():
			pipe.Close()
		case <-pipe.done:
		}
	}()
	return pipe, nil
}

func (r *sshRunner) supervisionTargetChannel(ctx context.Context, digest, targetHome string, modes ...string) (*supervisionTargetPipe, error) {
	mode := "supervision-target"
	if len(modes) == 1 {
		mode = modes[0]
	}
	session, err := r.client.newSession(ctx)
	if err != nil {
		return nil, err
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		session.Close()
		return nil, err
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		session.Close()
		return nil, err
	}
	session.Stderr = &supervisionSchedulerStderr{}
	if err := session.Start("/bin/sh -c " + shellQuote(supervisionTargetLaunchScript) + " sh " + shellQuote(digest) + " " + shellQuote(targetHome) + " " + shellQuote(mode)); err != nil {
		session.Close()
		return nil, err
	}
	pipe := &supervisionTargetPipe{reader: io.NopCloser(stdout), writer: stdin, stop: func() error { _ = session.Signal(ssh.SIGTERM); return session.Close() }, wait: func() error { err := session.Wait(); _ = session.Close(); return err }, done: make(chan struct{})}
	go func() {
		select {
		case <-ctx.Done():
			pipe.Close()
		case <-pipe.done:
		}
	}()
	return pipe, nil
}
