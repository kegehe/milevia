package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const (
	orchestrationQueued        = "queued"
	orchestrationPreparing     = "preparing"
	orchestrationImplementing  = "implementing"
	orchestrationChecking      = "checking"
	orchestrationNeedsHuman    = "needs_human"
	orchestrationPaused        = "paused"
	orchestrationLeaseDuration = 30 * time.Second
	orchestrationLeaseRenewal  = 10 * time.Second
)

var errOrchestrationWaiting = errors.New("orchestration queue head is waiting")

type OrchestrationConfig struct {
	ProjectID            string   `json:"projectId"`
	Enabled              bool     `json:"enabled"`
	MainBranch           string   `json:"mainBranch"`
	DevBranch            string   `json:"devBranch"`
	VerificationCommands []string `json:"verificationCommands"`
	MaxFixRounds         int      `json:"maxFixRounds"`
	FrozenReason         string   `json:"frozenReason,omitempty"`
}

type OrchestrationJob struct {
	ID             string    `json:"id"`
	ProjectID      string    `json:"projectId"`
	TaskID         string    `json:"taskId"`
	Position       int       `json:"position"`
	Status         string    `json:"status"`
	Attempt        int       `json:"attempt"`
	LeaseToken     int64     `json:"leaseToken"`
	BaseDevSHA     string    `json:"baseDevSha,omitempty"`
	LastError      string    `json:"lastError,omitempty"`
	PolicySnapshot string    `json:"-"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

type GitTaskRecord struct {
	JobID          string `json:"jobId"`
	BaseDevSHA     string `json:"baseDevSha"`
	TaskBranch     string `json:"taskBranch"`
	WorktreePath   string `json:"worktreePath"`
	TaskCommitSHA  string `json:"taskCommitSha,omitempty"`
	IntegrationSHA string `json:"integrationSha,omitempty"`
	ConversationID string `json:"conversationId,omitempty"`
}

func (s *Server) migrateOrchestration(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `create table if not exists project_orchestration_configs (
	project_id text primary key references projects(id) on delete cascade,
	enabled integer not null default 0,
	main_branch text not null default 'main',
	dev_branch text not null default 'dev',
	verification_commands text not null default '[]',
	max_fix_rounds integer not null default 3,
	frozen_reason text not null default '',
	updated_at datetime not null
);
create table if not exists task_orchestration_jobs (
	id text primary key,
	project_id text not null references projects(id) on delete cascade,
	task_id text not null unique references tasks(id) on delete cascade,
	queue_position integer not null,
	status text not null,
	attempt integer not null default 0,
	lease_token integer not null default 0,
	base_dev_sha text not null default '',
	last_error text not null default '',
	policy_snapshot text not null default '{}',
	created_at datetime not null,
	updated_at datetime not null
);
create table if not exists project_orchestration_leases (
	project_id text primary key references projects(id) on delete cascade,
	token integer not null default 0,
	owner_id text not null default '',
	expires_at datetime,
	updated_at datetime not null
);
create table if not exists task_execution_intents (
	id text primary key,
	job_id text not null references task_orchestration_jobs(id) on delete cascade,
	phase text not null,
	attempt integer not null,
	run_id text not null default '',
	status text not null,
	created_at datetime not null,
	updated_at datetime not null,
	unique(job_id,phase,attempt)
);
create table if not exists git_task_records (
	job_id text primary key references task_orchestration_jobs(id) on delete cascade,
	base_dev_sha text not null,
	task_branch text not null,
	worktree_path text not null,
	task_commit_sha text not null default '',
	integration_sha text not null default '',
	release_branch text not null default '',
	created_at datetime not null,
	updated_at datetime not null
);
create table if not exists verification_runs (
	id text primary key,
	job_id text not null references task_orchestration_jobs(id) on delete cascade,
	phase text not null,
	command text not null,
	reviewed_sha text not null default '',
	status text not null,
	exit_code integer not null default 0,
	output text not null default '',
	created_at datetime not null,
	completed_at datetime
);
create table if not exists orchestration_outbox (
	id text primary key,
	project_id text not null references projects(id) on delete cascade,
	job_id text references task_orchestration_jobs(id) on delete cascade,
	type text not null,
	idempotency_key text not null unique,
	status text not null,
	created_at datetime not null,
	completed_at datetime
);
create table if not exists release_snapshots (
	id text primary key,
	project_id text not null references projects(id) on delete cascade,
	dev_sha text not null,
	branch text not null,
	status text not null,
	created_at datetime not null,
	confirmed_at datetime,
	unique(project_id,branch)
);
create index if not exists task_orchestration_jobs_project_queue on task_orchestration_jobs(project_id,status,queue_position);
create index if not exists task_execution_intents_job on task_execution_intents(job_id,phase,attempt);
create index if not exists verification_runs_job on verification_runs(job_id,created_at);`)
	if err != nil {
		return fmt.Errorf("migrate orchestration: %w", err)
	}
	if err := ensureColumn(ctx, s.db, "task_orchestration_jobs", "policy_snapshot", "text not null default '{}'"); err != nil {
		return fmt.Errorf("add orchestration policy snapshot: %w", err)
	}
	if err := ensureColumn(ctx, s.db, "git_task_records", "conversation_id", "text not null default ''"); err != nil {
		return fmt.Errorf("add git_task_records conversation_id: %w", err)
	}
	if err := s.migrateReleaseSnapshotBranchScope(ctx); err != nil {
		return fmt.Errorf("migrate release snapshot branch scope: %w", err)
	}
	return nil
}

// Release branches live in separate repositories, so their names must only be
// unique within a project. Older databases made this a global constraint.
func (s *Server) migrateReleaseSnapshotBranchScope(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `pragma index_list(release_snapshots)`)
	if err != nil {
		return err
	}
	indexes := []string{}
	needsMigration := false
	for rows.Next() {
		var sequence int
		var name string
		var unique int
		var origin string
		var partial int
		if err := rows.Scan(&sequence, &name, &unique, &origin, &partial); err != nil {
			return err
		}
		if unique == 0 {
			continue
		}
		indexes = append(indexes, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, name := range indexes {
		columns, err := s.db.QueryContext(ctx, fmt.Sprintf(`pragma index_info(%q)`, name))
		if err != nil {
			return err
		}
		var count int
		var column string
		for columns.Next() {
			var position, cid int
			if err := columns.Scan(&position, &cid, &column); err != nil {
				columns.Close()
				return err
			}
			count++
		}
		if err := columns.Err(); err != nil {
			columns.Close()
			return err
		}
		columns.Close()
		if count == 1 && column == "branch" {
			needsMigration = true
			break
		}
	}
	if !needsMigration {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `alter table release_snapshots rename to release_snapshots_legacy;
create table release_snapshots (
	id text primary key,
	project_id text not null references projects(id) on delete cascade,
	dev_sha text not null,
	branch text not null,
	status text not null,
	created_at datetime not null,
	confirmed_at datetime,
	unique(project_id,branch)
);
insert into release_snapshots (id,project_id,dev_sha,branch,status,created_at,confirmed_at)
select id,project_id,dev_sha,branch,status,created_at,confirmed_at from release_snapshots_legacy;
drop table release_snapshots_legacy;`)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func defaultOrchestrationConfig(projectID string) OrchestrationConfig {
	return OrchestrationConfig{ProjectID: projectID, MainBranch: "main", DevBranch: "dev", VerificationCommands: []string{}, MaxFixRounds: 3}
}

// createOrchestrationConversation creates a dedicated background conversation
// (is_current=0) for an orchestrated task so the entire execution — prompt,
// implementation Run, verification commands, and independent review — is
// visible in one place without disturbing the user's current conversation. It
// inherits the agent, runner and profile from the project's current
// conversation so the task runs in the same environment the user works in.
func (s *Server) createOrchestrationConversation(ctx context.Context, project Project, task Task) (string, error) {
	var agentID, permissionMode, runnerID, profileRevisionID string
	err := s.db.QueryRowContext(ctx, `select agent_id,permission_mode,agent_runtime_id,coalesce(agent_profile_revision_id,'') from conversations where project_id=? and is_current=true`, project.ID).Scan(&agentID, &permissionMode, &runnerID, &profileRevisionID)
	if errors.Is(err, sql.ErrNoRows) {
		// No current conversation — fall back to the project's runner and a
		// default Claude Code policy.
		agentID = "claude-code"
		permissionMode = "approval_required"
		runnerID = project.Runner
		profileRevisionID = ""
	} else if err != nil {
		return "", err
	}
	if runnerID == "" {
		runnerID = project.Runner
	}
	now := time.Now().UTC()
	sessionID := uuid.NewString()
	conversationID := uuid.NewString()
	title := "编排：" + task.Title
	if len(title) > 80 {
		title = title[:80]
	}
	_, err = s.db.ExecContext(ctx, `insert into conversations (id,project_id,claude_session_id,agent_id,agent_session_id,agent_runtime_id,agent_profile_revision_id,execution_policy,status,permission_mode,title,last_activity_at,claude_initialized,agent_initialized,is_current,created_at) values (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		conversationID, project.ID, sessionID, agentID, sessionID, runnerID, profileRevisionID, permissionMode, "idle", permissionMode, title, now, false, false, 0, now)
	if err != nil {
		return "", fmt.Errorf("create orchestration conversation: %w", err)
	}
	return conversationID, nil
}

