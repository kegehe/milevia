package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	_ "github.com/mattn/go-sqlite3"
)

type Config struct {
	DatabasePath   string
	AllowedRoot    string
	ClaudePath     string
	PermissionMode string
	ControlURL     string
	ApprovalHook   string
}

func ConfigFromEnv() Config {
	home, _ := os.UserHomeDir()
	root := os.Getenv("AUTO_ALLOWED_ROOT")
	if root == "" {
		root = home
	}
	db := os.Getenv("AUTO_DATABASE_PATH")
	if db == "" {
		db = "../../data/auto.db"
	}
	mode := os.Getenv("AUTO_CLAUDE_PERMISSION_MODE")
	if mode == "" {
		mode = "acceptEdits"
	}
	if mode != "plan" && mode != "acceptEdits" {
		mode = "acceptEdits"
	}
	controlURL := os.Getenv("AUTO_CONTROL_URL")
	if controlURL == "" {
		controlURL = "http://127.0.0.1:8080"
	}
	hook := os.Getenv("AUTO_APPROVAL_HOOK")
	if hook == "" {
		hook = "../../scripts/claude-approval-hook.sh"
	}
	return Config{DatabasePath: db, AllowedRoot: root, ClaudePath: "claude", PermissionMode: mode, ControlURL: controlURL, ApprovalHook: hook}
}

type Server struct {
	db          *sql.DB
	config      Config
	runner      AgentRunner
	runtimeCtx  context.Context
	runtimeStop context.CancelFunc
	runWG       sync.WaitGroup
	closeOnce   sync.Once
	upgrader    websocket.Upgrader
	mu          sync.Mutex
	closing     bool
	subscribers map[string]map[*websocket.Conn]*subscriber
	cancels     map[string]context.CancelFunc
	runTokens   map[string]string
	runContexts map[string]string
	sessions    map[string]*activeAgentSession
	sessionMu   sync.Mutex
	streamMu    sync.Mutex
	approvals   map[string]*approvalWaiter
	usageMu     sync.Mutex
	runUsage    map[string]*runUsageAccumulator
}

type subscriber struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
}

type approvalWaiter struct {
	runID          string
	conversationID string
	decision       chan string
}

type activeAgentSession struct {
	agent         AgentSession
	approvalToken string
	activeRunID   string
	stopping      bool
}

type Project struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Path        string    `json:"-"`
	PathDisplay string    `json:"pathDisplay"`
	Runner      string    `json:"runner"`
	GitBranch   string    `json:"gitBranch"`
	ClaudeReady bool      `json:"claudeReady"`
	CreatedAt   time.Time `json:"createdAt"`
}

type Conversation struct {
	ID                string    `json:"id"`
	ProjectID         string    `json:"projectId"`
	ClaudeSessionID   string    `json:"claudeSessionId"`
	Status            string    `json:"status"`
	PermissionMode    string    `json:"permissionMode"`
	Title             string    `json:"title"`
	Preview           string    `json:"preview,omitempty"`
	LastActivityAt    time.Time `json:"lastActivityAt"`
	ClaudeInitialized bool      `json:"-"`
	IsCurrent         bool      `json:"isCurrent"`
	CreatedAt         time.Time `json:"createdAt"`
}

type Message struct {
	ID             string    `json:"id"`
	ConversationID string    `json:"conversationId"`
	Role           string    `json:"role"`
	Content        string    `json:"content"`
	CreatedAt      time.Time `json:"createdAt"`
}

