package app

// Ordinary CLI execution uses the same kernel containment as supervision, but
// keeps the ordinary CLI's arguments, credentials, approvals and session I/O.
import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

type ordinaryCLIContextKey struct{}
type ordinaryCLIExecution struct {
	config Config
	ticket workspaceGuardRecord
}

func ordinaryCLIConfig(ctx context.Context) Config {
	if launch, ok := ctx.Value(ordinaryCLIContextKey{}).(ordinaryCLIExecution); ok {
		return launch.config
	}
	return ConfigFromEnv()
}

func ordinaryCLIContext(ctx context.Context, config Config, ticket workspaceGuardRecord) context.Context {
	return context.WithValue(ctx, ordinaryCLIContextKey{}, ordinaryCLIExecution{config, ticket})
}

func (s *Server) beginCLIWorkspaceOperationAtPath(ctx context.Context, project Project, path, owner string) (context.Context, func(), error) {
	if project.RunnerID != "" {
		project.Runner = project.RunnerID
	}
	key := "path:" + filepath.Clean(path)
	if !isLocalRunnerID(project.Runner) {
		key = "remote:" + project.Runner + ":" + path
	}
	s.rememberWorkspaceScope(key, path, project.Runner)
	releaseMemory, err := s.beginWorkspaceOperationMode(key, owner, false)
	if err != nil {
		return ctx, nil, err
	}
	s.workspaceAdmissionMu.Lock()
	scope := s.workspaceScopeLocked(key)
	s.workspaceAdmissionMu.Unlock()
	var ticket workspaceGuardRecord
	releaseShared, err := s.beginSharedWorkspaceOperation(scope, &ticket)
	if err != nil {
		releaseMemory()
		return ctx, nil, err
	}
	var once sync.Once
	return ordinaryCLIContext(ctx, s.config, ticket), func() { once.Do(func() { releaseShared(); releaseMemory() }) }, nil
}

func (s *Server) runTrackedOrdinaryCLI(ctx context.Context, runner AgentRunner, runnerID string, request AgentRunRequest, sink AgentRunSink) error {
	var ticket workspaceGuardRecord
	release, err := s.beginSharedWorkspaceOperation(workspaceScope{Path: request.ProjectPath, OriginalPath: request.ProjectPath, RunnerID: runnerID}, &ticket)
	if err != nil {
		return err
	}
	defer release()
	return runner.Run(ordinaryCLIContext(ctx, s.config, ticket), request, sink)
}

type ordinaryCLIProcess struct {
	process supervisionContainedProcess
	done    chan struct{}
	err     error
}

var ordinaryCLIProcesses sync.Map      // *os.Process -> *ordinaryCLIProcess; removed when its Cmd is collected
var ordinaryCLITransportStops sync.Map // *exec.Cmd -> context.CancelFunc

func ordinaryCLICommandState(cmd *exec.Cmd) (*ordinaryCLIProcess, bool) {
	if cmd == nil || cmd.Process == nil {
		return nil, false
	}
	value, ok := ordinaryCLIProcesses.Load(cmd.Process)
	runtime.KeepAlive(cmd)
	if !ok {
		return nil, false
	}
	return value.(*ordinaryCLIProcess), true
}

func stopOrdinaryCLITransport(cmd *exec.Cmd) {
	if stop, ok := ordinaryCLITransportStops.LoadAndDelete(cmd); ok {
		stop.(context.CancelFunc)()
	}
}

func startOrdinaryCLITransport(cmd *exec.Cmd) error {
	err := cmd.Start()
	if err != nil {
		stopOrdinaryCLITransport(cmd)
	}
	return err
}

func stopOrdinaryCLICommand(cmd *exec.Cmd) bool {
	if state, ok := ordinaryCLICommandState(cmd); ok {
		_ = state.process.terminate()
		return true
	}
	return false
}

func closeOrdinaryCLIPipes(cmd *exec.Cmd) {
	// Std{in,out,err}Pipe leave their child ends on Cmd until Start/Wait. Here
	// the contained launcher owns Start/Wait, so it also closes those ends.
	for _, endpoint := range []any{cmd.Stdin, cmd.Stdout, cmd.Stderr} {
		if file, ok := endpoint.(*os.File); ok && file != os.Stdin && file != os.Stdout && file != os.Stderr {
			_ = file.Close()
		}
	}
}

