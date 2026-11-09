package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

// ServeSupervisionTarget is an authenticated pipe endpoint, invoked via the
// existing local/WSL/SSH execution identity. It owns no task plans or Task.done.
// All CLI credentials are resolved in this environment through existing runners.
func ServeSupervisionTarget(ctx context.Context, config Config, channel io.ReadWriteCloser) error {
	if config.DataDir == "" {
		return errors.New("目标执行服务需要独立数据目录")
	}
	stateDir := filepath.Join(config.DataDir, "supervision-target")
	store, err := targetexec.Open(stateDir)
	if err != nil {
		return err
	}
	defer store.Close()
	runtime := &supervisionTargetRuntime{store: store, storage: config.DataDir, config: config, paths: newAgentPathResolver(config)}
	facts, err := store.Facts()
	if err != nil {
		return err
	}
	for id, session := range facts.Sessions {
		if !session.Claimed || session.Released {
			continue
		}
		workspace, err := store.Workspace(session.Control.WorkspaceID)
		if err != nil {
			return err
		}
		if _, err := changeWorkspaceGuard(ctx, config, "reserve", workspaceGuardRecord{ID: id, Kind: "supervision", Path: workspace.Path, Identity: workspace.ID}); err != nil {
			return err
		}
	}
	supervisor, err := targetexec.NewSupervisor(ctx, store, runtime)
	if err != nil {
		return err
	}
	transport := &targetexec.Transport{Store: store, Supervisor: supervisor, Extension: runtime.handle, ValidateControl: runtime.validateControl, ValidateCredentials: runtime.validateCredentials, AfterRelease: func(control targetexec.Control) error {
		_, err := runtime.handleWorkspaceAdmission(ctx, targetexec.Request{Method: "workspace.release", SessionID: control.SessionID})
		return err
	}, Diagnostic: func(err error) string { return redactAgentText(err.Error()) }}
	return transport.Serve(ctx, channel)
}

type supervisionTargetRuntime struct {
	store   *targetexec.Store
	storage string
	config  Config
	paths   *agentPathResolver
}

func (r *supervisionTargetRuntime) adapter(agentID string) (supervisionCLIAdapter, error) {
	return r.adapterWithPaths(agentID, r.paths)
}

// Each execution freezes the CLI-management registration it actually observed.
// A shared owner's bootstrap path map must not keep an old installation alive
// after CLI management changes or removes that registration.
func (r *supervisionTargetRuntime) executionAdapter(agentID, installationPath string) (supervisionCLIAdapter, error) {
	if installationPath != "" && !supervisionTargetAbsolutePath(runtime.GOOS, installationPath) {
		return nil, targetexec.ErrConflict
	}
	paths := newAgentPathResolver(r.config)
	if installationPath != "" {
		paths.load(map[string]string{agentID: installationPath})
	}
	return r.adapterWithPaths(agentID, paths)
}

func (r *supervisionTargetRuntime) adapterWithPaths(agentID string, paths *agentPathResolver) (supervisionCLIAdapter, error) {
	var runner AgentRunner
	switch agentID {
	case "claude-code":
		runner = newClaudeCLIRunner(r.config, paths)
	case "codex":
		runner = newCodexCLIRunner(r.config, paths)
	case "codebuddy":
		runner = newCodebuddyManageRunner(r.config, paths)
	case "qoder":
		runner = newQoderManageRunner(r.config, paths)
	case "opencode":
		runner = newOpenCodeManageRunner(r.config, paths)
	case "cursor":
		runner = newCursorCLIRunner(r.config, paths)
	default:
		return nil, errors.New("未知的托管 CLI")
	}
	adapter, ok := runner.(supervisionCLIAdapter)
	if !ok {
		return nil, errors.New("目标 CLI 的受控执行适配尚待接入")
	}
	return adapter, nil
}

func (r *supervisionTargetRuntime) workspace(id string) (WorkspaceAdmission, error) {
	workspace, err := r.store.Workspace(id)
	if err != nil {
		return WorkspaceAdmission{}, err
	}
	admission := WorkspaceAdmission{OriginalPath: workspace.Path, CanonicalPath: workspace.Path, DirectoryIdentity: workspace.ID}
	return admission, verifyWorkspaceAdmissionPath(admission)
}

