package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestListOrchestrationBatchesAggregatesPerBatch 覆盖批次列表的作业汇总：每条计划的
// 任务数、完成数和派生状态都必须只看自己名下的作业。这些汇总原先逐批查、现在一次分组查，
// 最容易出的错就是分组键写错导致批次之间串数据或空计划被算成已完成。
func TestListOrchestrationBatchesAggregatesPerBatch(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	now := time.Now().UTC()

	// 每条 job 都要求独占一个 task（task_orchestration_jobs.task_id 唯一）。
	taskIDs := make([]string, 0, 4)
	for _, title := range []string{"completed job", "blocked job", "active job", "draft job"} {
		taskIDs = append(taskIDs, createTaskForTest(t, server.routes(), projectID, title))
	}

	batch := func(id, name string, startedAt *time.Time) {
		if _, err := server.db.Exec(`insert into orchestration_batches (id,project_id,name,conversation_strategy,started_at,created_at,updated_at) values (?,?,?,'new',?,?,?)`, id, projectID, name, startedAt, now, now); err != nil {
			t.Fatalf("insert batch %s: %v", id, err)
		}
	}
	started := now
	batch("batch-completed", "completed", &started)
	batch("batch-blocked", "blocked", &started)
	batch("batch-empty", "empty", &started)
	batch("batch-draft", "draft", nil)

	job := func(id, taskID, batchID, status string) {
		if _, err := server.db.Exec(`insert into task_orchestration_jobs (id,project_id,task_id,batch_id,queue_position,status,policy_snapshot,created_at,updated_at) values (?,?,?,?,1,?,'{}',?,?)`, id, projectID, taskID, batchID, status, now, now); err != nil {
			t.Fatalf("insert job %s: %v", id, err)
		}
	}
	job("job-done", taskIDs[0], "batch-completed", "released_to_main")
	job("job-blocked", taskIDs[1], "batch-blocked", "needs_human")
	job("job-active", taskIDs[2], "batch-blocked", "queued")
	job("job-draft", taskIDs[3], "batch-draft", "queued")

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/orchestration/batches", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("list batches: status=%d body=%s", response.Code, response.Body.String())
	}
	var batches []OrchestrationBatch
	if err := json.NewDecoder(response.Body).Decode(&batches); err != nil {
		t.Fatalf("decode batches: %v", err)
	}
	byID := map[string]OrchestrationBatch{}
	for _, item := range batches {
		byID[item.ID] = item
	}
	if len(byID) != 4 {
		t.Fatalf("listed batches: got %d want 4 (%#v)", len(byID), byID)
	}
	for _, expectation := range []struct {
		id        string
		tasks     int
		completed int
		status    string
	}{
		{"batch-completed", 1, 1, "completed"},
		{"batch-blocked", 2, 0, "needs_human"},
		{"batch-empty", 0, 0, "active"},
		{"batch-draft", 1, 0, "not_started"},
	} {
		got := byID[expectation.id]
		if got.TaskCount != expectation.tasks || got.CompletedCount != expectation.completed || got.Status != expectation.status {
			t.Fatalf("batch %s: tasks=%d completed=%d status=%q, want tasks=%d completed=%d status=%q",
				expectation.id, got.TaskCount, got.CompletedCount, got.Status, expectation.tasks, expectation.completed, expectation.status)
		}
	}
}