func startOrdinaryCLICommand(ctx context.Context, cmd *exec.Cmd, fallback Config) error {
	if cmd == nil || cmd.Process != nil {
		return errors.New("CLI 进程已经启动或无效")
	}
	if cmd.Err != nil {
		closeOrdinaryCLIPipes(cmd)
		return cmd.Err
	}
	if err := ctx.Err(); err != nil {
		closeOrdinaryCLIPipes(cmd)
		return err
	}
	launch, configured := ctx.Value(ordinaryCLIContextKey{}).(ordinaryCLIExecution)
	if !configured {
		launch.config = fallback
	}
	config, record := launch.config, launch.ticket
	if cmd.Dir == "" {
		cmd.Dir, _ = os.Getwd()
	}
	var err error
	cmd.Dir, err = filepath.Abs(cmd.Dir)
	if err != nil {
		closeOrdinaryCLIPipes(cmd)
		return err
	}
	if !filepath.IsAbs(cmd.Path) {
		cmd.Path, err = exec.LookPath(cmd.Path)
		if err != nil {
			closeOrdinaryCLIPipes(cmd)
			return err
		}
		cmd.Path, err = filepath.Abs(cmd.Path)
		if err != nil {
			closeOrdinaryCLIPipes(cmd)
			return err
		}
	}
	if record.ID == "" {
		record = workspaceGuardRecord{ID: uuid.NewString(), Kind: "operation", Path: cmd.Dir, ValidUntil: time.Now().UTC().Add(45 * time.Second), CLIContainer: uuid.NewString()}
		record, err = changeWorkspaceGuard(ctx, config, "begin", record)
		if err != nil {
			closeOrdinaryCLIPipes(cmd)
			return err
		}
	}
	if record.CLIContainer == "" || !validSupervisionContainer(supervisionJobPrefix+record.CLIContainer) {
		closeOrdinaryCLIPipes(cmd)
		return targetexec.ErrAuthority
	}
	// A remote admission ticket can name a symlink alias. Use the target's
	// canonical, identity-bound directory for creation and immutable cleanup.
	err = withWorkspaceGuard(ctx, config, func(f *workspaceGuardFacts) error {
		prior, ok := f.Records[record.ID]
		if !ok || prior.State != "active" || prior.CLIStarted || prior.CLIContainer != record.CLIContainer || record.JournalID != "" && record.JournalID != f.ID || !prior.ValidUntil.After(time.Now().UTC()) || !prior.CLILeaseUntil.IsZero() && !prior.CLILeaseUntil.After(time.Now().UTC()) {
			return targetexec.ErrAuthority
		}
		actual, err := canonicalWorkspacePath(cmd.Dir)
		if err != nil || actual != prior.Path {
			return targetexec.ErrConflict
		}
		record = prior
		record.JournalID = f.ID
		cmd.Dir = prior.Path
		return ctx.Err()
	})
	if err != nil {
		closeOrdinaryCLIPipes(cmd)
		return err
	}
	// .cmd/.bat shims must themselves start inside the atomic Windows Job.
	if runtime.GOOS == "windows" && (strings.EqualFold(filepath.Ext(cmd.Path), ".cmd") || strings.EqualFold(filepath.Ext(cmd.Path), ".bat")) {
		wrapped := codexCommandContext(context.Background(), cmd.Path, cmd.Args[1:]...)
		cmd.Path, cmd.Args = wrapped.Path, wrapped.Args
	}
	process, startErr := startOrdinaryContainedProcess(cmd, supervisionJobPrefix+record.CLIContainer)
	if process == nil {
		closeOrdinaryCLIPipes(cmd)
		_, releaseErr := changeWorkspaceGuard(context.Background(), config, "release", record)
		return errors.Join(startErr, releaseErr)
	}
	// Persist identity before resuming. Released/expired tokens cannot execute,
	// even if cancellation or recovery overtook the creation of a held process.
	if startErr == nil {
		startErr = withWorkspaceGuard(ctx, config, func(f *workspaceGuardFacts) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			prior, ok := f.Records[record.ID]
			if !ok || prior.State != "active" || prior.CLIStarted || prior.CLIContainer != record.CLIContainer || record.JournalID != "" && record.JournalID != f.ID || !prior.ValidUntil.After(time.Now().UTC()) || !prior.CLILeaseUntil.IsZero() && !prior.CLILeaseUntil.After(time.Now().UTC()) {
				return targetexec.ErrAuthority
			}
			actual, err := canonicalWorkspacePath(cmd.Dir)
			if err != nil || actual != prior.Path {
				return targetexec.ErrConflict
			}
			identity, err := workspaceDirectoryIdentity(actual)
			if err != nil || identity != prior.Identity {
				return targetexec.ErrConflict
			}
			prior.CLIStarted = true
			identityFacts := process.identity()
			prior.CLIProcess = &identityFacts
			f.Records[prior.ID] = prior
			return nil
		})
	}
	if startErr == nil {
		startErr = withWorkspaceGuard(ctx, config, func(f *workspaceGuardFacts) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			prior := f.Records[record.ID]
			if prior.State != "active" || !prior.CLIStarted || prior.CLIContainer != record.CLIContainer || !prior.CLILeaseUntil.IsZero() && !prior.CLILeaseUntil.After(time.Now().UTC()) {
				return targetexec.ErrAuthority
			}
			return process.resume()
		})
	}
	if startErr != nil {
		_ = process.terminate()
	}
	state := &ordinaryCLIProcess{process: process, done: make(chan struct{})}
	leaseFailure := make(chan error, 1)
	if startErr == nil {
		cmd.Process, err = os.FindProcess(process.identity().PID)
		if err != nil {
			startErr = err
			_ = process.terminate()
		}
	}
	if cmd.Process != nil {
		ordinaryCLIProcesses.Store(cmd.Process, state)
		// Keep the association for the entire Cmd lifetime: a late Stop must
		// never fall back to killing a recycled PID after this container ended.
		// The map key is Process, so it does not keep Cmd alive itself.
		runtime.AddCleanup(cmd, func(key *os.Process) { ordinaryCLIProcesses.Delete(key) }, cmd.Process)
	}
	go func() {
		defer close(state.done)
		defer process.close()
		waitErr := process.wait()
		stopErr := process.terminate()
		settle, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		quietErr := waitSupervisionProcessContainer(settle, process)
		cancel()
		closeOrdinaryCLIPipes(cmd)
		var leaseErr error
		if !record.CLILeaseUntil.IsZero() {
			leaseErr = withWorkspaceGuard(context.Background(), config, func(f *workspaceGuardFacts) error {
				prior, ok := f.Records[record.ID]
				if !ok || prior.CLIContainer != record.CLIContainer || !prior.CLILeaseUntil.After(time.Now().UTC()) {
					return targetexec.ErrAuthority
				}
				return nil
			})
		}
		select {
		case failure := <-leaseFailure:
			leaseErr = errors.Join(leaseErr, failure)
		default:
		}
		var releaseErr error
		if quietErr == nil {
			_, releaseErr = changeWorkspaceGuard(context.Background(), config, "release", record)
		}
		// systemd can report a deliberately stopped service as exit 0. Preserve
		// controller cancellation and lease loss independently of its exit code.
		state.err = errors.Join(ctx.Err(), startErr, waitErr, stopErr, quietErr, leaseErr, releaseErr)
	}()
	if !record.CLILeaseUntil.IsZero() {
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-state.done:
					return
				case <-ticker.C:
					probe, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					leaseErr := withWorkspaceGuard(probe, config, func(f *workspaceGuardFacts) error {
						prior := f.Records[record.ID]
						if prior.State == "released" {
							return nil
						}
						if prior.State != "active" || prior.CLIContainer != record.CLIContainer || !prior.CLILeaseUntil.After(time.Now().UTC()) {
							return targetexec.ErrAuthority
						}
						return nil
					})
					cancel()
					if leaseErr != nil {
						select {
						case leaseFailure <- leaseErr:
						default:
						}
						_ = process.terminate()
						return
					}
				}
			}
		}()
	}
	go func() {
		select {
		case <-ctx.Done():
			_ = process.terminate()
		case <-state.done:
		}
	}()
	if startErr != nil {
		<-state.done
		if cmd.Process != nil {
			ordinaryCLIProcesses.Delete(cmd.Process)
		}
		return state.err
	}
	return nil
}