type Shortcut struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Description   string    `json:"description"`
	Kind          string    `json:"kind"`
	Template      string    `json:"template"`
	Scope         string    `json:"scope"`
	DefaultAction string    `json:"defaultAction"`
	GroupName     string    `json:"groupName"`
	Pinned        bool      `json:"pinned"`
	Enabled       bool      `json:"enabled"`
	SortOrder     int       `json:"sortOrder"`
	ProjectIDs    []string  `json:"projectIds"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type ShortcutRun struct {
	ID              string     `json:"id"`
	ShortcutID      string     `json:"shortcutId"`
	ConversationID  string     `json:"conversationId"`
	RunID           string     `json:"runId"`
	RenderedContent string     `json:"renderedContent"`
	Action          string     `json:"action"`
	Status          string     `json:"status"`
	CreatedAt       time.Time  `json:"createdAt"`
	CompletedAt     *time.Time `json:"completedAt,omitempty"`
}

type Event struct {
	ID             string          `json:"id"`
	ConversationID string          `json:"conversationId"`
	RunID          string          `json:"runId"`
	Type           string          `json:"type"`
	Payload        json.RawMessage `json:"payload"`
	CreatedAt      time.Time       `json:"createdAt"`
}

func New(ctx context.Context, config Config) (*Server, error) {
	return NewWithRunner(ctx, config, nil)
}

// NewWithRunner allows future AI CLIs to reuse the project and conversation services.
// A nil runner selects the built-in Claude Code CLI implementation.
func NewWithRunner(ctx context.Context, config Config, runner AgentRunner) (*Server, error) {
	if err := os.MkdirAll(filepath.Dir(config.DatabasePath), 0o755); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	pool, err := sql.Open("sqlite3", config.DatabasePath)
	if err != nil {
		return nil, fmt.Errorf("open SQLite: %w", err)
	}
	pool.SetMaxOpenConns(1)
	if _, err := pool.ExecContext(ctx, `pragma foreign_keys=on; pragma journal_mode=wal; pragma busy_timeout=5000`); err != nil {
		pool.Close()
		return nil, fmt.Errorf("configure SQLite: %w", err)
	}
	if runner == nil {
		approvalHook, err := filepath.Abs(config.ApprovalHook)
		if err != nil {
			pool.Close()
			return nil, fmt.Errorf("resolve approval hook: %w", err)
		}
		if info, err := os.Stat(approvalHook); err != nil || info.IsDir() {
			pool.Close()
			return nil, fmt.Errorf("approval hook is unavailable: %s", approvalHook)
		}
		config.ApprovalHook = approvalHook
		runner = newClaudeCLIRunner(config)
	}
	runtimeCtx, runtimeStop := context.WithCancel(context.Background())
	s := &Server{db: pool, config: config, runner: runner, runtimeCtx: runtimeCtx, runtimeStop: runtimeStop, subscribers: map[string]map[*websocket.Conn]*subscriber{}, cancels: map[string]context.CancelFunc{}, runTokens: map[string]string{}, runContexts: map[string]string{}, sessions: map[string]*activeAgentSession{}, approvals: map[string]*approvalWaiter{}, runUsage: map[string]*runUsageAccumulator{}}
	s.upgrader.CheckOrigin = func(r *http.Request) bool { return isLocalWebOrigin(r.Header.Get("Origin")) }
	if err := s.migrate(ctx); err != nil {
		runtimeStop()
		pool.Close()
		return nil, err
	}
	if err := s.recoverInterruptedRuns(ctx); err != nil {
		runtimeStop()
		pool.Close()
		return nil, err
	}
	return s, nil
}

func (s *Server) Close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closing = true
		cancels := make([]context.CancelFunc, 0, len(s.cancels))
		runIDs := make([]string, 0, len(s.cancels))
		sessions := make([]AgentSession, 0, len(s.sessions))
		for runID, cancel := range s.cancels {
			cancels = append(cancels, cancel)
			runIDs = append(runIDs, runID)
		}
		for _, session := range s.sessions {
			session.stopping = true
			sessions = append(sessions, session.agent)
		}
		s.mu.Unlock()
		s.runtimeStop()
		for _, cancel := range cancels {
			cancel()
		}
		for _, session := range sessions {
			session.Stop()
		}
		for _, runID := range runIDs {
			s.resolveRunApprovals(runID, "deny")
		}
		s.runWG.Wait()
		_ = s.db.Close()
	})
}

func (s *Server) Listen(addr string) error { return http.ListenAndServe(addr, s.routes()) }

func (s *Server) routes() http.Handler {
	r := chi.NewRouter()
	r.Use(cors)
	r.Get("/api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Get("/api/runners", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, []map[string]string{{"id": "wsl-local", "name": "WSL Local Runner", "environment": "wsl", "root": s.config.AllowedRoot}})
	})
	r.Get("/api/directories", s.listDirectories)
	r.Post("/api/projects/validate", s.validateProject)
	r.Get("/api/projects", s.listProjects)
	r.Post("/api/projects", s.createProject)
	r.Get("/api/projects/{projectID}/conversations", s.listConversations)
	r.Post("/api/projects/{projectID}/conversations", s.createConversation)
	r.Get("/api/projects/{projectID}/shortcuts", s.listShortcuts)
	r.Post("/api/shortcuts/defaults", s.seedDefaultShortcuts)
	r.Post("/api/shortcuts", s.createShortcut)
	r.Patch("/api/shortcuts/{shortcutID}", s.updateShortcut)
	r.Delete("/api/shortcuts/{shortcutID}", s.deleteShortcut)
	r.Get("/api/conversations/{conversationID}", s.getConversation)
	r.Get("/api/conversations/{conversationID}/input-history", s.listInputHistory)
	r.Get("/api/conversations/{conversationID}/usage", s.getConversationUsage)
	r.Post("/api/conversations/{conversationID}/activate", s.activateConversation)
	r.Post("/api/conversations/{conversationID}/permission-mode", s.setConversationPermissionMode)
	r.Post("/api/conversations/{conversationID}/messages", s.sendMessage)
	r.Post("/api/conversations/{conversationID}/shortcuts/{shortcutID}/preview", s.previewShortcut)
	r.Post("/api/conversations/{conversationID}/shortcuts/{shortcutID}/run", s.runShortcut)
	r.Get("/api/conversations/{conversationID}/shortcut-runs", s.listShortcutRuns)
	r.Post("/api/runs/{runID}/stop", s.stopRun)
	r.Get("/api/runs/{runID}/usage", s.getRunUsage)
	r.Post("/api/approvals/{approvalID}", s.resolveApproval)
	r.Post("/api/internal/approvals/wait", s.waitForApproval)
	r.Get("/ws/conversations/{conversationID}", s.subscribe)
	return r
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); isLocalWebOrigin(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isLocalWebOrigin(origin string) bool {
	parsed, err := url.Parse(origin)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return false
	}
	return parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1"
}

func (s *Server) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `create table if not exists projects (id text primary key, name text not null, path text not null unique, runner text not null, git_branch text not null, claude_ready integer not null, created_at datetime not null);
create table if not exists conversations (id text primary key, project_id text not null references projects(id) on delete cascade, claude_session_id text not null unique, status text not null, permission_mode text not null default 'approval_required', title text not null default '新会话', last_activity_at datetime not null default current_timestamp, claude_initialized integer not null default 0, is_current integer not null default 1, created_at datetime not null);
create table if not exists messages (id text primary key, conversation_id text not null references conversations(id) on delete cascade, role text not null, content text not null, created_at datetime not null);
create table if not exists runs (id text primary key, conversation_id text not null references conversations(id) on delete cascade, status text not null, created_at datetime not null, completed_at datetime);
create table if not exists events (id text primary key, conversation_id text not null references conversations(id) on delete cascade, run_id text not null references runs(id) on delete cascade, type text not null, payload text not null, created_at datetime not null);
create table if not exists run_usage (run_id text primary key references runs(id) on delete cascade, conversation_id text not null references conversations(id) on delete cascade, model text not null default '', context_window integer not null default 0, context_input_tokens integer not null default 0, input_tokens integer not null default 0, output_tokens integer not null default 0, cache_read_tokens integer not null default 0, cache_creation_tokens integer not null default 0, estimated_cost_usd real not null default 0, agent_turns integer not null default 0, model_steps integer not null default 0, tool_calls integer not null default 0, subagent_count integer not null default 0, duration_ms integer not null default 0, ttft_ms integer not null default 0, terminal_reason text not null default '', has_result integer not null default 0, completed_at datetime not null);
create table if not exists run_model_usage (run_id text not null references runs(id) on delete cascade, model text not null, input_tokens integer not null default 0, output_tokens integer not null default 0, cache_read_tokens integer not null default 0, cache_creation_tokens integer not null default 0, estimated_cost_usd real not null default 0, context_window integer not null default 0, primary key (run_id,model));
create table if not exists shortcuts (id text primary key, name text not null, description text not null default '', kind text not null, template text not null, scope text not null, default_action text not null, group_name text not null default '', pinned integer not null default 0, enabled integer not null default 1, sort_order integer not null default 0, created_at datetime not null, updated_at datetime not null);
create table if not exists shortcut_projects (shortcut_id text not null references shortcuts(id) on delete cascade, project_id text not null references projects(id) on delete cascade, primary key (shortcut_id,project_id));
create table if not exists shortcut_runs (id text primary key, shortcut_id text references shortcuts(id) on delete set null, conversation_id text not null references conversations(id) on delete cascade, run_id text unique references runs(id) on delete set null, rendered_content text not null, action text not null, status text not null, created_at datetime not null, completed_at datetime);
create table if not exists shortcut_audit_events (id text primary key, shortcut_id text references shortcuts(id) on delete set null, action text not null, payload text not null default '{}', created_at datetime not null);
create table if not exists app_metadata (key text primary key, value text not null);
create unique index if not exists conversations_one_current_per_project on conversations(project_id) where is_current=1;
create index if not exists messages_conversation_created on messages(conversation_id,created_at desc);
create index if not exists run_usage_conversation_completed on run_usage(conversation_id,completed_at desc);`)
	if err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `pragma table_info(conversations)`)
	if err != nil {
		return fmt.Errorf("inspect conversations schema: %w", err)
	}
	defer rows.Close()
	hasPermissionMode, hasTitle, hasLastActivity := false, false, false
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var primaryKey int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			return fmt.Errorf("read conversations schema: %w", err)
		}
		if name == "permission_mode" {
			hasPermissionMode = true
		}
		if name == "title" {
			hasTitle = true
		}
		if name == "last_activity_at" {
			hasLastActivity = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate conversations schema: %w", err)
	}
	if !hasPermissionMode {
		if _, err := s.db.ExecContext(ctx, `alter table conversations add column permission_mode text not null default 'approval_required'`); err != nil {
			return fmt.Errorf("add conversation permission mode: %w", err)
		}
	}
	if !hasTitle {
		if _, err := s.db.ExecContext(ctx, `alter table conversations add column title text not null default '新会话'`); err != nil {
			return fmt.Errorf("add conversation title: %w", err)
		}
	}
	if !hasLastActivity {
		if _, err := s.db.ExecContext(ctx, `alter table conversations add column last_activity_at datetime`); err != nil {
			return fmt.Errorf("add conversation last activity: %w", err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `update conversations set last_activity_at=coalesce(last_activity_at,(select max(created_at) from messages where messages.conversation_id=conversations.id),created_at)`); err != nil {
		return fmt.Errorf("backfill conversation activity: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `update conversations set title=coalesce(nullif((select substr(trim(content),1,80) from messages where messages.conversation_id=conversations.id and role='user' order by created_at limit 1),''),'新会话') where title='' or title='新会话'`); err != nil {
		return fmt.Errorf("backfill conversation title: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `create index if not exists conversations_project_activity on conversations(project_id,last_activity_at desc)`); err != nil {
		return fmt.Errorf("create conversation activity index: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `create index if not exists shortcuts_visible on shortcuts(scope,enabled,pinned,sort_order);
