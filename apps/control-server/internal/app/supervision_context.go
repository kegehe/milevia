package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

type supervisionProjectContext struct {
	ID         string                       `json:"id"`
	RevisionID string                       `json:"revisionId"`
	RulesHash  string                       `json:"rulesHash"`
	Rules      []supervisionSourceRule      `json:"rules"`
	Manifest   supervisionWorkspaceManifest `json:"manifest"`
}

func (s *Server) migrateSupervisionContext(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `create table if not exists supervision_revision_blobs (
		sha256 text primary key, content blob not null, size integer not null, check(length(content)=size)
	);
	create table if not exists supervision_revisions (
		id text primary key, manifest text not null, created_at datetime not null
	);
	create table if not exists supervision_project_contexts (
		id text primary key, session_id text not null references supervision_sessions(id) on delete cascade,
		revision_id text not null references supervision_revisions(id), rules_hash text not null, rules_json text not null,
		admission_epoch integer not null, created_at datetime not null,
		unique(session_id,id)
	);
	create table if not exists supervision_context_state (
		session_id text primary key references supervision_sessions(id) on delete cascade,
		baseline_id text not null, current_id text not null,
		foreign key(session_id,baseline_id) references supervision_project_contexts(session_id,id),
		foreign key(session_id,current_id) references supervision_project_contexts(session_id,id)
	);
	create table if not exists supervision_context_captures (
		id text primary key, session_id text not null references supervision_sessions(id) on delete cascade,
		state text not null check(state in ('collecting','completed','failed')), created_at datetime not null, completed_at datetime
	);
	create unique index if not exists supervision_context_capture_active on supervision_context_captures(session_id) where state='collecting';`)
	if err != nil {
		return err
	}
	if err := ensureColumn(ctx, s.db, "supervision_context_state", "pending_base_id", "text not null default ''"); err != nil {
		return err
	}
	// 旧版已把未接受的观察写入 current。若该观察后没有恢复确认，
	// 保守以启动基线作为待确认依据，不能在升级时将观察自动当作授权。
	if _, err := s.db.ExecContext(ctx, `update supervision_context_state set pending_base_id=baseline_id where pending_base_id='' and exists(
		select 1 from supervision_events e where e.session_id=supervision_context_state.session_id and e.type='context.captured'
		and json_extract(e.payload,'$.contextId')=supervision_context_state.current_id and json_extract(e.payload,'$.drift')=1
		and not exists(select 1 from supervision_events later where later.session_id=e.session_id and later.seq>e.seq and later.type='session.resumed' and json_extract(later.payload,'$.acceptedDrift')=1))`); err != nil {
		return err
	}
	if err := s.migrateSupervisionPlans(ctx); err != nil {
		return err
	}
	return s.migrateSupervisionWorkflow(ctx)
}

func loadSupervisionProjectContext(ctx context.Context, q sqlQueryer, sessionID, which string) (supervisionProjectContext, error) {
	var out supervisionProjectContext
	column := "current_id"
	if which == "baseline" {
		column = "baseline_id"
	} else if which == "pending" {
		column = "pending_base_id"
	}
	var rules, manifest string
	err := q.QueryRowContext(ctx, `select c.id,c.revision_id,c.rules_hash,c.rules_json,r.manifest from supervision_context_state st join supervision_project_contexts c on c.id=st.`+column+` and c.session_id=st.session_id join supervision_revisions r on r.id=c.revision_id where st.session_id=?`, sessionID).Scan(&out.ID, &out.RevisionID, &out.RulesHash, &rules, &manifest)
	if err != nil {
		return out, err
	}
	if supervisionSHA256([]byte(manifest)) != out.RevisionID || supervisionSHA256([]byte(rules)) != out.RulesHash {
		return out, errors.New("托管项目上下文哈希不匹配")
	}
	if err := json.Unmarshal([]byte(rules), &out.Rules); err != nil {
		return out, err
	}
	if err := json.Unmarshal([]byte(manifest), &out.Manifest); err != nil {
		return out, err
	}
	return out, nil
}

