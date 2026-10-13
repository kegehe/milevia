package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const handoffSummaryPrompt = `Create a concise context handoff for a new Codex session. Do not use tools and do not modify files. Cover: current goal, completed work, remaining work, important decisions, constraints, changed files, validation, and unresolved risks. Use plain Markdown and stay under 6000 characters.`

const handoffInitializePrefix = "This is a context handoff from a previous Codex session. Read it as working context. Do not use tools and do not modify files. Reply with a short acknowledgement only.\n\n"

type conversationHandoff struct {
	ID                   string     `json:"id"`
	SourceConversationID string     `json:"sourceConversationId"`
	TargetConversationID string     `json:"targetConversationId,omitempty"`
	Status               string     `json:"status"`
	ErrorMessage         string     `json:"errorMessage,omitempty"`
	CreatedAt            time.Time  `json:"createdAt"`
	CompletedAt          *time.Time `json:"completedAt,omitempty"`
}

func (s *Server) startConversationHandoff(w http.ResponseWriter, r *http.Request) {
	conversationID := strings.TrimSpace(chi.URLParam(r, "conversationID"))
	if conversationID == "" {
		writeError(w, http.StatusBadRequest, errors.New("conversationID is required"))
		return
	}
	// Reserve the worker lifetime before touching the database. Close sets
	// closing under the same lock before it waits on runWG, which prevents a
	// successful request from starting a handoff after the database is closed.
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		writeError(w, http.StatusServiceUnavailable, errors.New("control service is shutting down"))
		return
	}
	s.runWG.Add(1)
	s.mu.Unlock()
	workerStarted := false
	defer func() {
		if !workerStarted {
			s.runWG.Done()
		}
	}()
	if s.isOrchestrationConversation(r.Context(), conversationID) {
		writeError(w, http.StatusConflict, errors.New("automatic orchestration conversations are read-only"))
		return
	}

	s.projectLifecycleMu.Lock()
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		s.projectLifecycleMu.Unlock()
		writeError(w, 500, err)
		return
	}
	defer tx.Rollback()
	var agentID, status string
	if err = tx.QueryRowContext(r.Context(), `select agent_id,status from conversations where id=?`, conversationID).Scan(&agentID, &status); err != nil {
		s.projectLifecycleMu.Unlock()
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, 404, errors.New("conversation not found"))
		} else {
			writeError(w, 500, err)
		}
		return
	}
	if agentID != "codex" {
		s.projectLifecycleMu.Unlock()
		writeError(w, 409, errors.New("context handoff is only supported for Codex conversations"))
		return
	}
	if status != "idle" {
		s.projectLifecycleMu.Unlock()
		writeError(w, 409, errors.New("stop the active agent run before handing off context"))
		return
	}
	var active int
	if err = tx.QueryRowContext(r.Context(), `select count(*) from conversation_handoffs where source_conversation_id=? and status in ('summarizing','initializing')`, conversationID).Scan(&active); err != nil {
		s.projectLifecycleMu.Unlock()
		writeError(w, 500, err)
		return
	}
	if active != 0 {
		s.projectLifecycleMu.Unlock()
		writeError(w, 409, errors.New("a context handoff is already in progress"))
		return
	}
	handoff := conversationHandoff{ID: uuid.NewString(), SourceConversationID: conversationID, Status: "summarizing", CreatedAt: time.Now().UTC()}
	if _, err = tx.ExecContext(r.Context(), `insert into conversation_handoffs (id,source_conversation_id,status,created_at) values (?,?,?,?)`, handoff.ID, handoff.SourceConversationID, handoff.Status, handoff.CreatedAt); err != nil {
		s.projectLifecycleMu.Unlock()
		writeError(w, 500, err)
		return
	}
	if err = tx.Commit(); err != nil {
		s.projectLifecycleMu.Unlock()
		writeError(w, 500, err)
		return
	}
	s.projectLifecycleMu.Unlock()
	workerStarted = true
	go func() {
		defer s.runWG.Done()
		s.runConversationHandoff(handoff.ID)
	}()
	writeJSON(w, http.StatusAccepted, handoff)
}

