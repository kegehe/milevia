package app

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	pathpkg "path"
	"path/filepath"
	"runtime"
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
	DatabasePath                 string
	DataDir                      string
	Mode                         string
	WebRoot                      string
	SessionToken                 string
	AllowedOrigins               []string
	AllowedRoot                  string
	ClaudePath                   string
	CodexPath                    string
	PermissionMode               string
	ControlURL                   string
	ApprovalHook                 string
	NativeApprovalHook           bool
	AgentUpdateTimeout           time.Duration
	ClaudeTurnIdleTimeout        time.Duration
	ClaudeInitialResponseTimeout time.Duration
	ClaudeToolResultTimeout      time.Duration
}

const (
	defaultClaudeTurnIdleTimeout        = 30 * time.Minute
	defaultClaudeInitialResponseTimeout = 5 * time.Minute
	defaultClaudeToolResultTimeout      = 5 * time.Minute
	defaultAgentUpdateTimeout           = 15 * time.Minute
	maxJSONBodyBytes                    = 16 * 1024 * 1024
	httpReadHeaderTimeout               = 5 * time.Second
	httpReadTimeout                     = 30 * time.Second
	httpIdleTimeout                     = 60 * time.Second
	maxHTTPHeaderBytes                  = 1 << 20
)

func ConfigFromEnv() Config {
	home, _ := os.UserHomeDir()
	root := os.Getenv("AUTO_ALLOWED_ROOT")
	if root == "" {
		root = home
	}
	dataDir := os.Getenv("AUTO_DATA_DIR")
	db := os.Getenv("AUTO_DATABASE_PATH")
	if db == "" && dataDir != "" {
		db = filepath.Join(dataDir, "data", "milevia.db")
	}
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
	codexPath := os.Getenv("AUTO_CODEX_PATH")
	if codexPath == "" {
		codexPath = "codex"
	}
	return Config{
		DatabasePath:                 db,
		DataDir:                      dataDir,
		Mode:                         "web",
		AllowedRoot:                  root,
		ClaudePath:                   "claude",
		CodexPath:                    codexPath,
		PermissionMode:               mode,
		ControlURL:                   controlURL,
		ApprovalHook:                 hook,
		AgentUpdateTimeout:           durationFromEnv("AUTO_AGENT_UPDATE_TIMEOUT", defaultAgentUpdateTimeout),
		ClaudeTurnIdleTimeout:        durationFromEnv("AUTO_CLAUDE_TURN_IDLE_TIMEOUT", defaultClaudeTurnIdleTimeout),
		ClaudeInitialResponseTimeout: durationFromEnv("AUTO_CLAUDE_INITIAL_RESPONSE_TIMEOUT", defaultClaudeInitialResponseTimeout),
		ClaudeToolResultTimeout:      durationFromEnv("AUTO_CLAUDE_TOOL_RESULT_TIMEOUT", defaultClaudeToolResultTimeout),
	}
}

func (c Config) agentUpdateTimeout() time.Duration {
	if c.AgentUpdateTimeout <= 0 {
		return defaultAgentUpdateTimeout
	}
	return c.AgentUpdateTimeout
}

func durationFromEnv(name string, fallback time.Duration) time.Duration {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

// RunConfig 持久化的项目启动配置。
type RunConfig struct {
	WorkDir         string             `json:"workDir"`
	Command         string             `json:"command"`
	EnvVars         map[string]string  `json:"envVars"`
	ExecutionTarget RunExecutionTarget `json:"executionTarget"`
}

// RunStatusResponse 是 GET /status 的响应。
type RunStatusResponse struct {
	Status          RunStatus        `json:"status"`
	ExecutionTarget RunExecutionTarget `json:"executionTarget"`
	StartedAt       *time.Time       `json:"startedAt"`
	PID             *int             `json:"pid"`
	ExitCode        *int             `json:"exitCode"`
	RecentLogs      []LogEntry       `json:"recentLogs"`
}

type Server struct {
	db                     *sql.DB
	config                 Config
	dataLock               *dataDirLock
	httpMu                 sync.Mutex
	httpServer             *http.Server
	runner                 AgentRunner
	codexRunner            AgentRunner
	runnerRegistry         *runnerRegistry
	runnerMaintenanceMu    sync.Mutex
	runnerUpdating         map[runnerAgentKey]bool
	runnerUpdateExecuting  map[string]bool
	projectLifecycleMu     sync.Mutex
	sshMu                  sync.Mutex
	sshPrepare             func(context.Context, SSHConnection) (*sshRunner, RunnerMeta, error)
	runtimeCtx             context.Context
	runtimeStop            context.CancelFunc
	runWG                  sync.WaitGroup
	closeOnce              sync.Once
	upgrader               websocket.Upgrader
	mu                     sync.Mutex
	closing                bool
	websocketWG            sync.WaitGroup
	subscribers            map[string]map[*websocket.Conn]*subscriber
	cancels                map[string]context.CancelFunc
	runTokens              map[string]string
	runContexts            map[string]string
	profileAdmissions      *profileRevisionAdmissionGate
	profileSecrets         *profileSecretStore
	profileRunCancels      map[string]map[string]context.CancelFunc
	streamingSetups        map[string]*streamingSetup
	projectWorkspaceLeases map[string]*projectWorkspaceLease
	runWorkspaceReleases   map[string]func()
	gitStateTokens         map[string]gitStateToken
	sessions               map[string]*activeAgentSession
	sessionMu              sync.Mutex
	streamMu               sync.Mutex
	approvals              map[string]*approvalWaiter
	usageMu                sync.Mutex
	runUsage               map[string]*runUsageAccumulator
	runManagers            map[string]projectRunnerInterface
	runManagersMu          sync.RWMutex
	runLogSubscribers      map[string]map[*websocket.Conn]*runLogSubscriber
	runLogSubMu            sync.Mutex
	notificationSubs       map[*websocket.Conn]*notificationSubscriber
	notificationSubMu      sync.Mutex
	processStatusSubs      map[*websocket.Conn]*processStatusSubscriber
	processStatusSubMu     sync.Mutex
	notificationMu         sync.Mutex
	orchestrationMu        sync.Mutex
	orchestrationActive    map[string]bool
	orchestrationWG        sync.WaitGroup
	orchestrationOwner     string
}

// runnerAgentKey scopes CLI maintenance to one tool on one Runner.
type runnerAgentKey struct {
	runnerID string
	agentID  string
}

type subscriber struct {
	conn           *websocket.Conn
	send           chan []byte
	closeOnce      sync.Once
	closeFrameOnce sync.Once
}

func (sub *subscriber) close() {
	sub.closeOnce.Do(func() { close(sub.send) })
}

func (sub *subscriber) closeWithStatus(code int, reason string) {
	sub.close()
	sub.closeFrameOnce.Do(func() { initiateWebSocketClose(sub.conn, code, reason) })
}

const (
	conversationSubscriberQueueSize = 256
	conversationWriteTimeout        = 10 * time.Second
	runLogSubscriberQueueSize       = 512
	runLogWriteTimeout              = 10 * time.Second
)

type runLogSubscriber struct {
	conn           *websocket.Conn
	send           chan LogEntry
	closeOnce      sync.Once
	closeFrameOnce sync.Once
}

func (sub *runLogSubscriber) close() {
	sub.closeOnce.Do(func() { close(sub.send) })
}

func (sub *runLogSubscriber) closeWithStatus(code int, reason string) {
	sub.close()
	sub.closeFrameOnce.Do(func() { initiateWebSocketClose(sub.conn, code, reason) })
}

type approvalWaiter struct {
	runID          string
	conversationID string
	decision       chan string
}

type activeAgentSession struct {
	agent         AgentSession
	approvalToken string
	runnerID      string
	agentID       string
	activeRunID   string
	stopping      bool
	runIDs        map[string]struct{}
}

// streamingSetup closes the gap between committing a streaming Run and
// admitting its first prompt to the persistent agent session. It is guarded
// by Server.mu.
type streamingSetup struct {
	cancelled   bool
	session     *activeAgentSession
	ownsSession bool
}

type projectWorkspaceLease struct {
	owner   string
	holders int
}

type Project struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Path        string    `json:"-"`
	PathDisplay string    `json:"pathDisplay"`
	FullPath    string    `json:"fullPath"`
	Runner      string    `json:"runner"`
	RunnerID    string    `json:"runnerId"`
	Environment string    `json:"environment"`
	GitBranch   string    `json:"gitBranch"`
	ClaudeReady bool      `json:"claudeReady"`
	CodexReady  bool      `json:"codexReady"`
	AgentReady  bool      `json:"agentReady"`
	CreatedAt   time.Time `json:"createdAt"`
	// DefaultProfileID is the agent profile applied to new conversations when no
	// explicit profile is requested, letting a project ride one credential.
	DefaultProfileID string `json:"defaultProfileId"`
}

type Conversation struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectId"`
	// ClaudeSessionID is retained for clients and databases created before the
	// agent-neutral session migration. New execution code uses AgentSessionID.
	ClaudeSessionID        string    `json:"claudeSessionId,omitempty"`
	AgentID                string    `json:"agentId"`
	AgentSessionID         string    `json:"agentSessionId"`
	AgentRuntimeID         string    `json:"agentRuntimeId"`
	AgentProfileRevisionID string    `json:"agentProfileRevisionId,omitempty"`
	ExecutionPolicy        string    `json:"executionPolicy"`
	Status                 string    `json:"status"`
	PermissionMode         string    `json:"permissionMode"`
	Title                  string    `json:"title"`
	Preview                string    `json:"preview,omitempty"`
	LastActivityAt         time.Time `json:"lastActivityAt"`
	ClaudeInitialized      bool      `json:"-"`
	AgentInitialized       bool      `json:"-"`
	IsCurrent              bool      `json:"isCurrent"`
	CreatedAt              time.Time `json:"createdAt"`
}

