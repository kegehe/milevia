package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const conversationWorktreeCleanupTimeout = 5 * time.Second

type workspaceGitRunner interface {
	runGit(context.Context, string, ...string) ([]byte, error)
}

// resolvedConversationWorkspace is the server-authoritative root selected for
// a project tool request. Clients may select a conversation, but never supply
// an arbitrary filesystem path.
type resolvedConversationWorkspace struct {
	Project        Project
	Workspace      ConversationWorkspace
	ConversationID string
}

// resolveRequestWorkspace keeps existing project-scoped URLs compatible: no
// conversationId means the project's shared root. When supplied, it resolves
// the conversation's active, ready workspace and proves it belongs to the URL
// project before any filesystem, Git, terminal, or run operation uses it.
func (s *Server) resolveRequestWorkspace(ctx context.Context, projectID, conversationID string) (resolvedConversationWorkspace, error) {
	project, err := s.getProjectByID(ctx, projectID)
	if err != nil {
		return resolvedConversationWorkspace{}, err
	}
	if strings.TrimSpace(conversationID) == "" {
		return resolvedConversationWorkspace{Project: project, Workspace: ConversationWorkspace{ID: "project-shared:" + projectID, Mode: "project_shared", Path: project.Path, State: "ready"}}, nil
	}
	var workspace ConversationWorkspace
	err = s.db.QueryRowContext(ctx, `select w.id,w.conversation_id,w.generation,w.mode,w.path,w.branch,w.base_revision,w.state,w.created_at,w.archived_at
		from conversations c join conversation_workspaces w on w.id=c.active_workspace_id
		where c.id=? and c.project_id=?`, conversationID, projectID).Scan(
		&workspace.ID, &workspace.ConversationID, &workspace.Generation, &workspace.Mode, &workspace.Path, &workspace.Branch, &workspace.BaseRevision, &workspace.State, &workspace.CreatedAt, &workspace.ArchivedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return resolvedConversationWorkspace{}, errors.New("conversation workspace was not found for this project")
	}
	if err != nil {
		return resolvedConversationWorkspace{}, err
	}
	if workspace.State != "ready" || strings.TrimSpace(workspace.Path) == "" {
		return resolvedConversationWorkspace{}, errors.New("conversation workspace is not ready")
	}
	return resolvedConversationWorkspace{Project: project, Workspace: workspace, ConversationID: conversationID}, nil
}

func (s *Server) resolveRequestWorkspaceFromRequest(r *http.Request) (resolvedConversationWorkspace, error) {
	return s.resolveRequestWorkspace(r.Context(), chi.URLParam(r, "projectID"), r.URL.Query().Get("conversationId"))
}

func workspaceLeaseKey(project Project, workspace ConversationWorkspace) string {
	if workspace.Mode == "project_shared" || sameCleanPath(workspace.Path, project.Path) {
		return project.ID
	}
	return "path:" + filepath.Clean(workspace.Path)
}

func runManagerKeyForWorkspace(projectID string, workspace ConversationWorkspace) string {
	if workspace.Mode == "project_shared" {
		return projectID
	}
	return runManagerKey(projectID, workspace.Path)
}

