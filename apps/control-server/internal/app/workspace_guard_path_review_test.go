package app

import (
	"context"
	"testing"
	"time"
)

func TestWorkspaceGuardWindowsPathComparison(t *testing.T) {
	for _, scenario := range []struct {
		name, a, b string
		overlap    bool
	}{
		{"drive-case", "C:/WORK/项目Ä", "c:/work/项目ä/src", true},
		{"drive-root", "C:/", "c:/work", true},
		{"neighbor", "C:/work", "c:/work-other", false},
		{"linux-case", "/srv/Repo", "/srv/repo/src", false},
		{"linux-child", "/srv/Repo", "/srv/Repo/src", true},
		{"linux-drive-like-segment", "/srv/C:/Repo", "/srv/c:/repo", false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			if got := workspaceFailurePathsOverlap(scenario.a, scenario.b); got != scenario.overlap {
				t.Fatalf("overlap(%q, %q)=%v", scenario.a, scenario.b, got)
			}
		})
	}
}

func TestWorkspaceGuardOperationListWindowsCaseAndLimit(t *testing.T) {
	s := newTestServer(t)
	now := time.Now().UTC()
	for _, row := range []struct{ id, path string }{
		{"00-unrelated", "C:/other"},
		{"01-neighbor", "C:/WORK-other"},
		{"02-parent", "C:/WORK"},
		{"03-child", "C:/WORK/项目Ä/src"},
		{"04-linux", "/srv/Repo"},
	} {
		if _, err := s.db.Exec(`insert into workspace_guard_operations(id,connection_key,path,state,created_at,updated_at) values (?,? ,?,'unconfirmed',?,?)`, row.id, "path-review", row.path, now, now); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	operations, err := s.workspaceGuardOperations(ctx, "path-review", false, 10, "c:/work/项目ä")
	if err != nil || len(operations) != 2 || operations[0].ID != "02-parent" || operations[1].ID != "03-child" {
		t.Fatalf("Windows aliases missing from recovery list: %+v %v", operations, err)
	}
	operations, err = s.workspaceGuardOperations(ctx, "path-review", false, 1, "c:/work/项目ä")
	if err != nil || len(operations) != 1 || operations[0].ID != "02-parent" {
		t.Fatalf("limit applied before path filtering: %+v %v", operations, err)
	}
	operations, err = s.workspaceGuardOperations(ctx, "path-review", false, 10, "/srv/repo")
	if err != nil || len(operations) != 0 {
		t.Fatalf("Linux paths incorrectly folded: %+v %v", operations, err)
	}
}
