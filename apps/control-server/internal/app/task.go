package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const (
	taskTodo           = "todo"
	taskRunning        = "running"
	taskAwaitingReview = "awaiting_review"
	taskActionRequired = "action_required"
	taskDone           = "done"
	taskCancelled      = "cancelled"
)

type Task struct {
	ID                     string           `json:"id"`
	ProjectID              string           `json:"projectId"`
	Title                  string           `json:"title"`
	Description            string           `json:"description"`
	AcceptanceCriteria     string           `json:"acceptanceCriteria"`
	Priority               string           `json:"priority"`
	Pinned                 bool             `json:"pinned"`
	Position               float64          `json:"position"`
	Status                 string           `json:"status"`
	DependsOn              []TaskDependency `json:"dependsOn"`
	BlockedBy              []TaskBlocker    `json:"blockedBy"`
	Blocks                 []TaskDependency `json:"blocks"`
	LastRun                *TaskRun         `json:"lastRun,omitempty"`
	OrchestrationStatus    string           `json:"orchestrationStatus,omitempty"`
	OrchestrationUpdatedAt *time.Time       `json:"orchestrationUpdatedAt,omitempty"`
	CreatedAt              time.Time        `json:"createdAt"`
	UpdatedAt              time.Time        `json:"updatedAt"`
	CompletedAt            *time.Time       `json:"completedAt,omitempty"`
	CancelledAt            *time.Time       `json:"cancelledAt,omitempty"`
}

