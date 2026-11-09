package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type supervisionCheckResult struct {
	Check            supervisionCheck `json:"check"`
	ExitCode         int              `json:"exitCode"`
	Stdout           string           `json:"stdout"`
	Stderr           string           `json:"stderr"`
	Truncated        bool             `json:"truncated"`
	Interrupted      bool             `json:"interrupted,omitempty"`
	NotExecuted      bool             `json:"notExecuted,omitempty"`
	Executable       string           `json:"executable"`
	Environment      string           `json:"environment"`
	StartedAt        time.Time        `json:"startedAt"`
	FinishedAt       time.Time        `json:"finishedAt"`
	Error            string           `json:"error,omitempty"`
	BrowserResult    json.RawMessage  `json:"browserResult,omitempty"`
	ScreenshotSHA256 string           `json:"screenshotSha256,omitempty"`
}

func validateSupervisionCheck(c supervisionCheck, plan supervisionTaskPlan, manifest supervisionWorkspaceManifest) error {
	if !supervisionTextValid(c.ID, 100) || len(c.AcceptanceIDs) == 0 || c.TimeoutMillis < 1 || c.TimeoutMillis > int64(30*time.Minute/time.Millisecond) {
		return errors.New("检查需要唯一ID、验收点和有效超时")
	}
	if c.Directory == "" {
		c.Directory = "."
	}
	if c.Directory != "." {
		if err := validateSupervisionCheckDirectory(c.Directory, manifest); err != nil {
			return err
		}
	}
	points := map[string]string{}
	for _, p := range plan.Acceptance {
		points[p.ID] = p.Kind
	}
	seen := map[string]bool{}
	for _, id := range c.AcceptanceIDs {
		kind, ok := points[id]
		if !ok || seen[id] {
			return errors.New("检查引用未知或重复的验收点")
		}
		seen[id] = true
		if c.Browser == nil && kind != "command" {
			return errors.New("命令检查不能代替浏览器或人工验收")
		}
		if c.Browser != nil && kind != "browser" {
			return errors.New("浏览器检查只能绑定浏览器验收点，命令验收点需要单独运行命令检查")
		}
	}
	if c.Browser != nil {
		b := c.Browser
		u, err := url.Parse(b.URL)
		if err != nil || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1") {
			return errors.New("浏览器验收仅访问本机测试服务")
		}
		if len(b.Steps) == 0 || len(b.Steps) > 100 {
			return errors.New("浏览器检查需要1–100步实际交互或断言")
		}
		assertion := false
		for _, step := range b.Steps {
			if len(step.Selector) > 2000 || len(step.Value) > 8000 {
				return errors.New("浏览器步骤超限")
			}
			switch step.Action {
			case "click", "fill", "check", "select", "visible", "text", "value", "url":
			default:
				return errors.New("浏览器步骤类型不支持")
			}
			if step.Action == "visible" || step.Action == "text" || step.Action == "value" || step.Action == "url" {
				assertion = true
			}
		}
		if !assertion {
			return errors.New("浏览器验收至少包含一项实际断言")
		}
		if b.ServerProgram == "" {
			return errors.New("浏览器验证必须启动本次快照的独立测试服务")
		}
		if err := validateSupervisionProgram(b.ServerProgram, b.ServerArgs, true); err != nil {
			return err
		}
		return nil
	}
	return validateSupervisionProgram(c.Program, c.Args, false)
}

func validateSupervisionCheckDirectory(name string, manifest supervisionWorkspaceManifest) error {
	if !supervisionManifestPathLocal(manifest, name) || path.Clean(name) != name || strings.ContainsAny(name, "\\:*?[]\x00") || strings.HasPrefix(name, "/") {
		return errors.New("检查目录必须是项目内的明确相对路径")
	}
	for _, segment := range strings.Split(name, "/") {
		if supervisionSnapshotExclusion(segment, true) != "" || supervisionGeneratedPath(segment, true) || strings.TrimRight(segment, ". ") != segment {
			return errors.New("检查目录包含排除或敏感路径")
		}
	}
	key := name
	if supervisionManifestWindows(manifest) {
		key = strings.ToLower(key)
	}
	found := false
	for _, entry := range manifest.Entries {
		entryPath := entry.Path
		if supervisionManifestWindows(manifest) {
			entryPath = strings.ToLower(entryPath)
		}
		if key == entryPath || strings.HasPrefix(key, entryPath+"/") {
			if entry.Kind != "directory" || entry.Gap != "" {
				return errors.New("检查目录及祖先必须是已覆盖的真实目录")
			}
			found = found || key == entryPath
		}
	}
	if !found {
		return errors.New("检查目录必须已存在于当前项目快照中")
	}
	// cwd does not grant write access to the subtree. Excluded descendants
	// such as node_modules are handled by the separate environment fingerprint.
	return nil
}

