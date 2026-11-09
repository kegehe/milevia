package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSupervisionRoleConfigurationPersistsAndRejectsStaleOrRunningUpdates(t *testing.T) {
	s, _, _, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	roles := supervisionRoleConfigs{
		"scheduler":      {AgentID: "claude-code", Model: "sonnet", ReasoningEffort: "low"},
		"implementation": {Model: "opus", ReasoningEffort: "high"},
		"review":         {Model: "sonnet", ReasoningEffort: "medium"},
	}
	if err := s.updateSupervisionRoles(context.Background(), ss.ID, ss.StateVersion, roles); err != nil {
		t.Fatal(err)
	}
	got := supervisionSessionForTest(t, s, ss.ID)
	if got.SchedulerAgentID != "claude-code" || got.roleConfig("implementation").Model != "opus" || got.roleConfig("test").Model != "sonnet" {
		t.Fatalf("roles did not persist: %+v", got)
	}
	if err := s.updateSupervisionRoles(context.Background(), ss.ID, ss.StateVersion, nil); !errors.Is(err, errSupervisionState) {
		t.Fatalf("stale update: %v", err)
	}
	startSupervisionForTest(t, s, ss.ID, now)
	got = supervisionSessionForTest(t, s, ss.ID)
	if err := s.updateSupervisionRoles(context.Background(), ss.ID, got.StateVersion, nil); err == nil {
		t.Fatal("running role update accepted")
	}
	if got := supervisionSessionForTest(t, s, ss.ID).roleConfig("review"); got.ReasoningEffort != "medium" {
		t.Fatalf("running update mutated roles: %+v", got)
	}
}

func TestSupervisionRoleConfigurationRejectsInvalidModelsAndRoles(t *testing.T) {
	for _, roles := range []supervisionRoleConfigs{
		{"test": {Model: "sonnet"}}, {"scheduler": {AgentID: "unknown"}},
		{"scheduler": {Model: "--help"}}, {"review": {Model: "model\ncontrol"}},
		{"review": {ReasoningEffort: "ultra"}},
	} {
		if validateSupervisionRoles(roles) == nil {
			t.Fatalf("invalid configuration accepted: %+v", roles)
		}
	}
}

func TestSupervisionCapabilitiesExposeOnlyProfileSelectorFields(t *testing.T) {
	s, p, _, _, _ := supervisionFixture(t, defaultSupervisionBudget())
	profile := createCLIManagedProfile(t, s, "claude-code", "sonnet")
	response := httptest.NewRecorder()
	s.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/projects/"+p.ID+"/supervision/capabilities", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), profile.ID) {
		t.Fatalf("profile options: %d %s", response.Code, response.Body.String())
	}
	for _, field := range []string{`"env"`, `"baseUrl"`, `"secret"`, `"secretRef"`} {
		if strings.Contains(response.Body.String(), field) {
			t.Fatalf("capabilities expose %s", field)
		}
	}
}

func TestSupervisionRoleConfigurationHTTPUsesNoContentAndVersionConflict(t *testing.T) {
	s, _, _, ss, _ := supervisionFixture(t, defaultSupervisionBudget())
	body := `{"stateVersion":1,"roles":{"scheduler":{"agentId":"claude-code"},"review":{"model":"sonnet"}}}`
	call := func() *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		s.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/api/supervision/"+ss.ID+"/roles", strings.NewReader(body)))
		return response
	}
	if response := call(); response.Code != http.StatusNoContent || response.Body.Len() != 0 {
		t.Fatalf("successful role update must match api() empty-body contract: %d %s", response.Code, response.Body.String())
	}
	if response := call(); response.Code != http.StatusConflict {
		t.Fatalf("stale role update status=%d", response.Code)
	}
	if got := supervisionSessionForTest(t, s, ss.ID); got.StateVersion != 2 || got.roleConfig("review").Model != "sonnet" {
		t.Fatalf("HTTP configuration not persisted: %+v", got)
	}
}

func TestSupervisionWriteAcknowledgementsAreValidJSON(t *testing.T) {
	s, _, taskID, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	if _, err := s.db.Exec(`insert into supervision_questions(id,session_id,task_id,question,created_at) values ('q',?,?,?,?)`, ss.ID, taskID, "需要确认", now); err != nil {
		t.Fatal(err)
	}
	for _, request := range []struct{ path, body string }{
		{"/instructions", `{"stateVersion":1,"content":"保留已有变更"}`},
		{"/questions/q/answer", `{"answer":"按任务内容验收"}`},
	} {
		response := httptest.NewRecorder()
		s.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/supervision/"+ss.ID+request.path, strings.NewReader(request.body)))
		if response.Code != http.StatusAccepted || !json.Valid(response.Body.Bytes()) {
			t.Fatalf("write acknowledgement incompatible with api(): %d %s", response.Code, response.Body.String())
		}
	}
}
