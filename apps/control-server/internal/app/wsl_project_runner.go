package app

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// wslProjectRunner 在 Windows 控制服务上经 wsl.exe 在 WSL 内运行项目命令。
//
// 状态机、日志环形缓冲与广播都沿用 projectRunner / sshProjectRunner 的模型，差别只在
// "进程在哪、怎么停"：
//
//   - 进程形态：`wsl.exe -d <distro> -- sh -c <脚本>`。脚本里 cd 到 WSL 内工作目录、
//     导出环境变量，再把用户命令按 shell 命令行原样接在后面——与本地 runner 的
//     `sh -c <command>` 语义一致（用户命令里的 &&、管道都仍由 shell 解释）。
//   - 路径：WSL 项目的项目路径是 `\\wsl$\<distro>\...` UNC，工作目录必须换成 WSL 内
//     Linux 路径后再交给 cd；Windows 侧的 filepath 不认这种路径，故路径运算一律用
//     path 包。
//   - 停止：杀掉 wsl.exe 不证明 Linux 侧进程已退出（wsl.exe 只是 Windows 客户端）。
//     因此启动脚本先把 shell 自身 PID 写进 WSL 内 pidfile，停止时再开一个 wsl.exe
//     对进程组发 SIGKILL（与 sshProjectRunner 的 pidfile 手法一致）。
type wslProjectRunner struct {
	projectID string
	distro    string

	logBuf    *ringBuffer
	broadcast logBroadcaster
	logMu     sync.Mutex
	nextLogID uint64

	statusBroadcast    statusBroadcaster
	workspaceAdmission func(string) (func(), error)

	status      RunStatus
	retired     bool
	startedAt   time.Time
	exitChan    chan struct{}
	exitCode    int
	hasExitCode bool
	// gen 记录当前运行代次：每次 start 自增，wait goroutine 收获启动时的代次，
	// 旧运行的收尾不得覆盖新一轮的状态。
	gen uint64

	cmd    *exec.Cmd
	ctx    context.Context
	cancel context.CancelFunc

	workDir       string
	workspacePath string
	envVars       map[string]string
	command       string

	opMu sync.Mutex
	mu   sync.RWMutex
}

func newWSLProjectRunner(projectID, distro string, broadcast logBroadcaster) *wslProjectRunner {
	return &wslProjectRunner{
		projectID: projectID,
		distro:    distro,
		logBuf:    newRingBuffer(projectRunLogCapacity),
		broadcast: broadcast,
		status:    RunStatusStopped,
	}
}

// wslProjectRunPIDFile 返回 WSL 内记录运行进程组 leader PID 的 pidfile 路径。
// 同一项目的不同工作区（worktree）各自一个 runner，因此路径要带上工作区标识，
// 否则停止一个会把另一个的 pidfile 一起删掉。
func wslProjectRunPIDFile(projectID, workspacePath string, gen uint64) string {
	sum := sha256.Sum256([]byte(workspacePath))
	return fmt.Sprintf("/tmp/milevia-run-%s-%s-%d.pid", projectID, hex.EncodeToString(sum[:4]), gen)
}

// wslProjectRunRoot 把 WSL 项目的 UNC 工作区路径转换为 WSL 内 Linux 路径。
// 已经是 Linux 绝对路径的（历史项目可能直接存 /mnt/c/...）原样规整后返回。
func wslProjectRunRoot(workspacePath, distro string) (string, error) {
	if strings.HasPrefix(workspacePath, "/") {
		return path.Clean(workspacePath), nil
	}
	linuxPath, ok := uncToWslPath(workspacePath, distro)
	if !ok || strings.TrimSpace(linuxPath) == "" {
		return "", fmt.Errorf("项目路径不在 WSL 发行版 %q 内，无法在 WSL 运行：%s", distro, workspacePath)
	}
	return path.Clean(linuxPath), nil
}

