package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
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
	LightStatusSnapshot() RunStatusResponse
	ClearLogs()
	registerLogSubscriber(register func([]LogEntry))
	setStatusListener(listener statusBroadcaster)
	Retire() error
}

// sshProjectRunner 通过 SSH 在远端服务器上管理长运行进程，复用本地 runner 的日志缓冲与状态模型。
type sshProjectRunner struct {
	projectID string
	client    *sshClient
	rootPath  string
	// remotePIDFile 记录当前运行在远端记录进程组 leader PID 的 pidfile 路径。每次
	// start 都按代次生成唯一路径（见 remoteRunPIDFile），旧运行收尾只清理自己的文件。
	// OpenSSH 会把 exec 的 shell 放到新会话/进程组中（shell 即 leader，$$ 等于 PGID），
	// 启动时写入 pidfile，停止时用 kill -- -PGID 终止整棵进程树。留空则退化为仅向
	// session 发 SIGKILL。
	remotePIDFile string

	logBuf    *ringBuffer
	broadcast logBroadcaster
	logMu     sync.Mutex
	nextLogID uint64

	statusBroadcast statusBroadcaster

	status      RunStatus
	retired     bool
	startedAt   time.Time
	session     *ssh.Session
	exitChan    chan struct{}
	exitCode    int
	hasExitCode bool
	// gen 记录当前 session 的代次：每次 start 自增，wait goroutine 捕获启动时的代次，
	// 收尾时若发现已被新一轮 start 覆盖（stop 超时后重新启动），则放弃改动状态机。
	gen uint64

	workDir string
	envVars map[string]string
	command string

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

// remoteRunPIDFile 返回远端记录运行进程组 leader PID 的 pidfile 路径。
// projectID 是 UUID；gen 区分同一项目的不同运行，避免旧运行收尾时误删新运行的文件。
func remoteRunPIDFile(projectID string, gen uint64) string {
	return fmt.Sprintf("/tmp/milevia-run-%s-%d.pid", projectID, gen)
}

// prefixRemotePIDCapture 在远端 shell 命令最前面注入 `$$` 到 pidfile 的写入。
// OpenSSH 将 exec 的命令作为新会话/进程组运行，`$$` 即进程组 leader 的 PID，
// stop() 用它 kill -- -PGID 终止整棵进程树。pidfile 为空时原样返回命令。
func prefixRemotePIDCapture(pidfile, shellCmd string) string {
	if pidfile == "" {
		return shellCmd
	}
	return "echo \"$$\" > " + shellQuote(pidfile) + " 2>/dev/null; " + shellCmd
}

// remoteProcessGroupKillCommand 构造终止远端进程组的 shell 命令：读取 pidfile 中的
// leader PID 并对负 PID 发 SIGKILL（终止整个进程组）。先校验 PID 是纯数字且 >1
// （pid=0 会把信号发给 kill 命令自身的进程组，pid=1 的负号形式 `kill -9 -1` 会发给
// 远端所有进程，二者都可能来自被篡改的 pidfile；正常 $$ 恒 ≥2）。进程组不存在时
// 静默忽略，最后移除 pidfile。用 `kill -9 -PID`（信号已显式给出后，负参数按 POSIX
// 即为进程组 ID），避免依赖非标准的 `--` 选项。
func remoteProcessGroupKillCommand(pidfile string) string {
	if pidfile == "" {
		return ""
	}
	pidfile = shellQuote(pidfile)
	return fmt.Sprintf(`if [ -f %s ]; then pid=$(cat %s 2>/dev/null || true); case "$pid" in ''|*[!0-9]*) : ;; *) [ "$pid" -gt 1 ] 2>/dev/null && kill -9 -"$pid" 2>/dev/null || true ;; esac; rm -f %s; fi`, pidfile, pidfile, pidfile)
}

// buildSSHRunShell 构造在远端执行的 shell 命令：cd 到目标目录、逐个 export 环境变量，
// 最后执行命令。每个 export 都必须是独立命令并以 "&& " 结尾：否则 shell 会把后续命令
// 当作 export 的参数（合法标识符被静默吞掉，含特殊字符时报 not a valid identifier），
// 真实命令永远不执行。与 git_ssh.buildGitShell 的修正保持一致。
func buildSSHRunShell(cdTarget string, envVars map[string]string, command string) string {
	shellCmd := "cd " + shellQuote(cdTarget) + " && "
	for k, v := range envVars {
		shellCmd += "export " + k + "=" + shellQuote(v) + " && "
	}
	return shellCmd + command
}

// setStatusListener 注册进程状态变更回调（由 Server 注入）。须在启动之前设置。
func (pr *sshProjectRunner) setStatusListener(listener statusBroadcaster) {
	pr.statusBroadcast = listener
}

// emitStatus 在状态切分归位后触发状态广播。调用方须已释放 pr.mu。
func (pr *sshProjectRunner) emitStatus(event RunStatusEvent) {
	if pr.statusBroadcast != nil {
		pr.statusBroadcast(event)
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
	pr.workDir = workDir
	pr.command = command
	pr.envVars = envVars
	pr.mu.Unlock()
	return pr.start(parentCtx, projectPath)
}

func (pr *sshProjectRunner) RestartWithConfig(parentCtx context.Context, projectPath, workDir, command string, envVars map[string]string, executionTarget RunExecutionTarget) error {
	pr.opMu.Lock()
	defer pr.opMu.Unlock()
	pr.mu.Lock()
	if pr.retired {
		pr.mu.Unlock()
		return fmt.Errorf("项目已删除，无法启动进程")
	}
	running := pr.status == RunStatusRunning || pr.status == RunStatusStarting
	pr.workDir = workDir
	pr.command = command
	pr.envVars = envVars
	pr.mu.Unlock()
	if running {
		if err := pr.stop(); err != nil {
			return fmt.Errorf("停止进程失败: %w", err)
		}
	}
	return pr.start(parentCtx, projectPath)
}

func (pr *sshProjectRunner) start(parentCtx context.Context, projectPath string) error {
	pr.mu.Lock()
	if pr.retired {
		pr.mu.Unlock()
		return fmt.Errorf("项目已删除，无法启动进程")
	}
	if pr.status == RunStatusRunning || pr.status == RunStatusStarting {
		pr.mu.Unlock()
		return fmt.Errorf("进程已在运行中")
	}
	command := pr.command
	workDir := pr.workDir
	envVars := pr.envVars
	if command == "" {
		pr.mu.Unlock()
		return errors.New("请先配置启动命令")
	}
	pr.status = RunStatusStarting
	pr.exitChan = make(chan struct{})
	pr.gen++
	gen := pr.gen
	pidfile := remoteRunPIDFile(pr.projectID, gen)
	pr.remotePIDFile = pidfile
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
		pr.emitStatus(RunStatusEvent{ProjectID: pr.projectID, Status: RunStatusFailed})
		return fmt.Errorf("创建 SSH session 失败: %w", err)
	}

	// cd 到项目目录（或配置的子工作目录），注入环境变量，然后执行命令。
	cdTarget := pr.rootPath
	if workDir != "" {
		cleaned := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(workDir)), "/")
		if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
			pr.cancel()
			pr.mu.Lock()
			pr.status = RunStatusFailed
			pr.exitCode = -1
			pr.hasExitCode = true
			close(pr.exitChan)
			pr.mu.Unlock()
			pr.emitStatus(RunStatusEvent{ProjectID: pr.projectID, Status: RunStatusFailed})
			return errors.New("工作目录不能超出项目路径")
		}
		cdTarget = pr.rootPath + "/" + cleaned
	}
	shellCmd := prefixRemotePIDCapture(pidfile, buildSSHRunShell(cdTarget, envVars, command))
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
		pr.emitStatus(RunStatusEvent{ProjectID: pr.projectID, Status: RunStatusFailed})
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
		pr.emitStatus(RunStatusEvent{ProjectID: pr.projectID, Status: RunStatusFailed})
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
		pr.emitStatus(RunStatusEvent{ProjectID: pr.projectID, Status: RunStatusFailed})
		return fmt.Errorf("启动进程失败: %w", err)
	}

	pr.mu.Lock()
	pr.session = session
	pr.startedAt = time.Now()
	pr.status = RunStatusRunning
	startedAt := pr.startedAt
	pr.mu.Unlock()

	pr.emitStatus(RunStatusEvent{ProjectID: pr.projectID, Status: RunStatusRunning, StartedAt: &startedAt})

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
	go pr.wait(session, pipeReadersDone, gen)
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

