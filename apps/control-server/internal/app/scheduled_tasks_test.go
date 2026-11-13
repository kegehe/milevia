package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
		"title":                "Nightly review",
		"prompt":               "Review the project and report risks.",
		"agentId":              "claude-code",
		"permissionMode":       "full_control",
		"fullControlConfirmed": true,
		"scheduleType":         "daily",
		"timezone":             "Asia/Shanghai",
		"timeOfDay":            "23:30",
		"enabled":              true,
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
	if run.ConversationID != "" {
		t.Fatalf("paused run kept a dangling conversation reference: %q", run.ConversationID)
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
	if _, err := server.db.Exec(`insert into notifications (id,type,project_id,conversation_id,scheduled_task_id,title,body,action_url,created_at) values ('notif-scheduled-deleted','scheduled_task.succeeded',?,?,?,'标题','正文',?,?)`,
		projectID, conversation.ID, task.ID, "/projects/"+projectID+"/tasks/schedules/"+task.ID, now); err != nil {
		t.Fatalf("insert scheduled notification: %v", err)
	}
	// 修复前的旧通知没有 scheduled_task_id，只有 action_url。
	if _, err := server.db.Exec(`insert into notifications (id,type,project_id,conversation_id,scheduled_task_id,title,body,action_url,created_at) values ('notif-scheduled-legacy','scheduled_task.succeeded',?,?,'','标题','正文',?,?)`,
		projectID, conversation.ID, "/projects/"+projectID+"/tasks/schedules/"+task.ID, now); err != nil {
		t.Fatalf("insert legacy scheduled notification: %v", err)
	}
	deleted := httptest.NewRecorder()
	server.routes().ServeHTTP(deleted, httptest.NewRequest(http.MethodDelete, "/api/scheduled-tasks/"+task.ID, nil))
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete scheduled task: status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	var remaining int
	if err := server.db.QueryRow(`select count(*) from notifications where id in ('notif-scheduled-deleted','notif-scheduled-legacy')`).Scan(&remaining); err != nil {
		t.Fatalf("count scheduled notifications: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("notifications targeting the deleted scheduled task still exist: %d", remaining)
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

// TestScheduledTaskListBatchesLatestRuns 覆盖列表接口批量取"每条定时任务最近一次执行"：
// 有执行的取 created_at 最新那条（哪怕它的 scheduled_for 更早），没执行的留空，
// 且互相之间不能串台（批量查询最容易犯的错就是按错误的键回填）。
func TestScheduledTaskListBatchesLatestRuns(t *testing.T) {
	server := newTestServer(t)
	projectID := seedScheduledTaskProject(t, server)
	handler := server.routes()

	createTask := func(title string) ScheduledTask {
		payload := map[string]any{
			"title":          title,
			"prompt":         "Summarize the project.",
			"agentId":        "claude-code",
			"permissionMode": "approval_required",
			"scheduleType":   "daily",
			"timezone":       "Asia/Shanghai",
			"timeOfDay":      "07:00",
			"enabled":        true,
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/scheduled-tasks", jsonBody(t, payload)))
		if response.Code != http.StatusCreated {
			t.Fatalf("create scheduled task %q: status=%d body=%s", title, response.Code, response.Body.String())
		}
		var task ScheduledTask
		if err := json.NewDecoder(response.Body).Decode(&task); err != nil {
			t.Fatalf("decode scheduled task: %v", err)
		}
		return task
	}

	withRuns := createTask("has runs")
	withoutRuns := createTask("no runs")
	now := time.Now().UTC()
	// 先插 scheduled_for 更晚、created_at 更早的那条；后插的那条才是"最近一次执行"。
	for _, run := range []struct {
		id           string
		scheduledFor time.Time
		createdAt    time.Time
	}{
		{"run-earlier", now.Add(48 * time.Hour), now},
		{"run-latest", now.Add(24 * time.Hour), now.Add(time.Minute)},
	} {
		if _, err := server.db.Exec(`insert into scheduled_task_runs (id,scheduled_task_id,scheduled_for,status,prompt_snapshot,created_at) values (?,?,?,?,?,?)`, run.id, withRuns.ID, run.scheduledFor, scheduledRunSucceeded, "prompt", run.createdAt); err != nil {
			t.Fatalf("insert scheduled run: %v", err)
		}
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/scheduled-tasks", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("list scheduled tasks: status=%d body=%s", response.Code, response.Body.String())
	}
	var listed []ScheduledTask
	if err := json.NewDecoder(response.Body).Decode(&listed); err != nil {
		t.Fatalf("decode scheduled tasks: %v", err)
	}
	byID := map[string]ScheduledTask{}
	for _, task := range listed {
		byID[task.ID] = task
	}
	if len(byID) != 2 {
		t.Fatalf("listed scheduled tasks: got %d want 2", len(byID))
	}
	if last := byID[withRuns.ID].LastRun; last == nil || last.ID != "run-latest" {
		t.Fatalf("scheduled task last run: %+v want run-latest (max created_at)", last)
	}
	if byID[withoutRuns.ID].LastRun != nil {
		t.Fatalf("scheduled task without runs got last run: %+v", byID[withoutRuns.ID].LastRun)
	}
}

// seedScheduledHistoryConversations 造出 n 条带消息的真实历史会话，last_activity_at
// 从旧到新递增，返回值按从旧到新排序（第一条是历史裁剪的首个淘汰对象）。
func seedScheduledHistoryConversations(t *testing.T, server *Server, projectID string, count int, now time.Time) []string {
	t.Helper()
	ids := make([]string, 0, count)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("history-%03d", i)
		at := now.Add(time.Duration(i-count) * time.Minute)
		if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,agent_id,status,title,last_activity_at,is_current,created_at,origin) values (?,?,?,?,?,?,?,0,?,?)`, id, projectID, "session-"+id, "claude-code", "idle", "History "+id, at, at, "interactive"); err != nil {
			t.Fatalf("insert history conversation: %v", err)
		}
		if _, err := server.db.Exec(`insert into messages (id,conversation_id,run_id,role,content,created_at) values (?,?,?,?,?,?)`, "message-"+id, id, "run-"+id, "user", "history", at); err != nil {
			t.Fatalf("insert history message: %v", err)
		}
		ids = append(ids, id)
	}
	return ids
}

func scheduledTestTask(projectID, id, title string, now time.Time) ScheduledTask {
	next := now.Add(time.Hour)
	return ScheduledTask{ID: id, ProjectID: projectID, Title: title, Prompt: "Check", AgentID: "claude-code", PermissionMode: "full_control", ScheduleType: scheduledTaskDaily, Timezone: "UTC", TimeOfDay: next.Format("15:04"), Skills: []string{}, Weekdays: []int{}, Enabled: true, NextRunAt: &next, CreatedAt: now, UpdatedAt: now}
}

func projectConversationCount(t *testing.T, server *Server, projectID string) int {
	t.Helper()
	var count int
	if err := server.db.QueryRow(`select count(*) from conversations where project_id=?`, projectID).Scan(&count); err != nil {
		t.Fatalf("count project conversations: %v", err)
	}
	return count
}

// 工作区被占用是"稍后再试"，不是失败：占位会话必须留着并在下次派发时复用，
// 不能删掉重建（也不该在等待期间被历史裁剪淘汰）。
func TestScheduledTaskWorkspaceWaitKeepsAndReusesConversation(t *testing.T) {
	server := newTestServer(t)
	server.runner = runnerFunc(func(_ context.Context, _ AgentRunRequest, sink AgentRunSink) error {
		sink.AssistantText("scheduled output", "")
		return nil
	})
	server.runnerRegistry.register(server.localRunnerID(), server.runner, server.localRunnerMeta())
	projectID := seedScheduledTaskProject(t, server)
	now := time.Now().UTC()
	task := scheduledTestTask(projectID, "workspace-wait", "Workspace wait", now)
	if err := server.insertScheduledTask(context.Background(), task); err != nil {
		t.Fatalf("insert scheduled task: %v", err)
	}
	queued, err := server.enqueueScheduledTaskRun(context.Background(), task, now)
	if err != nil {
		t.Fatalf("enqueue scheduled task: %v", err)
	}
	release, acquired := server.acquireProjectWorkspace(projectID, "scheduled-test-holder")
	if !acquired {
		t.Fatal("could not occupy the project workspace for the test")
	}
	server.launchScheduledTaskRun(context.Background(), queued.ID)
	release()

	waiting, err := server.scheduledTaskRunByID(context.Background(), queued.ID)
	if err != nil {
		t.Fatalf("read waiting run: %v", err)
	}
	if waiting.Status != scheduledRunQueued || waiting.RunID != "" {
		t.Fatalf("workspace conflict should leave the run queued: %#v", waiting)
	}
	if waiting.FailureReason != "项目工作区正被占用，正在等待其空闲" {
		t.Fatalf("unexpected waiting reason: %q", waiting.FailureReason)
	}
	if waiting.ConversationID == "" {
		t.Fatal("workspace wait dropped the placeholder conversation reference")
	}
	if count := projectConversationCount(t, server, projectID); count != 1 {
		t.Fatalf("workspace wait should keep exactly one placeholder conversation: %d", count)
	}

	server.launchScheduledTaskRun(context.Background(), queued.ID)
	started, err := server.scheduledTaskRunByID(context.Background(), queued.ID)
	if err != nil {
		t.Fatalf("read started run: %v", err)
	}
	// 重试必须真的派发：仍停在 queued（例如租约没释放、或重试又走了等待分支）
	// 时，下面关于会话的断言会被第一次尝试满足而看不出问题。
	if started.Status == scheduledRunQueued {
		t.Fatalf("retry after the workspace was released stayed queued: %#v", started)
	}
	if started.RunID == "" {
		t.Fatalf("retry did not dispatch a run: %#v", started)
	}
	if started.ConversationID != waiting.ConversationID {
		t.Fatalf("retry created a new conversation: %q want %q", started.ConversationID, waiting.ConversationID)
	}
	if count := projectConversationCount(t, server, projectID); count != 1 {
		t.Fatalf("retry duplicated the placeholder conversation: %d", count)
	}
}

// 启动被拒绝（服务关闭 / 执行环境不可用）时不得留下空会话。
func TestFailedScheduledTaskLaunchRemovesPlaceholderConversation(t *testing.T) {
	server := newTestServer(t)
	projectID := seedScheduledTaskProject(t, server)
	now := time.Now().UTC()
	task := scheduledTestTask(projectID, "launch-refused", "Launch refused", now)
	if err := server.insertScheduledTask(context.Background(), task); err != nil {
		t.Fatalf("insert scheduled task: %v", err)
	}
	queued, err := server.enqueueScheduledTaskRun(context.Background(), task, now)
	if err != nil {
		t.Fatalf("enqueue scheduled task: %v", err)
	}
	// 这段就是"占位会话已建好、startMessage 随后拒绝派发"的真实分支：
	// closing 判定在 app.go 里早于任何消息写入，用一个确定的开关复现。
	server.mu.Lock()
	server.closing = true
	server.mu.Unlock()
	server.launchScheduledTaskRun(context.Background(), queued.ID)
	server.mu.Lock()
	server.closing = false
	server.mu.Unlock()

	run, err := server.scheduledTaskRunByID(context.Background(), queued.ID)
	if err != nil {
		t.Fatalf("read scheduled run: %v", err)
	}
	if run.Status != scheduledRunFailed {
		t.Fatalf("refused launch should fail the run: %#v", run)
	}
	if run.ConversationID != "" {
		t.Fatalf("failed launch kept a conversation reference: %q", run.ConversationID)
	}
	if count := projectConversationCount(t, server, projectID); count != 0 {
		t.Fatalf("failed launch left %d conversation(s) behind", count)
	}
	list := httptest.NewRecorder()
	server.routes().ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/conversations", nil))
	if list.Code != http.StatusOK {
		t.Fatalf("list conversations: status=%d body=%s", list.Code, list.Body.String())
	}
	if bytes.Contains(list.Body.Bytes(), []byte("Scheduled:")) {
		t.Fatalf("conversation list still shows the empty placeholder: %s", list.Body.String())
	}
	var dangling int
	if err := server.db.QueryRow(`select count(*) from notifications where project_id=? and conversation_id<>'' and not exists(select 1 from conversations c where c.id=notifications.conversation_id)`, projectID).Scan(&dangling); err != nil {
		t.Fatalf("count dangling notifications: %v", err)
	}
	if dangling != 0 {
		t.Fatalf("%d notification(s) point at the removed placeholder conversation", dangling)
	}
}

// 反复失败不能累积空会话。
func TestRepeatedScheduledTaskLaunchFailuresDoNotAccumulateConversations(t *testing.T) {
	server := newTestServer(t)
	projectID := seedScheduledTaskProject(t, server)
	now := time.Now().UTC()
	task := scheduledTestTask(projectID, "launch-refused-repeated", "Repeated", now)
	if err := server.insertScheduledTask(context.Background(), task); err != nil {
		t.Fatalf("insert scheduled task: %v", err)
	}
	for i := 0; i < 3; i++ {
		queued, err := server.enqueueScheduledTaskRun(context.Background(), task, now.Add(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatalf("enqueue scheduled task: %v", err)
		}
		server.mu.Lock()
		server.closing = true
		server.mu.Unlock()
		server.launchScheduledTaskRun(context.Background(), queued.ID)
		server.mu.Lock()
		server.closing = false
		server.mu.Unlock()
		if count := projectConversationCount(t, server, projectID); count != 0 {
			t.Fatalf("failure %d left %d conversation(s) behind", i+1, count)
		}
	}
}

// 项目已经有 100 条真实历史时，一次启动失败不能再淘汰真实会话。
func TestFailedScheduledTaskLaunchKeepsFullConversationHistory(t *testing.T) {
	server := newTestServer(t)
	projectID := seedScheduledTaskProject(t, server)
	now := time.Now().UTC()
	history := seedScheduledHistoryConversations(t, server, projectID, maxConversationHistoryPerProject, now)
	task := scheduledTestTask(projectID, "launch-refused-full", "Full history", now)
	if err := server.insertScheduledTask(context.Background(), task); err != nil {
		t.Fatalf("insert scheduled task: %v", err)
	}
	queued, err := server.enqueueScheduledTaskRun(context.Background(), task, now)
	if err != nil {
		t.Fatalf("enqueue scheduled task: %v", err)
	}
	server.mu.Lock()
	server.closing = true
	server.mu.Unlock()
	server.launchScheduledTaskRun(context.Background(), queued.ID)
	server.mu.Lock()
	server.closing = false
	server.mu.Unlock()

	if count := projectConversationCount(t, server, projectID); count != maxConversationHistoryPerProject {
		t.Fatalf("conversation count: got %d want %d", count, maxConversationHistoryPerProject)
	}
	for _, id := range history {
		var exists bool
		if err := server.db.QueryRow(`select exists(select 1 from conversations where id=?)`, id).Scan(&exists); err != nil {
			t.Fatalf("read history conversation: %v", err)
		}
		if !exists {
			t.Fatalf("failed launch evicted real conversation %s", id)
		}
	}
}

// 运行真的开始之后仍然要执行 100 条上限，且淘汰的是最旧的真实会话。
func TestStartedScheduledTaskLaunchStillPrunesConversationHistory(t *testing.T) {
	server := newTestServer(t)
	server.runner = runnerFunc(func(_ context.Context, _ AgentRunRequest, sink AgentRunSink) error {
		sink.AssistantText("scheduled output", "")
		return nil
	})
	server.runnerRegistry.register(server.localRunnerID(), server.runner, server.localRunnerMeta())
	projectID := seedScheduledTaskProject(t, server)
	now := time.Now().UTC()
	history := seedScheduledHistoryConversations(t, server, projectID, maxConversationHistoryPerProject, now)
	task := scheduledTestTask(projectID, "launch-prunes", "Prunes", now)
	if err := server.insertScheduledTask(context.Background(), task); err != nil {
		t.Fatalf("insert scheduled task: %v", err)
	}
	queued, err := server.enqueueScheduledTaskRun(context.Background(), task, now)
	if err != nil {
		t.Fatalf("enqueue scheduled task: %v", err)
	}
	server.launchScheduledTaskRun(context.Background(), queued.ID)
	if count := projectConversationCount(t, server, projectID); count != maxConversationHistoryPerProject {
		t.Fatalf("conversation count after launch: got %d want %d", count, maxConversationHistoryPerProject)
	}
	var oldestExists bool
	if err := server.db.QueryRow(`select exists(select 1 from conversations where id=?)`, history[0]).Scan(&oldestExists); err != nil {
		t.Fatalf("read oldest conversation: %v", err)
	}
	if oldestExists {
		t.Fatal("started scheduled run should prune the oldest real conversation")
	}
	run, err := server.scheduledTaskRunByID(context.Background(), queued.ID)
	if err != nil || run.ConversationID == "" {
		t.Fatalf("started scheduled run lost its conversation: run=%#v err=%v", run, err)
	}
	var conversationExists bool
	if err := server.db.QueryRow(`select exists(select 1 from conversations where id=?)`, run.ConversationID).Scan(&conversationExists); err != nil || !conversationExists {
		t.Fatalf("scheduled conversation is missing: exists=%v err=%v", conversationExists, err)
	}
}

// 排队中的定时运行握着占位会话时，历史裁剪不得把它淘汰：否则
// run.conversation_id 变成悬空引用，这一轮之后再也派发不出去。
func TestPruneConversationHistoryKeepsQueuedScheduledPlaceholder(t *testing.T) {
	server := newTestServer(t)
	projectID := seedScheduledTaskProject(t, server)
	now := time.Now().UTC()
	task := scheduledTestTask(projectID, "prune-placeholder", "Prune placeholder", now)
	if err := server.insertScheduledTask(context.Background(), task); err != nil {
		t.Fatalf("insert scheduled task: %v", err)
	}
	queued, err := server.enqueueScheduledTaskRun(context.Background(), task, now)
	if err != nil {
		t.Fatalf("enqueue scheduled task: %v", err)
	}
	conversation, err := server.createScheduledTaskConversation(context.Background(), mustScheduledProject(t, server, projectID), task, queued)
	if err != nil {
		t.Fatalf("create placeholder conversation: %v", err)
	}
	history := seedScheduledHistoryConversations(t, server, projectID, maxConversationHistoryPerProject, now)
	// 占位会话比所有历史都旧：只按 last_activity_at 排的话它就是第一个淘汰对象。
	if _, err := server.db.Exec(`update conversations set last_activity_at=? where id=?`, now.Add(-48*time.Hour), conversation.ID); err != nil {
		t.Fatalf("age the placeholder conversation: %v", err)
	}

	tx, err := server.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin prune transaction: %v", err)
	}
	defer tx.Rollback()
	pruned, err := pruneConversationHistoryTx(context.Background(), tx, projectID)
	if err != nil {
		t.Fatalf("prune conversation history: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit prune transaction: %v", err)
	}
	if !pruned {
		t.Fatal("prune should have removed the excess conversation")
	}
	var placeholderExists bool
	if err := server.db.QueryRow(`select exists(select 1 from conversations where id=?)`, conversation.ID).Scan(&placeholderExists); err != nil || !placeholderExists {
		t.Fatalf("prune evicted the queued run's placeholder conversation: exists=%v err=%v", placeholderExists, err)
	}
	var oldestExists bool
	if err := server.db.QueryRow(`select exists(select 1 from conversations where id=?)`, history[0]).Scan(&oldestExists); err != nil {
		t.Fatalf("read oldest conversation: %v", err)
	}
	if oldestExists {
		t.Fatal("prune should have evicted the oldest real conversation instead of the placeholder")
	}
	if count := projectConversationCount(t, server, projectID); count != maxConversationHistoryPerProject {
		t.Fatalf("conversation count after prune: got %d want %d", count, maxConversationHistoryPerProject)
	}
}

// 等待期间占位会话被删掉后，下一次派发要重建一条，而不是拿着悬空引用报错。
func TestLaunchRecreatesDeletedPlaceholderConversation(t *testing.T) {
	server := newTestServer(t)
	projectID := seedScheduledTaskProject(t, server)
	now := time.Now().UTC()
	task := scheduledTestTask(projectID, "deleted-placeholder", "Deleted placeholder", now)
	if err := server.insertScheduledTask(context.Background(), task); err != nil {
		t.Fatalf("insert scheduled task: %v", err)
	}
	queued, err := server.enqueueScheduledTaskRun(context.Background(), task, now)
	if err != nil {
		t.Fatalf("enqueue scheduled task: %v", err)
	}
	first, err := server.createScheduledTaskConversation(context.Background(), mustScheduledProject(t, server, projectID), task, queued)
	if err != nil {
		t.Fatalf("create placeholder conversation: %v", err)
	}
	if _, err := server.db.Exec(`delete from conversations where id=?`, first.ID); err != nil {
		t.Fatalf("delete placeholder conversation: %v", err)
	}

	recreated, err := server.createScheduledTaskConversation(context.Background(), mustScheduledProject(t, server, projectID), task, queued)
	if err != nil {
		t.Fatalf("recreate deleted placeholder conversation: %v", err)
	}
	if recreated.ID == "" || recreated.ID == first.ID {
		t.Fatalf("conversation was not recreated: %#v", recreated)
	}
	run, err := server.scheduledTaskRunByID(context.Background(), queued.ID)
	if err != nil || run.ConversationID != recreated.ID {
		t.Fatalf("run still points at the deleted conversation: run=%#v err=%v", run, err)
	}
}

// 引用本来就悬空（旧版本裁剪留下的）时，失败路径要把 conversation_id 清掉，
// 而不是留着一条指向不存在会话的记录。
func TestFailScheduledTaskRunClearsDanglingConversationReference(t *testing.T) {
	server := newTestServer(t)
	projectID := seedScheduledTaskProject(t, server)
	now := time.Now().UTC()
	task := scheduledTestTask(projectID, "dangling", "Dangling", now)
	if err := server.insertScheduledTask(context.Background(), task); err != nil {
		t.Fatalf("insert scheduled task: %v", err)
	}
	queued, err := server.enqueueScheduledTaskRun(context.Background(), task, now)
	if err != nil {
		t.Fatalf("enqueue scheduled task: %v", err)
	}
	if _, err := server.db.Exec(`update scheduled_task_runs set conversation_id='missing-conversation' where id=?`, queued.ID); err != nil {
		t.Fatalf("plant dangling conversation reference: %v", err)
	}

	server.failScheduledTaskRun(queued.ID, "runner unavailable")

	run, err := server.scheduledTaskRunByID(context.Background(), queued.ID)
	if err != nil || run.Status != scheduledRunFailed {
		t.Fatalf("run with dangling conversation was not failed: run=%#v err=%v", run, err)
	}
	if run.ConversationID != "" {
		t.Fatalf("dangling conversation reference survived: %q", run.ConversationID)
	}
}

// 已经派发过（用户消息已提交）的排队运行失败时，它的会话是历史，不能被删。
func TestFailScheduledTaskRunKeepsDispatchedConversation(t *testing.T) {
	server := newTestServer(t)
	projectID := seedScheduledTaskProject(t, server)
	now := time.Now().UTC()
	task := scheduledTestTask(projectID, "dispatched", "Dispatched", now)
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
	if _, err := server.db.Exec(`insert into messages (id,conversation_id,run_id,role,content,created_at) values ('dispatched-message',?,?,?,?,?)`, conversation.ID, "dispatched-run", "user", "已发出的任务", now); err != nil {
		t.Fatalf("insert dispatched message: %v", err)
	}
	if _, err := server.db.Exec(`update scheduled_task_runs set run_id='dispatched-run',status=? where id=?`, scheduledRunQueued, queued.ID); err != nil {
		t.Fatalf("mark dispatched run: %v", err)
	}

	server.failScheduledTaskRun(queued.ID, "run failed before completion")

	run, err := server.scheduledTaskRunByID(context.Background(), queued.ID)
	if err != nil || run.Status != scheduledRunFailed {
		t.Fatalf("dispatched run was not failed: run=%#v err=%v", run, err)
	}
	if run.ConversationID != conversation.ID {
		t.Fatalf("dispatched run lost its conversation reference: %q want %q", run.ConversationID, conversation.ID)
	}
	var messages int
	if err := server.db.QueryRow(`select count(*) from messages where conversation_id=?`, conversation.ID).Scan(&messages); err != nil || messages != 1 {
		t.Fatalf("dispatched conversation history was removed: messages=%d err=%v", messages, err)
	}
}

// 启动恢复要把崩溃时留下的占位会话清掉，同时保留已经派发的定时会话。
func TestRecoverInterruptedScheduledTasksRemovesPlaceholderConversation(t *testing.T) {
	server := newTestServer(t)
	projectID := seedScheduledTaskProject(t, server)
	now := time.Now().UTC()
	task := scheduledTestTask(projectID, "recover", "Recover", now)
	if err := server.insertScheduledTask(context.Background(), task); err != nil {
		t.Fatalf("insert scheduled task: %v", err)
	}
	placeholder, err := server.enqueueScheduledTaskRun(context.Background(), task, now)
	if err != nil {
		t.Fatalf("enqueue scheduled task: %v", err)
	}
	conversation, err := server.createScheduledTaskConversation(context.Background(), mustScheduledProject(t, server, projectID), task, placeholder)
	if err != nil {
		t.Fatalf("create placeholder conversation: %v", err)
	}
	// 已经派发（run_id 非空）的定时会话即使还排着队也不能被恢复流程删掉。
	startedTask := scheduledTestTask(projectID, "recover-started", "Recover started", now)
	if err := server.insertScheduledTask(context.Background(), startedTask); err != nil {
		t.Fatalf("insert started scheduled task: %v", err)
	}
	started, err := server.enqueueScheduledTaskRun(context.Background(), startedTask, now.Add(time.Second))
	if err != nil {
		t.Fatalf("enqueue started run: %v", err)
	}
	startedConversation, err := server.createScheduledTaskConversation(context.Background(), mustScheduledProject(t, server, projectID), startedTask, started)
	if err != nil {
		t.Fatalf("create started conversation: %v", err)
	}
	if _, err := server.db.Exec(`insert into messages (id,conversation_id,run_id,role,content,created_at) values ('recover-message',?,?,?,?,?)`, startedConversation.ID, "recover-run", "user", "already dispatched", now); err != nil {
		t.Fatalf("insert started message: %v", err)
	}
	if _, err := server.db.Exec(`update scheduled_task_runs set run_id='recover-run',status=? where id=?`, scheduledRunQueued, started.ID); err != nil {
		t.Fatalf("mark started run: %v", err)
	}

	if err := server.recoverInterruptedScheduledTasks(context.Background()); err != nil {
		t.Fatalf("recover interrupted scheduled tasks: %v", err)
	}

	recovered, err := server.scheduledTaskRunByID(context.Background(), placeholder.ID)
	if err != nil || recovered.Status != scheduledRunInterrupted || recovered.ConversationID != "" {
		t.Fatalf("recovered run: %#v err=%v", recovered, err)
	}
	var placeholderExists bool
	if err := server.db.QueryRow(`select exists(select 1 from conversations where id=?)`, conversation.ID).Scan(&placeholderExists); err != nil {
		t.Fatalf("read placeholder conversation: %v", err)
	}
	if placeholderExists {
		t.Fatal("recovery left the placeholder conversation behind")
	}
	var startedExists bool
	if err := server.db.QueryRow(`select exists(select 1 from conversations where id=?)`, startedConversation.ID).Scan(&startedExists); err != nil || !startedExists {
		t.Fatalf("recovery removed a dispatched scheduled conversation: exists=%v err=%v", startedExists, err)
	}
}
