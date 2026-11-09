package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

type supervisionTargetHandoff struct {
	ID, State                    string
	StateVersion, AdmissionEpoch int64
	Payload                      supervisionTargetFinalization
}

func loadSupervisionTargetHandoff(ctx context.Context, q sqlQueryer, sessionID string) (supervisionTargetHandoff, error) {
	var h supervisionTargetHandoff
	var spec, checks string
	err := q.QueryRowContext(ctx, `select id,state,state_version,admission_epoch,spec,checks from supervision_target_finalizations where session_id=? and state='pending'`, sessionID).Scan(&h.ID, &h.State, &h.StateVersion, &h.AdmissionEpoch, &spec, &checks)
	if err == nil && (json.Unmarshal([]byte(spec), &h.Payload.Spec) != nil || json.Unmarshal([]byte(checks), &h.Payload.Checks) != nil || h.Payload.Spec.ID != h.ID || h.Payload.Spec.SessionID != sessionID) {
		err = targetexec.ErrProtocol
	}
	return h, err
}

// Freeze all business inputs, including evidence contents rather than only its
// high watermark. Heartbeats, transcript reads and time accounting are excluded.
// Queries are fixed code; their values never become SQL identifiers.
func supervisionTargetHandoffBinding(ctx context.Context, tx *sql.Tx, ss *supervisionSession, source supervisionWorkspaceSource, pc supervisionProjectContext) (string, error) {
	queries := []string{
		`select * from supervision_tasks where session_id=? order by task_id`,
		`select * from supervision_plan_versions where session_id=? order by task_id,revision`,
		`select rowid,* from supervision_evidence where session_id=? order by rowid`,
		`select * from supervision_dependency_state where session_id=?`,
		`select * from supervision_dependency_cutoff where session_id=?`,
		`select * from supervision_amendments where session_id=? order by id`,
		`select * from supervision_questions where session_id=? order by id`,
		`select * from supervision_mutations where session_id=? order by execution_id`,
		`select * from supervision_role_observations where session_id=? order by execution_id`,
		`select * from supervision_target_mutations where session_id=? order by execution_id`,
		`select * from tasks where id in (select task_id from supervision_tasks where session_id=?) order by id`,
		`select path,runner,runner_id from projects where id=(select project_id from supervision_sessions where id=?)`,
	}
	facts := make([][][]any, 0, len(queries))
	for _, query := range queries {
		rows, err := tx.QueryContext(ctx, query, ss.ID)
		if err != nil {
			return "", err
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			return "", err
		}
		group := [][]any{}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range pointers {
				pointers[i] = &values[i]
			}
			if err = rows.Scan(pointers...); err != nil {
				break
			}
			for i, value := range values {
				if body, ok := value.([]byte); ok {
					values[i] = string(body)
				}
			}
			group = append(group, values)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return "", err
		}
		facts = append(facts, group)
	}
	return supervisionSHA256(mustJSON(struct {
		Version, AdmissionEpoch, LeaseEpoch int64
		Owner, AdmissionID, Revision, Rules string
		Budget                              supervisionBudget
		Roles                               supervisionRoleConfigs
		Source                              supervisionWorkspaceSource
		Facts                               [][][]any
	}{ss.StateVersion, ss.AdmissionEpoch, ss.LeaseEpoch, ss.LeaseOwner, ss.AdmissionID, pc.RevisionID, pc.RulesHash, ss.Budget, ss.Roles, source, facts})), nil
}

