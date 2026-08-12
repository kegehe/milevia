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
	"runtime"
	"strings"
	"sync"
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
	orchestrationStopped       = "stopped"
	orchestrationRemoving      = "removing"
	orchestrationLeaseDuration = 30 * time.Second
	orchestrationLeaseRenewal  = 10 * time.Second
)

var errOrchestrationWaiting = errors.New("orchestration queue head is waiting")

const maxIndependentReviewAttempts = 3

type independentReviewProtocolError struct{ cause error }

func (err independentReviewProtocolError) Error() string {
	return fmt.Sprintf("independent review returned an invalid result after %d attempts: %v", maxIndependentReviewAttempts, err.cause)
}

func (err independentReviewProtocolError) Unwrap() error { return err.cause }

type OrchestrationConfig struct {
	ProjectID            string   `json:"projectId"`
	Enabled              bool     `json:"enabled"`
	MainBranch           string   `json:"mainBranch"`
	AgentID              string   `json:"agentId"`
	DevBranch            string   `json:"devBranch"`
	VerificationCommands []string `json:"verificationCommands"`
	MaxFixRounds         int      `json:"maxFixRounds"`
	FrozenReason         string   `json:"frozenReason,omitempty"`
}

type OrchestrationJob struct {
	ID                 string     `json:"id"`
	ProjectID          string     `json:"projectId"`
	TaskID             string     `json:"taskId"`
	Position           int        `json:"position"`
	Status             string     `json:"status"`
	Attempt            int        `json:"attempt"`
	LeaseToken         int64      `json:"leaseToken"`
	BaseDevSHA         string     `json:"baseDevSha,omitempty"`
	TaskBranch         string     `json:"taskBranch,omitempty"`
	WorktreePath       string     `json:"worktreePath,omitempty"`
	ConversationID     string     `json:"conversationId,omitempty"`
	BatchID            string     `json:"batchId,omitempty"`
	HumanDecision      string     `json:"humanDecision,omitempty"`
	ExecutionContext   string     `json:"-"`
	ResourcesCleanedAt *time.Time `json:"resourcesCleanedAt,omitempty"`
	LastError          string     `json:"lastError,omitempty"`
	PolicySnapshot     string     `json:"-"`
	CreatedAt          time.Time  `json:"createdAt"`
	UpdatedAt          time.Time  `json:"updatedAt"`
}

// OrchestrationBatch is a user-visible execution plan. Jobs remain strictly
// serial per project, while a batch provides the lifecycle boundary, name and
// conversation policy for a deliberately selected group of tasks.
type OrchestrationBatch struct {
	ID                   string    `json:"id"`
	ProjectID            string    `json:"projectId"`
	Name                 string    `json:"name"`
	ConversationStrategy string    `json:"conversationStrategy"`
	Status               string    `json:"status"`
	TaskCount            int       `json:"taskCount"`
	CompletedCount       int       `json:"completedCount"`
	CreatedAt            time.Time `json:"createdAt"`
	UpdatedAt            time.Time `json:"updatedAt"`
}

