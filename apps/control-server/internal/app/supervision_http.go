package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

type supervisionCapability struct {
	AgentID          string   `json:"agentId"`
	Name             string   `json:"name"`
	State            string   `json:"state"`
	Available        bool     `json:"available"`
	Reason           string   `json:"reason,omitempty"`
	Version          string   `json:"version,omitempty"`
	SupportsProfiles bool     `json:"supportsProfiles"`
	ReasoningEfforts []string `json:"reasoningEfforts"`
}

func (s *Server) registerSupervisionRoutes(r chi.Router) {
	r.Get("/api/projects/{projectID}/workspace-operations", s.workspaceGuardOperationsHTTP)
	r.Post("/api/projects/{projectID}/workspace-operations/{operationID}/reconcile", s.reconcileWorkspaceGuardOperationHTTP)
	r.Get("/api/runners/{runnerID}/supervision-profiles", s.supervisionProfileHandler(s.listAgentProfiles))
	r.Post("/api/runners/{runnerID}/supervision-profiles", s.createSupervisionProfileHTTP)
	r.Patch("/api/supervision-profiles/{profileID}", s.supervisionProfileHandler(s.updateAgentProfile))
	r.Post("/api/supervision-profiles/{profileID}/enable", s.supervisionProfileHandler(s.enableAgentProfile))
	r.Post("/api/supervision-profiles/{profileID}/disable", s.supervisionProfileHandler(s.disableAgentProfile))
	r.Post("/api/supervision-profile-revisions/{revisionID}/revoke", s.supervisionProfileHandler(s.revokeAgentProfileRevision))
	r.Get("/api/projects/{projectID}/supervision/capabilities", s.supervisionCapabilitiesHTTP)
	r.Get("/api/projects/{projectID}/supervision", s.listSupervisionHTTP)
	r.With(s.supervisionRequestMiddleware).Post("/api/projects/{projectID}/supervision", s.createSupervisionHTTP)
	r.Get("/api/supervision/{supervisionID}", s.supervisionDetailHTTP)
	r.With(s.supervisionRequestMiddleware).Post("/api/supervision/{supervisionID}/start", s.startSupervisionHTTP)
	r.With(s.supervisionRequestMiddleware).Post("/api/supervision/{supervisionID}/control", s.supervisionControlHTTP)
	r.With(s.supervisionRequestMiddleware).Put("/api/supervision/{supervisionID}/budget", s.supervisionBudgetHTTP)
	r.With(s.supervisionRequestMiddleware).Put("/api/supervision/{supervisionID}/roles", s.supervisionRolesHTTP)
	r.With(s.supervisionRequestMiddleware).Put("/api/supervision/{supervisionID}/archive", s.supervisionArchiveHTTP)
	r.With(s.supervisionRequestMiddleware).Post("/api/supervision/{supervisionID}/instructions", s.supervisionInstructionHTTP)
	r.With(s.supervisionRequestMiddleware).Post("/api/supervision/{supervisionID}/questions/{questionID}/answer", s.supervisionAnswerHTTP)
	r.With(s.supervisionRequestMiddleware).Post("/api/supervision/{supervisionID}/manual-evidence", s.supervisionManualEvidenceHTTP)
	r.Get("/api/supervision/{supervisionID}/process", s.supervisionProcessHTTP)
	r.Get("/api/supervision/{supervisionID}/source", s.supervisionSourceHTTP)
	r.Get("/api/supervision/{supervisionID}/changes", s.supervisionChangesHTTP)
	r.Get("/api/supervision/{supervisionID}/artifacts/{executionID}/{asset}", s.supervisionArtifactHTTP)
	r.Delete("/api/supervision/{supervisionID}/artifacts", s.cleanSupervisionArtifactsHTTP)
}

