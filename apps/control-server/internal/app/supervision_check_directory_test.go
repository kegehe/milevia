package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSupervisionCheckDirectoryUsesTargetPathSemantics(t *testing.T) {
	manifest := supervisionWorkspaceManifest{TargetOS: "linux", Entries: []supervisionSnapshotEntry{{Path: "App", Kind: "directory"}, {Path: "app", Kind: "file"}, {Path: "CON", Kind: "directory"}}}
	for _, name := range []string{"App", "CON"} {
		if err := validateSupervisionCheckDirectory(name, manifest); err != nil {
			t.Fatal("Linux directory was checked with controller semantics", name, err)
		}
	}
	if err := validateSupervisionCheckDirectory("app", manifest); err == nil {
		t.Fatal("Linux file accepted as differently cased directory")
	}
	manifest.TargetOS = "windows"
	manifest.Entries = []supervisionSnapshotEntry{{Path: "App", Kind: "directory"}, {Path: "CON", Kind: "directory"}}
	if err := validateSupervisionCheckDirectory("app", manifest); err != nil {
		t.Fatal("Windows directory lookup lost case folding", err)
	}
	if err := validateSupervisionCheckDirectory("CON", manifest); err == nil {
		t.Fatal("Windows reserved directory accepted")
	}
}

func TestSupervisionCheckDirectoryUsesCoveredAncestorsWithoutRequiringCoveredDescendants(t *testing.T) {
	manifest := supervisionWorkspaceManifest{Entries: []supervisionSnapshotEntry{
		{Path: ".", Kind: "directory"},
		{Path: "apps", Kind: "directory"},
		{Path: "apps/web", Kind: "directory"},
		{Path: "apps/web/src", Kind: "directory"},
		{Path: "apps/web/src/check.cjs", Kind: "file"},
		{Path: "apps/web/node_modules", Kind: "excluded", Gap: "excluded_directory"},
		{Path: "apps/web/.env", Kind: "excluded", Gap: "sensitive_path"},
		{Path: "apps/web/link", Kind: "symlink", Gap: "link_input_not_resolved"},
		{Path: "apps/data", Kind: "excluded", Gap: "application_storage"},
	}}
	plan := validSupervisionPlanActionForTest(supervisionSession{}, "task", supervisionProjectContext{}).Plan
	plan.Scope = []string{"apps/web/src"}
	if err := validateSupervisionPlan(&plan, manifest); err != nil {
		t.Fatal(err)
	}
	for _, browser := range []bool{false, true} {
		kind := "command"
		check := supervisionCheck{ID: "check", AcceptanceIDs: []string{"behavior"}, Program: "node", Args: []string{"src/check.cjs"}, TimeoutMillis: 30000}
		if browser {
			kind = "browser"
			check.Browser = &supervisionBrowserCheck{ServerProgram: "node", ServerArgs: []string{"src/server.cjs"}, URL: "http://127.0.0.1:12345", Steps: []supervisionBrowserStep{{Action: "visible", Selector: "body"}}}
		}
		t.Run(kind, func(t *testing.T) {
			plan.Acceptance[0].Kind = kind
			for _, directory := range []string{"", ".", "apps", "apps/web", "apps/web/src"} {
				t.Run("allowed/"+directory, func(t *testing.T) {
					check.Directory = directory
					if err := validateSupervisionCheck(check, plan, manifest); err != nil {
						t.Fatal("covered working directory rejected", err)
					}
				})
			}
			for _, directory := range []string{"missing", "apps/web/new", "apps/web/src/check.cjs", "apps/web/src/check.cjs/child", "apps/web/node_modules", "apps/web/node_modules/pkg", "apps/web/.env", "apps/web/link", "apps/web/link/child", "apps/data", "apps/data/child", "apps/web/dist", "apps/web/.cache", "apps/web/../web", "./apps/web", "../outside", "/apps/web", "C:/outside", `apps\web`, "apps/web/", "apps/web.", "apps/web ", "apps/web/*", "apps/web\x00"} {
				t.Run("rejected/"+directory, func(t *testing.T) {
					check.Directory = directory
					if err := validateSupervisionCheck(check, plan, manifest); err == nil {
						t.Fatal("unsafe or uncovered working directory accepted", directory)
					}
				})
			}
		})
	}
	// A working directory does not grant the implementation permission to edit
	// excluded descendants; the stricter write-scope contract remains intact.
	plan.Scope = []string{"apps/web"}
	if err := validateSupervisionPlan(&plan, manifest); err == nil {
		t.Fatal("working-directory validation weakened implementation write scopes")
	}
}

func TestSupervisionSensitiveRuntimeFilesParticipateInEnvironmentFingerprint(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(binary, []byte("tool fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	fingerprint := func() string {
		t.Helper()
		value, err := supervisionEnvironmentFingerprint(context.Background(), root, binary)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	for _, name := range []string{".env", ".ENV.local", ".npmrc", ".pypirc", ".netrc", "auth.json", "db.credentials", "config.pem", "test.key"} {
		t.Run(name, func(t *testing.T) {
			relative := "apps/web/" + name
			before := fingerprint()
			writeSupervisionSourceForTest(t, root, relative, "fixture setting version one\n")
			first := fingerprint()
			if first == before || first != fingerprint() {
				t.Fatal("new runtime configuration was ignored or fingerprint is unstable")
			}
			writeSupervisionSourceForTest(t, root, relative, "fixture setting version two\n")
			if fingerprint() == first {
				t.Fatal("runtime configuration change did not invalidate environment")
			}
			if err := os.Remove(filepath.Join(root, filepath.FromSlash(relative))); err != nil {
				t.Fatal(err)
			}
			if fingerprint() != before {
				t.Fatal("deleted runtime configuration remains in environment")
			}
		})
	}
}
