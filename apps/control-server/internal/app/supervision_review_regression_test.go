package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSupervisionBrowserCheckRequiresIndependentServerBeforeDispatch(t *testing.T) {
	plan := supervisionTaskPlan{Acceptance: []supervisionAcceptancePoint{{ID: "behavior", Kind: "browser", Critical: true}}}
	check := supervisionCheck{ID: "browser", AcceptanceIDs: []string{"behavior"}, Directory: ".", TimeoutMillis: 1000,
		Browser: &supervisionBrowserCheck{URL: "http://127.0.0.1:12345", Steps: []supervisionBrowserStep{{Action: "visible", Selector: "body"}}}}
	if err := validateSupervisionCheck(check, plan, supervisionWorkspaceManifest{}); err == nil {
		t.Fatal("browser check without an independent server was accepted for dispatch")
	}
	check.Browser.ServerProgram, check.Browser.ServerArgs = "node", []string{"server.cjs"}
	if err := validateSupervisionCheck(check, plan, supervisionWorkspaceManifest{}); err != nil {
		t.Fatal("valid browser server rejected", err)
	}
}

func TestSupervisionBrowserCheckCannotProduceUnusableCommandEvidence(t *testing.T) {
	plan := supervisionTaskPlan{Acceptance: []supervisionAcceptancePoint{{ID: "behavior", Kind: "command", Critical: true}}}
	check := supervisionCheck{ID: "check", AcceptanceIDs: []string{"behavior"}, Directory: ".", TimeoutMillis: 1000,
		Browser: &supervisionBrowserCheck{URL: "http://127.0.0.1:12345", ServerProgram: "node", ServerArgs: []string{"server.cjs"}, Steps: []supervisionBrowserStep{{Action: "visible", Selector: "body"}}}}
	if err := validateSupervisionCheck(check, plan, supervisionWorkspaceManifest{}); err == nil {
		t.Fatal("browser check accepted a command point even though it only records browser evidence")
	}
	plan.Acceptance[0].Kind = "browser"
	if err := validateSupervisionCheck(check, plan, supervisionWorkspaceManifest{}); err != nil {
		t.Fatal("valid browser point rejected", err)
	}
}

func TestSupervisionStopAfterDirectoryReplacementDoesNotKeepPermanentClaims(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "recovered"}[recovered], func(t *testing.T) {
			s, p, taskID, ss, now := supervisionFixture(t, defaultSupervisionBudget())
			p.Path = filepath.Join(t.TempDir(), "workspace")
			if err := os.Mkdir(p.Path, 0700); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`update projects set path=? where id=?`, p.Path, p.ID); err != nil {
				t.Fatal(err)
			}
			s.rememberWorkspaceScope(p.ID, p.Path, p.Runner)
			ss = startSupervisionForTest(t, s, ss.ID, now)
			if err := os.Rename(p.Path, p.Path+"-old"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(p.Path, 0700); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if recovered {
				if err := s.recoverWorkspaceAdmissions(ctx); err != nil {
					t.Fatal(err)
				}
				if err := s.recoverSupervisionSessions(ctx, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
			}
			ss = supervisionSessionForTest(t, s, ss.ID)
			if err := s.requestSupervisionControl(ctx, ss.ID, ss.StateVersion, "stop", time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			if recovered {
				if err := s.prepareRecoveredSupervisionControl(ctx, ss.ID); err != nil {
					t.Fatal("quiescent stop cannot recover after directory replacement", err)
				}
			}
			if _, err := s.db.Exec(`insert into supervision_context_captures(id,session_id,state,created_at) values ('active-read',?,'collecting',?)`, ss.ID, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			if err := s.settleSupervisionControl(ctx, ss.ID, time.Now().UTC()); !errors.Is(err, errWorkspaceNotQuiescent) {
				t.Fatal("directory replacement bypassed active resource protection", err)
			}
			if _, claimed, err := taskExecutionClaim(ctx, s.db, taskID); err != nil || !claimed {
				t.Fatal("active resource lost task protection", claimed, err)
			}
			if _, err := s.db.Exec(`update supervision_context_captures set state='failed' where id='active-read'`); err != nil {
				t.Fatal(err)
			}
			if err := s.settleSupervisionControl(ctx, ss.ID, time.Now().UTC()); err != nil {
				t.Fatal("quiescent stop cannot release after directory replacement", err)
			}
			if got := supervisionSessionForTest(t, s, ss.ID); got.State != "stopped" {
				t.Fatal("stop did not settle", got)
			}
			if _, claimed, err := taskExecutionClaim(ctx, s.db, taskID); err != nil || claimed {
				t.Fatal("stop kept a permanent task claim", claimed, err)
			}
			var count int
			if err := s.db.QueryRow(`select count(*) from workspace_admissions where owner_id=?`, ss.ID).Scan(&count); err != nil || count != 0 {
				t.Fatal("stop kept a permanent directory lock", count, err)
			}
		})
	}
}