func (pr *sshProjectRunner) wait(session *ssh.Session, pipeReadersDone <-chan struct{}, gen uint64) {
	<-pipeReadersDone
	err := session.Wait()
	// 尽力清理本代次 pidfile（异步，不阻塞状态切换与 stop() 返回）。路径按 gen 唯一，
	// 只删自己的文件，不影响后续运行；连接异常时静默失败，残留文件无害。
	pidfile := remoteRunPIDFile(pr.projectID, gen)
	go func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, _ = pr.client.execCommand(cleanupCtx, "rm -f -- "+shellQuote(pidfile))
	}()

	pr.mu.Lock()
	// stop() 超时返回后新一轮 start 会创建新的 exitChan/状态；旧 session 收尾信息已
	// 过期，不得再覆盖新运行的状态或关闭其 exitChan。
	if pr.gen != gen {
		pr.mu.Unlock()
		_ = session.Close()
		return
	}
	exitCode := 0
	// 与本地 runner 的 wait 一致：停止流程中（status==Stopping）即使远端 session.Wait()
	// 返回非零退出（cancel+SIGKILL 触发），也归为 Stopped 而非 Failed。
	if err != nil && pr.status != RunStatusStopping {
		if exitErr, ok := err.(*ssh.ExitError); ok {
			exitCode = exitErr.ExitStatus()
		} else {
			exitCode = -1
		}
		pr.status = RunStatusFailed
	} else {
		if exitErr, ok := err.(*ssh.ExitError); ok {
			exitCode = exitErr.ExitStatus()
		}
		pr.status = RunStatusStopped
	}
	pr.exitCode = exitCode
	pr.hasExitCode = true
	status := pr.status
	pr.mu.Unlock()

	// 先发事件、后关 exitChan：保证"进程已退出"在 stop() 放行新一轮 start 之前送达，
	// 避免日志/状态顺序颠倒。
	pr.emitStatus(RunStatusEvent{ProjectID: pr.projectID, Status: status})

	pr.emitLog(LogEntry{Timestamp: time.Now(), Stream: "system",
		Text: fmt.Sprintf("进程已退出: code=%d", exitCode)})
	_ = session.Close()

	// 关闭必须在持锁时进行：若先解锁再关，新一轮 start 可能已替换 pr.exitChan，导致
	// 关闭新运行的通道（其 wait goroutine 再 close 会 panic）。持锁 + 代次校验确保只
	// 关本代次的通道；若关闭前已被覆盖（自然退出且在事件窗口期立即重启），则旧代次的
	// exitChan 已无人等待，跳过即可。
	pr.mu.Lock()
	if pr.gen == gen {
		close(pr.exitChan)
	}
	pr.mu.Unlock()
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
	pidfile := pr.remotePIDFile
	pr.mu.Unlock()

	pr.emitStatus(RunStatusEvent{ProjectID: pr.projectID, Status: RunStatusStopping})

	pr.emitLog(LogEntry{Timestamp: time.Now(), Stream: "system", Text: "正在停止进程..."})

	// 取消 ctx，并终止远端整个进程组（kill -- -PGID），而非仅向 session 的 shell
	// 进程发 SIGKILL：npm run dev 之类的命令会留下子进程，若只杀 shell，子进程会
	// 继续占用远端 stdout 管道，导致 SSH channel 永不关闭、session.Wait 阻塞、
	// 停止请求卡住。SSH session.Signal 仅对部分 shell 生效，因此仍补发一次。
	if pr.cancel != nil {
		pr.cancel()
	}
	if killCmd := remoteProcessGroupKillCommand(pidfile); killCmd != "" {
		killCtx, killCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, _ = pr.client.execCommand(killCtx, killCmd)
		killCancel()
	}
	_ = session.Signal(ssh.SIGKILL)

	select {
	case <-pr.exitChan:
	case <-time.After(5 * time.Second):
		_ = session.Close()
		// 极端情况（连接半开、远端无法终止）下不再无限等待，wait goroutine 最终收尾。
		select {
		case <-pr.exitChan:
		case <-time.After(5 * time.Second):
		}
	}
	return nil
}

