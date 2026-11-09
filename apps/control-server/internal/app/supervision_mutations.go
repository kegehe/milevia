package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

func supervisionScopeContains(scopes []string, name string) bool {
	for _, scope := range scopes {
		if name == scope || strings.HasPrefix(name, scope+"/") {
			return true
		}
	}
	return false
}

func validateSupervisionEdits(edits []supervisionEdit, plan supervisionTaskPlan, pc supervisionProjectContext) error {
	if len(edits) > 100 {
		return errors.New("单轮文件变更超过100项")
	}
	seen := map[string]bool{}
	entries := map[string]supervisionSnapshotEntry{}
	for _, entry := range pc.Manifest.Entries {
		entries[entry.Path] = entry
	}
	for _, e := range edits {
		key := e.Path
		if supervisionManifestWindows(pc.Manifest) {
			key = strings.ToLower(key)
		}
		if !supervisionScopeContains(plan.Scope, e.Path) || seen[key] || supervisionIsRule(e.Path) {
			return errors.New("文件变更越出范围、重复或尝试修改项目规则")
		}
		seen[key] = true
		probe := plan
		probe.Scope = []string{e.Path}
		if err := validateSupervisionPlan(&probe, pc.Manifest); err != nil {
			return err
		}
		previous, exists := entries[e.Path]
		if exists && (previous.Kind != "file" || previous.Gap != "" || previous.SHA256 != e.BaseSHA256) {
			return errors.New("文件原始版本不匹配或不可安全修改")
		}
		if !exists && (e.BaseSHA256 != "" || e.Delete) {
			return errors.New("新文件不能带已有版本或删除指令")
		}
		if len(e.Content) > supervisionSnapshotFileLimit || strings.IndexByte(e.Content, 0) >= 0 || redactAgentText(e.Content) != e.Content {
			return errors.New("文件内容超限、为二进制或包含敏感内容")
		}
		if e.Delete && e.Content != "" {
			return errors.New("删除文件不能同时写入内容")
		}
	}
	return nil
}

func supervisionRootPathSafe(root *os.Root, name string) error {
	for prefix := path.Dir(name); prefix != "."; prefix = path.Dir(prefix) {
		info, err := root.Lstat(filepath.FromSlash(prefix))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("文件变更祖先不是普通目录")
		}
	}
	return nil
}

func (s *Server) applySupervisionEdits(ctx context.Context, permit supervisionExecutionPermit, j supervisionRoleJob, edits []supervisionEdit) (revision string, resultErr error) {
	ss, err := loadSupervisionSession(ctx, s.db, permit.Decision.SessionID)
	if err != nil {
		return "", err
	}
	pc, err := loadSupervisionProjectContext(ctx, s.db, ss.ID, "current")
	if err != nil {
		return "", err
	}
	if pc.RevisionID != j.InputRevision {
		return "", errSupervisionSnapshotChanged
	}
	var plan supervisionPlanVersion
	var admission WorkspaceAdmission
	var source supervisionWorkspaceSource
	err = s.withSupervisionTx(ctx, ss.ID, time.Now().UTC(), func(tx *sql.Tx, next map[string]WorkspaceAdmission, current *supervisionSession) error {
		if current.Control != "" || current.StateVersion != permit.Decision.ExpectedStateVersion {
			return errSupervisionState
		}
		if _, err := checkSupervisionExecutionTx(ctx, tx, current, permit); err != nil {
			return err
		}
		var ok bool
		admission, ok = next[current.AdmissionID]
		if !ok || admission.State != "owned" || admission.Epoch != current.AdmissionEpoch || s.admissionHasOperationsLocked(admission) {
			return errWorkspaceNotQuiescent
		}
		var err error
		plan, err = supervisionTaskPlanTx(ctx, tx, current.ID, j.TaskID)
		if err != nil {
			return err
		}
		source, err = s.supervisionWorkspaceSourceTx(ctx, tx, current, admission)
		if err != nil {
			return err
		}
		return validateSupervisionEdits(edits, plan.Plan, pc)
	})
	if err != nil {
		return "", err
	}
	observed, err := source.collect(ctx, s)
	if err != nil {
		return "", err
	}
	if observed.RevisionID != pc.RevisionID || observed.RulesHash != pc.RulesHash {
		return "", errSupervisionSnapshotChanged
	}
	if len(edits) == 0 {
		return pc.RevisionID, nil
	}
	if source.Control != nil {
		return s.applySupervisionTargetEdits(ctx, source, permit, j, plan.Plan, pc, observed, edits)
	}
	raw, _ := json.Marshal(edits)
	err = s.withSupervisionTx(ctx, ss.ID, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, current *supervisionSession) error {
		if current.Control != "" || current.StateVersion != permit.Decision.ExpectedStateVersion {
			return errSupervisionState
		}
		if _, err := checkSupervisionExecutionTx(ctx, tx, current, permit); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `insert into supervision_mutations(execution_id,session_id,state,edits,created_at) values (?,?,'applying',?,?)`, j.ExecutionID, ss.ID, string(raw), time.Now().UTC())
		return err
	})
	if err != nil {
		return "", err
	}
	defer func() {
		settleCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := s.finishSupervisionMutation(settleCtx, ss.ID, j.ExecutionID, revision, edits, resultErr)
		resultErr = errors.Join(resultErr, err)
	}()
	root, err := os.OpenRoot(admission.OriginalPath)
	if err != nil {
		return "", err
	}
	defer root.Close()
	for _, e := range edits {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err := verifyWorkspaceAdmissionPath(admission); err != nil {
			return "", err
		}
		if err := supervisionRootPathSafe(root, e.Path); err != nil {
			return "", err
		}
		name := filepath.FromSlash(e.Path)
		info, statErr := root.Lstat(name)
		mode := os.FileMode(0644)
		if statErr == nil {
			if !info.Mode().IsRegular() {
				return "", errors.New("写入目标被替换为链接或特殊文件")
			}
			data, err := root.ReadFile(name)
			if err != nil {
				return "", err
			}
			if supervisionSHA256(data) != e.BaseSHA256 {
				return "", errSupervisionSnapshotChanged
			}
			mode = info.Mode().Perm()
		} else if !errors.Is(statErr, os.ErrNotExist) || e.BaseSHA256 != "" {
			return "", errSupervisionSnapshotChanged
		}
		if e.Delete {
			if err := root.Remove(name); err != nil {
				return "", err
			}
			continue
		}
		if err := root.MkdirAll(filepath.Dir(name), 0755); err != nil {
			return "", err
		}
		temp := filepath.Join(filepath.Dir(name), ".milevia-write-"+uuid.NewString())
		file, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return "", err
		}
		_, writeErr := file.Write([]byte(e.Content))
		syncErr := file.Sync()
		closeErr := file.Close()
		if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
			_ = root.Remove(temp)
			return "", err
		}
		if err := root.Rename(temp, name); err != nil {
			_ = root.Remove(temp)
			return "", err
		}
	}
	after, err := collectSupervisionWorkspace(ctx, admission, s.checkpointStorageRoot())
	if err != nil {
		return "", err
	}
	if err := validateSupervisionMutationResult(observed, after, edits); err != nil {
		return "", err
	}
	return after.RevisionID, nil
}