create index if not exists shortcut_projects_project on shortcut_projects(project_id);
create index if not exists shortcut_runs_conversation on shortcut_runs(conversation_id,created_at desc);
create index if not exists shortcut_audit_events_shortcut on shortcut_audit_events(shortcut_id,created_at desc);`); err != nil {
		return fmt.Errorf("create shortcut indexes: %w", err)
	}
	return nil
}

func (s *Server) recoverInterruptedRuns(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin interrupted run recovery: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `select id,conversation_id from runs where status in ('queued','running')`)
	if err != nil {
		return fmt.Errorf("list interrupted runs: %w", err)
	}
	type interruptedRun struct{ id, conversationID string }
	runs := []interruptedRun{}
	for rows.Next() {
		var run interruptedRun
		if err := rows.Scan(&run.id, &run.conversationID); err != nil {
			rows.Close()
			return fmt.Errorf("read interrupted run: %w", err)
		}
		runs = append(runs, run)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close interrupted run list: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate interrupted runs: %w", err)
	}
	now := time.Now().UTC()
	if len(runs) > 0 {
		if _, err := tx.ExecContext(ctx, `update shortcut_runs set status='interrupted',completed_at=? where status in ('accepted','queued','running') and run_id in (select id from runs where status in ('queued','running'))`, now); err != nil {
			return fmt.Errorf("mark interrupted shortcut runs: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `update runs set status='interrupted',completed_at=? where status in ('queued','running')`, now); err != nil {
			return fmt.Errorf("mark interrupted runs: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `update conversations set status='idle' where status='running'`, now); err != nil {
		return fmt.Errorf("release interrupted conversations: %w", err)
	}
	for _, run := range runs {
		if _, err := tx.ExecContext(ctx, `insert into events (id,conversation_id,run_id,type,payload,created_at) values (?,?,?,?,?,?)`, uuid.NewString(), run.conversationID, run.id, "run.interrupted", mustJSON(map[string]string{"status": "interrupted", "error": "Control service restarted before the run completed."}), now); err != nil {
			return fmt.Errorf("record interrupted run: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit interrupted run recovery: %w", err)
	}
	return nil
}

func validPermissionMode(mode string) bool {
	return mode == "approval_required" || mode == "full_control"
}

func (s *Server) listDirectories(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		path = s.config.AllowedRoot
	}
	path, err := s.allowedPath(path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	type item struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	items := make([]item, 0)
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
			items = append(items, item{Name: entry.Name(), Path: filepath.Join(path, entry.Name())})
		}
	}
	sort.Slice(items, func(i, j int) bool { return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name) })
	writeJSON(w, http.StatusOK, map[string]any{"path": path, "parent": filepath.Dir(path), "directories": items})
}

func (s *Server) validateProject(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Path string `json:"path"`
	}
	if !decode(w, r, &input) {
		return
	}
	path, err := s.allowedPath(input.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		writeError(w, http.StatusBadRequest, errors.New("directory does not exist or is not accessible"))
		return
	}
	branch, gitReady := gitBranch(path)
	claudeReady := s.runner.Ready(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"path": path, "name": filepath.Base(path), "gitReady": gitReady, "gitBranch": branch, "claudeReady": claudeReady})
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if !decode(w, r, &input) {
		return
	}
	path, err := s.allowedPath(input.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	branch, gitReady := gitBranch(path)
	if !s.runner.Ready(r.Context()) {
		writeError(w, http.StatusBadRequest, errors.New("Claude Code is not available in this Runner"))
		return
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		name = filepath.Base(path)
	}
	if !gitReady {
		branch = "非 Git 目录"
	}
	p := Project{ID: uuid.NewString(), Name: name, Path: path, PathDisplay: filepath.Base(path), Runner: "wsl-local", GitBranch: branch, ClaudeReady: true, CreatedAt: time.Now().UTC()}
	_, err = s.db.ExecContext(r.Context(), `insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ($1,$2,$3,$4,$5,$6,$7)`, p.ID, p.Name, p.Path, p.Runner, p.GitBranch, p.ClaudeReady, p.CreatedAt)
	if err != nil {
		var existing Project
		lookupErr := s.db.QueryRowContext(r.Context(), `select id,name,path,runner,git_branch,claude_ready,created_at from projects where path=$1`, path).Scan(&existing.ID, &existing.Name, &existing.Path, &existing.Runner, &existing.GitBranch, &existing.ClaudeReady, &existing.CreatedAt)
		if lookupErr == nil {
			existing.PathDisplay = filepath.Base(existing.Path)
			writeJSON(w, http.StatusOK, existing)
			return
		}
		writeError(w, http.StatusConflict, fmt.Errorf("create project: %w", err))
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(), `select id,name,path,runner,git_branch,claude_ready,created_at from projects order by created_at desc`)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	defer rows.Close()
	projects := []Project{}
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name, &p.Path, &p.Runner, &p.GitBranch, &p.ClaudeReady, &p.CreatedAt); err != nil {
			writeError(w, 500, err)
			return
		}
		p.PathDisplay = filepath.Base(p.Path)
		projects = append(projects, p)
	}
	if err := rows.Err(); err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, 200, projects)
}

func (s *Server) createConversation(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	newSession := r.URL.Query().Get("new") == "true"
	var input struct {
		PermissionMode string `json:"permissionMode"`
	}
	if r.Body != http.NoBody {
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, errors.New("invalid JSON request"))
			return
		}
	}
	if input.PermissionMode == "" {
		input.PermissionMode = "approval_required"
	}
	if !validPermissionMode(input.PermissionMode) {
		writeError(w, http.StatusBadRequest, errors.New("unsupported permission mode"))
		return
	}
	now := time.Now().UTC()
	c := Conversation{ID: uuid.NewString(), ProjectID: projectID, ClaudeSessionID: uuid.NewString(), Status: "idle", PermissionMode: input.PermissionMode, Title: "新会话", LastActivityAt: now, IsCurrent: true, CreatedAt: now}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	defer tx.Rollback()
	if newSession {
		var status string
		err = tx.QueryRowContext(r.Context(), `select status from conversations where project_id=$1 and is_current=1`, projectID).Scan(&status)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			writeError(w, 500, err)
			return
		}
		if status == "running" {
			writeError(w, http.StatusConflict, errors.New("stop the active Claude run before starting a new conversation"))
			return
		}
		if _, err = tx.ExecContext(r.Context(), `update conversations set is_current=0 where project_id=$1 and is_current=1`, projectID); err != nil {
			writeError(w, 500, err)
			return
		}
	} else {
		var existing Conversation
		err = tx.QueryRowContext(r.Context(), `select id,project_id,claude_session_id,status,permission_mode,title,last_activity_at,claude_initialized,is_current,created_at from conversations where project_id=$1 and is_current=1`, projectID).Scan(&existing.ID, &existing.ProjectID, &existing.ClaudeSessionID, &existing.Status, &existing.PermissionMode, &existing.Title, &existing.LastActivityAt, &existing.ClaudeInitialized, &existing.IsCurrent, &existing.CreatedAt)
		if err == nil {
			if err = tx.Commit(); err != nil {
				writeError(w, 500, err)
				return
			}
			writeJSON(w, http.StatusOK, existing)
			return
		}
		if !errors.Is(err, sql.ErrNoRows) {
			writeError(w, 500, err)
			return
		}
	}
	if _, err = tx.ExecContext(r.Context(), `insert into conversations (id,project_id,claude_session_id,status,permission_mode,title,last_activity_at,claude_initialized,is_current,created_at) values (?,?,?,?,?,?,?,?,?,?)`, c.ID, c.ProjectID, c.ClaudeSessionID, c.Status, c.PermissionMode, c.Title, c.LastActivityAt, c.ClaudeInitialized, c.IsCurrent, c.CreatedAt); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err = tx.Commit(); err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}
func (s *Server) listConversations(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	rows, err := s.db.QueryContext(r.Context(), `select c.id,c.project_id,c.claude_session_id,c.status,c.permission_mode,c.title,c.last_activity_at,c.claude_initialized,c.is_current,c.created_at,coalesce((select content from messages m where m.conversation_id=c.id order by m.created_at desc limit 1),'') from conversations c where c.project_id=$1 and ($2='' or lower(c.title) like '%' || lower($2) || '%') order by c.is_current desc,c.last_activity_at desc limit 100`, chi.URLParam(r, "projectID"), query)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	defer rows.Close()
	items := []Conversation{}
	for rows.Next() {
		var c Conversation
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.ClaudeSessionID, &c.Status, &c.PermissionMode, &c.Title, &c.LastActivityAt, &c.ClaudeInitialized, &c.IsCurrent, &c.CreatedAt, &c.Preview); err != nil {
			writeError(w, 500, err)
			return
		}
		c.Preview = compactConversationText(c.Preview, 120)
		items = append(items, c)
	}
	if err := rows.Err(); err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, 200, items)
}

func (s *Server) getConversation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "conversationID")
	var c Conversation
	err := s.db.QueryRowContext(r.Context(), `select id,project_id,claude_session_id,status,permission_mode,title,last_activity_at,claude_initialized,is_current,created_at from conversations where id=$1`, id).Scan(&c.ID, &c.ProjectID, &c.ClaudeSessionID, &c.Status, &c.PermissionMode, &c.Title, &c.LastActivityAt, &c.ClaudeInitialized, &c.IsCurrent, &c.CreatedAt)
	if err != nil {
		writeError(w, 404, errors.New("conversation not found"))
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `select id,conversation_id,role,content,created_at from messages where conversation_id=$1 order by created_at`, id)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	defer rows.Close()
	messages := []Message{}
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.Role, &m.Content, &m.CreatedAt); err != nil {
			writeError(w, 500, err)
			return
		}
		messages = append(messages, m)
	}
	if err := rows.Err(); err != nil {
		writeError(w, 500, err)
		return
	}
	erows, err := s.db.QueryContext(r.Context(), `select id,conversation_id,run_id,type,payload,created_at from events where conversation_id=$1 order by created_at`, id)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	defer erows.Close()
	events := []Event{}
	for erows.Next() {
		var e Event
		if err := erows.Scan(&e.ID, &e.ConversationID, &e.RunID, &e.Type, &e.Payload, &e.CreatedAt); err != nil {
			writeError(w, 500, err)
			return
		}
		events = append(events, e)
	}
	if err := erows.Err(); err != nil {
		writeError(w, 500, err)
		return
	}
	var activeRunID *string
	if c.Status == "running" {
		var runID string
		if err := s.db.QueryRowContext(r.Context(), `select id from runs where conversation_id=$1 and status='running' order by created_at desc limit 1`, id).Scan(&runID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			writeError(w, 500, err)
			return
		} else if err == nil {
			activeRunID = &runID
		}
	}
	writeJSON(w, 200, map[string]any{"conversation": c, "activeRunId": activeRunID, "messages": messages, "events": events})
}

func (s *Server) listInputHistory(w http.ResponseWriter, r *http.Request) {
	conversationID := chi.URLParam(r, "conversationID")
	limit := 100
	if rawLimit := r.URL.Query().Get("limit"); rawLimit != "" {
		parsed, err := strconv.Atoi(rawLimit)
		if err != nil || parsed < 1 {
			writeError(w, http.StatusBadRequest, errors.New("limit must be a positive integer"))
			return
		}
		if parsed < limit {
			limit = parsed
		}
	}
	var exists bool
	if err := s.db.QueryRowContext(r.Context(), `select exists(select 1 from conversations where id=?)`, conversationID).Scan(&exists); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !exists {
		writeError(w, http.StatusNotFound, errors.New("conversation not found"))
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `select content from messages where conversation_id=? and role='user' order by created_at desc limit ?`, conversationID, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	history := make([]string, 0, limit)
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		history = append(history, content)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	for left, right := 0, len(history)-1; left < right; left, right = left+1, right-1 {
		history[left], history[right] = history[right], history[left]
	}
	writeJSON(w, http.StatusOK, history)
}

func (s *Server) activateConversation(w http.ResponseWriter, r *http.Request) {
	conversationID := chi.URLParam(r, "conversationID")
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	var conversation Conversation
	err = tx.QueryRowContext(r.Context(), `select id,project_id,claude_session_id,status,permission_mode,title,last_activity_at,claude_initialized,is_current,created_at from conversations where id=?`, conversationID).Scan(&conversation.ID, &conversation.ProjectID, &conversation.ClaudeSessionID, &conversation.Status, &conversation.PermissionMode, &conversation.Title, &conversation.LastActivityAt, &conversation.ClaudeInitialized, &conversation.IsCurrent, &conversation.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("conversation not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if conversation.Status == "running" && !conversation.IsCurrent {
		writeError(w, http.StatusConflict, errors.New("the selected conversation is already running"))
		return
	}
	var activeID, activeStatus string
	err = tx.QueryRowContext(r.Context(), `select id,status from conversations where project_id=? and is_current=1`, conversation.ProjectID).Scan(&activeID, &activeStatus)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err == nil && activeID != conversation.ID && activeStatus == "running" {
		writeError(w, http.StatusConflict, errors.New("stop the active Claude run before switching conversations"))
		return
	}
	if _, err = tx.ExecContext(r.Context(), `update conversations set is_current=0 where project_id=? and is_current=1`, conversation.ProjectID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if _, err = tx.ExecContext(r.Context(), `update conversations set is_current=1 where id=?`, conversation.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err = tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	conversation.IsCurrent = true
	writeJSON(w, http.StatusOK, conversation)
}

func (s *Server) setConversationPermissionMode(w http.ResponseWriter, r *http.Request) {
	var input struct {
		PermissionMode string `json:"permissionMode"`
	}
	if !decode(w, r, &input) {
		return
	}
	if !validPermissionMode(input.PermissionMode) {
		writeError(w, http.StatusBadRequest, errors.New("unsupported permission mode"))
		return
	}
	conversationID := chi.URLParam(r, "conversationID")
	var conversation Conversation
	err := s.db.QueryRowContext(r.Context(), `select id,project_id,claude_session_id,status,permission_mode,title,last_activity_at,claude_initialized,is_current,created_at from conversations where id=$1`, conversationID).Scan(&conversation.ID, &conversation.ProjectID, &conversation.ClaudeSessionID, &conversation.Status, &conversation.PermissionMode, &conversation.Title, &conversation.LastActivityAt, &conversation.ClaudeInitialized, &conversation.IsCurrent, &conversation.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("conversation not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if conversation.Status != "idle" {
		writeError(w, http.StatusConflict, errors.New("stop the active Claude run before changing permission mode"))
		return
	}
	result, err := s.db.ExecContext(r.Context(), `update conversations set permission_mode=? where id=? and status='idle'`, input.PermissionMode, conversationID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	updated, err := result.RowsAffected()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if updated != 1 {
		writeError(w, http.StatusConflict, errors.New("stop the active Claude run before changing permission mode"))
		return
	}
	conversation.PermissionMode = input.PermissionMode
	writeJSON(w, http.StatusOK, conversation)
}

type shortcutInput struct {
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	Kind          string   `json:"kind"`
	Template      string   `json:"template"`
	Scope         string   `json:"scope"`
	DefaultAction string   `json:"defaultAction"`
	GroupName     string   `json:"groupName"`
	Pinned        bool     `json:"pinned"`
	Enabled       *bool    `json:"enabled"`
	SortOrder     int      `json:"sortOrder"`
	ProjectIDs    []string `json:"projectIds"`
}

func validShortcut(shortcut shortcutInput) error {
	shortcut.Name = strings.TrimSpace(shortcut.Name)
	shortcut.Template = strings.TrimSpace(shortcut.Template)
	if shortcut.Name == "" || len([]rune(shortcut.Name)) > 64 {
		return errors.New("shortcut name must be between 1 and 64 characters")
	}
	if shortcut.Template == "" || len([]rune(shortcut.Template)) > 12000 {
		return errors.New("shortcut template must be between 1 and 12000 characters")
	}
	if shortcut.Kind != "prompt" && shortcut.Kind != "snippet" && shortcut.Kind != "command_request" {
		return errors.New("unsupported shortcut kind")
	}
	if shortcut.Scope != "local" && shortcut.Scope != "project" {
		return errors.New("unsupported shortcut scope")
	}
	if shortcut.DefaultAction != "fill" && shortcut.DefaultAction != "confirm" && shortcut.DefaultAction != "run" {
		return errors.New("unsupported shortcut action")
	}
	if shortcut.Kind == "snippet" && shortcut.DefaultAction != "fill" {
		return errors.New("snippets can only fill the composer")
	}
	if shortcut.Kind == "command_request" && shortcut.DefaultAction != "confirm" {
		return errors.New("command shortcuts require confirmation")
	}
	if shortcut.Scope == "project" && len(shortcut.ProjectIDs) == 0 {
		return errors.New("project shortcuts require at least one project")
	}
	return nil
}

func normalizedProjectIDs(ids []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		result = append(result, id)
	}
	return result
}

func scanShortcut(row interface{ Scan(...any) error }) (Shortcut, error) {
	var shortcut Shortcut
	var pinned, enabled int
	err := row.Scan(&shortcut.ID, &shortcut.Name, &shortcut.Description, &shortcut.Kind, &shortcut.Template, &shortcut.Scope, &shortcut.DefaultAction, &shortcut.GroupName, &pinned, &enabled, &shortcut.SortOrder, &shortcut.CreatedAt, &shortcut.UpdatedAt)
	shortcut.Pinned = pinned != 0
	shortcut.Enabled = enabled != 0
	return shortcut, err
}

func (s *Server) shortcutProjectIDs(ctx context.Context, shortcutID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `select project_id from shortcut_projects where shortcut_id=? order by project_id`, shortcutID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	projectIDs := []string{}
	for rows.Next() {
		var projectID string
		if err := rows.Scan(&projectID); err != nil {
			return nil, err
		}
		projectIDs = append(projectIDs, projectID)
	}
	return projectIDs, rows.Err()
}

func (s *Server) listShortcuts(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	includeDisabled := r.URL.Query().Get("includeDisabled") == "true"
	query := `select distinct s.id,s.name,s.description,s.kind,s.template,s.scope,s.default_action,s.group_name,s.pinned,s.enabled,s.sort_order,s.created_at,s.updated_at from shortcuts s left join shortcut_projects sp on sp.shortcut_id=s.id where (s.scope='local' or sp.project_id=?)`
	if !includeDisabled {
		query += ` and s.enabled=1`
	}
	query += ` order by s.enabled desc,s.pinned desc,s.sort_order,s.name`
	rows, err := s.db.QueryContext(r.Context(), query, projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	shortcuts := []Shortcut{}
	for rows.Next() {
		shortcut, err := scanShortcut(rows)
		if err != nil {
			rows.Close()
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		shortcuts = append(shortcuts, shortcut)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := rows.Close(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	for index := range shortcuts {
		projectIDs, err := s.shortcutProjectIDs(r.Context(), shortcuts[index].ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		shortcuts[index].ProjectIDs = projectIDs
	}
	writeJSON(w, http.StatusOK, shortcuts)
}

func (s *Server) seedDefaultShortcuts(w http.ResponseWriter, r *http.Request) {
	const seedKey = "common_shortcuts_v1"
	defaults := []struct {
		name, kind, template, group, action string
	}{
		{"了解项目", "prompt", "请快速了解当前项目的目录结构、技术栈和关键入口，简洁总结。", "常用提示词", "run"},
		{"检查状态", "prompt", "请检查当前项目的 Git 状态、未完成工作和明显风险，简洁总结。", "常用提示词", "run"},
		{"审查改动", "prompt", "请审查当前项目尚未提交的改动，指出风险、测试缺口和建议。", "常用提示词", "run"},
		{"清屏", "command_request", "clear", "常用命令", "confirm"},
		{"Git 状态", "command_request", "git status", "常用命令", "confirm"},
		{"当前目录", "command_request", "pwd", "常用命令", "confirm"},
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	var value string
	err = tx.QueryRowContext(r.Context(), `select value from app_metadata where key=?`, seedKey).Scan(&value)
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"seeded": false, "created": 0})
		return
	}
	if !errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	now := time.Now().UTC()
	created := 0
	for _, shortcut := range defaults {
		var exists bool
		if err := tx.QueryRowContext(r.Context(), `select exists(select 1 from shortcuts where scope='local' and kind=? and name=?)`, shortcut.kind, shortcut.name).Scan(&exists); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if exists {
			continue
		}
		id := uuid.NewString()
		if _, err := tx.ExecContext(r.Context(), `insert into shortcuts (id,name,description,kind,template,scope,default_action,group_name,pinned,enabled,sort_order,created_at,updated_at) values (?,?,?,?,?,'local',?,?,1,1,0,?,?)`, id, shortcut.name, "", shortcut.kind, shortcut.template, shortcut.action, shortcut.group, now, now); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if _, err := tx.ExecContext(r.Context(), `insert into shortcut_audit_events (id,shortcut_id,action,payload,created_at) values (?,?,?,?,?)`, uuid.NewString(), id, "created", mustJSON(map[string]any{"name": shortcut.name, "source": "default"}), now); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		created++
	}
	if _, err := tx.ExecContext(r.Context(), `insert into app_metadata (key,value) values (?,?)`, seedKey, now.Format(time.RFC3339Nano)); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"seeded": true, "created": created})
}

func (s *Server) saveShortcut(w http.ResponseWriter, r *http.Request, id string, created bool) {
	var input shortcutInput
	if !decode(w, r, &input) {
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	input.Description = strings.TrimSpace(input.Description)
	input.Template = strings.TrimSpace(input.Template)
	input.GroupName = strings.TrimSpace(input.GroupName)
	input.ProjectIDs = normalizedProjectIDs(input.ProjectIDs)
	if err := validShortcut(input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	enabled := true
	createdAt := now
	if !created {
		var existingEnabled bool
		if err = tx.QueryRowContext(r.Context(), `select enabled,created_at from shortcuts where id=?`, id).Scan(&existingEnabled, &createdAt); errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("shortcut not found"))
			return
		} else if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		enabled = existingEnabled
	}
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	if created {
		_, err = tx.ExecContext(r.Context(), `insert into shortcuts (id,name,description,kind,template,scope,default_action,group_name,pinned,enabled,sort_order,created_at,updated_at) values (?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, input.Name, input.Description, input.Kind, input.Template, input.Scope, input.DefaultAction, input.GroupName, input.Pinned, enabled, input.SortOrder, now, now)
	} else {
		result, updateErr := tx.ExecContext(r.Context(), `update shortcuts set name=?,description=?,kind=?,template=?,scope=?,default_action=?,group_name=?,pinned=?,enabled=?,sort_order=?,updated_at=? where id=?`, input.Name, input.Description, input.Kind, input.Template, input.Scope, input.DefaultAction, input.GroupName, input.Pinned, enabled, input.SortOrder, now, id)
		err = updateErr
		if err == nil {
			changed, changedErr := result.RowsAffected()
			if changedErr != nil {
				err = changedErr
			} else if changed != 1 {
				err = sql.ErrNoRows
			}
		}
	}
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("shortcut not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if _, err = tx.ExecContext(r.Context(), `delete from shortcut_projects where shortcut_id=?`, id); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if input.Scope == "project" {
		for _, projectID := range input.ProjectIDs {
			if _, err = tx.ExecContext(r.Context(), `insert into shortcut_projects (shortcut_id,project_id) values (?,?)`, id, projectID); err != nil {
				writeError(w, http.StatusBadRequest, errors.New("project is unavailable"))
				return
			}
		}
	}
	action := "updated"
	if created {
		action = "created"
	}
	if _, err = tx.ExecContext(r.Context(), `insert into shortcut_audit_events (id,shortcut_id,action,payload,created_at) values (?,?,?,?,?)`, uuid.NewString(), id, action, mustJSON(map[string]any{"name": input.Name}), now); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err = tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	shortcut := Shortcut{ID: id, Name: input.Name, Description: input.Description, Kind: input.Kind, Template: input.Template, Scope: input.Scope, DefaultAction: input.DefaultAction, GroupName: input.GroupName, Pinned: input.Pinned, Enabled: enabled, SortOrder: input.SortOrder, ProjectIDs: input.ProjectIDs, CreatedAt: createdAt, UpdatedAt: now}
	writeJSON(w, map[bool]int{true: http.StatusCreated, false: http.StatusOK}[created], shortcut)
}