func validateSupervisionProgram(program string, args []string, server bool) error {
	if filepath.Base(program) != program || strings.ContainsAny(program, "/\\:\x00") || len(args) > 100 || len(args) == 0 {
		return errors.New("验证只接受受支持工具的程序名及独立参数，禁止任意Shell")
	}
	for _, arg := range args {
		if len(arg) > 8000 || strings.IndexByte(arg, 0) >= 0 {
			return errors.New("验证参数超限")
		}
	}
	first := args[0]
	allowed := false
	switch strings.TrimSuffix(strings.ToLower(program), ".exe") {
	case "go":
		allowed = first == "test" || first == "vet" || first == "build" || first == "version"
	case "cargo":
		allowed = first == "test" || first == "check" || first == "build" || first == "clippy" || first == "fmt"
	case "dotnet":
		allowed = first == "test" || first == "build"
	case "npm", "pnpm", "yarn":
		allowed = first == "test" || first == "lint" || first == "typecheck" || first == "build" || first == "check"
		if (first == "run" || first == "run-script") && len(args) > 1 {
			script := args[1]
			allowed = script == "test" || script == "lint" || script == "typecheck" || script == "build" || script == "check" || strings.HasPrefix(script, "test:") || strings.HasPrefix(script, "lint:")
			if server && (script == "dev" || script == "start" || script == "preview") {
				allowed = true
			}
		}
	case "node":
		allowed = first == "--test" || first == "--version" || (!strings.HasPrefix(first, "-") && (strings.HasSuffix(first, ".js") || strings.HasSuffix(first, ".mjs") || strings.HasSuffix(first, ".cjs")))
	case "python", "python3":
		allowed = !strings.HasPrefix(first, "-") && strings.HasSuffix(first, ".py")
		if first == "-m" && len(args) > 1 {
			allowed = args[1] == "pytest" || args[1] == "unittest" || args[1] == "compileall" || args[1] == "ruff"
		}
	}
	if !allowed {
		return errors.New("该程序或子命令不属于测试、构建、检查能力；提交、部署和任意Shell不被允许")
	}
	return nil
}

func supervisionNativeVerificationCommand(program string, args []string) (string, []string, error) {
	binary, err := exec.LookPath(program)
	if err != nil {
		return "", nil, err
	}
	if runtime.GOOS == "linux" {
		if native, err := supervisionNativeClaudeBinary(binary); err == nil {
			return native, args, nil
		}
		// Linux npm/pnpm/yarn normally use a Node script or a symlink to it.
		// Resolve only their declared package entrypoint, never an arbitrary
		// shell wrapper or a Windows npm installation exposed through WSL.
		entry, err := filepath.EvalSymlinks(binary)
		if err != nil {
			return "", nil, err
		}
		base := strings.ToLower(program)
		script := map[string]string{"npm": "bin/npm-cli.js", "pnpm": "bin/pnpm.cjs", "yarn": "bin/yarn.js"}[base]
		root := filepath.Dir(filepath.Dir(entry))
		var pkg struct {
			Name string            `json:"name"`
			Bin  map[string]string `json:"bin"`
		}
		raw, readErr := os.ReadFile(filepath.Join(root, "package.json"))
		if script == "" || readErr != nil || len(raw) > 1<<20 || json.Unmarshal(raw, &pkg) != nil || pkg.Name != base || strings.TrimPrefix(pkg.Bin[base], "./") != script || filepath.Clean(entry) != filepath.Join(root, filepath.FromSlash(script)) {
			return "", nil, errors.New("Linux 验证需要目标原生程序或可核对的包管理器 Node 入口")
		}
		node, err := exec.LookPath("node")
		if err != nil {
			return "", nil, err
		}
		node, err = supervisionNativeClaudeBinary(node)
		if err != nil {
			return "", nil, err
		}
		return node, append([]string{entry}, args...), nil
	}
	if strings.EqualFold(filepath.Ext(binary), ".exe") || filepath.Ext(binary) == "" {
		return binary, args, nil
	}
	base := strings.TrimSuffix(strings.ToLower(program), ".cmd")
	packages := map[string]string{"npm": "npm/bin/npm-cli.js", "pnpm": "pnpm/bin/pnpm.cjs", "yarn": "yarn/bin/yarn.js"}
	if script, ok := packages[base]; ok {
		candidate := filepath.Join(filepath.Dir(binary), "node_modules", filepath.FromSlash(script))
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			node, err := exec.LookPath("node")
			return node, append([]string{candidate}, args...), err
		}
	}
	return "", nil, errors.New("验证工具需要可核对的原生程序，无法解析当前包装脚本")
}

