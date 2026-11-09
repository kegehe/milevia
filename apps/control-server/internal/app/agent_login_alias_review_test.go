package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAgentLoginGuidanceUsesAvailableTargetAlias(t *testing.T) {
	for _, command := range []string{"cursor-agent", "agent"} {
		t.Run(command, func(t *testing.T) {
			server := newTestServer(t)
			shell := newFakeCrossShell()
			shell.outputs[command] = "2026.11.04-build"
			meta := RunnerMeta{ID: "remote-login-alias", Name: "fixture", Environment: "remote-linux"}
			server.runnerRegistry.register(meta.ID, &crossShellOnlyRunner{shell}, meta)
			response := httptest.NewRecorder()
			server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost,
				"/api/runners/"+meta.ID+"/agents/cursor/login", nil))
			var info agentLoginInfo
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &info) != nil {
				t.Fatalf("login response=%d %s", response.Code, response.Body.String())
			}
			if !strings.Contains(info.Message, "运行 "+command+" login 完成登录") {
				t.Fatalf("guidance does not name the available target command %q: %s", command, info.Message)
			}
			if command == "agent" && strings.Contains(info.Message, "cursor-agent") {
				t.Fatalf("guidance names an unavailable command: %s", info.Message)
			}
		})
	}
}
