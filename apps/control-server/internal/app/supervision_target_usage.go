package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

// Target counters are cumulative facts, not increments. Commit their watermark
// with run_usage so retries/restarts never charge a second copy. Receiving usage
// does not release quota, settle execution, or acknowledge a model decision.
func (s *Server) consumeSupervisionTargetUsage(ctx context.Context, sessionID, targetID string, execution targetexec.Execution) error {
	usage := execution.Usage
	if execution.Spec.SessionID != sessionID || execution.Spec.TargetID != targetID || execution.State != "quiescent" || usage.Sequence < 1 {
		return targetexec.ErrConflict
	}
	// Reject impossible counters before arithmetic or SQLite aggregation. The
	// active duration is separately bound to this immutable execution's turn.
	for _, value := range []int64{usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens, usage.CacheCreationTokens} {
		if value < 0 || value > 1<<50 {
			return targetexec.ErrProtocol
		}
	}
	if usage.ActiveMillis < 0 || usage.ActiveMillis > (30*time.Minute+time.Minute).Milliseconds() {
		return targetexec.ErrProtocol
	}
	return s.withSupervisionTx(ctx, sessionID, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, _ *supervisionSession) error {
		var expected, runID, conversationID string
		if err := tx.QueryRowContext(ctx, `select t.spec,e.run_id,r.conversation_id from supervision_target_executions t join supervision_executions e on e.execution_id=t.execution_id join runs r on r.id=e.run_id where t.execution_id=? and t.session_id=? and t.target_id=? and e.session_id=?`, execution.Spec.ExecutionID, sessionID, targetID, sessionID).Scan(&expected, &runID, &conversationID); err != nil {
			return err
		}
		if expected != string(mustJSON(execution.Spec)) {
			return targetexec.ErrConflict
		}
		var previousRaw string
		err := tx.QueryRowContext(ctx, `select counters from supervision_target_usage where execution_id=?`, execution.Spec.ExecutionID).Scan(&previousRaw)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var previous targetexec.Usage
		if err == nil && json.Unmarshal([]byte(previousRaw), &previous) != nil {
			return targetexec.ErrProtocol
		}
		if usage.Sequence < previous.Sequence {
			return nil
		}
		raw := string(mustJSON(usage))
		if usage.Sequence == previous.Sequence {
			if raw != previousRaw {
				return targetexec.ErrConflict
			}
			return nil
		}
		if usage.InputTokens < previous.InputTokens || usage.OutputTokens < previous.OutputTokens || usage.CacheReadTokens < previous.CacheReadTokens || usage.CacheCreationTokens < previous.CacheCreationTokens || usage.ActiveMillis < previous.ActiveMillis {
			return targetexec.ErrConflict
		}
		if _, err := tx.ExecContext(ctx, `insert into supervision_target_usage(execution_id,session_id,counters) values (?,?,?) on conflict(execution_id) do update set counters=excluded.counters`, execution.Spec.ExecutionID, sessionID, raw); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `insert into run_usage(run_id,conversation_id,model,input_tokens,output_tokens,cache_read_tokens,cache_creation_tokens,duration_ms,completed_at) values (?,?,?,?,?,?,?,?,?)
		 on conflict(run_id) do update set model=excluded.model,input_tokens=excluded.input_tokens,output_tokens=excluded.output_tokens,cache_read_tokens=excluded.cache_read_tokens,cache_creation_tokens=excluded.cache_creation_tokens,duration_ms=excluded.duration_ms,completed_at=excluded.completed_at`, runID, conversationID, execution.Spec.Model, usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens, usage.CacheCreationTokens, usage.ActiveMillis, time.Now().UTC())
		return err
	})
}
