package app

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

// CLI observations are already frozen. Verification uses a narrower target
// input, derived here from that same immutable role input and persistent job;
// a caller cannot substitute another command while keeping the execution ID.
func supervisionTargetExecutionInputTx(ctx context.Context, tx *sql.Tx, permit supervisionExecutionPermit, raw json.RawMessage) (targetexec.Input, error) {
	if permit.Role != "test" {
		return targetexec.Input{Observation: raw, Instructions: supervisionRolePrompt(permit.Role)}, nil
	}
	var in supervisionRoleInput
	if json.Unmarshal(raw, &in) != nil || in.SchemaVersion != 1 || in.Decision.ID != permit.Decision.ID || in.Decision.ExecutionID != permit.Decision.ExecutionID || in.Decision.WorkItemID != permit.Decision.WorkItemID || in.Decision.SessionID != permit.Decision.SessionID || in.Decision.Attempt != permit.Decision.Attempt || in.Decision.LeaseEpoch != permit.Lease.Epoch {
		return targetexec.Input{}, targetexec.ErrConflict
	}
	job, err := loadSupervisionRoleJob(ctx, tx, permit.Decision.ExecutionID)
	if err != nil {
		return targetexec.Input{}, err
	}
	if job.Kind != "verification" || job.TaskID != permit.TaskID || in.Job.Kind != job.Kind || in.Job.ExecutionID != job.ExecutionID || in.Job.TaskID != job.TaskID || in.Job.InputRevision != job.InputRevision || in.Job.RequirementsRevision != job.RequirementsRevision || in.Job.PlanRevision != job.PlanRevision || string(in.Job.Spec) != string(job.Spec) || in.Task.TaskID != job.TaskID || in.Task.RequirementsRevision != job.RequirementsRevision || in.Plan.TaskID != job.TaskID || in.Plan.RequirementsRevision != job.RequirementsRevision || in.Plan.Revision != job.PlanRevision || in.ProjectContext.RevisionID != job.InputRevision {
		return targetexec.Input{}, targetexec.ErrConflict
	}
	var action supervisionWorkflowAction
	if json.Unmarshal(job.Spec, &action) != nil || action.Type != "request_verification" || action.SessionID != permit.Decision.SessionID || action.TaskID != job.TaskID || action.RequirementsRevision != job.RequirementsRevision || action.PlanRevision != job.PlanRevision || len(action.Checks) != 1 {
		return targetexec.Input{}, targetexec.ErrConflict
	}
	if err := validateSupervisionPlan(&in.Plan.Plan, in.ProjectContext.Manifest); err != nil {
		return targetexec.Input{}, err
	}
	if err := validateSupervisionCheck(action.Checks[0], in.Plan.Plan, in.ProjectContext.Manifest); err != nil {
		return targetexec.Input{}, err
	}
	return targetexec.Input{Instructions: supervisionTargetVerificationContract, Observation: mustJSON(supervisionTargetCheckInput{RevisionID: job.InputRevision, RulesHash: in.ProjectContext.RulesHash, Plan: in.Plan.Plan, Check: action.Checks[0]})}, nil
}

func (s *Server) supervisionTargetExecutionInput(ctx context.Context, permit supervisionExecutionPermit) (targetexec.Input, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return targetexec.Input{}, err
	}
	defer tx.Rollback()
	var raw string
	if err := tx.QueryRowContext(ctx, `select input_json from supervision_scheduler_inputs where execution_id=?`, permit.Decision.ExecutionID).Scan(&raw); err != nil {
		return targetexec.Input{}, err
	}
	return supervisionTargetExecutionInputTx(ctx, tx, permit, json.RawMessage(raw))
}
