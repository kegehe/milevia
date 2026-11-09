package targetexec

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTargetStorageCapacityPreservesStopAndDoesNotInventDurableFacts(t *testing.T) {
	for _, event := range []bool{false, true} {
		t.Run(map[bool]string{false: "prepare", true: "event"}[event], func(t *testing.T) {
			s, control, spec, now := fixture(t)
			var execution Execution
			if event {
				var err error
				execution, err = s.Prepare(spec, now)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.ClaimStart(control.SessionID, spec.ExecutionID, execution.Digest, now); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(filepath.Join(s.dir, "facts.json"))
			if err != nil {
				t.Fatal(err)
			}
			s.admissionLimit = len(before) + 1
			if event {
				_, err = s.AppendEvent(control.SessionID, spec.ExecutionID, "large-frame", "assistant", json.RawMessage(`{"text":"payload beyond admitted capacity"}`))
			} else {
				_, err = s.Prepare(spec, now)
			}
			if !errors.Is(err, ErrLimit) {
				t.Fatal("payload capacity was not enforced", err)
			}
			after, err := os.ReadFile(filepath.Join(s.dir, "facts.json"))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("refused admission changed durable state", err)
			}
			session, err := s.Session(control.SessionID)
			if err != nil || (!event && len(session.Executions) != 0) {
				t.Fatal("refused admission closed control or invented an execution", session, err)
			}
			page, err := s.EventsAfter(control.SessionID, 0, 100)
			if err != nil || page.NextSequence != 1 || len(page.Events) != 0 {
				t.Fatal("refused frame invented a sequence or event", page, err)
			}
			control.State, control.Version = "stopped", control.Version+1
			if err := s.ApplyControl(control, now); err != nil {
				t.Fatal("storage capacity disabled revocation", err)
			}
			if event {
				if err := s.RecordQuiet(control.SessionID, spec.ExecutionID, execution.Digest, "", true, Usage{}); err != nil {
					t.Fatal("storage capacity disabled process settlement", err)
				}
			}
			if err := s.Release(control.SessionID, control.OwnerEpoch, control.Version); err != nil {
				t.Fatal("storage capacity disabled resource release", err)
			}
			dir := s.dir
			s.Close()
			reopened, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			session, err = reopened.Session(control.SessionID)
			if err != nil || !session.Released || session.Control.State != "stopped" {
				t.Fatal("restart forgot storage-limit stop", session, err)
			}
		})
	}
}

func fixture(t *testing.T) (*Store, Control, ExecutionSpec, time.Time) {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	facts, err := s.Facts()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	c := Control{Protocol: ProtocolVersion, TargetID: facts.TargetID, SessionID: "session-1", OwnerEpoch: 1, Version: 1, State: "running", WorkspaceID: "physical-directory-1", AuthorizedUntil: now.Add(30 * time.Second)}
	if err := s.ApplyControl(c, now); err != nil {
		t.Fatal(err)
	}
	spec := ExecutionSpec{Protocol: ProtocolVersion, TargetID: facts.TargetID, SessionID: c.SessionID, ExecutionID: "execution-1", WorkItemID: "work-1", Attempt: 1, OwnerEpoch: 1, AuthorizationVersion: 1, WorkspaceID: c.WorkspaceID, Role: "scheduler", AgentID: "codex", Contract: "fixture-v1", InputSHA256: strings.Repeat("a", 64), InstructionsSHA256: strings.Repeat("b", 64), PreparedAt: now, TurnDeadline: now.Add(time.Minute)}
	return s, c, spec, now
}