// Independently recheck the entire accepted subset at preparation and commit.
// A target receipt alone can never stand in for domain acceptance.
func supervisionTargetHandoffReadyTx(ctx context.Context, tx *sql.Tx, ss *supervisionSession, pc supervisionProjectContext) (int, int, error) {
	if err := requireSupervisionRoleObservationsTx(ctx, tx, ss.ID); err != nil {
		return 0, 0, err
	}
	var pending, remaining, accepted, issues int
	if err := tx.QueryRowContext(ctx, `select count(*) from supervision_amendments where session_id=? and state='pending'`, ss.ID).Scan(&pending); err != nil {
		return 0, 0, err
	}
	if err := tx.QueryRowContext(ctx, `select count(case when phase not in ('accepted','unresolved','deferred') then 1 end),count(case when phase='accepted' then 1 end),count(case when phase in ('unresolved','deferred') then 1 end) from supervision_tasks where session_id=?`, ss.ID).Scan(&remaining, &accepted, &issues); err != nil {
		return 0, 0, err
	}
	if pending != 0 || remaining != 0 || accepted+issues == 0 {
		return 0, 0, errSupervisionState
	}
	if err := supervisionRunsQuiescentTx(ctx, tx, ss.ID); err != nil {
		return 0, 0, err
	}
	if err := verifySupervisionDependenciesTx(ctx, tx, ss.ID); err != nil {
		return 0, 0, err
	}
	plans, err := loadSupervisionPlansTx(ctx, tx, ss.ID)
	if err != nil {
		return 0, 0, err
	}
	checked := 0
	for _, plan := range plans {
		var phase, freshness string
		var requirements, revision int64
		if err := tx.QueryRowContext(ctx, `select phase,requirements_revision,plan_revision,evidence_freshness from supervision_tasks where session_id=? and task_id=?`, ss.ID, plan.TaskID).Scan(&phase, &requirements, &revision, &freshness); err != nil {
			return 0, 0, err
		}
		if phase != "accepted" {
			continue
		}
		if requirements != plan.RequirementsRevision || revision != plan.Revision || freshness != "fresh" || plan.Status == "review_required" {
			return 0, 0, invalidSupervisionWorkflow("候选任务的要求、计划或证据已改变，需要重新验收")
		}
		if err := supervisionAcceptanceReadyTx(ctx, tx, ss.ID, plan.TaskID, pc.RevisionID, plan.RequirementsRevision, plan.Revision, plan.Plan); err != nil {
			return 0, 0, err
		}
		if err := supervisionDependenciesReadyTx(ctx, tx, ss.ID, plan.Plan.DependsOn, pc.RevisionID); err != nil {
			return 0, 0, err
		}
		checked++
	}
	if checked != accepted {
		return 0, 0, errSupervisionPlanInvalid
	}
	return accepted, issues, nil
}