type Message struct {
	ID              string    `json:"id"`
	ConversationID  string    `json:"conversationId"`
	RunID           string    `json:"runId,omitempty"`
	Role            string    `json:"role"`
	Content         string    `json:"content"`
	ParentToolUseID string    `json:"parentToolUseId,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
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

type runStartRecord struct {
	Shortcut     *ShortcutRun
	Task         *TaskRun
	WorktreePath string
	Orchestrated bool
}

type Event struct {
	ID             string          `json:"id"`
	ConversationID string          `json:"conversationId"`
	RunID          string          `json:"runId"`
	Type           string          `json:"type"`
	Payload        json.RawMessage `json:"payload"`
	CreatedAt      time.Time       `json:"createdAt"`
}

// conversationPageCursor keeps independent positions for the two persisted
// streams. A timestamp alone is not enough because SQLite permits multiple
// records with the same created_at value.
type conversationPageCursor struct {
	Message *conversationPagePosition `json:"message,omitempty"`
	Event   *conversationPagePosition `json:"event,omitempty"`
}

type conversationListCursor struct {
	IsCurrent      bool      `json:"isCurrent"`
	LastActivityAt time.Time `json:"lastActivityAt"`
	ID             string    `json:"id"`
}

type conversationListPage struct {
	Items      []Conversation `json:"items"`
	NextCursor string         `json:"nextCursor"`
}

type conversationPagePosition struct {
	CreatedAt time.Time `json:"createdAt"`
	ID        string    `json:"id"`
}

func decodeConversationPageCursor(raw string) (conversationPageCursor, error) {
	if raw == "" {
		return conversationPageCursor{}, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return conversationPageCursor{}, errors.New("cursor is invalid")
	}
	var cursor conversationPageCursor
	if err := json.Unmarshal(decoded, &cursor); err != nil {
		return conversationPageCursor{}, errors.New("cursor is invalid")
	}
	for _, position := range []*conversationPagePosition{cursor.Message, cursor.Event} {
		if position != nil && (position.ID == "" || position.CreatedAt.IsZero()) {
			return conversationPageCursor{}, errors.New("cursor is invalid")
		}
	}
	return cursor, nil
}

func encodeConversationPageCursor(cursor conversationPageCursor) string {
	if cursor.Message == nil && cursor.Event == nil {
		return ""
	}
	encoded, err := json.Marshal(cursor)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func decodeConversationListCursor(raw string) (conversationListCursor, error) {
	if raw == "" {
		return conversationListCursor{}, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return conversationListCursor{}, errors.New("cursor is invalid")
	}
	var cursor conversationListCursor
	if err := json.Unmarshal(decoded, &cursor); err != nil || cursor.ID == "" || cursor.LastActivityAt.IsZero() {
		return conversationListCursor{}, errors.New("cursor is invalid")
	}
	return cursor, nil
}

func encodeConversationListCursor(cursor conversationListCursor) string {
	encoded, err := json.Marshal(cursor)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func New(ctx context.Context, config Config) (*Server, error) {
	return NewWithRunner(ctx, config, nil)
}

// NewWithRunner allows future AI CLIs to reuse the project and conversation services.
// A nil runner selects the built-in Claude Code CLI implementation.
func NewWithRunner(ctx context.Context, config Config, runner AgentRunner) (*Server, error) {
	if config.DataDir != "" {
		dataDir, err := filepath.Abs(config.DataDir)
		if err != nil {
			return nil, fmt.Errorf("resolve data directory: %w", err)
		}
		config.DataDir = dataDir
		if config.DatabasePath == "" {
			config.DatabasePath = filepath.Join(dataDir, "data", "milevia.db")
		}
	}
	if config.DatabasePath == "" {
		return nil, errors.New("database path is required")
	}
	var dataLock *dataDirLock
	if config.DataDir != "" {
		var err error
		dataLock, err = acquireDataDirLock(config.DataDir)
		if err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(config.DatabasePath), 0o755); err != nil {
		_ = dataLock.close()
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	pool, err := sql.Open("sqlite3", config.DatabasePath)
	if err != nil {
		_ = dataLock.close()
		return nil, fmt.Errorf("open SQLite: %w", err)
	}
	pool.SetMaxOpenConns(1)
	if _, err := pool.ExecContext(ctx, `pragma foreign_keys=on; pragma journal_mode=wal; pragma busy_timeout=5000`); err != nil {
		pool.Close()
		_ = dataLock.close()
		return nil, fmt.Errorf("configure SQLite: %w", err)
	}
	if runner == nil {
		approvalHook, err := filepath.Abs(config.ApprovalHook)
		if err != nil {
			pool.Close()
			_ = dataLock.close()
			return nil, fmt.Errorf("resolve approval hook: %w", err)
		}
		if info, err := os.Stat(approvalHook); err != nil || info.IsDir() {
			pool.Close()
			_ = dataLock.close()
			return nil, fmt.Errorf("approval hook is unavailable: %s", approvalHook)
		}
		config.ApprovalHook = approvalHook
		runner = newClaudeCLIRunner(config)
	}
	codexRunner := newCodexCLIRunner(config)
	runtimeCtx, runtimeStop := context.WithCancel(context.Background())
	s := &Server{db: pool, config: config, dataLock: dataLock, runner: runner, codexRunner: codexRunner, runnerRegistry: newRunnerRegistry(), runnerUpdating: map[runnerAgentKey]bool{}, runnerUpdateExecuting: map[string]bool{}, runtimeCtx: runtimeCtx, runtimeStop: runtimeStop, subscribers: map[string]map[*websocket.Conn]*subscriber{}, cancels: map[string]context.CancelFunc{}, runTokens: map[string]string{}, runContexts: map[string]string{}, profileAdmissions: newProfileRevisionAdmissionGate(), profileRunCancels: map[string]map[string]context.CancelFunc{}, streamingSetups: map[string]*streamingSetup{}, projectWorkspaceLeases: map[string]*projectWorkspaceLease{}, runWorkspaceReleases: map[string]func(){}, gitStateTokens: map[string]gitStateToken{}, sessions: map[string]*activeAgentSession{}, approvals: map[string]*approvalWaiter{}, runUsage: map[string]*runUsageAccumulator{}, runManagers: map[string]projectRunnerInterface{}, runLogSubscribers: map[string]map[*websocket.Conn]*runLogSubscriber{}, notificationSubs: map[*websocket.Conn]*notificationSubscriber{}, processStatusSubs: map[*websocket.Conn]*processStatusSubscriber{}, orchestrationActive: map[string]bool{}, orchestrationOwner: uuid.NewString()}
	s.upgrader.CheckOrigin = func(r *http.Request) bool { return s.allowedOrigin(r.Header.Get("Origin")) }
	if config.SessionToken != "" {
		s.upgrader.Subprotocols = []string{s.websocketSessionProtocol()}
	}
	keyPath := filepath.Join(config.DataDir, "profile-master.key")
	if keyPath == filepath.Join("", "profile-master.key") {
		keyPath = filepath.Join(filepath.Dir(config.DatabasePath), "profile-master.key")
	}
	if manager, err := newProfileSecretStore(pool, keyPath); err != nil {
		runtimeStop()
		pool.Close()
		_ = dataLock.close()
		return nil, fmt.Errorf("initialize managed credential store: %w", err)
	} else {
		s.profileSecrets = manager
	}
	if err := s.migrate(ctx); err != nil {
		runtimeStop()
		pool.Close()
		_ = dataLock.close()
		return nil, err
	}
	if err := s.recoverInterruptedRuns(ctx); err != nil {
		runtimeStop()
		pool.Close()
		_ = dataLock.close()
		return nil, err
	}
	if err := s.recoverGitOperations(ctx); err != nil {
		runtimeStop()
		pool.Close()
		_ = dataLock.close()
		return nil, err
	}
	// Register the native local runner for this server platform.
	s.runnerRegistry.register(s.localRunnerID(), runner, s.localRunnerMeta())
	// Recover previously-connected SSH connections (failures are non-fatal).
	if err := s.recoverSSHConnections(ctx); err != nil {
		log.Printf("[ssh] failed to recover SSH connections: %v", err)
	}
	s.recoverOrchestration(ctx)
	return s, nil
}

func (s *Server) Close() {
	s.closeOnce.Do(func() {
		s.httpMu.Lock()
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
		httpServer := s.httpServer
		s.httpMu.Unlock()
		// Upgraded connections are not regular HTTP requests. Notify them before
		// closing the listener so clients and the Vite proxy receive a WebSocket
		// close frame instead of a TCP reset.
		s.closeAllConversationSubscribers()
		s.closeAllRunLogSubscribers()
		s.closeAllNotificationSubscribers()
		s.closeAllProcessStatusSubscribers()
		s.waitForWebSocketSubscriptions()
		if httpServer != nil {
			_ = httpServer.Close()
		}
		s.runtimeStop()
		for _, cancel := range cancels {
			cancel()
		}
		for _, session := range sessions {
			session.Stop()
		}
		// No new orchestration worker may start once closing is set. Wait for
		// existing workers before releasing their fenced leases to another server.
		s.orchestrationMu.Lock()
		s.orchestrationMu.Unlock()
		s.orchestrationWG.Wait()
		s.releaseOrchestrationLeases()
		for _, runID := range runIDs {
			s.resolveRunApprovals(runID, "deny")
		}
		s.runManagersMu.RLock()
		runners := make([]projectRunnerInterface, 0, len(s.runManagers))
		for _, runner := range s.runManagers {
			runners = append(runners, runner)
		}
		s.runManagersMu.RUnlock()
		for _, runner := range runners {
			runner.Stop()
		}
		for _, runner := range s.runnerRegistry.all() {
			if sshR, ok := runner.(*sshRunner); ok {
				_ = sshR.client.close()
			}
		}
		s.runWG.Wait()
		_ = s.db.Close()
		_ = s.dataLock.close()
	})
}

func (s *Server) Listen(addr string) error {
	return s.ListenWithReady(addr, nil)
}

// ListenWithReady invokes ready after the TCP listener owns its assigned port.
// It is used by the desktop sidecar launcher to discover a random loopback port.
func (s *Server) ListenWithReady(addr string, ready func(string)) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.ServeListener(listener, ready)
}

// ServeListener serves an already-bound listener. Desktop startup uses it to
// discover the assigned random port before runners receive their ControlURL.
func (s *Server) ServeListener(listener net.Listener, ready func(string)) error {
	s.httpMu.Lock()
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		s.httpMu.Unlock()
		_ = listener.Close()
		return nil
	}
	if s.httpServer != nil {
		s.mu.Unlock()
		s.httpMu.Unlock()
		_ = listener.Close()
		return errors.New("HTTP server is already listening")
	}
	httpServer := s.newHTTPServer()
	s.httpServer = httpServer
	s.mu.Unlock()
	s.httpMu.Unlock()

	listenURL := "http://" + listener.Addr().String()
	if err := s.dataLock.writeState(fmt.Sprintf("pid=%d\nstarted_at=%s\napi_base=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano), listenURL)); err != nil {
		log.Printf("write data directory lock state: %v", err)
	}
	serveResult := make(chan error, 1)
	go func() { serveResult <- httpServer.Serve(listener) }()
	if ready != nil {
		deadline := time.Now().Add(2 * time.Second)
		for {
			connection, dialErr := net.DialTimeout("tcp", listener.Addr().String(), 50*time.Millisecond)
			if dialErr == nil {
				_ = connection.Close()
				break
			}
			if time.Now().After(deadline) {
				log.Printf("control server accept loop did not become ready before timeout: %v", dialErr)
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		ready(listenURL)
	}
	err := <-serveResult
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) newHTTPServer() *http.Server {
	// WebSocket connections are hijacked by Gorilla and have their own
	// heartbeat/deadline policy. The subscription handlers replace the initial
	// read deadline immediately after upgrading the connection.
	return &http.Server{
		Handler:           s.routes(),
		ReadHeaderTimeout: httpReadHeaderTimeout,
		ReadTimeout:       httpReadTimeout,
		IdleTimeout:       httpIdleTimeout,
		MaxHeaderBytes:    maxHTTPHeaderBytes,
	}
}

func (s *Server) routes() http.Handler {
	r := chi.NewRouter()
	r.Use(s.cors)
	r.Use(s.requireSession)
	r.Get("/api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	if s.config.Mode == "desktop-api" && s.config.SessionToken != "" {
		r.Post("/api/internal/shutdown", s.shutdown)
	}
	r.Get("/api/runners", s.listRunners)
	r.Get("/api/runners/{runnerID}/agent-profiles", s.listAgentProfiles)
	r.Post("/api/runners/{runnerID}/agent-profiles", s.createAgentProfile)
	r.Patch("/api/agent-profiles/{profileID}", s.updateAgentProfile)
	r.Post("/api/agent-profiles/{profileID}/validate", s.validateAgentProfile)
	r.Post("/api/agent-profiles/{profileID}/enable", s.enableAgentProfile)
	r.Post("/api/agent-profiles/{profileID}/disable", s.disableAgentProfile)
	r.Post("/api/agent-profile-revisions/{revisionID}/revoke", s.revokeAgentProfileRevision)
	r.Post("/api/runners/{runnerID}/claude/check-update", s.checkClaudeUpdate)
	r.Post("/api/runners/{runnerID}/claude/update", s.updateClaude)
	r.Post("/api/runners/{runnerID}/codex/check-update", s.checkCodexUpdate)
	r.Post("/api/runners/{runnerID}/codex/update", s.updateCodex)
	r.Get("/api/directories", s.listDirectories)
	r.Post("/api/directories/mkdir", s.createDirectory)
	r.Post("/api/projects/validate", s.validateProject)
	r.Get("/api/projects", s.listProjects)
	r.Get("/api/projects/statuses", s.listProjectStatuses)
	r.Get("/api/projects/processes/statuses", s.listProjectProcessStatuses)
	r.Post("/api/projects", s.createProject)
	r.Get("/api/projects/{projectID}", s.getProject)
	r.Delete("/api/projects/{projectID}", s.deleteProject)
	r.Patch("/api/projects/{projectID}/agent-profile", s.setProjectDefaultProfile)
	r.Get("/api/projects/{projectID}/git/summary", s.gitSummary)
	r.Get("/api/projects/{projectID}/git/changes", s.gitChanges)
	r.Get("/api/projects/{projectID}/git/diff", s.gitDiff)
	r.Get("/api/projects/{projectID}/git/log", s.gitLog)
	r.Get("/api/projects/{projectID}/git/branches", s.gitBranches)
	r.Get("/api/projects/{projectID}/git/operations", s.gitOperations)
	r.Post("/api/projects/{projectID}/git/stage", s.gitStage)
	r.Post("/api/projects/{projectID}/git/unstage", s.gitUnstage)
	r.Post("/api/projects/{projectID}/git/stage-all", s.gitStageAll)
	r.Post("/api/projects/{projectID}/git/unstage-all", s.gitUnstageAll)
	r.Post("/api/projects/{projectID}/git/commits", s.gitCommit)
	r.Post("/api/projects/{projectID}/git/discard", s.gitDiscard)
	r.Post("/api/projects/{projectID}/git/fetch", s.gitFetch)
	r.Post("/api/projects/{projectID}/git/push", s.gitPush)
	r.Post("/api/projects/{projectID}/git/branches", s.gitCreateBranch)
	r.Post("/api/projects/{projectID}/git/switch", s.gitSwitchBranch)
	r.Get("/api/projects/{projectID}/tasks", s.listTasks)
	r.Post("/api/projects/{projectID}/tasks", s.createTask)
	r.Post("/api/projects/{projectID}/tasks/review-all", s.reviewAllTasks)
	r.Get("/api/projects/{projectID}/input-history", s.listProjectInputHistory)
	r.Get("/api/projects/{projectID}/orchestration/config", s.getOrchestrationConfig)
	r.Put("/api/projects/{projectID}/orchestration/config", s.updateOrchestrationConfig)
	r.Get("/api/projects/{projectID}/orchestration", s.listOrchestrationJobs)
	r.Get("/api/projects/{projectID}/orchestration/releases", s.listReleaseSnapshots)
	r.Post("/api/projects/{projectID}/orchestration/release", s.createReleaseSnapshot)
	r.Post("/api/projects/{projectID}/orchestration/releases/{releaseID}/confirm-main", s.confirmReleaseMergedToMain)
	r.Post("/api/projects/{projectID}/orchestration/enqueue-batch", s.enqueueBatchForOrchestration)
	r.Patch("/api/projects/{projectID}/orchestration/order", s.reorderOrchestrationJobs)
	r.Get("/api/tasks/{taskID}", s.getTask)
	r.Patch("/api/tasks/{taskID}", s.updateTask)
	r.Delete("/api/tasks/{taskID}", s.deleteTask)
	r.Post("/api/tasks/{taskID}/dependencies", s.addTaskDependency)
	r.Put("/api/tasks/{taskID}/dependencies", s.replaceTaskDependencies)
	r.Delete("/api/tasks/{taskID}/dependencies/{predecessorTaskID}", s.deleteTaskDependency)
	r.Get("/api/tasks/{taskID}/runs", s.listTaskRuns)
	r.Get("/api/tasks/{taskID}/events", s.listTaskEvents)
	r.Post("/api/tasks/{taskID}/dispatch", s.dispatchTask)
	r.Post("/api/tasks/{taskID}/orchestration/enqueue", s.enqueueTaskForOrchestration)
	r.Post("/api/tasks/{taskID}/orchestration/pause", s.pauseOrchestrationJob)
	r.Post("/api/tasks/{taskID}/orchestration/resume", s.resumeOrchestrationJob)
	r.Delete("/api/tasks/{taskID}/orchestration/dequeue", s.dequeueTaskFromOrchestration)
	r.Post("/api/tasks/{taskID}/review", s.reviewTask)
	r.Post("/api/tasks/{taskID}/reopen", s.reopenTask)
	r.Post("/api/tasks/{taskID}/stop", s.stopTask)
	r.Get("/api/projects/{projectID}/conversations", s.listConversations)
	r.Post("/api/projects/{projectID}/conversations", s.createConversation)
	r.Get("/api/projects/{projectID}/shortcuts", s.listShortcuts)
	r.Put("/api/projects/{projectID}/shortcuts/reorder", s.reorderShortcuts)
	r.Post("/api/shortcuts/defaults", s.seedDefaultShortcuts)
	r.Post("/api/shortcuts", s.createShortcut)
	r.Patch("/api/shortcuts/{shortcutID}", s.updateShortcut)
	r.Delete("/api/shortcuts/{shortcutID}", s.deleteShortcut)
	r.Get("/api/conversations/{conversationID}", s.getConversation)
	r.Get("/api/conversations/{conversationID}/input-history", s.listInputHistory)
	r.Get("/api/conversations/{conversationID}/usage", s.getConversationUsage)
	r.Post("/api/conversations/{conversationID}/clear", s.clearConversation)
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
	r.Route("/api/projects/{projectID}/run", func(r chi.Router) {
		r.Get("/config", s.getRunConfig)
		r.Put("/config", s.updateRunConfig)
		r.Post("/start", s.startProjectRun)
		r.Post("/stop", s.stopProjectRun)
		r.Post("/restart", s.restartProjectRun)
		r.Get("/status", s.getProjectRunStatus)
	})
	r.Get("/ws/projects/{projectID}/run", s.subscribeRunLogs)
	r.Get("/ws/notifications", s.subscribeNotifications)
	r.Get("/ws/processes", s.subscribeProcessStatuses)
	r.Get("/api/notifications", s.listNotifications)
	r.Post("/api/notifications/{notificationID}/dismiss", s.dismissNotification)
	r.Post("/api/notifications/dismiss-all", s.dismissAllNotifications)
	s.registerSSHRoutes(r)
	s.registerFSRoutes(r)
	if s.config.Mode == "web" && s.config.WebRoot != "" {
		r.NotFound(s.serveWebApp)
	}
	return r
}

func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); s.allowedOrigin(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Milevia-Session")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) allowedOrigin(origin string) bool {
	if origin == "" {
		return s.config.SessionToken == ""
	}
	if len(s.config.AllowedOrigins) > 0 {
		for _, allowed := range s.config.AllowedOrigins {
			if origin == allowed {
				return true
			}
		}
		return false
	}
	return isLocalWebOrigin(origin)
}

func isLocalWebOrigin(origin string) bool {
	parsed, err := url.Parse(origin)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return false
	}
	return parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1"
}

func (s *Server) websocketSessionProtocol() string {
	return "milevia-session." + s.config.SessionToken
}

func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.config.SessionToken == "" || r.URL.Path == "/api/health" || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		// Claude hooks and SSH approval tunnels cannot carry the desktop page
		// session. The handler validates this separate, per-run secret before it
		// creates an approval waiter.
		if r.URL.Path == "/api/internal/approvals/wait" && r.Header.Get("X-Auto-Approval-Token") != "" {
			next.ServeHTTP(w, r)
			return
		}
		if websocket.IsWebSocketUpgrade(r) {
			for _, protocol := range websocket.Subprotocols(r) {
				if protocol == s.websocketSessionProtocol() {
					next.ServeHTTP(w, r)
					return
				}
			}
		} else if r.Header.Get("X-Milevia-Session") == s.config.SessionToken {
			next.ServeHTTP(w, r)
			return
		}
		writeError(w, http.StatusUnauthorized, errors.New("invalid desktop session"))
	})
}

func (s *Server) serveWebApp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusNotFound, errors.New("route not found"))
		return
	}
	// BrowserRouter owns application routes only. Unknown API and WebSocket
	// paths must remain real 404s instead of returning HTML with a 200 status.
	if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/ws/") {
		writeError(w, http.StatusNotFound, errors.New("route not found"))
		return
	}
	cleanPath := pathpkg.Clean("/" + r.URL.Path)
	target := filepath.Join(s.config.WebRoot, filepath.FromSlash(strings.TrimPrefix(cleanPath, "/")))
	root := filepath.Clean(s.config.WebRoot) + string(os.PathSeparator)
	if !strings.HasPrefix(filepath.Clean(target)+string(os.PathSeparator), root) {
		writeError(w, http.StatusBadRequest, errors.New("invalid asset path"))
		return
	}
	if info, err := os.Stat(target); err == nil && !info.IsDir() {
		http.ServeFile(w, r, target)
		return
	}
	http.ServeFile(w, r, filepath.Join(s.config.WebRoot, "index.html"))
}

// shutdown is intentionally available only through the desktop session
// middleware. Closing on a separate goroutine lets the sidecar flush the HTTP
// response before it stops its listener and SQLite connections.
func (s *Server) shutdown(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "shutting_down"})
	go s.Close()
}

func ensureColumn(ctx context.Context, db *sql.DB, table, column, definition string) error {
	rows, err := db.QueryContext(ctx, fmt.Sprintf("pragma table_info(%s)", table))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, fmt.Sprintf("alter table %s add column %s %s", table, column, definition))
	return err
}

func (s *Server) migrateProjectsRunnerPathUnique(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `pragma index_list(projects)`)
	if err != nil {
		return fmt.Errorf("inspect project indexes: %w", err)
	}
	uniqueIndexes := []string{}
	for rows.Next() {
		var sequence int
		var name, origin string
		var unique, partial int
		if err := rows.Scan(&sequence, &name, &unique, &origin, &partial); err != nil {
			rows.Close()
			return fmt.Errorf("read project index: %w", err)
		}
		if unique == 0 {
			continue
		}
		uniqueIndexes = append(uniqueIndexes, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate project indexes: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close project indexes: %w", err)
	}
	needsRebuild := false
	for _, name := range uniqueIndexes {
		columns, err := s.projectIndexColumns(ctx, name)
		if err != nil {
			return err
		}
		if len(columns) == 1 && columns[0] == "path" {
			needsRebuild = true
		}
	}
	if !needsRebuild {
		return nil
	}

	if _, err := s.db.ExecContext(ctx, `pragma foreign_keys=off`); err != nil {
		return fmt.Errorf("disable foreign keys for project rebuild: %w", err)
	}
	restoreForeignKeys := func() error {
		_, err := s.db.ExecContext(ctx, `pragma foreign_keys=on`)
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		_ = restoreForeignKeys()
		return fmt.Errorf("begin project rebuild: %w", err)
	}
	for _, statement := range []string{
		`create table projects_rebuilt (id text primary key, name text not null, path text not null, runner text not null, git_branch text not null, claude_ready integer not null, created_at datetime not null)`,
		`insert into projects_rebuilt (id,name,path,runner,git_branch,claude_ready,created_at) select id,name,path,runner,git_branch,claude_ready,created_at from projects`,
		`drop table projects`,
		`alter table projects_rebuilt rename to projects`,
		`create unique index projects_runner_path_unique on projects(runner,path)`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			_ = tx.Rollback()
			_ = restoreForeignKeys()
			return fmt.Errorf("rebuild projects table: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		_ = restoreForeignKeys()
		return fmt.Errorf("commit project rebuild: %w", err)
	}
	if err := restoreForeignKeys(); err != nil {
		return fmt.Errorf("restore foreign keys after project rebuild: %w", err)
	}
	return nil
}

func (s *Server) projectIndexColumns(ctx context.Context, indexName string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf("pragma index_info(%q)", indexName))
	if err != nil {
		return nil, fmt.Errorf("inspect project index columns: %w", err)
	}
	defer rows.Close()
	columns := []string{}
	for rows.Next() {
		var sequence, columnID int
		var name string
		if err := rows.Scan(&sequence, &columnID, &name); err != nil {
			return nil, fmt.Errorf("read project index column: %w", err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate project index columns: %w", err)
	}
	return columns, nil
}

func (s *Server) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `create table if not exists projects (id text primary key, name text not null, path text not null, runner text not null, git_branch text not null, claude_ready integer not null, created_at datetime not null);
create table if not exists conversations (id text primary key, project_id text not null references projects(id) on delete cascade, claude_session_id text not null unique, agent_id text not null default 'claude-code', agent_session_id text not null default '', agent_runtime_id text not null default '', execution_policy text not null default 'approval_required', status text not null, permission_mode text not null default 'approval_required', title text not null default '新会话', last_activity_at datetime not null default current_timestamp, claude_initialized integer not null default 0, agent_initialized integer not null default 0, is_current integer not null default 1, created_at datetime not null);
	create table if not exists messages (id text primary key, conversation_id text not null references conversations(id) on delete cascade, run_id text not null default '', role text not null, content text not null, parent_tool_use_id text not null default '', created_at datetime not null);
create table if not exists runs (id text primary key, conversation_id text not null references conversations(id) on delete cascade, agent_id text not null default 'claude-code', agent_runtime_id text not null default '', execution_policy text not null default 'approval_required', agent_run_id text not null default '', status text not null, created_at datetime not null, completed_at datetime);
create table if not exists events (id text primary key, conversation_id text not null references conversations(id) on delete cascade, run_id text not null references runs(id) on delete cascade, type text not null, payload text not null, created_at datetime not null);
create table if not exists run_usage (run_id text primary key references runs(id) on delete cascade, conversation_id text not null references conversations(id) on delete cascade, model text not null default '', context_window integer not null default 0, context_input_tokens integer not null default 0, input_tokens integer not null default 0, output_tokens integer not null default 0, cache_read_tokens integer not null default 0, cache_creation_tokens integer not null default 0, estimated_cost_usd real not null default 0, agent_turns integer not null default 0, model_steps integer not null default 0, tool_calls integer not null default 0, subagent_count integer not null default 0, duration_ms integer not null default 0, ttft_ms integer not null default 0, terminal_reason text not null default '', has_result integer not null default 0, completed_at datetime not null);
create table if not exists run_model_usage (run_id text not null references runs(id) on delete cascade, model text not null, input_tokens integer not null default 0, output_tokens integer not null default 0, cache_read_tokens integer not null default 0, cache_creation_tokens integer not null default 0, estimated_cost_usd real not null default 0, context_window integer not null default 0, primary key (run_id,model));
create table if not exists shortcuts (id text primary key, name text not null, description text not null default '', kind text not null, template text not null, scope text not null, default_action text not null, group_name text not null default '', pinned integer not null default 0, enabled integer not null default 1, sort_order integer not null default 0, created_at datetime not null, updated_at datetime not null);
create table if not exists shortcut_projects (shortcut_id text not null references shortcuts(id) on delete cascade, project_id text not null references projects(id) on delete cascade, primary key (shortcut_id,project_id));
create table if not exists shortcut_runs (id text primary key, shortcut_id text references shortcuts(id) on delete set null, conversation_id text not null references conversations(id) on delete cascade, run_id text unique references runs(id) on delete set null, rendered_content text not null, action text not null, status text not null, created_at datetime not null, completed_at datetime);
create table if not exists shortcut_audit_events (id text primary key, shortcut_id text references shortcuts(id) on delete set null, action text not null, payload text not null default '{}', created_at datetime not null);
create table if not exists app_metadata (key text primary key, value text not null);
	create table if not exists project_run_configs (project_id text primary key references projects(id) on delete cascade, work_dir text not null default '', command text not null default '', env_vars text not null default '{}', execution_target text not null default 'auto', updated_at datetime not null);
	create unique index if not exists projects_runner_path_unique on projects(runner,path);
	create unique index if not exists conversations_one_current_per_project on conversations(project_id) where is_current=1;
	create index if not exists messages_conversation_created on messages(conversation_id,created_at desc);
	create index if not exists events_conversation_created on events(conversation_id,created_at desc);
	create index if not exists run_usage_conversation_completed on run_usage(conversation_id,completed_at desc);`)
	if err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	if err := s.migrateProjectsRunnerPathUnique(ctx); err != nil {
		return fmt.Errorf("migrate project path uniqueness: %w", err)
	}
	if err := s.migrateSSHConnections(ctx); err != nil {
		return fmt.Errorf("migrate ssh connections: %w", err)
	}
	if err := s.migratePersistedRunners(ctx); err != nil {
		return fmt.Errorf("migrate persisted runners: %w", err)
	}
	if err := s.migrateAgentProfiles(ctx); err != nil {
		return err
	}
	if err := ensureColumn(ctx, s.db, "project_run_configs", "execution_target", "text not null default 'auto'"); err != nil {
		return fmt.Errorf("add project run execution target: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `pragma table_info(conversations)`)
	if err != nil {
		return fmt.Errorf("inspect conversations schema: %w", err)
	}
	defer rows.Close()
	hasPermissionMode, hasTitle, hasLastActivity, hasAgentID := false, false, false, false
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
		if name == "agent_id" {
			hasAgentID = true
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
	if !hasAgentID {
		if _, err := s.db.ExecContext(ctx, `alter table conversations add column agent_id text not null default 'claude-code'`); err != nil {
			return fmt.Errorf("add conversation agent id: %w", err)
		}
	}
	rows.Close()
	for _, column := range []struct {
		name       string
		definition string
	}{
		{"agent_session_id", "text not null default ''"},
		{"agent_initialized", "integer not null default 0"},
		{"agent_runtime_id", "text not null default ''"},
		{"execution_policy", "text not null default 'approval_required'"},
	} {
		if err := ensureColumn(ctx, s.db, "conversations", column.name, column.definition); err != nil {
			return fmt.Errorf("add conversation %s: %w", column.name, err)
		}
	}
	for _, column := range []struct {
		name       string
		definition string
	}{
		{"agent_id", "text not null default 'claude-code'"},
		{"agent_runtime_id", "text not null default ''"},
		{"execution_policy", "text not null default 'approval_required'"},
		{"agent_run_id", "text not null default ''"},
	} {
		if err := ensureColumn(ctx, s.db, "runs", column.name, column.definition); err != nil {
			return fmt.Errorf("add run %s: %w", column.name, err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `update conversations set agent_session_id=case when agent_session_id='' then claude_session_id else agent_session_id end, agent_initialized=case when agent_initialized=0 then claude_initialized else agent_initialized end, agent_runtime_id=case when agent_runtime_id='' then coalesce((select nullif(runner_id,'') from projects where projects.id=conversations.project_id),(select runner from projects where projects.id=conversations.project_id),'') else agent_runtime_id end, execution_policy=case when execution_policy='' or execution_policy='approval_required' then permission_mode else execution_policy end`); err != nil {
		return fmt.Errorf("backfill agent conversation fields: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `update runs set agent_id=coalesce(nullif((select agent_id from conversations where conversations.id=runs.conversation_id),''),'claude-code'), agent_runtime_id=coalesce(nullif((select agent_runtime_id from conversations where conversations.id=runs.conversation_id),''),''), execution_policy=coalesce(nullif((select execution_policy from conversations where conversations.id=runs.conversation_id),''),'approval_required') where agent_runtime_id=''`); err != nil {
		return fmt.Errorf("backfill agent run fields: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `create unique index if not exists conversations_agent_session_unique on conversations(agent_runtime_id,agent_id,agent_session_id) where agent_session_id<>''`); err != nil {
		return fmt.Errorf("create agent session index: %w", err)
	}
	messageColumns, err := s.db.QueryContext(ctx, `pragma table_info(messages)`)
	if err != nil {
		return fmt.Errorf("inspect messages schema: %w", err)
	}
	hasMessageParent, hasMessageRun := false, false
	for messageColumns.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var primaryKey int
		if err := messageColumns.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			messageColumns.Close()
			return fmt.Errorf("read messages schema: %w", err)
		}
		if name == "parent_tool_use_id" {
			hasMessageParent = true
		}
		if name == "run_id" {
			hasMessageRun = true
		}
	}
	if err := messageColumns.Err(); err != nil {
		messageColumns.Close()
		return fmt.Errorf("iterate messages schema: %w", err)
	}
	messageColumns.Close()
	if !hasMessageParent {
		if _, err := s.db.ExecContext(ctx, `alter table messages add column parent_tool_use_id text not null default ''`); err != nil {
			return fmt.Errorf("add message parent tool use id: %w", err)
		}
	}
	if !hasMessageRun {
		if _, err := s.db.ExecContext(ctx, `alter table messages add column run_id text not null default ''`); err != nil {
			return fmt.Errorf("add message run id: %w", err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `update conversations set last_activity_at=coalesce(last_activity_at,(select max(created_at) from messages where messages.conversation_id=conversations.id),created_at)`); err != nil {
		return fmt.Errorf("backfill conversation activity: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `update conversations set title=coalesce(nullif((select substr(trim(content),1,80) from messages where messages.conversation_id=conversations.id and role='user' order by created_at limit 1),''),'新会话') where title='' or title='新会话'`); err != nil {
		return fmt.Errorf("backfill conversation title: %w", err)
	}
	// Ensure shortcut_projects has the required project_id column.
	// An older database may have a shortcut_projects table without it.
	if err := s.ensureShortcutProjectID(ctx); err != nil {
		return fmt.Errorf("ensure shortcut_projects schema: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `create index if not exists conversations_project_activity on conversations(project_id,last_activity_at desc)`); err != nil {
		return fmt.Errorf("create conversation activity index: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `create index if not exists shortcuts_visible on shortcuts(scope,enabled,pinned,sort_order);
create index if not exists shortcut_projects_project on shortcut_projects(project_id);
create index if not exists shortcut_runs_conversation on shortcut_runs(conversation_id,created_at desc);
create index if not exists shortcut_audit_events_shortcut on shortcut_audit_events(shortcut_id,created_at desc);
create index if not exists messages_conversation_primary_created on messages(conversation_id,parent_tool_use_id,created_at desc);`); err != nil {
		return fmt.Errorf("create shortcut indexes: %w", err)
	}
	if err := s.migrateTasks(ctx); err != nil {
		return err
	}
	if err := s.migrateOrchestration(ctx); err != nil {
		return err
	}
	if err := s.migrateNotifications(ctx); err != nil {
		return err
	}
	return s.migrateGit(ctx)
}

// ensureShortcutProjectID verifies the shortcut_projects table has the
// project_id column and adds it if missing. This guards against databases
// created before the column was part of the schema.
func (s *Server) ensureShortcutProjectID(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `pragma table_info(shortcut_projects)`)
	if err != nil {
		// Unexpected error reading schema (e.g. database corruption).
		return fmt.Errorf("inspect shortcut_projects schema: %w", err)
	}
	defer rows.Close()
	hasProjectID := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var primaryKey int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			return fmt.Errorf("read shortcut_projects schema: %w", err)
		}
		if name == "project_id" {
			hasProjectID = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate shortcut_projects schema: %w", err)
	}
	if !hasProjectID {
		// The table exists but is missing project_id. Since SQLite does not
		// support adding columns with foreign-key constraints via ALTER TABLE,
		// and this table only contains association data (no independent
		// meaning), we drop and recreate it. Shortcut-to-project bindings
		// will be re-established when the user saves each shortcut again.
		if _, err := s.db.ExecContext(ctx, `drop table if exists shortcut_projects;
create table shortcut_projects (shortcut_id text not null references shortcuts(id) on delete cascade, project_id text not null references projects(id) on delete cascade, primary key (shortcut_id,project_id))`); err != nil {
			return fmt.Errorf("rebuild shortcut_projects: %w", err)
		}
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
		for _, run := range runs {
			if err := s.finishTaskRunTx(ctx, tx, run.id, "interrupted", "控制服务在任务完成前已重启，任务已中断。", now); err != nil {
				return fmt.Errorf("mark interrupted task run: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `update runs set status='interrupted',completed_at=? where status in ('queued','running')`, now); err != nil {
			return fmt.Errorf("mark interrupted runs: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `update conversations set status='idle' where status='running'`, now); err != nil {
		return fmt.Errorf("release interrupted conversations: %w", err)
	}
	for _, run := range runs {
		if _, err := tx.ExecContext(ctx, `insert into events (id,conversation_id,run_id,type,payload,created_at) values (?,?,?,?,?,?)`, uuid.NewString(), run.conversationID, run.id, "run.interrupted", mustJSON(map[string]string{"status": "interrupted", "error": "控制服务在任务完成前已重启，任务已中断。"}), now); err != nil {
			return fmt.Errorf("record interrupted run: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit interrupted run recovery: %w", err)
	}
	// 通知：中断恢复后任务状态变为 action_required
	for _, run := range runs {
		var taskID, taskStatus string
		if err := s.db.QueryRowContext(s.runtimeCtx, `select task_id from task_runs where run_id=?`, run.id).Scan(&taskID); err != nil || taskID == "" {
			continue
		}
		if err := s.db.QueryRowContext(s.runtimeCtx, `select status from tasks where id=?`, taskID).Scan(&taskStatus); err != nil {
			continue
		}
		s.notifyTaskStatusChange(s.runtimeCtx, taskID, taskStatus)
	}
	return nil
}

func validPermissionMode(mode string) bool {
	return mode == "approval_required" || mode == "full_control" || mode == "read_only" || mode == "workspace_write"
}

func validAgentPolicy(agentID, policy string) bool {
	if agentID == "codex" {
		return policy == "read_only" || policy == "workspace_write" || policy == "full_control"
	}
	return agentID == "claude-code" && (policy == "approval_required" || policy == "full_control")
}

func (c Conversation) sessionID() string {
	if c.AgentSessionID != "" {
		return c.AgentSessionID
	}
	return c.ClaudeSessionID
}

func (c Conversation) initialized() bool { return c.AgentInitialized || c.ClaudeInitialized }

func (c Conversation) executionPolicy() string {
	// Rows inserted by pre-migration callers have the new column's default but
	// no agent session yet. Their legacy permission is still authoritative.
	if c.AgentSessionID == "" {
		return c.PermissionMode
	}
	if c.ExecutionPolicy != "" {
		return c.ExecutionPolicy
	}
	return c.PermissionMode
}

func (s *Server) listDirectories(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	runnerID := r.URL.Query().Get("runner")
	if path == "" {
		path = s.config.AllowedRoot
	}
	// For SSH runners, use SFTP to list remote directories.
	if strings.HasPrefix(runnerID, "ssh-") {
		runner, ok := s.runnerRegistry.get(runnerID)
		if !ok {
			writeError(w, http.StatusBadRequest, errors.New("runner not found"))
			return
		}
		if sshR, ok := runner.(*sshRunner); ok {
			if r.URL.Query().Get("path") == "" {
				path = sshR.rootPath
			}
			path, err := sshR.canonicalProjectPath(r.Context(), path)
			if err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			entries, err := sshR.client.readDir(r.Context(), path)
			if err != nil {
				writeError(w, http.StatusBadRequest, fmt.Errorf("无法读取远程目录：%w", err))
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
			parent := filepath.Dir(path)
			meta, _ := s.runnerRegistry.getMeta(runnerID)
			rootPath := "/"
			if len(meta.Roots) > 0 {
				rootPath = meta.Roots[0].Path
			}
			if !strings.HasPrefix(parent, rootPath) {
				parent = rootPath
			}
			writeJSON(w, http.StatusOK, map[string]any{"path": path, "parent": parent, "directories": items})
			return
		}
	}
	path, err := s.allowedPathForRunner(path, runnerID)
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
	// 计算 parent：不允许越过 runner 的任意一个 root
	parent := filepath.Dir(path)
	if meta, ok := s.runnerRegistry.getMeta(runnerID); ok && len(meta.Roots) > 0 {
		withinRoot := false
		for _, root := range meta.Roots {
			resolvedRoot, err := filepath.EvalSymlinks(root.Path)
			if err != nil {
				continue
			}
			if parent == resolvedRoot || strings.HasPrefix(parent, resolvedRoot+string(os.PathSeparator)) {
				withinRoot = true
				break
			}
		}
		if !withinRoot {
			// parent 越过了所有 root，回退到当前路径所属的 root
			for _, root := range meta.Roots {
				resolvedRoot, err := filepath.EvalSymlinks(root.Path)
				if err != nil {
					continue
				}
				if path == resolvedRoot || strings.HasPrefix(path, resolvedRoot+string(os.PathSeparator)) {
					parent = resolvedRoot
					break
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": path, "parent": parent, "directories": items})
}

func (s *Server) createDirectory(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Path   string `json:"path"`
		Runner string `json:"runner"`
	}
	if !decode(w, r, &input) {
		return
	}
	runnerID := input.Runner
	if runnerID == "" {
		runnerID = s.localRunnerID()
	}
	if input.Path == "" {
		writeError(w, http.StatusBadRequest, errors.New("path 必填"))
		return
	}
	// 校验文件夹名称
	name := filepath.Base(input.Path)
	if name == "" || name == "." || name == ".." || strings.HasPrefix(name, ".") ||
		strings.Contains(name, "/") || strings.Contains(name, "\\") || strings.ContainsRune(name, 0) {
		writeError(w, http.StatusBadRequest, errors.New("文件夹名称无效"))
		return
	}
	// SSH runner
	if strings.HasPrefix(runnerID, "ssh-") {
		runner, ok := s.runnerRegistry.get(runnerID)
		if !ok {
			writeError(w, http.StatusBadRequest, errors.New("未找到运行环境"))
			return
		}
		sshR, ok := runner.(*sshRunner)
		if !ok {
			writeError(w, http.StatusBadRequest, errors.New("运行环境不是 SSH 类型"))
			return
		}
		// 校验父目录路径在 SSH root 范围内
		parentPath := pathpkg.Dir(input.Path)
		resolvedParent, err := sshR.canonicalProjectPath(r.Context(), parentPath)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		fullPath := pathpkg.Join(resolvedParent, name)
		sftpClient, err := sshR.client.getSFTPClient(r.Context())
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		// 检查是否已存在
		if info, err := sftpClient.Stat(fullPath); err == nil {
			if info.IsDir() {
				writeError(w, http.StatusConflict, errors.New("目录已存在"))
			} else {
				writeError(w, http.StatusConflict, errors.New("同名文件已存在"))
			}
			return
		}
		if err := sftpClient.Mkdir(fullPath); err != nil {
			// SFTP Mkdir 在目录已存在时可能返回错误，尝试 Stat 区分
			if info, statErr := sftpClient.Stat(fullPath); statErr == nil && info.IsDir() {
				writeError(w, http.StatusConflict, errors.New("目录已存在"))
				return
			}
			writeError(w, http.StatusBadRequest, fmt.Errorf("无法创建远程目录：%w", err))
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"path": fullPath, "name": name})
		return
	}
	// 本地 runner
	parentPath := filepath.Dir(input.Path)
	resolvedParent, err := s.allowedPathForRunner(parentPath, runnerID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	fullPath := filepath.Join(resolvedParent, name)
	// 检查是否已存在
	if info, err := os.Stat(fullPath); err == nil {
		if info.IsDir() {
			writeError(w, http.StatusConflict, errors.New("目录已存在"))
		} else {
			writeError(w, http.StatusConflict, errors.New("同名文件已存在"))
		}
		return
	}
	if err := os.Mkdir(fullPath, 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			writeError(w, http.StatusConflict, errors.New("目录已存在"))
			return
		}
		writeError(w, http.StatusBadRequest, fmt.Errorf("无法创建目录：%w", err))
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"path": fullPath, "name": name})
}

func (s *Server) validateProject(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Path   string `json:"path"`
		Runner string `json:"runner"`
	}
	if !decode(w, r, &input) {
		return
	}
	runnerID := input.Runner
	if runnerID == "" {
		runnerID = s.localRunnerID()
	}
	// For SSH runners, validate via SSH.
	if strings.HasPrefix(runnerID, "ssh-") {
		runner, ok := s.runnerRegistry.get(runnerID)
		if !ok {
			writeError(w, http.StatusBadRequest, errors.New("runner not found"))
			return
		}
		sshR, ok := runner.(*sshRunner)
		if !ok {
			writeError(w, http.StatusBadRequest, errors.New("runner is not an SSH runner"))
			return
		}
		path, err := sshR.canonicalProjectPath(r.Context(), input.Path)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if _, err := sshR.client.readDir(r.Context(), path); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("无法访问远程目录：%w", err))
			return
		}
		branch, gitReady := "", false
		if out, err := sshR.client.execCommand(r.Context(), fmt.Sprintf("cd %s && git rev-parse --abbrev-ref HEAD 2>/dev/null", shellQuote(path))); err == nil {
			branch = strings.TrimSpace(string(out))
			gitReady = branch != ""
		}
		if !gitReady {
			branch = "非 Git 目录"
		}
		claudeReady := sshR.Ready(r.Context())
		codexReady := sshR.CodexReady(r.Context())
		meta, _ := s.runnerRegistry.getMeta(runnerID)
		writeJSON(w, http.StatusOK, map[string]any{
			"path":        path,
			"name":        filepath.Base(path),
			"gitReady":    gitReady,
			"gitBranch":   branch,
			"claudeReady": claudeReady,
			"codexReady":  codexReady,
			"agentReady":  claudeReady || codexReady,
			"performance": "remote",
			"runnerName":  meta.Name,
		})
		return
	}
	path, err := s.allowedPathForRunner(input.Path, runnerID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		writeError(w, http.StatusBadRequest, errors.New("directory does not exist or is not accessible"))
		return
	}
	branch, gitReady := gitBranch(r.Context(), path)
	claudeReady := s.runner.Ready(r.Context())
	codexReady := s.codexRunner.Ready(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"path": path, "name": filepath.Base(path), "gitReady": gitReady, "gitBranch": branch, "claudeReady": claudeReady, "codexReady": codexReady, "agentReady": claudeReady || codexReady})
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Path   string `json:"path"`
		Name   string `json:"name"`
		Runner string `json:"runner"`
	}
	if !decode(w, r, &input) {
		return
	}
	runnerID := input.Runner
	if runnerID == "" {
		runnerID = s.localRunnerID()
	}
	// For SSH runners, skip local path validation.
	if strings.HasPrefix(runnerID, "ssh-") {
		// Keep the runner registered until the project row is committed. This
		// shares the lock used by SSH connection deletion and replacement.
		s.projectLifecycleMu.Lock()
		defer s.projectLifecycleMu.Unlock()
		projectRunner, ok := s.runnerRegistry.get(runnerID)
		if !ok {
			writeError(w, http.StatusBadRequest, errors.New("runner not found"))
			return
		}
		sshR, ok := projectRunner.(*sshRunner)
		if !ok {
			writeError(w, http.StatusBadRequest, errors.New("runner is not an SSH runner"))
			return
		}
		path, err := sshR.canonicalProjectPath(r.Context(), input.Path)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if !sshR.Ready(r.Context()) {
			writeError(w, http.StatusBadRequest, errors.New("远程服务器上 Claude Code 不可用"))
			return
		}
		branch, gitReady := "", false
		if out, err := sshR.client.execCommand(r.Context(), fmt.Sprintf("cd %s && git rev-parse --abbrev-ref HEAD 2>/dev/null", shellQuote(path))); err == nil {
			branch = strings.TrimSpace(string(out))
			gitReady = branch != ""
		}
		if !gitReady {
			branch = "非 Git 目录"
		}
		name := strings.TrimSpace(input.Name)
		if name == "" {
			name = filepath.Base(path)
		}
		meta, _ := s.runnerRegistry.getMeta(runnerID)
		codexReady := sshR.CodexReady(r.Context())
		p := Project{
			ID:          uuid.NewString(),
			Name:        name,
			Path:        path,
			PathDisplay: meta.Name + ":" + path,
			Runner:      runnerID,
			RunnerID:    runnerID,
			GitBranch:   branch,
			ClaudeReady: true,
			CodexReady:  codexReady,
			AgentReady:  true, // Claude Code is confirmed ready above
			CreatedAt:   time.Now().UTC(),
		}
		s.decorateProjectPresentation(&p)
		_, err = s.db.ExecContext(r.Context(), `insert into projects (id,name,path,runner,runner_id,git_branch,claude_ready,created_at) values ($1,$2,$3,$4,$5,$6,$7,$8)`, p.ID, p.Name, p.Path, p.Runner, p.RunnerID, p.GitBranch, p.ClaudeReady, p.CreatedAt)
		if err != nil {
			var existing Project
			lookupErr := s.db.QueryRowContext(r.Context(), `select id,name,path,runner,git_branch,claude_ready,created_at from projects where path=$1 and runner=$2`, path, runnerID).Scan(&existing.ID, &existing.Name, &existing.Path, &existing.Runner, &existing.GitBranch, &existing.ClaudeReady, &existing.CreatedAt)
			if lookupErr == nil {
				existing.RunnerID = existing.Runner
				s.decorateProjectPresentation(&existing)
				s.decorateProjectAvailability(&existing, s.codexRunner.Ready(r.Context()))
				writeJSON(w, http.StatusOK, existing)
				return
			}
			writeError(w, http.StatusConflict, fmt.Errorf("create project: %w", err))
			return
		}
		writeJSON(w, http.StatusCreated, p)
		return
	}
	path, err := s.allowedPathForRunner(input.Path, runnerID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	branch, gitReady := gitBranch(r.Context(), path)
	claudeReady := s.runner.Ready(r.Context())
	codexReady := s.codexRunner.Ready(r.Context())
	if !claudeReady && !codexReady {
		writeError(w, http.StatusBadRequest, errors.New("Claude Code and Codex CLI are unavailable in this Runner"))
		return
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		name = filepath.Base(path)
	}
	if !gitReady {
		branch = "非 Git 目录"
	}
	p := Project{ID: uuid.NewString(), Name: name, Path: path, PathDisplay: filepath.Base(path), Runner: runnerID, RunnerID: runnerID, GitBranch: branch, ClaudeReady: claudeReady, CodexReady: codexReady, AgentReady: claudeReady || codexReady, CreatedAt: time.Now().UTC()}
	s.decorateProjectPresentation(&p)
	_, err = s.db.ExecContext(r.Context(), `insert into projects (id,name,path,runner,runner_id,git_branch,claude_ready,created_at) values ($1,$2,$3,$4,$5,$6,$7,$8)`, p.ID, p.Name, p.Path, p.Runner, p.RunnerID, p.GitBranch, p.ClaudeReady, p.CreatedAt)
	if err != nil {
		var existing Project
		lookupErr := s.db.QueryRowContext(r.Context(), `select id,name,path,runner,git_branch,claude_ready,created_at from projects where path=$1 and runner=$2`, path, runnerID).Scan(&existing.ID, &existing.Name, &existing.Path, &existing.Runner, &existing.GitBranch, &existing.ClaudeReady, &existing.CreatedAt)
		if lookupErr == nil {
			existing.RunnerID = existing.Runner
			s.decorateProjectPresentation(&existing)
			s.decorateProjectAvailability(&existing, s.codexRunner.Ready(r.Context()))
			writeJSON(w, http.StatusOK, existing)
			return
		}
		writeError(w, http.StatusConflict, fmt.Errorf("create project: %w", err))
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	if projectID == "" {
		writeError(w, http.StatusBadRequest, errors.New("project ID is required"))
		return
	}
	// Perform the destructive step (signal agent sessions, delete the project row
	// and its cascaded records, drop the workspace lease and managed-process
	// runner). deleteProjectLocked serializes it with message admission under
	// projectLifecycleMu so no new agent process can be started for the project
	// after it is gone; the lock is held only for this fast section, and final
	// process teardown runs in the background below.
	runner, conversationIDs, status, err := s.deleteProjectLocked(s.runtimeCtx, projectID)
	if status != http.StatusNoContent {
		writeError(w, status, err)
		return
	}

	// Background teardown: from this point the project row and its cascaded
	// conversations/runs are gone, so any new request to start a run is rejected
	// by projectRunManagerForExistingProject's existence check. Retire the
	// detached managed-process runner and wait for the signalled agent runs to
	// actually settle so a straggler cannot linger beyond the project's lifetime.
	// Both are tracked in runWG so Close() does not return until the project's
	// processes have actually been collected.
	if runner != nil {
		s.runWG.Add(1)
		go func(runner projectRunnerInterface) {
			defer s.runWG.Done()
			_ = runner.Retire()
		}(runner)
	}
	if len(conversationIDs) > 0 {
		s.runWG.Add(1)
		go func() {
			defer s.runWG.Done()
			s.waitProjectAgentsStop(s.runtimeCtx, conversationIDs)
		}()
	}

	w.WriteHeader(http.StatusNoContent)
}

// deleteProjectLocked cancels and stops the project's agent sessions, then
// deletes the project row and its cascade, finally dropping the workspace lease
// and the managed-process runner. It acquires and releases projectLifecycleMu
// itself (via defer), serializing with message admission so no new agent process
// can be started for the project after it is gone. A future edit that adds an
// early return cannot accidentally skip the unlock and deadlock the server. It
// returns the released managed-process runner (nil if none), the set of
// conversation IDs that were active (empty if none), an HTTP status code
// (http.StatusNoContent on success), and the error to report when the status is
// not StatusNoContent.
func (s *Server) deleteProjectLocked(ctx context.Context, projectID string) (projectRunnerInterface, map[string]struct{}, int, error) {
	s.projectLifecycleMu.Lock()
	defer s.projectLifecycleMu.Unlock()

	// Signal phase: cancel + stop every agent session for this project. These
	// calls do not wait for the processes to actually exit (they are fast),
	// which keeps the lock short and deletion responsive even when a project
	// has a long-running task. Final process teardown happens in the background.
	// Deleting is a user's explicit, idempotent intent rooted in the runtime
	// context, so a client disconnect mid-request cannot leave a half-deleted
	// project (processes stopped but the row still present).
	conversationIDs, err := s.signalProjectStop(ctx, projectID)
	if err != nil {
		return nil, nil, http.StatusConflict, fmt.Errorf("stop project agents: %w", err)
	}
	// Foreign key cascades (on delete cascade) will clean up:
	//   conversations -> messages, runs, events, run_usage, run_model_usage
	//   shortcut_projects (via project_id), shortcut_runs (via conversation_id)
	//   tasks, task_runs, task_events, task_dependencies
	tx, txErr := s.db.BeginTx(ctx, nil)
	if txErr != nil {
		return nil, nil, http.StatusInternalServerError, txErr
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `delete from projects where id=$1`, projectID)
	if err != nil {
		return nil, nil, http.StatusInternalServerError, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return nil, nil, http.StatusInternalServerError, err
	}
	if changed != 1 {
		return nil, nil, http.StatusNotFound, errors.New("project not found")
	}
	if err = tx.Commit(); err != nil {
		return nil, nil, http.StatusInternalServerError, err
	}
	// Release the workspace lease only after a successful commit.
	s.mu.Lock()
	delete(s.projectWorkspaceLeases, projectID)
	s.mu.Unlock()
	s.runManagersMu.Lock()
	runner := s.runManagers[projectID]
	delete(s.runManagers, projectID)
	s.runManagersMu.Unlock()
	s.closeProjectRunLogSubscribers(projectID)
	return runner, conversationIDs, http.StatusNoContent, nil
}

// signalProjectStop collects the project's agent sessions and issues
// cancellation / stop signals for them. All operations are non-blocking; this
// marks sessions as stopping and requests their contexts be cancelled, but does
// not wait for the underlying processes to reap. It returns the set of
// conversation IDs that were active so the caller can wait for them to settle
// in the background. The caller must hold projectLifecycleMu.
func (s *Server) signalProjectStop(ctx context.Context, projectID string) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, `select id from conversations where project_id=?`, projectID)
	if err != nil {
		return nil, err
	}
	conversationIDs := map[string]struct{}{}
	for rows.Next() {
		var conversationID string
		if err := rows.Scan(&conversationID); err != nil {
			rows.Close()
			return nil, err
		}
		conversationIDs[conversationID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	cancels := []context.CancelFunc{}
	sessions := []AgentSession{}
	runIDs := map[string]struct{}{}
	s.mu.Lock()
	for runID, conversationID := range s.runContexts {
		if _, ok := conversationIDs[conversationID]; ok {
			runIDs[runID] = struct{}{}
			if cancel := s.cancels[runID]; cancel != nil {
				cancels = append(cancels, cancel)
			}
		}
	}
	for conversationID, session := range s.sessions {
		if _, ok := conversationIDs[conversationID]; ok {
			session.stopping = true
			sessions = append(sessions, session.agent)
			if session.activeRunID != "" {
				runIDs[session.activeRunID] = struct{}{}
			}
		}
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	for _, session := range sessions {
		session.Stop()
	}
	for runID := range runIDs {
		s.resolveRunApprovals(runID, "deny")
	}
	return conversationIDs, nil
}

// waitProjectAgentsStop blocks until the in-memory runs and sessions that were
// signalled for deletion have fully settled. It is intended to run in a
// background goroutine (rooted at the service runtime context) so project
// deletion is not blocked by a slow-to-stop agent. The 15s cap bounds how long
// a straggler run may linger beyond the project's deletion. Note: the durable
// run rows are gone the moment the project row is cascade-deleted, so only the
// in-memory maps (runContexts / sessions) matter here.
func (s *Server) waitProjectAgentsStop(ctx context.Context, conversationIDs map[string]struct{}) {
	waitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		s.mu.Lock()
		active := false
		for _, conversationID := range s.runContexts {
			if _, ok := conversationIDs[conversationID]; ok {
				active = true
				break
			}
		}
		if !active {
			for conversationID := range s.sessions {
				if _, ok := conversationIDs[conversationID]; ok {
					active = true
					break
				}
			}
		}
		s.mu.Unlock()
		if !active {
			return
		}
		select {
		case <-waitCtx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(), `select id,name,path,coalesce(nullif(runner_id,''),runner),git_branch,claude_ready,created_at from projects order by created_at desc`)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	defer rows.Close()
	projects := []Project{}
	localCodexReady := s.codexRunner.Ready(r.Context())
	// Cache remote Codex readiness per SSH runner so we don't issue redundant
	// remote checks when many projects share the same connection.
	sshCodexCache := map[string]bool{}
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name, &p.Path, &p.Runner, &p.GitBranch, &p.ClaudeReady, &p.CreatedAt); err != nil {
			writeError(w, 500, err)
			return
		}
		p.RunnerID = p.Runner
		s.decorateProjectPresentation(&p)
		if isLocalRunnerID(p.Runner) {
			s.decorateProjectAvailability(&p, localCodexReady)
		} else if strings.HasPrefix(p.Runner, "ssh-") {
			if cached, ok := sshCodexCache[p.Runner]; ok {
				p.CodexReady = cached
				p.AgentReady = p.ClaudeReady || p.CodexReady
			} else {
				s.decorateProjectAvailability(&p, localCodexReady)
				sshCodexCache[p.Runner] = p.CodexReady
			}
		} else {
			s.decorateProjectAvailability(&p, localCodexReady)
		}
		projects = append(projects, p)
	}
	if err := rows.Err(); err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, 200, projects)
}

// getProject returns a single project by ID. Unlike listProjects it performs no
// remote Codex/Claude readiness probing, so it returns immediately even when an
// SSH runner is slow or unreachable. Callers that need the current readiness
// state should use listProjects or the statuses endpoint instead.
func (s *Server) getProject(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	if projectID == "" {
		writeError(w, http.StatusBadRequest, errors.New("project ID is required"))
		return
	}
	p, err := s.getProjectByID(r.Context(), projectID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("project not found"))
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.decorateProjectPresentation(&p)
	// agentReady falls back to the persisted claude_ready flag; remote codex
	// readiness is intentionally not probed here.
	p.AgentReady = p.ClaudeReady
	writeJSON(w, 200, p)
}

// setProjectDefaultProfile sets (or clears, with profileId "") the agent profile
// applied to new conversations in a project. This is the "一键应用到项目" entry
// point: set it once and every subsequent conversation for that project uses the
// chosen profile and its managed credentials.
func (s *Server) setProjectDefaultProfile(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	var input struct {
		ProfileID string `json:"profileId"`
	}
	if !decode(w, r, &input) {
		return
	}
	profileID := strings.TrimSpace(input.ProfileID)
	var projectRunner string
	if err := s.db.QueryRowContext(r.Context(), `select coalesce(nullif(runner_id,''),runner) from projects where id=?`, projectID).Scan(&projectRunner); errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("project not found"))
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if profileID != "" {
		tx, err := s.db.BeginTx(r.Context(), nil)
		if err != nil {
			writeError(w, 500, err)
			return
		}
		defer tx.Rollback()
		var state, agentID string
		err = tx.QueryRowContext(r.Context(), `select p2.agent_id,r.state from agent_profiles p2 join agent_profile_revisions r on r.id=p2.current_revision_id where p2.id=? and p2.runner_id=? and p2.enabled=1`, profileID, projectRunner).Scan(&agentID, &state)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && state != "active") {
			writeError(w, http.StatusConflict, errors.New("agent profile is unavailable for this project runner"))
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		// The tx holds the only SQLite connection; the update must run on the
		// same tx or the subsequent db call would deadlock.
		if _, err = tx.ExecContext(r.Context(), `update projects set default_profile_id=? where id=?`, profileID, projectID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if err = tx.Commit(); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"profileId": profileID})
		return
	}
	if _, err := s.db.ExecContext(r.Context(), `update projects set default_profile_id=? where id=?`, profileID, projectID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"profileId": profileID})
}

func (s *Server) decorateProjectAvailability(project *Project, localCodexReady bool) {
	switch {
	case isLocalRunnerID(project.Runner):
		project.CodexReady = localCodexReady
	case strings.HasPrefix(project.Runner, "ssh-"):
		project.CodexReady = s.sshCodexReady(project.Runner)
	default:
		project.CodexReady = false
	}
	project.AgentReady = project.ClaudeReady || project.CodexReady
}

// sshCodexReady reports whether Codex is available on the remote host behind the
// given SSH runner. It returns false when the runner is disconnected or the
// remote Codex CLI is not installed/logged in.
func (s *Server) sshCodexReady(runnerID string) bool {
	r, ok := s.runnerRegistry.get(runnerID)
	if !ok {
		return false
	}
	codexR, ok := r.(CodexCapableRunner)
	if !ok {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return codexR.CodexReady(ctx)
}

func (s *Server) decorateProjectPresentation(project *Project) {
	project.FullPath = project.Path
	project.PathDisplay = filepath.Base(project.Path)
	project.Environment = "wsl"
	if strings.HasPrefix(project.Path, "/mnt/") {
		project.Environment = "windows"
	}
	if !strings.HasPrefix(project.Runner, "ssh-") {
		return
	}
	project.Environment = "remote-linux"
	project.PathDisplay = project.Path
	if meta, ok := s.runnerRegistry.getMeta(project.Runner); ok {
		// 远程项目用 服务器名:路径 作为完整路径，标识它所在的主机
		project.FullPath = meta.Name + ":" + project.Path
		project.PathDisplay = meta.Name + ":" + project.Path
	}
}

type projectStatusItem struct {
	ID                string `json:"id"`
	Running           int    `json:"running"`
	ConversationCount int    `json:"conversationCount"`
	ActiveTitle       string `json:"activeTitle"`
}

func (s *Server) listProjectStatuses(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(), `
		select
			p.id,
			case when exists(select 1 from conversations c where c.project_id = p.id and c.is_current = 1 and c.status = 'running') then 1 else 0 end,
			(select count(*) from conversations c where c.project_id = p.id),
			coalesce((select c.title from conversations c where c.project_id = p.id and c.is_current = 1 limit 1), '')
		from projects p
		order by p.created_at desc`)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	defer rows.Close()
	var items []projectStatusItem
	for rows.Next() {
		var item projectStatusItem
		if err := rows.Scan(&item.ID, &item.Running, &item.ConversationCount, &item.ActiveTitle); err != nil {
			writeError(w, 500, err)
			return
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		writeError(w, 500, err)
		return
	}
	if items == nil {
		items = []projectStatusItem{}
	}
	writeJSON(w, 200, items)
}

func (s *Server) createConversation(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	newSession := r.URL.Query().Get("new") == "true"
	var input struct {
		PermissionMode string  `json:"permissionMode"`
		AgentID        string  `json:"agentId"`
		ProfileID      *string `json:"profileId"`
	}
	if !decodeOptional(w, r, &input) {
		return
	}
	if input.AgentID == "" {
		input.AgentID = "claude-code"
	}
	if input.PermissionMode == "" {
		if input.AgentID == "codex" {
			input.PermissionMode = "workspace_write"
		} else {
			input.PermissionMode = "approval_required"
		}
	}
	if !validAgentPolicy(input.AgentID, input.PermissionMode) {
		writeError(w, http.StatusBadRequest, errors.New("unsupported agent or execution policy"))
		return
	}
	var projectRunner string
	if err := s.db.QueryRowContext(r.Context(), `select coalesce(nullif(runner_id,''),runner) from projects where id=?`, projectID).Scan(&projectRunner); errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("project not found"))
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// New conversations replace the project's current conversation. Keep that
	// transition ordered with message admission and context clearing.
	s.projectLifecycleMu.Lock()
	defer s.projectLifecycleMu.Unlock()
	now := time.Now().UTC()
	sessionID := uuid.NewString()
	c := Conversation{ID: uuid.NewString(), ProjectID: projectID, ClaudeSessionID: sessionID, AgentID: input.AgentID, AgentSessionID: sessionID, AgentRuntimeID: projectRunner, ExecutionPolicy: input.PermissionMode, Status: "idle", PermissionMode: input.PermissionMode, Title: "新会话", LastActivityAt: now, IsCurrent: true, CreatedAt: now}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	defer tx.Rollback()
	profileRevisionID, err := s.profileForNewConversationTx(r.Context(), tx, input.ProfileID, projectRunner, input.AgentID, projectID)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	c.AgentProfileRevisionID = profileRevisionID
	if input.AgentID == "codex" {
		if strings.HasPrefix(projectRunner, "ssh-") {
			runner, ok := s.runnerRegistry.get(projectRunner)
			if !ok {
				writeError(w, http.StatusServiceUnavailable, fmt.Errorf("SSH 连接不可用，请重新连接后再试"))
				return
			}
			codexR, ok := runner.(CodexCapableRunner)
			if !ok || !codexR.CodexReady(r.Context()) {
				writeError(w, http.StatusServiceUnavailable, errors.New("远程服务器上 Codex CLI 不可用或未登录"))
				return
			}
		} else {
			profile, profileErr := s.runtimeProfileTx(r.Context(), tx, profileRevisionID, projectRunner, input.AgentID)
			if profileErr != nil {
				writeError(w, http.StatusConflict, profileErr)
				return
			}
			if !s.codexUsableForProfile(r.Context(), profile) {
				writeError(w, http.StatusServiceUnavailable, errors.New("Codex CLI is unavailable or not logged in"))
				return
			}
		}
	}
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
		err = tx.QueryRowContext(r.Context(), `select id,project_id,claude_session_id,agent_id,agent_session_id,agent_runtime_id,agent_profile_revision_id,execution_policy,status,permission_mode,title,last_activity_at,claude_initialized,agent_initialized,is_current,created_at from conversations where project_id=$1 and is_current=1`, projectID).Scan(&existing.ID, &existing.ProjectID, &existing.ClaudeSessionID, &existing.AgentID, &existing.AgentSessionID, &existing.AgentRuntimeID, &existing.AgentProfileRevisionID, &existing.ExecutionPolicy, &existing.Status, &existing.PermissionMode, &existing.Title, &existing.LastActivityAt, &existing.ClaudeInitialized, &existing.AgentInitialized, &existing.IsCurrent, &existing.CreatedAt)
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
	if _, err = tx.ExecContext(r.Context(), `insert into conversations (id,project_id,claude_session_id,agent_id,agent_session_id,agent_runtime_id,agent_profile_revision_id,execution_policy,status,permission_mode,title,last_activity_at,claude_initialized,agent_initialized,is_current,created_at) values (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, c.ID, c.ProjectID, c.ClaudeSessionID, c.AgentID, c.AgentSessionID, c.AgentRuntimeID, c.AgentProfileRevisionID, c.ExecutionPolicy, c.Status, c.PermissionMode, c.Title, c.LastActivityAt, c.ClaudeInitialized, c.AgentInitialized, c.IsCurrent, c.CreatedAt); err != nil {
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
	limit := 100
	if rawLimit := r.URL.Query().Get("limit"); rawLimit != "" {
		parsed, err := strconv.Atoi(rawLimit)
		if err != nil || parsed < 1 || parsed > 100 {
			writeError(w, http.StatusBadRequest, errors.New("limit must be between 1 and 100"))
			return
		}
		limit = parsed
	}
	offset := 0
	if rawOffset := r.URL.Query().Get("offset"); rawOffset != "" {
		parsed, err := strconv.Atoi(rawOffset)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, errors.New("offset must be a non-negative integer"))
			return
		}
		offset = parsed
	}
	if offset != 0 {
		writeError(w, http.StatusBadRequest, errors.New("offset pagination is no longer supported; use cursor"))
		return
	}
	cursor, err := decodeConversationListCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	statement := `select c.id,c.project_id,c.claude_session_id,c.agent_id,c.agent_session_id,c.agent_runtime_id,c.agent_profile_revision_id,c.execution_policy,c.status,c.permission_mode,c.title,c.last_activity_at,c.claude_initialized,c.agent_initialized,c.is_current,c.created_at,coalesce((select content from messages m where m.conversation_id=c.id and m.parent_tool_use_id='' order by m.created_at desc limit 1),'') from conversations c where c.project_id=? and (?='' or lower(c.title) like '%' || lower(?) || '%' or exists(select 1 from messages m where m.conversation_id=c.id and m.parent_tool_use_id='' and lower(m.content) like '%' || lower(?) || '%'))`
	args := []any{chi.URLParam(r, "projectID"), query, query, query}
	if cursor.ID != "" {
		statement += ` and (c.is_current < ? or (c.is_current = ? and (c.last_activity_at < ? or (c.last_activity_at = ? and c.id < ?))))`
		args = append(args, boolToInt(cursor.IsCurrent), boolToInt(cursor.IsCurrent), cursor.LastActivityAt, cursor.LastActivityAt, cursor.ID)
	}
	statement += ` order by c.is_current desc,c.last_activity_at desc,c.id desc limit ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(r.Context(), statement, args...)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	defer rows.Close()
	items := []Conversation{}
	for rows.Next() {
		var c Conversation
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.ClaudeSessionID, &c.AgentID, &c.AgentSessionID, &c.AgentRuntimeID, &c.AgentProfileRevisionID, &c.ExecutionPolicy, &c.Status, &c.PermissionMode, &c.Title, &c.LastActivityAt, &c.ClaudeInitialized, &c.AgentInitialized, &c.IsCurrent, &c.CreatedAt, &c.Preview); err != nil {
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
	nextCursor := ""
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		nextCursor = encodeConversationListCursor(conversationListCursor{IsCurrent: last.IsCurrent, LastActivityAt: last.LastActivityAt, ID: last.ID})
	}
	writeJSON(w, http.StatusOK, conversationListPage{Items: items, NextCursor: nextCursor})
}

func (s *Server) clearConversation(w http.ResponseWriter, r *http.Request) {
	conversationID := chi.URLParam(r, "conversationID")
	s.projectLifecycleMu.Lock()
	locked := true
	defer func() {
		if locked {
			s.projectLifecycleMu.Unlock()
		}
	}()

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	var previous Conversation
	var projectRunner string
	err = tx.QueryRowContext(r.Context(), `select c.id,c.project_id,c.claude_session_id,c.agent_id,c.agent_session_id,c.agent_runtime_id,c.agent_profile_revision_id,c.execution_policy,c.status,c.permission_mode,c.title,c.last_activity_at,c.claude_initialized,c.agent_initialized,c.is_current,c.created_at,coalesce(nullif(p.runner_id,''),p.runner) from conversations c join projects p on p.id=c.project_id where c.id=?`, conversationID).Scan(&previous.ID, &previous.ProjectID, &previous.ClaudeSessionID, &previous.AgentID, &previous.AgentSessionID, &previous.AgentRuntimeID, &previous.AgentProfileRevisionID, &previous.ExecutionPolicy, &previous.Status, &previous.PermissionMode, &previous.Title, &previous.LastActivityAt, &previous.ClaudeInitialized, &previous.AgentInitialized, &previous.IsCurrent, &previous.CreatedAt, &projectRunner)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("conversation not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !previous.IsCurrent {
		writeError(w, http.StatusConflict, errors.New("only the current conversation can be cleared"))
		return
	}
	if previous.Status != "idle" {
		writeError(w, http.StatusConflict, errors.New("stop the active agent run before clearing the conversation"))
		return
	}

	now := time.Now().UTC()
	sessionID := uuid.NewString()
	fresh := Conversation{ID: uuid.NewString(), ProjectID: previous.ProjectID, ClaudeSessionID: sessionID, AgentID: previous.AgentID, AgentSessionID: sessionID, AgentRuntimeID: projectRunner, AgentProfileRevisionID: previous.AgentProfileRevisionID, ExecutionPolicy: previous.executionPolicy(), Status: "idle", PermissionMode: previous.PermissionMode, Title: "新会话", LastActivityAt: now, IsCurrent: true, CreatedAt: now}
	result, err := tx.ExecContext(r.Context(), `update conversations set is_current=0 where id=? and is_current=1 and status='idle'`, previous.ID)
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
		writeError(w, http.StatusConflict, errors.New("conversation changed while clearing"))
		return
	}
	if _, err = tx.ExecContext(r.Context(), `insert into conversations (id,project_id,claude_session_id,agent_id,agent_session_id,agent_runtime_id,agent_profile_revision_id,execution_policy,status,permission_mode,title,last_activity_at,claude_initialized,agent_initialized,is_current,created_at) values (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, fresh.ID, fresh.ProjectID, fresh.ClaudeSessionID, fresh.AgentID, fresh.AgentSessionID, fresh.AgentRuntimeID, fresh.AgentProfileRevisionID, fresh.ExecutionPolicy, fresh.Status, fresh.PermissionMode, fresh.Title, fresh.LastActivityAt, fresh.ClaudeInitialized, fresh.AgentInitialized, fresh.IsCurrent, fresh.CreatedAt); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err = tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// Mark the old session before another lifecycle operation can reactivate the
	// old conversation. Stopping the underlying local or remote process happens
	// after releasing the global lock because it may block.
	previousSession := s.markConversationSessionStopping(previous.ID)
	s.projectLifecycleMu.Unlock()
	locked = false
	if previousSession != nil {
		previousSession.Stop()
	}
	writeJSON(w, http.StatusCreated, fresh)
}

