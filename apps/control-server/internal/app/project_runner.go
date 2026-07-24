package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// RunStatus 表示项目运行进程的状态。
type RunStatus string

const (
	RunStatusStopped          RunStatus = "stopped"
	RunStatusStarting         RunStatus = "starting"
	RunStatusRunning          RunStatus = "running"
	RunStatusStopping         RunStatus = "stopping"
	RunStatusFailed           RunStatus = "failed"
	projectRunLogCapacity               = 2_000
	projectRunLogHistory                = 200
	projectRunLogMaxTextBytes           = 16 * 1024
)

// LogEntry 表示一行日志。
type LogEntry struct {
	ID        uint64    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Stream    string    `json:"stream"` // "stdout" | "stderr" | "system"
	Text      string    `json:"text"`
}

// ringBuffer 日志环形缓冲区，线程安全。
type ringBuffer struct {
	entries []LogEntry
	head    int
	size    int
	cap     int
	mu      sync.RWMutex
}

func newRingBuffer(cap int) *ringBuffer {
	return &ringBuffer{
		entries: make([]LogEntry, cap),
		cap:     cap,
	}
}

func (rb *ringBuffer) Append(e LogEntry) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	rb.entries[rb.head] = e
	rb.head = (rb.head + 1) % rb.cap
	if rb.size < rb.cap {
		rb.size++
	}
}

// Recent 返回最近 n 条日志（深拷贝，线程安全）。
func (rb *ringBuffer) Recent(n int) []LogEntry {
	rb.mu.RLock()
	defer rb.mu.RUnlock()
	if n > rb.size {
		n = rb.size
	}
	result := make([]LogEntry, n)
	start := (rb.head - n + rb.cap) % rb.cap
	for i := 0; i < n; i++ {
		result[i] = rb.entries[(start+i)%rb.cap]
	}
	return result
}

// logBroadcaster 是广播日志的回调，由 Server 注入。
type logBroadcaster func(entry LogEntry)

// projectRunner 管理单个项目的运行进程。
type projectRunner struct {
	projectID string
	workDir   string
	command   string
	envVars   map[string]string

	cmd    *exec.Cmd
	ctx    context.Context
	cancel context.CancelFunc

	logBuf    *ringBuffer
	broadcast logBroadcaster
	logMu     sync.Mutex
	nextLogID uint64

	status      RunStatus
	retired     bool
	startedAt   time.Time
	pid         int
	exitCode    int
	hasExitCode bool
	exitChan    chan struct{} // 进程退出时 close

	opMu sync.Mutex
	mu   sync.RWMutex
}

// newProjectRunner 创建一个未启动的 projectRunner。
func newProjectRunner(projectID, workDir, command string, envVars map[string]string, broadcast logBroadcaster) *projectRunner {
	return &projectRunner{
		projectID: projectID,
		workDir:   workDir,
		command:   command,
		envVars:   envVars,
		logBuf:    newRingBuffer(projectRunLogCapacity),
		broadcast: broadcast,
		status:    RunStatusStopped,
	}
}

// Start 启动进程。parentCtx 是 Server 的 runtimeCtx，projectPath 是项目根路径。
func (pr *projectRunner) Start(parentCtx context.Context, projectPath string) error {
	pr.opMu.Lock()
	defer pr.opMu.Unlock()
	return pr.start(parentCtx, projectPath)
}

func (pr *projectRunner) StartWithConfig(parentCtx context.Context, projectPath, workDir, command string, envVars map[string]string) error {
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
	pr.workDir = workDir
	pr.command = command
	pr.envVars = envVars
	pr.mu.Unlock()
	return pr.start(parentCtx, projectPath)
}

