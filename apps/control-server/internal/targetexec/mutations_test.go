package targetexec

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func nowUTC() time.Time { return time.Now().UTC() }

func mutationFixture(t *testing.T) (*Store, Control, MutationSpec) {
	t.Helper()
	s, control, execution, now := fixture(t)
	execution.Role = "implementation"
	prepared, err := s.Prepare(execution, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimStart(control.SessionID, execution.ExecutionID, prepared.Digest, now); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordQuiet(control.SessionID, execution.ExecutionID, prepared.Digest, "", false, Usage{}); err != nil {
		t.Fatal(err)
	}
	spec := MutationSpec{ID: "mutation-1", SessionID: control.SessionID, ExecutionID: execution.ExecutionID, OwnerEpoch: 1, AuthorizationVersion: 1, WorkspaceID: control.WorkspaceID, InputRevision: strings.Repeat("a", 64), RulesHash: strings.Repeat("b", 64), Files: []FileChange{{Path: "a.txt", BeforeSHA256: hashBytes([]byte("before")), AfterSHA256: hashBytes([]byte("after"))}, {Path: "b.txt", BeforeSHA256: hashBytes([]byte("before")), AfterSHA256: hashBytes([]byte("after"))}}}
	return s, control, spec
}

func TestMutationRevocationAfterFirstRealWritePreservesPartialFacts(t *testing.T) {
	s, control, spec := mutationFixture(t)
	root := t.TempDir()
	for _, file := range spec.Files {
		if err := os.WriteFile(filepath.Join(root, file.Path), []byte("before"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	_, fresh, err := s.BeginMutation(spec, nowUTC())
	if err != nil || !fresh {
		t.Fatal(fresh, err)
	}
	write := func(file FileChange) error {
		return os.WriteFile(filepath.Join(root, file.Path), []byte("after"), 0600)
	}
	if err := s.ApplyMutationFile(spec.ID, 0, nowUTC(), write); err != nil {
		t.Fatal(err)
	}
	control.Version++
	control.State = "stopped"
	if err := s.ApplyControl(control, nowUTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyMutationFile(spec.ID, 1, nowUTC(), write); !errors.Is(err, ErrAuthority) {
		t.Fatal("second file written after revoke", err)
	}
	if err := s.Release(control.SessionID, control.OwnerEpoch, control.Version); !errors.Is(err, ErrNotQuiet) {
		t.Fatal("writer not settled", err)
	}
	if err := s.AbandonMutation(spec.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(control.SessionID, control.OwnerEpoch, control.Version); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(filepath.Join(root, "a.txt"))
	second, _ := os.ReadFile(filepath.Join(root, "b.txt"))
	if string(first) != "after" || string(second) != "before" {
		t.Fatal("partial write replayed or rolled back", string(first), string(second))
	}
	dir := s.dir
	s.Close()
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	prior, fresh, err := reopened.BeginMutation(spec, nowUTC())
	if err != nil || fresh || prior.State != "unknown" || prior.Files[0] != "applied" || prior.Files[1] != "pending" {
		t.Fatal("restart lost partial facts", prior, fresh, err)
	}
	if err := reopened.ApplyMutationFile(spec.ID, 1, nowUTC(), write); !errors.Is(err, ErrConflict) {
		t.Fatal("unknown batch blindly replayed", err)
	}
}

func TestMutationLostFileReceiptDoesNotRepeatReplacement(t *testing.T) {
	s, _, spec := mutationFixture(t)
	_, _, err := s.BeginMutation(spec, nowUTC())
	if err != nil {
		t.Fatal(err)
	}
	// Persist the precise crash window: write intent reached disk, replacement
	// happened, receipt did not. The filesystem remains the user's live version.
	s.mu.Lock()
	mutation := s.facts.Mutations[spec.ID]
	mutation.State = "applying"
	mutation.Files[0] = "writing"
	s.facts.Mutations[spec.ID] = mutation
	err = s.save()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	dir := s.dir
	s.Close()
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	mutation, fresh, err := reopened.BeginMutation(spec, nowUTC())
	if err != nil || fresh || mutation.State != "unknown" || mutation.Files[0] != "unknown" {
		t.Fatal(mutation, fresh, err)
	}
	called := false
	if err := reopened.ApplyMutationFile(spec.ID, 0, nowUTC(), func(FileChange) error { called = true; return nil }); !errors.Is(err, ErrConflict) || called {
		t.Fatal("crash window replayed", err)
	}
}

func TestMutationCompletedReceiptAndIdentityAreIdempotent(t *testing.T) {
	s, _, spec := mutationFixture(t)
	_, _, err := s.BeginMutation(spec, nowUTC())
	if err != nil {
		t.Fatal(err)
	}
	for i := range spec.Files {
		if err := s.ApplyMutationFile(spec.ID, i, nowUTC(), func(FileChange) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	revision := strings.Repeat("c", 64)
	for range 2 {
		if err := s.FinishMutation(spec.ID, revision, nowUTC()); err != nil {
			t.Fatal(err)
		}
	}
	mutation, fresh, err := s.BeginMutation(spec, nowUTC())
	if err != nil || fresh || mutation.OutputRevision != revision {
		t.Fatal(mutation, fresh, err)
	}
	spec.Files[0].AfterSHA256 = strings.Repeat("d", 64)
	if _, _, err := s.BeginMutation(spec, nowUTC()); !errors.Is(err, ErrConflict) {
		t.Fatal("mutation ID rebound", err)
	}
}

func TestTargetWorkspaceProtectionCoversPhysicalParentAndChild(t *testing.T) {
	s, control, _, now := fixture(t)
	root := t.TempDir()
	if err := s.RegisterWorkspace(Workspace{ID: control.WorkspaceID, Path: root}); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterWorkspace(Workspace{ID: "child-id", Path: filepath.Join(root, "child")}); err != nil {
		t.Fatal(err)
	}
	other := control
	other.SessionID = "other-session"
	other.WorkspaceID = "child-id"
	if err := s.ApplyControl(other, now); !errors.Is(err, ErrNotQuiet) {
		t.Fatal("child directory bypassed target protection", err)
	}
}
