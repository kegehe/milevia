package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestLegacyCredentialModesAreRejectedButAPIKeyIsSupported(t *testing.T) {
	server := newTestServer(t)
	// keychain/env_ref legacy modes remain rejected.
	for _, agentID := range []string{"claude-code", "codex"} {
		for _, authMode := range []string{"keychain", "env_ref"} {
			t.Run(agentID+"/"+authMode, func(t *testing.T) {
				response := httptest.NewRecorder()
				body := `{"agentId":"` + agentID + `","name":"Credential profile","model":"test-model","baseUrl":"https://api.example.com/v1","authMode":"` + authMode + `","apiKey":"test-key","secretRef":"TEST_KEY"}`
				server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/runners/"+server.localRunnerID()+"/agent-profiles", strings.NewReader(body)))
				if response.Code != http.StatusBadRequest {
					t.Fatalf("legacy credential mode status=%d body=%s", response.Code, response.Body.String())
				}
			})
		}
	}
	// api_key profiles with a managed key and endpoint are accepted.
	for _, agentID := range []string{"claude-code", "codex"} {
		t.Run(agentID+"/api_key", func(t *testing.T) {
			response := httptest.NewRecorder()
			body := `{"agentId":"` + agentID + `","name":"Managed key profile","model":"test-model","baseUrl":"https://api.example.com/v1","authMode":"api_key","apiKey":"sk-test-managed-key"}`
			server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/runners/"+server.localRunnerID()+"/agent-profiles", strings.NewReader(body)))
			if response.Code != http.StatusCreated {
				t.Fatalf("api_key profile status=%d body=%s", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "sk-test-managed-key") {
				t.Fatalf("plaintext key leaked in response: %s", response.Body.String())
			}
		})
	}
	// A base URL that is not an HTTP(S) endpoint is rejected.
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/runners/"+server.localRunnerID()+"/agent-profiles", strings.NewReader(`{"agentId":"claude-code","name":"Bad endpoint","authMode":"api_key","baseUrl":"not-a-url","apiKey":"sk-x"}`)))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid base URL status=%d body=%s", response.Code, response.Body.String())
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

func TestAPIKeyManagedProfileRoundTripAndLaunch(t *testing.T) {
	server := newTestServer(t)
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/runners/"+server.localRunnerID()+"/agent-profiles", strings.NewReader(`{"agentId":"claude-code","name":"Managed key profile","model":"claude-test","baseUrl":"https://api.example.com/v1","authMode":"api_key","apiKey":"sk-super-secret-value","env":{"ANTHROPIC_VERSION":"2023-06-01","X_CUSTOM":"1"}}`)))
	if response.Code != http.StatusCreated {
		t.Fatalf("create api_key profile status=%d body=%s", response.Code, response.Body.String())
	}
	var result struct {
		Profile  AgentProfile         `json:"profile"`
		Revision AgentProfileRevision `json:"revision"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode api_key profile response: %v", err)
	}
	if strings.Contains(response.Body.String(), "sk-super-secret-value") {
		t.Fatalf("plaintext key leaked in response: %s", response.Body.String())
	}
	// Runtime admission resolves the encrypted secret back to the plaintext key.
	tx, beginErr := server.db.BeginTx(context.Background(), nil)
	if beginErr != nil {
		t.Fatalf("begin tx: %v", beginErr)
	}
	runtimeProfile, err := server.runtimeProfileTx(context.Background(), tx, result.Revision.ID, server.localRunnerID(), "claude-code")
	_ = tx.Rollback()
	if err != nil || runtimeProfile == nil {
		t.Fatalf("runtime profile err=%v", err)
	}
	if runtimeProfile.Secret != "sk-super-secret-value" || runtimeProfile.BaseURL != "https://api.example.com/v1" || runtimeProfile.AuthMode != "api_key" {
		t.Fatalf("runtime profile=%#v", runtimeProfile)
	}
	if runtimeProfile.Env["ANTHROPIC_VERSION"] != "2023-06-01" || runtimeProfile.Env["X_CUSTOM"] != "1" {
		t.Fatalf("runtime profile env=%#v", runtimeProfile.Env)
	}
	// Launch injects the managed key and endpoint into the CLI environment.
	environment, _, cleanup, err := (&claudeCLIRunner{config: Config{}}).profileLaunch(context.Background(), runtimeProfile, []string{})
	if err != nil {
		t.Fatalf("launch api_key profile: %v", err)
	}
	defer cleanup()
	joined := strings.Join(environment, "\n")
	if !strings.Contains(joined, "ANTHROPIC_API_KEY=sk-super-secret-value") {
		t.Fatalf("launch did not inject API key: %q", joined)
	}
	if !strings.Contains(joined, "ANTHROPIC_BASE_URL=https://api.example.com/v1") {
		t.Fatalf("launch did not inject base URL: %q", joined)
	}
	if strings.Contains(joined, "ANTHROPIC_MODEL=") {
		t.Fatalf("launch unexpectedly injected ANTHROPIC_MODEL: %q", joined)
	}
}

func TestAPIKeyRotationMintsIndependentSecretPerRevision(t *testing.T) {
	server := newTestServer(t)
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/runners/"+server.localRunnerID()+"/agent-profiles", strings.NewReader(`{"agentId":"claude-code","name":"Key profile","model":"m1","authMode":"api_key","apiKey":"key-one","env":{"X":"1"}}`)))
	if response.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	var created struct {
		Profile  AgentProfile         `json:"profile"`
		Revision AgentProfileRevision `json:"revision"`
	}
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if created.Revision.Environment["X"] != "1" {
		t.Fatalf("revision did not persist env: %#v", created.Revision.Environment)
	}
	secretForRevision := func(revisionID string) string {
		var ref string
		if err := server.db.QueryRow(`select secret_ref from agent_profile_revisions where id=?`, revisionID).Scan(&ref); err != nil {
			t.Fatalf("read secret ref: %v", err)
		}
		return ref
	}
	firstSecret := secretForRevision(created.Revision.ID)
	// Edit model (no new key): the same secret row is reused and env preserved.
	update := httptest.NewRecorder()
	server.routes().ServeHTTP(update, httptest.NewRequest(http.MethodPatch, "/api/agent-profiles/"+created.Profile.ID, strings.NewReader(`{"model":"m2"}`)))
	if update.Code != http.StatusOK {
		t.Fatalf("update model status=%d body=%s", update.Code, update.Body.String())
	}
	var updated struct {
		Profile  AgentProfile         `json:"profile"`
		Revision AgentProfileRevision `json:"revision"`
	}
	if err := json.NewDecoder(update.Body).Decode(&updated); err != nil {
		t.Fatalf("decode update: %v", err)
	}
	if updatedSecret := secretForRevision(updated.Revision.ID); updatedSecret != firstSecret {
		t.Fatalf("model-only edit rotated secret: old=%q new=%q", firstSecret, updatedSecret)
	}
	// Rotating the key mints a fresh secret; the old revision keeps its own.
	rotate := httptest.NewRecorder()
	server.routes().ServeHTTP(rotate, httptest.NewRequest(http.MethodPatch, "/api/agent-profiles/"+created.Profile.ID, strings.NewReader(`{"apiKey":"key-two"}`)))
	if rotate.Code != http.StatusOK {
		t.Fatalf("rotate status=%d body=%s", rotate.Code, rotate.Body.String())
	}
	var rotated struct {
		Revision AgentProfileRevision `json:"revision"`
	}
	if err := json.NewDecoder(rotate.Body).Decode(&rotated); err != nil {
		t.Fatalf("decode rotate: %v", err)
	}
	if rotatedSecret := secretForRevision(rotated.Revision.ID); rotatedSecret == "" || rotatedSecret == firstSecret {
		t.Fatalf("rotation did not mint an independent secret: first=%q new=%q", firstSecret, rotatedSecret)
	}
	// The old secret row still decrypts to the first key (preserved for the
	// immutable earlier revision).
	tx, err := server.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	oldKey, err := server.profileSecrets.Load(tx, context.Background(), firstSecret)
	_ = tx.Rollback()
	if err != nil || oldKey != "key-one" {
		t.Fatalf("old revision secret lost: key=%q err=%v", oldKey, err)
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
	revisionID, err := server.profileForNewConversationTx(context.Background(), tx, nil, server.localRunnerID(), "claude-code", "")
	_ = tx.Rollback()
	if err != nil || revisionID != "" {
		t.Fatalf("empty profile selection revision=%q err=%v", revisionID, err)
	}
	profile := createCLIManagedProfile(t, server, "claude-code", "claude-default")
	tx, err = server.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin default transaction: %v", err)
	}
	revisionID, err = server.profileForNewConversationTx(context.Background(), tx, nil, server.localRunnerID(), "claude-code", "")
	_ = tx.Rollback()
	if err != nil || revisionID != "" {
		t.Fatalf("implicit profile selection revision=%q err=%v", revisionID, err)
	}
	empty := ""
	tx, err = server.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin explicit fallback transaction: %v", err)
	}
	revisionID, err = server.profileForNewConversationTx(context.Background(), tx, &empty, server.localRunnerID(), "claude-code", "")
	_ = tx.Rollback()
	if err != nil || revisionID != "" {
		t.Fatalf("explicit inherited fallback revision=%q err=%v", revisionID, err)
	}
	tx, err = server.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin explicit profile transaction: %v", err)
	}
	revisionID, err = server.profileForNewConversationTx(context.Background(), tx, &profile.ID, server.localRunnerID(), "claude-code", "")
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
	revisionID, err = server.profileForNewConversationTx(context.Background(), tx, nil, server.localRunnerID(), "claude-code", "")
	_ = tx.Rollback()
	if err != nil || revisionID != "" {
		t.Fatalf("disabled default fallback revision=%q err=%v", revisionID, err)
	}
}

func TestProjectDefaultProfileAppliesToNewConversations(t *testing.T) {
	server := newTestServer(t)
	profile := createCLIManagedProfile(t, server, "claude-code", "claude-default")
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	// Set the project default through the one-click endpoint (non-empty path).
	set := httptest.NewRecorder()
	server.routes().ServeHTTP(set, httptest.NewRequest(http.MethodPatch, "/api/projects/project/agent-profile", strings.NewReader(`{"profileId":"`+profile.ID+`"}`)))
	if set.Code != http.StatusOK {
		t.Fatalf("set default status=%d body=%s", set.Code, set.Body.String())
	}
	// A new conversation with no explicit profile inherits the project default.
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/project/conversations?new=true", strings.NewReader(`{"agentId":"claude-code"}`)))
	if response.Code != http.StatusCreated {
		t.Fatalf("conversation status=%d body=%s", response.Code, response.Body.String())
	}
	var conversation Conversation
	if err := json.NewDecoder(response.Body).Decode(&conversation); err != nil {
		t.Fatalf("decode conversation: %v", err)
	}
	if conversation.AgentProfileRevisionID != profile.CurrentRevisionID {
		t.Fatalf("project default revision=%q want=%q", conversation.AgentProfileRevisionID, profile.CurrentRevisionID)
	}
	// The endpoint reports the configured default.
	detail := httptest.NewRecorder()
	server.routes().ServeHTTP(detail, httptest.NewRequest(http.MethodGet, "/api/projects/project", nil))
	if detail.Code != http.StatusOK {
		t.Fatalf("get project status=%d body=%s", detail.Code, detail.Body.String())
	}
	var project struct {
		DefaultProfileID string `json:"defaultProfileId"`
	}
	if err := json.NewDecoder(detail.Body).Decode(&project); err != nil {
		t.Fatalf("decode project: %v", err)
	}
	if project.DefaultProfileID != profile.ID {
		t.Fatalf("project default=%q want=%q", project.DefaultProfileID, profile.ID)
	}
	// Clearing the default can be done via the dedicated endpoint.
	clear := httptest.NewRecorder()
	server.routes().ServeHTTP(clear, httptest.NewRequest(http.MethodPatch, "/api/projects/project/agent-profile", strings.NewReader(`{"profileId":""}`)))
	if clear.Code != http.StatusOK {
		t.Fatalf("clear default status=%d body=%s", clear.Code, clear.Body.String())
	}
	tx, beginErr := server.db.BeginTx(context.Background(), nil)
	if beginErr != nil {
		t.Fatalf("begin tx: %v", beginErr)
	}
	empty := ""
	revisionID, err := server.profileForNewConversationTx(context.Background(), tx, &empty, server.localRunnerID(), "claude-code", "project")
	_ = tx.Rollback()
	if err != nil || revisionID != "" {
		t.Fatalf("cleared default revision=%q err=%v", revisionID, err)
	}
}

func TestCodexApiKeyProfileBypassesLoginReadiness(t *testing.T) {
	server := newTestServer(t)
	// A fake codex binary that exists but is NOT logged in (login status fails).
	bin := filepath.Join(t.TempDir(), "fake-codex")
	script := "#!/bin/sh\nif [ \"$1\" = \"login\" ]; then exit 1; fi\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}
	server.codexRunner = &codexCLIRunner{config: Config{CodexPath: bin}}
	// cli_managed still requires a login.
	if profile := (*AgentRuntimeProfile)(nil); server.codexUsableForProfile(context.Background(), profile) {
		t.Fatal("codex reported usable without login for cli_managed")
	}
	// An api_key profile injects its own key, so binary presence suffices.
	apiKeyProfile := &AgentRuntimeProfile{AgentID: "codex", AuthMode: "api_key", Secret: "sk-x"}
	if !server.codexUsableForProfile(context.Background(), apiKeyProfile) {
		t.Fatal("codex reported unusable for api_key profile despite binary present")
	}
}

func TestAPIKeyProfileAsProjectDefaultBindsAndAdmitsAtRuntime(t *testing.T) {
	server := newTestServer(t)
	created := httptest.NewRecorder()
	server.routes().ServeHTTP(created, httptest.NewRequest(http.MethodPost, "/api/runners/"+server.localRunnerID()+"/agent-profiles", strings.NewReader(`{"agentId":"codex","name":"Key default","model":"gpt-test","baseUrl":"https://api.example.com/v1","authMode":"api_key","apiKey":"sk-codex-key"}`)))
	if created.Code != http.StatusCreated {
		t.Fatalf("create codex api_key status=%d body=%s", created.Code, created.Body.String())
	}
	var result struct {
		Profile  AgentProfile         `json:"profile"`
		Revision AgentProfileRevision `json:"revision"`
	}
	if err := json.NewDecoder(created.Body).Decode(&result); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project-key','project-key',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	// One-click: make the api_key profile the project default.
	set := httptest.NewRecorder()
	server.routes().ServeHTTP(set, httptest.NewRequest(http.MethodPatch, "/api/projects/project-key/agent-profile", strings.NewReader(`{"profileId":"`+result.Profile.ID+`"}`)))
	if set.Code != http.StatusOK {
		t.Fatalf("set key default status=%d body=%s", set.Code, set.Body.String())
	}
	// A new conversation with no explicit profile inherits the api_key default.
	tx, beginErr := server.db.BeginTx(context.Background(), nil)
	if beginErr != nil {
		t.Fatalf("begin tx: %v", beginErr)
	}
	revisionID, err := server.profileForNewConversationTx(context.Background(), tx, nil, server.localRunnerID(), "codex", "project-key")
	if err != nil || revisionID != result.Revision.ID {
		t.Fatalf("api_key default revision=%q err=%v want=%q", revisionID, err, result.Revision.ID)
	}
	// Runtime admission resolves the managed codex key and endpoint.
	runtimeProfile, err := server.runtimeProfileTx(context.Background(), tx, revisionID, server.localRunnerID(), "codex")
	_ = tx.Rollback()
	if err != nil || runtimeProfile == nil {
		t.Fatalf("runtime admission err=%v", err)
	}
	if runtimeProfile.Secret != "sk-codex-key" || runtimeProfile.BaseURL != "https://api.example.com/v1" {
		t.Fatalf("runtime codex profile=%#v", runtimeProfile)
	}
}

func TestRevokingOrDisablingProjectDefaultClearsIt(t *testing.T) {
	server := newTestServer(t)
	for _, action := range []string{"revoke", "disable"} {
		t.Run(action, func(t *testing.T) {
			profile := createCLIManagedProfile(t, server, "claude-code", "")
			now := time.Now().UTC()
			projectID := "project-" + action
			if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at,default_profile_id) values (?,?,?,'wsl-local','main',1,?,?)`, projectID, projectID, t.TempDir(), now, profile.ID); err != nil {
				t.Fatalf("insert project: %v", err)
			}
			var endpoint string
			if action == "revoke" {
				endpoint = "/api/agent-profile-revisions/" + profile.CurrentRevisionID + "/revoke"
			} else {
				endpoint = "/api/agent-profiles/" + profile.ID + "/disable"
			}
			resp := httptest.NewRecorder()
			server.routes().ServeHTTP(resp, httptest.NewRequest(http.MethodPost, endpoint, nil))
			if resp.Code != http.StatusNoContent && resp.Code != http.StatusAccepted {
				t.Fatalf("%s status=%d body=%s", action, resp.Code, resp.Body.String())
			}
			var cleared string
			if err := server.db.QueryRow(`select default_profile_id from projects where id=?`, projectID).Scan(&cleared); err != nil {
				t.Fatalf("read project default: %v", err)
			}
			if cleared != "" {
				t.Fatalf("%s did not clear project default: %q", action, cleared)
			}
		})
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
	_, err = server.profileForNewConversationTx(context.Background(), tx, &requested, server.localRunnerID(), "claude-code", "")
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