func TestRevocationBeforePreparationSurvivesRestart(t *testing.T) {
	s, c, spec, now := fixture(t)
	c.Version = 3
	c.State = "stopped"
	if err := s.ApplyControl(c, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prepare(spec, now); !errors.Is(err, ErrAuthority) {
		t.Fatal("delayed prepare passed", err)
	}
	dir := s.dir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.Prepare(spec, now); !errors.Is(err, ErrAuthority) {
		t.Fatal("restart forgot revoke", err)
	}
	c.Version = 2
	c.State = "running"
	if err := reopened.ApplyControl(c, now); !errors.Is(err, ErrAuthority) {
		t.Fatal("old control revived session", err)
	}
	c.Version = 4
	if err := reopened.ApplyControl(c, now); !errors.Is(err, ErrAuthority) {
		t.Fatal("stopped session resumed", err)
	}
}

func TestStartIsDurableAndCannotBeRepeatedOrReplayed(t *testing.T) {
	s, c, spec, now := fixture(t)
	prepared, err := s.Prepare(spec, now)
	if err != nil {
		t.Fatal(err)
	}
	if repeated, err := s.Prepare(spec, now); err != nil || repeated.Digest != prepared.Digest {
		t.Fatal("prepare not idempotent", err)
	}
	changed := spec
	changed.InputSHA256 = strings.Repeat("c", 64)
	if _, err := s.Prepare(changed, now); !errors.Is(err, ErrConflict) {
		t.Fatal("same execution changed input", err)
	}
	if _, err := s.ClaimStart(c.SessionID, spec.ExecutionID, prepared.Digest, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimStart(c.SessionID, spec.ExecutionID, prepared.Digest, now); !errors.Is(err, ErrConflict) {
		t.Fatal("start replayed", err)
	}
	dir := s.dir
	s.Close()
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if repeated, err := reopened.Prepare(spec, now); err != nil || repeated.State != "unknown" {
		t.Fatal("lost start replayed", repeated, err)
	}
	if _, err := reopened.ClaimStart(c.SessionID, spec.ExecutionID, prepared.Digest, now); !errors.Is(err, ErrAuthority) {
		t.Fatal("restart replayed uncertain start", err)
	}
	stops, err := reopened.DueStops(now)
	if err != nil || len(stops) != 1 {
		t.Fatal("uncertain execution not reconciled", err)
	}
}

func TestExplicitDrainAllowsOnlyTheAlreadyClaimedTurn(t *testing.T) {
	s, c, spec, now := fixture(t)
	execution, err := s.Prepare(spec, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimStart(c.SessionID, spec.ExecutionID, execution.Digest, now); err != nil {
		t.Fatal(err)
	}
	c.State = "paused"
	c.Version++
	c.DrainExecutionID = spec.ExecutionID
	if err = s.ApplyControl(c, now); err != nil {
		t.Fatal(err)
	}
	if err = s.RegisterHeld(c.SessionID, spec.ExecutionID, ProcessIdentity{Container: execution.Container, PID: 42, Birth: "birth", Executable: "/fixture"}, now); err != nil {
		t.Fatal("authorized in-flight turn could not finish", err)
	}
	if err = s.Resume(c.SessionID, spec.ExecutionID, now, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	stops, err := s.DueStops(now)
	if err != nil || len(stops) != 0 {
		t.Fatal("draining turn stopped", err)
	}
	next := spec
	next.ExecutionID = "new-attempt"
	next.AuthorizationVersion = c.Version
	if _, err = s.Prepare(next, now); !errors.Is(err, ErrAuthority) {
		t.Fatal("pause allowed another turn", err)
	}
	c.Version++
	c.DrainExecutionID = ""
	if err = s.ApplyControl(c, now); err != nil {
		t.Fatal(err)
	}
	stops, err = s.DueStops(now)
	if err != nil || len(stops) != 1 {
		t.Fatal("immediate pause failed to revoke", err)
	}
}

func TestPauseFencesPreparedStartAndDelayedRenewal(t *testing.T) {
	s, c, spec, now := fixture(t)
	execution, err := s.Prepare(spec, now)
	if err != nil {
		t.Fatal(err)
	}
	c.Version = 2
	c.State = "paused"
	if err := s.ApplyControl(c, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimStart(c.SessionID, spec.ExecutionID, execution.Digest, now); !errors.Is(err, ErrAuthority) {
		t.Fatal("late start bypassed pause", err)
	}
	if err := s.RenewAuthorization(c.SessionID, 1, 1, now.Add(30*time.Second), now); !errors.Is(err, ErrAuthority) {
		t.Fatal("old lease cleared pause", err)
	}
	c.Version = 3
	c.State = "running"
	if err := s.ApplyControl(c, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimStart(c.SessionID, spec.ExecutionID, execution.Digest, now); !errors.Is(err, ErrAuthority) {
		t.Fatal("old attempt revived", err)
	}
	newSpec := spec
	newSpec.ExecutionID = "execution-2"
	newSpec.Attempt = 2
	newSpec.AuthorizationVersion = 3
	if _, err := s.Prepare(newSpec, now); err != nil {
		t.Fatal(err)
	}
}

func TestLeaseExpirationAndTurnDeadlineAreNotReset(t *testing.T) {
	s, c, spec, now := fixture(t)
	spec.TurnDeadline = now.Add(40 * time.Second)
	execution, err := s.Prepare(spec, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimStart(c.SessionID, spec.ExecutionID, execution.Digest, now); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterHeld(c.SessionID, spec.ExecutionID, ProcessIdentity{Container: execution.Container, PID: 42, Birth: "birth-1", Executable: "/bin/fixture"}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Resume(c.SessionID, spec.ExecutionID, now, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	later := now.Add(20 * time.Second)
	if err := s.RenewAuthorization(c.SessionID, 1, 1, later.Add(30*time.Second), later); err != nil {
		t.Fatal(err)
	}
	if err := s.Renew(c.SessionID, spec.ExecutionID, 1, 1, later.Add(30*time.Second), later); err != nil {
		t.Fatal(err)
	}
	facts, _ := s.Facts()
	if !facts.Sessions[c.SessionID].Executions[spec.ExecutionID].LeaseUntil.Equal(spec.TurnDeadline) {
		t.Fatal("turn deadline was reset")
	}
	stops, err := s.DueStops(spec.TurnDeadline)
	if err != nil || len(stops) != 1 {
		t.Fatal("expired execution not stopped", err)
	}
	if err := s.Renew(c.SessionID, spec.ExecutionID, 1, 1, now.Add(time.Minute), now.Add(41*time.Second)); !errors.Is(err, ErrAuthority) {
		t.Fatal("expired turn renewed", err)
	}
}

func TestWorkspaceRemainsClaimedBetweenTurnsAndUntilQuietRelease(t *testing.T) {
	s, c, spec, now := fixture(t)
	other := c
	other.SessionID = "other-session"
	if err := s.ApplyControl(other, now); !errors.Is(err, ErrNotQuiet) {
		t.Fatal("idle protected workspace was stolen", err)
	}
	execution, err := s.Prepare(spec, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimStart(c.SessionID, spec.ExecutionID, execution.Digest, now); err != nil {
		t.Fatal(err)
	}
	c.State = "stopped"
	c.Version++
	if err := s.ApplyControl(c, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(c.SessionID, c.OwnerEpoch, c.Version); !errors.Is(err, ErrNotQuiet) {
		t.Fatal("unconfirmed resources released", err)
	}
	usage := Usage{Sequence: 1, InputTokens: 10, OutputTokens: 3, ActiveMillis: 500}
	if err := s.RecordQuiet(c.SessionID, spec.ExecutionID, execution.Digest, "", true, usage); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordQuiet(c.SessionID, spec.ExecutionID, execution.Digest, "", true, usage); err != nil {
		t.Fatal("duplicate receipt not idempotent", err)
	}
	usage.InputTokens++
	if err := s.RecordQuiet(c.SessionID, spec.ExecutionID, execution.Digest, "", true, usage); !errors.Is(err, ErrConflict) {
		t.Fatal("duplicate receipt changed usage", err)
	}
	if err := s.Release(c.SessionID, c.OwnerEpoch, c.Version); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyControl(other, now); err != nil {
		t.Fatal("quiet released directory remained blocked", err)
	}
	if _, err := s.ClaimStart(c.SessionID, spec.ExecutionID, execution.Digest, now); !errors.Is(err, ErrAuthority) {
		t.Fatal("old startup after release accepted", err)
	}
}

func TestSingleOwnerUnknownProtocolAndTamperedFactsFailClosed(t *testing.T) {
	s, _, _, _ := fixture(t)
	if other, err := Open(s.dir); err == nil {
		other.Close()
		t.Fatal("two target authorities opened same state")
	}
	dir := s.dir
	s.Close()
	if err := os.WriteFile(filepath.Join(dir, "facts.json"), []byte(`{"protocol":999,"targetId":"old","sessions":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if other, err := Open(dir); !errors.Is(err, ErrProtocol) {
		if other != nil {
			other.Close()
		}
		t.Fatal("unknown protocol reset target identity", err)
	}
}
