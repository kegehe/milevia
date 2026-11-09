package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func claimSupervisionTasksForTest(t *testing.T, s *Server, projectID, sessionID string, taskIDs ...string) {
	t.Helper()
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := claimSupervisionTasksTx(context.Background(), tx, projectID, sessionID, taskIDs, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func insertOwnershipJobForTest(t *testing.T, s *Server, projectID, taskID, jobID, status string) error {
	t.Helper()
	now := time.Now().UTC()
	_, err := s.db.Exec(`insert into task_orchestration_jobs(id,project_id,task_id,queue_position,status,created_at,updated_at) values (?,?,?,1,?,?,?)`, jobID, projectID, taskID, status, now, now)
	return err
}

func assertExecutionClaimForTest(t *testing.T, s *Server, taskID, kind, ownerID string) {
	t.Helper()
	claim, exists, err := taskExecutionClaim(context.Background(), s.db, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if kind == "" {
		if exists {
			t.Fatalf("unexpected claim: %+v", claim)
		}
		return
	}
	if !exists || claim.OwnerKind != kind || claim.OwnerID != ownerID {
		t.Fatalf("claim=%+v exists=%v want=%s/%s", claim, exists, kind, ownerID)
	}
}

func TestTaskExecutionSelectionIsAtomicAndOwnershipIsIdempotent(t *testing.T) {
	s, projectID, _ := seedTaskConversation(t)
	first := createTaskForTest(t, s.routes(), projectID, "claim first")
	second := createTaskForTest(t, s.routes(), projectID, "claim second")
	claimSupervisionTasksForTest(t, s, projectID, "other-session", second)
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	err = claimSupervisionTasksTx(context.Background(), tx, projectID, "session", []string{first, second}, time.Now().UTC())
	if !errors.Is(err, errTaskExecutionClaimed) {
		t.Fatalf("selection conflict=%v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	assertExecutionClaimForTest(t, s, first, "", "")
	assertExecutionClaimForTest(t, s, second, "supervision", "other-session")
	claimSupervisionTasksForTest(t, s, projectID, "session", first)
	claimSupervisionTasksForTest(t, s, projectID, "session", first)
	var events int
	if err := s.db.QueryRow(`select count(*) from task_execution_claim_events where task_id=?`, first).Scan(&events); err != nil || events != 1 {
		t.Fatalf("idempotent claim events=%d err=%v", events, err)
	}
	tx, err = s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := releaseTaskExecutionTx(context.Background(), tx, first, "supervision", "wrong-owner"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	assertExecutionClaimForTest(t, s, first, "supervision", "session")
}

func TestTaskExecutionSelectionRejectsInvalidScopeAndActiveRun(t *testing.T) {
	s, projectID, conversationID := seedTaskConversation(t)
	taskID := createTaskForTest(t, s.routes(), projectID, "selection validation")
	for _, tc := range []struct {
		name, project string
		ids           []string
	}{
		{"empty", projectID, nil},
		{"duplicate", projectID, []string{taskID, taskID}},
		{"missing", projectID, []string{taskID, "missing"}},
		{"foreign", "another-project", []string{taskID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := s.db.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if err := claimSupervisionTasksTx(context.Background(), tx, tc.project, "session", tc.ids, time.Now().UTC()); err == nil {
				t.Fatal("invalid selection accepted")
			}
		})
		assertExecutionClaimForTest(t, s, taskID, "", "")
	}
	now := time.Now().UTC()
	if _, err := s.db.Exec(`insert into task_runs(id,task_id,conversation_id,sequence,status,prompt_snapshot,acceptance_snapshot,created_at) values ('active',?,?,1,'running','','',?)`, taskID, conversationID, now); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := claimSupervisionTasksTx(context.Background(), tx, projectID, "session", []string{taskID}, now); !errors.Is(err, errTaskExecutionClaimed) {
		t.Fatalf("active run claim=%v", err)
	}
}

func TestTaskExecutionOrchestrationClaimLifecycle(t *testing.T) {
	s, projectID, _ := seedTaskConversation(t)
	taskID := createTaskForTest(t, s.routes(), projectID, "old ownership")
	if err := insertOwnershipJobForTest(t, s, projectID, taskID, "job", "queued"); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"queued", "preparing", "implementing", "checking", "paused", "needs_human", "stopped", "removing", "awaiting_main", "integrated_to_dev"} {
		if _, err := s.db.Exec(`update task_orchestration_jobs set status=? where id='job'`, status); err != nil {
			t.Fatal(err)
		}
		assertExecutionClaimForTest(t, s, taskID, "orchestration", "job")
	}
	if _, err := s.db.Exec(`update task_orchestration_jobs set status='applied_to_branch' where id='job'`); err != nil {
		t.Fatal(err)
	}
	assertExecutionClaimForTest(t, s, taskID, "", "")
	claimSupervisionTasksForTest(t, s, projectID, "session", taskID)
	if _, err := s.db.Exec(`update task_orchestration_jobs set status='queued' where id='job'`); !isTaskExecutionConflict(err) {
		t.Fatalf("requeue stole supervision claim: %v", err)
	}
	// 清理一条已结束的旧记录不能删除后来取得的托管归属。
	if _, err := s.db.Exec(`delete from task_orchestration_jobs where id='job'`); err != nil {
		t.Fatal(err)
	}
	assertExecutionClaimForTest(t, s, taskID, "supervision", "session")
	other := createTaskForTest(t, s.routes(), projectID, "cannot enqueue")
	claimSupervisionTasksForTest(t, s, projectID, "session", other)
	if err := insertOwnershipJobForTest(t, s, projectID, other, "other-job", "queued"); !isTaskExecutionConflict(err) {
		t.Fatalf("enqueue stole supervision claim: %v", err)
	}
	var jobs int
	if err := s.db.QueryRow(`select count(*) from task_orchestration_jobs where task_id=?`, other).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("failed enqueue left job: count=%d err=%v", jobs, err)
	}
}

func TestTaskExecutionSupervisionProtectsMutationsAndBothDependencyEnds(t *testing.T) {
	s, projectID, _ := seedTaskConversation(t)
	owned := createTaskForTest(t, s.routes(), projectID, "owned")
	predecessor := createTaskForTest(t, s.routes(), projectID, "predecessor")
	successor := createTaskForTest(t, s.routes(), projectID, "successor")
	for _, pair := range [][2]string{{owned, predecessor}, {successor, owned}} {
		if _, err := s.db.Exec(`insert into task_dependencies(task_id,predecessor_task_id,created_at) values (?,?,?)`, pair[0], pair[1], time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	claimSupervisionTasksForTest(t, s, projectID, "session", owned)
	for _, tc := range []struct {
		name, method, path string
		body               any
	}{
		{"edit", "PATCH", "/api/tasks/" + owned, map[string]any{"title": "changed", "description": "changed", "priority": "normal"}},
		{"delete", "DELETE", "/api/tasks/" + owned, nil},
		{"dispatch", "POST", "/api/tasks/" + owned + "/dispatch", map[string]any{}},
		{"stop outside supervision", "POST", "/api/tasks/" + owned + "/stop", nil},
		{"add at owned", "POST", "/api/tasks/" + owned + "/dependencies", map[string]any{"predecessorTaskId": successor}},
		{"add from other", "POST", "/api/tasks/" + predecessor + "/dependencies", map[string]any{"predecessorTaskId": owned}},
		{"remove from other", "DELETE", "/api/tasks/" + successor + "/dependencies/" + owned, nil},
		{"replace from other", "PUT", "/api/tasks/" + successor + "/dependencies", map[string]any{"predecessorTaskIds": []string{}}},
		{"replace via patch", "PATCH", "/api/tasks/" + successor, map[string]any{"title": "successor", "description": "new", "priority": "normal", "predecessorTaskIds": []string{}}},
		{"create referring owned", "POST", "/api/projects/" + projectID + "/tasks", map[string]any{"description": "new task", "predecessorTaskIds": []string{owned}}},
		{"delete predecessor", "DELETE", "/api/tasks/" + predecessor, nil},
		{"delete successor", "DELETE", "/api/tasks/" + successor, nil},
		{"delete project", "DELETE", "/api/projects/" + projectID, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, nil)
			if tc.body != nil {
				r = httptest.NewRequest(tc.method, tc.path, jsonBody(t, tc.body))
			}
			w := httptest.NewRecorder()
			s.routes().ServeHTTP(w, r)
			if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "托管") {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
	got, err := s.taskByID(context.Background(), owned)
	if err != nil || got.ExecutionClaim == nil || got.ExecutionClaim.OwnerID != "session" {
		t.Fatalf("ownership projection=%+v err=%v", got.ExecutionClaim, err)
	}
}

func TestTaskExecutionSupervisionExcludesOrdinaryAcceptance(t *testing.T) {
	s, projectID, conversationID := seedTaskConversation(t)
	owned := createTaskForTest(t, s.routes(), projectID, "supervised acceptance")
	ordinary := createTaskForTest(t, s.routes(), projectID, "ordinary acceptance")
	claimSupervisionTasksForTest(t, s, projectID, "session", owned)
	if _, err := s.db.Exec(`update tasks set status='awaiting_review' where id in (?,?)`, owned, ordinary); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, httptest.NewRequest("POST", "/api/projects/"+projectID+"/tasks/review-all", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"skipped":1`) {
		t.Fatalf("batch review=%d %s", w.Code, w.Body.String())
	}
	assertTaskStatus(t, s, owned, taskAwaitingReview)
	assertTaskStatus(t, s, ordinary, taskDone)
	w = httptest.NewRecorder()
	s.routes().ServeHTTP(w, httptest.NewRequest("POST", "/api/tasks/"+owned+"/review", strings.NewReader(`{"action":"accept"}`)))
	if w.Code != http.StatusConflict {
		t.Fatalf("ordinary review=%d %s", w.Code, w.Body.String())
	}
	if _, err := s.db.Exec(`update tasks set status='running' where id=?; update app_preferences set auto_review=1 where id=1`, owned); err != nil {
		t.Fatal(err)
	}
	// 普通成功回调只能保存 Run 事实，不应将托管任务提前变为待验收或 done。
	finishRunningTaskForTest(t, s, projectID, conversationID, owned, "completed")
	assertTaskStatus(t, s, owned, taskRunning)
	assertExecutionClaimForTest(t, s, owned, "supervision", "session")
}

func TestTaskExecutionMigrationBackfillsStoppedAndActiveWithoutInventingHistory(t *testing.T) {
	s, projectID, conversationID := seedTaskConversation(t)
	stopped := createTaskForTest(t, s.routes(), projectID, "migrate stopped")
	completed := createTaskForTest(t, s.routes(), projectID, "migrate completed")
	manual := createTaskForTest(t, s.routes(), projectID, "migrate active")
	if err := insertOwnershipJobForTest(t, s, projectID, stopped, "stopped-job", "stopped"); err != nil {
		t.Fatal(err)
	}
	if err := insertOwnershipJobForTest(t, s, projectID, completed, "completed-job", "released_to_main"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := s.db.Exec(`delete from task_execution_claims;
		insert into task_runs(id,task_id,conversation_id,sequence,status,prompt_snapshot,acceptance_snapshot,created_at) values ('legacy-active',?,?,1,'running','','',?);`, manual, conversationID, now); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.migrateTaskExecution(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	assertExecutionClaimForTest(t, s, stopped, "orchestration", "stopped-job")
	assertExecutionClaimForTest(t, s, completed, "", "")
	assertExecutionClaimForTest(t, s, manual, "manual", "legacy-active")
	var source string
	if err := s.db.QueryRow(`select execution_source from task_runs where id='legacy-active'`).Scan(&source); err != nil || source != "legacy" {
		t.Fatalf("unproven historical source=%q err=%v", source, err)
	}
	var events int
	if err := s.db.QueryRow(`select count(*) from task_execution_claim_events where task_id=?`, manual).Scan(&events); err != nil || events != 1 {
		t.Fatalf("migration duplicated claims: %d %v", events, err)
	}
}

func TestTaskExecutionConcurrentClaimsHaveOneOwner(t *testing.T) {
	s, projectID, _ := seedTaskConversation(t)
	taskID := createTaskForTest(t, s.routes(), projectID, "concurrent ownership")
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, owner := range []string{"session-a", "session-b"} {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			<-start
			tx, err := s.db.BeginTx(context.Background(), nil)
			if err != nil {
				errs <- err
				return
			}
			defer tx.Rollback()
			err = claimSupervisionTasksTx(context.Background(), tx, projectID, owner, []string{taskID}, time.Now().UTC())
			if err == nil {
				err = tx.Commit()
			}
			errs <- err
		}(owner)
	}
	close(start)
	wg.Wait()
	close(errs)
	succeeded, conflicted := 0, 0
	for err := range errs {
		if err == nil {
			succeeded++
		} else if errors.Is(err, errTaskExecutionClaimed) {
			conflicted++
		} else {
			t.Fatal(err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("success=%d conflict=%d", succeeded, conflicted)
	}
}

func TestTaskExecutionDirectoryDoesNotImplyOrchestrationSource(t *testing.T) {
	s, projectID, conversationID := seedTaskConversation(t)
	predecessor := createTaskForTest(t, s.routes(), projectID, "unfinished predecessor")
	taskID := createTaskForTest(t, s.routes(), projectID, "ordinary with directory")
	if err := insertOwnershipJobForTest(t, s, projectID, predecessor, "predecessor-job", "applied_to_branch"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`insert into task_dependencies(task_id,predecessor_task_id,created_at) values (?,?,?)`, taskID, predecessor, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	project, err := s.getProjectByID(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	_, status, err := s.dispatchTaskWithOptions(context.Background(), taskID, taskDispatchOptions{Source: taskSourceManual, WorkspacePath: project.Path, ConversationID: conversationID})
	if status != http.StatusConflict || err == nil {
		t.Fatalf("directory bypassed ordinary dependency: status=%d err=%v", status, err)
	}
	assertExecutionClaimForTest(t, s, taskID, "", "")
	_, status, err = s.dispatchTaskWithOptions(context.Background(), taskID, taskDispatchOptions{Source: taskSourceSupervision})
	if status != http.StatusConflict || err == nil {
		t.Fatalf("incomplete supervision dispatch opened: status=%d err=%v", status, err)
	}
}

func TestTaskExecutionManualAdmissionAndFinishReleaseOnlyItsOwnClaim(t *testing.T) {
	s, projectID, conversationID := seedTaskConversation(t)
	taskID := createTaskForTest(t, s.routes(), projectID, "manual lifecycle")
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	run := TaskRun{ID: "manual-task-run", TaskID: taskID, ConversationID: conversationID}
	if err := admitTaskExecutionTx(context.Background(), tx, &run, taskSourceManual, now); err != nil {
		t.Fatal(err)
	}
	if run.ExecutionSource != taskSourceManual || run.ExecutionOwnerID != run.ID {
		t.Fatalf("source=%s owner=%s", run.ExecutionSource, run.ExecutionOwnerID)
	}
	if _, err := tx.Exec(`insert into runs(id,conversation_id,status,created_at) values ('manual-run',?,'running',?);
		insert into task_runs(id,task_id,conversation_id,run_id,sequence,status,prompt_snapshot,acceptance_snapshot,created_at,execution_source,execution_owner_id)
		values (?,?,?,'manual-run',1,'running','','',?,'manual',?);
		update tasks set status='running' where id=?`, conversationID, now, run.ID, taskID, conversationID, now, run.ID, taskID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	assertExecutionClaimForTest(t, s, taskID, "manual", run.ID)
	if err := insertOwnershipJobForTest(t, s, projectID, taskID, "cannot-steal", "queued"); !isTaskExecutionConflict(err) {
		t.Fatalf("enqueue during manual run=%v", err)
	}
	tx, err = s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for range 2 {
		if err := s.finishTaskRunTx(context.Background(), tx, "manual-run", "completed", "", now); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	assertExecutionClaimForTest(t, s, taskID, "", "")
	assertTaskStatus(t, s, taskID, taskAwaitingReview)
	storedRun, err := s.latestTaskRun(context.Background(), taskID)
	if err != nil || storedRun.ExecutionSource != taskSourceManual || storedRun.ExecutionOwnerID != run.ID {
		t.Fatalf("stored run source=%s owner=%s err=%v", storedRun.ExecutionSource, storedRun.ExecutionOwnerID, err)
	}
	var releases int
	if err := s.db.QueryRow(`select count(*) from task_execution_claim_events where task_id=? and type='released'`, taskID).Scan(&releases); err != nil || releases != 1 {
		t.Fatalf("release events=%d err=%v", releases, err)
	}
}

func TestTaskExecutionClaimAuditRollsBackWithFailedAdmission(t *testing.T) {
	s, projectID, _ := seedTaskConversation(t)
	taskID := createTaskForTest(t, s.routes(), projectID, "rollback admission")
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := claimTaskExecutionTx(context.Background(), tx, taskID, "manual", "attempt", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	assertExecutionClaimForTest(t, s, taskID, "", "")
	var count int
	if err := s.db.QueryRow(`select count(*) from task_execution_claim_events where task_id=?`, taskID).Scan(&count); err != nil || count != 0 {
		t.Fatal(fmt.Errorf("rolled-back audit count=%d: %w", count, err))
	}
}

func TestTaskExecutionSourceSurvivesDispatchWithoutWorkspaceOverride(t *testing.T) {
	s, projectID, conversationID := seedTaskConversation(t)
	release := make(chan struct{})
	var releaseOnce sync.Once
	finish := func() { releaseOnce.Do(func() { close(release) }) }
	defer finish()
	s.runner = runnerFunc(func(ctx context.Context, _ AgentRunRequest, _ AgentRunSink) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	s.wslRunner = s.runner
	predecessor := createTaskForTest(t, s.routes(), projectID, "integrated predecessor")
	taskID := createTaskForTest(t, s.routes(), projectID, "explicit orchestration source")
	if err := insertOwnershipJobForTest(t, s, projectID, predecessor, "integrated-job", "applied_to_branch"); err != nil {
		t.Fatal(err)
	}
	if err := insertOwnershipJobForTest(t, s, projectID, taskID, "implementation-job", "preparing"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`insert into task_dependencies(task_id,predecessor_task_id,created_at) values (?,?,?)`, taskID, predecessor, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	result, status, err := s.dispatchTaskWithOptions(context.Background(), taskID, taskDispatchOptions{Source: taskSourceOrchestration, ConversationID: conversationID})
	if err != nil || status != http.StatusAccepted {
		t.Fatalf("explicit source without directory override: status=%d err=%v", status, err)
	}
	stored, err := s.latestTaskRun(context.Background(), taskID)
	if err != nil || stored.RunID != result.RunID || stored.ExecutionSource != taskSourceOrchestration || stored.ExecutionOwnerID != "implementation-job" {
		t.Fatalf("persisted dispatch=%+v err=%v", stored, err)
	}
	assertExecutionClaimForTest(t, s, taskID, "orchestration", "implementation-job")
	finish()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		stored, err = s.latestTaskRun(context.Background(), taskID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Status == "succeeded" {
			assertExecutionClaimForTest(t, s, taskID, "orchestration", "implementation-job")
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run did not finish: %+v", stored)
}

func TestTaskExecutionIntentCannotCrossTaskOwners(t *testing.T) {
	s, projectID, _ := seedTaskConversation(t)
	first := createTaskForTest(t, s.routes(), projectID, "first intent owner")
	second := createTaskForTest(t, s.routes(), projectID, "second intent owner")
	for _, pair := range [][2]string{{first, "first-job"}, {second, "second-job"}} {
		if err := insertOwnershipJobForTest(t, s, projectID, pair[0], pair[1], "preparing"); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	if _, err := s.db.Exec(`insert into task_execution_intents(id,job_id,phase,attempt,status,created_at,updated_at) values ('first-intent','first-job','implementation',1,'pending',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	for _, source := range []taskExecutionSource{taskSourceManual, taskSourceOrchestration} {
		tx, err := s.db.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		run := TaskRun{ID: "incorrect-run", TaskID: second, ExecutionIntentID: "first-intent"}
		err = admitTaskExecutionTx(context.Background(), tx, &run, source, now)
		tx.Rollback()
		if err == nil {
			t.Fatalf("source %s accepted another task's intent", source)
		}
	}
	assertExecutionClaimForTest(t, s, first, "orchestration", "first-job")
	assertExecutionClaimForTest(t, s, second, "orchestration", "second-job")
}

func TestTaskExecutionMigrationSourceRequiresUniqueMatchingIntent(t *testing.T) {
	s, projectID, conversationID := seedTaskConversation(t)
	now := time.Now().UTC()
	for _, name := range []string{"unique", "ambiguous", "foreign"} {
		taskID := createTaskForTest(t, s.routes(), projectID, "historical "+name)
		jobID, runID := name+"-job", name+"-run"
		if err := insertOwnershipJobForTest(t, s, projectID, taskID, jobID, "released_to_main"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`insert into runs(id,conversation_id,status,created_at) values (?,?,'completed',?);
			insert into task_runs(id,task_id,conversation_id,run_id,sequence,status,prompt_snapshot,acceptance_snapshot,created_at) values (?,?,?,?,1,'succeeded','','',?)`,
			runID, conversationID, now, name+"-task-run", taskID, conversationID, runID, now); err != nil {
			t.Fatal(err)
		}
		intentJob := jobID
		if name == "foreign" {
			intentJob = "unique-job"
		}
		if _, err := s.db.Exec(`insert into task_execution_intents(id,job_id,phase,attempt,run_id,status,created_at,updated_at) values (?,?,?,1,?,'completed',?,?)`, name+"-intent", intentJob, name, runID, now, now); err != nil {
			t.Fatal(err)
		}
		if name == "ambiguous" {
			if _, err := s.db.Exec(`insert into task_execution_intents(id,job_id,phase,attempt,run_id,status,created_at,updated_at) values ('second-intent',?,'review',1,?,'completed',?,?)`, jobID, runID, now, now); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.migrateTaskExecution(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"unique", "ambiguous", "foreign"} {
		var source, owner string
		if err := s.db.QueryRow(`select execution_source,execution_owner_id from task_runs where id=?`, name+"-task-run").Scan(&source, &owner); err != nil {
			t.Fatal(err)
		}
		wantSource, wantOwner := "legacy", ""
		if name == "unique" {
			wantSource, wantOwner = "orchestration", "unique-job"
		}
		if source != wantSource || owner != wantOwner {
			t.Fatalf("%s source=%s owner=%s want=%s/%s", name, source, owner, wantSource, wantOwner)
		}
	}
}

func TestTaskExecutionRecoveryReleasesManualButKeepsStoppedOrchestration(t *testing.T) {
	s, projectID, conversationID := seedTaskConversation(t)
	manual := createTaskForTest(t, s.routes(), projectID, "interrupted manual")
	stopped := createTaskForTest(t, s.routes(), projectID, "stopped owner survives")
	if err := insertOwnershipJobForTest(t, s, projectID, stopped, "stopped-owner", "stopped"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := claimTaskExecutionTx(context.Background(), tx, manual, "manual", "interrupted-task-run", now); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`insert into runs(id,conversation_id,status,created_at) values ('interrupted-run',?,'running',?);
		insert into task_runs(id,task_id,conversation_id,run_id,sequence,status,prompt_snapshot,acceptance_snapshot,created_at,execution_source,execution_owner_id)
		values ('interrupted-task-run',?,?,'interrupted-run',1,'running','','',?,'manual','interrupted-task-run');
		update tasks set status='running' where id=?`, conversationID, now, manual, conversationID, now, manual); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.recoverInterruptedRuns(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	assertExecutionClaimForTest(t, s, manual, "", "")
	assertExecutionClaimForTest(t, s, stopped, "orchestration", "stopped-owner")
	assertTaskStatus(t, s, manual, taskActionRequired)
	run, err := s.latestTaskRun(context.Background(), manual)
	if err != nil || run.Status != "interrupted" || run.ExecutionSource != taskSourceManual {
		t.Fatalf("recovered run=%+v err=%v", run, err)
	}
}