func (pr *projectRunner) start(parentCtx context.Context, projectPath string) error {
	pr.mu.Lock()
	if pr.retired {
		pr.mu.Unlock()
		return fmt.Errorf("项目已删除，无法启动进程")
	}
	if pr.status == RunStatusRunning || pr.status == RunStatusStarting {
		pr.mu.Unlock()
		return fmt.Errorf("进程已在运行中")
	}

	workDir, err := resolveProjectRunWorkDir(projectPath, pr.workDir)
	if err != nil {
		pr.mu.Unlock()
		return err
	}

	pr.status = RunStatusStarting
	pr.exitChan = make(chan struct{})
	pr.startedAt = time.Time{}
	pr.pid = 0
	pr.exitCode = 0
	pr.hasExitCode = false
	pr.mu.Unlock()

	pr.ctx, pr.cancel = context.WithCancel(parentCtx)
	pr.cmd = exec.CommandContext(pr.ctx, "sh", "-c", pr.command)
	pr.cmd.Dir = workDir

	// 设置环境变量
	pr.cmd.Env = os.Environ()
	for k, v := range pr.envVars {
		pr.cmd.Env = append(pr.cmd.Env, k+"="+v)
	}

	// 配置进程组，方便终止整个进程树
	configureProcessGroup(pr.cmd)

	stdout, err := pr.cmd.StdoutPipe()
	if err != nil {
		pr.mu.Lock()
		pr.status = RunStatusFailed
		pr.exitCode = -1
		pr.hasExitCode = true
		close(pr.exitChan)
		pr.mu.Unlock()
		return fmt.Errorf("创建 stdout 管道失败: %w", err)
	}
	stderr, err := pr.cmd.StderrPipe()
	if err != nil {
		pr.mu.Lock()
		pr.status = RunStatusFailed
		pr.exitCode = -1
		pr.hasExitCode = true
		close(pr.exitChan)
		pr.mu.Unlock()
		return fmt.Errorf("创建 stderr 管道失败: %w", err)
	}

	if err := pr.cmd.Start(); err != nil {
		pr.mu.Lock()
		pr.status = RunStatusFailed
		pr.exitCode = -1
		pr.hasExitCode = true
		close(pr.exitChan)
		pr.mu.Unlock()
		return fmt.Errorf("启动进程失败: %w", err)
	}

	pr.mu.Lock()
	pr.pid = pr.cmd.Process.Pid
	pr.startedAt = time.Now()
	pr.status = RunStatusRunning
	pr.mu.Unlock()

	pr.emitLog(LogEntry{Timestamp: time.Now(), Stream: "system",
		Text: fmt.Sprintf("进程已启动: pid=%d, 命令=%s", pr.pid, pr.command)})

	go pr.readPipe(stdout, "stdout")
	go pr.readPipe(stderr, "stderr")
	go pr.wait()

	return nil
}

func resolveProjectRunWorkDir(projectPath, configuredWorkDir string) (string, error) {
	workDir := projectPath
	if configuredWorkDir != "" {
		workDir = filepath.Join(projectPath, configuredWorkDir)
	}
	if !isPathWithin(projectPath, workDir) {
		return "", errors.New("工作目录不能超出项目路径")
	}

	resolvedProjectPath, err := filepath.EvalSymlinks(projectPath)
	if err != nil {
		return "", fmt.Errorf("解析项目路径失败: %w", err)
	}
	resolvedWorkDir, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		return "", fmt.Errorf("解析工作目录失败: %w", err)
	}
	if !isPathWithin(resolvedProjectPath, resolvedWorkDir) {
		return "", errors.New("工作目录不能超出项目路径")
	}
	return resolvedWorkDir, nil
}

func isPathWithin(basePath, candidatePath string) bool {
	rel, err := filepath.Rel(basePath, candidatePath)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (pr *projectRunner) emitLog(entry LogEntry) {
	entry.Text = truncateRunLogText(entry.Text)
	pr.logMu.Lock()
	pr.nextLogID++
	entry.ID = pr.nextLogID
	pr.logBuf.Append(entry)
	pr.broadcast(entry)
	pr.logMu.Unlock()
}

// registerLogSubscriber makes history replay and live delivery share one log order.
func (pr *projectRunner) registerLogSubscriber(register func([]LogEntry)) {
	pr.logMu.Lock()
	defer pr.logMu.Unlock()
	register(pr.logBuf.Recent(projectRunLogHistory))
}

func truncateRunLogText(text string) string {
	if len(text) <= projectRunLogMaxTextBytes {
		return text
	}
	const suffix = "... [truncated]"
	limit := projectRunLogMaxTextBytes - len(suffix)
	for limit > 0 && (text[limit]&0xc0) == 0x80 {
		limit--
	}
	return text[:limit] + suffix
}

func (pr *projectRunner) readPipe(r io.Reader, stream string) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		entry := LogEntry{
			Timestamp: time.Now(),
			Stream:    stream,
			Text:      scanner.Text(),
		}
		pr.emitLog(entry)
	}
}

