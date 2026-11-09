//go:build windows

package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestSupervisionStorageReservationProtectsArtifactCapacityAndReleasesAtSettlement(t *testing.T) {
	s, _, permit, now, _ := supervisionExecutionForTest(t)
	ctx := context.Background()
	budget := defaultSupervisionBudget()
	budget.MaxStorageBytes = 32 << 20
	if _, err := s.db.Exec(`update supervision_sessions set budget=? where id=?`, string(mustJSON(budget)), permit.Decision.SessionID); err != nil {
		t.Fatal(err)
	}
	if err := s.withSupervisionTx(ctx, permit.Decision.SessionID, now, func(tx *sql.Tx, _ map[string]WorkspaceAdmission, _ *supervisionSession) error {
		_, err := tx.Exec(`insert into supervision_storage_reservations(execution_id,session_id,bytes) values (?,?,?)`, permit.Decision.ExecutionID, permit.Decision.SessionID, 24<<20)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	err := s.withSupervisionTx(ctx, permit.Decision.SessionID, now, func(tx *sql.Tx, _ map[string]WorkspaceAdmission, _ *supervisionSession) error {
		_, err := tx.Exec(`insert into supervision_amendments(id,session_id,content,state,created_at) values ('log-growth',?,?,'pending',?)`, permit.Decision.SessionID, strings.Repeat("x", 9<<20), now)
		return err
	})
	if !errors.Is(err, errSupervisionStorage) {
		t.Fatalf("artifact reservation was ignored: %v", err)
	}
	if err := s.finishSupervisionExecution(ctx, permit, supervisionSchedulerCLIReceipt{ContainerQuiet: true}, errSupervisionStorage, now); err != nil {
		t.Fatal(err)
	}
	if reserved, err := supervisionReservedBytes(ctx, s.db, permit.Decision.SessionID); err != nil || reserved != 0 {
		t.Fatalf("reservation leaked after settlement: %d %v", reserved, err)
	}
}

func TestSupervisionPausedRoleUpdateFencesQueuedWorkAndFreezesNewProfile(t *testing.T) {
	s, _, _, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	ctx := context.Background()
	if err := s.updateSupervisionRoles(ctx, ss.ID, 1, supervisionRoleConfigs{"scheduler": {AgentID: "claude-code"}}); err != nil {
		t.Fatal(err)
	}
	startSupervisionForTest(t, s, ss.ID, now)
	_, d := decisionForTest(t, s, ss.ID, now)
	ss = supervisionSessionForTest(t, s, ss.ID)
	if err := s.requestSupervisionControl(ctx, ss.ID, ss.StateVersion, "pause", now); err != nil {
		t.Fatal(err)
	}
	if err := s.settleSupervisionControl(ctx, ss.ID, now); err != nil {
		t.Fatal(err)
	}
	ss = supervisionSessionForTest(t, s, ss.ID)
	profile := createCLIManagedProfile(t, s, "claude-code", "sonnet")
	// Capability discovery only checks a native executable here; no model call.
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s.runner = &claudeCLIRunner{config: Config{ClaudePath: binary}}
	if err := s.updateSupervisionRoles(ctx, ss.ID, ss.StateVersion, supervisionRoleConfigs{"review": {ProfileID: profile.ID, Model: "sonnet"}}); err != nil {
		t.Fatal(err)
	}
	got := supervisionSessionForTest(t, s, ss.ID)
	if got.State != "paused" || got.LeaseEpoch <= ss.LeaseEpoch || !strings.Contains(string(got.RouteSnapshot), profile.CurrentRevisionID) {
		t.Fatalf("paused update did not preserve control/fence/frozen profile: %+v", got)
	}
	var state string
	if err := s.db.QueryRow(`select state from supervision_decisions where id=?`, d.ID).Scan(&state); err != nil || state != "stale" {
		t.Fatalf("old decision survived configuration: %s %v", state, err)
	}
}

func supervisionExecutionForTest(t *testing.T) (*Server, string, supervisionExecutionPermit, time.Time, time.Time) {
	t.Helper()
	s, _, taskID, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	if _, err := s.db.Exec(`update supervision_sessions set scheduler_agent_id='claude-code' where id=?`, ss.ID); err != nil {
		t.Fatal(err)
	}
	startSupervisionForTest(t, s, ss.ID, now)
	lease, d := decisionForTest(t, s, ss.ID, now)
	deadline, err := s.claimSupervisionDecisionLaunch(context.Background(), d.ID, lease, now)
	if err != nil {
		t.Fatal(err)
	}
	permit, err := s.prepareSupervisionExecution(context.Background(), d, lease, now)
	if err != nil {
		t.Fatal(err)
	}
	return s, taskID, permit, now, deadline
}

func TestSupervisionExecutionUsesIndependentFrozenRoleProfileAndModel(t *testing.T) {
	for _, role := range []string{"scheduler", "implementation", "review", "test"} {
		t.Run(role, func(t *testing.T) {
			s, _, taskID, ss, now := supervisionFixture(t, defaultSupervisionBudget())
			first := createCLIManagedProfile(t, s, "claude-code", "sonnet")
			second := createCLIManagedProfile(t, s, "claude-code", "opus")
			roles := supervisionRoleConfigs{
				"scheduler":      {AgentID: "claude-code", ProfileID: first.ID, Model: "sonnet", ReasoningEffort: "low"},
				"implementation": {ProfileID: second.ID, Model: "opus", ReasoningEffort: "high"},
				"review":         {ProfileID: second.ID, Model: "sonnet", ReasoningEffort: "medium"},
			}
			if err := s.updateSupervisionRoles(context.Background(), ss.ID, 1, roles); err != nil {
				t.Fatal(err)
			}
			startSupervisionForTest(t, s, ss.ID, now)
			lease, d := decisionForTest(t, s, ss.ID, now)
			if _, err := s.db.Exec(`update supervision_work_items set role=?,task_id=? where execution_id=?`, role, taskID, d.ExecutionID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.claimSupervisionDecisionLaunch(context.Background(), d.ID, lease, now); err != nil {
				t.Fatal(err)
			}
			permit, err := s.prepareSupervisionExecution(context.Background(), d, lease, now)
			if err != nil {
				t.Fatal(err)
			}
			config := supervisionSessionForTest(t, s, ss.ID).roleConfig(role)
			wantProfile := second.CurrentRevisionID
			if role == "scheduler" || role == "test" {
				wantProfile = first.CurrentRevisionID
			}
			if permit.Profile == nil || permit.Profile.RevisionID != wantProfile || permit.Model != config.Model || permit.ReasoningEffort != config.ReasoningEffort {
				t.Fatalf("wrong role execution config: %+v", permit)
			}
			var model, effort string
			if err := s.db.QueryRow(`select c.model_override,m.reasoning_effort from conversations c join conversation_model_settings m on m.conversation_id=c.id where c.id=?`, permit.ConversationID).Scan(&model, &effort); err != nil || model != config.Model || effort != config.ReasoningEffort {
				t.Fatalf("role history lost model: %s %s %v", model, effort, err)
			}
		})
	}
}

func registerAndResumeExecutionForTest(t *testing.T, s *Server, permit supervisionExecutionPermit, now time.Time) {
	t.Helper()
	if err := s.registerSupervisionProcess(context.Background(), permit, supervisionProcessIdentity{PID: 123, Birth: "test-process-birth", Container: permit.Container}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.resumeSupervisionProcess(context.Background(), permit, func() error { return nil }, now); err != nil {
		t.Fatal(err)
	}
}

func TestSupervisionExecutionPreparesRoleRunTokenAndFrozenInputAtomically(t *testing.T) {
	s, taskID, permit, now, _ := supervisionExecutionForTest(t)
	var hash, state, content string
	if err := s.db.QueryRow(`select token_hash,state from supervision_executions where execution_id=?`, permit.Decision.ExecutionID).Scan(&hash, &state); err != nil {
		t.Fatal(err)
	}
	if hash == permit.Token || hash != supervisionTokenHash(permit.Token) || state != "prepared" {
		t.Fatal("execution token was not scoped and hashed")
	}
	if err := s.db.QueryRow(`select content from messages where run_id=? and role='user'`, permit.RunID).Scan(&content); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, taskID) || strings.Contains(content, permit.Token) {
		t.Fatal("input missing or contains execution token")
	}
	if err := rejectSupervisionConversationMessage(context.Background(), s.db, permit.ConversationID); err == nil {
		t.Fatal("role conversation can use ordinary send")
	}
	if _, err := s.prepareSupervisionExecution(context.Background(), permit.Decision, permit.Lease, now); err == nil {
		t.Fatal("duplicate execution prepared")
	}
	var count int
	if err := s.db.QueryRow(`select count(*) from supervision_conversation_links where session_id=?`, permit.Decision.SessionID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("partial duplicate role left behind: %d %v", count, err)
	}
	assertExecutionClaimForTest(t, s, taskID, "supervision", permit.Decision.SessionID)
}

func TestSupervisionExecutionTokenAndIdentityCannotBeRebound(t *testing.T) {
	s, _, permit, now, _ := supervisionExecutionForTest(t)
	for _, change := range []func(*supervisionExecutionPermit){
		func(p *supervisionExecutionPermit) { p.Token = "forged" },
		func(p *supervisionExecutionPermit) { p.Container += "-other" },
		func(p *supervisionExecutionPermit) { p.Decision.ID = "other" },
		func(p *supervisionExecutionPermit) { p.Decision.WorkItemID = "other" },
		func(p *supervisionExecutionPermit) { p.Decision.ExpectedStateVersion++ },
		func(p *supervisionExecutionPermit) { p.Decision.BasisSeq++ },
		func(p *supervisionExecutionPermit) { p.RunID = "other" },
		func(p *supervisionExecutionPermit) { p.ConversationID = "other" },
		func(p *supervisionExecutionPermit) { p.Lease.Epoch++ },
	} {
		forged := permit
		change(&forged)
		if err := s.registerSupervisionProcess(context.Background(), forged, supervisionProcessIdentity{PID: 123, Birth: "test", Container: forged.Container}, now); err == nil {
			t.Fatal("forged execution permit accepted")
		}
	}
	if err := s.registerSupervisionProcess(context.Background(), permit, supervisionProcessIdentity{PID: 123, Birth: "test", Container: "foreign"}, now); err == nil {
		t.Fatal("foreign process container accepted")
	}
	registerAndResumeExecutionForTest(t, s, permit, now)
	if err := s.resumeSupervisionProcess(context.Background(), permit, func() error { t.Fatal("second resume called"); return nil }, now); err == nil {
		t.Fatal("execution resumed twice")
	}
}

func TestSupervisionExecutionPauseBeforeResumePersistsCreatedProcessWithoutRunningIt(t *testing.T) {
	s, _, permit, now, _ := supervisionExecutionForTest(t)
	ss := supervisionSessionForTest(t, s, permit.Decision.SessionID)
	if err := s.requestSupervisionControl(context.Background(), ss.ID, ss.StateVersion, "pause", now); err != nil {
		t.Fatal(err)
	}
	if err := s.registerSupervisionProcess(context.Background(), permit, supervisionProcessIdentity{PID: 123, Birth: "test", Container: permit.Container}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.resumeSupervisionProcess(context.Background(), permit, func() error { t.Fatal("paused process resumed"); return nil }, now); err == nil {
		t.Fatal("pause did not block resume")
	}
	if err := s.settleSupervisionControl(context.Background(), ss.ID, now); !errors.Is(err, errWorkspaceNotQuiescent) {
		t.Fatal("pause ignored created process")
	}
	if err := s.finishSupervisionExecution(context.Background(), permit, supervisionSchedulerCLIReceipt{ProcessStarted: true, ProcessWaited: true, ContainerQuiet: true}, context.Canceled, now); err != nil {
		t.Fatal(err)
	}
	if err := s.settleSupervisionControl(context.Background(), ss.ID, now); err != nil {
		t.Fatal(err)
	}
	if current := supervisionSessionForTest(t, s, ss.ID); current.State != "paused" {
		t.Fatal(current.State)
	}
}

func TestSupervisionExecutionRoleOutputIsDurableButCannotConfirmExitOrAcceptTask(t *testing.T) {
	s, taskID, permit, now, _ := supervisionExecutionForTest(t)
	registerAndResumeExecutionForTest(t, s, permit, now)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := &supervisionRoleSink{server: s, permit: permit, cancel: cancel}
	sink.SessionIdentified("native-" + permit.RunID)
	sink.SessionInitialized()
	sink.AssistantText("dispatch rationale", "")
	sink.Event("result", json.RawMessage(`{"type":"result","subtype":"success"}`))
	if err := sink.failure(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`select count(*) from messages where run_id=? and role='assistant'`, permit.RunID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("role message missing: %d %v", count, err)
	}
	if err := s.confirmSupervisionDecisionQuiescent(context.Background(), permit.Decision.ID, permit.Decision.ExecutionID, permit.Lease.Epoch, now); !errors.Is(err, errWorkspaceNotQuiescent) {
		t.Fatal("model result was treated as process exit")
	}
	if err := s.finishSupervisionExecution(context.Background(), permit, supervisionSchedulerCLIReceipt{ProcessStarted: true, ProcessWaited: true, ContainerQuiet: true}, nil, now); err != nil {
		t.Fatal(err)
	}
	if disposition, err := s.recordSupervisionDecisionOutput(context.Background(), permit.Decision.ID, permit.Decision.ExecutionID, decisionProposalForTest(permit.Decision), now); err != nil || disposition != "proposed" {
		t.Fatalf("result=%s err=%v", disposition, err)
	}
	var taskStatus string
	if err := s.db.QueryRow(`select status from tasks where id=?`, taskID).Scan(&taskStatus); err != nil || taskStatus != "todo" {
		t.Fatalf("scheduler accepted task: %s %v", taskStatus, err)
	}
	if ctx.Err() != nil {
		t.Fatal(ctx.Err())
	}
}

func TestSupervisionExecutionUnknownKeepsRunClaimQuotaAndWorkspaceProtected(t *testing.T) {
	s, taskID, permit, now, _ := supervisionExecutionForTest(t)
	registerAndResumeExecutionForTest(t, s, permit, now)
	if err := s.finishSupervisionExecution(context.Background(), permit, supervisionSchedulerCLIReceipt{ProcessStarted: true, ProcessWaited: true, ContainerQuiet: false}, errors.New("container query failed"), now); err != nil {
		t.Fatal(err)
	}
	current := supervisionSessionForTest(t, s, permit.Decision.SessionID)
	if current.RecoveryState != "outcome_unknown" {
		t.Fatal(current.RecoveryState)
	}
	if err := s.releaseWorkspaceAdmission(context.Background(), current.AdmissionID, current.ID, current.AdmissionEpoch); err == nil {
		t.Fatal("unknown execution released workspace")
	}
	if err := s.reconcileSupervisionExecution(context.Background(), permit.Decision.ExecutionID, now); err != nil {
		t.Fatal(err)
	}
	current = supervisionSessionForTest(t, s, current.ID)
	if current.RecoveryState == "ready" {
		t.Fatal("process reconciliation implicitly resumed session")
	}
	assertExecutionClaimForTest(t, s, taskID, "supervision", current.ID)
}

func TestSupervisionExecutionRecoveryFencesOldCallbacksAndReconcilesOnlyFacts(t *testing.T) {
	s, _, permit, now, _ := supervisionExecutionForTest(t)
	if err := s.recoverWorkspaceAdmissions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.recoverSupervisionSessions(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if err := s.registerSupervisionProcess(context.Background(), permit, supervisionProcessIdentity{PID: 123, Birth: "test", Container: permit.Container}, now); !errors.Is(err, errSupervisionLease) {
		t.Fatalf("late callback=%v", err)
	}
	if err := s.reconcileSupervisionExecution(context.Background(), permit.Decision.ExecutionID, now); err != nil {
		t.Fatal(err)
	}
	var status, outcome string
	if err := s.db.QueryRow(`select r.status,l.outcome_state from runs r join supervision_run_links l on l.run_id=r.id where r.id=?`, permit.RunID).Scan(&status, &outcome); err != nil || status != "interrupted" || outcome != "unknown" {
		t.Fatalf("recovery invented outcome: %s %s %v", status, outcome, err)
	}
}

func TestSupervisionExecutionSinkFailureCancelsWithoutWritingToForeignRun(t *testing.T) {
	s, _, permit, now, _ := supervisionExecutionForTest(t)
	registerAndResumeExecutionForTest(t, s, permit, now)
	permit.Token = "forged"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := &supervisionRoleSink{server: s, permit: permit, cancel: cancel}
	sink.AssistantText("must not persist", "")
	if sink.failure() == nil || ctx.Err() == nil {
		t.Fatal("sink ignored authorization failure")
	}
	var count int
	if err := s.db.QueryRow(`select count(*) from messages where run_id=? and role='assistant'`, permit.RunID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("foreign output persisted: %d %v", count, err)
	}
}

func TestSupervisionExecutionFreezesLegacyProfileAndSettlesQuota(t *testing.T) {
	s, p, _, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	first := createCLIManagedProfile(t, s, "claude-code", "first-model")
	if _, err := s.db.Exec(`update projects set default_profile_id=? where id=?`, first.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`update supervision_sessions set scheduler_agent_id='claude-code' where id=?`, ss.ID); err != nil {
		t.Fatal(err)
	}
	startSupervisionForTest(t, s, ss.ID, now)
	second := createCLIManagedProfile(t, s, "claude-code", "second-model")
	if _, err := s.db.Exec(`update projects set default_profile_id=? where id=?`, second.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	lease, d := decisionForTest(t, s, ss.ID, now)
	if _, err := s.claimSupervisionDecisionLaunch(context.Background(), d.ID, lease, now); err != nil {
		t.Fatal(err)
	}
	permit, err := s.prepareSupervisionExecution(context.Background(), d, lease, now)
	if err != nil {
		t.Fatal(err)
	}
	if permit.Profile == nil || permit.Profile.RevisionID != first.CurrentRevisionID || permit.Profile.Model != "first-model" {
		t.Fatal("scheduler followed a changed project default")
	}
	var count int
	if err := s.db.QueryRow(`select count(*) from quota_reservations where run_id=? and profile_revision_id=? and state in ('reserved','running')`, permit.RunID, first.CurrentRevisionID).Scan(&count); err != nil || count == 0 {
		t.Fatalf("quota not reserved: %d %v", count, err)
	}
	if err := s.finishSupervisionExecution(context.Background(), permit, supervisionSchedulerCLIReceipt{ContainerQuiet: true}, errors.New("preflight failed"), now); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`select count(*) from quota_reservations where run_id=? and state in ('reserved','running')`, permit.RunID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("quota leaked: %d %v", count, err)
	}
}

func TestSupervisionExecutionBudgetDeadlineKeepsBudgetPauseReason(t *testing.T) {
	s, _, permit, now, _ := supervisionExecutionForTest(t)
	registerAndResumeExecutionForTest(t, s, permit, now)
	if err := s.finishSupervisionExecution(context.Background(), permit, supervisionSchedulerCLIReceipt{ProcessStarted: true, ProcessWaited: true, ContainerQuiet: true}, context.DeadlineExceeded, now.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	current := supervisionSessionForTest(t, s, permit.Decision.SessionID)
	if current.State != "pausing" || current.Control != "pause" || current.Reason != "budget_exhausted" {
		t.Fatalf("budget expiry became scheduler fault: %+v", current)
	}
}

func TestSupervisionExecutionPersistenceFailureCancelsAndDoesNotInventMessage(t *testing.T) {
	s, _, permit, now, _ := supervisionExecutionForTest(t)
	registerAndResumeExecutionForTest(t, s, permit, now)
	if _, err := s.db.Exec(`create trigger reject_supervision_output before insert on messages when new.role='assistant' begin select raise(abort,'simulated persistence failure');end`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := &supervisionRoleSink{server: s, permit: permit, cancel: cancel}
	sink.AssistantText("must persist before display", "")
	if sink.failure() == nil || ctx.Err() == nil {
		t.Fatal("persistence error was ignored")
	}
	var count int
	if err := s.db.QueryRow(`select count(*) from messages where run_id=? and role='assistant'`, permit.RunID).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed message became visible")
	}
}

func TestSupervisionExecutionDuplicateExecutorCannotChangeClaimedAttempt(t *testing.T) {
	s, _, permit, _, deadline := supervisionExecutionForTest(t)
	if _, err := s.db.Exec(`update supervision_executions set state='starting' where execution_id=?`, permit.Decision.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if err := s.executeSupervisionScheduler(context.Background(), permit, deadline, &claudeCLIRunner{}); !errors.Is(err, errSupervisionState) {
		t.Fatalf("duplicate executor=%v", err)
	}
	var state string
	if err := s.db.QueryRow(`select state from supervision_executions where execution_id=?`, permit.Decision.ExecutionID).Scan(&state); err != nil || state != "starting" {
		t.Fatalf("duplicate wake changed original attempt: %s %v", state, err)
	}
}

func TestSupervisionExecutionCorruptInputSettlesBeforeProcessStart(t *testing.T) {
	s, _, permit, _, deadline := supervisionExecutionForTest(t)
	if _, err := s.db.Exec(`update supervision_scheduler_inputs set input_json='{}' where execution_id=?`, permit.Decision.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if err := s.executeSupervisionScheduler(context.Background(), permit, deadline, &claudeCLIRunner{}); err == nil {
		t.Fatal("corrupt input accepted")
	}
	var executionState, runStatus string
	var pid int
	if err := s.db.QueryRow(`select e.state,e.pid,r.status from supervision_executions e join runs r on r.id=e.run_id where e.execution_id=?`, permit.Decision.ExecutionID).Scan(&executionState, &pid, &runStatus); err != nil || executionState != "quiescent" || pid != 0 || runStatus != "failed" {
		t.Fatalf("corrupt input left launch window: %s pid=%d run=%s err=%v", executionState, pid, runStatus, err)
	}
}

func TestSupervisionWorkerInstalledClaudePOC(t *testing.T) {
	if os.Getenv("MILEVIA_RUN_SUPERVISION_SCHEDULER_POC") != "1" {
		t.Skip("set MILEVIA_RUN_SUPERVISION_SCHEDULER_POC=1 to test the scheduler worker")
	}
	s, p, taskID, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	writeSupervisionSourceForTest(t, p.Path, "src/main.go", "package main\nfunc add(a, b int) int { return a - b }\n")
	if _, err := s.db.Exec(`update tasks set description=? where id=?`, "修复 src/main.go 的 add 函数使其返回两数之和；验证正数、负数和零。首轮先为本任务制定 record_plan，范围限 src/main.go，可在后续实施新增 src/main_test.go；不运行实施或测试。", taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`update supervision_sessions set scheduler_agent_id='claude-code' where id=?`, ss.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.startSupervisionSession(context.Background(), ss.ID, ss.StateVersion, now); err != nil {
		t.Fatal(err)
	}
	if !s.startSupervisionWorkerLoop() {
		t.Fatal("worker did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		var state string
		err := s.db.QueryRowContext(ctx, `select state from supervision_decisions where session_id=? order by created_at desc limit 1`, ss.ID).Scan(&state)
		if err == nil && state == "applied" {
			break
		}
		current, loadErr := loadSupervisionSession(ctx, s.db, ss.ID)
		if loadErr == nil && current.State == "needs_input" {
			t.Fatalf("worker needs input: %s", current.Reason)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("worker did not apply a valid plan: decision=%s query=%v", state, err)
		case <-ticker.C:
		}
	}
	current := supervisionSessionForTest(t, s, ss.ID)
	var executions, usage, active, taskRuns int
	if err := s.db.QueryRow(`select count(*),coalesce(sum(case when state<>'quiescent' then 1 else 0 end),0) from supervision_executions where session_id=?`, ss.ID).Scan(&executions, &active); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`select count(*) from run_usage u join supervision_run_links l on l.run_id=u.run_id where l.session_id=?`, ss.ID).Scan(&usage); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`select count(*) from task_runs where task_id=?`, taskID).Scan(&taskRuns); err != nil {
		t.Fatal(err)
	}
	if executions < 1 || executions > 3 || active != 0 || current.UsedTurns != executions || usage != executions || taskRuns != 0 {
		t.Fatalf("executions=%d active=%d turns=%d usage=%d taskRuns=%d", executions, active, current.UsedTurns, usage, taskRuns)
	}
	var plans int
	if err := s.db.QueryRow(`select count(*) from supervision_plan_versions where session_id=? and task_id=? and revision=1 and status='ready'`, ss.ID, taskID).Scan(&plans); err != nil || plans != 1 {
		t.Fatalf("protected worker did not persist the selected task plan: plans=%d err=%v", plans, err)
	}
	baseline, err := loadSupervisionProjectContext(context.Background(), s.db, ss.ID, "baseline")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.supervisionRevisionContent(context.Background(), ss.ID, baseline.RevisionID, "src/main.go"); err != nil {
		t.Fatalf("worker baseline source missing: %v", err)
	}
	assertTaskStatus(t, s, taskID, taskTodo)
	assertExecutionClaimForTest(t, s, taskID, "supervision", ss.ID)
	t.Logf("worker completed %d protected scheduler attempt(s), persisted baseline/usage and applied one plan; no implementation or acceptance", executions)
}

func TestSupervisionExecutionInstalledClaudePOC(t *testing.T) {
	if os.Getenv("MILEVIA_RUN_SUPERVISION_SCHEDULER_POC") != "1" {
		t.Skip("set MILEVIA_RUN_SUPERVISION_SCHEDULER_POC=1 to test the protected execution")
	}
	binary, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal(err)
	}
	s, taskID, permit, _, _ := supervisionExecutionForTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	err = s.executeSupervisionScheduler(ctx, permit, time.Now().Add(55*time.Second), &claudeCLIRunner{config: Config{ClaudePath: binary}})
	if err != nil {
		t.Fatal(err)
	}
	var executionState, runStatus, decisionState string
	if err := s.db.QueryRow(`select e.state,r.status,d.state from supervision_executions e join runs r on r.id=e.run_id join supervision_work_items w on w.execution_id=e.execution_id join supervision_decisions d on d.id=w.decision_id where e.execution_id=?`, permit.Decision.ExecutionID).Scan(&executionState, &runStatus, &decisionState); err != nil {
		t.Fatal(err)
	}
	if executionState != "quiescent" || runStatus != "completed" || decisionState != "proposed" {
		var output, validationError string
		if err := s.db.QueryRow(`select output,validation_error from supervision_decisions where id=?`, permit.Decision.ID).Scan(&output, &validationError); err != nil {
			t.Fatal(err)
		}
		t.Fatalf("execution=%s run=%s decision=%s validation=%s output=%s", executionState, runStatus, decisionState, validationError, output)
	}
	if exists, err := s.hasPersistedRunUsage(context.Background(), permit.RunID); err != nil || !exists {
		t.Fatalf("usage missing: %v", err)
	}
	var directory, executable string
	if err := s.db.QueryRow(`select directory,executable from supervision_executions where execution_id=?`, permit.Decision.ExecutionID).Scan(&directory, &executable); err != nil || directory == "" || executable == "" {
		t.Fatalf("actual process path missing: %v", err)
	}
	assertExecutionClaimForTest(t, s, taskID, "supervision", permit.Decision.SessionID)
	t.Log("protected native CLI: process identity persisted, empty capability inventory checked, role transcript saved, container quiescent, proposal retained without applying action")
}