func (s *Server) createShortcut(w http.ResponseWriter, r *http.Request) {
	s.saveShortcut(w, r, uuid.NewString(), true)
}
func (s *Server) updateShortcut(w http.ResponseWriter, r *http.Request) {
	s.saveShortcut(w, r, chi.URLParam(r, "shortcutID"), false)
}

func (s *Server) deleteShortcut(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "shortcutID")
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(r.Context(), `delete from shortcuts where id=?`, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	changed, err := result.RowsAffected()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if changed != 1 {
		writeError(w, http.StatusNotFound, errors.New("shortcut not found"))
		return
	}
	if _, err = tx.ExecContext(r.Context(), `insert into shortcut_audit_events (id,shortcut_id,action,payload,created_at) values (?,?,?,?,?)`, uuid.NewString(), nil, "deleted", mustJSON(map[string]string{"shortcutId": id}), time.Now().UTC()); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err = tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) shortcutForConversation(ctx context.Context, shortcutID, conversationID string) (Shortcut, Conversation, Project, error) {
	var conversation Conversation
	var project Project
	err := s.db.QueryRowContext(ctx, `select c.id,c.project_id,c.claude_session_id,c.status,c.permission_mode,c.title,c.last_activity_at,c.claude_initialized,c.is_current,c.created_at,p.id,p.name,p.path,p.runner,p.git_branch,p.claude_ready,p.created_at from conversations c join projects p on p.id=c.project_id where c.id=?`, conversationID).Scan(&conversation.ID, &conversation.ProjectID, &conversation.ClaudeSessionID, &conversation.Status, &conversation.PermissionMode, &conversation.Title, &conversation.LastActivityAt, &conversation.ClaudeInitialized, &conversation.IsCurrent, &conversation.CreatedAt, &project.ID, &project.Name, &project.Path, &project.Runner, &project.GitBranch, &project.ClaudeReady, &project.CreatedAt)
	if err != nil {
		return Shortcut{}, Conversation{}, Project{}, err
	}
	row := s.db.QueryRowContext(ctx, `select s.id,s.name,s.description,s.kind,s.template,s.scope,s.default_action,s.group_name,s.pinned,s.enabled,s.sort_order,s.created_at,s.updated_at from shortcuts s where s.id=? and s.enabled=1 and (s.scope='local' or exists(select 1 from shortcut_projects sp where sp.shortcut_id=s.id and sp.project_id=?))`, shortcutID, project.ID)
	shortcut, err := scanShortcut(row)
	if err == nil {
		shortcut.ProjectIDs, err = s.shortcutProjectIDs(ctx, shortcut.ID)
	}
	return shortcut, conversation, project, err
}