func (r *supervisionTargetRuntime) validateControl(control targetexec.Control) error {
	if (control.State == "paused" || control.State == "stopped") && control.DrainExecutionID == "" {
		// Revocation must remain possible after the directory was renamed,
		// removed or replaced. It grants no filesystem access or new execution;
		// the durable store still checks the existing session/workspace binding.
		_, err := r.store.Workspace(control.WorkspaceID)
		return err
	}
	if control.GuardID != "" && control.GuardID != control.SessionID {
		return targetexec.ErrConflict
	}
	workspace, err := r.workspace(control.WorkspaceID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := changeWorkspaceGuard(ctx, r.config, "reserve", workspaceGuardRecord{ID: control.SessionID, Kind: "supervision", Path: workspace.CanonicalPath, Identity: workspace.DirectoryIdentity}); err != nil {
		return err
	}
	_, err = changeWorkspaceGuard(ctx, r.config, "own", workspaceGuardRecord{ID: control.SessionID, Kind: "supervision"})
	return err
}

func (r *supervisionTargetRuntime) handle(ctx context.Context, request targetexec.Request) (json.RawMessage, error) {
	switch request.Method {
	case "workspace.reserve", "workspace.release":
		return r.handleWorkspaceAdmission(ctx, request)
	case "health":
		if err := supervisionLocalProcessCapability(); err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"protocol": targetexec.ProtocolVersion, "os": runtime.GOOS, "arch": runtime.GOARCH, "processContainment": true})
	case "configure.paths":
		// This authenticated bootstrap carries only existing CLI management
		// records. Never copy controller credentials or put paths into a shell.
		var paths map[string]string
		if len(request.Parameters) > 32<<10 || json.Unmarshal(request.Parameters, &paths) != nil || len(paths) > len(agentCatalogEntries) {
			return nil, targetexec.ErrProtocol
		}
		for agentID, path := range paths {
			if _, known := agentByID(agentID); !known || !filepath.IsAbs(path) || len(path) > 4096 || strings.ContainsAny(path, "\x00\r\n") {
				return nil, targetexec.ErrConflict
			}
		}
		if r.paths == nil {
			r.paths = newAgentPathResolver(r.config)
		}
		// Replace the full recorded map: a removed registration must not linger
		// in a reused target process. Explicit target environment overrides retain
		// the same precedence as ordinary CLI management.
		r.paths.load(paths)
		return json.RawMessage(`{}`), nil
	case "capability":
		if err := supervisionLocalProcessCapability(); err != nil {
			return nil, err
		}
		adapter, err := r.adapter(request.AgentID)
		if err != nil {
			return nil, err
		}
		probe := true
		if len(request.Parameters) > 0 {
			var options struct {
				Probe bool `json:"probe"`
			}
			if json.Unmarshal(request.Parameters, &options) != nil {
				return nil, targetexec.ErrProtocol
			}
			probe = options.Probe
		}
		info, err := adapter.supervisionCLIInfo(ctx, probe)
		if err != nil {
			return nil, err
		}
		return json.Marshal(info)
	case "execution.resolve":
		return r.resolveExecution(ctx, request)
	case "workspace":
		if !filepath.IsAbs(request.Path) {
			return nil, targetexec.ErrConflict
		}
		canonical, err := canonicalWorkspacePath(request.Path)
		if err != nil {
			return nil, err
		}
		identity, err := workspaceDirectoryIdentity(canonical)
		if err != nil {
			return nil, err
		}
		storage, err := canonicalWorkspacePath(r.storage)
		if err != nil {
			return nil, err
		}
		if canonical == storage || isPathWithin(storage, canonical) || isPathWithin(canonical, storage) {
			return nil, errors.New("目标项目与应用数据目录重叠")
		}
		workspace := targetexec.Workspace{ID: identity, Path: canonical}
		if err := r.store.RegisterWorkspace(workspace); err != nil {
			return nil, err
		}
		return json.Marshal(workspace)
	case "snapshot", "read", "blob", "mutate", "mutation.query", "environment", "snapshot.reconcile", "blob.reconcile":
		return r.handleFiles(ctx, request)
	case "finalize", "finalize.clear":
		return r.handleFinalization(ctx, request)
	case "artifact":
		return r.handleArtifact(ctx, request)
	case "artifact.clear":
		return r.clearArtifacts(ctx, request)
	case "events.clear":
		return nil, r.store.ClearReleasedEvents(request.SessionID, request.OwnerEpoch, request.AuthorizationVersion)
	default:
		return nil, targetexec.ErrProtocol
	}
}