func (s *Server) supervisionCapabilityForProject(ctx context.Context, project Project, agent string, probe bool) supervisionCapability {
	entry, known := agentByID(agent)
	c := supervisionCapability{AgentID: agent, Name: entry.Name, State: "adapter_pending", SupportsProfiles: entry.SupportsProfiles, ReasoningEfforts: supervisionReasoningOptions(agent)}
	if !known {
		c.State, c.Reason = "unknown_cli", "该 CLI 不在现有目录中"
		return c
	}
	target := s.supervisionProjectTarget(project)
	if target.State != "ready" {
		c.State, c.Reason = target.State, target.Reason
		return c
	}
	var info supervisionCLIInfo
	var err error
	if target.Local {
		var adapter supervisionCLIAdapter
		adapter, err = s.supervisionAdapter(agent)
		if err == nil {
			info, err = adapter.supervisionCLIInfo(ctx, probe)
		}
	} else {
		probeCtx, cancel := context.WithTimeout(ctx, 35*time.Second)
		defer cancel()
		var link *supervisionTargetLink
		link, err = s.connectSupervisionProjectTarget(probeCtx, project)
		if err == nil {
			var response targetexec.Response
			response, err = link.Call(probeCtx, targetexec.Request{ID: uuid.NewString(), Method: "capability", AgentID: agent, Parameters: mustJSON(map[string]bool{"probe": probe})})
			if err == nil && json.Unmarshal(response.Data, &info) != nil {
				err = targetexec.ErrProtocol
			}
			if err == nil && !supervisionTargetAbsolutePath(link.Artifact.OS, info.Path) {
				err = targetexec.ErrConflict
			}
		}
	}
	if err != nil {
		c.State, c.Reason = "capability_unavailable", redactAgentText(err.Error())
		if errors.Is(err, errSupervisionCLINotInstalled) {
			c.State = "not_installed"
		}
		return c
	}
	if info.AgentID != agent || info.Contract == "" || target.Local && !filepath.IsAbs(info.Path) || (probe && info.Version == "") {
		c.State, c.Reason = "capability_unavailable", "CLI 未返回与所选工具匹配的执行路径及协议"
		return c
	}
	if info.ValidationPending {
		c.Version, c.State, c.Reason = info.Version, "validation_pending", "受控适配代码已接入，真实账号下的权限及多任务验收尚未完成"
		return c
	}
	c.Version, c.Available, c.State = info.Version, true, "probe_required"
	if probe {
		c.State = "ready"
	}
	return c
}

func (s *Server) supervisionCapabilitiesHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	project, err := s.getProjectByID(r.Context(), chi.URLParam(r, "projectID"))
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	agents := make([]supervisionCapability, 0, len(agentCatalogEntries))
	for _, entry := range agentCatalog() {
		agents = append(agents, s.supervisionCapabilityForProject(r.Context(), project, entry.ID, false))
	}
	profiles := []map[string]string{}
	destination, destinationErr := s.supervisionProfileDestinationTx(r.Context(), s.db, project)
	if destinationErr != nil && !errors.Is(destinationErr, sql.ErrNoRows) {
		writeSupervisionHTTPError(w, destinationErr)
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `select p.id,p.name,p.agent_id,v.model from agent_profiles p join agent_profile_revisions v on v.id=p.current_revision_id left join supervision_profile_targets t on t.revision_id=v.id where p.runner_id=? and p.enabled=1 and v.state='active' and ((v.execution_mode='isolated' and ?=1) or (v.execution_mode='target_isolated' and t.target_id=?)) order by p.name,p.id`, s.supervisionProjectRoute(project).RunnerID, s.supervisionProjectRoute(project).local(), destination)
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	for rows.Next() {
		var id, name, agent, model string
		if err = rows.Scan(&id, &name, &agent, &model); err != nil {
			break
		}
		profiles = append(profiles, map[string]string{"id": id, "name": name, "agentId": agent, "model": model})
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	preferences, err := s.readAppPreferences(r.Context(), s.db.QueryRowContext)
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	target := s.supervisionProjectTarget(project)
	target.TargetID = destination
	writeJSON(w, http.StatusOK, map[string]any{"profiles": profiles, "agents": agents, "target": target, "defaultAgentId": preferences.DefaultAgentID, "defaultBudget": defaultSupervisionBudget(), "browser": "按需核对项目目标环境的Playwright、Chromium及独立测试服务", "maxTasks": 500})
}

