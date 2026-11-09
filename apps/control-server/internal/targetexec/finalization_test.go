package targetexec

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func finalSpec(control Control) FinalizationSpec {
	return FinalizationSpec{ID: "final-1", SessionID: control.SessionID, WorkspaceID: control.WorkspaceID, OwnerEpoch: control.OwnerEpoch, AuthorizationVersion: control.Version, BindingSHA256: strings.Repeat("a", 64), RevisionID: strings.Repeat("b", 64), RulesHash: strings.Repeat("c", 64), EnvironmentHash: strings.Repeat("d", 64)}
}

func TestFinalizationFreezesExecutionAndReceiptSurvivesLostResponse(t *testing.T) {
	s, control, execution, now := fixture(t)
	spec := finalSpec(control)
	final, fresh, err := s.BeginFinalization(spec, now)
	if err != nil || !fresh || final.State != "collecting" {
		t.Fatal(final, fresh, err)
	}
	if _, err := s.Prepare(execution, now); !errors.Is(err, ErrAuthority) {
		t.Fatal("execution admitted during final handoff", err)
	}
	if err := s.CheckGrant(control.SessionID, control.WorkspaceID, 1, 1, now); !errors.Is(err, ErrAuthority) {
		t.Fatal("writes admitted during final handoff", err)
	}
	if err := s.SealFinalization(spec, now); err != nil {
		t.Fatal(err)
	}
	dir := s.dir
	s.Close()
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	final, fresh, err = reopened.BeginFinalization(spec, now)
	if err != nil || fresh || final.State != "sealed" || !final.ObservedAt.Equal(now) {
		t.Fatal("lost receipt triggered recollection", final, fresh, err)
	}
	control.State = "stopped"
	control.Version++
	if err := reopened.ApplyControl(control, now); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := reopened.Release(control.SessionID, control.OwnerEpoch, control.Version); err != nil {
			t.Fatal(err)
		}
	}
	if err := reopened.ClearFinalization(control.SessionID, spec.ID, control.OwnerEpoch, control.Version); !errors.Is(err, ErrAuthority) {
		t.Fatal("released terminal session reopened", err)
	}
}

func TestFinalizationRejectsPauseAndRequiresNewAuthorizationToReopen(t *testing.T) {
	s, control, _, now := fixture(t)
	spec := finalSpec(control)
	if _, _, err := s.BeginFinalization(spec, now); err != nil {
		t.Fatal(err)
	}
	control.State = "paused"
	control.Version++
	if err := s.ApplyControl(control, now); err != nil {
		t.Fatal(err)
	}
	if err := s.SealFinalization(spec, now); !errors.Is(err, ErrAuthority) {
		t.Fatal("pause consumed old final receipt", err)
	}
	if err := s.ClearFinalization(control.SessionID, spec.ID, control.OwnerEpoch, control.Version); !errors.Is(err, ErrAuthority) {
		t.Fatal("pause reopened writes", err)
	}
	control.State = "running"
	control.Version++
	control.AuthorizedUntil = now.Add(29 * time.Second)
	if err := s.ApplyControl(control, now); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearFinalization(control.SessionID, spec.ID, control.OwnerEpoch, control.Version); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckGrant(control.SessionID, control.WorkspaceID, control.OwnerEpoch, control.Version, now); err != nil {
		t.Fatal(err)
	}
	rebound := spec
	rebound.AuthorizationVersion = control.Version
	if _, _, err := s.BeginFinalization(rebound, now); !errors.Is(err, ErrConflict) {
		t.Fatal("cleared intent ID rebound to new authorization", err)
	}
	prior, fresh, err := s.BeginFinalization(spec, now)
	if err != nil || fresh || prior.State != "unknown" {
		t.Fatal("clearing deleted durable intent history", prior, fresh, err)
	}
}

func TestFinalizationInterruptedCaptureRemainsUnknownAndCannotSealAfterRestart(t *testing.T) {
	s, control, _, now := fixture(t)
	spec := finalSpec(control)
	if _, _, err := s.BeginFinalization(spec, now); err != nil {
		t.Fatal(err)
	}
	dir := s.dir
	s.Close()
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	prior, fresh, err := reopened.BeginFinalization(spec, now)
	if err != nil || fresh || prior.State != "unknown" {
		t.Fatal(prior, fresh, err)
	}
	if err := reopened.SealFinalization(spec, now); !errors.Is(err, ErrAuthority) {
		t.Fatal("interrupted capture fabricated complete receipt", err)
	}
}
