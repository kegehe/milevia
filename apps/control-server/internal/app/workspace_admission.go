package app

// 托管排他准入叠加在现有工作区租约之上。普通操作彼此的兼容规则不变，
// 但都要登记实际目录，pending/owned/releasing 会阻止新的重叠目录操作。
// 锁顺序：SQLite 事务 -> workspaceAdmissionMu；持有此锁时不调用模型或进程。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

var errWorkspaceSupervised = errors.New("该目录正由任务托管持有，请在任务托管页停止并接管，或等待托管结束")
var errWorkspaceAdmissionStale = errors.New("workspace admission version or owner changed")
var errWorkspaceNotQuiescent = errors.New("workspace still has active or unconfirmed execution")

type workspaceScope struct {
	Key          string
	Path         string
	Domain       string
	RunnerID     string
	OriginalPath string
}

type workspaceOperation struct {
	Scope workspaceScope
	Owner string
}

type WorkspaceAdmission struct {
	Domain            string    `json:"-"`
	ID                string    `json:"id"`
	ProjectID         string    `json:"projectId"`
	OwnerID           string    `json:"ownerId"`
	OriginalPath      string    `json:"-"`
	CanonicalPath     string    `json:"-"`
	DirectoryIdentity string    `json:"-"`
	State             string    `json:"state"`
	Epoch             int64     `json:"epoch"`
	RecoveryState     string    `json:"recoveryState"`
	CreatedAt         time.Time `json:"createdAt"`
	UpdatedAt         time.Time `json:"updatedAt"`
}

func canonicalWorkspacePath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("workspace path is empty")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	// 尚未创建的 worktree 也必须按真实父目录保护；不能仅凭词法路径绕过链接。
	current := filepath.Clean(absolute)
	var missing []string
	for {
		resolved, resolveErr := filepath.EvalSymlinks(current)
		if resolveErr == nil {
			resolved, resolveErr = workspaceFinalDirectoryPath(resolved)
			if resolveErr != nil {
				return "", resolveErr
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			resolved = filepath.Clean(resolved)
			if runtime.GOOS == "windows" && !isWSLUncPath(resolved) {
				resolved = strings.ToLower(resolved)
			}
			return resolved, nil
		}
		if !errors.Is(resolveErr, os.ErrNotExist) {
			return "", resolveErr
		}
		// 悬空链接不能被当作尚未创建的普通目录。
		if info, statErr := os.Lstat(current); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", resolveErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", resolveErr
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func workspaceScopesOverlap(a, b workspaceScope) bool {
	if a.Domain != b.Domain {
		return false
	}
	if a.Path == "" || b.Path == "" {
		return true // 无法解析的已有操作不得被当成已收敛。
	}
	return isPathWithin(a.Path, b.Path) || isPathWithin(b.Path, a.Path)
}

func (s *Server) rememberWorkspaceScope(key, path, runnerID string) {
	scope := workspaceScope{Key: key, Domain: "local", RunnerID: runnerID, OriginalPath: path}
	if isLocalRunnerID(runnerID) {
		scope.Path, _ = canonicalWorkspacePath(path)
	} else {
		// 远端路径先保留管理命名空间；实际共享保护在目标侧按物理目录核对。
		scope.Domain, scope.Path = "runner:"+runnerID, filepath.Clean(path)
	}
	s.workspaceAdmissionMu.Lock()
	defer s.workspaceAdmissionMu.Unlock()
	if s.workspaceScopes == nil {
		s.workspaceScopes = make(map[string]workspaceScope)
	}
	s.workspaceScopes[key] = scope
}

func (s *Server) workspaceScopeLocked(key string) workspaceScope {
	if scope, ok := s.workspaceScopes[key]; ok {
		return scope
	}
	return workspaceScope{Key: key, Domain: "local"}
}

func (s *Server) admissionOverlapsLocked(admission WorkspaceAdmission, scope workspaceScope) bool {
	if scope.Key == admission.ProjectID {
		return true // 项目根路径被移动或链接被替换后仍不能通过同一项目入口绕开。
	}
	domain := admission.Domain
	if domain == "" {
		domain = "local"
	}
	if workspaceScopesOverlap(workspaceScope{Path: admission.CanonicalPath, Domain: domain}, scope) {
		return true
	}
	// 同时保护当前项目根解析出的新位置，不用新路径覆盖原始归属。
	current, exists := s.workspaceScopes[admission.ProjectID]
	return exists && workspaceScopesOverlap(current, scope)
}

func (s *Server) beginWorkspaceOperation(key, owner string) (func(), error) {
	return s.beginWorkspaceOperationMode(key, owner, true)
}

func (s *Server) beginWorkspaceOperationMode(key, owner string, shared bool) (func(), error) {
	if strings.HasPrefix(key, "path:") {
		s.workspaceAdmissionMu.Lock()
		scope, known := s.workspaceScopes[key]
		s.workspaceAdmissionMu.Unlock()
		if !known || scope.Domain == "local" {
			s.rememberWorkspaceScope(key, strings.TrimPrefix(key, "path:"), s.localRunnerID())
		}
	}
	s.workspaceAdmissionMu.Lock()
	scope := s.workspaceScopeLocked(key)
	for _, admission := range s.workspaceAdmissions {
		if s.admissionOverlapsLocked(admission, scope) {
			s.workspaceAdmissionMu.Unlock()
			return nil, errWorkspaceSupervised
		}
	}
	if s.workspaceOperations == nil {
		s.workspaceOperations = make(map[string]workspaceOperation)
	}
	id := uuid.NewString()
	s.workspaceOperations[id] = workspaceOperation{Scope: scope, Owner: owner}
	s.workspaceAdmissionMu.Unlock()
	releaseShared := func() {}
	var err error
	if shared {
		releaseShared, err = s.beginSharedWorkspaceOperation(scope)
	}
	if err != nil {
		s.workspaceAdmissionMu.Lock()
		delete(s.workspaceOperations, id)
		s.workspaceAdmissionMu.Unlock()
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			releaseShared()
			s.workspaceAdmissionMu.Lock()
			delete(s.workspaceOperations, id)
			s.workspaceAdmissionMu.Unlock()
		})
	}, nil
}