func renderShortcut(shortcut Shortcut, conversation Conversation, project Project, variables map[string]string) (string, error) {
	values := map[string]string{"project.name": project.Name, "project.path": project.Path, "project.branch": project.GitBranch, "conversation.title": conversation.Title, "selection": strings.TrimSpace(variables["selection"]), "error": strings.TrimSpace(variables["error"])}
	for _, required := range []string{"selection", "error"} {
		if strings.Contains(shortcut.Template, "${"+required+"}") && values[required] == "" {
			return "", fmt.Errorf("%s is required", required)
		}
	}
	replacer := make([]string, 0, len(values)*2)
	for key, value := range values {
		replacer = append(replacer, "${"+key+"}", value)
	}
	rendered := strings.TrimSpace(strings.NewReplacer(replacer...).Replace(shortcut.Template))
	if strings.Contains(rendered, "${") {
		return "", errors.New("shortcut contains an unsupported variable")
	}
	if rendered == "" || len([]rune(rendered)) > 12000 {
		return "", errors.New("rendered shortcut is invalid")
	}
	if shortcut.Kind == "command_request" {
		rendered = "请在当前项目中执行以下命令：\n`" + rendered + "`\n\n遵循当前权限模式；完成后总结结果。"
	}
	return rendered, nil
}

