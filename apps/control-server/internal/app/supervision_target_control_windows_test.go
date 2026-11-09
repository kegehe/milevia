//go:build windows

package app

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

func supervisionTargetControlFixture(t *testing.T) (*Server, supervisionExecutionPermit, *supervisionTargetRuntime) {
	t.Helper()
	s, _, permit, _, _ := supervisionExecutionForTest(t)
	ss := supervisionSessionForTest(t, s, permit.Decision.SessionID)
	var workspace targetexec.Workspace
	if err := s.db.QueryRow(`select directory_identity,canonical_path from workspace_admissions where id=?`, ss.AdmissionID).Scan(&workspace.ID, &workspace.Path); err != nil {
		t.Fatal(err)
	}
	store, err := targetexec.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.RegisterWorkspace(workspace); err != nil {
		t.Fatal(err)
	}
	targetID, err := store.Identity()
	if err != nil {
		t.Fatal(err)
	}
	var runnerID string
	if err := s.db.QueryRow(`select agent_runtime_id from runs where id=?`, permit.RunID).Scan(&runnerID); err != nil {
		t.Fatal(err)
	}
	if err := s.recordSupervisionTargetBinding(context.Background(), runnerID, &supervisionTargetLink{TargetID: targetID, Artifact: targetexec.HelperArtifact{OS: runtime.GOOS, Arch: runtime.GOARCH, SHA256: strings.Repeat("a", 64)}}); err != nil {
		t.Fatal(err)
	}
	if err := s.stageSupervisionTargetControl(context.Background(), ss.ID, runnerID, targetID, workspace.ID); err != nil {
		t.Fatal(err)
	}
	return s, permit, &supervisionTargetRuntime{store: store, storage: t.TempDir(), config: Config{WorkspaceGuardDir: t.TempDir()}}
}