func waitOrdinaryCLICommand(cmd *exec.Cmd) error {
	defer stopOrdinaryCLITransport(cmd)
	state, ok := ordinaryCLICommandState(cmd)
	if !ok {
		return cmd.Wait()
	} // WSL transport commands still use os/exec.
	<-state.done
	if cmd.Process != nil {
		// Reap the duplicate os.Process handle and close Cmd's original parent
		// pipe ends. Linux's service main is not our direct child (ECHILD is
		// expected); the containment wait above is the authoritative result.
		_ = cmd.Wait()
		_ = cmd.Process.Release()
	}
	runtime.KeepAlive(cmd)
	return state.err
}

func (r *wslAgentRunner) wslOrdinaryCLICommand(ctx context.Context, cli string, args, env []string, directory string, extraForward ...string) (*exec.Cmd, error) {
	script := r.wslPathPrefix() + "; cd " + shellQuote(directory) + " || exit 1; exec " + shellQuote(cli)
	for _, arg := range args {
		script += " " + shellQuote(arg)
	}
	wrapped, stop, err := ordinaryCLIRemoteScript(ctx, r.config, r, directory, script)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "wsl.exe", "-d", r.distro, "--exec", "/bin/sh", "-c", wrapped)
	cmdEnv, wslenv := wslBuildEnv(env, extraForward...)
	if wslenv != "" {
		cmdEnv = append(cmdEnv, "WSLENV="+wslenv)
	}
	cmd.Env = cmdEnv
	configureProcessGroup(cmd)
	ordinaryCLITransportStops.Store(cmd, stop)
	return cmd, nil
}