// 采集只读取文件，不运行 Git/CLI。持久 collecting 保护暂停/停止与并发派发；
// 文件 I/O 在领域 gate 外，提交时重新检查版本、目录身份与恢复代际。
func (s *Server) captureSupervisionProjectContext(ctx context.Context, sessionID string, version int64, authorizedChange ...string) (out supervisionProjectContext, resultErr error) {
	captureID := uuid.NewString()
	var admission WorkspaceAdmission
	var source supervisionWorkspaceSource
	var epoch int64
	err := s.withSupervisionTx(ctx, sessionID, time.Now().UTC(), func(tx *sql.Tx, next map[string]WorkspaceAdmission, ss *supervisionSession) error {
		if err := requireSupervisionVersion(ss, version); err != nil {
			return err
		}
		if ss.Control != "" || ss.RecoveryState != "ready" || (ss.State != "running" && ss.State != "finalizing") {
			return errSupervisionState
		}
		if err := requireSupervisionNoPendingDrift(ctx, tx, sessionID); err != nil {
			return err
		}
		var ok bool
		admission, ok = next[ss.AdmissionID]
		if !ok || admission.OwnerID != ss.ID || admission.Epoch != ss.AdmissionEpoch || admission.State != "owned" || admission.RecoveryState != "ready" {
			return errWorkspaceAdmissionStale
		}
		if s.admissionHasOperationsLocked(admission) {
			return errWorkspaceNotQuiescent
		}
		if err := supervisionRunsQuiescentTx(ctx, tx, ss.ID); err != nil {
			return err
		}
		var err error
		source, err = s.supervisionWorkspaceSourceTx(ctx, tx, ss, admission)
		if err != nil {
			return err
		}
		epoch = ss.LeaseEpoch
		_, err = tx.ExecContext(ctx, `insert into supervision_context_captures(id,session_id,state,created_at) values (?,?,'collecting',?)`, captureID, ss.ID, time.Now().UTC())
		return err
	})
	if err != nil {
		return out, err
	}
	defer func() {
		if resultErr == nil {
			return
		}
		settleCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cleanupErr := s.withSupervisionTx(settleCtx, sessionID, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
			res, err := tx.ExecContext(settleCtx, `update supervision_context_captures set state='failed',completed_at=? where id=? and session_id=? and state='collecting'`, time.Now().UTC(), captureID, sessionID)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil || n == 0 {
				return err
			}
			if ss.Control == "" && ss.RecoveryState == "ready" && ss.StateVersion == version && ss.LeaseEpoch == epoch {
				ss.State, ss.Reason, ss.ClockAt = "needs_input", "workspace_capture_failed", nil
				ss.StateVersion++
			}
			return appendSupervisionEventTx(settleCtx, tx, ss, "context.capture_failed", map[string]string{"captureId": captureID, "error": redactAgentText(resultErr.Error())}, time.Now().UTC())
		})
		resultErr = errors.Join(resultErr, cleanupErr)
	}()
	collected, err := source.collect(ctx, s)
	if err != nil {
		return out, err
	}
	contextID := uuid.NewString()
	var drift bool
	err = s.withSupervisionTx(ctx, sessionID, time.Now().UTC(), func(tx *sql.Tx, next map[string]WorkspaceAdmission, ss *supervisionSession) error {
		if err := requireSupervisionVersion(ss, version); err != nil {
			return err
		}
		if ss.LeaseEpoch != epoch || ss.Control != "" || ss.RecoveryState != "ready" || (ss.State != "running" && ss.State != "finalizing") {
			return errSupervisionLease
		}
		currentAdmission, ok := next[ss.AdmissionID]
		if !ok || currentAdmission != admission {
			return errWorkspaceAdmissionStale
		}
		if err := source.verifyTx(ctx, s, tx, ss, currentAdmission); err != nil {
			return err
		}
		var collecting bool
		if err := tx.QueryRowContext(ctx, `select exists(select 1 from supervision_context_captures where id=? and session_id=? and state='collecting')`, captureID, sessionID).Scan(&collecting); err != nil {
			return err
		}
		if !collecting {
			return errSupervisionState
		}
		current, err := loadSupervisionProjectContext(ctx, tx, sessionID, "current")
		if err == nil && current.RevisionID == collected.RevisionID && current.RulesHash == collected.RulesHash {
			out = current // 相同物理事实不产生新版本/事件，不使自身决策过期。
			_, err = tx.ExecContext(ctx, `update supervision_context_captures set state='completed',completed_at=? where id=?`, time.Now().UTC(), captureID)
			return err
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		drift = err == nil
		authorized := len(authorizedChange) > 0 && authorizedChange[0] == collected.RevisionID && current.RulesHash == collected.RulesHash
		manifest, _ := json.Marshal(collected.Manifest)
		rules, _ := json.Marshal(collected.Rules)
		if err := checkSupervisionCASBudgetTx(ctx, tx, sessionID, collected.Blobs); err != nil {
			return err
		}
		for hash, data := range collected.Blobs {
			if _, err := tx.ExecContext(ctx, `insert into supervision_revision_blobs(sha256,content,size) values (?,?,?) on conflict(sha256) do nothing`, hash, data, len(data)); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `insert into supervision_revisions(id,manifest,created_at) values (?,?,?) on conflict(id) do nothing`, collected.RevisionID, string(manifest), time.Now().UTC()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `insert into supervision_project_contexts(id,session_id,revision_id,rules_hash,rules_json,admission_epoch,created_at) values (?,?,?,?,?,?,?)`, contextID, sessionID, collected.RevisionID, collected.RulesHash, string(rules), admission.Epoch, time.Now().UTC()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `insert into supervision_context_state(session_id,baseline_id,current_id) values (?,?,?) on conflict(session_id) do update set current_id=excluded.current_id`, sessionID, contextID, contextID); err != nil {
			return err
		}
		if drift && !authorized {
			if _, err := tx.ExecContext(ctx, `update supervision_context_state set pending_base_id=? where session_id=? and pending_base_id=''`, current.ID, sessionID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `update supervision_context_captures set state='completed',completed_at=? where id=?`, time.Now().UTC(), captureID); err != nil {
			return err
		}
		out = supervisionProjectContext{ID: contextID, RevisionID: collected.RevisionID, RulesHash: collected.RulesHash, Rules: collected.Rules, Manifest: collected.Manifest}
		ss.StateVersion++
		if err := suspendSupervisionQueuedTx(ctx, tx, ss, false, time.Now().UTC()); err != nil {
			return err
		}
		if drift && !authorized {
			ss.State, ss.Reason, ss.ClockAt = "needs_input", "workspace_or_rules_drift", nil
		}
		if authorized {
			drift = false
		}
		return appendSupervisionEventTx(ctx, tx, ss, "context.captured", map[string]any{"contextId": contextID, "revisionId": out.RevisionID, "rulesHash": out.RulesHash, "drift": drift}, time.Now().UTC())
	})
	if err == nil && drift {
		return out, errSupervisionSnapshotChanged // 漂移事实已保存，拒绝自动接纳为新授权。
	}
	return out, err
}

func requireSupervisionNoPendingDrift(ctx context.Context, q sqlQueryer, id string) error {
	var pending bool
	if err := q.QueryRowContext(ctx, `select exists(select 1 from supervision_context_state where session_id=? and pending_base_id<>'')`, id).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return errSupervisionSnapshotChanged
	}
	return nil
}

