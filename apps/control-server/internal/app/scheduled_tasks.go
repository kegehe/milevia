package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const (
	scheduledTaskOnce   = "once"
	scheduledTaskDaily  = "daily"
	scheduledTaskWeekly = "weekly"

	scheduledRunQueued      = "queued"
	scheduledRunRunning     = "running"
	scheduledRunSucceeded   = "succeeded"
	scheduledRunFailed      = "failed"
	scheduledRunStopped     = "stopped"
	scheduledRunInterrupted = "interrupted"
)

// ScheduledTask is an autonomous prompt that is eligible to run only while
// the Milevia control service is alive. It deliberately does not reuse Task:
// a board task has a human-review lifecycle and cannot safely repeat.
type ScheduledTask struct {
	ID             string             `json:"id"`
	ProjectID      string             `json:"projectId"`
	Title          string             `json:"title"`
	Prompt         string             `json:"prompt"`
	Skills         []string           `json:"skills"`
	AgentID        string             `json:"agentId"`
	PermissionMode string             `json:"permissionMode"`
	ScheduleType   string             `json:"scheduleType"`
	Timezone       string             `json:"timezone"`
	RunAt          *time.Time         `json:"runAt,omitempty"`
	TimeOfDay      string             `json:"timeOfDay,omitempty"`
	Weekdays       []int              `json:"weekdays"`
	Enabled        bool               `json:"enabled"`
	NextRunAt      *time.Time         `json:"nextRunAt,omitempty"`
	LastRunAt      *time.Time         `json:"lastRunAt,omitempty"`
	CreatedAt      time.Time          `json:"createdAt"`
	UpdatedAt      time.Time          `json:"updatedAt"`
	LastRun        *ScheduledTaskRun  `json:"lastRun,omitempty"`
	Runs           []ScheduledTaskRun `json:"runs,omitempty"`
}

type ScheduledTaskRun struct {
	ID                      string     `json:"id"`
	ScheduledTaskID         string     `json:"scheduledTaskId"`
	ScheduledFor            time.Time  `json:"scheduledFor"`
	Status                  string     `json:"status"`
	TitleSnapshot           string     `json:"titleSnapshot,omitempty"`
	AgentIDSnapshot         string     `json:"agentIdSnapshot,omitempty"`
	PermissionModeSnapshot  string     `json:"permissionModeSnapshot,omitempty"`
	ProfileRevisionSnapshot string     `json:"profileRevisionSnapshot,omitempty"`
	RouteRevisionSnapshot   string     `json:"routeRevisionSnapshot,omitempty"`
	PromptSnapshot          string     `json:"promptSnapshot"`
	SkillsSnapshot          []string   `json:"skillsSnapshot"`
	ConversationID          string     `json:"conversationId,omitempty"`
	RunID                   string     `json:"runId,omitempty"`
	FailureReason           string     `json:"failureReason,omitempty"`
	CreatedAt               time.Time  `json:"createdAt"`
	StartedAt               *time.Time `json:"startedAt,omitempty"`
	FinishedAt              *time.Time `json:"finishedAt,omitempty"`
}

type scheduledTaskInput struct {
	Title                string   `json:"title"`
	Prompt               string   `json:"prompt"`
	Skills               []string `json:"skills"`
	AgentID              string   `json:"agentId"`
	PermissionMode       string   `json:"permissionMode"`
	ScheduleType         string   `json:"scheduleType"`
	Timezone             string   `json:"timezone"`
	RunAt                string   `json:"runAt"`
	TimeOfDay            string   `json:"timeOfDay"`
	Weekdays             []int    `json:"weekdays"`
	Enabled              *bool    `json:"enabled"`
	FullControlConfirmed bool     `json:"fullControlConfirmed"`
}

type rowScanner interface {
	Scan(...any) error
}

func (s *Server) migrateScheduledTasks(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `create table if not exists scheduled_tasks (
	id text primary key,
	project_id text not null references projects(id) on delete cascade,
	title text not null,
	prompt text not null,
	skills_json text not null default '[]',
	agent_id text not null default 'claude-code',
	permission_mode text not null default 'workspace_write',
	schedule_type text not null,
	timezone text not null,
	run_at datetime,
	time_of_day text not null default '',
	weekdays_json text not null default '[]',
	enabled integer not null default 1,
	next_run_at datetime,
	last_run_at datetime,
	created_at datetime not null,
	updated_at datetime not null
);
create table if not exists scheduled_task_runs (
	id text primary key,
	scheduled_task_id text not null references scheduled_tasks(id) on delete cascade,
scheduled_for datetime not null,
status text not null,
title_snapshot text not null default '',
agent_id_snapshot text not null default '',
permission_mode_snapshot text not null default '',
profile_revision_snapshot text not null default '',
route_revision_snapshot text not null default '',
prompt_snapshot text not null,
skills_snapshot text not null default '[]',
	conversation_id text not null default '',
	run_id text not null default '',
	failure_reason text not null default '',
	created_at datetime not null,
	started_at datetime,
	finished_at datetime,
	unique(scheduled_task_id, scheduled_for)
);
create index if not exists scheduled_tasks_due on scheduled_tasks(enabled,next_run_at);
create index if not exists scheduled_task_runs_task_created on scheduled_task_runs(scheduled_task_id,created_at desc);
create index if not exists scheduled_task_runs_run on scheduled_task_runs(run_id);`)
	if err != nil {
		return fmt.Errorf("migrate scheduled tasks: %w", err)
	}
	if err := ensureColumn(ctx, s.db, "conversations", "origin", "text not null default 'interactive'"); err != nil {
		return err
	}
	for _, column := range []struct {
		name       string
		definition string
	}{
		{"title_snapshot", "text not null default ''"},
		{"agent_id_snapshot", "text not null default ''"},
		{"permission_mode_snapshot", "text not null default ''"},
		{"profile_revision_snapshot", "text not null default ''"},
		{"route_revision_snapshot", "text not null default ''"},
	} {
		if err := ensureColumn(ctx, s.db, "scheduled_task_runs", column.name, column.definition); err != nil {
			return fmt.Errorf("add scheduled task run %s: %w", column.name, err)
		}
	}
	return nil
}