func (s *Server) previewShortcut(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Variables map[string]string `json:"variables"`
	}
	if !decode(w, r, &input) {
		return
	}
	shortcut, conversation, project, err := s.shortcutForConversation(r.Context(), chi.URLParam(r, "shortcutID"), chi.URLParam(r, "conversationID"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("shortcut or conversation not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	rendered, err := renderShortcut(shortcut, conversation, project, input.Variables)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"shortcut": shortcut, "content": rendered, "requiresConfirmation": shortcut.DefaultAction == "confirm"})
}

func (s *Server) listShortcutRuns(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(), `select id,coalesce(shortcut_id,''),conversation_id,coalesce(run_id,''),rendered_content,action,status,created_at,completed_at from shortcut_runs where conversation_id=? order by created_at desc limit 20`, chi.URLParam(r, "conversationID"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	items := []ShortcutRun{}
	for rows.Next() {
		var item ShortcutRun
		if err := rows.Scan(&item.ID, &item.ShortcutID, &item.ConversationID, &item.RunID, &item.RenderedContent, &item.Action, &item.Status, &item.CreatedAt, &item.CompletedAt); err != nil {
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

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Content string `json:"content"`
	}
	if !decode(w, r, &input) {
		return
	}
	m, runID, _, status, err := s.startMessage(r.Context(), chi.URLParam(r, "conversationID"), input.Content, nil)
	if err != nil {
		writeError(w, status, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"message": m, "runId": runID})
}

func (s *Server) runShortcut(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Variables map[string]string `json:"variables"`
		Action    string            `json:"action"`
	}
	if !decode(w, r, &input) {
		return
	}
	conversationID, shortcutID := chi.URLParam(r, "conversationID"), chi.URLParam(r, "shortcutID")
	shortcut, conversation, project, err := s.shortcutForConversation(r.Context(), shortcutID, conversationID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("shortcut or conversation not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if shortcut.Kind == "snippet" {
		writeError(w, http.StatusBadRequest, errors.New("snippets cannot run"))
		return
	}
	if input.Action != "confirm" && input.Action != "run" {
		writeError(w, http.StatusBadRequest, errors.New("shortcut action must run or confirm"))
		return
	}
	if shortcut.DefaultAction == "fill" {
		writeError(w, http.StatusBadRequest, errors.New("this shortcut only fills the composer"))
		return
	}
	if shortcut.DefaultAction == "confirm" && input.Action != "confirm" {
		writeError(w, http.StatusBadRequest, errors.New("shortcut requires confirmation"))
		return
	}
	if shortcut.DefaultAction == "run" && input.Action != "run" {
		writeError(w, http.StatusBadRequest, errors.New("shortcut is configured for immediate run"))
		return
	}
	content, err := renderShortcut(shortcut, conversation, project, input.Variables)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	shortcutRun := &ShortcutRun{ID: uuid.NewString(), ShortcutID: shortcut.ID, ConversationID: conversationID, RenderedContent: content, Action: input.Action, Status: "accepted", CreatedAt: time.Now().UTC()}
	m, runID, execution, status, err := s.startMessage(r.Context(), conversationID, content, shortcutRun)
	if err != nil {
		writeError(w, status, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"message": m, "runId": runID, "shortcutRun": execution})
}

func (s *Server) startMessage(ctx context.Context, conversationID, content string, shortcutRun *ShortcutRun) (Message, string, *ShortcutRun, int, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return Message{}, "", nil, http.StatusBadRequest, errors.New("message is required")
	}
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	if closing {
		return Message{}, "", nil, http.StatusServiceUnavailable, errors.New("control service is shutting down")
	}
	m := Message{ID: uuid.NewString(), ConversationID: conversationID, Role: "user", Content: content, CreatedAt: time.Now().UTC()}
	runID := uuid.NewString()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Message{}, "", nil, http.StatusInternalServerError, err
	}
	defer tx.Rollback()
	var conversation Conversation
	var projectPath string
	err = tx.QueryRowContext(ctx, `select c.id,c.project_id,c.claude_session_id,c.status,c.permission_mode,c.title,c.last_activity_at,c.claude_initialized,c.is_current,c.created_at,p.path from conversations c join projects p on p.id=c.project_id where c.id=$1`, conversationID).Scan(&conversation.ID, &conversation.ProjectID, &conversation.ClaudeSessionID, &conversation.Status, &conversation.PermissionMode, &conversation.Title, &conversation.LastActivityAt, &conversation.ClaudeInitialized, &conversation.IsCurrent, &conversation.CreatedAt, &projectPath)
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, "", nil, http.StatusNotFound, errors.New("conversation not found")
	}
	if err != nil {
		return Message{}, "", nil, http.StatusInternalServerError, err
	}
	streamingRunner, streaming := s.runner.(StreamingAgentRunner)
	if conversation.Status != "idle" && !streaming {
		return Message{}, "", nil, http.StatusConflict, errors.New("conversation already has a running Claude turn")
	}
	if conversation.Title == "" || conversation.Title == "新会话" {
		conversation.Title = conversationTitle(content)
	}
	result, updateErr := tx.ExecContext(ctx, `update conversations set status='running',title=?,last_activity_at=? where id=? and (status='idle' or ?=1)`, conversation.Title, m.CreatedAt, conversationID, boolToInt(streaming))
	err = updateErr
	if err == nil {
		var changed int64
		changed, err = result.RowsAffected()
		if err == nil && changed != 1 {
			return Message{}, "", nil, http.StatusConflict, errors.New("conversation already has a running Claude turn")
		}
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, `insert into messages (id,conversation_id,role,content,created_at) values ($1,$2,$3,$4,$5)`, m.ID, m.ConversationID, m.Role, m.Content, m.CreatedAt)
	}
	if err == nil {
		status := "running"
		if streaming {
			status = "queued"
		}
		_, err = tx.ExecContext(ctx, `insert into runs (id,conversation_id,status,created_at) values ($1,$2,$3,$4)`, runID, conversationID, status, m.CreatedAt)
	}
	if err == nil && shortcutRun != nil {
		shortcutRun.RunID = runID
		_, err = tx.ExecContext(ctx, `insert into shortcut_runs (id,shortcut_id,conversation_id,run_id,rendered_content,action,status,created_at) values (?,?,?,?,?,?,?,?)`, shortcutRun.ID, shortcutRun.ShortcutID, shortcutRun.ConversationID, shortcutRun.RunID, shortcutRun.RenderedContent, shortcutRun.Action, shortcutRun.Status, shortcutRun.CreatedAt)
	}
	if err != nil {
		return Message{}, "", nil, http.StatusInternalServerError, err
	}
	if err = tx.Commit(); err != nil {
		return Message{}, "", nil, http.StatusInternalServerError, err
	}
	if streaming {
		s.submitStreamingRun(streamingRunner, runID, conversation, projectPath, content)
		return m, runID, shortcutRun, http.StatusAccepted, nil
	}
	runCtx, cancel := context.WithCancel(s.runtimeCtx)
	runToken := uuid.NewString()
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		cancel()
		s.finishRun(runID, conversation.ID, "stopped", errors.New("control service is shutting down"))
		return Message{}, "", nil, http.StatusServiceUnavailable, errors.New("control service is shutting down")
	}
	s.runWG.Add(1)
	s.cancels[runID] = cancel
	s.runTokens[runID] = runToken
	s.runContexts[runID] = conversation.ID
	s.mu.Unlock()
	s.beginRunUsage(runID, conversation.ID)
	go func() { defer s.runWG.Done(); s.runClaude(runCtx, runID, runToken, conversation, projectPath, content) }()
	return m, runID, shortcutRun, http.StatusAccepted, nil
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (s *Server) submitStreamingRun(runner StreamingAgentRunner, runID string, conversation Conversation, projectPath, prompt string) {
	// A CLI stdin is an ordered transport. Keep session lookup and the write in
	// one critical section so rapid submissions cannot be reordered.
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	session, err := s.streamingSession(runner, conversation, projectPath)
	if err != nil {
		s.finishStreamingRun(runID, conversation.ID, err)
		return
	}
	request := AgentRunRequest{SessionID: conversation.ClaudeSessionID, ProjectPath: projectPath, Prompt: prompt, PermissionMode: conversation.PermissionMode, Resume: conversation.ClaudeInitialized, RunID: runID}
	if err := session.Send(request, &claudeRunSink{server: s, runID: runID, conversationID: conversation.ID, streaming: true}); err != nil {
		s.finishStreamingRun(runID, conversation.ID, err)
	}
}

