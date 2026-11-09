package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func supervisionSessionForTest(t *testing.T, s *Server, id string) supervisionSession {
	t.Helper()
	ss, err := loadSupervisionSession(context.Background(), s.db, id)
	if err != nil {
		t.Fatal(err)
	}
	return ss
}

func supervisionFixture(t *testing.T, budget supervisionBudget) (*Server, Project, string, supervisionSession, time.Time) {
	t.Helper()
	s, p, _ := admissionProjectForTest(t)
	// Seed domain input without HTTP's asynchronous title refinement. A delayed
	// title pass could pick up a profile added later by a test and launch an
	// unrelated real CLI. Task creation is covered by its own HTTP tests.
	taskID := uuid.NewString()
	now := time.Now().UTC()
	description := "Implement selected task safely."
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`insert into tasks(id,project_id,title,description,priority,pinned,position,status,created_at,updated_at) values (?,?,?,?, 'normal',0,1,'todo',?,?)`, taskID, p.ID, generatedTaskTitle(description), description, now, now); err != nil {
		t.Fatal(err)
	}
	if err := s.recordTaskEventTx(context.Background(), tx, taskID, "", "task.created", map[string]string{"status": taskTodo}, now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	ss, err := s.createSupervisionDraft(context.Background(), p.ID, []string{taskID}, "codex", budget)
	if err != nil {
		t.Fatal(err)
	}
	return s, p, taskID, ss, time.Now().UTC()
}

func startSupervisionForTest(t *testing.T, s *Server, id string, now time.Time) supervisionSession {
	t.Helper()
	ss := supervisionSessionForTest(t, s, id)
	if err := s.startSupervisionSession(context.Background(), id, ss.StateVersion, now); err != nil {
		t.Fatal(err)
	}
	ss = supervisionSessionForTest(t, s, id)
	if err := s.admitSupervisionWorkspace(context.Background(), id, ss.StateVersion, now); err != nil {
		t.Fatal(err)
	}
	return supervisionSessionForTest(t, s, id)
}

func decisionForTest(t *testing.T, s *Server, id string, now time.Time) (supervisionLease, supervisionDecision) {
	t.Helper()
	lease, err := s.acquireSupervisionLease(context.Background(), id, "worker", now)
	if err != nil {
		t.Fatal(err)
	}
	ss := supervisionSessionForTest(t, s, id)
	d, err := s.prepareSupervisionDecision(context.Background(), id, ss.StateVersion, lease, "", now)
	if err != nil {
		t.Fatal(err)
	}
	return lease, d
}

func decisionProposalForTest(d supervisionDecision) string {
	return fmt.Sprintf(`{"schemaVersion":1,"decisionId":%q,"expectedStateVersion":%d,"basisSeq":%d,"reason":"等待已知资源","action":{"type":"wait","sessionId":%q,"condition":"resource_available"}}`, d.ID, d.ExpectedStateVersion, d.BasisSeq, d.SessionID)
}

