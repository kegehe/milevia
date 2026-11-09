package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

const maxSupervisionSchedulerInputBytes = 1024 * 1024

// 调度器只接收已持久化资料，不把项目目录或其他任务的会话交给 CLI 自行探索。
// 项目上下文与计划必须来自持久版本；没有上下文的早期能力实验不能用于实际派发。
type supervisionSchedulerInput struct {
	SchemaVersion        int                             `json:"schemaVersion"`
	Decision             supervisionDecision             `json:"decision"`
	State                string                          `json:"state"`
	Budget               supervisionBudget               `json:"budget"`
	ActiveMillis         int64                           `json:"activeMillis"`
	UsedTurns            int                             `json:"usedTurns"`
	ReservedTurns        int                             `json:"reservedTurns"`
	Tasks                []supervisionTask               `json:"tasks"`
	RecentEvents         []supervisionEvent              `json:"recentEvents"`
	EventsTruncated      bool                            `json:"eventsTruncated"`
	Correction           *supervisionSchedulerCorrection `json:"correction,omitempty"`
	ProjectContext       *supervisionProjectContext      `json:"projectContext,omitempty"`
	Plans                []supervisionPlanVersion        `json:"plans"`
	Sources              []supervisionSource             `json:"sources"`
	Evidence             []supervisionEvidence           `json:"evidence"`
	RoleJobs             []supervisionRoleJob            `json:"roleJobs"`
	Amendments           []supervisionAmendment          `json:"amendments"`
	AvailableActions     []string                        `json:"availableActions"`
	ManifestOffset       int                             `json:"manifestOffset"`
	ManifestTotal        int                             `json:"manifestTotal"`
	ManifestTruncated    bool                            `json:"manifestTruncated"`
	RecordsTruncated     bool                            `json:"recordsTruncated"`
	ExternalDependencies json.RawMessage                 `json:"externalDependencies"`
}

type supervisionSchedulerCorrection struct {
	ExecutionID     string `json:"executionId"`
	Attempt         int    `json:"attempt"`
	Output          string `json:"output"`
	ValidationError string `json:"validationError"`
}