// RunOrdinaryCLI is the remote helper's transparent stdio entry point. The
// ticket contains no command or credentials. Real CLI argv remains positional.
func RunOrdinaryCLI(ctx context.Context, config Config, encodedTicket string, argv []string, input io.Reader, output, stderr io.Writer) error {
	if len(argv) == 0 {
		return errors.New("缺少普通 CLI 命令")
	}
	var ticket workspaceGuardRecord
	if encodedTicket != "" {
		raw, err := base64.RawURLEncoding.DecodeString(encodedTicket)
		if err != nil || json.Unmarshal(raw, &ticket) != nil || ticket.ID == "" || ticket.CLIContainer == "" || ticket.JournalID == "" {
			return targetexec.ErrProtocol
		}
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = ticket.Path
	cmd.Stdin, cmd.Stdout, cmd.Stderr = input, output, stderr
	if err := startOrdinaryCLICommand(ordinaryCLIContext(ctx, config, ticket), cmd, config); err != nil {
		return err
	}
	return waitOrdinaryCLICommand(cmd)
}

func ordinaryCLIRemoteScript(ctx context.Context, config Config, runner AgentRunner, directory, script string) (string, context.CancelFunc, error) {
	// Deploy using the same verified bundle/target identity as common admission.
	launch, _ := ctx.Value(ordinaryCLIContextKey{}).(ordinaryCLIExecution)
	ticket := launch.ticket
	var link *supervisionTargetLink
	var err error
	if ticket.helperSHA256 != "" {
		link, err = connectOrdinaryCLIInstalledTarget(ctx, runner, ticket)
	} else {
		link, err = openSupervisionTargetMode(ctx, config, runner, ticket.JournalID, "", "workspace-guard")
	}
	if err != nil {
		return "", nil, err
	}
	keepLink := false
	defer func() {
		if !keepLink {
			_ = link.Close()
		}
	}()
	encoded := ""
	if ticket.ID == "" {
		deadline, err := link.Client.TargetDeadline(30 * time.Second)
		if err != nil {
			return "", nil, err
		}
		ticket = workspaceGuardRecord{ID: uuid.NewString(), Kind: "operation", Path: directory, ValidUntil: deadline, JournalID: link.TargetID, CLIContainer: uuid.NewString(), CLILeaseUntil: deadline}
		response, err := link.Call(ctx, targetexec.Request{ID: uuid.NewString(), Method: "guard.begin", Parameters: mustJSON(ticket)})
		if err != nil {
			return "", nil, err
		}
		if json.Unmarshal(response.Data, &ticket) != nil || ticket.State != "active" || ticket.CLIContainer == "" {
			return "", nil, targetexec.ErrProtocol
		}
		ticket.JournalID = link.TargetID
	}
	if ticket.ID != "" {
		if ticket.JournalID != link.TargetID {
			return "", nil, targetexec.ErrAuthority
		}
		encoded = base64.RawURLEncoding.EncodeToString(mustJSON(ticket))
	}
	if len(link.Artifact.SHA256) != 64 {
		return "", nil, targetexec.ErrProtocol
	}
	// Existing session admission tickets also acquire a finite remote lease.
	if ticket.CLILeaseUntil.IsZero() {
		return "", nil, errors.New("跨端 CLI 启动票据缺少执行租约")
	}
	heartbeat, cancel := context.WithCancel(ctx)
	keepLink = true
	go func() {
		defer link.Close()
		defer func() {
			// Closing stdin/SSH/wsl.exe alone is not an execution stop receipt.
			// Request target-side containment stop; the finite lease backs this
			// up when the transport has already gone away.
			probe, done := context.WithTimeout(context.Background(), 3*time.Second)
			defer done()
			_, _ = link.Call(probe, targetexec.Request{ID: uuid.NewString(), Method: "guard.stop-cli", Parameters: mustJSON(workspaceGuardRecord{ID: ticket.ID, Kind: "operation", JournalID: ticket.JournalID, CLIContainer: ticket.CLIContainer})})
		}()
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeat.Done():
				return
			case <-ticker.C:
				probe, done := context.WithTimeout(heartbeat, 3*time.Second)
				_, err := link.Call(probe, targetexec.Request{ID: uuid.NewString(), Method: "guard.renew-cli", Parameters: mustJSON(workspaceGuardRecord{ID: ticket.ID, Kind: "operation", JournalID: ticket.JournalID, CLIContainer: ticket.CLIContainer})})
				done()
				if err != nil {
					return
				}
			}
		}
	}()
	// Keep HOME expansion in trusted fixed code; the content digest is verified
	// both during deployment and immediately before exec.
	prefix := `set -eu; file="$HOME/.local/share/milevia/supervision-targets/` + link.Artifact.SHA256 + `/helper"; test ! -L "$file"; printf '%s  %s\n' ` + shellQuote(link.Artifact.SHA256) + ` "$file" | sha256sum -c - >/dev/null; exec "$file" --mode ordinary-cli --ordinary-ticket ` + shellQuote(encoded) + ` -- /bin/sh -c ` + shellQuote(script)
	return prefix, cancel, nil
}

