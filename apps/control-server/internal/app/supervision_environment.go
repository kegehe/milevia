package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// 测试返回后环境仍可能被外部工具改变。派发前与动作应用前再次核对；
// I/O 在事务外进行，提交时核对版本及静默状态，旧观察不能完成验收。
func (s *Server) refreshSupervisionEvidenceEnvironments(ctx context.Context, id string) error {
	ss, err := loadSupervisionSession(ctx, s.db, id)
	if err != nil {
		return err
	}
	if ss.Control != "" || ss.RecoveryState != "ready" || (ss.State != "running" && ss.State != "finalizing") {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `select e.details,coalesce(p.plan_json,'') from supervision_evidence e join supervision_tasks t on t.session_id=e.session_id and t.task_id=e.task_id left join supervision_plan_versions p on p.session_id=e.session_id and p.task_id=e.task_id and p.revision=e.plan_revision join supervision_context_state st on st.session_id=e.session_id join supervision_project_contexts c on c.session_id=e.session_id and c.id=st.current_id where e.session_id=? and e.revision_id=c.revision_id and e.requirements_revision=t.requirements_revision and e.plan_revision=t.plan_revision and e.kind in ('command','browser') and e.rowid>coalesce((select evidence_rowid from supervision_dependency_cutoff where session_id=?),0) order by e.rowid desc`, id, id)
	if err != nil {
		return err
	}
	type environmentCheck struct {
		Result supervisionCheckResult
		Plan   supervisionTaskPlan
	}
	var checks []environmentCheck
	for rows.Next() {
		var raw, rawPlan string
		if err = rows.Scan(&raw, &rawPlan); err != nil {
			break
		}
		var result supervisionCheckResult
		if err = json.Unmarshal([]byte(raw), &result); err != nil {
			break
		}
		// 已执行的失败同样绑定环境。修复依赖后必须允许同规格重新验证，
		// 旧的实施前基线也不能继续授权；中断或未执行记录只保留历史。
		if !result.Interrupted && !result.NotExecuted && result.Environment != "" {
			var plan supervisionTaskPlan
			if err = json.Unmarshal([]byte(rawPlan), &plan); err != nil {
				break
			}
			checks = append(checks, environmentCheck{result, plan})
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	if len(checks) == 0 {
		return nil
	}
	var source supervisionWorkspaceSource
	if err := s.withSupervisionTx(ctx, id, time.Now().UTC(), func(tx *sql.Tx, admissions map[string]WorkspaceAdmission, current *supervisionSession) error {
		if current.StateVersion != ss.StateVersion || current.Control != "" || current.RecoveryState != "ready" {
			return errSupervisionState
		}
		var err error
		source, err = s.supervisionWorkspaceSourceTx(ctx, tx, current, admissions[current.AdmissionID])
		return err
	}); err != nil {
		return err
	}
	cache := map[string]string{}
	changed := false
	reason := ""
	for _, item := range checks {
		result := item.Result
		key := supervisionCheckSemanticHash(result.Check)
		actual, exists := cache[key]
		if !exists {
			actual, err = source.environment(ctx, s, item.Plan, result.Check)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				changed = true
				reason = redactAgentText(err.Error())
				break
			}
			cache[key] = actual
		}
		if actual != result.Environment {
			changed = true
			reason = "验证之后依赖环境或工具发生变化，需要重新测试和审查"
			break
		}
	}
	if !changed {
		return nil
	}
	return s.withSupervisionTx(ctx, id, time.Now().UTC(), func(tx *sql.Tx, admissions map[string]WorkspaceAdmission, current *supervisionSession) error {
		if current.StateVersion != ss.StateVersion || current.Control != "" || current.RecoveryState != "ready" {
			return errSupervisionState
		}
		if err := source.verifyTx(ctx, s, tx, current, admissions[current.AdmissionID]); err != nil {
			return err
		}
		active, err := supervisionHasExecutionTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if active {
			return errWorkspaceNotQuiescent
		}
		now := time.Now().UTC()
		if err := suspendSupervisionQueuedTx(ctx, tx, current, false, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `insert into supervision_dependency_cutoff(session_id,evidence_rowid) select ?,coalesce(max(rowid),0) from supervision_evidence where session_id=? on conflict(session_id) do update set evidence_rowid=excluded.evidence_rowid;update supervision_tasks set evidence_freshness='stale',phase=case when phase in ('verifying','reviewing') then 'implemented' when phase='implementing' then case when implementation_started=1 then 'needs_repair' else 'ready' end else phase end where session_id=? and phase not in ('unresolved','deferred')`, id, id, id); err != nil {
			return err
		}
		current.StateVersion++
		return appendSupervisionEventTx(ctx, tx, current, "verification.environment_changed", map[string]string{"reason": reason}, now)
	})
}
