package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const supervisionSourceBudget = 320 << 10

type supervisionSource struct {
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated,omitempty"`
}

type supervisionCheck struct {
	Baseline      bool                     `json:"baseline,omitempty"`
	ID            string                   `json:"id"`
	AcceptanceIDs []string                 `json:"acceptanceIds"`
	Program       string                   `json:"program"`
	Args          []string                 `json:"args"`
	Directory     string                   `json:"directory"`
	TimeoutMillis int64                    `json:"timeoutMillis"`
	Browser       *supervisionBrowserCheck `json:"browser,omitempty"`
}

type supervisionBrowserCheck struct {
	ServerProgram string                   `json:"serverProgram"`
	ServerArgs    []string                 `json:"serverArgs"`
	URL           string                   `json:"url"`
	Steps         []supervisionBrowserStep `json:"steps"`
}

type supervisionBrowserStep struct {
	Action   string `json:"action"`
	Selector string `json:"selector"`
	Value    string `json:"value"`
}

type supervisionWorkflowAction struct {
	Type                 string             `json:"type"`
	SessionID            string             `json:"sessionId"`
	TaskID               string             `json:"taskId,omitempty"`
	RequirementsRevision int64              `json:"requirementsRevision,omitempty"`
	PlanRevision         int64              `json:"planRevision,omitempty"`
	ContextID            string             `json:"contextId,omitempty"`
	Instruction          string             `json:"instruction,omitempty"`
	Paths                []string           `json:"paths,omitempty"`
	Checks               []supervisionCheck `json:"checks,omitempty"`
	Disposition          string             `json:"disposition,omitempty"`
	Condition            string             `json:"condition,omitempty"`
	Question             string             `json:"question,omitempty"`
	WorkItemID           string             `json:"workItemId,omitempty"`
	ManifestOffset       int                `json:"manifestOffset,omitempty"`
}

type supervisionRoleJob struct {
	ExecutionID          string          `json:"executionId"`
	TaskID               string          `json:"taskId"`
	Kind                 string          `json:"kind"`
	RequirementsRevision int64           `json:"requirementsRevision"`
	PlanRevision         int64           `json:"planRevision"`
	InputRevision        string          `json:"inputRevision"`
	OutputRevision       string          `json:"outputRevision"`
	Spec                 json.RawMessage `json:"spec"`
	State                string          `json:"state"`
	Result               json.RawMessage `json:"result"`
	Error                string          `json:"error"`
	CreatedAt            time.Time       `json:"createdAt"`
}

type supervisionEvidence struct {
	ID                   string          `json:"id"`
	TaskID               string          `json:"taskId"`
	ExecutionID          string          `json:"executionId"`
	Kind                 string          `json:"kind"`
	RevisionID           string          `json:"revisionId"`
	RequirementsRevision int64           `json:"requirementsRevision"`
	PlanRevision         int64           `json:"planRevision"`
	Passed               bool            `json:"passed"`
	Details              json.RawMessage `json:"details"`
	CreatedAt            time.Time       `json:"createdAt"`
}

type supervisionAmendment struct {
	ID        string    `json:"id"`
	TaskID    string    `json:"taskId"`
	Content   string    `json:"content"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"createdAt"`
}

func supervisionRoleTitle(role string) string {
	switch role {
	case "implementation":
		return "托管实施 Agent"
	case "review":
		return "独立审查 Agent"
	case "test":
		return "测试执行器"
	default:
		return "全局调度 Agent"
	}
}