func (s *Server) listScheduledTasks(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	if _, err := s.getProjectByID(r.Context(), projectID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("project not found"))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	items, err := s.scheduledTasksForProject(r.Context(), projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) getScheduledTask(w http.ResponseWriter, r *http.Request) {
	task, err := s.scheduledTaskByID(r.Context(), chi.URLParam(r, "scheduledTaskID"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("scheduled task not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	runs, err := s.scheduledTaskRuns(r.Context(), task.ID, 100)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	task.Runs = runs
	if len(runs) > 0 {
		task.LastRun = &runs[0]
	}
	writeJSON(w, http.StatusOK, task)
}

func (s *Server) createScheduledTask(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	project, err := s.getProjectByID(r.Context(), projectID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("project not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var input scheduledTaskInput
	if !decode(w, r, &input) {
		return
	}
	task, err := scheduledTaskFromInput(projectID, input, time.Now().UTC())
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if task.PermissionMode == "full_control" && !input.FullControlConfirmed {
		writeError(w, http.StatusBadRequest, errors.New("full_control scheduled tasks require explicit confirmation"))
		return
	}
	if err := s.validateScheduledTaskSkills(r.Context(), project, task.AgentID, task.Skills); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	task.ID = uuid.NewString()
	if err := s.insertScheduledTask(r.Context(), task); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, task)
}

func (s *Server) updateScheduledTask(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "scheduledTaskID")
	previous, err := s.scheduledTaskByID(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("scheduled task not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	project, err := s.getProjectByID(r.Context(), previous.ProjectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var input scheduledTaskInput
	if !decode(w, r, &input) {
		return
	}
	allowPastDisabledOneTime := previous.ScheduleType == scheduledTaskOnce && !previous.Enabled && input.Enabled != nil && !*input.Enabled
	task, err := scheduledTaskFromInputWithOptions(previous.ProjectID, input, time.Now().UTC(), allowPastDisabledOneTime)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if task.PermissionMode == "full_control" && previous.PermissionMode != "full_control" && !input.FullControlConfirmed {
		writeError(w, http.StatusBadRequest, errors.New("full_control scheduled tasks require explicit confirmation"))
		return
	}
	if err := s.validateScheduledTaskSkills(r.Context(), project, task.AgentID, task.Skills); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	task.ID, task.CreatedAt = previous.ID, previous.CreatedAt
	if err := s.replaceScheduledTaskAndStopQueuedRuns(r.Context(), task); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

func (s *Server) deleteScheduledTask(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "scheduledTaskID")
	var active bool
	err := s.db.QueryRowContext(r.Context(), `select exists(select 1 from scheduled_task_runs where scheduled_task_id=? and status in ('queued','running'))`, id).Scan(&active)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if active {
		writeError(w, http.StatusConflict, errors.New("scheduled task has an active run"))
		return
	}
	result, err := s.db.ExecContext(r.Context(), `delete from scheduled_tasks where id=?`, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	changed, err := result.RowsAffected()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if changed == 0 {
		writeError(w, http.StatusNotFound, errors.New("scheduled task not found"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) pauseScheduledTask(w http.ResponseWriter, r *http.Request) {
	s.setScheduledTaskEnabled(w, r, false)
}

func (s *Server) resumeScheduledTask(w http.ResponseWriter, r *http.Request) {
	s.setScheduledTaskEnabled(w, r, true)
}

func (s *Server) setScheduledTaskEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	task, err := s.scheduledTaskByID(r.Context(), chi.URLParam(r, "scheduledTaskID"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("scheduled task not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	now := time.Now().UTC()
	if enabled {
		if task.ScheduleType == scheduledTaskOnce && (task.RunAt == nil || !task.RunAt.After(now)) {
			writeError(w, http.StatusConflict, errors.New("a past one-time schedule cannot be resumed"))
			return
		}
		if task.ScheduleType != scheduledTaskOnce {
			next, nextErr := nextScheduledTaskOccurrence(task, now)
			if nextErr != nil {
				writeError(w, http.StatusBadRequest, nextErr)
				return
			}
			task.NextRunAt = next
		}
	} else {
		task.NextRunAt = nil
	}
	task.Enabled, task.UpdatedAt = enabled, now
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(r.Context(), `update scheduled_tasks set enabled=?,next_run_at=?,updated_at=? where id=?`, boolToInt(enabled), task.NextRunAt, now, task.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !enabled {
		stopped, err := s.stopUnstartedScheduledTaskRunsTx(r.Context(), tx, task.ID, now)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if stopped {
			task.LastRunAt = &now
		}
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

func (s *Server) runScheduledTaskNow(w http.ResponseWriter, r *http.Request) {
	task, err := s.scheduledTaskByID(r.Context(), chi.URLParam(r, "scheduledTaskID"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("scheduled task not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	run, err := s.enqueueScheduledTaskRun(r.Context(), task, time.Now().UTC())
	if err != nil {
		if errors.Is(err, errScheduledTaskAlreadyActive) {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.launchScheduledTaskRun(s.runtimeCtx, run.ID)
	updated, err := s.scheduledTaskRunByID(r.Context(), run.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusAccepted, updated)
}

func (s *Server) listScheduledTaskRuns(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "scheduledTaskID")
	if _, err := s.scheduledTaskByID(r.Context(), taskID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("scheduled task not found"))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	runs, err := s.scheduledTaskRuns(r.Context(), taskID, 100)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

func scheduledTaskFromInput(projectID string, input scheduledTaskInput, now time.Time) (ScheduledTask, error) {
	return scheduledTaskFromInputWithOptions(projectID, input, now, false)
}

// scheduledTaskFromInputWithOptions keeps new one-time schedules strict while
// allowing an already-disabled, historical one-time task to be edited. Such a
// task still cannot be resumed until its time is changed to a future instant.
func scheduledTaskFromInputWithOptions(projectID string, input scheduledTaskInput, now time.Time, allowPastDisabledOneTime bool) (ScheduledTask, error) {
	if !validWeekdayValues(input.Weekdays) {
		return ScheduledTask{}, errors.New("weekdays must use values from 1 (Monday) through 7 (Sunday)")
	}
	task := ScheduledTask{
		ProjectID:      projectID,
		Title:          strings.TrimSpace(input.Title),
		Prompt:         strings.TrimSpace(input.Prompt),
		AgentID:        strings.TrimSpace(input.AgentID),
		PermissionMode: strings.TrimSpace(input.PermissionMode),
		ScheduleType:   strings.TrimSpace(input.ScheduleType),
		Timezone:       strings.TrimSpace(input.Timezone),
		TimeOfDay:      strings.TrimSpace(input.TimeOfDay),
		Skills:         normalizedStringList(input.Skills),
		Weekdays:       normalizedWeekdays(input.Weekdays),
		Enabled:        true,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if input.Enabled != nil {
		task.Enabled = *input.Enabled
	}
	if task.Title == "" {
		return ScheduledTask{}, errors.New("scheduled task title is required")
	}
	if len([]rune(task.Title)) > 120 {
		return ScheduledTask{}, errors.New("scheduled task title is too long")
	}
	if task.Prompt == "" {
		return ScheduledTask{}, errors.New("scheduled task prompt is required")
	}
	if !validAgentPolicy(task.AgentID, task.PermissionMode) {
		return ScheduledTask{}, errors.New("unsupported agent or execution policy")
	}
	if _, err := time.LoadLocation(task.Timezone); err != nil {
		return ScheduledTask{}, errors.New("timezone is invalid")
	}
	switch task.ScheduleType {
	case scheduledTaskOnce:
		if task.TimeOfDay != "" || len(task.Weekdays) != 0 {
			return ScheduledTask{}, errors.New("one-time schedules cannot include repeat fields")
		}
		runAt, err := parseScheduledTaskRunAt(input.RunAt, task.Timezone)
		if err != nil {
			return ScheduledTask{}, err
		}
		if !runAt.After(now.Add(10*time.Second)) && !(allowPastDisabledOneTime && !task.Enabled) {
			return ScheduledTask{}, errors.New("one-time schedules must be at least 10 seconds in the future")
		}
		task.RunAt, task.NextRunAt = &runAt, &runAt
	case scheduledTaskDaily:
		if len(task.Weekdays) != 0 {
			return ScheduledTask{}, errors.New("daily schedules cannot include weekdays")
		}
		if !validTimeOfDay(task.TimeOfDay) {
			return ScheduledTask{}, errors.New("timeOfDay must use HH:MM")
		}
		next, err := nextScheduledTaskOccurrence(task, now)
		if err != nil {
			return ScheduledTask{}, err
		}
		task.NextRunAt = next
	case scheduledTaskWeekly:
		if !validTimeOfDay(task.TimeOfDay) || len(task.Weekdays) == 0 {
			return ScheduledTask{}, errors.New("weekly schedules require timeOfDay and at least one weekday")
		}
		next, err := nextScheduledTaskOccurrence(task, now)
		if err != nil {
			return ScheduledTask{}, err
		}
		task.NextRunAt = next
	default:
		return ScheduledTask{}, errors.New("scheduleType must be once, daily, or weekly")
	}
	if !task.Enabled {
		task.NextRunAt = nil
	}
	return task, nil
}

func validTimeOfDay(value string) bool {
	if len(value) != len("15:04") {
		return false
	}
	_, err := time.Parse("15:04", value)
	return err == nil
}

// parseScheduledTaskRunAt accepts the local datetime produced by datetime-local
// and interprets it in the task's configured IANA timezone. RFC3339 remains
// accepted for API clients that already provide an explicit instant.
func parseScheduledTaskRunAt(value, timezone string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed.UTC(), nil
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return time.Time{}, errors.New("timezone is invalid")
	}
	const localDateTimeLayout = "2006-01-02T15:04"
	parsed, err := time.ParseInLocation(localDateTimeLayout, value, loc)
	if err != nil || parsed.In(loc).Format(localDateTimeLayout) != value {
		return time.Time{}, errors.New("runAt must be an ISO timestamp or a valid local datetime in the selected timezone")
	}
	return parsed.UTC(), nil
}

func normalizedStringList(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func normalizedWeekdays(values []int) []int {
	seen := map[int]struct{}{}
	out := make([]int, 0, len(values))
	for _, value := range values {
		if value < 1 || value > 7 {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Ints(out)
	return out
}

func validWeekdayValues(values []int) bool {
	for _, value := range values {
		if value < 1 || value > 7 {
			return false
		}
	}
	return true
}

func nextScheduledTaskOccurrence(task ScheduledTask, after time.Time) (*time.Time, error) {
	loc, err := time.LoadLocation(task.Timezone)
	if err != nil {
		return nil, err
	}
	clock, err := time.Parse("15:04", task.TimeOfDay)
	if err != nil {
		return nil, err
	}
	localAfter := after.In(loc)
	for offset := 0; offset <= 8; offset++ {
		date := localAfter.AddDate(0, 0, offset)
		if task.ScheduleType == scheduledTaskWeekly {
			weekday := int(date.Weekday())
			if weekday == 0 {
				weekday = 7
			}
			if !containsWeekday(task.Weekdays, weekday) {
				continue
			}
		}
		candidate := time.Date(date.Year(), date.Month(), date.Day(), clock.Hour(), clock.Minute(), 0, 0, loc)
		candidateLocal := candidate.In(loc)
		// time.Date normalizes a wall-clock time that does not exist during a
		// daylight-saving transition. Skipping that date is less surprising than
		// silently running the task at a different local time.
		if candidateLocal.Year() != date.Year() || candidateLocal.Month() != date.Month() || candidateLocal.Day() != date.Day() || candidateLocal.Hour() != clock.Hour() || candidateLocal.Minute() != clock.Minute() {
			continue
		}
		if candidate.After(after) {
			utc := candidate.UTC()
			return &utc, nil
		}
	}
	return nil, errors.New("could not calculate the next scheduled occurrence")
}

func containsWeekday(values []int, value int) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func (s *Server) validateScheduledTaskSkills(ctx context.Context, project Project, agentID string, skills []string) error {
	if len(skills) == 0 {
		return nil
	}
	available := s.discoverSkillsForProject(ctx, project, agentID)
	byName := make(map[string]struct{}, len(available))
	for _, skill := range available {
		byName[skill.Name] = struct{}{}
	}
	for _, name := range skills {
		if _, ok := byName[name]; !ok {
			return fmt.Errorf("selected skill %q is not available for the selected agent", name)
		}
	}
	return nil
}

func scheduledTaskPrompt(prompt string, skills []string) string {
	if len(skills) == 0 {
		return prompt
	}
	lines := make([]string, 0, len(skills)+2)
	lines = append(lines, "Use the following installed skills for this task and follow each SKILL.md instruction:")
	for _, skill := range skills {
		lines = append(lines, "- "+skill)
	}
	lines = append(lines, "", prompt)
	return strings.Join(lines, "\n")
}

func (s *Server) insertScheduledTask(ctx context.Context, task ScheduledTask) error {
	skills, err := json.Marshal(task.Skills)
	if err != nil {
		return err
	}
	weekdays, err := json.Marshal(task.Weekdays)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `insert into scheduled_tasks (id,project_id,title,prompt,skills_json,agent_id,permission_mode,schedule_type,timezone,run_at,time_of_day,weekdays_json,enabled,next_run_at,last_run_at,created_at,updated_at) values (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		task.ID, task.ProjectID, task.Title, task.Prompt, string(skills), task.AgentID, task.PermissionMode, task.ScheduleType, task.Timezone, task.RunAt, task.TimeOfDay, string(weekdays), boolToInt(task.Enabled), task.NextRunAt, task.LastRunAt, task.CreatedAt, task.UpdatedAt)
	return err
}

func (s *Server) replaceScheduledTask(ctx context.Context, task ScheduledTask) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.replaceScheduledTaskTx(ctx, tx, task); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Server) replaceScheduledTaskAndStopQueuedRuns(ctx context.Context, task ScheduledTask) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.replaceScheduledTaskTx(ctx, tx, task); err != nil {
		return err
	}
	if !task.Enabled {
		if _, err := s.stopUnstartedScheduledTaskRunsTx(ctx, tx, task.ID, task.UpdatedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Server) replaceScheduledTaskTx(ctx context.Context, tx *sql.Tx, task ScheduledTask) error {
	skills, err := json.Marshal(task.Skills)
	if err != nil {
		return err
	}
	weekdays, err := json.Marshal(task.Weekdays)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `update scheduled_tasks set title=?,prompt=?,skills_json=?,agent_id=?,permission_mode=?,schedule_type=?,timezone=?,run_at=?,time_of_day=?,weekdays_json=?,enabled=?,next_run_at=?,updated_at=? where id=?`,
		task.Title, task.Prompt, string(skills), task.AgentID, task.PermissionMode, task.ScheduleType, task.Timezone, task.RunAt, task.TimeOfDay, string(weekdays), boolToInt(task.Enabled), task.NextRunAt, task.UpdatedAt, task.ID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return sql.ErrNoRows
	}
	return nil
}

// stopUnstartedScheduledTaskRunsTx stops occurrences that have not obtained a
// run ID. A queued occurrence can have a placeholder conversation while it is
// waiting for the project workspace; remove that empty conversation in the
// same transaction so disabling a schedule cannot leave orphaned entries.
func (s *Server) stopUnstartedScheduledTaskRunsTx(ctx context.Context, tx *sql.Tx, taskID string, now time.Time) (bool, error) {
	if _, err := tx.ExecContext(ctx, `delete from conversations where origin='scheduled' and id in (
		select conversation_id from scheduled_task_runs
		where scheduled_task_id=? and status=? and run_id='' and conversation_id!=''
	)`, taskID, scheduledRunQueued); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `update scheduled_task_runs set status=?,failure_reason=?,finished_at=? where scheduled_task_id=? and status=? and run_id=''`, scheduledRunStopped, "任务在开始前已暂停。", now, taskID, scheduledRunQueued)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if changed == 0 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `update scheduled_tasks set last_run_at=?,updated_at=? where id=?`, now, now, taskID); err != nil {
		return false, err
	}
	return true, nil
}

const scheduledTaskSelect = `id,project_id,title,prompt,skills_json,agent_id,permission_mode,schedule_type,timezone,run_at,time_of_day,weekdays_json,enabled,next_run_at,last_run_at,created_at,updated_at`

func scanScheduledTask(scanner rowScanner) (ScheduledTask, error) {
	var task ScheduledTask
	var skillsJSON, weekdaysJSON string
	var runAt, nextRunAt, lastRunAt sql.NullTime
	if err := scanner.Scan(&task.ID, &task.ProjectID, &task.Title, &task.Prompt, &skillsJSON, &task.AgentID, &task.PermissionMode, &task.ScheduleType, &task.Timezone, &runAt, &task.TimeOfDay, &weekdaysJSON, &task.Enabled, &nextRunAt, &lastRunAt, &task.CreatedAt, &task.UpdatedAt); err != nil {
		return ScheduledTask{}, err
	}
	if err := json.Unmarshal([]byte(skillsJSON), &task.Skills); err != nil {
		return ScheduledTask{}, fmt.Errorf("decode scheduled task skills: %w", err)
	}
	if err := json.Unmarshal([]byte(weekdaysJSON), &task.Weekdays); err != nil {
		return ScheduledTask{}, fmt.Errorf("decode scheduled task weekdays: %w", err)
	}
	if runAt.Valid {
		value := runAt.Time.UTC()
		task.RunAt = &value
	}
	if nextRunAt.Valid {
		value := nextRunAt.Time.UTC()
		task.NextRunAt = &value
	}
	if lastRunAt.Valid {
		value := lastRunAt.Time.UTC()
		task.LastRunAt = &value
	}
	if task.Skills == nil {
		task.Skills = []string{}
	}
	if task.Weekdays == nil {
		task.Weekdays = []int{}
	}
	return task, nil
}

func (s *Server) scheduledTaskByID(ctx context.Context, id string) (ScheduledTask, error) {
	return scanScheduledTask(s.db.QueryRowContext(ctx, `select `+scheduledTaskSelect+` from scheduled_tasks where id=?`, id))
}

func (s *Server) scheduledTasksForProject(ctx context.Context, projectID string) ([]ScheduledTask, error) {
	rows, err := s.db.QueryContext(ctx, `select `+scheduledTaskSelect+` from scheduled_tasks where project_id=? order by enabled desc,next_run_at is null,next_run_at,title`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []ScheduledTask{}
	for rows.Next() {
		task, err := scanScheduledTask(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, task)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for index := range items {
		last, err := s.latestScheduledTaskRun(ctx, items[index].ID)
		if err == nil {
			items[index].LastRun = &last
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	return items, nil
}

const scheduledTaskRunSelect = `id,scheduled_task_id,scheduled_for,status,title_snapshot,agent_id_snapshot,permission_mode_snapshot,profile_revision_snapshot,route_revision_snapshot,prompt_snapshot,skills_snapshot,conversation_id,run_id,failure_reason,created_at,started_at,finished_at`

func scanScheduledTaskRun(scanner rowScanner) (ScheduledTaskRun, error) {
	var run ScheduledTaskRun
	var skillsJSON string
	var startedAt, finishedAt sql.NullTime
	if err := scanner.Scan(&run.ID, &run.ScheduledTaskID, &run.ScheduledFor, &run.Status, &run.TitleSnapshot, &run.AgentIDSnapshot, &run.PermissionModeSnapshot, &run.ProfileRevisionSnapshot, &run.RouteRevisionSnapshot, &run.PromptSnapshot, &skillsJSON, &run.ConversationID, &run.RunID, &run.FailureReason, &run.CreatedAt, &startedAt, &finishedAt); err != nil {
		return ScheduledTaskRun{}, err
	}
	if err := json.Unmarshal([]byte(skillsJSON), &run.SkillsSnapshot); err != nil {
		return ScheduledTaskRun{}, fmt.Errorf("decode scheduled task run skills: %w", err)
	}
	if run.SkillsSnapshot == nil {
		run.SkillsSnapshot = []string{}
	}
	if startedAt.Valid {
		value := startedAt.Time.UTC()
		run.StartedAt = &value
	}
	if finishedAt.Valid {
		value := finishedAt.Time.UTC()
		run.FinishedAt = &value
	}
	return run, nil
}

func (s *Server) scheduledTaskRunByID(ctx context.Context, id string) (ScheduledTaskRun, error) {
	return scanScheduledTaskRun(s.db.QueryRowContext(ctx, `select `+scheduledTaskRunSelect+` from scheduled_task_runs where id=?`, id))
}

func (s *Server) scheduledTaskRunByRunID(ctx context.Context, runID string) (ScheduledTaskRun, error) {
	return scanScheduledTaskRun(s.db.QueryRowContext(ctx, `select `+scheduledTaskRunSelect+` from scheduled_task_runs where run_id=?`, runID))
}

func (s *Server) latestScheduledTaskRun(ctx context.Context, taskID string) (ScheduledTaskRun, error) {
	return scanScheduledTaskRun(s.db.QueryRowContext(ctx, `select `+scheduledTaskRunSelect+` from scheduled_task_runs where scheduled_task_id=? order by created_at desc limit 1`, taskID))
}

func (s *Server) scheduledTaskRuns(ctx context.Context, taskID string, limit int) ([]ScheduledTaskRun, error) {
	rows, err := s.db.QueryContext(ctx, `select `+scheduledTaskRunSelect+` from scheduled_task_runs where scheduled_task_id=? order by created_at desc limit ?`, taskID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := []ScheduledTaskRun{}
	for rows.Next() {
		run, err := scanScheduledTaskRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

// startScheduledTaskLoop intentionally belongs to the control service lifetime.
// It is cancelled by Server.Close, so closing Milevia never leaves a scheduler
// running in the background and missed schedules are not replayed on restart.
func (s *Server) startScheduledTaskLoop() {
	s.runWG.Add(1)
	go func() {
		defer s.runWG.Done()
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			s.runDueScheduledTasks(s.runtimeCtx, time.Now().UTC())
			select {
			case <-s.runtimeCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (s *Server) runDueScheduledTasks(ctx context.Context, now time.Time) {
	if ctx.Err() != nil {
		return
	}
	if _, err := s.claimDueScheduledTaskRuns(ctx, now); err != nil {
		if !errors.Is(err, context.Canceled) {
			log.Printf("claim due scheduled tasks: %v", err)
		}
		return
	}
	rows, err := s.db.QueryContext(ctx, `select id from scheduled_task_runs where status=? and run_id='' order by created_at limit 32`, scheduledRunQueued)
	if err != nil {
		log.Printf("list queued scheduled task runs: %v", err)
		return
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			log.Printf("read queued scheduled task run: %v", err)
			return
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		log.Printf("close queued scheduled task runs: %v", err)
		return
	}
	for _, id := range ids {
		s.launchScheduledTaskRun(ctx, id)
	}
}

func (s *Server) claimDueScheduledTaskRuns(ctx context.Context, now time.Time) ([]ScheduledTaskRun, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `select `+scheduledTaskSelect+` from scheduled_tasks
		where enabled=1 and next_run_at is not null and next_run_at<=?
		and not exists(select 1 from scheduled_task_runs r where r.scheduled_task_id=scheduled_tasks.id and r.status in ('queued','running'))
		order by next_run_at limit 32`, now)
	if err != nil {
		return nil, err
	}
	tasks := []ScheduledTask{}
	for rows.Next() {
		task, scanErr := scanScheduledTask(rows)
		if scanErr != nil {
			rows.Close()
			return nil, scanErr
		}
		tasks = append(tasks, task)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	claimed := make([]ScheduledTaskRun, 0, len(tasks))
	for _, task := range tasks {
		if task.NextRunAt == nil {
			continue
		}
		selection, profileErr := s.scheduledRunProfileRevisionTx(ctx, tx, task)
		if profileErr != nil {
			return nil, profileErr
		}
		run := ScheduledTaskRun{
			ID:                      uuid.NewString(),
			ScheduledTaskID:         task.ID,
			ScheduledFor:            *task.NextRunAt,
			Status:                  scheduledRunQueued,
			TitleSnapshot:           task.Title,
			AgentIDSnapshot:         task.AgentID,
			PermissionModeSnapshot:  task.PermissionMode,
			ProfileRevisionSnapshot: selection.ProfileRevisionID,
			RouteRevisionSnapshot:   selection.RouteRevisionID,
			PromptSnapshot:          scheduledTaskPrompt(task.Prompt, task.Skills),
			SkillsSnapshot:          task.Skills,
			CreatedAt:               now,
		}
		skills, marshalErr := json.Marshal(run.SkillsSnapshot)
		if marshalErr != nil {
			return nil, marshalErr
		}
		if _, err := tx.ExecContext(ctx, `insert into scheduled_task_runs (id,scheduled_task_id,scheduled_for,status,title_snapshot,agent_id_snapshot,permission_mode_snapshot,profile_revision_snapshot,route_revision_snapshot,prompt_snapshot,skills_snapshot,created_at) values (?,?,?,?,?,?,?,?,?,?,?,?)`, run.ID, run.ScheduledTaskID, run.ScheduledFor, run.Status, run.TitleSnapshot, run.AgentIDSnapshot, run.PermissionModeSnapshot, run.ProfileRevisionSnapshot, run.RouteRevisionSnapshot, run.PromptSnapshot, string(skills), run.CreatedAt); err != nil {
			return nil, err
		}
		var next *time.Time
		enabled := task.Enabled
		if task.ScheduleType == scheduledTaskOnce {
			enabled = false
		} else {
			next, err = nextScheduledTaskOccurrence(task, now)
			if err != nil {
				return nil, err
			}
		}
		if _, err := tx.ExecContext(ctx, `update scheduled_tasks set enabled=?,next_run_at=?,updated_at=? where id=? and enabled=1 and next_run_at=?`, boolToInt(enabled), next, now, task.ID, task.NextRunAt); err != nil {
			return nil, err
		}
		claimed = append(claimed, run)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return claimed, nil
}

var errScheduledTaskAlreadyActive = errors.New("scheduled task already has an active run")

func (s *Server) enqueueScheduledTaskRun(ctx context.Context, task ScheduledTask, scheduledFor time.Time) (ScheduledTaskRun, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ScheduledTaskRun{}, err
	}
	defer tx.Rollback()
	var active bool
	if err := tx.QueryRowContext(ctx, `select exists(select 1 from scheduled_task_runs where scheduled_task_id=? and status in ('queued','running'))`, task.ID).Scan(&active); err != nil {
		return ScheduledTaskRun{}, err
	}
	if active {
		return ScheduledTaskRun{}, errScheduledTaskAlreadyActive
	}
	selection, err := s.scheduledRunProfileRevisionTx(ctx, tx, task)
	if err != nil {
		return ScheduledTaskRun{}, err
	}
	run := ScheduledTaskRun{
		ID:                      uuid.NewString(),
		ScheduledTaskID:         task.ID,
		ScheduledFor:            scheduledFor,
		Status:                  scheduledRunQueued,
		TitleSnapshot:           task.Title,
		AgentIDSnapshot:         task.AgentID,
		PermissionModeSnapshot:  task.PermissionMode,
		ProfileRevisionSnapshot: selection.ProfileRevisionID,
		RouteRevisionSnapshot:   selection.RouteRevisionID,
		PromptSnapshot:          scheduledTaskPrompt(task.Prompt, task.Skills),
		SkillsSnapshot:          task.Skills,
		CreatedAt:               time.Now().UTC(),
	}
	skills, err := json.Marshal(run.SkillsSnapshot)
	if err != nil {
		return ScheduledTaskRun{}, err
	}
	if _, err := tx.ExecContext(ctx, `insert into scheduled_task_runs (id,scheduled_task_id,scheduled_for,status,title_snapshot,agent_id_snapshot,permission_mode_snapshot,profile_revision_snapshot,route_revision_snapshot,prompt_snapshot,skills_snapshot,created_at) values (?,?,?,?,?,?,?,?,?,?,?,?)`, run.ID, run.ScheduledTaskID, run.ScheduledFor, run.Status, run.TitleSnapshot, run.AgentIDSnapshot, run.PermissionModeSnapshot, run.ProfileRevisionSnapshot, run.RouteRevisionSnapshot, run.PromptSnapshot, string(skills), run.CreatedAt); err != nil {
		return ScheduledTaskRun{}, err
	}
	if err := tx.Commit(); err != nil {
		return ScheduledTaskRun{}, err
	}
	return run, nil
}

func (s *Server) launchScheduledTaskRun(ctx context.Context, scheduledRunID string) {
	run, err := s.scheduledTaskRunByID(ctx, scheduledRunID)
	if errors.Is(err, sql.ErrNoRows) {
		return
	}
	if err != nil || run.Status != scheduledRunQueued || run.RunID != "" {
		return
	}
	task, err := s.scheduledTaskByID(ctx, run.ScheduledTaskID)
	if err != nil {
		s.failScheduledTaskRun(scheduledRunID, "scheduled task no longer exists")
		return
	}
	project, err := s.getProjectByID(ctx, task.ProjectID)
	if err != nil {
		s.failScheduledTaskRun(scheduledRunID, "project is unavailable")
		return
	}
	_, agentID, permissionMode := scheduledRunExecutionConfig(task, run)
	if !validAgentPolicy(agentID, permissionMode) {
		s.failScheduledTaskRun(scheduledRunID, "scheduled run has an unsupported execution policy")
		return
	}
	if err := s.validateScheduledTaskSkills(ctx, project, agentID, run.SkillsSnapshot); err != nil {
		s.failScheduledTaskRun(scheduledRunID, err.Error())
		return
	}
	conversation, err := s.createScheduledTaskConversation(ctx, project, task, run)
	if err != nil {
		s.failScheduledTaskRun(scheduledRunID, err.Error())
		return
	}
	record := &runStartRecord{Scheduled: &run}
	_, _, _, status, err := s.startMessage(ctx, conversation.ID, run.PromptSnapshot, "scheduled:"+run.ID, record)
	if err == nil {
		return
	}
	if (status == http.StatusConflict && strings.Contains(err.Error(), "project workspace is occupied")) || status == http.StatusTooManyRequests {
		reason := "waiting for the project workspace to become idle"
		if status == http.StatusTooManyRequests {
			reason = "waiting for credential quota availability"
		}
		_, _ = s.db.ExecContext(context.Background(), `update scheduled_task_runs set failure_reason=? where id=? and status=? and run_id=''`, reason, run.ID, scheduledRunQueued)
		return
	}
	s.failScheduledTaskRun(scheduledRunID, err.Error())
}

// scheduledRunExecutionConfig preserves the configuration that was selected
// when the occurrence was queued. Empty snapshots are from records created by
// earlier versions and deliberately fall back to the current task.
func scheduledRunExecutionConfig(task ScheduledTask, run ScheduledTaskRun) (title, agentID, permissionMode string) {
	title, agentID, permissionMode = run.TitleSnapshot, run.AgentIDSnapshot, run.PermissionModeSnapshot
	if title == "" {
		title = task.Title
	}
	if agentID == "" {
		agentID = task.AgentID
	}
	if permissionMode == "" {
		permissionMode = task.PermissionMode
	}
	return title, agentID, permissionMode
}

func (s *Server) scheduledRunProfileRevisionTx(ctx context.Context, tx *sql.Tx, task ScheduledTask) (profileRouteSelection, error) {
	var runnerID string
	if err := tx.QueryRowContext(ctx, `select coalesce(nullif(runner_id,''),runner) from projects where id=?`, task.ProjectID).Scan(&runnerID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return profileRouteSelection{}, errors.New("project is unavailable")
		}
		return profileRouteSelection{}, err
	}
	return s.profileRouteForNewConversationTx(ctx, tx, nil, runnerID, task.AgentID, task.ProjectID)
}

func (s *Server) createScheduledTaskConversation(ctx context.Context, project Project, task ScheduledTask, run ScheduledTaskRun) (Conversation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Conversation{}, err
	}
	defer tx.Rollback()
	var status, existingConversationID, existingRunID string
	err = tx.QueryRowContext(ctx, `select status,conversation_id,run_id from scheduled_task_runs where id=?`, run.ID).Scan(&status, &existingConversationID, &existingRunID)
	if err != nil {
		return Conversation{}, err
	}
	if status != scheduledRunQueued || existingRunID != "" {
		return Conversation{}, errors.New("scheduled run is no longer ready to start")
	}
	if existingConversationID != "" {
		var existing Conversation
		err = tx.QueryRowContext(ctx, `select id,project_id,claude_session_id,agent_id,agent_session_id,agent_runtime_id,agent_profile_revision_id,project_agent_route_revision_id,execution_policy,status,permission_mode,title,last_activity_at,claude_initialized,agent_initialized,is_current,created_at from conversations where id=?`, existingConversationID).Scan(&existing.ID, &existing.ProjectID, &existing.ClaudeSessionID, &existing.AgentID, &existing.AgentSessionID, &existing.AgentRuntimeID, &existing.AgentProfileRevisionID, &existing.ProjectAgentRouteRevisionID, &existing.ExecutionPolicy, &existing.Status, &existing.PermissionMode, &existing.Title, &existing.LastActivityAt, &existing.ClaudeInitialized, &existing.AgentInitialized, &existing.IsCurrent, &existing.CreatedAt)
		if err != nil {
			return Conversation{}, err
		}
		if err := tx.Commit(); err != nil {
			return Conversation{}, err
		}
		return existing, nil
	}
	title, agentID, permissionMode := scheduledRunExecutionConfig(task, run)
	profileRevisionID := run.ProfileRevisionSnapshot
	routeRevisionID := run.RouteRevisionSnapshot
	if run.AgentIDSnapshot == "" {
		selection, selectionErr := s.profileRouteForNewConversationTx(ctx, tx, nil, project.Runner, agentID, task.ProjectID)
		profileRevisionID, routeRevisionID, err = selection.ProfileRevisionID, selection.RouteRevisionID, selectionErr
		if err != nil {
			return Conversation{}, err
		}
	}
	now := time.Now().UTC()
	sessionID := uuid.NewString()
	conversation := Conversation{ID: uuid.NewString(), ProjectID: task.ProjectID, ClaudeSessionID: sessionID, AgentID: agentID, AgentSessionID: sessionID, AgentRuntimeID: project.Runner, AgentProfileRevisionID: profileRevisionID, ProjectAgentRouteRevisionID: routeRevisionID, ExecutionPolicy: permissionMode, Status: "idle", PermissionMode: permissionMode, Title: "Scheduled: " + title, LastActivityAt: now, IsCurrent: false, CreatedAt: now}
	if _, err := tx.ExecContext(ctx, `insert into conversations (id,project_id,claude_session_id,agent_id,agent_session_id,agent_runtime_id,agent_profile_revision_id,project_agent_route_revision_id,execution_policy,status,permission_mode,title,last_activity_at,claude_initialized,agent_initialized,is_current,created_at,origin) values (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, conversation.ID, conversation.ProjectID, conversation.ClaudeSessionID, conversation.AgentID, conversation.AgentSessionID, conversation.AgentRuntimeID, conversation.AgentProfileRevisionID, conversation.ProjectAgentRouteRevisionID, conversation.ExecutionPolicy, conversation.Status, conversation.PermissionMode, conversation.Title, conversation.LastActivityAt, conversation.ClaudeInitialized, conversation.AgentInitialized, conversation.IsCurrent, conversation.CreatedAt, "scheduled"); err != nil {
		return Conversation{}, err
	}
	result, err := tx.ExecContext(ctx, `update scheduled_task_runs set conversation_id=? where id=? and status=? and conversation_id=''`, conversation.ID, run.ID, scheduledRunQueued)
	if err != nil {
		return Conversation{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return Conversation{}, err
	}
	if changed != 1 {
		return Conversation{}, errors.New("scheduled run state changed before conversation creation")
	}
	if err := tx.Commit(); err != nil {
		return Conversation{}, err
	}
	return conversation, nil
}

func (s *Server) failScheduledTaskRun(runID, reason string) {
	now := time.Now().UTC()
	result, err := s.db.ExecContext(context.Background(), `update scheduled_task_runs set status=?,failure_reason=?,finished_at=? where id=? and status=?`, scheduledRunFailed, reason, now, runID, scheduledRunQueued)
	if err != nil {
		log.Printf("fail scheduled task run %s: %v", runID, err)
		return
	}
	changed, err := result.RowsAffected()
	if err == nil && changed == 1 {
		_, _ = s.db.ExecContext(context.Background(), `update scheduled_tasks set last_run_at=?,updated_at=? where id=(select scheduled_task_id from scheduled_task_runs where id=?)`, now, now, runID)
		s.notifyScheduledTaskRun(runID)
	}
}

func (s *Server) finishScheduledTaskRunTx(ctx context.Context, tx *sql.Tx, runID, runStatus, failureReason string, now time.Time) error {
	var scheduledRunID, scheduledTaskID, scheduledStatus string
	err := tx.QueryRowContext(ctx, `select id,scheduled_task_id,status from scheduled_task_runs where run_id=?`, runID).Scan(&scheduledRunID, &scheduledTaskID, &scheduledStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if scheduledStatus != scheduledRunQueued && scheduledStatus != scheduledRunRunning {
		return nil
	}
	terminal := scheduledRunFailed
	switch runStatus {
	case "completed":
		terminal = scheduledRunSucceeded
	case "failed":
		terminal = scheduledRunFailed
	case "stopped":
		terminal = scheduledRunStopped
	case "interrupted":
		terminal = scheduledRunInterrupted
	default:
		return fmt.Errorf("unsupported scheduled run terminal status: %s", runStatus)
	}
	if failureReason == "" && terminal != scheduledRunSucceeded {
		failureReason = terminal
	}
	result, err := tx.ExecContext(ctx, `update scheduled_task_runs set status=?,failure_reason=?,finished_at=? where id=? and status in ('queued','running')`, terminal, failureReason, now, scheduledRunID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return errors.New("scheduled task run status changed before completion")
	}
	if _, err := tx.ExecContext(ctx, `update scheduled_tasks set last_run_at=?,updated_at=? where id=?`, now, now, scheduledTaskID); err != nil {
		return err
	}
	return nil
}

func (s *Server) recoverInterruptedScheduledTasks(ctx context.Context) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `update scheduled_task_runs set status=?,failure_reason=?,finished_at=? where status=? and run_id=''`, scheduledRunInterrupted, "Milevia was closed before this scheduled run could start.", now, scheduledRunQueued)
	if err != nil {
		return fmt.Errorf("recover queued scheduled task runs: %w", err)
	}
	return nil
}

// skipMissedScheduledTasks applies the documented desktop-only semantics. A
// service restart never replays an occurrence that elapsed while Milevia was
// closed; repeating schedules advance to their next future occurrence.
func (s *Server) skipMissedScheduledTasks(ctx context.Context, now time.Time) error {
	rows, err := s.db.QueryContext(ctx, `select `+scheduledTaskSelect+` from scheduled_tasks where enabled=1 and next_run_at is not null and next_run_at<?`, now)
	if err != nil {
		return err
	}
	tasks := []ScheduledTask{}
	for rows.Next() {
		task, scanErr := scanScheduledTask(rows)
		if scanErr != nil {
			rows.Close()
			return scanErr
		}
		tasks = append(tasks, task)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, task := range tasks {
		next := (*time.Time)(nil)
		enabled := task.Enabled
		if task.ScheduleType == scheduledTaskOnce {
			enabled = false
		} else {
			var nextErr error
			next, nextErr = nextScheduledTaskOccurrence(task, now)
			if nextErr != nil {
				return nextErr
			}
		}
		if _, err := s.db.ExecContext(ctx, `update scheduled_tasks set enabled=?,next_run_at=?,updated_at=? where id=? and enabled=1`, boolToInt(enabled), next, now, task.ID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) isScheduledConversation(ctx context.Context, conversationID string) bool {
	var exists bool
	// The task (and its run records) can be deleted after a completed run while
	// the corresponding conversation remains available as history. Its own
	// origin is therefore the durable authority for the read-only boundary.
	err := s.db.QueryRowContext(ctx, `select exists(select 1 from conversations where id=? and origin='scheduled')`, conversationID).Scan(&exists)
	return err == nil && exists
}

func (s *Server) notifyScheduledTaskRun(scheduledRunID string) {
	ctx := s.runtimeCtx
	var task ScheduledTask
	run, err := s.scheduledTaskRunByID(ctx, scheduledRunID)
	if err != nil {
		return
	}
	task, err = s.scheduledTaskByID(ctx, run.ScheduledTaskID)
	if err != nil {
		return
	}
	project, err := s.getProjectByID(ctx, task.ProjectID)
	if err != nil {
		return
	}
	var typ, title, body, priority string
	switch run.Status {
	case scheduledRunSucceeded:
		typ, title, body, priority = "scheduled_task.succeeded", "定时任务已完成", "定时任务“"+task.Title+"”已在项目“"+project.Name+"”中完成。", "normal"
	case scheduledRunFailed:
		typ, title, body, priority = "scheduled_task.failed", "定时任务需要处理", "定时任务“"+task.Title+"”执行失败："+run.FailureReason, "high"
	case scheduledRunStopped:
		typ, title, body, priority = "scheduled_task.stopped", "定时任务已停止", "定时任务“"+task.Title+"”已停止。", "normal"
	default:
		return
	}
	s.broadcastNotification(NotificationEvent{ID: uuid.NewString(), Type: typ, ProjectID: task.ProjectID, ProjectName: project.Name, ConversationID: run.ConversationID, Title: title, Body: body, Priority: priority, ActionURL: "/projects/" + task.ProjectID + "/tasks/schedules/" + task.ID, DedupeKey: "scheduled-task-run:" + run.ID})
}