func TestSupervisionDraftDoesNotClaimAndStartSnapshotsLatestTaskAtomically(t *testing.T) {
	s, p, first, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	assertExecutionClaimForTest(t, s, first, "", "")
	if _, err := s.db.Exec(`update tasks set description='updated before start' where id=?`, first); err != nil {
		t.Fatal(err)
	}
	startSupervisionForTest(t, s, ss.ID, now)
	assertExecutionClaimForTest(t, s, first, "supervision", ss.ID)
	var snapshot string
	if err := s.db.QueryRow(`select task_snapshot from supervision_tasks where session_id=? and task_id=?`, ss.ID, first).Scan(&snapshot); err != nil || !strings.Contains(snapshot, "updated before start") {
		t.Fatalf("start snapshot=%s err=%v", snapshot, err)
	}
	second := createTaskForTest(t, s.routes(), p.ID, "another selection")
	draft, err := s.createSupervisionDraft(context.Background(), p.ID, []string{second}, "codex", defaultSupervisionBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.startSupervisionSession(context.Background(), draft.ID, draft.StateVersion, now); err == nil {
		t.Fatal("second active session started")
	}
	assertExecutionClaimForTest(t, s, second, "", "")
	if got := supervisionSessionForTest(t, s, draft.ID); got.State != "draft" || got.StateVersion != 1 {
		t.Fatalf("failed start changed draft: %+v", got)
	}
	var count int
	if err := s.db.QueryRow(`select count(*) from workspace_admissions where project_id=?`, p.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("admission count=%d err=%v", count, err)
	}
}

func TestSupervisionStartFailureRollsBackEntireSelection(t *testing.T) {
	s, p, first, _, now := supervisionFixture(t, defaultSupervisionBudget())
	second := createTaskForTest(t, s.routes(), p.ID, "owned elsewhere")
	draft, err := s.createSupervisionDraft(context.Background(), p.ID, []string{first, second}, "codex", defaultSupervisionBudget())
	if err != nil {
		t.Fatal(err)
	}
	claimSupervisionTasksForTest(t, s, p.ID, "other", second)
	if err := s.startSupervisionSession(context.Background(), draft.ID, 1, now); !errors.Is(err, errTaskExecutionClaimed) {
		t.Fatalf("conflict=%v", err)
	}
	assertExecutionClaimForTest(t, s, first, "", "")
	assertExecutionClaimForTest(t, s, second, "supervision", "other")
	if _, err := s.db.Exec(`delete from task_execution_claims where task_id=?`, second); err != nil {
		t.Fatal(err)
	}
	reserveAdmissionForTest(t, s, p, "other-directory-owner")
	if err := s.startSupervisionSession(context.Background(), draft.ID, 1, now); !errors.Is(err, errWorkspaceSupervised) {
		t.Fatalf("directory conflict=%v", err)
	}
	assertExecutionClaimForTest(t, s, first, "", "")
	assertExecutionClaimForTest(t, s, second, "", "")
}

func TestSupervisionDraftValidationAndDeletionDoNotLockOrdinaryTasks(t *testing.T) {
	s, p, taskID, ss, _ := supervisionFixture(t, defaultSupervisionBudget())
	for _, ids := range [][]string{nil, {taskID, taskID}, {"foreign"}, {""}} {
		if _, err := s.createSupervisionDraft(context.Background(), p.ID, ids, "codex", defaultSupervisionBudget()); err == nil {
			t.Fatalf("invalid selection=%v", ids)
		}
	}
	if _, err := s.db.Exec(`delete from tasks where id=?`, taskID); err != nil {
		t.Fatalf("draft blocked deletion: %v", err)
	}
	if err := s.startSupervisionSession(context.Background(), ss.ID, 1, time.Now().UTC()); err == nil {
		t.Fatal("deleted task was silently dropped")
	}
	if _, err := s.createSupervisionDraft(context.Background(), p.ID, []string{taskID}, "unknown", defaultSupervisionBudget()); err == nil {
		t.Fatal("unknown scheduler accepted")
	}
}

func TestSupervisionPendingWaitsForExistingOperationAndStopReleasesOnlyAfterDrain(t *testing.T) {
	s, p, taskID, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	release, err := s.beginWorkspaceOperation(p.ID, "existing")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := s.startSupervisionSession(context.Background(), ss.ID, 1, now); err != nil {
		t.Fatal(err)
	}
	ss = supervisionSessionForTest(t, s, ss.ID)
	if err := s.admitSupervisionWorkspace(context.Background(), ss.ID, ss.StateVersion, now); !errors.Is(err, errWorkspaceNotQuiescent) {
		t.Fatalf("existing operation ignored: %v", err)
	}
	if err := s.requestSupervisionControl(context.Background(), ss.ID, ss.StateVersion, "stop", now); err != nil {
		t.Fatal(err)
	}
	if err := s.settleSupervisionControl(context.Background(), ss.ID, now); !errors.Is(err, errWorkspaceNotQuiescent) {
		t.Fatalf("live directory released: %v", err)
	}
	release()
	for range 2 {
		if err := s.settleSupervisionControl(context.Background(), ss.ID, now); err != nil {
			t.Fatal(err)
		}
	}
	assertExecutionClaimForTest(t, s, taskID, "", "")
	assertTaskStatus(t, s, taskID, taskTodo)
	ss = supervisionSessionForTest(t, s, ss.ID)
	if ss.State != "stopped" {
		t.Fatalf("state=%s", ss.State)
	}
}

func TestSupervisionDecisionWakeupsReserveOnceAndDoNotInvalidateTheirVersion(t *testing.T) {
	s, _, _, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	ss = startSupervisionForTest(t, s, ss.ID, now)
	lease, d := decisionForTest(t, s, ss.ID, now)
	results := make(chan supervisionDecision, 12)
	errorsCh := make(chan error, 12)
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := s.prepareSupervisionDecision(context.Background(), ss.ID, ss.StateVersion, lease, "", now)
			results <- result
			errorsCh <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	for result := range results {
		if result.ID != d.ID || result.ExecutionID != d.ExecutionID {
			t.Fatalf("duplicate wake created a new intent: %+v", result)
		}
	}
	current := supervisionSessionForTest(t, s, ss.ID)
	if current.StateVersion != ss.StateVersion || current.ReservedTurns != 1 || current.UsedTurns != 0 || current.EventSeq <= ss.EventSeq {
		t.Fatalf("decision changed business version or budget incorrectly: %+v", current)
	}
	deadline, err := s.claimSupervisionDecisionLaunch(context.Background(), d.ID, lease, now)
	if err != nil {
		t.Fatal(err)
	}
	if !deadline.Equal(now.Add(30 * time.Minute)) {
		t.Fatalf("deadline=%s", deadline)
	}
	if _, err := s.claimSupervisionDecisionLaunch(context.Background(), d.ID, lease, now); err == nil {
		t.Fatal("launch was delivered twice")
	}
	current = supervisionSessionForTest(t, s, ss.ID)
	if current.UsedTurns != 1 || current.ReservedTurns != 0 || current.StateVersion != ss.StateVersion {
		t.Fatalf("launch counters=%+v", current)
	}
}

func TestSupervisionPauseBeforeLaunchSuspendsAndRefundsUnstartedReservation(t *testing.T) {
	s, _, taskID, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	startSupervisionForTest(t, s, ss.ID, now)
	lease, d := decisionForTest(t, s, ss.ID, now)
	ss = supervisionSessionForTest(t, s, ss.ID)
	if err := s.requestSupervisionControl(context.Background(), ss.ID, ss.StateVersion, "pause", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.claimSupervisionDecisionLaunch(context.Background(), d.ID, lease, now); err == nil {
		t.Fatal("launch bypassed committed pause")
	}
	if err := s.settleSupervisionControl(context.Background(), ss.ID, now); err != nil {
		t.Fatal(err)
	}
	ss = supervisionSessionForTest(t, s, ss.ID)
	if ss.State != "paused" || ss.ReservedTurns != 0 || ss.UsedTurns != 0 {
		t.Fatalf("paused session=%+v", ss)
	}
	assertExecutionClaimForTest(t, s, taskID, "supervision", ss.ID)
	if err := s.requestSupervisionControl(context.Background(), ss.ID, ss.StateVersion, "stop", now); err != nil {
		t.Fatal(err)
	}
	if err := s.settleSupervisionControl(context.Background(), ss.ID, now); err != nil {
		t.Fatal(err)
	}
	assertExecutionClaimForTest(t, s, taskID, "", "")
}

func TestSupervisionLaunchBeforeStopWaitsForProcessAndKeepsConsumedBudget(t *testing.T) {
	s, _, _, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	startSupervisionForTest(t, s, ss.ID, now)
	lease, d := decisionForTest(t, s, ss.ID, now)
	if _, err := s.claimSupervisionDecisionLaunch(context.Background(), d.ID, lease, now); err != nil {
		t.Fatal(err)
	}
	ss = supervisionSessionForTest(t, s, ss.ID)
	if err := s.requestSupervisionControl(context.Background(), ss.ID, ss.StateVersion, "stop", now); err != nil {
		t.Fatal(err)
	}
	if err := s.settleSupervisionControl(context.Background(), ss.ID, now); !errors.Is(err, errWorkspaceNotQuiescent) {
		t.Fatalf("launch handshake ignored: %v", err)
	}
	state, err := s.recordSupervisionDecisionOutput(context.Background(), d.ID, d.ExecutionID, decisionProposalForTest(d), now)
	if err != nil || state != "stale" {
		t.Fatalf("late output state=%s err=%v", state, err)
	}
	if err := s.settleSupervisionControl(context.Background(), ss.ID, now); !errors.Is(err, errWorkspaceNotQuiescent) {
		t.Fatalf("model output was treated as exit: %v", err)
	}
	if err := s.confirmSupervisionDecisionQuiescent(context.Background(), d.ID, d.ExecutionID, lease.Epoch, now); err != nil {
		t.Fatal(err)
	}
	if err := s.settleSupervisionControl(context.Background(), ss.ID, now); err != nil {
		t.Fatal(err)
	}
	ss = supervisionSessionForTest(t, s, ss.ID)
	if ss.State != "stopped" || ss.UsedTurns != 1 || ss.ReservedTurns != 0 {
		t.Fatalf("stop=%+v", ss)
	}
}

func TestSupervisionLeaseExpiryFencesOldLauncherAndDoesNotResetCounters(t *testing.T) {
	s, _, _, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	startSupervisionForTest(t, s, ss.ID, now)
	old, d := decisionForTest(t, s, ss.ID, now)
	if _, err := s.acquireSupervisionLease(context.Background(), ss.ID, "other", now); !errors.Is(err, errSupervisionLease) {
		t.Fatalf("lease stolen: %v", err)
	}
	newLease, err := s.acquireSupervisionLease(context.Background(), ss.ID, "other", now.Add(31*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if newLease.Epoch <= old.Epoch {
		t.Fatal("epoch did not advance")
	}
	if _, err := s.claimSupervisionDecisionLaunch(context.Background(), d.ID, old, now.Add(31*time.Second)); !errors.Is(err, errSupervisionLease) {
		t.Fatalf("old launcher=%v", err)
	}
	ss = supervisionSessionForTest(t, s, ss.ID)
	if ss.UsedTurns != 0 || ss.ReservedTurns != 0 {
		t.Fatalf("unstarted reservation not refunded: %+v", ss)
	}
	_, newDecision := decisionForTestWithLease(t, s, ss.ID, newLease, now.Add(31*time.Second))
	if _, err := s.claimSupervisionDecisionLaunch(context.Background(), newDecision.ID, newLease, now.Add(31*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.acquireSupervisionLease(context.Background(), ss.ID, "third", now.Add(62*time.Second)); !errors.Is(err, errWorkspaceNotQuiescent) {
		t.Fatalf("expired lease bypassed live process: %v", err)
	}
}

func decisionForTestWithLease(t *testing.T, s *Server, id string, lease supervisionLease, now time.Time) (supervisionSession, supervisionDecision) {
	t.Helper()
	ss := supervisionSessionForTest(t, s, id)
	d, err := s.prepareSupervisionDecision(context.Background(), id, ss.StateVersion, lease, "", now)
	if err != nil {
		t.Fatal(err)
	}
	return ss, d
}

func TestSupervisionInvalidOutputCorrectionsAreBoundedAndCharged(t *testing.T) {
	s, _, _, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	startSupervisionForTest(t, s, ss.ID, now)
	lease, d := decisionForTest(t, s, ss.ID, now)
	for attempt := 1; attempt <= 3; attempt++ {
		if _, err := s.claimSupervisionDecisionLaunch(context.Background(), d.ID, lease, now); err != nil {
			t.Fatal(err)
		}
		state, err := s.recordSupervisionDecisionOutput(context.Background(), d.ID, d.ExecutionID, "invalid JSON", now)
		if err != nil || state != "invalid" {
			t.Fatalf("output=%s err=%v", state, err)
		}
		if err := s.confirmSupervisionDecisionQuiescent(context.Background(), d.ID, d.ExecutionID, lease.Epoch, now); err != nil {
			t.Fatal(err)
		}
		if attempt < 3 {
			ss = supervisionSessionForTest(t, s, ss.ID)
			replayed, err := s.prepareSupervisionDecision(context.Background(), ss.ID, ss.StateVersion, lease, "", now)
			if err != nil || replayed.ID != d.ID || replayed.Attempt != attempt {
				t.Fatalf("same fact wake created another decision: %+v %v", replayed, err)
			}
			d, err = s.prepareSupervisionDecision(context.Background(), ss.ID, ss.StateVersion, lease, d.ID, now)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	ss = supervisionSessionForTest(t, s, ss.ID)
	if ss.UsedTurns != 3 || ss.State != "needs_input" || ss.Reason != "invalid_scheduler_output" {
		t.Fatalf("unbounded corrections: %+v", ss)
	}
	if _, err := s.prepareSupervisionDecision(context.Background(), ss.ID, ss.StateVersion, lease, d.ID, now); err == nil {
		t.Fatal("fourth automatic correction accepted")
	}
}

func TestSupervisionDecisionOutputReplayIsImmutableAndDoesNotAdvanceBusinessState(t *testing.T) {
	s, _, _, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	ss = startSupervisionForTest(t, s, ss.ID, now)
	lease, d := decisionForTest(t, s, ss.ID, now)
	if _, err := s.claimSupervisionDecisionLaunch(context.Background(), d.ID, lease, now); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		state, err := s.recordSupervisionDecisionOutput(context.Background(), d.ID, d.ExecutionID, decisionProposalForTest(d), now)
		if err != nil || state != "proposed" {
			t.Fatalf("proposal=%s %v", state, err)
		}
	}
	if _, err := s.recordSupervisionDecisionOutput(context.Background(), d.ID, d.ExecutionID, strings.ReplaceAll(decisionProposalForTest(d), "resource_available", "different"), now); err == nil {
		t.Fatal("immutable result overwritten")
	}
	if current := supervisionSessionForTest(t, s, ss.ID); current.StateVersion != ss.StateVersion {
		t.Fatal("assistant output invalidated its own decision")
	}
	events, err := s.supervisionEvents(context.Background(), ss.ID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for i, event := range events {
		if event.Seq != int64(i+1) {
			t.Fatalf("event gap: %+v", event)
		}
		if event.Type == "decision.result" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("duplicate result events=%d", count)
	}
	last := events[len(events)-1].Seq
	if page, err := s.supervisionEvents(context.Background(), ss.ID, last, 50); err != nil || len(page) != 0 {
		t.Fatalf("cursor replay=%v %v", page, err)
	}
}

func TestSupervisionProposalRejectsMultipleActionsDuplicateKeysAndForeignIdentity(t *testing.T) {
	d := supervisionDecision{ID: "decision", SessionID: "session", ExpectedStateVersion: 3, BasisSeq: 8}
	valid := decisionProposalForTest(d)
	if _, err := parseSupervisionProposal(valid, d); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		valid + valid,
		strings.Replace(valid, `"schemaVersion":1`, `"schemaVersion":1,"schemaVersion":1`, 1),
		strings.Replace(valid, `"schemaVersion":1`, `"SchemaVersion":1`, 1),
		strings.Replace(valid, `"schemaVersion":1`, `"schemaVersion":1,"SchemaVersion":1`, 1),
		strings.Replace(valid, `"type":"wait"`, `"type":"wait","type":"record_plan"`, 1),
		strings.Replace(valid, `"sessionId":"session"`, `"sessionId":"other"`, 1),
		strings.Replace(valid, `"type":"wait"`, `"type":"run_shell"`, 1),
		strings.Replace(valid, `"expectedStateVersion":3`, `"expectedStateVersion":4`, 1),
		strings.Replace(valid, `"action":{`, `"unknown":1,"action":{`, 1),
		strings.Replace(valid, `"action":{`, `"action":[{`, 1),
		valid + strings.Repeat(" ", 256*1024),
	} {
		if _, err := parseSupervisionProposal(raw, d); err == nil {
			t.Fatalf("invalid protocol accepted: %.300s", raw)
		}
	}
}

func TestSupervisionBudgetReservesFinalShareAndDoesNotRefundLaunchFailure(t *testing.T) {
	budget := defaultSupervisionBudget()
	budget.MaxAgentTurns = 5
	s, _, _, ss, now := supervisionFixture(t, budget)
	startSupervisionForTest(t, s, ss.ID, now)
	lease, d := decisionForTest(t, s, ss.ID, now)
	if _, err := s.claimSupervisionDecisionLaunch(context.Background(), d.ID, lease, now); err != nil {
		t.Fatal(err)
	}
	// 真实启动失败也必须经过退出核对；已发生的尝试不能退还。
	if err := s.confirmSupervisionDecisionQuiescent(context.Background(), d.ID, d.ExecutionID, lease.Epoch, now); err != nil {
		t.Fatal(err)
	}
	if current := supervisionSessionForTest(t, s, ss.ID); current.UsedTurns != 1 {
		t.Fatal("failed launch refunded consumed turn")
	}
	// 填充其余已启动工作回合，模拟跨阶段/跨决策的累计。
	for i := 0; i < 3; i++ {
		if _, err := s.db.Exec(`insert into supervision_work_items(id,session_id,decision_id,role,attempt,execution_id,state,lease_epoch,admission_epoch,budget_class,turn_state,created_at) values (?,?,?,'scheduler',?,?,'quiescent',?,1,'work','consumed',?)`, fmt.Sprintf("used-%d", i), ss.ID, d.ID, i+2, fmt.Sprintf("used-exec-%d", i), lease.Epoch, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`update supervision_sessions set used_turns=4 where id=?;update supervision_decisions set state='stale' where id=?`, ss.ID, d.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.checkSupervisionBudget(context.Background(), ss.ID, now); err != nil {
		t.Fatal(err)
	}
	ss = supervisionSessionForTest(t, s, ss.ID)
	if ss.State != "pausing" || ss.Reason != "budget_exhausted" {
		t.Fatalf("final share consumed by work: %+v", ss)
	}
	if err := s.settleSupervisionControl(context.Background(), ss.ID, now); err != nil {
		t.Fatal(err)
	}
	if current := supervisionSessionForTest(t, s, ss.ID); current.UsedTurns != 4 || current.State != "paused" {
		t.Fatalf("budget pause=%+v", current)
	}
}

func TestSupervisionBudgetDecreaseBelowUsedStopsNewLaunchesAndTimeIsConservative(t *testing.T) {
	s, _, _, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	startSupervisionForTest(t, s, ss.ID, now)
	lease, d := decisionForTest(t, s, ss.ID, now)
	if _, err := s.claimSupervisionDecisionLaunch(context.Background(), d.ID, lease, now); err != nil {
		t.Fatal(err)
	}
	ss = supervisionSessionForTest(t, s, ss.ID)
	budget := ss.Budget
	budget.MaxAgentTurns = 1
	if err := s.updateSupervisionBudget(context.Background(), ss.ID, ss.StateVersion, budget, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	ss = supervisionSessionForTest(t, s, ss.ID)
	if ss.State != "pausing" || ss.ActiveMillis != 1000 || ss.UsedTurns != 1 {
		t.Fatalf("lowering budget=%+v", ss)
	}
	if err := s.settleSupervisionControl(context.Background(), ss.ID, now.Add(2*time.Second)); !errors.Is(err, errWorkspaceNotQuiescent) {
		t.Fatalf("budget stop ignored process: %v", err)
	}
	if err := s.confirmSupervisionDecisionQuiescent(context.Background(), d.ID, d.ExecutionID, lease.Epoch, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.settleSupervisionControl(context.Background(), ss.ID, now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.checkSupervisionBudget(context.Background(), ss.ID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if current := supervisionSessionForTest(t, s, ss.ID); current.ActiveMillis != 4000 {
		t.Fatalf("paused time was charged: %+v", current)
	}
}

func TestSupervisionBudgetDeadlineUsesRemainingWorkShareAndTimerPauses(t *testing.T) {
	budget := defaultSupervisionBudget()
	budget.MaxActiveMillis = 10000
	s, _, _, ss, now := supervisionFixture(t, budget)
	startSupervisionForTest(t, s, ss.ID, now)
	lease, d := decisionForTest(t, s, ss.ID, now)
	deadline, err := s.claimSupervisionDecisionLaunch(context.Background(), d.ID, lease, now)
	if err != nil {
		t.Fatal(err)
	}
	if !deadline.Equal(now.Add(8 * time.Second)) {
		t.Fatalf("deadline did not reserve final time: %s", deadline)
	}
	if err := s.checkSupervisionBudget(context.Background(), ss.ID, now.Add(8*time.Second)); err != nil {
		t.Fatal(err)
	}
	ss = supervisionSessionForTest(t, s, ss.ID)
	if ss.State != "pausing" || ss.Reason != "budget_exhausted" || ss.ActiveMillis != 8000 {
		t.Fatalf("timer failed to pause: %+v", ss)
	}
}

func TestSupervisionRecoveryKeepsUnknownExecutionAndFencesLateCallbacks(t *testing.T) {
	s, _, taskID, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	startSupervisionForTest(t, s, ss.ID, now)
	lease, d := decisionForTest(t, s, ss.ID, now)
	if _, err := s.claimSupervisionDecisionLaunch(context.Background(), d.ID, lease, now); err != nil {
		t.Fatal(err)
	}
	if err := s.recoverWorkspaceAdmissions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.recoverSupervisionSessions(context.Background(), now.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	ss = supervisionSessionForTest(t, s, ss.ID)
	if ss.RecoveryState != "outcome_unknown" || ss.AdmissionEpoch != 2 || ss.LeaseEpoch <= lease.Epoch || ss.ActiveMillis != 10000 {
		t.Fatalf("recovery=%+v", ss)
	}
	if err := s.confirmSupervisionDecisionQuiescent(context.Background(), d.ID, d.ExecutionID, lease.Epoch, now.Add(11*time.Second)); !errors.Is(err, errSupervisionLease) {
		t.Fatalf("late callback bypassed recovery: %v", err)
	}
	state, err := s.recordSupervisionDecisionOutput(context.Background(), d.ID, d.ExecutionID, decisionProposalForTest(d), now.Add(11*time.Second))
	if err != nil || state != "stale" {
		t.Fatalf("late output=%s %v", state, err)
	}
	assertExecutionClaimForTest(t, s, taskID, "supervision", ss.ID)
	if _, err := s.acquireSupervisionLease(context.Background(), ss.ID, "new", now.Add(12*time.Second)); err == nil {
		t.Fatal("unknown execution was automatically rescheduled")
	}
}

func TestSupervisionRecoveryPreservesPausedAndRefundsOnlyQueuedTurns(t *testing.T) {
	s, _, _, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	startSupervisionForTest(t, s, ss.ID, now)
	_, _ = decisionForTest(t, s, ss.ID, now)
	if err := s.recoverWorkspaceAdmissions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.recoverSupervisionSessions(context.Background(), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	ss = supervisionSessionForTest(t, s, ss.ID)
	if ss.ReservedTurns != 0 || ss.UsedTurns != 0 || ss.ActiveMillis != 0 || ss.RecoveryState != "reconciling" || ss.ClockAt != nil {
		t.Fatalf("idle shutdown charged runtime: %+v", ss)
	}
	// 独立 fixture 验证显式暂停不会被重启改回 running。
	s2, _, _, paused, now2 := supervisionFixture(t, defaultSupervisionBudget())
	paused = startSupervisionForTest(t, s2, paused.ID, now2)
	if err := s2.requestSupervisionControl(context.Background(), paused.ID, paused.StateVersion, "pause", now2); err != nil {
		t.Fatal(err)
	}
	if err := s2.settleSupervisionControl(context.Background(), paused.ID, now2); err != nil {
		t.Fatal(err)
	}
	if err := s2.recoverWorkspaceAdmissions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s2.recoverSupervisionSessions(context.Background(), now2.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if current := supervisionSessionForTest(t, s2, paused.ID); current.State != "paused" || current.Control != "pause" {
		t.Fatalf("pause intent lost: %+v", current)
	}
}

func TestSupervisionStopAfterImplementationDoesNotAcceptTask(t *testing.T) {
	s, _, taskID, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	ss = startSupervisionForTest(t, s, ss.ID, now)
	if _, err := s.db.Exec(`update supervision_tasks set implementation_started=1 where session_id=?;update tasks set status='awaiting_review' where id=?`, ss.ID, taskID); err != nil {
		t.Fatal(err)
	}
	if err := s.requestSupervisionControl(context.Background(), ss.ID, ss.StateVersion, "stop", now); err != nil {
		t.Fatal(err)
	}
	if err := s.settleSupervisionControl(context.Background(), ss.ID, now); err != nil {
		t.Fatal(err)
	}
	assertTaskStatus(t, s, taskID, taskActionRequired)
	assertExecutionClaimForTest(t, s, taskID, "", "")
}

func TestSupervisionEventTransactionRollbackDoesNotLeaveSequenceGaps(t *testing.T) {
	s, _, _, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	cause := errors.New("roll back observation")
	err := s.withSupervisionTx(context.Background(), ss.ID, now, func(tx *sql.Tx, _ map[string]WorkspaceAdmission, current *supervisionSession) error {
		if err := appendSupervisionEventTx(context.Background(), tx, current, "observation", json.RawMessage(`{"ok":true}`), now); err != nil {
			return err
		}
		return cause
	})
	if !errors.Is(err, cause) {
		t.Fatal(err)
	}
	events, err := s.supervisionEvents(context.Background(), ss.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Seq != 1 || supervisionSessionForTest(t, s, ss.ID).EventSeq != 1 {
		t.Fatalf("rollback published event=%+v", events)
	}
}

func TestSupervisionConcurrentControlAndLaunchHaveOneOrderedOutcome(t *testing.T) {
	for _, control := range []string{"pause", "stop"} {
		t.Run(control, func(t *testing.T) {
			s, _, _, ss, now := supervisionFixture(t, defaultSupervisionBudget())
			ss = startSupervisionForTest(t, s, ss.ID, now)
			lease, d := decisionForTest(t, s, ss.ID, now)
			ready := make(chan struct{})
			launch := make(chan error, 1)
			command := make(chan error, 1)
			go func() {
				<-ready
				_, err := s.claimSupervisionDecisionLaunch(context.Background(), d.ID, lease, now)
				launch <- err
			}()
			go func() {
				<-ready
				command <- s.requestSupervisionControl(context.Background(), ss.ID, ss.StateVersion, control, now)
			}()
			close(ready)
			launchErr, controlErr := <-launch, <-command
			if controlErr != nil {
				t.Fatal(controlErr)
			}
			if launchErr != nil && !errors.Is(launchErr, errSupervisionState) {
				t.Fatal(launchErr)
			}
			current := supervisionSessionForTest(t, s, ss.ID)
			if current.Control != control || current.ReservedTurns != 0 {
				t.Fatalf("control lost: %+v", current)
			}
			if launchErr == nil {
				if current.UsedTurns != 1 {
					t.Fatal("launched turn was refunded")
				}
				if err := s.settleSupervisionControl(context.Background(), ss.ID, now); !errors.Is(err, errWorkspaceNotQuiescent) {
					t.Fatalf("handshake did not block settlement: %v", err)
				}
				if err := s.confirmSupervisionDecisionQuiescent(context.Background(), d.ID, d.ExecutionID, lease.Epoch, now); err != nil {
					t.Fatal(err)
				}
			} else if current.UsedTurns != 0 {
				t.Fatal("blocked launch charged a turn")
			}
			if err := s.settleSupervisionControl(context.Background(), ss.ID, now); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSupervisionStartupRestoresProtectionAndBudgetLedger(t *testing.T) {
	s, p, taskID, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	startSupervisionForTest(t, s, ss.ID, now)
	lease, d := decisionForTest(t, s, ss.ID, now)
	if _, err := s.claimSupervisionDecisionLaunch(context.Background(), d.ID, lease, now); err != nil {
		t.Fatal(err)
	}
	// 模拟老汇总缺失：恢复必须依照 consumed 账本，不能把可用回合重置。
	if _, err := s.db.Exec(`update supervision_sessions set used_turns=0 where id=?`, ss.ID); err != nil {
		t.Fatal(err)
	}
	config := s.config
	s.Close()
	reopened, err := NewWithRunner(context.Background(), config, runnerFunc(func(context.Context, AgentRunRequest, AgentRunSink) error {
		return errors.New("unexpected agent execution during recovery")
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reopened.Close)
	current := supervisionSessionForTest(t, reopened, ss.ID)
	if current.UsedTurns != 1 || current.ReservedTurns != 0 || current.RecoveryState != "outcome_unknown" || current.AdmissionEpoch != 2 || current.LeaseEpoch <= lease.Epoch {
		t.Fatalf("startup state=%+v", current)
	}
	assertExecutionClaimForTest(t, reopened, taskID, "supervision", ss.ID)
	if release, err := reopened.beginWorkspaceOperationAtPath(p, p.Path, "file"); err == nil {
		release()
		t.Fatal("startup opened directory before process reconciliation")
	}
}

func TestSupervisionBudgetValidationAndUnavailableReservationArePersisted(t *testing.T) {
	for _, mutate := range []func(*supervisionBudget){
		func(b *supervisionBudget) { b.MaxAgentTurns = 0 }, func(b *supervisionBudget) { b.MaxActiveMillis = -1 },
		func(b *supervisionBudget) { b.MaxTurnMillis = int64(time.Hour / time.Millisecond) }, func(b *supervisionBudget) { b.FinalReservePercent = 0 },
		func(b *supervisionBudget) { b.FinalReservePercent = 100 }, func(b *supervisionBudget) { b.MaxFixRounds = -1 },
	} {
		budget := defaultSupervisionBudget()
		mutate(&budget)
		if err := budget.validate(); err == nil {
			t.Fatalf("invalid budget=%+v", budget)
		}
	}
	budget := defaultSupervisionBudget()
	budget.MaxAgentTurns = 1
	s, _, _, ss, now := supervisionFixture(t, budget)
	ss = startSupervisionForTest(t, s, ss.ID, now)
	lease, err := s.acquireSupervisionLease(context.Background(), ss.ID, "worker", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.prepareSupervisionDecision(context.Background(), ss.ID, ss.StateVersion, lease, "", now); !errors.Is(err, errSupervisionBudget) {
		t.Fatalf("unavailable work share admitted: %v", err)
	}
	if current := supervisionSessionForTest(t, s, ss.ID); current.State != "pausing" || current.Reason != "budget_exhausted" || current.ReservedTurns != 0 {
		t.Fatalf("budget failure rolled back pause: %+v", current)
	}
}

func TestSupervisionRunRegistrationAfterControlPreservesIdentityAndRequestsCorrectCancellation(t *testing.T) {
	for _, control := range []string{"pause", "stop"} {
		t.Run(control, func(t *testing.T) {
			s, p, _, ss, now := supervisionFixture(t, defaultSupervisionBudget())
			ss = startSupervisionForTest(t, s, ss.ID, now)
			lease, d := decisionForTest(t, s, ss.ID, now)
			if _, err := s.claimSupervisionDecisionLaunch(context.Background(), d.ID, lease, now); err != nil {
				t.Fatal(err)
			}
			if err := s.requestSupervisionControl(context.Background(), ss.ID, ss.StateVersion, control, now); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`insert into runs(id,conversation_id,status,workspace_path,created_at) values ('registered-run','conversation','running',?,?)`, p.Path, now); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				cancelRequired, err := s.bindSupervisionDecisionRun(context.Background(), d.ID, d.ExecutionID, "conversation", "registered-run", lease, now)
				if err != nil || cancelRequired != (control == "stop") {
					t.Fatalf("registration cancel=%v err=%v", cancelRequired, err)
				}
			}
			if err := s.confirmSupervisionDecisionQuiescent(context.Background(), d.ID, d.ExecutionID, lease.Epoch, now); !errors.Is(err, errWorkspaceNotQuiescent) {
				t.Fatalf("registration lost process: %v", err)
			}
			if _, err := s.db.Exec(`update runs set status='completed' where id='registered-run'`); err != nil {
				t.Fatal(err)
			}
			if err := s.confirmSupervisionDecisionQuiescent(context.Background(), d.ID, d.ExecutionID, lease.Epoch, now); !errors.Is(err, errWorkspaceNotQuiescent) {
				t.Fatalf("Run completion proved process exit: %v", err)
			}
			if _, err := s.db.Exec(`update supervision_run_links set process_state='quiescent' where run_id='registered-run'`); err != nil {
				t.Fatal(err)
			}
			if err := s.confirmSupervisionDecisionQuiescent(context.Background(), d.ID, d.ExecutionID, lease.Epoch, now); err != nil {
				t.Fatal(err)
			}
			if err := s.settleSupervisionControl(context.Background(), ss.ID, now); err != nil {
				t.Fatal(err)
			}
			if control == "stop" {
				if _, err := s.db.Exec(`delete from projects where id=?`, p.ID); err != nil {
					t.Fatalf("terminal supervision blocked project cascade: %v", err)
				}
			}
		})
	}
}

func TestSupervisionFinalShareIsUsableButNeverExpandsTotalBudget(t *testing.T) {
	budget := defaultSupervisionBudget()
	budget.MaxAgentTurns = 1
	budget.MaxActiveMillis = 10000
	s, _, _, ss, now := supervisionFixture(t, budget)
	ss = startSupervisionForTest(t, s, ss.ID, now)
	if err := s.withSupervisionTx(context.Background(), ss.ID, now, func(_ *sql.Tx, _ map[string]WorkspaceAdmission, current *supervisionSession) error {
		current.State = "finalizing"
		current.StateVersion++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	lease, d := decisionForTest(t, s, ss.ID, now)
	deadline, err := s.claimSupervisionDecisionLaunch(context.Background(), d.ID, lease, now)
	if err != nil {
		t.Fatal(err)
	}
	if !deadline.Equal(now.Add(10 * time.Second)) {
		t.Fatalf("final deadline=%v", deadline)
	}
	if _, err := s.recordSupervisionDecisionOutput(context.Background(), d.ID, d.ExecutionID, "invalid", now); err != nil {
		t.Fatal(err)
	}
	if err := s.confirmSupervisionDecisionQuiescent(context.Background(), d.ID, d.ExecutionID, lease.Epoch, now); err != nil {
		t.Fatal(err)
	}
	current := supervisionSessionForTest(t, s, ss.ID)
	if _, err := s.prepareSupervisionDecision(context.Background(), ss.ID, current.StateVersion, lease, d.ID, now); !errors.Is(err, errSupervisionBudget) {
		t.Fatalf("final share expanded total turns: %v", err)
	}
}