func supervisionTargetHandoffChecks(ctx context.Context, tx *sql.Tx, ss *supervisionSession, pc supervisionProjectContext) ([]supervisionCheck, string, error) {
	rows, err := tx.QueryContext(ctx, `select e.task_id,e.details,e.passed from supervision_evidence e join supervision_tasks t on t.session_id=e.session_id and t.task_id=e.task_id where e.session_id=? and t.phase='accepted' and e.revision_id=? and e.requirements_revision=t.requirements_revision and e.plan_revision=t.plan_revision and e.kind in ('command','browser') and e.rowid>coalesce((select evidence_rowid from supervision_dependency_cutoff where session_id=?),0) order by e.rowid desc`, ss.ID, pc.RevisionID, ss.ID)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	checks := map[string]supervisionCheck{}
	environments := map[string]string{}
	latest := map[string]bool{}
	for rows.Next() {
		var taskID, raw string
		var passed bool
		if err := rows.Scan(&taskID, &raw, &passed); err != nil {
			return nil, "", err
		}
		var result supervisionCheckResult
		if err := json.Unmarshal([]byte(raw), &result); err != nil {
			return nil, "", err
		}
		if result.Check.Baseline || result.Interrupted || result.NotExecuted {
			continue
		}
		key := supervisionCheckSemanticHash(result.Check)
		if latest[taskID+":"+key] {
			continue
		}
		latest[taskID+":"+key] = true
		if !passed {
			continue // A later failed check cannot revive an older passing environment.
		}
		if result.Environment == "" || environments[key] != "" && environments[key] != result.Environment {
			return nil, "", errSupervisionSnapshotChanged
		}
		checks[key], environments[key] = result.Check, result.Environment
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(checks) > supervisionTargetEnvironmentCheckLimit {
		return nil, "", targetexec.ErrLimit
	}
	keys := make([]string, 0, len(checks))
	for key := range checks {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	ordered, facts := []supervisionCheck{}, []map[string]string{}
	for _, key := range keys {
		ordered = append(ordered, checks[key])
		facts = append(facts, map[string]string{"check": key, "environment": environments[key]})
	}
	return ordered, supervisionSHA256(mustJSON(facts)), nil
}

// Called before ordinary maintenance reads: a sealed target intentionally
// refuses normal source reads. Retries first finish (or explicitly supersede)
// the original handoff instead of opening another scheduler turn.
func (s *Server) advanceSupervisionTargetHandoff(ctx context.Context, id, owner string) (bool, error) {
	ss, err := loadSupervisionSession(ctx, s.db, id)
	if err != nil {
		return false, err
	}
	if ss.Control != "" || ss.RecoveryState != "ready" || (ss.State != "running" && ss.State != "finalizing") {
		return false, nil
	}
	prior, priorErr := loadSupervisionTargetHandoff(ctx, s.db, id)
	if priorErr != nil && !errors.Is(priorErr, sql.ErrNoRows) {
		return true, priorErr
	}
	if errors.Is(priorErr, sql.ErrNoRows) && (ss.State != "finalizing" || ss.Reason != "target_finalization") {
		if ss.State != "finalizing" {
			return false, nil
		}
		if _, err := loadSupervisionTargetControl(ctx, s.db, id); errors.Is(err, sql.ErrNoRows) {
			return false, nil
		} else if err != nil {
			return true, err
		}
		// After guarded recovery/supersession, an unchanged accepted subset
		// needs another seal, not another paid scheduler call. If its evidence
		// is no longer ready, ordinary maintenance and revalidation continue.
		ready := false
		err := s.withSupervisionTx(ctx, id, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, current *supervisionSession) error {
			if current.Control != "" || current.RecoveryState != "ready" || current.State != "finalizing" {
				return nil
			}
			pc, err := loadSupervisionProjectContext(ctx, tx, id, "current")
			if err != nil {
				return err
			}
			if _, _, err := supervisionTargetHandoffReadyTx(ctx, tx, current, pc); err != nil {
				if errors.Is(err, errSupervisionState) || errors.Is(err, errSupervisionPlanInvalid) {
					return nil
				}
				return err
			}
			ready = true
			return s.maybeFinalizeSupervisionTx(ctx, tx, current, pc)
		})
		return ready, err
	}
	blocked := false
	err = s.withSupervisionTx(ctx, id, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, current *supervisionSession) error {
		blocked = current.Control != "" || current.RecoveryState != "ready" || (current.State != "running" && current.State != "finalizing")
		if !blocked && supervisionTimeAvailable(current, "final") <= 0 {
			blocked = true
			return pauseSupervisionForBudgetTx(ctx, tx, current, time.Now().UTC())
		}
		return nil
	})
	if err != nil || blocked {
		return true, err
	}
	lease, err := s.acquireSupervisionLease(ctx, id, owner, time.Now().UTC())
	if err != nil {
		return true, err
	}
	var source supervisionWorkspaceSource
	var pc supervisionProjectContext
	var binding string
	err = s.withSupervisionTx(ctx, id, time.Now().UTC(), func(tx *sql.Tx, admissions map[string]WorkspaceAdmission, current *supervisionSession) error {
		if err := requireSupervisionLease(current, lease, time.Now().UTC()); err != nil {
			return err
		}
		var err error
		source, err = s.supervisionWorkspaceSourceTx(ctx, tx, current, admissions[current.AdmissionID])
		if err != nil {
			return err
		}
		if source.Control == nil {
			return targetexec.ErrAuthority
		}
		pc, err = loadSupervisionProjectContext(ctx, tx, id, "current")
		if err != nil {
			return err
		}
		binding, err = supervisionTargetHandoffBinding(ctx, tx, current, source, pc)
		return err
	})
	if err != nil {
		return true, err
	}
	link, err := source.connect(ctx, s)
	if err != nil {
		return true, err
	}
	defer link.Close()
	if priorErr == nil && (prior.Payload.Spec.BindingSHA256 != binding || prior.Payload.Spec.OwnerEpoch != source.Control.OwnerEpoch || prior.Payload.Spec.AuthorizationVersion != source.Control.Version) {
		return true, s.supersedeSupervisionTargetHandoff(ctx, link, source, prior)
	}
	if priorErr != nil {
		err = s.withSupervisionTx(ctx, id, time.Now().UTC(), func(tx *sql.Tx, admissions map[string]WorkspaceAdmission, current *supervisionSession) error {
			if err := requireSupervisionLease(current, lease, time.Now().UTC()); err != nil {
				return err
			}
			if err := source.verifyTx(ctx, s, tx, current, admissions[current.AdmissionID]); err != nil {
				return err
			}
			if current.State != "finalizing" || current.Reason != "target_finalization" || supervisionTimeAvailable(current, "final") <= 0 {
				return errSupervisionState
			}
			actual, err := supervisionTargetHandoffBinding(ctx, tx, current, source, pc)
			if err != nil {
				return err
			}
			if actual != binding {
				return errSupervisionState
			}
			if _, _, err := supervisionTargetHandoffReadyTx(ctx, tx, current, pc); err != nil {
				return err
			}
			checks, environment, err := supervisionTargetHandoffChecks(ctx, tx, current, pc)
			if err != nil {
				return err
			}
			c := source.Control
			prior = supervisionTargetHandoff{ID: uuid.NewString(), State: "pending", StateVersion: current.StateVersion, AdmissionEpoch: current.AdmissionEpoch}
			prior.Payload = supervisionTargetFinalization{Spec: targetexec.FinalizationSpec{ID: prior.ID, SessionID: id, WorkspaceID: c.WorkspaceID, OwnerEpoch: c.OwnerEpoch, AuthorizationVersion: c.Version, BindingSHA256: binding, RevisionID: pc.RevisionID, RulesHash: pc.RulesHash, EnvironmentHash: environment}, Checks: checks}
			if len(mustJSON(prior.Payload)) > 1536<<10 {
				return targetexec.ErrLimit // Leave room for the authenticated RPC envelope.
			}
			_, err = tx.ExecContext(ctx, `insert into supervision_target_finalizations(id,session_id,state,state_version,admission_epoch,spec,checks) values (?,?,'pending',?,?,?,?)`, prior.ID, id, prior.StateVersion, prior.AdmissionEpoch, string(mustJSON(prior.Payload.Spec)), string(mustJSON(checks)))
			return err
		})
		if err != nil {
			if errors.Is(err, errSupervisionPlanInvalid) {
				return true, s.withSupervisionTx(ctx, id, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, current *supervisionSession) error {
					if current.StateVersion != ss.StateVersion || current.Control != "" {
						return errSupervisionState
					}
					current.Reason = ""
					current.StateVersion++
					return appendSupervisionEventTx(ctx, tx, current, "target.finalization_revalidation_required", map[string]string{"reason": redactAgentText(err.Error())}, time.Now().UTC())
				})
			}
			if errors.Is(err, targetexec.ErrLimit) {
				return true, s.withSupervisionTx(ctx, id, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, current *supervisionSession) error {
					if current.StateVersion != ss.StateVersion || current.Control != "" {
						return errSupervisionState
					}
					current.State, current.Reason, current.ClockAt = "needs_input", "target_finalization_limit", nil
					current.StateVersion++
					return appendSupervisionEventTx(ctx, tx, current, "target.finalization_failed", map[string]string{"reason": "最终检查规格超过单次交接容量，请停止后按较小批次重新托管"}, time.Now().UTC())
				})
			}
			return true, err
		}
	}
	c := source.Control
	sealCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	response, err := link.Call(sealCtx, targetexec.Request{ID: uuid.NewString(), Method: "finalize", SessionID: id, WorkspaceID: c.WorkspaceID, OwnerEpoch: c.OwnerEpoch, AuthorizationVersion: c.Version, Parameters: mustJSON(prior.Payload)})
	if err != nil {
		if errors.Is(err, targetexec.ErrAuthority) {
			// A transport/target restart revokes grants even with no active
			// execution. Reconcile through the existing read-only recovery path;
			// retrying the saved acknowledgement cannot revive that authority.
			return true, errors.Join(err, s.recoverSupervisionTargetHandoffGrant(ctx, link, source, prior))
		}
		return true, err // Keep the exact intent through lost ACK or disconnect.
	}
	var receipt targetexec.Finalization
	if json.Unmarshal(response.Data, &receipt) != nil || receipt.Spec != prior.Payload.Spec || receipt.Digest != supervisionSHA256(mustJSON(prior.Payload.Spec)) {
		return true, targetexec.ErrProtocol
	}
	if receipt.State != "sealed" {
		return true, s.withSupervisionTx(ctx, id, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, current *supervisionSession) error {
			if current.StateVersion != prior.StateVersion || current.Control != "" {
				return errSupervisionState
			}
			current.State, current.Reason, current.ClockAt = "needs_input", "target_finalization_failed", nil
			current.StateVersion++
			return appendSupervisionEventTx(ctx, tx, current, "target.finalization_failed", map[string]string{"id": prior.ID, "reason": "目标最终采集未能封存，需核对目录及依赖变化后恢复"}, time.Now().UTC())
		})
	}
	// TargetTime is sampled before the handler collects/seals. The receipt may
	// legitimately be later; its upper bound is the frozen target grant below.
	if receipt.ObservedAt.IsZero() {
		return true, targetexec.ErrProtocol
	}
	return true, s.commitSupervisionTargetHandoff(ctx, source, prior, receipt, lease)
}