func (pr *projectRunner) wait() {
	err := pr.cmd.Wait()
	pr.mu.Lock()
	exitCode := 0
	if err != nil && pr.status != RunStatusStopping {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
		pr.status = RunStatusFailed
	} else {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
		pr.status = RunStatusStopped
	}
	pr.exitCode = exitCode
	pr.hasExitCode = true
	pr.mu.Unlock()

	pr.emitLog(LogEntry{Timestamp: time.Now(), Stream: "system",
		Text: fmt.Sprintf("进程已退出: code=%d", exitCode)})
	close(pr.exitChan)
}

// Stop 停止进程。先发送 SIGTERM，5s 后仍未退出则 SIGKILL。
func (pr *projectRunner) Stop() error {
	pr.opMu.Lock()
	defer pr.opMu.Unlock()
	return pr.stop()
}

func (pr *projectRunner) stop() error {
	pr.mu.Lock()
	if pr.status != RunStatusRunning {
		pr.mu.Unlock()
		return fmt.Errorf("进程未在运行")
	}
	if pr.cmd == nil {
		pr.mu.Unlock()
		return fmt.Errorf("进程状态异常：未找到命令实例")
	}
	pr.status = RunStatusStopping
	pr.mu.Unlock()

	pr.emitLog(LogEntry{Timestamp: time.Now(), Stream: "system", Text: "正在停止进程..."})

	terminateProcessGroup(pr.cmd)

	select {
	case <-pr.exitChan:
		// 正常退出
	case <-time.After(5 * time.Second):
		forceTerminateProcessGroup(pr.cmd)
		<-pr.exitChan
	}
	return nil
}

// Restart 重启进程：先停止，等待退出，再启动。
func (pr *projectRunner) Restart(parentCtx context.Context, projectPath string) error {
	pr.opMu.Lock()
	defer pr.opMu.Unlock()
	return pr.restart(parentCtx, projectPath)
}

func (pr *projectRunner) RestartWithConfig(parentCtx context.Context, projectPath, workDir, command string, envVars map[string]string) error {
	pr.opMu.Lock()
	defer pr.opMu.Unlock()
	pr.mu.Lock()
	if pr.retired {
		pr.mu.Unlock()
		return fmt.Errorf("项目已删除，无法启动进程")
	}
	pr.workDir = workDir
	pr.command = command
	pr.envVars = envVars
	pr.mu.Unlock()
	return pr.restart(parentCtx, projectPath)
}

func (pr *projectRunner) restart(parentCtx context.Context, projectPath string) error {
	pr.mu.RLock()
	running := pr.status == RunStatusRunning || pr.status == RunStatusStarting
	pr.mu.RUnlock()

	if running {
		if err := pr.stop(); err != nil {
			return fmt.Errorf("停止进程失败: %w", err)
		}
	}
	return pr.start(parentCtx, projectPath)
}

// Retire 永久禁止后续启动，并停止当前运行进程。
func (pr *projectRunner) Retire() error {
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

// StatusSnapshot 返回当前状态快照（线程安全）。
func (pr *projectRunner) StatusSnapshot() RunStatusResponse {
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
	if pr.pid > 0 && (pr.status == RunStatusRunning || pr.status == RunStatusStopping) {
		pid := pr.pid
		snapshot.PID = &pid
	}
	if pr.hasExitCode {
		exitCode := pr.exitCode
		snapshot.ExitCode = &exitCode
	}
	return snapshot
}
