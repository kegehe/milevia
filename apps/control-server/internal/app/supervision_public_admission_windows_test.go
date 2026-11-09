package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

// Opt-in integration: actual HTTP admission, SQLite, WSL helper, common guard
// journal, target source capture, credential revocation and confirmed release.
// CLI capability is probed, but no model is called and no task is accepted done.
func TestSupervisionPublicWSLAdmissionMultipleTasksAndIndependentWriter(t *testing.T) {
	bundle := os.Getenv("MILEVIA_SUPERVISION_TARGET_BUNDLE")
	if bundle == "" {
		t.Skip("set a freshly built target bundle and provide Ubuntu WSL with Codex installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	distro := os.Getenv("MILEVIA_SUPERVISION_TARGET_DISTRO")
	if distro == "" {
		distro = "Ubuntu-24.04"
	}
	runner := newWSLAgentRunner(Config{}, distro, ctx)
	raw, err := runner.supervisionTargetCommand(ctx, `mktemp -d /tmp/milevia-public-admission-XXXXXX`, nil)
	if err != nil {
		t.Fatal(err)
	}
	root := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(root, "/tmp/milevia-public-admission-") || strings.ContainsAny(root, "\r\n") {
		t.Fatal("unexpected isolated target root")
	}
	if _, err := runner.supervisionTargetCommand(ctx, `mkdir -p "$1/ProjectCase/src" "$1/home"; printf '%s' 'ACTUAL_LINUX_SOURCE' > "$1/ProjectCase/src/source.txt"`, nil, root); err != nil {
		t.Fatal(err)
	}
	s, p, first, _, _ := supervisionFixture(t, defaultSupervisionBudget())
	p.Path, p.Runner, p.RunnerID = wslToUncPath(root+"/ProjectCase", distro), "wsl-local", "wsl-local"
	if _, err := s.db.Exec(`update projects set path=?,runner=?,runner_id=? where id=?`, p.Path, p.Runner, p.RunnerID, p.ID); err != nil {
		t.Fatal(err)
	}
	s.config.SupervisionTargetsDir = bundle
	s.wslMu.Lock()
	s.wslRunner, s.wslDistro = runner, distro
	s.wslMu.Unlock()
	s.runnerRegistry.register("wsl-local", runner, RunnerMeta{ID: "wsl-local", Environment: "wsl"})
	s.rememberWorkspaceScope(p.ID, p.Path, p.Runner)
	route := s.supervisionProjectRoute(p)
	// Isolate execution state from the user's real tasks while retaining the
	// production helper and the target user's common protection journal.
	s.supervisionTargets = newSupervisionTargetConnections(s.runtimeCtx, func(ctx context.Context, key string) (*supervisionTargetLink, error) {
		if key != route.Key {
			return nil, targetexec.ErrConflict
		}
		link, err := openSupervisionTargetAt(ctx, s.config, runner, "", root+"/home")
		if err == nil {
			err = s.recordSupervisionTargetBinding(ctx, key, link)
		}
		if err == nil && os.Getenv("MILEVIA_SUPERVISION_WSL_CODEX") != "" {
			_, err = link.Call(ctx, targetexec.Request{ID: uuid.NewString(), Method: "configure.paths", Parameters: mustJSON(map[string]string{"codex": os.Getenv("MILEVIA_SUPERVISION_WSL_CODEX")})})
		}
		if err != nil && link != nil {
			link.Close()
		}
		return link, err
	})
	newGuardPool := func(controller *Server) {
		controller.workspaceGuardTargets = newSupervisionTargetConnections(controller.runtimeCtx, func(ctx context.Context, key string) (*supervisionTargetLink, error) {
			if key != route.Key {
				return nil, targetexec.ErrConflict
			}
			return openSupervisionTargetMode(ctx, s.config, runner, "", root+"/home", "workspace-guard")
		})
	}
	newGuardPool(s)
	other := newTestServer(t)
	other.wslMu.Lock()
	other.wslRunner, other.wslDistro = runner, distro
	other.wslMu.Unlock()
	newGuardPool(other)
	second, now := uuid.NewString(), time.Now().UTC()
	if _, err := s.db.Exec(`insert into tasks(id,project_id,title,description,priority,pinned,position,status,created_at,updated_at) values (?,?, 'Independent task','Verify independently','normal',0,2,'todo',?,?)`, second, p.ID, now, now); err != nil {
		t.Fatal(err)
	}
	profile := createSupervisionProfileForTest(t, s, p)
	ordinary, err := other.beginWorkspaceOperationAtPath(p, p.Path, "independent-controller")
	if err != nil {
		t.Fatal("ordinary remote writer did not register", err)
	}
	defer ordinary()
	// One failed launcher in an unrelated project on the same WSL distro must
	// not retain this project's ordinary writer and deadlock its pending drain.
	if _, err := runner.supervisionTargetCommand(ctx, `mkdir -p "$1/UnrelatedCase"`, nil, root); err != nil {
		t.Fatal(err)
	}
	unrelated := p
	unrelated.Path = wslToUncPath(root+"/UnrelatedCase", distro)
	unrelatedRelease, err := other.beginWorkspaceOperationAtPath(unrelated, unrelated.Path, "unrelated-launcher")
	if err != nil {
		t.Fatal(err)
	}
	_, launcherErr := runner.supervisionTargetCommand(ctx, `exit 7`, nil)
	if launcherErr == nil {
		t.Fatal("actual WSL launcher did not fail")
	}
	noteWorkspaceWSLExit(distro, ctx, launcherErr, true, root+"/UnrelatedCase")
	unrelatedRelease()
	create := httptest.NewRecorder()
	s.routes().ServeHTTP(create, httptest.NewRequest(http.MethodPost, "/api/projects/"+p.ID+"/supervision", strings.NewReader(string(mustJSON(map[string]any{"taskIds": []string{first, second}, "schedulerAgentId": "codex", "roles": supervisionRoleConfigs{"scheduler": {AgentID: "codex", ProfileID: profile.ID}}})))))
	var ss supervisionSession
	if create.Code != http.StatusCreated || json.Unmarshal(create.Body.Bytes(), &ss) != nil {
		t.Fatal(create.Code, create.Body.String())
	}
	start := httptest.NewRecorder()
	s.routes().ServeHTTP(start, httptest.NewRequest(http.MethodPost, "/api/supervision/"+ss.ID+"/start", strings.NewReader(string(mustJSON(map[string]any{"stateVersion": ss.StateVersion})))))
	if start.Code != http.StatusAccepted {
		t.Fatal("public cross-environment start rejected", start.Code, start.Body.String())
	}
	ss = supervisionSessionForTest(t, s, ss.ID)
	if err := s.admitSupervisionWorkspace(ctx, ss.ID, ss.StateVersion, time.Now().UTC()); !errors.Is(err, targetexec.ErrNotQuiet) {
		t.Fatal("controller's empty operation map bypassed another controller's writer", err)
	}
	if release, err := other.beginWorkspaceOperationAtPath(p, p.Path, "late-writer"); err == nil {
		release()
		t.Fatal("pending remote admission allowed another ordinary writer")
	}
	ordinary()
	if err := s.admitSupervisionWorkspace(ctx, ss.ID, ss.StateVersion, time.Now().UTC()); err != nil {
		t.Fatal("remote admission did not drain", err)
	}
	if err := s.ensureSupervisionPublicTarget(ctx, ss.ID, "public-integration"); err != nil {
		t.Fatal(err)
	}
	if release, err := other.beginWorkspaceOperationAtPath(Project{Runner: "windows-local"}, p.Path, "unc-alias-writer"); err == nil {
		release()
		t.Fatal("Windows view of the WSL UNC directory bypassed its Linux protection journal")
	}
	ss = supervisionSessionForTest(t, s, ss.ID)
	pc, err := s.captureSupervisionProjectContext(ctx, ss.ID, ss.StateVersion)
	if err != nil || pc.RevisionID == "" {
		t.Fatal("baseline failed to capture actual target", err)
	}
	for _, taskID := range []string{first, second} {
		assertExecutionClaimForTest(t, s, taskID, "supervision", ss.ID)
	}
	revoke := httptest.NewRecorder()
	s.routes().ServeHTTP(revoke, httptest.NewRequest(http.MethodPost, "/api/supervision-profile-revisions/"+profile.CurrentRevisionID+"/revoke", nil))
	if revoke.Code != http.StatusAccepted {
		t.Fatal(revoke.Body.String())
	}
	if err := s.settleSupervisionControl(ctx, ss.ID, time.Now().UTC()); err != nil {
		t.Fatal("target credential fence did not settle", err)
	}
	ss = supervisionSessionForTest(t, s, ss.ID)
	if ss.State != "paused" || ss.Reason != "credential_revoked" {
		t.Fatal("revoked target profile did not pause both tasks", ss.State, ss.Reason)
	}
	if release, err := other.beginWorkspaceOperationAtPath(p, p.Path, "paused-writer"); err == nil {
		release()
		t.Fatal("pause falsely released target directory")
	}
	if err := s.requestSupervisionControl(ctx, ss.ID, ss.StateVersion, "stop", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.settleSupervisionControl(ctx, ss.ID, time.Now().UTC()); err != nil {
		t.Fatal("confirmed stop failed to release common target protection", err)
	}
	for _, taskID := range []string{first, second} {
		assertExecutionClaimForTest(t, s, taskID, "", "")
	}
	released, err := other.beginWorkspaceOperationAtPath(p, p.Path, "after-confirmed-release")
	if err != nil {
		t.Fatal("target protection remained after confirmed release", err)
	}
	released()
	aliasRelease, err := other.beginWorkspaceOperationAtPath(Project{Runner: "windows-local"}, p.Path, "released-unc-alias")
	if err != nil {
		t.Fatal("UNC routing changed the case-sensitive Linux directory", err)
	}
	aliasRelease()
	// Stop may overtake initial target binding. Its pending common reservation
	// still needs a target tombstone before either task claim can be released.
	ordinary, err = other.beginWorkspaceOperationAtPath(p, p.Path, "pending-drain-writer")
	if err != nil {
		t.Fatal(err)
	}
	defer ordinary()
	pending, err := s.createSupervisionDraft(ctx, p.ID, []string{first, second}, "codex", defaultSupervisionBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.startSupervisionSession(ctx, pending.ID, pending.StateVersion, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	pending = supervisionSessionForTest(t, s, pending.ID)
	if err := s.admitSupervisionWorkspace(ctx, pending.ID, pending.StateVersion, time.Now().UTC()); !errors.Is(err, targetexec.ErrNotQuiet) {
		t.Fatal("pending drain did not observe the independent writer", err)
	}
	if err := s.requestSupervisionControl(ctx, pending.ID, pending.StateVersion, "stop", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.settleSupervisionControl(ctx, pending.ID, time.Now().UTC()); err != nil {
		t.Fatal("stop before target binding stranded common ownership", err)
	}
	for _, taskID := range []string{first, second} {
		assertExecutionClaimForTest(t, s, taskID, "", "")
	}
	late, err := other.beginWorkspaceOperationAtPath(p, p.Path, "after-unbound-stop")
	if err != nil {
		t.Fatal("unbound stop left a pending common reservation", err)
	}
	late()
	ordinary()
	// Lose the controller's cleanup after the real helper has acknowledged
	// release. Durable intent must recover through the same pinned journal.
	finished, err := other.beginWorkspaceOperationAtPath(p, p.Path, "lost-release-cleanup")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.db.Exec(`create trigger fail_public_guard_delete before delete on workspace_guard_operations begin select raise(abort,'lost release cleanup'); end`); err != nil {
		t.Fatal(err)
	}
	finished()
	if _, err := other.db.Exec(`drop trigger fail_public_guard_delete`); err != nil {
		t.Fatal(err)
	}
	var pendingReleases int
	if err := other.db.QueryRow(`select count(*) from workspace_guard_operations where state='release_pending'`).Scan(&pendingReleases); err != nil || pendingReleases != 1 {
		t.Fatal("real target release intent was lost", pendingReleases, err)
	}
	if err := other.reconcilePendingWorkspaceGuardOperations(ctx); err != nil {
		t.Fatal(err)
	}
	if err := other.db.QueryRow(`select count(*) from workspace_guard_operations where state='release_pending'`).Scan(&pendingReleases); err != nil || pendingReleases != 0 {
		t.Fatal("real target release could not be recovered", pendingReleases, err)
	}
	t.Log("real public admission and two-task ownership verified; model calls=0; retained target", root)
}
