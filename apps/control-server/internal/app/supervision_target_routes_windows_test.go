//go:build windows

package app

import (
	"context"
	"testing"
)

func TestSupervisionTargetRoutesInstallationUsesActualEnvironment(t *testing.T) {
	s, permit, _ := supervisionTargetControlFixture(t)
	ctx := context.Background()
	// Model a WSL service's two project targets in the persistent run/route
	// records. No Windows or Linux installation is launched by this test.
	if _, err := s.db.Exec(`update runs set agent_runtime_id='wsl-local' where id=?;update supervision_target_controls set runner_id='wsl-local' where session_id=?`, permit.RunID, permit.Decision.SessionID); err != nil {
		t.Fatal(err)
	}
	for runner, path := range map[string]string{"windows-local": `C:\native\cli.exe`, "wsl-local": "/linux/bin/cli"} {
		if err := s.recordAgentInstallation(ctx, agentInstallation{RunnerID: runner, AgentID: permit.AgentID, BinaryPath: path}); err != nil {
			t.Fatal(err)
		}
	}
	for _, environment := range []agentTargetEnv{agentTargetEnvWindows, agentTargetEnvWSL} {
		route := newSupervisionTargetRoute("wsl-local", environment)
		if _, err := s.db.Exec(`insert into supervision_target_routes(connection_key,runner_id,environment) values (?,?,?);update supervision_target_controls set connection_key=? where session_id=?`, route.Key, route.RunnerID, route.Environment, route.Key, permit.Decision.SessionID); err != nil {
			t.Fatal(err)
		}
		expected := "/linux/bin/cli"
		if environment == agentTargetEnvWindows {
			expected = `C:\native\cli.exe`
		}
		if actual, err := supervisionTargetInstallationPath(ctx, s.db, permit); err != nil || actual != expected {
			t.Fatal("frozen execution selected another environment's CLI installation", environment, actual, err)
		}
	}
	if _, err := s.db.Exec(`delete from agent_installations where runner_id='windows-local';update supervision_target_controls set connection_key=? where session_id=?`, newSupervisionTargetRoute("wsl-local", agentTargetEnvWindows).Key, permit.Decision.SessionID); err != nil {
		t.Fatal(err)
	}
	if path, err := supervisionTargetInstallationPath(ctx, s.db, permit); err != nil || path != "" {
		t.Fatal("missing Windows registration reused the Linux installation", path, err)
	}
}
