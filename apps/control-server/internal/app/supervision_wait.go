package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const supervisionWaitInterval = 60 * time.Second

type supervisionWait struct {
	Condition   string    `json:"condition"`
	NextCheckAt time.Time `json:"nextCheckAt"`
	Rounds      int       `json:"rounds"`
}

func loadSupervisionWait(ctx context.Context, q sqlQueryer, ss supervisionSession) (*supervisionWait, error) {
	if ss.Control != "" || ss.RecoveryState != "ready" || (ss.State != "running" && ss.State != "finalizing") {
		return nil, nil
	}
	var out supervisionWait
	err := q.QueryRowContext(ctx, `select condition,next_check_at,rounds from supervision_wait_state where session_id=? and expected_state_version=?`, ss.ID, ss.StateVersion).Scan(&out.Condition, &out.NextCheckAt, &out.Rounds)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &out, err
}

func recordSupervisionWaitTx(ctx context.Context, tx *sql.Tx, ss *supervisionSession, pc supervisionProjectContext, condition, reason string, now time.Time) error {
	// Wording and elapsed time are not progress. Scope the bounded reassessment
	// to actual task, source, rule, dependency and evidence facts.
	var tasks, dependencies string
	var evidence, cutoff int64
	if err := tx.QueryRowContext(ctx, `select coalesce(json_group_array(json_array(task_id,requirements_revision,plan_revision,phase)), '[]') from (select task_id,requirements_revision,plan_revision,phase from supervision_tasks where session_id=? order by task_id)`, ss.ID).Scan(&tasks); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `select coalesce((select fingerprint from supervision_dependency_state where session_id=?),'')`, ss.ID).Scan(&dependencies); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `select coalesce(max(rowid),0) from supervision_evidence where session_id=?`, ss.ID).Scan(&evidence); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `select coalesce((select evidence_rowid from supervision_dependency_cutoff where session_id=?),0)`, ss.ID).Scan(&cutoff); err != nil {
		return err
	}
	fingerprint := supervisionSHA256(mustJSON([]any{pc.RevisionID, pc.RulesHash, tasks, dependencies, evidence, cutoff}))
	var previous string
	var rounds int
	err := tx.QueryRowContext(ctx, `select fingerprint,rounds from supervision_wait_state where session_id=?`, ss.ID).Scan(&previous, &rounds)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if previous != fingerprint {
		rounds = 0
	}
	rounds++
	at := now.Add(supervisionWaitInterval)
	if _, err := tx.ExecContext(ctx, `insert into supervision_wait_state(session_id,expected_state_version,condition,next_check_at,fingerprint,rounds) values (?,?,?,?,?,?) on conflict(session_id) do update set expected_state_version=excluded.expected_state_version,condition=excluded.condition,next_check_at=excluded.next_check_at,fingerprint=excluded.fingerprint,rounds=excluded.rounds`, ss.ID, ss.StateVersion, condition, at, fingerprint, rounds); err != nil {
		return err
	}
	if rounds >= 3 {
		ss.State, ss.Reason, ss.ClockAt = "needs_input", "no_progress", nil
		ss.StateVersion++
		return appendSupervisionEventTx(ctx, tx, ss, "scheduler.no_progress", map[string]any{"condition": condition, "waitingRounds": rounds, "reason": "连续复查后没有新事实，需要明确处理等待条件"}, now)
	}
	return appendSupervisionEventTx(ctx, tx, ss, "scheduler.waiting", map[string]any{"condition": condition, "reason": reason, "nextCheckAt": at, "rounds": rounds}, now)
}

func (s *Server) refreshSupervisionWait(ctx context.Context, id string, now time.Time) error {
	ss, err := loadSupervisionSession(ctx, s.db, id)
	if err != nil || ss.Control != "" || ss.RecoveryState != "ready" || (ss.State != "running" && ss.State != "finalizing") {
		return err
	}
	wait, err := loadSupervisionWait(ctx, s.db, ss)
	if err != nil {
		return err
	}
	if wait == nil {
		// Upgrade an already-applied legacy wait once, without replaying its
		// model call or requiring the user to pause/resume to unstick it.
		var output string
		err := s.db.QueryRowContext(ctx, `select output from supervision_decisions where session_id=? and expected_state_version=? and state='applied' order by created_at desc,id desc limit 1`, id, ss.StateVersion).Scan(&output)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var proposal struct {
			Action supervisionWorkflowAction `json:"action"`
		}
		if json.Unmarshal([]byte(output), &proposal) != nil || proposal.Action.Type != "wait" {
			return nil
		}
		return s.withSupervisionTx(ctx, id, now, func(tx *sql.Tx, _ map[string]WorkspaceAdmission, current *supervisionSession) error {
			if current.StateVersion != ss.StateVersion || current.Control != "" || current.RecoveryState != "ready" {
				return errSupervisionState
			}
			pc, err := loadSupervisionProjectContext(ctx, tx, id, "current")
			if err != nil {
				return err
			}
			return recordSupervisionWaitTx(ctx, tx, current, pc, proposal.Action.Condition, "恢复已有等待的定期核对", now)
		})
	}
	// The 30-second worker sweep checks physical facts even before the model
	// reassessment is due. No model call is needed to detect external edits.
	if _, err := s.captureSupervisionProjectContext(ctx, id, ss.StateVersion); err != nil {
		if errors.Is(err, errSupervisionSnapshotChanged) || errors.Is(err, errWorkspaceNotQuiescent) {
			return nil
		}
		return err
	}
	if now.Before(wait.NextCheckAt) {
		return nil
	}
	return s.withSupervisionTx(ctx, id, now, func(tx *sql.Tx, _ map[string]WorkspaceAdmission, current *supervisionSession) error {
		if current.StateVersion != ss.StateVersion || current.Control != "" || current.RecoveryState != "ready" || (current.State != "running" && current.State != "finalizing") {
			return errSupervisionState
		}
		if err := supervisionRunsQuiescentTx(ctx, tx, id); err != nil {
			return err
		}
		if err := requireSupervisionNoPendingDrift(ctx, tx, id); err != nil {
			return err
		}
		current.StateVersion++ // A due, bounded reassessment is an explicit trigger.
		return appendSupervisionEventTx(ctx, tx, current, "scheduler.wait_recheck", map[string]any{"condition": wait.Condition, "rounds": wait.Rounds}, now)
	})
}
