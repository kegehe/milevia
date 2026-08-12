package app

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
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

type RunExecutionTarget string

const (
	RunExecutionTargetAuto    RunExecutionTarget = "auto"
	RunExecutionTargetWSL     RunExecutionTarget = "wsl"
	RunExecutionTargetWindows RunExecutionTarget = "windows"
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

// Clear drops all buffered log entries while preserving the buffer allocation.
func (rb *ringBuffer) Clear() {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	rb.head = 0
	rb.size = 0
}

// logBroadcaster 是广播日志的回调，由 Server 注入。
type logBroadcaster func(entry LogEntry)

// RunStatusEvent 是推送给总览进程状态订阅者的状态变更事件（轻量、不落库、无日志）。
// 与批量端点 /api/projects/processes/statuses 的 runStatus/runPid/runStartedAt 字段
// 一一对应，前端从此单一来源收敛，不派生冗余布尔（status=="running" 即 running）。
type RunStatusEvent struct {
	ProjectID string     `json:"projectId"`
	Status    RunStatus  `json:"status"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
	PID       *int       `json:"pid,omitempty"`
}

// statusBroadcaster 是广播进程状态变更的回调，由 Server 注入（仿 logBroadcaster）。
type statusBroadcaster func(event RunStatusEvent)

// projectRunner 管理单个项目的运行进程。
type projectRunner struct {
	projectID         string
	workDir           string
	command           string
	envVars           map[string]string
	executionTarget   RunExecutionTarget
	windowsPID        int
	windowsPIDMarker  string
	windowsPIDReady   chan struct{}
	windowsStartError string

	cmd    *exec.Cmd
	ctx    context.Context
	cancel context.CancelFunc

	logBuf    *ringBuffer
	broadcast logBroadcaster
	logMu     sync.Mutex
	nextLogID uint64

	statusBroadcast statusBroadcaster

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
		projectID:       projectID,
		workDir:         workDir,
		command:         command,
		envVars:         envVars,
		executionTarget: RunExecutionTargetAuto,
		logBuf:          newRingBuffer(projectRunLogCapacity),
		broadcast:       broadcast,
		status:          RunStatusStopped,
	}
}

// setStatusListener 注册进程状态变更回调（由 Server 注入）。须在启动之前设置。
func (pr *projectRunner) setStatusListener(listener statusBroadcaster) {
	pr.statusBroadcast = listener
}

// emitStatus 在状态切分归位后触发状态广播。调用方须已释放 pr.mu（与 emitLog 同一位），
// 避免在锁内调用 Server 广播造成二次锁与死锁风险。
func (pr *projectRunner) emitStatus(event RunStatusEvent) {
	if pr.statusBroadcast != nil {
		pr.statusBroadcast(event)
	}
}

// Start 启动进程。parentCtx 是 Server 的 runtimeCtx，projectPath 是项目根路径。
func (pr *projectRunner) Start(parentCtx context.Context, projectPath string) error {
	pr.opMu.Lock()
	defer pr.opMu.Unlock()
	return pr.start(parentCtx, projectPath)
}

func (pr *projectRunner) StartWithConfig(parentCtx context.Context, projectPath, workDir, command string, envVars map[string]string, executionTarget RunExecutionTarget) error {
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
	pr.executionTarget = normalizeRunExecutionTarget(executionTarget)
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
	executionTarget, err := resolveRunExecutionTarget(projectPath, pr.executionTarget)
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
	cmd, windowsPIDMarker, err := newProjectRunCommand(pr.ctx, executionTarget, workDir, pr.command)
	if err != nil {
		pr.cancel()
		pr.mu.Lock()
		pr.status = RunStatusFailed
		pr.exitCode = -1
		pr.hasExitCode = true
		close(pr.exitChan)
		pr.mu.Unlock()
		pr.emitStatus(RunStatusEvent{ProjectID: pr.projectID, Status: RunStatusFailed})
		return err
	}
	pr.cmd = cmd
	pr.mu.Lock()
	// 记录解析后的真实运行目标：auto 可能被解析为 windows，readPipe 靠它决定
	// 是否需要 GBK 转码。executionTarget 在本次运行内不再改变。
	pr.executionTarget = executionTarget
	pr.windowsPID = 0
	pr.windowsPIDMarker = windowsPIDMarker
	pr.windowsPIDReady = nil
	pr.windowsStartError = ""
	if executionTarget == RunExecutionTargetWindows && runtime.GOOS != "windows" {
		pr.windowsPIDReady = make(chan struct{})
	}
	windowsPIDReady := pr.windowsPIDReady
	pr.mu.Unlock()

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
		pr.emitStatus(RunStatusEvent{ProjectID: pr.projectID, Status: RunStatusFailed})
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
		pr.emitStatus(RunStatusEvent{ProjectID: pr.projectID, Status: RunStatusFailed})
		return fmt.Errorf("创建 stderr 管道失败: %w", err)
	}

	if err := pr.cmd.Start(); err != nil {
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
	pr.pid = pr.cmd.Process.Pid
	pr.startedAt = time.Now()
	pr.status = RunStatusRunning
	pid := pr.pid
	startedAt := pr.startedAt
	pr.mu.Unlock()

	pr.emitStatus(RunStatusEvent{ProjectID: pr.projectID, Status: RunStatusRunning, StartedAt: &startedAt, PID: &pid})

	pr.emitLog(LogEntry{Timestamp: time.Now(), Stream: "system",
		Text: fmt.Sprintf("进程已启动: pid=%d, 环境=%s, 命令=%s", pid, executionTarget, pr.command)})

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
	go pr.wait(pipeReadersDone)
	if windowsPIDReady != nil {
		select {
		case <-windowsPIDReady:
		case <-pr.exitChan:
			return pr.windowsLaunchError()
		case <-time.After(3 * time.Second):
			terminateProcessGroup(pr.cmd)
			return errors.New("Windows 启动器未返回进程 ID，已停止启动")
		}
	}

	return nil
}

func resolveProjectRunWorkDir(projectPath, configuredWorkDir string) (string, error) {
	workDir := projectPath
	if configuredWorkDir != "" {
		// Absolute directories only reach this point after the API has resolved a
		// recorded orchestration worktree. Configuration updates still reject
		// arbitrary absolute paths, preserving the normal project boundary.
		if filepath.IsAbs(configuredWorkDir) {
			workDir = configuredWorkDir
		} else {
			workDir = filepath.Join(projectPath, configuredWorkDir)
		}
	}
	if !filepath.IsAbs(configuredWorkDir) && !isPathWithin(projectPath, workDir) {
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
	if !filepath.IsAbs(configuredWorkDir) && !isPathWithin(resolvedProjectPath, resolvedWorkDir) {
		return "", errors.New("工作目录不能超出项目路径")
	}
	return resolvedWorkDir, nil
}

func normalizeRunExecutionTarget(target RunExecutionTarget) RunExecutionTarget {
	if target == "" {
		return RunExecutionTargetAuto
	}
	return target
}

func resolveRunExecutionTarget(projectPath string, requested RunExecutionTarget) (RunExecutionTarget, error) {
	if runtime.GOOS == "windows" {
		switch normalizeRunExecutionTarget(requested) {
		case RunExecutionTargetAuto, RunExecutionTargetWindows:
			return RunExecutionTargetWindows, nil
		case RunExecutionTargetWSL:
			return "", errors.New("WSL 项目运行需要配置专用 WSL Runner")
		default:
			return "", errors.New("运行环境必须为 auto、wsl 或 windows")
		}
	}
	switch normalizeRunExecutionTarget(requested) {
	case RunExecutionTargetAuto:
		if _, ok := wslPathToWindowsPath(projectPath); ok {
			return RunExecutionTargetWindows, nil
		}
		return RunExecutionTargetWSL, nil
	case RunExecutionTargetWSL:
		return RunExecutionTargetWSL, nil
	case RunExecutionTargetWindows:
		if _, ok := wslPathToWindowsPath(projectPath); !ok {
			return "", errors.New("Windows 运行环境仅支持 /mnt/<盘符>/ 下的项目")
		}
		return RunExecutionTargetWindows, nil
	default:
		return "", errors.New("运行环境必须为 auto、wsl 或 windows")
	}
}

func wslPathToWindowsPath(wslPath string) (string, bool) {
	cleaned := filepath.ToSlash(filepath.Clean(wslPath))
	if len(cleaned) < len("/mnt/d") || !strings.HasPrefix(cleaned, "/mnt/") {
		return "", false
	}
	drive := cleaned[len("/mnt/")]
	if !((drive >= 'a' && drive <= 'z') || (drive >= 'A' && drive <= 'Z')) {
		return "", false
	}
	if len(cleaned) > len("/mnt/d") && cleaned[len("/mnt/d")] != '/' {
		return "", false
	}
	rest := strings.TrimPrefix(cleaned[len("/mnt/d"):], "/")
	windowsPath := strings.ToUpper(string(drive)) + `:\`
	if rest != "" {
		windowsPath += strings.ReplaceAll(rest, "/", `\`)
	}
	return windowsPath, true
}

func newProjectRunCommand(ctx context.Context, executionTarget RunExecutionTarget, workDir, command string) (*exec.Cmd, string, error) {
	if executionTarget == RunExecutionTargetWindows && runtime.GOOS == "windows" {
		cmd := exec.CommandContext(ctx, "cmd.exe", "/d", "/s", "/c", command)
		cmd.Dir = workDir
		return cmd, "", nil
	}
	if executionTarget == RunExecutionTargetWSL {
		cmd := exec.CommandContext(ctx, "sh", "-c", command)
		cmd.Dir = workDir
		return cmd, "", nil
	}
	if executionTarget != RunExecutionTargetWindows {
		return nil, "", fmt.Errorf("不支持的运行环境: %s", executionTarget)
	}
	windowsWorkDir, ok := wslPathToWindowsPath(workDir)
	if !ok {
		return nil, "", errors.New("无法将工作目录转换为 Windows 路径")
	}
	powershellPath, err := windowsSystemExecutable("powershell.exe")
	if err != nil {
		return nil, "", err
	}

	marker, err := newWindowsPIDMarker()
	if err != nil {
		return nil, "", fmt.Errorf("生成 Windows 进程标识失败: %w", err)
	}
	workDirEncoded := base64.StdEncoding.EncodeToString([]byte(windowsWorkDir))
	commandEncoded := base64.StdEncoding.EncodeToString([]byte(command))
	// 工作目录和命令被编码为纯 ASCII 数据，再由 PowerShell 解码，避免依赖
	// WSLENV 传递环境变量，也不将用户命令插入 PowerShell 源码。
	script := fmt.Sprintf("$ProgressPreference = 'SilentlyContinue'\n$ErrorActionPreference = 'Stop'\ntry {\n  $workDir = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('%s'))\n  $command = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('%s'))\n  Set-Location -LiteralPath $workDir\n  $child = Start-Process -FilePath cmd.exe -ArgumentList @('/d', '/s', '/c', $command) -PassThru -NoNewWindow\n  [Console]::Out.WriteLine('%s' + $child.Id)\n  try {\n    $child.WaitForExit()\n    exit $child.ExitCode\n  } finally {\n    if (!$child.HasExited) { & taskkill.exe /PID $child.Id /T /F | Out-Null }\n  }\n} catch {\n  [Console]::Error.WriteLine($_.Exception.Message)\n  exit 1\n}\n", workDirEncoded, commandEncoded, marker)
	encoded := base64.StdEncoding.EncodeToString(utf16LE(script))
	return newWindowsPowerShellCommand(ctx, powershellPath, encoded), marker, nil
}

// newWindowsPowerShellCommand keeps the PowerShell success stream in text mode.
// The generated script also suppresses its progress stream, which otherwise
// serializes as CLIXML when WSL attaches redirected pipes.
func newWindowsPowerShellCommand(ctx context.Context, powershellPath, encodedScript string) *exec.Cmd {
	return exec.CommandContext(ctx, powershellPath,
		"-NoLogo", "-NoProfile", "-NonInteractive",
		"-OutputFormat", "Text",
		"-EncodedCommand", encodedScript,
	)
}

func windowsSystemExecutable(name string) (string, error) {
	if path, err := exec.LookPath(name); err == nil {
		return path, nil
	}
	relativePath := filepath.Join("Windows", "System32", name)
	if name == "powershell.exe" {
		relativePath = filepath.Join("Windows", "System32", "WindowsPowerShell", "v1.0", name)
	}
	for _, drive := range append([]string{"c"}, mountedWindowsDrives()...) {
		candidate := filepath.Join("/mnt", drive, relativePath)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("Windows 运行环境不可用：未找到 %s。请启用 WSL Windows 互操作", name)
}

func mountedWindowsDrives() []string {
	entries, err := os.ReadDir("/mnt")
	if err != nil {
		return nil
	}
	drives := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if len(name) != 1 || name == "c" || !entry.IsDir() {
			continue
		}
		if (name[0] >= 'a' && name[0] <= 'z') || (name[0] >= 'A' && name[0] <= 'Z') {
			drives = append(drives, name)
		}
	}
	return drives
}

func newWindowsPIDMarker() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return "__AUTO_WINDOWS_CHILD_" + hex.EncodeToString(bytes) + "__=", nil
}

func utf16LE(value string) []byte {
	units := utf16.Encode([]rune(value))
	output := make([]byte, len(units)*2)
	for i, unit := range units {
		output[i*2] = byte(unit)
		output[i*2+1] = byte(unit >> 8)
	}
	return output
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
	// Windows 目标下，cmd/原生程序常按系统代码页（GBK）把中文写进重定向管道，
	// 而 Go 端统一按 UTF-8 读取，导致中文乱码。这里对每一行先判断其是否为合法
	// UTF-8：是则原样保留（不误伤已按 UTF-8 输出的程序），否则按 GBK 解码。
	transcode := pr.decodeWindowsOutput()
	for scanner.Scan() {
		line := scanner.Bytes()
		text := decodeRunOutputLine(line, transcode)
		if stream == "stdout" && pr.recordWindowsPID(text) {
			continue
		}
		if stream == "stderr" {
			pr.recordWindowsStartError(text)
		}
		entry := LogEntry{
			Timestamp: time.Now(),
			Stream:    stream,
			Text:      text,
		}
		pr.emitLog(entry)
	}
}

// decodeWindowsOutput 报告当前运行是否面向 Windows 目标：是则输出行可能是 GBK。
// executionTarget 在启动后保持不变，可安全读取。
func (pr *projectRunner) decodeWindowsOutput() bool {
	pr.mu.RLock()
	defer pr.mu.RUnlock()
	return pr.executionTarget == RunExecutionTargetWindows
}

// gbkDecoder 复用的 GBK→UTF-8 解码器，避免每行重建。
var gbkDecoder = simplifiedchinese.GBK.NewDecoder()

// decodeRunOutputLine 把一行输出规范化为 UTF-8。transcode 为 false 时原样返回；
// 为 true 时，若整行已是合法 UTF-8 则原样返回（避免把已按 UTF-8 输出的程序二次
// 破坏），否则尝试按 GBK 解码。
func decodeRunOutputLine(line []byte, transcode bool) string {
	if !transcode || utf8.Valid(line) || len(line) == 0 {
		return string(line)
	}
	decoded, err := gbkDecoder.Bytes(line)
	if err != nil {
		return string(line)
	}
	return string(decoded)
}

func (pr *projectRunner) recordWindowsStartError(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if pr.windowsPID == 0 && pr.windowsPIDMarker != "" {
		pr.windowsStartError = text
	}
}

func (pr *projectRunner) windowsLaunchError() error {
	pr.mu.RLock()
	defer pr.mu.RUnlock()
	if pr.windowsStartError != "" {
		return fmt.Errorf("Windows 启动失败: %s", pr.windowsStartError)
	}
	if pr.hasExitCode {
		return fmt.Errorf("Windows 启动器在返回进程 ID 前退出: code=%d", pr.exitCode)
	}
	return errors.New("Windows 启动器在返回进程 ID 前退出")
}

func (pr *projectRunner) recordWindowsPID(text string) bool {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if pr.windowsPIDMarker == "" || !strings.HasPrefix(text, pr.windowsPIDMarker) {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimPrefix(text, pr.windowsPIDMarker))
	if err != nil || pid <= 0 {
		return false
	}
	if pr.windowsPID != 0 {
		return true
	}
	pr.windowsPID = pid
	if pr.windowsPIDReady != nil {
		close(pr.windowsPIDReady)
	}
	return true
}

func (pr *projectRunner) wait(pipeReadersDone <-chan struct{}) {
	// Cmd.Wait closes the pipe handles. Wait for both scanners first so the
	// final output (including Windows launcher errors) is not discarded.
	<-pipeReadersDone
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
	status := pr.status
	pr.mu.Unlock()

	pr.emitStatus(RunStatusEvent{ProjectID: pr.projectID, Status: status})

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

	pr.emitStatus(RunStatusEvent{ProjectID: pr.projectID, Status: RunStatusStopping})

	pr.emitLog(LogEntry{Timestamp: time.Now(), Stream: "system", Text: "正在停止进程..."})

	pr.mu.RLock()
	windowsPID := pr.windowsPID
	pr.mu.RUnlock()
	if windowsPID > 0 {
		terminateWindowsProcessTree(windowsPID)
	}
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

func terminateWindowsProcessTree(pid int) {
	taskkillPath, err := windowsSystemExecutable("taskkill.exe")
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, taskkillPath, "/PID", strconv.Itoa(pid), "/T", "/F").Run()
}

// Restart 重启进程：先停止，等待退出，再启动。
func (pr *projectRunner) Restart(parentCtx context.Context, projectPath string) error {
	pr.opMu.Lock()
	defer pr.opMu.Unlock()
	return pr.restart(parentCtx, projectPath)
}

func (pr *projectRunner) RestartWithConfig(parentCtx context.Context, projectPath, workDir, command string, envVars map[string]string, executionTarget RunExecutionTarget) error {
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
	pr.executionTarget = normalizeRunExecutionTarget(executionTarget)
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

// ClearLogs removes the retained output. Clearing a failed run also acknowledges
// its terminal failure, so project overviews return to the ordinary stopped state.
func (pr *projectRunner) ClearLogs() {
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
		pr.pid = 0
		pr.exitCode = 0
		pr.hasExitCode = false
	}
	pr.mu.Unlock()
	if failed {
		pr.emitStatus(RunStatusEvent{ProjectID: pr.projectID, Status: RunStatusStopped})
	}
}

// StatusSnapshot 返回当前状态快照（线程安全）。
func (pr *projectRunner) StatusSnapshot() RunStatusResponse {
	pr.mu.RLock()
	defer pr.mu.RUnlock()
	snapshot := RunStatusResponse{
		Status:          pr.status,
		ExecutionTarget: pr.executionTarget,
		RecentLogs:      pr.logBuf.Recent(projectRunLogHistory),
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

// LightStatusSnapshot 返回精简状态快照（线程安全），仅供批量总览端点使用：
// 不拷贝环形日志缓冲区（避免 N×200 拷贝），其余取值与 StatusSnapshot 一致。
func (pr *projectRunner) LightStatusSnapshot() RunStatusResponse {
	pr.mu.RLock()
	defer pr.mu.RUnlock()
	snapshot := RunStatusResponse{
		Status:          pr.status,
		ExecutionTarget: pr.executionTarget,
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