func writeSupervisionHTTPError(w http.ResponseWriter, err error) {
	status := http.StatusConflict
	if errors.Is(err, sql.ErrNoRows) {
		status = http.StatusNotFound
	}
	if errors.Is(err, context.DeadlineExceeded) {
		status = http.StatusRequestTimeout
	}
	writeError(w, status, err)
}

func (s *Server) listSupervisionHTTP(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(), `select id from supervision_sessions where project_id=? and (archived_at is null or ?) order by created_at desc,id desc limit 100`, chi.URLParam(r, "projectID"), r.URL.Query().Get("includeArchived") == "1")
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			writeSupervisionHTTPError(w, err)
			return
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	out := []supervisionSession{}
	for _, id := range ids {
		ss, err := loadSupervisionSession(r.Context(), s.db, id)
		if err != nil {
			writeSupervisionHTTPError(w, err)
			return
		}
		out = append(out, ss)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createSupervisionHTTP(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Roles            supervisionRoleConfigs `json:"roles"`
		TaskIDs          []string               `json:"taskIds"`
		SchedulerAgentID string                 `json:"schedulerAgentId"`
		Budget           *supervisionBudget     `json:"budget"`
	}
	if !decode(w, r, &input) {
		return
	}
	budget := defaultSupervisionBudget()
	if input.Budget != nil {
		budget = *input.Budget
	}
	if input.SchedulerAgentID == "" {
		preferences, err := s.readAppPreferences(r.Context(), s.db.QueryRowContext)
		if err != nil {
			writeSupervisionHTTPError(w, err)
			return
		}
		input.SchedulerAgentID = preferences.DefaultAgentID
	}
	ss, err := s.createSupervisionDraft(r.Context(), chi.URLParam(r, "projectID"), input.TaskIDs, input.SchedulerAgentID, budget, input.Roles)
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, ss)
}

func (s *Server) startSupervisionHTTP(w http.ResponseWriter, r *http.Request) {
	var input struct {
		StateVersion int64 `json:"stateVersion"`
	}
	if !decode(w, r, &input) {
		return
	}
	id := chi.URLParam(r, "supervisionID")
	ss, err := loadSupervisionSession(r.Context(), s.db, id)
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	if ss.State != "draft" || ss.StateVersion != input.StateVersion {
		writeSupervisionHTTPError(w, errSupervisionState)
		return
	}
	project, err := s.getProjectByID(r.Context(), ss.ProjectID)
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	checked := map[string]bool{}
	for _, role := range []string{"scheduler", "implementation", "review"} {
		agentID := ss.roleConfig(role).AgentID
		if checked[agentID] {
			continue
		}
		checked[agentID] = true
		capability := s.supervisionCapabilityForProject(r.Context(), project, agentID, true)
		if !capability.Available {
			writeSupervisionHTTPError(w, errors.New(role+": "+capability.Reason))
			return
		}
	}
	if err := s.startSupervisionSession(r.Context(), id, input.StateVersion, time.Now().UTC()); err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	s.wakeSupervisionWorker(id)
	ss, err = loadSupervisionSession(r.Context(), s.db, id)
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, ss)
}