// markConversationSessionStopping prevents a new streaming turn from joining
// the existing session. Callers must hold projectLifecycleMu before invoking
// it so the conversation cannot be reactivated between clearing and marking.
func (s *Server) markConversationSessionStopping(conversationID string) AgentSession {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.sessions[conversationID]
	if session == nil || session.stopping {
		return nil
	}
	session.stopping = true
	return session.agent
}

func (s *Server) getConversation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "conversationID")
	limit := 400
	if rawLimit := r.URL.Query().Get("limit"); rawLimit != "" {
		parsed, err := strconv.Atoi(rawLimit)
		if err != nil || parsed < 1 || parsed > 1000 {
			writeError(w, http.StatusBadRequest, errors.New("limit must be between 1 and 1000"))
			return
		}
		limit = parsed
	}
	cursor, err := decodeConversationPageCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var c Conversation
	err = s.db.QueryRowContext(r.Context(), `select id,project_id,claude_session_id,agent_id,agent_session_id,agent_runtime_id,agent_profile_revision_id,execution_policy,status,permission_mode,title,last_activity_at,claude_initialized,agent_initialized,is_current,created_at from conversations where id=$1`, id).Scan(&c.ID, &c.ProjectID, &c.ClaudeSessionID, &c.AgentID, &c.AgentSessionID, &c.AgentRuntimeID, &c.AgentProfileRevisionID, &c.ExecutionPolicy, &c.Status, &c.PermissionMode, &c.Title, &c.LastActivityAt, &c.ClaudeInitialized, &c.AgentInitialized, &c.IsCurrent, &c.CreatedAt)
	if err != nil {
		writeError(w, 404, errors.New("conversation not found"))
		return
	}
	messageQuery := `select id,conversation_id,run_id,role,content,parent_tool_use_id,created_at from messages where conversation_id=?`
	messageArgs := []any{id}
	if cursor.Message != nil {
		messageQuery += ` and (created_at < ? or (created_at = ? and id < ?))`
		messageArgs = append(messageArgs, cursor.Message.CreatedAt, cursor.Message.CreatedAt, cursor.Message.ID)
	}
	messageQuery += ` order by created_at desc,id desc limit ?`
	messageArgs = append(messageArgs, limit+1)
	rows, err := s.db.QueryContext(r.Context(), messageQuery, messageArgs...)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	defer rows.Close()
	messages := []Message{}
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.RunID, &m.Role, &m.Content, &m.ParentToolUseID, &m.CreatedAt); err != nil {
			writeError(w, 500, err)
			return
		}
		messages = append(messages, m)
	}
	if err := rows.Err(); err != nil {
		writeError(w, 500, err)
		return
	}
	hasMoreMessages := len(messages) > limit
	if hasMoreMessages {
		messages = messages[:limit]
	}
	for left, right := 0, len(messages)-1; left < right; left, right = left+1, right-1 {
		messages[left], messages[right] = messages[right], messages[left]
	}
	eventQuery := `select id,conversation_id,run_id,type,payload,created_at from events where conversation_id=?`
	eventArgs := []any{id}
	if cursor.Event != nil {
		eventQuery += ` and (created_at < ? or (created_at = ? and id < ?))`
		eventArgs = append(eventArgs, cursor.Event.CreatedAt, cursor.Event.CreatedAt, cursor.Event.ID)
	}
	eventQuery += ` order by created_at desc,id desc limit ?`
	eventArgs = append(eventArgs, limit+1)
	erows, err := s.db.QueryContext(r.Context(), eventQuery, eventArgs...)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	defer erows.Close()
	events := []Event{}
	for erows.Next() {
		var e Event
		var payload string
		if err := erows.Scan(&e.ID, &e.ConversationID, &e.RunID, &e.Type, &payload, &e.CreatedAt); err != nil {
			writeError(w, 500, err)
			return
		}
		e.Payload = json.RawMessage(payload)
		events = append(events, e)
	}
	if err := erows.Err(); err != nil {
		writeError(w, 500, err)
		return
	}
	hasMoreEvents := len(events) > limit
	if hasMoreEvents {
		events = events[:limit]
	}
	for left, right := 0, len(events)-1; left < right; left, right = left+1, right-1 {
		events[left], events[right] = events[right], events[left]
	}
	nextCursor := conversationPageCursor{}
	if len(messages) > 0 {
		oldest := messages[0]
		nextCursor.Message = &conversationPagePosition{CreatedAt: oldest.CreatedAt, ID: oldest.ID}
	}
	if len(events) > 0 {
		oldest := events[0]
		nextCursor.Event = &conversationPagePosition{CreatedAt: oldest.CreatedAt, ID: oldest.ID}
	}
	var activeRunID *string
	if c.Status == "running" {
		var runID string
		if err := s.db.QueryRowContext(r.Context(), `select id from runs where conversation_id=$1 and status in ('queued','running') order by created_at desc limit 1`, id).Scan(&runID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			writeError(w, 500, err)
			return
		} else if err == nil {
			activeRunID = &runID
		}
	}
	hasMore := hasMoreMessages || hasMoreEvents
	if !hasMore {
		nextCursor = conversationPageCursor{}
	}
	writeJSON(w, 200, map[string]any{"conversation": c, "activeRunId": activeRunID, "messages": messages, "events": events, "hasMore": hasMore, "hasMoreMessages": hasMoreMessages, "nextCursor": encodeConversationPageCursor(nextCursor)})
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