type OrchestrationWorktree struct {
	TaskID       string `json:"taskId"`
	JobID        string `json:"jobId"`
	TaskBranch   string `json:"taskBranch"`
	WorktreePath string `json:"worktreePath"`
	Status       string `json:"status"`
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
	agent_id text not null default 'claude-code',
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
create table if not exists orchestration_batches (
	id text primary key,
	project_id text not null references projects(id) on delete cascade,
	name text not null,
	conversation_strategy text not null default 'new',
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
	reviewed_sha text not null default '',
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
	if err := ensureColumn(ctx, s.db, "task_orchestration_jobs", "batch_id", "text not null default ''"); err != nil {
		return fmt.Errorf("add orchestration batch: %w", err)
	}
	if err := ensureColumn(ctx, s.db, "task_orchestration_jobs", "human_decision", "text not null default ''"); err != nil {
		return fmt.Errorf("add orchestration decision: %w", err)
	}
	if err := ensureColumn(ctx, s.db, "task_orchestration_jobs", "execution_context", "text not null default ''"); err != nil {
		return fmt.Errorf("add orchestration execution context: %w", err)
	}
	if err := ensureColumn(ctx, s.db, "git_task_records", "resources_cleaned_at", "datetime"); err != nil {
		return fmt.Errorf("add orchestration cleanup timestamp: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `create index if not exists orchestration_batches_project_created on orchestration_batches(project_id,created_at desc);
create index if not exists task_orchestration_jobs_batch on task_orchestration_jobs(batch_id,queue_position);`); err != nil {
		return fmt.Errorf("create orchestration batch indexes: %w", err)
	}
	if err := ensureColumn(ctx, s.db, "git_task_records", "conversation_id", "text not null default ''"); err != nil {
		return fmt.Errorf("add git_task_records conversation_id: %w", err)
	}
	if err := ensureColumn(ctx, s.db, "project_orchestration_configs", "agent_id", "text not null default 'claude-code'"); err != nil {
		return fmt.Errorf("add orchestration agent_id: %w", err)
	}
	if err := ensureColumn(ctx, s.db, "task_execution_intents", "reviewed_sha", "text not null default ''"); err != nil {
		return fmt.Errorf("add orchestration review intent SHA: %w", err)
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
	return OrchestrationConfig{ProjectID: projectID, MainBranch: "main", AgentID: "claude-code", VerificationCommands: []string{}, MaxFixRounds: 3}
}

// createOrchestrationConversation creates a dedicated background conversation
// (is_current=0) for an orchestrated task so the entire execution — prompt,
// implementation Run, verification commands, and independent review — is
// visible in one place without disturbing the user's current conversation. It
// uses the configured agent rather than inheriting the current conversation.
func (s *Server) createOrchestrationConversation(ctx context.Context, project Project, task Task, cfg OrchestrationConfig) (string, error) {
	permissionMode := "full_control"
	if cfg.AgentID == "codex" {
		permissionMode = "workspace_write"
	}
	now := time.Now().UTC()
	sessionID := uuid.NewString()
	conversationID := uuid.NewString()
	title := "编排：" + task.Title
	if len(title) > 80 {
		title = title[:80]
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	profileRevisionID, err := s.profileForNewConversationTx(ctx, tx, nil, project.Runner, cfg.AgentID, project.ID)
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `insert into conversations (id,project_id,claude_session_id,agent_id,agent_session_id,agent_runtime_id,agent_profile_revision_id,execution_policy,status,permission_mode,title,last_activity_at,claude_initialized,agent_initialized,is_current,created_at) values (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		conversationID, project.ID, sessionID, cfg.AgentID, sessionID, project.Runner, profileRevisionID, permissionMode, "idle", permissionMode, title, now, false, false, 0, now)
	if err != nil {
		return "", fmt.Errorf("create orchestration conversation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", err
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

func (s *Server) batchContext(ctx context.Context, job OrchestrationJob) (string, error) {
	if job.BatchID == "" {
		return "", nil
	}
	var strategy string
	err := s.db.QueryRowContext(ctx, `select conversation_strategy from orchestration_batches where id=?`, job.BatchID).Scan(&strategy)
	if errors.Is(err, sql.ErrNoRows) || strategy != "continue" {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var conversationID string
	err = s.db.QueryRowContext(ctx, `select record.conversation_id from task_orchestration_jobs prior join git_task_records record on record.job_id=prior.id where prior.batch_id=? and prior.queue_position< ? and record.conversation_id<>'' order by prior.queue_position desc limit 1`, job.BatchID, job.Position).Scan(&conversationID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	rows, err := s.db.QueryContext(ctx, `select role,content from messages where conversation_id=? and parent_tool_use_id='' order by created_at desc limit 8`, conversationID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	entries := []string{}
	for rows.Next() {
		var role, content string
		if err := rows.Scan(&role, &content); err != nil {
			return "", err
		}
		content = strings.TrimSpace(content)
		if content == "" {
			continue
		}
		if len(content) > 1000 {
			content = content[:1000]
		}
		entries = append(entries, role+": "+content)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	for left, right := 0, len(entries)-1; left < right; left, right = left+1, right-1 {
		entries[left], entries[right] = entries[right], entries[left]
	}
	if len(entries) == 0 {
		return "", nil
	}
	return "上一任务的对话摘要（仅供理解上下文；请始终在当前 worktree 工作）：\n" + strings.Join(entries, "\n\n"), nil
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
	// Orchestration status messages are durable Messages but do not have a Run.
	// events.run_id has a non-null foreign key, so broadcast this notification
	// directly instead of attempting to persist an invalid event row.
	s.broadcastConversationEvent(Event{ID: uuid.NewString(), ConversationID: conversationID, Type: "assistant.message", Payload: mustJSON(m), CreatedAt: now})
}

func (s *Server) orchestrationConfig(ctx context.Context, projectID string) (OrchestrationConfig, error) {
	cfg := defaultOrchestrationConfig(projectID)
	var commands string
	err := s.db.QueryRowContext(ctx, `select enabled,main_branch,agent_id,dev_branch,verification_commands,max_fix_rounds,frozen_reason from project_orchestration_configs where project_id=?`, projectID).Scan(&cfg.Enabled, &cfg.MainBranch, &cfg.AgentID, &cfg.DevBranch, &commands, &cfg.MaxFixRounds, &cfg.FrozenReason)
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
	if !validOrchestrationBranch(cfg.MainBranch) {
		return errors.New("main branch is invalid")
	}
	if cfg.AgentID != "claude-code" && cfg.AgentID != "codex" {
		return errors.New("automatic orchestration agent is invalid")
	}
	if cfg.MaxFixRounds < 1 || cfg.MaxFixRounds > 10 {
		return errors.New("maxFixRounds must be between 1 and 10")
	}
	if cfg.Enabled && len(cfg.VerificationCommands) == 0 {
		return errors.New("at least one verification command is required when automatic orchestration is enabled")
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
	if cfg.AgentID == "" {
		cfg.AgentID = "claude-code"
	}
	if err := validateOrchestrationConfig(cfg); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	commands, _ := json.Marshal(cfg.VerificationCommands)
	_, err := s.db.ExecContext(r.Context(), `insert into project_orchestration_configs (project_id,enabled,main_branch,agent_id,dev_branch,verification_commands,max_fix_rounds,frozen_reason,updated_at) values (?,?,?,?,?,?,?,coalesce((select frozen_reason from project_orchestration_configs where project_id=?),''),?) on conflict(project_id) do update set enabled=excluded.enabled,main_branch=excluded.main_branch,agent_id=excluded.agent_id,verification_commands=excluded.verification_commands,max_fix_rounds=excluded.max_fix_rounds,updated_at=excluded.updated_at`, projectID, cfg.Enabled, cfg.MainBranch, cfg.AgentID, cfg.DevBranch, string(commands), cfg.MaxFixRounds, projectID, time.Now().UTC())
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
		var result sql.Result
		result, err = tx.ExecContext(ctx, `insert into task_orchestration_jobs (id,project_id,task_id,queue_position,status,policy_snapshot,created_at,updated_at) values (?,?,?,?,?,?,?,?) on conflict(task_id) do nothing`, job.ID, job.ProjectID, job.TaskID, job.Position, job.Status, job.PolicySnapshot, job.CreatedAt, job.UpdatedAt)
		if err == nil {
			changed, changedErr := result.RowsAffected()
			if changedErr != nil {
				return OrchestrationJob{}, changedErr
			}
			if changed == 0 {
				err = tx.QueryRowContext(ctx, `select id,project_id,task_id,queue_position,status,attempt,lease_token,base_dev_sha,last_error,policy_snapshot,created_at,updated_at from task_orchestration_jobs where task_id=?`, task.ID).Scan(&job.ID, &job.ProjectID, &job.TaskID, &job.Position, &job.Status, &job.Attempt, &job.LeaseToken, &job.BaseDevSHA, &job.LastError, &job.PolicySnapshot, &job.CreatedAt, &job.UpdatedAt)
				if err == nil {
					if err = tx.Commit(); err != nil {
						return OrchestrationJob{}, err
					}
					s.kickProjectOrchestrator(task.ProjectID)
					return job, nil
				}
			}
		}
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

func validOrchestrationConversationStrategy(value string) bool {
	return value == "new" || value == "continue"
}

// createOrchestrationBatch creates a named execution plan and atomically puts
// its selected tasks on the existing project-serial queue in the supplied
// order. Existing single-task enqueue remains available for backwards
// compatibility, but new plans always receive a batch identity.
func (s *Server) createOrchestrationBatch(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	if !s.projectExists(r.Context(), projectID) {
		writeError(w, http.StatusNotFound, errors.New("project not found"))
		return
	}
	var input struct {
		Name                 string   `json:"name"`
		TaskIDs              []string `json:"taskIds"`
		ConversationStrategy string   `json:"conversationStrategy"`
	}
	if !decode(w, r, &input) {
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || len(input.Name) > 120 {
		writeError(w, http.StatusBadRequest, errors.New("batch name must be between 1 and 120 characters"))
		return
	}
	if len(input.TaskIDs) == 0 || len(input.TaskIDs) > 100 {
		writeError(w, http.StatusBadRequest, errors.New("a batch must contain between 1 and 100 tasks"))
		return
	}
	if input.ConversationStrategy == "" {
		input.ConversationStrategy = "new"
	}
	if !validOrchestrationConversationStrategy(input.ConversationStrategy) {
		writeError(w, http.StatusBadRequest, errors.New("conversationStrategy must be new or continue"))
		return
	}
	cfg, err := s.orchestrationConfig(r.Context(), projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !cfg.Enabled {
		writeError(w, http.StatusConflict, errOrchestrationDisabled)
		return
	}
	seen := make(map[string]bool, len(input.TaskIDs))
	tasks := make([]Task, 0, len(input.TaskIDs))
	for _, taskID := range input.TaskIDs {
		if taskID == "" || seen[taskID] {
			writeError(w, http.StatusBadRequest, errors.New("taskIds must be non-empty and unique"))
			return
		}
		seen[taskID] = true
		task, taskErr := s.taskByID(r.Context(), taskID)
		if errors.Is(taskErr, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, errOrchestrationTaskNotFound)
			return
		}
		if taskErr != nil {
			writeError(w, http.StatusInternalServerError, taskErr)
			return
		}
		if task.ProjectID != projectID {
			writeError(w, http.StatusBadRequest, errOrchestrationTaskForeign)
			return
		}
		if task.Status != taskTodo && task.Status != taskActionRequired {
			writeError(w, http.StatusConflict, errOrchestrationTaskIneligible)
			return
		}
		if s.hasOrchestrationJob(r.Context(), taskID) {
			writeError(w, http.StatusConflict, fmt.Errorf("task %s already has an orchestration record", taskID))
			return
		}
		tasks = append(tasks, task)
	}
	now := time.Now().UTC()
	batch := OrchestrationBatch{ID: uuid.NewString(), ProjectID: projectID, Name: input.Name, ConversationStrategy: input.ConversationStrategy, Status: "active", CreatedAt: now, UpdatedAt: now}
	policy, err := json.Marshal(cfg)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(r.Context(), `insert into orchestration_batches (id,project_id,name,conversation_strategy,created_at,updated_at) values (?,?,?,?,?,?)`, batch.ID, batch.ProjectID, batch.Name, batch.ConversationStrategy, now, now); err == nil {
		var position int
		err = tx.QueryRowContext(r.Context(), `select coalesce(max(queue_position),0) from task_orchestration_jobs where project_id=?`, projectID).Scan(&position)
		for _, task := range tasks {
			if err != nil {
				break
			}
			position++
			jobID := uuid.NewString()
			_, err = tx.ExecContext(r.Context(), `insert into task_orchestration_jobs (id,project_id,task_id,batch_id,queue_position,status,policy_snapshot,created_at,updated_at) values (?,?,?,?,?,'queued',?,?,?)`, jobID, projectID, task.ID, batch.ID, position, string(policy), now, now)
			if err == nil {
				_, err = tx.ExecContext(r.Context(), `insert into orchestration_outbox (id,project_id,job_id,type,idempotency_key,status,created_at) values (?,?,?,?,?,?,?)`, uuid.NewString(), projectID, jobID, "dispatch", "enqueue:"+jobID, "pending", now)
			}
			if err == nil {
				err = recordTaskEventTx(r.Context(), tx, task.ID, "", "orchestration.queued", map[string]any{"jobId": jobID, "batchId": batch.ID, "position": position}, now)
			}
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
	s.kickProjectOrchestrator(projectID)
	writeJSON(w, http.StatusAccepted, batch)
}

func (s *Server) listOrchestrationBatches(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	if !s.projectExists(r.Context(), projectID) {
		writeError(w, http.StatusNotFound, errors.New("project not found"))
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `select id,project_id,name,conversation_strategy,created_at,updated_at from orchestration_batches where project_id=? order by created_at desc`, projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	items := []OrchestrationBatch{}
	for rows.Next() {
		var item OrchestrationBatch
		if err := rows.Scan(&item.ID, &item.ProjectID, &item.Name, &item.ConversationStrategy, &item.CreatedAt, &item.UpdatedAt); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		var active, blocked, paused, awaitingMain int
		if err := s.db.QueryRowContext(r.Context(), `select count(*), coalesce(sum(case when status in ('queued','preparing','implementing','checking') then 1 else 0 end),0), coalesce(sum(case when status='needs_human' then 1 else 0 end),0), coalesce(sum(case when status in ('paused','stopped') then 1 else 0 end),0), coalesce(sum(case when status in ('awaiting_main','integrated_to_dev') then 1 else 0 end),0) from task_orchestration_jobs where batch_id=?`, item.ID).Scan(&item.TaskCount, &active, &blocked, &paused, &awaitingMain); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		item.CompletedCount = item.TaskCount - active - blocked - paused - awaitingMain
		if blocked > 0 {
			item.Status = "needs_human"
		} else if paused > 0 {
			item.Status = "paused"
		} else if awaitingMain > 0 {
			item.Status = "awaiting_main"
		} else if active > 0 {
			item.Status = "active"
		} else {
			item.Status = "completed"
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

// addTasksToOrchestrationBatch permits a running plan to grow without
// disturbing work already in progress. The existing reorder endpoint then
// controls the order of every queued or paused item in the project.
func (s *Server) addTasksToOrchestrationBatch(w http.ResponseWriter, r *http.Request) {
	projectID, batchID := chi.URLParam(r, "projectID"), chi.URLParam(r, "batchID")
	var input struct {
		TaskIDs []string `json:"taskIds"`
	}
	if !decode(w, r, &input) {
		return
	}
	if len(input.TaskIDs) == 0 || len(input.TaskIDs) > 100 {
		writeError(w, http.StatusBadRequest, errors.New("taskIds must contain between 1 and 100 tasks"))
		return
	}
	cfg, err := s.orchestrationConfig(r.Context(), projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !cfg.Enabled {
		writeError(w, http.StatusConflict, errOrchestrationDisabled)
		return
	}
	var batchExists bool
	if err = s.db.QueryRowContext(r.Context(), `select exists(select 1 from orchestration_batches where id=? and project_id=?)`, batchID, projectID).Scan(&batchExists); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !batchExists {
		writeError(w, http.StatusNotFound, errors.New("orchestration batch not found"))
		return
	}
	seen := map[string]bool{}
	tasks := make([]Task, 0, len(input.TaskIDs))
	for _, taskID := range input.TaskIDs {
		if taskID == "" || seen[taskID] {
			writeError(w, http.StatusBadRequest, errors.New("taskIds must be non-empty and unique"))
			return
		}
		seen[taskID] = true
		task, taskErr := s.taskByID(r.Context(), taskID)
		if errors.Is(taskErr, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, errOrchestrationTaskNotFound)
			return
		}
		if taskErr != nil {
			writeError(w, http.StatusInternalServerError, taskErr)
			return
		}
		if task.ProjectID != projectID {
			writeError(w, http.StatusBadRequest, errOrchestrationTaskForeign)
			return
		}
		if task.Status != taskTodo && task.Status != taskActionRequired {
			writeError(w, http.StatusConflict, errOrchestrationTaskIneligible)
			return
		}
		if s.hasOrchestrationJob(r.Context(), taskID) {
			writeError(w, http.StatusConflict, fmt.Errorf("task %s already has an orchestration record", taskID))
			return
		}
		tasks = append(tasks, task)
	}
	policy, err := json.Marshal(cfg)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	var position int
	err = tx.QueryRowContext(r.Context(), `select coalesce(max(queue_position),0) from task_orchestration_jobs where project_id=?`, projectID).Scan(&position)
	for _, task := range tasks {
		if err != nil {
			break
		}
		position++
		jobID := uuid.NewString()
		_, err = tx.ExecContext(r.Context(), `insert into task_orchestration_jobs (id,project_id,task_id,batch_id,queue_position,status,policy_snapshot,created_at,updated_at) values (?,?,?,?,?,'queued',?,?,?)`, jobID, projectID, task.ID, batchID, position, string(policy), now, now)
		if err == nil {
			_, err = tx.ExecContext(r.Context(), `insert into orchestration_outbox (id,project_id,job_id,type,idempotency_key,status,created_at) values (?,?,?,?,?,?,?)`, uuid.NewString(), projectID, jobID, "dispatch", "enqueue:"+jobID, "pending", now)
		}
		if err == nil {
			err = recordTaskEventTx(r.Context(), tx, task.ID, "", "orchestration.queued", map[string]any{"jobId": jobID, "batchId": batchID, "position": position}, now)
		}
	}
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `update orchestration_batches set updated_at=? where id=?`, now, batchID)
	}
	if err != nil {
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
	var jobID, projectID, projectPath, status, worktree, branch string
	err = tx.QueryRowContext(r.Context(), `select job.id,job.project_id,project.path,job.status,coalesce(record.worktree_path,''),coalesce(record.task_branch,'') from task_orchestration_jobs job join projects project on project.id=job.project_id left join git_task_records record on record.job_id=job.id where job.task_id=? and job.status in ('queued','preparing','implementing','checking','paused','needs_human','stopped') order by job.updated_at desc limit 1`, taskID).Scan(&jobID, &projectID, &projectPath, &status, &worktree, &branch)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("task is not in the orchestration queue"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if status != orchestrationQueued && status != orchestrationPaused && status != orchestrationStopped {
		writeError(w, http.StatusConflict, fmt.Errorf("cannot remove a task that is %s", status))
		return
	}
	// Commit an internal cleanup state before touching Git. A queued job can
	// otherwise be selected by the scheduler between the read above and
	// worktree cleanup; resume is intentionally unavailable in this state.
	claim, err := tx.ExecContext(r.Context(), `update task_orchestration_jobs set status=?,updated_at=? where id=? and status=?`, orchestrationRemoving, time.Now().UTC(), jobID, status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if changed, _ := claim.RowsAffected(); changed != 1 {
		writeError(w, http.StatusConflict, errors.New("job state changed before it could be removed"))
		return
	}
	if err = tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.cancelOrchestrationJob(jobID)
	s.stopOrchestrationRun(r.Context(), taskID)
	cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelCleanup()
	if err = s.waitForOrchestrationJobShutdown(cleanupCtx, jobID); err != nil {
		_, _ = s.db.ExecContext(context.Background(), `update task_orchestration_jobs set status=?,updated_at=? where id=? and status=?`, orchestrationStopped, time.Now().UTC(), jobID, orchestrationRemoving)
		writeError(w, http.StatusConflict, fmt.Errorf("wait for task execution to stop before cleanup: %w", err))
		return
	}
	// A removed job must not retain its deterministic branch name. Clean the
	// Git resources before deleting the record so a cleanup failure remains
	// actionable rather than becoming an invisible orphan.
	if err = s.removeOrchestrationGitResources(r.Context(), projectPath, worktree, branch); err != nil {
		_, _ = s.db.ExecContext(r.Context(), `update task_orchestration_jobs set status=?,updated_at=? where id=? and status=?`, orchestrationStopped, time.Now().UTC(), jobID, orchestrationRemoving)
		writeError(w, http.StatusConflict, err)
		return
	}
	tx, err = s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(r.Context(), `delete from task_orchestration_jobs where id=? and status=?`, jobID, orchestrationRemoving)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		writeError(w, http.StatusConflict, errors.New("job state changed while removing it"))
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
	rows, err := s.db.QueryContext(r.Context(), `select job.id,job.project_id,job.task_id,job.queue_position,job.status,job.attempt,job.lease_token,job.base_dev_sha,coalesce(record.task_branch,''),coalesce(record.worktree_path,''),coalesce(record.conversation_id,''),job.batch_id,job.human_decision,record.resources_cleaned_at,job.last_error,job.created_at,job.updated_at from task_orchestration_jobs job left join git_task_records record on record.job_id=job.id where job.project_id=? order by job.queue_position`, projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	items := []OrchestrationJob{}
	for rows.Next() {
		var item OrchestrationJob
		if err := rows.Scan(&item.ID, &item.ProjectID, &item.TaskID, &item.Position, &item.Status, &item.Attempt, &item.LeaseToken, &item.BaseDevSHA, &item.TaskBranch, &item.WorktreePath, &item.ConversationID, &item.BatchID, &item.HumanDecision, &item.ResourcesCleanedAt, &item.LastError, &item.CreatedAt, &item.UpdatedAt); err != nil {
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

func (s *Server) submitOrchestrationDecision(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")
	var input struct {
		Decision string `json:"decision"`
	}
	if !decode(w, r, &input) {
		return
	}
	input.Decision = strings.TrimSpace(input.Decision)
	if input.Decision == "" || len(input.Decision) > 8000 {
		writeError(w, http.StatusBadRequest, errors.New("decision must be between 1 and 8000 characters"))
		return
	}
	var job OrchestrationJob
	err := s.db.QueryRowContext(r.Context(), `select job.id,job.project_id,coalesce(record.conversation_id,'') from task_orchestration_jobs job left join git_task_records record on record.job_id=job.id where job.task_id=? and job.status='needs_human'`, taskID).Scan(&job.ID, &job.ProjectID, &job.ConversationID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusConflict, errors.New("task is not waiting for a human decision"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	now := time.Now().UTC()
	if _, err = s.db.ExecContext(r.Context(), `update task_orchestration_jobs set human_decision=?,updated_at=? where id=? and status='needs_human'`, input.Decision, now, job.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if job.ConversationID != "" {
		message := Message{ID: uuid.NewString(), ConversationID: job.ConversationID, Role: "user", Content: "人工决策：\n" + input.Decision, CreatedAt: now}
		if _, err = s.db.ExecContext(r.Context(), `insert into messages (id,conversation_id,run_id,role,content,parent_tool_use_id,created_at) values (?,?,?,?,?,?,?)`, message.ID, message.ConversationID, "", message.Role, message.Content, "", now); err == nil {
			s.broadcastConversationEvent(Event{ID: uuid.NewString(), ConversationID: job.ConversationID, Type: "user.message", Payload: mustJSON(message), CreatedAt: now})
		}
	}
	// Resume through the same guarded state transition used by the normal UI.
	s.setOrchestrationJobPause(w, r, orchestrationQueued)
}

func (s *Server) cleanupOrchestrationResources(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")
	var input struct {
		ConfirmUnmerged bool `json:"confirmUnmerged"`
	}
	if !decode(w, r, &input) {
		return
	}
	var job OrchestrationJob
	var projectPath string
	err := s.db.QueryRowContext(r.Context(), `select job.id,job.project_id,job.status,coalesce(record.task_branch,''),coalesce(record.worktree_path,''),record.resources_cleaned_at,project.path from task_orchestration_jobs job join projects project on project.id=job.project_id join git_task_records record on record.job_id=job.id where job.task_id=?`, taskID).Scan(&job.ID, &job.ProjectID, &job.Status, &job.TaskBranch, &job.WorktreePath, &job.ResourcesCleanedAt, &projectPath)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("orchestration resources not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if job.ResourcesCleanedAt != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if job.Status != "released_to_main" && job.Status != orchestrationStopped && job.Status != orchestrationNeedsHuman {
		writeError(w, http.StatusConflict, errors.New("pause or stop the orchestration task before cleaning its resources"))
		return
	}
	if job.Status != "released_to_main" && !input.ConfirmUnmerged {
		writeError(w, http.StatusConflict, errors.New("this branch is not merged into main; explicit unmerged cleanup confirmation is required"))
		return
	}
	s.runManagersMu.RLock()
	runner := s.runManagers[job.ProjectID]
	s.runManagersMu.RUnlock()
	if runner != nil && runner.StatusSnapshot().Status == RunStatusRunning {
		writeError(w, http.StatusConflict, errors.New("stop the project acceptance process before cleaning its worktree"))
		return
	}
	if err = s.removeOrchestrationGitResources(r.Context(), projectPath, job.WorktreePath, job.TaskBranch); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	now := time.Now().UTC()
	if _, err = s.db.ExecContext(r.Context(), `update git_task_records set worktree_path='',resources_cleaned_at=?,updated_at=? where job_id=?`, now, now, job.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) pauseOrchestrationJob(w http.ResponseWriter, r *http.Request) {
	s.setOrchestrationJobPause(w, r, orchestrationPaused)
}

// stopOrchestrationJob removes a job from scheduling but intentionally keeps
// its branch and worktree available for inspection or a later restart.
func (s *Server) stopOrchestrationJob(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	var jobID, projectID string
	err = tx.QueryRowContext(r.Context(), `select id,project_id from task_orchestration_jobs where task_id=? and status in ('queued','preparing','implementing','checking','paused','needs_human')`, taskID).Scan(&jobID, &projectID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusConflict, errors.New("job cannot be stopped"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	now := time.Now().UTC()
	result, err := tx.ExecContext(r.Context(), `update task_orchestration_jobs set status=?,last_error='',updated_at=? where id=? and status in ('queued','preparing','implementing','checking','paused','needs_human')`, orchestrationStopped, now, jobID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		writeError(w, http.StatusConflict, errors.New("job state changed before it could be stopped"))
		return
	}
	if _, err = tx.ExecContext(r.Context(), `update tasks set status=?,updated_at=? where id=? and status in ('todo','running','awaiting_review','action_required')`, taskActionRequired, now, taskID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err = recordTaskEventTx(r.Context(), tx, taskID, "", "orchestration.stopped", map[string]any{"jobId": jobID}, now); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var blocked int
	if err = tx.QueryRowContext(r.Context(), `select count(*) from task_orchestration_jobs where project_id=? and status='needs_human'`, projectID).Scan(&blocked); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if blocked == 0 {
		if _, err = tx.ExecContext(r.Context(), `update project_orchestration_configs set frozen_reason='',updated_at=? where project_id=?`, now, projectID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	if err = tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.cancelOrchestrationJob(jobID)
	s.stopOrchestrationRun(r.Context(), taskID)
	s.kickProjectOrchestrator(projectID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) resumeOrchestrationJob(w http.ResponseWriter, r *http.Request) {
	s.setOrchestrationJobPause(w, r, orchestrationQueued)
}

func (s *Server) mergeTaskBranchToMain(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")
	var job OrchestrationJob
	var branch, commit string
	err := s.db.QueryRowContext(r.Context(), `select job.id,job.project_id,job.status,record.task_branch,record.integration_sha from task_orchestration_jobs job join git_task_records record on record.job_id=job.id where job.task_id=?`, taskID).Scan(&job.ID, &job.ProjectID, &job.Status, &branch, &commit)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("orchestration task branch not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if (job.Status != "awaiting_main" && job.Status != "integrated_to_dev") || strings.TrimSpace(branch) == "" || strings.TrimSpace(commit) == "" {
		writeError(w, http.StatusConflict, errors.New("task branch is not ready to merge"))
		return
	}
	project, err := s.getProjectByID(r.Context(), job.ProjectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	cfg, err := s.orchestrationConfig(r.Context(), job.ProjectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	release, acquired := s.acquireProjectWorkspace(job.ProjectID, "orchestration-merge:"+job.ID)
	if !acquired {
		writeError(w, http.StatusConflict, errors.New("project workspace is occupied"))
		return
	}
	defer release()
	if err = s.ensureCleanRepository(r.Context(), project.Path); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	currentBranch, err := s.gitOutput(r.Context(), project.Path, "branch", "--show-current")
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	if strings.TrimSpace(currentBranch) != cfg.MainBranch {
		writeError(w, http.StatusConflict, fmt.Errorf("project worktree must be on %s before merging", cfg.MainBranch))
		return
	}
	branchCommit, err := s.gitOutput(r.Context(), project.Path, "rev-parse", branch)
	if err != nil {
		writeError(w, http.StatusConflict, fmt.Errorf("task branch is unavailable: %w", err))
		return
	}
	if strings.TrimSpace(branchCommit) != strings.TrimSpace(commit) {
		writeError(w, http.StatusConflict, errors.New("task branch no longer points to the reviewed commit"))
		return
	}
	if err = s.gitCommand(r.Context(), project.Path, "merge-base", "--is-ancestor", commit, cfg.MainBranch); err != nil {
		if err = s.gitCommand(r.Context(), project.Path, "merge", "--no-ff", "--no-edit", branch); err != nil {
			// Never leave a conflicted merge in the project worktree. The user can
			// resolve it on a dedicated branch and try again after it is clean.
			_ = s.gitCommand(r.Context(), project.Path, "merge", "--abort")
			writeError(w, http.StatusConflict, fmt.Errorf("cannot merge task branch %s into %s: %w", branch, cfg.MainBranch, err))
			return
		}
	}
	if err = s.gitCommand(r.Context(), project.Path, "merge-base", "--is-ancestor", commit, cfg.MainBranch); err != nil {
		writeError(w, http.StatusConflict, errors.New("main does not contain this task branch commit after merge"))
		return
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	var result sql.Result
	if result, err = tx.ExecContext(r.Context(), `update task_orchestration_jobs set status='released_to_main',updated_at=? where id=? and status in ('awaiting_main','integrated_to_dev')`, now, job.ID); err == nil {
		if changed, _ := result.RowsAffected(); changed != 1 {
			writeError(w, http.StatusConflict, errors.New("task branch status changed before confirmation"))
			return
		}
		_, err = tx.ExecContext(r.Context(), `update tasks set status=?,completed_at=?,updated_at=? where id=? and status in ('awaiting_review','action_required')`, taskDone, now, now, taskID)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err = tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.kickProjectOrchestrator(job.ProjectID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) setOrchestrationJobPause(w http.ResponseWriter, r *http.Request, target string) {
	taskID := chi.URLParam(r, "taskID")
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	var jobID, projectID, previousStatus, taskCommitSHA string
	err = tx.QueryRowContext(r.Context(), `select job.id,job.project_id,job.status,coalesce(record.task_commit_sha,'') from task_orchestration_jobs job left join git_task_records record on record.job_id=job.id where job.task_id=?`, taskID).Scan(&jobID, &projectID, &previousStatus, &taskCommitSHA)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("orchestration job not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	allowed := "status in ('queued','preparing','implementing','checking')"
	if target == orchestrationQueued {
		allowed = "status in ('paused','needs_human','stopped')"
	}
	update := `update task_orchestration_jobs set status=?,updated_at=? where task_id=? and ` + allowed
	if target == orchestrationQueued {
		update = `update task_orchestration_jobs set status=?,last_error='',updated_at=? where task_id=? and ` + allowed
	}
	result, err := tx.ExecContext(r.Context(), update, target, time.Now().UTC(), taskID)
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
		eligibleStatuses := "'todo','running','awaiting_review','action_required'"
		if previousStatus == orchestrationPaused {
			eligibleStatuses = "'running','awaiting_review','action_required'"
		}
		if previousStatus == orchestrationNeedsHuman || previousStatus == orchestrationPaused || previousStatus == orchestrationStopped {
			nextTaskStatus := taskActionRequired
			// A recorded commit means implementation has already completed. Resume
			// the verification/review stage rather than dispatching the agent again.
			if taskCommitSHA != "" {
				nextTaskStatus = taskAwaitingReview
			}
			if _, err := tx.ExecContext(r.Context(), `update tasks set status=?,updated_at=? where id=? and status in (`+eligibleStatuses+`)`, nextTaskStatus, time.Now().UTC(), taskID); err != nil {
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
		s.cancelOrchestrationJob(jobID)
		// 通知：编排任务已暂停
		s.notifyOrchestrationPaused(r.Context(), projectID, taskID)
		s.stopOrchestrationRun(r.Context(), taskID)
	} else if target == orchestrationQueued && previousStatus == orchestrationNeedsHuman {
		// 通知：编排任务从 needs_human 恢复
		s.notifyOrchestrationResumed(r.Context(), projectID, taskID)
	}
	if target == orchestrationQueued {
		s.kickProjectOrchestrator(projectID)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) stopOrchestrationRun(ctx context.Context, taskID string) {
	var runID string
	if err := s.db.QueryRowContext(ctx, `select run_id from task_execution_intents where job_id=(select id from task_orchestration_jobs where task_id=?) and run_id<>'' order by attempt desc limit 1`, taskID).Scan(&runID); err == nil {
		// The job status already prevents late worker transitions. Force-stop also
		// handles an agent currently streaming in the dedicated conversation.
		_, _, _ = s.stopRunByID(ctx, runID, true)
	}
}

func (s *Server) recoverOrchestration(ctx context.Context) {
	// A process can exit after reserving a job for resource cleanup. Release the
	// reservation for manual retry rather than leaving an invisible job forever.
	_, _ = s.db.ExecContext(ctx, `update task_orchestration_jobs set status=?,updated_at=? where status=?`, orchestrationStopped, time.Now().UTC(), orchestrationRemoving)
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
	jobCtx, cancel := context.WithCancel(ctx)
	s.orchestrationMu.Lock()
	s.orchestrationCancels[job.ID] = cancel
	done := make(chan struct{})
	s.orchestrationDone[job.ID] = done
	s.orchestrationMu.Unlock()
	defer func() {
		cancel()
		s.orchestrationMu.Lock()
		delete(s.orchestrationCancels, job.ID)
		delete(s.orchestrationDone, job.ID)
		close(done)
		s.orchestrationMu.Unlock()
	}()
	var cfg OrchestrationConfig
	if err := json.Unmarshal([]byte(job.PolicySnapshot), &cfg); err != nil {
		s.failOrchestrationJob(jobCtx, *job, fmt.Errorf("decode policy snapshot: %w", err))
		return
	}
	// Snapshots created before agent selection was added have no agentId.
	// Preserve their previous behavior instead of leaving them undispatchable.
	if cfg.AgentID == "" {
		cfg.AgentID = "claude-code"
	}
	if job.Status == orchestrationChecking {
		if record, err := s.orchestrationTaskRecord(jobCtx, job.ID); err == nil && record.TaskCommitSHA != "" {
			if err := s.resumeCommittedOrchestrationReview(jobCtx, *job, cfg, record); err != nil && !errors.Is(err, context.Canceled) {
				var protocolErr independentReviewProtocolError
				if errors.As(err, &protocolErr) {
					s.failOrchestrationJob(jobCtx, *job, err)
				} else {
					s.retryOrchestrationJob(jobCtx, *job, cfg, err)
				}
			}
			return
		}
	}
	if job.Status == orchestrationImplementing || job.Status == orchestrationChecking {
		s.completeOrchestrationImplementation(jobCtx, *job, cfg)
		return
	}
	if job.Status == orchestrationQueued {
		if record, err := s.orchestrationTaskRecord(jobCtx, job.ID); err == nil && record.TaskCommitSHA != "" {
			if err := s.resumeCommittedOrchestrationReview(jobCtx, *job, cfg, record); err != nil && !errors.Is(err, context.Canceled) {
				var protocolErr independentReviewProtocolError
				if errors.As(err, &protocolErr) {
					s.failOrchestrationJob(jobCtx, *job, err)
				} else {
					s.retryOrchestrationJob(jobCtx, *job, cfg, err)
				}
			}
			return
		}
	}
	if err := s.prepareAndDispatchOrchestrationJob(jobCtx, *job, cfg); err != nil && !errors.Is(err, errOrchestrationWaiting) && !errors.Is(err, context.Canceled) {
		s.failOrchestrationJob(jobCtx, *job, err)
		return
	}
}

// resumeCommittedOrchestrationReview continues from an implementation commit.
// It is used after pausing or unfreezing a job in checking, and deliberately
// never creates another implementation run for the same task branch.
func (s *Server) resumeCommittedOrchestrationReview(ctx context.Context, job OrchestrationJob, cfg OrchestrationConfig, record GitTaskRecord) error {
	if err := s.assertOrchestrationLease(ctx, job); err != nil {
		return err
	}
	project, err := s.getProjectByID(ctx, job.ProjectID)
	if err != nil {
		return err
	}
	if record.WorktreePath == "" || record.TaskBranch == "" {
		return errors.New("committed orchestration record is incomplete")
	}
	if _, err := os.Stat(record.WorktreePath); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(record.WorktreePath), 0700); err != nil {
			return err
		}
		if err := s.gitCommand(ctx, project.Path, "worktree", "add", record.WorktreePath, record.TaskBranch); err != nil {
			return fmt.Errorf("restore task worktree for review: %w", err)
		}
	} else if err != nil {
		return err
	}
	commit, err := s.gitOutput(ctx, record.WorktreePath, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(commit) != record.TaskCommitSHA {
		return errors.New("task branch no longer points to the recorded commit")
	}
	if job.Status == orchestrationQueued {
		if err := s.advanceOrchestrationJob(ctx, job, orchestrationQueued, orchestrationChecking); err != nil {
			return err
		}
	} else if job.Status != orchestrationChecking {
		return errors.New("committed review is not ready to resume")
	}
	if err := s.runIndependentReview(ctx, project, job, cfg, record.WorktreePath, record.TaskCommitSHA); err != nil {
		return err
	}
	return s.completeOrchestrationTaskBranch(ctx, project.Path, job, record.WorktreePath, record.TaskCommitSHA)
}

func (s *Server) cancelOrchestrationJob(jobID string) {
	s.orchestrationMu.Lock()
	cancel := s.orchestrationCancels[jobID]
	s.orchestrationMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// waitForOrchestrationJobShutdown waits for both the orchestration worker and
// the process-backed implementation run. Git resources are only removed after
// both have released the worktree.
func (s *Server) waitForOrchestrationJobShutdown(ctx context.Context, jobID string) error {
	s.orchestrationMu.Lock()
	done := s.orchestrationDone[jobID]
	s.orchestrationMu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		var active bool
		err := s.db.QueryRowContext(ctx, `select exists(select 1 from task_execution_intents intent join runs run on run.id=intent.run_id where intent.job_id=? and run.status in ('queued','running'))`, jobID).Scan(&active)
		if err != nil {
			return err
		}
		if !active {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Server) nextOrchestrationJob(ctx context.Context, projectID string) (*OrchestrationJob, error) {
	var job OrchestrationJob
	err := s.db.QueryRowContext(ctx, `select id,project_id,task_id,queue_position,status,attempt,lease_token,base_dev_sha,last_error,policy_snapshot,batch_id,human_decision,execution_context,created_at,updated_at from task_orchestration_jobs where project_id=? and status not in ('integrated_to_dev','awaiting_main','released_to_main','stopped','removing') order by queue_position limit 1`, projectID).Scan(&job.ID, &job.ProjectID, &job.TaskID, &job.Position, &job.Status, &job.Attempt, &job.LeaseToken, &job.BaseDevSHA, &job.LastError, &job.PolicySnapshot, &job.BatchID, &job.HumanDecision, &job.ExecutionContext, &job.CreatedAt, &job.UpdatedAt)
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
	result, err := s.db.ExecContext(ctx, `update project_orchestration_leases set expires_at=?,updated_at=? where token=? and owner_id=? and expires_at>? and project_id=(select project_id from task_orchestration_jobs where id=? and lease_token=? and status in ('queued','preparing','implementing','checking'))`, now.Add(orchestrationLeaseDuration), now, job.LeaseToken, s.orchestrationOwner, now, job.ID, job.LeaseToken)
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
	err := s.db.QueryRowContext(ctx, `select count(*) from task_dependencies dependency left join task_orchestration_jobs predecessor on predecessor.task_id=dependency.predecessor_task_id and predecessor.status='released_to_main' where dependency.task_id=? and predecessor.id is null`, taskID).Scan(&unresolved)
	return unresolved == 0, err
}

func (s *Server) isOrchestrationBranchAwaitingMain(ctx context.Context, taskID string) bool {
	var awaiting bool
	err := s.db.QueryRowContext(ctx, `select exists(select 1 from task_orchestration_jobs where task_id=? and status in ('awaiting_main','integrated_to_dev'))`, taskID).Scan(&awaiting)
	return err == nil && awaiting
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
	var base, branch, worktree string
	newRecord := false
	if job.Status == orchestrationQueued {
		record, recordErr := s.orchestrationTaskRecord(ctx, job.ID)
		if recordErr == nil {
			base, branch, worktree = record.BaseDevSHA, record.TaskBranch, record.WorktreePath
		} else if !errors.Is(recordErr, sql.ErrNoRows) {
			return recordErr
		} else {
			newRecord = true
			branch = automaticOrchestrationBranch(time.Now(), task.ID)
			if err := s.gitCommand(ctx, project.Path, "show-ref", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
				return errors.New("automatic orchestration branch already exists for this task")
			}
			base, err = s.gitOutput(ctx, project.Path, "rev-parse", cfg.MainBranch)
			if err != nil {
				return err
			}
			base = strings.TrimSpace(base)
			worktree = filepath.Join(filepath.Dir(project.Path), ".auto-worktrees", project.ID, job.ID)
		}
		now := time.Now().UTC()
		// Create a dedicated background conversation for this task so the
		// entire orchestration process — prompt, implementation, verification,
		// and review — is visible in one place. The conversation is not the
		// project's current one, so it does not disturb the user's view.
		conversationID := ""
		continuityContext := ""
		if recordErr != nil {
			var cerr error
			continuityContext, cerr = s.batchContext(ctx, job)
			if cerr == nil {
				conversationID, cerr = s.createOrchestrationConversation(ctx, project, task, cfg)
			}
			if cerr != nil {
				return cerr
			}
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
		if err == nil && continuityContext != "" {
			_, err = tx.ExecContext(ctx, `update task_orchestration_jobs set execution_context=? where id=?`, continuityContext, job.ID)
		}
		if err == nil && recordErr != nil {
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
		job.ExecutionContext = continuityContext
	} else if job.Status != orchestrationPreparing {
		return errors.New("job is not ready to prepare")
	} else {
		record, recordErr := s.orchestrationTaskRecord(ctx, job.ID)
		if recordErr != nil {
			return recordErr
		}
		base, branch, worktree = record.BaseDevSHA, record.TaskBranch, record.WorktreePath
	}
	conversationID, err := s.orchestrationConversationID(ctx, job.ID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(conversationID) == "" {
		conversationID, err = s.createOrchestrationConversation(ctx, project, task, cfg)
		if err != nil {
			return err
		}
		result, updateErr := s.db.ExecContext(ctx, `update git_task_records set conversation_id=?,updated_at=? where job_id=? and conversation_id=''`, conversationID, time.Now().UTC(), job.ID)
		if updateErr != nil {
			return updateErr
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return errors.New("orchestration conversation changed while preparing")
		}
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
	// A repair attempt must keep the failed implementation worktree and proceed
	// directly to the agent. The one exception is an initial baseline failure:
	// no implementation Run was started, so resuming must verify the baseline
	// again before any agent is dispatched.
	var implementationStarted bool
	if !newRecord {
		err = s.db.QueryRowContext(ctx, `select exists(select 1 from task_execution_intents where job_id=? and phase='implementation' and run_id<>'')`, job.ID).Scan(&implementationStarted)
		if err != nil {
			return err
		}
	}
	if newRecord || !implementationStarted {
		if err := s.runVerificationCommands(ctx, job, worktree, base, "baseline", cfg.VerificationCommands); err != nil {
			return fmt.Errorf("main baseline verification failed: %w", err)
		}
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
	repairContext := strings.TrimSpace(job.LastError)
	if strings.TrimSpace(job.ExecutionContext) != "" {
		if repairContext != "" {
			repairContext += "\n\n"
		}
		repairContext += strings.TrimSpace(job.ExecutionContext)
	}
	if strings.TrimSpace(job.HumanDecision) != "" {
		if repairContext != "" {
			repairContext += "\n\n"
		}
		repairContext += "用户已作出以下业务决策，请据此继续：\n" + strings.TrimSpace(job.HumanDecision)
	}
	_, _, err = s.dispatchTaskByIDInWorkspaceWithExecutionIntentForConversation(ctx, task.ID, worktree, repairContext, executionIntentID, conversationID)
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
	var taskStatus, worktree string
	err := s.db.QueryRowContext(ctx, `select task.status,record.worktree_path from task_orchestration_jobs job join tasks task on task.id=job.task_id join git_task_records record on record.job_id=job.id where job.id=?`, job.ID).Scan(&taskStatus, &worktree)
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
	if err = s.runIndependentReview(ctx, project, job, cfg, worktree, commit); err != nil {
		var protocolErr independentReviewProtocolError
		if errors.As(err, &protocolErr) {
			s.failOrchestrationJob(ctx, job, err)
			return
		}
		s.retryOrchestrationJob(ctx, job, cfg, err)
		return
	}
	if err = s.completeOrchestrationTaskBranch(ctx, project.Path, job, worktree, commit); err != nil {
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

type orchestrationReviewSink struct {
	mu     sync.Mutex
	text   strings.Builder
	onText func(string)
}

func (sink *orchestrationReviewSink) Event(string, json.RawMessage) {}
func (sink *orchestrationReviewSink) AssistantText(content, _ string) {
	if strings.TrimSpace(content) == "" {
		return
	}
	sink.mu.Lock()
	sink.text.WriteString(content)
	sink.mu.Unlock()
	if sink.onText != nil {
		sink.onText(content)
	}
}
func (sink *orchestrationReviewSink) SessionIdentified(string) {}
func (sink *orchestrationReviewSink) SessionInitialized()      {}

func (sink *orchestrationReviewSink) String() string {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return sink.text.String()
}

func parseOrchestrationReview(output, commit string) (orchestrationReview, error) {
	output = strings.TrimSpace(output)
	for offset := 0; offset < len(output); {
		index := strings.IndexByte(output[offset:], '{')
		if index < 0 {
			break
		}
		offset += index
		decoder := json.NewDecoder(strings.NewReader(output[offset:]))
		var review orchestrationReview
		if err := decoder.Decode(&review); err == nil && (review.Verdict == "pass" || review.Verdict == "fail") && review.ReviewedCommit == commit {
			return review, nil
		}
		offset++
	}
	return orchestrationReview{}, errors.New("no valid review JSON object found")
}

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

func (s *Server) runIndependentReview(ctx context.Context, project Project, job OrchestrationJob, cfg OrchestrationConfig, worktree, commit string) error {
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
	reviewIntentID, err := s.beginIndependentReviewIntent(ctx, job, commit)
	if err != nil {
		return err
	}
	completeReview := func(status, output string) error {
		now := time.Now().UTC()
		result, recordErr := s.db.ExecContext(ctx, `update task_execution_intents set status=?,updated_at=? where id=? and exists (select 1 from task_orchestration_jobs where id=? and lease_token=?)`, status, now, reviewIntentID, job.ID, job.LeaseToken)
		if recordErr != nil {
			return recordErr
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return errors.New("orchestration lease is no longer current")
		}
		result, recordErr = s.db.ExecContext(ctx, `update verification_runs set status=?,output=?,completed_at=? where id=? and job_id=?`, status, output, now, reviewIntentID, job.ID)
		if recordErr != nil {
			return recordErr
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return errors.New("independent review record is missing")
		}
		return nil
	}
	prompt := fmt.Sprintf("你是独立代码审查者。不要修改文件。仅输出一个 JSON 对象，不得使用 Markdown。审查提交 %s。\n任务：%s\n说明：%s\n验收条件：%s\n文件：%s\nDiff：\n%s\n返回 {\"verdict\":\"pass|fail\",\"blockingFindings\":[],\"nonBlockingFindings\":[],\"acceptanceCoverage\":[],\"testGaps\":[],\"reviewedCommit\":\"%s\"}。任何无法确认的关键验收条件、风险或缺失测试都必须为 fail 并写入 blockingFindings。", commit, task.Title, task.Description, task.AcceptanceCriteria, files, diff, commit)
	runner := s.runner
	reviewPolicy := "read_only"
	readOnlyTools := orchestrationReviewReadOnlyTools(cfg.AgentID)
	if cfg.AgentID == "codex" {
		runner = s.codexRunnerFor(s.resolveAgentTargetEnv(project.Runner, project.Path))
	} else {
		// 让独立审查的 Claude 也跟随项目所在环境（docs/20 §3.3）：Windows 目标项目
		// 审查跑 Windows 侧，WSL 项目跑 WSL 侧。
		runner = s.agentClaudeRunnerFor(s.resolveAgentTargetEnv(project.Runner, project.Path))
	}
	profile, err := s.orchestrationRuntimeProfile(ctx, project, job, cfg.AgentID)
	if err != nil {
		return fmt.Errorf("resolve orchestration profile: %w", err)
	}
	reviewCtx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	stopHeartbeat := s.keepOrchestrationReviewAlive(reviewCtx, job)
	defer stopHeartbeat()
	var review orchestrationReview
	var reviewErr error
	for attempt := 1; attempt <= maxIndependentReviewAttempts; attempt++ {
		sink := &orchestrationReviewSink{onText: func(content string) {
			// The review prompt requires one JSON object. Persisting the returned
			// text makes the dedicated orchestration conversation useful while the
			// review is running without exposing its prompt or diff.
			s.postOrchestrationMessage(context.Background(), job, content)
		}}
		s.postOrchestrationMessage(ctx, job, fmt.Sprintf("开始独立审查（第 %d/%d 次）。", attempt, maxIndependentReviewAttempts))
		if err := runner.Run(reviewCtx, AgentRunRequest{
			SessionID:      uuid.NewString(),
			ProjectPath:    worktree,
			Prompt:         prompt,
			PermissionMode: reviewPolicy,
			RunID:          uuid.NewString(),
			AgentID:        cfg.AgentID,
			Profile:        profile,
			SkipSessionID:  true,
			ReadOnlyTools:  readOnlyTools,
			PromptViaStdin: len(readOnlyTools) > 0,
		}, sink); err != nil {
			if !errors.Is(err, context.Canceled) && !errors.Is(reviewCtx.Err(), context.Canceled) {
				_ = completeReview("failed", errorText(err))
			}
			s.postOrchestrationMessage(ctx, job, "独立审查失败："+errorText(err))
			return fmt.Errorf("independent review: %w", err)
		}
		review, reviewErr = parseOrchestrationReview(sink.String(), commit)
		if reviewErr == nil {
			break
		}
		s.postOrchestrationMessage(ctx, job, fmt.Sprintf("独立审查结果格式无效，将在同一提交上重试：%s", errorText(reviewErr)))
	}
	if reviewErr != nil {
		_ = completeReview("failed", errorText(reviewErr))
		return independentReviewProtocolError{cause: reviewErr}
	}
	payload, _ := json.Marshal(review)
	status := "passed"
	if review.Verdict != "pass" || len(review.BlockingFindings) > 0 || review.ReviewedCommit != commit {
		status = "failed"
	}
	if err := completeReview(status, string(payload)); err != nil {
		return err
	}
	if status != "passed" {
		s.postOrchestrationMessage(ctx, job, "独立审查未通过："+string(payload))
		return errors.New("independent review reported blocking findings")
	}
	s.postOrchestrationMessage(ctx, job, "独立审查通过。")
	return nil
}

// beginIndependentReviewIntent records the review before starting the agent.
// A restart can therefore resume the same committed review instead of making
// the checking phase appear idle or dispatching implementation again.
func (s *Server) beginIndependentReviewIntent(ctx context.Context, job OrchestrationJob, commit string) (string, error) {
	now := time.Now().UTC()
	intentID := uuid.NewString()
	result, err := s.db.ExecContext(ctx, `insert into task_execution_intents (id,job_id,phase,attempt,reviewed_sha,status,created_at,updated_at)
	select ?,?,?,?,?,'running',?,? where exists (select 1 from task_orchestration_jobs where id=? and lease_token=?)
	on conflict(job_id,phase,attempt) do update set reviewed_sha=excluded.reviewed_sha,status='running',updated_at=excluded.updated_at`, intentID, job.ID, "review", job.Attempt, commit, now, now, job.ID, job.LeaseToken)
	if err != nil {
		return "", err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return "", errors.New("orchestration lease is no longer current")
	}
	if err := s.db.QueryRowContext(ctx, `select id from task_execution_intents where job_id=? and phase='review' and attempt=?`, job.ID, job.Attempt).Scan(&intentID); err != nil {
		return "", err
	}
	_, err = s.db.ExecContext(ctx, `insert into verification_runs (id,job_id,phase,command,reviewed_sha,status,created_at)
	values (?,?,?,'independent-agent',?,'running',?) on conflict(id) do update set reviewed_sha=excluded.reviewed_sha,status='running',output='',completed_at=null`, intentID, job.ID, "review", commit, now)
	if err != nil {
		return "", err
	}
	return intentID, nil
}

// orchestrationRuntimeProfile uses the exact profile revision bound to the
// dedicated orchestration conversation. Review and implementation therefore
// use the same model, endpoint, and managed credential even if project defaults
// change while a job is running.
func (s *Server) orchestrationRuntimeProfile(ctx context.Context, project Project, job OrchestrationJob, agentID string) (*AgentRuntimeProfile, error) {
	conversationID, err := s.orchestrationConversationID(ctx, job.ID)
	if err != nil || conversationID == "" {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var revisionID string
	if err := tx.QueryRowContext(ctx, `select agent_profile_revision_id from conversations where id=?`, conversationID).Scan(&revisionID); err != nil {
		return nil, err
	}
	return s.runtimeProfileTx(ctx, tx, revisionID, project.Runner, agentID)
}

// orchestrationReviewReadOnlyTools restricts Claude reviews to inspection
// tools. Codex uses its native read-only sandbox and does not need this list.
func orchestrationReviewReadOnlyTools(agentID string) []string {
	if agentID != "claude-code" {
		return nil
	}
	return []string{"Read", "Glob", "Grep", "List", "ReadMultiToolInfo"}
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

func (s *Server) completeOrchestrationTaskBranch(ctx context.Context, repo string, job OrchestrationJob, worktree, commit string) error {
	if err := s.assertOrchestrationLease(ctx, job); err != nil {
		return err
	}
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `update git_task_records set integration_sha=?,updated_at=? where job_id=? and exists (select 1 from task_orchestration_jobs where id=? and lease_token=?)`, commit, now, job.ID, job.ID, job.LeaseToken)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return errors.New("orchestration lease is no longer current")
	}
	if err = s.advanceOrchestrationJob(ctx, job, orchestrationChecking, "awaiting_main"); err != nil {
		return err
	}
	_, _ = s.db.ExecContext(ctx, `update orchestration_outbox set status='completed',completed_at=? where job_id=? and status='pending'`, now, job.ID)
	// Retain the completed worktree for project-run acceptance. It is removed by
	// the explicit cleanup action after merge, or by an acknowledged unmerged
	// cleanup request.
	return nil
}

func (s *Server) retryOrchestrationJob(ctx context.Context, job OrchestrationJob, cfg OrchestrationConfig, cause error) {
	// Attempt includes the initial implementation. MaxFixRounds counts only
	// additional repair rounds after that initial attempt.
	if job.Attempt > cfg.MaxFixRounds {
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
	// The next queued attempt must dispatch a repair run. A committed SHA is
	// retained only for paused/protocol-error resumes, which re-run review.
	_, _ = s.db.ExecContext(ctx, `update git_task_records set task_commit_sha='',updated_at=? where job_id=?`, now, job.ID)
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
	result, err := tx.ExecContext(ctx, `update task_orchestration_jobs set status=?,last_error=?,updated_at=? where id=? and lease_token=? and status in ('queued','preparing','implementing','checking')`, orchestrationNeedsHuman, errorText(cause), now, job.ID, job.LeaseToken)
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
func (s *Server) runVerificationCommands(ctx context.Context, job OrchestrationJob, worktree, sha, phase string, commands []string) error {
	for _, command := range commands {
		if err := s.assertOrchestrationLease(ctx, job); err != nil {
			return err
		}
		started := time.Now().UTC()
		commandCtx, cancel := context.WithTimeout(ctx, 20*time.Minute)
		cmd := newVerificationCommand(commandCtx, command)
		cmd.Dir = worktree
		configureProcessGroup(cmd)
		output, err := cmd.CombinedOutput()
		cancel()
		outputText := strings.TrimSpace(decodeRunOutputLine(output, runtime.GOOS == "windows"))
		status := "passed"
		exitCode := 0
		if err != nil {
			status = "failed"
			exitCode = 1
		}
		result, recordErr := s.db.ExecContext(ctx, `insert into verification_runs (id,job_id,phase,command,reviewed_sha,status,exit_code,output,created_at,completed_at)
		select ?,?,?,?,?,?,?,?,?,? where exists (select 1 from task_orchestration_jobs where id=? and lease_token=?)`, uuid.NewString(), job.ID, phase, command, sha, status, exitCode, outputText, started, time.Now().UTC(), job.ID, job.LeaseToken)
		if recordErr != nil {
			return recordErr
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return errors.New("orchestration lease is no longer current")
		}
		if err != nil {
			s.postOrchestrationMessage(ctx, job, fmt.Sprintf("%s 验证失败：%s\n%s", phase, command, outputText))
			if outputText != "" {
				return fmt.Errorf("%s: %w: %s", command, err, outputText)
			}
			return fmt.Errorf("%s: %w", command, err)
		}
		s.postOrchestrationMessage(ctx, job, fmt.Sprintf("%s 验证通过：%s\n%s", phase, command, outputText))
	}
	return nil
}

func (s *Server) orchestrationTaskRecord(ctx context.Context, jobID string) (GitTaskRecord, error) {
	var record GitTaskRecord
	err := s.db.QueryRowContext(ctx, `select job_id,base_dev_sha,task_branch,worktree_path,task_commit_sha,integration_sha,conversation_id from git_task_records where job_id=?`, jobID).Scan(&record.JobID, &record.BaseDevSHA, &record.TaskBranch, &record.WorktreePath, &record.TaskCommitSHA, &record.IntegrationSHA, &record.ConversationID)
	return record, err
}

func (s *Server) removeOrchestrationGitResources(ctx context.Context, repo, worktree, branch string) error {
	if worktree != "" {
		if _, err := os.Stat(worktree); err == nil {
			if err := s.gitCommand(ctx, repo, "worktree", "remove", "--force", worktree); err != nil {
				return fmt.Errorf("clean task worktree: %w", err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if branch != "" {
		if err := s.gitCommand(ctx, repo, "worktree", "prune"); err != nil {
			return fmt.Errorf("prune task worktree: %w", err)
		}
		if err := s.gitCommand(ctx, repo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
			if err := s.gitCommand(ctx, repo, "branch", "-D", branch); err != nil {
				return fmt.Errorf("clean task branch: %w", err)
			}
		}
	}
	return nil
}

func automaticOrchestrationBranch(now time.Time, taskID string) string {
	shortID := taskID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	return "自动编排-" + now.Format("20060102") + "-" + shortID
}

// newVerificationCommand selects the shell available on the control server.
// Verification commands run in the task worktree, not inside an AI CLI, so a
// Windows server must not depend on a separately installed POSIX shell.
func newVerificationCommand(ctx context.Context, command string) *exec.Cmd {
	executable, args := verificationCommandArgs(runtime.GOOS, command)
	return exec.CommandContext(ctx, executable, args...)
}

func verificationCommandArgs(goos, command string) (string, []string) {
	if goos == "windows" {
		return "powershell.exe", []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-Command", command}
	}
	return "sh", []string{"-lc", command}
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
