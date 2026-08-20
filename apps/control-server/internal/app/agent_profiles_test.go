package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
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
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/runners/"+server.localRunnerID()+"/agent-profiles", strings.NewReader(`{"agentId":"claude-code","name":"Managed key profile","model":"claude-test","baseUrl":"https://api.example.com/v1","authMode":"api_key","apiKey":"sk-super-secret-value","env":{"ANTHROPIC_VERSION":"2023-06-01"}}`)))
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
	if runtimeProfile.Env["ANTHROPIC_VERSION"] != "2023-06-01" {
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
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/runners/"+server.localRunnerID()+"/agent-profiles", strings.NewReader(`{"agentId":"claude-code","name":"Key profile","model":"m1","authMode":"api_key","apiKey":"key-one","env":{"ANTHROPIC_BETA":"test"}}`)))
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
	if created.Revision.Environment["ANTHROPIC_BETA"] != "test" {
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
	// Edit model without providing a new key. The new revision gets its own
	// encrypted copy so either revision can later be revoked independently.
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
	updatedSecret := secretForRevision(updated.Revision.ID)
	if updatedSecret == "" || updatedSecret == firstSecret {
		t.Fatalf("model-only edit did not create an independent secret: old=%q new=%q", firstSecret, updatedSecret)
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
	if rotatedSecret := secretForRevision(rotated.Revision.ID); rotatedSecret == "" || rotatedSecret == updatedSecret || rotatedSecret == firstSecret {
		t.Fatalf("rotation did not mint an independent secret: first=%q model=%q new=%q", firstSecret, updatedSecret, rotatedSecret)
	}
	// Revoking the model-only revision must not revoke the original revision's
	// independent secret snapshot.
	revoke := httptest.NewRecorder()
	server.routes().ServeHTTP(revoke, httptest.NewRequest(http.MethodPost, "/api/agent-profile-revisions/"+updated.Revision.ID+"/revoke", nil))
	if revoke.Code != http.StatusAccepted {
		t.Fatalf("revoke model revision status=%d body=%s", revoke.Code, revoke.Body.String())
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
	// Changing the current profile back to CLI-managed must not revoke a key
	// still referenced by the immediately preceding immutable revision.
	switchToCLI := httptest.NewRecorder()
	server.routes().ServeHTTP(switchToCLI, httptest.NewRequest(http.MethodPatch, "/api/agent-profiles/"+created.Profile.ID, strings.NewReader(`{"authMode":"cli_managed"}`)))
	if switchToCLI.Code != http.StatusOK {
		t.Fatalf("switch to cli-managed status=%d body=%s", switchToCLI.Code, switchToCLI.Body.String())
	}
	rotatedSecret := secretForRevision(rotated.Revision.ID)
	tx, err = server.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin snapshot verification: %v", err)
	}
	keyBeforeCLI, err := server.profileSecrets.Load(tx, context.Background(), rotatedSecret)
	_ = tx.Rollback()
	if err != nil || keyBeforeCLI != "key-two" {
		t.Fatalf("api-key snapshot lost after CLI switch: key=%q err=%v", keyBeforeCLI, err)
	}
}

func TestRecoverInterruptedRunReleasesQuotaAndCompletesRevocation(t *testing.T) {
	server := newTestServer(t)
	profile := createCLIManagedProfile(t, server, "claude-code", "recovery-model")
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('recovery-project','recovery-project',?,?,'main',1,?)`, t.TempDir(), server.localRunnerID(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,agent_id,agent_session_id,agent_runtime_id,agent_profile_revision_id,status,created_at) values ('recovery-conversation','recovery-project','recovery-session','claude-code','recovery-session',?,'','running',?)`, server.localRunnerID(), now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	tx, err := server.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin recovery setup: %v", err)
	}
	if _, err := tx.Exec(`update conversations set agent_profile_revision_id=? where id='recovery-conversation'`, profile.CurrentRevisionID); err != nil {
		t.Fatalf("set conversation profile: %v", err)
	}
	if _, err := tx.Exec(`insert into runs (id,conversation_id,agent_id,agent_runtime_id,agent_profile_revision_id,status,created_at) values ('recovery-run','recovery-conversation','claude-code',?,?, 'running',?)`, server.localRunnerID(), profile.CurrentRevisionID, now); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if err := server.reserveProfileQuotaTx(context.Background(), tx, "recovery-run", profile.CurrentRevisionID); err != nil {
		t.Fatalf("reserve quota: %v", err)
	}
	if _, err := tx.Exec(`insert into revocation_jobs (id,profile_revision_id,state,error,created_at,updated_at) values ('recovery-revoke',?,'stopping','',?,?)`, profile.CurrentRevisionID, now, now); err != nil {
		t.Fatalf("insert revocation job: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit recovery setup: %v", err)
	}
	if err := server.recoverInterruptedRuns(context.Background()); err != nil {
		t.Fatalf("recover interrupted runs: %v", err)
	}
	var runStatus, reservationState, revocationState string
	if err := server.db.QueryRow(`select status from runs where id='recovery-run'`).Scan(&runStatus); err != nil {
		t.Fatalf("read recovered run: %v", err)
	}
	if err := server.db.QueryRow(`select state from quota_reservations where run_id='recovery-run'`).Scan(&reservationState); err != nil {
		t.Fatalf("read recovered reservation: %v", err)
	}
	if err := server.db.QueryRow(`select state from revocation_jobs where id='recovery-revoke'`).Scan(&revocationState); err != nil {
		t.Fatalf("read recovered revocation: %v", err)
	}
	if runStatus != "interrupted" || reservationState != "released" || revocationState != "completed" {
		t.Fatalf("recovery state: run=%q reservation=%q revocation=%q", runStatus, reservationState, revocationState)
	}
}

func TestManagedProfileSnapshotsRevisionAndRevocationCancelsRun(t *testing.T) {
	server := newTestServer(t)
	profile := createCLIManagedProfile(t, server, "claude-code", "claude-test-model")
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,?,'main',1,?)`, t.TempDir(), server.localRunnerID(), now); err != nil {
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
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,?,'main',1,?)`, t.TempDir(), server.localRunnerID(), now); err != nil {
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
	// New bindings are stored in a route revision, not the retired single
	// projects.default_profile_id column.
	var routeRevisionID, routedProfileID string
	if err := server.db.QueryRow(`select pr.current_revision_id,rr.profile_id from project_agent_routes pr join project_agent_route_revisions rr on rr.id=pr.current_revision_id where pr.project_id=? and pr.agent_id=?`, "project", "claude-code").Scan(&routeRevisionID, &routedProfileID); err != nil {
		t.Fatalf("read project route: %v", err)
	}
	if routeRevisionID == "" || routedProfileID != profile.ID {
		t.Fatalf("project route=%q profile=%q want=%q", routeRevisionID, routedProfileID, profile.ID)
	}
	// Clearing the default can be done via the dedicated endpoint.
	clear := httptest.NewRecorder()
	server.routes().ServeHTTP(clear, httptest.NewRequest(http.MethodPatch, "/api/projects/project/agent-profile", strings.NewReader(`{"agentId":"claude-code","profileId":""}`)))
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

func TestProjectAgentRoutesKeepClaudeAndCodexIndependent(t *testing.T) {
	server := newTestServer(t)
	claude := createCLIManagedProfile(t, server, "claude-code", "claude-model")
	codex := createCLIManagedProfile(t, server, "codex", "codex-model")
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,?,'main',1,?)`, t.TempDir(), server.localRunnerID(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	for _, item := range []struct{ agentID, profileID string }{{"claude-code", claude.ID}, {"codex", codex.ID}} {
		response := httptest.NewRecorder()
		server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/api/projects/project/agent-profile", strings.NewReader(`{"agentId":"`+item.agentID+`","profileId":"`+item.profileID+`"}`)))
		if response.Code != http.StatusOK {
			t.Fatalf("set %s route status=%d body=%s", item.agentID, response.Code, response.Body.String())
		}
	}
	for _, item := range []struct{ agentID, revisionID string }{{"claude-code", claude.CurrentRevisionID}, {"codex", codex.CurrentRevisionID}} {
		tx, err := server.db.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		selection, err := server.profileRouteForNewConversationTx(context.Background(), tx, nil, server.localRunnerID(), item.agentID, "project")
		_ = tx.Rollback()
		if err != nil || selection.ProfileRevisionID != item.revisionID || selection.RouteRevisionID == "" {
			t.Fatalf("%s selection=%#v err=%v", item.agentID, selection, err)
		}
	}
	clear := httptest.NewRecorder()
	server.routes().ServeHTTP(clear, httptest.NewRequest(http.MethodPatch, "/api/projects/project/agent-profile", strings.NewReader(`{"agentId":"claude-code","profileId":""}`)))
	if clear.Code != http.StatusOK {
		t.Fatalf("clear Claude route status=%d body=%s", clear.Code, clear.Body.String())
	}
	tx, err := server.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin codex tx: %v", err)
	}
	selection, err := server.profileRouteForNewConversationTx(context.Background(), tx, nil, server.localRunnerID(), "codex", "project")
	_ = tx.Rollback()
	if err != nil || selection.ProfileRevisionID != codex.CurrentRevisionID {
		t.Fatalf("Codex route changed after Claude clear: %#v err=%v", selection, err)
	}
}

func TestCredentialQuotaReservationLimitsConcurrentRuns(t *testing.T) {
	server := newTestServer(t)
	profile := createCLIManagedProfile(t, server, "claude-code", "quota-model")
	var groupID string
	if err := server.db.QueryRow(`select quota_group_id from credential_quota_groups where profile_id=?`, profile.ID).Scan(&groupID); err != nil {
		t.Fatalf("private quota group: %v", err)
	}
	if _, err := server.db.Exec(`update quota_groups set max_concurrency=1 where id=?`, groupID); err != nil {
		t.Fatalf("set quota limit: %v", err)
	}
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('quota-project','quota-project',?,?,'main',1,?)`, t.TempDir(), server.localRunnerID(), time.Now().UTC()); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,agent_id,agent_session_id,agent_runtime_id,status,created_at) values ('quota-conversation','quota-project','quota-session','claude-code','quota-session',?,'idle',?)`, server.localRunnerID(), time.Now().UTC()); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	for _, runID := range []string{"quota-run-1", "quota-run-2"} {
		tx, err := server.db.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatalf("begin %s: %v", runID, err)
		}
		if _, err := tx.Exec(`insert into runs (id,conversation_id,status,created_at) values (?,?,?,?)`, runID, "quota-conversation", "running", time.Now().UTC()); err != nil {
			t.Fatalf("insert %s: %v", runID, err)
		}
		err = server.reserveProfileQuotaTx(context.Background(), tx, runID, profile.CurrentRevisionID)
		if runID == "quota-run-1" {
			if err != nil {
				t.Fatalf("first reservation: %v", err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit first: %v", err)
			}
		} else {
			_ = tx.Rollback()
			var quotaErr *quotaAdmissionError
			if !errors.As(err, &quotaErr) {
				t.Fatalf("second reservation err=%v, want quota admission", err)
			}
		}
	}
}

func TestAttachDisabledQuotaGroupReturnsConflict(t *testing.T) {
	server := newTestServer(t)
	profile := createCLIManagedProfile(t, server, "claude-code", "disabled-quota-group")
	var groupID string
	if err := server.db.QueryRow(`select quota_group_id from credential_quota_groups where profile_id=?`, profile.ID).Scan(&groupID); err != nil {
		t.Fatalf("private quota group: %v", err)
	}
	if _, err := server.db.Exec(`update quota_groups set enabled=0 where id=?`, groupID); err != nil {
		t.Fatalf("disable quota group: %v", err)
	}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/quota-groups/"+groupID+"/profiles/"+profile.ID, nil))
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "quota group is disabled") {
		t.Fatalf("attach disabled quota group status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestCredentialQuotaReservationIsIdempotent(t *testing.T) {
	server := newTestServer(t)
	profile := createCLIManagedProfile(t, server, "claude-code", "quota-idempotent")
	var groupID string
	if err := server.db.QueryRow(`select quota_group_id from credential_quota_groups where profile_id=?`, profile.ID).Scan(&groupID); err != nil {
		t.Fatalf("private quota group: %v", err)
	}
	now := time.Now().UTC()
	if _, err := server.db.Exec(`update quota_groups set rpm_limit=2,max_concurrency=1 where id=?`, groupID); err != nil {
		t.Fatalf("set quota group: %v", err)
	}
	if _, err := server.db.Exec(`update quota_group_state set rpm_tokens=2,last_refilled_at=?,updated_at=? where quota_group_id=?`, now, now, groupID); err != nil {
		t.Fatalf("seed quota state: %v", err)
	}
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('idempotent-project','idempotent-project',?,?,'main',1,?)`, t.TempDir(), server.localRunnerID(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,agent_id,agent_session_id,agent_runtime_id,status,created_at) values ('idempotent-conversation','idempotent-project','session','claude-code','session',?,'idle',?)`, server.localRunnerID(), now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	tx, err := server.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`insert into runs (id,conversation_id,status,created_at) values ('idempotent-run','idempotent-conversation','running',?)`, now); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := server.reserveProfileQuotaTx(context.Background(), tx, "idempotent-run", profile.CurrentRevisionID); err != nil {
			t.Fatalf("reserve attempt %d: %v", attempt+1, err)
		}
	}
	var reservations int
	var tokens float64
	if err := tx.QueryRow(`select count(*) from quota_reservations where run_id='idempotent-run'`).Scan(&reservations); err != nil {
		t.Fatalf("count reservations: %v", err)
	}
	if err := tx.QueryRow(`select rpm_tokens from quota_group_state where quota_group_id=?`, groupID).Scan(&tokens); err != nil {
		t.Fatalf("read tokens: %v", err)
	}
	if reservations != 1 || tokens != 1 {
		t.Fatalf("reservations=%d tokens=%v, want one reservation and one consumed RPM", reservations, tokens)
	}
}

func TestCredentialSchedulingMigrationDoesNotRefillExhaustedBuckets(t *testing.T) {
	server := newTestServer(t)
	profile := createCLIManagedProfile(t, server, "claude-code", "quota-restart")
	var groupID string
	if err := server.db.QueryRow(`select quota_group_id from credential_quota_groups where profile_id=?`, profile.ID).Scan(&groupID); err != nil {
		t.Fatalf("private quota group: %v", err)
	}
	now := time.Now().UTC()
	if _, err := server.db.Exec(`update quota_groups set rpm_limit=1,tpm_limit=1 where id=?`, groupID); err != nil {
		t.Fatalf("set quota limits: %v", err)
	}
	if _, err := server.db.Exec(`update quota_group_state set rpm_tokens=0,tpm_tokens=0,last_refilled_at=?,updated_at=? where quota_group_id=?`, now, now, groupID); err != nil {
		t.Fatalf("exhaust quota state: %v", err)
	}
	if err := server.migrateCredentialScheduling(context.Background()); err != nil {
		t.Fatalf("rerun scheduling migration: %v", err)
	}
	var rpm, tpm float64
	if err := server.db.QueryRow(`select rpm_tokens,tpm_tokens from quota_group_state where quota_group_id=?`, groupID).Scan(&rpm, &tpm); err != nil {
		t.Fatalf("read quota state: %v", err)
	}
	if rpm != 0 || tpm != 0 {
		t.Fatalf("migration refilled exhausted bucket: rpm=%v tpm=%v", rpm, tpm)
	}
}

func TestCredentialQuotaReconcilesReportedTPMUsage(t *testing.T) {
	server := newTestServer(t)
	profile := createCLIManagedProfile(t, server, "claude-code", "quota-usage")
	var groupID string
	if err := server.db.QueryRow(`select quota_group_id from credential_quota_groups where profile_id=?`, profile.ID).Scan(&groupID); err != nil {
		t.Fatalf("private quota group: %v", err)
	}
	now := time.Now().UTC()
	if _, err := server.db.Exec(`update quota_groups set tpm_limit=10000 where id=?`, groupID); err != nil {
		t.Fatalf("set TPM limit: %v", err)
	}
	if _, err := server.db.Exec(`update quota_group_state set tpm_tokens=10000,last_refilled_at=?,updated_at=? where quota_group_id=?`, now, now, groupID); err != nil {
		t.Fatalf("seed quota state: %v", err)
	}
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('usage-project','usage-project',?,?,'main',1,?)`, t.TempDir(), server.localRunnerID(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,agent_id,agent_session_id,agent_runtime_id,status,created_at) values ('usage-conversation','usage-project','session','claude-code','session',?,'idle',?)`, server.localRunnerID(), now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	tx, err := server.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin reservation: %v", err)
	}
	if _, err := tx.Exec(`insert into runs (id,conversation_id,status,created_at) values ('usage-run','usage-conversation','running',?)`, now); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if err := server.reserveProfileQuotaTx(context.Background(), tx, "usage-run", profile.CurrentRevisionID); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit reservation: %v", err)
	}
	if _, err := server.db.Exec(`insert into run_usage (run_id,conversation_id,input_tokens,output_tokens,cache_read_tokens,cache_creation_tokens,completed_at) values ('usage-run','usage-conversation',300,100,100,100,?)`, now); err != nil {
		t.Fatalf("insert usage: %v", err)
	}
	tx, err = server.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin reconciliation: %v", err)
	}
	if err := server.reconcileQuotaUsageTx(context.Background(), tx, "usage-run"); err != nil {
		t.Fatalf("reconcile usage: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit reconciliation: %v", err)
	}
	var tokens, reserved float64
	if err := server.db.QueryRow(`select tpm_tokens from quota_group_state where quota_group_id=?`, groupID).Scan(&tokens); err != nil {
		t.Fatalf("read TPM tokens: %v", err)
	}
	if err := server.db.QueryRow(`select reserved_tpm from quota_reservations where run_id='usage-run' and quota_group_id=?`, groupID).Scan(&reserved); err != nil {
		t.Fatalf("read reserved TPM: %v", err)
	}
	if tokens != 9400 || reserved != 600 {
		t.Fatalf("tokens=%v reserved=%v, want tokens=9400 reserved=600", tokens, reserved)
	}
}

func TestCredentialPoolSkipsCoolingMember(t *testing.T) {
	server := newTestServer(t)
	first := createCLIManagedProfile(t, server, "claude-code", "pool-cooling-first")
	second := createCLIManagedProfile(t, server, "claude-code", "pool-cooling-first")
	response := httptest.NewRecorder()
	body := `{"name":"cooling pool","strategy":"round_robin","profileIds":["` + first.ID + `","` + second.ID + `"]}`
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/runners/"+server.localRunnerID()+"/credential-pools", strings.NewReader(body)))
	if response.Code != http.StatusCreated {
		t.Fatalf("create pool status=%d body=%s", response.Code, response.Body.String())
	}
	var created struct {
		PoolRevisionID string `json:"poolRevisionId"`
	}
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatalf("decode pool: %v", err)
	}
	var firstGroupID string
	if err := server.db.QueryRow(`select quota_group_id from credential_quota_groups where profile_id=?`, first.ID).Scan(&firstGroupID); err != nil {
		t.Fatalf("first quota group: %v", err)
	}
	if _, err := server.db.Exec(`update quota_group_state set cooldown_until=?,updated_at=? where quota_group_id=?`, time.Now().UTC().Add(time.Minute), time.Now().UTC(), firstGroupID); err != nil {
		t.Fatalf("cool first member: %v", err)
	}
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('cooling-project','cooling-project',?,?,'main',1,?)`, t.TempDir(), server.localRunnerID(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	set := httptest.NewRecorder()
	server.routes().ServeHTTP(set, httptest.NewRequest(http.MethodPost, "/api/projects/cooling-project/agent-pool", strings.NewReader(`{"agentId":"claude-code","poolRevisionId":"`+created.PoolRevisionID+`"}`)))
	if set.Code != http.StatusOK {
		t.Fatalf("set pool status=%d body=%s", set.Code, set.Body.String())
	}
	config := httptest.NewRecorder()
	server.routes().ServeHTTP(config, httptest.NewRequest(http.MethodGet, "/api/projects/cooling-project/agent-config", nil))
	if config.Code != http.StatusOK {
		t.Fatalf("get pool config status=%d body=%s", config.Code, config.Body.String())
	}
	var configView struct {
		Claude struct {
			Mode           string `json:"mode"`
			PoolRevisionID string `json:"poolRevisionId"`
		} `json:"claude"`
	}
	if err := json.NewDecoder(config.Body).Decode(&configView); err != nil {
		t.Fatalf("decode pool config: %v", err)
	}
	if configView.Claude.Mode != "pool" || configView.Claude.PoolRevisionID != created.PoolRevisionID {
		t.Fatalf("pool config=%#v want mode=pool revision=%q", configView.Claude, created.PoolRevisionID)
	}
	tx, err := server.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	selection, err := server.profileRouteForNewConversationTx(context.Background(), tx, nil, server.localRunnerID(), "claude-code", "cooling-project")
	if err != nil {
		t.Fatalf("route selection: %v", err)
	}
	selectedRevisionID, err := server.selectPoolProfileRevisionTx(context.Background(), tx, selection.RouteRevisionID, server.localRunnerID(), "claude-code")
	if err != nil {
		t.Fatalf("select pool member: %v", err)
	}
	if selectedRevisionID != second.CurrentRevisionID {
		t.Fatalf("selected cooling member %q, want available member %q", selectedRevisionID, second.CurrentRevisionID)
	}
}

