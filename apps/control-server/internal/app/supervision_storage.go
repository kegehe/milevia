package app

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

const supervisionStorageLimit int64 = 2 << 30

func supervisionCASHashesTx(ctx context.Context, tx *sql.Tx, id string) (map[string]int64, error) {
	rows, err := tx.QueryContext(ctx, `select distinct b.sha256,b.size from supervision_project_contexts c join supervision_revisions r on r.id=c.revision_id,json_each(r.manifest,'$.entries') e join supervision_revision_blobs b on b.sha256=json_extract(e.value,'$.sha256') where c.session_id=? and coalesce(json_extract(e.value,'$.gap'),'')=''`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hashes := map[string]int64{}
	for rows.Next() {
		var hash string
		var size int64
		if err := rows.Scan(&hash, &size); err != nil {
			return nil, err
		}
		hashes[hash] = size
	}
	return hashes, rows.Err()
}

func checkSupervisionCASBudgetTx(ctx context.Context, tx *sql.Tx, id string, blobs map[string][]byte) error {
	hashes, err := supervisionCASHashesTx(ctx, tx, id)
	if err != nil {
		return err
	}
	total, err := supervisionAdmittedBytes(ctx, tx, id)
	if err != nil {
		return err
	}
	ss, err := loadSupervisionSession(ctx, tx, id)
	if err != nil {
		return err
	}
	for hash, data := range blobs {
		if _, exists := hashes[hash]; !exists {
			total += int64(len(data))
		}
	}
	if total > ss.Budget.storageLimit() {
		return errSupervisionStorage
	}
	return nil
}

// Logical payload bytes are bounded per session; SQLite pages, indexes and WAL are shared.
// Keep a separate reserve so stopping and settling facts remain possible at the limit.
const supervisionStorageControlReserve int64 = 8 << 20
const supervisionDiskReserve int64 = 256 << 20

var errSupervisionStorage = errors.New("托管存储预算不足，请增加预算或归档后重新托管")
var errSupervisionDisk = errors.New("托管数据磁盘余量不足，请释放空间后继续")

func (b supervisionBudget) storageLimit() int64 {
	if b.MaxStorageBytes > 0 {
		return b.MaxStorageBytes
	}
	return supervisionStorageLimit
}

func supervisionStoredBytes(ctx context.Context, q sqlQueryer, id string) (int64, error) {
	// CAST AS BLOB is essential: SQLite length(TEXT) counts characters, not UTF-8 bytes.
	parts := []struct{ table, fields, where string }{
		{"supervision_requests", "scope,request_key,request_hash", "session_id=?"},
		{"supervision_sessions", "budget,role_config,route_snapshot,reason", "id=?"},
		{"supervision_tasks", "task_snapshot,dependency_snapshot", "session_id=?"},
		{"supervision_events", "payload", "session_id=?"},
		{"supervision_decisions", "output,validation_error", "session_id=?"},
		{"supervision_work_items", "result_text,result_error", "session_id=?"},
		{"supervision_executions", "receipt,error", "session_id=?"},
		{"supervision_execution_specs", "spec", "execution_id in (select execution_id from supervision_work_items where session_id=?)"},
		{"supervision_target_executions", "target_id,spec,native_session_id", "session_id=?"},
		{"supervision_target_cursors", "target_id", "session_id=?"},
		{"supervision_target_inbox", "event_key,execution_id,sha256", "session_id=?"},
		{"supervision_target_usage", "counters", "session_id=?"},
		{"supervision_target_resolutions", "resolution", "session_id=?"},
		{"supervision_target_releases", "target_id,workspace_id,receipt", "session_id=?"},
		{"supervision_target_finalizations", "spec,checks,receipt", "session_id=?"},
		{"supervision_target_controls", "runner_id,connection_key,control_json,basis,wire_control,renewal_json", "session_id=?"},
		{"supervision_scheduler_inputs", "input_json", "execution_id in (select execution_id from supervision_work_items where session_id=?)"},
		{"supervision_role_jobs", "spec,result,error,completion_input_sha256", "session_id=?"},
		{"supervision_role_observations", "expected_revision,state", "session_id=?"},
		{"supervision_evidence", "details", "session_id=?"},
		{"supervision_plan_versions", "plan_json,reason", "session_id=?"},
		{"supervision_project_contexts", "rules_json", "session_id=?"},
		{"supervision_revisions", "manifest", "id in (select revision_id from supervision_project_contexts where session_id=?)"},
		{"supervision_source_requests", "paths", "session_id=?"},
		{"supervision_mutations", "edits", "session_id=?"},
		{"supervision_target_mutations", "spec,receipt", "session_id=?"},
		{"supervision_amendments", "content", "session_id=?"},
		{"supervision_questions", "question,answer,requirements_snapshot", "session_id=?"},
		{"supervision_dependency_state", "facts", "session_id=?"},
		{"supervision_retry_state", "error", "session_id=?"},
		{"supervision_wait_state", "condition,fingerprint", "session_id=?"},
		{"supervision_action_results", "error", "decision_id in (select id from supervision_decisions where session_id=?)"},
		{"events", "payload", "run_id in (select run_id from supervision_run_links where session_id=?)"},
		{"messages", "content", "run_id in (select run_id from supervision_run_links where session_id=?)"},
		{"task_runs", "prompt_snapshot,acceptance_snapshot", "execution_owner_id=? and execution_source='supervision'"},
	}
	queries := []string{}
	args := []any{}
	for _, part := range parts {
		fields := []string{}
		for _, field := range strings.Split(part.fields, ",") {
			fields = append(fields, "coalesce(length(cast("+field+" as blob)),0)")
		}
		queries = append(queries, "select coalesce(sum("+strings.Join(fields, "+")+"),0) as bytes from "+part.table+" where "+part.where)
		args = append(args, id)
	}
	queries = append(queries, `select coalesce(sum(size),0) as bytes from supervision_artifacts where session_id=? and state='present'`)
	args = append(args, id)
	queries = append(queries, `select coalesce(sum(size),0) as bytes from supervision_revision_blobs where sha256 in (select json_extract(e.value,'$.sha256') from supervision_project_contexts c join supervision_revisions r on r.id=c.revision_id,json_each(r.manifest,'$.entries') e where c.session_id=? and coalesce(json_extract(e.value,'$.gap'),'')='')`)
	args = append(args, id)
	var total int64
	err := q.QueryRowContext(ctx, "select sum(bytes) from ("+strings.Join(queries, " union all ")+")", args...).Scan(&total)
	return total, err
}

func supervisionReservedBytes(ctx context.Context, q sqlQueryer, id string) (int64, error) {
	var size int64
	err := q.QueryRowContext(ctx, `select coalesce(sum(bytes),0) from supervision_storage_reservations where session_id=?`, id).Scan(&size)
	return size, err
}
func supervisionAdmittedBytes(ctx context.Context, q sqlQueryer, id string) (int64, error) {
	size, err := supervisionStoredBytes(ctx, q, id)
	if err != nil {
		return 0, err
	}
	reserved, err := supervisionReservedBytes(ctx, q, id)
	return size + reserved, err
}

func (s *Server) supervisionDiskAvailable(required int64) error {
	paths := []string{filepath.Dir(s.config.DatabasePath), s.checkpointStorageRoot()}
	for _, path := range paths {
		available, err := supervisionDiskFree(path)
		if err != nil {
			return err
		}
		if available < uint64(supervisionDiskReserve+required) {
			return errSupervisionDisk
		}
	}
	return nil
}

func (s *Server) pauseSupervisionForStorage(ctx context.Context, id, reason string) error {
	// This small control transaction intentionally bypasses payload admission.
	return s.withWorkspaceAdmissionTx(ctx, func(tx *sql.Tx, _ map[string]WorkspaceAdmission) error {
		ss, err := loadSupervisionSession(ctx, tx, id)
		if err != nil {
			return err
		}
		if ss.Control != "" || supervisionTerminal(ss.State) || ss.State == "draft" {
			return nil
		}
		now := time.Now().UTC()
		accountSupervisionTime(&ss, now)
		if err := suspendSupervisionQueuedTx(ctx, tx, &ss, false, now); err != nil {
			return err
		}
		ss.Control, ss.State, ss.Reason = "pause", "pausing", reason
		ss.StateVersion++
		if err := appendSupervisionEventTx(ctx, tx, &ss, "storage.paused", map[string]string{"reason": reason}, now); err != nil {
			return err
		}
		return saveSupervisionSessionTx(ctx, tx, &ss)
	})
}

func (s *Server) checkSupervisionStorage(ctx context.Context, id string) error {
	ss, err := loadSupervisionSession(ctx, s.db, id)
	if err != nil {
		return err
	}
	if (ss.State != "running" && ss.State != "finalizing") || ss.Control != "" {
		return nil
	}
	total, err := supervisionAdmittedBytes(ctx, s.db, id)
	if err != nil {
		return err
	}
	reason := ""
	if total >= ss.Budget.storageLimit() {
		reason = "storage_limit"
	} else if err := s.supervisionDiskAvailable(0); err != nil {
		if !errors.Is(err, errSupervisionDisk) {
			return err
		}
		reason = "disk_space_low"
	}
	if reason != "" {
		return s.pauseSupervisionForStorage(ctx, id, reason)
	}
	return nil
}

func (s *Server) registerSupervisionArtifacts(ctx context.Context, sessionID, executionID string) error {
	type artifact struct {
		name string
		size int64
		hash string
	}
	items := []artifact{}
	for _, asset := range []string{"screenshot.png", "report.json"} {
		name := filepath.Join(s.checkpointStorageRoot(), "supervision", "artifacts", executionID, asset)
		info, err := os.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() > 20<<20 {
			return errors.New("浏览器产物类型或大小超限")
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		items = append(items, artifact{asset, int64(len(data)), supervisionSHA256(data)})
	}
	return s.withSupervisionTx(ctx, sessionID, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, _ *supervisionSession) error {
		if _, err := tx.ExecContext(ctx, `delete from supervision_storage_reservations where execution_id=? and session_id=?`, executionID, sessionID); err != nil {
			return err
		}
		for _, item := range items {
			result, err := tx.ExecContext(ctx, `insert into supervision_artifacts(execution_id,session_id,asset,size,sha256,state) values (?,?,?,?,?,'present') on conflict(execution_id,asset) do update set sha256=excluded.sha256 where supervision_artifacts.session_id=excluded.session_id and supervision_artifacts.size=excluded.size and supervision_artifacts.sha256=excluded.sha256 and supervision_artifacts.state='present'`, executionID, sessionID, item.name, item.size, item.hash)
			if err != nil {
				return err
			}
			if rows, err := result.RowsAffected(); err != nil {
				return err
			} else if rows != 1 {
				return errors.New("浏览器产物与已登记内容不一致或已被清理")
			}
		}
		return nil
	})
}

// 只清理用户指定终态会话的大产物，结构化报告及内容哈希继续保留。
func (s *Server) cleanSupervisionArtifactsHTTP(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "supervisionID")
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	defer tx.Rollback()
	ss, err := loadSupervisionSession(r.Context(), tx, id)
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	if err := requireSupervisionReleasedTx(r.Context(), tx, ss); err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	rows, err := tx.QueryContext(r.Context(), `select execution_id,asset from supervision_artifacts where session_id=? and state='present'`, id)
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	type item struct{ execution, asset string }
	var items []item
	for rows.Next() {
		var value item
		if err = rows.Scan(&value.execution, &value.asset); err != nil {
			break
		}
		items = append(items, value)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	// Terminal sessions cannot reacquire a workspace or start another role.
	// Finish the consistent read before touching disk or using s.db again.
	if err := tx.Commit(); err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	for _, value := range items {
		if value.asset != "screenshot.png" && value.asset != "report.json" {
			writeSupervisionHTTPError(w, errors.New("产物清单无效"))
			return
		}
		if err := os.Remove(filepath.Join(s.checkpointStorageRoot(), "supervision", "artifacts", value.execution, value.asset)); err != nil && !errors.Is(err, os.ErrNotExist) {
			writeSupervisionHTTPError(w, err)
			return
		}
		if _, err := s.db.ExecContext(r.Context(), `update supervision_artifacts set state='cleaned' where execution_id=? and asset=?`, value.execution, value.asset); err != nil {
			writeSupervisionHTTPError(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