func (r *supervisionTargetRuntime) Run(ctx context.Context, execution targetexec.Execution, input targetexec.Input, handshake targetexec.Handshake) (targetexec.Receipt, error) {
	if execution.Spec.Role == "test" {
		return r.runVerification(ctx, execution, input, handshake)
	}
	result := targetexec.Receipt{Quiet: true}
	if err := validateSupervisionTargetCLIInput(execution.Spec, input); err != nil {
		return result, err
	}
	if _, err := r.workspace(execution.Spec.WorkspaceID); err != nil {
		return result, err
	}
	if input.Instructions != supervisionRolePrompt(execution.Spec.Role) || execution.Spec.CLIPath == "" || execution.Spec.CLIVersion == "" {
		return result, targetexec.ErrConflict
	}
	profile, err := supervisionTargetRuntimeProfile(execution.Spec, targetexec.ExecutionCredentials(ctx))
	if err != nil {
		return result, err
	}
	adapter, err := r.executionAdapter(execution.Spec.AgentID, execution.Spec.InstallationPath)
	if err != nil {
		return result, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	secret := ""
	if profile != nil {
		secret = profile.Secret
	}
	sink := &supervisionTargetSink{runtime: r, execution: execution, cancel: cancel, secret: secret, usage: newRunUsageAccumulator(execution.Spec.ExecutionID, execution.Spec.SessionID)}
	start := time.Now()
	receipt, runErr := adapter.runSupervisionScheduler(ctx, supervisionSchedulerCLIRequest{
		Input: input.Observation, Profile: profile, SystemPrompt: input.Instructions, Container: execution.Container, Model: execution.Spec.Model, ReasoningEffort: execution.Spec.ReasoningEffort,
		Resolved: func(info supervisionCLIInfo) error {
			if err := requireSupervisionValidatedCLI(info); err != nil {
				return err
			}
			if info.ModelSettingsSHA256 != execution.Spec.ModelSettingsSHA256 {
				return targetexec.ErrConflict
			}
			if info.AgentID != execution.Spec.AgentID || info.Path != execution.Spec.CLIPath || info.Version != execution.Spec.CLIVersion || info.Contract != execution.Spec.Contract || info.EntrypointSHA256 != execution.Spec.EntrypointSHA256 || execution.Spec.RuntimeVersion != "" && info.RuntimeVersion != execution.Spec.RuntimeVersion || info.EffectiveModel != "" && info.EffectiveModel != execution.Spec.Model || info.EffectiveReasoningEffort != "" && info.EffectiveReasoningEffort != execution.Spec.ReasoningEffort {
				return targetexec.ErrConflict
			}
			sink.append("execution.configuration", mustJSON(info))
			return sink.failure()
		},
		Handshake: func(identity supervisionProcessIdentity, resume func() error) error {
			return handshake(targetexec.ProcessIdentity{Container: identity.Container, PID: identity.PID, Birth: identity.Birth, Executable: identity.Executable}, resume)
		},
	}, sink)
	result.Quiet = receipt.ContainerQuiet
	if runErr != nil && secret != "" {
		runErr = errors.New(strings.ReplaceAll(runErr.Error(), secret, "[REDACTED]"))
	}
	result.Usage = sink.snapshotUsage(time.Since(start).Milliseconds())
	if result.Quiet {
		if redactAgentText(receipt.Output) != receipt.Output || secret != "" && strings.Contains(receipt.Output, secret) {
			receipt.Output = ""
			runErr = errors.Join(runErr, errors.New("目标 CLI 输出包含不可保留的敏感内容"))
		}
		terminal, _ := supervisionTargetPayload(mustJSON(map[string]any{"receipt": receipt, "failed": runErr != nil, "error": redactAgentText(errorString(runErr))}))
		sink.append("execution.result", terminal)
		if sink.failure() == nil {
			result.ResultSHA256 = supervisionSHA256(terminal)
		}
	}
	return result, errors.Join(runErr, sink.failure())
}

// A hash binds bytes, but does not prove that the observation describes the
// execution being authorized. Check both before resolving or starting any CLI.
// Implementation/review inputs share the decision envelope with the scheduler,
// and additionally bind their task, plan and role job to the same snapshot.
func validateSupervisionTargetCLIInput(spec targetexec.ExecutionSpec, input targetexec.Input) error {
	if spec.Role != "scheduler" && spec.Role != "implementation" && spec.Role != "review" {
		return targetexec.ErrConflict
	}
	if len(input.Observation) > maxSupervisionSchedulerInputBytes || input.Instructions != supervisionRolePrompt(spec.Role) || spec.InputSHA256 != supervisionSHA256(input.Observation) || spec.InstructionsSHA256 != supervisionSHA256([]byte(input.Instructions)) {
		return targetexec.ErrConflict
	}
	var observation supervisionRoleInput
	if json.Unmarshal(input.Observation, &observation) != nil || observation.SchemaVersion != 1 {
		return targetexec.ErrConflict
	}
	d := observation.Decision
	if d.ID == "" || d.SessionID != spec.SessionID || d.ExecutionID != spec.ExecutionID || d.WorkItemID != spec.WorkItemID || d.Attempt != spec.Attempt || d.LeaseEpoch != spec.OwnerEpoch {
		return targetexec.ErrConflict
	}
	if observation.ProjectContext.RevisionID != "" && observation.ProjectContext.Manifest.DirectoryIdentity != spec.WorkspaceID {
		return targetexec.ErrConflict
	}
	if spec.Role != "scheduler" {
		job, task, plan := observation.Job, observation.Task, observation.Plan
		if job.ExecutionID != spec.ExecutionID || job.Kind != spec.Role || job.TaskID == "" || job.TaskID != task.TaskID || job.TaskID != plan.TaskID || job.RequirementsRevision != task.RequirementsRevision || job.RequirementsRevision != plan.RequirementsRevision || job.PlanRevision != plan.Revision || job.InputRevision != observation.ProjectContext.RevisionID || observation.ProjectContext.Manifest.DirectoryIdentity != spec.WorkspaceID {
			return targetexec.ErrConflict
		}
	}
	return nil
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (r *supervisionTargetRuntime) StopAndProbe(ctx context.Context, execution targetexec.Execution) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := stopSupervisionContainer(execution.Container); err != nil {
		return false, err
	}
	return supervisionContainerQuiescent(execution.Container)
}

type supervisionTargetSink struct {
	secret    string
	mu        sync.Mutex
	runtime   *supervisionTargetRuntime
	execution targetexec.Execution
	cancel    context.CancelFunc
	sequence  int64
	err       error
	usage     *runUsageAccumulator
}

func (s *supervisionTargetSink) snapshotUsage(active int64) targetexec.Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	usage := targetexec.Usage{Sequence: 1, ActiveMillis: active}
	if s.usage != nil {
		usage.InputTokens = s.usage.inputTokens
		usage.OutputTokens = s.usage.outputTokens
		usage.CacheReadTokens = s.usage.cacheRead
		usage.CacheCreationTokens = s.usage.cacheCreation
	}
	return usage
}