func (s *Server) beginWorkspaceOperationAtPath(project Project, path, owner string) (func(), error) {
	if project.RunnerID != "" {
		project.Runner = project.RunnerID
	}
	key := "path:" + filepath.Clean(path)
	// 远端不能使用 path: 前缀，避免 beginWorkspaceOperation 按本机重解析。
	if !isLocalRunnerID(project.Runner) {
		key = "remote:" + project.Runner + ":" + path
	}
	s.rememberWorkspaceScope(key, path, project.Runner)
	return s.beginWorkspaceOperation(key, owner)
}

func (s *Server) beginWorkspaceOperationsAtPaths(project Project, paths []string, owner string) (func(), error) {
	var releases []func()
	releaseAll := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	for _, path := range paths {
		release, err := s.beginWorkspaceOperationAtPath(project, path, owner)
		if err != nil {
			releaseAll()
			return nil, err
		}
		releases = append(releases, release)
	}
	return releaseAll, nil
}

func (s *Server) migrateWorkspaceAdmissions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `create table if not exists workspace_admissions (
		id text primary key,
		project_id text not null references projects(id) on delete restrict,
		owner_id text not null unique,
		original_path text not null,
		canonical_path text not null,
		directory_identity text not null,
		state text not null check(state in ('pending','owned','releasing')),
		epoch integer not null check(epoch>0),
		recovery_state text not null,
		created_at datetime not null,
		updated_at datetime not null
	);
	create unique index if not exists workspace_admissions_project on workspace_admissions(project_id);`)
	if err != nil {
		return err
	}
	if err := ensureColumn(ctx, s.db, "workspace_admissions", "domain", "text not null default 'local'"); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `create table if not exists workspace_guard_bindings (connection_key text primary key, target_id text not null);
	create table if not exists workspace_guard_operations (
		id text primary key, connection_key text not null, path text not null,
		state text not null, created_at datetime not null, updated_at datetime not null
	)`)
	if err != nil {
		return err
	}
	if err := ensureColumn(ctx, s.db, "workspace_guard_operations", "target_id", "text not null default ''"); err != nil {
		return err
	}
	return ensureColumn(ctx, s.db, "workspace_guard_operations", "valid_until", "datetime")
}

