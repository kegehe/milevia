package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func createCLIManagedProfile(t *testing.T, server *Server, agentID, model string) AgentProfile {
	t.Helper()
	response := httptest.NewRecorder()
	body := `{"agentId":"` + agentID + `","name":"Team profile","model":"` + model + `","authMode":"cli_managed","isDefault":true}`
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/runners/"+server.localRunnerID()+"/agent-profiles", strings.NewReader(body)))
	if response.Code != http.StatusCreated {
		t.Fatalf("create profile status=%d body=%s", response.Code, response.Body.String())
	}
	var result struct {
		Profile AgentProfile `json:"profile"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode profile response: %v", err)
	}
	if result.Profile.ID == "" || result.Profile.CurrentRevisionID == "" {
		t.Fatalf("invalid created profile: %#v", result.Profile)
	}
	return result.Profile
}

func TestManagedCLIEnvironmentOverridesServerControlledValues(t *testing.T) {
	profile := &AgentRuntimeProfile{AgentID: "claude-code"}
	environment := managedCLIEnvironment(profile, []string{
		"PATH=/bin",
		"ANTHROPIC_API_KEY=inherited-key",
		"AUTO_CONTROL_URL=http://untrusted.example",
		"AUTO_APPROVAL_TOKEN=old-token",
	}, "AUTO_CONTROL_URL=http://127.0.0.1:4317", "AUTO_APPROVAL_TOKEN=new-token")
	joined := strings.Join(environment, "\n")
	if strings.Contains(joined, "ANTHROPIC_API_KEY=") || strings.Contains(joined, "untrusted.example") || strings.Contains(joined, "old-token") {
		t.Fatalf("managed environment retained an inherited controlled value: %q", joined)
	}
	if !strings.Contains(joined, "AUTO_CONTROL_URL=http://127.0.0.1:4317") || !strings.Contains(joined, "AUTO_APPROVAL_TOKEN=new-token") {
		t.Fatalf("managed environment did not contain controlled additions: %q", joined)
	}
}

func TestCredentialProfileInputsAreRejectedForBothAgents(t *testing.T) {
	server := newTestServer(t)
	for _, agentID := range []string{"claude-code", "codex"} {
		for _, authMode := range []string{"keychain", "env_ref"} {
			t.Run(agentID+"/"+authMode, func(t *testing.T) {
				response := httptest.NewRecorder()
				body := `{"agentId":"` + agentID + `","name":"Credential profile","model":"test-model","baseUrl":"https://api.example.com/v1","authMode":"` + authMode + `","apiKey":"test-key","secretRef":"TEST_KEY"}`
				server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/runners/"+server.localRunnerID()+"/agent-profiles", strings.NewReader(body)))
				if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "only cli_managed") {
					t.Fatalf("credential profile status=%d body=%s", response.Code, response.Body.String())
				}
			})
		}
	}
}

func TestValidateCLIManagedProfileDoesNotRequireExternalConnectivity(t *testing.T) {
	server := newTestServer(t)
	profile := createCLIManagedProfile(t, server, "claude-code", "claude-test")
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/agent-profiles/"+profile.ID+"/validate", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("validate profile status=%d body=%s", response.Code, response.Body.String())
	}
	var result map[string]string
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil || result["structure"] != "ok" || result["credential"] != "not_required" {
		t.Fatalf("validate result=%#v err=%v", result, err)
	}
}

func TestClaudeCLIManagedProfileLaunchHasNoCredentialOrEndpointInjection(t *testing.T) {
	profile := &AgentRuntimeProfile{RevisionID: "9c70a9bd-89f7-4ebd-94fb-809a872f8b", AgentID: "claude-code", Model: "claude-test", AuthMode: "cli_managed"}
	claude := &claudeCLIRunner{config: Config{}}
	environment, args, cleanup, err := claude.profileLaunch(context.Background(), profile, []string{"AUTO_CONTROL_URL=http://127.0.0.1:4317"})
	if err != nil {
		t.Fatalf("prepare Claude profile: %v", err)
	}
	defer cleanup()
	joinedArgs, joinedEnvironment := strings.Join(args, "\n"), strings.Join(environment, "\n")
	for _, forbidden := range []string{"ANTHROPIC_API_KEY=", "ANTHROPIC_BASE_URL=", "apiKeyHelper", "--bare"} {
		if strings.Contains(joinedArgs, forbidden) || strings.Contains(joinedEnvironment, forbidden) {
			t.Fatalf("Claude CLI-managed launch injected %q: args=%q env=%q", forbidden, joinedArgs, joinedEnvironment)
		}
	}
}

func TestManagedProfileSnapshotsRevisionAndRevocationCancelsRun(t *testing.T) {
	server := newTestServer(t)
	profile := createCLIManagedProfile(t, server, "claude-code", "claude-test-model")
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	started := make(chan AgentRunRequest, 1)
	cancelled := make(chan struct{})
	server.runner = runnerFunc(func(ctx context.Context, request AgentRunRequest, _ AgentRunSink) error {
		started <- request
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	})
	created := httptest.NewRecorder()
	server.routes().ServeHTTP(created, httptest.NewRequest(http.MethodPost, "/api/projects/project/conversations?new=true", strings.NewReader(`{"agentId":"claude-code","profileId":"`+profile.ID+`"}`)))
	if created.Code != http.StatusCreated {
		t.Fatalf("create conversation status=%d body=%s", created.Code, created.Body.String())
	}
	var conversation Conversation
	if err := json.NewDecoder(created.Body).Decode(&conversation); err != nil {
		t.Fatalf("decode conversation: %v", err)
	}
	if conversation.AgentProfileRevisionID != profile.CurrentRevisionID {
		t.Fatalf("conversation revision=%q want %q", conversation.AgentProfileRevisionID, profile.CurrentRevisionID)
	}
	message := httptest.NewRecorder()
	server.routes().ServeHTTP(message, httptest.NewRequest(http.MethodPost, "/api/conversations/"+conversation.ID+"/messages", strings.NewReader(`{"content":"run"}`)))
	if message.Code != http.StatusAccepted {
		t.Fatalf("start message status=%d body=%s", message.Code, message.Body.String())
	}
	var accepted struct {
		RunID string `json:"runId"`
	}
	if err := json.NewDecoder(message.Body).Decode(&accepted); err != nil {
		t.Fatalf("decode message response: %v", err)
	}
	select {
	case request := <-started:
		if request.Profile == nil || request.Profile.RevisionID != profile.CurrentRevisionID || request.Profile.Model != "claude-test-model" {
			t.Fatalf("runner profile=%#v", request.Profile)
		}
	case <-time.After(time.Second):
		t.Fatal("runner was not started")
	}
	var runRevisionID string
	if err := server.db.QueryRow(`select agent_profile_revision_id from runs where id=?`, accepted.RunID).Scan(&runRevisionID); err != nil {
		t.Fatalf("read run profile revision: %v", err)
	}
	if runRevisionID != profile.CurrentRevisionID {
		t.Fatalf("run revision=%q want %q", runRevisionID, profile.CurrentRevisionID)
	}
	revoke := httptest.NewRecorder()
	server.routes().ServeHTTP(revoke, httptest.NewRequest(http.MethodPost, "/api/agent-profile-revisions/"+profile.CurrentRevisionID+"/revoke", nil))
	if revoke.Code != http.StatusAccepted {
		t.Fatalf("revoke status=%d body=%s", revoke.Code, revoke.Body.String())
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("revocation did not cancel the admitted run")
	}
	var state string
	if err := server.db.QueryRow(`select state from agent_profile_revisions where id=?`, profile.CurrentRevisionID).Scan(&state); err != nil {
		t.Fatalf("read revision state: %v", err)
	}
	if state != "revoked" {
		t.Fatalf("revision state=%q", state)
	}
}

func TestManagedCLIEnvironmentRemovesInheritedCredentials(t *testing.T) {
	profile := &AgentRuntimeProfile{RevisionID: "revision", AgentID: "codex", AuthMode: "cli_managed"}
	env := managedCLIEnvironment(profile, []string{"PATH=/bin", "ANTHROPIC_API_KEY=secret", "openai_base_url=https://example.test", "SAFE=value"}, "AUTO_CONTROL_URL=http://127.0.0.1")
	joined := strings.Join(env, "\n")
	for _, forbidden := range []string{"ANTHROPIC_API_KEY=", "openai_base_url="} {
		if strings.Contains(strings.ToLower(joined), strings.ToLower(forbidden)) {
			t.Fatalf("managed environment retained %q: %q", forbidden, joined)
		}
	}
	for _, required := range []string{"PATH=/bin", "SAFE=value", "AUTO_CONTROL_URL=http://127.0.0.1"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("managed environment omitted %q: %q", required, joined)
		}
	}
}

func TestProfileSelectionDefaultsToInheritedConfiguration(t *testing.T) {
	server := newTestServer(t)
	tx, err := server.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin inherited transaction: %v", err)
	}
	revisionID, err := server.profileForNewConversationTx(context.Background(), tx, nil, server.localRunnerID(), "claude-code")
	_ = tx.Rollback()
	if err != nil || revisionID != "" {
		t.Fatalf("empty profile selection revision=%q err=%v", revisionID, err)
	}
	profile := createCLIManagedProfile(t, server, "claude-code", "claude-default")
	tx, err = server.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin default transaction: %v", err)
	}
	revisionID, err = server.profileForNewConversationTx(context.Background(), tx, nil, server.localRunnerID(), "claude-code")
	_ = tx.Rollback()
	if err != nil || revisionID != "" {
		t.Fatalf("implicit profile selection revision=%q err=%v", revisionID, err)
	}
	empty := ""
	tx, err = server.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin explicit fallback transaction: %v", err)
	}
	revisionID, err = server.profileForNewConversationTx(context.Background(), tx, &empty, server.localRunnerID(), "claude-code")
	_ = tx.Rollback()
	if err != nil || revisionID != "" {
		t.Fatalf("explicit inherited fallback revision=%q err=%v", revisionID, err)
	}
	tx, err = server.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin explicit profile transaction: %v", err)
	}
	revisionID, err = server.profileForNewConversationTx(context.Background(), tx, &profile.ID, server.localRunnerID(), "claude-code")
	_ = tx.Rollback()
	if err != nil || revisionID != profile.CurrentRevisionID {
		t.Fatalf("explicit profile selection revision=%q err=%v want=%q", revisionID, err, profile.CurrentRevisionID)
	}
	if _, err := server.db.Exec(`update agent_profiles set enabled=0,is_default=0 where id=?`, profile.ID); err != nil {
		t.Fatalf("disable default profile: %v", err)
	}
	tx, err = server.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin fallback transaction: %v", err)
	}
	revisionID, err = server.profileForNewConversationTx(context.Background(), tx, nil, server.localRunnerID(), "claude-code")
	_ = tx.Rollback()
	if err != nil || revisionID != "" {
		t.Fatalf("disabled default fallback revision=%q err=%v", revisionID, err)
	}
}

func TestConversationProfileRequestUsesInheritedConfigurationUnlessExplicitlySelected(t *testing.T) {
	server := newTestServer(t)
	profile := createCLIManagedProfile(t, server, "claude-code", "claude-default")
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	defaultResponse := httptest.NewRecorder()
	server.routes().ServeHTTP(defaultResponse, httptest.NewRequest(http.MethodPost, "/api/projects/project/conversations?new=true", strings.NewReader(`{"agentId":"claude-code"}`)))
	if defaultResponse.Code != http.StatusCreated {
		t.Fatalf("default conversation status=%d body=%s", defaultResponse.Code, defaultResponse.Body.String())
	}
	var defaultConversation Conversation
	if err := json.NewDecoder(defaultResponse.Body).Decode(&defaultConversation); err != nil {
		t.Fatalf("decode default conversation: %v", err)
	}
	if defaultConversation.AgentProfileRevisionID != "" {
		t.Fatalf("implicit conversation unexpectedly bound revision %q", defaultConversation.AgentProfileRevisionID)
	}
	inheritedResponse := httptest.NewRecorder()
	server.routes().ServeHTTP(inheritedResponse, httptest.NewRequest(http.MethodPost, "/api/projects/project/conversations?new=true", strings.NewReader(`{"agentId":"claude-code","profileId":""}`)))
	if inheritedResponse.Code != http.StatusCreated {
		t.Fatalf("inherited conversation status=%d body=%s", inheritedResponse.Code, inheritedResponse.Body.String())
	}
	var inheritedConversation Conversation
	if err := json.NewDecoder(inheritedResponse.Body).Decode(&inheritedConversation); err != nil {
		t.Fatalf("decode inherited conversation: %v", err)
	}
	if inheritedConversation.AgentProfileRevisionID != "" {
		t.Fatalf("inherited conversation unexpectedly bound revision %q", inheritedConversation.AgentProfileRevisionID)
	}
	profileResponse := httptest.NewRecorder()
	server.routes().ServeHTTP(profileResponse, httptest.NewRequest(http.MethodPost, "/api/projects/project/conversations?new=true", strings.NewReader(`{"agentId":"claude-code","profileId":"`+profile.ID+`"}`)))
	if profileResponse.Code != http.StatusCreated {
		t.Fatalf("profile conversation status=%d body=%s", profileResponse.Code, profileResponse.Body.String())
	}
	var profileConversation Conversation
	if err := json.NewDecoder(profileResponse.Body).Decode(&profileConversation); err != nil {
		t.Fatalf("decode profile conversation: %v", err)
	}
	if profileConversation.AgentProfileRevisionID != profile.CurrentRevisionID {
		t.Fatalf("profile conversation revision=%q want=%q", profileConversation.AgentProfileRevisionID, profile.CurrentRevisionID)
	}
	listResponse := httptest.NewRecorder()
	server.routes().ServeHTTP(listResponse, httptest.NewRequest(http.MethodGet, "/api/projects/project/conversations", nil))
	if listResponse.Code != http.StatusOK {
		t.Fatalf("list conversations status=%d body=%s", listResponse.Code, listResponse.Body.String())
	}
	var listed conversationListPage
	if err := json.NewDecoder(listResponse.Body).Decode(&listed); err != nil {
		t.Fatalf("decode conversation list: %v", err)
	}
	if len(listed.Items) == 0 || listed.Items[0].AgentProfileRevisionID != profile.CurrentRevisionID {
		t.Fatalf("listed profile revision=%q want=%q", listed.Items[0].AgentProfileRevisionID, profile.CurrentRevisionID)
	}
	detailResponse := httptest.NewRecorder()
	server.routes().ServeHTTP(detailResponse, httptest.NewRequest(http.MethodGet, "/api/conversations/"+profileConversation.ID, nil))
	if detailResponse.Code != http.StatusOK {
		t.Fatalf("get conversation status=%d body=%s", detailResponse.Code, detailResponse.Body.String())
	}
	var detailed struct {
		Conversation Conversation `json:"conversation"`
	}
	if err := json.NewDecoder(detailResponse.Body).Decode(&detailed); err != nil {
		t.Fatalf("decode conversation detail: %v", err)
	}
	if detailed.Conversation.AgentProfileRevisionID != profile.CurrentRevisionID {
		t.Fatalf("detailed profile revision=%q want=%q", detailed.Conversation.AgentProfileRevisionID, profile.CurrentRevisionID)
	}
}

func TestLegacyCredentialRevisionIsRejectedForSelectionAndRuntime(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into agent_profiles (id,runner_id,agent_id,name,current_revision_id,enabled,is_default,created_at,updated_at) values ('legacy-credential',?,'claude-code','Legacy credential profile','legacy-credential-revision',1,0,?,?)`, server.localRunnerID(), now, now); err != nil {
		t.Fatalf("insert legacy profile: %v", err)
	}
	if _, err := server.db.Exec(`insert into agent_profile_revisions (id,profile_id,revision,base_url,model,protocol,auth_mode,secret_ref,state,execution_mode,created_at) values ('legacy-credential-revision','legacy-credential',1,'https://api.example.com/v1','claude-test','claude_cli','keychain','keychain:legacy','active','isolated',?)`, now); err != nil {
		t.Fatalf("insert legacy revision: %v", err)
	}
	tx, err := server.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin runtime transaction: %v", err)
	}
	_, err = server.runtimeProfileTx(context.Background(), tx, "legacy-credential-revision", server.localRunnerID(), "claude-code")
	_ = tx.Rollback()
	if err == nil {
		t.Fatalf("legacy credential revision was admitted at runtime: %v", err)
	}
	requested := "legacy-credential"
	tx, err = server.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin selection transaction: %v", err)
	}
	_, err = server.profileForNewConversationTx(context.Background(), tx, &requested, server.localRunnerID(), "claude-code")
	_ = tx.Rollback()
	if err == nil {
		t.Fatal("legacy credential revision was admitted for a new conversation")
	}
	migrate := httptest.NewRecorder()
	server.routes().ServeHTTP(migrate, httptest.NewRequest(http.MethodPatch, "/api/agent-profiles/legacy-credential", strings.NewReader(`{"authMode":"cli_managed"}`)))
	if migrate.Code != http.StatusOK {
		t.Fatalf("migrate legacy profile status=%d body=%s", migrate.Code, migrate.Body.String())
	}
	var migrated struct {
		Profile  AgentProfile         `json:"profile"`
		Revision AgentProfileRevision `json:"revision"`
	}
	if err := json.NewDecoder(migrate.Body).Decode(&migrated); err != nil {
		t.Fatalf("decode migrated profile: %v", err)
	}
	if migrated.Revision.AuthMode != "cli_managed" || migrated.Revision.BaseURL != "" || migrated.Revision.SecretRef != "" || migrated.Revision.Revision != 2 {
		t.Fatalf("unsafe migrated revision: %#v", migrated.Revision)
	}
	tx, err = server.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin migrated runtime transaction: %v", err)
	}
	runtimeProfile, err := server.runtimeProfileTx(context.Background(), tx, migrated.Profile.CurrentRevisionID, server.localRunnerID(), "claude-code")
	_ = tx.Rollback()
	if err != nil || runtimeProfile == nil || runtimeProfile.BaseURL != "" || runtimeProfile.AuthMode != "cli_managed" {
		t.Fatalf("migrated runtime profile=%#v err=%v", runtimeProfile, err)
	}
}

