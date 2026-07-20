package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const (
	gitOperationQueued         = "queued"
	gitOperationRunning        = "running"
	gitOperationSucceeded      = "succeeded"
	gitOperationFailed         = "failed"
	gitOperationCancelled      = "cancelled"
	gitOperationNeedsAttention = "needs_attention"
)

type GitOperation struct {
	ID             string     `json:"id"`
	ProjectID      string     `json:"projectId"`
	Type           string     `json:"type"`
	Status         string     `json:"status"`
	RequestSummary string     `json:"requestSummary"`
	BeforeState    string     `json:"beforeState"`
	AfterState     string     `json:"afterState"`
	ErrorCode      string     `json:"errorCode,omitempty"`
	ErrorMessage   string     `json:"errorMessage,omitempty"`
	RequestedAt    time.Time  `json:"requestedAt"`
	StartedAt      *time.Time `json:"startedAt,omitempty"`
	FinishedAt     *time.Time `json:"finishedAt,omitempty"`
}

type gitStateToken struct {
	projectID    string
	snapshot     GitSnapshot
	changes      []GitChange
	fingerprints map[string]string
	expiresAt    time.Time
}

type gitSummaryResponse struct {
	GitSnapshot
	ObservedAt time.Time `json:"observedAt"`
	StateToken string    `json:"stateToken"`
}

func (s *Server) migrateGit(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `create table if not exists git_operations (
	id text primary key,
	project_id text not null references projects(id) on delete cascade,
	type text not null,
	status text not null,
	request_summary text not null,
	before_state text not null default '{}',
	after_state text not null default '{}',
	error_code text not null default '',
	error_message text not null default '',
	output_excerpt text not null default '',
	executor_id text,
	lease_until datetime,
	requested_at datetime not null,
	started_at datetime,
	finished_at datetime
);
create index if not exists git_operations_project_requested on git_operations(project_id,requested_at desc);`)
	if err != nil {
		return fmt.Errorf("migrate Git operations: %w", err)
	}
	return nil
}

func (s *Server) recoverGitOperations(ctx context.Context) error {
	now := time.Now().UTC()
	rows, err := s.db.QueryContext(ctx, `select operations.id,operations.type,operations.after_state,projects.path from git_operations operations join projects on projects.id=operations.project_id where operations.status in ('queued','running')`)
	if err != nil {
		return fmt.Errorf("list interrupted Git operations: %w", err)
	}
	type interruptedOperation struct {
		id, typ, afterState, path string
	}
	operations := []interruptedOperation{}
	for rows.Next() {
		var operation interruptedOperation
		if err := rows.Scan(&operation.id, &operation.typ, &operation.afterState, &operation.path); err != nil {
			rows.Close()
			return fmt.Errorf("read interrupted Git operation: %w", err)
		}
		operations = append(operations, operation)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close interrupted Git operation list: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate interrupted Git operations: %w", err)
	}
	for _, operation := range operations {
		status, errorCode, errorMessage := gitOperationNeedsAttention, "result_unknown", "Control service restarted before the operation result was confirmed"
		if operation.typ == "commit" && recoveredGitCommit(ctx, operation.path, operation.afterState) {
			status, errorCode, errorMessage = gitOperationSucceeded, "", ""
		}
		if operation.typ == "fetch" && recoveredGitFetch(ctx, operation.path, operation.afterState) {
			status, errorCode, errorMessage = gitOperationSucceeded, "", ""
		}
		if _, err := s.db.ExecContext(ctx, `update git_operations set status=?,error_code=?,error_message=?,finished_at=? where id=? and status in ('queued','running')`, status, errorCode, errorMessage, now, operation.id); err != nil {
			return fmt.Errorf("recover Git operation %s: %w", operation.id, err)
		}
	}
	return nil
}

func recoveredGitFetch(ctx context.Context, repo, afterState string) bool {
	var evidence struct {
		RemoteRefs map[string]string `json:"remoteRefs"`
	}
	if json.Unmarshal([]byte(afterState), &evidence) != nil || len(evidence.RemoteRefs) == 0 {
		return false
	}
	for ref, oid := range evidence.RemoteRefs {
		if !strings.HasPrefix(ref, "refs/remotes/") || !isFullGitObjectID(oid) {
			return false
		}
	}
	runner := newGitRunner().(*gitCLIRunner)
	output, err := runner.command(ctx, repo, "for-each-ref", "--format=%(refname)%00%(objectname)%00", "refs/remotes")
	if err != nil {
		return false
	}
	refs := map[string]string{}
	fields := bytes.Split(output, []byte{0})
	for index := 0; index+1 < len(fields); index += 2 {
		ref := strings.TrimSpace(string(fields[index]))
		if ref != "" {
			refs[ref] = strings.TrimSpace(string(fields[index+1]))
		}
	}
	for ref, oid := range evidence.RemoteRefs {
		if refs[ref] != oid {
			return false
		}
	}
	return true
}