func (s *Server) migrateSupervisionWorkflow(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `create table if not exists supervision_role_jobs (
	 execution_id text primary key references supervision_work_items(execution_id) on delete cascade,
	 session_id text not null references supervision_sessions(id) on delete cascade, task_id text not null,
	 kind text not null check(kind in ('implementation','verification','review')),
	 requirements_revision integer not null, plan_revision integer not null, input_revision text not null,
	 output_revision text not null default '', spec text not null,
	 state text not null check(state in ('queued','running','completed','failed','interrupted','unknown')),
	 result text not null default '{}', error text not null default '', created_at datetime not null,
	 foreign key(session_id,task_id) references supervision_tasks(session_id,task_id) on delete cascade
	);
	create index if not exists supervision_role_jobs_session on supervision_role_jobs(session_id,state);
	create table if not exists supervision_evidence (
	 id text primary key, session_id text not null references supervision_sessions(id) on delete cascade,
	 task_id text not null, execution_id text not null references supervision_role_jobs(execution_id) on delete cascade,
	 kind text not null, revision_id text not null, requirements_revision integer not null, plan_revision integer not null,
	 passed integer not null, details text not null, created_at datetime not null,
	 foreign key(session_id,task_id) references supervision_tasks(session_id,task_id) on delete cascade
	);
	create index if not exists supervision_evidence_task on supervision_evidence(session_id,task_id,created_at);
	create table if not exists supervision_source_requests (
	 session_id text primary key references supervision_sessions(id) on delete cascade, paths text not null
	);
	create table if not exists supervision_mutations (
	 execution_id text primary key references supervision_role_jobs(execution_id) on delete cascade,
	 session_id text not null references supervision_sessions(id) on delete cascade,
	 state text not null check(state in ('applying','completed','unknown')), edits text not null, created_at datetime not null
	);
	create table if not exists supervision_amendments (
	 id text primary key, session_id text not null references supervision_sessions(id) on delete cascade,
	 task_id text not null default '', content text not null, state text not null check(state in ('pending','applied')),
	 created_at datetime not null
	);
	create table if not exists supervision_retry_state (
	 session_id text primary key references supervision_sessions(id) on delete cascade,
	 count integer not null default 0, next_at datetime, error text not null default ''
	);
	create table if not exists supervision_questions (
	 id text primary key, session_id text not null references supervision_sessions(id) on delete cascade,
	 task_id text not null default '', question text not null, answer text not null default '', created_at datetime not null, answered_at datetime
	 );
	 create table if not exists supervision_dependency_state (
	 session_id text primary key references supervision_sessions(id) on delete cascade,
	 fingerprint text not null, facts text not null, changed_at datetime not null
	 );
	 create table if not exists supervision_context_window (
	 session_id text primary key references supervision_sessions(id) on delete cascade,
	 task_id text not null default '', manifest_offset integer not null default 0
	 );
	 create table if not exists supervision_context_reads (
	 session_id text not null references supervision_sessions(id) on delete cascade,
	 fingerprint text not null, resource text not null,
	 primary key(session_id,fingerprint,resource)
	 );
	 create table if not exists supervision_dependency_cutoff (
	 session_id text primary key references supervision_sessions(id) on delete cascade,
	 evidence_rowid integer not null default 0
	 );
	 create table if not exists supervision_budget_deferred (
	 session_id text not null references supervision_sessions(id) on delete cascade,
	 task_id text not null, primary key(session_id,task_id)
	 );
	 create table if not exists supervision_wait_state (
	 session_id text primary key references supervision_sessions(id) on delete cascade,
	 expected_state_version integer not null, condition text not null, next_check_at datetime not null,
	 fingerprint text not null, rounds integer not null
	 );
	 create table if not exists supervision_observation_progress (
	 session_id text primary key references supervision_sessions(id) on delete cascade,
	 fingerprint text not null, count integer not null default 0
	 );
	 create table if not exists supervision_artifacts (
	 execution_id text not null references supervision_role_jobs(execution_id) on delete cascade,
	 session_id text not null references supervision_sessions(id) on delete cascade,
	 asset text not null, size integer not null, sha256 text not null, state text not null check(state in ('present','cleaned')),
	 primary key(execution_id,asset)
	 );
	 create table if not exists supervision_role_observations (
	 execution_id text primary key references supervision_role_jobs(execution_id) on delete cascade,
	 session_id text not null references supervision_sessions(id) on delete cascade,
	 expected_revision text not null,
	 state text not null check(state in ('pending','completed','abandoned')),
	 created_at datetime not null
	 );`)
	if err != nil {
		return err
	}
	if err := ensureColumn(ctx, s.db, "supervision_role_jobs", "review_fingerprint", "text not null default ''"); err != nil {
		return err
	}
	if err := ensureColumn(ctx, s.db, "supervision_role_jobs", "completion_input_sha256", "text not null default ''"); err != nil {
		return err
	}
	if err := ensureColumn(ctx, s.db, "supervision_questions", "requirements_snapshot", "text not null default ''"); err != nil {
		return err
	}
	return ensureColumn(ctx, s.db, "supervision_questions", "expired", "integer not null default 0")
}