func (s *Server) getConversationHandoff(w http.ResponseWriter, r *http.Request) {
	handoff, err := s.conversationHandoff(r.Context(), chi.URLParam(r, "handoffID"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, 404, errors.New("context handoff not found"))
		return
	}
	if err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, http.StatusOK, handoff)
}

func (s *Server) conversationHandoff(ctx context.Context, id string) (conversationHandoff, error) {
	var item conversationHandoff
	err := s.db.QueryRowContext(ctx, `select id,source_conversation_id,coalesce(target_conversation_id,''),status,error_message,created_at,completed_at from conversation_handoffs where id=?`, id).Scan(&item.ID, &item.SourceConversationID, &item.TargetConversationID, &item.Status, &item.ErrorMessage, &item.CreatedAt, &item.CompletedAt)
	return item, err
}

// conversationHandoffInProgress is called while projectLifecycleMu is held by
// lifecycle writers. It keeps an in-progress transfer stable even when another
// client bypasses the conversation page's disabled controls.
func (s *Server) conversationHandoffInProgress(ctx context.Context, conversationID string) (bool, error) {
	var active bool
	err := s.db.QueryRowContext(ctx, `select exists(select 1 from conversation_handoffs where source_conversation_id=? and status in ('summarizing','initializing'))`, conversationID).Scan(&active)
	return active, err
}

func (s *Server) runConversationHandoff(id string) {
	ctx := s.runtimeCtx
	handoff, err := s.conversationHandoff(ctx, id)
	if err != nil {
		return
	}
	_, runID, _, status, err := s.startMessage(ctx, handoff.SourceConversationID, handoffSummaryPrompt, "", &runStartRecord{Handoff: &conversationHandoffRun{ID: id}})
	if err != nil {
		s.failConversationHandoff(id, errorText(err))
		return
	}
	if status >= 300 {
		s.failConversationHandoff(id, fmt.Sprintf("无法启动上下文摘要任务（HTTP %d）", status))
		return
	}
	if _, err = s.db.ExecContext(ctx, `update conversation_handoffs set source_run_id=? where id=? and status='summarizing'`, runID, id); err != nil {
		s.failConversationHandoff(id, err.Error())
		return
	}
	if err = s.waitForHandoffRun(ctx, runID); err != nil {
		s.failConversationHandoff(id, err.Error())
		return
	}
	var summary string
	err = s.db.QueryRowContext(ctx, `select content from messages where run_id=? and role='assistant' and parent_tool_use_id='' order by created_at desc,id desc limit 1`, runID).Scan(&summary)
	summary = strings.TrimSpace(summary)
	if err != nil || summary == "" || len(summary) > 8000 {
		s.failConversationHandoff(id, "无法生成有效的上下文交接摘要")
		return
	}
	target, err := s.createHandoffConversation(ctx, id, handoff.SourceConversationID, summary)
	if err != nil {
		s.failConversationHandoff(id, err.Error())
		return
	}
	_, targetRunID, _, status, err := s.startMessage(ctx, target.ID, handoffInitializePrefix+summary, "", &runStartRecord{Handoff: &conversationHandoffRun{ID: id}})
	if err != nil {
		s.failConversationHandoff(id, errorText(err))
		return
	}
	if status >= 300 {
		s.failConversationHandoff(id, fmt.Sprintf("无法启动新会话初始化任务（HTTP %d）", status))
		return
	}
	if _, err = s.db.ExecContext(ctx, `update conversation_handoffs set target_run_id=? where id=? and status='initializing'`, targetRunID, id); err != nil {
		s.failConversationHandoff(id, err.Error())
		return
	}
	if err = s.waitForHandoffRun(ctx, targetRunID); err != nil {
		s.failConversationHandoff(id, err.Error())
		return
	}
	if err = s.completeConversationHandoff(ctx, id, handoff.SourceConversationID, target.ID); err != nil {
		s.failConversationHandoff(id, err.Error())
	}
}

