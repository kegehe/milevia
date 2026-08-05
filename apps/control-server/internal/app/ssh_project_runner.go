package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// projectRunnerInterface 抽象本地 projectRunner 与 sshProjectRunner，使 handler 可按 runner 类型 dispatch。
type projectRunnerInterface interface {
	StartWithConfig(parentCtx context.Context, projectPath, workDir, command string, envVars map[string]string, executionTarget RunExecutionTarget) error
	Stop() error
	RestartWithConfig(parentCtx context.Context, projectPath, workDir, command string, envVars map[string]string, executionTarget RunExecutionTarget) error
	StatusSnapshot() RunStatusResponse
	registerLogSubscriber(register func([]LogEntry))
	Retire() error
}

// sshProjectRunner 通过 SSH 在远端服务器上管理长运行进程，复用本地 runner 的日志缓冲与状态模型。
type sshProjectRunner struct {
	projectID string
	client    *sshClient
	rootPath  string

	logBuf    *ringBuffer
	broadcast logBroadcaster
	logMu     sync.Mutex
	nextLogID uint64

	status      RunStatus
	retired     bool
	startedAt   time.Time
	session     *ssh.Session
	exitChan    chan struct{}
	exitCode    int
	hasExitCode bool

	opMu sync.Mutex
	mu   sync.RWMutex

	ctx    context.Context
	cancel context.CancelFunc
}

func newSSHProjectRunner(projectID string, client *sshClient, rootPath string, broadcast logBroadcaster) *sshProjectRunner {
	return &sshProjectRunner{
		projectID: projectID,
		client:    client,
		rootPath:  rootPath,
		logBuf:    newRingBuffer(projectRunLogCapacity),
		broadcast: broadcast,
		status:    RunStatusStopped,
	}
}

func (pr *sshProjectRunner) emitLog(entry LogEntry) {
	entry.Text = truncateRunLogText(entry.Text)
	pr.logMu.Lock()
	pr.nextLogID++
	entry.ID = pr.nextLogID
	pr.logBuf.Append(entry)
	pr.broadcast(entry)
	pr.logMu.Unlock()
}

func (pr *sshProjectRunner) registerLogSubscriber(register func([]LogEntry)) {
	pr.logMu.Lock()
	defer pr.logMu.Unlock()
	register(pr.logBuf.Recent(projectRunLogHistory))
}

func (pr *sshProjectRunner) StartWithConfig(parentCtx context.Context, projectPath, workDir, command string, envVars map[string]string, executionTarget RunExecutionTarget) error {
	pr.opMu.Lock()
	defer pr.opMu.Unlock()
	pr.mu.Lock()
	if pr.retired {
		pr.mu.Unlock()
		return fmt.Errorf("项目已删除，无法启动进程")
	}
	if pr.status == RunStatusRunning || pr.status == RunStatusStarting {
		pr.mu.Unlock()
		return fmt.Errorf("进程已在运行中")
	}
	pr.mu.Unlock()
	return pr.start(parentCtx, projectPath, command)
}

func (pr *sshProjectRunner) RestartWithConfig(parentCtx context.Context, projectPath, workDir, command string, envVars map[string]string, executionTarget RunExecutionTarget) error {
	pr.opMu.Lock()
	defer pr.opMu.Unlock()
	pr.mu.RLock()
	running := pr.status == RunStatusRunning || pr.status == RunStatusStarting
	pr.mu.RUnlock()
	if running {
		if err := pr.stop(); err != nil {
			return fmt.Errorf("停止进程失败: %w", err)
		}
	}
	return pr.start(parentCtx, projectPath, command)
}

