//go:build windows

package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

func TestSupervisionTargetResolutionPersistsEffectiveDefaultsAndRejectsChangedLaunch(t *testing.T) {
	s, permit, target := supervisionTargetControlFixture(t)
	link := supervisionTargetControlTestLink(t, target)
	ctx := context.Background()
	if err := s.deliverSupervisionTargetControl(ctx, link, permit.Decision.SessionID); err != nil {
		t.Fatal(err)
	}
	// The controller is Windows; target-native paths must use its recorded OS.
	if _, err := s.db.Exec(`update supervision_target_bindings set target_os='linux' where target_id=?`, link.TargetID); err != nil {
		t.Fatal(err)
	}
	intent, err := loadSupervisionTargetControl(ctx, s.db, permit.Decision.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	input, err := s.supervisionTargetExecutionInput(ctx, permit)
	if err != nil {
		t.Fatal(err)
	}
	info := supervisionCLIInfo{AgentID: permit.AgentID, Path: "/target/bin/claude", Version: "2.1.308", Contract: "fixture-v1", EffectiveModel: "target-default", EffectiveReasoningEffort: "high", RuntimeVersion: "target-runtime"}
	spec, err := s.recordSupervisionTargetResolution(ctx, permit, intent.Control, input, info)
	if err != nil || spec.Model != "target-default" || spec.ReasoningEffort != "high" || spec.RuntimeVersion != "target-runtime" {
		t.Fatal("effective target settings were not frozen", spec, err)
	}
	if again, err := s.recordSupervisionTargetResolution(ctx, permit, intent.Control, input, info); err != nil || again != spec {
		t.Fatal("identical resolution was not idempotent", again, err)
	}
	changed := info
	changed.EffectiveModel = "later-target-default"
	if _, err := s.recordSupervisionTargetResolution(ctx, permit, intent.Control, input, changed); !errors.Is(err, targetexec.ErrConflict) {
		t.Fatal("target defaults changed an immutable resolution", err)
	}
	spec.PreparedAt = time.Now().UTC()
	spec.TurnDeadline = spec.PreparedAt.Add(time.Minute)
	for _, field := range []string{"model", "reasoning", "path", "runtime"} {
		altered := spec
		switch field {
		case "model":
			altered.Model = "other"
		case "reasoning":
			altered.ReasoningEffort = "low"
		case "path":
			altered.CLIPath = "/other/claude"
		case "runtime":
			altered.RuntimeVersion = "other"
		}
		if err := s.stageSupervisionTargetExecution(ctx, permit, altered); !errors.Is(err, targetexec.ErrConflict) {
			t.Fatal("changed launch metadata accepted", field, err)
		}
	}
	if err := s.stageSupervisionTargetExecution(ctx, permit, spec); err != nil {
		t.Fatal("actual target defaults rejected at staging", err)
	}
	ss := supervisionSessionForTest(t, s, permit.Decision.SessionID)
	if err := s.requestSupervisionControl(ctx, ss.ID, ss.StateVersion, "pause", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.recordSupervisionTargetResolution(ctx, permit, intent.Control, input, info); err == nil {
		t.Fatal("pause allowed a late resolution to commit")
	}
}

func TestSupervisionTargetResolutionRejectsForgedPermitBeforeRPC(t *testing.T) {
	s, permit, target := supervisionTargetControlFixture(t)
	link := supervisionTargetControlTestLink(t, target)
	ctx := context.Background()
	if err := s.deliverSupervisionTargetControl(ctx, link, permit.Decision.SessionID); err != nil {
		t.Fatal(err)
	}
	// Even a closed transport must not be touched for an invalid permit.
	if err := link.Close(); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"token", "agent", "model", "role"} {
		altered := permit
		switch field {
		case "token":
			altered.Token = "forged"
		case "agent":
			altered.AgentID = "codex"
		case "model":
			altered.Model = "forged"
		case "role":
			altered.Role = "review"
		}
		if _, err := s.resolveSupervisionTargetExecution(ctx, link, altered); err == nil || errors.Is(err, targetexec.ErrClosed) {
			t.Fatal("invalid permit reached target RPC", field, err)
		}
	}
}

func TestSupervisionTargetDispatchPreservesOriginalTurnStartAcrossPreparationReplay(t *testing.T) {
	s, permit, target := supervisionTargetControlFixture(t)
	link := supervisionTargetControlTestLink(t, target)
	ctx := context.Background()
	if err := s.deliverSupervisionTargetControl(ctx, link, permit.Decision.SessionID); err != nil {
		t.Fatal(err)
	}
	intent, err := loadSupervisionTargetControl(ctx, s.db, permit.Decision.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	input, err := s.supervisionTargetExecutionInput(ctx, permit)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.recordSupervisionTargetResolution(ctx, permit, intent.Control, input, supervisionCLIInfo{AgentID: permit.AgentID, Path: `C:\target\claude.exe`, Version: "2.1.308", Contract: "fixture-v1", EffectiveModel: "target-default"}); err != nil {
		t.Fatal(err)
	}
	// The turn began before configuration/transport preparation. It has fifteen
	// seconds left, rather than a new sixty-second turn at the target.
	budget := defaultSupervisionBudget()
	budget.MaxTurnMillis = 60000
	if _, err := s.db.Exec(`update supervision_sessions set budget=? where id=?`, string(mustJSON(budget)), permit.Decision.SessionID); err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC().Add(-45 * time.Second)
	if _, err := s.db.Exec(`update supervision_work_items set started_at=? where execution_id=?`, started, permit.Decision.ExecutionID); err != nil {
		t.Fatal(err)
	}
	spec, gotInput, err := s.prepareSupervisionTargetDispatch(ctx, link, permit)
	if err != nil || spec.TurnDeadline.Sub(spec.PreparedAt) > 15*time.Second || spec.TurnDeadline.Sub(spec.PreparedAt) < 10*time.Second || string(gotInput.Observation) != string(input.Observation) {
		t.Fatal("preparation renewed original turn budget", spec, err)
	}
	if again, _, err := s.prepareSupervisionTargetDispatch(ctx, link, permit); err != nil || again != spec {
		t.Fatal("preparation replay changed target window", again, err)
	}
	var targetSession targetexec.Session
	targetSession, err = target.store.Session(permit.Decision.SessionID)
	if err != nil || len(targetSession.Executions) != 0 {
		t.Fatal("preparation launched a model or command", err)
	}
	ss := supervisionSessionForTest(t, s, permit.Decision.SessionID)
	if err := s.requestSupervisionControl(ctx, ss.ID, ss.StateVersion, "pause", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.prepareSupervisionTargetDispatch(ctx, link, permit); err == nil {
		t.Fatal("pause admitted preparation replay")
	}
}

func TestSupervisionTargetResolutionFencesCLIManagementChangesDuringProbeAndDispatch(t *testing.T) {
	s, permit, target := supervisionTargetControlFixture(t)
	link := supervisionTargetControlTestLink(t, target)
	ctx := context.Background()
	if err := s.deliverSupervisionTargetControl(ctx, link, permit.Decision.SessionID); err != nil {
		t.Fatal(err)
	}
	intent, err := loadSupervisionTargetControl(ctx, s.db, permit.Decision.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	input, err := s.supervisionTargetExecutionInput(ctx, permit)
	if err != nil {
		t.Fatal(err)
	}
	var runnerID string
	if err := s.db.QueryRow(`select agent_runtime_id from runs where id=?`, permit.RunID).Scan(&runnerID); err != nil {
		t.Fatal(err)
	}
	old, selected := `C:\old\claude.cmd`, `C:\selected\claude.cmd`
	if err := s.recordAgentInstallation(ctx, agentInstallation{RunnerID: runnerID, AgentID: permit.AgentID, BinaryPath: selected}); err != nil {
		t.Fatal(err)
	}
	info := supervisionCLIInfo{AgentID: permit.AgentID, Path: `C:\native\claude.exe`, Version: "2.1.308", Contract: "fixture-v1"}
	if _, err := s.recordSupervisionTargetResolution(ctx, permit, intent.Control, input, info, old); !errors.Is(err, targetexec.ErrConflict) {
		t.Fatal("old probe committed after installation changed", err)
	}
	spec, err := s.recordSupervisionTargetResolution(ctx, permit, intent.Control, input, info, selected)
	if err != nil || spec.InstallationPath != selected {
		t.Fatal("actual registration was not frozen", spec, err)
	}
	if err := s.recordAgentInstallation(ctx, agentInstallation{RunnerID: runnerID, AgentID: permit.AgentID, BinaryPath: old}); err != nil {
		t.Fatal(err)
	}
	spec.PreparedAt, spec.TurnDeadline = time.Now().UTC(), time.Now().UTC().Add(time.Minute)
	if err := s.stageSupervisionTargetExecution(ctx, permit, spec); !errors.Is(err, targetexec.ErrConflict) {
		t.Fatal("stale CLI installation dispatched after management changed", err)
	}
}