func TestLegacyCodexCredentialRevisionIsRejectedAtRuntime(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into agent_profiles (id,runner_id,agent_id,name,current_revision_id,enabled,is_default,created_at,updated_at) values ('legacy-codex-credential',?,'codex','Legacy Codex credential','legacy-codex-credential-revision',1,0,?,?)`, server.localRunnerID(), now, now); err != nil {
		t.Fatalf("insert legacy Codex profile: %v", err)
	}
	if _, err := server.db.Exec(`insert into agent_profile_revisions (id,profile_id,revision,base_url,model,protocol,auth_mode,secret_ref,state,execution_mode,created_at) values ('legacy-codex-credential-revision','legacy-codex-credential',1,'https://api.example.com/v1','gpt-test','codex_cli','env_ref','env:TEST_KEY#1','active','isolated',?)`, now); err != nil {
		t.Fatalf("insert legacy Codex revision: %v", err)
	}
	tx, err := server.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin runtime transaction: %v", err)
	}
	_, err = server.runtimeProfileTx(context.Background(), tx, "legacy-codex-credential-revision", server.localRunnerID(), "codex")
	_ = tx.Rollback()
	if err == nil {
		t.Fatal("legacy Codex credential revision was admitted at runtime")
	}
}

func TestRemoteProfilesCannotBeDisabledOrRevokedLocally(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into runners (id,kind,display_name,config_json,enabled,last_error,created_at,updated_at) values ('ssh-example','ssh','Example SSH','{}',1,'',?,?)`, now, now); err != nil {
		t.Fatalf("insert remote runner: %v", err)
	}
	if _, err := server.db.Exec(`insert into agent_profiles (id,runner_id,agent_id,name,current_revision_id,enabled,is_default,created_at,updated_at) values ('remote-profile','ssh-example','claude-code','Remote profile','remote-revision',1,0,?,?)`, now, now); err != nil {
		t.Fatalf("insert remote profile: %v", err)
	}
	if _, err := server.db.Exec(`insert into agent_profile_revisions (id,profile_id,revision,base_url,model,protocol,auth_mode,secret_ref,state,execution_mode,created_at) values ('remote-revision','remote-profile',1,'','','claude_cli','cli_managed','','active','isolated',?)`, now); err != nil {
		t.Fatalf("insert remote revision: %v", err)
	}
	disable := httptest.NewRecorder()
	server.routes().ServeHTTP(disable, httptest.NewRequest(http.MethodPost, "/api/agent-profiles/remote-profile/disable", nil))
	if disable.Code != http.StatusConflict {
		t.Fatalf("disable remote profile status=%d body=%s", disable.Code, disable.Body.String())
	}
	revoke := httptest.NewRecorder()
	server.routes().ServeHTTP(revoke, httptest.NewRequest(http.MethodPost, "/api/agent-profile-revisions/remote-revision/revoke", nil))
	if revoke.Code != http.StatusConflict {
		t.Fatalf("revoke remote profile status=%d body=%s", revoke.Code, revoke.Body.String())
	}
	var enabled int
	var state string
	if err := server.db.QueryRow(`select enabled from agent_profiles where id='remote-profile'`).Scan(&enabled); err != nil {
		t.Fatalf("read remote profile: %v", err)
	}
	if err := server.db.QueryRow(`select state from agent_profile_revisions where id='remote-revision'`).Scan(&state); err != nil {
		t.Fatalf("read remote revision: %v", err)
	}
	if enabled != 1 || state != "active" {
		t.Fatalf("remote profile was mutated: enabled=%d state=%q", enabled, state)
	}
}

