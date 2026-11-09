package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"testing"
)

func TestSupervisionCatalogDrivesRoleValidationAndDrafts(t *testing.T) {
	for _, entry := range agentCatalog() {
		t.Run(entry.ID, func(t *testing.T) {
			s, p, taskID, _, _ := supervisionFixture(t, defaultSupervisionBudget())
			roles := supervisionRoleConfigs{"scheduler": {AgentID: entry.ID}, "implementation": {AgentID: entry.ID}, "review": {AgentID: entry.ID}}
			if err := validateSupervisionRoles(roles); err != nil {
				t.Fatal(err)
			}
			ss, err := s.createSupervisionDraft(context.Background(), p.ID, []string{taskID}, entry.ID, defaultSupervisionBudget(), roles)
			if err != nil || ss.SchedulerAgentID != entry.ID || ss.roleConfig("review").AgentID != entry.ID {
				t.Fatalf("catalog draft: %+v %v", ss, err)
			}
			assertExecutionClaimForTest(t, s, taskID, "", "")
			if !entry.SupportsProfiles {
				if err := validateSupervisionRoles(supervisionRoleConfigs{"review": {ProfileID: "not-supported"}}, entry.ID); err == nil {
					t.Fatal("CLI-managed tool accepted a platform profile")
				}
			}
			if len(supervisionReasoningOptions(entry.ID)) == 0 && validateSupervisionRoles(supervisionRoleConfigs{"review": {ReasoningEffort: "high"}}, entry.ID) == nil {
				t.Fatal("tool inherited another CLI's effort options")
			}
		})
	}
}

func TestSupervisionCapabilitiesExposeWholeCatalogAndTarget(t *testing.T) {
	s, p, taskID, _, _ := supervisionFixture(t, defaultSupervisionBudget())
	response := httptest.NewRecorder()
	s.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/projects/"+p.ID+"/supervision/capabilities", nil))
	var body struct {
		Agents []supervisionCapability `json:"agents"`
		Target supervisionTarget       `json:"target"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != http.StatusOK {
		t.Fatalf("capabilities: %d %s %v", response.Code, response.Body.String(), err)
	}
	if len(body.Agents) != len(agentCatalog()) || body.Target.RunnerID != s.localRunnerID() || !body.Target.Local {
		t.Fatalf("catalog/target missing: %+v", body)
	}
	for i, entry := range agentCatalog() {
		capability := body.Agents[i]
		if capability.AgentID != entry.ID || capability.Name != entry.Name || capability.State == "" || capability.SupportsProfiles != entry.SupportsProfiles || !reflect.DeepEqual(capability.ReasoningEfforts, supervisionReasoningOptions(entry.ID)) {
			t.Fatalf("catalog facts diverged: %+v %+v", entry, capability)
		}
		if capability.State == "adapter_pending" && (capability.Available || capability.Reason == "") {
			t.Fatalf("missing adapter advertised as runnable: %+v", capability)
		}
	}
	assertExecutionClaimForTest(t, s, taskID, "", "")
}

func TestSupervisionDefaultAgentFollowsPreferencesWithoutSilentFallback(t *testing.T) {
	s, p, taskID, _, _ := supervisionFixture(t, defaultSupervisionBudget())
	if _, err := s.db.Exec(`update app_preferences set default_agent_id='codebuddy' where id=1`); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	s.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/projects/"+p.ID+"/supervision/capabilities", nil))
	var capabilities struct {
		DefaultAgentID string `json:"defaultAgentId"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &capabilities); err != nil || capabilities.DefaultAgentID != "codebuddy" {
		t.Fatalf("default silently replaced: %s %v", response.Body.String(), err)
	}
	response = httptest.NewRecorder()
	s.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/"+p.ID+"/supervision", jsonBody(t, map[string]any{"taskIds": []string{taskID}})))
	var draft supervisionSession
	if err := json.Unmarshal(response.Body.Bytes(), &draft); err != nil || response.Code != http.StatusCreated || draft.SchedulerAgentID != "codebuddy" {
		t.Fatalf("draft did not use configured CLI: %d %s %v", response.Code, response.Body.String(), err)
	}
}