type supervisionVerificationWriter struct {
	mu        sync.Mutex
	data      []byte
	total     int
	sink      *supervisionRoleSink
	stream    string
	truncated bool
	cancel    context.CancelFunc
}

func (w *supervisionVerificationWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.total += len(p)
	if len(w.data) < 256<<10 {
		remaining := (256 << 10) - len(w.data)
		if len(p) > remaining {
			w.data = append(w.data, p[:remaining]...)
		} else {
			w.data = append(w.data, p...)
		}
	}
	if w.total > 256<<10 {
		w.truncated = true
	}
	if w.sink != nil {
		w.sink.Event("verification.output", mustJSON(map[string]string{"stream": w.stream, "text": redactAgentText(string(p))}))
	}
	if w.total > 64<<20 {
		if w.cancel != nil {
			w.cancel()
		}
		return 0, errors.New("测试输出超过64MiB上限")
	}
	return len(p), nil
}

func supervisionEnvironmentFingerprint(ctx context.Context, rootPath, binary string, extraPaths ...string) (string, error) {
	h := sha256.New()
	file, err := os.Open(binary)
	if err != nil {
		return "", err
	}
	defer file.Close()
	for _, name := range extraPaths {
		if !filepath.IsAbs(name) {
			continue
		}
		if name == binary || (filepath.Separator == '\\' && strings.EqualFold(name, binary)) {
			continue
		}
		info, err := os.Stat(name)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if info.Size() > 64<<20 {
			return "", errors.New("验证启动文件超过64MiB核对上限")
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return "", err
		}
		io.WriteString(h, name)
		h.Write(data)
	}
	_, err = io.Copy(h, file)
	file.Close()
	if err != nil {
		return "", err
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return "", err
	}
	defer root.Close()
	var total int64
	entries := 0
	visited := map[string]bool{}
	var walk func(string) error
	walk = func(name string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := root.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if supervisionEnvironmentCache(name) {
			return nil
		}
		key := filepath.Clean(name)
		if filepath.Separator == '\\' {
			key = strings.ToLower(key)
		}
		if visited[key] {
			return nil
		}
		visited[key] = true
		entries++
		if entries > 150000 {
			return errors.New("验证依赖环境文件数超过上限")
		}
		fmt.Fprintf(h, "%s:%d\n", name, info.Mode())
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := root.Readlink(name)
			if err != nil {
				return err
			}
			io.WriteString(h, target)
			// 链接文字本身不能证明实际运行的依赖内容未变。只核对项目内目标，
			// 外部依赖无法完整留在本次环境边界中，必须明确拒绝。
			resolved := filepath.Clean(filepath.Join(filepath.Dir(name), target))
			if filepath.IsAbs(target) {
				resolved, err = filepath.Rel(rootPath, target)
				if err != nil {
					return err
				}
			}
			if filepath.IsAbs(resolved) || resolved == ".." || strings.HasPrefix(resolved, ".."+string(filepath.Separator)) {
				return errors.New("验证依赖链接指向项目外，无法完整核对环境")
			}
			if _, err := root.Stat(resolved); err != nil {
				return fmt.Errorf("验证依赖链接不可在项目内核对：%s: %w", name, err)
			}
			return walk(resolved)
		}
		if info.IsDir() {
			dir, err := root.Open(name)
			if err != nil {
				return err
			}
			children, err := dir.ReadDir(-1)
			dir.Close()
			if err != nil {
				return err
			}
			sort.Slice(children, func(i, j int) bool { return children[i].Name() < children[j].Name() })
			for _, child := range children {
				if err := walk(filepath.Join(name, child.Name())); err != nil {
					return err
				}
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return errors.New("验证环境包含不可核对的特殊文件")
		}
		total += info.Size()
		if total > 2<<30 {
			return errors.New("验证依赖环境超过2GiB核对上限")
		}
		f, err := root.Open(name)
		if err != nil {
			return err
		}
		_, err = io.Copy(h, f)
		f.Close()
		return err
	}
	scanned := 0
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		scanned++
		if scanned > 20000 {
			return errors.New("验证源码环境扫描目录项超过上限")
		}
		// 与源码快照的排除策略一致，尤其 Windows 上不同大小写仍是
		// 同一依赖目录；不能既从源码排除又漏掉实际执行环境核对。
		base := strings.ToLower(entry.Name())
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			switch base {
			case "node_modules", ".venv", "venv", "vendor":
				if err := walk(name); err != nil {
					return err
				}
				if entry.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
		}
		if entry.IsDir() {
			if name != "." && (supervisionGeneratedPath(name, true) || supervisionSnapshotExclusion(name, true) != "") {
				return fs.SkipDir
			}
		} else {
			// Sensitive runtime configuration is excluded from model snapshots,
			// but its content can affect real checks. Hash it without retaining
			// or displaying the value so old evidence cannot survive a change.
			if supervisionSnapshotExclusion(name, false) != "" {
				return walk(name)
			}
			switch base {
			case "package-lock.json", "pnpm-lock.yaml", "yarn.lock", "go.sum", "Cargo.lock", "requirements.txt", "pyproject.toml":
				return walk(name)
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func supervisionEnvironmentCache(name string) bool {
	base := strings.ToLower(filepath.Base(name))
	switch base {
	case "__pycache__", ".cache", ".pytest_cache", ".ruff_cache", ".mypy_cache":
		return true
	}
	return strings.HasSuffix(base, ".pyc")
}

// 用可重建的真实启动规格核对环境，不把已清理的浏览器临时驱动路径纳入指纹。
func supervisionCheckEnvironment(ctx context.Context, rootPath string, check supervisionCheck) (string, error) {
	spec, err := resolveSupervisionCheckEnvironment(check)
	if err != nil {
		return "", err
	}
	return spec.collect(ctx, rootPath)
}

type supervisionCheckEnvironmentSpec struct {
	Executable string
	Paths      []string
	Browser    bool
}

// Source arguments are already bound by the workspace revision. The runtime
// fingerprint reads only absolute startup files, exactly as the original
// collector does. This lets a large batch share one observation per real
// runtime without conflating distinct executables or absolute startup scripts.
func resolveSupervisionCheckEnvironment(check supervisionCheck) (supervisionCheckEnvironmentSpec, error) {
	var spec supervisionCheckEnvironmentSpec
	var paths []string
	if check.Browser == nil {
		binary, args, err := supervisionNativeVerificationCommand(check.Program, check.Args)
		if err != nil {
			return spec, err
		}
		spec.Executable, paths = binary, args
	} else {
		node, _, err := supervisionNativeVerificationCommand("node", nil)
		if err != nil {
			return spec, err
		}
		server, args, err := supervisionNativeVerificationCommand(check.Browser.ServerProgram, check.Browser.ServerArgs)
		if err != nil {
			return spec, err
		}
		spec.Executable, spec.Browser, paths = node, true, append([]string{server}, args...)
	}
	spec.Paths = []string{}
	for _, name := range paths {
		if filepath.IsAbs(name) && name != spec.Executable && !(filepath.Separator == '\\' && strings.EqualFold(name, spec.Executable)) {
			spec.Paths = append(spec.Paths, name)
		}
	}
	return spec, nil
}

func (spec supervisionCheckEnvironmentSpec) collect(ctx context.Context, rootPath string) (string, error) {
	value, err := supervisionEnvironmentFingerprint(ctx, rootPath, spec.Executable, spec.Paths...)
	if err != nil {
		return "", err
	}
	if spec.Browser {
		value = supervisionSHA256([]byte(value + "\x00" + supervisionBrowserDriver))
	}
	return value, nil
}

func (s *Server) executeSupervisionVerification(ctx context.Context, permit supervisionExecutionPermit, deadline time.Time) error {
	if err := s.requireSupervisionLocalExecution(ctx, permit.Decision.SessionID); err != nil {
		return err
	}
	if err := s.withSupervisionTx(ctx, permit.Decision.SessionID, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		state, err := checkSupervisionExecutionTx(ctx, tx, ss, permit)
		if err != nil {
			return err
		}
		if state != "prepared" {
			return errSupervisionState
		}
		_, err = tx.ExecContext(ctx, `update supervision_executions set state='starting',updated_at=? where execution_id=?`, time.Now().UTC(), permit.Decision.ExecutionID)
		return err
	}); err != nil {
		return err
	}
	j, err := loadSupervisionRoleJob(ctx, s.db, permit.Decision.ExecutionID)
	if err != nil {
		return err
	}
	var action supervisionWorkflowAction
	if err := json.Unmarshal(j.Spec, &action); err != nil {
		return err
	}
	if len(action.Checks) != 1 {
		return errors.New("单工作项只执行一项检查")
	}
	check := action.Checks[0]
	ss, err := loadSupervisionSession(ctx, s.db, permit.Decision.SessionID)
	if err != nil {
		return err
	}
	var rootPath string
	if err := s.db.QueryRowContext(ctx, `select original_path from workspace_admissions where id=?`, ss.AdmissionID).Scan(&rootPath); err != nil {
		return err
	}
	if check.TimeoutMillis > 0 && time.Now().Add(time.Duration(check.TimeoutMillis)*time.Millisecond).Before(deadline) {
		deadline = time.Now().Add(time.Duration(check.TimeoutMillis) * time.Millisecond)
	}
	runCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	sink := &supervisionRoleSink{server: s, permit: permit, cancel: cancel}
	result := supervisionCheckResult{Check: check, ExitCode: -1, NotExecuted: true, StartedAt: time.Now().UTC()}
	var receipt supervisionSchedulerCLIReceipt
	receipt.ContainerQuiet = true
	cwd, runErr := canonicalWorkspacePath(filepath.Join(rootPath, filepath.FromSlash(check.Directory)))
	if runErr == nil {
		canonicalRoot, e := canonicalWorkspacePath(rootPath)
		if e != nil || !isPathWithin(canonicalRoot, cwd) {
			runErr = errors.New("检查目录越出托管项目")
		}
	}
	var binary string
	var args []string
	if runErr == nil && check.Browser == nil {
		binary, args, runErr = supervisionNativeVerificationCommand(check.Program, check.Args)
	}
	var cleanup func()
	var browserPath string
	if runErr == nil && check.Browser != nil {
		binary, args, browserPath, cleanup, runErr = s.prepareSupervisionBrowser(cwd, *check.Browser, permit.Decision.ExecutionID)
	}
	if cleanup != nil {
		defer cleanup()
	}
	result.Executable = binary
	if runErr == nil {
		result.Environment, runErr = supervisionCheckEnvironment(runCtx, rootPath, check)
	}
	stdout, stderr := &supervisionVerificationWriter{sink: sink, stream: "stdout", cancel: cancel}, &supervisionVerificationWriter{sink: sink, stream: "stderr", cancel: cancel}
	monitorDone, monitorStopped := make(chan struct{}), make(chan struct{})
	var monitorErr error
	go func() {
		defer close(monitorStopped)
		monitorErr = s.monitorSupervisionExecution(runCtx, permit, cancel, monitorDone)
	}()
	if runErr == nil {
		cmd := exec.Command(binary, args...)
		cmd.Dir = cwd
		cmd.Env = managedCLIEnvironment(nil, os.Environ(), "CI=1", "PYTHONDONTWRITEBYTECODE=1", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "AUTO_APPROVAL_TOKEN=", "AUTO_CONTROL_URL=")
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		if check.Browser != nil {
			cmd.Stdin = bytes.NewReader(mustJSON(check.Browser))
		}
		receipt.ProcessStarted, receipt.ProcessWaited, receipt.ContainerQuiet, runErr = runSupervisionContainedCommand(runCtx, cmd, permit.Container, func(id supervisionProcessIdentity, resume func() error) error {
			if err := s.registerSupervisionProcess(runCtx, permit, id, time.Now().UTC()); err != nil {
				return err
			}
			return s.resumeSupervisionProcess(runCtx, permit, func() error {
				// Creating a suspended process is not an executed check. Record
				// that fact only after the authorized resume actually succeeds.
				err := resume()
				if err == nil {
					result.NotExecuted = false
				}
				return err
			}, time.Now().UTC())
		})
		if runErr == nil {
			result.ExitCode = 0
		} else {
			var exit *supervisionExitError
			if errors.As(runErr, &exit) {
				result.ExitCode = exit.Code
			}
		}
	}
	close(monitorDone)
	<-monitorStopped
	runErr = errors.Join(runErr, monitorErr, sink.failure())
	result.Stdout, result.Stderr = redactAgentText(string(stdout.data)), redactAgentText(string(stderr.data))
	result.Truncated = stdout.truncated || stderr.truncated
	result.FinishedAt = time.Now().UTC()
	if check.Browser != nil && browserPath != "" {
		data, e := os.ReadFile(browserPath)
		if e == nil && len(data) < 256<<10 && json.Valid(data) {
			result.BrowserResult = json.RawMessage(redactAgentText(string(data)))
			var report struct {
				Passed bool `json:"passed"`
				Steps  []struct {
					Passed bool `json:"passed"`
				} `json:"steps"`
			}
			image, imageErr := os.ReadFile(filepath.Join(filepath.Dir(browserPath), "screenshot.png"))
			valid := json.Unmarshal(data, &report) == nil && report.Passed && len(report.Steps) == len(check.Browser.Steps) && imageErr == nil && len(image) >= 8 && len(image) <= 20<<20 && bytes.Equal(image[:8], []byte{137, 80, 78, 71, 13, 10, 26, 10})
			for _, step := range report.Steps {
				valid = valid && step.Passed
			}
			if !valid && runErr == nil {
				runErr = errors.New("浏览器报告或截图缺失、不完整或未通过")
			}
			if imageErr == nil {
				result.ScreenshotSHA256 = supervisionSHA256(image)
			}
		} else if runErr == nil {
			runErr = errors.New("浏览器没有留下完整的实际执行报告")
		}
	}
	if runErr == nil {
		after, e := supervisionCheckEnvironment(runCtx, rootPath, check)
		if e != nil {
			runErr = e
		} else if after != result.Environment {
			runErr = errors.New("测试过程中依赖环境变化，证据失效")
		}
	}
	if runErr != nil {
		result.Error = redactAgentText(runErr.Error())
	}
	settleCtx, settleCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer settleCancel()
	if check.Browser != nil {
		runErr = errors.Join(runErr, s.registerSupervisionArtifacts(settleCtx, ss.ID, j.ExecutionID))
	}
	if err := s.finishSupervisionExecution(settleCtx, permit, receipt, runErr, time.Now().UTC()); err != nil {
		return errors.Join(runErr, err)
	}
	if !receipt.ContainerQuiet {
		return errWorkspaceNotQuiescent
	}
	return s.finishSupervisionVerification(settleCtx, permit, j, result, runErr)
}

func (s *Server) finishSupervisionVerification(ctx context.Context, permit supervisionExecutionPermit, j supervisionRoleJob, result supervisionCheckResult, runErr error) error {
	// Bind the unmodified input before computing interruption from current
	// control state. A retry after pause/version changes must not create a
	// different historical result, a second evidence row or a new state version.
	inputHash := supervisionSHA256(mustJSON(struct {
		TaskID, Kind, InputRevision, Spec string
		Requirements, Plan                int64
		Result                            supervisionCheckResult
		Failure                           string
		Cancelled                         bool
	}{j.TaskID, j.Kind, j.InputRevision, string(j.Spec), j.RequirementsRevision, j.PlanRevision, result, redactAgentText(errorString(runErr)), errors.Is(runErr, context.Canceled)}))
	err := s.withSupervisionTx(ctx, permit.Decision.SessionID, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		executionState, err := checkSupervisionExecutionIdentityTx(ctx, tx, ss.ID, permit)
		if err != nil {
			return err
		}
		actual, err := loadSupervisionRoleJob(ctx, tx, permit.Decision.ExecutionID)
		if err != nil {
			return err
		}
		if permit.Role != "test" || j.ExecutionID != permit.Decision.ExecutionID || actual.Kind != "verification" || j.Kind != actual.Kind || j.TaskID != permit.TaskID || j.TaskID != actual.TaskID || j.RequirementsRevision != actual.RequirementsRevision || j.PlanRevision != actual.PlanRevision || j.InputRevision != actual.InputRevision || string(j.Spec) != string(actual.Spec) {
			return errSupervisionState
		}
		var action supervisionWorkflowAction
		if json.Unmarshal(actual.Spec, &action) != nil || len(action.Checks) != 1 {
			return errSupervisionState
		}
		expected := action.Checks[0]
		observedCheck := result.Check
		if expected.Directory == "" {
			expected.Directory = "."
		}
		if observedCheck.Directory == "" {
			observedCheck.Directory = "."
		}
		if string(mustJSON(expected)) != string(mustJSON(observedCheck)) {
			return errSupervisionState
		}
		var previous string
		if err := tx.QueryRowContext(ctx, `select completion_input_sha256 from supervision_role_jobs where execution_id=?`, j.ExecutionID).Scan(&previous); err != nil {
			return err
		}
		if previous != "" {
			if previous != inputHash || executionState != "quiescent" || actual.State != "completed" && actual.State != "failed" && actual.State != "interrupted" {
				return errSupervisionState
			}
			return nil
		}
		if executionState != "quiescent" || actual.State != "running" {
			return errWorkspaceNotQuiescent
		}
		if _, err := checkSupervisionExecutionTx(ctx, tx, ss, permit); err != nil {
			return err
		}
		result.Interrupted = result.Interrupted || ss.Control != "" || ss.StateVersion != permit.Decision.ExpectedStateVersion || errors.Is(runErr, context.Canceled)
		passed := runErr == nil && result.Error == "" && result.ExitCode == 0 && !result.Truncated && !result.Interrupted && !result.NotExecuted
		state, phase := "completed", "verified"
		if !passed {
			state, phase = "failed", "needs_repair"
		}
		if result.Interrupted {
			state = "interrupted"
			if result.Error == "" {
				result.Error = "控制或授权版本已变化，检查结果仅作为历史保存，可在重新授权后运行"
			}
		}
		kind := "command"
		if result.Check.Browser != nil {
			kind = "browser"
		}
		raw := mustJSON(result)
		if _, err := tx.ExecContext(ctx, `insert into supervision_evidence(id,session_id,task_id,execution_id,kind,revision_id,requirements_revision,plan_revision,passed,details,created_at) values (?,?,?,?,?,?,?,?,?,?,?)`, uuid.NewString(), ss.ID, j.TaskID, j.ExecutionID, kind, j.InputRevision, j.RequirementsRevision, j.PlanRevision, passed, string(raw), time.Now().UTC()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `update supervision_role_jobs set completion_input_sha256=? where execution_id=?`, inputHash, j.ExecutionID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `update supervision_role_jobs set state=?,result=?,error=? where execution_id=?;update supervision_work_items set result_text=?,result_state=?,result_error=?,result_received_at=? where execution_id=?;update supervision_decisions set state='applied',completed_at=? where id=?;update supervision_tasks set phase=? where session_id=? and task_id=?`, state, string(raw), result.Error, j.ExecutionID, string(raw), state, result.Error, time.Now().UTC(), j.ExecutionID, time.Now().UTC(), permit.Decision.ID, phase, ss.ID, j.TaskID); err != nil {
			return err
		}
		if state == "failed" {
			if err := reopenSupervisionFinalRepairTx(ctx, tx, ss, j, time.Now().UTC()); err != nil {
				return err
			}
		}
		if state != "interrupted" {
			if err := queueSupervisionRoleObservationTx(ctx, tx, ss.ID, j, j.InputRevision); err != nil {
				return err
			}
		}
		ss.StateVersion++
		return appendSupervisionEventTx(ctx, tx, ss, "verification.completed", map[string]any{"taskId": j.TaskID, "executionId": j.ExecutionID, "passed": passed, "result": result}, time.Now().UTC())
	})
	if err != nil {
		return err
	}
	return s.finishSupervisionRoleObservation(ctx, permit.Decision.SessionID, j.ExecutionID)
}
