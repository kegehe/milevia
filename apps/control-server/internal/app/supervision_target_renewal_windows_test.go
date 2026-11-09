//go:build windows

package app

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

func supervisionTargetControlTestLink(t *testing.T, target *supervisionTargetRuntime) *supervisionTargetLink {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
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
	return &supervisionTargetLink{Client: client, TargetID: hello.TargetID}
}

func TestSupervisionTargetControlRenewalReconcilesTargetAndReplaysExactDeadline(t *testing.T) {
	s, permit, target := supervisionTargetControlFixture(t)
	link := supervisionTargetControlTestLink(t, target)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id := permit.Decision.SessionID
	if err := s.deliverSupervisionTargetControl(ctx, link, id); err != nil {
		t.Fatal(err)
	}
	initial, _ := target.store.Session(id)
	// Exercise the real controller heartbeat, preserving domain/control version.
	if _, err := s.db.Exec(`update supervision_executions set state='starting' where execution_id=?`, permit.Decision.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if err := s.renewSupervisionExecutionLease(ctx, permit, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.renewSupervisionTargetControl(ctx, link, id, ""); err != nil {
		t.Fatal(err)
	}
	first, _ := target.store.Session(id)
	intent, err := loadSupervisionTargetControl(ctx, s.db, id)
	if err != nil || intent.Renewal == nil || !intent.Renewal.Acknowledged || !first.Control.AuthorizedUntil.After(initial.Control.AuthorizedUntil) {
		t.Fatal("live renewal not acknowledged", intent, err)
	}
	// The remote reply arrived but the local renewal ACK was lost.
	intent.Renewal.Acknowledged = false
	if _, err := s.db.Exec(`update supervision_target_controls set renewal_json=? where session_id=?`, string(mustJSON(intent.Renewal)), id); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := s.renewSupervisionTargetControl(ctx, link, id, ""); err != nil {
			t.Fatal("lost renewal reply could not replay", err)
		}
		after, _ := target.store.Session(id)
		if after.Control != first.Control {
			t.Fatal("retry extended original deadline", after.Control)
		}
		// Repeat the same simulated controller crash before ACK commit.
		if _, err := s.db.Exec(`update supervision_target_controls set renewal_json=? where session_id=?`, string(mustJSON(intent.Renewal)), id); err != nil {
			t.Fatal(err)
		}
	}
	ss := supervisionSessionForTest(t, s, id)
	if err := s.requestSupervisionControl(ctx, id, ss.StateVersion, "pause", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.renewSupervisionTargetControl(ctx, link, id, ""); !errors.Is(err, targetexec.ErrAuthority) {
		t.Fatal("pending pause allowed stale renewal", err)
	}
	if err := s.deliverSupervisionTargetControl(ctx, link, id); err != nil {
		t.Fatal(err)
	}
	paused, _ := target.store.Session(id)
	if paused.Control.State != "paused" || paused.Control.DrainExecutionID != "" {
		t.Fatal("pause was not delivered", paused.Control)
	}
}

func TestSupervisionTargetControlRenewalDoesNotTrustOldLocalAcknowledgement(t *testing.T) {
	s, permit, target := supervisionTargetControlFixture(t)
	link := supervisionTargetControlTestLink(t, target)
	ctx, id := context.Background(), permit.Decision.SessionID
	if err := s.deliverSupervisionTargetControl(ctx, link, id); err != nil {
		t.Fatal(err)
	}
	// A newer target revocation models a disconnect/recovery observation.
	actual, _ := target.store.Session(id)
	actual.Control.Version++
	actual.Control.State = "paused"
	if err := target.store.ApplyControl(actual.Control, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.renewSupervisionTargetControl(ctx, link, id, ""); !errors.Is(err, targetexec.ErrAuthority) {
		t.Fatal("stale ACK restored target authority", err)
	}
	after, _ := target.store.Session(id)
	if after.Control != actual.Control {
		t.Fatal("reconciliation revived revoked authority", after.Control)
	}
}

func TestSupervisionTargetControlGrantDeadlineRespectsRemainingBudget(t *testing.T) {
	s, permit, target := supervisionTargetControlFixture(t)
	ctx, id := context.Background(), permit.Decision.SessionID
	if err := s.withSupervisionTx(ctx, id, time.Now().UTC(), func(_ *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		// Keep only 1500ms of the existing work budget. The final share stays
		// reserved; a 30-second target grant would exceed current authority.
		ss.ActiveMillis += supervisionTimeAvailable(ss, "work") - 1500
		ss.StateVersion++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	link := supervisionTargetControlTestLink(t, target)
	if err := s.deliverSupervisionTargetControl(ctx, link, id); err != nil {
		t.Fatal(err)
	}
	actual, _ := target.store.Session(id)
	if remaining := time.Until(actual.Control.AuthorizedUntil); remaining <= 0 || remaining > 1500*time.Millisecond {
		t.Fatal("target grant exceeded remaining work budget", remaining)
	}
	intent, _ := loadSupervisionTargetControl(ctx, s.db, id)
	if intent.Wire == nil || intent.Wire.AuthorizedUntil != actual.Control.AuthorizedUntil {
		t.Fatal("bounded grant not frozen durably", intent)
	}
}