func (s *Server) supervisionControlHTTP(w http.ResponseWriter, r *http.Request) {
	var input struct {
		StateVersion int64  `json:"stateVersion"`
		Action       string `json:"action"`
		AcceptDrift  bool   `json:"acceptDrift"`
	}
	if !decode(w, r, &input) {
		return
	}
	id := chi.URLParam(r, "supervisionID")
	var err error
	if input.Action == "resume" {
		err = s.resumeSupervisionSession(r.Context(), id, input.StateVersion, input.AcceptDrift, false)
	} else {
		err = s.requestSupervisionControl(r.Context(), id, input.StateVersion, input.Action, time.Now().UTC())
		if err == nil {
			settleErr := s.settleSupervisionControl(r.Context(), id, time.Now().UTC())
			if !errors.Is(settleErr, errWorkspaceNotQuiescent) && !errors.Is(settleErr, errSupervisionState) {
				err = settleErr
			}
		}
	}
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	s.wakeSupervisionWorker(id)
	ss, err := loadSupervisionSession(r.Context(), s.db, id)
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, ss)
}

func (s *Server) supervisionBudgetHTTP(w http.ResponseWriter, r *http.Request) {
	var input struct {
		StateVersion int64             `json:"stateVersion"`
		Budget       supervisionBudget `json:"budget"`
	}
	if !decode(w, r, &input) {
		return
	}
	if err := s.updateSupervisionBudget(r.Context(), chi.URLParam(r, "supervisionID"), input.StateVersion, input.Budget, time.Now().UTC()); err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) supervisionInstructionHTTP(w http.ResponseWriter, r *http.Request) {
	var input struct {
		StateVersion int64  `json:"stateVersion"`
		TaskID       string `json:"taskId"`
		Content      string `json:"content"`
		Immediate    bool   `json:"immediate"`
	}
	if !decode(w, r, &input) {
		return
	}
	if !supervisionTextValid(input.Content, 16000) {
		writeError(w, http.StatusBadRequest, errors.New("补充要求不能为空或超过16000字节"))
		return
	}
	id := chi.URLParam(r, "supervisionID")
	err := s.withSupervisionRequestTx(r.Context(), id, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		if err := requireSupervisionVersion(ss, input.StateVersion); err != nil {
			return err
		}
		if supervisionTerminal(ss.State) || ss.Control == "stop" {
			return errSupervisionState
		}
		if input.TaskID != "" {
			var selected bool
			if err := tx.QueryRowContext(r.Context(), `select exists(select 1 from supervision_tasks where session_id=? and task_id=?)`, id, input.TaskID).Scan(&selected); err != nil {
				return err
			}
			if !selected {
				return errors.New("任务不属于本次托管")
			}
		}
		if _, err := tx.ExecContext(r.Context(), `insert into supervision_amendments(id,session_id,task_id,content,state,created_at) values (?,?,?,?,'pending',?)`, uuid.NewString(), id, input.TaskID, input.Content, time.Now().UTC()); err != nil {
			return err
		}
		if input.Immediate && ss.Control == "" && (ss.State == "running" || ss.State == "finalizing") {
			ss.State, ss.Control, ss.Reason = "pausing", "pause", "amendment_interrupt"
			ss.StateVersion++
		}
		return appendSupervisionEventTx(r.Context(), tx, ss, "instructions.received", map[string]any{"taskId": input.TaskID, "content": input.Content, "immediate": input.Immediate}, time.Now().UTC())
	})
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	s.wakeSupervisionWorker(id)
	writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
}