func validateSupervisionMutationResult(observed, after supervisionCollectedContext, edits []supervisionEdit) error {
	if after.RulesHash != observed.RulesHash {
		return errSupervisionSnapshotChanged
	}
	allowed := map[string]supervisionEdit{}
	for _, e := range edits {
		allowed[e.Path] = e
	}
	beforeEntries := map[string]supervisionSnapshotEntry{}
	for _, e := range observed.Manifest.Entries {
		beforeEntries[e.Path] = e
	}
	for _, entry := range after.Manifest.Entries {
		previous, existed := beforeEntries[entry.Path]
		delete(beforeEntries, entry.Path)
		if previous == entry {
			continue
		}
		if edit, ok := allowed[entry.Path]; ok {
			if edit.Delete || entry.Kind != "file" || entry.Gap != "" || entry.SHA256 != supervisionSHA256([]byte(edit.Content)) {
				return errSupervisionSnapshotChanged
			}
			continue
		}
		if !existed && entry.Kind == "directory" {
			parentOfEdit := false
			for _, edit := range edits {
				if strings.HasPrefix(edit.Path, entry.Path+"/") {
					parentOfEdit = true
				}
			}
			if parentOfEdit {
				continue
			}
		}
		return errSupervisionSnapshotChanged
	}
	for name := range beforeEntries {
		edit, ok := allowed[name]
		if !ok || !edit.Delete {
			return errSupervisionSnapshotChanged
		}
	}
	return nil
}

// Called only after local file handles have closed and the writer has ended.
func (s *Server) finishSupervisionMutation(ctx context.Context, sessionID, executionID, revision string, edits []supervisionEdit, resultErr error) error {
	return s.withSupervisionTx(ctx, sessionID, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, current *supervisionSession) error {
		state := "completed"
		if resultErr != nil {
			state = "unknown"
			if current.Control != "stop" {
				current.State, current.Reason, current.ClockAt = "needs_input", "file_application_incomplete", nil
			}
			current.StateVersion++
		}
		if _, err := tx.ExecContext(ctx, `update supervision_mutations set state=? where execution_id=?`, state, executionID); err != nil {
			return err
		}
		return appendSupervisionEventTx(ctx, tx, current, "files."+state, map[string]any{"executionId": executionID, "paths": editsPaths(edits), "revisionId": revision}, time.Now().UTC())
	})
}

func editsPaths(edits []supervisionEdit) []string {
	out := make([]string, 0, len(edits))
	for _, e := range edits {
		out = append(out, e.Path)
	}
	return out
}