func (s *Server) recoverSupervisionTargetHandoffGrant(ctx context.Context, link *supervisionTargetLink, source supervisionWorkspaceSource, h supervisionTargetHandoff) error {
	response, err := link.Call(ctx, targetexec.Request{ID: uuid.NewString(), Method: "query", SessionID: h.Payload.Spec.SessionID})
	if err != nil {
		return err
	}
	actual := response.Session
	if actual == nil || !sameSupervisionTargetAuthority(actual.Control, *source.Control) || actual.Released || !actual.Claimed || actual.Finalization != nil {
		return targetexec.ErrAuthority
	}
	for _, execution := range actual.Executions {
		if execution.State != "quiescent" {
			return errWorkspaceNotQuiescent
		}
	}
	return s.withSupervisionTx(ctx, h.Payload.Spec.SessionID, time.Now().UTC(), func(tx *sql.Tx, admissions map[string]WorkspaceAdmission, ss *supervisionSession) error {
		if ss.StateVersion != h.StateVersion || ss.Control != "" || ss.RecoveryState != "ready" {
			return errSupervisionState
		}
		if err := source.verifyTx(ctx, s, tx, ss, admissions[ss.AdmissionID]); err != nil {
			return err
		}
		ss.RecoveryState, ss.Reason = "reconciling", "target_finalization_unconfirmed"
		ss.StateVersion++
		return appendSupervisionEventTx(ctx, tx, ss, "target.finalization_recovery", map[string]string{"id": h.ID}, time.Now().UTC())
	})
}