func (pr *sshProjectRunner) Retire() error {
	pr.opMu.Lock()
	defer pr.opMu.Unlock()
	pr.mu.Lock()
	pr.retired = true
	running := pr.status == RunStatusRunning || pr.status == RunStatusStarting
	pr.mu.Unlock()
	if !running {
		return nil
	}
	return pr.stop()
}

// ClearLogs removes the retained output and acknowledges a terminal failure.
func (pr *sshProjectRunner) ClearLogs() {
	pr.opMu.Lock()
	defer pr.opMu.Unlock()

	pr.logMu.Lock()
	pr.logBuf.Clear()
	pr.logMu.Unlock()

	pr.mu.Lock()
	failed := pr.status == RunStatusFailed
	if failed {
		pr.status = RunStatusStopped
		pr.startedAt = time.Time{}
		pr.exitCode = 0
		pr.hasExitCode = false
	}
	pr.mu.Unlock()
	if failed {
		pr.emitStatus(RunStatusEvent{ProjectID: pr.projectID, Status: RunStatusStopped})
	}
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

// LightStatusSnapshot 返回精简状态快照（线程安全），仅供批量总览端点使用：
// 不拷贝环形日志缓冲区。SSH 远端无本地 PID，恒不填充 PID。
func (pr *sshProjectRunner) LightStatusSnapshot() RunStatusResponse {
	pr.mu.RLock()
	defer pr.mu.RUnlock()
	snapshot := RunStatusResponse{
		Status: pr.status,
	}
	if !pr.startedAt.IsZero() {
		startedAt := pr.startedAt
		snapshot.StartedAt = &startedAt
	}
	if pr.hasExitCode {
		exitCode := pr.exitCode
		snapshot.ExitCode = &exitCode
	}
	return snapshot
}