func TestCredentialPoolPreservesMemberRevisionSnapshot(t *testing.T) {
	server := newTestServer(t)
	profile := createCLIManagedProfile(t, server, "claude-code", "snapshot-model")
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/runners/"+server.localRunnerID()+"/credential-pools", strings.NewReader(`{"name":"snapshot pool","profileIds":["`+profile.ID+`"]}`)))
	if response.Code != http.StatusCreated {
		t.Fatalf("create pool status=%d body=%s", response.Code, response.Body.String())
	}
	var created struct {
		PoolRevisionID string `json:"poolRevisionId"`
	}
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatalf("decode pool: %v", err)
	}
	var memberRevisionID string
	if err := server.db.QueryRow(`select profile_revision_id from credential_pool_revision_members where pool_revision_id=? and profile_id=?`, created.PoolRevisionID, profile.ID).Scan(&memberRevisionID); err != nil {
		t.Fatalf("read pool member revision: %v", err)
	}
	if memberRevisionID != profile.CurrentRevisionID {
		t.Fatalf("member revision=%q want %q", memberRevisionID, profile.CurrentRevisionID)
	}
	update := httptest.NewRecorder()
	server.routes().ServeHTTP(update, httptest.NewRequest(http.MethodPatch, "/api/agent-profiles/"+profile.ID, strings.NewReader(`{"model":"rotated-model"}`)))
	if update.Code != http.StatusOK {
		t.Fatalf("rotate profile status=%d body=%s", update.Code, update.Body.String())
	}
	var rotated struct {
		Profile AgentProfile `json:"profile"`
	}
	if err := json.NewDecoder(update.Body).Decode(&rotated); err != nil {
		t.Fatalf("decode rotated profile: %v", err)
	}
	if rotated.Profile.CurrentRevisionID == memberRevisionID {
		t.Fatal("profile rotation did not create a revision")
	}
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('snapshot-project','snapshot-project',?,?,'main',1,?)`, t.TempDir(), server.localRunnerID(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	set := httptest.NewRecorder()
	server.routes().ServeHTTP(set, httptest.NewRequest(http.MethodPost, "/api/projects/snapshot-project/agent-pool", strings.NewReader(`{"agentId":"claude-code","poolRevisionId":"`+created.PoolRevisionID+`"}`)))
	if set.Code != http.StatusOK {
		t.Fatalf("set pool status=%d body=%s", set.Code, set.Body.String())
	}
	tx, err := server.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	selection, err := server.profileRouteForNewConversationTx(context.Background(), tx, nil, server.localRunnerID(), "claude-code", "snapshot-project")
	if err != nil {
		t.Fatalf("route selection: %v", err)
	}
	selected, err := server.selectPoolProfileRevisionTx(context.Background(), tx, selection.RouteRevisionID, server.localRunnerID(), "claude-code")
	if err != nil || selected != memberRevisionID {
		t.Fatalf("selected member revision=%q err=%v want=%q", selected, err, memberRevisionID)
	}
}

func TestProjectRouteRevisionRejectsPoolSnapshotMutation(t *testing.T) {
	server := newTestServer(t)
	profile := createCLIManagedProfile(t, server, "claude-code", "route-snapshot")
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('route-project','route-project',?,?,'main',1,?)`, t.TempDir(), server.localRunnerID(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	tx, err := server.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin route creation: %v", err)
	}
	if err := server.replaceProjectAgentRouteTx(context.Background(), tx, "route-project", "claude-code", profile.ID, profile.CurrentRevisionID, "pinned"); err != nil {
		t.Fatalf("create route: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit route: %v", err)
	}
	var routeRevisionID string
	if err := server.db.QueryRow(`select current_revision_id from project_agent_routes where project_id='route-project' and agent_id='claude-code'`).Scan(&routeRevisionID); err != nil {
		t.Fatalf("read route revision: %v", err)
	}
	if _, err := server.db.Exec(`update project_agent_route_revisions set pool_revision_id='unexpected-pool' where id=?`, routeRevisionID); err == nil {
		t.Fatal("pool revision snapshot mutation unexpectedly succeeded")
	}
}