func (s *Server) commitSupervisionTargetHandoff(ctx context.Context, source supervisionWorkspaceSource, h supervisionTargetHandoff, receipt targetexec.Finalization, lease supervisionLease) error {
	return s.withSupervisionTx(ctx, h.Payload.Spec.SessionID, time.Now().UTC(), func(tx *sql.Tx, admissions map[string]WorkspaceAdmission, ss *supervisionSession) error {
		var state, saved, spec, checks string
		if err := tx.QueryRowContext(ctx, `select state,receipt,spec,checks from supervision_target_finalizations where id=? and session_id=?`, h.ID, ss.ID).Scan(&state, &saved, &spec, &checks); err != nil {
			return err
		}
		if spec != string(mustJSON(h.Payload.Spec)) || checks != string(mustJSON(h.Payload.Checks)) {
			return targetexec.ErrConflict
		}
		if state == "committed" && saved == string(mustJSON(receipt)) {
			return nil // Retry after the business transaction committed.
		}
		if state != "pending" || ss.StateVersion != h.StateVersion || ss.AdmissionEpoch != h.AdmissionEpoch || ss.State != "finalizing" || ss.Reason != "target_finalization" || ss.Control != "" || supervisionTimeAvailable(ss, "final") <= 0 {
			return errSupervisionState
		}
		if receipt.Spec != h.Payload.Spec || receipt.State != "sealed" || receipt.Digest != supervisionSHA256(mustJSON(h.Payload.Spec)) || receipt.ObservedAt.IsZero() {
			return targetexec.ErrProtocol
		}
		if err := requireSupervisionLease(ss, lease, time.Now().UTC()); err != nil {
			return err
		}
		if err := source.verifyTx(ctx, s, tx, ss, admissions[ss.AdmissionID]); err != nil {
			return err
		}
		intent, err := loadSupervisionTargetControl(ctx, tx, ss.ID)
		if err != nil {
			return err
		}
		if intent.Wire == nil || receipt.ObservedAt.After(intent.Wire.AuthorizedUntil) || intent.Control.OwnerEpoch != h.Payload.Spec.OwnerEpoch || intent.Control.Version != h.Payload.Spec.AuthorizationVersion {
			return targetexec.ErrAuthority
		}
		pc, err := loadSupervisionProjectContext(ctx, tx, ss.ID, "current")
		if err != nil {
			return err
		}
		binding, err := supervisionTargetHandoffBinding(ctx, tx, ss, source, pc)
		if err != nil {
			return err
		}
		if binding != h.Payload.Spec.BindingSHA256 {
			return errSupervisionState
		}
		accepted, issues, err := supervisionTargetHandoffReadyTx(ctx, tx, ss, pc)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `update supervision_target_finalizations set state='committed',receipt=? where id=? and state='pending'`, string(mustJSON(receipt)), h.ID); err != nil {
			return err
		}
		if err := s.commitSupervisionTasksTx(ctx, tx, ss, pc, accepted, issues); err != nil {
			return err
		}
		admission := admissions[ss.AdmissionID]
		admission.State, admission.UpdatedAt = "releasing", time.Now().UTC()
		if _, err := tx.ExecContext(ctx, `update workspace_admissions set state='releasing',updated_at=? where id=?`, admission.UpdatedAt, admission.ID); err != nil {
			return err
		}
		admissions[admission.ID] = admission
		return appendSupervisionEventTx(ctx, tx, ss, "target.finalization_committed", map[string]string{"id": h.ID, "digest": receipt.Digest}, time.Now().UTC())
	})
}