func recoveredGitCommit(ctx context.Context, repo, afterState string) bool {
	var evidence struct {
		HeadOID string `json:"headOid"`
	}
	if json.Unmarshal([]byte(afterState), &evidence) != nil || !isFullGitObjectID(evidence.HeadOID) {
		return false
	}
	snapshot, err := newGitRunner().Snapshot(ctx, repo)
	return err == nil && snapshot.Head.OID == evidence.HeadOID
}

func (s *Server) gitSummary(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.projectGitRepository(w, r)
	if !ok {
		return
	}
	snapshot, changes, fingerprints, err := readGitState(r.Context(), repo)
	if err != nil {
		s.writeGitReadError(w, err)
		return
	}
	observedAt := time.Now().UTC()
	token := s.issueGitStateToken(chi.URLParam(r, "projectID"), snapshot, changes, fingerprints, observedAt)
	writeJSON(w, http.StatusOK, gitSummaryResponse{GitSnapshot: snapshot, ObservedAt: observedAt, StateToken: token})
}

func (s *Server) gitChanges(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.projectGitRepository(w, r)
	if !ok {
		return
	}
	changes, err := newGitRunner().Changes(r.Context(), repo)
	if err != nil {
		s.writeGitReadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, changes)
}

func (s *Server) gitDiff(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.projectGitRepository(w, r)
	if !ok {
		return
	}
	path := r.URL.Query().Get("path")
	stage := GitDiffStage(r.URL.Query().Get("stage"))
	if stage == "" {
		stage = gitDiffWorktree
	}
	if err := validateGitPath(path); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if stage != gitDiffWorktree && stage != gitDiffIndex {
		writeError(w, http.StatusBadRequest, errors.New("unsupported Git diff stage"))
		return
	}
	changes, err := newGitRunner().Changes(r.Context(), repo)
	if err != nil {
		s.writeGitReadError(w, err)
		return
	}
	var selected *GitChange
	for index := range changes {
		if changes[index].Path == path {
			selected = &changes[index]
			break
		}
	}
	if selected == nil || (stage == gitDiffIndex && !selected.Staged) || (stage == gitDiffWorktree && !(selected.Modified || selected.Untracked || selected.Deleted || selected.Renamed || selected.Conflicted)) {
		writeError(w, http.StatusBadRequest, errors.New("Git path is not available in the selected change set"))
		return
	}
	if selected.Untracked && stage == gitDiffWorktree {
		content, err := readUntrackedGitContent(repo, path)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"path": path, "stage": string(stage), "content": content})
		return
	}
	diff, err := newGitRunner().Diff(r.Context(), repo, path, stage)
	if err != nil {
		s.writeGitReadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": path, "stage": string(stage), "content": diff})
}

func (s *Server) gitStage(w http.ResponseWriter, r *http.Request) {
	s.gitPathsMutation(w, r, "stage", func(ctx context.Context, repo string, paths []string) error {
		return newGitRunner().(*gitCLIRunner).Stage(ctx, repo, paths)
	})
}
func (s *Server) gitUnstage(w http.ResponseWriter, r *http.Request) {
	s.gitPathsMutation(w, r, "unstage", func(ctx context.Context, repo string, paths []string) error {
		return newGitRunner().(*gitCLIRunner).Unstage(ctx, repo, paths)
	})
}