func TestRemoteCredentialPoolWritesAreRejectedWithoutRunnerAgent(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into runners (id,kind,display_name,config_json,enabled,last_error,created_at,updated_at) values ('ssh-pool','ssh','SSH pool','{}',1,'',?,?)`, now, now); err != nil {
		t.Fatalf("insert remote runner: %v", err)
	}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/runners/ssh-pool/credential-pools", strings.NewReader(`{"name":"remote pool","profileIds":["profile"]}`)))
	if response.Code != http.StatusConflict {
		t.Fatalf("remote pool status=%d body=%s, want conflict", response.Code, response.Body.String())
	}
}

func TestRetryAfterFromRateLimitError(t *testing.T) {
	if got := retryAfterFromError(errors.New("HTTP 429 Retry-After: 17")); got != 17*time.Second {
		t.Fatalf("retry after=%s want 17s", got)
	}
	if got := retryAfterFromError(errors.New("Retry-After: 7200")); got != time.Hour {
		t.Fatalf("bounded retry after=%s want 1h", got)
	}
}

func TestCredentialPoolRouteSelectsAvailableProfile(t *testing.T) {
	server := newTestServer(t)
	first := createCLIManagedProfile(t, server, "claude-code", "pool-first")
	second := createCLIManagedProfile(t, server, "claude-code", "pool-first")
	response := httptest.NewRecorder()
	body := `{"name":"shared pool","strategy":"least_loaded","profileIds":["` + first.ID + `","` + second.ID + `"]}`
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/runners/"+server.localRunnerID()+"/credential-pools", strings.NewReader(body)))
	if response.Code != http.StatusCreated {
		t.Fatalf("create pool status=%d body=%s", response.Code, response.Body.String())
	}
	var created struct {
		PoolRevisionID string `json:"poolRevisionId"`
	}
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatalf("decode pool: %v", err)
	}
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('pool-project','pool-project',?,?,'main',1,?)`, t.TempDir(), server.localRunnerID(), time.Now().UTC()); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	set := httptest.NewRecorder()
	server.routes().ServeHTTP(set, httptest.NewRequest(http.MethodPost, "/api/projects/pool-project/agent-pool", strings.NewReader(`{"agentId":"claude-code","poolRevisionId":"`+created.PoolRevisionID+`"}`)))
	if set.Code != http.StatusOK {
		t.Fatalf("set pool status=%d body=%s", set.Code, set.Body.String())
	}
	tx, err := server.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	selection, err := server.profileRouteForNewConversationTx(context.Background(), tx, nil, server.localRunnerID(), "claude-code", "pool-project")
	if err != nil || selection.ProfileRevisionID != "" || selection.RouteRevisionID == "" {
		_ = tx.Rollback()
		t.Fatalf("queued pool selection=%#v err=%v", selection, err)
	}
	selectedRevisionID, err := server.selectPoolProfileRevisionTx(context.Background(), tx, selection.RouteRevisionID, server.localRunnerID(), "claude-code")
	_ = tx.Rollback()
	if err != nil || (selectedRevisionID != first.CurrentRevisionID && selectedRevisionID != second.CurrentRevisionID) {
		t.Fatalf("admitted pool revision=%q err=%v", selectedRevisionID, err)
	}
}