// listProjectInputHistory 返回项目级别的用户输入历史（最近 limit 条），
// 连续重复内容会被压缩为一条。与对话级别的历史不同，它在新建/清空对话后仍然保留。
func (s *Server) listProjectInputHistory(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
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
	rows, err := s.db.QueryContext(r.Context(), `select messages.content from messages join conversations on conversations.id = messages.conversation_id where conversations.project_id = ? and messages.role = 'user' order by messages.created_at desc limit ?`, projectID, limit)
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
	// 反转为按时间正序（旧 -> 新）
	for left, right := 0, len(history)-1; left < right; left, right = left+1, right-1 {
		history[left], history[right] = history[right], history[left]
	}
	// 压缩连续重复项：连续多次输入相同内容只保留一条
	deduped := make([]string, 0, len(history))
	for _, content := range history {
		if len(deduped) > 0 && deduped[len(deduped)-1] == content {
			continue
		}
		deduped = append(deduped, content)
	}
	writeJSON(w, http.StatusOK, deduped)
}

func (s *Server) activateConversation(w http.ResponseWriter, r *http.Request) {
	conversationID := chi.URLParam(r, "conversationID")
	// Activation changes the same current-conversation pointer as clear.
	s.projectLifecycleMu.Lock()
	defer s.projectLifecycleMu.Unlock()
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	var conversation Conversation
	err = tx.QueryRowContext(r.Context(), `select id,project_id,claude_session_id,agent_id,agent_session_id,agent_runtime_id,agent_profile_revision_id,execution_policy,status,permission_mode,title,last_activity_at,claude_initialized,agent_initialized,is_current,created_at from conversations where id=?`, conversationID).Scan(&conversation.ID, &conversation.ProjectID, &conversation.ClaudeSessionID, &conversation.AgentID, &conversation.AgentSessionID, &conversation.AgentRuntimeID, &conversation.AgentProfileRevisionID, &conversation.ExecutionPolicy, &conversation.Status, &conversation.PermissionMode, &conversation.Title, &conversation.LastActivityAt, &conversation.ClaudeInitialized, &conversation.AgentInitialized, &conversation.IsCurrent, &conversation.CreatedAt)
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
	// Keep permission changes ordered with conversation clearing and activation.
	s.projectLifecycleMu.Lock()
	defer s.projectLifecycleMu.Unlock()
	conversationID := chi.URLParam(r, "conversationID")
	var conversation Conversation
	err := s.db.QueryRowContext(r.Context(), `select id,project_id,claude_session_id,agent_id,agent_session_id,agent_runtime_id,agent_profile_revision_id,execution_policy,status,permission_mode,title,last_activity_at,claude_initialized,agent_initialized,is_current,created_at from conversations where id=$1`, conversationID).Scan(&conversation.ID, &conversation.ProjectID, &conversation.ClaudeSessionID, &conversation.AgentID, &conversation.AgentSessionID, &conversation.AgentRuntimeID, &conversation.AgentProfileRevisionID, &conversation.ExecutionPolicy, &conversation.Status, &conversation.PermissionMode, &conversation.Title, &conversation.LastActivityAt, &conversation.ClaudeInitialized, &conversation.AgentInitialized, &conversation.IsCurrent, &conversation.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("conversation not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !validAgentPolicy(conversation.AgentID, input.PermissionMode) {
		writeError(w, http.StatusBadRequest, errors.New("unsupported execution policy for this agent"))
		return
	}
	if conversation.Status != "idle" {
		writeError(w, http.StatusConflict, errors.New("stop the active agent run before changing permission mode"))
		return
	}
	if !conversation.IsCurrent {
		writeError(w, http.StatusConflict, errors.New("activate this conversation before changing permission mode"))
		return
	}
	result, err := s.db.ExecContext(r.Context(), `update conversations set permission_mode=?,execution_policy=? where id=? and status='idle' and is_current=1`, input.PermissionMode, input.PermissionMode, conversationID)
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
		writeError(w, http.StatusConflict, errors.New("stop the active agent run before changing permission mode"))
		return
	}
	conversation.PermissionMode = input.PermissionMode
	conversation.ExecutionPolicy = input.PermissionMode
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
	query += ` order by s.sort_order,s.name`
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

type reorderShortcutsInput struct {
	Kind       string   `json:"kind"`
	OrderedIDs []string `json:"orderedIds"`
}

// reorderShortcuts 以提交的完整顺序一次性重写某类别（提示词/命令）在当前项目下可见
// 快捷方式的 sort_order。客户端必须提交该类别在当前项目下可见的全部 id（无增减），
// 服务端据此校验为一种排列后，在单个事务内按数组下标重新编号，保证原子与幂等。
func (s *Server) reorderShortcuts(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	var input reorderShortcutsInput
	if !decode(w, r, &input) {
		return
	}
	if input.Kind != "prompt" && input.Kind != "snippet" && input.Kind != "command_request" {
		writeError(w, http.StatusBadRequest, errors.New("invalid shortcut kind"))
		return
	}
	if len(input.OrderedIDs) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("orderedIds must not be empty"))
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `select distinct s.id from shortcuts s left join shortcut_projects sp on sp.shortcut_id=s.id where s.kind=? and (s.scope='local' or sp.project_id=?)`, input.Kind, projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	expected := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if !expected[id] {
			expected[id] = true
		}
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
	seen := map[string]bool{}
	for _, id := range input.OrderedIDs {
		if !expected[id] {
			writeError(w, http.StatusBadRequest, errors.New("orderedIds must contain exactly the visible shortcuts of this kind"))
			return
		}
		if seen[id] {
			writeError(w, http.StatusBadRequest, errors.New("orderedIds contains duplicates"))
			return
		}
		seen[id] = true
	}
	if len(seen) != len(expected) {
		writeError(w, http.StatusBadRequest, errors.New("orderedIds must contain exactly the visible shortcuts of this kind"))
		return
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	for index, id := range input.OrderedIDs {
		if _, err := tx.ExecContext(r.Context(), `update shortcuts set sort_order=?,updated_at=? where id=?`, index, now, id); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	if _, err := tx.ExecContext(r.Context(), `insert into shortcut_audit_events (id,shortcut_id,action,payload,created_at) values (?,?,?,?,?)`, uuid.NewString(), nil, "reordered", mustJSON(map[string]any{"kind": input.Kind, "count": len(input.OrderedIDs)}), now); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"reordered": len(input.OrderedIDs)})
}

