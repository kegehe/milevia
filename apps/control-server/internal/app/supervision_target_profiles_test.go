package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

func TestSupervisionTargetProfileProjectionBindsMetadataWithoutDurableSecrets(t *testing.T) {
	profile := &AgentRuntimeProfile{RevisionID: "revision", AgentID: "codex", Model: "target-model", AuthMode: "api_key", Secret: "literal-private-value", ExecutionMode: "target_isolated", BaseURL: "https://provider.example/v1"}
	spec, input := supervisionResolutionInput()
	profile.TargetID = spec.TargetID
	spec.Model = profile.Model
	spec.ProfileRevisionID = profile.RevisionID
	spec.ProfileSHA256 = supervisionTargetProfileFingerprint(profile)
	projection := supervisionTargetCredentials(profile)
	actual, err := supervisionTargetRuntimeProfile(spec, projection)
	if err != nil || actual.Secret != profile.Secret {
		t.Fatal("projection was not available to the authorized adapter", err)
	}
	if strings.Contains(string(mustJSON(spec)), profile.Secret) {
		t.Fatal("secret entered durable specification")
	}
	for _, kind := range []string{"missing", "agent", "revision", "endpoint", "model", "mode", "unexpected", "target", "unbound_target"} {
		t.Run(kind, func(t *testing.T) {
			copy := *projection
			bound := spec
			switch kind {
			case "missing":
				projection = nil
				defer func() { projection = &copy }()
			case "agent":
				copy.AgentID = "claude-code"
			case "revision":
				copy.RevisionID = "other"
			case "endpoint":
				copy.BaseURL = "https://other.example/v1"
			case "model":
				copy.Model = "other"
			case "mode":
				copy.ExecutionMode = "shared"
			case "unexpected":
				bound.ProfileRevisionID = ""
			case "target":
				copy.TargetID = "other-target"
				metadata := *profile
				metadata.TargetID = copy.TargetID
				bound.ProfileSHA256 = supervisionTargetProfileFingerprint(&metadata)
			case "unbound_target":
				copy.TargetID = ""
				metadata := *profile
				metadata.TargetID = ""
				bound.ProfileSHA256 = supervisionTargetProfileFingerprint(&metadata)
			}
			candidate := &copy
			if kind == "missing" {
				candidate = nil
			}
			if _, err := supervisionTargetRuntimeProfile(bound, candidate); !errors.Is(err, targetexec.ErrConflict) {
				t.Fatal("foreign projection accepted", err)
			}
		})
	}
	// Native resolution sees the projection but never launches a model process.
	adapter := supervisionResolutionAdapter{run: func(_ context.Context, request supervisionSchedulerCLIRequest, _ AgentRunSink) (supervisionSchedulerCLIReceipt, error) {
		if request.Profile == nil || request.Profile.Secret != profile.Secret {
			t.Fatal("resolution lost target profile")
		}
		err := request.Resolved(supervisionCLIInfo{AgentID: "codex", Path: "/target/codex", Version: "1.0", Contract: supervisionCodexContract, EffectiveModel: profile.Model})
		return supervisionSchedulerCLIReceipt{ContainerQuiet: true}, err
	}}
	if _, err := resolveSupervisionTargetCLI(context.Background(), adapter, spec, input, profile); err != nil {
		t.Fatal(err)
	}
	raw, err := supervisionTargetPayload(mustJSON(map[string]any{"content": "echo " + profile.Secret, "nested": []any{profile.Secret}, "input_tokens": json.Number("9007199254740993")}), profile.Secret)
	if err != nil || strings.Contains(string(raw), profile.Secret) || !strings.Contains(string(raw), "9007199254740993") {
		t.Fatal("projection secret leaked or counters changed", string(raw), err)
	}
}

