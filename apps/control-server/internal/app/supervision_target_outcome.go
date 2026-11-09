package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

type supervisionTargetOutcome struct {
	Execution targetexec.Execution
	Receipt   supervisionSchedulerCLIReceipt
	Check     *supervisionCheckResult
	RunError  error
}

// A visible terminal message alone cannot settle execution. Query the original
// target execution, drain its durable event prefix, and require the exact result
// payload named by that execution's independently persisted result digest.
func (s *Server) readSupervisionTargetOutcome(ctx context.Context, link *supervisionTargetLink, permit supervisionExecutionPermit) (supervisionTargetOutcome, error) {
	var out supervisionTargetOutcome
	if link == nil || link.Client == nil {
		return out, targetexec.ErrClosed
	}
	response, err := link.Call(ctx, targetexec.Request{ID: uuid.NewString(), Method: "query", SessionID: permit.Decision.SessionID, ExecutionID: permit.Decision.ExecutionID})
	if err != nil {
		return out, err
	}
	if response.Execution == nil {
		return out, targetexec.ErrProtocol
	}
	actual := *response.Execution
	if actual.Spec.SessionID != permit.Decision.SessionID || actual.Spec.ExecutionID != permit.Decision.ExecutionID || actual.Spec.TargetID != link.TargetID || actual.Spec.Role != permit.Role || actual.Spec.AgentID != permit.AgentID {
		return out, targetexec.ErrConflict
	}
	if err := verifySupervisionTargetExecutionFact(ctx, s.db, actual); err != nil {
		return out, err
	}
	if actual.State != "quiescent" {
		return out, errWorkspaceNotQuiescent
	}
	for {
		more, err := s.readSupervisionTargetEvents(ctx, link, permit.Decision.SessionID)
		if err != nil {
			return out, err
		}
		if !more {
			break
		}
	}
	payload, err := supervisionTargetResultPayload(ctx, s.db, permit, actual)
	if err != nil {
		return out, err
	}
	return s.decodeSupervisionTargetOutcome(ctx, permit, actual, payload)
}

func (s *Server) decodeSupervisionTargetOutcome(ctx context.Context, permit supervisionExecutionPermit, actual targetexec.Execution, payload json.RawMessage) (supervisionTargetOutcome, error) {
	out := supervisionTargetOutcome{Execution: actual}
	if permit.Role == "test" {
		var result supervisionCheckResult
		if json.Unmarshal(payload, &result) != nil || result.FinishedAt.IsZero() || result.FinishedAt.Before(result.StartedAt) {
			return out, targetexec.ErrProtocol
		}
		input, err := s.supervisionTargetExecutionInput(ctx, permit)
		if err != nil {
			return out, err
		}
		var expected supervisionTargetCheckInput
		if json.Unmarshal(input.Observation, &expected) != nil {
			return out, targetexec.ErrProtocol
		}
		if expected.Check.Directory == "" {
			expected.Check.Directory = "."
		}
		if string(mustJSON(expected.Check)) != string(mustJSON(result.Check)) {
			return out, targetexec.ErrConflict
		}
		out.Check = &result
		out.Receipt = supervisionSchedulerCLIReceipt{AgentID: permit.AgentID, Contract: supervisionTargetVerificationContract, ContainerQuiet: true, ProcessStarted: !result.NotExecuted, ProcessWaited: !result.NotExecuted, InputSHA256: actual.Spec.InputSHA256}
		if result.Error != "" || result.ExitCode != 0 || result.NotExecuted || result.Interrupted {
			out.RunError = errors.New("目标验证未通过：" + result.Error)
		}
	} else {
		var result struct {
			Receipt supervisionSchedulerCLIReceipt `json:"receipt"`
			Failed  bool                           `json:"failed"`
			Error   string                         `json:"error"`
		}
		if json.Unmarshal(payload, &result) != nil || !result.Receipt.ContainerQuiet || result.Receipt.ProcessStarted && !result.Receipt.ProcessWaited || result.Failed != (result.Error != "") {
			return out, targetexec.ErrProtocol
		}
		out.Receipt = result.Receipt
		if !result.Failed || out.Receipt.ProcessStarted {
			if out.Receipt.AgentID != actual.Spec.AgentID || out.Receipt.Contract != actual.Spec.Contract || out.Receipt.CLIPath != actual.Spec.CLIPath || out.Receipt.CLIVersion != actual.Spec.CLIVersion || out.Receipt.InputSHA256 != actual.Spec.InputSHA256 || !result.Failed && (!out.Receipt.Initialized || !out.Receipt.ProcessStarted) {
				return out, targetexec.ErrConflict
			}
		}
		if result.Failed {
			out.RunError = errors.New(result.Error)
		}
	}
	if out.Receipt.ProcessStarted && (actual.Process.PID < 1 || actual.Process.Birth == "" || actual.Process.Container != actual.Container || actual.Process.Executable == "" || permit.Role != "test" && actual.Process.Executable != actual.Spec.CLIPath) {
		return out, targetexec.ErrConflict
	}
	if actual.Interrupted && out.RunError == nil {
		out.RunError = errors.Join(out.RunError, context.Canceled)
	}
	return out, nil
}