func (s *Server) listConversationWorkspaces(w http.ResponseWriter, r *http.Request) {
	conversationID := chi.URLParam(r, "conversationID")
	rows, err := s.db.QueryContext(r.Context(), `select w.id,w.conversation_id,w.generation,w.mode,w.path,w.branch,w.base_revision,w.state,w.created_at,w.archived_at,w.id=c.active_workspace_id
		from conversation_workspaces w join conversations c on c.id=w.conversation_id where w.conversation_id=? order by w.generation desc`, conversationID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	items := []ConversationWorkspace{}
	for rows.Next() {
		var item ConversationWorkspace
		if err := rows.Scan(&item.ID, &item.ConversationID, &item.Generation, &item.Mode, &item.Path, &item.Branch, &item.BaseRevision, &item.State, &item.CreatedAt, &item.ArchivedAt, &item.Active); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func conversationWorktreePath(projectPath, projectID, conversationID string, generation int) string {
	return filepath.Join(filepath.Dir(filepath.Clean(projectPath)), ".milevia-workspaces", projectID, conversationID, strconv.Itoa(generation))
}

func conversationWorktreeBranch(conversationID string, generation int) string {
	shortID := conversationID
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}
	return fmt.Sprintf("milevia/conversation-%s-%d", shortID, generation)
}

func isExpectedConversationWorktree(projectPath, projectID, conversationID string, generation int, path string) bool {
	return sameCleanPath(path, conversationWorktreePath(projectPath, projectID, conversationID, generation))
}

func conversationWorktreeRegistered(ctx context.Context, git workspaceGitRunner, repo, path string) (bool, error) {
	output, err := git.runGit(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, "worktree ") && sameCleanPath(filepath.FromSlash(strings.TrimPrefix(line, "worktree ")), path) {
			return true, nil
		}
	}
	return false, nil
}

func conversationBranchExists(ctx context.Context, git workspaceGitRunner, repo, branch string) (bool, error) {
	if err := validateGitRef(branch); err != nil {
		return false, err
	}
	output, err := git.runGit(ctx, repo, "for-each-ref", "--format=%(refname)", "refs/heads/"+branch)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(output)) != "", nil
}

func removeEmptyConversationWorktreeParents(projectPath, projectID, conversationID string) {
	root := filepath.Join(filepath.Dir(filepath.Clean(projectPath)), ".milevia-workspaces")
	current := filepath.Join(root, projectID, conversationID)
	for {
		if err := os.Remove(current); err != nil {
			return
		}
		if sameCleanPath(current, root) {
			return
		}
		current = filepath.Dir(current)
	}
}

// removeProjectConversationWorktrees force-removes conversation worktrees as
// part of destructive project deletion. The project itself is being deleted,
// so preserving unmerged conversation branches would only leave unreachable
// Git resources behind.
func (s *Server) removeProjectConversationWorktrees(ctx context.Context, projectID string) error {
	return s.removeConversationWorktrees(ctx, projectID, "")
}

