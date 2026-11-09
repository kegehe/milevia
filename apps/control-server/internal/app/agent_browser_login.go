package app

import (
	"context"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Own the long-running login independently of a short HTTP request. One
// controller per CLI keeps repeated clicks in the same authorization flow.
type browserLogin struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	url     string
	done    chan struct{}
	ready   chan struct{}
	release func()
	closed  bool
}

func cursorAuthURL(output string) string {
	for _, field := range strings.Fields(cleanAuthOutput(output)) {
		candidate := strings.Trim(field, "\"'()<>,;.")
		u, err := url.Parse(candidate)
		if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || len(candidate) > 8192 {
			continue
		}
		host := strings.ToLower(u.Hostname())
		if (host == "cursor.com" || strings.HasSuffix(host, ".cursor.com")) && (u.Port() == "" || u.Port() == "443") {
			return candidate
		}
	}
	return ""
}

type browserLoginOutput struct {
	owner *browserLogin
	tail  string
}

func (w *browserLoginOutput) Write(p []byte) (int, error) {
	w.owner.mu.Lock()
	defer w.owner.mu.Unlock()
	w.tail += string(p)
	if len(w.tail) > 16384 {
		w.tail = w.tail[len(w.tail)-16384:]
	}
	if w.owner.url == "" {
		// Process completed lines only: never expose a partially received URL.
		if i := strings.LastIndexAny(w.tail, "\r\n"); i >= 0 {
			if u := cursorAuthURL(w.tail[:i]); u != "" {
				w.owner.url = u
				close(w.owner.ready)
			}
			w.tail = w.tail[i+1:]
		}
	}
	return len(p), nil
}

func (b *browserLogin) start(parent context.Context, makeCommand func(context.Context) *exec.Cmd) (agentLoginInfo, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return agentLoginInfo{}, errors.New("服务正在关闭，请稍后重试")
	}
	if b.cmd == nil {
		dir, err := os.MkdirTemp("", "milevia-cursor-login-")
		if err != nil {
			b.mu.Unlock()
			return agentLoginInfo{}, errors.New("无法创建授权目录")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		cmd := makeCommand(ctx)
		cmd.Dir = dir
		configureProcessGroup(cmd)
		cmd.Cancel = func() error { forceTerminateProcessGroup(cmd); return nil }
		cmd.WaitDelay = probePipeCloseGrace
		b.done, b.ready = make(chan struct{}), make(chan struct{})
		b.url = ""
		cmd.Stdout, cmd.Stderr = &browserLoginOutput{owner: b}, &browserLoginOutput{owner: b}
		if err := cmd.Start(); err != nil {
			cancel()
			_ = os.RemoveAll(dir)
			b.mu.Unlock()
			return agentLoginInfo{}, errors.New("无法启动 Cursor 授权，请在目标环境运行 agent login")
		}
		b.cmd = cmd
		release := containAgentProcess(cmd)
		b.release = release
		done := b.done
		go func() {
			_ = cmd.Wait()
			cancel()
			forceTerminateProcessGroup(cmd)
			release()
			_ = os.RemoveAll(dir)
			b.mu.Lock()
			if b.cmd == cmd {
				b.cmd, b.url, b.release = nil, "", nil
			}
			close(done)
			b.mu.Unlock()
		}()
	}
	ready, done := b.ready, b.done
	b.mu.Unlock()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case <-ready:
	case <-done:
	case <-parent.Done():
	case <-timer.C:
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cmd == nil {
		return agentLoginInfo{Message: "登录进程已结束，请检查登录状态；未完成授权时可重新发起。"}, nil
	}
	if b.url != "" {
		return agentLoginInfo{AuthURL: b.url, Message: "已发起 Cursor 登录，请打开授权链接完成授权，然后检查登录状态。"}, nil
	}
	return agentLoginInfo{Message: "Cursor 登录流程正在等待授权链接，可稍后再次点「发起登录」读取同一次流程的链接；也可在目标环境运行 agent login。"}, nil
}

func (b *browserLogin) stop() {
	b.mu.Lock()
	b.closed = true
	cmd, release, done := b.cmd, b.release, b.done
	b.mu.Unlock()
	if cmd == nil {
		return
	}
	forceTerminateProcessGroup(cmd)
	if release != nil {
		release()
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
}
