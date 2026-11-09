package app

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestSupervisionArchiveProtectsActiveSessionsAndRetainsHistory(t *testing.T) {
	s, p, _, ss, _ := supervisionFixture(t, defaultSupervisionBudget())
	path := "/api/supervision/" + ss.ID + "/archive"
	archive := `{"stateVersion":1,"archived":true}`
	if response := supervisionRequestForTest(s, "PUT", path, "archive-once", archive); response.Code != 409 {
		t.Fatalf("active session archived: %d %s", response.Code, response.Body.String())
	}
	if response := supervisionRequestForTest(s, "POST", "/api/supervision/"+ss.ID+"/control", "stop", `{"stateVersion":1,"action":"stop"}`); response.Code != 202 {
		t.Fatal(response.Code, response.Body.String())
	}
	archive = `{"stateVersion":2,"archived":true}`
	for i := 0; i < 2; i++ {
		if response := supervisionRequestForTest(s, "PUT", path, "archive-once", archive); response.Code != 204 {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	got := supervisionSessionForTest(t, s, ss.ID)
	if got.ArchivedAt == nil || got.State != "stopped" || got.StateVersion != 3 {
		t.Fatalf("archive changed execution state or replayed: %+v", got)
	}
	for _, query := range []string{"", "?includeArchived=1"} {
		response := supervisionRequestForTest(s, "GET", "/api/projects/"+p.ID+"/supervision"+query, "", "")
		var sessions []supervisionSession
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &sessions) != nil {
			t.Fatal(response.Code, response.Body.String())
		}
		if (len(sessions) == 1) != (query != "") {
			t.Fatalf("archive filter %q: %+v", query, sessions)
		}
	}
	response := supervisionRequestForTest(s, "GET", "/api/supervision/"+ss.ID, "", "")
	var detail struct {
		Session supervisionSession `json:"session"`
		Tasks   []map[string]any   `json:"tasks"`
		Events  []supervisionEvent `json:"events"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &detail) != nil || len(detail.Tasks) != 1 || len(detail.Events) == 0 || detail.Session.ArchivedAt == nil {
		t.Fatal("archive lost detail history", response.Code, response.Body.String())
	}
	if response := supervisionRequestForTest(s, "PUT", path, "unarchive-stale", `{"stateVersion":2,"archived":false}`); response.Code != 409 {
		t.Fatal("stale request restored session", response.Code)
	}
	if response := supervisionRequestForTest(s, "PUT", path, "unarchive", fmt.Sprintf(`{"stateVersion":%d,"archived":false}`, got.StateVersion)); response.Code != 204 {
		t.Fatal(response.Code, response.Body.String())
	}
	got = supervisionSessionForTest(t, s, ss.ID)
	if got.ArchivedAt != nil || got.State != "stopped" || got.StateVersion != 4 {
		t.Fatalf("unarchive resumed execution: %+v", got)
	}
}