type TaskDependency struct {
	TaskID string `json:"taskId"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

type TaskBlocker struct {
	TaskID string `json:"taskId"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

type TaskRun struct {
	ID                 string     `json:"id"`
	TaskID             string     `json:"taskId"`
	ConversationID     string     `json:"conversationId"`
	RunID              string     `json:"runId"`
	Sequence           int        `json:"sequence"`
	Status             string     `json:"status"`
	PromptSnapshot     string     `json:"promptSnapshot"`
	AcceptanceSnapshot string     `json:"acceptanceSnapshot"`
	FailureReason      string     `json:"failureReason"`
	CreatedAt          time.Time  `json:"createdAt"`
	StartedAt          *time.Time `json:"startedAt,omitempty"`
	FinishedAt         *time.Time `json:"finishedAt,omitempty"`
	ExecutionIntentID  string     `json:"-"`
}

type TaskEvent struct {
	ID        string          `json:"id"`
	TaskID    string          `json:"taskId"`
	TaskRunID string          `json:"taskRunId,omitempty"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"createdAt"`
}

type VerificationRun struct {
	ID          string     `json:"id"`
	Phase       string     `json:"phase"`
	Command     string     `json:"command"`
	ReviewedSHA string     `json:"reviewedSha,omitempty"`
	Status      string     `json:"status"`
	ExitCode    int        `json:"exitCode"`
	Output      string     `json:"output,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
}

type TaskDetail struct {
	Task
	PromptPreview    string            `json:"promptPreview"`
	CanDispatch      bool              `json:"canDispatch"`
	BlockReason      string            `json:"blockReason,omitempty"`
	Runs             []TaskRun         `json:"runs"`
	Events           []TaskEvent       `json:"events"`
	VerificationRuns []VerificationRun `json:"verificationRuns"`
}

type taskInput struct {
	Title              string    `json:"title"`
	Description        string    `json:"description"`
	AcceptanceCriteria string    `json:"acceptanceCriteria"`
	Priority           string    `json:"priority"`
	Pinned             *bool     `json:"pinned"`
	Position           *float64  `json:"position"`
	Status             string    `json:"status"`
	PredecessorTaskIDs *[]string `json:"predecessorTaskIds"`
}

func (s *Server) migrateTasks(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `create table if not exists tasks (
	id text primary key,
	project_id text not null references projects(id) on delete cascade,
	title text not null,
	description text not null default '',
	acceptance_criteria text not null default '',
	priority text not null default 'normal',
	pinned integer not null default 0,
	position real not null default 0,
	status text not null default 'todo',
	last_task_run_id text,
	created_at datetime not null,
	updated_at datetime not null,
	completed_at datetime,
	cancelled_at datetime
);
create table if not exists task_dependencies (
	task_id text not null references tasks(id) on delete cascade,
	predecessor_task_id text not null references tasks(id) on delete restrict,
	created_at datetime not null,
	primary key (task_id, predecessor_task_id)
);
create table if not exists task_runs (
	id text primary key,
	task_id text not null references tasks(id) on delete cascade,
	conversation_id text not null references conversations(id) on delete restrict,
	run_id text unique references runs(id) on delete set null,
	sequence integer not null,
	status text not null,
	prompt_snapshot text not null,
	acceptance_snapshot text not null,
	failure_reason text not null default '',
	created_at datetime not null,
	started_at datetime,
	finished_at datetime
);
create table if not exists task_events (
	id text primary key,
	task_id text not null references tasks(id) on delete cascade,
	task_run_id text references task_runs(id) on delete set null,
	type text not null,
	payload text not null default '{}',
	created_at datetime not null
);
create index if not exists tasks_project_status_priority on tasks(project_id,status,priority,position);
create index if not exists task_dependencies_predecessor on task_dependencies(predecessor_task_id);
create index if not exists task_runs_task_created on task_runs(task_id,created_at desc);
create index if not exists task_runs_run on task_runs(run_id);
create index if not exists task_events_task_created on task_events(task_id,created_at);`)
	if err != nil {
		return fmt.Errorf("migrate tasks: %w", err)
	}
	if err := dropColumnIfPresent(ctx, s.db, "tasks", "execution_mode"); err != nil {
		return fmt.Errorf("remove legacy task execution mode: %w", err)
	}
	if err := ensureColumn(ctx, s.db, "tasks", "pinned", "integer not null default 0"); err != nil {
		return fmt.Errorf("add tasks.pinned: %w", err)
	}
	return nil
}

func dropColumnIfPresent(ctx context.Context, db *sql.DB, table, column string) error {
	rows, err := db.QueryContext(ctx, fmt.Sprintf("pragma table_info(%s)", table))
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			return err
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !found {
		return nil
	}
	_, err = db.ExecContext(ctx, fmt.Sprintf("alter table %s drop column %s", table, column))
	return err
}

func validTaskPriority(value string) bool {
	return value == "urgent" || value == "high" || value == "normal" || value == "low"
}

func validTaskStatus(value string) bool {
	return value == taskTodo || value == taskRunning || value == taskAwaitingReview || value == taskActionRequired || value == taskDone || value == taskCancelled
}

func rejectCancelledTaskMutation(w http.ResponseWriter, task Task) bool {
	if task.Status != taskCancelled {
		return false
	}
	writeError(w, http.StatusConflict, errors.New("cancelled tasks can only be deleted"))
	return true
}

func canUpdateTaskStatus(from, to string) bool {
	if from == to {
		return true
	}
	return (from == taskTodo && to == taskActionRequired) || (from == taskActionRequired && to == taskTodo)
}

func validateTaskInput(input *taskInput) error {
	input.Title = strings.TrimSpace(input.Title)
	input.Description = strings.TrimSpace(input.Description)
	input.AcceptanceCriteria = strings.TrimSpace(input.AcceptanceCriteria)
	if input.Description == "" {
		return errors.New("task description is required")
	}
	if len([]rune(input.Title)) > 120 || len([]rune(input.Description)) > 12000 || len([]rune(input.AcceptanceCriteria)) > 12000 {
		return errors.New("task content exceeds the allowed length")
	}
	if input.Priority == "" {
		input.Priority = "normal"
	}
	if !validTaskPriority(input.Priority) {
		return errors.New("unsupported task priority")
	}
	return nil
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	if !s.projectExists(r.Context(), projectID) {
		writeError(w, http.StatusNotFound, errors.New("project not found"))
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `select id,project_id,title,description,acceptance_criteria,priority,pinned,position,status,created_at,updated_at,completed_at,cancelled_at from tasks where project_id=? order by pinned desc,position,created_at`, projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	items := []Task{}
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		items = append(items, task)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := rows.Close(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	for index := range items {
		if err := s.hydrateTask(r.Context(), &items[index]); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	if !s.projectExists(r.Context(), projectID) {
		writeError(w, http.StatusNotFound, errors.New("project not found"))
		return
	}
	var input taskInput
	if !decode(w, r, &input) {
		return
	}
	if err := validateTaskInput(&input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	now := time.Now().UTC()
	position := 0.0
	if input.Position != nil {
		position = *input.Position
	}
	task := Task{ID: uuid.NewString(), ProjectID: projectID, Title: input.Title, Description: input.Description, AcceptanceCriteria: input.AcceptanceCriteria, Priority: input.Priority, Position: position, Status: taskTodo, CreatedAt: now, UpdatedAt: now, DependsOn: []TaskDependency{}, BlockedBy: []TaskBlocker{}, Blocks: []TaskDependency{}}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	if task.Position == 0 {
		if err := tx.QueryRowContext(r.Context(), `select coalesce(max(position),0)+1 from tasks where project_id=?`, projectID).Scan(&task.Position); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	if _, err := tx.ExecContext(r.Context(), `insert into tasks (id,project_id,title,description,acceptance_criteria,priority,pinned,position,status,created_at,updated_at) values (?,?,?,?,?,?,?,?,?,?,?)`, task.ID, task.ProjectID, task.Title, task.Description, task.AcceptanceCriteria, task.Priority, task.Pinned, task.Position, task.Status, task.CreatedAt, task.UpdatedAt); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if input.PredecessorTaskIDs != nil {
		seen := map[string]struct{}{}
		for _, predecessorID := range *input.PredecessorTaskIDs {
			predecessorID = strings.TrimSpace(predecessorID)
			if predecessorID == "" || predecessorID == task.ID {
				writeError(w, http.StatusBadRequest, errors.New("a task cannot depend on itself"))
				return
			}
			if _, exists := seen[predecessorID]; exists {
				writeError(w, http.StatusBadRequest, errors.New("duplicate predecessor task"))
				return
			}
			seen[predecessorID] = struct{}{}
			var predecessorProjectID string
			if err := tx.QueryRowContext(r.Context(), `select project_id from tasks where id=?`, predecessorID).Scan(&predecessorProjectID); errors.Is(err, sql.ErrNoRows) || predecessorProjectID != projectID {
				writeError(w, http.StatusBadRequest, errors.New("predecessor task must belong to the same project"))
				return
			} else if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
		}
		for predecessorID := range seen {
			if _, err := tx.ExecContext(r.Context(), `insert into task_dependencies (task_id,predecessor_task_id,created_at) values (?,?,?)`, task.ID, predecessorID, now); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
		}
	}
	if err := recordTaskEventTx(r.Context(), tx, task.ID, "", "task.created", map[string]string{"status": task.Status}, now); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, task)
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	task, err := s.taskByID(r.Context(), chi.URLParam(r, "taskID"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("task not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	detail := TaskDetail{Task: task, PromptPreview: taskPrompt(task), Runs: []TaskRun{}, Events: []TaskEvent{}, VerificationRuns: []VerificationRun{}}
	detail.CanDispatch, detail.BlockReason, err = s.taskDispatchEligibility(r.Context(), task, false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	detail.Runs, err = s.taskRuns(r.Context(), task.ID)
	if err == nil {
		detail.Events, err = s.taskEvents(r.Context(), task.ID)
	}
	if err == nil {
		detail.VerificationRuns, err = s.verificationRunsForTask(r.Context(), task.ID)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) updateTask(w http.ResponseWriter, r *http.Request) {
	task, err := s.taskByID(r.Context(), chi.URLParam(r, "taskID"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("task not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if rejectCancelledTaskMutation(w, task) {
		return
	}
	var input taskInput
	if !decode(w, r, &input) {
		return
	}
	if err := validateTaskInput(&input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	originalStatus := task.Status
	if input.Status != "" {
		if !validTaskStatus(input.Status) {
			writeError(w, http.StatusBadRequest, errors.New("unsupported task status"))
			return
		}
		if !canUpdateTaskStatus(task.Status, input.Status) {
			writeError(w, http.StatusConflict, errors.New("task cannot transition from its current status"))
			return
		}
		task.Status = input.Status
	}
	if (task.Status == taskRunning || task.Status == taskAwaitingReview) && (task.Description != input.Description || task.AcceptanceCriteria != input.AcceptanceCriteria) {
		writeError(w, http.StatusConflict, errors.New("cannot change execution content while a task is running or awaiting review"))
		return
	}
	task.Title, task.Description, task.AcceptanceCriteria, task.Priority = input.Title, input.Description, input.AcceptanceCriteria, input.Priority
	if input.Pinned != nil {
		task.Pinned = *input.Pinned
	}
	if input.Position != nil {
		task.Position = *input.Position
	}
	task.UpdatedAt = time.Now().UTC()
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(r.Context(), `update tasks set title=?,description=?,acceptance_criteria=?,priority=?,pinned=?,position=?,status=?,updated_at=? where id=? and status=?`, task.Title, task.Description, task.AcceptanceCriteria, task.Priority, task.Pinned, task.Position, task.Status, task.UpdatedAt, task.ID, originalStatus)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	changed, err := result.RowsAffected()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if changed != 1 {
		writeError(w, http.StatusConflict, errors.New("task started before execution content could be updated"))
		return
	}
	if input.PredecessorTaskIDs != nil {
		var currentStatus string
		if err := tx.QueryRowContext(r.Context(), `select status from tasks where id=?`, task.ID).Scan(&currentStatus); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if currentStatus == taskRunning || currentStatus == taskAwaitingReview {
			writeError(w, http.StatusConflict, errors.New("cannot change dependencies while a task is running or awaiting review"))
			return
		}
		seen := map[string]struct{}{}
		for _, predecessorID := range *input.PredecessorTaskIDs {
			predecessorID = strings.TrimSpace(predecessorID)
			if predecessorID == "" || predecessorID == task.ID {
				writeError(w, http.StatusBadRequest, errors.New("a task cannot depend on itself"))
				return
			}
			if _, exists := seen[predecessorID]; exists {
				writeError(w, http.StatusBadRequest, errors.New("duplicate predecessor task"))
				return
			}
			seen[predecessorID] = struct{}{}
			var projectID string
			if err := tx.QueryRowContext(r.Context(), `select project_id from tasks where id=?`, predecessorID).Scan(&projectID); errors.Is(err, sql.ErrNoRows) || projectID != task.ProjectID {
				writeError(w, http.StatusBadRequest, errors.New("predecessor task must belong to the same project"))
				return
			} else if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			cycle, err := s.wouldCreateTaskCycleTx(r.Context(), tx, task.ID, predecessorID)
			if err != nil || cycle {
				if err != nil {
					writeError(w, http.StatusInternalServerError, err)
				} else {
					writeError(w, http.StatusBadRequest, errors.New("dependency would create a cycle"))
				}
				return
			}
		}
		if _, err := tx.ExecContext(r.Context(), `delete from task_dependencies where task_id=?`, task.ID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		for predecessorID := range seen {
			if _, err := tx.ExecContext(r.Context(), `insert into task_dependencies (task_id,predecessor_task_id,created_at) values (?,?,?)`, task.ID, predecessorID, task.UpdatedAt); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
		}
		if err := recordTaskEventTx(r.Context(), tx, task.ID, "", "task.dependencies_replaced", map[string]any{"predecessorTaskIds": *input.PredecessorTaskIDs}, task.UpdatedAt); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	if err := recordTaskEventTx(r.Context(), tx, task.ID, "", "task.updated", map[string]string{"title": task.Title, "priority": task.Priority}, task.UpdatedAt); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// 通知：任务状态变更（仅对需要关注的状态发通知）
	if originalStatus != task.Status {
		s.notifyTaskStatusChange(r.Context(), task.ID, task.Status)
	}
	writeJSON(w, http.StatusOK, task)
}

func (s *Server) deleteTask(w http.ResponseWriter, r *http.Request) {
	// Keep deletion ordered with task dispatch. Otherwise a task can start after
	// the initial status read and be deleted with its task run by cascading FKs.
	s.projectLifecycleMu.Lock()
	defer s.projectLifecycleMu.Unlock()

	task, err := s.taskByID(r.Context(), chi.URLParam(r, "taskID"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("task not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if task.Status == taskRunning {
		writeError(w, http.StatusConflict, errors.New("stop the active task before deleting it"))
		return
	}
	if s.hasActiveOrchestrationJob(r.Context(), task.ID) {
		writeError(w, http.StatusConflict, errors.New("pause or resolve the active orchestration job before deleting the task"))
		return
	}
	var hasOrchestrationHistory bool
	if err := s.db.QueryRowContext(r.Context(), `select exists(select 1 from task_orchestration_jobs where task_id=?)`, task.ID).Scan(&hasOrchestrationHistory); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if hasOrchestrationHistory {
		writeError(w, http.StatusConflict, errors.New("orchestrated tasks are retained for audit and cannot be deleted"))
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(r.Context(), `delete from task_dependencies where task_id=? or predecessor_task_id=?`, task.ID, task.ID); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("delete task dependencies: %w", err))
		return
	}
	result, err := tx.ExecContext(r.Context(), `delete from tasks where id=? and status<>?`, task.ID, taskRunning)
	if err != nil {
		writeError(w, http.StatusConflict, fmt.Errorf("delete task: %w", err))
		return
	}
	changed, err := result.RowsAffected()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if changed != 1 {
		writeError(w, http.StatusConflict, errors.New("task started before it could be deleted"))
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) addTaskDependency(w http.ResponseWriter, r *http.Request) {
	task, err := s.taskByID(r.Context(), chi.URLParam(r, "taskID"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("task not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if rejectCancelledTaskMutation(w, task) {
		return
	}
	if task.Status == taskRunning || task.Status == taskAwaitingReview {
		writeError(w, http.StatusConflict, errors.New("cannot change dependencies while a task is running or awaiting review"))
		return
	}
	var input struct {
		PredecessorTaskID string `json:"predecessorTaskId"`
	}
	if !decode(w, r, &input) {
		return
	}
	predecessor, err := s.taskByID(r.Context(), strings.TrimSpace(input.PredecessorTaskID))
	if errors.Is(err, sql.ErrNoRows) || predecessor.ProjectID != task.ProjectID {
		writeError(w, http.StatusBadRequest, errors.New("predecessor task must belong to the same project"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if predecessor.ID == task.ID {
		writeError(w, http.StatusBadRequest, errors.New("a task cannot depend on itself"))
		return
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	var taskProjectID, taskStatus, predecessorProjectID string
	if err := tx.QueryRowContext(r.Context(), `select project_id,status from tasks where id=?`, task.ID).Scan(&taskProjectID, &taskStatus); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := tx.QueryRowContext(r.Context(), `select project_id from tasks where id=?`, predecessor.ID).Scan(&predecessorProjectID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if taskStatus == taskRunning || taskStatus == taskAwaitingReview {
		writeError(w, http.StatusConflict, errors.New("cannot change dependencies while a task is running or awaiting review"))
		return
	}
	if taskProjectID != predecessorProjectID {
		writeError(w, http.StatusBadRequest, errors.New("predecessor task must belong to the same project"))
		return
	}
	cycle, err := s.wouldCreateTaskCycleTx(r.Context(), tx, task.ID, predecessor.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if cycle {
		writeError(w, http.StatusBadRequest, errors.New("dependency would create a cycle"))
		return
	}
	result, err := tx.ExecContext(r.Context(), `insert or ignore into task_dependencies (task_id,predecessor_task_id,created_at) values (?,?,?)`, task.ID, predecessor.ID, now)
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
		writeError(w, http.StatusConflict, errors.New("dependency already exists"))
		return
	}
	if err := recordTaskEventTx(r.Context(), tx, task.ID, "", "task.dependency_added", map[string]string{"predecessorTaskId": predecessor.ID}, now); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, TaskDependency{TaskID: predecessor.ID, Title: predecessor.Title, Status: predecessor.Status})
}

func (s *Server) deleteTaskDependency(w http.ResponseWriter, r *http.Request) {
	task, err := s.taskByID(r.Context(), chi.URLParam(r, "taskID"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("task not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if rejectCancelledTaskMutation(w, task) {
		return
	}
	if task.Status == taskRunning || task.Status == taskAwaitingReview {
		writeError(w, http.StatusConflict, errors.New("cannot change dependencies while a task is running or awaiting review"))
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRowContext(r.Context(), `select status from tasks where id=?`, task.ID).Scan(&status); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if status == taskRunning || status == taskAwaitingReview {
		writeError(w, http.StatusConflict, errors.New("cannot change dependencies while a task is running or awaiting review"))
		return
	}
	result, err := tx.ExecContext(r.Context(), `delete from task_dependencies where task_id=? and predecessor_task_id=?`, task.ID, chi.URLParam(r, "predecessorTaskID"))
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
		writeError(w, http.StatusNotFound, errors.New("dependency not found"))
		return
	}
	if err := recordTaskEventTx(r.Context(), tx, task.ID, "", "task.dependency_removed", map[string]string{"predecessorTaskId": chi.URLParam(r, "predecessorTaskID")}, time.Now().UTC()); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) replaceTaskDependencies(w http.ResponseWriter, r *http.Request) {
	task, err := s.taskByID(r.Context(), chi.URLParam(r, "taskID"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("task not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if rejectCancelledTaskMutation(w, task) {
		return
	}
	var input struct {
		PredecessorTaskIDs []string `json:"predecessorTaskIds"`
	}
	if !decode(w, r, &input) {
		return
	}
	unique := make(map[string]struct{}, len(input.PredecessorTaskIDs))
	ids := make([]string, 0, len(input.PredecessorTaskIDs))
	for _, id := range input.PredecessorTaskIDs {
		id = strings.TrimSpace(id)
		if id == "" || id == task.ID {
			writeError(w, http.StatusBadRequest, errors.New("a task cannot depend on itself"))
			return
		}
		if _, exists := unique[id]; exists {
			writeError(w, http.StatusBadRequest, errors.New("duplicate predecessor task"))
			return
		}
		unique[id] = struct{}{}
		ids = append(ids, id)
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRowContext(r.Context(), `select status from tasks where id=?`, task.ID).Scan(&status); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if status == taskRunning || status == taskAwaitingReview {
		writeError(w, http.StatusConflict, errors.New("cannot change dependencies while a task is running or awaiting review"))
		return
	}
	for _, predecessorID := range ids {
		var projectID string
		if err := tx.QueryRowContext(r.Context(), `select project_id from tasks where id=?`, predecessorID).Scan(&projectID); errors.Is(err, sql.ErrNoRows) || projectID != task.ProjectID {
			writeError(w, http.StatusBadRequest, errors.New("predecessor task must belong to the same project"))
			return
		} else if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		cycle, err := s.wouldCreateTaskCycleTx(r.Context(), tx, task.ID, predecessorID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if cycle {
			writeError(w, http.StatusBadRequest, errors.New("dependency would create a cycle"))
			return
		}
	}
	if _, err := tx.ExecContext(r.Context(), `delete from task_dependencies where task_id=?`, task.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	now := time.Now().UTC()
	for _, predecessorID := range ids {
		if _, err := tx.ExecContext(r.Context(), `insert into task_dependencies (task_id,predecessor_task_id,created_at) values (?,?,?)`, task.ID, predecessorID, now); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	if err := recordTaskEventTx(r.Context(), tx, task.ID, "", "task.dependencies_replaced", map[string]any{"predecessorTaskIds": ids}, now); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"predecessorTaskIds": ids})
}

func (s *Server) listTaskRuns(w http.ResponseWriter, r *http.Request) {
	if _, err := s.taskByID(r.Context(), chi.URLParam(r, "taskID")); errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("task not found"))
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	items, err := s.taskRuns(r.Context(), chi.URLParam(r, "taskID"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) listTaskEvents(w http.ResponseWriter, r *http.Request) {
	if _, err := s.taskByID(r.Context(), chi.URLParam(r, "taskID")); errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("task not found"))
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	items, err := s.taskEvents(r.Context(), chi.URLParam(r, "taskID"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) projectExists(ctx context.Context, projectID string) bool {
	var exists bool
	return s.db.QueryRowContext(ctx, `select exists(select 1 from projects where id=?)`, projectID).Scan(&exists) == nil && exists
}

func (s *Server) taskByID(ctx context.Context, taskID string) (Task, error) {
	task, err := scanTask(s.db.QueryRowContext(ctx, `select id,project_id,title,description,acceptance_criteria,priority,pinned,position,status,created_at,updated_at,completed_at,cancelled_at from tasks where id=?`, taskID))
	if err != nil {
		return Task{}, err
	}
	if err := s.hydrateTask(ctx, &task); err != nil {
		return Task{}, err
	}
	return task, nil
}

func scanTask(row interface{ Scan(...any) error }) (Task, error) {
	var task Task
	var completedAt, cancelledAt sql.NullTime
	if err := row.Scan(&task.ID, &task.ProjectID, &task.Title, &task.Description, &task.AcceptanceCriteria, &task.Priority, &task.Pinned, &task.Position, &task.Status, &task.CreatedAt, &task.UpdatedAt, &completedAt, &cancelledAt); err != nil {
		return Task{}, err
	}
	if completedAt.Valid {
		task.CompletedAt = &completedAt.Time
	}
	if cancelledAt.Valid {
		task.CancelledAt = &cancelledAt.Time
	}
	return task, nil
}

func (s *Server) hydrateTask(ctx context.Context, task *Task) error {
	task.DependsOn, task.BlockedBy, task.Blocks = []TaskDependency{}, []TaskBlocker{}, []TaskDependency{}
	predecessors, err := s.db.QueryContext(ctx, `select predecessor.id,predecessor.title,predecessor.status from task_dependencies dependency join tasks predecessor on predecessor.id=dependency.predecessor_task_id where dependency.task_id=? order by dependency.created_at`, task.ID)
	if err != nil {
		return err
	}
	for predecessors.Next() {
		var item TaskDependency
		if err := predecessors.Scan(&item.TaskID, &item.Title, &item.Status); err != nil {
			predecessors.Close()
			return err
		}
		task.DependsOn = append(task.DependsOn, item)
		if item.Status != taskDone {
			task.BlockedBy = append(task.BlockedBy, TaskBlocker(item))
		}
	}
	if err := predecessors.Close(); err != nil {
		return err
	}
	if err := predecessors.Err(); err != nil {
		return err
	}
	dependents, err := s.db.QueryContext(ctx, `select dependent.id,dependent.title,dependent.status from task_dependencies dependency join tasks dependent on dependent.id=dependency.task_id where dependency.predecessor_task_id=? order by dependency.created_at`, task.ID)
	if err != nil {
		return err
	}
	defer dependents.Close()
	for dependents.Next() {
		var item TaskDependency
		if err := dependents.Scan(&item.TaskID, &item.Title, &item.Status); err != nil {
			return err
		}
		task.Blocks = append(task.Blocks, item)
	}
	if err := dependents.Err(); err != nil {
		return err
	}
	lastRun, err := s.latestTaskRun(ctx, task.ID)
	if err == nil {
		task.LastRun = &lastRun
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var orchestrationUpdatedAt time.Time
	err = s.db.QueryRowContext(ctx, `select status,updated_at from task_orchestration_jobs where task_id=? and status in ('queued','preparing','implementing','checking','paused','needs_human','stopped','removing','awaiting_main','integrated_to_dev') order by updated_at desc limit 1`, task.ID).Scan(&task.OrchestrationStatus, &orchestrationUpdatedAt)
	if err == nil {
		task.OrchestrationUpdatedAt = &orchestrationUpdatedAt
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return nil
}

func (s *Server) verificationRunsForTask(ctx context.Context, taskID string) ([]VerificationRun, error) {
	rows, err := s.db.QueryContext(ctx, `select verification.id,verification.phase,verification.command,verification.reviewed_sha,verification.status,verification.exit_code,verification.output,verification.created_at,verification.completed_at
		from verification_runs verification join task_orchestration_jobs job on job.id=verification.job_id
		where job.task_id=? order by verification.created_at desc`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []VerificationRun{}
	for rows.Next() {
		var item VerificationRun
		var completedAt sql.NullTime
		if err := rows.Scan(&item.ID, &item.Phase, &item.Command, &item.ReviewedSHA, &item.Status, &item.ExitCode, &item.Output, &item.CreatedAt, &completedAt); err != nil {
			return nil, err
		}
		if completedAt.Valid {
			item.CompletedAt = &completedAt.Time
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Server) latestTaskRun(ctx context.Context, taskID string) (TaskRun, error) {
	return scanTaskRun(s.db.QueryRowContext(ctx, `select id,task_id,conversation_id,coalesce(run_id,''),sequence,status,prompt_snapshot,acceptance_snapshot,failure_reason,created_at,started_at,finished_at from task_runs where task_id=? order by sequence desc limit 1`, taskID))
}

func (s *Server) wouldCreateTaskCycle(ctx context.Context, taskID, predecessorTaskID string) (bool, error) {
	var cycle bool
	err := s.db.QueryRowContext(ctx, `with recursive ancestors(id) as (
		select predecessor_task_id from task_dependencies where task_id=?
		union
		select dependency.predecessor_task_id from task_dependencies dependency join ancestors on dependency.task_id=ancestors.id
	) select exists(select 1 from ancestors where id=?)`, predecessorTaskID, taskID).Scan(&cycle)
	return cycle, err
}

func (s *Server) wouldCreateTaskCycleTx(ctx context.Context, tx *sql.Tx, taskID, predecessorTaskID string) (bool, error) {
	var cycle bool
	err := tx.QueryRowContext(ctx, `with recursive ancestors(id) as (
		select predecessor_task_id from task_dependencies where task_id=?
		union
		select dependency.predecessor_task_id from task_dependencies dependency join ancestors on dependency.task_id=ancestors.id
	) select exists(select 1 from ancestors where id=?)`, predecessorTaskID, taskID).Scan(&cycle)
	return cycle, err
}

func (s *Server) taskRuns(ctx context.Context, taskID string) ([]TaskRun, error) {
	rows, err := s.db.QueryContext(ctx, `select id,task_id,conversation_id,coalesce(run_id,''),sequence,status,prompt_snapshot,acceptance_snapshot,failure_reason,created_at,started_at,finished_at from task_runs where task_id=? order by sequence desc`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []TaskRun{}
	for rows.Next() {
		item, err := scanTaskRun(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func scanTaskRun(row interface{ Scan(...any) error }) (TaskRun, error) {
	var item TaskRun
	var startedAt, finishedAt sql.NullTime
	if err := row.Scan(&item.ID, &item.TaskID, &item.ConversationID, &item.RunID, &item.Sequence, &item.Status, &item.PromptSnapshot, &item.AcceptanceSnapshot, &item.FailureReason, &item.CreatedAt, &startedAt, &finishedAt); err != nil {
		return TaskRun{}, err
	}
	if startedAt.Valid {
		item.StartedAt = &startedAt.Time
	}
	if finishedAt.Valid {
		item.FinishedAt = &finishedAt.Time
	}
	return item, nil
}

func (s *Server) taskEvents(ctx context.Context, taskID string) ([]TaskEvent, error) {
	rows, err := s.db.QueryContext(ctx, `select id,task_id,coalesce(task_run_id,''),type,payload,created_at from task_events where task_id=? order by created_at desc`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []TaskEvent{}
	for rows.Next() {
		var item TaskEvent
		if err := rows.Scan(&item.ID, &item.TaskID, &item.TaskRunID, &item.Type, &item.Payload, &item.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func recordTaskEventTx(ctx context.Context, tx *sql.Tx, taskID, taskRunID, typ string, payload any, now time.Time) error {
	_, err := tx.ExecContext(ctx, `insert into task_events (id,task_id,task_run_id,type,payload,created_at) values (?,?,?,?,?,?)`, uuid.NewString(), taskID, nullTaskRunID(taskRunID), typ, mustJSON(payload), now)
	return err
}

func nullTaskRunID(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func taskPrompt(task Task) string {
	var parts []string
	if task.Title != "" {
		parts = append(parts, task.Title)
	}
	if task.Description != "" {
		parts = append(parts, task.Description)
	}
	if task.AcceptanceCriteria != "" {
		parts = append(parts, task.AcceptanceCriteria)
	}
	return strings.Join(parts, "\n\n")
}

func (s *Server) taskDispatchEligibility(ctx context.Context, task Task, orchestrated bool) (bool, string, error) {
	if task.Status != taskTodo && task.Status != taskActionRequired {
		return false, "任务当前状态不可下发", nil
	}
	blocked := len(task.BlockedBy) > 0
	if blocked {
		if orchestrated {
			ready, err := s.orchestrationDependenciesIntegrated(ctx, task.ID)
			if err != nil {
				return false, "", err
			}
			if ready {
				blocked = false
			}
		}
	}
	if blocked {
		titles := make([]string, 0, len(task.BlockedBy))
		for _, blocker := range task.BlockedBy {
			titles = append(titles, blocker.Title)
		}
		return false, "等待前置任务完成：" + strings.Join(titles, "、"), nil
	}
	var active bool
	if err := s.db.QueryRowContext(ctx, `select exists(select 1 from task_runs where task_id=? and status in ('queued','running'))`, task.ID).Scan(&active); err != nil {
		return false, "", err
	}
	if active {
		return false, "任务已有活动执行", nil
	}
	return true, "", nil
}

func (s *Server) dispatchTask(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ConversationID string `json:"conversationId"`
	}
	if r.Body != http.NoBody && r.ContentLength != 0 && !decode(w, r, &input) {
		return
	}
	result, status, err := s.dispatchTaskByIDForConversation(r.Context(), chi.URLParam(r, "taskID"), input.ConversationID)
	if err != nil {
		writeError(w, status, err)
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

type taskDispatchResult struct {
	Message Message `json:"message"`
	RunID   string  `json:"runId"`
	TaskRun TaskRun `json:"taskRun"`
}

// dispatchTaskByID keeps task dispatch on the same path as direct messages so
// it creates the Message, Run, TaskRun, workspace lease, and agent process.
func (s *Server) dispatchTaskByID(ctx context.Context, taskID string) (taskDispatchResult, int, error) {
	return s.dispatchTaskByIDInWorkspace(ctx, taskID, "")
}

func (s *Server) dispatchTaskByIDForConversation(ctx context.Context, taskID, conversationID string) (taskDispatchResult, int, error) {
	return s.dispatchTaskByIDInWorkspaceWithExecutionIntentForConversation(ctx, taskID, "", "", "", conversationID)
}

func (s *Server) dispatchTaskByIDInWorkspace(ctx context.Context, taskID, worktreePath string) (taskDispatchResult, int, error) {
	return s.dispatchTaskByIDInWorkspaceWithContext(ctx, taskID, worktreePath, "")
}

func (s *Server) dispatchTaskByIDInWorkspaceWithContext(ctx context.Context, taskID, worktreePath, repairContext string) (taskDispatchResult, int, error) {
	return s.dispatchTaskByIDInWorkspaceWithExecutionIntent(ctx, taskID, worktreePath, repairContext, "")
}

func (s *Server) dispatchTaskByIDInWorkspaceWithExecutionIntent(ctx context.Context, taskID, worktreePath, repairContext, executionIntentID string) (taskDispatchResult, int, error) {
	return s.dispatchTaskByIDInWorkspaceWithExecutionIntentForConversation(ctx, taskID, worktreePath, repairContext, executionIntentID, "")
}

func (s *Server) dispatchTaskByIDInWorkspaceWithExecutionIntentForConversation(ctx context.Context, taskID, worktreePath, repairContext, executionIntentID, expectedConversationID string) (taskDispatchResult, int, error) {
	task, err := s.taskByID(ctx, taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return taskDispatchResult{}, http.StatusNotFound, errors.New("task not found")
	}
	if err != nil {
		return taskDispatchResult{}, http.StatusInternalServerError, err
	}
	orchestrated := worktreePath != ""
	allowed, reason, err := s.taskDispatchEligibility(ctx, task, orchestrated)
	if err != nil {
		return taskDispatchResult{}, http.StatusInternalServerError, err
	}
	if !allowed {
		return taskDispatchResult{}, http.StatusConflict, errors.New(reason)
	}
	conversationID := expectedConversationID
	if conversationID == "" {
		err = s.db.QueryRowContext(ctx, `select id from conversations where project_id=? and is_current=true`, task.ProjectID).Scan(&conversationID)
	} else if orchestrated {
		// Orchestrated dispatch targets a dedicated background conversation that
		// is intentionally not the project's current one; accept it by ID alone.
		err = s.db.QueryRowContext(ctx, `select id from conversations where id=? and project_id=?`, conversationID, task.ProjectID).Scan(&conversationID)
	} else {
		err = s.db.QueryRowContext(ctx, `select id from conversations where id=? and project_id=? and is_current=true`, conversationID, task.ProjectID).Scan(&conversationID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return taskDispatchResult{}, http.StatusConflict, errors.New("conversation is no longer current")
	}
	if err != nil {
		return taskDispatchResult{}, http.StatusInternalServerError, err
	}
	now := time.Now().UTC()
	prompt := taskPrompt(task)
	if strings.TrimSpace(repairContext) != "" {
		prompt += "\n\n上轮检查或审查发现的问题：\n" + strings.TrimSpace(repairContext) + "\n\n请只修复这些问题，并重新运行相关检查。"
	}
	taskRun := &TaskRun{ID: uuid.NewString(), TaskID: task.ID, ConversationID: conversationID, PromptSnapshot: prompt, AcceptanceSnapshot: task.AcceptanceCriteria, CreatedAt: now, ExecutionIntentID: executionIntentID}
	message, runID, record, status, err := s.startMessage(ctx, conversationID, taskRun.PromptSnapshot, &runStartRecord{Task: taskRun, WorktreePath: worktreePath, Orchestrated: orchestrated})
	if err != nil {
		return taskDispatchResult{}, status, err
	}
	if record == nil || record.Task == nil {
		return taskDispatchResult{}, http.StatusInternalServerError, errors.New("task dispatch did not create a task run")
	}
	return taskDispatchResult{Message: message, RunID: runID, TaskRun: *record.Task}, http.StatusAccepted, nil
}

func (s *Server) reviewTask(w http.ResponseWriter, r *http.Request) {
	task, err := s.taskByID(r.Context(), chi.URLParam(r, "taskID"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("task not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var input struct {
		Action string `json:"action"`
		Note   string `json:"note"`
	}
	if !decode(w, r, &input) {
		return
	}
	if task.Status != taskAwaitingReview {
		writeError(w, http.StatusConflict, errors.New("task is not awaiting review"))
		return
	}
	if s.hasActiveOrchestrationJob(r.Context(), task.ID) {
		writeError(w, http.StatusConflict, errors.New("automatic orchestration is still verifying this task"))
		return
	}
	if s.isOrchestrationBranchAwaitingMain(r.Context(), task.ID) {
		writeError(w, http.StatusConflict, errors.New("automatic orchestration branch must be merged into main before this task can be completed"))
		return
	}
	if input.Action != "accept" && input.Action != "request_changes" {
		writeError(w, http.StatusBadRequest, errors.New("review action must accept or request_changes"))
		return
	}
	input.Note = strings.TrimSpace(input.Note)
	if input.Action == "request_changes" && input.Note == "" {
		writeError(w, http.StatusBadRequest, errors.New("a change request needs a reason"))
		return
	}
	now := time.Now().UTC()
	nextStatus, eventType := taskDone, "task.accepted"
	if input.Action == "request_changes" {
		nextStatus, eventType = taskActionRequired, "task.changes_requested"
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(r.Context(), `update tasks set status=?,updated_at=?,completed_at=case when ?='done' then ? else null end where id=? and status=?`, nextStatus, now, nextStatus, now, task.ID, taskAwaitingReview)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	changed, err := result.RowsAffected()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if changed != 1 {
		writeError(w, http.StatusConflict, errors.New("task status changed before review was recorded"))
		return
	}
	if err := recordTaskEventTx(r.Context(), tx, task.ID, "", eventType, map[string]string{"note": input.Note}, now); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// 人工确认验收后无需再次通知；要求修改仍需提醒后续处理。
	if nextStatus == taskActionRequired {
		s.notifyTaskStatusChange(r.Context(), task.ID, nextStatus)
	}
	task.Status, task.UpdatedAt = nextStatus, now
	if nextStatus == taskDone {
		task.CompletedAt = &now
	} else {
		task.CompletedAt = nil
	}
	writeJSON(w, http.StatusOK, task)
}

// reviewAllTasks accepts every task in a project that is currently awaiting
// review. Tasks that a live automatic-orchestration job is still verifying are
// skipped so an in-flight review is not overwritten. The response reports how
// many tasks were accepted and how many were skipped.
func (s *Server) reviewAllTasks(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	if !s.projectExists(r.Context(), projectID) {
		writeError(w, http.StatusNotFound, errors.New("project not found"))
		return
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	// 统计项目当前待验收任务总数。
	var total int
	if err := tx.QueryRowContext(r.Context(), `select count(*) from tasks where project_id=? and status=?`, projectID, taskAwaitingReview).Scan(&total); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if total == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"accepted": 0, "skipped": 0, "total": 0})
		return
	}
	// 选出本次可实际验收的任务：仍在自动编排验证（queued/preparing/implementing/checking）中的
	// 一律排除。该筛选与下方 UPDATE 处于同一事务、同一 Snapshot 连接（SQLite 写事务持有数据库锁），
	// 从而把“检查编排状态”与“写入验收”放进同一个原子窗口，消除二者间的 TOCTOU 间隙。
	// 全程只使用事务连接 tx，避免在 SQLite 单连接下对 s.db 再发查询导致死锁。
	rows, err := tx.QueryContext(r.Context(), `select id from tasks where project_id=? and status=? and not exists(select 1 from task_orchestration_jobs oc where oc.task_id=tasks.id and oc.status in ('queued','preparing','implementing','checking','awaiting_main','integrated_to_dev'))`, projectID, taskAwaitingReview)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var accepted []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		accepted = append(accepted, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := rows.Close(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	skipped := total - len(accepted)
	if len(accepted) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"accepted": 0, "skipped": skipped, "total": total})
		return
	}
	placeholders := strings.Repeat("?,", len(accepted))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, 0, len(accepted)+3)
	args = append(args, taskDone, now, now)
	for _, id := range accepted {
		args = append(args, id)
	}
	query := `update tasks set status=?,updated_at=?,completed_at=? where id in (` + placeholders + `) and status=?`
	args = append(args, taskAwaitingReview)
	result, err := tx.ExecContext(r.Context(), query, args...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// 事务内同一锁窗口下仍可能让状态被并发改动漏过 where status=?；
	// 一旦实际修改行数与预期不符，整批中止（下面的 defer rollback），避免写入落空事件。
	changed, err := result.RowsAffected()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if int(changed) != len(accepted) {
		writeError(w, http.StatusConflict, errors.New("task status changed before review was recorded"))
		return
	}
	for _, id := range accepted {
		if err := recordTaskEventTx(r.Context(), tx, id, "", "task.accepted", map[string]string{"note": ""}, now); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// 人工确认验收后无需再次通知，与单任务验收行为一致。
	writeJSON(w, http.StatusOK, map[string]any{"accepted": len(accepted), "skipped": skipped, "total": total})
}

func (s *Server) reopenTask(w http.ResponseWriter, r *http.Request) {
	s.transitionTaskState(w, r, "task.reopened", taskDone, taskTodo)
}

func (s *Server) transitionTaskState(w http.ResponseWriter, r *http.Request, eventType, allowedStatus, nextStatus string) {
	task, err := s.taskByID(r.Context(), chi.URLParam(r, "taskID"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("task not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if task.Status != allowedStatus {
		writeError(w, http.StatusConflict, errors.New("task cannot transition from its current status"))
		return
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(r.Context(), `update tasks set status=?,updated_at=?,completed_at=null,cancelled_at=null where id=? and status=?`, nextStatus, now, task.ID, allowedStatus)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	changed, err := result.RowsAffected()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if changed != 1 {
		writeError(w, http.StatusConflict, errors.New("task status changed before transition was recorded"))
		return
	}
	if err := recordTaskEventTx(r.Context(), tx, task.ID, "", eventType, map[string]string{"status": nextStatus}, now); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	task.Status, task.UpdatedAt, task.CompletedAt, task.CancelledAt = nextStatus, now, nil, nil
	writeJSON(w, http.StatusOK, task)
}

func (s *Server) stopTask(w http.ResponseWriter, r *http.Request) {
	task, err := s.taskByID(r.Context(), chi.URLParam(r, "taskID"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("task not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var runID, taskRunStatus, conversationID string
	err = s.db.QueryRowContext(r.Context(), `select run_id,status,conversation_id from task_runs where task_id=? and status in ('queued','running') order by created_at desc limit 1`, task.ID).Scan(&runID, &taskRunStatus, &conversationID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusConflict, errors.New("task has no active execution"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if taskRunStatus == "queued" {
		writeError(w, http.StatusConflict, errors.New("queued task cannot be stopped independently"))
		return
	}
	var otherActive bool
	if err := s.db.QueryRowContext(r.Context(), `select exists(select 1 from task_runs where conversation_id=? and run_id<>? and status in ('queued','running'))`, conversationID, runID).Scan(&otherActive); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if otherActive {
		writeError(w, http.StatusConflict, errors.New("cannot stop a task while this conversation has other queued task runs"))
		return
	}
	status, code, err := s.stopRunByID(r.Context(), runID, false)
	if err != nil {
		writeError(w, code, err)
		return
	}
	writeJSON(w, code, map[string]string{"status": status})
}

func (s *Server) validateTaskDispatchTx(ctx context.Context, tx *sql.Tx, taskRun TaskRun, conversation Conversation, orchestrated bool) error {
	var projectID, status string
	if err := tx.QueryRowContext(ctx, `select project_id,status from tasks where id=?`, taskRun.TaskID).Scan(&projectID, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("task not found")
		}
		return err
	}
	if projectID != conversation.ProjectID || taskRun.ConversationID != conversation.ID {
		return errors.New("task and conversation must belong to the same project")
	}
	if status != taskTodo && status != taskActionRequired {
		return errors.New("task current status cannot be dispatched")
	}
	var blockers int
	dependencyQuery := `select count(*) from task_dependencies dependency join tasks predecessor on predecessor.id=dependency.predecessor_task_id where dependency.task_id=? and predecessor.status<>?`
	dependencyArgs := []any{taskRun.TaskID, taskDone}
	if orchestrated {
		dependencyQuery = `select count(*) from task_dependencies dependency
			left join task_orchestration_jobs predecessor on predecessor.task_id=dependency.predecessor_task_id and predecessor.status='released_to_main'
			where dependency.task_id=? and predecessor.id is null`
		dependencyArgs = []any{taskRun.TaskID}
	}
	if err := tx.QueryRowContext(ctx, dependencyQuery, dependencyArgs...).Scan(&blockers); err != nil {
		return err
	}
	if blockers > 0 {
		return errors.New("task has unfinished predecessor tasks")
	}
	var active bool
	if err := tx.QueryRowContext(ctx, `select exists(select 1 from task_runs where task_id=? and status in ('queued','running'))`, taskRun.TaskID).Scan(&active); err != nil {
		return err
	}
	if active {
		return errors.New("task already has an active execution")
	}
	return nil
}

func (s *Server) finishTaskRunTx(ctx context.Context, tx *sql.Tx, runID, runStatus, failureReason string, now time.Time) error {
	var taskRunID, taskID, taskRunStatus string
	err := tx.QueryRowContext(ctx, `select id,task_id,status from task_runs where run_id=?`, runID).Scan(&taskRunID, &taskID, &taskRunStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if taskRunStatus != "queued" && taskRunStatus != "running" {
		return nil
	}
	taskRunTerminalStatus, taskStatus := "", ""
	switch runStatus {
	case "completed":
		taskRunTerminalStatus, taskStatus = "succeeded", taskAwaitingReview
	case "failed", "stopped", "interrupted":
		taskRunTerminalStatus, taskStatus = runStatus, taskActionRequired
	default:
		return fmt.Errorf("unsupported task run terminal status: %s", runStatus)
	}
	if failureReason == "" && runStatus != "completed" {
		failureReason = runStatus
	}
	result, err := tx.ExecContext(ctx, `update task_runs set status=?,finished_at=?,failure_reason=? where id=? and status in ('queued','running')`, taskRunTerminalStatus, now, failureReason, taskRunID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return fmt.Errorf("task run %s: expected queued/running but status was already terminal", taskRunID)
	}
	taskResult, err := tx.ExecContext(ctx, `update tasks set status=?,updated_at=? where id=? and status=?`, taskStatus, now, taskID, taskRunning)
	if err != nil {
		return err
	}
	taskChanged, err := taskResult.RowsAffected()
	if err != nil {
		return err
	}
	if taskChanged == 0 {
		return fmt.Errorf("task %s: expected running but status had already changed", taskID)
	}
	return recordTaskEventTx(ctx, tx, taskID, taskRunID, "task.run_"+taskRunTerminalStatus, map[string]string{"runId": runID, "status": taskRunTerminalStatus}, now)
}