// 控制原因可被预算、用户暂停等替换；待接受的目录事实必须独立保留。
func (s *Server) fenceSupervisionPendingDrift(ctx context.Context, id string) error {
	if err := requireSupervisionNoPendingDrift(ctx, s.db, id); !errors.Is(err, errSupervisionSnapshotChanged) {
		return err
	}
	return s.withSupervisionTx(ctx, id, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		if ss.Control != "" || supervisionTerminal(ss.State) || ss.State == "draft" || ss.RecoveryState != "ready" || (ss.State == "needs_input" && ss.Reason == "workspace_or_rules_drift") {
			return nil
		}
		if err := requireSupervisionNoPendingDrift(ctx, tx, id); !errors.Is(err, errSupervisionSnapshotChanged) {
			return err
		}
		if err := suspendSupervisionQueuedTx(ctx, tx, ss, false, time.Now().UTC()); err != nil {
			return err
		}
		ss.State, ss.Reason, ss.ClockAt = "needs_input", "workspace_or_rules_drift", nil
		ss.StateVersion++
		return appendSupervisionEventTx(ctx, tx, ss, "context.acceptance_required", nil, time.Now().UTC())
	})
}

func (s *Server) supervisionRevisionContent(ctx context.Context, sessionID, revisionID, name string) ([]byte, error) {
	// 内部读取也按本会话拥有的清单授权，不能用全局 CAS 哈希跨项目取内容。
	var manifest string
	if err := s.db.QueryRowContext(ctx, `select r.manifest from supervision_revisions r where r.id=? and exists(select 1 from supervision_project_contexts c where c.revision_id=r.id and c.session_id=?)`, revisionID, sessionID).Scan(&manifest); err != nil {
		return nil, err
	}
	if supervisionSHA256([]byte(manifest)) != revisionID {
		return nil, errors.New("托管清单哈希不匹配")
	}
	var parsed supervisionWorkspaceManifest
	if err := json.Unmarshal([]byte(manifest), &parsed); err != nil {
		return nil, err
	}
	for _, entry := range parsed.Entries {
		if entry.Path != name || entry.Kind != "file" || entry.Gap != "" {
			continue
		}
		var data []byte
		if err := s.db.QueryRowContext(ctx, `select content from supervision_revision_blobs where sha256=?`, entry.SHA256).Scan(&data); err != nil {
			return nil, err
		}
		if supervisionSHA256(data) != entry.SHA256 || int64(len(data)) != entry.Size {
			return nil, errors.New("托管基线内容哈希不匹配")
		}
		return data, nil
	}
	return nil, sql.ErrNoRows
}
