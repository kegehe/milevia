package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func writeSupervisionSourceForTest(t *testing.T, root, name, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func snapshotAdmissionForTest(t *testing.T, root string) WorkspaceAdmission {
	t.Helper()
	canonical, err := canonicalWorkspacePath(root)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := workspaceDirectoryIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	return WorkspaceAdmission{OriginalPath: root, CanonicalPath: canonical, DirectoryIdentity: identity}
}

func TestSupervisionSnapshotCapturesUntrackedIgnoredSourceAndBehaviorMetadata(t *testing.T) {
	root, storage := t.TempDir(), t.TempDir()
	writeSupervisionSourceForTest(t, root, ".gitignore", "ignored-source/\n")
	writeSupervisionSourceForTest(t, root, "ignored-source/input.go", "package first\n")
	a := snapshotAdmissionForTest(t, root)
	first, err := collectSupervisionWorkspace(context.Background(), a, storage)
	if err != nil {
		t.Fatal(err)
	}
	var hash string
	for _, entry := range first.Manifest.Entries {
		if entry.Path == "ignored-source/input.go" {
			hash = entry.SHA256
		}
	}
	if string(first.Blobs[hash]) != "package first\n" {
		t.Fatal("untracked gitignored input missing from reconstructable baseline")
	}
	name := filepath.Join(root, "ignored-source", "input.go")
	stamp := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(name, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	same, err := collectSupervisionWorkspace(context.Background(), a, storage)
	if err != nil || same.RevisionID != first.RevisionID {
		t.Fatalf("mtime changed content identity: %v", err)
	}
	writeSupervisionSourceForTest(t, root, "ignored-source/input.go", "package other\n")
	if err := os.Chtimes(name, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	changed, err := collectSupervisionWorkspace(context.Background(), a, storage)
	if err != nil || changed.RevisionID == first.RevisionID {
		t.Fatalf("same-size content change with old mtime missed: %v", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(name, 0755); err != nil {
			t.Fatal(err)
		}
		executable, err := collectSupervisionWorkspace(context.Background(), a, storage)
		if err != nil || executable.RevisionID == changed.RevisionID {
			t.Fatalf("execution mode change missed: %v", err)
		}
	}
}

func TestSupervisionSnapshotRecordsExclusionsAndNeverFollowsExternalLinks(t *testing.T) {
	root, storage := t.TempDir(), t.TempDir()
	writeSupervisionSourceForTest(t, root, ".env", "PRIVATE_ENV=do-not-copy")
	writeSupervisionSourceForTest(t, root, "node_modules/dependency.js", "dependency")
	writeSupervisionSourceForTest(t, root, "fixture.bin", "a\x00b")
	writeSupervisionSourceForTest(t, root, "large.txt", strings.Repeat("x", supervisionSnapshotFileLimit+1))
	writeSupervisionSourceForTest(t, root, "embedded.go", `const credential = "sk-abcdefgh12345678"`)
	collected, err := collectSupervisionWorkspace(context.Background(), snapshotAdmissionForTest(t, root), storage)
	if err != nil {
		t.Fatal(err)
	}
	gaps := map[string]string{}
	for _, entry := range collected.Manifest.Entries {
		gaps[entry.Path] = entry.Gap
	}
	for name, reason := range map[string]string{".env": "sensitive_path", "node_modules": "excluded_directory", "fixture.bin": "binary_content_not_retained", "large.txt": "file_size_limit", "embedded.go": "sensitive_content_not_retained"} {
		if gaps[name] != reason {
			t.Fatalf("%s gap=%q", name, gaps[name])
		}
	}
	for _, data := range collected.Blobs {
		if strings.Contains(string(data), "do-not-copy") || strings.Contains(string(data), "sk-abcdefgh") {
			t.Fatal("secret content retained")
		}
	}
	t.Run("external_link", func(t *testing.T) {
		outside := filepath.Join(t.TempDir(), "external.txt")
		if err := os.WriteFile(outside, []byte("outside-value"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "link.txt")); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		linked, err := collectSupervisionWorkspace(context.Background(), snapshotAdmissionForTest(t, root), storage)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range linked.Manifest.Entries {
			if entry.Path == "link.txt" && (entry.Kind != "symlink" || entry.Target != outside || entry.Gap == "") {
				t.Fatalf("external link not represented: %+v", entry)
			}
		}
		if _, exists := linked.Blobs[supervisionSHA256([]byte("outside-value"))]; exists {
			t.Fatal("followed external link")
		}
	})
}

func TestSupervisionSnapshotKeepsAncestorAndNestedRuleProvenance(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	writeSupervisionSourceForTest(t, parent, "AGENTS.md", "Do not commit.\n")
	writeSupervisionSourceForTest(t, root, "CLAUDE.md", "Run relevant checks.\n")
	writeSupervisionSourceForTest(t, root, "src/AGENTS.md", "Keep compatibility.\n")
	out, err := collectSupervisionWorkspace(context.Background(), snapshotAdmissionForTest(t, root), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var inherited, project, nested bool
	canonicalParent, err := canonicalWorkspacePath(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range out.Rules {
		inherited = inherited || (rule.Source == filepath.Join(canonicalParent, "AGENTS.md") && rule.Scope == ".")
		project = project || (rule.Source == "CLAUDE.md" && rule.Scope == ".")
		nested = nested || (rule.Source == "src/AGENTS.md" && rule.Scope == "src")
	}
	if !inherited || !project || !nested {
		t.Fatalf("rule source/scope lost: %+v", out.Rules)
	}
}

func TestSupervisionSnapshotRejectsUnreadableProjectRules(t *testing.T) {
	for _, kind := range []string{"directory", "oversized", "binary", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			rule := filepath.Join(root, "AGENTS.md")
			switch kind {
			case "directory":
				if err := os.Mkdir(rule, 0755); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				writeSupervisionSourceForTest(t, root, "AGENTS.md", strings.Repeat("x", supervisionRulesLimit+1))
			case "binary":
				writeSupervisionSourceForTest(t, root, "AGENTS.md", "rule\x00content")
			case "symlink":
				target := filepath.Join(t.TempDir(), "rule.md")
				if err := os.WriteFile(target, []byte("Do not commit."), 0644); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, rule); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			}
			if _, err := collectSupervisionWorkspace(context.Background(), snapshotAdmissionForTest(t, root), t.TempDir()); err == nil {
				t.Fatal("unretained project rule silently ignored")
			}
		})
	}
}

func TestSupervisionSnapshotExcludesApplicationStorageAndRejectsUnsafeRoot(t *testing.T) {
	root := t.TempDir()
	storage := filepath.Join(root, "application-data")
	writeSupervisionSourceForTest(t, storage, "database-copy.txt", "application-private-data")
	a := snapshotAdmissionForTest(t, root)
	out, err := collectSupervisionWorkspace(context.Background(), a, storage)
	if err != nil {
		t.Fatal(err)
	}
	var excluded bool
	for _, entry := range out.Manifest.Entries {
		if entry.Path == "application-data" && entry.Gap == "application_storage" {
			excluded = true
		}
	}
	if !excluded {
		t.Fatal("application storage not recorded as excluded")
	}
	if _, exists := out.Blobs[supervisionSHA256([]byte("application-private-data"))]; exists {
		t.Fatal("application data copied into source baseline")
	}
	for _, unsafe := range []string{root, filepath.Dir(root)} {
		if _, err := collectSupervisionWorkspace(context.Background(), a, unsafe); err == nil {
			t.Fatal("project inside application storage accepted")
		}
	}
	broken := filepath.Join(t.TempDir(), "broken-storage")
	if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), broken); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := collectSupervisionWorkspace(context.Background(), a, broken); err == nil {
		t.Fatal("unresolved storage silently fell back to lexical path")
	}
}

func TestSupervisionContextIsDeduplicatedAndDriftNeverReplacesStartupBaseline(t *testing.T) {
	s, p, taskID, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	writeSupervisionSourceForTest(t, p.Path, "src/main.go", "package first\n")
	writeSupervisionSourceForTest(t, p.Path, "AGENTS.md", "Never publish.\n")
	ss = startSupervisionForTest(t, s, ss.ID, now)
	first, err := s.captureSupervisionProjectContext(context.Background(), ss.ID, ss.StateVersion)
	if err != nil {
		t.Fatal(err)
	}
	before := supervisionSessionForTest(t, s, ss.ID)
	second, err := s.captureSupervisionProjectContext(context.Background(), ss.ID, before.StateVersion)
	if err != nil || first.ID != second.ID || supervisionSessionForTest(t, s, ss.ID).StateVersion != before.StateVersion {
		t.Fatalf("same facts changed context/version: %v", err)
	}
	_, d := decisionForTest(t, s, ss.ID, time.Now().UTC())
	raw, _, err := s.supervisionSchedulerInput(context.Background(), d.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	var input supervisionSchedulerInput
	if err := json.Unmarshal(raw, &input); err != nil || input.ProjectContext == nil || input.ProjectContext.ID != first.ID {
		t.Fatalf("context not frozen into observation: %v", err)
	}
	writeSupervisionSourceForTest(t, p.Path, "src/main.go", "package later\n")
	writeSupervisionSourceForTest(t, p.Path, "AGENTS.md", "Now allow publishing.\n")
	drift, err := s.captureSupervisionProjectContext(context.Background(), ss.ID, before.StateVersion)
	if !errors.Is(err, errSupervisionSnapshotChanged) || drift.ID == first.ID {
		t.Fatalf("drift not fenced: %v", err)
	}
	baseline, err := loadSupervisionProjectContext(context.Background(), s.db, ss.ID, "baseline")
	if err != nil || baseline.ID != first.ID {
		t.Fatal("startup baseline overwritten")
	}
	data, err := s.supervisionRevisionContent(context.Background(), ss.ID, first.RevisionID, "src/main.go")
	if err != nil || string(data) != "package first\n" {
		t.Fatalf("original uncommitted source lost: %q %v", data, err)
	}
	if _, err := s.supervisionRevisionContent(context.Background(), "foreign-session", first.RevisionID, "src/main.go"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("CAS content readable from foreign session")
	}
	current := supervisionSessionForTest(t, s, ss.ID)
	if current.State != "needs_input" || current.Reason != "workspace_or_rules_drift" {
		t.Fatalf("new rules automatically accepted: %+v", current)
	}
	assertExecutionClaimForTest(t, s, taskID, "supervision", ss.ID)
}

func TestSupervisionContextPersistenceFailureRollsBackSourceAndReleasesCapture(t *testing.T) {
	s, p, _, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	writeSupervisionSourceForTest(t, p.Path, "main.go", "package baseline\n")
	ss = startSupervisionForTest(t, s, ss.ID, now)
	if _, err := s.db.Exec(`create trigger reject_context before insert on supervision_project_contexts begin select raise(abort,'simulated context failure');end`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.captureSupervisionProjectContext(context.Background(), ss.ID, ss.StateVersion); err == nil {
		t.Fatal("context persistence failure ignored")
	}
	for _, table := range []string{"supervision_revision_blobs", "supervision_revisions", "supervision_project_contexts", "supervision_context_state"} {
		var count int
		if err := s.db.QueryRow(`select count(*) from ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial %s persisted: %d %v", table, count, err)
		}
	}
	var collecting int
	if err := s.db.QueryRow(`select count(*) from supervision_context_captures where state='collecting'`).Scan(&collecting); err != nil || collecting != 0 {
		t.Fatalf("capture stuck: %d %v", collecting, err)
	}
	if got := supervisionSessionForTest(t, s, ss.ID); got.State != "needs_input" || got.Reason != "workspace_capture_failed" {
		t.Fatalf("failure state=%+v", got)
	}
}

func TestSupervisionContextCollectingBlocksStopAndRecoveryDoesNotReplayRead(t *testing.T) {
	s, _, _, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	ss = startSupervisionForTest(t, s, ss.ID, now)
	if _, err := s.db.Exec(`insert into supervision_context_captures(id,session_id,state,created_at) values ('capture',?,'collecting',?)`, ss.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := s.requestSupervisionControl(context.Background(), ss.ID, ss.StateVersion, "stop", now); err != nil {
		t.Fatal(err)
	}
	if err := s.settleSupervisionControl(context.Background(), ss.ID, now); !errors.Is(err, errWorkspaceNotQuiescent) {
		t.Fatalf("stop ignored collecting read: %v", err)
	}
	if err := s.recoverWorkspaceAdmissions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.recoverSupervisionSessions(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := s.db.QueryRow(`select state from supervision_context_captures where id='capture'`).Scan(&state); err != nil || state != "failed" {
		t.Fatalf("capture replayed on recovery: %s %v", state, err)
	}
	if got := supervisionSessionForTest(t, s, ss.ID); got.RecoveryState == "ready" || got.Control != "stop" {
		t.Fatal("recovery automatically unprotected session")
	}
}