// resolveWSLProjectRunWorkDir 把配置里的工作目录解析成 WSL 内绝对路径，并保证不越出
// 项目根。configured 支持：空（项目根）、项目内相对路径、WSL 绝对路径、WSL 项目路径的
// UNC 形式。
func resolveWSLProjectRunWorkDir(linuxRoot, configured string) (string, error) {
	root := path.Clean(strings.TrimSpace(linuxRoot))
	if configured = strings.TrimSpace(configured); configured == "" {
		return root, nil
	}
	candidate := configured
	if strings.HasPrefix(configured, `\\`) {
		converted, ok := uncToWslPath(configured, "")
		if !ok {
			return "", errors.New("工作目录必须是项目内相对路径")
		}
		candidate = converted
	} else if isWindowsDrivePath(configured) {
		// 盘符绝对路径（C:\proj）在 WSL 里没有意义，不能靠拼接混进项目根之下。
		return "", errors.New("工作目录必须是项目内相对路径")
	}
	candidate = path.Clean(strings.ReplaceAll(candidate, `\`, "/"))
	workDir := candidate
	if !strings.HasPrefix(candidate, "/") {
		workDir = path.Join(root, candidate)
	}
	if !wslLinuxPathWithin(root, workDir) {
		return "", errors.New("工作目录不能超出项目路径")
	}
	return workDir, nil
}

// wslLinuxPathWithin 判断 candidate 是否等于 base 或位于 base 之下（两者都已 path.Clean）。
func wslLinuxPathWithin(base, candidate string) bool {
	if candidate == base {
		return true
	}
	return base == "/" || strings.HasPrefix(candidate, strings.TrimSuffix(base, "/")+"/")
}

// isWindowsDrivePath 判断是否为盘符绝对路径（C:\x 或 C:/x），Linux 侧不存在这种写法。
func isWindowsDrivePath(value string) bool {
	if len(value) < 2 || value[1] != ':' {
		return false
	}
	c := value[0]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// buildWSLProjectRunShell 构造在 WSL 内执行的 shell 脚本：cd 到工作目录、导出环境变量，
// 最后接用户命令。
//
// ⚠️ 环境变量名按 POSIX 标识符原样拼进 shell，只有值走 shellQuote——这与
// buildSSHRunShell 是同一条约束，安全性来自上游 validateRunEnvironmentVariables
// （isPosixEnvName）。新增调用方若换了数据来源，必须先过同一道校验。
func buildWSLProjectRunShell(workDir string, envVars map[string]string, command string) string {
	script := "cd " + shellQuote(workDir) + " || exit 1; "
	names := make([]string, 0, len(envVars))
	for name := range envVars {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		script += "export " + name + "=" + shellQuote(envVars[name]) + " && "
	}
	return script + command
}

// newWSLProjectRunCommand 构造启动命令。脚本整体经 wslEncodeArg 编码成单个 token，
// 避免 wsl.exe 把 `--` 后的参数交给默认 shell 时按空格重新切分（见 wslEncodeArg）。
func newWSLProjectRunCommand(ctx context.Context, distro, workDir, command string, envVars map[string]string, pidfile string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "wsl.exe", "-d", distro, "--", "sh", "-c", wslEncodeArg(wslProjectRunScript(workDir, command, envVars, pidfile)))
	cmd.Env = os.Environ()
	configureProcessGroup(cmd)
	return cmd
}

// wslProjectRunScript 在用户脚本前面补两件事：
//   - 记录 shell 自身 PID（wsl.exe 下这个 shell 就是新会话/进程组的 leader，见
//     wslProjectRunner 的说明），供停止时 kill -9 -PGID；
//   - 退出时删掉 pidfile。正常退出由这里的 trap 清理，不必为删一个 /tmp 文件再拉一次
//     wsl.exe；被 SIGKILL 时 trap 不执行，由停止脚本里的 rm -f 兜底。
func wslProjectRunScript(workDir, command string, envVars map[string]string, pidfile string) string {
	prefix := "echo \"$$\" > " + shellQuote(pidfile) + " 2>/dev/null; trap " +
		shellQuote("rm -f "+shellQuote(pidfile)) + " EXIT; "
	return prefix + buildWSLProjectRunShell(workDir, envVars, command)
}

// newWSLControlCommand 构造"控制用"的短命令（停止时杀进程组等）。与启动命令一样要
// configureProcessGroup——否则每次停止都会在桌面上闪一个控制台窗口。
func newWSLControlCommand(ctx context.Context, distro, script string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "wsl.exe", "-d", distro, "--", "sh", "-c", wslEncodeArg(script))
	cmd.Env = os.Environ()
	configureProcessGroup(cmd)
	return cmd
}

func (pr *wslProjectRunner) setStatusListener(listener statusBroadcaster) {
	pr.statusBroadcast = listener
}

// emitStatus 在状态切分归位后触发状态广播。调用方须已释放 pr.mu。
func (pr *wslProjectRunner) emitStatus(event RunStatusEvent) {
	if pr.statusBroadcast != nil {
		pr.statusBroadcast(event)
	}
}

func (pr *wslProjectRunner) emitLog(entry LogEntry) {
	entry.Text = truncateRunLogText(entry.Text)
	pr.logMu.Lock()
	pr.nextLogID++
	entry.ID = pr.nextLogID
	pr.logBuf.Append(entry)
	pr.broadcast(entry)
	pr.logMu.Unlock()
}

func (pr *wslProjectRunner) registerLogSubscriber(register func([]LogEntry)) {
	pr.logMu.Lock()
	defer pr.logMu.Unlock()
	register(pr.logBuf.Recent(projectRunLogHistory))
}

func (pr *wslProjectRunner) StartWithConfig(parentCtx context.Context, projectPath, workDir, command string, envVars map[string]string, executionTarget RunExecutionTarget) error {
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
	if pr.status == RunStatusStopping {
		// stop() 超时后 wsl.exe 可能仍在退出中；此时再起一代会造成同一工作区两份进程，
		// 而且旧代次的 pidfile 已被 kill 脚本删除，无法再定位旧进程组。
		pr.mu.Unlock()
		return fmt.Errorf("进程正在停止，请稍后重试")
	}
	pr.workDir = workDir
	pr.command = command
	pr.envVars = envVars
	pr.mu.Unlock()
	return pr.start(parentCtx, projectPath)
}

func (pr *wslProjectRunner) RestartWithConfig(parentCtx context.Context, projectPath, workDir, command string, envVars map[string]string, executionTarget RunExecutionTarget) error {
	pr.opMu.Lock()
	defer pr.opMu.Unlock()
	pr.mu.Lock()
	if pr.retired {
		pr.mu.Unlock()
		return fmt.Errorf("项目已删除，无法启动进程")
	}
	if pr.status == RunStatusStopping {
		pr.mu.Unlock()
		return fmt.Errorf("进程正在停止，请稍后重试")
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

func (pr *wslProjectRunner) start(parentCtx context.Context, workspacePath string) error {
	pr.mu.Lock()
	if pr.retired {
		pr.mu.Unlock()
		return fmt.Errorf("项目已删除，无法启动进程")
	}
	if pr.status == RunStatusRunning || pr.status == RunStatusStarting {
		pr.mu.Unlock()
		return fmt.Errorf("进程已在运行中")
	}
	if pr.status == RunStatusStopping {
		pr.mu.Unlock()
		return fmt.Errorf("进程正在停止，请稍后重试")
	}
	command := pr.command
	workDir := pr.workDir
	envVars := pr.envVars
	if strings.TrimSpace(command) == "" {
		pr.mu.Unlock()
		return errors.New("请先配置启动命令")
	}
	pr.status = RunStatusStarting
	pr.exitChan = make(chan struct{})
	pr.gen++
	gen := pr.gen
	pr.workspacePath = workspacePath
	pidfile := wslProjectRunPIDFile(pr.projectID, workspacePath, gen)
	pr.startedAt = time.Time{}
	pr.exitCode = 0
	pr.hasExitCode = false
	pr.mu.Unlock()

	linuxRoot, err := wslProjectRunRoot(workspacePath, pr.distro)
	if err != nil {
		return pr.abortStart(err)
	}
	resolvedWorkDir, err := resolveWSLProjectRunWorkDir(linuxRoot, workDir)
	if err != nil {
		return pr.abortStart(err)
	}

	// The HTTP request guard ends when Start returns. The process needs its own
	// token until the target's actual exit is observed.
	releaseWorkspace := func() {}
	if pr.workspaceAdmission != nil {
		releaseWorkspace, err = pr.workspaceAdmission(workspacePath)
		if err != nil {
			return pr.abortStart(err)
		}
		if releaseWorkspace == nil {
			// 准入实现必须给释放函数；缺失时退化为空操作，绝不在 defer 里调 nil。
			releaseWorkspace = func() {}
		}
	}
	transferred := false
	defer func() {
		if !transferred {
			releaseWorkspace()
		}
	}()

	pr.ctx, pr.cancel = context.WithCancel(parentCtx)
	cmd := newWSLProjectRunCommand(pr.ctx, pr.distro, resolvedWorkDir, command, envVars, pidfile)
	pr.mu.Lock()
	pr.cmd = cmd
	pr.mu.Unlock()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return pr.abortStart(fmt.Errorf("创建 stdout 管道失败: %w", err))
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return pr.abortStart(fmt.Errorf("创建 stderr 管道失败: %w", err))
	}
	if err := cmd.Start(); err != nil {
		return pr.abortStart(fmt.Errorf("启动进程失败: %w", err))
	}

	pr.mu.Lock()
	pr.startedAt = time.Now()
	pr.status = RunStatusRunning
	startedAt := pr.startedAt
	pr.mu.Unlock()

	pr.emitStatus(RunStatusEvent{ProjectID: pr.projectID, Status: RunStatusRunning, StartedAt: &startedAt})

	pr.emitLog(LogEntry{Timestamp: time.Now(), Stream: "system",
		Text: fmt.Sprintf("WSL 进程已启动（发行版 %s）: 目录=%s, 命令=%s", pr.distro, resolvedWorkDir, command)})

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
	transferred = true
	go pr.wait(pipeReadersDone, gen, releaseWorkspace, cmd)
	return nil
}

// abortStart 把启动失败归位到 Failed，并释放等待 exitChan 的调用方。
func (pr *wslProjectRunner) abortStart(err error) error {
	if pr.cancel != nil {
		pr.cancel()
	}
	pr.mu.Lock()
	pr.status = RunStatusFailed
	pr.exitCode = -1
	pr.hasExitCode = true
	close(pr.exitChan)
	pr.mu.Unlock()
	pr.emitStatus(RunStatusEvent{ProjectID: pr.projectID, Status: RunStatusFailed})
	return err
}

func (pr *wslProjectRunner) readPipe(r io.Reader, stream string) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	if stream == "stderr" {
		// wsl.exe 主机侧警告是 UTF-16LE，与 WSL 内命令的 UTF-8 混在同一管道。
		scanner.Split(wslStderrSplit)
	}
	for scanner.Scan() {
		text := scanner.Text()
		// 每次启动 wsl.exe 都会重复的 NAT/代理提示，与项目输出无关。
		if stream == "stderr" && isWslHostWarning(text) {
			continue
		}
		pr.emitLog(LogEntry{
			Timestamp: time.Now(),
			Stream:    stream,
			Text:      text,
		})
	}
}

func (pr *wslProjectRunner) wait(pipeReadersDone <-chan struct{}, gen uint64, releaseWorkspace func(), cmd *exec.Cmd) {
	// Cmd.Wait 会关闭管道；先等两个扫描器读完，避免丢掉最后几行。
	<-pipeReadersDone
	err := cmd.Wait()
	// WSL 用的是"包装进程退出即收尾"的判据（见 projectRunWorkspaceExitObserved）：
	// 停止流程已先 SIGKILL 进程组，wsl.exe 退出就是目标侧结束的凭证。若改用 cross
	// 规则，停止/失败一次就会把准入永久留在目标侧 journal 里。
	//
	// 释放放在 defer 里：它可能是一次跨端 guard RPC（最长 10 秒），不能算进 stop() 的
	// "等进程退出"预算 —— 否则进程其实已经退出，却会被报成"未在 10 秒内退出"。
	// defer 同时覆盖 gen 已变化的提前返回：那条准入属于旧运行，仍要释放。
	if release := projectRunWorkspaceExitObserved(err); release {
		defer releaseWorkspace()
	}

	pr.mu.Lock()
	// stop() 超时返回后新一轮 start 会创建新的 exitChan/状态；旧运行的收尾信息已过期。
	if pr.gen != gen {
		pr.mu.Unlock()
		return
	}
	exitCode := 0
	// 与本地 runner 一致：停止流程中（status==Stopping）即使 wsl.exe 以非零退出
	// 结束，也归为 Stopped 而非 Failed。
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
	status := pr.status
	pr.mu.Unlock()

	pr.emitStatus(RunStatusEvent{ProjectID: pr.projectID, Status: status})

	pr.emitLog(LogEntry{Timestamp: time.Now(), Stream: "system",
		Text: fmt.Sprintf("进程已退出: code=%d", exitCode)})

	pr.mu.Lock()
	if pr.gen == gen {
		close(pr.exitChan)
	}
	pr.mu.Unlock()
}

func (pr *wslProjectRunner) Stop() error {
	pr.opMu.Lock()
	defer pr.opMu.Unlock()
	return pr.stop()
}

func (pr *wslProjectRunner) stop() error {
	pr.mu.Lock()
	// Stopping 也允许再停一次：上一次 stop() 可能在 wsl.exe 退出前超时返回，
	// 再次点击停止/删除项目时要能把进程组重新杀一遍，而不是报"进程未在运行"。
	if pr.status != RunStatusRunning && pr.status != RunStatusStarting && pr.status != RunStatusStopping {
		pr.mu.Unlock()
		return fmt.Errorf("进程未在运行")
	}
	if pr.cmd == nil {
		pr.mu.Unlock()
		return fmt.Errorf("进程状态异常：未找到命令实例")
	}
	pr.status = RunStatusStopping
	gen := pr.gen
	pr.mu.Unlock()

	pr.emitStatus(RunStatusEvent{ProjectID: pr.projectID, Status: RunStatusStopping})
	pr.emitLog(LogEntry{Timestamp: time.Now(), Stream: "system", Text: "正在停止进程..."})

	// 先在 WSL 内杀掉进程组，再取消本地 ctx：顺序反了会先终止 wsl.exe，
	// Linux 侧的后代就失去可追踪的父进程，变成看不见的孤儿。
	//
	// 本机实测：直接把 wsl.exe 客户端杀掉，Linux 侧整棵启动子树（含 `a & b` 的后台子进程）
	// 也会消失，所以"pidfile 还没写出来就被停止"这个窗口是安全的；仍保留进程组 SIGKILL，
	// 是为了不依赖客户端的连带清理，并覆盖 shell 已退出、后代还挂在同一进程组里的情况。
	if killCmd := remoteProcessGroupKillCommand(pr.pidFileForGen(gen)); killCmd != "" {
		killCtx, killCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = newWSLControlCommand(killCtx, pr.distro, killCmd).Run()
		killCancel()
	}
	if pr.cancel != nil {
		pr.cancel()
	}

	select {
	case <-pr.exitChan:
	case <-time.After(5 * time.Second):
		// wsl.exe 仍未退出：Linux 侧进程组已在上面被 SIGKILL，但退出还没被观测到。
		select {
		case <-pr.exitChan:
		case <-time.After(5 * time.Second):
			// 必须如实报错：返回 nil 会让状态永远停在 Stopping，重启/再启动看起来"成功"，
			// 实际可能起第二份进程。状态留给后续的 wait() 收尾，期间 start 会拒绝。
			return fmt.Errorf("WSL 进程未在 10 秒内退出；已在发行版内结束进程组，请稍后重试")
		}
	}
	return nil
}

func (pr *wslProjectRunner) pidFileForGen(gen uint64) string {
	pr.mu.RLock()
	defer pr.mu.RUnlock()
	return wslProjectRunPIDFile(pr.projectID, pr.workspacePath, gen)
}

func (pr *wslProjectRunner) Retire() error {
	pr.opMu.Lock()
	defer pr.opMu.Unlock()
	pr.mu.Lock()
	pr.retired = true
	status := pr.status
	// Stopping 也算"还需要收尾"：上一次停止可能超时返回，删除项目/关闭服务时要再试一次。
	running := status == RunStatusRunning || status == RunStatusStarting || status == RunStatusStopping
	pr.mu.Unlock()
	if !running {
		return nil
	}
	if status == RunStatusStopping {
		// 已经在停：再试一次进程组清理，但"还没退出"不能让删除项目失败 ——
		// 删除方持有 projectLifecycleMu，把超时错误抛出去只会让用户反复重试。
		if err := pr.stop(); err != nil {
			log.Printf("[project-run] %s 停止未完成：%v", pr.projectID, err)
		}
		return nil
	}
	return pr.stop()
}

// ClearLogs removes the retained output and acknowledges a terminal failure.
func (pr *wslProjectRunner) ClearLogs() {
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

func (pr *wslProjectRunner) StatusSnapshot() RunStatusResponse {
	pr.mu.RLock()
	defer pr.mu.RUnlock()
	snapshot := RunStatusResponse{
		Status:          pr.status,
		ExecutionTarget: RunExecutionTargetWSL,
		Command:         pr.command,
		RecentLogs:      pr.logBuf.Recent(projectRunLogHistory),
	}
	if !pr.startedAt.IsZero() {
		startedAt := pr.startedAt
		snapshot.StartedAt = &startedAt
	}
	// wsl.exe 的 Windows PID 对用户没有意义，与 SSH 远端进程一样不返回。
	if pr.hasExitCode {
		exitCode := pr.exitCode
		snapshot.ExitCode = &exitCode
	}
	return snapshot
}

func (pr *wslProjectRunner) LightStatusSnapshot() RunStatusResponse {
	pr.mu.RLock()
	defer pr.mu.RUnlock()
	snapshot := RunStatusResponse{
		Status:          pr.status,
		ExecutionTarget: RunExecutionTargetWSL,
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