// orchestrationConversationID loads the conversation ID recorded for a job from
// git_task_records. It returns "" when no conversation has been associated yet.
func (s *Server) orchestrationConversationID(ctx context.Context, jobID string) (string, error) {
	var conversationID string
	err := s.db.QueryRowContext(ctx, `select conversation_id from git_task_records where job_id=?`, jobID).Scan(&conversationID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return conversationID, err
}

// postOrchestrationMessage writes an assistant message into the job's
// orchestration conversation and broadcasts it so the user sees it live in the
// conversation view. It is used to surface verification command output and
// independent review results that would otherwise be invisible.
func (s *Server) postOrchestrationMessage(ctx context.Context, job OrchestrationJob, content string) {
	conversationID, err := s.orchestrationConversationID(ctx, job.ID)
	if err != nil || conversationID == "" {
		return
	}
	now := time.Now().UTC()
	messageID := uuid.NewString()
	if _, err := s.db.ExecContext(ctx, `insert into messages (id,conversation_id,run_id,role,content,parent_tool_use_id,created_at) values (?,?,?,?,?,?,?)`, messageID, conversationID, "", "assistant", content, "", now); err != nil {
		log.Printf("post orchestration message for job %s: %v", job.ID, err)
		return
	}
	if _, err := s.db.ExecContext(ctx, `update conversations set last_activity_at=? where id=?`, now, conversationID); err != nil {
		log.Printf("update conversation activity for job %s: %v", job.ID, err)
	}
	m := Message{ID: messageID, ConversationID: conversationID, Role: "assistant", Content: content, CreatedAt: now}
	s.appendEvent("", conversationID, "assistant.message", mustJSON(m))
}

func (s *Server) orchestrationConfig(ctx context.Context, projectID string) (OrchestrationConfig, error) {
	cfg := defaultOrchestrationConfig(projectID)
	var commands string
	err := s.db.QueryRowContext(ctx, `select enabled,main_branch,dev_branch,verification_commands,max_fix_rounds,frozen_reason from project_orchestration_configs where project_id=?`, projectID).Scan(&cfg.Enabled, &cfg.MainBranch, &cfg.DevBranch, &commands, &cfg.MaxFixRounds, &cfg.FrozenReason)
	if errors.Is(err, sql.ErrNoRows) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal([]byte(commands), &cfg.VerificationCommands); err != nil {
		return cfg, fmt.Errorf("decode verification commands: %w", err)
	}
	return cfg, nil
}

func validateOrchestrationConfig(cfg OrchestrationConfig) error {
	if !validOrchestrationBranch(cfg.MainBranch) || !validOrchestrationBranch(cfg.DevBranch) || cfg.MainBranch == cfg.DevBranch {
		return errors.New("main/dev branch is invalid")
	}
	if cfg.MaxFixRounds < 1 || cfg.MaxFixRounds > 10 {
		return errors.New("maxFixRounds must be between 1 and 10")
	}
	if len(cfg.VerificationCommands) > 20 {
		return errors.New("at most 20 verification commands are allowed")
	}
	for _, command := range cfg.VerificationCommands {
		if strings.TrimSpace(command) == "" || len(command) > 2000 {
			return errors.New("verification command is invalid")
		}
	}
	return nil
}

var orchestrationBranchPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,120}$`)

func validOrchestrationBranch(branch string) bool {
	return orchestrationBranchPattern.MatchString(branch) && !strings.Contains(branch, "..") && !strings.HasSuffix(branch, "/")
}

func (s *Server) getOrchestrationConfig(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	if !s.projectExists(r.Context(), projectID) {
		writeError(w, http.StatusNotFound, errors.New("project not found"))
		return
	}
	cfg, err := s.orchestrationConfig(r.Context(), projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

func (s *Server) updateOrchestrationConfig(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	if !s.projectExists(r.Context(), projectID) {
		writeError(w, http.StatusNotFound, errors.New("project not found"))
		return
	}
	var cfg OrchestrationConfig
	if !decode(w, r, &cfg) {
		return
	}
	cfg.ProjectID = projectID
	if err := validateOrchestrationConfig(cfg); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	commands, _ := json.Marshal(cfg.VerificationCommands)
	_, err := s.db.ExecContext(r.Context(), `insert into project_orchestration_configs (project_id,enabled,main_branch,dev_branch,verification_commands,max_fix_rounds,frozen_reason,updated_at) values (?,?,?,?,?,?,coalesce((select frozen_reason from project_orchestration_configs where project_id=?),''),?) on conflict(project_id) do update set enabled=excluded.enabled,main_branch=excluded.main_branch,dev_branch=excluded.dev_branch,verification_commands=excluded.verification_commands,max_fix_rounds=excluded.max_fix_rounds,updated_at=excluded.updated_at`, projectID, cfg.Enabled, cfg.MainBranch, cfg.DevBranch, string(commands), cfg.MaxFixRounds, projectID, time.Now().UTC())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if cfg.Enabled {
		s.kickProjectOrchestrator(projectID)
	}
	writeJSON(w, http.StatusOK, cfg)
}

// enqueueTaskForOrchestration adds a single task to the project's automatic
// orchestration queue. The task must be in todo or action_required state and
// automatic orchestration must be enabled for the project.
func (s *Server) enqueueTaskForOrchestration(w http.ResponseWriter, r *http.Request) {
	task, err := s.taskByID(r.Context(), chi.URLParam(r, "taskID"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("task not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	cfg, err := s.orchestrationConfig(r.Context(), task.ProjectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	job, err := s.enqueueTask(r.Context(), task, cfg)
	if err != nil {
		writeError(w, errorStatusForEnqueue(err), err)
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

// enqueueTask performs the queue insertion shared by the single and batch
// enqueue endpoints. It does not write an HTTP response.
func (s *Server) enqueueTask(ctx context.Context, task Task, cfg OrchestrationConfig) (OrchestrationJob, error) {
	if task.Status != taskTodo && task.Status != taskActionRequired {
		return OrchestrationJob{}, errOrchestrationTaskIneligible
	}
	if !cfg.Enabled {
		return OrchestrationJob{}, errOrchestrationDisabled
	}
	now := time.Now().UTC()
	policySnapshot, err := json.Marshal(cfg)
	if err != nil {
		return OrchestrationJob{}, err
	}
	job := OrchestrationJob{ID: uuid.NewString(), ProjectID: task.ProjectID, TaskID: task.ID, Status: orchestrationQueued, PolicySnapshot: string(policySnapshot), CreatedAt: now, UpdatedAt: now}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return OrchestrationJob{}, err
	}
	defer tx.Rollback()
	if err = tx.QueryRowContext(ctx, `select coalesce(max(queue_position),0)+1 from task_orchestration_jobs where project_id=?`, task.ProjectID).Scan(&job.Position); err == nil {
		_, err = tx.ExecContext(ctx, `insert into task_orchestration_jobs (id,project_id,task_id,queue_position,status,policy_snapshot,created_at,updated_at) values (?,?,?,?,?,?,?,?)`, job.ID, job.ProjectID, job.TaskID, job.Position, job.Status, job.PolicySnapshot, job.CreatedAt, job.UpdatedAt)
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, `insert into orchestration_outbox (id,project_id,job_id,type,idempotency_key,status,created_at) values (?,?,?,?,?,?,?)`, uuid.NewString(), job.ProjectID, job.ID, "dispatch", "enqueue:"+job.ID, "pending", now)
	}
	if err == nil {
		err = recordTaskEventTx(ctx, tx, task.ID, "", "orchestration.queued", map[string]any{"jobId": job.ID, "position": job.Position}, now)
	}
	if err != nil {
		return OrchestrationJob{}, err
	}
	if err = tx.Commit(); err != nil {
		return OrchestrationJob{}, err
	}
	s.kickProjectOrchestrator(task.ProjectID)
	return job, nil
}

// errorStatusForEnqueue maps enqueue errors to the HTTP status code a handler
// should return. Business-rule violations are 409; anything else is 500.
func errorStatusForEnqueue(err error) int {
	if errors.Is(err, errOrchestrationDisabled) || errors.Is(err, errOrchestrationTaskIneligible) {
		return http.StatusConflict
	}
	if errors.Is(err, errOrchestrationTaskNotFound) {
		return http.StatusNotFound
	}
	if errors.Is(err, errOrchestrationTaskForeign) {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

var errOrchestrationDisabled = errors.New("automatic orchestration is not enabled for this project")
var errOrchestrationTaskIneligible = errors.New("only todo or action-required tasks can enter the automatic queue")
var errOrchestrationTaskNotFound = errors.New("task not found")
var errOrchestrationTaskForeign = errors.New("task does not belong to this project")

// enqueueBatchTasks adds multiple tasks to the project's orchestration queue in
// the order given. Tasks already in the queue are skipped so a partial failure
// does not double-enqueue. The returned slice mirrors the input order.
func (s *Server) enqueueBatchTasks(ctx context.Context, projectID string, taskIDs []string) ([]OrchestrationJob, error) {
	cfg, err := s.orchestrationConfig(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, errOrchestrationDisabled
	}
	jobs := make([]OrchestrationJob, 0, len(taskIDs))
	for _, taskID := range taskIDs {
		task, err := s.taskByID(ctx, taskID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s", errOrchestrationTaskNotFound, taskID)
		}
		if err != nil {
			return nil, err
		}
		if task.ProjectID != projectID {
			return nil, fmt.Errorf("%w: %s", errOrchestrationTaskForeign, taskID)
		}
		if s.hasOrchestrationJob(ctx, taskID) {
			continue // already queued, running, paused, or finished; skip idempotently
		}
		job, err := s.enqueueTask(ctx, task, cfg)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

func (s *Server) enqueueBatchForOrchestration(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	if !s.projectExists(r.Context(), projectID) {
		writeError(w, http.StatusNotFound, errors.New("project not found"))
		return
	}
	var payload struct {
		TaskIDs []string `json:"taskIds"`
	}
	if !decode(w, r, &payload) {
		return
	}
	if len(payload.TaskIDs) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("taskIds must not be empty"))
		return
	}
	if len(payload.TaskIDs) > 100 {
		writeError(w, http.StatusBadRequest, errors.New("at most 100 tasks can be enqueued at once"))
		return
	}
	seen := make(map[string]bool, len(payload.TaskIDs))
	for _, id := range payload.TaskIDs {
		if id == "" {
			writeError(w, http.StatusBadRequest, errors.New("taskIds contains an empty value"))
			return
		}
		if seen[id] {
			writeError(w, http.StatusBadRequest, errors.New("taskIds contains duplicates"))
			return
		}
		seen[id] = true
	}
	jobs, err := s.enqueueBatchTasks(r.Context(), projectID, payload.TaskIDs)
	if err != nil {
		writeError(w, errorStatusForEnqueue(err), err)
		return
	}
	writeJSON(w, http.StatusAccepted, jobs)
}

// dequeueTaskFromOrchestration removes a task from the automatic queue. Only
// tasks that are queued or paused can be removed — a task already in
// preparing/implementing/checking may hold a worktree and must not be pulled.
func (s *Server) dequeueTaskFromOrchestration(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	var jobID, projectID, status string
	err = tx.QueryRowContext(r.Context(), `select id,project_id,status from task_orchestration_jobs where task_id=? and status in ('queued','preparing','implementing','checking','paused','needs_human') order by updated_at desc limit 1`, taskID).Scan(&jobID, &projectID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("task is not in the orchestration queue"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if status != orchestrationQueued && status != orchestrationPaused {
		writeError(w, http.StatusConflict, fmt.Errorf("cannot remove a task that is %s", status))
		return
	}
	if _, err = tx.ExecContext(r.Context(), `delete from task_orchestration_jobs where id=?`, jobID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err = s.compactQueuePositionsTx(r.Context(), tx, projectID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err = recordTaskEventTx(r.Context(), tx, taskID, "", "orchestration.dequeued", map[string]any{"jobId": jobID}, time.Now().UTC()); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err = tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.kickProjectOrchestrator(projectID)
	w.WriteHeader(http.StatusNoContent)
}

// compactQueuePositionsTx renumbers every queue row for a project so that
// queue_position is contiguous starting at 1, preserving the existing order.
// It must run inside a transaction holding the rows it updates.
func (s *Server) compactQueuePositionsTx(ctx context.Context, tx *sql.Tx, projectID string) error {
	rows, err := tx.QueryContext(ctx, `select id from task_orchestration_jobs where project_id=? order by queue_position`, projectID)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for index, id := range ids {
		if _, err = tx.ExecContext(ctx, `update task_orchestration_jobs set queue_position=?,updated_at=? where id=?`, index+1, time.Now().UTC(), id); err != nil {
			return err
		}
	}
	return nil
}

// reorderOrchestrationJobs rewrites the queue order for the queued and paused
// tasks of a project. Tasks already executing (preparing/implementing/checking)
// keep their positions and are not part of the supplied list; the supplied
// tasks are renumbered to follow them.
func (s *Server) reorderOrchestrationJobs(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	if !s.projectExists(r.Context(), projectID) {
		writeError(w, http.StatusNotFound, errors.New("project not found"))
		return
	}
	var payload struct {
		TaskIDs []string `json:"taskIds"`
	}
	if !decode(w, r, &payload) {
		return
	}
	if len(payload.TaskIDs) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("taskIds must not be empty"))
		return
	}
	seen := make(map[string]bool, len(payload.TaskIDs))
	for _, id := range payload.TaskIDs {
		if id == "" {
			writeError(w, http.StatusBadRequest, errors.New("taskIds contains an empty value"))
			return
		}
		if seen[id] {
			writeError(w, http.StatusBadRequest, errors.New("taskIds contains duplicates"))
			return
		}
		seen[id] = true
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	// Fetch the tasks that are eligible for reordering (queued or paused) and
	// the highest position held by an in-flight job, in one pass.
	rows, err := tx.QueryContext(r.Context(), `select task_id from task_orchestration_jobs where project_id=? and status in ('queued','paused') order by queue_position`, projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	current := make(map[string]bool)
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		current[id] = true
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	rows.Close()
	for _, id := range payload.TaskIDs {
		if !current[id] {
			writeError(w, http.StatusBadRequest, fmt.Errorf("task %s is not queued or paused", id))
			return
		}
	}
	if len(payload.TaskIDs) != len(current) {
		writeError(w, http.StatusBadRequest, errors.New("taskIds must include every queued or paused task exactly once"))
		return
	}
	var basePosition int
	if err = tx.QueryRowContext(r.Context(), `select coalesce(max(queue_position),0) from task_orchestration_jobs where project_id=? and status not in ('queued','paused','integrated_to_dev','released_to_main')`, projectID).Scan(&basePosition); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	now := time.Now().UTC()
	for offset, taskID := range payload.TaskIDs {
		if _, err = tx.ExecContext(r.Context(), `update task_orchestration_jobs set queue_position=?,updated_at=? where project_id=? and task_id=? and status in ('queued','paused')`, basePosition+offset+1, now, projectID, taskID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	if err = tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.kickProjectOrchestrator(projectID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listOrchestrationJobs(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	rows, err := s.db.QueryContext(r.Context(), `select id,project_id,task_id,queue_position,status,attempt,lease_token,base_dev_sha,last_error,created_at,updated_at from task_orchestration_jobs where project_id=? order by queue_position`, projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	items := []OrchestrationJob{}
	for rows.Next() {
		var item OrchestrationJob
		if err := rows.Scan(&item.ID, &item.ProjectID, &item.TaskID, &item.Position, &item.Status, &item.Attempt, &item.LeaseToken, &item.BaseDevSHA, &item.LastError, &item.CreatedAt, &item.UpdatedAt); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) pauseOrchestrationJob(w http.ResponseWriter, r *http.Request) {
	s.setOrchestrationJobPause(w, r, orchestrationPaused)
}
func (s *Server) resumeOrchestrationJob(w http.ResponseWriter, r *http.Request) {
	s.setOrchestrationJobPause(w, r, orchestrationQueued)
}
func (s *Server) setOrchestrationJobPause(w http.ResponseWriter, r *http.Request, target string) {
	taskID := chi.URLParam(r, "taskID")
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	var projectID, previousStatus string
	err = tx.QueryRowContext(r.Context(), `select project_id,status from task_orchestration_jobs where task_id=?`, taskID).Scan(&projectID, &previousStatus)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("orchestration job not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	allowed := "status='queued'"
	if target == orchestrationQueued {
		allowed = "status in ('paused','needs_human')"
	}
	result, err := tx.ExecContext(r.Context(), `update task_orchestration_jobs set status=?,updated_at=? where task_id=? and `+allowed, target, time.Now().UTC(), taskID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		writeError(w, http.StatusConflict, errors.New("job cannot change state"))
		return
	}
	if target == orchestrationQueued {
		if previousStatus == orchestrationNeedsHuman {
			if _, err := tx.ExecContext(r.Context(), `update tasks set status=?,updated_at=? where id=? and status in ('todo','running','awaiting_review','action_required')`, taskActionRequired, time.Now().UTC(), taskID); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
		}
		var blocked int
		if err := tx.QueryRowContext(r.Context(), `select count(*) from task_orchestration_jobs where project_id=? and status='needs_human'`, projectID).Scan(&blocked); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if blocked == 0 {
			if _, err := tx.ExecContext(r.Context(), `update project_orchestration_configs set frozen_reason='',updated_at=? where project_id=?`, time.Now().UTC(), projectID); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
		}
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if target == orchestrationPaused {
		// 通知：编排任务已暂停
		s.notifyOrchestrationPaused(r.Context(), projectID, taskID)
	} else if target == orchestrationQueued && previousStatus == orchestrationNeedsHuman {
		// 通知：编排任务从 needs_human 恢复
		s.notifyOrchestrationResumed(r.Context(), projectID, taskID)
	}
	if target == orchestrationQueued {
		s.kickProjectOrchestrator(projectID)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) recoverOrchestration(ctx context.Context) {
	rows, err := s.db.QueryContext(ctx, `select distinct project_id from task_orchestration_jobs where status in ('queued','preparing','implementing','checking')`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var projectID string
		if rows.Scan(&projectID) == nil {
			s.kickProjectOrchestrator(projectID)
		}
	}
}

func (s *Server) kickProjectOrchestrator(projectID string) {
	if projectID == "" {
		return
	}
	s.orchestrationMu.Lock()
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	if closing {
		s.orchestrationMu.Unlock()
		return
	}
	if s.orchestrationActive[projectID] {
		s.orchestrationMu.Unlock()
		return
	}
	s.orchestrationActive[projectID] = true
	s.orchestrationWG.Add(1)
	s.orchestrationMu.Unlock()
	go func() {
		defer s.orchestrationWG.Done()
		defer func() { s.orchestrationMu.Lock(); delete(s.orchestrationActive, projectID); s.orchestrationMu.Unlock() }()
		s.processProjectOrchestration(s.runtimeCtx, projectID)
	}()
}

func (s *Server) processProjectOrchestration(ctx context.Context, projectID string) {
	currentConfig, err := s.orchestrationConfig(ctx, projectID)
	if err != nil || !currentConfig.Enabled || currentConfig.FrozenReason != "" {
		return
	}
	leaseToken, ok := s.acquireOrchestrationLease(ctx, projectID)
	if !ok {
		return
	}
	stopLeaseRenewal := s.renewOrchestrationLease(ctx, projectID, leaseToken)
	defer stopLeaseRenewal()
	job, err := s.nextOrchestrationJob(ctx, projectID)
	if err != nil || job == nil {
		return
	}
	if job.Status == orchestrationPaused || job.Status == orchestrationNeedsHuman {
		return
	}
	result, err := s.db.ExecContext(ctx, `update task_orchestration_jobs set lease_token=?,updated_at=? where id=? and status=?`, leaseToken, time.Now().UTC(), job.ID, job.Status)
	if err != nil {
		return
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return
	}
	job.LeaseToken = leaseToken
	var cfg OrchestrationConfig
	if err := json.Unmarshal([]byte(job.PolicySnapshot), &cfg); err != nil {
		s.failOrchestrationJob(ctx, *job, fmt.Errorf("decode policy snapshot: %w", err))
		return
	}
	if job.Status == orchestrationImplementing || job.Status == orchestrationChecking {
		s.completeOrchestrationImplementation(ctx, *job, cfg)
		return
	}
	if err := s.prepareAndDispatchOrchestrationJob(ctx, *job, cfg); err != nil && !errors.Is(err, errOrchestrationWaiting) {
		s.failOrchestrationJob(ctx, *job, err)
		return
	}
}

func (s *Server) nextOrchestrationJob(ctx context.Context, projectID string) (*OrchestrationJob, error) {
	var job OrchestrationJob
	err := s.db.QueryRowContext(ctx, `select id,project_id,task_id,queue_position,status,attempt,lease_token,base_dev_sha,last_error,policy_snapshot,created_at,updated_at from task_orchestration_jobs where project_id=? and status not in ('integrated_to_dev','released_to_main') order by queue_position limit 1`, projectID).Scan(&job.ID, &job.ProjectID, &job.TaskID, &job.Position, &job.Status, &job.Attempt, &job.LeaseToken, &job.BaseDevSHA, &job.LastError, &job.PolicySnapshot, &job.CreatedAt, &job.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &job, nil
}

func (s *Server) acquireOrchestrationLease(ctx context.Context, projectID string) (int64, bool) {
	now := time.Now().UTC()
	expires := now.Add(orchestrationLeaseDuration)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `insert into project_orchestration_leases (project_id,token,owner_id,expires_at,updated_at) values (?,0,'',?,?) on conflict(project_id) do nothing`, projectID, now, now)
	if err != nil {
		return 0, false
	}
	result, err := tx.ExecContext(ctx, `update project_orchestration_leases set token=token+1,owner_id=?,expires_at=?,updated_at=? where project_id=? and (owner_id='' or owner_id=? or expires_at is null or expires_at<?)`, s.orchestrationOwner, expires, now, projectID, s.orchestrationOwner, now)
	if err != nil {
		return 0, false
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return 0, false
	}
	var token int64
	if err = tx.QueryRowContext(ctx, `select token from project_orchestration_leases where project_id=? and owner_id=?`, projectID, s.orchestrationOwner).Scan(&token); err != nil {
		return 0, false
	}
	if err = tx.Commit(); err != nil {
		return 0, false
	}
	return token, true
}

func (s *Server) releaseOrchestrationLeases() {
	_, _ = s.db.ExecContext(context.Background(), `update project_orchestration_leases set owner_id='',expires_at=null,updated_at=? where owner_id=?`, time.Now().UTC(), s.orchestrationOwner)
}

func (s *Server) renewOrchestrationLease(ctx context.Context, projectID string, token int64) func() {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(orchestrationLeaseRenewal)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				now := time.Now().UTC()
				_, _ = s.db.ExecContext(context.Background(), `update project_orchestration_leases set expires_at=?,updated_at=? where project_id=? and token=? and owner_id=? and expires_at>?`, now.Add(orchestrationLeaseDuration), now, projectID, token, s.orchestrationOwner, now)
			}
		}
	}()
	return func() { close(done) }
}

func (s *Server) assertOrchestrationLease(ctx context.Context, job OrchestrationJob) error {
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `update project_orchestration_leases set expires_at=?,updated_at=? where token=? and owner_id=? and expires_at>? and project_id=(select project_id from task_orchestration_jobs where id=? and lease_token=?)`, now.Add(orchestrationLeaseDuration), now, job.LeaseToken, s.orchestrationOwner, now, job.ID, job.LeaseToken)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return errors.New("orchestration lease is no longer current")
	}
	return nil
}

func (s *Server) hasActiveOrchestrationJob(ctx context.Context, taskID string) bool {
	var active bool
	err := s.db.QueryRowContext(ctx, `select exists(select 1 from task_orchestration_jobs where task_id=? and status in ('queued','preparing','implementing','checking'))`, taskID).Scan(&active)
	return err == nil && active
}

// hasOrchestrationJob reports whether a task has any orchestration job record at
// all (active, paused, or terminal). Because task_id is unique on
// task_orchestration_jobs, any existing row blocks inserting a new one.
func (s *Server) hasOrchestrationJob(ctx context.Context, taskID string) bool {
	var exists bool
	err := s.db.QueryRowContext(ctx, `select exists(select 1 from task_orchestration_jobs where task_id=?)`, taskID).Scan(&exists)
	return err == nil && exists
}

func (s *Server) orchestrationDependenciesIntegrated(ctx context.Context, taskID string) (bool, error) {
	var unresolved int
	err := s.db.QueryRowContext(ctx, `select count(*) from task_dependencies dependency left join task_orchestration_jobs predecessor on predecessor.task_id=dependency.predecessor_task_id and predecessor.status in ('integrated_to_dev','released_to_main') left join git_task_records record on record.job_id=predecessor.id and record.integration_sha<>'' where dependency.task_id=? and record.job_id is null`, taskID).Scan(&unresolved)
	return unresolved == 0, err
}

func (s *Server) prepareAndDispatchOrchestrationJob(ctx context.Context, job OrchestrationJob, cfg OrchestrationConfig) error {
	if err := s.assertOrchestrationLease(ctx, job); err != nil {
		return err
	}
	project, err := s.getProjectByID(ctx, job.ProjectID)
	if err != nil {
		return err
	}
	if !isLocalRunnerID(project.Runner) {
		return errors.New("automatic orchestration currently requires a local runner")
	}
	if len(cfg.VerificationCommands) == 0 {
		return errors.New("verification commands must be configured before automatic orchestration")
	}
	task, err := s.taskByID(ctx, job.TaskID)
	if err != nil {
		return err
	}
	if task.Status != taskTodo && task.Status != taskActionRequired {
		return errors.New("task is no longer eligible for orchestration")
	}
	if ready, err := s.orchestrationDependenciesIntegrated(ctx, task.ID); err != nil {
		return err
	} else if !ready {
		return errOrchestrationWaiting
	}
	if err := s.ensureCleanRepository(ctx, project.Path); err != nil {
		return err
	}
	if err := s.gitCommand(ctx, project.Path, "show-ref", "--verify", "--quiet", "refs/heads/"+cfg.MainBranch); err != nil {
		return fmt.Errorf("main branch check: %w", err)
	}
	if err := s.ensureDevBranch(ctx, project.Path, cfg); err != nil {
		return err
	}
	if err := s.gitCommand(ctx, project.Path, "merge-base", "--is-ancestor", cfg.MainBranch, cfg.DevBranch); err != nil {
		return errors.New("main is no longer an ancestor of dev; reconcile main into dev manually before resuming automatic orchestration")
	}
	base, err := s.gitOutput(ctx, project.Path, "rev-parse", cfg.DevBranch)
	if err != nil {
		return err
	}
	base = strings.TrimSpace(base)
	branch := "task/" + task.ID + "-" + orchestrationSlug(task.Title)
	worktree := filepath.Join(filepath.Dir(project.Path), ".auto-worktrees", project.ID, job.ID)
	if job.Status == orchestrationQueued {
		now := time.Now().UTC()
		// Create a dedicated background conversation for this task so the
		// entire orchestration process — prompt, implementation, verification,
		// and review — is visible in one place. The conversation is not the
		// project's current one, so it does not disturb the user's view.
		conversationID, cerr := s.createOrchestrationConversation(ctx, project, task)
		if cerr != nil {
			return cerr
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `update task_orchestration_jobs set status=?,attempt=attempt+1,base_dev_sha=?,updated_at=? where id=? and status='queued' and lease_token=?`, orchestrationPreparing, base, now, job.ID, job.LeaseToken)
		if err == nil {
			changed, _ := result.RowsAffected()
			if changed != 1 {
				err = errors.New("job lease or state changed while preparing")
			}
		}
		if err == nil {
			_, err = tx.ExecContext(ctx, `insert into git_task_records (job_id,base_dev_sha,task_branch,worktree_path,conversation_id,created_at,updated_at) values (?,?,?,?,?,?,?) on conflict(job_id) do update set base_dev_sha=excluded.base_dev_sha,task_branch=excluded.task_branch,worktree_path=excluded.worktree_path,conversation_id=excluded.conversation_id,updated_at=excluded.updated_at`, job.ID, base, branch, worktree, conversationID, now, now)
		}
		if err == nil {
			_, err = tx.ExecContext(ctx, `insert into task_execution_intents (id,job_id,phase,attempt,status,created_at,updated_at) values (?,?,?,?,?,?,?) on conflict(job_id,phase,attempt) do nothing`, uuid.NewString(), job.ID, "implementation", job.Attempt+1, "pending", now, now)
		}
		if err == nil {
			err = recordTaskEventTx(ctx, tx, task.ID, "", "orchestration.preparing", map[string]string{"baseDevSha": base, "branch": branch}, now)
		}
		if err != nil {
			tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		job.Attempt++
	} else if job.Status != orchestrationPreparing {
		return errors.New("job is not ready to prepare")
	}
	if err := os.MkdirAll(filepath.Dir(worktree), 0700); err != nil {
		return err
	}
	if _, err := os.Stat(worktree); errors.Is(err, os.ErrNotExist) {
		branchErr := s.gitCommand(ctx, project.Path, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
		var worktreeErr error
		if branchErr == nil {
			worktreeErr = s.gitCommand(ctx, project.Path, "worktree", "add", worktree, branch)
		} else {
			worktreeErr = s.gitCommand(ctx, project.Path, "worktree", "add", "-b", branch, worktree, base)
		}
		if worktreeErr != nil {
			return fmt.Errorf("create task worktree: %w", worktreeErr)
		}
	}
	if err := s.runVerificationCommands(ctx, job, worktree, base, "baseline", cfg.VerificationCommands); err != nil {
		return fmt.Errorf("dev baseline verification failed: %w", err)
	}
	if err := s.assertOrchestrationLease(ctx, job); err != nil {
		return err
	}
	var executionIntentID, existingRun string
	if err := s.db.QueryRowContext(ctx, `select id,run_id from task_execution_intents where job_id=? and phase='implementation' and attempt=?`, job.ID, job.Attempt).Scan(&executionIntentID, &existingRun); err != nil {
		return fmt.Errorf("load implementation intent: %w", err)
	}
	if existingRun != "" {
		return s.resumeOrchestrationIntent(ctx, job, existingRun)
	}
	_, _, err = s.dispatchTaskByIDInWorkspaceWithExecutionIntent(ctx, task.ID, worktree, job.LastError, executionIntentID)
	if err != nil {
		return fmt.Errorf("dispatch implementation: %s", err)
	}
	result, err := s.db.ExecContext(ctx, `update task_orchestration_jobs set status=?,updated_at=? where id=? and status=? and lease_token=?`, orchestrationImplementing, time.Now().UTC(), job.ID, orchestrationPreparing, job.LeaseToken)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return errors.New("job lease or state changed while dispatching")
	}
	return nil
}

func (s *Server) kickOrchestratorForRun(runID string) {
	var projectID string
	if err := s.db.QueryRowContext(context.Background(), `select job.project_id from task_execution_intents intent join task_orchestration_jobs job on job.id=intent.job_id where intent.run_id=?`, runID).Scan(&projectID); err == nil {
		s.kickProjectOrchestrator(projectID)
	}
}

func (s *Server) resumeOrchestrationIntent(ctx context.Context, job OrchestrationJob, runID string) error {
	var runStatus string
	if err := s.db.QueryRowContext(ctx, `select status from runs where id=?`, runID).Scan(&runStatus); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `update task_orchestration_jobs set status=?,updated_at=? where id=? and status=? and lease_token=?`, orchestrationImplementing, time.Now().UTC(), job.ID, orchestrationPreparing, job.LeaseToken)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return errors.New("job lease or state changed while resuming implementation")
	}
	if runStatus != "queued" && runStatus != "running" {
		time.AfterFunc(50*time.Millisecond, func() { s.kickProjectOrchestrator(job.ProjectID) })
	}
	return nil
}

func (s *Server) completeOrchestrationImplementation(ctx context.Context, job OrchestrationJob, cfg OrchestrationConfig) {
	if err := s.assertOrchestrationLease(ctx, job); err != nil {
		return
	}
	var taskStatus, worktree, branch string
	err := s.db.QueryRowContext(ctx, `select task.status,record.worktree_path,record.task_branch from task_orchestration_jobs job join tasks task on task.id=job.task_id join git_task_records record on record.job_id=job.id where job.id=?`, job.ID).Scan(&taskStatus, &worktree, &branch)
	if err != nil {
		s.retryOrchestrationJob(ctx, job, cfg, err)
		return
	}
	if taskStatus == taskRunning {
		return
	}
	if taskStatus != taskAwaitingReview {
		s.retryOrchestrationJob(ctx, job, cfg, fmt.Errorf("implementation ended with task status %s", taskStatus))
		return
	}
	if job.Status == orchestrationImplementing {
		if err := s.advanceOrchestrationJob(ctx, job, orchestrationImplementing, orchestrationChecking); err != nil {
			return
		}
	} else if job.Status != orchestrationChecking {
		return
	}
	project, err := s.getProjectByID(ctx, job.ProjectID)
	if err != nil {
		s.retryOrchestrationJob(ctx, job, cfg, err)
		return
	}
	if err = s.gitCommand(ctx, worktree, "diff", "--check"); err == nil {
		err = s.runVerificationCommands(ctx, job, worktree, job.BaseDevSHA, "task", cfg.VerificationCommands)
	}
	if err != nil {
		s.retryOrchestrationJob(ctx, job, cfg, err)
		return
	}
	if err = s.commitOrchestrationWorktree(ctx, worktree, job.TaskID); err != nil {
		s.failOrchestrationJob(ctx, job, err)
		return
	}
	commit, err := s.gitOutput(ctx, worktree, "rev-parse", "HEAD")
	if err != nil {
		s.failOrchestrationJob(ctx, job, err)
		return
	}
	commit = strings.TrimSpace(commit)
	if err = s.recordTaskCommit(ctx, job, commit); err != nil {
		s.failOrchestrationJob(ctx, job, err)
		return
	}
	if err = s.runIndependentReview(ctx, project, job, worktree, commit); err != nil {
		s.retryOrchestrationJob(ctx, job, cfg, err)
		return
	}
	if err = s.integrateTaskBranch(ctx, project, job, cfg, worktree, branch, commit); err != nil {
		s.failOrchestrationJob(ctx, job, err)
		return
	}
	// Let the current worker release its in-memory guard before selecting the
	// next strictly ordered job for this project.
	time.AfterFunc(50*time.Millisecond, func() { s.kickProjectOrchestrator(job.ProjectID) })
}

func (s *Server) advanceOrchestrationJob(ctx context.Context, job OrchestrationJob, from, to string) error {
	result, err := s.db.ExecContext(ctx, `update task_orchestration_jobs set status=?,updated_at=? where id=? and status=? and lease_token=?`, to, time.Now().UTC(), job.ID, from, job.LeaseToken)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return errors.New("orchestration job state changed")
	}
	return nil
}

func (s *Server) commitOrchestrationWorktree(ctx context.Context, worktree, taskID string) error {
	status, err := s.gitOutput(ctx, worktree, "status", "--porcelain")
	if err != nil {
		return err
	}
	if strings.TrimSpace(status) == "" {
		return errors.New("implementation produced no changes to commit")
	}
	if err = s.gitCommand(ctx, worktree, "add", "--all"); err != nil {
		return err
	}
	return s.gitCommand(ctx, worktree, "commit", "-m", "Task: "+taskID)
}

type orchestrationReview struct {
	Verdict             string   `json:"verdict"`
	BlockingFindings    []string `json:"blockingFindings"`
	NonBlockingFindings []string `json:"nonBlockingFindings"`
	AcceptanceCoverage  []string `json:"acceptanceCoverage"`
	TestGaps            []string `json:"testGaps"`
	ReviewedCommit      string   `json:"reviewedCommit"`
}

type orchestrationReviewSink struct{ text strings.Builder }

func (sink *orchestrationReviewSink) Event(string, json.RawMessage)   {}
func (sink *orchestrationReviewSink) AssistantText(content, _ string) { sink.text.WriteString(content) }
func (sink *orchestrationReviewSink) SessionIdentified(string)        {}
func (sink *orchestrationReviewSink) SessionInitialized()             {}

const orchestrationReviewHeartbeatInterval = 10 * time.Second

// keepOrchestrationReviewAlive makes a long independent review observable to
// the task UI without exposing its prompt, diff, or intermediate model output.
func (s *Server) keepOrchestrationReviewAlive(ctx context.Context, job OrchestrationJob) func() {
	touch := func() {
		now := time.Now().UTC()
		result, err := s.db.ExecContext(context.Background(), `update task_orchestration_jobs set updated_at=? where id=? and lease_token=? and status='checking'`, now, job.ID, job.LeaseToken)
		if err != nil {
			return
		}
		if changed, _ := result.RowsAffected(); changed == 1 {
			_, _ = s.db.ExecContext(context.Background(), `update tasks set updated_at=? where id=?`, now, job.TaskID)
		}
	}
	touch()
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(orchestrationReviewHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				touch()
			}
		}
	}()
	return func() { close(done) }
}

func (s *Server) runIndependentReview(ctx context.Context, project Project, job OrchestrationJob, worktree, commit string) error {
	var task Task
	if err := s.db.QueryRowContext(ctx, `select id,project_id,title,description,acceptance_criteria,priority,position,status,created_at,updated_at,completed_at,cancelled_at from tasks where id=?`, job.TaskID).Scan(&task.ID, &task.ProjectID, &task.Title, &task.Description, &task.AcceptanceCriteria, &task.Priority, &task.Position, &task.Status, &task.CreatedAt, &task.UpdatedAt, new(sql.NullTime), new(sql.NullTime)); err != nil {
		return err
	}
	diff, err := s.gitOutput(ctx, worktree, "diff", "--no-ext-diff", job.BaseDevSHA+".."+commit)
	if err != nil {
		return err
	}
	files, err := s.gitOutput(ctx, worktree, "diff", "--name-only", job.BaseDevSHA+".."+commit)
	if err != nil {
		return err
	}
	prompt := fmt.Sprintf("你是独立代码审查者。不要修改文件。仅输出一个 JSON 对象，不得使用 Markdown。审查提交 %s。\n任务：%s\n说明：%s\n验收条件：%s\n文件：%s\nDiff：\n%s\n返回 {\"verdict\":\"pass|fail\",\"blockingFindings\":[],\"nonBlockingFindings\":[],\"acceptanceCoverage\":[],\"testGaps\":[],\"reviewedCommit\":\"%s\"}。任何无法确认的关键验收条件、风险或缺失测试都必须为 fail 并写入 blockingFindings。", commit, task.Title, task.Description, task.AcceptanceCriteria, files, diff, commit)
	runner := s.runner
	reviewPolicy := "plan"
	var agentID string
	_ = s.db.QueryRowContext(ctx, `select agent_id from conversations where project_id=? and is_current=true`, project.ID).Scan(&agentID)
	if agentID == "codex" {
		runner = s.codexRunnerFor(s.resolveAgentTargetEnv(project.Runner, project.Path))
		reviewPolicy = "read_only"
	} else {
		// 让独立审查的 Claude 也跟随项目所在环境（docs/20 §3.3）：Windows 目标项目
		// 审查跑 Windows 侧，WSL 项目跑 WSL 侧。
		runner = s.agentClaudeRunnerFor(s.resolveAgentTargetEnv(project.Runner, project.Path))
	}
	sink := &orchestrationReviewSink{}
	reviewCtx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	stopHeartbeat := s.keepOrchestrationReviewAlive(reviewCtx, job)
	defer stopHeartbeat()
	if err := runner.Run(reviewCtx, AgentRunRequest{SessionID: uuid.NewString(), ProjectPath: worktree, Prompt: prompt, PermissionMode: reviewPolicy, RunID: uuid.NewString()}, sink); err != nil {
		return fmt.Errorf("independent review: %w", err)
	}
	var review orchestrationReview
	if err := json.Unmarshal([]byte(strings.TrimSpace(sink.text.String())), &review); err != nil {
		return fmt.Errorf("independent review did not return valid JSON: %w", err)
	}
	payload, _ := json.Marshal(review)
	status := "passed"
	if review.Verdict != "pass" || len(review.BlockingFindings) > 0 || review.ReviewedCommit != commit {
		status = "failed"
	}
	result, err := s.db.ExecContext(ctx, `insert into verification_runs (id,job_id,phase,command,reviewed_sha,status,output,created_at,completed_at)
	select ?,?,?,?,?,?,?,?,? where exists (select 1 from task_orchestration_jobs where id=? and lease_token=?)`, uuid.NewString(), job.ID, "review", "independent-agent", commit, status, string(payload), time.Now().UTC(), time.Now().UTC(), job.ID, job.LeaseToken)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return errors.New("orchestration lease is no longer current")
	}
	if status != "passed" {
		return errors.New("independent review reported blocking findings")
	}
	return nil
}

func (s *Server) recordTaskCommit(ctx context.Context, job OrchestrationJob, commit string) error {
	result, err := s.db.ExecContext(ctx, `update git_task_records set task_commit_sha=?,updated_at=? where job_id=? and exists (select 1 from task_orchestration_jobs where id=? and lease_token=?)`, commit, time.Now().UTC(), job.ID, job.ID, job.LeaseToken)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return errors.New("orchestration lease is no longer current")
	}
	return nil
}

func (s *Server) integrateTaskBranch(ctx context.Context, project Project, job OrchestrationJob, cfg OrchestrationConfig, worktree, branch, commit string) error {
	if err := s.assertOrchestrationLease(ctx, job); err != nil {
		return err
	}
	currentDev, err := s.gitOutput(ctx, project.Path, "rev-parse", cfg.DevBranch)
	if err != nil {
		return err
	}
	currentDev = strings.TrimSpace(currentDev)
	if currentDev != job.BaseDevSHA {
		return errors.New("dev changed after task preparation; refusing to integrate")
	}
	if err = s.gitCommand(ctx, project.Path, "merge-base", "--is-ancestor", currentDev, commit); err != nil {
		return errors.New("task branch is not a fast-forward descendant of dev")
	}
	if err = s.assertOrchestrationLease(ctx, job); err != nil {
		return err
	}
	integrationWorktree := worktree + "-integration"
	defer func() {
		_ = s.gitCommand(context.Background(), project.Path, "worktree", "remove", "--force", integrationWorktree)
	}()
	// Verify the exact candidate commit before moving dev. A failed integration
	// check must never leave an unverified task commit on the shared branch.
	if err = s.gitCommand(ctx, project.Path, "worktree", "add", "--detach", integrationWorktree, commit); err != nil {
		return err
	}
	if err = s.runVerificationCommands(ctx, job, integrationWorktree, commit, "integration", cfg.VerificationCommands); err != nil {
		return fmt.Errorf("dev integration verification failed: %w", err)
	}
	if err = s.assertOrchestrationLease(ctx, job); err != nil {
		return err
	}
	if err = s.gitCommand(ctx, project.Path, "update-ref", "refs/heads/"+cfg.DevBranch, commit, currentDev); err != nil {
		return fmt.Errorf("fast-forward dev: %w", err)
	}
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `update git_task_records set integration_sha=?,updated_at=? where job_id=? and exists (select 1 from task_orchestration_jobs where id=? and lease_token=?)`, commit, now, job.ID, job.ID, job.LeaseToken)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return errors.New("orchestration lease is no longer current")
	}
	if err = s.advanceOrchestrationJob(ctx, job, orchestrationChecking, "integrated_to_dev"); err != nil {
		return err
	}
	_, _ = s.db.ExecContext(ctx, `update orchestration_outbox set status='completed',completed_at=? where job_id=? and status='pending'`, now, job.ID)
	if err = s.gitCommand(ctx, project.Path, "worktree", "remove", "--force", worktree); err != nil {
		return fmt.Errorf("clean task worktree: %w", err)
	}
	_, _ = s.db.ExecContext(ctx, `update git_task_records set worktree_path='',updated_at=? where job_id=?`, now, job.ID)
	return nil
}

func (s *Server) retryOrchestrationJob(ctx context.Context, job OrchestrationJob, cfg OrchestrationConfig, cause error) {
	if job.Attempt >= cfg.MaxFixRounds {
		s.failOrchestrationJob(ctx, job, cause)
		return
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.failOrchestrationJob(ctx, job, err)
		return
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `update task_orchestration_jobs set status=?,last_error=?,updated_at=? where id=? and lease_token=? and status in ('implementing','checking')`, orchestrationQueued, errorText(cause), now, job.ID, job.LeaseToken)
	if err != nil {
		s.failOrchestrationJob(ctx, job, err)
		return
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return
	}
	if _, err = tx.ExecContext(ctx, `update tasks set status=?,updated_at=? where id=? and status in ('running','awaiting_review','action_required')`, taskActionRequired, now, job.TaskID); err == nil {
		_, err = tx.ExecContext(ctx, `update task_execution_intents set status='completed',updated_at=? where job_id=? and phase='implementation' and attempt=?`, now, job.ID, job.Attempt)
	}
	if err != nil {
		s.failOrchestrationJob(ctx, job, err)
		return
	}
	if err = tx.Commit(); err != nil {
		s.failOrchestrationJob(ctx, job, err)
		return
	}
	// 通知：编排任务重试，任务状态变为 action_required
	s.notifyOrchestrationRetry(ctx, job, cause)
	time.AfterFunc(50*time.Millisecond, func() { s.kickProjectOrchestrator(job.ProjectID) })
}

func (s *Server) failOrchestrationJob(ctx context.Context, job OrchestrationJob, cause error) {
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `update task_orchestration_jobs set status=?,last_error=?,updated_at=? where id=? and lease_token=? and status not in ('integrated_to_dev','released_to_main')`, orchestrationNeedsHuman, errorText(cause), now, job.ID, job.LeaseToken)
	if err != nil {
		return
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return
	}
	if _, err = tx.ExecContext(ctx, `update project_orchestration_configs set frozen_reason=?,updated_at=? where project_id=?`, errorText(cause), now, job.ProjectID); err != nil {
		return
	}
	if _, err = tx.ExecContext(ctx, `update orchestration_outbox set status='failed',completed_at=? where job_id=? and status='pending'`, now, job.ID); err != nil {
		return
	}
	if err = tx.Commit(); err != nil {
		return
	}
	// 通知：编排任务失败，需要人工介入
	s.notifyOrchestrationNeedsHuman(ctx, job, cause)
}

func (s *Server) ensureDevBranch(ctx context.Context, repo string, cfg OrchestrationConfig) error {
	if err := s.gitCommand(ctx, repo, "show-ref", "--verify", "--quiet", "refs/heads/"+cfg.DevBranch); err == nil {
		return nil
	}
	return s.gitCommand(ctx, repo, "branch", cfg.DevBranch, cfg.MainBranch)
}
func (s *Server) ensureCleanRepository(ctx context.Context, repo string) error {
	output, err := s.gitOutput(ctx, repo, "status", "--porcelain")
	if err != nil {
		return err
	}
	if strings.TrimSpace(output) != "" {
		return errors.New("project worktree is not clean")
	}
	return nil
}
func (s *Server) gitCommand(ctx context.Context, repo string, args ...string) error {
	_, err := s.gitOutput(ctx, repo, args...)
	return err
}
func (s *Server) gitOutput(ctx context.Context, repo string, args ...string) (string, error) {
	commandCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, "git", args...)
	cmd.Dir = repo
	cmd.Env = gitCommandEnvironment(os.Environ())
	configureProcessGroup(cmd)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}
func orchestrationSlug(value string) string {
	value = strings.ToLower(value)
	value = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(value, "-")
	value = strings.Trim(value, "-")
	if value == "" {
		return "task"
	}
	if len(value) > 48 {
		return value[:48]
	}
	return value
}
func (s *Server) runVerificationCommands(ctx context.Context, job OrchestrationJob, worktree, sha, phase string, commands []string) error {
	for _, command := range commands {
		if err := s.assertOrchestrationLease(ctx, job); err != nil {
			return err
		}
		started := time.Now().UTC()
		commandCtx, cancel := context.WithTimeout(ctx, 20*time.Minute)
		cmd := exec.CommandContext(commandCtx, "sh", "-lc", command)
		cmd.Dir = worktree
		configureProcessGroup(cmd)
		output, err := cmd.CombinedOutput()
		cancel()
		status := "passed"
		exitCode := 0
		if err != nil {
			status = "failed"
			exitCode = 1
		}
		result, recordErr := s.db.ExecContext(ctx, `insert into verification_runs (id,job_id,phase,command,reviewed_sha,status,exit_code,output,created_at,completed_at)
		select ?,?,?,?,?,?,?,?,?,? where exists (select 1 from task_orchestration_jobs where id=? and lease_token=?)`, uuid.NewString(), job.ID, phase, command, sha, status, exitCode, string(output), started, time.Now().UTC(), job.ID, job.LeaseToken)
		if recordErr != nil {
			return recordErr
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return errors.New("orchestration lease is no longer current")
		}
		if err != nil {
			return fmt.Errorf("%s: %w", command, err)
		}
	}
	return nil
}

type ReleaseSnapshot struct {
	ID          string     `json:"id"`
	ProjectID   string     `json:"projectId"`
	DevSHA      string     `json:"devSha"`
	Branch      string     `json:"branch"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"createdAt"`
	ConfirmedAt *time.Time `json:"confirmedAt,omitempty"`
}

func (s *Server) listReleaseSnapshots(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	if !s.projectExists(r.Context(), projectID) {
		writeError(w, http.StatusNotFound, errors.New("project not found"))
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `select id,project_id,dev_sha,branch,status,created_at,confirmed_at from release_snapshots where project_id=? order by created_at desc`, projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	items := []ReleaseSnapshot{}
	for rows.Next() {
		var item ReleaseSnapshot
		if err := rows.Scan(&item.ID, &item.ProjectID, &item.DevSHA, &item.Branch, &item.Status, &item.CreatedAt, &item.ConfirmedAt); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) createReleaseSnapshot(w http.ResponseWriter, r *http.Request) {
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
	cfg, err := s.orchestrationConfig(r.Context(), projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if cfg.FrozenReason != "" {
		writeError(w, http.StatusConflict, errors.New("project orchestration is frozen: "+cfg.FrozenReason))
		return
	}
	sha, err := s.gitOutput(r.Context(), project.Path, "rev-parse", cfg.DevBranch)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	sha = strings.TrimSpace(sha)
	branch := "release/" + sha[:12]
	var existing ReleaseSnapshot
	err = s.db.QueryRowContext(r.Context(), `select id,project_id,dev_sha,branch,status,created_at,confirmed_at from release_snapshots where project_id=? and dev_sha=? order by created_at desc limit 1`, projectID, sha).Scan(&existing.ID, &existing.ProjectID, &existing.DevSHA, &existing.Branch, &existing.Status, &existing.CreatedAt, &existing.ConfirmedAt)
	if err == nil {
		writeJSON(w, http.StatusOK, existing)
		return
	}
	if !errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.gitCommand(r.Context(), project.Path, "show-ref", "--verify", "--quiet", "refs/heads/"+branch); err != nil {
		if err := s.gitCommand(r.Context(), project.Path, "branch", branch, sha); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
	}
	now := time.Now().UTC()
	item := ReleaseSnapshot{ID: uuid.NewString(), ProjectID: projectID, DevSHA: sha, Branch: branch, Status: "awaiting_main", CreatedAt: now}
	if _, err := s.db.ExecContext(r.Context(), `insert into release_snapshots (id,project_id,dev_sha,branch,status,created_at) values (?,?,?,?,?,?)`, item.ID, item.ProjectID, item.DevSHA, item.Branch, item.Status, item.CreatedAt); err != nil {
		if queryErr := s.db.QueryRowContext(r.Context(), `select id,project_id,dev_sha,branch,status,created_at,confirmed_at from release_snapshots where project_id=? and dev_sha=? order by created_at desc limit 1`, projectID, sha).Scan(&existing.ID, &existing.ProjectID, &existing.DevSHA, &existing.Branch, &existing.Status, &existing.CreatedAt, &existing.ConfirmedAt); queryErr == nil {
			writeJSON(w, http.StatusOK, existing)
			return
		}
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) confirmReleaseMergedToMain(w http.ResponseWriter, r *http.Request) {
	projectID, releaseID := chi.URLParam(r, "projectID"), chi.URLParam(r, "releaseID")
	project, err := s.getProjectByID(r.Context(), projectID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("project not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var item ReleaseSnapshot
	err = s.db.QueryRowContext(r.Context(), `select id,project_id,dev_sha,branch,status,created_at,confirmed_at from release_snapshots where id=? and project_id=?`, releaseID, projectID).Scan(&item.ID, &item.ProjectID, &item.DevSHA, &item.Branch, &item.Status, &item.CreatedAt, &item.ConfirmedAt)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("release snapshot not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if item.Status != "awaiting_main" {
		writeError(w, http.StatusConflict, errors.New("release snapshot was already confirmed"))
		return
	}
	cfg, err := s.orchestrationConfig(r.Context(), projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err = s.gitCommand(r.Context(), project.Path, "merge-base", "--is-ancestor", item.DevSHA, cfg.MainBranch); err != nil {
		writeError(w, http.StatusConflict, errors.New("main does not contain the fixed release snapshot"))
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `select job.id,record.integration_sha from task_orchestration_jobs job join git_task_records record on record.job_id=job.id where job.project_id=? and job.status='integrated_to_dev'`, projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	jobIDs := []string{}
	for rows.Next() {
		var jobID, integrationSHA string
		if err := rows.Scan(&jobID, &integrationSHA); err != nil {
			rows.Close()
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if s.gitCommand(r.Context(), project.Path, "merge-base", "--is-ancestor", integrationSHA, item.DevSHA) == nil {
			jobIDs = append(jobIDs, jobID)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	rows.Close()
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(r.Context(), `update release_snapshots set status='released_to_main',confirmed_at=? where id=? and status='awaiting_main'`, now, item.ID)
	for _, jobID := range jobIDs {
		if err == nil {
			_, err = tx.ExecContext(r.Context(), `update task_orchestration_jobs set status='released_to_main',updated_at=? where id=? and status='integrated_to_dev'`, now, jobID)
		}
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err = tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	item.Status = "released_to_main"
	item.ConfirmedAt = &now
	writeJSON(w, http.StatusOK, item)
}