// withWorkspaceAdmissionTx 供后续 start 事务同时认领任务、保存会话和建立 pending 准入。
// 先获得 SQLite 连接再锁内存镜像，避免一个已持有事务的 Run 反向等待而死锁。
// fn 只能使用 tx，不得再访问 s.db、启动进程或等待资源。
func (s *Server) withWorkspaceAdmissionTx(ctx context.Context, fn func(*sql.Tx, map[string]WorkspaceAdmission) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	s.workspaceAdmissionMu.Lock()
	defer s.workspaceAdmissionMu.Unlock()
	next := make(map[string]WorkspaceAdmission, len(s.workspaceAdmissions))
	for id, admission := range s.workspaceAdmissions {
		next[id] = admission
	}
	if err := fn(tx, next); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.workspaceAdmissions = next
	return nil
}

func (s *Server) reserveWorkspaceAdmissionTx(ctx context.Context, tx *sql.Tx, next map[string]WorkspaceAdmission, project Project, ownerID string) (WorkspaceAdmission, error) {
	if strings.TrimSpace(ownerID) == "" {
		return WorkspaceAdmission{}, errWorkspaceAdmissionStale
	}
	route := s.supervisionProjectRoute(project)
	domain, canonical, identity := "local", "", ""
	if route.local() {
		var err error
		canonical, err = canonicalWorkspacePath(project.Path)
		if err != nil {
			return WorkspaceAdmission{}, err
		}
		if runtime.GOOS == "windows" && isWSLUncPath(canonical) {
			return WorkspaceAdmission{}, errors.New("项目实际指向 WSL 目录，请选择对应 WSL Runner 后托管")
		}
		identity, err = workspaceDirectoryIdentity(canonical)
		if err != nil {
			return WorkspaceAdmission{}, err
		}
		if runtime.GOOS != "windows" && !s.supervisionProjectRoute(Project{Runner: project.Runner, Path: canonical}).local() {
			return WorkspaceAdmission{}, errors.New("项目实际指向 Windows 共享目录，请使用实际 Windows 路径建立项目后托管")
		}
	} else {
		domain = route.Key
	}
	for _, admission := range next {
		if admission.OwnerID == ownerID && admission.ProjectID == project.ID && admission.CanonicalPath == canonical && admission.DirectoryIdentity == identity {
			return admission, nil
		}
		if admission.OwnerID == ownerID || admission.ProjectID == project.ID || (canonical != "" && s.admissionOverlapsLocked(admission, workspaceScope{Path: canonical, Domain: domain})) {
			return WorkspaceAdmission{}, errWorkspaceSupervised
		}
	}
	now := time.Now().UTC()
	admission := WorkspaceAdmission{Domain: domain, ID: uuid.NewString(), ProjectID: project.ID, OwnerID: ownerID, OriginalPath: project.Path, CanonicalPath: canonical, DirectoryIdentity: identity, State: "pending", Epoch: 1, RecoveryState: "ready", CreatedAt: now, UpdatedAt: now}
	_, err := tx.ExecContext(ctx, `insert into workspace_admissions(id,project_id,owner_id,original_path,canonical_path,directory_identity,state,epoch,recovery_state,created_at,updated_at,domain) values (?,?,?,?,?,?,?,?,?,?,?,?)`, admission.ID, project.ID, ownerID, project.Path, canonical, identity, admission.State, admission.Epoch, admission.RecoveryState, now, now, domain)
	if err != nil {
		return WorkspaceAdmission{}, err
	}
	next[admission.ID] = admission
	return admission, nil
}

func (s *Server) admissionHasOperationsLocked(admission WorkspaceAdmission) bool {
	for _, operation := range s.workspaceOperations {
		if s.admissionOverlapsLocked(admission, operation.Scope) {
			return true
		}
	}
	return false
}