func (s *Server) gitPathsMutation(w http.ResponseWriter, r *http.Request, typ string, execute func(context.Context, string, []string) error) {
	projectID := chi.URLParam(r, "projectID")
	var input struct {
		Paths      []string `json:"paths"`
		StateToken string   `json:"stateToken"`
	}
	if !decode(w, r, &input) {
		return
	}
	if len(input.Paths) == 0 || len(input.Paths) > 100 {
		writeError(w, http.StatusBadRequest, errors.New("between 1 and 100 Git paths are required"))
		return
	}
	for _, path := range input.Paths {
		if err := validateGitPath(path); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	repo, err := s.projectPath(r.Context(), projectID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("project not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	release, acquired := s.acquireProjectWorkspace(projectID, "git:"+uuid.NewString())
	if !acquired {
		writeError(w, http.StatusConflict, errors.New("project workspace is occupied"))
		return
	}
	defer release()
	state, err := s.validateGitStateToken(r.Context(), projectID, repo, input.StateToken)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	available := map[string]GitChange{}
	for _, change := range state.changes {
		available[change.Path] = change
	}
	for _, path := range input.Paths {
		change, found := available[path]
		if !found || (typ == "stage" && !(change.Modified || change.Untracked || change.Deleted || change.Renamed || change.Conflicted)) || (typ == "unstage" && !change.Staged) {
			writeError(w, http.StatusConflict, errors.New("selected Git paths are no longer available"))
			return
		}
	}
	now := time.Now().UTC()
	operation := GitOperation{ID: uuid.NewString(), ProjectID: projectID, Type: typ, Status: gitOperationQueued, RequestSummary: strings.Join(input.Paths, ", "), RequestedAt: now}
	if _, err := s.db.ExecContext(r.Context(), `insert into git_operations (id,project_id,type,status,request_summary,requested_at) values (?,?,?,?,?,?)`, operation.ID, operation.ProjectID, operation.Type, operation.Status, operation.RequestSummary, operation.RequestedAt); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	operation.Status, operation.StartedAt = gitOperationRunning, &now
	if _, err := s.db.ExecContext(r.Context(), `update git_operations set status=?,started_at=? where id=? and status=?`, operation.Status, operation.StartedAt, operation.ID, gitOperationQueued); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	err = execute(r.Context(), repo, input.Paths)
	finished := time.Now().UTC()
	if err != nil {
		_, _ = s.db.ExecContext(r.Context(), `update git_operations set status=?,error_code=?,error_message=?,finished_at=? where id=?`, gitOperationFailed, "git_failed", "Git operation failed", finished, operation.ID)
		writeJSON(w, http.StatusAccepted, map[string]string{"operationId": operation.ID})
		return
	}
	_, _ = s.db.ExecContext(r.Context(), `update git_operations set status=?,finished_at=? where id=?`, gitOperationSucceeded, finished, operation.ID)
	writeJSON(w, http.StatusAccepted, map[string]string{"operationId": operation.ID})
}

func readGitState(ctx context.Context, repo string) (GitSnapshot, []GitChange, map[string]string, error) {
	runner := newGitRunner()
	snapshot, err := runner.Snapshot(ctx, repo)
	if err != nil {
		return GitSnapshot{}, nil, nil, err
	}
	changes, err := runner.Changes(ctx, repo)
	if err != nil {
		return GitSnapshot{}, nil, nil, err
	}
	return snapshot, changes, gitChangeFingerprints(repo, changes), nil
}

func gitChangeFingerprints(repo string, changes []GitChange) map[string]string {
	result := make(map[string]string, len(changes))
	for _, change := range changes {
		info, err := os.Lstat(filepath.Join(repo, change.Path))
		if err != nil {
			result[change.Path] = "missing"
			continue
		}
		result[change.Path] = fmt.Sprintf("%d:%d:%d:%d", info.Mode(), info.Size(), info.ModTime().UnixNano(), info.ModTime().Unix())
	}
	return result
}

func (s *Server) issueGitStateToken(projectID string, snapshot GitSnapshot, changes []GitChange, fingerprints map[string]string, now time.Time) string {
	token := uuid.NewString()
	expiresAt := now.Add(2 * time.Minute)
	s.mu.Lock()
	defer s.mu.Unlock()
	for value, record := range s.gitStateTokens {
		if !record.expiresAt.After(now) {
			delete(s.gitStateTokens, value)
		}
	}
	s.gitStateTokens[token] = gitStateToken{projectID: projectID, snapshot: snapshot, changes: changes, fingerprints: fingerprints, expiresAt: expiresAt}
	return token
}

func (s *Server) validateGitStateToken(ctx context.Context, projectID, repo, token string) (gitStateToken, error) {
	s.mu.Lock()
	record, found := s.gitStateTokens[token]
	s.mu.Unlock()
	if token == "" || !found || record.projectID != projectID || !record.expiresAt.After(time.Now().UTC()) {
		return gitStateToken{}, errors.New("Git state changed; refresh the repository")
	}
	snapshot, changes, fingerprints, err := readGitState(ctx, repo)
	if err != nil {
		return gitStateToken{}, errors.New("Git repository state is unavailable")
	}
	if !reflect.DeepEqual(record.snapshot, snapshot) || !reflect.DeepEqual(record.changes, changes) || !reflect.DeepEqual(record.fingerprints, fingerprints) {
		return gitStateToken{}, errors.New("Git state changed; refresh the repository")
	}
	return record, nil
}

func readUntrackedGitContent(repo, path string) (string, error) {
	if err := validateGitPath(path); err != nil {
		return "", err
	}
	fullPath := filepath.Join(repo, path)
	info, err := os.Lstat(fullPath)
	if err != nil {
		return "", errors.New("untracked Git path is unavailable")
	}
	if !info.Mode().IsRegular() {
		return "(Untracked non-regular file)", nil
	}
	if info.Size() > gitOutputLimit {
		return "", errors.New("untracked file exceeds the display limit")
	}
	content, err := os.ReadFile(fullPath)
	if err != nil {
		return "", errors.New("cannot read untracked Git file")
	}
	return string(content), nil
}

func (s *Server) gitLog(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.projectGitRepository(w, r)
	if !ok {
		return
	}
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		ref = "HEAD"
	}
	limit := 50
	if rawLimit := r.URL.Query().Get("limit"); rawLimit != "" {
		parsed, err := strconv.Atoi(rawLimit)
		if err != nil || parsed < 1 || parsed > 100 {
			writeError(w, http.StatusBadRequest, errors.New("Git log limit must be between 1 and 100"))
			return
		}
		limit = parsed
	}
	commits, err := newGitRunner().Log(r.Context(), repo, ref, limit)
	if err != nil {
		if validateGitRef(ref) != nil || strings.Contains(err.Error(), "reference is not available") || strings.Contains(err.Error(), "object ID is not a commit") {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		s.writeGitReadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, commits)
}

func (s *Server) gitBranches(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.projectGitRepository(w, r)
	if !ok {
		return
	}
	branches, err := newGitRunner().Branches(r.Context(), repo)
	if err != nil {
		s.writeGitReadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, branches)
}

func (s *Server) gitOperations(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	limit := 50
	if rawLimit := r.URL.Query().Get("limit"); rawLimit != "" {
		parsed, err := strconv.Atoi(rawLimit)
		if err != nil || parsed < 1 || parsed > 100 {
			writeError(w, http.StatusBadRequest, errors.New("Git operation limit must be between 1 and 100"))
			return
		}
		limit = parsed
	}
	if _, err := s.projectPath(r.Context(), projectID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("project not found"))
		} else {
			writeError(w, http.StatusInternalServerError, err)
		}
		return
	}
	operations, err := s.listGitOperations(r.Context(), projectID, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, operations)
}

func (s *Server) projectGitRepository(w http.ResponseWriter, r *http.Request) (string, bool) {
	repo, err := s.projectPath(r.Context(), chi.URLParam(r, "projectID"))
	if err == nil {
		return repo, true
	}
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("project not found"))
	} else {
		writeError(w, http.StatusInternalServerError, err)
	}
	return "", false
}

