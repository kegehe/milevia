package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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
	rows, err := s.db.QueryContext(ctx, `select operations.id,operations.type,operations.after_state,projects.id,projects.path from git_operations operations join projects on projects.id=operations.project_id where operations.status in ('queued','running')`)
	if err != nil {
		return fmt.Errorf("list interrupted Git operations: %w", err)
	}
	type interruptedOperation struct {
		id, typ, afterState, projectID, path string
	}
	operations := []interruptedOperation{}
	for rows.Next() {
		var operation interruptedOperation
		if err := rows.Scan(&operation.id, &operation.typ, &operation.afterState, &operation.projectID, &operation.path); err != nil {
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
		status, errorCode, errorMessage := gitOperationNeedsAttention, "result_unknown", "控制服务在确认 Git 操作结果前已重启，请刷新并检查工作区。"
		runner, repo, runnerErr := s.gitRunnerForProject(ctx, operation.projectID)
		if runnerErr == nil {
			if operation.typ == "commit" && recoveredGitCommit(ctx, runner, repo, operation.afterState) {
				status, errorCode, errorMessage = gitOperationSucceeded, "", ""
			}
			if operation.typ == "fetch" && recoveredGitFetch(ctx, runner, repo, operation.afterState) {
				status, errorCode, errorMessage = gitOperationSucceeded, "", ""
			}
		}
		if _, err := s.db.ExecContext(ctx, `update git_operations set status=?,error_code=?,error_message=?,finished_at=? where id=? and status in ('queued','running')`, status, errorCode, errorMessage, now, operation.id); err != nil {
			return fmt.Errorf("recover Git operation %s: %w", operation.id, err)
		}
	}
	return nil
}

func recoveredGitFetch(ctx context.Context, runner GitRunner, repo, afterState string) bool {
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
	output, err := runner.runGit(ctx, repo, "for-each-ref", "--format=%(refname)%00%(objectname)%00", "refs/remotes")
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

func recoveredGitCommit(ctx context.Context, runner GitRunner, repo, afterState string) bool {
	var evidence struct {
		HeadOID string `json:"headOid"`
	}
	if json.Unmarshal([]byte(afterState), &evidence) != nil || !isFullGitObjectID(evidence.HeadOID) {
		return false
	}
	snapshot, err := runner.Snapshot(ctx, repo)
	return err == nil && snapshot.Head.OID == evidence.HeadOID
}

func (s *Server) gitSummary(w http.ResponseWriter, r *http.Request) {
	runner, repo, ok := s.getGitRunner(w, r)
	if !ok {
		return
	}
	snapshot, changes, fingerprints, err := readGitState(r.Context(), runner, repo)
	if err != nil {
		s.writeGitReadError(w, err)
		return
	}
	observedAt := time.Now().UTC()
	token := s.issueGitStateToken(chi.URLParam(r, "projectID"), snapshot, changes, fingerprints, observedAt)
	writeJSON(w, http.StatusOK, gitSummaryResponse{GitSnapshot: snapshot, ObservedAt: observedAt, StateToken: token})
}

func (s *Server) gitChanges(w http.ResponseWriter, r *http.Request) {
	runner, repo, ok := s.getGitRunner(w, r)
	if !ok {
		return
	}
	changes, err := runner.Changes(r.Context(), repo)
	if err != nil {
		s.writeGitReadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, changes)
}

func (s *Server) gitDiff(w http.ResponseWriter, r *http.Request) {
	runner, repo, ok := s.getGitRunner(w, r)
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
	changes, err := runner.Changes(r.Context(), repo)
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
		content, err := readUntrackedGitContent(runner, repo, path)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"path": path, "stage": string(stage), "content": content})
		return
	}
	diff, err := runner.Diff(r.Context(), repo, path, stage)
	if err != nil {
		s.writeGitReadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": path, "stage": string(stage), "content": diff})
}

func (s *Server) gitStage(w http.ResponseWriter, r *http.Request) {
	s.gitPathsMutation(w, r, "stage", func(ctx context.Context, runner GitRunner, repo string, paths []string) error {
		return runner.Stage(ctx, repo, paths)
	})
}

func (s *Server) gitUnstage(w http.ResponseWriter, r *http.Request) {
	s.gitPathsMutation(w, r, "unstage", func(ctx context.Context, runner GitRunner, repo string, paths []string) error {
		return runner.Unstage(ctx, repo, paths)
	})
}

