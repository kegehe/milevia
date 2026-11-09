package targetexec

import (
	"errors"
	"testing"
)

func TestArtifactDiskAdmissionCountsUnfinishedReservations(t *testing.T) {
	s, control, spec, now := fixture(t)
	spec.Role = "test"
	first, err := s.Prepare(spec, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimStart(control.SessionID, spec.ExecutionID, first.Digest, now); err != nil {
		t.Fatal(err)
	}
	if err := s.ReserveArtifactsWithAvailable(control.SessionID, spec.ExecutionID, 32, 40, now); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordQuiet(control.SessionID, spec.ExecutionID, first.Digest, "", false, Usage{}); err != nil {
		t.Fatal(err)
	}
	second := spec
	second.ExecutionID, second.WorkItemID = "next", "next-work"
	execution, err := s.Prepare(second, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimStart(control.SessionID, second.ExecutionID, execution.Digest, now); err != nil {
		t.Fatal(err)
	}
	if err := s.ReserveArtifactsWithAvailable(control.SessionID, second.ExecutionID, 16, 40, now); !errors.Is(err, ErrLimit) {
		t.Fatal("unfinished output double-spent filesystem capacity", err)
	}
	if err := s.SettleArtifacts(control.SessionID, spec.ExecutionID, 8); err != nil {
		t.Fatal(err)
	}
	if err := s.ReserveArtifactsWithAvailable(control.SessionID, second.ExecutionID, 16, 40, now); err != nil {
		t.Fatal("settled size did not free unused reservation", err)
	}
}

func TestArtifactCapacityAndCleanupSurviveRestart(t *testing.T) {
	s, control, spec, now := fixture(t)
	spec.Role = "test"
	execution, err := s.Prepare(spec, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimStart(control.SessionID, spec.ExecutionID, execution.Digest, now); err != nil {
		t.Fatal(err)
	}
	if err := s.ReserveArtifacts(control.SessionID, spec.ExecutionID, MaxArtifactStorageBytes, now); err != nil {
		t.Fatal(err)
	}
	if err := s.ReserveArtifacts(control.SessionID, spec.ExecutionID, MaxArtifactStorageBytes-1, now); !errors.Is(err, ErrConflict) {
		t.Fatal("changed reservation accepted", err)
	}
	if err := s.BeginArtifactCleanup(control.SessionID, spec.ExecutionID); !errors.Is(err, ErrNotQuiet) {
		t.Fatal("active evidence could be cleaned", err)
	}
	if err := s.RecordQuiet(control.SessionID, spec.ExecutionID, execution.Digest, "", false, Usage{}); err != nil {
		t.Fatal(err)
	}
	second := spec
	second.ExecutionID, second.WorkItemID = "second", "second-work"
	prepared, err := s.Prepare(second, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimStart(control.SessionID, second.ExecutionID, prepared.Digest, now); err != nil {
		t.Fatal(err)
	}
	if err := s.ReserveArtifacts(control.SessionID, second.ExecutionID, 1, now); !errors.Is(err, ErrLimit) {
		t.Fatal("aggregate artifact capacity not enforced", err)
	}
	if err := s.RecordQuiet(control.SessionID, second.ExecutionID, prepared.Digest, "", false, Usage{}); err != nil {
		t.Fatal(err)
	}
	control.State, control.Version = "stopped", 2
	if err := s.ApplyControl(control, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(control.SessionID, 1, 2); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginArtifactCleanup(control.SessionID, spec.ExecutionID); err != nil {
		t.Fatal(err)
	}
	dir := s.dir
	s.Close()
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	facts, _ := reopened.Facts()
	if facts.Artifacts[spec.ExecutionID].State != "cleaning" || facts.Artifacts[spec.ExecutionID].Bytes != MaxArtifactStorageBytes {
		t.Fatal("restart freed unfinished cleanup reservation")
	}
	for range 2 {
		if err := reopened.BeginArtifactCleanup(control.SessionID, spec.ExecutionID); err != nil {
			t.Fatal(err)
		}
		if err := reopened.FinishArtifactCleanup(control.SessionID, spec.ExecutionID); err != nil {
			t.Fatal(err)
		}
	}
	facts, _ = reopened.Facts()
	if allocation := facts.Artifacts[spec.ExecutionID]; allocation.Bytes != 0 || allocation.State != "cleared" {
		t.Fatal(allocation)
	}
}

func TestReleasedEventCleanupKeepsCursorGapAndRejectsLateStart(t *testing.T) {
	s, control, spec, now := fixture(t)
	execution, err := s.Prepare(spec, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(control.SessionID, spec.ExecutionID, "one", "output", []byte(`{"text":"large output"}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearReleasedEvents(control.SessionID, 1, 1); !errors.Is(err, ErrAuthority) {
		t.Fatal("live journal could be cleaned", err)
	}
	control.State, control.Version = "stopped", 2
	if err := s.ApplyControl(control, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(control.SessionID, 1, 2); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.ClearReleasedEvents(control.SessionID, 1, 2); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.EventsAfter(control.SessionID, 0, 100)
	if !errors.Is(err, ErrCursor) || page.FirstSequence != 2 || page.NextSequence != 2 {
		t.Fatal("cleared prefix looked complete", page, err)
	}
	if _, err := s.ClaimStart(control.SessionID, spec.ExecutionID, execution.Digest, now); !errors.Is(err, ErrAuthority) {
		t.Fatal("journal cleanup allowed old execution", err)
	}
	dir := s.dir
	s.Close()
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.EventsAfter(control.SessionID, 0, 100); !errors.Is(err, ErrCursor) {
		t.Fatal("restart forgot cleared cursor", err)
	}
}