func TestSupervisionTargetNeverFallsBackToControlHost(t *testing.T) {
	s, _, _, _, _ := supervisionFixture(t, defaultSupervisionBudget())
	s.runnerRegistry.register("ssh-supervision-test", s.runner, RunnerMeta{ID: "ssh-supervision-test", Environment: "remote-linux"})
	remote := Project{Runner: "ssh-supervision-test", Path: "/srv/example"}
	for _, entry := range agentCatalog() {
		capability := s.supervisionCapabilityForProject(context.Background(), remote, entry.ID, true)
		if capability.Available || capability.State != "capability_unavailable" || capability.Reason == "" {
			t.Fatalf("remote used local capabilities: %+v", capability)
		}
	}
	missing := s.supervisionProjectTarget(Project{Runner: "ssh-missing", Path: "/srv/example"})
	if missing.Local || missing.State != "environment_unavailable" {
		t.Fatalf("missing remote treated as local: %+v", missing)
	}
	bridge := Project{Runner: "wsl-local", Path: `\\wsl.localhost\Ubuntu\home\test`}
	if runtime.GOOS != "windows" {
		// 即使 Runner ID 是服务端本机，/mnt/c 项目仍按已有路由使用 Windows CLI。
		bridge = Project{Runner: s.localRunnerID(), Path: "/mnt/c/example"}
	}
	if target := s.supervisionProjectTarget(bridge); target.Local || target.Environment == s.supervisionProjectTarget(Project{Runner: s.localRunnerID()}).Environment {
		t.Fatalf("cross-environment project fell back to control host: %+v", target)
	}
}

func TestSupervisionCLIManagedToolsFreezeRoutesWithoutPlatformProfiles(t *testing.T) {
	for _, entry := range agentCatalog() {
		if entry.SupportsProfiles {
			continue
		}
		t.Run(entry.ID, func(t *testing.T) {
			s, p, taskID, _, now := supervisionFixture(t, defaultSupervisionBudget())
			ss, err := s.createSupervisionDraft(context.Background(), p.ID, []string{taskID}, entry.ID, defaultSupervisionBudget())
			if err != nil {
				t.Fatal(err)
			}
			if err := s.startSupervisionSession(context.Background(), ss.ID, ss.StateVersion, now); err != nil {
				t.Fatal(err)
			}
			ss = supervisionSessionForTest(t, s, ss.ID)
			var routes []map[string]string
			if err := json.Unmarshal(ss.RouteSnapshot, &routes); err != nil || len(routes) == 0 {
				t.Fatalf("routes: %s %v", ss.RouteSnapshot, err)
			}
			for _, route := range routes {
				if route["agentId"] != entry.ID || route["mode"] != "cli_managed" || route["profileRevisionId"] != "" || route["routeRevisionId"] != "" {
					t.Fatalf("invented profile or substituted tool: %+v", route)
				}
			}
		})
	}
}

type supervisionAdapterForTest struct{ AgentRunner }

func (*supervisionAdapterForTest) supervisionCLIInfo(context.Context, bool) (supervisionCLIInfo, error) {
	return supervisionCLIInfo{}, nil
}
func (*supervisionAdapterForTest) runSupervisionScheduler(context.Context, supervisionSchedulerCLIRequest, AgentRunSink) (supervisionSchedulerCLIReceipt, error) {
	return supervisionSchedulerCLIReceipt{}, nil
}

func TestSupervisionAdapterSelectionUsesCatalogBackendAndProtocol(t *testing.T) {
	s, _, _, _, _ := supervisionFixture(t, defaultSupervisionBudget())
	s.runner = unavailableRunner{}
	adapter := &supervisionAdapterForTest{AgentRunner: s.runner}
	s.codebuddyRunner = adapter
	if got, err := s.supervisionAdapter("codebuddy"); err != nil || got != adapter {
		t.Fatalf("did not select tool backend: %v %v", got, err)
	}
	if _, err := s.supervisionAdapter("claude-code"); err == nil {
		t.Fatal("ordinary runner was accepted as a supervision adapter")
	}
	s.runner = adapter
	if got, err := s.supervisionAdapter("claude-code"); err != nil || got != adapter {
		t.Fatalf("concrete Claude type still required: %v %v", got, err)
	}
	if _, err := s.supervisionAdapter("not-in-catalog"); err == nil {
		t.Fatal("unknown tool resolved")
	}
}