func (s *supervisionTargetSink) failure() error { s.mu.Lock(); defer s.mu.Unlock(); return s.err }
func (s *supervisionTargetSink) append(kind string, payload json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return
	}
	s.sequence++
	// Keep JSON structure intact while applying the project's secret redactor.
	var err error
	payload, err = supervisionTargetPayload(payload, s.secret)
	if err != nil {
		s.err = targetexec.ErrProtocol
		s.cancel()
		return
	}
	key := "frame-" + supervisionSHA256([]byte(fmt.Sprintf("%s:%d", s.execution.Spec.ExecutionID, s.sequence)))
	_, s.err = s.runtime.store.AppendEvent(s.execution.Spec.SessionID, s.execution.Spec.ExecutionID, key, kind, payload)
	if s.err != nil {
		s.cancel()
	}
}

func supervisionTargetPayload(payload json.RawMessage, secrets ...string) (json.RawMessage, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if !json.Valid(payload) {
		return nil, targetexec.ErrProtocol
	}
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var scrub func(any) any
	scrub = func(value any) any {
		switch item := value.(type) {
		case string:
			for _, secret := range secrets {
				if secret != "" {
					item = strings.ReplaceAll(item, secret, "[REDACTED]")
				}
			}
			return item
		case []any:
			for i, v := range item {
				item[i] = scrub(v)
			}
		case map[string]any:
			for k, v := range item {
				item[k] = scrub(v)
			}
		}
		return value
	}
	return json.Marshal(redactSupervisionTargetValue(scrub(value)))
}
func redactSupervisionTargetValue(value any) any {
	return redactAgentJSONValue(value, "")
}
func (s *supervisionTargetSink) Event(kind string, payload json.RawMessage) {
	if isThinkingTokensEvent(kind, payload) {
		return
	}
	s.mu.Lock()
	if s.usage != nil {
		s.usage.collect(kind, payload)
	}
	s.mu.Unlock()
	s.append("cli.event", mustJSON(map[string]any{"type": kind, "payload": payload}))
}
func (s *supervisionTargetSink) AssistantText(content, parent string) {
	s.append("assistant.message", mustJSON(map[string]string{"content": content, "parentToolUseId": parent}))
}
func (s *supervisionTargetSink) SessionIdentified(id string) {
	s.append("cli.session", mustJSON(map[string]string{"sessionId": id}))
}
func (s *supervisionTargetSink) SessionInitialized() {
	s.append("cli.initialized", json.RawMessage(`{}`))
}

// Only stdin/stdout are protocol traffic; all diagnostics remain on stderr.
type SupervisionTargetStdio struct {
	Input  *os.File
	Output *os.File
	once   sync.Once
}

func (s *SupervisionTargetStdio) Read(p []byte) (int, error)  { return s.Input.Read(p) }
func (s *SupervisionTargetStdio) Write(p []byte) (int, error) { return s.Output.Write(p) }
func (s *SupervisionTargetStdio) Close() error {
	var err error
	s.once.Do(func() { err = s.Input.Close() })
	return err
}

var _ targetexec.Executor = (*supervisionTargetRuntime)(nil)
var _ AgentRunSink = (*supervisionTargetSink)(nil)