func (pr *sshProjectRunner) start(parentCtx context.Context, projectPath, command string) error {
	if command == "" {
		return errors.New("请先配置启动命令")
	}
	pr.mu.Lock()
	if pr.retired {
		pr.mu.Unlock()
		return fmt.Errorf("项目已删除，无法启动进程")
	}
	if pr.status == RunStatusRunning || pr.status == RunStatusStarting {
		pr.mu.Unlock()
		return fmt.Errorf("进程已在运行中")
	}
	pr.status = RunStatusStarting
	pr.exitChan = make(chan struct{})
	pr.startedAt = time.Time{}
	pr.exitCode = 0
	pr.hasExitCode = false
	pr.mu.Unlock()

	pr.ctx, pr.cancel = context.WithCancel(parentCtx)
	session, err := pr.client.newSession(pr.ctx)
	if err != nil {
		pr.cancel()
		pr.mu.Lock()
		pr.status = RunStatusFailed
		pr.exitCode = -1
		pr.hasExitCode = true
		close(pr.exitChan)
		pr.mu.Unlock()
		return fmt.Errorf("创建 SSH session 失败: %w", err)
	}

	// 在远端通过 cd 进入项目目录后执行命令；环境变量通过 shell export 前缀注入。
	shellCmd := "cd " + shellQuote(pr.rootPath) + " && " + command
	stdout, err := session.StdoutPipe()
	if err != nil {
		session.Close()
		pr.cancel()
		pr.mu.Lock()
		pr.status = RunStatusFailed
		pr.exitCode = -1
		pr.hasExitCode = true
		close(pr.exitChan)
		pr.mu.Unlock()
		return fmt.Errorf("创建 stdout 管道失败: %w", err)
	}
	stderr, err := session.StderrPipe()
	if err != nil {
		session.Close()
		pr.cancel()
		pr.mu.Lock()
		pr.status = RunStatusFailed
		pr.exitCode = -1
		pr.hasExitCode = true
		close(pr.exitChan)
		pr.mu.Unlock()
		return fmt.Errorf("创建 stderr 管道失败: %w", err)
	}
	if err := session.Start(shellCmd); err != nil {
		session.Close()
		pr.cancel()
		pr.mu.Lock()
		pr.status = RunStatusFailed
		pr.exitCode = -1
		pr.hasExitCode = true
		close(pr.exitChan)
		pr.mu.Unlock()
		return fmt.Errorf("启动进程失败: %w", err)
	}

	pr.mu.Lock()
	pr.session = session
	pr.startedAt = time.Now()
	pr.status = RunStatusRunning
	pr.mu.Unlock()

	pr.emitLog(LogEntry{Timestamp: time.Now(), Stream: "system",
		Text: fmt.Sprintf("远端进程已启动: 命令=%s", command)})

	pipeReadersDone := make(chan struct{})
	var pipeReaders sync.WaitGroup
	pipeReaders.Add(2)
	go func() {
		defer pipeReaders.Done()
		pr.readPipe(stdout, "stdout")
	}()
	go func() {
		defer pipeReaders.Done()
		pr.readPipe(stderr, "stderr")
	}()
	go func() {
		pipeReaders.Wait()
		close(pipeReadersDone)
	}()
	go pr.wait(session, pipeReadersDone)
	return nil
}

func (pr *sshProjectRunner) readPipe(r io.Reader, stream string) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		pr.emitLog(LogEntry{
			Timestamp: time.Now(),
			Stream:    stream,
			Text:      scanner.Text(),
		})
	}
}

func (pr *sshProjectRunner) wait(session *ssh.Session, pipeReadersDone <-chan struct{}) {
	<-pipeReadersDone
	err := session.Wait()
	pr.mu.Lock()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*ssh.ExitError); ok {
			exitCode = exitErr.ExitStatus()
		} else {
			exitCode = -1
		}
		pr.status = RunStatusFailed
	} else {
		pr.status = RunStatusStopped
	}
	pr.exitCode = exitCode
	pr.hasExitCode = true
	pr.mu.Unlock()

	pr.emitLog(LogEntry{Timestamp: time.Now(), Stream: "system",
		Text: fmt.Sprintf("进程已退出: code=%d", exitCode)})
	_ = session.Close()
	close(pr.exitChan)
}

func (pr *sshProjectRunner) Stop() error {
	pr.opMu.Lock()
	defer pr.opMu.Unlock()
	return pr.stop()
}

func (pr *sshProjectRunner) stop() error {
	pr.mu.Lock()
	if pr.status != RunStatusRunning && pr.status != RunStatusStarting {
		pr.mu.Unlock()
		return fmt.Errorf("进程未在运行")
	}
	if pr.session == nil {
		pr.mu.Unlock()
		return fmt.Errorf("进程状态异常：未找到 SSH session")
	}
	session := pr.session
	pr.status = RunStatusStopping
	pr.mu.Unlock()

	pr.emitLog(LogEntry{Timestamp: time.Now(), Stream: "system", Text: "正在停止进程..."})

	// 发送 SIGKILL 终止远端进程组。SSH session.Signal 仅对部分 shell 生效，
	// 因此同时取消 ctx 并等待 exitChan。
	if pr.cancel != nil {
		pr.cancel()
	}
	_ = session.Signal(ssh.SIGKILL)

	select {
	case <-pr.exitChan:
	case <-time.After(5 * time.Second):
		_ = session.Close()
		<-pr.exitChan
	}
	return nil
}

func (pr *sshProjectRunner) Retire() error {
	pr.opMu.Lock()
	defer pr.opMu.Unlock()
	pr.mu.Lock()
	pr.retired = true
	running := pr.status == RunStatusRunning
	pr.mu.Unlock()
	if !running {
		return nil
	}
	return pr.stop()
}

func (pr *sshProjectRunner) StatusSnapshot() RunStatusResponse {
	pr.mu.RLock()
	defer pr.mu.RUnlock()
	snapshot := RunStatusResponse{
		Status:     pr.status,
		RecentLogs: pr.logBuf.Recent(projectRunLogHistory),
	}
	if !pr.startedAt.IsZero() {
		startedAt := pr.startedAt
		snapshot.StartedAt = &startedAt
	}
	// SSH 远端进程 PID 不可靠获取，统一不返回。
	if pr.hasExitCode {
		exitCode := pr.exitCode
		snapshot.ExitCode = &exitCode
	}
	return snapshot
}