func verifySupervisionTargetExecutionFact(ctx context.Context, q sqlQueryer, actual targetexec.Execution) error {
	if actual.Digest != supervisionSHA256(mustJSON(actual.Spec)) || actual.Container == "" {
		return targetexec.ErrProtocol
	}
	var expected string
	if err := q.QueryRowContext(ctx, `select spec from supervision_target_executions where execution_id=? and session_id=? and target_id=?`, actual.Spec.ExecutionID, actual.Spec.SessionID, actual.Spec.TargetID).Scan(&expected); err != nil {
		return err
	}
	if expected != string(mustJSON(actual.Spec)) {
		return targetexec.ErrConflict
	}
	return nil
}

func supervisionTargetResultPayload(ctx context.Context, q sqlQueryer, permit supervisionExecutionPermit, actual targetexec.Execution) (json.RawMessage, error) {
	if actual.State != "quiescent" || len(actual.ResultSHA256) != 64 {
		return nil, targetexec.ErrNotQuiet // Quiet without a result remains unknown.
	}
	kind := "supervision.target.execution.result"
	if permit.Role == "test" {
		kind = "supervision.target.verification.result"
	}
	var raw string
	var count int
	if err := q.QueryRowContext(ctx, `select count(*),coalesce(min(payload),'') from events where run_id=? and conversation_id=? and type=?`, permit.RunID, permit.ConversationID, kind).Scan(&count, &raw); err != nil {
		return nil, err
	}
	if count != 1 || supervisionSHA256([]byte(raw)) != actual.ResultSHA256 {
		return nil, targetexec.ErrConflict
	}
	return json.RawMessage(raw), nil
}

func (s *Server) finishSupervisionTargetExecution(ctx context.Context, permit supervisionExecutionPermit, outcome supervisionTargetOutcome, now time.Time) error {
	if outcome.Execution.Spec.SessionID != permit.Decision.SessionID || outcome.Execution.Spec.ExecutionID != permit.Decision.ExecutionID || outcome.Execution.State != "quiescent" {
		return targetexec.ErrConflict
	}
	payload, err := supervisionTargetResultPayload(ctx, s.db, permit, outcome.Execution)
	if err != nil {
		return err
	}
	// Rebuild status/output from the immutable result, rather than accepting a
	// caller's edited receipt or a separately supplied success/failure flag.
	bound, err := s.decodeSupervisionTargetOutcome(ctx, permit, outcome.Execution, payload)
	if err != nil {
		return err
	}
	return s.finishSupervisionExecutionFacts(ctx, permit, bound.Receipt, bound.RunError, now, func(tx *sql.Tx) (bool, error) {
		if err := verifySupervisionTargetExecutionFact(ctx, tx, outcome.Execution); err != nil {
			return false, err
		}
		current, err := supervisionTargetResultPayload(ctx, tx, permit, outcome.Execution)
		if err != nil {
			return false, err
		}
		if string(current) != string(payload) {
			return false, targetexec.ErrConflict
		}
		return bound.Receipt.ContainerQuiet && (!bound.Receipt.ProcessStarted || bound.Receipt.ProcessWaited), nil
	})
}