func (s *Server) transitionWorkspaceAdmission(ctx context.Context, id, ownerID string, epoch int64, target string) error {
	return s.withWorkspaceAdmissionTx(ctx, func(tx *sql.Tx, next map[string]WorkspaceAdmission) error {
		admission, exists := next[id]
		if !exists || admission.OwnerID != ownerID || admission.Epoch != epoch {
			return errWorkspaceAdmissionStale
		}
		if admission.RecoveryState != "ready" || s.admissionHasOperationsLocked(admission) {
			return errWorkspaceNotQuiescent
		}
		if target != "owned" && target != "releasing" {
			return errors.New("unsupported workspace admission transition")
		}
		if (target == "owned" && admission.State != "pending") || (target == "releasing" && admission.State != "owned" && admission.State != "pending") {
			return errWorkspaceAdmissionStale
		}
		if err := verifyWorkspaceAdmissionPath(admission); err != nil {
			return err
		}
		if err := supervisionRunsQuiescentTx(ctx, tx, ownerID); err != nil {
			return err
		}
		admission.State, admission.UpdatedAt = target, time.Now().UTC()
		if _, err := tx.ExecContext(ctx, `update workspace_admissions set state=?,updated_at=? where id=?`, target, admission.UpdatedAt, id); err != nil {
			return err
		}
		next[id] = admission
		return nil
	})
}

func verifyWorkspaceAdmissionPath(admission WorkspaceAdmission) error {
	if admission.Domain != "" && admission.Domain != "local" {
		return errSupervisionProcessUnsupported
	}
	canonical, err := canonicalWorkspacePath(admission.OriginalPath)
	if err != nil {
		return err
	}
	identity, err := workspaceDirectoryIdentity(canonical)
	if err != nil {
		return err
	}
	if canonical != admission.CanonicalPath || identity != admission.DirectoryIdentity {
		return errors.New("workspace directory was moved, replaced or retargeted")
	}
	return nil
}

