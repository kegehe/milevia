package targetexec

import (
	"errors"
	"testing"
)

func TestReconciliationReadsRequirePausedQuietProtectedWorkspace(t *testing.T) {
	s, c, spec, now := fixture(t)
	read := func() error { return s.CheckReconciliationRead(c.SessionID, c.WorkspaceID, c.OwnerEpoch, c.Version) }
	if err := read(); !errors.Is(err, ErrAuthority) {
		t.Fatal("running grant allowed recovery read", err)
	}
	e, err := s.Prepare(spec, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimStart(c.SessionID, spec.ExecutionID, e.Digest, now); err != nil {
		t.Fatal(err)
	}
	c.State, c.Version = "paused", c.Version+1
	if err := s.ApplyControl(c, now); err != nil {
		t.Fatal(err)
	}
	if err := read(); !errors.Is(err, ErrNotQuiet) {
		t.Fatal("unconfirmed execution allowed recovery read", err)
	}
	if err := s.RecordQuiet(c.SessionID, spec.ExecutionID, e.Digest, "", true, Usage{}); err != nil {
		t.Fatal(err)
	}
	if err := read(); err != nil {
		t.Fatal("quiet paused workspace cannot be read", err)
	}
	if err := s.CheckGrant(c.SessionID, c.WorkspaceID, c.OwnerEpoch, c.Version, now); !errors.Is(err, ErrAuthority) {
		t.Fatal("reconciliation granted writes", err)
	}
	if err := s.CheckReconciliationRead(c.SessionID, c.WorkspaceID, c.OwnerEpoch, c.Version-1); !errors.Is(err, ErrAuthority) {
		t.Fatal("old control allowed read", err)
	}
	c.State, c.Version = "stopped", c.Version+1
	if err := s.ApplyControl(c, now); err != nil {
		t.Fatal(err)
	}
	if err := read(); !errors.Is(err, ErrAuthority) {
		t.Fatal("stopped session allowed recovery read", err)
	}
}