func (s *Server) waitForHandoffRun(ctx context.Context, runID string) error {
	deadline := time.NewTimer(3 * time.Minute)
	defer deadline.Stop()
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	for {
		var status string
		err := s.db.QueryRowContext(ctx, `select status from runs where id=?`, runID).Scan(&status)
		if err != nil {
			return err
		}
		if status == "completed" {
			return nil
		}
		if status == "failed" || status == "stopped" || status == "interrupted" {
			return fmt.Errorf("交接任务%s", status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			// A timed-out handoff must not leave an unseen Codex process running.
			// finishRun makes the conversation idle once cancellation completes.
			_, _, _ = s.stopRunByID(context.Background(), runID, true)
			return errors.New("上下文交接超时")
		case <-tick.C:
		}
	}
}

func (s *Server) createHandoffConversation(ctx context.Context, handoffID, sourceID, summary string) (Conversation, error) {
	s.projectLifecycleMu.Lock()
	defer s.projectLifecycleMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Conversation{}, err
	}
	defer tx.Rollback()
	var source Conversation
	var workspaceID, projectRunner string
	err = tx.QueryRowContext(ctx, `select c.id,c.project_id,c.claude_session_id,c.agent_id,c.agent_session_id,c.agent_runtime_id,c.agent_profile_revision_id,c.project_agent_route_revision_id,c.execution_policy,c.status,c.permission_mode,c.model_override,c.title,c.last_activity_at,c.claude_initialized,c.agent_initialized,c.is_current,c.created_at,c.active_workspace_id,coalesce(nullif(p.runner_id,''),p.runner) from conversations c join projects p on p.id=c.project_id where c.id=?`, sourceID).Scan(&source.ID, &source.ProjectID, &source.ClaudeSessionID, &source.AgentID, &source.AgentSessionID, &source.AgentRuntimeID, &source.AgentProfileRevisionID, &source.ProjectAgentRouteRevisionID, &source.ExecutionPolicy, &source.Status, &source.PermissionMode, &source.ModelOverride, &source.Title, &source.LastActivityAt, &source.ClaudeInitialized, &source.AgentInitialized, &source.IsCurrent, &source.CreatedAt, &workspaceID, &projectRunner)
	if err != nil {
		return Conversation{}, err
	}
	if source.Status != "idle" || source.AgentID != "codex" {
		return Conversation{}, errors.New("source conversation changed before handoff")
	}
	now := time.Now().UTC()
	sessionID := uuid.NewString()
	target := Conversation{ID: uuid.NewString(), ProjectID: source.ProjectID, ClaudeSessionID: sessionID, AgentID: source.AgentID, AgentSessionID: sessionID, AgentRuntimeID: projectRunner, AgentProfileRevisionID: source.AgentProfileRevisionID, ProjectAgentRouteRevisionID: source.ProjectAgentRouteRevisionID, ExecutionPolicy: source.executionPolicy(), Status: "idle", PermissionMode: source.PermissionMode, ModelOverride: source.ModelOverride, Title: "上下文交接未完成", LastActivityAt: now, CreatedAt: now}
	if _, err = tx.ExecContext(ctx, `insert into conversations (id,project_id,claude_session_id,agent_id,agent_session_id,agent_runtime_id,agent_profile_revision_id,project_agent_route_revision_id,execution_policy,status,permission_mode,model_override,title,last_activity_at,claude_initialized,agent_initialized,is_current,created_at) values (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, target.ID, target.ProjectID, target.ClaudeSessionID, target.AgentID, target.AgentSessionID, target.AgentRuntimeID, target.AgentProfileRevisionID, target.ProjectAgentRouteRevisionID, target.ExecutionPolicy, target.Status, target.PermissionMode, target.ModelOverride, target.Title, target.LastActivityAt, 0, 0, 0, target.CreatedAt); err != nil {
		return Conversation{}, err
	}
	if err = ensureConversationWorkspaceTx(ctx, tx, target.ID, target.ProjectID, now, workspaceID); err != nil {
		return Conversation{}, err
	}
	result, err := tx.ExecContext(ctx, `update conversation_handoffs set target_conversation_id=?,summary=?,status='initializing' where id=? and status='summarizing'`, target.ID, summary, handoffID)
	if err != nil {
		return Conversation{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return Conversation{}, err
	}
	if changed != 1 {
		return Conversation{}, errors.New("context handoff changed before initialization")
	}
	if err = tx.Commit(); err != nil {
		return Conversation{}, err
	}
	return target, nil
}

func (s *Server) completeConversationHandoff(ctx context.Context, handoffID, sourceID, targetID string) error {
	s.projectLifecycleMu.Lock()
	defer s.projectLifecycleMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	var sourceCurrent bool
	if err = tx.QueryRowContext(ctx, `select status,is_current from conversations where id=?`, sourceID).Scan(&status, &sourceCurrent); err != nil {
		return err
	}
	if status != "idle" {
		return errors.New("source conversation changed before handoff completed")
	}
	if err = tx.QueryRowContext(ctx, `select status from conversations where id=?`, targetID).Scan(&status); err != nil {
		return err
	}
	if status != "idle" {
		return errors.New("new conversation did not finish initialization")
	}
	result, err := tx.ExecContext(ctx, `update conversations set is_current=0,status='archived' where id=? and status='idle'`, sourceID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return errors.New("source conversation changed before handoff completed")
	}
	// A user may have selected another tab while the summary was generated.
	// Only replace the current marker when the source is still that selection.
	if sourceCurrent {
		if _, err = tx.ExecContext(ctx, `update conversations set is_current=1,title='上下文交接' where id=? and status='idle'`, targetID); err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	result, err = tx.ExecContext(ctx, `update conversation_handoffs set status='completed',completed_at=? where id=? and status='initializing'`, now, handoffID)
	if err != nil {
		return err
	}
	changed, err = result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return errors.New("context handoff changed before completion")
	}
	return tx.Commit()
}

func (s *Server) failConversationHandoff(id, message string) {
	message = strings.TrimSpace(message)
	if message == "" {
		message = "上下文交接失败"
	}
	// runtimeCtx is cancelled before Close waits for tracked handoff workers.
	// Use an independent, bounded context so the terminal failure remains
	// visible while the database is still open.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = s.db.ExecContext(ctx, `update conversation_handoffs set status='failed',error_message=?,completed_at=? where id=? and status in ('summarizing','initializing')`, message, time.Now().UTC(), id)
}

// A process restart has already interrupted the associated Codex run. Keep the
// durable record visible to the client instead of leaving the operation stuck
// in a non-terminal state that blocks every later handoff from the source.
func (s *Server) recoverInterruptedConversationHandoffs(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `update conversation_handoffs set status='failed',error_message='控制服务重启导致上下文交接中断，请从原会话重新发起。',completed_at=? where status in ('summarizing','initializing')`, time.Now().UTC())
	return err
}

// The first handoff schema accidentally made the target foreign key NOT NULL
// with an empty-string default. SQLite validates that empty string as a real
// foreign key value, so a newly-created handoff could never be inserted.
// Rebuild only databases created by that short-lived schema.
func (s *Server) migrateConversationHandoffs(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `pragma table_info(conversation_handoffs)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	badTargetColumn := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			return err
		}
		if name == "target_conversation_id" && notNull != 0 {
			badTargetColumn = true
		}
	}
	if err := rows.Err(); err != nil || !badTargetColumn {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `drop index if exists conversation_handoffs_source_status;
create table conversation_handoffs_rebuilt (id text primary key, source_conversation_id text not null references conversations(id) on delete cascade, target_conversation_id text references conversations(id) on delete set null, source_run_id text not null default '', target_run_id text not null default '', status text not null check(status in ('summarizing','initializing','completed','failed')), summary text not null default '', error_message text not null default '', created_at datetime not null, completed_at datetime);
insert into conversation_handoffs_rebuilt (id,source_conversation_id,target_conversation_id,source_run_id,target_run_id,status,summary,error_message,created_at,completed_at) select id,source_conversation_id,nullif(target_conversation_id,''),source_run_id,target_run_id,status,summary,error_message,created_at,completed_at from conversation_handoffs;
drop table conversation_handoffs;
alter table conversation_handoffs_rebuilt rename to conversation_handoffs;
create index conversation_handoffs_source_status on conversation_handoffs(source_conversation_id,status);`); err != nil {
		return err
	}
	return tx.Commit()
}