// removeConversationWorktrees force-removes the isolated Git worktrees owned by
// one conversation (or every project conversation when conversationID is empty).
func (s *Server) removeConversationWorktrees(ctx context.Context, projectID, conversationID string) error {
	filter := `where p.id=?`
	args := []any{projectID}
	if conversationID != "" {
		filter += ` and c.id=?`
		args = append(args, conversationID)
	}
	rows, err := s.db.QueryContext(ctx, `select p.path,w.conversation_id,w.generation,w.mode,w.path,w.branch,w.state
		from conversation_workspaces w join conversations c on c.id=w.conversation_id join projects p on p.id=c.project_id
		`+filter+` and w.mode='isolated_worktree'`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	type item struct {
		projectPath, conversationID, path, branch, state string
		generation                                       int
	}
	items := []item{}
	for rows.Next() {
		var value item
		var mode string
		if err := rows.Scan(&value.projectPath, &value.conversationID, &value.generation, &mode, &value.path, &value.branch, &value.state); err != nil {
			return err
		}
		items = append(items, value)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(items) == 0 {
		return nil
	}
	runner, repo, err := s.gitRunnerForProject(ctx, projectID)
	if err != nil {
		return err
	}
	git, ok := runner.(workspaceGitRunner)
	if !ok {
		return errors.New("runner does not support isolated Git worktree cleanup")
	}
	for _, value := range items {
		if !isExpectedConversationWorktree(value.projectPath, projectID, value.conversationID, value.generation, value.path) {
			continue
		}
		if _, statErr := os.Stat(value.path); statErr == nil {
			if _, err := git.runGit(ctx, repo, "worktree", "remove", "--force", value.path); err != nil {
				return fmt.Errorf("remove project worktree: %w", err)
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("inspect project worktree: %w", statErr)
		}
		if value.branch != "" {
			if _, err := git.runGit(ctx, repo, "branch", "-D", value.branch); err != nil {
				// A missing branch is already clean; other failures must be visible.
				if exists, checkErr := conversationBranchExists(ctx, git, repo, value.branch); checkErr != nil || exists {
					return fmt.Errorf("remove project worktree branch: %w", err)
				}
			}
		}
		removeEmptyConversationWorktreeParents(value.projectPath, projectID, value.conversationID)
	}
	_, _ = git.runGit(ctx, repo, "worktree", "prune")
	return nil
}

func (s *Server) createConversationWorktree(w http.ResponseWriter, r *http.Request) {
	conversationID := chi.URLParam(r, "conversationID")
	s.projectLifecycleMu.Lock()
	defer s.projectLifecycleMu.Unlock()

	var project Project
	var conversationProjectID, status string
	err := s.db.QueryRowContext(r.Context(), `select p.id,p.name,p.path,coalesce(nullif(p.runner_id,''),p.runner),p.git_branch,p.claude_ready,p.created_at,coalesce(p.default_profile_id,''),c.project_id,c.status
		from conversations c join projects p on p.id=c.project_id where c.id=?`, conversationID).Scan(&project.ID, &project.Name, &project.Path, &project.Runner, &project.GitBranch, &project.ClaudeReady, &project.CreatedAt, &project.DefaultProfileID, &conversationProjectID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("conversation not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if status != "idle" {
		writeError(w, http.StatusConflict, errors.New("stop the active agent run before creating an isolated workspace"))
		return
	}
	if !isLocalRunnerID(project.Runner) && project.Runner != "wsl-local" {
		writeError(w, http.StatusConflict, errors.New("isolated conversation workspaces currently require a local runner"))
		return
	}

	release, acquired := s.acquireProjectWorkspace(project.ID, "workspace-create:"+conversationID)
	if !acquired {
		writeError(w, http.StatusConflict, s.projectWorkspaceOccupiedError(project.ID))
		return
	}
	defer release()

	gitRunner, repo, err := s.gitRunnerForProject(r.Context(), project.ID)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	runner, ok := gitRunner.(workspaceGitRunner)
	if !ok {
		writeError(w, http.StatusConflict, errors.New("runner does not support isolated Git worktrees"))
		return
	}
	baseBytes, err := runner.runGit(r.Context(), repo, "rev-parse", "HEAD")
	if err != nil {
		writeError(w, http.StatusConflict, errors.New("project is not currently a readable Git repository"))
		return
	}
	baseRevision := strings.TrimSpace(string(baseBytes))
	if baseRevision == "" {
		writeError(w, http.StatusConflict, errors.New("project Git repository has no HEAD commit"))
		return
	}

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	var generation int
	if err := tx.QueryRowContext(r.Context(), `select coalesce(max(generation),0)+1 from conversation_workspaces where conversation_id=?`, conversationID).Scan(&generation); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	path := conversationWorktreePath(project.Path, project.ID, conversationID, generation)
	branch := conversationWorktreeBranch(conversationID, generation)
	if err := validateGitRef(branch); err != nil || !isExpectedConversationWorktree(project.Path, project.ID, conversationID, generation, path) {
		writeError(w, http.StatusInternalServerError, errors.New("could not derive an isolated workspace path"))
		return
	}
	workspace := ConversationWorkspace{ID: uuid.NewString(), ConversationID: conversationID, Generation: generation, Mode: "isolated_worktree", Path: path, Branch: branch, BaseRevision: baseRevision, State: "provisioning", CreatedAt: time.Now().UTC()}
	if _, err := tx.ExecContext(r.Context(), `insert into conversation_workspaces (id,conversation_id,generation,mode,path,branch,base_revision,state,created_at) values (?,?,?,?,?,?,?,?,?)`, workspace.ID, workspace.ConversationID, workspace.Generation, workspace.Mode, workspace.Path, workspace.Branch, workspace.BaseRevision, workspace.State, workspace.CreatedAt); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	if err := os.MkdirAll(filepath.Dir(path), 0700); err == nil {
		_, err = runner.runGit(r.Context(), repo, "worktree", "add", "-b", branch, path, baseRevision)
	}
	if err != nil {
		_, _ = s.db.ExecContext(r.Context(), `update conversation_workspaces set state='failed' where id=? and state='provisioning'`, workspace.ID)
		writeError(w, http.StatusConflict, fmt.Errorf("create isolated conversation workspace: %w", err))
		return
	}
	workspace.State = "ready"
	if _, err := s.db.ExecContext(r.Context(), `update conversation_workspaces set state='ready' where id=? and state='provisioning'`, workspace.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, workspace)
}

func (s *Server) workspacePathIsExpectedForProject(ctx context.Context, project Project, workspace ConversationWorkspace) (bool, error) {
	rows, err := s.db.QueryContext(ctx, `select w.conversation_id,w.generation from conversation_workspaces w join conversations c on c.id=w.conversation_id where c.project_id=? and w.mode='isolated_worktree' and w.path=?`, project.ID, workspace.Path)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var conversationID string
		var generation int
		if err := rows.Scan(&conversationID, &generation); err != nil {
			return false, err
		}
		if isExpectedConversationWorktree(project.Path, project.ID, conversationID, generation, workspace.Path) {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *Server) activateConversationWorkspace(w http.ResponseWriter, r *http.Request) {
	conversationID := chi.URLParam(r, "conversationID")
	workspaceID := chi.URLParam(r, "workspaceID")
	s.projectLifecycleMu.Lock()
	defer s.projectLifecycleMu.Unlock()

	var status string
	err := s.db.QueryRowContext(r.Context(), `select c.status from conversations c join conversation_workspaces w on w.conversation_id=c.id where c.id=? and w.id=? and w.state='ready'`, conversationID, workspaceID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("conversation workspace not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if status != "idle" {
		writeError(w, http.StatusConflict, errors.New("stop the active agent run before changing the workspace"))
		return
	}
	if !s.sessionManager.retireForConfiguration(conversationID) {
		writeError(w, http.StatusConflict, errors.New("native agent session is stopping for a workspace change; retry shortly"))
		return
	}
	if _, err := s.db.ExecContext(r.Context(), `update conversations set active_workspace_id=? where id=?`, workspaceID, conversationID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"activeWorkspaceId": workspaceID})
}

// archiveConversationWorkspace removes only a server-derived isolated
// worktree. Run records keep their immutable path snapshot; the directory is
// removed only after every live owner has released it.
func (s *Server) archiveConversationWorkspace(w http.ResponseWriter, r *http.Request) {
	conversationID := chi.URLParam(r, "conversationID")
	workspaceID := chi.URLParam(r, "workspaceID")
	s.projectLifecycleMu.Lock()
	defer s.projectLifecycleMu.Unlock()

	var project Project
	var workspace ConversationWorkspace
	var activeID, conversationStatus string
	err := s.db.QueryRowContext(r.Context(), `select p.id,p.name,p.path,coalesce(nullif(p.runner_id,''),p.runner),p.git_branch,p.claude_ready,p.created_at,coalesce(p.default_profile_id,''),
		w.id,w.conversation_id,w.generation,w.mode,w.path,w.branch,w.base_revision,w.state,w.created_at,w.archived_at,c.active_workspace_id,c.status
		from conversation_workspaces w join conversations c on c.id=w.conversation_id join projects p on p.id=c.project_id
		where w.id=? and c.id=?`, workspaceID, conversationID).Scan(
		&project.ID, &project.Name, &project.Path, &project.Runner, &project.GitBranch, &project.ClaudeReady, &project.CreatedAt, &project.DefaultProfileID,
		&workspace.ID, &workspace.ConversationID, &workspace.Generation, &workspace.Mode, &workspace.Path, &workspace.Branch, &workspace.BaseRevision, &workspace.State, &workspace.CreatedAt, &workspace.ArchivedAt, &activeID, &conversationStatus,
	)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("conversation workspace not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if workspace.Mode != "isolated_worktree" || workspace.State != "ready" || workspace.ID == activeID {
		writeError(w, http.StatusConflict, errors.New("only a non-active ready isolated workspace can be archived"))
		return
	}
	expectedPath, err := s.workspacePathIsExpectedForProject(r.Context(), project, workspace)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if conversationStatus != "idle" || !expectedPath {
		writeError(w, http.StatusConflict, errors.New("conversation workspace is not safe to archive"))
		return
	}
	var activeRuns int
	if err := s.db.QueryRowContext(r.Context(), `select count(*) from runs r join conversation_workspaces w on w.id=r.workspace_id join conversations c on c.id=w.conversation_id where c.project_id=? and w.path=? and r.status in ('queued','running')`, project.ID, workspace.Path).Scan(&activeRuns); err != nil || activeRuns != 0 {
		writeError(w, http.StatusConflict, errors.New("stop active runs before archiving this workspace"))
		return
	}
	var otherReferences int
	if err := s.db.QueryRowContext(r.Context(), `select count(*) from conversation_workspaces w join conversations c on c.id=w.conversation_id where w.id<>? and c.project_id=? and w.path=? and w.state in ('ready','provisioning')`, workspace.ID, project.ID, workspace.Path).Scan(&otherReferences); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if otherReferences > 0 {
		// This row is only an alias for another conversation's live directory.
		// Detach it without removing the shared worktree or branch.
		now := time.Now().UTC()
		if _, err := s.db.ExecContext(r.Context(), `update conversation_workspaces set state='archived',archived_at=? where id=? and state='ready'`, now, workspace.ID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"workspaceId": workspace.ID, "state": "archived"})
		return
	}
	if s.workspaceHasTerminalForPath(r.Context(), project.ID, workspace.Path) || s.workspaceProjectRunActive(project.ID, workspace.Path) {
		writeError(w, http.StatusConflict, errors.New("close terminal and project run before archiving this workspace"))
		return
	}
	key := workspaceLeaseKey(project, workspace)
	release, acquired := s.acquireWorkspace(key, "workspace-archive:"+workspace.ID)
	if !acquired {
		writeError(w, http.StatusConflict, s.workspaceOccupiedError(key))
		return
	}
	defer release()
	runner, repo, err := s.gitRunnerForProject(r.Context(), project.ID)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	git, ok := runner.(workspaceGitRunner)
	if !ok {
		writeError(w, http.StatusConflict, errors.New("runner does not support isolated Git worktree cleanup"))
		return
	}
	pathExists := false
	if _, statErr := os.Stat(workspace.Path); statErr == nil {
		pathExists = true
	} else if !errors.Is(statErr, os.ErrNotExist) {
		writeError(w, http.StatusConflict, fmt.Errorf("inspect isolated workspace: %w", statErr))
		return
	}
	if pathExists {
		if dirty, err := git.runGit(r.Context(), workspace.Path, "status", "--porcelain"); err != nil {
			writeError(w, http.StatusConflict, fmt.Errorf("inspect isolated workspace: %w", err))
			return
		} else if strings.TrimSpace(string(dirty)) != "" {
			writeError(w, http.StatusConflict, errors.New("commit, stash, or discard all workspace changes before archiving"))
			return
		}
	}
	registered := pathExists
	if !pathExists {
		registered, err = conversationWorktreeRegistered(r.Context(), git, repo, workspace.Path)
		if err != nil {
			writeError(w, http.StatusConflict, fmt.Errorf("inspect isolated workspace metadata: %w", err))
			return
		}
	}
	branchExists := false
	if workspace.Branch != "" {
		branchExists, err = conversationBranchExists(r.Context(), git, repo, workspace.Branch)
		if err != nil {
			writeError(w, http.StatusConflict, fmt.Errorf("inspect isolated workspace branch: %w", err))
			return
		}
		if branchExists && (pathExists || registered) {
			if _, err := git.runGit(r.Context(), repo, "merge-base", "--is-ancestor", workspace.Branch, "HEAD"); err != nil {
				writeError(w, http.StatusConflict, errors.New("merge or preserve the isolated workspace branch before archiving"))
				return
			}
		}
	}
	if registered {
		args := []string{"worktree", "remove"}
		if !pathExists {
			// The directory may have been removed by a previous attempt. The
			// expected path is empty, so force only clears stale Git metadata.
			args = append(args, "--force")
		}
		args = append(args, workspace.Path)
		if _, err := git.runGit(r.Context(), repo, args...); err != nil {
			writeError(w, http.StatusConflict, fmt.Errorf("remove isolated workspace: %w", err))
			return
		}
	} else if pathExists {
		writeError(w, http.StatusConflict, errors.New("isolated workspace Git metadata is missing"))
		return
	}
	if !pathExists {
		// A prior interrupted cleanup may have removed the directory but left
		// the linked-worktree record. Prune only missing worktrees before branch
		// deletion so the retry is idempotent.
		if _, err := git.runGit(r.Context(), repo, "worktree", "prune"); err != nil {
			writeError(w, http.StatusConflict, fmt.Errorf("prune removed workspace metadata: %w", err))
			return
		}
	}
	if workspace.Branch != "" && branchExists {
		deleteMode := "-d"
		if !pathExists && !registered {
			// Recovery for a prior partial cleanup: the worktree is already gone,
			// so its branch can no longer be used to recover files.
			deleteMode = "-D"
		}
		if _, err := git.runGit(r.Context(), repo, "branch", deleteMode, workspace.Branch); err != nil {
			writeError(w, http.StatusConflict, fmt.Errorf("remove isolated workspace branch: %w", err))
			return
		}
	}
	removeEmptyConversationWorktreeParents(project.Path, project.ID, conversationID)
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(r.Context(), `update conversation_workspaces set state='archived',archived_at=? where id=? and state='ready'`, now, workspace.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"workspaceId": workspace.ID, "state": "archived"})
}

func (s *Server) workspaceHasTerminal(workspaceID string) bool {
	if s.terminals == nil {
		return false
	}
	s.terminals.mu.Lock()
	defer s.terminals.mu.Unlock()
	for _, terminal := range s.terminals.sessions {
		if terminal.workspaceID == workspaceID {
			return true
		}
	}
	return false
}

func (s *Server) workspaceHasTerminalForPath(ctx context.Context, projectID, workspacePath string) bool {
	rows, err := s.db.QueryContext(ctx, `select w.id from conversation_workspaces w join conversations c on c.id=w.conversation_id where c.project_id=? and w.path=? and w.state in ('ready','provisioning')`, projectID, workspacePath)
	if err != nil {
		return true
	}
	defer rows.Close()
	for rows.Next() {
		var workspaceID string
		if err := rows.Scan(&workspaceID); err != nil {
			return true
		}
		if s.workspaceHasTerminal(workspaceID) {
			return true
		}
	}
	return rows.Err() != nil
}

func (s *Server) workspaceProjectRunActive(projectID, workspacePath string) bool {
	s.runManagersMu.RLock()
	runner := s.runManagers[runManagerKey(projectID, workspacePath)]
	s.runManagersMu.RUnlock()
	if runner == nil {
		return false
	}
	status := runner.LightStatusSnapshot().Status
	return status == RunStatusStarting || status == RunStatusRunning || status == RunStatusStopping
}