func TestSupervisionTargetControlCommitsRevocationWithDomainStateAndRollback(t *testing.T) {
	s, permit, _ := supervisionTargetControlFixture(t)
	ctx, id := context.Background(), permit.Decision.SessionID
	initial, err := loadSupervisionTargetControl(ctx, s.db, id)
	if err != nil || initial.Control.State != "running" || initial.Control.Version != 1 || !initial.Control.AuthorizedUntil.IsZero() {
		t.Fatal(initial, err)
	}
	// Transcript/clock updates cannot mint new execution authority.
	if err := s.withSupervisionTx(ctx, id, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		return appendSupervisionEventTx(ctx, tx, ss, "test.history", map[string]string{"text": "observed"}, time.Now().UTC())
	}); err != nil {
		t.Fatal(err)
	}
	unchanged, _ := loadSupervisionTargetControl(ctx, s.db, id)
	if unchanged.Control != initial.Control {
		t.Fatal("history extended authority")
	}
	cause := errors.New("rollback control")
	err = s.withSupervisionTx(ctx, id, time.Now().UTC(), func(_ *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		ss.Control = "stop"
		ss.StateVersion++
		return cause
	})
	if !errors.Is(err, cause) {
		t.Fatal(err)
	}
	unchanged, _ = loadSupervisionTargetControl(ctx, s.db, id)
	if unchanged.Control != initial.Control {
		t.Fatal("rolled back control escaped transaction")
	}
	for _, step := range []struct{ control, reason, state, expected string }{
		{"pause", "user_pause", "pausing", "paused"},
		{"", "budget_exhausted", "paused", "paused"},
		{"", "", "running", "running"},
		{"stop", "", "stopping", "stopped"},
	} {
		previous, _ := loadSupervisionTargetControl(ctx, s.db, id)
		err := s.withSupervisionTx(ctx, id, time.Now().UTC(), func(_ *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
			ss.Control, ss.Reason, ss.State = step.control, step.reason, step.state
			ss.StateVersion++
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		next, err := loadSupervisionTargetControl(ctx, s.db, id)
		if err != nil || next.Control.State != step.expected || next.Control.Version != previous.Control.Version+1 || next.Control.DrainExecutionID != "" || next.Acknowledged != 0 || !next.Control.AuthorizedUntil.IsZero() {
			t.Fatal(step, next, err)
		}
	}
	if err := s.withSupervisionTx(ctx, id, time.Now().UTC(), func(_ *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		ss.Control, ss.State = "", "running"
		ss.StateVersion++
		return nil
	}); !errors.Is(err, targetexec.ErrAuthority) {
		t.Fatal("stopped target identity was reopened", err)
	}
}

func TestSupervisionTargetControlDeliveryDoesNotHoldDomainTransactionOrAcknowledgeSupersededVersion(t *testing.T) {
	s, permit, target := supervisionTargetControlFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	left, right := net.Pipe()
	supervisor, err := targetexec.NewSupervisor(ctx, target.store, target)
	if err != nil {
		t.Fatal(err)
	}
	var paused atomic.Bool
	transport := &targetexec.Transport{Store: target.store, Supervisor: supervisor, Extension: target.handle, ValidateControl: func(control targetexec.Control) error {
		if err := target.validateControl(control); err != nil {
			return err
		}
		if control.State == "running" && paused.CompareAndSwap(false, true) {
			// Would deadlock if network delivery held SQLite or the admission gate.
			return s.withSupervisionTx(ctx, permit.Decision.SessionID, time.Now().UTC(), func(_ *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
				ss.Control, ss.State, ss.Reason = "pause", "pausing", "user_pause"
				ss.StateVersion++
				return nil
			})
		}
		return nil
	}}
	done := make(chan error, 1)
	go func() { done <- transport.Serve(ctx, right) }()
	client := targetexec.NewClient(left)
	t.Cleanup(func() { client.Close(); cancel(); <-done })
	hello, err := client.Call(ctx, targetexec.Request{ID: "hello", Method: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	link := &supervisionTargetLink{Client: client, TargetID: hello.TargetID}
	if err := s.deliverSupervisionTargetControl(ctx, link, permit.Decision.SessionID); !errors.Is(err, errSupervisionState) {
		t.Fatal("superseded grant was acknowledged", err)
	}
	pending, err := loadSupervisionTargetControl(ctx, s.db, permit.Decision.SessionID)
	if err != nil || pending.Control.State != "paused" || pending.Control.Version != 2 || pending.Acknowledged != 0 || pending.Wire != nil {
		t.Fatal(pending, err)
	}
	if err := s.deliverSupervisionTargetControl(ctx, link, permit.Decision.SessionID); err != nil {
		t.Fatal(err)
	}
	ack, err := loadSupervisionTargetControl(ctx, s.db, permit.Decision.SessionID)
	if err != nil || ack.Acknowledged != 2 || ack.Wire == nil {
		t.Fatal(ack, err)
	}
	actual, err := target.store.Session(permit.Decision.SessionID)
	if err != nil || actual.Control.State != "paused" || actual.Control.Version != 2 {
		t.Fatal(actual, err)
	}
	if err := s.deliverSupervisionTargetControl(ctx, link, permit.Decision.SessionID); err != nil {
		t.Fatal("idempotent delivery", err)
	}
}

func TestSupervisionTargetControlRetriesKeepExactTargetDeadline(t *testing.T) {
	s, permit, target := supervisionTargetControlFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	left, right := net.Pipe()
	supervisor, err := targetexec.NewSupervisor(ctx, target.store, target)
	if err != nil {
		t.Fatal(err)
	}
	transport := &targetexec.Transport{Store: target.store, Supervisor: supervisor, Extension: target.handle, ValidateControl: target.validateControl}
	done := make(chan error, 1)
	go func() { done <- transport.Serve(ctx, right) }()
	client := targetexec.NewClient(left)
	t.Cleanup(func() { client.Close(); cancel(); <-done })
	hello, err := client.Call(ctx, targetexec.Request{ID: "hello", Method: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	link := &supervisionTargetLink{Client: client, TargetID: hello.TargetID}
	if err := s.deliverSupervisionTargetControl(ctx, link, permit.Decision.SessionID); err != nil {
		t.Fatal(err)
	}
	first, err := target.store.Session(permit.Decision.SessionID)
	if err != nil || first.Control.AuthorizedUntil.IsZero() {
		t.Fatal(first, err)
	}
	for range 3 {
		// The target committed, but its acknowledgement did not commit locally.
		if _, err := s.db.Exec(`update supervision_target_controls set acknowledged_version=0 where session_id=?`, permit.Decision.SessionID); err != nil {
			t.Fatal(err)
		}
		if err := s.deliverSupervisionTargetControl(ctx, link, permit.Decision.SessionID); err != nil {
			t.Fatal("lost acknowledgement could not be retried", err)
		}
		after, err := target.store.Session(permit.Decision.SessionID)
		if err != nil || after.Control != first.Control {
			t.Fatal("retry extended authorization or changed identity", after.Control, err)
		}
	}
}

func TestSupervisionTargetControlPauseDrainsOnlyObservedWorkUntilNextDomainChange(t *testing.T) {
	for _, phase := range []string{"prepared", "starting", "created", "running"} {
		t.Run(phase, func(t *testing.T) {
			s, permit, _, _ := supervisionTargetInboxFixture(t)
			ctx, id := context.Background(), permit.Decision.SessionID
			if _, err := s.db.Exec(`update supervision_executions set state=? where execution_id=?`, phase, permit.Decision.ExecutionID); err != nil {
				t.Fatal(err)
			}
			ss := supervisionSessionForTest(t, s, id)
			if err := s.requestSupervisionControl(ctx, id, ss.StateVersion, "pause", time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			intent, err := loadSupervisionTargetControl(ctx, s.db, id)
			want := ""
			if phase == "created" || phase == "running" {
				want = permit.Decision.ExecutionID
			}
			if err != nil || intent.Control.State != "paused" || intent.Control.DrainExecutionID != want {
				t.Fatal("incorrect drain exception", phase, intent.Control, err)
			}
			if err := s.withSupervisionTx(ctx, id, time.Now().UTC(), func(_ *sql.Tx, _ map[string]WorkspaceAdmission, _ *supervisionSession) error { return nil }); err != nil {
				t.Fatal(err)
			}
			stable, _ := loadSupervisionTargetControl(ctx, s.db, id)
			if stable.Control != intent.Control {
				t.Fatal("monitoring removed or extended drain")
			}
			if err := s.withSupervisionTx(ctx, id, time.Now().UTC(), func(_ *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
				ss.Budget.MaxTurnMillis--
				ss.StateVersion++
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			changed, _ := loadSupervisionTargetControl(ctx, s.db, id)
			if changed.Control.State != "paused" || changed.Control.DrainExecutionID != "" || changed.Control.Version != intent.Control.Version+1 {
				t.Fatal("budget/domain change retained stale drain", changed.Control)
			}
		})
	}
}