func (s *Server) streamingSession(runner StreamingAgentRunner, conversation Conversation, projectPath string) (AgentSession, error) {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	s.mu.Lock()
	if session := s.sessions[conversation.ID]; session != nil {
		s.mu.Unlock()
		return session.agent, nil
	}
	if s.closing {
		s.mu.Unlock()
		return nil, errors.New("control service is shutting down")
	}
	s.mu.Unlock()
	token := uuid.NewString()
	agent, err := runner.StartSession(s.runtimeCtx, AgentSessionRequest{SessionID: conversation.ClaudeSessionID, ProjectPath: projectPath, PermissionMode: conversation.PermissionMode, Resume: conversation.ClaudeInitialized, ConversationID: conversation.ID, ApprovalToken: token})
	if err != nil {
		return nil, err
	}
	managed := &activeAgentSession{agent: agent, approvalToken: token}
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		agent.Stop()
		return nil, errors.New("control service is shutting down")
	}
	s.sessions[conversation.ID] = managed
	s.runWG.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.runWG.Done()
		<-agent.Done()
		s.mu.Lock()
		if s.sessions[conversation.ID] == managed {
			delete(s.sessions, conversation.ID)
		}
		s.mu.Unlock()
	}()
	return agent, nil
}

func (s *Server) runClaude(ctx context.Context, runID, runToken string, c Conversation, projectPath, prompt string) {
	defer func() {
		s.discardRunUsage(runID)
		s.resolveRunApprovals(runID, "deny")
		s.mu.Lock()
		delete(s.cancels, runID)
		delete(s.runTokens, runID)
		delete(s.runContexts, runID)
		s.mu.Unlock()
	}()
	err := s.runner.Run(ctx, AgentRunRequest{
		SessionID:      c.ClaudeSessionID,
		ProjectPath:    projectPath,
		Prompt:         prompt,
		PermissionMode: c.PermissionMode,
		Resume:         c.ClaudeInitialized,
		RunID:          runID,
		RunToken:       runToken,
	}, &claudeRunSink{server: s, runID: runID, conversationID: c.ID})
	status := "completed"
	if ctx.Err() != nil {
		status = "stopped"
	} else if err != nil {
		status = "failed"
	}
	s.recordUsagePersistenceError(runID, c.ID, s.persistRunUsage(runID, status))
	s.finishRun(runID, c.ID, status, err)
}

type claudeRunSink struct {
	server         *Server
	runID          string
	conversationID string
	streaming      bool
}

func (sink *claudeRunSink) Event(eventType string, payload json.RawMessage) {
	sink.server.appendEvent(sink.runID, sink.conversationID, eventType, payload)
	sink.server.collectUsageEvent(sink.runID, sink.conversationID, eventType, payload)
}

func (sink *claudeRunSink) AssistantText(content string) {
	m := Message{ID: uuid.NewString(), ConversationID: sink.conversationID, Role: "assistant", Content: content, CreatedAt: time.Now().UTC()}
	_, _ = sink.server.db.ExecContext(context.Background(), `insert into messages (id,conversation_id,role,content,created_at) values ($1,$2,$3,$4,$5)`, m.ID, m.ConversationID, m.Role, m.Content, m.CreatedAt)
	_, _ = sink.server.db.ExecContext(context.Background(), `update conversations set last_activity_at=? where id=?`, m.CreatedAt, sink.conversationID)
}

func (sink *claudeRunSink) SessionInitialized() {
	_, _ = sink.server.db.ExecContext(context.Background(), `update conversations set claude_initialized=true where id=?`, sink.conversationID)
}

func (sink *claudeRunSink) TurnStarted() {
	if !sink.streaming {
		return
	}
	sink.server.beginRunUsage(sink.runID, sink.conversationID)
	_, _ = sink.server.db.ExecContext(context.Background(), `update runs set status='running' where id=? and status='queued'`, sink.runID)
	sink.server.mu.Lock()
	sink.server.runContexts[sink.runID] = sink.conversationID
	if session := sink.server.sessions[sink.conversationID]; session != nil {
		session.activeRunID = sink.runID
	}
	sink.server.mu.Unlock()
}

func (sink *claudeRunSink) TurnFinished(err error) {
	if !sink.streaming {
		return
	}
	sink.server.finishStreamingRun(sink.runID, sink.conversationID, err)
}

