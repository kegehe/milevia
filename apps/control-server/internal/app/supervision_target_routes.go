package app

import (
	"context"
	"database/sql"
	"errors"
	"runtime"
	"strings"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

// Runner identity is the management namespace, not necessarily the execution
// environment: a WSL runner can address both Linux and mounted Windows projects.
// A route is frozen independently of project paths and contains no credentials.
type supervisionTargetRoute struct {
	Key         string         `json:"key"`
	RunnerID    string         `json:"runnerId"`
	Environment agentTargetEnv `json:"environment"`
}

const supervisionTargetRoutePrefix = "supervision-route-v1:"

func (s *Server) supervisionProjectRoute(p Project) supervisionTargetRoute {
	id := p.RunnerID
	if id == "" {
		id = p.Runner
	}
	if id == "" {
		id = s.localRunnerID()
	}
	return newSupervisionTargetRoute(id, s.resolveAgentTargetEnv(id, p.Path))
}

func newSupervisionTargetRoute(id string, environment agentTargetEnv) supervisionTargetRoute {
	route := supervisionTargetRoute{RunnerID: id, Environment: environment}
	route.Key = supervisionTargetRoutePrefix + supervisionSHA256(mustJSON([]string{id, string(route.Environment)}))
	return route
}

func (route supervisionTargetRoute) local() bool {
	return isLocalRunnerID(route.RunnerID) && ((runtime.GOOS == "windows") == (route.Environment == agentTargetEnvWindows))
}

func (s *Server) migrateSupervisionTargetRoutes(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `create table if not exists supervision_target_routes (
	 connection_key text primary key, runner_id text not null, environment text not null,
	 unique(runner_id,environment)
	)`)
	return err
}

func loadSupervisionTargetRoute(ctx context.Context, q sqlQueryer, key string) (supervisionTargetRoute, error) {
	route := supervisionTargetRoute{Key: key}
	err := q.QueryRowContext(ctx, `select runner_id,environment from supervision_target_routes where connection_key=?`, key).Scan(&route.RunnerID, &route.Environment)
	if err == nil && (route.RunnerID == "" || key != supervisionTargetRoutePrefix+supervisionSHA256(mustJSON([]string{route.RunnerID, string(route.Environment)})) || route.Environment != agentTargetEnvWindows && route.Environment != agentTargetEnvWSL && route.Environment != agentTargetEnvRemote) {
		return route, targetexec.ErrConflict
	}
	return route, err
}

func (s *Server) connectSupervisionProjectTarget(ctx context.Context, p Project) (*supervisionTargetLink, error) {
	route := s.supervisionProjectRoute(p)
	// An immutable routing observation is safe before workspace admission. It
	// grants neither a session nor permission to start a process or change files.
	if _, err := s.db.ExecContext(ctx, `insert into supervision_target_routes(connection_key,runner_id,environment) values (?,?,?) on conflict(connection_key) do nothing`, route.Key, route.RunnerID, route.Environment); err != nil {
		return nil, err
	}
	return s.connectSupervisionTarget(ctx, route.Key)
}

func (s *Server) supervisionTargetRouteRunner(ctx context.Context, key string) (AgentRunner, string, error) {
	if !strings.HasPrefix(key, supervisionTargetRoutePrefix) {
		runner, ok := s.runnerRegistry.get(key)
		if !ok {
			return nil, "", errors.New("项目目标 Runner 尚未连接或注册")
		}
		return runner, key, nil // Existing sessions retain their exact old mapping.
	}
	route, err := loadSupervisionTargetRoute(ctx, s.db, key)
	if err != nil {
		return nil, "", err // A missing routed identity must not fall back locally.
	}
	switch route.Environment {
	case agentTargetEnvRemote:
		runner, ok := s.runnerRegistry.get(route.RunnerID)
		if !ok {
			return nil, "", errors.New("项目目标 Runner 尚未连接或注册")
		}
		if _, ok := runner.(*sshRunner); !ok {
			return nil, "", targetexec.ErrConflict
		}
		return runner, route.RunnerID, nil
	case agentTargetEnvWindows:
		if runtime.GOOS == "windows" {
			// Native sessions use their existing local driver, not a second owner.
			return nil, "", errSupervisionProcessUnsupported
		}
		if !isLocalRunnerID(route.RunnerID) && route.RunnerID != "windows-local" {
			return nil, "", targetexec.ErrConflict
		}
		return s.windowsAgentRunner(), "windows-local", nil
	case agentTargetEnvWSL:
		if runtime.GOOS != "windows" || route.RunnerID != "wsl-local" {
			return nil, "", errSupervisionProcessUnsupported
		}
		runner := s.wslAgentRunner()
		if runner == nil {
			return nil, "", errors.New("项目 WSL 目标尚未连接")
		}
		return runner, "wsl-local", nil
	}
	return nil, "", targetexec.ErrConflict
}

func supervisionTargetConnectionKey(intent supervisionTargetControlIntent) string {
	if intent.ConnectionKey != "" {
		return intent.ConnectionKey
	}
	return intent.RunnerID // Compatibility: never reinterpret an active old session.
}

func supervisionTargetInstallationRunner(route supervisionTargetRoute) string {
	switch route.Environment {
	case agentTargetEnvWindows:
		return "windows-local"
	case agentTargetEnvWSL:
		return "wsl-local"
	default:
		return route.RunnerID
	}
}

func verifySupervisionTargetRoutePlatform(ctx context.Context, q sqlQueryer, key, targetOS string) error {
	if !strings.HasPrefix(key, supervisionTargetRoutePrefix) {
		return nil // The exact platform of a legacy binding was frozen separately.
	}
	route, err := loadSupervisionTargetRoute(ctx, q, key)
	if err != nil {
		return err
	}
	expectedOS := "linux"
	if route.Environment == agentTargetEnvWindows {
		expectedOS = "windows"
	}
	if targetOS != expectedOS {
		return targetexec.ErrConflict
	}
	return nil
}

// Check the routing record against the current project within the same short
// transaction as domain authority. No target I/O is performed here.
func (s *Server) checkSupervisionTargetProjectRoute(ctx context.Context, q sqlQueryer, key string, p Project) (string, error) {
	expected := s.supervisionProjectRoute(p)
	if !strings.HasPrefix(key, supervisionTargetRoutePrefix) {
		if key != expected.RunnerID {
			return "", targetexec.ErrConflict
		}
		return key, nil
	}
	route, err := loadSupervisionTargetRoute(ctx, q, key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", targetexec.ErrConflict
	}
	if err != nil {
		return "", err
	}
	if route != expected {
		return "", targetexec.ErrConflict
	}
	return route.RunnerID, nil
}