func (s *Server) gitStageAll(w http.ResponseWriter, r *http.Request) {
	s.gitAllPathsMutation(w, r, "stage", "全部暂存", func(change GitChange) bool {
		return change.Modified || change.Untracked || change.Deleted || change.Renamed || change.Conflicted
	}, func(ctx context.Context, runner GitRunner, repo string, paths []string) error {
		return runner.Stage(ctx, repo, paths)
	})
}

func (s *Server) gitUnstageAll(w http.ResponseWriter, r *http.Request) {
	s.gitAllPathsMutation(w, r, "unstage", "全部取消暂存", func(change GitChange) bool {
		return change.Staged
	}, func(ctx context.Context, runner GitRunner, repo string, paths []string) error {
		return runner.Unstage(ctx, repo, paths)
	})
}

func (s *Server) gitAllPathsMutation(w http.ResponseWriter, r *http.Request, typ, label string, eligible func(GitChange) bool, execute func(context.Context, GitRunner, string, []string) error) {
	var input struct {
		StateToken string `json:"stateToken"`
	}
	if !decode(w, r, &input) {
		return
	}
	runner, projectID, repo, state, release, ok := s.gitMutationState(w, r, input.StateToken)
	if !ok {
		return
	}
	defer release()
	paths := make([]string, 0, len(state.changes))
	for _, change := range state.changes {
		if eligible(change) {
			paths = append(paths, change.Path)
		}
	}
	if len(paths) == 0 {
		writeError(w, http.StatusConflict, errors.New("there are no eligible Git changes"))
		return
	}
	result, err := s.executeGitOperation(r.Context(), runner, projectID, repo, typ, fmt.Sprintf("%s（%d 个文件）", label, len(paths)), state.snapshot, func(runner GitRunner) error {
		return execute(r.Context(), runner, repo, paths)
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

func (s *Server) gitPathsMutation(w http.ResponseWriter, r *http.Request, typ string, execute func(context.Context, GitRunner, string, []string) error) {
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
	runner, projectID, repo, state, release, ok := s.gitMutationState(w, r, input.StateToken)
	if !ok {
		return
	}
	defer release()
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
	result, err := s.executeGitOperation(r.Context(), runner, projectID, repo, typ, strings.Join(input.Paths, ", "), state.snapshot, func(runner GitRunner) error {
		return execute(r.Context(), runner, repo, input.Paths)
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

func (s *Server) gitCommit(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Message    string `json:"message"`
		StateToken string `json:"stateToken"`
	}
	if !decode(w, r, &input) {
		return
	}
	message := strings.TrimSpace(input.Message)
	if message == "" || utf8.RuneCountInString(message) > 4000 || strings.ContainsRune(message, 0) {
		writeError(w, http.StatusBadRequest, errors.New("Git commit message must be between 1 and 4000 characters"))
		return
	}
	if firstLine := strings.Split(message, "\n")[0]; utf8.RuneCountInString(firstLine) > 72 {
		writeError(w, http.StatusBadRequest, errors.New("Git commit subject must be at most 72 characters"))
		return
	}
	runner, projectID, repo, state, release, ok := s.gitMutationState(w, r, input.StateToken)
	if !ok {
		return
	}
	defer release()
	hasStaged := false
	for _, change := range state.changes {
		if change.Conflicted {
			writeError(w, http.StatusConflict, errors.New("resolve Git conflicts before committing"))
			return
		}
		hasStaged = hasStaged || change.Staged
	}
	if !hasStaged {
		writeError(w, http.StatusConflict, errors.New("there are no staged Git changes to commit"))
		return
	}
	result, err := s.executeGitOperation(r.Context(), runner, projectID, repo, "commit", message, state.snapshot, func(runner GitRunner) error {
		return runner.Commit(r.Context(), repo, message)
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

func (s *Server) gitDiscard(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Paths            []string `json:"paths"`
		Mode             string   `json:"mode"`
		IncludeUntracked bool     `json:"includeUntracked"`
		StateToken       string   `json:"stateToken"`
	}
	if !decode(w, r, &input) {
		return
	}
	if input.Mode != "worktree" && input.Mode != "all" {
		writeError(w, http.StatusBadRequest, errors.New("unsupported Git discard mode"))
		return
	}
	if len(input.Paths) > 100 {
		writeError(w, http.StatusBadRequest, errors.New("at most 100 Git paths are allowed"))
		return
	}
	for _, path := range input.Paths {
		if err := validateGitPath(path); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	runner, projectID, repo, state, release, ok := s.gitMutationState(w, r, input.StateToken)
	if !ok {
		return
	}
	defer release()
	if input.Mode == "worktree" {
		if len(input.Paths) == 0 {
			writeError(w, http.StatusBadRequest, errors.New("at least one Git path is required"))
			return
		}
		available := map[string]GitChange{}
		for _, change := range state.changes {
			available[change.Path] = change
		}
		tracked, untracked := make([]string, 0, len(input.Paths)), make([]string, 0)
		for _, path := range input.Paths {
			change, found := available[path]
			if !found || change.Conflicted {
				writeError(w, http.StatusConflict, errors.New("selected Git paths cannot be restored from the index"))
				return
			}
			if change.Untracked {
				if !input.IncludeUntracked {
					writeError(w, http.StatusConflict, errors.New("explicit confirmation is required to remove untracked Git paths"))
					return
				}
				untracked = append(untracked, path)
				continue
			}
			if !(change.Modified || change.Deleted || change.Renamed) {
				writeError(w, http.StatusConflict, errors.New("selected Git paths cannot be restored from the index"))
				return
			}
			tracked = append(tracked, path)
		}
		if err := runner.ValidateUntrackedRemoval(repo, untracked); err != nil {
			writeError(w, http.StatusConflict, errors.New("untracked Git paths changed; refresh the repository"))
			return
		}
		result, err := s.executeGitOperation(r.Context(), runner, projectID, repo, "discard_worktree", strings.Join(input.Paths, ", "), state.snapshot, func(runner GitRunner) error {
			if len(tracked) > 0 {
				if err := runner.RestoreWorktree(r.Context(), repo, tracked); err != nil {
					return err
				}
			}
			if len(untracked) == 0 {
				return nil
			}
			if err := runner.RemoveUntracked(repo, untracked); err != nil {
				if len(tracked) > 0 {
					return partiallyAppliedGitError(err)
				}
				return err
			}
			return nil
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusAccepted, result)
		return
	}
	if len(input.Paths) != 0 {
		writeError(w, http.StatusBadRequest, errors.New("discard all does not accept individual Git paths"))
		return
	}
	tracked, untracked := make([]string, 0, len(state.changes)), make([]string, 0)
	for _, change := range state.changes {
		if change.Conflicted {
			writeError(w, http.StatusConflict, errors.New("resolve Git conflicts before discarding all changes"))
			return
		}
		if change.Untracked {
			untracked = append(untracked, change.Path)
		} else {
			tracked = append(tracked, change.Path)
		}
	}
	if len(tracked) == 0 && (!input.IncludeUntracked || len(untracked) == 0) {
		writeError(w, http.StatusConflict, errors.New("there are no Git changes eligible for discard"))
		return
	}
	summary := fmt.Sprintf("丢弃全部未提交改动（%d 个文件）", len(tracked))
	if input.IncludeUntracked {
		summary = fmt.Sprintf("丢弃全部未提交改动及 %d 个未跟踪文件", len(untracked))
	}
	initial := !hasGitHead(state.snapshot)
	removalPaths := append([]string{}, untracked...)
	if initial {
		removalPaths = append(removalPaths, tracked...)
	}
	if err := runner.ValidateUntrackedRemoval(repo, removalPaths); err != nil {
		writeError(w, http.StatusConflict, errors.New("untracked Git paths changed; refresh the repository"))
		return
	}
	result, err := s.executeGitOperation(r.Context(), runner, projectID, repo, "discard_all", summary, state.snapshot, func(runner GitRunner) error {
		if initial {
			if err := runner.DiscardInitialChanges(r.Context(), repo, tracked); err != nil {
				return err
			}
			if input.IncludeUntracked {
				if err := runner.RemoveUntracked(repo, untracked); err != nil {
					if len(tracked) > 0 {
						return partiallyAppliedGitError(err)
					}
					return err
				}
			}
			return nil
		}
		if err := runner.RestoreAll(r.Context(), repo, tracked); err != nil {
			return err
		}
		if input.IncludeUntracked {
			if err := runner.RemoveUntracked(repo, untracked); err != nil {
				if len(tracked) > 0 {
					return partiallyAppliedGitError(err)
				}
				return err
			}
		}
		return nil
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

func (s *Server) gitFetch(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Remote     string `json:"remote"`
		StateToken string `json:"stateToken"`
	}
	if !decode(w, r, &input) {
		return
	}
	if input.Remote == "" {
		input.Remote = "origin"
	}
	if err := validateGitRef(input.Remote); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	runner, projectID, repo, state, release, ok := s.gitMutationState(w, r, input.StateToken)
	if !ok {
		return
	}
	defer release()
	result, err := s.executeGitOperation(r.Context(), runner, projectID, repo, "fetch",
		fmt.Sprintf("fetch %s", input.Remote), state.snapshot, func(runner GitRunner) error {
			return runner.Fetch(r.Context(), repo, input.Remote)
		})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

func (s *Server) gitPush(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Remote      string `json:"remote"`
		Branch      string `json:"branch"`
		SetUpstream bool   `json:"setUpstream"`
		StateToken  string `json:"stateToken"`
	}
	if !decode(w, r, &input) {
		return
	}
	if input.Remote == "" {
		input.Remote = "origin"
	}
	if input.Branch == "" {
		writeError(w, http.StatusBadRequest, errors.New("branch is required"))
		return
	}
	if err := validateGitRef(input.Remote); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := validateGitRef(input.Branch); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	runner, projectID, repo, state, release, ok := s.gitMutationState(w, r, input.StateToken)
	if !ok {
		return
	}
	defer release()
	for _, change := range state.changes {
		if change.Conflicted {
			writeError(w, http.StatusConflict, errors.New("resolve Git conflicts before pushing"))
			return
		}
	}
	result, err := s.executeGitOperation(r.Context(), runner, projectID, repo, "push",
		fmt.Sprintf("push %s %s", input.Remote, input.Branch), state.snapshot, func(runner GitRunner) error {
			return runner.Push(r.Context(), repo, input.Remote, input.Branch, input.SetUpstream)
		})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

func (s *Server) gitCreateBranch(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name       string `json:"name"`
		StartPoint string `json:"startPoint"`
	}
	if !decode(w, r, &input) {
		return
	}
	if input.Name == "" || utf8.RuneCountInString(input.Name) > 200 {
		writeError(w, http.StatusBadRequest, errors.New("branch name is required and must be under 200 characters"))
		return
	}
	if err := validateGitRef(input.Name); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if input.StartPoint != "" {
		if err := validateGitRef(input.StartPoint); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	projectID := chi.URLParam(r, "projectID")
	runner, repo, ok := s.getGitRunner(w, r)
	if !ok {
		return
	}
	release, acquired := s.acquireProjectWorkspace(projectID, "git:"+uuid.NewString())
	if !acquired {
		writeError(w, http.StatusConflict, errors.New("project workspace is occupied"))
		return
	}
	defer release()
	snapshot, _, _, err := readGitState(r.Context(), runner, repo)
	if err != nil {
		writeError(w, http.StatusConflict, errors.New("Git repository state is unavailable"))
		return
	}
	summary := fmt.Sprintf("创建分支 %s", input.Name)
	if input.StartPoint != "" {
		summary = fmt.Sprintf("创建分支 %s（基于 %s）", input.Name, input.StartPoint)
	}
	result, err := s.executeGitOperation(r.Context(), runner, projectID, repo, "create_branch",
		summary, snapshot, func(runner GitRunner) error {
			return runner.CreateBranch(r.Context(), repo, input.Name, input.StartPoint)
		})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

func (s *Server) gitSwitchBranch(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Branch     string `json:"branch"`
		StateToken string `json:"stateToken"`
	}
	if !decode(w, r, &input) {
		return
	}
	if input.Branch == "" {
		writeError(w, http.StatusBadRequest, errors.New("branch is required"))
		return
	}
	if err := validateGitRef(input.Branch); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	runner, projectID, repo, state, release, ok := s.gitMutationState(w, r, input.StateToken)
	if !ok {
		return
	}
	defer release()
	for _, change := range state.changes {
		if change.Conflicted {
			writeError(w, http.StatusConflict, errors.New("resolve Git conflicts before switching branches"))
			return
		}
		if change.Staged || change.Modified || change.Deleted || change.Renamed || change.Untracked {
			writeError(w, http.StatusConflict, errors.New("commit or discard changes before switching branches"))
			return
		}
	}
	if state.snapshot.Head.Branch == input.Branch && !state.snapshot.Head.Detached {
		writeError(w, http.StatusConflict, errors.New("already on the target branch"))
		return
	}
	result, err := s.executeGitOperation(r.Context(), runner, projectID, repo, "switch_branch",
		fmt.Sprintf("切换到 %s", input.Branch), state.snapshot, func(runner GitRunner) error {
			return runner.SwitchBranch(r.Context(), repo, input.Branch)
		})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

func (s *Server) gitMutationState(w http.ResponseWriter, r *http.Request, stateToken string) (GitRunner, string, string, gitStateToken, func(), bool) {
	runner, repo, ok := s.getGitRunner(w, r)
	if !ok {
		return nil, "", "", gitStateToken{}, nil, false
	}
	projectID := chi.URLParam(r, "projectID")
	release, acquired := s.acquireProjectWorkspace(projectID, "git:"+uuid.NewString())
	if !acquired {
		writeError(w, http.StatusConflict, errors.New("project workspace is occupied"))
		return nil, "", "", gitStateToken{}, nil, false
	}
	state, err := s.validateGitStateToken(r.Context(), runner, projectID, repo, stateToken)
	if err != nil {
		release()
		writeError(w, http.StatusConflict, err)
		return nil, "", "", gitStateToken{}, nil, false
	}
	return runner, projectID, repo, state, release, true
}

type gitOperationResult struct {
	OperationID  string `json:"operationId"`
	Status       string `json:"status"`
	ErrorMessage string `json:"errorMessage,omitempty"`
}

func (s *Server) executeGitOperation(ctx context.Context, runner GitRunner, projectID, repo, typ, summary string, before GitSnapshot, execute func(GitRunner) error) (gitOperationResult, error) {
	now := time.Now().UTC()
	beforeState, _ := json.Marshal(before)
	operation := GitOperation{ID: uuid.NewString(), ProjectID: projectID, Type: typ, Status: gitOperationQueued, RequestSummary: summary, RequestedAt: now}
	if _, err := s.db.ExecContext(ctx, `insert into git_operations (id,project_id,type,status,request_summary,before_state,requested_at) values (?,?,?,?,?,?,?)`, operation.ID, operation.ProjectID, operation.Type, operation.Status, operation.RequestSummary, string(beforeState), operation.RequestedAt); err != nil {
		return gitOperationResult{}, err
	}
	operation.Status, operation.StartedAt = gitOperationRunning, &now
	if _, err := s.db.ExecContext(ctx, `update git_operations set status=?,started_at=? where id=? and status=?`, operation.Status, operation.StartedAt, operation.ID, gitOperationQueued); err != nil {
		return gitOperationResult{}, err
	}
	if err := execute(runner); err != nil {
		finished := time.Now().UTC()
		status, code, message := gitOperationFailure(err)
		if err := s.updateGitOperation(ctx, operation.ID, status, "{}", code, message, finished); err != nil {
			return s.markGitOperationNeedsAttention(ctx, operation.ID, "{}", "audit_update_failed", "Git 操作返回失败，但无法记录最终状态；请刷新并检查工作区", finished), nil
		}
		return gitOperationResult{OperationID: operation.ID, Status: status, ErrorMessage: message}, nil
	}
	finished := time.Now().UTC()
	snapshot, _, _, err := readGitState(ctx, runner, repo)
	if err != nil {
		return s.markGitOperationNeedsAttention(ctx, operation.ID, "{}", "result_unverified", "Git 操作已执行，但无法读取最新仓库状态；请刷新并检查工作区", finished), nil
	}
	afterState, err := json.Marshal(snapshot)
	if err != nil {
		return s.markGitOperationNeedsAttention(ctx, operation.ID, "{}", "result_unverified", "Git 操作已执行，但无法编码最新仓库状态；请刷新并检查工作区", finished), nil
	}
	if err := s.updateGitOperation(ctx, operation.ID, gitOperationSucceeded, string(afterState), "", "", finished); err != nil {
		return s.markGitOperationNeedsAttention(ctx, operation.ID, string(afterState), "audit_update_failed", "Git 操作已执行，但无法记录最终状态；请刷新并检查工作区", finished), nil
	}
	return gitOperationResult{OperationID: operation.ID, Status: gitOperationSucceeded}, nil
}

func partiallyAppliedGitError(err error) error {
	return fmt.Errorf("%w: %v", errGitPartiallyApplied, err)
}

func (s *Server) updateGitOperation(ctx context.Context, operationID, status, afterState, code, message string, finished time.Time) error {
	_, err := s.db.ExecContext(ctx, `update git_operations set status=?,after_state=?,error_code=?,error_message=?,finished_at=? where id=?`, status, afterState, code, message, finished, operationID)
	return err
}

func (s *Server) markGitOperationNeedsAttention(ctx context.Context, operationID, afterState, code, message string, finished time.Time) gitOperationResult {
	if err := s.updateGitOperation(ctx, operationID, gitOperationNeedsAttention, afterState, code, message, finished); err != nil {
		go s.retryGitOperationUpdate(operationID, afterState, code, message, finished)
	}
	return gitOperationResult{OperationID: operationID, Status: gitOperationNeedsAttention, ErrorMessage: message}
}

func (s *Server) retryGitOperationUpdate(operationID, afterState, code, message string, finished time.Time) {
	for _, delay := range []time.Duration{250 * time.Millisecond, time.Second, 3 * time.Second} {
		select {
		case <-s.runtimeCtx.Done():
			return
		case <-time.After(delay):
		}
		result, err := s.db.ExecContext(s.runtimeCtx, `update git_operations set status=?,after_state=?,error_code=?,error_message=?,finished_at=? where id=? and status in (?,?)`, gitOperationNeedsAttention, afterState, code, message, finished, operationID, gitOperationQueued, gitOperationRunning)
		if err != nil {
			continue
		}
		changed, err := result.RowsAffected()
		if err != nil || changed > 0 {
			return
		}
	}
}

func gitOperationFailure(err error) (status, code, message string) {
	if errors.Is(err, errGitPartiallyApplied) {
		return gitOperationNeedsAttention, "partial_result", "部分文件可能已被处理；请刷新仓库并检查工作区"
	}
	var commandErr *gitCommandError
	if !errors.As(err, &commandErr) {
		return gitOperationFailed, "git_failed", "Git 操作失败"
	}
	stderr := strings.ToLower(commandErr.stderr)
	switch {
	case strings.Contains(stderr, "author identity unknown"), strings.Contains(stderr, "please tell me who you are"), strings.Contains(stderr, "unable to auto-detect email address"):
		return gitOperationFailed, "identity_not_configured", "Git 用户身份未配置，请设置 user.name 和 user.email"
	case strings.Contains(stderr, "index.lock"), strings.Contains(stderr, "another git process"):
		return gitOperationFailed, "repository_locked", "Git 暂存区正被其他操作占用"
	case strings.Contains(stderr, "hook declined"), strings.Contains(stderr, "hook failed"):
		return gitOperationFailed, "hook_rejected", "Git hook 拒绝了本次操作"
	case strings.Contains(stderr, "nothing to commit"):
		return gitOperationFailed, "nothing_to_commit", "没有可提交的已暂存改动"
	case strings.Contains(stderr, "non-fast-forward"), strings.Contains(stderr, "updates were rejected"):
		return gitOperationFailed, "push_rejected", "推送被拒绝，远端有更新。请先 fetch 再推送"
	case strings.Contains(stderr, "authentication failed"), strings.Contains(stderr, "permission denied"):
		return gitOperationFailed, "auth_failed", "Git 远端认证失败，请检查凭证"
	case strings.Contains(stderr, "could not resolve host"), strings.Contains(stderr, "unable to access"):
		return gitOperationFailed, "remote_unavailable", "无法连接到 Git 远端，请检查网络或远端地址"
	case strings.Contains(stderr, "your local changes"), strings.Contains(stderr, "would be overwritten"):
		return gitOperationFailed, "dirty_worktree", "工作区有未提交改动，请先提交或丢弃"
	default:
		return gitOperationFailed, "git_failed", "Git 操作失败"
	}
}

func readGitState(ctx context.Context, runner GitRunner, repo string) (GitSnapshot, []GitChange, map[string]string, error) {
	snapshot, err := runner.Snapshot(ctx, repo)
	if err != nil {
		return GitSnapshot{}, nil, nil, err
	}
	changes, err := runner.Changes(ctx, repo)
	if err != nil {
		return GitSnapshot{}, nil, nil, err
	}
	return snapshot, changes, gitChangeFingerprints(runner, repo, changes), nil
}

func gitChangeFingerprints(runner GitRunner, repo string, changes []GitChange) map[string]string {
	result := make(map[string]string, len(changes))
	for _, change := range changes {
		mode, size, mtimeNano, mtimeUnix, err := runner.lstat(repo, change.Path)
		if err != nil {
			result[change.Path] = "missing"
			continue
		}
		result[change.Path] = fmt.Sprintf("%d:%d:%d:%d", mode, size, mtimeNano, mtimeUnix)
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

func (s *Server) validateGitStateToken(ctx context.Context, runner GitRunner, projectID, repo, token string) (gitStateToken, error) {
	s.mu.Lock()
	record, found := s.gitStateTokens[token]
	s.mu.Unlock()
	if token == "" || !found || record.projectID != projectID || !record.expiresAt.After(time.Now().UTC()) {
		return gitStateToken{}, errors.New("Git state changed; refresh the repository")
	}
	snapshot, changes, fingerprints, err := readGitState(ctx, runner, repo)
	if err != nil {
		return gitStateToken{}, errors.New("Git repository state is unavailable")
	}
	if !reflect.DeepEqual(record.snapshot, snapshot) || !reflect.DeepEqual(record.changes, changes) || !reflect.DeepEqual(record.fingerprints, fingerprints) {
		return gitStateToken{}, errors.New("Git state changed; refresh the repository")
	}
	return record, nil
}

func readUntrackedGitContent(runner GitRunner, repo, path string) (string, error) {
	if err := validateGitPath(path); err != nil {
		return "", err
	}
	mode, size, _, _, err := runner.lstat(repo, path)
	if err != nil {
		return "", errors.New("untracked Git path is unavailable")
	}
	if !mode.IsRegular() {
		return "(Untracked non-regular file)", nil
	}
	if size > gitOutputLimit {
		return "", errors.New("untracked file exceeds the display limit")
	}
	content, err := runner.readFile(repo, path)
	if err != nil {
		return "", errors.New("cannot read untracked Git file")
	}
	return string(content), nil
}

func (s *Server) gitLog(w http.ResponseWriter, r *http.Request) {
	runner, repo, ok := s.getGitRunner(w, r)
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
	commits, err := runner.Log(r.Context(), repo, ref, limit)
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
	runner, repo, ok := s.getGitRunner(w, r)
	if !ok {
		return
	}
	branches, err := runner.Branches(r.Context(), repo)
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

// getGitRunner 根据项目的 runner 类型返回对应的 GitRunner 与仓库路径。
// 本地项目（wsl-local）使用本地 exec.Command；SSH 项目通过 SSH 在远端执行 git。
func (s *Server) getGitRunner(w http.ResponseWriter, r *http.Request) (GitRunner, string, bool) {
	projectID := chi.URLParam(r, "projectID")
	project, err := s.getProjectByID(r.Context(), projectID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("project not found"))
		} else {
			writeError(w, http.StatusInternalServerError, err)
		}
		return nil, "", false
	}
	if project.Runner == "wsl-local" || project.Runner == "" {
		return newGitRunner(), project.Path, true
	}
	runner, ok := s.runnerRegistry.get(project.Runner)
	if !ok {
		writeError(w, http.StatusConflict, &runnerOfflineError{RunnerID: project.Runner})
		return nil, "", false
	}
	sshR, ok := runner.(*sshRunner)
	if !ok {
		writeError(w, http.StatusInternalServerError, errors.New("runner 不是 SSH 类型"))
		return nil, "", false
	}
	repo, err := sshR.canonicalProjectPath(r.Context(), project.Path)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return nil, "", false
	}
	return newSSHGitRunner(sshR.client, repo), repo, true
}

// gitRunnerForPath 根据 projectID 返回对应的 GitRunner 与仓库路径（用于非 HTTP 上下文，如恢复逻辑）。
func (s *Server) gitRunnerForProject(ctx context.Context, projectID string) (GitRunner, string, error) {
	project, err := s.getProjectByID(ctx, projectID)
	if err != nil {
		return nil, "", err
	}
	if project.Runner == "wsl-local" || project.Runner == "" {
		return newGitRunner(), project.Path, nil
	}
	runner, ok := s.runnerRegistry.get(project.Runner)
	if !ok {
		return nil, "", &runnerOfflineError{RunnerID: project.Runner}
	}
	sshR, ok := runner.(*sshRunner)
	if !ok {
		return nil, "", errors.New("runner 不是 SSH 类型")
	}
	repo, err := sshR.canonicalProjectPath(ctx, project.Path)
	if err != nil {
		return nil, "", err
	}
	return newSSHGitRunner(sshR.client, repo), repo, nil
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
