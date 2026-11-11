package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInsightReleaseReviewBulkTaskRelayFitsFrame(t *testing.T) {
	s := newTestServer(t)
	p := insightTestProject(t, s)
	const count = 40
	for i := 0; i < count; i++ {
		f := insightCompletionSeed(t, s, p, fmt.Sprintf("large-%d", i), fmt.Sprintf("问题%d", i))
		if _, err := s.db.Exec(`update project_insights set evidence=?,acceptance_criteria=? where id=?`,
			strings.Repeat("据", 2000), strings.Repeat("验", 2000), f.ID); err != nil {
			t.Fatal(err)
		}
	}
	status, reply := relayRPC(t, s, map[string]any{"op": "insights.toTask", "projectId": p})
	if status != http.StatusOK || reply["ok"] != true {
		t.Fatalf("bulk conversion failed: %d %v", status, reply)
	}
	encoded, err := json.Marshal(reply)
	if err != nil || len(encoded) > 320<<10 {
		t.Fatalf("successful write response cannot traverse mobile relay: bytes=%d err=%v", len(encoded), err)
	}
	data := reply["data"].(map[string]any)
	if data["created"] != float64(count) || data["failed"] != float64(0) {
		t.Fatalf("conversion totals lost: %v", data)
	}
	var tasks, findings int
	if err := s.db.QueryRow(`select count(*) from tasks where project_id=? and description like ?`, p, "%"+strings.Repeat("验", 2000)+"%").Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`select count(*) from project_insights where project_id=?`, p).Scan(&findings); err != nil {
		t.Fatal(err)
	}
	if tasks != count || findings != 0 {
		t.Fatalf("conversion lost details or atomic deletion: tasks=%d findings=%d", tasks, findings)
	}
	if _, exists := data["tasks"]; exists {
		t.Fatal("mobile bulk response must omit task bodies")
	}
	// 桌面的原有响应契约保留，不能因手机容量限制丢掉 tasks。
	insightCompletionSeed(t, s, p, "desktop", "桌面任务")
	response := httptest.NewRecorder()
	s.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/"+p+"/insights/to-task", strings.NewReader(`{}`)))
	var desktop struct {
		Created int    `json:"created"`
		Tasks   []Task `json:"tasks"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &desktop); err != nil || response.Code != http.StatusCreated || desktop.Created != 1 || len(desktop.Tasks) != 1 || desktop.Tasks[0].Title != "桌面任务" {
		t.Fatalf("desktop contract changed: %d %+v %v", response.Code, desktop, err)
	}
}