func TestGetProjectAgentConfig(t *testing.T) {
	server := newTestServer(t)
	// Create a project.
	projectPath := filepath.Join(server.config.AllowedRoot, "cfgproj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	create := httptest.NewRecorder()
	server.routes().ServeHTTP(create, httptest.NewRequest(http.MethodPost, "/api/projects", strings.NewReader(`{"path":"`+projectPath+`"}`)))
	if create.Code != http.StatusCreated {
		t.Fatalf("create project status=%d body=%s", create.Code, create.Body.String())
	}
	var project Project
	if err := json.NewDecoder(create.Body).Decode(&project); err != nil {
		t.Fatalf("decode project: %v", err)
	}
	// Configure a cli_managed Claude profile and an api_key Codex profile.
	claude := createCLIManagedProfile(t, server, "claude-code", "opus-config")
	codexResponse := httptest.NewRecorder()
	server.routes().ServeHTTP(codexResponse, httptest.NewRequest(http.MethodPost, "/api/runners/"+server.localRunnerID()+"/agent-profiles", strings.NewReader(`{"agentId":"codex","name":"Codex key","model":"gpt-config","baseUrl":"https://api.example.com/v1","authMode":"api_key","apiKey":"sk-do-not-leak"}`)))
	if codexResponse.Code != http.StatusCreated {
		t.Fatalf("create codex profile status=%d body=%s", codexResponse.Code, codexResponse.Body.String())
	}
	var codexA struct {
		Profile AgentProfile `json:"profile"`
	}
	if err := json.NewDecoder(codexResponse.Body).Decode(&codexA); err != nil {
		t.Fatalf("decode codex profile: %v", err)
	}
	// Set the Claude profile as the project default.
	setDefault := httptest.NewRecorder()
	server.routes().ServeHTTP(setDefault, httptest.NewRequest(http.MethodPatch, "/api/projects/"+project.ID+"/agent-profile", strings.NewReader(`{"profileId":"`+claude.ID+`"}`)))
	if setDefault.Code != http.StatusOK {
		t.Fatalf("set default status=%d body=%s", setDefault.Code, setDefault.Body.String())
	}
	// The aggregate view reflects both agents and marks the Claude one as default.
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/projects/"+project.ID+"/agent-config", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("agent-config status=%d body=%s", response.Code, response.Body.String())
	}
	var view ProjectAgentConfigView
	if err := json.NewDecoder(response.Body).Decode(&view); err != nil {
		t.Fatalf("decode agent-config: %v", err)
	}
	if !view.RunnerManaged {
		t.Fatalf("local project should be runner-managed: %#v", view)
	}
	if view.Claude == nil || view.Codex == nil {
		t.Fatalf("expected both agent entries: %#v", view)
	}
	if view.Claude.ProfileID != claude.ID || !view.Claude.IsDefault || view.Claude.Model != "opus-config" || view.Claude.AuthMode != "cli_managed" {
		t.Fatalf("claude entry mismatch: %#v", view.Claude)
	}
	if view.Codex.ProfileID != codexA.Profile.ID || view.Codex.IsDefault || view.Codex.Model != "gpt-config" || view.Codex.BaseURL != "https://api.example.com/v1" || view.Codex.AuthMode != "api_key" {
		t.Fatalf("codex entry mismatch: %#v", view.Codex)
	}
	if strings.Contains(response.Body.String(), "sk-do-not-leak") {
		t.Fatalf("managed key leaked in agent-config body: %s", response.Body.String())
	}
}

