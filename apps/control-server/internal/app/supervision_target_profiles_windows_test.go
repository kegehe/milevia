package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSupervisionProfileRevocationPersistsRemoteAuthorizationVersion(t *testing.T) {
	s, permit, _ := supervisionTargetControlFixture(t)
	profile := createCLIManagedProfile(t, s, permit.AgentID, "test-model")
	if _, err := s.db.Exec(`update runs set agent_profile_revision_id=? where id=?`, profile.CurrentRevisionID, permit.RunID); err != nil {
		t.Fatal(err)
	}
	before, err := loadSupervisionTargetControl(context.Background(), s.db, permit.Decision.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	s.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/supervision-profile-revisions/"+profile.CurrentRevisionID+"/revoke", nil))
	after, err := loadSupervisionTargetControl(context.Background(), s.db, permit.Decision.SessionID)
	if response.Code != http.StatusAccepted || err != nil || after.Control.State != "paused" || after.Control.Version != before.Control.Version+1 || after.Control.DrainExecutionID != "" || after.Wire != nil {
		t.Fatal("revocation did not persist an immediate target fence", response.Body.String(), after, err)
	}
}
