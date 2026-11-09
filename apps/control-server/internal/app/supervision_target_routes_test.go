package app

import (
	"context"
	"database/sql"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

func TestSupervisionTargetRoutesSeparateActualEnvironments(t *testing.T) {
	s := &Server{}
	native := s.supervisionProjectRoute(Project{Runner: "wsl-local", Path: "/home/project"})
	mounted := s.supervisionProjectRoute(Project{Runner: "wsl-local", Path: "/mnt/c/project"})
	if runtime.GOOS != "windows" && (native.Key == mounted.Key || native.Environment != agentTargetEnvWSL || mounted.Environment != agentTargetEnvWindows) {
		t.Fatal("WSL native and Windows interop reused an execution route", native, mounted)
	}
	wsl := newSupervisionTargetRoute("wsl-local", agentTargetEnvWSL)
	windows := newSupervisionTargetRoute("wsl-local", agentTargetEnvWindows)
	if wsl.Key == windows.Key {
		t.Fatal("management identity collapsed different actual targets")
	}
	a := s.supervisionProjectRoute(Project{Runner: "wrong", RunnerID: "ssh-target", Path: "/mnt/c/project"})
	b := s.supervisionProjectRoute(Project{Runner: "ssh-target", Path: "/other/project"})
	if a != b || a.Environment != agentTargetEnvRemote {
		t.Fatal("preferred runner or remote path semantics were lost", a, b)
	}
	if s.supervisionProjectTarget(Project{Runner: "wrong", RunnerID: s.localRunnerID()}).RunnerID != s.localRunnerID() {
		t.Fatal("capability and routing disagree about the preferred runner")
	}
}

func TestSupervisionTargetRoutesPersistBothEnvironmentsAndRejectAlteredRecord(t *testing.T) {
	s, _, _, _, _ := supervisionFixture(t, defaultSupervisionBudget())
	ctx := context.Background()
	routes := []supervisionTargetRoute{newSupervisionTargetRoute("wsl-local", agentTargetEnvWSL), newSupervisionTargetRoute("wsl-local", agentTargetEnvWindows)}
	for _, route := range routes {
		if _, err := s.db.Exec(`insert into supervision_target_routes(connection_key,runner_id,environment) values (?,?,?)`, route.Key, route.RunnerID, route.Environment); err != nil {
			t.Fatal(err)
		}
		if actual, err := loadSupervisionTargetRoute(ctx, s.db, route.Key); err != nil || actual != route {
			t.Fatal("durable route changed", actual, err)
		}
		expectedOS, otherOS := "linux", "windows"
		if route.Environment == agentTargetEnvWindows {
			expectedOS, otherOS = otherOS, expectedOS
		}
		if err := verifySupervisionTargetRoutePlatform(ctx, s.db, route.Key, expectedOS); err != nil {
			t.Fatal(err)
		}
		if err := verifySupervisionTargetRoutePlatform(ctx, s.db, route.Key, otherOS); !errors.Is(err, targetexec.ErrConflict) {
			t.Fatal("route accepted another environment's physical target", err)
		}
	}
	if _, err := s.db.Exec(`update supervision_target_routes set runner_id='ssh-other' where connection_key=?`, routes[0].Key); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.supervisionTargetRouteRunner(ctx, routes[0].Key); !errors.Is(err, targetexec.ErrConflict) {
		t.Fatal("altered route accepted", err)
	}
	if _, _, err := s.supervisionTargetRouteRunner(ctx, supervisionTargetRoutePrefix+strings.Repeat("f", 64)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("missing route silently fell back to a local runner", err)
	}
}

func TestSupervisionTargetRoutesUseFrozenConnectionForSourceAndStop(t *testing.T) {
	s, id, link := supervisionWorkspaceTargetFixture(t, nil)
	ctx := context.Background()
	ss := supervisionSessionForTest(t, s, id)
	var p Project
	if err := s.db.QueryRow(`select id,path,coalesce(nullif(runner_id,''),runner) from projects where id=?`, ss.ProjectID).Scan(&p.ID, &p.Path, &p.Runner); err != nil {
		t.Fatal(err)
	}
	route := s.supervisionProjectRoute(p)
	if _, err := s.db.Exec(`insert into supervision_target_routes(connection_key,runner_id,environment) values (?,?,?)`, route.Key, route.RunnerID, route.Environment); err != nil {
		t.Fatal(err)
	}
	if err := s.recordSupervisionTargetBinding(ctx, route.Key, link); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`update supervision_target_controls set connection_key=? where session_id=?`, route.Key, id); err != nil {
		t.Fatal(err)
	}
	s.supervisionTargets.open = func(_ context.Context, key string) (*supervisionTargetLink, error) {
		if key != route.Key {
			return nil, errors.New("borrowed another environment's owner")
		}
		return link, nil
	}
	var source supervisionWorkspaceSource
	err := s.withSupervisionTx(ctx, id, time.Now().UTC(), func(tx *sql.Tx, admissions map[string]WorkspaceAdmission, current *supervisionSession) error {
		var err error
		source, err = s.supervisionWorkspaceSourceTx(ctx, tx, current, admissions[current.AdmissionID])
		return err
	})
	if err != nil || source.ConnectionKey != route.Key || source.RunnerID != route.RunnerID {
		t.Fatal("source confused connection identity and management identity", source, err)
	}
	collected, err := source.collect(ctx, s)
	if err != nil || collected.RevisionID == "" {
		t.Fatal("source did not use the frozen environment connection", err)
	}
	if err := s.requestSupervisionControl(ctx, id, ss.StateVersion, "stop", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.settleSupervisionControl(ctx, id, time.Now().UTC()); err != nil {
		t.Fatal("stop/release did not use the original environment", err)
	}
	actual := supervisionSessionForTest(t, s, id)
	if actual.State != "stopped" {
		t.Fatal(actual.State)
	}
}

func TestSupervisionTargetRoutesRejectProjectEnvironmentChange(t *testing.T) {
	s, p, _, _, _ := supervisionFixture(t, defaultSupervisionBudget())
	ctx := context.Background()
	route := s.supervisionProjectRoute(p)
	if _, err := s.db.Exec(`insert into supervision_target_routes(connection_key,runner_id,environment) values (?,?,?)`, route.Key, route.RunnerID, route.Environment); err != nil {
		t.Fatal(err)
	}
	if id, err := s.checkSupervisionTargetProjectRoute(ctx, s.db, route.Key, p); err != nil || id != route.RunnerID {
		t.Fatal(id, err)
	}
	p.Runner, p.RunnerID = "ssh-different", "ssh-different"
	if _, err := s.checkSupervisionTargetProjectRoute(ctx, s.db, route.Key, p); !errors.Is(err, targetexec.ErrConflict) {
		t.Fatal("old route authorized a different project target", err)
	}
	if got := supervisionTargetConnectionKey(supervisionTargetControlIntent{RunnerID: "old-runner"}); got != "old-runner" {
		t.Fatal("old active session was silently assigned a new route", got)
	}
}
