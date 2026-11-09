//go:build windows

package app

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

func TestSupervisionTargetExecutionIntentFreezesVerificationInsteadOfCLIInput(t *testing.T) {
	s, _, taskID, ss, pc, _ := implementedWorkflowForTest(t)
	check := supervisionCheck{ID: "runtime", AcceptanceIDs: []string{"behavior"}, Program: "node", Args: []string{"--version"}, Directory: ".", TimeoutMillis: 10000}
	applyWorkflowActionForTest(t, s, ss.ID, supervisionWorkflowAction{Type: "request_verification", SessionID: ss.ID, TaskID: taskID, RequirementsRevision: 1, PlanRevision: 1, ContextID: pc.ID, Checks: []supervisionCheck{check}})
	permit, _ := workflowPermitForTest(t, s, ss.ID)
	if permit.Role != "test" {
		t.Fatal("verification did not use the test role", permit.Role)
	}
	ctx := context.Background()
	input, err := s.supervisionTargetExecutionInput(ctx, permit)
	if err != nil {
		t.Fatal(err)
	}
	var parsed supervisionTargetCheckInput
	if json.Unmarshal(input.Observation, &parsed) != nil || input.Instructions != supervisionTargetVerificationContract || string(mustJSON(parsed.Check)) != string(mustJSON(check)) || parsed.RevisionID != pc.RevisionID || parsed.RulesHash != pc.RulesHash {
		t.Fatal("verification input changed frozen command or source", string(input.Observation))
	}
	store, err := targetexec.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	targetID, err := store.Identity()
	if err != nil {
		t.Fatal(err)
	}
	var runnerID string
	var workspace targetexec.Workspace
	if err := s.db.QueryRow(`select r.agent_runtime_id,a.directory_identity,a.canonical_path from runs r join supervision_sessions ss on ss.id=? join workspace_admissions a on a.id=ss.admission_id where r.id=?`, ss.ID, permit.RunID).Scan(&runnerID, &workspace.ID, &workspace.Path); err != nil {
		t.Fatal(err)
	}
	if err := s.recordSupervisionTargetBinding(ctx, runnerID, &supervisionTargetLink{TargetID: targetID, Artifact: targetexec.HelperArtifact{OS: runtime.GOOS, Arch: runtime.GOARCH, SHA256: strings.Repeat("a", 64)}}); err != nil {
		t.Fatal(err)
	}
	if err := s.stageSupervisionTargetControl(ctx, ss.ID, runnerID, targetID, workspace.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	intent, err := loadSupervisionTargetControl(ctx, s.db, ss.ID)
	if err != nil {
		t.Fatal(err)
	}
	control := intent.Control
	control.AuthorizedUntil = now.Add(30 * time.Second)
	if err := store.RegisterWorkspace(workspace); err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyControl(control, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`update supervision_target_controls set wire_control=?,acknowledged_version=? where session_id=?`, string(mustJSON(control)), control.Version, ss.ID); err != nil {
		t.Fatal(err)
	}
	spec := targetexec.ExecutionSpec{Protocol: 1, TargetID: targetID, SessionID: ss.ID, ExecutionID: permit.Decision.ExecutionID, WorkItemID: permit.Decision.WorkItemID, Attempt: permit.Decision.Attempt, OwnerEpoch: permit.Lease.Epoch, AuthorizationVersion: 1, WorkspaceID: workspace.ID, Role: "test", AgentID: permit.AgentID, Contract: supervisionTargetVerificationContract, InputSHA256: supervisionSHA256(input.Observation), InstructionsSHA256: supervisionSHA256([]byte(input.Instructions)), PreparedAt: now, TurnDeadline: now.Add(10 * time.Second)}
	for _, scenario := range []string{"rehashed_command", "cli_instructions", "model", "cli_path", "cli_contract"} {
		changed := spec
		switch scenario {
		case "rehashed_command":
			body := parsed
			body.Check.Args = []string{"another-check.js"}
			changed.InputSHA256 = supervisionSHA256(mustJSON(body))
		case "cli_instructions":
			changed.InstructionsSHA256 = supervisionSHA256([]byte(supervisionRolePrompt("test")))
		case "model":
			changed.Model = "not-a-cli-call"
		case "cli_path":
			changed.CLIPath = "/unapproved/cli"
		case "cli_contract":
			changed.Contract = supervisionCodexContract
		}
		if err := s.stageSupervisionTargetExecution(ctx, permit, changed); !errors.Is(err, targetexec.ErrConflict) {
			t.Fatal("invalid verification intent accepted", scenario, err)
		}
	}
	for range 2 {
		if err := s.stageSupervisionTargetExecution(ctx, permit, spec); err != nil {
			t.Fatal("immutable verification could not stage/replay", err)
		}
	}
	var count int
	if err := s.db.QueryRow(`select count(*) from supervision_target_executions where execution_id=?`, spec.ExecutionID).Scan(&count); err != nil || count != 1 {
		t.Fatal("verification intent duplicated", count, err)
	}
	// A different role's permit cannot reinterpret the persisted verification.
	foreign := permit
	foreign.TaskID = "other-task"
	if _, err := s.supervisionTargetExecutionInput(ctx, foreign); !errors.Is(err, targetexec.ErrConflict) {
		t.Fatal("foreign task bound to existing verification", err)
	}
}