func saveSupervisionSchedulerInputTx(ctx context.Context, tx *sql.Tx, ss *supervisionSession, d supervisionDecision, now time.Time) error {
	input := supervisionSchedulerInput{SchemaVersion: 1, Decision: d, State: ss.State, Budget: ss.Budget,
		ActiveMillis: ss.ActiveMillis, UsedTurns: ss.UsedTurns, ReservedTurns: ss.ReservedTurns,
		Tasks: []supervisionTask{}, RecentEvents: []supervisionEvent{}}
	projectContext, err := loadSupervisionProjectContext(ctx, tx, ss.ID, "current")
	if err == nil {
		input.ProjectContext = &projectContext
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	input.Plans, err = loadSupervisionPlansTx(ctx, tx, ss.ID)
	if err != nil {
		return err
	}
	input.AvailableActions = []string{"read_context", "record_plan", "dispatch_implementation", "request_verification", "request_review", "record_acceptance", "record_task_disposition", "wait", "ask_user"}
	if input.ProjectContext != nil {
		var paths []string
		var rawPaths string
		if err := tx.QueryRowContext(ctx, `select paths from supervision_source_requests where session_id=?`, ss.ID).Scan(&rawPaths); err == nil {
			if err := json.Unmarshal([]byte(rawPaths), &paths); err != nil {
				return err
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		input.Sources, err = supervisionSourcesTx(ctx, tx, *input.ProjectContext, paths, supervisionSourceBudget)
		if err != nil {
			return err
		}
	}
	input.Evidence, err = supervisionEvidenceTx(ctx, tx, ss.ID)
	if err != nil {
		return err
	}
	for i := range input.Evidence {
		input.Evidence[i].Details = supervisionModelJSON(input.Evidence[i].Details)
	}
	var requestedTask string
	if err := tx.QueryRowContext(ctx, `select task_id,manifest_offset from supervision_context_window where session_id=?`, ss.ID).Scan(&requestedTask, &input.ManifestOffset); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if input.ProjectContext != nil {
		input.ManifestTotal = len(input.ProjectContext.Manifest.Entries)
		start := min(input.ManifestOffset, input.ManifestTotal)
		end := min(start+1000, input.ManifestTotal)
		input.ProjectContext.Manifest.Entries = input.ProjectContext.Manifest.Entries[start:end]
		input.ManifestTruncated = start > 0 || end < input.ManifestTotal
	}
	var external string
	if err := tx.QueryRowContext(ctx, `select facts from supervision_dependency_state where session_id=?`, ss.ID).Scan(&external); err == nil {
		input.ExternalDependencies = json.RawMessage(external)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	roleRows, err := tx.QueryContext(ctx, `select execution_id from supervision_role_jobs where session_id=? order by created_at desc limit 30`, ss.ID)
	if err != nil {
		return err
	}
	var executionIDs []string
	for roleRows.Next() {
		var id string
		if err := roleRows.Scan(&id); err != nil {
			roleRows.Close()
			return err
		}
		executionIDs = append(executionIDs, id)
	}
	err = roleRows.Err()
	roleRows.Close()
	if err != nil {
		return err
	}
	input.RoleJobs = []supervisionRoleJob{}
	for _, id := range executionIDs {
		j, err := loadSupervisionRoleJob(ctx, tx, id)
		if err != nil {
			return err
		}
		input.RoleJobs = append(input.RoleJobs, j)
		input.RoleJobs[len(input.RoleJobs)-1].Result = supervisionModelJSON(j.Result)
	}
	amendRows, err := tx.QueryContext(ctx, `select id,task_id,content,state,created_at from supervision_amendments where session_id=? order by created_at,id`, ss.ID)
	if err != nil {
		return err
	}
	input.Amendments = []supervisionAmendment{}
	for amendRows.Next() {
		var a supervisionAmendment
		if err := amendRows.Scan(&a.ID, &a.TaskID, &a.Content, &a.State, &a.CreatedAt); err != nil {
			amendRows.Close()
			return err
		}
		input.Amendments = append(input.Amendments, a)
	}
	err = amendRows.Err()
	amendRows.Close()
	if err != nil {
		return err
	}
	if d.Attempt > 1 {
		previous := &supervisionSchedulerCorrection{}
		if err := tx.QueryRowContext(ctx, `select execution_id,attempt,result_text,result_error from supervision_work_items where decision_id=? and session_id=? and attempt=? and state='quiescent' and result_state='invalid'`, d.ID, ss.ID, d.Attempt-1).Scan(&previous.ExecutionID, &previous.Attempt, &previous.Output, &previous.ValidationError); err != nil {
			return err
		}
		input.Correction = previous
	}
	rows, err := tx.QueryContext(ctx, `select task_id,position,phase,requirements_revision,task_snapshot,dependency_snapshot,plan_revision,evidence_freshness,implementation_started,fix_rounds from supervision_tasks where session_id=? order by position`, ss.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var task supervisionTask
		var snapshot, dependencies string
		if err := rows.Scan(&task.TaskID, &task.Position, &task.Phase, &task.RequirementsRevision, &snapshot, &dependencies, &task.PlanRevision, &task.EvidenceFreshness, &task.ImplementationStarted, &task.FixRounds); err != nil {
			rows.Close()
			return err
		}
		task.TaskSnapshot, task.DependencySnapshot = json.RawMessage(snapshot), json.RawMessage(dependencies)
		if task.TaskID != requestedTask {
			task.TaskSnapshot = supervisionTaskSummary(task.TaskSnapshot)
		}
		input.Tasks = append(input.Tasks, task)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	// 事件严格截至决策依据；不把 decision.requested 或随后的控制事件混入旧观察。
	rows, err = tx.QueryContext(ctx, `select seq,type,payload,created_at from supervision_events where session_id=? and seq<=? order by seq desc limit 100`, ss.ID, d.BasisSeq)
	if err != nil {
		return err
	}
	for rows.Next() {
		event := supervisionEvent{SessionID: ss.ID}
		var payload string
		if err := rows.Scan(&event.Seq, &event.Type, &payload, &event.CreatedAt); err != nil {
			rows.Close()
			return err
		}
		event.Payload = supervisionModelJSON(json.RawMessage(payload))
		input.RecentEvents = append(input.RecentEvents, event)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for left, right := 0, len(input.RecentEvents)-1; left < right; left, right = left+1, right-1 {
		input.RecentEvents[left], input.RecentEvents[right] = input.RecentEvents[right], input.RecentEvents[left]
	}
	input.EventsTruncated = len(input.RecentEvents) > 0 && input.RecentEvents[0].Seq > 1
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	for len(raw) > maxSupervisionSchedulerInputBytes {
		input.RecordsTruncated = true
		switch {
		case len(input.RecentEvents) > 0:
			input.RecentEvents = input.RecentEvents[1:]
			input.EventsTruncated = true
		case len(input.RoleJobs) > 0:
			input.RoleJobs = input.RoleJobs[:len(input.RoleJobs)-1]
		case len(input.Evidence) > 0:
			input.Evidence = input.Evidence[:len(input.Evidence)-1]
		case len(input.Sources) > 0:
			input.Sources = input.Sources[:len(input.Sources)-1]
		default:
			return errors.New("托管任务和规则资料超过1MiB，需要拆分任务集合或缩减项目规则")
		}
		raw, err = json.Marshal(input)
		if err != nil {
			return err
		}
	}
	digest := sha256.Sum256(raw)
	_, err = tx.ExecContext(ctx, `insert into supervision_scheduler_inputs(execution_id,input_json,input_sha256,created_at) values (?,?,?,?)`, d.ExecutionID, string(raw), hex.EncodeToString(digest[:]), now)
	return err
}

func (s *Server) supervisionSchedulerInput(ctx context.Context, executionID string) (json.RawMessage, string, error) {
	var raw, digest string
	if err := s.db.QueryRowContext(ctx, `select input_json,input_sha256 from supervision_scheduler_inputs where execution_id=?`, executionID).Scan(&raw, &digest); err != nil {
		return nil, "", err
	}
	actual := sha256.Sum256([]byte(raw))
	if len(raw) > maxSupervisionSchedulerInputBytes || hex.EncodeToString(actual[:]) != digest || !json.Valid([]byte(raw)) {
		return nil, "", errors.New("持久化调度资料校验失败")
	}
	return json.RawMessage(raw), digest, nil
}