func (s *Server) seedDefaultShortcuts(w http.ResponseWriter, r *http.Request) {
	const seedKey = "common_shortcuts_v1"
	defaults := []struct {
		name, kind, template, group, action string
	}{
		{"了解项目", "prompt", "请快速了解当前项目的目录结构、技术栈和关键入口，简洁总结。", "常用提示词", "run"},
		{"检查状态", "prompt", "请检查当前项目的 Git 状态、未完成工作和明显风险，简洁总结。", "常用提示词", "run"},
		{"审查改动", "prompt", "请审查当前项目尚未提交的改动，指出风险、测试缺口和建议。", "常用提示词", "run"},
		{"清屏", "command_request", "/clear", "常用命令", "confirm"},
		{"压缩上下文", "command_request", "/compact", "常用命令", "confirm"},
		{"代码审查", "command_request", "/review", "常用命令", "confirm"},
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
	err := s.db.QueryRowContext(ctx, `select c.id,c.project_id,c.claude_session_id,c.agent_id,c.agent_session_id,c.agent_runtime_id,c.agent_profile_revision_id,c.execution_policy,c.status,c.permission_mode,c.title,c.last_activity_at,c.claude_initialized,c.agent_initialized,c.is_current,c.created_at,p.id,p.name,p.path,coalesce(nullif(p.runner_id,''),p.runner),p.git_branch,p.claude_ready,p.created_at from conversations c join projects p on p.id=c.project_id where c.id=?`, conversationID).Scan(&conversation.ID, &conversation.ProjectID, &conversation.ClaudeSessionID, &conversation.AgentID, &conversation.AgentSessionID, &conversation.AgentRuntimeID, &conversation.AgentProfileRevisionID, &conversation.ExecutionPolicy, &conversation.Status, &conversation.PermissionMode, &conversation.Title, &conversation.LastActivityAt, &conversation.ClaudeInitialized, &conversation.AgentInitialized, &conversation.IsCurrent, &conversation.CreatedAt, &project.ID, &project.Name, &project.Path, &project.Runner, &project.GitBranch, &project.ClaudeReady, &project.CreatedAt)
	if err != nil {
		return Shortcut{}, Conversation{}, Project{}, err
	}
	project.RunnerID = project.Runner
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
		if strings.HasPrefix(rendered, "/") {
			// Slash commands (e.g. /compact, /clear, /review) are native CLI
			// commands — pass them through verbatim so the CLI executes them
			// directly rather than treating them as a prompt to the model.
		} else {
			rendered = "请在当前项目中执行以下命令：\n`" + rendered + "`\n\n遵循当前权限模式；完成后总结结果。"
		}
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
	m, runID, execution, status, err := s.startMessage(r.Context(), conversationID, content, &runStartRecord{Shortcut: shortcutRun})
	if err != nil {
		writeError(w, status, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"message": m, "runId": runID, "shortcutRun": execution.Shortcut})
}

type messageAdmission struct {
	agentID           string
	runnerID          string
	profileRevisionID string
}

func (s *Server) messageAdmission(ctx context.Context, conversationID string) (messageAdmission, error) {
	var admission messageAdmission
	err := s.db.QueryRowContext(ctx, `select c.agent_id,coalesce(nullif(p.runner_id,''),p.runner),c.agent_profile_revision_id from conversations c join projects p on p.id=c.project_id where c.id=?`, conversationID).Scan(&admission.agentID, &admission.runnerID, &admission.profileRevisionID)
	return admission, err
}

func (s *Server) startMessage(ctx context.Context, conversationID, content string, record *runStartRecord) (Message, string, *runStartRecord, int, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return Message{}, "", nil, http.StatusBadRequest, errors.New("message is required")
	}

	// Fast-path admission checks before hitting the database.
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	if closing {
		return Message{}, "", nil, http.StatusServiceUnavailable, errors.New("control service is shutting down")
	}
	admission, err := s.messageAdmission(ctx, conversationID)
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, "", nil, http.StatusNotFound, errors.New("conversation not found")
	}
	if err != nil {
		return Message{}, "", nil, http.StatusInternalServerError, err
	}
	// Do not hold a database transaction while waiting for either of these
	// locks. updateClaude takes the maintenance lock before querying SQLite.
	s.projectLifecycleMu.Lock()
	defer s.projectLifecycleMu.Unlock()
	if admission.agentID == "claude-code" || admission.agentID == "codex" {
		s.runnerMaintenanceMu.Lock()
		defer s.runnerMaintenanceMu.Unlock()
		if s.runnerUpdating[runnerAgentKey{runnerID: admission.runnerID, agentID: admission.agentID}] {
			return Message{}, "", nil, http.StatusConflict, errors.New("this AI CLI is being updated on this runner")
		}
	}
	var profileGate *sync.RWMutex
	if admission.profileRevisionID != "" {
		profileGate = s.profileAdmissions.gate(admission.profileRevisionID)
		profileGate.RLock()
		defer profileGate.RUnlock()
	}

	runID := uuid.NewString()
	m := Message{ID: uuid.NewString(), ConversationID: conversationID, RunID: runID, Role: "user", Content: content, CreatedAt: time.Now().UTC()}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Message{}, "", nil, http.StatusInternalServerError, err
	}
	defer tx.Rollback()
	var conversation Conversation
	var projectPath string
	var projectRunner string
	err = tx.QueryRowContext(ctx, `select c.id,c.project_id,c.claude_session_id,c.agent_id,c.agent_session_id,c.agent_runtime_id,c.agent_profile_revision_id,c.execution_policy,c.status,c.permission_mode,c.title,c.last_activity_at,c.claude_initialized,c.agent_initialized,c.is_current,c.created_at,p.path,coalesce(nullif(p.runner_id,''),p.runner) from conversations c join projects p on p.id=c.project_id where c.id=$1`, conversationID).Scan(&conversation.ID, &conversation.ProjectID, &conversation.ClaudeSessionID, &conversation.AgentID, &conversation.AgentSessionID, &conversation.AgentRuntimeID, &conversation.AgentProfileRevisionID, &conversation.ExecutionPolicy, &conversation.Status, &conversation.PermissionMode, &conversation.Title, &conversation.LastActivityAt, &conversation.ClaudeInitialized, &conversation.AgentInitialized, &conversation.IsCurrent, &conversation.CreatedAt, &projectPath, &projectRunner)
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, "", nil, http.StatusNotFound, errors.New("conversation not found")
	}
	if err != nil {
		return Message{}, "", nil, http.StatusInternalServerError, err
	}
	profile, err := s.runtimeProfileTx(ctx, tx, conversation.AgentProfileRevisionID, projectRunner, conversation.AgentID)
	if err != nil {
		return Message{}, "", nil, http.StatusConflict, err
	}
	if !conversation.IsCurrent {
		return Message{}, "", nil, http.StatusConflict, errors.New("activate this conversation before sending a message")
	}
	if record != nil && record.WorktreePath != "" {
		projectPath = record.WorktreePath
	}
	if runtime.GOOS == "windows" && projectRunner == "wsl-local" {
		return Message{}, "", nil, http.StatusServiceUnavailable,
			errors.New("该项目使用旧 WSL Runner；请先配置对应的 WSL 发行版 Runner")
	}
	// Select the runner for this specific project.
	// For SSH and other non-default runners, use the registry. For the built-in
	// local runner, prefer s.runner directly (tests may replace it).
	runnerObj := s.runner // default to the server-level runner
	if conversation.AgentID == "codex" {
		if strings.HasPrefix(projectRunner, "ssh-") {
			r, ok := s.runnerRegistry.get(projectRunner)
			if !ok {
				return Message{}, "", nil, http.StatusServiceUnavailable, fmt.Errorf("SSH 连接不可用，请重新连接后再试")
			}
			codexR, ok := r.(CodexCapableRunner)
			if !ok || !codexR.CodexReady(ctx) {
				return Message{}, "", nil, http.StatusServiceUnavailable, errors.New("远程服务器上 Codex CLI 不可用或未登录")
			}
			runnerObj = r
		} else {
			if !s.codexUsableForProfile(ctx, profile) {
				return Message{}, "", nil, http.StatusServiceUnavailable, errors.New("Codex CLI is unavailable or not logged in")
			}
			runnerObj = s.codexRunner
		}
	}
	if conversation.AgentID != "codex" && strings.HasPrefix(projectRunner, "ssh-") {
		r, ok := s.runnerRegistry.get(projectRunner)
		if !ok {
			return Message{}, "", nil, http.StatusServiceUnavailable, fmt.Errorf("SSH 连接不可用，请重新连接后再试")
		}
		runnerObj = r
	}
	if conversation.AgentID == "claude-code" && !runnerObj.Ready(ctx) {
		return Message{}, "", nil, http.StatusServiceUnavailable, errors.New("Claude Code is unavailable or not logged in")
	}
	streamingRunner, streaming := runnerObj.(StreamingAgentRunner)
	// Codex runs one-shot turns (non-streaming) even on SSH runners, which
	// otherwise implement StreamingAgentRunner for Claude Code.
	if conversation.AgentID == "codex" {
		streaming = false
		streamingRunner = nil
	}
	if streaming {
		s.streamMu.Lock()
		defer s.streamMu.Unlock()
		s.mu.Lock()
		stopping := s.sessions[conversationID] != nil && s.sessions[conversationID].stopping
		s.mu.Unlock()
		if stopping {
			return Message{}, "", nil, http.StatusConflict, errors.New("conversation is stopping")
		}
	}
	if conversation.Status != "idle" && !streaming {
		return Message{}, "", nil, http.StatusConflict, errors.New("conversation already has a running agent turn")
	}
	workspaceOwner := "run:" + runID
	if streaming {
		workspaceOwner = "conversation:" + conversation.ID
	}
	releaseWorkspace, acquired := s.acquireProjectWorkspace(conversation.ProjectID, workspaceOwner)
	if !acquired {
		return Message{}, "", nil, http.StatusConflict, errors.New("project workspace is occupied by another run or Git operation")
	}
	workspaceAdmitted := false
	defer func() {
		if !workspaceAdmitted {
			releaseWorkspace()
		}
	}()
	if record != nil && record.Task != nil {
		if err := s.validateTaskDispatchTx(ctx, tx, *record.Task, conversation, record.Orchestrated); err != nil {
			return Message{}, "", nil, http.StatusConflict, err
		}
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
			return Message{}, "", nil, http.StatusConflict, errors.New("conversation already has a running agent turn")
		}
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, `insert into messages (id,conversation_id,run_id,role,content,parent_tool_use_id,created_at) values ($1,$2,$3,$4,$5,$6,$7)`, m.ID, m.ConversationID, m.RunID, m.Role, m.Content, m.ParentToolUseID, m.CreatedAt)
	}
	if err == nil {
		status := "running"
		if streaming {
			status = "queued"
		}
		_, err = tx.ExecContext(ctx, `insert into runs (id,conversation_id,agent_id,agent_runtime_id,agent_profile_revision_id,execution_policy,agent_run_id,status,created_at) values ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, runID, conversationID, conversation.AgentID, projectRunner, conversation.AgentProfileRevisionID, conversation.executionPolicy(), "", status, m.CreatedAt)
	}
	if err == nil && record != nil && record.Shortcut != nil {
		record.Shortcut.RunID = runID
		_, err = tx.ExecContext(ctx, `insert into shortcut_runs (id,shortcut_id,conversation_id,run_id,rendered_content,action,status,created_at) values (?,?,?,?,?,?,?,?)`, record.Shortcut.ID, record.Shortcut.ShortcutID, record.Shortcut.ConversationID, record.Shortcut.RunID, record.Shortcut.RenderedContent, record.Shortcut.Action, record.Shortcut.Status, record.Shortcut.CreatedAt)
	}
	if err == nil && record != nil && record.Task != nil {
		if streaming {
			record.Task.Status = "queued"
		} else {
			record.Task.Status = "running"
			record.Task.StartedAt = &m.CreatedAt
		}
		record.Task.RunID = runID
		var sequence int
		err = tx.QueryRowContext(ctx, `select coalesce(max(sequence),0)+1 from task_runs where task_id=?`, record.Task.TaskID).Scan(&sequence)
		if err == nil {
			record.Task.Sequence = sequence
			_, err = tx.ExecContext(ctx, `insert into task_runs (id,task_id,conversation_id,run_id,sequence,status,prompt_snapshot,acceptance_snapshot,failure_reason,created_at,started_at) values (?,?,?,?,?,?,?,?,?,?,?)`, record.Task.ID, record.Task.TaskID, record.Task.ConversationID, record.Task.RunID, record.Task.Sequence, record.Task.Status, record.Task.PromptSnapshot, record.Task.AcceptanceSnapshot, record.Task.FailureReason, record.Task.CreatedAt, record.Task.StartedAt)
		}
		if err == nil && record.Task.ExecutionIntentID != "" {
			var result sql.Result
			result, err = tx.ExecContext(ctx, `update task_execution_intents set run_id=?,status='started',updated_at=? where id=? and run_id=''`, runID, m.CreatedAt, record.Task.ExecutionIntentID)
			if err == nil {
				var changed int64
				changed, err = result.RowsAffected()
				if err == nil && changed != 1 {
					err = errors.New("task execution intent was already linked to a run")
				}
			}
		}
		if err == nil {
			_, err = tx.ExecContext(ctx, `update tasks set status=?,last_task_run_id=?,updated_at=? where id=? and status in ('todo','action_required')`, taskRunning, record.Task.ID, m.CreatedAt, record.Task.TaskID)
		}
		if err == nil {
			err = recordTaskEventTx(ctx, tx, record.Task.TaskID, record.Task.ID, "task.dispatched", map[string]string{"runId": runID, "status": record.Task.Status}, m.CreatedAt)
		}
	}
	if err != nil {
		return Message{}, "", nil, http.StatusInternalServerError, err
	}
	if err = tx.Commit(); err != nil {
		return Message{}, "", nil, http.StatusInternalServerError, err
	}
	s.registerRunWorkspace(runID, releaseWorkspace)
	workspaceAdmitted = true
	if streaming {
		// A streaming runner can still be establishing its persistent process
		// after the Run row has been committed. Register a per-run cancellation
		// immediately so /runs/{id}/stop can cancel that setup window as well.
		runCtx, cancel := context.WithCancel(s.runtimeCtx)
		s.mu.Lock()
		s.cancels[runID] = cancel
		s.runContexts[runID] = conversation.ID
		s.registerProfileRunCancelLocked(conversation.AgentProfileRevisionID, runID, cancel)
		s.streamingSetups[runID] = &streamingSetup{}
		s.mu.Unlock()
		s.submitStreamingRunWithProfile(runCtx, streamingRunner, runID, conversation, profile, projectPath, content)
		return m, runID, record, http.StatusAccepted, nil
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
	s.registerProfileRunCancelLocked(conversation.AgentProfileRevisionID, runID, cancel)
	s.mu.Unlock()
	s.beginRunUsage(runID, conversation.ID)
	go func() {
		defer s.runWG.Done()
		s.runAgent(runCtx, runnerObj, runID, runToken, conversation, profile, projectPath, content)
	}()
	return m, runID, record, http.StatusAccepted, nil
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (s *Server) acquireProjectWorkspace(projectID, owner string) (func(), bool) {
	s.mu.Lock()
	lease := s.projectWorkspaceLeases[projectID]
	if lease != nil && lease.owner != owner {
		s.mu.Unlock()
		return nil, false
	}
	if lease == nil {
		lease = &projectWorkspaceLease{owner: owner}
		s.projectWorkspaceLeases[projectID] = lease
	}
	lease.holders++
	s.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			current := s.projectWorkspaceLeases[projectID]
			if current == nil || current.owner != owner {
				return
			}
			current.holders--
			if current.holders == 0 {
				delete(s.projectWorkspaceLeases, projectID)
			}
		})
	}, true
}

func (s *Server) registerRunWorkspace(runID string, release func()) {
	s.mu.Lock()
	s.runWorkspaceReleases[runID] = release
	s.mu.Unlock()
}

// registerProfileRunCancelLocked makes a Run visible to revision revocation
// before the admission lock is released. Caller must hold s.mu.
func (s *Server) registerProfileRunCancelLocked(revisionID, runID string, cancel context.CancelFunc) {
	if revisionID == "" {
		return
	}
	if s.profileRunCancels[revisionID] == nil {
		s.profileRunCancels[revisionID] = map[string]context.CancelFunc{}
	}
	s.profileRunCancels[revisionID][runID] = cancel
}

func (s *Server) unregisterProfileRunCancel(runID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for revisionID, cancels := range s.profileRunCancels {
		delete(cancels, runID)
		if len(cancels) == 0 {
			delete(s.profileRunCancels, revisionID)
		}
	}
}

func (s *Server) releaseRunWorkspace(runID string) {
	s.mu.Lock()
	release := s.runWorkspaceReleases[runID]
	delete(s.runWorkspaceReleases, runID)
	s.mu.Unlock()
	if release != nil {
		release()
	}
}

func (s *Server) submitStreamingRun(ctx context.Context, runner StreamingAgentRunner, runID string, conversation Conversation, projectPath, prompt string) {
	s.submitStreamingRunWithProfile(ctx, runner, runID, conversation, nil, projectPath, prompt)
}

func (s *Server) submitStreamingRunWithProfile(ctx context.Context, runner StreamingAgentRunner, runID string, conversation Conversation, profile *AgentRuntimeProfile, projectPath, prompt string) {
	// startMessage holds streamMu across admission and the ordered transport
	// write. streamingSetups separately lets a stop cancel initialization before
	// the first prompt is admitted.
	session, err := s.streamingSession(ctx, runner, runID, conversation, profile, projectPath)
	if err != nil {
		s.finishStreamingRun(runID, conversation.ID, err)
		return
	}
	request := AgentRunRequest{SessionID: conversation.sessionID(), ProjectPath: projectPath, Prompt: prompt, PermissionMode: conversation.executionPolicy(), Resume: conversation.initialized(), RunID: runID, Profile: profile}
	if err := s.beginStreamingRun(ctx, runID); err != nil {
		s.finishStreamingRun(runID, conversation.ID, err)
		return
	}
	if err := session.Send(request, &agentRunSink{server: s, runID: runID, conversationID: conversation.ID, agentID: conversation.AgentID, streaming: true}); err != nil {
		s.finishStreamingRun(runID, conversation.ID, err)
		return
	}
}

func (s *Server) beginStreamingRun(ctx context.Context, runID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return context.Canceled
	}
	setup := s.streamingSetups[runID]
	if setup == nil || setup.cancelled {
		return context.Canceled
	}
	delete(s.streamingSetups, runID)
	delete(s.cancels, runID)
	return nil
}

func (s *Server) streamingSession(ctx context.Context, runner StreamingAgentRunner, runID string, conversation Conversation, profile *AgentRuntimeProfile, projectPath string) (AgentSession, error) {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	s.mu.Lock()
	if session := s.sessions[conversation.ID]; session != nil {
		if session.stopping {
			s.mu.Unlock()
			return nil, errors.New("conversation is stopping")
		}
		setup := s.streamingSetups[runID]
		if setup == nil || setup.cancelled {
			s.mu.Unlock()
			return nil, context.Canceled
		}
		setup.session = session
		if session.runIDs == nil {
			session.runIDs = map[string]struct{}{}
		}
		session.runIDs[runID] = struct{}{}
		s.mu.Unlock()
		return session.agent, nil
	}
	if s.closing {
		s.mu.Unlock()
		return nil, errors.New("control service is shutting down")
	}
	s.mu.Unlock()
	token := uuid.NewString()
	agent, err := runner.StartSession(ctx, AgentSessionRequest{SessionID: conversation.sessionID(), ProjectPath: projectPath, PermissionMode: conversation.executionPolicy(), Resume: conversation.initialized(), ConversationID: conversation.ID, ApprovalToken: token, Profile: profile})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		agent.Stop()
		return nil, err
	}
	managed := &activeAgentSession{agent: agent, approvalToken: token, runnerID: conversation.AgentRuntimeID, agentID: conversation.AgentID, runIDs: map[string]struct{}{runID: {}}}
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		agent.Stop()
		return nil, errors.New("control service is shutting down")
	}
	setup := s.streamingSetups[runID]
	if setup == nil || setup.cancelled {
		s.mu.Unlock()
		agent.Stop()
		return nil, context.Canceled
	}
	s.sessions[conversation.ID] = managed
	setup.session = managed
	setup.ownsSession = true
	s.runWG.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.runWG.Done()
		s.watchStreamingSession(conversation.ID, managed)
	}()
	return agent, nil
}

// watchStreamingSession keeps the workspace lease held until the underlying
// process has actually exited. A stopped session may still flush output after
// Stop returns, so its active runs must not be finalized early.
func (s *Server) watchStreamingSession(conversationID string, managed *activeAgentSession) {
	<-managed.agent.Done()

	// Block new streaming admission while removing this stopped session and
	// snapshotting its admitted runs. A new session may start once the lock is
	// released, so only the captured IDs are eligible for this cleanup.
	s.streamMu.Lock()
	s.mu.Lock()
	if s.sessions[conversationID] != managed {
		s.mu.Unlock()
		s.streamMu.Unlock()
		return
	}
	stopping := managed.stopping
	runIDs := make([]string, 0, len(managed.runIDs))
	for runID := range managed.runIDs {
		runIDs = append(runIDs, runID)
		// finishStreamingRun normally removes these mappings. This watcher is
		// its fallback when the process exits without a TurnFinished callback.
		delete(s.cancels, runID)
		delete(s.runContexts, runID)
		delete(s.streamingSetups, runID)
	}
	delete(s.sessions, conversationID)
	s.mu.Unlock()
	s.streamMu.Unlock()
	status := "failed"
	runErr := errors.New("streaming agent session ended unexpectedly")
	if stopping {
		status = "stopped"
		runErr = errors.New("streaming agent session stopped")
	}
	for _, runID := range runIDs {
		s.unregisterProfileRunCancel(runID)
		s.recordUsagePersistenceError(runID, conversationID, s.persistRunUsage(runID, status))
		s.discardRunUsage(runID)
		s.resolveRunApprovals(runID, "deny")
		s.finishRun(runID, conversationID, status, runErr)
	}
}

func (s *Server) runAgent(ctx context.Context, runner AgentRunner, runID, runToken string, c Conversation, profile *AgentRuntimeProfile, projectPath, prompt string) {
	defer func() {
		s.unregisterProfileRunCancel(runID)
		s.discardRunUsage(runID)
		s.resolveRunApprovals(runID, "deny")
		s.mu.Lock()
		delete(s.cancels, runID)
		delete(s.runTokens, runID)
		delete(s.runContexts, runID)
		s.mu.Unlock()
	}()
	err := runner.Run(ctx, AgentRunRequest{
		SessionID:      c.sessionID(),
		ProjectPath:    projectPath,
		Prompt:         prompt,
		PermissionMode: c.executionPolicy(),
		Resume:         c.initialized(),
		RunID:          runID,
		RunToken:       runToken,
		AgentID:        c.AgentID,
		Profile:        profile,
	}, &agentRunSink{server: s, runID: runID, conversationID: c.ID, agentID: c.AgentID})
	status := "completed"
	if ctx.Err() != nil {
		status = "stopped"
	} else if err != nil {
		status = "failed"
	}
	s.recordUsagePersistenceError(runID, c.ID, s.persistRunUsage(runID, status))
	s.finishRun(runID, c.ID, status, err)
}

type agentRunSink struct {
	server         *Server
	runID          string
	conversationID string
	agentID        string
	streaming      bool
}

func (sink *agentRunSink) Event(eventType string, payload json.RawMessage) {
	sink.server.appendEvent(sink.runID, sink.conversationID, eventType, payload)
	sink.server.collectUsageEvent(sink.runID, sink.conversationID, eventType, payload)
}

func (sink *agentRunSink) AssistantText(content, parentToolUseID string) {
	m := Message{ID: uuid.NewString(), ConversationID: sink.conversationID, RunID: sink.runID, Role: "assistant", Content: content, ParentToolUseID: parentToolUseID, CreatedAt: time.Now().UTC()}
	if _, err := sink.server.db.ExecContext(context.Background(), `insert into messages (id,conversation_id,run_id,role,content,parent_tool_use_id,created_at) values ($1,$2,$3,$4,$5,$6,$7)`, m.ID, m.ConversationID, m.RunID, m.Role, m.Content, m.ParentToolUseID, m.CreatedAt); err != nil {
		log.Printf("persist assistant message for run %s: %v", sink.runID, err)
		sink.server.appendEvent(sink.runID, sink.conversationID, "stream.error", mustJSON(map[string]string{"error": "无法保存助手输出，刷新后该段内容可能不可用。"}))
		return
	}
	if _, err := sink.server.db.ExecContext(context.Background(), `update conversations set last_activity_at=? where id=?`, m.CreatedAt, sink.conversationID); err != nil {
		log.Printf("update conversation activity for run %s: %v", sink.runID, err)
	}
	// Raw CLI events differ by runtime. Broadcast the durable message as a
	// runtime-neutral event so every client can render it immediately.
	sink.server.appendEvent(sink.runID, sink.conversationID, "assistant.message", mustJSON(m))
}

func (sink *agentRunSink) SessionIdentified(sessionID string) {
	if sessionID == "" {
		return
	}
	_, _ = sink.server.db.ExecContext(context.Background(), `update conversations set agent_session_id=?,claude_session_id=case when agent_id='claude-code' then ? else claude_session_id end where id=?`, sessionID, sessionID, sink.conversationID)
	_, _ = sink.server.db.ExecContext(context.Background(), `update runs set agent_run_id=? where id=?`, sessionID, sink.runID)
}

func (sink *agentRunSink) SessionInitialized() {
	_, _ = sink.server.db.ExecContext(context.Background(), `update conversations set agent_initialized=true,claude_initialized=case when agent_id='claude-code' then true else claude_initialized end where id=?`, sink.conversationID)
}

func (sink *agentRunSink) TurnStarted() {
	if !sink.streaming {
		return
	}
	sink.server.beginRunUsage(sink.runID, sink.conversationID)
	_, _ = sink.server.db.ExecContext(context.Background(), `update runs set status='running' where id=? and status='queued'`, sink.runID)
	_, _ = sink.server.db.ExecContext(context.Background(), `update task_runs set status='running',started_at=? where run_id=? and status='queued'`, time.Now().UTC(), sink.runID)
	sink.server.mu.Lock()
	sink.server.runContexts[sink.runID] = sink.conversationID
	if session := sink.server.sessions[sink.conversationID]; session != nil {
		session.activeRunID = sink.runID
	}
	sink.server.mu.Unlock()
}

func (sink *agentRunSink) TurnFinished(err error) {
	if !sink.streaming {
		return
	}
	sink.server.finishStreamingRun(sink.runID, sink.conversationID, err)
}

func (s *Server) finishStreamingRun(runID, conversationID string, runErr error) {
	s.unregisterProfileRunCancel(runID)
	s.mu.Lock()
	stopped := false
	if session := s.sessions[conversationID]; session != nil {
		stopped = session.stopping
		delete(session.runIDs, runID)
		if errors.Is(runErr, errClaudeTurnIdleTimeout) {
			// The CLI has stopped accepting turns after its watchdog fires. Keep
			// the session unavailable until Done removes it, rather than letting a
			// new Run reuse a known-stopped process.
			session.stopping = true
		}
		if session.activeRunID == runID {
			session.activeRunID = ""
		}
	}
	delete(s.cancels, runID)
	delete(s.runContexts, runID)
	delete(s.streamingSetups, runID)
	s.mu.Unlock()
	status := "completed"
	if stopped || errors.Is(runErr, context.Canceled) {
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
	committed := false
	if err == nil {
		var result sql.Result
		result, err = tx.ExecContext(context.Background(), `update runs set status=?,completed_at=? where id=? and status in ('queued','running')`, status, time.Now().UTC(), runID)
		if err == nil {
			var changed int64
			changed, err = result.RowsAffected()
			if err == nil && changed == 0 {
				tx.Rollback()
				return
			}
		}
		if err == nil {
			_, err = tx.ExecContext(context.Background(), `update conversations set status=case when exists(select 1 from runs where conversation_id=? and id<>? and status in ('queued','running')) then 'running' else 'idle' end,last_activity_at=? where id=?`, conversationID, runID, time.Now().UTC(), conversationID)
		}
		if err == nil {
			_, err = tx.ExecContext(context.Background(), `update shortcut_runs set status=?,completed_at=? where run_id=?`, status, time.Now().UTC(), runID)
		}
		if err == nil {
			failureReason := ""
			if runErr != nil {
				failureReason = errorText(runErr)
			}
			err = s.finishTaskRunTx(context.Background(), tx, runID, status, failureReason, time.Now().UTC())
		}
		if err == nil {
			err = tx.Commit()
			committed = err == nil
		}
		if err != nil {
			tx.Rollback()
		}
	}
	payload := map[string]any{"status": status, "error": errorText(runErr)}
	// Attach taskId so the frontend can link error cards to the task detail page.
	{
		var payloadTaskID string
		if err := s.db.QueryRowContext(s.runtimeCtx, `select task_id from task_runs where run_id=?`, runID).Scan(&payloadTaskID); err == nil && payloadTaskID != "" {
			payload["taskId"] = payloadTaskID
		}
	}
	if details, ok := runErr.(interface{ ErrorDetails() map[string]string }); ok {
		for key, value := range details.ErrorDetails() {
			payload[key] = value
		}
	}
	payloadJSON, _ := json.Marshal(payload)
	s.appendEvent(runID, conversationID, "run."+status, payloadJSON)
	s.releaseRunWorkspace(runID)
	if committed {
		s.kickOrchestratorForRun(runID)
		// 通知：任务运行结束后的状态变更（awaiting_review 或 action_required）
		var taskID string
		if err := s.db.QueryRowContext(s.runtimeCtx, `select task_id from task_runs where run_id=?`, runID).Scan(&taskID); err == nil && taskID != "" {
			var taskStatus string
			if err := s.db.QueryRowContext(s.runtimeCtx, `select status from tasks where id=?`, taskID).Scan(&taskStatus); err == nil {
				s.notifyTaskStatusChange(s.runtimeCtx, taskID, taskStatus)
			}
		}
	}
}
func (s *Server) stopRun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "runID")
	force := r.URL.Query().Get("force") == "true"
	status, code, err := s.stopRunByID(r.Context(), id, force)
	if err != nil {
		writeError(w, code, err)
		return
	}
	writeJSON(w, code, map[string]string{"status": status})
}

func (s *Server) stopRunByID(ctx context.Context, id string, force bool) (string, int, error) {
	if force {
		return s.forceStopConversationRuns(ctx, id)
	}
	s.mu.Lock()
	cancel, ok := s.cancels[id]
	conversationID := s.runContexts[id]
	session := s.sessions[conversationID]
	setup := s.streamingSetups[id]
	setupStop := setup != nil
	var setupAgent AgentSession
	if setupStop {
		setup.cancelled = true
		if setup.ownsSession && session == setup.session {
			session.stopping = true
			setupAgent = session.agent
		}
	}
	s.mu.Unlock()
	if setupStop {
		if ok {
			cancel()
		}
		if setupAgent != nil {
			setupAgent.Stop()
		}
		s.resolveRunApprovals(id, "deny")
		return "stopping", http.StatusAccepted, nil
	}
	if session != nil {
		s.streamMu.Lock()
		defer s.streamMu.Unlock()
		s.mu.Lock()
		session = s.sessions[conversationID]
		s.mu.Unlock()
		if session == nil {
			return "", http.StatusConflict, errors.New("streaming session is no longer active")
		}
		if err := s.ensureStreamingStopIsIsolated(ctx, conversationID, id); err != nil {
			return "", http.StatusConflict, err
		}
		s.mu.Lock()
		if s.sessions[conversationID] == session {
			session.stopping = true
		}
		s.mu.Unlock()
		session.agent.Stop()
		s.resolveRunApprovals(id, "deny")
		return "stopping", http.StatusAccepted, nil
	}
	if !ok {
		var status, queuedConversationID string
		err := s.db.QueryRowContext(ctx, `select status,conversation_id from runs where id=?`, id).Scan(&status, &queuedConversationID)
		if errors.Is(err, sql.ErrNoRows) {
			return "", http.StatusNotFound, errors.New("run not found")
		}
		if err != nil {
			return "", http.StatusInternalServerError, err
		}
		s.mu.Lock()
		session = s.sessions[queuedConversationID]
		s.mu.Unlock()
		if session != nil {
			s.streamMu.Lock()
			defer s.streamMu.Unlock()
			s.mu.Lock()
			session = s.sessions[queuedConversationID]
			s.mu.Unlock()
			if session == nil {
				return "", http.StatusConflict, errors.New("streaming session is no longer active")
			}
			if err := s.ensureStreamingStopIsIsolated(ctx, queuedConversationID, id); err != nil {
				return "", http.StatusConflict, err
			}
			s.mu.Lock()
			if s.sessions[queuedConversationID] == session {
				session.stopping = true
			}
			s.mu.Unlock()
			session.agent.Stop()
			s.resolveRunApprovals(id, "deny")
			return "stopping", http.StatusAccepted, nil
		}
		if status != "running" {
			return status, http.StatusOK, nil
		}
		return "", http.StatusConflict, errors.New("run is not controlled by this service instance")
	}
	cancel()
	s.resolveRunApprovals(id, "deny")
	return "stopping", http.StatusAccepted, nil
}

// forceStopConversationRuns stops every active turn for the requested run's
// conversation. A streaming agent owns a single process for its queued turns,
// so stopping only the selected run would otherwise leave that process and its
// queued work active.
func (s *Server) forceStopConversationRuns(ctx context.Context, requestedRunID string) (string, int, error) {
	var conversationID, requestedStatus string
	err := s.db.QueryRowContext(ctx, `select conversation_id,status from runs where id=?`, requestedRunID).Scan(&conversationID, &requestedStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return "", http.StatusNotFound, errors.New("run not found")
	}
	if err != nil {
		return "", http.StatusInternalServerError, err
	}
	if requestedStatus != "queued" && requestedStatus != "running" {
		return requestedStatus, http.StatusOK, nil
	}

	// Serialize with streaming admission before collecting active runs. This
	// prevents a new queued turn from escaping the force-stop snapshot.
	s.streamMu.Lock()
	defer s.streamMu.Unlock()

	type activeRun struct{ id string }
	rows, err := s.db.QueryContext(ctx, `select id from runs where conversation_id=? and status in ('queued','running')`, conversationID)
	if err != nil {
		return "", http.StatusInternalServerError, err
	}
	activeRuns := []activeRun{}
	for rows.Next() {
		var run activeRun
		if err := rows.Scan(&run.id); err != nil {
			rows.Close()
			return "", http.StatusInternalServerError, err
		}
		activeRuns = append(activeRuns, run)
	}
	if err := rows.Close(); err != nil {
		return "", http.StatusInternalServerError, err
	}
	if err := rows.Err(); err != nil {
		return "", http.StatusInternalServerError, err
	}

	cancels := make([]context.CancelFunc, 0, len(activeRuns))
	var session AgentSession
	s.mu.Lock()
	if activeSession := s.sessions[conversationID]; activeSession != nil {
		activeSession.stopping = true
		session = activeSession.agent
	}
	for _, run := range activeRuns {
		if cancel := s.cancels[run.id]; cancel != nil {
			cancels = append(cancels, cancel)
		}
	}
	s.mu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
	if session != nil {
		session.Stop()
	}
	for _, run := range activeRuns {
		s.resolveRunApprovals(run.id, "deny")
	}
	return "stopping", http.StatusAccepted, nil
}

func (s *Server) ensureStreamingStopIsIsolated(ctx context.Context, conversationID, runID string) error {
	var otherActive bool
	if err := s.db.QueryRowContext(ctx, `select exists(select 1 from runs where conversation_id=? and id<>? and status in ('queued','running'))`, conversationID, runID).Scan(&otherActive); err != nil {
		return err
	}
	if otherActive {
		return errors.New("cannot stop a run while this conversation has other queued or running runs")
	}
	return nil
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
	if _, err := s.db.ExecContext(context.Background(), `insert into events (id,conversation_id,run_id,type,payload,created_at) values ($1,$2,$3,$4,$5,$6)`, e.ID, e.ConversationID, e.RunID, e.Type, e.Payload, e.CreatedAt); err != nil {
		log.Printf("persist %s event for run %s: %v", typ, runID, err)
		return
	}
	data, _ := json.Marshal(e)
	s.enqueueConversationEvent(conversationID, data)
}

// enqueueConversationEvent keeps agent output independent from WebSocket I/O.
// A slow client is removed once its bounded queue is full; it can reconnect and
// recover durable events through the conversation HTTP endpoint.
func (s *Server) enqueueConversationEvent(conversationID string, data []byte) {
	toClose := make([]*subscriber, 0)
	s.mu.Lock()
	for conn, sub := range s.subscribers[conversationID] {
		select {
		case sub.send <- data:
		default:
			delete(s.subscribers[conversationID], conn)
			toClose = append(toClose, sub)
		}
	}
	if len(s.subscribers[conversationID]) == 0 {
		delete(s.subscribers, conversationID)
	}
	s.mu.Unlock()
	for _, sub := range toClose {
		log.Printf("[ws] /ws/conversations/%s subscriber queue full; closing slow client", conversationID)
		go sub.closeWithStatus(websocket.CloseTryAgainLater, "client is too slow")
	}
}

func (s *Server) removeConversationSubscriber(conversationID string, sub *subscriber) {
	removed := false
	s.mu.Lock()
	if current := s.subscribers[conversationID][sub.conn]; current == sub {
		delete(s.subscribers[conversationID], sub.conn)
		if len(s.subscribers[conversationID]) == 0 {
			delete(s.subscribers, conversationID)
		}
		removed = true
	}
	s.mu.Unlock()
	if removed {
		sub.close()
		// A failed writer has already removed this subscriber from the registry.
		// Closing the transport unblocks the owning reader and heartbeat so the
		// handler cannot outlive an unreachable subscription.
		if sub.conn != nil {
			_ = sub.conn.Close()
		}
	}
}

func (s *Server) closeAllConversationSubscribers() {
	s.mu.Lock()
	all := make([]*subscriber, 0)
	for _, subscribers := range s.subscribers {
		for _, sub := range subscribers {
			all = append(all, sub)
		}
	}
	s.subscribers = map[string]map[*websocket.Conn]*subscriber{}
	s.mu.Unlock()
	var closeWG sync.WaitGroup
	closeWG.Add(len(all))
	for _, sub := range all {
		go func(sub *subscriber) {
			defer closeWG.Done()
			sub.closeWithStatus(websocket.CloseGoingAway, "server shutting down")
		}(sub)
	}
	closeWG.Wait()
}

func (s *Server) subscribe(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "conversationID")
	path := "/ws/conversations/" + id
	if !s.beginWebSocketSubscription(w) {
		return
	}
	defer s.websocketWG.Done()
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	if s.isClosing() {
		initiateWebSocketClose(conn, websocket.CloseGoingAway, "server shutting down")
		waitForWebSocketClose(conn)
		return
	}
	stopHeartbeat := startWebSocketHeartbeat(conn, path)
	sub := &subscriber{conn: conn, send: make(chan []byte, conversationSubscriberQueueSize)}
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for data := range sub.send {
			_ = conn.SetWriteDeadline(time.Now().Add(conversationWriteTimeout))
			if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
				logWebSocketDisconnect(path, err)
				s.removeConversationSubscriber(id, sub)
				return
			}
		}
	}()
	if !s.addConversationSubscriber(id, sub) {
		stopHeartbeat()
		sub.closeWithStatus(websocket.CloseGoingAway, "server shutting down")
		waitForWebSocketClose(conn)
		<-writerDone
		return
	}
	defer func() {
		stopHeartbeat()
		s.removeConversationSubscriber(id, sub)
		_ = conn.Close()
		<-writerDone
	}()
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			logWebSocketDisconnect(path, err)
			return
		}
	}
}

// addConversationSubscriber atomically checks the service lifecycle and
// records the subscription, so Server.Close either observes it or rejects it.
func (s *Server) addConversationSubscriber(conversationID string, sub *subscriber) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return false
	}
	if s.subscribers[conversationID] == nil {
		s.subscribers[conversationID] = map[*websocket.Conn]*subscriber{}
	}
	s.subscribers[conversationID][sub.conn] = sub
	return true
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

// allowedPathForRunner 校验路径是否在指定 runner 的任意一个 root 下。
// 对于 wsl-local runner，root 包括 WSL Home 和 /mnt/c、/mnt/d 等 Windows 挂载点。
func (s *Server) allowedPathForRunner(path string, runnerID string) (string, error) {
	absolute, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	// 从 runner 注册表获取所有 roots
	if meta, ok := s.runnerRegistry.getMeta(runnerID); ok && len(meta.Roots) > 0 {
		for _, root := range meta.Roots {
			resolvedRoot, err := filepath.EvalSymlinks(root.Path)
			if err != nil {
				continue
			}
			relative, err := filepath.Rel(resolvedRoot, absolute)
			if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
				return absolute, nil
			}
		}
		return "", errors.New("path is outside the Runner allowed roots")
	}
	// 回退到原有 allowedPath 逻辑
	return s.allowedPath(path)
}
func gitBranch(ctx context.Context, path string) (string, bool) {
	snapshot, err := newGitRunner().Snapshot(ctx, path)
	if err != nil {
		return "", false
	}
	if snapshot.Head.Detached {
		return "HEAD", true
	}
	return snapshot.Head.Branch, true
}
func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	return decodeJSON(w, r, target, false)
}

func decodeOptional(w http.ResponseWriter, r *http.Request, target any) bool {
	return decodeJSON(w, r, target, true)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any, allowEmpty bool) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes))
	if err := decoder.Decode(target); err != nil {
		if errors.Is(err, io.EOF) && allowEmpty {
			return true
		}
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeError(w, http.StatusRequestEntityTooLarge, errors.New("request body too large"))
		} else {
			writeError(w, http.StatusBadRequest, errors.New("invalid JSON request"))
		}
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
	payload := map[string]string{"error": localizedHTTPErrorText(status, err)}
	if code := httpErrorCode(err); code != "" {
		payload["code"] = code
	}
	writeJSON(w, status, payload)
}

// httpErrorCode provides stable client behavior without coupling it to a
// localized error message.
func httpErrorCode(err error) string {
	if err != nil && strings.TrimSpace(err.Error()) == "cannot stop a run while this conversation has other queued or running runs" {
		return "active_runs_present"
	}
	return ""
}
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return localizedErrorText(err, "任务执行失败，请查看任务日志后重试。")
}

// localizedHTTPErrorText keeps internal implementation errors out of the UI
// while preserving actionable messages that have already been localized.
func localizedHTTPErrorText(status int, err error) string {
	fallback := "操作失败，请稍后重试。"
	switch status {
	case http.StatusBadRequest:
		fallback = "请求参数无效，请检查后重试。"
	case http.StatusRequestEntityTooLarge:
		fallback = "请求内容过大，请缩小后重试。"
	case http.StatusUnauthorized:
		fallback = "未授权访问，请重新登录后重试。"
	case http.StatusForbidden:
		fallback = "没有执行此操作的权限。"
	case http.StatusNotFound:
		fallback = "请求的资源不存在或已被删除。"
	case http.StatusConflict:
		fallback = "当前操作与进行中的操作冲突，请稍后重试。"
	case http.StatusGatewayTimeout:
		fallback = "操作超时，请稍后重试。"
	case http.StatusServiceUnavailable:
		fallback = "服务暂时不可用，请稍后重试。"
	case http.StatusInternalServerError:
		fallback = "服务内部错误，请稍后重试。"
	}
	return localizedErrorText(err, fallback)
}

func localizedErrorText(err error, fallback string) string {
	if err == nil {
		return ""
	}

	message := strings.TrimSpace(err.Error())
	if message == "" {
		return fallback
	}
	if strings.HasPrefix(message, "project workspace is occupied") {
		return "项目工作区正被其他 AI 任务或 Git 操作占用，请等待当前操作完成后重试。"
	}

	translations := map[string]string{
		"project not found":      "项目不存在或已被删除。",
		"conversation not found": "会话不存在或已被删除。",
		"run not found":          "任务运行记录不存在。",
		"invalid JSON request":   "请求内容不是有效的 JSON。",
		"activate this conversation before sending a message":                        "请先激活该会话，再发送消息。",
		"Codex is currently available only on the local WSL runner":                  "Codex 目前仅支持本地 WSL 运行器。",
		"远程服务器上 Codex CLI 不可用或未登录":                                                   "远程服务器上 Codex CLI 不可用或未登录。",
		"远程服务器上未安装 Codex CLI":                                                        "远程服务器上未安装 Codex CLI。",
		"此 Runner 不支持 Codex":                                                         "此运行器不支持 Codex。",
		"Codex CLI is unavailable or not logged in":                                  "Codex CLI 不可用或尚未登录。",
		"Claude Code is unavailable or not logged in":                                "Claude Code 不可用或尚未登录。",
		"conversation is stopping":                                                   "会话正在停止，请稍后再试。",
		"conversation already has a running agent turn":                              "该会话已有正在运行的 AI 任务。",
		"cannot stop a run while this conversation has other queued or running runs": "当前会话还有排队或运行中的任务，暂时无法单独停止此任务。",
		"cannot stop a task while this conversation has other queued task runs":      "当前会话还有排队的任务，暂时无法单独停止此任务。",
		"queued task cannot be stopped independently":                                "排队中的任务无法单独停止。",
		"Git request timed out":                                                      "Git 操作超时，请稍后重试。",
		"project is not currently a readable Git repository":                         "当前项目不是可读取的 Git 仓库。",
		"runner not found":                                                           "运行器不存在。",
		"runner is not an SSH runner":                                                "当前运行器不是 SSH 运行器。",
		"another AI CLI is already being updated on this runner":                     "该运行器上已有 AI CLI 正在更新。",
	}
	if translated, ok := translations[message]; ok {
		return translated
	}
	if errors.Is(err, context.Canceled) {
		return "操作已取消。"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "操作超时，请稍后重试。"
	}
	// Internal Go errors (stack traces, panics) — keep the fallback only
	// to avoid leaking implementation details. Checked first so a wrapped
	// "… 失败" message can never carry a stack trace to the UI.
	if isInternalError(message) {
		return fallback
	}
	// Messages that already carry a descriptive Chinese failure label (e.g. the
	// update errors "更新 … 失败：<原因>") are self-explanatory and must be shown
	// as-is, rather than being buried under the generic "查看日志" fallback that
	// gives no reason. Pure Chinese errors also display directly.
	if containsChinese(message) && (strings.Contains(message, "失败") || !containsUntranslatedEnglish(message)) {
		return message
	}
	// English or mixed-language errors — keep the fallback as a prefix so
	// users always see a Chinese description, then append the original error.
	return fallback + "：" + message
}

// isInternalError detects Go runtime errors that would leak implementation
// details to the UI — stack traces, panics, and file/line references.
func isInternalError(message string) bool {
	return strings.Contains(message, ".go:") ||
		strings.Contains(message, "panic:") ||
		strings.Contains(message, "goroutine")
}

func containsChinese(value string) bool {
	for _, r := range value {
		if r >= '\u4e00' && r <= '\u9fff' {
			return true
		}
	}
	return false
}

func containsUntranslatedEnglish(value string) bool {
	allowed := map[string]bool{
		"AI": true, "API": true, "CLI": true, "Codex": true, "Claude": true,
		"Git": true, "HTTP": true, "ID": true, "JSON": true, "NUL": true,
		"SSH": true, "URL": true, "WSL": true,
	}
	for index := 0; index < len(value); {
		if (value[index] < 'A' || value[index] > 'Z') && (value[index] < 'a' || value[index] > 'z') {
			index++
			continue
		}
		start := index
		for index < len(value) && ((value[index] >= 'A' && value[index] <= 'Z') || (value[index] >= 'a' && value[index] <= 'z')) {
			index++
		}
		word := value[start:index]
		if len(word) >= 3 && !allowed[word] {
			return true
		}
	}
	return false
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
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

// codexUsableForProfile reports whether the local Codex binary can run the given
// bound profile. A managed api_key profile injects its own OPENAI_API_KEY, so it
// only needs the binary; cli_managed and profile-less runs also require a
// persisted CLI login.
func (s *Server) codexUsableForProfile(ctx context.Context, profile *AgentRuntimeProfile) bool {
	if codexRunner, ok := s.codexRunner.(*codexCLIRunner); ok {
		if profile != nil && profile.AuthMode == "api_key" {
			return codexRunner.BinaryReady()
		}
	}
	return s.codexRunner.Ready(ctx)
}

func (s *Server) localRunnerID() string {
	if runtime.GOOS == "windows" {
		return "windows-local"
	}
	return "wsl-local"
}

func isLocalRunnerID(id string) bool {
	if runtime.GOOS == "windows" {
		return id == "" || id == "windows-local"
	}
	return id == "" || id == "wsl-local"
}

func (s *Server) localRunnerMeta() RunnerMeta {
	if runtime.GOOS == "windows" {
		return s.windowsLocalMeta()
	}
	return s.wslLocalMeta()
}

// wslLocalMeta builds the RunnerMeta for the built-in WSL local runner.
func (s *Server) wslLocalMeta() RunnerMeta {
	roots := []RootEntry{
		{Name: "WSL Home", Path: s.config.AllowedRoot},
	}
	for _, drive := range []string{"c", "d", "e", "f"} {
		mntPath := filepath.Join("/mnt", drive)
		if info, err := os.Stat(mntPath); err == nil && info.IsDir() {
			roots = append(roots, RootEntry{
				Name:  fmt.Sprintf("Windows (%s:)", strings.ToUpper(drive)),
				Path:  mntPath,
				Label: "windows",
			})
		}
	}
	return RunnerMeta{
		ID:          "wsl-local",
		Name:        "WSL Local Runner",
		Environment: "wsl",
		Root:        s.config.AllowedRoot,
		Roots:       roots,
	}
}

func (s *Server) windowsLocalMeta() RunnerMeta {
	roots := []RootEntry{{Name: "用户目录", Path: s.config.AllowedRoot}}
	for drive := 'C'; drive <= 'Z'; drive++ {
		path := string(drive) + `:\\`
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			roots = append(roots, RootEntry{Name: "Windows (" + string(drive) + ":)", Path: path, Label: "windows"})
		}
	}
	return RunnerMeta{ID: "windows-local", Name: "Windows Local Runner", Environment: "windows", Root: s.config.AllowedRoot, Roots: roots}
}

func (s *Server) listRunners(w http.ResponseWriter, r *http.Request) {
	metas := s.runnerRegistry.list()
	result := make([]map[string]any, 0, len(metas))
	for _, m := range metas {
		entry := map[string]any{
			"id":                m.ID,
			"name":              m.Name,
			"environment":       m.Environment,
			"root":              m.Root,
			"roots":             m.Roots,
			"profileManagement": s.canManageProfileRunner(m.ID),
		}
		if m.Host != "" {
			entry["host"] = m.Host
		}
		// Fetch Claude version from each runner.
		if runner, ok := s.runnerRegistry.get(m.ID); ok {
			v := runner.Version(r.Context())
			status := "ready"
			if v == "" {
				status = "unavailable"
			}
			// Reflect in-progress updates so the frontend can show "更新中...".
			s.runnerMaintenanceMu.Lock()
			if s.runnerUpdating[runnerAgentKey{runnerID: m.ID, agentID: "claude-code"}] {
				status = "updating"
			}
			s.runnerMaintenanceMu.Unlock()
			entry["claude"] = map[string]string{
				"status":  status,
				"version": v,
			}
		}
		codexStatus := "unavailable"
		codexVersion := ""
		codexReason := ""
		if isLocalRunnerID(m.ID) {
			if s.codexRunner.Ready(r.Context()) {
				codexStatus = "ready"
				codexVersion = s.codexRunner.Version(r.Context())
			} else {
				codexReason = "本机 Codex CLI 未安装或未登录"
			}
		} else if runner, ok := s.runnerRegistry.get(m.ID); ok {
			if codexR, ok := runner.(CodexCapableRunner); ok {
				if codexR.CodexReady(r.Context()) {
					codexStatus = "ready"
					codexVersion = codexR.CodexVersion(r.Context())
				} else {
					codexReason = "远程服务器上 Codex CLI 未安装或未登录"
				}
			} else {
				codexReason = "此 Runner 不支持 Codex"
			}
		}
		s.runnerMaintenanceMu.Lock()
		if s.runnerUpdating[runnerAgentKey{runnerID: m.ID, agentID: "codex"}] {
			codexStatus = "updating"
			codexReason = ""
		}
		s.runnerMaintenanceMu.Unlock()
		entry["codex"] = map[string]string{
			"status":  codexStatus,
			"version": codexVersion,
			"reason":  codexReason,
		}
		result = append(result, entry)
	}
	if result == nil {
		result = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) checkClaudeUpdate(w http.ResponseWriter, r *http.Request) {
	runnerID := chi.URLParam(r, "runnerID")
	runner, ok := s.runnerRegistry.get(runnerID)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("runner not found"))
		return
	}
	s.checkAgentUpdate(w, r, runner)
}

func (s *Server) checkCodexUpdate(w http.ResponseWriter, r *http.Request) {
	runnerID := chi.URLParam(r, "runnerID")
	if isLocalRunnerID(runnerID) {
		s.checkAgentUpdate(w, r, s.codexRunner)
		return
	}
	runner, ok := s.runnerRegistry.get(runnerID)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("runner not found"))
		return
	}
	codexR, ok := runner.(CodexCapableRunner)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("此 Runner 不支持 Codex"))
		return
	}
	s.checkAgentUpdate(w, r, codexRunnerAdapter{codexR})
}

func (s *Server) checkAgentUpdate(w http.ResponseWriter, r *http.Request, runner AgentRunner) {
	available, latestVersion, err := runner.CheckUpdate(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"updateAvailable": false,
			"currentVersion":  runner.Version(r.Context()),
			"error":           errorText(err),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"updateAvailable": available,
		"currentVersion":  runner.Version(r.Context()),
		"latestVersion":   latestVersion,
	})
}

func (s *Server) updateClaude(w http.ResponseWriter, r *http.Request) {
	runnerID := chi.URLParam(r, "runnerID")
	runner, ok := s.runnerRegistry.get(runnerID)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("runner not found"))
		return
	}

	s.updateAgent(w, r, runnerID, "claude-code", "Claude Code", runner)
}

func (s *Server) updateCodex(w http.ResponseWriter, r *http.Request) {
	runnerID := chi.URLParam(r, "runnerID")
	if isLocalRunnerID(runnerID) {
		s.updateAgent(w, r, runnerID, "codex", "Codex", s.codexRunner)
		return
	}
	runner, ok := s.runnerRegistry.get(runnerID)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("runner not found"))
		return
	}
	codexR, ok := runner.(CodexCapableRunner)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("此 Runner 不支持 Codex"))
		return
	}
	s.updateAgent(w, r, runnerID, "codex", "Codex", codexRunnerAdapter{codexR})
}

// codexRunnerAdapter exposes a CodexCapableRunner (e.g. an SSH runner) through
// the AgentRunner interface so that updateAgent can drive Codex updates with the
// same locking and active-run checks as Claude Code.
type codexRunnerAdapter struct{ inner CodexCapableRunner }

func (a codexRunnerAdapter) Ready(ctx context.Context) bool     { return a.inner.CodexReady(ctx) }
func (a codexRunnerAdapter) Version(ctx context.Context) string { return a.inner.CodexVersion(ctx) }
func (a codexRunnerAdapter) Run(ctx context.Context, _ AgentRunRequest, _ AgentRunSink) error {
	return errors.New("codexRunnerAdapter does not support Run")
}
func (a codexRunnerAdapter) CheckUpdate(ctx context.Context) (bool, string, error) {
	return a.inner.CodexCheckUpdate(ctx)
}
func (a codexRunnerAdapter) Update(ctx context.Context) (string, string, error) {
	return a.inner.CodexUpdate(ctx)
}

func (s *Server) updateAgent(w http.ResponseWriter, r *http.Request, runnerID, agentID, agentName string, runner AgentRunner) {
	// Serialize updates with run admission. startMessage keeps this lock until
	// its run record is committed, closing the check-then-start race.
	maintenanceKey := runnerAgentKey{runnerID: runnerID, agentID: agentID}
	s.runnerMaintenanceMu.Lock()
	if s.runnerUpdateExecuting[runnerID] {
		s.runnerMaintenanceMu.Unlock()
		writeError(w, http.StatusConflict, errors.New("another AI CLI is already being updated on this runner"))
		return
	}

	var active bool
	if err := s.db.QueryRowContext(r.Context(), `select exists(select 1 from runs where agent_runtime_id=? and agent_id=? and status in ('queued','running'))`, runnerID, agentID).Scan(&active); err != nil {
		s.runnerMaintenanceMu.Unlock()
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if active {
		s.runnerMaintenanceMu.Unlock()
		writeError(w, http.StatusConflict, fmt.Errorf("cannot update %s while this runner has active conversations", agentName))
		return
	}
	s.mu.Lock()
	for _, session := range s.sessions {
		if session.runnerID == runnerID && (session.agentID == agentID || (session.agentID == "" && agentID == "claude-code")) {
			s.mu.Unlock()
			s.runnerMaintenanceMu.Unlock()
			writeError(w, http.StatusConflict, fmt.Errorf("cannot update %s while this runner has an active session", agentName))
			return
		}
	}
	s.mu.Unlock()
	s.runnerUpdateExecuting[runnerID] = true
	s.runnerUpdating[maintenanceKey] = true
	s.runnerMaintenanceMu.Unlock()
	defer func() {
		s.runnerMaintenanceMu.Lock()
		delete(s.runnerUpdateExecuting, runnerID)
		delete(s.runnerUpdating, maintenanceKey)
		s.runnerMaintenanceMu.Unlock()
	}()

	updateCtx, cancel := context.WithTimeout(s.runtimeCtx, s.config.agentUpdateTimeout())
	defer cancel()
	previousVersion, currentVersion, err := runner.Update(updateCtx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success":         false,
			"previousVersion": previousVersion,
			"currentVersion":  currentVersion,
			"error":           errorText(err),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":         true,
		"previousVersion": previousVersion,
		"currentVersion":  currentVersion,
	})
}

func (s *Server) loadRunConfig(ctx context.Context, projectID string) (RunConfig, error) {
	var c RunConfig
	var envJSON string
	err := s.db.QueryRowContext(ctx, `select work_dir, command, env_vars, execution_target from project_run_configs where project_id=$1`, projectID).Scan(&c.WorkDir, &c.Command, &envJSON, &c.ExecutionTarget)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RunConfig{}, nil
		}
		return RunConfig{}, err
	}
	if envJSON != "" {
		json.Unmarshal([]byte(envJSON), &c.EnvVars)
	}
	c.ExecutionTarget = normalizeRunExecutionTarget(c.ExecutionTarget)
	return c, nil
}

func (s *Server) saveRunConfig(ctx context.Context, projectID string, c RunConfig) error {
	envJSON, _ := json.Marshal(c.EnvVars)
	c.ExecutionTarget = normalizeRunExecutionTarget(c.ExecutionTarget)
	_, err := s.db.ExecContext(ctx,
		`insert into project_run_configs (project_id, work_dir, command, env_vars, execution_target, updated_at) values ($1,$2,$3,$4,$5,$6) on conflict(project_id) do update set work_dir=excluded.work_dir, command=excluded.command, env_vars=excluded.env_vars, execution_target=excluded.execution_target, updated_at=excluded.updated_at`,
		projectID, c.WorkDir, c.Command, string(envJSON), c.ExecutionTarget, time.Now().UTC())
	return err
}

// projectRunManagerForExistingProject rechecks project existence while holding
// the manager map lock. A concurrent deletion therefore either retires this
// runner after creation or prevents a new runner from being registered.
// 根据项目的 runner 类型创建对应的 projectRunner（本地或 SSH）。
// 先在锁外解析远端路径（SSH 往返），再持锁检查并注册 runner，避免长时间持锁。
func (s *Server) projectRunManagerForExistingProject(ctx context.Context, projectID string) (projectRunnerInterface, error) {
	project, err := s.getProjectByID(ctx, projectID)
	if err != nil {
		return nil, err
	}

	// SSH 项目需要先解析远端规范路径（含一次 SSH 往返），在锁外完成。
	type preparedRunner struct {
		runner projectRunnerInterface
	}
	var prepared *preparedRunner
	if !isLocalRunnerID(project.Runner) {
		agentRunner, ok := s.runnerRegistry.get(project.Runner)
		if !ok {
			return nil, &runnerOfflineError{RunnerID: project.Runner}
		}
		sshR, ok := agentRunner.(*sshRunner)
		if !ok {
			return nil, errors.New("runner 不是 SSH 类型")
		}
		repo, err := sshR.canonicalProjectPath(ctx, project.Path)
		if err != nil {
			return nil, err
		}
		broadcast := func(entry LogEntry) {
			s.broadcastRunLog(projectID, entry)
		}
		prepared = &preparedRunner{runner: newSSHProjectRunner(projectID, sshR.client, repo, broadcast)}
	}

	s.runManagersMu.Lock()
	defer s.runManagersMu.Unlock()
	// 持锁后重新检查项目是否存在（并发删除可能在锁释放期间发生）。
	if _, err := s.getProjectByID(ctx, projectID); err != nil {
		return nil, err
	}
	if runner, ok := s.runManagers[projectID]; ok {
		return runner, nil
	}
	broadcast := func(entry LogEntry) {
		s.broadcastRunLog(projectID, entry)
	}
	var runner projectRunnerInterface
	if prepared != nil {
		runner = prepared.runner
	} else {
		runner = newProjectRunner(projectID, "", "", nil, broadcast)
	}
	runner.setStatusListener(func(event RunStatusEvent) {
		s.broadcastProcessStatus(event)
	})
	s.runManagers[projectID] = runner
	return runner, nil
}

func validateProjectRunConfig(project Project, cfg RunConfig) error {
	if cfg.Command == "" {
		return errors.New("请先配置启动命令")
	}
	if err := validateRunEnvironmentVariables(cfg.EnvVars); err != nil {
		return err
	}
	// SSH 远程项目的执行环境与工作目录在远端解析，跳过本地路径校验。
	if !isLocalRunnerID(project.Runner) {
		return nil
	}
	if _, err := resolveRunExecutionTarget(project.Path, cfg.ExecutionTarget); err != nil {
		return err
	}
	_, err := resolveProjectRunWorkDir(project.Path, cfg.WorkDir)
	return err
}

func validateRunEnvironmentVariables(envVars map[string]string) error {
	for key, value := range envVars {
		if key == "" || strings.ContainsAny(key, "=\x00") {
			return errors.New("环境变量名称不能为空，且不能包含 = 或 NUL 字符")
		}
		if !isPosixEnvName(key) {
			return errors.New("环境变量名称只能包含字母、数字和下划线，且不能以数字开头")
		}
		if strings.ContainsRune(value, 0) {
			return errors.New("环境变量值不能包含 NUL 字符")
		}
	}
	return nil
}

// isPosixEnvName 检查是否为合法的 POSIX 环境变量名：
// 仅含字母、数字、下划线，且不以数字开头。
func isPosixEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if i == 0 && r >= '0' && r <= '9' {
			return false
		}
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_') {
			return false
		}
	}
	return true
}

func (s *Server) getProjectByID(ctx context.Context, projectID string) (Project, error) {
	var p Project
	err := s.db.QueryRowContext(ctx, `select id,name,path,coalesce(nullif(runner_id,''),runner),git_branch,claude_ready,created_at,coalesce(default_profile_id,'') from projects where id=$1`, projectID).Scan(&p.ID, &p.Name, &p.Path, &p.Runner, &p.GitBranch, &p.ClaudeReady, &p.CreatedAt, &p.DefaultProfileID)
	if err != nil {
		return Project{}, err
	}
	p.RunnerID = p.Runner
	return p, nil
}

func (s *Server) requireRunProject(w http.ResponseWriter, r *http.Request) bool {
	if _, err := s.getProjectByID(r.Context(), chi.URLParam(r, "projectID")); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("项目不存在"))
			return false
		}
		writeError(w, http.StatusInternalServerError, err)
		return false
	}
	return true
}

func (s *Server) getRunConfig(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	if !s.requireRunProject(w, r) {
		return
	}
	c, err := s.loadRunConfig(r.Context(), projectID)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	if c.EnvVars == nil {
		c.EnvVars = map[string]string{}
	}
	c.ExecutionTarget = normalizeRunExecutionTarget(c.ExecutionTarget)
	writeJSON(w, 200, c)
}

func (s *Server) updateRunConfig(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	project, err := s.getProjectByID(r.Context(), projectID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("项目不存在"))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var c RunConfig
	if !decode(w, r, &c) {
		return
	}
	if err := validateRunEnvironmentVariables(c.EnvVars); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if isLocalRunnerID(project.Runner) {
		if _, err := resolveRunExecutionTarget(project.Path, c.ExecutionTarget); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if _, err := resolveProjectRunWorkDir(project.Path, c.WorkDir); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	c.ExecutionTarget = normalizeRunExecutionTarget(c.ExecutionTarget)
	if err := s.saveRunConfig(r.Context(), projectID, c); err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, 200, c)
}

func (s *Server) startProjectRun(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	project, err := s.getProjectByID(r.Context(), projectID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, 404, fmt.Errorf("项目不存在"))
			return
		}
		writeError(w, 500, err)
		return
	}
	if !isLocalRunnerID(project.Runner) {
		// SSH 等远程项目：校验 runner 在线，由 projectRunManagerForExistingProject 负责。
		if _, ok := s.runnerRegistry.get(project.Runner); !ok {
			writeError(w, http.StatusConflict, &runnerOfflineError{RunnerID: project.Runner})
			return
		}
	}

	// 加载已保存配置
	cfg, err := s.loadRunConfig(r.Context(), projectID)
	if err != nil {
		writeError(w, 500, err)
		return
	}

	// 用请求体覆盖
	var req RunConfig
	if r.Body != http.NoBody {
		if !decode(w, r, &req) {
			return
		}
		if req.Command != "" {
			cfg.Command = req.Command
			cfg.WorkDir = req.WorkDir
			cfg.ExecutionTarget = req.ExecutionTarget
			if req.EnvVars != nil {
				cfg.EnvVars = req.EnvVars
			}
		}
	}

	if cfg.EnvVars == nil {
		cfg.EnvVars = map[string]string{}
	}

	if err := validateProjectRunConfig(project, cfg); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	runner, err := s.projectRunManagerForExistingProject(r.Context(), projectID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("项目不存在"))
			return
		}
		if offline, ok := err.(*runnerOfflineError); ok {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "runner_offline", "runnerId": offline.RunnerID})
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := runner.StartWithConfig(s.runtimeCtx, project.Path, cfg.WorkDir, cfg.Command, cfg.EnvVars, cfg.ExecutionTarget); err != nil {
		writeError(w, 409, err)
		return
	}
	writeJSON(w, 202, map[string]string{"status": "starting"})
}

func (s *Server) stopProjectRun(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	if !s.requireRunProject(w, r) {
		return
	}
	s.runManagersMu.RLock()
	runner, ok := s.runManagers[projectID]
	s.runManagersMu.RUnlock()
	if !ok {
		writeError(w, 404, fmt.Errorf("进程未在运行"))
		return
	}
	if err := runner.Stop(); err != nil {
		writeError(w, 409, err)
		return
	}
	writeJSON(w, 202, map[string]string{"status": "stopping"})
}

func (s *Server) restartProjectRun(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	project, err := s.getProjectByID(r.Context(), projectID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, 404, fmt.Errorf("项目不存在"))
			return
		}
		writeError(w, 500, err)
		return
	}
	if !isLocalRunnerID(project.Runner) {
		if _, ok := s.runnerRegistry.get(project.Runner); !ok {
			writeError(w, http.StatusConflict, &runnerOfflineError{RunnerID: project.Runner})
			return
		}
	}
	cfg, err := s.loadRunConfig(r.Context(), projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if cfg.EnvVars == nil {
		cfg.EnvVars = map[string]string{}
	}
	if err := validateProjectRunConfig(project, cfg); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	runner, err := s.projectRunManagerForExistingProject(r.Context(), projectID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("项目不存在"))
			return
		}
		if offline, ok := err.(*runnerOfflineError); ok {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "runner_offline", "runnerId": offline.RunnerID})
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := runner.RestartWithConfig(s.runtimeCtx, project.Path, cfg.WorkDir, cfg.Command, cfg.EnvVars, cfg.ExecutionTarget); err != nil {
		writeError(w, 409, err)
		return
	}
	writeJSON(w, 202, map[string]string{"status": "starting"})
}

func (s *Server) getProjectRunStatus(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	if !s.requireRunProject(w, r) {
		return
	}
	s.runManagersMu.RLock()
	runner, ok := s.runManagers[projectID]
	s.runManagersMu.RUnlock()
	if !ok {
		writeJSON(w, 200, RunStatusResponse{Status: RunStatusStopped, RecentLogs: []LogEntry{}})
		return
	}
	writeJSON(w, 200, runner.StatusSnapshot())
}

// subscribeRunLogs 处理 WebSocket 日志订阅。
func (s *Server) subscribeRunLogs(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	path := "/ws/projects/" + projectID + "/run"
	if !s.requireRunProject(w, r) {
		return
	}
	if !s.beginWebSocketSubscription(w) {
		return
	}
	defer s.websocketWG.Done()

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	if s.isClosing() {
		initiateWebSocketClose(conn, websocket.CloseGoingAway, "server shutting down")
		waitForWebSocketClose(conn)
		return
	}
	stopHeartbeat := startWebSocketHeartbeat(conn, path)

	sub := &runLogSubscriber{conn: conn, send: make(chan LogEntry, runLogSubscriberQueueSize)}
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for entry := range sub.send {
			_ = conn.SetWriteDeadline(time.Now().Add(runLogWriteTimeout))
			if err := conn.WriteJSON(entry); err != nil {
				logWebSocketDisconnect(path, err)
				_ = conn.Close()
				return
			}
		}
	}()
	if !s.registerRunLogSubscriber(r.Context(), projectID, sub) {
		stopHeartbeat()
		sub.closeWithStatus(websocket.CloseGoingAway, "project log stream unavailable")
		waitForWebSocketClose(conn)
		<-writerDone
		return
	}

	defer func() {
		stopHeartbeat()
		s.runLogSubMu.Lock()
		delete(s.runLogSubscribers[projectID], conn)
		s.runLogSubMu.Unlock()
		sub.close()
		_ = conn.Close()
		<-writerDone
	}()

	// 读取循环检测断开
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			logWebSocketDisconnect(path, err)
			return
		}
	}
}

// registerRunLogSubscriber adds a subscriber at the same log order point used
// by emitLog. History is queued before any later live entry can be enqueued.
func (s *Server) registerRunLogSubscriber(ctx context.Context, projectID string, sub *runLogSubscriber) bool {
	s.runManagersMu.Lock()
	defer s.runManagersMu.Unlock()
	if _, err := s.getProjectByID(ctx, projectID); err != nil {
		return false
	}
	runner := s.runManagers[projectID]
	if runner == nil {
		return s.addRunLogSubscriber(projectID, sub, nil)
	}
	registered := false
	runner.registerLogSubscriber(func(history []LogEntry) {
		registered = s.addRunLogSubscriber(projectID, sub, history)
	})
	return registered
}

// addRunLogSubscriber atomically checks the service lifecycle and records the
// subscription, so Server.Close either observes this connection or rejects it.
func (s *Server) addRunLogSubscriber(projectID string, sub *runLogSubscriber, history []LogEntry) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return false
	}
	s.runLogSubMu.Lock()
	defer s.runLogSubMu.Unlock()
	for _, entry := range history {
		sub.send <- entry
	}
	if s.runLogSubscribers[projectID] == nil {
		s.runLogSubscribers[projectID] = map[*websocket.Conn]*runLogSubscriber{}
	}
	s.runLogSubscribers[projectID][sub.conn] = sub
	return true
}

// broadcastRunLog enqueues logs without allowing a slow client to block a
// project process. A full queue disconnects that subscriber.
func (s *Server) broadcastRunLog(projectID string, entry LogEntry) {
	toClose := make([]*runLogSubscriber, 0)
	s.runLogSubMu.Lock()
	for conn, sub := range s.runLogSubscribers[projectID] {
		select {
		case sub.send <- entry:
		default:
			delete(s.runLogSubscribers[projectID], conn)
			toClose = append(toClose, sub)
		}
	}
	s.runLogSubMu.Unlock()
	for _, sub := range toClose {
		log.Printf("[ws] /ws/projects/%s/run subscriber queue full; closing slow client", projectID)
		go sub.closeWithStatus(websocket.CloseTryAgainLater, "client is too slow")
	}
}

func (s *Server) closeProjectRunLogSubscribers(projectID string) {
	s.runLogSubMu.Lock()
	subs := s.runLogSubscribers[projectID]
	delete(s.runLogSubscribers, projectID)
	s.runLogSubMu.Unlock()
	var closeWG sync.WaitGroup
	closeWG.Add(len(subs))
	for _, sub := range subs {
		go func(sub *runLogSubscriber) {
			defer closeWG.Done()
			sub.closeWithStatus(websocket.CloseGoingAway, "project log stream closed")
		}(sub)
	}
	closeWG.Wait()
}

func (s *Server) closeAllRunLogSubscribers() {
	s.runLogSubMu.Lock()
	all := make([]*runLogSubscriber, 0)
	for _, subs := range s.runLogSubscribers {
		for _, sub := range subs {
			all = append(all, sub)
		}
	}
	s.runLogSubscribers = map[string]map[*websocket.Conn]*runLogSubscriber{}
	s.runLogSubMu.Unlock()
	var closeWG sync.WaitGroup
	closeWG.Add(len(all))
	for _, sub := range all {
		go func(sub *runLogSubscriber) {
			defer closeWG.Done()
			sub.closeWithStatus(websocket.CloseGoingAway, "server shutting down")
		}(sub)
	}
	closeWG.Wait()
}
