//go:build windows

package app

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

func TestSupervisionTargetTurnCancellationPersistsAndDeliversRevocation(t *testing.T) {
	executor := &supervisionTargetTurnTestExecutor{wait: make(chan struct{}), started: make(chan struct{})}
	s, permit, link := supervisionTargetTurnFixture(t, executor)
	pool := newSupervisionTargetConnections(s.runtimeCtx, func(context.Context, string) (*supervisionTargetLink, error) { return link, nil })
	t.Cleanup(func() { pool.Close() })
	borrowed, err := pool.acquire(context.Background(), "fixture-runner")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { _, err := s.runSupervisionTargetTurn(ctx, borrowed, permit); finished <- err }()
	select {
	case <-executor.started:
	case <-time.After(10 * time.Second):
		t.Fatal("target did not start")
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("caller cancellation lost", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("cancelled turn did not finish")
	}
	ss := supervisionSessionForTest(t, s, permit.Decision.SessionID)
	intent, err := loadSupervisionTargetControl(context.Background(), s.db, ss.ID)
	if err != nil || ss.RecoveryState != "outcome_unknown" || intent.Control.State != "paused" || intent.Control.DrainExecutionID != "" || intent.Acknowledged != intent.Control.Version || intent.Wire == nil {
		t.Fatal("cancellation did not durably revoke target", ss.RecoveryState, intent, err)
	}
	actual, err := executor.store.Session(ss.ID)
	if err != nil || actual.Control != *intent.Wire {
		t.Fatal("target did not receive revocation", actual.Control, err)
	}
	var state string
	if err := s.db.QueryRow(`select state from supervision_executions where execution_id=?`, permit.Decision.ExecutionID).Scan(&state); err != nil || state != "unknown" {
		t.Fatal("uncertain cancelled outcome was settled without reconciliation", state, err)
	}
	var admissions int
	if err := s.db.QueryRow(`select count(*) from workspace_admissions where owner_id=?`, ss.ID).Scan(&admissions); err != nil || admissions != 1 {
		t.Fatal("cancelled turn released protection", admissions, err)
	}
}

func TestSupervisionTargetUnknownRequiresOriginalExecutionIdentity(t *testing.T) {
	s, permit, link := supervisionTargetTurnFixture(t, &supervisionTargetTurnTestExecutor{})
	if _, _, err := s.prepareSupervisionTargetDispatch(context.Background(), link, permit); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*supervisionExecutionPermit){
		"token":     func(p *supervisionExecutionPermit) { p.Token = "wrong" },
		"run":       func(p *supervisionExecutionPermit) { p.RunID = "foreign-run" },
		"container": func(p *supervisionExecutionPermit) { p.Container = "foreign-container" },
		"work-item": func(p *supervisionExecutionPermit) { p.Decision.WorkItemID = "foreign-work" },
		"epoch":     func(p *supervisionExecutionPermit) { p.Lease.Epoch++ },
	} {
		t.Run(name, func(t *testing.T) {
			forged := permit
			change(&forged)
			if err := s.markSupervisionTargetExecutionUnknown(context.Background(), forged, context.Canceled); !errors.Is(err, errSupervisionLease) {
				t.Fatal("foreign cleanup identity accepted", err)
			}
		})
	}
	ss := supervisionSessionForTest(t, s, permit.Decision.SessionID)
	if ss.RecoveryState != "ready" {
		t.Fatal("forged cleanup changed session", ss.RecoveryState)
	}
	// Expired authority still permits recording facts about the original turn.
	if err := s.withSupervisionTx(context.Background(), ss.ID, time.Now().UTC(), func(_ *sql.Tx, _ map[string]WorkspaceAdmission, current *supervisionSession) error {
		current.LeaseEpoch++
		current.LeaseOwner = "replacement-owner"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.markSupervisionTargetExecutionUnknown(context.Background(), permit, context.Canceled); err != nil {
		t.Fatal("expired original permit could not record uncertainty", err)
	}
	ss = supervisionSessionForTest(t, s, ss.ID)
	if ss.LeaseEpoch != permit.Lease.Epoch+1 || ss.LeaseOwner != "replacement-owner" || ss.RecoveryState != "outcome_unknown" {
		t.Fatal("cleanup changed new ownership or missed uncertainty", ss)
	}
}

type supervisionTargetTurnTestExecutor struct {
	store   *targetexec.Store
	starts  atomic.Int32
	failure bool
	wait    chan struct{}
	started chan struct{}
}

// This executor is a protocol/ledger fixture, not a real CLI/model experiment.
func (e *supervisionTargetTurnTestExecutor) Run(ctx context.Context, execution targetexec.Execution, _ targetexec.Input, handshake targetexec.Handshake) (targetexec.Receipt, error) {
	e.starts.Add(1)
	if err := handshake(targetexec.ProcessIdentity{Container: execution.Container, PID: 4242, Birth: "fixture-birth", Executable: execution.Spec.CLIPath}, func() error { return nil }); err != nil {
		return targetexec.Receipt{Quiet: true}, err
	}
	if e.started != nil {
		close(e.started)
	}
	var runErr error
	if e.wait != nil {
		select {
		case <-e.wait:
		case <-ctx.Done():
			runErr = ctx.Err()
		}
	}
	if e.failure {
		runErr = errors.New("fixture target turn failed")
	}
	info := execution.Spec
	receipt := supervisionSchedulerCLIReceipt{AgentID: info.AgentID, CLIPath: info.CLIPath, CLIVersion: info.CLIVersion, Contract: info.Contract, InputSHA256: info.InputSHA256, Initialized: true, ProcessStarted: true, ProcessWaited: true, ContainerQuiet: true, Output: "fixture target response"}
	if _, err := e.store.AppendEvent(info.SessionID, info.ExecutionID, "output", "assistant.message", mustJSON(map[string]string{"content": receipt.Output})); err != nil {
		return targetexec.Receipt{Quiet: true}, err
	}
	payload, _ := supervisionTargetPayload(mustJSON(map[string]any{"receipt": receipt, "failed": runErr != nil, "error": errorString(runErr)}))
	if _, err := e.store.AppendEvent(info.SessionID, info.ExecutionID, "result", "execution.result", payload); err != nil {
		return targetexec.Receipt{Quiet: true}, err
	}
	return targetexec.Receipt{Quiet: true, ResultSHA256: supervisionSHA256(payload), Usage: targetexec.Usage{Sequence: 1, InputTokens: 12, OutputTokens: 4, ActiveMillis: 1}}, runErr
}

func (e *supervisionTargetTurnTestExecutor) StopAndProbe(context.Context, targetexec.Execution) (bool, error) {
	return true, nil
}

func supervisionTargetTurnFixture(t *testing.T, executor *supervisionTargetTurnTestExecutor) (*Server, supervisionExecutionPermit, *supervisionTargetLink) {
	t.Helper()
	s, permit, target := supervisionTargetControlFixture(t)
	executor.store = target.store
	supervisor, err := targetexec.NewSupervisor(context.Background(), target.store, executor)
	if err != nil {
		t.Fatal(err)
	}
	left, right := net.Pipe()
	done := make(chan error, 1)
	transport := &targetexec.Transport{Store: target.store, Supervisor: supervisor, Extension: target.handle, ValidateControl: target.validateControl}
	go func() { done <- transport.Serve(context.Background(), right) }()
	client := targetexec.NewClient(left)
	t.Cleanup(func() { client.Close(); <-done })
	hello, err := client.Call(context.Background(), targetexec.Request{ID: "hello", Method: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	link := &supervisionTargetLink{Client: client, TargetID: hello.TargetID}
	if err := s.deliverSupervisionTargetControl(context.Background(), link, permit.Decision.SessionID); err != nil {
		t.Fatal(err)
	}
	intent, err := loadSupervisionTargetControl(context.Background(), s.db, permit.Decision.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	input, err := s.supervisionTargetExecutionInput(context.Background(), permit)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.recordSupervisionTargetResolution(context.Background(), permit, intent.Control, input, supervisionCLIInfo{AgentID: permit.AgentID, Path: `C:\target\fixture.exe`, Version: "1.0", Contract: "fixture-v1"}); err != nil {
		t.Fatal(err)
	}
	return s, permit, link
}

func TestSupervisionTargetTurnDispatchesConsumesAndSettlesWithoutRepeatingStart(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed", true: "failed"}[failure], func(t *testing.T) {
			executor := &supervisionTargetTurnTestExecutor{failure: failure}
			s, permit, link := supervisionTargetTurnFixture(t, executor)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			outcome, err := s.runSupervisionTargetTurn(ctx, link, permit)
			if err != nil || (outcome.RunError != nil) != failure || outcome.Receipt.Output != "fixture target response" {
				t.Fatal("turn outcome lost", outcome, err)
			}
			if executor.starts.Load() != 1 {
				t.Fatal("duplicate target start", executor.starts.Load())
			}
			if _, err := s.runSupervisionTargetTurn(ctx, link, permit); err == nil {
				t.Fatal("settled work item started again")
			}
			var state, status string
			if err := s.db.QueryRow(`select e.state,r.status from supervision_executions e join runs r on r.id=e.run_id where e.execution_id=?`, permit.Decision.ExecutionID).Scan(&state, &status); err != nil {
				t.Fatal(err)
			}
			want := "completed"
			if failure {
				want = "failed"
			}
			if state != "quiescent" || status != want {
				t.Fatal("target turn misclassified", state, status)
			}
			var messages, tokens int
			if err := s.db.QueryRow(`select (select count(*) from messages where run_id=? and role='assistant'),(select input_tokens from run_usage where run_id=?)`, permit.RunID, permit.RunID).Scan(&messages, &tokens); err != nil || messages != 1 || tokens != 12 {
				t.Fatal("turn transcript/usage not consumed exactly once", messages, tokens, err)
			}
		})
	}
}

func TestSupervisionTargetTurnStopPropagatesToActualTargetAndSettlesInterrupted(t *testing.T) {
	executor := &supervisionTargetTurnTestExecutor{wait: make(chan struct{}), started: make(chan struct{})}
	s, permit, link := supervisionTargetTurnFixture(t, executor)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() { _, err := s.runSupervisionTargetTurn(ctx, link, permit); finished <- err }()
	select {
	case <-executor.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	ss := supervisionSessionForTest(t, s, permit.Decision.SessionID)
	if err := s.requestSupervisionControl(ctx, ss.ID, ss.StateVersion, "stop", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal("known stopped turn became unknown", err)
		}
	case <-ctx.Done():
		t.Fatal("target stop did not converge")
	}
	var state, status, recovery string
	if err := s.db.QueryRow(`select e.state,r.status,ss.recovery_state from supervision_executions e join runs r on r.id=e.run_id join supervision_sessions ss on ss.id=e.session_id where e.execution_id=?`, permit.Decision.ExecutionID).Scan(&state, &status, &recovery); err != nil {
		t.Fatal(err)
	}
	if state != "quiescent" || status != "interrupted" || recovery != "ready" || executor.starts.Load() != 1 {
		t.Fatal("target stop did not settle original turn", state, status, recovery)
	}
}

func TestSupervisionTargetTurnPauseDrainsThenConfirmsTargetQuiet(t *testing.T) {
	executor := &supervisionTargetTurnTestExecutor{wait: make(chan struct{}), started: make(chan struct{})}
	s, permit, link := supervisionTargetTurnFixture(t, executor)
	s.supervisionTargets = newSupervisionTargetConnections(s.runtimeCtx, func(context.Context, string) (*supervisionTargetLink, error) { return link, nil })
	t.Cleanup(func() { s.supervisionTargets.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() { _, err := s.runSupervisionTargetTurn(ctx, link, permit); finished <- err }()
	select {
	case <-executor.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for {
		var state string
		if err := s.db.QueryRow(`select state from supervision_executions where execution_id=?`, permit.Decision.ExecutionID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state == "running" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	ss := supervisionSessionForTest(t, s, permit.Decision.SessionID)
	if err := s.requestSupervisionControl(ctx, ss.ID, ss.StateVersion, "pause", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	intent, err := loadSupervisionTargetControl(ctx, s.db, ss.ID)
	if err != nil || intent.Control.State != "paused" || intent.Control.DrainExecutionID != permit.Decision.ExecutionID {
		t.Fatal("pause did not bind the already observed turn", intent, err)
	}
	if err := s.settleSupervisionControl(ctx, ss.ID, time.Now().UTC()); !errors.Is(err, errWorkspaceNotQuiescent) {
		t.Fatal("pause settled before running turn drained", err)
	}
	if err := s.deliverSupervisionTargetControl(ctx, link, ss.ID); err != nil {
		t.Fatal(err)
	}
	close(executor.wait)
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal("drained result became unknown", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := s.settleSupervisionControl(ctx, ss.ID, time.Now().UTC()); err != nil {
		t.Fatal("quiet target could not settle pause", err)
	}
	actual, err := executor.store.Session(ss.ID)
	if err != nil || actual.Control.State != "paused" || actual.Control.DrainExecutionID != "" || actual.Released || executor.starts.Load() != 1 {
		t.Fatal("pause left an active grant or released protection", actual, err)
	}
	if got := supervisionSessionForTest(t, s, ss.ID); got.State != "paused" || got.RecoveryState != "ready" {
		t.Fatal("controller pause did not settle", got)
	}
}