func (s *Server) supersedeSupervisionTargetHandoff(ctx context.Context, link *supervisionTargetLink, source supervisionWorkspaceSource, h supervisionTargetHandoff) error {
	c := source.Control
	if c.Version <= h.Payload.Spec.AuthorizationVersion {
		// A changed evidence watermark without a domain version must still
		// revoke the old grant before abandoning its durable receipt.
		return s.withSupervisionTx(ctx, c.SessionID, time.Now().UTC(), func(tx *sql.Tx, admissions map[string]WorkspaceAdmission, ss *supervisionSession) error {
			if err := source.verifyTx(ctx, s, tx, ss, admissions[ss.AdmissionID]); err != nil {
				return err
			}
			ss.StateVersion++
			return appendSupervisionEventTx(ctx, tx, ss, "target.finalization_superseded", map[string]string{"id": h.ID}, time.Now().UTC())
		})
	}
	response, err := link.Call(ctx, targetexec.Request{ID: uuid.NewString(), Method: "query", SessionID: c.SessionID})
	if err != nil {
		return err
	}
	actual := response.Session
	if actual == nil || !sameSupervisionTargetAuthority(actual.Control, *c) || !actual.Claimed || actual.Released || actual.RecoveryRequired {
		return targetexec.ErrAuthority
	}
	if active := actual.Finalization; active != nil {
		if active.Spec != h.Payload.Spec {
			return targetexec.ErrConflict
		}
		if _, err := link.Call(ctx, targetexec.Request{ID: uuid.NewString(), Method: "finalize.clear", SessionID: c.SessionID, WorkspaceID: c.WorkspaceID, OwnerEpoch: c.OwnerEpoch, AuthorizationVersion: c.Version, Parameters: mustJSON(h.Payload)}); err != nil {
			return err
		}
	}
	return s.withSupervisionTx(ctx, c.SessionID, time.Now().UTC(), func(tx *sql.Tx, admissions map[string]WorkspaceAdmission, ss *supervisionSession) error {
		if err := source.verifyTx(ctx, s, tx, ss, admissions[ss.AdmissionID]); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `update supervision_target_finalizations set state='abandoned' where id=? and session_id=? and state='pending'`, h.ID, ss.ID)
		return err
	})
}