func (s *Server) supervisionAnswerHTTP(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Answer string `json:"answer"`
	}
	if !decode(w, r, &input) {
		return
	}
	if !supervisionTextValid(input.Answer, 16000) {
		writeError(w, http.StatusBadRequest, errors.New("回答不能为空或超限"))
		return
	}
	id := chi.URLParam(r, "supervisionID")
	err := s.withSupervisionRequestTx(r.Context(), id, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		var taskID, question, answer, snapshot string
		var expired bool
		var answered sql.NullTime
		if err := tx.QueryRowContext(r.Context(), `select task_id,question,answer,answered_at,requirements_snapshot,expired from supervision_questions where id=? and session_id=?`, chi.URLParam(r, "questionID"), id).Scan(&taskID, &question, &answer, &answered, &snapshot, &expired); err != nil {
			return err
		}
		if answered.Valid {
			if answer == input.Answer {
				return nil
			}
			return errors.New("问题已经回答，不能覆盖历史")
		}
		current, err := supervisionQuestionRequirementsTx(r.Context(), tx, id, taskID)
		if err != nil {
			return err
		}
		var pending bool
		if err := tx.QueryRowContext(r.Context(), `select exists(select 1 from supervision_amendments where session_id=? and state='pending' and (task_id='' or ?='' or task_id=?))`, id, taskID, taskID).Scan(&pending); err != nil {
			return err
		}
		stale := expired || pending || (snapshot != "" && snapshot != current) || supervisionTerminal(ss.State) || ss.Control == "stop"
		if stale {
			now := time.Now().UTC()
			if _, err := tx.ExecContext(r.Context(), `update supervision_questions set answer=?,answered_at=?,expired=1 where id=? and session_id=?`, input.Answer, now, chi.URLParam(r, "questionID"), id); err != nil {
				return err
			}
			return appendSupervisionEventTx(r.Context(), tx, ss, "question.late_answer", map[string]string{"questionId": chi.URLParam(r, "questionID"), "taskId": taskID, "answer": input.Answer}, now)
		}
		now := time.Now().UTC()
		if _, err := tx.ExecContext(r.Context(), `update supervision_questions set answer=?,answered_at=? where id=?;insert into supervision_amendments(id,session_id,task_id,content,state,created_at) values (?,?,?,?,'pending',?)`, input.Answer, now, chi.URLParam(r, "questionID"), uuid.NewString(), id, taskID, "问题："+question+"\n用户回答："+input.Answer, now); err != nil {
			return err
		}
		ss.StateVersion++
		if ss.Control == "" && ss.RecoveryState == "ready" && ss.State == "needs_input" && ss.Reason == "user_question" {
			ss.State, ss.Reason = "running", ""
			ss.ClockAt = &now
		}
		return appendSupervisionEventTx(r.Context(), tx, ss, "question.answered", map[string]string{"questionId": chi.URLParam(r, "questionID"), "taskId": taskID, "answer": input.Answer}, now)
	})
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	s.wakeSupervisionWorker(id)
	writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
}