func TestCodexApiKeyProfileBypassesLoginReadiness(t *testing.T) {
	server := newTestServer(t)
	// A fake codex binary that exists but is NOT logged in (login status fails).
	bin := filepath.Join(t.TempDir(), "fake-codex")
	if runtime.GOOS == "windows" {
		bin = filepath.Join(os.Getenv("SystemRoot"), "System32", "where.exe")
		if _, err := os.Stat(bin); err != nil {
			t.Skipf("where.exe is unavailable: %v", err)
		}
	} else {
		script := "#!/bin/sh\nif [ \"$1\" = \"login\" ]; then exit 1; fi\nexit 0\n"
		if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
			t.Fatalf("write fake codex: %v", err)
		}
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
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project-key','project-key',?,?,'main',1,?)`, t.TempDir(), server.localRunnerID(), now); err != nil {
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
			if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at,default_profile_id) values (?,?,?,?,'main',1,?,?)`, projectID, projectID, t.TempDir(), server.localRunnerID(), now, profile.ID); err != nil {
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
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,?,'main',1,?)`, t.TempDir(), server.localRunnerID(), now); err != nil {
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
	server.routes().ServeHTTP(create, httptest.NewRequest(http.MethodPost, "/api/projects", jsonBody(t, map[string]string{"path": projectPath})))
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
	server.routes().ServeHTTP(create, httptest.NewRequest(http.MethodPost, "/api/projects", jsonBody(t, map[string]string{"path": projectPath})))
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
	server.routes().ServeHTTP(create, httptest.NewRequest(http.MethodPost, "/api/projects", jsonBody(t, map[string]string{"path": projectPath})))
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
