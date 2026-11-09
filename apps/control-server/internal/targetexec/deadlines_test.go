package targetexec

import (
	"errors"
	"testing"
	"time"
)

// Pass a still-valid historical wall timestamp after expiring the independent
// local deadline. This models a wall clock rollback without changing host time.
func TestMonotonicGrantExpiryRejectsAuthorityAndFinalization(t *testing.T) {
	for _, seal := range []bool{false, true} {
		t.Run(map[bool]string{false: "begin", true: "seal"}[seal], func(t *testing.T) {
			s, control, execution, now := fixture(t)
			final := finalSpec(control)
			if seal {
				if _, _, err := s.BeginFinalization(final, now); err != nil {
					t.Fatal(err)
				}
			}
			s.grantDeadlines[control.SessionID] = time.Now().Add(-time.Second)
			if err := s.CheckGrant(control.SessionID, control.WorkspaceID, control.OwnerEpoch, control.Version, now); !errors.Is(err, ErrAuthority) {
				t.Fatal("expired local grant accepted historical wall time", err)
			}
			if err := s.RenewAuthorization(control.SessionID, control.OwnerEpoch, control.Version, now.Add(30*time.Second), now); !errors.Is(err, ErrAuthority) {
				t.Fatal("renewal resurrected local expiry", err)
			}
			if _, err := s.Prepare(execution, now); !errors.Is(err, ErrAuthority) {
				t.Fatal("expired grant prepared work", err)
			}
			if seal {
				if err := s.SealFinalization(final, now); !errors.Is(err, ErrAuthority) {
					t.Fatal("expired grant sealed evidence", err)
				}
			} else if _, _, err := s.BeginFinalization(final, now); !errors.Is(err, ErrAuthority) {
				t.Fatal("expired grant began finalization", err)
			}
			// Replaying an identical control cannot create fresh local authority.
			if err := s.ApplyControl(control, now); err != nil {
				t.Fatal(err)
			}
			if s.grantLive(control.SessionID) {
				t.Fatal("control replay extended local grant")
			}
		})
	}
}

func TestMonotonicExecutionExpiryCannotResumeOrRenew(t *testing.T) {
	for _, stage := range []string{"prepared", "held", "running", "turn"} {
		t.Run(stage, func(t *testing.T) {
			s, control, spec, now := fixture(t)
			execution, err := s.Prepare(spec, now)
			if err != nil {
				t.Fatal(err)
			}
			if stage != "prepared" {
				if _, err := s.ClaimStart(control.SessionID, spec.ExecutionID, execution.Digest, now); err != nil {
					t.Fatal(err)
				}
				identity := ProcessIdentity{Container: execution.Container, PID: 123, Birth: "fixture", Executable: "fixture"}
				if err := s.RegisterHeld(control.SessionID, spec.ExecutionID, identity, now); err != nil {
					t.Fatal(err)
				}
				if stage != "held" {
					if err := s.Resume(control.SessionID, spec.ExecutionID, now, func() error { return nil }); err != nil {
						t.Fatal(err)
					}
				}
			}
			if stage == "turn" {
				s.turnDeadlines[spec.ExecutionID] = time.Now().Add(-time.Second)
			} else {
				s.leaseDeadlines[spec.ExecutionID] = time.Now().Add(-time.Second)
			}
			switch stage {
			case "prepared":
				if _, err := s.ClaimStart(control.SessionID, spec.ExecutionID, execution.Digest, now); !errors.Is(err, ErrAuthority) {
					t.Fatal("expired local lease started", err)
				}
			case "held":
				if err := s.Resume(control.SessionID, spec.ExecutionID, now, func() error { t.Fatal("expired process resumed"); return nil }); !errors.Is(err, ErrAuthority) {
					t.Fatal(err)
				}
			default:
				if err := s.Renew(control.SessionID, spec.ExecutionID, 1, 1, now.Add(30*time.Second), now); !errors.Is(err, ErrAuthority) {
					t.Fatal("local expiry renewed", err)
				}
			}
			if stage != "prepared" {
				stops, err := s.DueStops(now)
				if err != nil || len(stops) != 1 {
					t.Fatal("local expiry did not stop execution", stops, err)
				}
			}
		})
	}
}

func TestLeaseRenewalPreservesOriginalMonotonicTurnDeadline(t *testing.T) {
	s, control, spec, now := fixture(t)
	execution, err := s.Prepare(spec, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimStart(control.SessionID, spec.ExecutionID, execution.Digest, now); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterHeld(control.SessionID, spec.ExecutionID, ProcessIdentity{Container: execution.Container, PID: 123, Birth: "fixture", Executable: "fixture"}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Resume(control.SessionID, spec.ExecutionID, now, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	original := time.Now().Add(5 * time.Second)
	s.turnDeadlines[spec.ExecutionID] = original
	if err := s.RenewAuthorization(control.SessionID, 1, 1, now.Add(31*time.Second), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.Renew(control.SessionID, spec.ExecutionID, 1, 1, now.Add(31*time.Second), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if s.turnDeadlines[spec.ExecutionID] != original || s.leaseDeadlines[spec.ExecutionID].After(original) {
		t.Fatal("renewal extended original turn deadline")
	}
}