func loadSupervisionRoleJob(ctx context.Context, q sqlQueryer, executionID string) (supervisionRoleJob, error) {
	var j supervisionRoleJob
	var spec, result string
	err := q.QueryRowContext(ctx, `select execution_id,task_id,kind,requirements_revision,plan_revision,input_revision,output_revision,spec,state,result,error,created_at from supervision_role_jobs where execution_id=?`, executionID).Scan(&j.ExecutionID, &j.TaskID, &j.Kind, &j.RequirementsRevision, &j.PlanRevision, &j.InputRevision, &j.OutputRevision, &spec, &j.State, &result, &j.Error, &j.CreatedAt)
	j.Spec, j.Result = json.RawMessage(spec), json.RawMessage(result)
	return j, err
}

func supervisionEvidenceTx(ctx context.Context, tx *sql.Tx, sessionID string, taskIDs ...string) ([]supervisionEvidence, error) {
	taskID := ""
	if len(taskIDs) > 0 {
		taskID = taskIDs[0]
	}
	rows, err := tx.QueryContext(ctx, `select id,task_id,execution_id,kind,revision_id,requirements_revision,plan_revision,passed,details,created_at from supervision_evidence where session_id=? and (?='' or task_id=?) order by rowid desc limit 100`, sessionID, taskID, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []supervisionEvidence{}
	for rows.Next() {
		var e supervisionEvidence
		var raw string
		if err := rows.Scan(&e.ID, &e.TaskID, &e.ExecutionID, &e.Kind, &e.RevisionID, &e.RequirementsRevision, &e.PlanRevision, &e.Passed, &raw, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Details = json.RawMessage(raw)
		out = append(out, e)
	}
	return out, rows.Err()
}

func supervisionSourcesTx(ctx context.Context, tx *sql.Tx, pc supervisionProjectContext, paths []string, limit int) ([]supervisionSource, error) {
	out := []supervisionSource{}
	requested := map[string]bool{}
	for _, p := range paths {
		requested[p] = true
	}
	used := 0
	for _, e := range pc.Manifest.Entries {
		if e.Kind != "file" || e.Gap != "" {
			continue
		}
		if len(paths) > 0 && !requested[e.Path] {
			continue
		}
		if e.Size > int64(limit-used) {
			if requested[e.Path] {
				out = append(out, supervisionSource{Path: e.Path, SHA256: e.SHA256, Truncated: true})
			}
			continue
		}
		var data []byte
		if err := tx.QueryRowContext(ctx, `select content from supervision_revision_blobs where sha256=?`, e.SHA256).Scan(&data); err != nil {
			return nil, err
		}
		if supervisionSHA256(data) != e.SHA256 {
			return nil, errors.New("源码正文校验失败")
		}
		out = append(out, supervisionSource{Path: e.Path, SHA256: e.SHA256, Content: string(data)})
		used += len(data)
	}
	return out, nil
}

func supervisionTaskPlanTx(ctx context.Context, tx *sql.Tx, sessionID, taskID string) (supervisionPlanVersion, error) {
	plans, err := loadSupervisionPlansTx(ctx, tx, sessionID)
	if err != nil {
		return supervisionPlanVersion{}, err
	}
	for _, p := range plans {
		if p.TaskID == taskID {
			return p, nil
		}
	}
	return supervisionPlanVersion{}, sql.ErrNoRows
}

func invalidSupervisionWorkflow(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errSupervisionPlanInvalid, fmt.Sprintf(format, args...))
}

func supervisionTextValid(text string, max int) bool {
	return strings.TrimSpace(text) != "" && len(text) <= max
}
