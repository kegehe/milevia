package app

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
)

// Model catalogs require bounded, separate streams. A shell adapter without
// this optional capability falls back to manual model input.
type boundedCrossShellRunner interface {
	crossShellRunner
	crossRunLimited(ctx context.Context, script string, stdoutLimit int) (string, error)
}

func (r *sshRunner) crossRunLimited(ctx context.Context, script string, stdoutLimit int) (string, error) {
	if stdoutLimit <= 0 {
		return "", errors.New("CLI 探测输出限制必须大于零")
	}
	stdout, _, err := r.client.execCommandSeparate(ctx, crossManagedPathPrefix()+"\n"+script, stdoutLimit)
	if len(stdout) > stdoutLimit {
		return "", errors.New("CLI 探测输出超过大小限制")
	}
	return string(stdout), err
}

func (r *wslAgentRunner) crossRunLimited(ctx context.Context, script string, stdoutLimit int) (string, error) {
	if stdoutLimit <= 0 {
		return "", errors.New("CLI 探测输出限制必须大于零")
	}
	wslPath, err := wslExePath()
	if err != nil {
		return "", err
	}
	prefix, err := r.wslPathPrefixContext(ctx)
	if err != nil {
		return "", err
	}
	// Keep the same distro, native PATH and argument encoding as crossRun.
	cmd := exec.CommandContext(ctx, wslPath, "-d", r.distro, "--", "sh", "-c", wslEncodeArg(prefix+"\n"+script))
	configureProcessGroup(cmd)
	cmd.WaitDelay = probePipeCloseGrace
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &cappedWriter{buf: &stdout, limit: stdoutLimit + 1}
	cmd.Stderr = &cappedWriter{buf: &stderr, limit: 64 << 10}
	if err := cmd.Run(); err != nil {
		if cmd.Process != nil {
			noteWorkspaceWSLExit(r.distro, ctx, err, false)
		}
		return "", err
	}
	if stdout.Len() > stdoutLimit {
		return "", errors.New("CLI 探测输出超过大小限制")
	}
	return stdout.String(), nil
}

var _ boundedCrossShellRunner = (*sshRunner)(nil)
var _ boundedCrossShellRunner = (*wslAgentRunner)(nil)
