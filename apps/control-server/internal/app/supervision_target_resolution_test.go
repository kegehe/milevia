package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

func supervisionProbeInstalledTargetCLI(t *testing.T, ctx context.Context, link *supervisionTargetLink, installation string, workspace targetexec.Workspace) supervisionCLIInfo {
	t.Helper()
	call := func(request targetexec.Request) targetexec.Response {
		t.Helper()
		request.ID = uuid.NewString()
		response, err := link.Call(ctx, request)
		if err != nil {
			t.Fatalf("native target configuration probe %s: %v", request.Method, err)
		}
		return response
	}
	call(targetexec.Request{Method: "hello"})
	until, err := link.Client.TargetDeadline(30 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	control := targetexec.Control{Protocol: targetexec.ProtocolVersion, TargetID: link.TargetID, SessionID: uuid.NewString(), WorkspaceID: workspace.ID, OwnerEpoch: 1, Version: 1, State: "running", AuthorizedUntil: until}
	call(targetexec.Request{Method: "control", SessionID: control.SessionID, Control: &control})
	spec := targetexec.ExecutionSpec{Protocol: targetexec.ProtocolVersion, TargetID: link.TargetID, SessionID: control.SessionID, WorkspaceID: workspace.ID, ExecutionID: uuid.NewString(), WorkItemID: uuid.NewString(), Attempt: 1, Role: "scheduler", AgentID: "claude-code", OwnerEpoch: 1, AuthorizationVersion: 1, InstallationPath: installation}
	input := targetexec.Input{Observation: mustJSON(supervisionSchedulerInput{SchemaVersion: 1, Decision: supervisionDecision{ID: uuid.NewString(), SessionID: spec.SessionID, ExecutionID: spec.ExecutionID, WorkItemID: spec.WorkItemID, Attempt: 1, LeaseEpoch: 1}}), Instructions: supervisionRolePrompt("scheduler")}
	spec.InputSHA256, spec.InstructionsSHA256 = supervisionSHA256(input.Observation), supervisionSHA256([]byte(input.Instructions))
	response := call(targetexec.Request{Method: "execution.resolve", SessionID: spec.SessionID, ExecutionID: spec.ExecutionID, WorkspaceID: workspace.ID, OwnerEpoch: 1, AuthorizationVersion: 1, Spec: &spec, Input: &input})
	var info supervisionCLIInfo
	if json.Unmarshal(response.Data, &info) != nil || info.AgentID != spec.AgentID || info.Version == "" || info.Path == "" || info.Contract == "" {
		t.Fatal("native target resolution returned incomplete metadata")
	}
	actual := call(targetexec.Request{Method: "query", SessionID: spec.SessionID}).Session
	if actual == nil || len(actual.Executions) != 0 {
		t.Fatal("configuration probe created target execution")
	}
	control.Version++
	control.State = "stopped"
	call(targetexec.Request{Method: "control", SessionID: spec.SessionID, Control: &control})
	call(targetexec.Request{Method: "release", SessionID: spec.SessionID, OwnerEpoch: 1, AuthorizationVersion: control.Version})
	return info
}

type supervisionResolutionAdapter struct {
	AgentRunner
	run func(context.Context, supervisionSchedulerCLIRequest, AgentRunSink) (supervisionSchedulerCLIReceipt, error)
}

func (a supervisionResolutionAdapter) supervisionCLIInfo(context.Context, bool) (supervisionCLIInfo, error) {
	return supervisionCLIInfo{}, nil
}
func (a supervisionResolutionAdapter) runSupervisionScheduler(ctx context.Context, r supervisionSchedulerCLIRequest, sink AgentRunSink) (supervisionSchedulerCLIReceipt, error) {
	return a.run(ctx, r, sink)
}

func supervisionResolutionInput() (targetexec.ExecutionSpec, targetexec.Input) {
	spec := targetexec.ExecutionSpec{Protocol: 1, TargetID: "target", SessionID: "session", ExecutionID: "execution", WorkItemID: "work", Attempt: 1, OwnerEpoch: 1, AuthorizationVersion: 1, WorkspaceID: "workspace", AgentID: "codex", Role: "scheduler"}
	input := targetexec.Input{Observation: mustJSON(supervisionSchedulerInput{SchemaVersion: 1, Decision: supervisionDecision{ID: "decision", SessionID: spec.SessionID, ExecutionID: spec.ExecutionID, WorkItemID: spec.WorkItemID, Attempt: spec.Attempt, LeaseEpoch: spec.OwnerEpoch}}), Instructions: supervisionRolePrompt(spec.Role)}
	spec.InputSHA256, spec.InstructionsSHA256 = supervisionSHA256(input.Observation), supervisionSHA256([]byte(input.Instructions))
	return spec, input
}

func TestSupervisionTargetResolutionUsesTargetPathSemantics(t *testing.T) {
	for _, tc := range []struct {
		os, path string
		valid    bool
	}{
		{"linux", "/home/user/bin/cli", true}, {"linux", `C:\cli.exe`, false},
		{"windows", `C:\Program Files\cli.exe`, true}, {"windows", `C:/tools/cli.exe`, true},
		{"windows", `\\server\share\cli.exe`, true}, {"windows", `/usr/bin/cli`, false},
		{"windows", `C:cli.exe`, false}, {"windows", `\\.\pipe\cli`, false},
		{"windows", `\\server\\cli.exe`, false}, {"linux", "relative/cli", false},
		{"linux", "/bin/cli\nother", false}, {"other", "/bin/cli", false},
	} {
		if got := supervisionTargetAbsolutePath(tc.os, tc.path); got != tc.valid {
			t.Errorf("%s %q: got %v want %v", tc.os, tc.path, got, tc.valid)
		}
	}
}

func TestSupervisionTargetResolutionAbortsBeforeModelLaunchAndPreservesTargetDefaults(t *testing.T) {
	spec, input := supervisionResolutionInput()
	actual := supervisionCLIInfo{AgentID: "codex", Path: "/target/bin/codex", Version: "0.166.0", Contract: supervisionCodexContract, EffectiveModel: "target-default", EffectiveReasoningEffort: "high"}
	for _, scenario := range []string{"resolved", "started", "handshake", "emitted_output", "ignored_abort", "explicit_model_mismatch", "missing_resolution"} {
		t.Run(scenario, func(t *testing.T) {
			bound := spec
			if scenario == "explicit_model_mismatch" {
				bound.Model = "user-selected"
			}
			adapter := supervisionResolutionAdapter{run: func(_ context.Context, r supervisionSchedulerCLIRequest, sink AgentRunSink) (supervisionSchedulerCLIReceipt, error) {
				if scenario == "missing_resolution" {
					return supervisionSchedulerCLIReceipt{ContainerQuiet: true}, nil
				}
				err := r.Resolved(actual)
				out := supervisionSchedulerCLIReceipt{ContainerQuiet: true}
				switch scenario {
				case "started":
					out.ProcessStarted = true
				case "handshake":
					_ = r.Handshake(supervisionProcessIdentity{}, func() error { t.Fatal("resolution resumed a process"); return nil })
				case "emitted_output":
					sink.AssistantText("unexpected model output", "")
				case "ignored_abort":
					err = nil
				}
				return out, err
			}}
			got, err := resolveSupervisionTargetCLI(context.Background(), adapter, bound, input)
			if scenario == "resolved" {
				if err != nil || got != actual {
					t.Fatal("target effective defaults lost", got, err)
				}
			} else if err == nil || got.AgentID != "" {
				t.Fatal("unsafe or mismatched resolution accepted", got, err)
			}
		})
	}
	foreign := input
	foreign.Observation = mustJSON(supervisionSchedulerInput{SchemaVersion: 1})
	called := false
	adapter := supervisionResolutionAdapter{run: func(context.Context, supervisionSchedulerCLIRequest, AgentRunSink) (supervisionSchedulerCLIReceipt, error) {
		called = true
		return supervisionSchedulerCLIReceipt{}, nil
	}}
	if _, err := resolveSupervisionTargetCLI(context.Background(), adapter, spec, foreign); !errors.Is(err, targetexec.ErrConflict) || called {
		t.Fatal("foreign observation reached native preparation", err, called)
	}
}

// Resolves the installed native CLI and actual target configuration, without
// starting a model turn. Opt in separately from paid live workflow tests.
func TestSupervisionTargetInstalledCLIResolvesWithoutLaunching(t *testing.T) {
	binary := os.Getenv("MILEVIA_SUPERVISION_RESOLVE_CLAUDE")
	if binary == "" {
		t.Skip("set explicit native Claude CLI path for target resolution experiment")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	spec, input := supervisionResolutionInput()
	spec.AgentID = "claude-code"
	adapter := newClaudeCLIRunner(Config{ClaudePath: binary}, nil).(supervisionCLIAdapter)
	info, err := resolveSupervisionTargetCLI(ctx, adapter, spec, input)
	if err != nil || info.AgentID != "claude-code" || info.Version == "" || info.Path == "" {
		t.Fatal("installed CLI did not resolve before launch", info, err)
	}
	t.Log("native CLI resolved with no model process:", info.AgentID, info.Version)
}
