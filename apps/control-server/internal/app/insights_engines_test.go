package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestInsightEnginesUsesProjectRunnerOverHTTPAndRelay(t *testing.T) {
	s := newTestServer(t)
	p := newTaskTransferProject(t, s, "engine-project")
	s.runnerRegistry.register("ssh-engines", &stubRunner{ready: true}, RunnerMeta{ID: "ssh-engines", Name: "remote", Environment: "remote-linux"})
	for _, runnerID := range []string{s.localRunnerID(), "ssh-engines"} {
		if _, err := s.db.Exec(`update projects set runner=? where id=?`, runnerID, p); err != nil {
			t.Fatal(err)
		}
		s.agentReadingsMu.Lock()
		if s.agentReadings == nil {
			s.agentReadings = map[string]agentReading{}
		}
		s.agentReadings[runnerID] = agentReading{at: time.Now(), statuses: []AgentStatus{
			{ID: "codex", Status: agentStatusReady},
			{ID: "claude-code", Status: agentStatusUnavailable, Reason: "未登录"},
		}}
		s.agentReadingsMu.Unlock()
		check := func(data map[string]any) {
			t.Helper()
			runner := data["runner"].(map[string]any)
			if runner["id"] != runnerID || runner["local"] != isLocalRunnerID(runnerID) {
				t.Fatalf("wrong project runner: %v", runner)
			}
			entries := data["entries"].([]any)
			if len(entries) != len(agentCatalog()) {
				t.Fatalf("catalog incomplete: %v", entries)
			}
			agents := runner["agents"].([]any)
			if len(agents) != 2 || agents[0].(map[string]any)["status"] != agentStatusReady || agents[1].(map[string]any)["status"] != agentStatusUnavailable {
				t.Fatalf("readiness changed: %v", agents)
			}
		}
		response := httptest.NewRecorder()
		s.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/projects/"+p+"/insights/engines?runnerID=other", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("HTTP engines: %d %s", response.Code, response.Body)
		}
		var data map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &data); err != nil {
			t.Fatal(err)
		}
		check(data)
		status, reply := relayRPC(t, s, map[string]any{"op": "insights.engines", "projectId": p})
		if status != http.StatusOK || reply["ok"] != true {
			t.Fatalf("relay engines: %d %v", status, reply)
		}
		check(reply["data"].(map[string]any))
	}
	for _, projectID := range []string{"missing", p} {
		if _, err := s.db.Exec(`update projects set runner='missing-runner' where id=?`, p); err != nil {
			t.Fatal(err)
		}
		status, reply := relayRPC(t, s, map[string]any{"op": "insights.engines", "projectId": projectID})
		if status != http.StatusOK || reply["ok"] != false || reply["status"] != float64(http.StatusNotFound) {
			t.Fatalf("unavailable project/runner must fail: %d %v", status, reply)
		}
	}
}
