package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func seedScheduledTaskProject(t *testing.T, server *Server) string {
	t.Helper()
	projectID := "scheduled-project"
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values (?,?,?,?,?,?,?)`, projectID, "Scheduled project", t.TempDir(), server.localRunnerID(), "main", true, time.Now().UTC()); err != nil {
		t.Fatalf("insert scheduled project: %v", err)
	}
	return projectID
}

func TestScheduledTaskCreateListAndManualRun(t *testing.T) {
	server := newTestServer(t)
	server.runner = runnerFunc(func(_ context.Context, _ AgentRunRequest, sink AgentRunSink) error {
		sink.AssistantText("scheduled output", "")
		return nil
	})
	server.runnerRegistry.register(server.localRunnerID(), server.runner, server.localRunnerMeta())
	projectID := seedScheduledTaskProject(t, server)

	payload := map[string]any{
		"title":          "Nightly review",
		"prompt":         "Review the project and report risks.",
		"agentId":        "claude-code",
		"permissionMode": "full_control",
		"fullControlConfirmed": true,
		"scheduleType":   "daily",
		"timezone":       "Asia/Shanghai",
		"timeOfDay":      "23:30",
		"enabled":        true,
	}
	created := httptest.NewRecorder()
	server.routes().ServeHTTP(created, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/scheduled-tasks", jsonBody(t, payload)))
	if created.Code != http.StatusCreated {
		t.Fatalf("create scheduled task: status=%d body=%s", created.Code, created.Body.String())
	}
	var task ScheduledTask
	if err := json.NewDecoder(created.Body).Decode(&task); err != nil {
		t.Fatalf("decode scheduled task: %v", err)
	}
	if task.ID == "" || task.NextRunAt == nil {
		t.Fatalf("created scheduled task missing identity or next run: %#v", task)
	}

	list := httptest.NewRecorder()
	server.routes().ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/scheduled-tasks", nil))
	if list.Code != http.StatusOK || !bytes.Contains(list.Body.Bytes(), []byte("Nightly review")) {
		t.Fatalf("list scheduled tasks: status=%d body=%s", list.Code, list.Body.String())
	}

	run := httptest.NewRecorder()
	server.routes().ServeHTTP(run, httptest.NewRequest(http.MethodPost, "/api/scheduled-tasks/"+task.ID+"/run", nil))
	if run.Code != http.StatusAccepted {
		t.Fatalf("run scheduled task: status=%d body=%s", run.Code, run.Body.String())
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runs, err := server.scheduledTaskRuns(context.Background(), task.ID, 1)
		if err == nil && len(runs) == 1 && runs[0].Status == scheduledRunSucceeded && runs[0].ConversationID != "" && runs[0].RunID != "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	runs, err := server.scheduledTaskRuns(context.Background(), task.ID, 1)
	t.Fatalf("scheduled task did not complete: runs=%#v err=%v", runs, err)
}

func TestScheduledTaskFullControlRequiresExplicitConfirmation(t *testing.T) {
	server := newTestServer(t)
	projectID := seedScheduledTaskProject(t, server)
	payload := map[string]any{
		"title":          "Unsafe scheduled run",
		"prompt":         "Run without approval.",
		"agentId":        "claude-code",
		"permissionMode": "full_control",
		"scheduleType":   "daily",
		"timezone":       "UTC",
		"timeOfDay":      "23:30",
		"enabled":        true,
	}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/scheduled-tasks", jsonBody(t, payload)))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unconfirmed full_control schedule: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestDueScheduledTaskClaimsAndLaunchesWhileServiceIsRunning(t *testing.T) {
	server := newTestServer(t)
	server.runner = runnerFunc(func(_ context.Context, _ AgentRunRequest, sink AgentRunSink) error {
		sink.AssistantText("scheduled output", "")
		return nil
	})
	server.runnerRegistry.register(server.localRunnerID(), server.runner, server.localRunnerMeta())
	projectID := seedScheduledTaskProject(t, server)
	now := time.Now().UTC()
	due := now.Add(-time.Second)
	task := ScheduledTask{ID: "due-daily", ProjectID: projectID, Title: "Due daily", Prompt: "Check", AgentID: "claude-code", PermissionMode: "full_control", ScheduleType: scheduledTaskDaily, Timezone: "UTC", TimeOfDay: now.Format("15:04"), Skills: []string{}, Weekdays: []int{}, Enabled: true, NextRunAt: &due, CreatedAt: now, UpdatedAt: now}
	if err := server.insertScheduledTask(context.Background(), task); err != nil {
		t.Fatalf("insert due scheduled task: %v", err)
	}
	server.runDueScheduledTasks(context.Background(), now)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runs, err := server.scheduledTaskRuns(context.Background(), task.ID, 1)
		if err == nil && len(runs) == 1 && runs[0].Status == scheduledRunSucceeded {
			updated, getErr := server.scheduledTaskByID(context.Background(), task.ID)
			if getErr != nil || !updated.Enabled || updated.NextRunAt == nil || !updated.NextRunAt.After(now) {
				t.Fatalf("repeating task did not advance after a due run: task=%#v err=%v", updated, getErr)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	runs, err := server.scheduledTaskRuns(context.Background(), task.ID, 1)
	t.Fatalf("due scheduled task did not complete: runs=%#v err=%v", runs, err)
}

func TestSkipMissedScheduledTasksDoesNotReplayWhileMileviaWasClosed(t *testing.T) {
	server := newTestServer(t)
	projectID := seedScheduledTaskProject(t, server)
	now := time.Now().UTC()
	past := now.Add(-time.Minute)

	daily := ScheduledTask{ID: "daily", ProjectID: projectID, Title: "Daily", Prompt: "Check", AgentID: "claude-code", PermissionMode: "full_control", ScheduleType: scheduledTaskDaily, Timezone: "UTC", TimeOfDay: now.Add(-time.Minute).Format("15:04"), Skills: []string{}, Weekdays: []int{}, Enabled: true, NextRunAt: &past, CreatedAt: now, UpdatedAt: now}
	if err := server.insertScheduledTask(context.Background(), daily); err != nil {
		t.Fatalf("insert daily scheduled task: %v", err)
	}
	oneTime := ScheduledTask{ID: "once", ProjectID: projectID, Title: "Once", Prompt: "Check", AgentID: "claude-code", PermissionMode: "full_control", ScheduleType: scheduledTaskOnce, Timezone: "UTC", RunAt: &past, Skills: []string{}, Weekdays: []int{}, Enabled: true, NextRunAt: &past, CreatedAt: now, UpdatedAt: now}
	if err := server.insertScheduledTask(context.Background(), oneTime); err != nil {
		t.Fatalf("insert one-time scheduled task: %v", err)
	}

	if err := server.skipMissedScheduledTasks(context.Background(), now); err != nil {
		t.Fatalf("skip missed scheduled tasks: %v", err)
	}
	updatedDaily, err := server.scheduledTaskByID(context.Background(), daily.ID)
	if err != nil || updatedDaily.NextRunAt == nil || !updatedDaily.NextRunAt.After(now) || !updatedDaily.Enabled {
		t.Fatalf("daily task should advance without replay: task=%#v err=%v", updatedDaily, err)
	}
	updatedOnce, err := server.scheduledTaskByID(context.Background(), oneTime.ID)
	if err != nil || updatedOnce.Enabled || updatedOnce.NextRunAt != nil {
		t.Fatalf("missed one-time task should be disabled: task=%#v err=%v", updatedOnce, err)
	}
	claimed, err := server.claimDueScheduledTaskRuns(context.Background(), now)
	if err != nil || len(claimed) != 0 {
		t.Fatalf("missed schedules must not be replayed: claimed=%#v err=%v", claimed, err)
	}
}

func TestScheduledTaskValidatesTimeAndWeekdays(t *testing.T) {
	now := time.Now().UTC()
	_, err := scheduledTaskFromInput("project", scheduledTaskInput{Title: "Weekly", Prompt: "Check", AgentID: "claude-code", PermissionMode: "full_control", ScheduleType: scheduledTaskWeekly, Timezone: "UTC", TimeOfDay: "09:00", Weekdays: []int{1, 8}}, now)
	if err == nil {
		t.Fatal("invalid weekday should be rejected")
	}
	_, err = scheduledTaskFromInput("project", scheduledTaskInput{Title: "Weekly", Prompt: "Check", AgentID: "claude-code", PermissionMode: "full_control", ScheduleType: scheduledTaskWeekly, Timezone: "UTC", TimeOfDay: "25:00", Weekdays: []int{1}}, now)
	if err == nil {
		t.Fatal("invalid weekly time should be rejected")
	}
	_, err = scheduledTaskFromInput("project", scheduledTaskInput{Title: "Once", Prompt: "Check", AgentID: "claude-code", PermissionMode: "full_control", ScheduleType: scheduledTaskOnce, Timezone: "UTC", RunAt: now.Add(15 * time.Second).Format(time.RFC3339)}, now)
	if err != nil {
		t.Fatalf("future one-time schedule should be accepted: %v", err)
	}
}

func TestScheduledTaskUsesConfiguredTimezoneForLocalOneTimeInput(t *testing.T) {
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	task, err := scheduledTaskFromInput("project", scheduledTaskInput{Title: "Once", Prompt: "Check", AgentID: "claude-code", PermissionMode: "full_control", ScheduleType: scheduledTaskOnce, Timezone: "America/New_York", RunAt: "2026-04-10T09:30"}, now)
	if err != nil {
		t.Fatalf("parse local one-time input: %v", err)
	}
	want := time.Date(2026, time.April, 10, 13, 30, 0, 0, time.UTC)
	if task.RunAt == nil || !task.RunAt.Equal(want) {
		t.Fatalf("runAt = %v, want %v", task.RunAt, want)
	}
	_, err = scheduledTaskFromInput("project", scheduledTaskInput{Title: "Once", Prompt: "Check", AgentID: "claude-code", PermissionMode: "full_control", ScheduleType: scheduledTaskOnce, Timezone: "America/New_York", RunAt: "2026-03-08T02:30"}, now)
	if err == nil {
		t.Fatal("nonexistent daylight-saving local time should be rejected")
	}
}

func TestMigrateScheduledTasksAddsExecutionSnapshotsToExistingRuns(t *testing.T) {
	server := newTestServer(t)
	server.runtimeStop()
	server.runWG.Wait()
	if _, err := server.db.Exec(`drop table scheduled_task_runs`); err != nil {
		t.Fatalf("drop scheduled task runs: %v", err)
	}
	if _, err := server.db.Exec(`create table scheduled_task_runs (
		id text primary key,
		scheduled_task_id text not null,
		scheduled_for datetime not null,
		status text not null,
		prompt_snapshot text not null,
		skills_snapshot text not null default '[]',
		conversation_id text not null default '',
		run_id text not null default '',
		failure_reason text not null default '',
		created_at datetime not null,
		started_at datetime,
		finished_at datetime
	)`); err != nil {
		t.Fatalf("create legacy scheduled task runs: %v", err)
	}
	if err := server.migrateScheduledTasks(context.Background()); err != nil {
		t.Fatalf("migrate legacy scheduled task runs: %v", err)
	}
	rows, err := server.db.Query(`pragma table_info(scheduled_task_runs)`)
	if err != nil {
		t.Fatalf("read scheduled task runs schema: %v", err)
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatalf("scan scheduled task runs schema: %v", err)
		}
		columns[name] = true
	}
	for _, name := range []string{"title_snapshot", "agent_id_snapshot", "permission_mode_snapshot", "profile_revision_snapshot"} {
		if !columns[name] {
			t.Fatalf("migration did not add %s", name)
		}
	}
}

func TestQueuedScheduledTaskKeepsItsExecutionSnapshot(t *testing.T) {
	server := newTestServer(t)
	server.runner = runnerFunc(func(_ context.Context, _ AgentRunRequest, sink AgentRunSink) error {
		sink.AssistantText("scheduled output", "")
		return nil
	})
	server.runnerRegistry.register(server.localRunnerID(), server.runner, server.localRunnerMeta())
	projectID := seedScheduledTaskProject(t, server)
	profile := createCLIManagedProfile(t, server, "claude-code", "claude-before-queue")
	if _, err := server.db.Exec(`update projects set default_profile_id=? where id=?`, profile.ID, projectID); err != nil {
		t.Fatalf("set scheduled project profile: %v", err)
	}
	now := time.Now().UTC()
	next := now.Add(time.Hour)
	task := ScheduledTask{ID: "snapshot", ProjectID: projectID, Title: "Original title", Prompt: "Use the original configuration.", AgentID: "claude-code", PermissionMode: "full_control", ScheduleType: scheduledTaskDaily, Timezone: "UTC", TimeOfDay: next.Format("15:04"), Skills: []string{}, Weekdays: []int{}, Enabled: true, NextRunAt: &next, CreatedAt: now, UpdatedAt: now}
	if err := server.insertScheduledTask(context.Background(), task); err != nil {
		t.Fatalf("insert scheduled task: %v", err)
	}
	queued, err := server.enqueueScheduledTaskRun(context.Background(), task, now)
	if err != nil {
		t.Fatalf("enqueue scheduled task: %v", err)
	}
	if queued.AgentIDSnapshot != "claude-code" || queued.PermissionModeSnapshot != "full_control" || queued.TitleSnapshot != "Original title" || queued.ProfileRevisionSnapshot != profile.CurrentRevisionID {
		t.Fatalf("unexpected run snapshot: %#v", queued)
	}
	updatedProfile := httptest.NewRecorder()
	server.routes().ServeHTTP(updatedProfile, httptest.NewRequest(http.MethodPatch, "/api/agent-profiles/"+profile.ID, bytes.NewBufferString(`{"model":"claude-after-queue"}`)))
	if updatedProfile.Code != http.StatusOK {
		t.Fatalf("update agent profile: status=%d body=%s", updatedProfile.Code, updatedProfile.Body.String())
	}
	var latestProfileRevision string
	if err := server.db.QueryRow(`select current_revision_id from agent_profiles where id=?`, profile.ID).Scan(&latestProfileRevision); err != nil {
		t.Fatalf("read updated profile: %v", err)
	}
	if latestProfileRevision == profile.CurrentRevisionID {
		t.Fatal("profile update did not create a new revision")
	}
	task.Title = "Updated title"
	task.AgentID = "codex"
	task.PermissionMode = "workspace_write"
	task.UpdatedAt = now.Add(time.Minute)
	if err := server.replaceScheduledTask(context.Background(), task); err != nil {
		t.Fatalf("update scheduled task: %v", err)
	}
	server.launchScheduledTaskRun(context.Background(), queued.ID)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		run, err := server.scheduledTaskRunByID(context.Background(), queued.ID)
		if err == nil && run.Status == scheduledRunSucceeded && run.ConversationID != "" {
			var agentID, permissionMode, title, profileRevisionID string
			if err := server.db.QueryRow(`select agent_id,permission_mode,title,agent_profile_revision_id from conversations where id=?`, run.ConversationID).Scan(&agentID, &permissionMode, &title, &profileRevisionID); err != nil {
				t.Fatalf("read scheduled conversation: %v", err)
			}
			if agentID != "claude-code" || permissionMode != "full_control" || title != "Scheduled: Original title" || profileRevisionID != profile.CurrentRevisionID {
				t.Fatalf("queued run used edited configuration: agent=%q permission=%q title=%q profile=%q", agentID, permissionMode, title, profileRevisionID)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("queued scheduled task did not complete")
}

func TestPauseScheduledTaskStopsQueuedRun(t *testing.T) {
	server := newTestServer(t)
	projectID := seedScheduledTaskProject(t, server)
	now := time.Now().UTC()
	next := now.Add(time.Hour)
	task := ScheduledTask{ID: "pause-queued", ProjectID: projectID, Title: "Pause queued", Prompt: "Check", AgentID: "claude-code", PermissionMode: "full_control", ScheduleType: scheduledTaskDaily, Timezone: "UTC", TimeOfDay: next.Format("15:04"), Skills: []string{}, Weekdays: []int{}, Enabled: true, NextRunAt: &next, CreatedAt: now, UpdatedAt: now}
	if err := server.insertScheduledTask(context.Background(), task); err != nil {
		t.Fatalf("insert scheduled task: %v", err)
	}
	queued, err := server.enqueueScheduledTaskRun(context.Background(), task, now)
	if err != nil {
		t.Fatalf("enqueue scheduled task: %v", err)
	}
	conversation, err := server.createScheduledTaskConversation(context.Background(), mustScheduledProject(t, server, projectID), task, queued)
	if err != nil {
		t.Fatalf("create queued scheduled conversation: %v", err)
	}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/scheduled-tasks/"+task.ID+"/pause", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("pause scheduled task: status=%d body=%s", response.Code, response.Body.String())
	}
	run, err := server.scheduledTaskRunByID(context.Background(), queued.ID)
	if err != nil || run.Status != scheduledRunStopped || run.RunID != "" {
		t.Fatalf("queued run should be stopped after pause: run=%#v err=%v", run, err)
	}
	var conversations int
	if err := server.db.QueryRow(`select count(*) from conversations where id=?`, conversation.ID).Scan(&conversations); err != nil || conversations != 0 {
		t.Fatalf("pause should remove the queued run's empty conversation: count=%d err=%v", conversations, err)
	}
	server.runDueScheduledTasks(context.Background(), time.Now().UTC())
	run, err = server.scheduledTaskRunByID(context.Background(), queued.ID)
	if err != nil || run.Status != scheduledRunStopped {
		t.Fatalf("paused queued run should not launch: run=%#v err=%v", run, err)
	}
}

func TestUpdateScheduledTaskDisablingStopsQueuedRun(t *testing.T) {
	server := newTestServer(t)
	projectID := seedScheduledTaskProject(t, server)
	now := time.Now().UTC()
	next := now.Add(time.Hour)
	task := ScheduledTask{ID: "update-disable-queued", ProjectID: projectID, Title: "Update disable", Prompt: "Check", AgentID: "claude-code", PermissionMode: "full_control", ScheduleType: scheduledTaskDaily, Timezone: "UTC", TimeOfDay: next.Format("15:04"), Skills: []string{}, Weekdays: []int{}, Enabled: true, NextRunAt: &next, CreatedAt: now, UpdatedAt: now}
	if err := server.insertScheduledTask(context.Background(), task); err != nil {
		t.Fatalf("insert scheduled task: %v", err)
	}
	queued, err := server.enqueueScheduledTaskRun(context.Background(), task, now)
	if err != nil {
		t.Fatalf("enqueue scheduled task: %v", err)
	}
	payload := map[string]any{
		"title":          task.Title,
		"prompt":         task.Prompt,
		"skills":         task.Skills,
		"agentId":        task.AgentID,
		"permissionMode": task.PermissionMode,
		"scheduleType":   task.ScheduleType,
		"timezone":       task.Timezone,
		"timeOfDay":      task.TimeOfDay,
		"weekdays":       task.Weekdays,
		"enabled":        false,
	}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/api/scheduled-tasks/"+task.ID, jsonBody(t, payload)))
	if response.Code != http.StatusOK {
		t.Fatalf("disable scheduled task through update: status=%d body=%s", response.Code, response.Body.String())
	}
	run, err := server.scheduledTaskRunByID(context.Background(), queued.ID)
	if err != nil || run.Status != scheduledRunStopped || run.RunID != "" {
		t.Fatalf("update-disable should stop queued run: run=%#v err=%v", run, err)
	}
	server.runDueScheduledTasks(context.Background(), time.Now().UTC())
	run, err = server.scheduledTaskRunByID(context.Background(), queued.ID)
	if err != nil || run.Status != scheduledRunStopped {
		t.Fatalf("stopped run should not launch: run=%#v err=%v", run, err)
	}
}

func TestUpdateAllowsEditingAnExpiredDisabledOneTimeTask(t *testing.T) {
	server := newTestServer(t)
	projectID := seedScheduledTaskProject(t, server)
	now := time.Now().UTC()
	past := now.Add(-time.Hour).Truncate(time.Minute)
	task := ScheduledTask{ID: "expired-once", ProjectID: projectID, Title: "Expired", Prompt: "Original", AgentID: "claude-code", PermissionMode: "full_control", ScheduleType: scheduledTaskOnce, Timezone: "UTC", RunAt: &past, Skills: []string{}, Weekdays: []int{}, Enabled: false, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)}
	if err := server.insertScheduledTask(context.Background(), task); err != nil {
		t.Fatalf("insert expired scheduled task: %v", err)
	}
	payload := map[string]any{
		"title":          "Expired updated",
		"prompt":         "Updated without rescheduling.",
		"skills":         []string{},
		"agentId":        "claude-code",
		"permissionMode": "full_control",
		"scheduleType":   "once",
		"timezone":       "UTC",
		"runAt":          past.Format("2006-01-02T15:04"),
		"weekdays":       []int{},
		"enabled":        false,
	}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/api/scheduled-tasks/"+task.ID, jsonBody(t, payload)))
	if response.Code != http.StatusOK {
		t.Fatalf("update expired disabled one-time task: status=%d body=%s", response.Code, response.Body.String())
	}
	updated, err := server.scheduledTaskByID(context.Background(), task.ID)
	if err != nil || updated.Title != "Expired updated" || updated.Enabled || updated.NextRunAt != nil {
		t.Fatalf("expired task update was not retained: task=%#v err=%v", updated, err)
	}
}

func TestScheduledConversationStaysReadOnlyAfterTaskDeletion(t *testing.T) {
	server := newTestServer(t)
	projectID := seedScheduledTaskProject(t, server)
	now := time.Now().UTC()
	next := now.Add(time.Hour)
	task := ScheduledTask{ID: "delete-scheduled-history", ProjectID: projectID, Title: "Deleted history", Prompt: "Check", AgentID: "claude-code", PermissionMode: "full_control", ScheduleType: scheduledTaskDaily, Timezone: "UTC", TimeOfDay: next.Format("15:04"), Skills: []string{}, Weekdays: []int{}, Enabled: false, CreatedAt: now, UpdatedAt: now}
	if err := server.insertScheduledTask(context.Background(), task); err != nil {
		t.Fatalf("insert scheduled task: %v", err)
	}
	queued, err := server.enqueueScheduledTaskRun(context.Background(), task, now)
	if err != nil {
		t.Fatalf("enqueue scheduled task: %v", err)
	}
	conversation, err := server.createScheduledTaskConversation(context.Background(), mustScheduledProject(t, server, projectID), task, queued)
	if err != nil {
		t.Fatalf("create scheduled conversation: %v", err)
	}
	if _, err := server.db.Exec(`update scheduled_task_runs set status=?,finished_at=? where id=?`, scheduledRunStopped, now, queued.ID); err != nil {
		t.Fatalf("stop queued scheduled run: %v", err)
	}
	deleted := httptest.NewRecorder()
	server.routes().ServeHTTP(deleted, httptest.NewRequest(http.MethodDelete, "/api/scheduled-tasks/"+task.ID, nil))
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete scheduled task: status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	if !server.isScheduledConversation(context.Background(), conversation.ID) {
		t.Fatal("scheduled conversation became writable after its task was deleted")
	}
	_, _, _, status, err := server.startMessage(context.Background(), conversation.ID, "continue this conversation", "", nil)
	if status != http.StatusConflict || err == nil {
		t.Fatalf("deleted scheduled task conversation should remain read-only: status=%d err=%v", status, err)
	}
}

func TestNextScheduledTaskOccurrenceSkipsNonexistentDaylightSavingTime(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	task := ScheduledTask{ScheduleType: scheduledTaskDaily, Timezone: location.String(), TimeOfDay: "02:30"}
	after := time.Date(2026, time.March, 8, 0, 0, 0, 0, location)
	next, err := nextScheduledTaskOccurrence(task, after)
	if err != nil {
		t.Fatalf("calculate daylight-saving occurrence: %v", err)
	}
	want := time.Date(2026, time.March, 9, 2, 30, 0, 0, location).UTC()
	if next == nil || !next.Equal(want) {
		t.Fatalf("next occurrence = %v, want %v", next, want)
	}
}

func mustScheduledProject(t *testing.T, server *Server, projectID string) Project {
	t.Helper()
	project, err := server.getProjectByID(context.Background(), projectID)
	if err != nil {
		t.Fatalf("get scheduled project: %v", err)
	}
	return project
}

func TestScheduledTaskPromptIncludesSelectedSkills(t *testing.T) {
	got := scheduledTaskPrompt("检查项目风险并汇报。", []string{"repo-audit", "release-notes"})
	want := "Use the following installed skills for this task and follow each SKILL.md instruction:\n- repo-audit\n- release-notes\n\n检查项目风险并汇报。"
	if got != want {
		t.Fatalf("scheduled task prompt = %q, want %q", got, want)
	}
}