func connectOrdinaryCLIInstalledTarget(ctx context.Context, runner AgentRunner, ticket workspaceGuardRecord) (*supervisionTargetLink, error) {
	var channel *supervisionTargetPipe
	var err error
	switch r := runner.(type) {
	case *wslAgentRunner:
		channel, err = r.supervisionTargetChannel(ctx, ticket.helperSHA256, "", "workspace-guard")
	case *sshRunner:
		channel, err = r.supervisionTargetChannel(ctx, ticket.helperSHA256, "", "workspace-guard")
	default:
		return nil, errSupervisionProcessUnsupported
	}
	if err != nil {
		return nil, err
	}
	client := targetexec.NewClient(channel)
	probe, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	hello, err := client.Call(probe, targetexec.Request{ID: "hello", Method: "hello"})
	if err != nil || hello.TargetID != ticket.JournalID {
		client.Close()
		return nil, errors.Join(targetexec.ErrAuthority, err)
	}
	response, err := client.Call(probe, targetexec.Request{ID: "health", Method: "health"})
	var health struct {
		Protocol int  `json:"protocol"`
		Guard    bool `json:"workspaceGuard"`
	}
	if err != nil || json.Unmarshal(response.Data, &health) != nil || health.Protocol != targetexec.ProtocolVersion || !health.Guard {
		client.Close()
		return nil, errors.Join(targetexec.ErrProtocol, err)
	}
	return &supervisionTargetLink{Client: client, TargetID: hello.TargetID, Artifact: targetexec.HelperArtifact{SHA256: ticket.helperSHA256}}, nil
}

func OrdinaryCLIExitCode(err error) int {
	var exit *supervisionExitError
	if errors.As(err, &exit) && exit.Code > 0 && exit.Code < 256 {
		return exit.Code
	}
	return 125
}