func TestActiveProfileCanBeEnabledAfterItIsDisabled(t *testing.T) {
	server := newTestServer(t)
	profile := createCLIManagedProfile(t, server, "claude-code", "")
	disable := httptest.NewRecorder()
	server.routes().ServeHTTP(disable, httptest.NewRequest(http.MethodPost, "/api/agent-profiles/"+profile.ID+"/disable", nil))
	if disable.Code != http.StatusNoContent {
		t.Fatalf("disable profile status=%d body=%s", disable.Code, disable.Body.String())
	}
	enable := httptest.NewRecorder()
	server.routes().ServeHTTP(enable, httptest.NewRequest(http.MethodPost, "/api/agent-profiles/"+profile.ID+"/enable", nil))
	if enable.Code != http.StatusNoContent {
		t.Fatalf("enable profile status=%d body=%s", enable.Code, enable.Body.String())
	}
	var enabled int
	if err := server.db.QueryRow(`select enabled from agent_profiles where id=?`, profile.ID).Scan(&enabled); err != nil {
		t.Fatalf("read enabled profile: %v", err)
	}
	if enabled != 1 {
		t.Fatalf("profile remained disabled: %d", enabled)
	}
}

func TestProfileDefaultsAreRetiredAndDoNotChangeConversationFallback(t *testing.T) {
	server := newTestServer(t)
	first := createCLIManagedProfile(t, server, "codex", "gpt-first")
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/runners/"+server.localRunnerID()+"/agent-profiles", strings.NewReader(`{"agentId":"codex","name":"Second profile","model":"gpt-second","authMode":"cli_managed","isDefault":true}`)))
	if response.Code != http.StatusCreated {
		t.Fatalf("second default profile status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), `"isDefault"`) {
		t.Fatalf("retired default marker leaked in response: %s", response.Body.String())
	}
	var firstDefault, defaultCount int
	if err := server.db.QueryRow(`select is_default from agent_profiles where id=?`, first.ID).Scan(&firstDefault); err != nil {
		t.Fatalf("read first default: %v", err)
	}
	if err := server.db.QueryRow(`select count(*) from agent_profiles where runner_id=? and agent_id='codex' and enabled=1 and is_default=1`, server.localRunnerID()).Scan(&defaultCount); err != nil {
		t.Fatalf("count defaults: %v", err)
	}
	if firstDefault != 0 || defaultCount != 0 {
		t.Fatalf("defaults first=%d total=%d", firstDefault, defaultCount)
	}
}

func TestUpdatingProfileModelCreatesImmutableRevision(t *testing.T) {
	server := newTestServer(t)
	profile := createCLIManagedProfile(t, server, "codex", "gpt-first")
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/api/agent-profiles/"+profile.ID, strings.NewReader(`{"model":"gpt-second"}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("update profile status=%d body=%s", response.Code, response.Body.String())
	}
	var result struct {
		Profile  AgentProfile         `json:"profile"`
		Revision AgentProfileRevision `json:"revision"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode update response: %v", err)
	}
	if result.Revision.Revision != 2 || result.Revision.Model != "gpt-second" || result.Profile.CurrentRevisionID != result.Revision.ID {
		t.Fatalf("updated profile=%#v revision=%#v", result.Profile, result.Revision)
	}
	var firstModel, secondModel string
	if err := server.db.QueryRow(`select model from agent_profile_revisions where profile_id=? and revision=1`, profile.ID).Scan(&firstModel); err != nil {
		t.Fatalf("read first revision: %v", err)
	}
	if err := server.db.QueryRow(`select model from agent_profile_revisions where profile_id=? and revision=2`, profile.ID).Scan(&secondModel); err != nil {
		t.Fatalf("read second revision: %v", err)
	}
	if firstModel != "gpt-first" || secondModel != "gpt-second" {
		t.Fatalf("revision models: first=%q second=%q", firstModel, secondModel)
	}
	if _, err := server.db.Exec(`update agent_profile_revisions set model='tampered' where id=?`, result.Revision.ID); err == nil {
		t.Fatal("immutable revision accepted a model update")
	}
}