func TestGetProjectAgentConfigOnlyLoadsExecutableAgents(t *testing.T) {
	server := newTestServer(t)
	projectPath := filepath.Join(server.config.AllowedRoot, "cfgproj2")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	create := httptest.NewRecorder()
	server.routes().ServeHTTP(create, httptest.NewRequest(http.MethodPost, "/api/projects", strings.NewReader(`{"path":"`+projectPath+`"}`)))
	if create.Code != http.StatusCreated {
		t.Fatalf("create project status=%d body=%s", create.Code, create.Body.String())
	}
	var project Project
	if err := json.NewDecoder(create.Body).Decode(&project); err != nil {
		t.Fatalf("decode project: %v", err)
	}
	// Only a Claude profile exists; codex stays nil.
	_ = createCLIManagedProfile(t, server, "claude-code", "only-claude")
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/projects/"+project.ID+"/agent-config", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("agent-config status=%d body=%s", response.Code, response.Body.String())
	}
	var view ProjectAgentConfigView
	if err := json.NewDecoder(response.Body).Decode(&view); err != nil {
		t.Fatalf("decode agent-config: %v", err)
	}
	if view.Claude == nil || view.Codex != nil {
		t.Fatalf("expected only claude entry: %#v", view)
	}
}

func TestGetProjectAgentConfigPrefersProjectDefaultOverAlphabeticOrder(t *testing.T) {
	server := newTestServer(t)
	projectPath := filepath.Join(server.config.AllowedRoot, "cfgproj3")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	create := httptest.NewRecorder()
	server.routes().ServeHTTP(create, httptest.NewRequest(http.MethodPost, "/api/projects", strings.NewReader(`{"path":"`+projectPath+`"}`)))
	if create.Code != http.StatusCreated {
		t.Fatalf("create project status=%d body=%s", create.Code, create.Body.String())
	}
	var project Project
	if err := json.NewDecoder(create.Body).Decode(&project); err != nil {
		t.Fatalf("decode project: %v", err)
	}
	// Two claude profiles; the second (alphabetically later) is set as default.
	first := createCLIManagedProfile(t, server, "claude-code", "a-model")
	second := createCLIManagedProfile(t, server, "claude-code", "b-model")
	set := httptest.NewRecorder()
	server.routes().ServeHTTP(set, httptest.NewRequest(http.MethodPatch, "/api/projects/"+project.ID+"/agent-profile", strings.NewReader(`{"profileId":"`+second.ID+`"}`)))
	if set.Code != http.StatusOK {
		t.Fatalf("set default status=%d body=%s", set.Code, set.Body.String())
	}
	_ = first
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/projects/"+project.ID+"/agent-config", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("agent-config status=%d body=%s", response.Code, response.Body.String())
	}
	var view ProjectAgentConfigView
	if err := json.NewDecoder(response.Body).Decode(&view); err != nil {
		t.Fatalf("decode agent-config: %v", err)
	}
	if view.Claude == nil || view.Claude.ProfileID != second.ID || !view.Claude.IsDefault || view.Claude.Model != "b-model" {
		t.Fatalf("aggregate did not prefer project default: %#v", view.Claude)
	}
}
