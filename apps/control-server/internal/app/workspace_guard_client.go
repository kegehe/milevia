package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

// Path translation is explicit and belongs to the actual filesystem endpoint.
// In particular an invalid UNC or a lookalike /mnt path never falls back home.
func (s *Server) workspaceGuardTargetPath(p Project) (supervisionTargetRoute, string, error) {
	route := s.supervisionProjectRoute(p)
	path := p.Path
	// Ordinary entry points may access a Windows drive through an older
	// wsl-local management selection. Protect the physical filesystem rather
	// than inventing a second WSL journal for that drive. Execution routing is
	// unchanged and supervision still validates its actual Runner separately.
	if runtime.GOOS == "windows" && route.Environment == agentTargetEnvWSL && route.RunnerID == "wsl-local" && supervisionTargetAbsolutePath("windows", path) && !isWSLUncPath(path) {
		route = newSupervisionTargetRoute(s.localRunnerID(), agentTargetEnvWindows)
	}
	if route.local() {
		canonical, err := canonicalWorkspacePath(path)
		if err != nil {
			return route, "", err
		}
		// Resolve Linux symlink aliases before deciding whether Windows owns the
		// physical mounted directory. Other platform aliases resolve natively.
		if runtime.GOOS != "windows" {
			actual := s.supervisionProjectRoute(Project{Runner: route.RunnerID, Path: canonical})
			if !actual.local() {
				route, path = actual, canonical
			}
		}
		if route.local() {
			if runtime.GOOS == "windows" && isWSLUncPath(canonical) {
				// UNC is a view of the Linux filesystem, not a second Windows
				// protection domain. Use its actual WSL owner's common journal.
				route, path = newSupervisionTargetRoute("wsl-local", agentTargetEnvWSL), canonical
			} else {
				return route, canonical, nil
			}
		}
	}
	switch route.Environment {
	case agentTargetEnvWindows:
		if runtime.GOOS != "windows" {
			converted, ok := wslPathToWindowsPath(path)
			if !ok {
				return route, "", targetexec.ErrConflict
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			actual, err := exec.CommandContext(ctx, "wslpath", "-w", path).Output()
			if err != nil || !strings.EqualFold(strings.TrimSpace(string(actual)), converted) {
				return route, "", errors.New("无法证明该路径映射到实际 Windows 共享目录")
			}
			path = converted
		}
	case agentTargetEnvWSL:
		if runtime.GOOS == "windows" {
			runner, ok := s.wslAgentRunner().(*wslAgentRunner)
			if !ok || runner == nil {
				return route, "", errSupervisionProcessUnsupported
			}
			converted, ok := uncToWslPath(path, runner.distro)
			if !ok {
				return route, "", targetexec.ErrConflict
			}
			path = converted
		}
	}
	targetOS := "linux"
	if route.Environment == agentTargetEnvWindows {
		targetOS = "windows"
	}
	if !supervisionTargetAbsolutePath(targetOS, path) {
		return route, "", targetexec.ErrConflict
	}
	return route, path, nil
}

func (s *Server) connectWorkspaceGuard(ctx context.Context, route supervisionTargetRoute) (*supervisionTargetLink, error) {
	if _, err := s.db.ExecContext(ctx, `insert into supervision_target_routes(connection_key,runner_id,environment) values (?,?,?) on conflict(connection_key) do nothing`, route.Key, route.RunnerID, route.Environment); err != nil {
		return nil, err
	}
	s.supervisionMu.Lock()
	if s.runtimeCtx == nil || s.runtimeCtx.Err() != nil {
		s.supervisionMu.Unlock()
		return nil, targetexec.ErrClosed
	}
	if s.workspaceGuardTargets == nil {
		s.workspaceGuardTargets = newSupervisionTargetConnections(s.runtimeCtx, func(ctx context.Context, key string) (*supervisionTargetLink, error) {
			runner, _, err := s.supervisionTargetRouteRunner(ctx, key)
			if err != nil {
				return nil, err
			}
			var expected string
			err = s.db.QueryRowContext(ctx, `select target_id from workspace_guard_bindings where connection_key=?`, key).Scan(&expected)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			link, err := openSupervisionTargetMode(ctx, s.config, runner, expected, "", "workspace-guard")
			if err != nil {
				return nil, err
			}
			result, err := s.db.ExecContext(ctx, `insert into workspace_guard_bindings(connection_key,target_id) values (?,?) on conflict(connection_key) do update set target_id=excluded.target_id where workspace_guard_bindings.target_id=excluded.target_id`, key, link.TargetID)
			if err == nil {
				var count int64
				count, err = result.RowsAffected()
				if count != 1 && err == nil {
					err = targetexec.ErrConflict
				}
			}
			if err != nil {
				link.Close()
				return nil, err
			}
			return link, nil
		})
	}
	pool := s.workspaceGuardTargets
	s.supervisionMu.Unlock()
	return pool.acquire(ctx, route.Key)
}

func (s *Server) beginSharedWorkspaceOperation(scope workspaceScope, cliTicket ...*workspaceGuardRecord) (func(), error) {
	if scope.Path == "" {
		if strings.TrimSpace(scope.OriginalPath) != "" {
			return nil, errors.New("无法解析工作目录，不能跳过共享目录保护")
		}
		return func() {}, nil
	} // Legacy non-directory lease keys.
	if s.db == nil {
		return nil, errors.New("共享目录保护数据库不可用")
	}
	project := Project{Runner: scope.RunnerID, Path: scope.OriginalPath}
	if project.Path == "" {
		project.Path = scope.Path
	}
	route, path, err := s.workspaceGuardTargetPath(project)
	if err != nil {
		return nil, err
	}
	record := workspaceGuardRecord{ID: uuid.NewString(), Kind: "operation", Path: path, ValidUntil: time.Now().UTC().Add(45 * time.Second)}
	if len(cliTicket) != 0 {
		record.CLIContainer = uuid.NewString()
	}
	var sshClient *sshClient
	var sshEpoch uint64
	var wslUnchanged func() bool
	var wslTracker *workspaceFailureTracker
	var wslDistro string
	var checks []func() bool
	defer func() {
		for _, check := range checks {
			check()
		}
	}()
	if runtime.GOOS == "windows" && (route.Environment == agentTargetEnvWSL || scope.RunnerID == "wsl-local") {
		if runner, ok := s.wslAgentRunner().(*wslAgentRunner); ok && runner != nil {
			counter := workspaceWSLUncertainty(runner.distro)
			epoch := counter.Load()
			wslUnchanged = func() bool { return counter.Load() == epoch }
			tracker := workspaceWSLFailureTracker(runner.distro)
			wslTracker, wslDistro = tracker, runner.distro
			checks = append(checks, tracker.watch(workspaceWSLTrackingPath(runner.distro, path)), tracker.watch(workspaceWSLTrackingPath(runner.distro, project.Path)))
		}
	}
	if s.runnerRegistry != nil {
		if runner, ok := s.runnerRegistry.get(route.RunnerID); ok {
			if remote, ok := runner.(*sshRunner); ok && remote.client != nil {
				sshClient = remote.client
				sshEpoch = sshClient.workspaceUncertainty.Load()
				checks = append(checks, sshClient.workspaceFailures.watch(path), sshClient.workspaceFailures.watch(project.Path))
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	var call func(context.Context, string, workspaceGuardRecord) error
	var journalID string
	var helperSHA256 string
	if route.local() {
		if err := withWorkspaceGuard(ctx, s.config, func(f *workspaceGuardFacts) error { journalID = f.ID; return nil }); err != nil {
			return nil, err
		}
		call = func(ctx context.Context, method string, rec workspaceGuardRecord) error {
			rec.JournalID = journalID
			_, err := changeWorkspaceGuard(ctx, s.config, method, rec)
			return err
		}
	} else {
		link, err := s.connectWorkspaceGuard(ctx, route)
		if err != nil {
			return nil, err
		}
		journalID = link.TargetID
		helperSHA256 = link.Artifact.SHA256
		// A pooled pipe's prior clock sample can be arbitrarily old. Refresh it
		// before each new begin, within the protocol's 30-second grant ceiling.
		response, err := link.Call(ctx, targetexec.Request{ID: uuid.NewString(), Method: "health"})
		if err != nil {
			return nil, err
		}
		var health struct {
			Protocol    int  `json:"protocol"`
			Guard       bool `json:"workspaceGuard"`
			OrdinaryCLI bool `json:"ordinaryCLIContainment"`
		}
		if json.Unmarshal(response.Data, &health) != nil || health.Protocol != targetexec.ProtocolVersion || !health.Guard {
			return nil, targetexec.ErrProtocol
		}
		if record.CLIContainer != "" && !health.OrdinaryCLI {
			return nil, errors.New("目标辅助程序尚未支持普通 CLI 严格容器，请更新目标构建产物")
		}
		record.ValidUntil, err = link.Client.TargetDeadline(30 * time.Second)
		if record.CLIContainer != "" {
			record.CLILeaseUntil = record.ValidUntil
		}
		if err != nil {
			return nil, err
		}
		call = func(ctx context.Context, method string, rec workspaceGuardRecord) error {
			rec.JournalID = journalID
			response, err := link.Call(ctx, targetexec.Request{ID: uuid.NewString(), Method: "guard." + method, Parameters: mustJSON(rec)})
			if err != nil {
				return err
			}
			var actual workspaceGuardRecord
			if json.Unmarshal(response.Data, &actual) != nil || actual.ID != rec.ID || actual.Kind != rec.Kind || method == "release" && actual.State != "released" || method == "begin" && actual.State != "active" {
				return targetexec.ErrProtocol
			}
			if method == "begin" && actual.Path != "" {
				if sshClient != nil {
					checks = append(checks, sshClient.workspaceFailures.watch(actual.Path))
				}
				if wslTracker != nil {
					checks = append(checks, wslTracker.watch(workspaceWSLTrackingPath(wslDistro, actual.Path)))
				}
			}
			return nil
		}
	}
	// Release carries just the immutable token. A target-canonicalized alias must
	// not make cleanup depend on re-resolving a path that was moved meanwhile.
	token := workspaceGuardRecord{ID: record.ID, Kind: record.Kind, ValidUntil: record.ValidUntil}
	// Save the immutable operation token before sending begin. Controller crashes
	// and unknown exits must remain diagnosable after the in-memory closure is lost.
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx, `insert into supervision_target_routes(connection_key,runner_id,environment) values (?,?,?) on conflict(connection_key) do nothing`, route.Key, route.RunnerID, route.Environment); err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, `insert into workspace_guard_operations(id,connection_key,path,state,created_at,updated_at,target_id,valid_until) values (?,?,?,'active',?,?,?,?)`, record.ID, route.Key, path, now, now, journalID, record.ValidUntil); err != nil {
		return nil, err
	}
	retain := func(state string) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := s.db.ExecContext(ctx, `update workspace_guard_operations set state=?,updated_at=? where id=?`, state, time.Now().UTC(), record.ID); err != nil {
			log.Printf("[workspace] 保护记录状态待核对 %s: %s", record.ID, redactAgentText(err.Error()))
		}
	}
	release := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Persist the safe-to-release fact before I/O. A crash or lost ACK can
		// then be retried without guessing whether the ordinary writer exited.
		if _, err := s.db.ExecContext(ctx, `update workspace_guard_operations set state='release_pending',updated_at=? where id=?`, time.Now().UTC(), record.ID); err != nil {
			return err
		}
		if err := call(ctx, "release", token); err != nil {
			retain("release_pending")
			return err
		}
		_, err := s.db.ExecContext(ctx, `delete from workspace_guard_operations where id=?`, record.ID)
		return err
	}
	if err := call(ctx, "begin", record); err != nil {
		// No caller code has run. This is safe even after a lost begin ACK.
		return nil, errors.Join(err, release())
	}
	if len(cliTicket) != 0 && cliTicket[0] != nil {
		*cliTicket[0] = record
		cliTicket[0].JournalID = journalID
		cliTicket[0].helperSHA256 = helperSHA256
	}
	watchChecks := checks
	checks = nil // The successful caller now owns these watches until release.
	var once sync.Once
	return func() {
		once.Do(func() {
			certain := true
			for _, check := range watchChecks {
				if !check() {
					certain = false
				}
			}
			if !certain || sshClient != nil && sshClient.workspaceUncertainty.Load() != sshEpoch || wslUnchanged != nil && !wslUnchanged() {
				retain("unconfirmed")
				log.Printf("[workspace] 跨端目标退出未经确认，保留共享目录保护 %s", record.ID)
				return
			}
			if err := release(); err != nil {
				log.Printf("[workspace] 普通写入已结束，共享保护交还待核对 %s: %s", record.ID, redactAgentText(err.Error()))
			}
		})
	}, nil
}