func (s *Server) releaseWorkspaceAdmission(ctx context.Context, id, ownerID string, epoch int64) error {
	return s.withWorkspaceAdmissionTx(ctx, func(tx *sql.Tx, next map[string]WorkspaceAdmission) error {
		admission, exists := next[id]
		if !exists {
			return nil // 终态清理幂等。
		}
		if admission.OwnerID != ownerID || admission.Epoch != epoch || admission.State != "releasing" {
			return errWorkspaceAdmissionStale
		}
		var liveSession, terminalSession bool
		if err := tx.QueryRowContext(ctx, `select exists(select 1 from supervision_sessions where id=? and state not in ('stopped','completed','completed_with_issues','failed')),exists(select 1 from supervision_sessions where id=? and state in ('stopped','completed','completed_with_issues','failed'))`, ownerID, ownerID).Scan(&liveSession, &terminalSession); err != nil {
			return err
		}
		if liveSession {
			return errWorkspaceNotQuiescent
		}
		var targetReleasePending bool
		if err := tx.QueryRowContext(ctx, `select exists(select 1 from supervision_target_controls c where c.session_id=? and not exists(select 1 from supervision_target_releases r where r.session_id=c.session_id and r.target_id=json_extract(c.control_json,'$.targetId') and r.workspace_id=? and r.admission_epoch=?))`, ownerID, admission.DirectoryIdentity, admission.Epoch).Scan(&targetReleasePending); err != nil {
			return err
		}
		if targetReleasePending {
			return errWorkspaceNotQuiescent
		}
		if s.admissionHasOperationsLocked(admission) {
			return errWorkspaceNotQuiescent
		}
		if err := supervisionRunsQuiescentTx(ctx, tx, ownerID); err != nil {
			return err
		}
		// 正式终态且全部执行已静默时，只释放元数据归属。目录漂移不得
		// 使停止永久锁住任务；没有正式会话的兼容准入仍保留原身份核对。
		if err := verifyWorkspaceAdmissionPath(admission); err != nil && !terminalSession {
			return err
		}
		rows, err := tx.QueryContext(ctx, `select task_id from task_execution_claims where owner_kind='supervision' and owner_id=?`, ownerID)
		if err != nil {
			return err
		}
		var taskIDs []string
		for rows.Next() {
			var taskID string
			if err := rows.Scan(&taskID); err != nil {
				rows.Close()
				return err
			}
			taskIDs = append(taskIDs, taskID)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, taskID := range taskIDs {
			if err := releaseTaskExecutionTx(ctx, tx, taskID, "supervision", ownerID); err != nil {
				return err
			}
		}
		if admission.Domain == "" || admission.Domain == "local" {
			if _, err := changeWorkspaceGuard(ctx, s.config, "release", workspaceGuardRecord{ID: ownerID, Kind: "supervision"}); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `delete from workspace_admissions where id=? and owner_id=? and epoch=? and state='releasing'`, id, ownerID, epoch); err != nil {
			return err
		}
		delete(next, id)
		return nil
	})
}

// recoverWorkspaceAdmissions 必须在通用 Run 恢复和所有 worker 之前运行。
// owned/pending 一律保留保护、增加 epoch 并要求专属核对，不能凭内存操作为空即继续。
func (s *Server) recoverWorkspaceAdmissions(ctx context.Context) error {
	// These are operations from a previous controller lifetime. No in-memory
	// reader/waiter can certify their exit after restart.
	if _, err := s.db.ExecContext(ctx, `update workspace_guard_operations set state='unconfirmed',updated_at=? where state='active'`, time.Now().UTC()); err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `select id,path,coalesce(nullif(runner_id,''),runner) from projects`)
	if err != nil {
		return err
	}
	var projects []Project
	for rows.Next() {
		var project Project
		if err := rows.Scan(&project.ID, &project.Path, &project.Runner); err != nil {
			rows.Close()
			return err
		}
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, project := range projects {
		s.rememberWorkspaceScope(project.ID, project.Path, project.Runner)
	}
	rows, err = s.db.QueryContext(ctx, `select id,project_id,owner_id,original_path,canonical_path,directory_identity,state,epoch,recovery_state,created_at,updated_at,domain from workspace_admissions`)
	if err != nil {
		return err
	}
	var admissions []WorkspaceAdmission
	for rows.Next() {
		var a WorkspaceAdmission
		if err := rows.Scan(&a.ID, &a.ProjectID, &a.OwnerID, &a.OriginalPath, &a.CanonicalPath, &a.DirectoryIdentity, &a.State, &a.Epoch, &a.RecoveryState, &a.CreatedAt, &a.UpdatedAt, &a.Domain); err != nil {
			rows.Close()
			return err
		}
		admissions = append(admissions, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	err = s.withWorkspaceAdmissionTx(ctx, func(tx *sql.Tx, next map[string]WorkspaceAdmission) error {
		for _, a := range admissions {
			a.Epoch++
			a.RecoveryState, a.UpdatedAt = "reconciling", time.Now().UTC()
			_, targetErr := loadSupervisionTargetControl(ctx, tx, a.OwnerID)
			if targetErr != nil && !errors.Is(targetErr, sql.ErrNoRows) {
				return targetErr
			}
			if errors.Is(targetErr, sql.ErrNoRows) && (a.Domain == "" || a.Domain == "local") && verifyWorkspaceAdmissionPath(a) != nil {
				a.RecoveryState = "path_changed"
			}
			if _, err := tx.ExecContext(ctx, `update workspace_admissions set epoch=?,recovery_state=?,updated_at=? where id=?`, a.Epoch, a.RecoveryState, a.UpdatedAt, a.ID); err != nil {
				return err
			}
			next[a.ID] = a
		}
		if _, err := tx.ExecContext(ctx, `update supervision_run_links set process_state='unknown',outcome_state='unknown' where process_state in ('launching','running')`); err != nil {
			return fmt.Errorf("preserve unconfirmed supervision runs: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	// releasing 说明停机前已收敛；仍需核对路径和全部 Run。
	// 清理失败保留保护，不能凭传输终态释放 unknown。
	for _, a := range admissions {
		if a.State == "releasing" && verifyWorkspaceAdmissionPath(a) == nil {
			_ = s.releaseWorkspaceAdmission(ctx, a.ID, a.OwnerID, a.Epoch+1)
		}
	}
	return nil
}