func createSupervisionProfileForTest(t *testing.T, s *Server, project Project) AgentProfile {
	t.Helper()
	response := httptest.NewRecorder()
	s.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/runners/"+s.supervisionProjectRoute(project).RunnerID+"/supervision-profiles?projectId="+project.ID, strings.NewReader(`{"agentId":"codex","name":"Scoped profile","authMode":"cli_managed","model":"test-model"}`)))
	if response.Code != http.StatusCreated {
		t.Fatal(response.Code, response.Body.String())
	}
	var result struct {
		Profile AgentProfile `json:"profile"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result.Profile
}

func TestSupervisionTargetProfileRevisionRetainsDestinationAndFrozenSelection(t *testing.T) {
	s, p, _, draft, now := supervisionFixture(t, defaultSupervisionBudget())
	profile := createSupervisionProfileForTest(t, s, p)
	if err := s.updateSupervisionRoles(context.Background(), draft.ID, draft.StateVersion, supervisionRoleConfigs{"scheduler": {AgentID: "codex", ProfileID: profile.ID}}); err != nil {
		t.Fatal(err)
	}
	ss := startSupervisionForTest(t, s, draft.ID, now)
	response := httptest.NewRecorder()
	s.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/api/supervision-profiles/"+profile.ID, strings.NewReader(`{"model":"updated-model"}`)))
	if response.Code != http.StatusOK {
		t.Fatal(response.Code, response.Body.String())
	}
	var current string
	if err := s.db.QueryRow(`select current_revision_id from agent_profiles where id=?`, profile.ID).Scan(&current); err != nil || current == profile.CurrentRevisionID {
		t.Fatal("editing did not create an independent revision", current, err)
	}
	for _, revision := range []string{profile.CurrentRevisionID, current} {
		var target, route string
		if err := s.db.QueryRow(`select target_id,connection_key from supervision_profile_targets where revision_id=?`, revision).Scan(&target, &route); err != nil || target != "native:"+s.localRunnerID() || route != s.supervisionProjectRoute(p).Key {
			t.Fatal("revision lost its immutable destination", target, route, err)
		}
	}
	if _, err := s.captureSupervisionProjectContext(context.Background(), ss.ID, ss.StateVersion); err != nil {
		t.Fatal(err)
	}
	lease, d := decisionForTest(t, s, ss.ID, time.Now().UTC())
	if _, err := s.claimSupervisionDecisionLaunch(context.Background(), d.ID, lease, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	permit, err := s.prepareSupervisionExecution(context.Background(), d, lease, time.Now().UTC())
	if err != nil || permit.Profile == nil || permit.Profile.RevisionID != profile.CurrentRevisionID || permit.Profile.TargetID != "native:"+s.localRunnerID() {
		t.Fatal("existing session silently adopted a new credential version", err)
	}
	if _, err := s.db.Exec(`update supervision_profile_targets set target_id='other' where revision_id=?`, current); err == nil {
		t.Fatal("credential destination was mutable")
	}
	for _, mutate := range []func(*AgentRuntimeProfile){
		func(p *AgentRuntimeProfile) { p.TargetID = "other-target" },
		func(p *AgentRuntimeProfile) { p.Secret = "injected-secret" },
	} {
		copy := *permit.Profile
		mutate(&copy)
		forged := permit
		forged.Profile = &copy
		if err := s.admitSupervisionTargetProfile(context.Background(), forged, func() {}); err == nil {
			t.Fatal("caller-owned credential projection replaced frozen authorization")
		}
	}
}

func TestSupervisionProfileRevocationBeforeFirstRunFencesFrozenSelection(t *testing.T) {
	s, p, _, draft, now := supervisionFixture(t, defaultSupervisionBudget())
	profile := createSupervisionProfileForTest(t, s, p)
	if err := s.updateSupervisionRoles(context.Background(), draft.ID, draft.StateVersion, supervisionRoleConfigs{"scheduler": {AgentID: "codex", ProfileID: profile.ID}}); err != nil {
		t.Fatal(err)
	}
	startSupervisionForTest(t, s, draft.ID, now)
	response := httptest.NewRecorder()
	s.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/supervision-profile-revisions/"+profile.CurrentRevisionID+"/revoke", nil))
	ss := supervisionSessionForTest(t, s, draft.ID)
	if response.Code != http.StatusAccepted || ss.Control != "pause" || ss.Reason != "credential_revoked" {
		t.Fatal("frozen credentials survived revocation before first execution", response.Body.String(), ss.State, ss.Reason)
	}
}

func TestSupervisionProfileRevocationIgnoresUnusedExecutionHistory(t *testing.T) {
	s, _, _, draft, now := supervisionFixture(t, defaultSupervisionBudget())
	profile := createCLIManagedProfile(t, s, "codex", "test-model")
	if err := s.updateSupervisionRoles(context.Background(), draft.ID, draft.StateVersion, supervisionRoleConfigs{"scheduler": {AgentID: "codex", ProfileID: profile.ID}}); err != nil {
		t.Fatal(err)
	}
	ss := startSupervisionForTest(t, s, draft.ID, now)
	if _, err := s.captureSupervisionProjectContext(context.Background(), ss.ID, ss.StateVersion); err != nil {
		t.Fatal(err)
	}
	lease, d := decisionForTest(t, s, ss.ID, time.Now().UTC())
	if _, err := s.claimSupervisionDecisionLaunch(context.Background(), d.ID, lease, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	permit, err := s.prepareSupervisionExecution(context.Background(), d, lease, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	// Model a quiescent historical turn after the user has selected CLI login.
	if _, err := s.db.Exec(`update supervision_executions set state='quiescent' where execution_id=?;update supervision_sessions set route_snapshot='[]' where id=?`, permit.Decision.ExecutionID, ss.ID); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	s.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/supervision-profile-revisions/"+profile.CurrentRevisionID+"/revoke", nil))
	ss = supervisionSessionForTest(t, s, ss.ID)
	if response.Code != http.StatusAccepted || ss.Control != "" {
		t.Fatal("unused historical credential paused the current session", response.Body.String(), ss.Control)
	}
}

func TestSupervisionTargetProfileRejectsAnotherDestinationBeforeClaimingTasks(t *testing.T) {
	s, p, taskID, draft, now := supervisionFixture(t, defaultSupervisionBudget())
	profile := createSupervisionProfileForTest(t, s, p)
	// A stored profile can belong to the same management Runner while pointing
	// at another actual target. Revision metadata alone cannot authorize it.
	if _, err := s.db.Exec(`delete from supervision_profile_targets where revision_id=?;insert into supervision_profile_targets(revision_id,target_id,connection_key) values (?,'another-target','another-route')`, profile.CurrentRevisionID, profile.CurrentRevisionID); err != nil {
		t.Fatal(err)
	}
	if err := s.updateSupervisionRoles(context.Background(), draft.ID, draft.StateVersion, supervisionRoleConfigs{"scheduler": {AgentID: "codex", ProfileID: profile.ID}}); err != nil {
		t.Fatal(err)
	}
	ss := supervisionSessionForTest(t, s, draft.ID)
	if err := s.startSupervisionSession(context.Background(), ss.ID, ss.StateVersion, now); err == nil {
		t.Fatal("management Runner identity authorized a foreign actual target")
	}
	assertExecutionClaimForTest(t, s, taskID, "", "")
	if actual := supervisionSessionForTest(t, s, ss.ID); actual.State != "draft" || actual.AdmissionID != "" {
		t.Fatal("failed target binding leaked directory or task ownership", actual.State)
	}
}

func TestSupervisionTargetProfileSaveRechecksObservedDestination(t *testing.T) {
	s, p, _, _, _ := supervisionFixture(t, defaultSupervisionBudget())
	profile := createSupervisionProfileForTest(t, s, p)
	ctx := context.WithValue(supervisionProfileContext(context.Background()), supervisionProfileDestinationKey{}, supervisionProfileDestination{TargetID: "another-target", RunnerID: s.localRunnerID(), ProjectID: p.ID, ConnectionKey: s.supervisionProjectRoute(p).Key})
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := s.saveSupervisionProfileDestinationTx(ctx, tx, profile.CurrentRevisionID, s.localRunnerID()); !errors.Is(err, targetexec.ErrConflict) {
		t.Fatal("a stale network observation created a new credential destination", err)
	}
}

func TestSupervisionTargetOnlyProfilesStayOutOfOrdinaryAdmission(t *testing.T) {
	s, _, _, _, _ := supervisionFixture(t, defaultSupervisionBudget())
	response := httptest.NewRecorder()
	s.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/runners/"+s.localRunnerID()+"/supervision-profiles", strings.NewReader(`{"agentId":"codex","name":"Target only","authMode":"api_key","apiKey":"target-private-value","model":"target-model"}`)))
	if response.Code != http.StatusCreated {
		t.Fatal(response.Code, response.Body.String())
	}
	var result struct {
		Profile  AgentProfile         `json:"profile"`
		Revision AgentProfileRevision `json:"revision"`
	}
	if json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Revision.ExecutionMode != "target_isolated" || strings.Contains(response.Body.String(), "target-private-value") {
		t.Fatal("target profile response invalid", response.Body.String())
	}
	for _, scoped := range []bool{false, true} {
		ctx := context.Background()
		if scoped {
			ctx = supervisionProfileContext(ctx)
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		profile, err := s.runtimeProfileTx(ctx, tx, result.Revision.ID, s.localRunnerID(), "codex")
		tx.Rollback()
		if scoped && (err != nil || profile.Secret != "target-private-value") || !scoped && err == nil {
			t.Fatal("profile scope crossed ordinary admission", scoped, err)
		}
	}
	for _, endpoint := range []string{"agent-profiles", "supervision-profiles"} {
		response = httptest.NewRecorder()
		s.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/runners/"+s.localRunnerID()+"/"+endpoint, nil))
		if response.Code != 200 || strings.Contains(response.Body.String(), result.Profile.ID) != (endpoint == "supervision-profiles") {
			t.Fatal("profile selector crossed scope", endpoint, response.Body.String())
		}
	}
}

func TestSupervisionProfileRevocationPersistsTargetFenceWithRevision(t *testing.T) {
	s, _, _, draft, now := supervisionFixture(t, defaultSupervisionBudget())
	profile := createCLIManagedProfile(t, s, "codex", "test-model")
	if _, err := s.db.Exec(`update supervision_sessions set role_config=? where id=?`, string(mustJSON(supervisionRoleConfigs{"scheduler": {AgentID: "codex", ProfileID: profile.ID}})), draft.ID); err != nil {
		t.Fatal(err)
	}
	ss := startSupervisionForTest(t, s, draft.ID, now)
	if _, err := s.captureSupervisionProjectContext(context.Background(), ss.ID, ss.StateVersion); err != nil {
		t.Fatal(err)
	}
	lease, d := decisionForTest(t, s, ss.ID, time.Now().UTC())
	if _, err := s.claimSupervisionDecisionLaunch(context.Background(), d.ID, lease, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	permit, err := s.prepareSupervisionExecution(context.Background(), d, lease, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	var cancelled bool
	if err := s.admitSupervisionTargetProfile(context.Background(), permit, func() { cancelled = true }); err != nil {
		t.Fatal(err)
	}
	defer s.unregisterProfileRunCancel(permit.RunID)
	response := httptest.NewRecorder()
	s.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/supervision-profile-revisions/"+profile.CurrentRevisionID+"/revoke", nil))
	if response.Code != http.StatusAccepted || !cancelled {
		t.Fatal("revocation did not cancel admitted run", response.Code, response.Body.String())
	}
	ss = supervisionSessionForTest(t, s, ss.ID)
	if ss.Control != "pause" || ss.Reason != "credential_revoked" {
		t.Fatal("revocation lacked durable session fence", ss.Control, ss.Reason)
	}
	err = s.withSupervisionTx(context.Background(), ss.ID, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, _ *supervisionSession) error {
		return s.verifySupervisionTargetProfileTx(context.Background(), tx, permit)
	})
	if err == nil {
		t.Fatal("revoked projection retained dispatch permission")
	}
}