func (s *Server) finishStreamingRun(runID, conversationID string, runErr error) {
	s.mu.Lock()
	stopped := false
	if session := s.sessions[conversationID]; session != nil {
		stopped = session.stopping
		if session.activeRunID == runID {
			session.activeRunID = ""
		}
	}
	delete(s.runContexts, runID)
	s.mu.Unlock()
	status := "completed"
	if stopped {
		status = "stopped"
	} else if runErr != nil {
		status = "failed"
	}
	s.recordUsagePersistenceError(runID, conversationID, s.persistRunUsage(runID, status))
	s.discardRunUsage(runID)
	s.resolveRunApprovals(runID, "deny")
	s.finishRun(runID, conversationID, status, runErr)
}
func (s *Server) finishRun(runID, conversationID, status string, runErr error) {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err == nil {
		_, err = tx.ExecContext(context.Background(), `update runs set status=?,completed_at=? where id=?`, status, time.Now().UTC(), runID)
		if err == nil {
			_, err = tx.ExecContext(context.Background(), `update conversations set status=case when exists(select 1 from runs where conversation_id=? and id<>? and status in ('queued','running')) then 'running' else 'idle' end,last_activity_at=? where id=?`, conversationID, runID, time.Now().UTC(), conversationID)
		}
		if err == nil {
			_, err = tx.ExecContext(context.Background(), `update shortcut_runs set status=?,completed_at=? where run_id=?`, status, time.Now().UTC(), runID)
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			tx.Rollback()
		}
	}
	payload, _ := json.Marshal(map[string]string{"status": status, "error": errorText(runErr)})
	s.appendEvent(runID, conversationID, "run."+status, payload)
}
func (s *Server) stopRun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "runID")
	s.mu.Lock()
	cancel, ok := s.cancels[id]
	conversationID := s.runContexts[id]
	session := s.sessions[conversationID]
	if session != nil {
		session.stopping = true
	}
	s.mu.Unlock()
	if session != nil {
		session.agent.Stop()
		s.resolveRunApprovals(id, "deny")
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "stopping"})
		return
	}
	if !ok {
		var status, queuedConversationID string
		err := s.db.QueryRowContext(r.Context(), `select status,conversation_id from runs where id=?`, id).Scan(&status, &queuedConversationID)
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("run not found"))
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		s.mu.Lock()
		session = s.sessions[queuedConversationID]
		if session != nil {
			session.stopping = true
		}
		s.mu.Unlock()
		if session != nil {
			session.agent.Stop()
			s.resolveRunApprovals(id, "deny")
			writeJSON(w, http.StatusAccepted, map[string]string{"status": "stopping"})
			return
		}
		if status != "running" {
			writeJSON(w, http.StatusOK, map[string]string{"status": status})
			return
		}
		writeError(w, http.StatusConflict, errors.New("run is not controlled by this service instance"))
		return
	}
	cancel()
	s.resolveRunApprovals(id, "deny")
	writeJSON(w, 202, map[string]string{"status": "stopping"})
}

func (s *Server) waitForApproval(w http.ResponseWriter, r *http.Request) {
	runID := r.Header.Get("X-Auto-Run-ID")
	conversationID := r.Header.Get("X-Auto-Conversation-ID")
	token := r.Header.Get("X-Auto-Approval-Token")
	if token == "" || (runID == "" && conversationID == "") {
		writeError(w, http.StatusUnauthorized, errors.New("approval credentials are required"))
		return
	}
	var input struct {
		ToolName  string          `json:"tool_name"`
		ToolInput json.RawMessage `json:"tool_input"`
	}
	if !decode(w, r, &input) {
		return
	}
	if input.ToolName != "Bash" || len(input.ToolInput) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("only Bash tool approvals are supported"))
		return
	}
	approvalID := uuid.NewString()
	waiter := &approvalWaiter{runID: runID, decision: make(chan string, 1)}
	s.mu.Lock()
	if conversationID != "" {
		session := s.sessions[conversationID]
		if session == nil || session.approvalToken != token || session.activeRunID == "" {
			s.mu.Unlock()
			writeError(w, http.StatusUnauthorized, errors.New("approval session is not active"))
			return
		}
		runID = session.activeRunID
		waiter.runID = runID
	} else {
		if s.runTokens[runID] != token {
			s.mu.Unlock()
			writeError(w, http.StatusUnauthorized, errors.New("approval run is not active"))
			return
		}
		var active bool
		conversationID, active = s.runContexts[runID]
		if !active {
			s.mu.Unlock()
			writeError(w, http.StatusNotFound, errors.New("run not found"))
			return
		}
	}
	for _, active := range s.approvals {
		if active.runID == runID {
			s.mu.Unlock()
			writeError(w, http.StatusConflict, errors.New("another command is already awaiting approval"))
			return
		}
	}
	waiter.conversationID = conversationID
	s.approvals[approvalID] = waiter
	s.mu.Unlock()

	pending := map[string]any{"approvalId": approvalID, "status": "pending", "toolName": input.ToolName, "toolInput": json.RawMessage(input.ToolInput)}
	s.appendEvent(runID, conversationID, "approval.pending", mustJSON(pending))
	decision := "deny"
	select {
	case decision = <-waiter.decision:
	case <-r.Context().Done():
	case <-time.After(5 * time.Minute):
	}
	s.mu.Lock()
	delete(s.approvals, approvalID)
	s.mu.Unlock()
	result := map[string]any{"approvalId": approvalID, "status": decision, "toolName": input.ToolName, "toolInput": json.RawMessage(input.ToolInput)}
	s.appendEvent(runID, conversationID, "approval."+decision, mustJSON(result))
	writeJSON(w, http.StatusOK, map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":            "PreToolUse",
			"permissionDecision":       decision,
			"permissionDecisionReason": "Command " + decision + " by the project operator.",
		},
	})
}

func (s *Server) resolveApproval(w http.ResponseWriter, r *http.Request) {
	approvalID := chi.URLParam(r, "approvalID")
	var input struct {
		Decision string `json:"decision"`
	}
	if !decode(w, r, &input) {
		return
	}
	if input.Decision != "allow" && input.Decision != "deny" {
		writeError(w, http.StatusBadRequest, errors.New("decision must be allow or deny"))
		return
	}
	s.mu.Lock()
	waiter, ok := s.approvals[approvalID]
	if ok {
		delete(s.approvals, approvalID)
	}
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("pending approval not found"))
		return
	}
	waiter.decision <- input.Decision
	writeJSON(w, http.StatusAccepted, map[string]string{"status": input.Decision})
}

func (s *Server) resolveRunApprovals(runID, decision string) {
	s.mu.Lock()
	waiters := make([]*approvalWaiter, 0)
	for approvalID, waiter := range s.approvals {
		if waiter.runID == runID {
			delete(s.approvals, approvalID)
			waiters = append(waiters, waiter)
		}
	}
	s.mu.Unlock()
	for _, waiter := range waiters {
		select {
		case waiter.decision <- decision:
		default:
		}
	}
}

func (s *Server) appendEvent(runID, conversationID, typ string, payload []byte) {
	e := Event{ID: uuid.NewString(), ConversationID: conversationID, RunID: runID, Type: typ, Payload: payload, CreatedAt: time.Now().UTC()}
	s.db.ExecContext(context.Background(), `insert into events (id,conversation_id,run_id,type,payload,created_at) values ($1,$2,$3,$4,$5,$6)`, e.ID, e.ConversationID, e.RunID, e.Type, e.Payload, e.CreatedAt)
	data, _ := json.Marshal(e)
	s.mu.Lock()
	connections := make([]*subscriber, 0, len(s.subscribers[conversationID]))
	for _, connection := range s.subscribers[conversationID] {
		connections = append(connections, connection)
	}
	s.mu.Unlock()
	for _, connection := range connections {
		connection.writeMu.Lock()
		err := connection.conn.WriteMessage(websocket.TextMessage, data)
		connection.writeMu.Unlock()
		if err != nil {
			s.mu.Lock()
			delete(s.subscribers[conversationID], connection.conn)
			s.mu.Unlock()
			connection.conn.Close()
		}
	}
}
func (s *Server) subscribe(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "conversationID")
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	s.mu.Lock()
	if s.subscribers[id] == nil {
		s.subscribers[id] = map[*websocket.Conn]*subscriber{}
	}
	s.subscribers[id][conn] = &subscriber{conn: conn}
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.subscribers[id], conn); s.mu.Unlock(); conn.Close() }()
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}
func (s *Server) allowedPath(path string) (string, error) {
	absolute, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	root, err := filepath.EvalSymlinks(s.config.AllowedRoot)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(root, absolute)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return "", errors.New("path is outside the Runner allowed root")
	}
	return absolute, nil
}
func gitBranch(path string) (string, bool) {
	cmd := exec.Command("git", "-C", path, "rev-parse", "--abbrev-ref", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}
func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	if err := json.NewDecoder(r.Body).Decode(target); err != nil {
		writeError(w, 400, errors.New("invalid JSON request"))
		return false
	}
	return true
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func conversationTitle(content string) string { return compactConversationText(content, 80) }

func compactConversationText(content string, limit int) string {
	content = strings.Join(strings.Fields(content), " ")
	runes := []rune(content)
	if len(runes) <= limit {
		return content
	}
	return string(runes[:limit]) + "..."
}

func mustJSON(value any) []byte { data, _ := json.Marshal(value); return data }

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\\"'\\\"'") + "'"
}