func (s *Server) projectPath(ctx context.Context, projectID string) (string, error) {
	var path string
	err := s.db.QueryRowContext(ctx, `select path from projects where id=?`, projectID).Scan(&path)
	return path, err
}

func (s *Server) writeGitReadError(w http.ResponseWriter, err error) {
	if errors.Is(err, context.DeadlineExceeded) {
		writeError(w, http.StatusGatewayTimeout, errors.New("Git request timed out"))
		return
	}
	writeError(w, http.StatusConflict, errors.New("project is not currently a readable Git repository"))
}

func (s *Server) listGitOperations(ctx context.Context, projectID string, limit int) ([]GitOperation, error) {
	rows, err := s.db.QueryContext(ctx, `select id,project_id,type,status,request_summary,before_state,after_state,error_code,error_message,requested_at,started_at,finished_at from git_operations where project_id=? order by requested_at desc limit ?`, projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("list Git operations: %w", err)
	}
	defer rows.Close()
	operations := []GitOperation{}
	for rows.Next() {
		var operation GitOperation
		var startedAt, finishedAt sql.NullTime
		if err := rows.Scan(&operation.ID, &operation.ProjectID, &operation.Type, &operation.Status, &operation.RequestSummary, &operation.BeforeState, &operation.AfterState, &operation.ErrorCode, &operation.ErrorMessage, &operation.RequestedAt, &startedAt, &finishedAt); err != nil {
			return nil, fmt.Errorf("read Git operation: %w", err)
		}
		if startedAt.Valid {
			operation.StartedAt = &startedAt.Time
		}
		if finishedAt.Valid {
			operation.FinishedAt = &finishedAt.Time
		}
		operations = append(operations, operation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Git operations: %w", err)
	}
	return operations, nil
}