func (s *Server) supervisionDetailHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "supervisionID")
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	defer tx.Rollback()
	ss, err := loadSupervisionSession(ctx, tx, id)
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	plans, err := loadSupervisionPlansTx(ctx, tx, id)
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	evidence, err := supervisionEvidenceTx(ctx, tx, id, r.URL.Query().Get("taskId"))
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	tasks := []map[string]any{}
	rows, err := tx.QueryContext(ctx, `select task_id,phase,requirements_revision,plan_revision,evidence_freshness,implementation_started,fix_rounds,task_snapshot from supervision_tasks where session_id=? order by position`, id)
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	for rows.Next() {
		var taskID, phase, freshness, raw string
		var req, plan, fixes int
		var started bool
		if err := rows.Scan(&taskID, &phase, &req, &plan, &freshness, &started, &fixes, &raw); err != nil {
			rows.Close()
			writeSupervisionHTTPError(w, err)
			return
		}
		tasks = append(tasks, map[string]any{"taskId": taskID, "phase": phase, "requirementsRevision": req, "planRevision": plan, "evidenceFreshness": freshness, "implementationStarted": started, "fixRounds": fixes, "taskSnapshot": json.RawMessage(raw)})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	jobs := []supervisionRoleJob{}
	rows, err = tx.QueryContext(ctx, `select execution_id from supervision_role_jobs where session_id=? order by created_at desc limit 100`, id)
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	var ids []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			rows.Close()
			writeSupervisionHTTPError(w, err)
			return
		}
		ids = append(ids, value)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	for _, execution := range ids {
		job, err := loadSupervisionRoleJob(ctx, tx, execution)
		if err != nil {
			writeSupervisionHTTPError(w, err)
			return
		}
		jobs = append(jobs, job)
	}
	questions := []map[string]any{}
	rows, err = tx.QueryContext(ctx, `select id,task_id,question,answer,created_at,answered_at,expired from supervision_questions where session_id=? order by created_at,id`, id)
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	for rows.Next() {
		var questionID, taskID, question, answer string
		var expired bool
		var created time.Time
		var answered sql.NullTime
		if err := rows.Scan(&questionID, &taskID, &question, &answer, &created, &answered, &expired); err != nil {
			rows.Close()
			writeSupervisionHTTPError(w, err)
			return
		}
		questions = append(questions, map[string]any{"id": questionID, "taskId": taskID, "question": question, "answer": answer, "createdAt": created, "answered": answered.Valid, "expired": expired})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	events := []supervisionEvent{}
	rows, err = tx.QueryContext(ctx, `select seq,type,payload,created_at from supervision_events where session_id=? order by seq desc limit 150`, id)
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	for rows.Next() {
		e := supervisionEvent{SessionID: id}
		var raw string
		if err := rows.Scan(&e.Seq, &e.Type, &raw, &e.CreatedAt); err != nil {
			rows.Close()
			writeSupervisionHTTPError(w, err)
			return
		}
		e.Payload = json.RawMessage(raw)
		events = append(events, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	pc, contextErr := loadSupervisionProjectContext(ctx, tx, id, "current")
	if contextErr != nil && !errors.Is(contextErr, sql.ErrNoRows) {
		writeSupervisionHTTPError(w, contextErr)
		return
	}
	artifacts := []map[string]any{}
	rows, err = tx.QueryContext(ctx, `select execution_id,asset,size,sha256,state from supervision_artifacts where session_id=? order by execution_id,asset`, id)
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	for rows.Next() {
		var execution, asset, hash, state string
		var size int64
		if err := rows.Scan(&execution, &asset, &size, &hash, &state); err != nil {
			rows.Close()
			writeSupervisionHTTPError(w, err)
			return
		}
		artifacts = append(artifacts, map[string]any{"executionId": execution, "asset": asset, "size": size, "sha256": hash, "state": state})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	stored, err := supervisionStoredBytes(r.Context(), tx, id)
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	diskFree, diskErr := supervisionDiskFree(s.checkpointStorageRoot())
	reserved, err := supervisionReservedBytes(r.Context(), tx, id)
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	wait, err := loadSupervisionWait(ctx, tx, ss)
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	protection, err := supervisionProtectionViewTx(ctx, tx, ss)
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	var projectContext any
	if contextErr == nil {
		projectContext = map[string]any{"id": pc.ID, "revisionId": pc.RevisionID, "rulesHash": pc.RulesHash, "rules": pc.Rules, "entries": len(pc.Manifest.Entries)}
	}
	storage := map[string]any{"reservedBytes": reserved, "bytes": stored, "limitBytes": ss.Budget.storageLimit(), "diskReserveBytes": supervisionDiskReserve, "accounting": "logical_payload"}
	if diskErr == nil {
		storage["diskFreeBytes"] = diskFree
	}
	writeJSON(w, http.StatusOK, map[string]any{"protection": protection, "wait": wait, "storage": storage, "session": ss, "tasks": tasks, "plans": plans, "jobs": jobs, "evidence": evidence, "questions": questions, "events": events, "projectContext": projectContext, "artifacts": artifacts, "evidenceTruncated": len(evidence) == 100, "jobsTruncated": len(jobs) == 100, "eventsTruncated": len(events) == 150 && events[len(events)-1].Seq > 1})
}

func (s *Server) supervisionProcessHTTP(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "supervisionID")
	if _, err := loadSupervisionSession(r.Context(), s.db, id); err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	afterMessage, _ := strconv.ParseInt(r.URL.Query().Get("afterMessage"), 10, 64)
	afterEvent, _ := strconv.ParseInt(r.URL.Query().Get("afterEvent"), 10, 64)
	if afterMessage < 0 || afterEvent < 0 {
		writeError(w, http.StatusBadRequest, errors.New("无效的过程游标"))
		return
	}
	messages := []map[string]any{}
	events := []map[string]any{}
	rows, err := s.db.QueryContext(r.Context(), `select m.rowid,m.id,m.run_id,m.role,m.content,m.created_at,l.role,l.task_id,l.conversation_id from messages m join supervision_run_links l on l.run_id=m.run_id where l.session_id=? and m.rowid>? order by m.rowid limit 100`, id, afterMessage)
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	for rows.Next() {
		var cursor int64
		var messageID, runID, role, content, agentRole, taskID, conversationID string
		var created time.Time
		if err := rows.Scan(&cursor, &messageID, &runID, &role, &content, &created, &agentRole, &taskID, &conversationID); err != nil {
			rows.Close()
			writeSupervisionHTTPError(w, err)
			return
		}
		messages = append(messages, map[string]any{"cursor": cursor, "id": messageID, "runId": runID, "role": role, "content": content, "createdAt": created, "agentRole": agentRole, "taskId": taskID, "conversationId": conversationID})
		afterMessage = cursor
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	rows, err = s.db.QueryContext(r.Context(), `select e.rowid,e.id,e.run_id,e.type,e.payload,e.created_at,l.role,l.task_id from events e join supervision_run_links l on l.run_id=e.run_id where l.session_id=? and e.rowid>? and e.type<>'assistant.message' order by e.rowid limit 200`, id, afterEvent)
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	for rows.Next() {
		var cursor int64
		var eventID, runID, kind, payload, role, taskID string
		var created time.Time
		if err := rows.Scan(&cursor, &eventID, &runID, &kind, &payload, &created, &role, &taskID); err != nil {
			rows.Close()
			writeSupervisionHTTPError(w, err)
			return
		}
		events = append(events, map[string]any{"cursor": cursor, "id": eventID, "runId": runID, "type": kind, "payload": json.RawMessage(payload), "createdAt": created, "agentRole": role, "taskId": taskID})
		afterEvent = cursor
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": messages, "events": events, "afterMessage": afterMessage, "afterEvent": afterEvent, "hasMore": len(messages) == 100 || len(events) == 200})
}

func (s *Server) supervisionSourceHTTP(w http.ResponseWriter, r *http.Request) {
	data, err := s.supervisionRevisionContent(r.Context(), chi.URLParam(r, "supervisionID"), r.URL.Query().Get("revisionId"), r.URL.Query().Get("path"))
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func (s *Server) supervisionArtifactHTTP(w http.ResponseWriter, r *http.Request) {
	asset := chi.URLParam(r, "asset")
	if asset != "screenshot.png" && asset != "report.json" {
		writeError(w, http.StatusNotFound, errors.New("产物不存在"))
		return
	}
	var authorized bool
	if err := s.db.QueryRowContext(r.Context(), `select exists(select 1 from supervision_role_jobs where execution_id=? and session_id=? and kind='verification')`, chi.URLParam(r, "executionID"), chi.URLParam(r, "supervisionID")).Scan(&authorized); err != nil || !authorized {
		writeError(w, http.StatusNotFound, errors.New("产物不存在"))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	var hash, state string
	if err := s.db.QueryRowContext(r.Context(), `select sha256,state from supervision_artifacts where execution_id=? and session_id=? and asset=?`, chi.URLParam(r, "executionID"), chi.URLParam(r, "supervisionID"), asset).Scan(&hash, &state); err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	if state == "cleaned" {
		writeError(w, http.StatusGone, errors.New("原始产物已清理，结构化结果与哈希仍保留"))
		return
	}
	data, err := os.ReadFile(filepath.Join(s.checkpointStorageRoot(), "supervision", "artifacts", chi.URLParam(r, "executionID"), asset))
	if err != nil || len(data) > 20<<20 || supervisionSHA256(data) != hash {
		writeError(w, http.StatusConflict, errors.New("原始产物缺失或完整性校验失败"))
		return
	}
	if asset == "screenshot.png" {
		w.Header().Set("Content-Type", "image/png")
	} else {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(data)
}

func (s *Server) supervisionManualEvidenceHTTP(w http.ResponseWriter, r *http.Request) {
	var input struct {
		StateVersion int64  `json:"stateVersion"`
		TaskID       string `json:"taskId"`
		PointID      string `json:"pointId"`
		Note         string `json:"note"`
		Passed       bool   `json:"passed"`
	}
	if !decode(w, r, &input) {
		return
	}
	if !supervisionTextValid(input.Note, 8000) {
		writeError(w, http.StatusBadRequest, errors.New("人工验证需要记录实际操作与结果"))
		return
	}
	id := chi.URLParam(r, "supervisionID")
	err := s.withSupervisionRequestTx(r.Context(), id, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		if err := requireSupervisionVersion(ss, input.StateVersion); err != nil {
			return err
		}
		if supervisionTerminal(ss.State) || ss.Control == "stop" {
			return errSupervisionState
		}
		if err := supervisionRunsQuiescentTx(r.Context(), tx, id); err != nil {
			return err
		}
		pc, err := loadSupervisionProjectContext(r.Context(), tx, id, "current")
		if err != nil {
			return err
		}
		p, err := supervisionTaskPlanTx(r.Context(), tx, id, input.TaskID)
		if err != nil {
			return err
		}
		valid := false
		for _, point := range p.Plan.Acceptance {
			if point.ID == input.PointID && point.Kind == "manual" {
				valid = true
			}
		}
		if !valid {
			return errors.New("只允许对当前计划的人工验收点提供人工记录")
		}
		var execution string
		if err := tx.QueryRowContext(r.Context(), `select execution_id from supervision_role_jobs where session_id=? and task_id=? order by created_at desc limit 1`, id, input.TaskID).Scan(&execution); err != nil {
			return err
		}
		raw := mustJSON(map[string]any{"pointId": input.PointID, "note": input.Note, "actor": "authenticated_user", "passed": input.Passed})
		if _, err := tx.ExecContext(r.Context(), `insert into supervision_evidence(id,session_id,task_id,execution_id,kind,revision_id,requirements_revision,plan_revision,passed,details,created_at) values (?,?,?,?,'manual',?,?,?,?,?,?)`, uuid.NewString(), id, input.TaskID, execution, pc.RevisionID, p.RequirementsRevision, p.Revision, input.Passed, string(raw), time.Now().UTC()); err != nil {
			return err
		}
		ss.StateVersion++
		return appendSupervisionEventTx(r.Context(), tx, ss, "evidence.manual", raw, time.Now().UTC())
	})
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
}

func (s *Server) supervisionChangesHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "supervisionID")
	baseline, err := loadSupervisionProjectContext(ctx, s.db, id, "baseline")
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	current, err := loadSupervisionProjectContext(ctx, s.db, id, "current")
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	before := map[string]supervisionSnapshotEntry{}
	after := map[string]supervisionSnapshotEntry{}
	for _, entry := range baseline.Manifest.Entries {
		if entry.Kind == "file" {
			before[entry.Path] = entry
		}
	}
	for _, entry := range current.Manifest.Entries {
		if entry.Kind == "file" {
			after[entry.Path] = entry
		}
	}
	paths := map[string]bool{}
	for name := range before {
		paths[name] = true
	}
	for name := range after {
		paths[name] = true
	}
	changes := []map[string]any{}
	for name := range paths {
		a, b := before[name], after[name]
		if a.SHA256 == b.SHA256 {
			continue
		}
		changes = append(changes, map[string]any{"path": name, "beforeSha256": a.SHA256, "afterSha256": b.SHA256, "readable": a.Gap == "" && b.Gap == "", "beforeSize": a.Size, "afterSize": b.Size})
	}
	writeJSON(w, http.StatusOK, map[string]any{"baselineRevisionId": baseline.RevisionID, "currentRevisionId": current.RevisionID, "changes": changes})
}
