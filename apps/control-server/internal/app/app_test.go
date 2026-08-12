package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type runnerFunc func(context.Context, AgentRunRequest, AgentRunSink) error

func (fn runnerFunc) Run(ctx context.Context, request AgentRunRequest, sink AgentRunSink) error {
	return fn(ctx, request, sink)
}

func (runnerFunc) Ready(context.Context) bool                        { return true }
func (runnerFunc) Version(context.Context) string                    { return "2.1.216" }
func (runnerFunc) CheckUpdate(context.Context) (bool, string, error) { return false, "", nil }
func (runnerFunc) Update(context.Context) (string, string, error)    { return "2.1.216", "2.1.217", nil }

type unavailableRunner struct{}

func (unavailableRunner) Ready(context.Context) bool                               { return false }
func (unavailableRunner) Run(context.Context, AgentRunRequest, AgentRunSink) error { return nil }
func (unavailableRunner) Version(context.Context) string                           { return "" }
func (unavailableRunner) CheckUpdate(context.Context) (bool, string, error)        { return false, "", nil }
func (unavailableRunner) Update(context.Context) (string, string, error)           { return "", "", nil }

type countingReadyRunner struct {
	ready bool
	calls int
}

func (runner *countingReadyRunner) Ready(context.Context) bool {
	runner.calls++
	return runner.ready
}
func (*countingReadyRunner) Run(context.Context, AgentRunRequest, AgentRunSink) error { return nil }
func (*countingReadyRunner) Version(context.Context) string                           { return "test" }
func (*countingReadyRunner) CheckUpdate(context.Context) (bool, string, error)        { return false, "", nil }
func (*countingReadyRunner) Update(context.Context) (string, string, error)           { return "", "", nil }

type updateTestRunner struct {
	checkAvailable bool
	latestVersion  string
	checkErr       error
	updateErr      error
	updateCalls    int
}

type blockingUpdateRunner struct {
	updateTestRunner
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

type requestContextUpdateRunner struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

type admissionBlockingRunner struct {
	readyStarted chan struct{}
	releaseReady chan struct{}
	once         sync.Once
}

func (runner *admissionBlockingRunner) Ready(context.Context) bool {
	runner.once.Do(func() { close(runner.readyStarted) })
	<-runner.releaseReady
	return true
}
func (runner *admissionBlockingRunner) Run(ctx context.Context, _ AgentRunRequest, _ AgentRunSink) error {
	<-ctx.Done()
	return ctx.Err()
}
func (*admissionBlockingRunner) Version(context.Context) string { return "2.1.216" }
func (*admissionBlockingRunner) CheckUpdate(context.Context) (bool, string, error) {
	return false, "", nil
}
func (*admissionBlockingRunner) Update(context.Context) (string, string, error) {
	return "2.1.216", "2.1.217", nil
}

func (runner *blockingUpdateRunner) Update(context.Context) (string, string, error) {
	runner.once.Do(func() { close(runner.started) })
	<-runner.release
	runner.updateCalls++
	return "2.1.216", "2.1.217", nil
}

func (*requestContextUpdateRunner) Ready(context.Context) bool { return true }
func (*requestContextUpdateRunner) Run(context.Context, AgentRunRequest, AgentRunSink) error {
	return nil
}
func (*requestContextUpdateRunner) Version(context.Context) string { return "0.145.0" }
func (*requestContextUpdateRunner) CheckUpdate(context.Context) (bool, string, error) {
	return false, "", nil
}
func (runner *requestContextUpdateRunner) Update(ctx context.Context) (string, string, error) {
	runner.once.Do(func() { close(runner.started) })
	select {
	case <-ctx.Done():
		return "0.145.0", "", ctx.Err()
	case <-runner.release:
		return "0.145.0", "0.146.0", nil
	}
}

func (*updateTestRunner) Ready(context.Context) bool                               { return true }
func (*updateTestRunner) Run(context.Context, AgentRunRequest, AgentRunSink) error { return nil }
func (*updateTestRunner) Version(context.Context) string                           { return "2.1.216" }
func (runner *updateTestRunner) CheckUpdate(context.Context) (bool, string, error) {
	return runner.checkAvailable, runner.latestVersion, runner.checkErr
}
func (runner *updateTestRunner) Update(context.Context) (string, string, error) {
	runner.updateCalls++
	if runner.updateErr != nil {
		return "2.1.216", "", runner.updateErr
	}
	return "2.1.216", "2.1.217", nil
}

type idleAgentSession struct {
	done chan error
	once sync.Once
}

type queuedStreamingRunner struct{ session *queuedAgentSession }

func (runner queuedStreamingRunner) Ready(context.Context) bool     { return true }
func (runner queuedStreamingRunner) Version(context.Context) string { return "2.1.216" }
func (runner queuedStreamingRunner) CheckUpdate(context.Context) (bool, string, error) {
	return false, "", nil
}
func (runner queuedStreamingRunner) Update(context.Context) (string, string, error) {
	return "2.1.216", "2.1.217", nil
}
func (runner queuedStreamingRunner) Run(context.Context, AgentRunRequest, AgentRunSink) error {
	return errors.New("one-shot run is not expected")
}
func (runner queuedStreamingRunner) StartSession(context.Context, AgentSessionRequest) (AgentSession, error) {
	return runner.session, nil
}

type queuedAgentTurn struct {
	request AgentRunRequest
	sink    AgentRunSink
}

type queuedAgentSession struct {
	done  chan error
	mu    sync.Mutex
	once  sync.Once
	turns []queuedAgentTurn
}

type blockingStopAgentSession struct {
	*queuedAgentSession
	stopEntered chan struct{}
	releaseStop chan struct{}
	stopOnce    sync.Once
}

type delayedDoneAgentSession struct {
	*queuedAgentSession
	stopped chan struct{}
	release chan struct{}
	once    sync.Once
}

func newDelayedDoneAgentSession() *delayedDoneAgentSession {
	return &delayedDoneAgentSession{
		queuedAgentSession: newQueuedAgentSession(),
		stopped:            make(chan struct{}),
		release:            make(chan struct{}),
	}
}

func (session *delayedDoneAgentSession) Stop() {
	session.once.Do(func() {
		close(session.stopped)
		go func() {
			<-session.release
			close(session.done)
		}()
	})
}

func newBlockingStopAgentSession() *blockingStopAgentSession {
	return &blockingStopAgentSession{
		queuedAgentSession: newQueuedAgentSession(),
		stopEntered:        make(chan struct{}),
		releaseStop:        make(chan struct{}),
	}
}

func (session *blockingStopAgentSession) Stop() {
	session.stopOnce.Do(func() {
		close(session.stopEntered)
		<-session.releaseStop
		close(session.done)
	})
}

type blockingStreamingRunner struct{ session AgentSession }

func (runner blockingStreamingRunner) Ready(context.Context) bool     { return true }
func (runner blockingStreamingRunner) Version(context.Context) string { return "2.1.216" }
func (runner blockingStreamingRunner) CheckUpdate(context.Context) (bool, string, error) {
	return false, "", nil
}
func (runner blockingStreamingRunner) Update(context.Context) (string, string, error) {
	return "2.1.216", "2.1.217", nil
}
func (runner blockingStreamingRunner) Run(context.Context, AgentRunRequest, AgentRunSink) error {
	return errors.New("one-shot run is not expected")
}
func (runner blockingStreamingRunner) StartSession(context.Context, AgentSessionRequest) (AgentSession, error) {
	return runner.session, nil
}

type setupBlockingStreamingRunner struct {
	started chan struct{}
	once    sync.Once
}

func (*setupBlockingStreamingRunner) Ready(context.Context) bool     { return true }
func (*setupBlockingStreamingRunner) Version(context.Context) string { return "2.1.216" }
func (*setupBlockingStreamingRunner) CheckUpdate(context.Context) (bool, string, error) {
	return false, "", nil
}
func (*setupBlockingStreamingRunner) Update(context.Context) (string, string, error) {
	return "2.1.216", "2.1.217", nil
}
func (*setupBlockingStreamingRunner) Run(context.Context, AgentRunRequest, AgentRunSink) error {
	return errors.New("one-shot run is not expected")
}
func (runner *setupBlockingStreamingRunner) StartSession(ctx context.Context, _ AgentSessionRequest) (AgentSession, error) {
	runner.once.Do(func() { close(runner.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

type returnedStreamingRunner struct {
	session    AgentSession
	returned   chan struct{}
	returnOnce sync.Once
}

func (*returnedStreamingRunner) Ready(context.Context) bool     { return true }
func (*returnedStreamingRunner) Version(context.Context) string { return "2.1.216" }
func (*returnedStreamingRunner) CheckUpdate(context.Context) (bool, string, error) {
	return false, "", nil
}
func (*returnedStreamingRunner) Update(context.Context) (string, string, error) {
	return "2.1.216", "2.1.217", nil
}
func (*returnedStreamingRunner) Run(context.Context, AgentRunRequest, AgentRunSink) error {
	return errors.New("one-shot run is not expected")
}
func (runner *returnedStreamingRunner) StartSession(context.Context, AgentSessionRequest) (AgentSession, error) {
	runner.returnOnce.Do(func() { close(runner.returned) })
	return runner.session, nil
}

type errBlockingContext struct {
	context.Context
	blockCall int
	entered   chan struct{}
	release   chan struct{}
	mu        sync.Mutex
	calls     int
	once      sync.Once
}

func (ctx *errBlockingContext) Err() error {
	ctx.mu.Lock()
	ctx.calls++
	shouldBlock := ctx.calls == ctx.blockCall
	ctx.mu.Unlock()
	if !shouldBlock {
		return nil
	}
	ctx.once.Do(func() { close(ctx.entered) })
	<-ctx.release
	return nil
}

func newQueuedAgentSession() *queuedAgentSession { return &queuedAgentSession{done: make(chan error)} }
func (session *queuedAgentSession) Send(request AgentRunRequest, sink AgentRunSink) error {
	session.mu.Lock()
	defer session.mu.Unlock()
	session.turns = append(session.turns, queuedAgentTurn{request: request, sink: sink})
	return nil
}
func (session *queuedAgentSession) Stop()              { session.once.Do(func() { close(session.done) }) }
func (session *queuedAgentSession) Done() <-chan error { return session.done }
func (session *queuedAgentSession) queuedCount() int {
	session.mu.Lock()
	defer session.mu.Unlock()
	return len(session.turns)
}
func (session *queuedAgentSession) finishNext(err error) bool {
	session.mu.Lock()
	if len(session.turns) == 0 {
		session.mu.Unlock()
		return false
	}
	turn := session.turns[0]
	session.turns = session.turns[1:]
	session.mu.Unlock()
	sink, ok := turn.sink.(AgentTurnSink)
	if !ok {
		return false
	}
	sink.TurnStarted()
	sink.TurnFinished(err)
	return true
}

func newIdleAgentSession() *idleAgentSession { return &idleAgentSession{done: make(chan error)} }

func (session *idleAgentSession) Send(AgentRunRequest, AgentRunSink) error { return nil }
func (session *idleAgentSession) Stop() {
	session.once.Do(func() { close(session.done) })
}
func (session *idleAgentSession) Done() <-chan error { return session.done }

type recordingSink struct {
	mu          sync.Mutex
	events      []string
	texts       []string
	parents     []string
	initialized int
	sessions    []string
}

func (sink *recordingSink) Event(eventType string, _ json.RawMessage) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.events = append(sink.events, eventType)
}

func (sink *recordingSink) AssistantText(content, parentToolUseID string) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.texts = append(sink.texts, content)
	sink.parents = append(sink.parents, parentToolUseID)
}

func (sink *recordingSink) SessionIdentified(sessionID string) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.sessions = append(sink.sessions, sessionID)
}

func (sink *recordingSink) SessionInitialized() {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.initialized++
}

type turnRecordingSink struct {
	mu       sync.Mutex
	started  int
	finished []error
}

func (sink *turnRecordingSink) Event(string, json.RawMessage) {}
func (sink *turnRecordingSink) AssistantText(string, string)  {}
func (sink *turnRecordingSink) SessionIdentified(string)      {}
func (sink *turnRecordingSink) SessionInitialized()           {}
func (sink *turnRecordingSink) TurnStarted() {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.started++
}
func (sink *turnRecordingSink) TurnFinished(err error) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.finished = append(sink.finished, err)
}

type rejectingWriteCloser struct{}

func (rejectingWriteCloser) Write([]byte) (int, error) { return 0, errors.New("write rejected") }
func (rejectingWriteCloser) Close() error              { return nil }

func newTestServer(t *testing.T) *Server {
	t.Helper()
	hookPath, err := filepath.Abs("../../../../scripts/claude-approval-hook.sh")
	if err != nil {
		t.Fatalf("resolve hook path: %v", err)
	}
	server, err := New(context.Background(), Config{
		DatabasePath: filepath.Join(t.TempDir(), "auto.db"),
		AllowedRoot:  t.TempDir(),
		ClaudePath:   "claude",
		ControlURL:   "http://127.0.0.1:8080",
		ApprovalHook: hookPath,
	})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	t.Cleanup(server.Close)
	return server
}

func TestDecodeJSONRejectsOversizedBody(t *testing.T) {
	body := `{"value":"` + strings.Repeat("x", maxJSONBodyBytes) + `"}`
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	response := httptest.NewRecorder()
	var target map[string]string
	if decode(response, request, &target) {
		t.Fatal("oversized JSON body was accepted")
	}
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status: got %d want %d", response.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestHTTPServerUsesBoundedRequestSettings(t *testing.T) {
	server := newTestServer(t)
	httpServer := server.newHTTPServer()
	if httpServer.ReadHeaderTimeout != httpReadHeaderTimeout {
		t.Fatalf("ReadHeaderTimeout: got %s want %s", httpServer.ReadHeaderTimeout, httpReadHeaderTimeout)
	}
	if httpServer.ReadTimeout != httpReadTimeout {
		t.Fatalf("ReadTimeout: got %s want %s", httpServer.ReadTimeout, httpReadTimeout)
	}
	if httpServer.IdleTimeout != httpIdleTimeout {
		t.Fatalf("IdleTimeout: got %s want %s", httpServer.IdleTimeout, httpIdleTimeout)
	}
	if httpServer.MaxHeaderBytes != maxHTTPHeaderBytes {
		t.Fatalf("MaxHeaderBytes: got %d want %d", httpServer.MaxHeaderBytes, maxHTTPHeaderBytes)
	}
}

func TestFinishRunUpdatesRunAndConversation(t *testing.T) {
	server := newTestServer(t)

	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,claude_initialized,is_current,created_at) values ('conversation','project','00000000-0000-4000-8000-000000000000','running',0,1,?)`, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values ('run','conversation','running',?)`, now); err != nil {
		t.Fatalf("insert run: %v", err)
	}

	server.finishRun("run", "conversation", "completed", nil)

	var runStatus, conversationStatus string
	if err := server.db.QueryRow(`select status from runs where id='run'`).Scan(&runStatus); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	if err := server.db.QueryRow(`select status from conversations where id='conversation'`).Scan(&conversationStatus); err != nil {
		t.Fatalf("read conversation status: %v", err)
	}
	if runStatus != "completed" || conversationStatus != "idle" {
		t.Fatalf("unexpected statuses: run=%q conversation=%q", runStatus, conversationStatus)
	}
}

func TestAssistantTextBroadcastsDurableMessageEvent(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,claude_initialized,is_current,created_at) values ('conversation','project','00000000-0000-4000-8000-000000000000','running',0,1,?)`, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values ('run','conversation','running',?)`, now); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	sink := &agentRunSink{server: server, runID: "run", conversationID: "conversation"}
	sink.AssistantText("live reply", "")

	var eventType string
	var payload []byte
	if err := server.db.QueryRow(`select type,payload from events where conversation_id='conversation' order by created_at desc limit 1`).Scan(&eventType, &payload); err != nil {
		t.Fatalf("load assistant event: %v", err)
	}
	if eventType != "assistant.message" {
		t.Fatalf("event type=%q, want assistant.message", eventType)
	}
	var message Message
	if err := json.Unmarshal(payload, &message); err != nil {
		t.Fatalf("decode assistant event: %v", err)
	}
	if message.Content != "live reply" || message.RunID != "run" || message.Role != "assistant" {
		t.Fatalf("broadcast message=%+v", message)
	}
}

func TestCreateCodexConversationPersistsAgentAndPolicy(t *testing.T) {
	server := newTestServer(t)
	server.codexRunner = runnerFunc(func(context.Context, AgentRunRequest, AgentRunSink) error { return nil })
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/projects/project/conversations?new=true", strings.NewReader(`{"agentId":"codex","permissionMode":"workspace_write"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	var conversation Conversation
	if err := json.NewDecoder(response.Body).Decode(&conversation); err != nil {
		t.Fatalf("decode conversation: %v", err)
	}
	if conversation.AgentID != "codex" || conversation.PermissionMode != "workspace_write" || conversation.ExecutionPolicy != "workspace_write" || conversation.AgentRuntimeID != "wsl-local" || conversation.AgentSessionID == "" {
		t.Fatalf("conversation=%#v", conversation)
	}
}

func TestCreateCodexFullControlConversation(t *testing.T) {
	server := newTestServer(t)
	server.codexRunner = runnerFunc(func(context.Context, AgentRunRequest, AgentRunSink) error { return nil })
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/projects/project/conversations?new=true", strings.NewReader(`{"agentId":"codex","permissionMode":"full_control"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	var conversation Conversation
	if err := json.NewDecoder(response.Body).Decode(&conversation); err != nil {
		t.Fatalf("decode conversation: %v", err)
	}
	if conversation.AgentID != "codex" || conversation.PermissionMode != "full_control" || conversation.ExecutionPolicy != "full_control" {
		t.Fatalf("conversation=%#v", conversation)
	}
}

func TestCodexReadyAllowsProjectLoadWithoutClaude(t *testing.T) {
	server := newTestServer(t)
	server.runner = unavailableRunner{}
	server.codexRunner = runnerFunc(func(context.Context, AgentRunRequest, AgentRunSink) error { return nil })
	projectPath := server.config.AllowedRoot

	validate := httptest.NewRecorder()
	server.routes().ServeHTTP(validate, httptest.NewRequest(http.MethodPost, "/api/projects/validate", strings.NewReader(`{"path":"`+projectPath+`"}`)))
	if validate.Code != http.StatusOK {
		t.Fatalf("validate status=%d body=%s", validate.Code, validate.Body.String())
	}
	var result struct {
		ClaudeReady bool `json:"claudeReady"`
		CodexReady  bool `json:"codexReady"`
		AgentReady  bool `json:"agentReady"`
	}
	if err := json.NewDecoder(validate.Body).Decode(&result); err != nil {
		t.Fatalf("decode validation: %v", err)
	}
	if result.ClaudeReady || !result.CodexReady || !result.AgentReady {
		t.Fatalf("validation=%#v", result)
	}

	create := httptest.NewRecorder()
	server.routes().ServeHTTP(create, httptest.NewRequest(http.MethodPost, "/api/projects", strings.NewReader(`{"path":"`+projectPath+`"}`)))
	if create.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	var project Project
	if err := json.NewDecoder(create.Body).Decode(&project); err != nil {
		t.Fatalf("decode project: %v", err)
	}
	if project.ClaudeReady || !project.CodexReady || !project.AgentReady {
		t.Fatalf("project=%#v", project)
	}

	existing := httptest.NewRecorder()
	server.routes().ServeHTTP(existing, httptest.NewRequest(http.MethodPost, "/api/projects", strings.NewReader(`{"path":"`+projectPath+`"}`)))
	if existing.Code != http.StatusOK {
		t.Fatalf("repeat create status=%d body=%s", existing.Code, existing.Body.String())
	}
	if err := json.NewDecoder(existing.Body).Decode(&project); err != nil {
		t.Fatalf("decode existing project: %v", err)
	}
	if project.ClaudeReady || !project.CodexReady || !project.AgentReady {
		t.Fatalf("existing project=%#v", project)
	}
}

func TestListProjectsChecksCodexOnce(t *testing.T) {
	server := newTestServer(t)
	codex := &countingReadyRunner{ready: true}
	server.codexRunner = codex
	now := time.Now().UTC()
	for index := 0; index < 3; index++ {
		projectID := fmt.Sprintf("project-%d", index)
		if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values (?,?,?,?,?,?,?)`, projectID, projectID, filepath.Join(server.config.AllowedRoot, projectID), "wsl-local", "main", 0, now); err != nil {
			t.Fatalf("insert project %d: %v", index, err)
		}
	}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/projects", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", response.Code, response.Body.String())
	}
	if codex.calls != 1 {
		t.Fatalf("Codex readiness checks=%d, want 1", codex.calls)
	}
}

func TestUnavailableCodexCannotCreateConversation(t *testing.T) {
	server := newTestServer(t)
	server.codexRunner = unavailableRunner{}
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, server.config.AllowedRoot, now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/project/conversations?new=true", strings.NewReader(`{"agentId":"codex","permissionMode":"workspace_write"}`)))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	var count int
	if err := server.db.QueryRow(`select count(*) from conversations where project_id='project'`).Scan(&count); err != nil {
		t.Fatalf("count conversations: %v", err)
	}
	if count != 0 {
		t.Fatalf("created unavailable Codex conversation count=%d", count)
	}
}

func TestCodexUsageCollectedFromEvents(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, server.config.AllowedRoot, now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,agent_id,agent_session_id,agent_runtime_id,execution_policy,status,permission_mode,title,last_activity_at,claude_initialized,agent_initialized,is_current,created_at) values ('conversation','project','legacy','codex','thread-1','wsl-local','read_only','idle','read_only','Codex',?,0,1,1,?)`, now, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	server.codexRunner = runnerFunc(func(_ context.Context, _ AgentRunRequest, sink AgentRunSink) error {
		// Two turns with independent per-turn usage. The accumulator must
		// sum them, not overwrite with the last turn's values.
		sink.Event("turn.completed", json.RawMessage(`{"usage":{"input_tokens":1000,"output_tokens":50,"cached_input_tokens":800,"cache_write_input_tokens":0}}`))
		sink.Event("turn.completed", json.RawMessage(`{"usage":{"input_tokens":3000,"output_tokens":150,"cached_input_tokens":2000,"cache_write_input_tokens":100}}`))
		return nil
	})
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/conversations/conversation/messages", strings.NewReader(`{"content":"检查项目"}`)))
	if response.Code != http.StatusAccepted {
		t.Fatalf("send status=%d body=%s", response.Code, response.Body.String())
	}
	waitForConversationIdle(t, server, "conversation")
	var runID string
	if err := server.db.QueryRow(`select id from runs where conversation_id='conversation'`).Scan(&runID); err != nil {
		t.Fatalf("read run: %v", err)
	}
	var usageRows int
	if err := server.db.QueryRow(`select count(*) from run_usage where run_id=?`, runID).Scan(&usageRows); err != nil {
		t.Fatalf("count usage: %v", err)
	}
	if usageRows != 1 {
		t.Fatalf("Codex usage rows=%d, want 1 (turn.completed should persist usage)", usageRows)
	}
	response = httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/conversations/conversation/usage", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("conversation usage status=%d body=%s", response.Code, response.Body.String())
	}
	var usage ConversationUsageResponse
	if err := json.NewDecoder(response.Body).Decode(&usage); err != nil {
		t.Fatalf("decode usage: %v", err)
	}
	if !usage.Available {
		t.Fatalf("Codex usage should be available, got reason=%q", usage.Reason)
	}
	// Two turns: 1000 + 3000 = 4000 input, 50 + 150 = 200 output.
	if usage.Session.InputTokens != 4000 {
		t.Fatalf("session input tokens=%d, want 4000 (sum of two turns)", usage.Session.InputTokens)
	}
	if usage.Session.OutputTokens != 200 {
		t.Fatalf("session output tokens=%d, want 200 (sum of two turns)", usage.Session.OutputTokens)
	}
	if usage.Session.CacheReadTokens != 2800 {
		t.Fatalf("session cache read tokens=%d, want 2800 (800+2000)", usage.Session.CacheReadTokens)
	}
	if usage.Session.CacheCreationTokens != 100 {
		t.Fatalf("session cache creation tokens=%d, want 100 (0+100)", usage.Session.CacheCreationTokens)
	}
	if usage.Session.AgentTurns != 2 {
		t.Fatalf("session agent turns=%d, want 2", usage.Session.AgentTurns)
	}
}

func TestActivateCodexConversationReturnsAgentMetadata(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,agent_id,agent_session_id,agent_runtime_id,execution_policy,status,permission_mode,title,last_activity_at,claude_initialized,agent_initialized,is_current,created_at) values ('codex','project','legacy-codex','codex','thread-codex','wsl-local','read_only','idle','read_only','Codex',?,0,1,0,?),('claude','project','legacy-claude','claude-code','legacy-claude','wsl-local','approval_required','idle','approval_required','Claude',?,1,1,1,?)`, now, now, now, now); err != nil {
		t.Fatalf("insert conversations: %v", err)
	}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/conversations/codex/activate", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("activate status=%d body=%s", response.Code, response.Body.String())
	}
	var conversation Conversation
	if err := json.NewDecoder(response.Body).Decode(&conversation); err != nil {
		t.Fatalf("decode conversation: %v", err)
	}
	if conversation.AgentID != "codex" || conversation.AgentSessionID != "thread-codex" || conversation.ExecutionPolicy != "read_only" || !conversation.IsCurrent {
		t.Fatalf("activated conversation=%#v", conversation)
	}
}

func TestCodexRunSnapshotsAgentRuntimeAndPolicy(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,agent_id,agent_session_id,agent_runtime_id,execution_policy,status,permission_mode,title,last_activity_at,claude_initialized,agent_initialized,is_current,created_at) values ('conversation','project','legacy','codex','pending-thread','wsl-local','read_only','idle','read_only','Codex',?,0,0,1,?)`, now, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	server.codexRunner = runnerFunc(func(_ context.Context, request AgentRunRequest, sink AgentRunSink) error {
		if request.Resume || request.SessionID != "pending-thread" || request.PermissionMode != "read_only" {
			t.Fatalf("unexpected Codex request: %#v", request)
		}
		sink.SessionIdentified("thread-1")
		sink.SessionInitialized()
		return nil
	})
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/conversations/conversation/messages", strings.NewReader(`{"content":"检查项目"}`)))
	if response.Code != http.StatusAccepted {
		t.Fatalf("send status=%d body=%s", response.Code, response.Body.String())
	}
	waitForConversationIdle(t, server, "conversation")
	var agentID, runtimeID, policy, agentRunID string
	if err := server.db.QueryRow(`select agent_id,agent_runtime_id,execution_policy,agent_run_id from runs where conversation_id='conversation'`).Scan(&agentID, &runtimeID, &policy, &agentRunID); err != nil {
		t.Fatalf("read run snapshot: %v", err)
	}
	if agentID != "codex" || runtimeID != "wsl-local" || policy != "read_only" || agentRunID != "thread-1" {
		t.Fatalf("unexpected run snapshot: agent=%q runtime=%q policy=%q agentRunID=%q", agentID, runtimeID, policy, agentRunID)
	}
}

func TestRunSnapshotUsesEffectiveExecutionPolicy(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,agent_id,agent_session_id,agent_runtime_id,execution_policy,status,permission_mode,title,last_activity_at,claude_initialized,agent_initialized,is_current,created_at) values ('conversation','project','00000000-0000-4000-8000-000000000000','claude-code','','wsl-local','approval_required','idle','full_control','Claude',?,0,0,1,?)`, now, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	server.runner = runnerFunc(func(_ context.Context, request AgentRunRequest, _ AgentRunSink) error {
		if request.PermissionMode != "full_control" {
			t.Fatalf("execution policy = %q", request.PermissionMode)
		}
		return nil
	})
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/conversations/conversation/messages", strings.NewReader(`{"content":"继续旧会话"}`)))
	if response.Code != http.StatusAccepted {
		t.Fatalf("send status=%d body=%s", response.Code, response.Body.String())
	}
	waitForConversationIdle(t, server, "conversation")
	var policy string
	if err := server.db.QueryRow(`select execution_policy from runs where conversation_id='conversation'`).Scan(&policy); err != nil {
		t.Fatalf("read run snapshot: %v", err)
	}
	if policy != "full_control" {
		t.Fatalf("run policy=%q, want full_control", policy)
	}
}

func TestMigratePreservesRunSnapshotWithoutAgentRunID(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "auto.db")
	hookPath, err := filepath.Abs("../../../../scripts/claude-approval-hook.sh")
	if err != nil {
		t.Fatalf("resolve hook path: %v", err)
	}
	config := Config{DatabasePath: databasePath, AllowedRoot: t.TempDir(), ClaudePath: "claude", ControlURL: "http://127.0.0.1:8080", ApprovalHook: hookPath}
	server, err := New(context.Background(), config)
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		server.Close()
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,agent_id,agent_session_id,agent_runtime_id,execution_policy,status,permission_mode,title,last_activity_at,claude_initialized,agent_initialized,is_current,created_at) values ('conversation','project','legacy','codex','thread-1','wsl-local','workspace_write','idle','workspace_write','Codex',?,0,1,1,?)`, now, now); err != nil {
		server.Close()
		t.Fatalf("insert conversation: %v", err)
	}
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,agent_id,agent_runtime_id,execution_policy,agent_run_id,status,created_at,completed_at) values ('run','conversation','codex','wsl-local','read_only','','failed',?,?)`, now, now); err != nil {
		server.Close()
		t.Fatalf("insert run: %v", err)
	}
	server.Close()

	reopened, err := New(context.Background(), config)
	if err != nil {
		t.Fatalf("reopen server: %v", err)
	}
	t.Cleanup(reopened.Close)
	var agentID, runtimeID, policy, agentRunID string
	if err := reopened.db.QueryRow(`select agent_id,agent_runtime_id,execution_policy,agent_run_id from runs where id='run'`).Scan(&agentID, &runtimeID, &policy, &agentRunID); err != nil {
		t.Fatalf("read run snapshot: %v", err)
	}
	if agentID != "codex" || runtimeID != "wsl-local" || policy != "read_only" || agentRunID != "" {
		t.Fatalf("run snapshot changed: agent=%q runtime=%q policy=%q runID=%q", agentID, runtimeID, policy, agentRunID)
	}
}

func TestRunUsageDeduplicatesEventsAndPersistsConversationSummary(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,claude_initialized,is_current,created_at) values ('conversation','project','00000000-0000-4000-8000-000000000000','running',0,1,?)`, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values ('run','conversation','running',?)`, now); err != nil {
		t.Fatalf("insert run: %v", err)
	}

	server.beginRunUsage("run", "conversation")
	mainStep := json.RawMessage(`{"type":"assistant","message":{"id":"main-step","model":"claude-sonnet","usage":{"input_tokens":48000,"cache_read_input_tokens":1200,"cache_creation_input_tokens":300},"content":[{"type":"tool_use","id":"tool-1"}]}}`)
	server.collectUsageEvent("run", "conversation", "assistant", mainStep)
	server.collectUsageEvent("run", "conversation", "assistant", mainStep)
	subagentStep := json.RawMessage(`{"type":"assistant","parent_tool_use_id":"tool-1","message":{"id":"subagent-step","model":"claude-haiku","usage":{"input_tokens":1200},"content":[{"type":"tool_use","id":"tool-2"}]}}`)
	server.collectUsageEvent("run", "conversation", "assistant", subagentStep)
	server.collectUsageEvent("run", "conversation", "assistant", subagentStep)
	if live := server.liveRunUsage("run", "running"); live == nil || live.ContextInputTokens != 49500 {
		t.Fatalf("unexpected live context snapshot: %#v", live)
	}
	result := json.RawMessage(`{"type":"result","num_turns":3,"duration_ms":4200,"ttft_ms":700,"total_cost_usd":0.1234,"terminal_reason":"end_turn","usage":{"input_tokens":50000,"output_tokens":2000,"cache_read_input_tokens":3000,"cache_creation_input_tokens":400},"modelUsage":{"claude-sonnet":{"inputTokens":48000,"outputTokens":1800,"cacheReadInputTokens":2800,"cacheCreationInputTokens":400,"costUSD":0.11,"contextWindow":200000},"claude-haiku":{"inputTokens":2000,"outputTokens":200,"cacheReadInputTokens":200,"cacheCreationInputTokens":0,"costUSD":0.0134,"contextWindow":200000}}}`)
	server.collectUsageEvent("run", "conversation", "result", result)
	if err := server.persistRunUsage("run", "completed"); err != nil {
		t.Fatalf("persist usage: %v", err)
	}
	server.finishRun("run", "conversation", "completed", nil)
	server.discardRunUsage("run")

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/conversations/conversation/usage", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("get conversation usage status: %d body=%s", response.Code, response.Body.String())
	}
	var usage ConversationUsageResponse
	if err := json.NewDecoder(response.Body).Decode(&usage); err != nil {
		t.Fatalf("decode conversation usage: %v", err)
	}
	if usage.Session.TaskCount != 1 || usage.Session.AgentTurns != 3 || usage.Session.ModelSteps != 1 || usage.Session.ToolCalls != 2 || usage.Session.SubagentCount != 1 {
		t.Fatalf("unexpected summary: %#v", usage.Session)
	}
	if usage.Context.ContextInputTokens != 49500 || usage.Context.ContextWindow != 200000 || usage.Context.EstimatedCostUSD != 0.1234 || len(usage.Models) != 2 {
		t.Fatalf("unexpected context or models: %#v models=%#v", usage.Context, usage.Models)
	}
}

func TestRunUsageDoesNotUseCumulativeResultUsageAsContextSnapshot(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,claude_initialized,is_current,created_at) values ('conversation','project','session','running',0,1,?)`, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values ('run','conversation','running',?)`, now); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	server.beginRunUsage("run", "conversation")

	server.collectUsageEvent("run", "conversation", "assistant", json.RawMessage(`{"type":"assistant","message":{"id":"streamed-step","model":"claude-sonnet","usage":{},"content":[]}}`))
	server.collectUsageEvent("run", "conversation", "result", json.RawMessage(`{"type":"result","num_turns":3,"usage":{"input_tokens":1000,"cache_read_input_tokens":150000,"cache_creation_input_tokens":2000},"modelUsage":{"claude-sonnet":{"contextWindow":200000}}}`))

	usage := server.liveRunUsage("run", "running")
	if usage == nil || usage.ContextInputTokens != 0 || usage.ContextWindow != 200000 {
		t.Fatalf("live usage=%#v, want no fabricated context snapshot", usage)
	}
}

func TestBeginRunUsageDoesNotReportPriorRunContextAsCurrent(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,claude_initialized,is_current,created_at) values ('conversation','project','session','idle',0,1,?)`, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at,completed_at) values ('previous','conversation','completed',?,?),('current','conversation','running',?,null)`, now, now, now); err != nil {
		t.Fatalf("insert runs: %v", err)
	}
	if _, err := server.db.Exec(`insert into run_usage (run_id,conversation_id,context_input_tokens,completed_at) values ('previous','conversation',48000,?)`, now); err != nil {
		t.Fatalf("insert prior usage: %v", err)
	}

	server.beginRunUsage("current", "conversation")
	usage := server.liveRunUsage("current", "running")
	if usage == nil || usage.ContextInputTokens != 0 {
		t.Fatalf("live usage=%#v, want no stale context snapshot", usage)
	}
}

func TestConversationUsageDoesNotDoubleCountPersistedRunningUsage(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,claude_initialized,is_current,created_at) values ('conversation','project','session','running',0,1,?)`, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values ('run','conversation','running',?)`, now); err != nil {
		t.Fatalf("insert run: %v", err)
	}

	server.beginRunUsage("run", "conversation")
	server.collectUsageEvent("run", "conversation", "result", json.RawMessage(`{"type":"result","num_turns":2,"total_cost_usd":0.01,"usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":500,"cache_creation_input_tokens":10},"modelUsage":{"claude-sonnet":{"inputTokens":100,"outputTokens":20,"cacheReadInputTokens":500,"cacheCreationInputTokens":10,"costUSD":0.01,"contextWindow":200000}}}`))
	if err := server.persistRunUsage("run", "completed"); err != nil {
		t.Fatalf("persist usage: %v", err)
	}

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/conversations/conversation/usage", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("get conversation usage status: %d body=%s", response.Code, response.Body.String())
	}
	var usage ConversationUsageResponse
	if err := json.NewDecoder(response.Body).Decode(&usage); err != nil {
		t.Fatalf("decode conversation usage: %v", err)
	}
	if usage.Session.AgentTurns != 2 || usage.Session.InputTokens != 100 || usage.Session.OutputTokens != 20 || usage.Session.CacheReadTokens != 500 || usage.Session.EstimatedCostUSD != 0.01 {
		t.Fatalf("persisted running usage was counted more than once: %#v", usage.Session)
	}
	if len(usage.Models) != 1 || usage.Models[0].Model != "claude-sonnet" || usage.Models[0].InputTokens != 100 || usage.Models[0].OutputTokens != 20 || usage.Models[0].CacheReadTokens != 500 || usage.Models[0].CacheCreationTokens != 10 || usage.Models[0].EstimatedCostUSD != 0.01 {
		t.Fatalf("persisted running model usage was counted more than once: %#v", usage.Models)
	}
}

func TestConversationUsageDoesNotFallBackToPriorRunAfterUsagePersistenceFailure(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	prior := now.Add(-time.Minute)
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), prior); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,claude_initialized,is_current,created_at) values ('conversation','project','session','idle',0,1,?)`, prior); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at,completed_at) values ('prior','conversation','completed',?,?),('failed','conversation','completed',?,?)`, prior, prior, now, now); err != nil {
		t.Fatalf("insert runs: %v", err)
	}
	if _, err := server.db.Exec(`insert into run_usage (run_id,conversation_id,model,context_window,context_input_tokens,input_tokens,has_result,completed_at) values ('prior','conversation','claude-sonnet',200000,48000,100,1,?)`, prior); err != nil {
		t.Fatalf("insert prior usage: %v", err)
	}

	conversationResponse := httptest.NewRecorder()
	server.routes().ServeHTTP(conversationResponse, httptest.NewRequest(http.MethodGet, "/api/conversations/conversation/usage", nil))
	if conversationResponse.Code != http.StatusOK {
		t.Fatalf("get conversation usage status: %d body=%s", conversationResponse.Code, conversationResponse.Body.String())
	}
	var conversationUsage ConversationUsageResponse
	if err := json.NewDecoder(conversationResponse.Body).Decode(&conversationUsage); err != nil {
		t.Fatalf("decode conversation usage: %v", err)
	}
	if conversationUsage.LatestRun == nil || conversationUsage.LatestRun.RunID != "failed" || conversationUsage.LatestRun.Available || conversationUsage.LatestRun.Reason == "" {
		t.Fatalf("latest run fell back to prior usage: %#v", conversationUsage.LatestRun)
	}
	if conversationUsage.Context.RunID != "failed" || conversationUsage.Context.Available || conversationUsage.Context.ContextInputTokens != 0 {
		t.Fatalf("context fell back to prior usage: %#v", conversationUsage.Context)
	}

	runResponse := httptest.NewRecorder()
	server.routes().ServeHTTP(runResponse, httptest.NewRequest(http.MethodGet, "/api/runs/failed/usage", nil))
	if runResponse.Code != http.StatusOK {
		t.Fatalf("get failed run usage status: %d body=%s", runResponse.Code, runResponse.Body.String())
	}
	var runUsage RunUsage
	if err := json.NewDecoder(runResponse.Body).Decode(&runUsage); err != nil {
		t.Fatalf("decode failed run usage: %v", err)
	}
	if runUsage.RunID != "failed" || runUsage.Available || runUsage.Reason == "" {
		t.Fatalf("failed run usage=%#v", runUsage)
	}
}

func TestClaudeUpdateEndpointsValidateRunnerAndActiveRuns(t *testing.T) {
	server := newTestServer(t)
	runner := &updateTestRunner{checkAvailable: true, latestVersion: "2.1.217"}
	server.runnerRegistry.register("update-runner", runner, RunnerMeta{ID: "update-runner"})

	missing := httptest.NewRecorder()
	server.routes().ServeHTTP(missing, httptest.NewRequest(http.MethodPost, "/api/runners/missing/claude/check-update", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing runner status=%d body=%s", missing.Code, missing.Body.String())
	}

	check := httptest.NewRecorder()
	server.routes().ServeHTTP(check, httptest.NewRequest(http.MethodPost, "/api/runners/update-runner/claude/check-update", nil))
	if check.Code != http.StatusOK {
		t.Fatalf("check update status=%d body=%s", check.Code, check.Body.String())
	}
	var checkResult struct {
		UpdateAvailable bool   `json:"updateAvailable"`
		CurrentVersion  string `json:"currentVersion"`
		LatestVersion   string `json:"latestVersion"`
	}
	if err := json.NewDecoder(check.Body).Decode(&checkResult); err != nil {
		t.Fatalf("decode check update: %v", err)
	}
	if !checkResult.UpdateAvailable || checkResult.CurrentVersion != "2.1.216" || checkResult.LatestVersion != "2.1.217" {
		t.Fatalf("check result=%#v", checkResult)
	}

	update := httptest.NewRecorder()
	server.routes().ServeHTTP(update, httptest.NewRequest(http.MethodPost, "/api/runners/update-runner/claude/update", nil))
	if update.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", update.Code, update.Body.String())
	}
	var updateResult struct {
		Success         bool   `json:"success"`
		PreviousVersion string `json:"previousVersion"`
		CurrentVersion  string `json:"currentVersion"`
	}
	if err := json.NewDecoder(update.Body).Decode(&updateResult); err != nil {
		t.Fatalf("decode update: %v", err)
	}
	if !updateResult.Success || updateResult.PreviousVersion != "2.1.216" || updateResult.CurrentVersion != "2.1.217" || runner.updateCalls != 1 {
		t.Fatalf("update result=%#v calls=%d", updateResult, runner.updateCalls)
	}

	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'update-runner','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,agent_runtime_id,status,claude_initialized,is_current,created_at) values ('conversation','project','session','update-runner','running',0,1,?)`, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,agent_runtime_id,status,created_at) values ('active','conversation','update-runner','running',?)`, now); err != nil {
		t.Fatalf("insert active run: %v", err)
	}

	blocked := httptest.NewRecorder()
	server.routes().ServeHTTP(blocked, httptest.NewRequest(http.MethodPost, "/api/runners/update-runner/claude/update", nil))
	if blocked.Code != http.StatusConflict || runner.updateCalls != 1 {
		t.Fatalf("blocked update status=%d calls=%d body=%s", blocked.Code, runner.updateCalls, blocked.Body.String())
	}

	if _, err := server.db.Exec(`update runs set status='completed' where id='active'`); err != nil {
		t.Fatalf("complete active run: %v", err)
	}
	runner.updateErr = errors.New("update failed")
	failed := httptest.NewRecorder()
	server.routes().ServeHTTP(failed, httptest.NewRequest(http.MethodPost, "/api/runners/update-runner/claude/update", nil))
	if failed.Code != http.StatusInternalServerError || runner.updateCalls != 2 {
		t.Fatalf("failed update status=%d calls=%d body=%s", failed.Code, runner.updateCalls, failed.Body.String())
	}
	var failureResult struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if err := json.NewDecoder(failed.Body).Decode(&failureResult); err != nil {
		t.Fatalf("decode failed update: %v", err)
	}
	if failureResult.Success || failureResult.Error != "任务执行失败，请查看任务日志后重试。：update failed" {
		t.Fatalf("failure result=%#v", failureResult)
	}
}

func TestCodexUpdateEndpointsUseLocalCodexRunner(t *testing.T) {
	server := newTestServer(t)
	codex := &updateTestRunner{checkAvailable: true, latestVersion: "0.146.0"}
	server.codexRunner = codex

	unsupported := httptest.NewRecorder()
	server.routes().ServeHTTP(unsupported, httptest.NewRequest(http.MethodPost, "/api/runners/ssh-example/codex/check-update", nil))
	if unsupported.Code != http.StatusNotFound {
		t.Fatalf("unsupported Codex runner status=%d body=%s", unsupported.Code, unsupported.Body.String())
	}

	check := httptest.NewRecorder()
	server.routes().ServeHTTP(check, httptest.NewRequest(http.MethodPost, "/api/runners/wsl-local/codex/check-update", nil))
	if check.Code != http.StatusOK {
		t.Fatalf("check Codex update status=%d body=%s", check.Code, check.Body.String())
	}
	var checkResult struct {
		UpdateAvailable bool   `json:"updateAvailable"`
		CurrentVersion  string `json:"currentVersion"`
		LatestVersion   string `json:"latestVersion"`
	}
	if err := json.NewDecoder(check.Body).Decode(&checkResult); err != nil {
		t.Fatalf("decode Codex check update: %v", err)
	}
	if !checkResult.UpdateAvailable || checkResult.CurrentVersion != "2.1.216" || checkResult.LatestVersion != "0.146.0" {
		t.Fatalf("Codex check result=%#v", checkResult)
	}

	update := httptest.NewRecorder()
	server.routes().ServeHTTP(update, httptest.NewRequest(http.MethodPost, "/api/runners/wsl-local/codex/update", nil))
	if update.Code != http.StatusOK || codex.updateCalls != 1 {
		t.Fatalf("update Codex status=%d calls=%d body=%s", update.Code, codex.updateCalls, update.Body.String())
	}
}

func TestCodexUpdateIsIsolatedFromClaudeSessions(t *testing.T) {
	server := newTestServer(t)
	codex := &blockingUpdateRunner{started: make(chan struct{}), release: make(chan struct{})}
	server.codexRunner = codex
	server.runner = runnerFunc(func(context.Context, AgentRunRequest, AgentRunSink) error { return nil })
	server.runnerRegistry.register("wsl-local", server.runner, server.wslLocalMeta())
	server.mu.Lock()
	server.sessions["claude"] = &activeAgentSession{agent: &idleAgentSession{done: make(chan error)}, runnerID: "wsl-local", agentID: "claude-code"}
	server.mu.Unlock()

	update := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		server.routes().ServeHTTP(update, httptest.NewRequest(http.MethodPost, "/api/runners/wsl-local/codex/update", nil))
		close(done)
	}()
	select {
	case <-codex.started:
	case <-time.After(time.Second):
		t.Fatal("Codex update was incorrectly blocked by a Claude session")
	}

	status := httptest.NewRecorder()
	server.routes().ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/api/runners", nil))
	var runners []struct {
		ID     string `json:"id"`
		Claude struct {
			Status string `json:"status"`
		} `json:"claude"`
		Codex struct {
			Status string `json:"status"`
		} `json:"codex"`
	}
	if err := json.NewDecoder(status.Body).Decode(&runners); err != nil {
		t.Fatalf("decode runner status: %v", err)
	}
	if len(runners) != 1 || runners[0].Claude.Status == "updating" || runners[0].Codex.Status != "updating" {
		t.Fatalf("unexpected isolated update status: %#v", runners)
	}

	close(codex.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Codex update did not finish")
	}
	if update.Code != http.StatusOK {
		t.Fatalf("Codex update status=%d body=%s", update.Code, update.Body.String())
	}
}

func TestRunnerUpdatesAreSerializedAcrossAgents(t *testing.T) {
	server := newTestServer(t)
	codex := &blockingUpdateRunner{started: make(chan struct{}), release: make(chan struct{})}
	claude := &updateTestRunner{}
	server.codexRunner = codex
	server.runner = claude
	server.runnerRegistry.register("wsl-local", claude, server.wslLocalMeta())

	codexUpdate := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		server.routes().ServeHTTP(codexUpdate, httptest.NewRequest(http.MethodPost, "/api/runners/wsl-local/codex/update", nil))
		close(done)
	}()
	select {
	case <-codex.started:
	case <-time.After(time.Second):
		t.Fatal("Codex update did not start")
	}

	claudeUpdate := httptest.NewRecorder()
	server.routes().ServeHTTP(claudeUpdate, httptest.NewRequest(http.MethodPost, "/api/runners/wsl-local/claude/update", nil))
	if claudeUpdate.Code != http.StatusConflict || claude.updateCalls != 0 {
		t.Fatalf("Claude update must wait for Codex update: status=%d calls=%d body=%s", claudeUpdate.Code, claude.updateCalls, claudeUpdate.Body.String())
	}

	close(codex.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Codex update did not finish")
	}
	if codexUpdate.Code != http.StatusOK {
		t.Fatalf("Codex update status=%d body=%s", codexUpdate.Code, codexUpdate.Body.String())
	}
}

func TestCodexUpdateSurvivesRequestCancellation(t *testing.T) {
	server := newTestServer(t)
	runner := &requestContextUpdateRunner{started: make(chan struct{}), release: make(chan struct{})}
	server.codexRunner = runner
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, "/api/runners/wsl-local/codex/update", nil).WithContext(ctx)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		server.routes().ServeHTTP(response, request)
		close(done)
	}()
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("Codex update did not start")
	}
	cancel()
	select {
	case <-done:
		t.Fatalf("update ended after request cancellation: status=%d body=%s", response.Code, response.Body.String())
	case <-time.After(100 * time.Millisecond):
	}
	close(runner.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Codex update did not finish")
	}
	if response.Code != http.StatusOK {
		t.Fatalf("Codex update status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestSendMessagePersistsUsageFromRunnerLifecycle(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	projectPath := t.TempDir()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, projectPath, now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,claude_initialized,is_current,created_at) values ('conversation','project','00000000-0000-4000-8000-000000000000','idle',0,1,?)`, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	server.runner = runnerFunc(func(_ context.Context, _ AgentRunRequest, sink AgentRunSink) error {
		sink.Event("system", json.RawMessage(`{"type":"system","subtype":"init","model":"claude-sonnet"}`))
		sink.Event("assistant", json.RawMessage(`{"type":"assistant","message":{"id":"step","model":"claude-sonnet","usage":{"input_tokens":1200},"content":[{"type":"tool_use","id":"tool"}]}}`))
		sink.Event("result", json.RawMessage(`{"type":"result","num_turns":1,"duration_ms":2100,"ttft_ms":500,"total_cost_usd":0.0042,"terminal_reason":"end_turn","usage":{"input_tokens":1200,"output_tokens":200,"cache_read_input_tokens":100,"cache_creation_input_tokens":0},"modelUsage":{"claude-sonnet":{"inputTokens":1200,"outputTokens":200,"cacheReadInputTokens":100,"cacheCreationInputTokens":0,"costUSD":0.0042,"contextWindow":200000}}}`))
		return nil
	})

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/conversations/conversation/messages", bytes.NewBufferString(`{"content":"检查项目"}`)))
	if response.Code != http.StatusAccepted {
		t.Fatalf("send message status: %d body=%s", response.Code, response.Body.String())
	}
	waitForConversationIdle(t, server, "conversation")

	response = httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/conversations/conversation/usage", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("get conversation usage status: %d body=%s", response.Code, response.Body.String())
	}
	var usage ConversationUsageResponse
	if err := json.NewDecoder(response.Body).Decode(&usage); err != nil {
		t.Fatalf("decode conversation usage: %v", err)
	}
	if usage.Session.TaskCount != 1 || usage.Session.AgentTurns != 1 || usage.Context.ContextInputTokens != 1200 || usage.Context.ContextWindow != 200000 || usage.Context.ToolCalls != 1 || !usage.Context.HasResult {
		t.Fatalf("unexpected lifecycle usage: %#v session=%#v", usage.Context, usage.Session)
	}
}

func TestShortcutPreviewAndRunReuseConversationLifecycle(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	projectPath := t.TempDir()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','demo',?,'wsl-local','main',1,?)`, projectPath, now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,claude_initialized,is_current,created_at) values ('conversation','project','00000000-0000-4000-8000-000000000000','idle',0,1,?)`, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	server.runner = runnerFunc(func(_ context.Context, request AgentRunRequest, _ AgentRunSink) error {
		if !strings.Contains(request.Prompt, "demo") || !strings.Contains(request.Prompt, "报错内容") {
			return fmt.Errorf("unexpected shortcut prompt: %q", request.Prompt)
		}
		return nil
	})

	handler := server.routes()
	create := httptest.NewRecorder()
	handler.ServeHTTP(create, httptest.NewRequest(http.MethodPost, "/api/shortcuts", bytes.NewBufferString(`{"name":"解释错误","description":"","kind":"prompt","template":"检查 ${project.name} 的问题：${error}","scope":"project","defaultAction":"confirm","groupName":"检查","enabled":true,"projectIds":["project"]}`)))
	if create.Code != http.StatusCreated {
		t.Fatalf("create shortcut status: %d body=%s", create.Code, create.Body.String())
	}
	var shortcut Shortcut
	if err := json.NewDecoder(create.Body).Decode(&shortcut); err != nil {
		t.Fatalf("decode shortcut: %v", err)
	}

	preview := httptest.NewRecorder()
	handler.ServeHTTP(preview, httptest.NewRequest(http.MethodPost, "/api/conversations/conversation/shortcuts/"+shortcut.ID+"/preview", bytes.NewBufferString(`{"variables":{"error":"报错内容"}}`)))
	if preview.Code != http.StatusOK || !strings.Contains(preview.Body.String(), "demo") || !strings.Contains(preview.Body.String(), `"projectIds":["project"]`) {
		t.Fatalf("preview shortcut status: %d body=%s", preview.Code, preview.Body.String())
	}

	run := httptest.NewRecorder()
	handler.ServeHTTP(run, httptest.NewRequest(http.MethodPost, "/api/conversations/conversation/shortcuts/"+shortcut.ID+"/run", bytes.NewBufferString(`{"variables":{"error":"报错内容"},"action":"confirm"}`)))
	if run.Code != http.StatusAccepted {
		t.Fatalf("run shortcut status: %d body=%s", run.Code, run.Body.String())
	}
	waitForConversationIdle(t, server, "conversation")
	var messageCount, runCount int
	var status string
	if err := server.db.QueryRow(`select count(*) from messages where conversation_id='conversation' and role='user'`).Scan(&messageCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if err := server.db.QueryRow(`select count(*),max(status) from shortcut_runs where conversation_id='conversation'`).Scan(&runCount, &status); err != nil {
		t.Fatalf("read shortcut run: %v", err)
	}
	if messageCount != 1 || runCount != 1 || status != "completed" {
		t.Fatalf("unexpected shortcut lifecycle: messages=%d runs=%d status=%q", messageCount, runCount, status)
	}
}

func TestShortcutValidationVisibilityAndCORS(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	handler := server.routes()

	preflight := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodOptions, "/api/shortcuts/example", nil)
	request.Header.Set("Origin", "http://127.0.0.1:5173")
	request.Header.Set("Access-Control-Request-Method", http.MethodPut)
	handler.ServeHTTP(preflight, request)
	if preflight.Code != http.StatusNoContent || preflight.Header().Get("Access-Control-Allow-Methods") != "GET, POST, PUT, PATCH, DELETE, OPTIONS" {
		t.Fatalf("unexpected preflight response: status=%d methods=%q", preflight.Code, preflight.Header().Get("Access-Control-Allow-Methods"))
	}

	invalid := httptest.NewRecorder()
	handler.ServeHTTP(invalid, httptest.NewRequest(http.MethodPost, "/api/shortcuts", bytes.NewBufferString(`{"name":"无绑定","kind":"prompt","template":"检查","scope":"project","defaultAction":"fill","projectIds":["", "  "]}`)))
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("empty project bindings status: %d body=%s", invalid.Code, invalid.Body.String())
	}

	create := httptest.NewRecorder()
	handler.ServeHTTP(create, httptest.NewRequest(http.MethodPost, "/api/shortcuts", bytes.NewBufferString(`{"name":"已停用","kind":"prompt","template":"检查","scope":"local","defaultAction":"fill","enabled":false}`)))
	if create.Code != http.StatusCreated {
		t.Fatalf("create disabled shortcut status: %d body=%s", create.Code, create.Body.String())
	}
	visible := httptest.NewRecorder()
	handler.ServeHTTP(visible, httptest.NewRequest(http.MethodGet, "/api/projects/project/shortcuts", nil))
	if visible.Code != http.StatusOK || visible.Body.String() != "[]\n" {
		t.Fatalf("disabled shortcut must stay hidden: status=%d body=%s", visible.Code, visible.Body.String())
	}
	managed := httptest.NewRecorder()
	handler.ServeHTTP(managed, httptest.NewRequest(http.MethodGet, "/api/projects/project/shortcuts?includeDisabled=true", nil))
	if managed.Code != http.StatusOK || !strings.Contains(managed.Body.String(), "已停用") {
		t.Fatalf("disabled shortcut missing from management list: status=%d body=%s", managed.Code, managed.Body.String())
	}
}

func TestClaudeUpdateBlocksConcurrentUpdatesAndNewClaudeRuns(t *testing.T) {
	server := newTestServer(t)
	runner := &blockingUpdateRunner{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	server.runner = runner
	server.runnerRegistry.register("wsl-local", runner, server.wslLocalMeta())

	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,agent_runtime_id,status,claude_initialized,is_current,created_at) values ('conversation','project','session','wsl-local','idle',0,1,?)`, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	idle := &idleAgentSession{done: make(chan error)}
	server.mu.Lock()
	server.sessions["conversation"] = &activeAgentSession{agent: idle, runnerID: "wsl-local"}
	server.mu.Unlock()
	idleSessionUpdate := httptest.NewRecorder()
	server.routes().ServeHTTP(idleSessionUpdate, httptest.NewRequest(http.MethodPost, "/api/runners/wsl-local/claude/update", nil))
	if idleSessionUpdate.Code != http.StatusConflict {
		t.Fatalf("update with idle session status=%d body=%s", idleSessionUpdate.Code, idleSessionUpdate.Body.String())
	}
	server.mu.Lock()
	delete(server.sessions, "conversation")
	server.mu.Unlock()

	update := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		server.routes().ServeHTTP(update, httptest.NewRequest(http.MethodPost, "/api/runners/wsl-local/claude/update", nil))
		close(done)
	}()
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("update did not start")
	}

	secondUpdate := httptest.NewRecorder()
	server.routes().ServeHTTP(secondUpdate, httptest.NewRequest(http.MethodPost, "/api/runners/wsl-local/claude/update", nil))
	if secondUpdate.Code != http.StatusConflict {
		t.Fatalf("second update status=%d body=%s", secondUpdate.Code, secondUpdate.Body.String())
	}

	message := httptest.NewRecorder()
	server.routes().ServeHTTP(message, httptest.NewRequest(http.MethodPost, "/api/conversations/conversation/messages", strings.NewReader(`{"content":"检查更新锁"}`)))
	if message.Code != http.StatusConflict {
		t.Fatalf("message during update status=%d body=%s", message.Code, message.Body.String())
	}
	var runCount int
	if err := server.db.QueryRow(`select count(*) from runs where conversation_id='conversation'`).Scan(&runCount); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runCount != 0 {
		t.Fatalf("run admitted during update: %d", runCount)
	}

	close(runner.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("update did not finish")
	}
	if update.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", update.Code, update.Body.String())
	}
}

func TestClaudeUpdateDoesNotDeadlockWithMessageAdmission(t *testing.T) {
	server := newTestServer(t)
	runner := &admissionBlockingRunner{readyStarted: make(chan struct{}), releaseReady: make(chan struct{})}
	server.runner = runner
	server.runnerRegistry.register("wsl-local", runner, server.wslLocalMeta())
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,agent_runtime_id,status,claude_initialized,is_current,created_at) values ('conversation','project','session','wsl-local','idle',0,1,?)`, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}

	messageDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/conversations/conversation/messages", strings.NewReader(`{"content":"并发准入"}`)))
		messageDone <- response
	}()
	select {
	case <-runner.readyStarted:
	case <-time.After(time.Second):
		t.Fatal("message admission did not reach readiness check")
	}

	updateDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/runners/wsl-local/claude/update", nil))
		updateDone <- response
	}()
	close(runner.releaseReady)
	select {
	case response := <-messageDone:
		if response.Code != http.StatusAccepted {
			t.Fatalf("message status=%d body=%s", response.Code, response.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("message admission deadlocked")
	}
	select {
	case response := <-updateDone:
		if response.Code != http.StatusConflict {
			t.Fatalf("update status=%d body=%s", response.Code, response.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("update deadlocked")
	}
}

func TestShortcutDefaultsPreserveProjectBindingsAndWrapCommands(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	for _, id := range []string{"project-one", "project-two"} {
		if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values (?,?,?,'wsl-local','main',1,?)`, id, id, t.TempDir(), now); err != nil {
			t.Fatalf("insert project %s: %v", id, err)
		}
	}
	handler := server.routes()
	create := httptest.NewRecorder()
	handler.ServeHTTP(create, httptest.NewRequest(http.MethodPost, "/api/shortcuts", bytes.NewBufferString(`{"name":"测试","kind":"prompt","template":"检查","scope":"project","defaultAction":"fill","projectIds":["project-one","project-two"]}`)))
	if create.Code != http.StatusCreated {
		t.Fatalf("create shortcut status: %d body=%s", create.Code, create.Body.String())
	}
	var shortcut Shortcut
	if err := json.NewDecoder(create.Body).Decode(&shortcut); err != nil {
		t.Fatalf("decode shortcut: %v", err)
	}
	if !shortcut.Enabled || len(shortcut.ProjectIDs) != 2 {
		t.Fatalf("unexpected shortcut defaults: %#v", shortcut)
	}
	listed := httptest.NewRecorder()
	handler.ServeHTTP(listed, httptest.NewRequest(http.MethodGet, "/api/projects/project-one/shortcuts", nil))
	var shortcuts []Shortcut
	if err := json.NewDecoder(listed.Body).Decode(&shortcuts); err != nil || len(shortcuts) != 1 || len(shortcuts[0].ProjectIDs) != 2 {
		t.Fatalf("project bindings were not returned: err=%v items=%#v", err, shortcuts)
	}
	update := httptest.NewRecorder()
	handler.ServeHTTP(update, httptest.NewRequest(http.MethodPatch, "/api/shortcuts/"+shortcut.ID, bytes.NewBufferString(`{"name":"测试更新","kind":"prompt","template":"检查","scope":"project","defaultAction":"fill","projectIds":["project-one","project-two"]}`)))
	if update.Code != http.StatusOK {
		t.Fatalf("update shortcut status: %d body=%s", update.Code, update.Body.String())
	}
	var updated Shortcut
	if err := json.NewDecoder(update.Body).Decode(&updated); err != nil || !updated.CreatedAt.Equal(shortcut.CreatedAt) {
		t.Fatalf("update changed shortcut creation time: updated=%#v err=%v", updated, err)
	}
	var projectCount int
	if err := server.db.QueryRow(`select count(*) from shortcut_projects where shortcut_id=?`, shortcut.ID).Scan(&projectCount); err != nil || projectCount != 2 {
		t.Fatalf("project bindings changed after update: count=%d err=%v", projectCount, err)
	}
	command := Shortcut{Kind: "command_request", Template: "pnpm test", DefaultAction: "confirm"}
	content, err := renderShortcut(command, Conversation{}, Project{}, nil)
	if err != nil || !strings.Contains(content, "pnpm test") {
		t.Fatalf("command shortcut render failed: content=%q err=%v", content, err)
	}
	slash := Shortcut{Kind: "command_request", Template: "/clear", DefaultAction: "confirm"}
	slashContent, err := renderShortcut(slash, Conversation{}, Project{}, nil)
	if err != nil || !strings.Contains(slashContent, "/clear") {
		t.Fatalf("slash command shortcut render failed: content=%q err=%v", slashContent, err)
	}
}

func TestReorderShortcuts(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','Project','project-path','wsl-local','main',1,?)`, now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	// 插入三个提示词 + 两个命令，用于验证 kind 内重排与跨 kind 隔离。
	for i, entry := range []struct {
		id, kind string
	}{
		{"p-a", "prompt"}, {"p-b", "prompt"}, {"p-c", "prompt"},
		{"c-a", "command_request"}, {"c-b", "command_request"},
	} {
		if _, err := server.db.Exec(`insert into shortcuts (id,name,description,kind,template,scope,default_action,group_name,pinned,enabled,sort_order,created_at,updated_at) values (?,?,?,?,?,'local','run','',1,1,?,?,?)`, entry.id, entry.id, "", entry.kind, entry.kind, i, now, now); err != nil {
			t.Fatalf("insert shortcut %s: %v", entry.id, err)
		}
	}
	// 让 p-c 停用，验证自由排序下停用项也能被重排。
	if _, err := server.db.Exec(`update shortcuts set enabled=0 where id='p-c'`); err != nil {
		t.Fatalf("disable p-c: %v", err)
	}

	handler := server.routes()
	payload := `{"kind":"prompt","orderedIds":["p-c","p-a","p-b"]}`
	reorder := httptest.NewRecorder()
	handler.ServeHTTP(reorder, httptest.NewRequest(http.MethodPut, "/api/projects/project/shortcuts/reorder", bytes.NewBufferString(payload)))
	if reorder.Code != http.StatusOK {
		t.Fatalf("reorder prompt status: %d body=%s", reorder.Code, reorder.Body.String())
	}
	// 验证 sort_order 已按提交顺序重新编号。
	for index, id := range []string{"p-c", "p-a", "p-b"} {
		var order int
		if err := server.db.QueryRow(`select sort_order from shortcuts where id=?`, id).Scan(&order); err != nil {
			t.Fatalf("read sort_order for %s: %v", id, err)
		}
		if order != index {
			t.Fatalf("sort_order for %s: want %d got %d", id, index, order)
		}
	}
	// list 应返回新顺序（含停用项，因 includeDisabled=true），且 p-c 在前。
	listed := httptest.NewRecorder()
	handler.ServeHTTP(listed, httptest.NewRequest(http.MethodGet, "/api/projects/project/shortcuts?includeDisabled=true", nil))
	var shortcuts []Shortcut
	if err := json.NewDecoder(listed.Body).Decode(&shortcuts); err != nil {
		t.Fatalf("decode listed shortcuts: %v", err)
	}
	promptIDs := []string{}
	for _, sc := range shortcuts {
		if sc.Kind == "prompt" {
			promptIDs = append(promptIDs, sc.ID)
		}
	}
	if strings.Join(promptIDs, ",") != "p-c,p-a,p-b" {
		t.Fatalf("listed prompt order: want p-c,p-a,p-b got %s", strings.Join(promptIDs, ","))
	}
	// 命令种类不受 prompt 重排影响，仍保持各自原 sort_order（c-a=3, c-b=4）不变。
	for _, wanted := range []struct {
		id   string
		from int
	}{
		{"c-a", 3}, {"c-b", 4},
	} {
		var commandOrder int
		if err := server.db.QueryRow(`select sort_order from shortcuts where id=?`, wanted.id).Scan(&commandOrder); err != nil || commandOrder != wanted.from {
			t.Fatalf("command %s sort_order altered by prompt reorder: want %d got %d err=%v", wanted.id, wanted.from, commandOrder, err)
		}
	}
}

func TestReorderShortcutsRejectsInvalidInput(t *testing.T) {
	server := newTestServer(t)
	// 无项目时 reorder 也要求 orderedIds 与可见集一致，用于校验 400 分支。
	cases := []string{
		`{"kind":"unknown","orderedIds":["x"]}`,    // 非法 kind
		`{"kind":"prompt","orderedIds":[]}`,        // 空数组
		`{"orderedIds":["x"]}`,                     // 缺 kind
		`{"kind":"prompt"}`,                        // 缺 orderedIds（为空）
		`{"kind":"prompt","orderedIds":["x","y"]}`, // 含不可见 id
	}
	for _, payload := range cases {
		rec := httptest.NewRecorder()
		handler := server.routes()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/projects/project/shortcuts/reorder", bytes.NewBufferString(payload)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("reorder %s: want 400 got %d body=%s", payload, rec.Code, rec.Body.String())
		}
	}
}

// 项目级快捷方式只在绑定它的项目内可见：项目 A 的 reorder 不能写入绑定到项目 B 的
// 项目级项（服务端应拒绝未知 id），而 local 全局项在所有项目可见、可被任一项目重排。
func TestReorderShortcutsScopeIsolation(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	for _, id := range []string{"project-a", "project-b"} {
		if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values (?,?,?,'wsl-local','main',1,?)`, id, id, id+"-path", now); err != nil {
			t.Fatalf("insert project %s: %v", id, err)
		}
	}
	type row struct {
		id, kind, scope string
		projectID       string // 空表示 local
	}
	rows := []row{
		{"pa-1", "prompt", "project", "project-a"},
		{"bonly", "prompt", "project", "project-b"},
		{"pa-2", "command_request", "project", "project-a"},
	}
	for _, r := range rows {
		if _, err := server.db.Exec(`insert into shortcuts (id,name,description,kind,template,scope,default_action,group_name,pinned,enabled,sort_order,created_at,updated_at) values (?,?,?,?,?,'project','run','',1,1,?,?,?)`, r.id, r.id, "", r.kind, r.kind+"-tpl", len(rows), now, now); err != nil {
			t.Fatalf("insert shortcut %s: %v", r.id, err)
		}
		if _, err := server.db.Exec(`insert into shortcut_projects (shortcut_id,project_id) values (?,?)`, r.id, r.projectID); err != nil {
			t.Fatalf("bind shortcut %s: %v", r.id, err)
		}
	}
	handler := server.routes()
	// 项目 A 同时提交 pa-1（自己的）和 bonly（项目 B 的）→ 必须 400，不能越权改动 B 的项。
	payload := `{"kind":"prompt","orderedIds":["pa-1","bonly"]}`
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/projects/project-a/shortcuts/reorder", bytes.NewBufferString(payload)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("reorder across projects: want 400 got %d body=%s", rec.Code, rec.Body.String())
	}
	// 项目 A 按 kind=prompt 可见集只有 pa-1，正确重排应只含 pa-1。
	ok := httptest.NewRecorder()
	handler.ServeHTTP(ok, httptest.NewRequest(http.MethodPut, "/api/projects/project-a/shortcuts/reorder", bytes.NewBufferString(`{"kind":"prompt","orderedIds":["pa-1"]}`)))
	if ok.Code != http.StatusOK {
		t.Fatalf("reorder own prompt: want 200 got %d body=%s", ok.Code, ok.Body.String())
	}
	// 越权请求不得改动 bonly 的 sort_order。
	var bOrder int
	if err := server.db.QueryRow(`select sort_order from shortcuts where id='bonly'`).Scan(&bOrder); err != nil || bOrder != len(rows) {
		t.Fatalf("project B shortcut sort_order altered: order=%d err=%v", bOrder, err)
	}
	// 未被提交的项目项 pa-2（command），其 sort_order 也不得被改。
	var cOrder int
	if err := server.db.QueryRow(`select sort_order from shortcuts where id='pa-2'`).Scan(&cOrder); err != nil || cOrder != len(rows) {
		t.Fatalf("untouched command sort_order altered: order=%d err=%v", cOrder, err)
	}
}

func TestDefaultShortcutsSeedOnceAndDoNotReappearAfterDeletion(t *testing.T) {
	server := newTestServer(t)
	handler := server.routes()

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodPost, "/api/shortcuts/defaults", nil))
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), `"seeded":true`) || !strings.Contains(first.Body.String(), `"created":6`) {
		t.Fatalf("first shortcut seed response: status=%d body=%s", first.Code, first.Body.String())
	}
	var count int
	if err := server.db.QueryRow(`select count(*) from shortcuts`).Scan(&count); err != nil || count != 6 {
		t.Fatalf("default shortcut count: count=%d err=%v", count, err)
	}
	if _, err := server.db.Exec(`delete from shortcuts`); err != nil {
		t.Fatalf("delete default shortcuts: %v", err)
	}

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodPost, "/api/shortcuts/defaults", nil))
	if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), `"seeded":false`) || !strings.Contains(second.Body.String(), `"created":0`) {
		t.Fatalf("repeat shortcut seed response: status=%d body=%s", second.Code, second.Body.String())
	}
	if err := server.db.QueryRow(`select count(*) from shortcuts`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("deleted default shortcuts reappeared: count=%d err=%v", count, err)
	}
}

func TestDeleteProjectRemovesProjectAndCascades(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	projectPath := t.TempDir()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('delete-me','Delete Me',?,'wsl-local','main',1,?)`, projectPath, now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	// Create a conversation so we can verify cascading deletion.
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,permission_mode,title,last_activity_at,claude_initialized,is_current,created_at) values ('conv','delete-me','sess','idle','approval_required','Test',?,0,1,?)`, now, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	// Create a shortcut bound to this project.
	if _, err := server.db.Exec(`insert into shortcuts (id,name,description,kind,template,scope,default_action,group_name,pinned,enabled,sort_order,created_at,updated_at) values ('sc','Test','','prompt','test','project','run','',0,1,0,?,?)`, now, now); err != nil {
		t.Fatalf("insert shortcut: %v", err)
	}
	if _, err := server.db.Exec(`insert into shortcut_projects (shortcut_id,project_id) values ('sc','delete-me')`); err != nil {
		t.Fatalf("bind shortcut to project: %v", err)
	}

	handler := server.routes()
	req := httptest.NewRequest(http.MethodDelete, "/api/projects/delete-me", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete project status: want 204 got %d body=%s", rec.Code, rec.Body.String())
	}

	// Project should be gone.
	var remain int
	if err := server.db.QueryRow(`select count(*) from projects where id='delete-me'`).Scan(&remain); err != nil {
		t.Fatalf("count projects after delete: %v", err)
	}
	if remain != 0 {
		t.Fatalf("project still exists after deletion: count=%d", remain)
	}
	// Conversations should cascade.
	if err := server.db.QueryRow(`select count(*) from conversations where id='conv'`).Scan(&remain); err != nil {
		t.Fatalf("count conversations after delete: %v", err)
	}
	if remain != 0 {
		t.Fatalf("conversation still exists after project deletion: count=%d", remain)
	}
	// shortcut_projects binding should cascade (via project_id FK).
	if err := server.db.QueryRow(`select count(*) from shortcut_projects where project_id='delete-me'`).Scan(&remain); err != nil {
		t.Fatalf("count shortcut_projects after delete: %v", err)
	}
	if remain != 0 {
		t.Fatalf("shortcut_projects binding still exists: count=%d", remain)
	}
	// The shortcut itself should survive (it is not owned by the project).
	if err := server.db.QueryRow(`select count(*) from shortcuts where id='sc'`).Scan(&remain); err != nil {
		t.Fatalf("count shortcuts after delete: %v", err)
	}
	if remain != 1 {
		t.Fatalf("shortcut unexpectedly deleted: count=%d", remain)
	}
}

func TestDeleteProjectStopsActiveAgentRun(t *testing.T) {
	server := newTestServer(t)
	started := make(chan struct{})
	stopped := make(chan struct{})
	var once sync.Once
	server.runner = runnerFunc(func(ctx context.Context, _ AgentRunRequest, _ AgentRunSink) error {
		once.Do(func() { close(started) })
		<-ctx.Done()
		close(stopped)
		return ctx.Err()
	})
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,agent_runtime_id,status,claude_initialized,is_current,created_at) values ('conversation','project','session','wsl-local','idle',0,1,?)`, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	message := httptest.NewRecorder()
	server.routes().ServeHTTP(message, httptest.NewRequest(http.MethodPost, "/api/conversations/conversation/messages", strings.NewReader(`{"content":"删除时停止"}`)))
	if message.Code != http.StatusAccepted {
		t.Fatalf("send message: %d body=%s", message.Code, message.Body.String())
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("agent did not start")
	}

	deleted := httptest.NewRecorder()
	server.routes().ServeHTTP(deleted, httptest.NewRequest(http.MethodDelete, "/api/projects/project", nil))
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete project: %d body=%s", deleted.Code, deleted.Body.String())
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("delete project did not stop the active agent")
	}
	var remaining int
	if err := server.db.QueryRow(`select count(*) from projects where id='project'`).Scan(&remaining); err != nil {
		t.Fatalf("count projects: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("project remains after delete: %d", remaining)
	}
}

func TestClaudeSessionForceTerminatesStoppedProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test fixture uses a POSIX shell")
	}
	path := filepath.Join(t.TempDir(), "stubborn-claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\ntrap '' TERM\nwhile :; do sleep 1; done\n"), 0o755); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	runner := &claudeCLIRunner{config: Config{ClaudePath: path}}
	session, err := runner.StartSession(context.Background(), AgentSessionRequest{ProjectPath: t.TempDir(), PermissionMode: "full_control"})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	session.Stop()
	select {
	case err := <-session.Done():
		if err == nil {
			t.Fatal("stopped Claude session unexpectedly succeeded")
		}
	case <-time.After(7 * time.Second):
		t.Fatal("stopped Claude session was not force terminated")
	}
}

func TestDeleteProjectNotFound(t *testing.T) {
	server := newTestServer(t)
	handler := server.routes()
	req := httptest.NewRequest(http.MethodDelete, "/api/projects/nonexistent", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete nonexistent project: want 404 got %d", rec.Code)
	}
}

func TestEnsureShortcutProjectIDHandlesMissingTable(t *testing.T) {
	server := newTestServer(t)
	// Drop the table created during migration to simulate a database that
	// never had shortcut_projects.
	if _, err := server.db.Exec(`drop table if exists shortcut_projects`); err != nil {
		t.Fatalf("drop shortcut_projects: %v", err)
	}
	// ensureShortcutProjectID should recreate it without error.
	if err := server.ensureShortcutProjectID(context.Background()); err != nil {
		t.Fatalf("ensureShortcutProjectID after drop: %v", err)
	}
	// Verify the table exists and has project_id.
	var count int
	if err := server.db.QueryRow(`select count(*) from pragma_table_info('shortcut_projects') where name='project_id'`).Scan(&count); err != nil {
		t.Fatalf("verify project_id column: %v", err)
	}
	if count != 1 {
		t.Fatalf("shortcut_projects missing project_id column after ensure")
	}
}

func TestEnsureShortcutProjectIDHandlesMissingColumn(t *testing.T) {
	server := newTestServer(t)
	// Create shortcut_projects without project_id to simulate an old schema.
	if _, err := server.db.Exec(`drop table if exists shortcut_projects`); err != nil {
		t.Fatalf("drop shortcut_projects: %v", err)
	}
	if _, err := server.db.Exec(`create table shortcut_projects (shortcut_id text not null, primary key (shortcut_id))`); err != nil {
		t.Fatalf("create malformed shortcut_projects: %v", err)
	}
	// ensureShortcutProjectID should detect the missing column and rebuild.
	if err := server.ensureShortcutProjectID(context.Background()); err != nil {
		t.Fatalf("ensureShortcutProjectID with missing column: %v", err)
	}
	// Verify the rebuilt table has project_id.
	var count int
	if err := server.db.QueryRow(`select count(*) from pragma_table_info('shortcut_projects') where name='project_id'`).Scan(&count); err != nil {
		t.Fatalf("verify project_id column: %v", err)
	}
	if count != 1 {
		t.Fatalf("rebuilt shortcut_projects still missing project_id column")
	}
}

func TestConversationPermissionModeLifecycle(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	handler := server.routes()
	request := httptest.NewRequest(http.MethodPost, "/api/projects/project/conversations", bytes.NewBufferString(`{"permissionMode":"full_control"}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create conversation status: %d body=%s", response.Code, response.Body.String())
	}
	var conversation Conversation
	if err := json.NewDecoder(response.Body).Decode(&conversation); err != nil {
		t.Fatalf("decode conversation: %v", err)
	}
	if conversation.PermissionMode != "full_control" {
		t.Fatalf("unexpected created mode: %q", conversation.PermissionMode)
	}

	request = httptest.NewRequest(http.MethodPost, "/api/conversations/"+conversation.ID+"/permission-mode", bytes.NewBufferString(`{"permissionMode":"approval_required"}`))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("switch permission mode status: %d body=%s", response.Code, response.Body.String())
	}
	if err := json.NewDecoder(response.Body).Decode(&conversation); err != nil {
		t.Fatalf("decode switched conversation: %v", err)
	}
	if conversation.PermissionMode != "approval_required" || conversation.ExecutionPolicy != "approval_required" {
		t.Fatalf("unexpected switched conversation: %#v", conversation)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,agent_id,agent_session_id,agent_runtime_id,execution_policy,status,permission_mode,is_current,created_at) values ('historic','project','historic-session','claude-code','historic-session','wsl-local','approval_required','idle','approval_required',0,?)`, now); err != nil {
		t.Fatalf("insert historic conversation: %v", err)
	}
	request = httptest.NewRequest(http.MethodPost, "/api/conversations/historic/permission-mode", bytes.NewBufferString(`{"permissionMode":"full_control"}`))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("historic switch status: %d body=%s", response.Code, response.Body.String())
	}
	var historicMode string
	if err := server.db.QueryRow(`select permission_mode from conversations where id='historic'`).Scan(&historicMode); err != nil {
		t.Fatalf("read historic permission mode: %v", err)
	}
	if historicMode != "approval_required" {
		t.Fatalf("historic permission mode changed: %q", historicMode)
	}
	if _, err := server.db.Exec(`update conversations set status='running' where id=?`, conversation.ID); err != nil {
		t.Fatalf("mark conversation running: %v", err)
	}
	request = httptest.NewRequest(http.MethodPost, "/api/conversations/"+conversation.ID+"/permission-mode", bytes.NewBufferString(`{"permissionMode":"full_control"}`))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("running switch status: %d body=%s", response.Code, response.Body.String())
	}
}

func TestConversationHistoryListsSummariesAndActivatesPriorSession(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	conversations := []struct {
		id       string
		title    string
		current  bool
		activity time.Time
		preview  string
	}{
		{id: "older", title: "旧会话", current: false, activity: now.Add(-2 * time.Hour), preview: "旧会话最后回复"},
		{id: "recent", title: "最近的修复", current: false, activity: now.Add(-time.Hour), preview: "最近会话最后回复"},
		{id: "current", title: "当前会话", current: true, activity: now, preview: "当前会话最后回复"},
	}
	for _, item := range conversations {
		if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,permission_mode,title,last_activity_at,claude_initialized,is_current,created_at) values (?,?,?,?,?,?,?,?,?,?)`, item.id, "project", fmt.Sprintf("00000000-0000-4000-8000-%012d", len(item.id)), "idle", "approval_required", item.title, item.activity, 1, item.current, now.Add(-3*time.Hour)); err != nil {
			t.Fatalf("insert conversation %q: %v", item.id, err)
		}
		if _, err := server.db.Exec(`insert into messages (id,conversation_id,role,content,created_at) values (?,?,?,?,?)`, "message-"+item.id, item.id, "assistant", item.preview, item.activity); err != nil {
			t.Fatalf("insert preview %q: %v", item.id, err)
		}
	}
	if _, err := server.db.Exec(`insert into messages (id,conversation_id,role,content,parent_tool_use_id,created_at) values ('child-preview','current','assistant','不应出现在会话预览中的子代理日志','agent-call',?)`, now.Add(time.Minute)); err != nil {
		t.Fatalf("insert child preview: %v", err)
	}

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/projects/project/conversations", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("list conversation history status: %d body=%s", response.Code, response.Body.String())
	}
	var history conversationListPage
	if err := json.NewDecoder(response.Body).Decode(&history); err != nil {
		t.Fatalf("decode conversation history: %v", err)
	}
	if history.NextCursor != "" || len(history.Items) != 3 || history.Items[0].ID != "current" || history.Items[0].Preview != "当前会话最后回复" || history.Items[1].ID != "recent" || history.Items[1].Preview != "最近会话最后回复" {
		t.Fatalf("unexpected conversation history: %#v", history)
	}

	response = httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/conversations/recent/activate", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("activate prior conversation status: %d body=%s", response.Code, response.Body.String())
	}
	var activated Conversation
	if err := json.NewDecoder(response.Body).Decode(&activated); err != nil {
		t.Fatalf("decode activated conversation: %v", err)
	}
	if !activated.IsCurrent || activated.ID != "recent" {
		t.Fatalf("unexpected activated conversation: %#v", activated)
	}
	var currentID string
	if err := server.db.QueryRow(`select id from conversations where project_id='project' and is_current=1`).Scan(&currentID); err != nil {
		t.Fatalf("read current conversation: %v", err)
	}
	if currentID != "recent" {
		t.Fatalf("unexpected current conversation: %q", currentID)
	}

	if _, err := server.db.Exec(`update conversations set status='running' where id='recent'`); err != nil {
		t.Fatalf("mark active conversation running: %v", err)
	}
	response = httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/conversations/older/activate", nil))
	if response.Code != http.StatusConflict {
		t.Fatalf("activate while active conversation is running status: %d body=%s", response.Code, response.Body.String())
	}
}

func TestConversationHistoryPaginatesAndSearchesMessageContent(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	for index := 0; index < 101; index++ {
		id := fmt.Sprintf("conversation-%03d", index)
		// All entries share a timestamp so the cursor's ID tie-breaker is exercised.
		activity := now
		if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,permission_mode,title,last_activity_at,claude_initialized,is_current,created_at) values (?,?,?,?,?,?,?,?,?,?)`, id, "project", fmt.Sprintf("session-%03d", index), "idle", "approval_required", fmt.Sprintf("会话 %03d", index), activity, 1, false, activity); err != nil {
			t.Fatalf("insert conversation %d: %v", index, err)
		}
		content := fmt.Sprintf("普通消息 %03d", index)
		if index == 0 {
			content = "只在最早会话中出现的搜索词"
		}
		if _, err := server.db.Exec(`insert into messages (id,conversation_id,role,content,created_at) values (?,?,?,?,?)`, fmt.Sprintf("message-%03d", index), id, "assistant", content, activity); err != nil {
			t.Fatalf("insert message %d: %v", index, err)
		}
	}

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/projects/project/conversations?limit=100", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("list first conversation page status: %d body=%s", response.Code, response.Body.String())
	}
	var first conversationListPage
	if err := json.NewDecoder(response.Body).Decode(&first); err != nil {
		t.Fatalf("decode first conversation page: %v", err)
	}
	if len(first.Items) != 100 || first.NextCursor == "" || first.Items[0].ID != "conversation-100" || first.Items[99].ID != "conversation-001" {
		t.Fatalf("unexpected first conversation page: %#v", first)
	}

	response = httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/projects/project/conversations?limit=100&cursor="+first.NextCursor, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("list second conversation page status: %d body=%s", response.Code, response.Body.String())
	}
	var second conversationListPage
	if err := json.NewDecoder(response.Body).Decode(&second); err != nil {
		t.Fatalf("decode second conversation page: %v", err)
	}
	if len(second.Items) != 1 || second.NextCursor != "" || second.Items[0].ID != "conversation-000" {
		t.Fatalf("unexpected second conversation page: %#v", second)
	}

	response = httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/projects/project/conversations?q=%E6%90%9C%E7%B4%A2%E8%AF%8D", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("search conversations status: %d body=%s", response.Code, response.Body.String())
	}
	var matches conversationListPage
	if err := json.NewDecoder(response.Body).Decode(&matches); err != nil {
		t.Fatalf("decode searched conversations: %v", err)
	}
	if len(matches.Items) != 1 || matches.NextCursor != "" || matches.Items[0].ID != "conversation-000" {
		t.Fatalf("unexpected searched conversations: %#v", matches)
	}

	response = httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/projects/project/conversations?limit=0", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid page limit status: %d body=%s", response.Code, response.Body.String())
	}

	response = httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/projects/project/conversations?cursor=not-a-valid-cursor", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid page cursor status: %d body=%s", response.Code, response.Body.String())
	}
}

func TestActivatedConversationResumesItsClaudeSession(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	projectPath := t.TempDir()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, projectPath, now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,permission_mode,title,last_activity_at,claude_initialized,is_current,created_at) values ('old','project','00000000-0000-4000-8000-000000000001','idle','approval_required','旧会话',?,1,0,?)`, now, now); err != nil {
		t.Fatalf("insert old conversation: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,permission_mode,title,last_activity_at,claude_initialized,is_current,created_at) values ('current','project','00000000-0000-4000-8000-000000000002','idle','approval_required','当前会话',?,1,1,?)`, now, now); err != nil {
		t.Fatalf("insert current conversation: %v", err)
	}
	requests := make(chan AgentRunRequest, 1)
	server.runner = runnerFunc(func(_ context.Context, request AgentRunRequest, _ AgentRunSink) error {
		requests <- request
		return nil
	})

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/conversations/old/activate", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("activate old conversation status: %d body=%s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/conversations/old/messages", bytes.NewBufferString(`{"content":"继续旧会话"}`)))
	if response.Code != http.StatusAccepted {
		t.Fatalf("send resumed message status: %d body=%s", response.Code, response.Body.String())
	}
	select {
	case request := <-requests:
		if request.SessionID != "00000000-0000-4000-8000-000000000001" || !request.Resume {
			t.Fatalf("activated session was not resumed: %#v", request)
		}
	case <-time.After(time.Second):
		t.Fatal("runner did not receive resumed conversation request")
	}
	waitForConversationIdle(t, server, "old")
}

func TestMigrateAddsHistoryColumnsToExistingDatabase(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite3", databasePath)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	legacyNow := time.Now().UTC()
	if _, err := legacy.Exec(`create table projects (id text primary key, name text not null, path text not null unique, runner text not null, git_branch text not null, claude_ready integer not null, created_at datetime not null);
create table conversations (id text primary key, project_id text not null references projects(id) on delete cascade, claude_session_id text not null unique, status text not null, permission_mode text not null default 'approval_required', claude_initialized integer not null default 0, is_current integer not null default 1, created_at datetime not null);
create table messages (id text primary key, conversation_id text not null references conversations(id) on delete cascade, role text not null, content text not null, created_at datetime not null);`); err != nil {
		legacy.Close()
		t.Fatalf("create legacy schema: %v", err)
	}
	if _, err := legacy.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project','/tmp/project','wsl-local','main',1,?)`, legacyNow); err != nil {
		legacy.Close()
		t.Fatalf("insert legacy project: %v", err)
	}
	if _, err := legacy.Exec(`insert into conversations (id,project_id,claude_session_id,status,permission_mode,claude_initialized,is_current,created_at) values ('conversation','project','00000000-0000-4000-8000-000000000000','idle','approval_required',1,1,?)`, legacyNow); err != nil {
		legacy.Close()
		t.Fatalf("insert legacy conversation: %v", err)
	}
	if _, err := legacy.Exec(`insert into messages (id,conversation_id,role,content,created_at) values ('message','conversation','user','修复历史会话标题',?)`, legacyNow); err != nil {
		legacy.Close()
		t.Fatalf("insert legacy message: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}
	hookPath, err := filepath.Abs("../../../../scripts/claude-approval-hook.sh")
	if err != nil {
		t.Fatalf("resolve hook path: %v", err)
	}
	server, err := New(context.Background(), Config{DatabasePath: databasePath, AllowedRoot: t.TempDir(), ClaudePath: "claude", ControlURL: "http://127.0.0.1:8080", ApprovalHook: hookPath})
	if err != nil {
		t.Fatalf("migrate legacy database: %v", err)
	}
	t.Cleanup(server.Close)
	rows, err := server.db.Query(`pragma table_info(conversations)`)
	if err != nil {
		t.Fatalf("inspect migrated conversations: %v", err)
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var primaryKey int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatalf("read migrated column: %v", err)
		}
		columns[name] = true
	}
	for _, name := range []string{"title", "last_activity_at", "agent_id", "agent_session_id", "agent_initialized", "agent_runtime_id", "execution_policy"} {
		if !columns[name] {
			t.Fatalf("column %q missing after migration: %#v", name, columns)
		}
	}
	var agentID, sessionID, runtimeID, policy string
	var initialized bool
	if err := server.db.QueryRow(`select agent_id,agent_session_id,agent_initialized,agent_runtime_id,execution_policy from conversations where id='conversation'`).Scan(&agentID, &sessionID, &initialized, &runtimeID, &policy); err != nil {
		t.Fatalf("read migrated agent fields: %v", err)
	}
	if agentID != "claude-code" || sessionID != "00000000-0000-4000-8000-000000000000" || !initialized || runtimeID != "wsl-local" || policy != "approval_required" {
		t.Fatalf("unexpected migrated agent fields: id=%q session=%q initialized=%t runtime=%q policy=%q", agentID, sessionID, initialized, runtimeID, policy)
	}
	messageRows, err := server.db.Query(`pragma table_info(messages)`)
	if err != nil {
		t.Fatalf("inspect migrated message columns: %v", err)
	}
	defer messageRows.Close()
	messageColumns := map[string]bool{}
	for messageRows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var primaryKey int
		if err := messageRows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatalf("read migrated message column: %v", err)
		}
		messageColumns[name] = true
	}
	if !messageColumns["parent_tool_use_id"] || !messageColumns["run_id"] {
		t.Fatalf("message attribution columns missing after migration: %#v", messageColumns)
	}
	var title string
	if err := server.db.QueryRow(`select title from conversations where id='conversation'`).Scan(&title); err != nil {
		t.Fatalf("read migrated title: %v", err)
	}
	if title != "修复历史会话标题" {
		t.Fatalf("unexpected migrated title: %q", title)
	}
}

func TestConversationPageReturnsLatestWindowAndCursor(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC().Add(-time.Minute)
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,permission_mode,title,last_activity_at,claude_initialized,is_current,created_at) values ('conversation','project','00000000-0000-4000-8000-000000000000','idle','approval_required','conversation',?,1,1,?)`, now, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values ('run','conversation','completed',?)`, now); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	for index := 0; index < 3; index++ {
		createdAt := now
		if _, err := server.db.Exec(`insert into messages (id,conversation_id,run_id,role,content,parent_tool_use_id,created_at) values (?,?,?,?,?,?,?)`, fmt.Sprintf("message-%d", index), "conversation", "run", "assistant", fmt.Sprintf("message-%d", index), "", createdAt); err != nil {
			t.Fatalf("insert message %d: %v", index, err)
		}
		if _, err := server.db.Exec(`insert into events (id,conversation_id,run_id,type,payload,created_at) values (?,?,?,?,?,?)`, fmt.Sprintf("event-%d", index), "conversation", "run", "assistant", `{}`, createdAt); err != nil {
			t.Fatalf("insert event %d: %v", index, err)
		}
	}
	type conversationPage struct {
		Messages        []Message `json:"messages"`
		Events          []Event   `json:"events"`
		HasMore         bool      `json:"hasMore"`
		HasMoreMessages bool      `json:"hasMoreMessages"`
		NextCursor      string    `json:"nextCursor"`
	}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/conversations/conversation?limit=2", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("first page status: %d body=%s", response.Code, response.Body.String())
	}
	var first conversationPage
	if err := json.NewDecoder(response.Body).Decode(&first); err != nil {
		t.Fatalf("decode first page: %v", err)
	}
	if !first.HasMore || !first.HasMoreMessages || first.NextCursor == "" || len(first.Messages) != 2 || first.Messages[0].ID != "message-1" || len(first.Events) != 2 || first.Events[0].ID != "event-1" {
		t.Fatalf("unexpected first page: %#v", first)
	}
	response = httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/conversations/conversation?limit=2&cursor="+first.NextCursor, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("second page status: %d body=%s", response.Code, response.Body.String())
	}
	var second conversationPage
	if err := json.NewDecoder(response.Body).Decode(&second); err != nil {
		t.Fatalf("decode second page: %v", err)
	}
	if second.HasMore || second.HasMoreMessages || len(second.Messages) != 1 || second.Messages[0].ID != "message-0" || len(second.Events) != 1 || second.Events[0].ID != "event-0" {
		t.Fatalf("unexpected second page: %#v", second)
	}
	response = httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/conversations/conversation?cursor=not-a-valid-cursor", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid cursor status: %d body=%s", response.Code, response.Body.String())
	}
}

func TestInputHistoryReturnsLatestUserMessagesInChronologicalOrder(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,claude_initialized,is_current,created_at) values ('conversation','project','00000000-0000-4000-8000-000000000000','idle',0,1,?)`, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	for index := 0; index < 102; index++ {
		createdAt := now.Add(time.Duration(index) * time.Second)
		if _, err := server.db.Exec(`insert into messages (id,conversation_id,run_id,role,content,parent_tool_use_id,created_at) values (?,?,?,'user',?,'',?)`, fmt.Sprintf("message-%d", index), "conversation", "", fmt.Sprintf("prompt-%d", index), createdAt); err != nil {
			t.Fatalf("insert input history message %d: %v", index, err)
		}
	}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/conversations/conversation/input-history", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("history status: %d body=%s", response.Code, response.Body.String())
	}
	var history []string
	if err := json.NewDecoder(response.Body).Decode(&history); err != nil {
		t.Fatalf("decode history: %v", err)
	}
	if len(history) != 100 || history[0] != "prompt-2" || history[len(history)-1] != "prompt-101" {
		t.Fatalf("unexpected history: count=%d first=%q last=%q", len(history), history[0], history[len(history)-1])
	}
}

func TestClaudeCLIArgsUseNewSessionUntilClaudeInit(t *testing.T) {
	runner := claudeCLIRunner{config: Config{PermissionMode: "acceptEdits", ApprovalHook: "/tmp/approval hook.sh"}}
	request := AgentRunRequest{SessionID: "00000000-0000-4000-8000-000000000000", Prompt: "first prompt", PermissionMode: "approval_required"}
	args, err := runner.args(request)
	if err != nil {
		t.Fatalf("build new-session arguments: %v", err)
	}
	if !containsArguments(args, "--session-id", request.SessionID) || containsArgument(args, "--resume") {
		t.Fatalf("new session arguments must use --session-id: %q", args)
	}
	if !containsArgument(args, "--settings") || !containsArgument(args, "--permission-mode") || args[len(args)-1] != request.Prompt {
		t.Fatalf("approval arguments are incomplete: %q", args)
	}

	request.Resume = true
	args, err = runner.args(request)
	if err != nil {
		t.Fatalf("build resumed-session arguments: %v", err)
	}
	if !containsArguments(args, "--resume", request.SessionID) || containsArgument(args, "--session-id") {
		t.Fatalf("resumed session arguments must use --resume: %q", args)
	}

	request.PermissionMode = "full_control"
	args, err = runner.args(request)
	if err != nil {
		t.Fatalf("build full-control arguments: %v", err)
	}
	if !containsArguments(args, "--permission-mode", "bypassPermissions") || !containsArgument(args, "--dangerously-skip-permissions") || containsArgument(args, "--settings") {
		t.Fatalf("full-control arguments are incorrect: %q", args)
	}

	streamArgs, err := runner.sessionArgs(AgentSessionRequest{SessionID: request.SessionID, ProjectPath: t.TempDir(), PermissionMode: "approval_required"})
	if err != nil {
		t.Fatalf("build streaming arguments: %v", err)
	}
	if !containsArguments(streamArgs, "--input-format", "stream-json") || !containsArguments(streamArgs, "--output-format", "stream-json") || !containsArgument(streamArgs, "--replay-user-messages") || streamArgs[len(streamArgs)-1] != request.SessionID {
		t.Fatalf("streaming arguments are incomplete: %q", streamArgs)
	}

	request.PermissionMode = "plan"
	args, err = runner.args(request)
	if err != nil {
		t.Fatalf("build plan arguments: %v", err)
	}
	if !containsArguments(args, "--permission-mode", "plan") || containsArgument(args, "--settings") || containsArgument(args, "acceptEdits") {
		t.Fatalf("plan arguments must be read-only: %q", args)
	}
	streamArgs, err = runner.sessionArgs(AgentSessionRequest{SessionID: request.SessionID, ProjectPath: t.TempDir(), PermissionMode: "read_only"})
	if err != nil {
		t.Fatalf("build read-only streaming arguments: %v", err)
	}
	if !containsArguments(streamArgs, "--permission-mode", "plan") || containsArgument(streamArgs, "--settings") || containsArgument(streamArgs, "acceptEdits") {
		t.Fatalf("read-only streaming arguments must be read-only: %q", streamArgs)
	}
}

func TestSSHPermissionArgsKeepReviewsReadOnly(t *testing.T) {
	for _, permissionMode := range []string{"plan", "read_only"} {
		args := sshClaudePermissionArgs(permissionMode, "curl example.test")
		if args != "--permission-mode plan" {
			t.Fatalf("%s permission args = %q, want plan mode", permissionMode, args)
		}
	}
	if needsClaudeApprovalHook("plan") || needsClaudeApprovalHook("read_only") || needsClaudeApprovalHook("full_control") {
		t.Fatal("read-only and full-control modes must not open an approval hook")
	}
	if !needsClaudeApprovalHook("approval_required") {
		t.Fatal("approval-required mode must open an approval hook")
	}
}

func TestStreamingClaudeSessionAcceptsMessagesWhileTurnIsRunning(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	projectPath := t.TempDir()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, projectPath, now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,permission_mode,claude_initialized,is_current,created_at) values ('conversation','project','00000000-0000-4000-8000-000000000000','idle','full_control',0,1,?)`, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	capturePath := filepath.Join(t.TempDir(), "stream-input.jsonl")
	releasePath := filepath.Join(t.TempDir(), "release-first-turn")
	scriptPath := filepath.Join(t.TempDir(), "fake-streaming-claude")
	script := "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\"}'\ncount=0\nwhile IFS= read -r line; do\n  printf '%s\\n' \"$line\" >> \"$CAPTURE_PATH\"\n  count=$((count + 1))\n  if [ \"$count\" -eq 1 ]; then\n    while [ ! -f \"$RELEASE_PATH\" ]; do sleep 0.01; done\n  fi\n  printf '%s\\n' \"{\\\"type\\\":\\\"assistant\\\",\\\"message\\\":{\\\"content\\\":[{\\\"type\\\":\\\"text\\\",\\\"text\\\":\\\"reply-$count\\\"}]}}\"\n  printf '%s\\n' '{\"type\":\"result\",\"num_turns\":1,\"usage\":{},\"modelUsage\":{}}'\ndone\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write streaming Claude CLI: %v", err)
	}
	t.Setenv("CAPTURE_PATH", capturePath)
	t.Setenv("RELEASE_PATH", releasePath)
	server.runner = &claudeCLIRunner{config: Config{ClaudePath: scriptPath, PermissionMode: "acceptEdits", ControlURL: "http://127.0.0.1:8080", ApprovalHook: server.config.ApprovalHook}}
	handler := server.routes()
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodPost, "/api/conversations/conversation/messages", bytes.NewBufferString(`{"content":"first"}`)))
	if first.Code != http.StatusAccepted {
		t.Fatalf("first streaming send status: %d body=%s", first.Code, first.Body.String())
	}
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodPost, "/api/conversations/conversation/messages", bytes.NewBufferString(`{"content":"second"}`)))
	if second.Code != http.StatusAccepted {
		t.Fatalf("second streaming send while running status: %d body=%s", second.Code, second.Body.String())
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		input, err := os.ReadFile(capturePath)
		if err == nil && strings.Count(strings.TrimSpace(string(input)), "\n") == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	inputBeforeResult, err := os.ReadFile(capturePath)
	if err != nil || strings.Contains(string(inputBeforeResult), `"text":"second"`) {
		t.Fatalf("second input was written before the first result: %q err=%v", inputBeforeResult, err)
	}
	if err := os.WriteFile(releasePath, nil, 0o600); err != nil {
		t.Fatalf("release first turn: %v", err)
	}
	waitForConversationIdle(t, server, "conversation")
	input, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read streaming input: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(input)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"text":"first"`) || !strings.Contains(lines[1], `"text":"second"`) {
		t.Fatalf("unexpected stream input: %q", input)
	}
	var completed int
	if err := server.db.QueryRow(`select count(*) from runs where conversation_id='conversation' and status='completed'`).Scan(&completed); err != nil || completed != 2 {
		t.Fatalf("streaming runs did not complete: count=%d err=%v", completed, err)
	}
}

func TestStreamingStopSerializesConcurrentAdmission(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	projectPath := t.TempDir()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, projectPath, now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,permission_mode,claude_initialized,is_current,created_at) values ('conversation','project','00000000-0000-4000-8000-000000000000','idle','full_control',0,1,?)`, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	session := newBlockingStopAgentSession()
	server.runner = blockingStreamingRunner{session: session}
	handler := server.routes()

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodPost, "/api/conversations/conversation/messages", bytes.NewBufferString(`{"content":"first"}`)))
	if first.Code != http.StatusAccepted {
		t.Fatalf("first streaming send status: %d body=%s", first.Code, first.Body.String())
	}
	var firstResult struct {
		RunID string `json:"runId"`
	}
	if err := json.NewDecoder(first.Body).Decode(&firstResult); err != nil || firstResult.RunID == "" {
		t.Fatalf("decode first streaming result: runID=%q err=%v", firstResult.RunID, err)
	}

	stop := httptest.NewRecorder()
	stopDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(stop, httptest.NewRequest(http.MethodPost, "/api/runs/"+firstResult.RunID+"/stop", nil))
		close(stopDone)
	}()
	select {
	case <-session.stopEntered:
	case <-time.After(time.Second):
		t.Fatal("streaming stop did not reach the session")
	}

	second := httptest.NewRecorder()
	secondDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(second, httptest.NewRequest(http.MethodPost, "/api/conversations/conversation/messages", bytes.NewBufferString(`{"content":"second"}`)))
		close(secondDone)
	}()
	secondCompletedWhileStopping := false
	select {
	case <-secondDone:
		secondCompletedWhileStopping = true
	case <-time.After(100 * time.Millisecond):
	}

	close(session.releaseStop)
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("streaming stop did not finish")
	}
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("second streaming admission did not finish")
	}
	if stop.Code != http.StatusAccepted {
		t.Fatalf("streaming stop status: %d body=%s", stop.Code, stop.Body.String())
	}
	if secondCompletedWhileStopping {
		t.Fatalf("second streaming admission completed while stop was in progress: status=%d body=%s", second.Code, second.Body.String())
	}
	if second.Code != http.StatusAccepted && second.Code != http.StatusConflict {
		t.Fatalf("second streaming admission after stop: %d body=%s", second.Code, second.Body.String())
	}
}

func TestStopCancelsStreamingSessionSetupBeforeItCanRun(t *testing.T) {
	server, _, conversationID := seedTaskConversation(t)
	runner := &setupBlockingStreamingRunner{started: make(chan struct{})}
	server.runner = runner
	handler := server.routes()

	sent := httptest.NewRecorder()
	sendDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(sent, httptest.NewRequest(http.MethodPost, "/api/conversations/"+conversationID+"/messages", bytes.NewBufferString(`{"content":"must not start after stop"}`)))
		close(sendDone)
	}()
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("streaming session setup did not start")
	}

	var runID string
	if err := server.db.QueryRow(`select id from runs where conversation_id=? order by created_at desc limit 1`, conversationID).Scan(&runID); err != nil {
		t.Fatalf("load queued run: %v", err)
	}
	stopped := httptest.NewRecorder()
	handler.ServeHTTP(stopped, httptest.NewRequest(http.MethodPost, "/api/runs/"+runID+"/stop", nil))
	if stopped.Code != http.StatusAccepted {
		t.Fatalf("stop setup status=%d body=%s", stopped.Code, stopped.Body.String())
	}
	select {
	case <-sendDone:
	case <-time.After(time.Second):
		t.Fatal("cancelled streaming setup did not return")
	}
	if sent.Code != http.StatusAccepted {
		t.Fatalf("send status=%d body=%s", sent.Code, sent.Body.String())
	}
	var status string
	if err := server.db.QueryRow(`select status from runs where id=?`, runID).Scan(&status); err != nil {
		t.Fatalf("load run status: %v", err)
	}
	if status != "stopped" {
		t.Fatalf("run status=%q, want stopped", status)
	}
}

func TestStopPreventsStreamingSendAfterSessionSetupReturns(t *testing.T) {
	server, _, conversationID := seedTaskConversation(t)
	now := time.Now().UTC()
	const runID = "setup-returned-run"
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values (?,?,?,?)`, runID, conversationID, "queued", now); err != nil {
		t.Fatalf("insert queued run: %v", err)
	}
	conversation := Conversation{ID: conversationID, ClaudeSessionID: "00000000-0000-4000-8000-000000000000", AgentRuntimeID: "wsl-local", PermissionMode: "full_control"}
	session := newQueuedAgentSession()
	runner := &returnedStreamingRunner{session: session, returned: make(chan struct{})}
	runCtx, cancel := context.WithCancel(context.Background())
	server.mu.Lock()
	server.cancels[runID] = cancel
	server.runContexts[runID] = conversationID
	server.streamingSetups[runID] = &streamingSetup{}
	server.mu.Unlock()
	// streamingSession checks Err after StartSession returns. The next check is
	// immediately before Send, after the Session has been registered.
	setupCtx := &errBlockingContext{Context: context.Background(), blockCall: 2, entered: make(chan struct{}), release: make(chan struct{})}
	finished := make(chan struct{})
	go func() {
		server.submitStreamingRun(setupCtx, runner, runID, conversation, t.TempDir(), "must not be sent")
		close(finished)
	}()
	select {
	case <-runner.returned:
	case <-time.After(time.Second):
		t.Fatal("streaming runner did not return a session")
	}
	select {
	case <-setupCtx.entered:
	case <-time.After(time.Second):
		t.Fatal("streaming setup did not reach the send boundary")
	}
	server.mu.Lock()
	registered := server.sessions[conversationID] != nil
	server.mu.Unlock()
	if !registered {
		t.Fatal("streaming session was not registered before stop")
	}

	stopped := httptest.NewRecorder()
	server.routes().ServeHTTP(stopped, httptest.NewRequest(http.MethodPost, "/api/runs/"+runID+"/stop", nil))
	if stopped.Code != http.StatusAccepted {
		t.Fatalf("stop setup status=%d body=%s", stopped.Code, stopped.Body.String())
	}
	select {
	case <-runCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("stop did not cancel the setup context")
	}
	close(setupCtx.release)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("cancelled streaming setup did not finish")
	}
	if got := session.queuedCount(); got != 0 {
		t.Fatalf("cancelled setup sent %d prompt(s)", got)
	}
	select {
	case <-session.done:
	case <-time.After(time.Second):
		t.Fatal("session returned during cancelled setup was not stopped")
	}
	var status string
	if err := server.db.QueryRow(`select status from runs where id=?`, runID).Scan(&status); err != nil {
		t.Fatalf("load run status: %v", err)
	}
	if status != "stopped" {
		t.Fatalf("run status=%q, want stopped", status)
	}
	deadline := time.Now().Add(time.Second)
	for {
		server.mu.Lock()
		_, sessionRegistered := server.sessions[conversationID]
		_, setupRegistered := server.streamingSetups[runID]
		server.mu.Unlock()
		if !sessionRegistered && !setupRegistered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cancelled setup leaked session=%v setup=%v", sessionRegistered, setupRegistered)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestStoppedClaudeSessionDoesNotStartQueuedTurnAfterResult(t *testing.T) {
	currentSink := &turnRecordingSink{}
	nextSink := &turnRecordingSink{}
	session := &claudeCLISession{
		stdin:   rejectingWriteCloser{},
		stopped: true,
		current: &claudeSessionTurn{sink: currentSink},
		queued:  []*claudeSessionTurn{{sink: nextSink}},
	}

	session.finishCurrent(nil)

	currentSink.mu.Lock()
	currentFinished := len(currentSink.finished)
	currentSink.mu.Unlock()
	nextSink.mu.Lock()
	nextStarted := nextSink.started
	nextSink.mu.Unlock()
	if currentFinished != 1 || nextStarted != 0 {
		t.Fatalf("stopped session advanced its queue: currentFinished=%d nextStarted=%d", currentFinished, nextStarted)
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.current != nil || len(session.queued) != 1 {
		t.Fatalf("stopped session lost its queued turn: current=%v queued=%d", session.current != nil, len(session.queued))
	}
}

func TestCloseMarksStreamingRunsStopped(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	projectPath := t.TempDir()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, projectPath, now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,permission_mode,claude_initialized,is_current,created_at) values ('conversation','project','00000000-0000-4000-8000-000000000000','idle','full_control',0,1,?)`, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	scriptPath := filepath.Join(t.TempDir(), "fake-long-running-claude")
	script := "#!/bin/sh\nif [ \"$1\" = \"auth\" ] && [ \"$2\" = \"status\" ]; then exit 0; fi\nprintf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\"}'\nIFS= read -r line\nwhile :; do sleep 1; done\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake Claude CLI: %v", err)
	}
	server.runner = &claudeCLIRunner{config: Config{ClaudePath: scriptPath, PermissionMode: "acceptEdits", ControlURL: "http://127.0.0.1:8080", ApprovalHook: server.config.ApprovalHook}}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/conversations/conversation/messages", bytes.NewBufferString(`{"content":"wait"}`)))
	if response.Code != http.StatusAccepted {
		t.Fatalf("streaming send status: %d body=%s", response.Code, response.Body.String())
	}
	waitForRunStatus(t, server, "conversation", "running")
	config := server.config
	server.Close()
	reopened, err := New(context.Background(), config)
	if err != nil {
		t.Fatalf("reopen server: %v", err)
	}
	t.Cleanup(reopened.Close)
	var status string
	if err := reopened.db.QueryRow(`select status from runs where conversation_id='conversation'`).Scan(&status); err != nil {
		t.Fatalf("read closed run: %v", err)
	}
	if status != "stopped" {
		t.Fatalf("server shutdown marked streaming run %q, want stopped", status)
	}
}

func TestStreamingClaudeResultErrorFailsTheRun(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	projectPath := t.TempDir()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, projectPath, now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,permission_mode,claude_initialized,is_current,created_at) values ('conversation','project','00000000-0000-4000-8000-000000000000','idle','full_control',0,1,?)`, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	scriptPath := filepath.Join(t.TempDir(), "fake-error-claude")
	script := "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\"}'\nIFS= read -r line\nprintf '%s\\n' '{\"type\":\"result\",\"is_error\":true,\"subtype\":\"error_during_execution\",\"errors\":[\"simulated failure\"]}'\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake Claude CLI: %v", err)
	}
	server.runner = &claudeCLIRunner{config: Config{ClaudePath: scriptPath, PermissionMode: "acceptEdits", ControlURL: "http://127.0.0.1:8080", ApprovalHook: server.config.ApprovalHook}}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/conversations/conversation/messages", bytes.NewBufferString(`{"content":"fail"}`)))
	if response.Code != http.StatusAccepted {
		t.Fatalf("streaming send status: %d body=%s", response.Code, response.Body.String())
	}
	waitForConversationIdle(t, server, "conversation")
	var status string
	if err := server.db.QueryRow(`select status from runs where conversation_id='conversation'`).Scan(&status); err != nil {
		t.Fatalf("read failed run: %v", err)
	}
	if status != "failed" {
		t.Fatalf("error result marked run %q, want failed", status)
	}
}

func TestStreamingClaudeExitBeforeResultFailsTheRun(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	projectPath := t.TempDir()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, projectPath, now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,permission_mode,claude_initialized,is_current,created_at) values ('conversation','project','00000000-0000-4000-8000-000000000000','idle','full_control',0,1,?)`, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	scriptPath := filepath.Join(t.TempDir(), "fake-exit-claude")
	script := "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\"}'\nIFS= read -r line\nexit 0\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake Claude CLI: %v", err)
	}
	server.runner = &claudeCLIRunner{config: Config{ClaudePath: scriptPath, PermissionMode: "acceptEdits", ControlURL: "http://127.0.0.1:8080", ApprovalHook: server.config.ApprovalHook}}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/conversations/conversation/messages", bytes.NewBufferString(`{"content":"exit"}`)))
	if response.Code != http.StatusAccepted {
		t.Fatalf("streaming send status: %d body=%s", response.Code, response.Body.String())
	}
	waitForConversationIdle(t, server, "conversation")
	var status string
	if err := server.db.QueryRow(`select status from runs where conversation_id='conversation'`).Scan(&status); err != nil {
		t.Fatalf("read failed run: %v", err)
	}
	if status != "failed" {
		t.Fatalf("clean process exit marked run %q, want failed", status)
	}
}

func TestStreamingApprovalUsesTheActiveConversationTurn(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,claude_initialized,is_current,created_at) values ('conversation','project','00000000-0000-4000-8000-000000000000','running',1,1,?)`, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values ('run','conversation','running',?)`, now); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	session := newIdleAgentSession()
	server.mu.Lock()
	server.sessions["conversation"] = &activeAgentSession{agent: session, approvalToken: "conversation-token", activeRunID: "run"}
	server.mu.Unlock()

	request := httptest.NewRequest(http.MethodPost, "/api/internal/approvals/wait", bytes.NewBufferString(`{"tool_name":"Bash","tool_input":{"command":"git status"}}`))
	request.Header.Set("X-Auto-Conversation-ID", "conversation")
	request.Header.Set("X-Auto-Approval-Token", "conversation-token")
	response := httptest.NewRecorder()
	finished := make(chan struct{})
	go func() { server.routes().ServeHTTP(response, request); close(finished) }()

	var approvalID string
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		server.mu.Lock()
		for id := range server.approvals {
			approvalID = id
		}
		server.mu.Unlock()
		if approvalID != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if approvalID == "" {
		t.Fatal("streaming approval was not registered")
	}
	resolve := httptest.NewRecorder()
	server.routes().ServeHTTP(resolve, httptest.NewRequest(http.MethodPost, "/api/approvals/"+approvalID, bytes.NewBufferString(`{"decision":"allow"}`)))
	if resolve.Code != http.StatusAccepted {
		t.Fatalf("resolve streaming approval status: %d body=%s", resolve.Code, resolve.Body.String())
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("streaming approval did not return")
	}
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"permissionDecision":"allow"`) {
		t.Fatalf("streaming approval response: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestClaudeCLIRunnerStreamsClaudeEvents(t *testing.T) {
	scriptPath := filepath.Join(t.TempDir(), "fake-claude")
	script := "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\"}'\nprintf '%s\\n' '{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"runner output\"}]}}'\nprintf '%s\\n' '{\"type\":\"assistant\",\"parent_tool_use_id\":\"agent-call\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"subagent output\"}]}}'\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake Claude CLI: %v", err)
	}
	runner := claudeCLIRunner{config: Config{ClaudePath: scriptPath}}
	sink := &recordingSink{}
	err := runner.Run(context.Background(), AgentRunRequest{
		SessionID:      "00000000-0000-4000-8000-000000000000",
		ProjectPath:    t.TempDir(),
		Prompt:         "test",
		PermissionMode: "full_control",
	}, sink)
	if err != nil {
		t.Fatalf("run fake Claude CLI: %v", err)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.initialized != 1 || len(sink.texts) != 2 || sink.texts[0] != "runner output" || sink.parents[0] != "" || sink.texts[1] != "subagent output" || sink.parents[1] != "agent-call" || !containsArgument(sink.events, "system") || !containsArgument(sink.events, "assistant") {
		t.Fatalf("unexpected Claude stream: initialized=%d texts=%q parents=%q events=%q", sink.initialized, sink.texts, sink.parents, sink.events)
	}
}

func TestFailedFirstRunDoesNotMarkClaudeSessionInitialized(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	projectPath := t.TempDir()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, projectPath, now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,claude_initialized,is_current,created_at) values ('conversation','project','00000000-0000-4000-8000-000000000000','idle',0,1,?)`, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	requests := make(chan AgentRunRequest, 2)
	attempt := 0
	server.runner = runnerFunc(func(_ context.Context, request AgentRunRequest, sink AgentRunSink) error {
		attempt++
		requests <- request
		if attempt == 1 {
			return errors.New("simulated initial startup failure")
		}
		sink.Event("system", json.RawMessage(`{"type":"system","subtype":"init"}`))
		sink.SessionInitialized()
		sink.AssistantText("ready", "")
		return nil
	})

	handler := server.routes()
	for _, content := range []string{"first", "second"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/conversations/conversation/messages", bytes.NewBufferString(`{"content":"`+content+`"}`)))
		if response.Code != http.StatusAccepted {
			t.Fatalf("send %q status: %d body=%s", content, response.Code, response.Body.String())
		}
		select {
		case request := <-requests:
			if request.Resume {
				t.Fatalf("%q incorrectly resumed a session that never initialized", content)
			}
		case <-time.After(time.Second):
			t.Fatalf("runner did not receive %q", content)
		}
		waitForConversationIdle(t, server, "conversation")
	}
	var initialized bool
	if err := server.db.QueryRow(`select claude_initialized from conversations where id='conversation'`).Scan(&initialized); err != nil {
		t.Fatalf("read session initialization state: %v", err)
	}
	if !initialized {
		t.Fatal("successful system/init event did not initialize the Claude session")
	}
	var title string
	if err := server.db.QueryRow(`select title from conversations where id='conversation'`).Scan(&title); err != nil {
		t.Fatalf("read conversation title: %v", err)
	}
	if title != "first" {
		t.Fatalf("first user message did not become conversation title: %q", title)
	}
}

func TestNewRecoversInterruptedRuns(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,claude_initialized,is_current,created_at) values ('conversation','project','00000000-0000-4000-8000-000000000000','running',0,1,?)`, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values ('running-run','conversation','running',?),('queued-run','conversation','queued',?)`, now, now); err != nil {
		t.Fatalf("insert runs: %v", err)
	}
	if _, err := server.db.Exec(`insert into shortcut_runs (id,conversation_id,run_id,rendered_content,action,status,created_at) values ('shortcut-run','conversation','queued-run','queued shortcut','prompt','accepted',?)`, now); err != nil {
		t.Fatalf("insert shortcut run: %v", err)
	}
	if _, err := server.db.Exec(`insert into tasks (id,project_id,title,description,acceptance_criteria,priority,position,status,created_at,updated_at) values ('task','project','task','description','criteria','normal',1,'running',?,?)`, now, now); err != nil {
		t.Fatalf("insert task: %v", err)
	}
	if _, err := server.db.Exec(`insert into task_runs (id,task_id,conversation_id,run_id,sequence,status,prompt_snapshot,acceptance_snapshot,failure_reason,created_at) values ('task-run','task','conversation','queued-run',1,'queued','prompt','criteria','',?)`, now); err != nil {
		t.Fatalf("insert task run: %v", err)
	}
	config := server.config
	server.Close()
	recovered, err := New(context.Background(), config)
	if err != nil {
		t.Fatalf("restart server: %v", err)
	}
	t.Cleanup(recovered.Close)
	var runningStatus, queuedStatus, conversationStatus, eventType, shortcutStatus, taskStatus, taskRunStatus string
	var interruptedEvents int
	if err := recovered.db.QueryRow(`select status from runs where id='running-run'`).Scan(&runningStatus); err != nil {
		t.Fatalf("read recovered running run: %v", err)
	}
	if err := recovered.db.QueryRow(`select status from runs where id='queued-run'`).Scan(&queuedStatus); err != nil {
		t.Fatalf("read recovered queued run: %v", err)
	}
	if err := recovered.db.QueryRow(`select status from conversations where id='conversation'`).Scan(&conversationStatus); err != nil {
		t.Fatalf("read recovered conversation: %v", err)
	}
	if err := recovered.db.QueryRow(`select type from events where run_id='queued-run' order by created_at desc limit 1`).Scan(&eventType); err != nil {
		t.Fatalf("read recovery event: %v", err)
	}
	if err := recovered.db.QueryRow(`select count(*) from events where conversation_id='conversation' and type='run.interrupted'`).Scan(&interruptedEvents); err != nil {
		t.Fatalf("count recovery events: %v", err)
	}
	if err := recovered.db.QueryRow(`select status from shortcut_runs where id='shortcut-run'`).Scan(&shortcutStatus); err != nil {
		t.Fatalf("read recovered shortcut run: %v", err)
	}
	if err := recovered.db.QueryRow(`select status from tasks where id='task'`).Scan(&taskStatus); err != nil {
		t.Fatalf("read recovered task: %v", err)
	}
	if err := recovered.db.QueryRow(`select status from task_runs where id='task-run'`).Scan(&taskRunStatus); err != nil {
		t.Fatalf("read recovered task run: %v", err)
	}
	if runningStatus != "interrupted" || queuedStatus != "interrupted" || shortcutStatus != "interrupted" || taskStatus != taskActionRequired || taskRunStatus != "interrupted" || conversationStatus != "idle" || eventType != "run.interrupted" || interruptedEvents != 2 {
		t.Fatalf("unexpected recovery state: running=%q queued=%q shortcut=%q task=%q taskRun=%q conversation=%q event=%q events=%d", runningStatus, queuedStatus, shortcutStatus, taskStatus, taskRunStatus, conversationStatus, eventType, interruptedEvents)
	}
}

func TestNewReleasesRunningConversationWithoutRun(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,claude_initialized,is_current,created_at) values ('conversation','project','00000000-0000-4000-8000-000000000000','running',0,1,?)`, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	config := server.config
	server.Close()
	recovered, err := New(context.Background(), config)
	if err != nil {
		t.Fatalf("restart server: %v", err)
	}
	t.Cleanup(recovered.Close)
	var status string
	if err := recovered.db.QueryRow(`select status from conversations where id='conversation'`).Scan(&status); err != nil {
		t.Fatalf("read recovered conversation: %v", err)
	}
	if status != "idle" {
		t.Fatalf("orphaned running conversation was not released: %q", status)
	}
}

func TestClearConversationStopsIdleClaudeSessionAndRejectsOldMessages(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,agent_id,agent_session_id,agent_runtime_id,execution_policy,status,permission_mode,claude_initialized,agent_initialized,is_current,created_at) values ('conversation','project','00000000-0000-4000-8000-000000000000','claude-code','00000000-0000-4000-8000-000000000000','wsl-local','full_control','idle','full_control',1,1,1,?)`, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	oldSession := newIdleAgentSession()
	server.mu.Lock()
	server.sessions["conversation"] = &activeAgentSession{agent: oldSession}
	server.mu.Unlock()

	cleared := httptest.NewRecorder()
	server.routes().ServeHTTP(cleared, httptest.NewRequest(http.MethodPost, "/api/conversations/conversation/clear", nil))
	if cleared.Code != http.StatusCreated {
		t.Fatalf("clear conversation status: %d body=%s", cleared.Code, cleared.Body.String())
	}
	var fresh Conversation
	if err := json.NewDecoder(cleared.Body).Decode(&fresh); err != nil {
		t.Fatalf("decode fresh conversation: %v", err)
	}
	if fresh.ID == "conversation" || fresh.AgentID != "claude-code" || fresh.PermissionMode != "full_control" || fresh.AgentSessionID == "" || fresh.AgentSessionID == "00000000-0000-4000-8000-000000000000" {
		t.Fatalf("unexpected fresh conversation: %#v", fresh)
	}
	select {
	case <-oldSession.Done():
	case <-time.After(time.Second):
		t.Fatal("clear did not stop the old Claude session")
	}

	var oldCurrent, freshCurrent bool
	if err := server.db.QueryRow(`select is_current from conversations where id='conversation'`).Scan(&oldCurrent); err != nil {
		t.Fatalf("read old conversation: %v", err)
	}
	if err := server.db.QueryRow(`select is_current from conversations where id=?`, fresh.ID).Scan(&freshCurrent); err != nil {
		t.Fatalf("read fresh conversation: %v", err)
	}
	if oldCurrent || !freshCurrent {
		t.Fatalf("unexpected current flags: old=%t fresh=%t", oldCurrent, freshCurrent)
	}

	oldMessage := httptest.NewRecorder()
	server.routes().ServeHTTP(oldMessage, httptest.NewRequest(http.MethodPost, "/api/conversations/conversation/messages", bytes.NewBufferString(`{"content":"must not run"}`)))
	if oldMessage.Code != http.StatusConflict {
		t.Fatalf("old conversation message status: %d body=%s", oldMessage.Code, oldMessage.Body.String())
	}
}

func TestClearConversationDoesNotBlockActivationWhileStoppingOldSession(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,agent_id,agent_session_id,agent_runtime_id,execution_policy,status,permission_mode,is_current,created_at) values ('conversation','project','00000000-0000-4000-8000-000000000000','claude-code','00000000-0000-4000-8000-000000000000','wsl-local','full_control','idle','full_control',1,?)`, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	oldSession := newBlockingStopAgentSession()
	server.runner = blockingStreamingRunner{session: oldSession}
	released := false
	defer func() {
		if !released {
			close(oldSession.releaseStop)
		}
	}()
	server.mu.Lock()
	server.sessions["conversation"] = &activeAgentSession{agent: oldSession}
	server.mu.Unlock()

	clearDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/conversations/conversation/clear", nil))
		clearDone <- response
	}()
	select {
	case <-oldSession.stopEntered:
	case <-time.After(time.Second):
		t.Fatal("clear did not begin stopping the old session")
	}

	activationDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/conversations/conversation/activate", nil))
		activationDone <- response
	}()
	select {
	case response := <-activationDone:
		if response.Code != http.StatusOK {
			t.Fatalf("activation status: %d body=%s", response.Code, response.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("activation remained blocked by session cleanup")
	}

	var currentID string
	if err := server.db.QueryRow(`select id from conversations where project_id='project' and is_current=1`).Scan(&currentID); err != nil {
		t.Fatalf("read current conversation: %v", err)
	}
	if currentID != "conversation" {
		t.Fatalf("activated conversation is not current: %q", currentID)
	}
	server.mu.Lock()
	stopping := server.sessions["conversation"] != nil && server.sessions["conversation"].stopping
	server.mu.Unlock()
	if !stopping {
		t.Fatal("old session was not marked stopping before the lifecycle lock was released")
	}
	oldMessage := httptest.NewRecorder()
	server.routes().ServeHTTP(oldMessage, httptest.NewRequest(http.MethodPost, "/api/conversations/conversation/messages", bytes.NewBufferString(`{"content":"must not restart while stopping"}`)))
	if oldMessage.Code != http.StatusConflict {
		t.Fatalf("reactivated stopping conversation message status: %d body=%s", oldMessage.Code, oldMessage.Body.String())
	}

	close(oldSession.releaseStop)
	released = true
	if response := <-clearDone; response.Code != http.StatusCreated {
		t.Fatalf("clear status: %d body=%s", response.Code, response.Body.String())
	}
}

func TestTaskDispatchRejectsConversationClearedAfterRequestStarted(t *testing.T) {
	server, projectID, conversationID := seedTaskConversation(t)
	taskID := createTaskForTest(t, server.routes(), projectID, "Do not cross the clear boundary")

	cleared := httptest.NewRecorder()
	server.routes().ServeHTTP(cleared, httptest.NewRequest(http.MethodPost, "/api/conversations/"+conversationID+"/clear", nil))
	if cleared.Code != http.StatusCreated {
		t.Fatalf("clear status: %d body=%s", cleared.Code, cleared.Body.String())
	}
	var fresh Conversation
	if err := json.NewDecoder(cleared.Body).Decode(&fresh); err != nil {
		t.Fatalf("decode fresh conversation: %v", err)
	}

	dispatch := httptest.NewRecorder()
	server.routes().ServeHTTP(dispatch, httptest.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/dispatch", bytes.NewBufferString(`{"conversationId":"`+conversationID+`"}`)))
	if dispatch.Code != http.StatusConflict {
		t.Fatalf("stale dispatch status: %d body=%s", dispatch.Code, dispatch.Body.String())
	}
	var messages, taskRuns int
	if err := server.db.QueryRow(`select count(*) from messages where conversation_id=?`, fresh.ID).Scan(&messages); err != nil {
		t.Fatalf("count fresh messages: %v", err)
	}
	if err := server.db.QueryRow(`select count(*) from task_runs where task_id=?`, taskID).Scan(&taskRuns); err != nil {
		t.Fatalf("count task runs: %v", err)
	}
	if messages != 0 || taskRuns != 0 {
		t.Fatalf("stale dispatch crossed clear boundary: messages=%d taskRuns=%d", messages, taskRuns)
	}
}

func TestNewWithRunnerDoesNotRequireClaudeApprovalHook(t *testing.T) {
	allowedRoot := t.TempDir()
	server, err := NewWithRunner(context.Background(), Config{
		DatabasePath: filepath.Join(t.TempDir(), "auto.db"),
		AllowedRoot:  allowedRoot,
		ApprovalHook: filepath.Join(t.TempDir(), "does-not-exist"),
	}, runnerFunc(func(context.Context, AgentRunRequest, AgentRunSink) error { return nil }))
	if err != nil {
		t.Fatalf("create server with a non-Claude runner: %v", err)
	}
	t.Cleanup(server.Close)
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects", bytes.NewBufferString(`{"path":"`+allowedRoot+`"}`)))
	if response.Code != http.StatusCreated {
		t.Fatalf("create project with a non-Claude runner status: %d body=%s", response.Code, response.Body.String())
	}
}

func TestIsLocalWebOrigin(t *testing.T) {
	for _, test := range []struct {
		origin string
		valid  bool
	}{
		{origin: "http://127.0.0.1:5173", valid: true},
		{origin: "http://localhost:5174", valid: true},
		{origin: "https://localhost:8443", valid: true},
		{origin: "http://example.com:5173", valid: false},
		{origin: "not-a-url", valid: false},
	} {
		if actual := isLocalWebOrigin(test.origin); actual != test.valid {
			t.Fatalf("origin %q validity: got %t want %t", test.origin, actual, test.valid)
		}
	}
}

func TestCloseCancelsAndWaitsForActiveRunner(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	projectPath := t.TempDir()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, projectPath, now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,claude_initialized,is_current,created_at) values ('conversation','project','00000000-0000-4000-8000-000000000000','idle',0,1,?)`, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	started := make(chan struct{})
	stopped := make(chan struct{})
	server.runner = runnerFunc(func(ctx context.Context, _ AgentRunRequest, _ AgentRunSink) error {
		close(started)
		<-ctx.Done()
		close(stopped)
		return ctx.Err()
	})

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/conversations/conversation/messages", bytes.NewBufferString(`{"content":"start"}`)))
	if response.Code != http.StatusAccepted {
		t.Fatalf("start run status: %d body=%s", response.Code, response.Body.String())
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("runner did not start")
	}
	server.Close()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("server close did not cancel the active runner")
	}
}

func TestStopCompletedRunIsIdempotent(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,claude_initialized,is_current,created_at) values ('conversation','project','00000000-0000-4000-8000-000000000000','idle',0,1,?)`, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at,completed_at) values ('run','conversation','completed',?,?)`, now, now); err != nil {
		t.Fatalf("insert completed run: %v", err)
	}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/runs/run/stop", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("stop completed run status: %d body=%s", response.Code, response.Body.String())
	}
	var result map[string]string
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode completed run response: %v", err)
	}
	if result["status"] != "completed" {
		t.Fatalf("unexpected completed run response: %q", result["status"])
	}
}

func TestTaskCreationValidatesInput(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	projectID := "project"
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values (?,?,?,?,?,?,?)`, projectID, projectID, filepath.Join(t.TempDir(), projectID), "wsl-local", "main", true, now); err != nil {
		t.Fatalf("insert %s: %v", projectID, err)
	}
	handler := server.routes()

	invalid := httptest.NewRecorder()
	handler.ServeHTTP(invalid, httptest.NewRequest(http.MethodPost, "/api/projects/project/tasks", bytes.NewBufferString(`{"title":"missing fields"}`)))
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid task status: %d body=%s", invalid.Code, invalid.Body.String())
	}

	createTaskForTest(t, handler, "project", "Create API")
	createTaskForTest(t, handler, "project", "Create UI")
}

func TestMigrateTasksRemovesLegacyExecutionModeColumn(t *testing.T) {
	server := newTestServer(t)
	if _, err := server.db.Exec(`alter table tasks add column execution_mode text not null default 'manual'`); err != nil {
		t.Fatalf("add legacy execution mode: %v", err)
	}
	if err := server.migrateTasks(context.Background()); err != nil {
		t.Fatalf("remove legacy execution mode: %v", err)
	}
	rows, err := server.db.Query(`pragma table_info(tasks)`)
	if err != nil {
		t.Fatalf("inspect task schema: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatalf("read task schema: %v", err)
		}
		if name == "execution_mode" {
			t.Fatal("legacy execution_mode column remains")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate task schema: %v", err)
	}
}

func TestTaskExposesActiveOrchestrationReviewStatus(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	taskID := createTaskForTest(t, server.routes(), "project", "Review visibility")
	if _, err := server.db.Exec(`insert into task_orchestration_jobs (id,project_id,task_id,queue_position,status,lease_token,policy_snapshot,created_at,updated_at) values ('job','project',?,1,'checking',9,'{}',?,?)`, taskID, now, now); err != nil {
		t.Fatalf("insert checking job: %v", err)
	}
	task, err := server.taskByID(context.Background(), taskID)
	if err != nil {
		t.Fatalf("load task: %v", err)
	}
	if task.OrchestrationStatus != "checking" || task.OrchestrationUpdatedAt == nil {
		t.Fatalf("orchestration visibility status=%q updated=%v", task.OrchestrationStatus, task.OrchestrationUpdatedAt)
	}

	stop := server.keepOrchestrationReviewAlive(context.Background(), OrchestrationJob{ID: "job", TaskID: taskID, LeaseToken: 9})
	stop()
	var updated time.Time
	if err := server.db.QueryRow(`select updated_at from task_orchestration_jobs where id='job'`).Scan(&updated); err != nil {
		t.Fatalf("load review heartbeat: %v", err)
	}
	if updated.Before(now) {
		t.Fatalf("heartbeat did not update review activity: %s before %s", updated, now)
	}
}

func TestOrchestrationReviewSinkStreamsTextAndAccumulatesJSON(t *testing.T) {
	var streamed []string
	sink := &orchestrationReviewSink{onText: func(content string) { streamed = append(streamed, content) }}
	sink.AssistantText("{\"verdict\":", "")
	sink.AssistantText("\"pass\"}", "")
	sink.AssistantText("   ", "")
	if got := sink.String(); got != "{\"verdict\":\"pass\"}" {
		t.Fatalf("review output=%q", got)
	}
	if !reflect.DeepEqual(streamed, []string{"{\"verdict\":", "\"pass\"}"}) {
		t.Fatalf("streamed output=%q", streamed)
	}
}

func TestAutomaticOrchestrationIntegratesVerifiedTaskToDev(t *testing.T) {
	server := newTestServer(t)
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-b", "main"}, {"config", "user.email", "test@example.com"}, {"config", "user.name", "Test User"}} {
		command := exec.Command("git", args...)
		command.Dir = repo
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("baseline\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "README.md"}, {"commit", "-m", "baseline"}} {
		command := exec.Command("git", args...)
		command.Dir = repo
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,?,?,1,?)`, repo, server.localRunnerID(), "main", now); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,claude_initialized,is_current,created_at) values ('conversation','project','00000000-0000-4000-8000-000000000000','idle',0,1,?)`, now); err != nil {
		t.Fatal(err)
	}
	server.runner = runnerFunc(func(_ context.Context, request AgentRunRequest, sink AgentRunSink) error {
		if strings.Contains(request.Prompt, "独立代码审查") {
			sink.AssistantText(`{"verdict":"pass","blockingFindings":[],"nonBlockingFindings":[],"acceptanceCoverage":["implemented"],"testGaps":[],"reviewedCommit":"`+mustGitHead(t, request.ProjectPath)+`"}`, "")
			return nil
		}
		return os.WriteFile(filepath.Join(request.ProjectPath, "implemented.txt"), []byte("done\n"), 0o600)
	})
	_, err := server.db.Exec(`insert into project_orchestration_configs (project_id,enabled,main_branch,dev_branch,verification_commands,max_fix_rounds,frozen_reason,updated_at) values ('project',1,'main','dev','["ls"]',3,'',?)`, now)
	if err != nil {
		t.Fatal(err)
	}
	taskID := createTaskForTest(t, server.routes(), "project", "Implement file")
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/orchestration/enqueue", nil))
	if response.Code != http.StatusAccepted {
		t.Fatalf("enqueue: %d body=%s", response.Code, response.Body.String())
	}
	deadline := time.Now().Add(10 * time.Second)
	var status string
	for time.Now().Before(deadline) {
		if err := server.db.QueryRow(`select status from task_orchestration_jobs where task_id=?`, taskID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status == "awaiting_main" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if status != "awaiting_main" {
		var reason string
		_ = server.db.QueryRow(`select last_error from task_orchestration_jobs where task_id=?`, taskID).Scan(&reason)
		t.Fatalf("job status=%q reason=%q", status, reason)
	}
	var branch string
	if err := server.db.QueryRow(`select task_branch from git_task_records where job_id=(select id from task_orchestration_jobs where task_id=?)`, taskID).Scan(&branch); err != nil {
		t.Fatalf("load task branch: %v", err)
	}
	if !strings.HasPrefix(branch, "自动编排-") {
		t.Fatalf("task branch = %q, want automatic orchestration branch", branch)
	}
	var orchestrationConversationID, taskRunConversationID string
	if err := server.db.QueryRow(`select conversation_id from git_task_records where job_id=(select id from task_orchestration_jobs where task_id=?)`, taskID).Scan(&orchestrationConversationID); err != nil {
		t.Fatalf("load orchestration conversation: %v", err)
	}
	if err := server.db.QueryRow(`select conversation_id from task_runs where task_id=? order by sequence desc limit 1`, taskID).Scan(&taskRunConversationID); err != nil {
		t.Fatalf("load task run conversation: %v", err)
	}
	if orchestrationConversationID == "" || taskRunConversationID != orchestrationConversationID || orchestrationConversationID == "conversation" {
		t.Fatalf("task run must use its dedicated conversation: record=%q task_run=%q", orchestrationConversationID, taskRunConversationID)
	}
	var isCurrent bool
	if err := server.db.QueryRow(`select is_current from conversations where id=?`, orchestrationConversationID).Scan(&isCurrent); err != nil {
		t.Fatalf("load orchestration conversation state: %v", err)
	}
	if isCurrent {
		t.Fatal("orchestration conversation must not replace the current conversation")
	}
	output, err := server.gitOutput(context.Background(), repo, "show", branch+":implemented.txt")
	if err != nil || strings.TrimSpace(output) != "done" {
		t.Fatalf("task branch missing implementation: output=%q err=%v", output, err)
	}
}

func TestOrchestrationConfigSelectsAgent(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPut, "/api/projects/project/orchestration/config", strings.NewReader(`{"enabled":false,"mainBranch":"main","agentId":"codex","verificationCommands":["true"],"maxFixRounds":3}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("save config: %d body=%s", response.Code, response.Body.String())
	}
	cfg, err := server.orchestrationConfig(context.Background(), "project")
	if err != nil || cfg.AgentID != "codex" {
		t.Fatalf("load config agent=%q err=%v", cfg.AgentID, err)
	}
	invalid := httptest.NewRecorder()
	server.routes().ServeHTTP(invalid, httptest.NewRequest(http.MethodPut, "/api/projects/project/orchestration/config", strings.NewReader(`{"enabled":false,"mainBranch":"main","agentId":"other","verificationCommands":["true"],"maxFixRounds":3}`)))
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid agent status=%d body=%s", invalid.Code, invalid.Body.String())
	}
	emptyCommands := httptest.NewRecorder()
	server.routes().ServeHTTP(emptyCommands, httptest.NewRequest(http.MethodPut, "/api/projects/project/orchestration/config", strings.NewReader(`{"enabled":true,"mainBranch":"main","agentId":"codex","verificationCommands":[],"maxFixRounds":3}`)))
	if emptyCommands.Code != http.StatusBadRequest {
		t.Fatalf("enabled config without verification commands status=%d body=%s", emptyCommands.Code, emptyCommands.Body.String())
	}
	disabledEmptyCommands := httptest.NewRecorder()
	server.routes().ServeHTTP(disabledEmptyCommands, httptest.NewRequest(http.MethodPut, "/api/projects/project/orchestration/config", strings.NewReader(`{"enabled":false,"mainBranch":"main","agentId":"codex","verificationCommands":[],"maxFixRounds":3}`)))
	if disabledEmptyCommands.Code != http.StatusOK {
		t.Fatalf("disabled config without verification commands status=%d body=%s", disabledEmptyCommands.Code, disabledEmptyCommands.Body.String())
	}
}

func TestOrchestrationConversationUsesConfiguredAgent(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	project := Project{ID: "project", Name: "project", Path: t.TempDir(), Runner: server.localRunnerID(), GitBranch: "main", CreatedAt: now}
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values (?,?,?,?,?,1,?)`, project.ID, project.Name, project.Path, project.Runner, project.GitBranch, now); err != nil {
		t.Fatal(err)
	}
	conversationID, err := server.createOrchestrationConversation(context.Background(), project, Task{ID: "task", Title: "Run with Codex"}, OrchestrationConfig{AgentID: "codex"})
	if err != nil {
		t.Fatalf("create orchestration conversation: %v", err)
	}
	var agentID, permissionMode string
	var isCurrent bool
	if err := server.db.QueryRow(`select agent_id,permission_mode,is_current from conversations where id=?`, conversationID).Scan(&agentID, &permissionMode, &isCurrent); err != nil {
		t.Fatal(err)
	}
	if agentID != "codex" || permissionMode != "workspace_write" || isCurrent {
		t.Fatalf("conversation agent=%q permission=%q isCurrent=%v", agentID, permissionMode, isCurrent)
	}
}

func TestPostOrchestrationMessageBroadcastsWithoutRunEvent(t *testing.T) {
	server, projectID, conversationID := seedTaskConversation(t)
	taskID := createTaskForTest(t, server.routes(), projectID, "Broadcast orchestration message")
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into task_orchestration_jobs (id,project_id,task_id,queue_position,status,policy_snapshot,created_at,updated_at) values ('job',?,?,1,'checking','{}',?,?)`, projectID, taskID, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into git_task_records (job_id,base_dev_sha,task_branch,worktree_path,conversation_id,created_at,updated_at) values ('job','','','',?,?,?)`, conversationID, now, now); err != nil {
		t.Fatal(err)
	}
	sub := &subscriber{send: make(chan []byte, 1)}
	server.mu.Lock()
	server.subscribers[conversationID] = map[*websocket.Conn]*subscriber{nil: sub}
	server.mu.Unlock()

	server.postOrchestrationMessage(context.Background(), OrchestrationJob{ID: "job"}, "verification passed")
	select {
	case payload := <-sub.send:
		var event Event
		if err := json.Unmarshal(payload, &event); err != nil {
			t.Fatalf("decode broadcast event: %v", err)
		}
		if event.Type != "assistant.message" || event.RunID != "" {
			t.Fatalf("broadcast event=%#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("orchestration message was not broadcast")
	}
	var messages, events int
	if err := server.db.QueryRow(`select count(*) from messages where conversation_id=?`, conversationID).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if err := server.db.QueryRow(`select count(*) from events where conversation_id=?`, conversationID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if messages != 1 || events != 0 {
		t.Fatalf("messages=%d events=%d, want one durable message and no invalid run event", messages, events)
	}
}

func TestOrchestrationConversationRejectsExternalMessages(t *testing.T) {
	server, projectID, conversationID := seedTaskConversation(t)
	taskID := createTaskForTest(t, server.routes(), projectID, "Reject external orchestration message")
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into task_orchestration_jobs (id,project_id,task_id,queue_position,status,policy_snapshot,created_at,updated_at) values ('job',?,?,1,'queued','{}',?,?)`, projectID, taskID, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into git_task_records (job_id,base_dev_sha,task_branch,worktree_path,conversation_id,created_at,updated_at) values ('job','','','',?,?,?)`, conversationID, now, now); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/conversations/"+conversationID+"/messages", strings.NewReader(`{"content":"do not interrupt"}`)))
	if response.Code != http.StatusConflict {
		t.Fatalf("send to orchestration conversation: %d body=%s", response.Code, response.Body.String())
	}
}

func TestOrchestrationConversationRejectsExternalRunStop(t *testing.T) {
	server, projectID, conversationID := seedTaskConversation(t)
	taskID := createTaskForTest(t, server.routes(), projectID, "Reject external orchestration stop")
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into task_orchestration_jobs (id,project_id,task_id,queue_position,status,policy_snapshot,created_at,updated_at) values ('job',?,?,1,'implementing','{}',?,?)`, projectID, taskID, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into git_task_records (job_id,base_dev_sha,task_branch,worktree_path,conversation_id,created_at,updated_at) values ('job','','','',?,?,?)`, conversationID, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values ('run',?,'running',?)`, conversationID, now); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/runs/run/stop", nil))
	if response.Code != http.StatusConflict {
		t.Fatalf("stop orchestration run: %d body=%s", response.Code, response.Body.String())
	}
}

func TestResumeCommittedOrchestrationTaskKeepsTaskAwaitingReview(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	taskID := createTaskForTest(t, server.routes(), projectID, "Resume committed orchestration")
	now := time.Now().UTC()
	if _, err := server.db.Exec(`update tasks set status='awaiting_review' where id=?`, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into task_orchestration_jobs (id,project_id,task_id,queue_position,status,lease_token,policy_snapshot,created_at,updated_at) values ('job',?,?,1,'needs_human',7,'{}',?,?)`, projectID, taskID, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into git_task_records (job_id,base_dev_sha,task_branch,worktree_path,task_commit_sha,conversation_id,created_at,updated_at) values ('job','','branch','worktree','commit','',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/orchestration/resume", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("resume: %d body=%s", response.Code, response.Body.String())
	}
	assertTaskStatus(t, server, taskID, taskAwaitingReview)
}

func TestVerificationCommandArgsUsePlatformShell(t *testing.T) {
	tests := []struct {
		goos       string
		wantBinary string
		wantArgs   []string
	}{
		{goos: "windows", wantBinary: "powershell.exe", wantArgs: []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-Command", "npm test"}},
		{goos: "linux", wantBinary: "sh", wantArgs: []string{"-lc", "npm test"}},
	}
	for _, test := range tests {
		binary, args := verificationCommandArgs(test.goos, "npm test")
		if binary != test.wantBinary || !reflect.DeepEqual(args, test.wantArgs) {
			t.Fatalf("verification command for %s = %q %q, want %q %q", test.goos, binary, args, test.wantBinary, test.wantArgs)
		}
	}
}

func TestParseOrchestrationReviewExtractsJSONFromAgentNarration(t *testing.T) {
	commit := "abc123"
	output := "审查完成，以下是结果：\n```json\n{\"verdict\":\"pass\",\"blockingFindings\":[],\"nonBlockingFindings\":[],\"acceptanceCoverage\":[\"README updated\"],\"testGaps\":[],\"reviewedCommit\":\"abc123\"}\n```\n无需额外操作。"
	review, err := parseOrchestrationReview(output, commit)
	if err != nil {
		t.Fatalf("parse narrated review: %v", err)
	}
	if review.Verdict != "pass" || review.ReviewedCommit != commit {
		t.Fatalf("review=%#v", review)
	}
}

func TestParseOrchestrationReviewRejectsUnstructuredAgentOutput(t *testing.T) {
	if _, err := parseOrchestrationReview("README 已更新，建议提交改动。", "abc123"); err == nil {
		t.Fatal("expected unstructured review output to be rejected")
	}
}

func TestRetryOrchestrationJobAllowsConfiguredRepairRoundsAfterInitialAttempt(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	taskID := createTaskForTest(t, server.routes(), projectID, "Retry limit")
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into task_orchestration_jobs (id,project_id,task_id,queue_position,status,attempt,lease_token,policy_snapshot,created_at,updated_at) values ('job',?,?,1,'checking',3,7,'{}',?,?)`, projectID, taskID, now, now); err != nil {
		t.Fatal(err)
	}
	server.orchestrationMu.Lock()
	server.orchestrationActive[projectID] = true
	server.orchestrationMu.Unlock()
	server.retryOrchestrationJob(context.Background(), OrchestrationJob{ID: "job", ProjectID: projectID, TaskID: taskID, Status: orchestrationChecking, Attempt: 3, LeaseToken: 7}, OrchestrationConfig{MaxFixRounds: 3}, errors.New("review failed"))
	var status string
	if err := server.db.QueryRow(`select status from task_orchestration_jobs where id='job'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != orchestrationQueued {
		t.Fatalf("status after third repair round=%q, want queued", status)
	}
}

func TestWindowsPowerShellVerificationCommandRunsLS(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-specific verification command")
	}
	cmd := newVerificationCommand(context.Background(), "ls | Out-Null")
	cmd.Dir = t.TempDir()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Windows verification command failed: %v: %s", err, output)
	}
}

func TestAutomaticOrchestrationBranchUsesDateAndTaskShortID(t *testing.T) {
	branch := automaticOrchestrationBranch(time.Date(2026, time.August, 12, 0, 0, 0, 0, time.UTC), "12345678-90ab-cdef")
	if branch != "自动编排-20260812-12345678" {
		t.Fatalf("branch = %q", branch)
	}
}

func TestAutomaticOrchestrationDoesNotAdvanceDevWhenIntegrationVerificationFails(t *testing.T) {
	server := newTestServer(t)
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-b", "main"}, {"config", "user.email", "test@example.com"}, {"config", "user.name", "Test User"}} {
		command := exec.Command("git", args...)
		command.Dir = repo
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("baseline\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "README.md"}, {"commit", "-m", "baseline"}} {
		command := exec.Command("git", args...)
		command.Dir = repo
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	baseline := mustGitHead(t, repo)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,?,?,1,?)`, repo, server.localRunnerID(), "main", now); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,claude_initialized,is_current,created_at) values ('conversation','project','00000000-0000-4000-8000-000000000000','idle',0,1,?)`, now); err != nil {
		t.Fatal(err)
	}
	server.runner = runnerFunc(func(_ context.Context, request AgentRunRequest, sink AgentRunSink) error {
		if strings.Contains(request.Prompt, "独立代码审查") {
			sink.AssistantText(`{"verdict":"pass","blockingFindings":[],"nonBlockingFindings":[],"acceptanceCoverage":["implemented"],"testGaps":[],"reviewedCommit":"`+mustGitHead(t, request.ProjectPath)+`"}`, "")
			return nil
		}
		return os.WriteFile(filepath.Join(request.ProjectPath, "implemented.txt"), []byte("done\n"), 0o600)
	})
	_, err := server.db.Exec(`insert into project_orchestration_configs (project_id,enabled,main_branch,dev_branch,verification_commands,max_fix_rounds,frozen_reason,updated_at) values ('project',1,'main','dev','["false"]',1,'',?)`, now)
	if err != nil {
		t.Fatal(err)
	}
	taskID := createTaskForTest(t, server.routes(), "project", "Integration check must protect dev")
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/orchestration/enqueue", nil))
	if response.Code != http.StatusAccepted {
		t.Fatalf("enqueue: %d body=%s", response.Code, response.Body.String())
	}
	deadline := time.Now().Add(10 * time.Second)
	var status string
	for time.Now().Before(deadline) {
		if err := server.db.QueryRow(`select status from task_orchestration_jobs where task_id=?`, taskID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status == orchestrationNeedsHuman {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if status != orchestrationNeedsHuman {
		t.Fatalf("job status=%q, want %q", status, orchestrationNeedsHuman)
	}
	main, err := server.gitOutput(context.Background(), repo, "rev-parse", "main")
	if err != nil {
		t.Fatalf("read main after failed verification: %v", err)
	}
	if strings.TrimSpace(main) != baseline {
		t.Fatalf("failed verification changed main: got=%s want=%s", strings.TrimSpace(main), baseline)
	}
}

func TestOrchestrationResumeClearsProjectFreeze(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	taskID := createTaskForTest(t, server.routes(), projectID, "Resume blocked orchestration")
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into project_orchestration_configs (project_id,enabled,main_branch,dev_branch,verification_commands,max_fix_rounds,frozen_reason,updated_at) values (?,0,'main','dev','[]',3,'verification failed',?)`, projectID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into task_orchestration_jobs (id,project_id,task_id,queue_position,status,lease_token,last_error,policy_snapshot,created_at,updated_at) values ('job',?,?,1,'needs_human',7,'verification failed','{}',?,?)`, projectID, taskID, now, now); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/orchestration/resume", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("resume: %d body=%s", response.Code, response.Body.String())
	}
	var status, frozen, lastError string
	if err := server.db.QueryRow(`select status,last_error from task_orchestration_jobs where id='job'`).Scan(&status, &lastError); err != nil {
		t.Fatal(err)
	}
	if err := server.db.QueryRow(`select frozen_reason from project_orchestration_configs where project_id=?`, projectID).Scan(&frozen); err != nil {
		t.Fatal(err)
	}
	if status != orchestrationQueued || frozen != "" || lastError != "" {
		t.Fatalf("resume must restart cleanly: status=%q frozen=%q lastError=%q", status, frozen, lastError)
	}
}

func TestStopOrchestrationJobPreservesWorktreeAndAllowsRestart(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	taskID := createTaskForTest(t, server.routes(), projectID, "Stop active orchestration")
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into project_orchestration_configs (project_id,enabled,main_branch,dev_branch,verification_commands,max_fix_rounds,frozen_reason,updated_at) values (?,1,'main','dev','[]',3,'previous failure',?)`, projectID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into task_orchestration_jobs (id,project_id,task_id,queue_position,status,lease_token,last_error,policy_snapshot,created_at,updated_at) values ('job',?,?,1,'implementing',7,'previous failure','{}',?,?)`, projectID, taskID, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into git_task_records (job_id,base_dev_sha,task_branch,worktree_path,created_at,updated_at) values ('job','base','自动编排-20260812-12345678','D:/worktrees/job',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/orchestration/stop", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("stop: %d body=%s", response.Code, response.Body.String())
	}
	var status, worktree, lastError string
	if err := server.db.QueryRow(`select job.status,record.worktree_path,job.last_error from task_orchestration_jobs job join git_task_records record on record.job_id=job.id where job.id='job'`).Scan(&status, &worktree, &lastError); err != nil {
		t.Fatal(err)
	}
	if status != orchestrationStopped || worktree != "D:/worktrees/job" || lastError != "" {
		t.Fatalf("stopped job=%q worktree=%q lastError=%q", status, worktree, lastError)
	}
	assertTaskStatus(t, server, taskID, taskActionRequired)

	response = httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/orchestration/resume", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("restart: %d body=%s", response.Code, response.Body.String())
	}
	if err := server.db.QueryRow(`select status,last_error from task_orchestration_jobs where id='job'`).Scan(&status, &lastError); err != nil {
		t.Fatal(err)
	}
	if status != orchestrationQueued || lastError != "" {
		t.Fatalf("restarted job status=%q lastError=%q", status, lastError)
	}
}

func TestStoppedOrchestrationJobIsSkippedByScheduler(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	stoppedTaskID := createTaskForTest(t, server.routes(), projectID, "Stopped orchestration")
	queuedTaskID := createTaskForTest(t, server.routes(), projectID, "Queued orchestration")
	now := time.Now().UTC()
	for _, job := range []struct {
		id, taskID, status string
		position           int
	}{
		{"stopped", stoppedTaskID, orchestrationStopped, 1},
		{"queued", queuedTaskID, orchestrationQueued, 2},
	} {
		if _, err := server.db.Exec(`insert into task_orchestration_jobs (id,project_id,task_id,queue_position,status,policy_snapshot,created_at,updated_at) values (?,?,?,?,?,'{}',?,?)`, job.id, projectID, job.taskID, job.position, job.status, now, now); err != nil {
			t.Fatal(err)
		}
	}
	job, err := server.nextOrchestrationJob(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	if job == nil || job.ID != "queued" {
		t.Fatalf("next job=%#v, want queued job", job)
	}
}

func TestListOrchestrationJobsReturnsWorktreePath(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	taskID := createTaskForTest(t, server.routes(), projectID, "List worktree")
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into task_orchestration_jobs (id,project_id,task_id,queue_position,status,policy_snapshot,created_at,updated_at) values ('job',?,?,1,'paused','{}',?,?)`, projectID, taskID, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into git_task_records (job_id,base_dev_sha,task_branch,worktree_path,created_at,updated_at) values ('job','','自动编排-20260812-12345678','D:/worktrees/job',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/orchestration", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("list: %d body=%s", response.Code, response.Body.String())
	}
	var jobs []OrchestrationJob
	if err := json.Unmarshal(response.Body.Bytes(), &jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].WorktreePath != "D:/worktrees/job" {
		t.Fatalf("jobs=%#v", jobs)
	}
}

func TestOrchestrationResumeMakesNeedsHumanTaskDispatchable(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	taskID := createTaskForTest(t, server.routes(), projectID, "Resume checking task")
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into project_orchestration_configs (project_id,enabled,main_branch,dev_branch,verification_commands,max_fix_rounds,frozen_reason,updated_at) values (?,0,'main','dev','[]',3,'test failed',?)`, projectID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`update tasks set status='awaiting_review' where id=?`, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into task_orchestration_jobs (id,project_id,task_id,queue_position,status,lease_token,last_error,policy_snapshot,created_at,updated_at) values ('job',?,?,1,'needs_human',7,'test failed','{}',?,?)`, projectID, taskID, now, now); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/orchestration/resume", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("resume: %d body=%s", response.Code, response.Body.String())
	}
	assertTaskStatus(t, server, taskID, taskActionRequired)
}

func TestOrchestrationPauseStopsAnActiveJob(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	taskID := createTaskForTest(t, server.routes(), projectID, "Pause active orchestration")
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into task_orchestration_jobs (id,project_id,task_id,queue_position,status,lease_token,policy_snapshot,created_at,updated_at) values ('job',?,?,1,'implementing',7,'{}',?,?)`, projectID, taskID, now, now); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/orchestration/pause", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("pause active job: %d body=%s", response.Code, response.Body.String())
	}
	var status string
	if err := server.db.QueryRow(`select status from task_orchestration_jobs where id='job'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != orchestrationPaused {
		t.Fatalf("paused job status=%q", status)
	}
}

func TestOrchestrationResumePausedCheckingTaskMakesItDispatchable(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	taskID := createTaskForTest(t, server.routes(), projectID, "Resume paused checking task")
	now := time.Now().UTC()
	if _, err := server.db.Exec(`update tasks set status='awaiting_review' where id=?`, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into task_orchestration_jobs (id,project_id,task_id,queue_position,status,lease_token,policy_snapshot,created_at,updated_at) values ('job',?,?,1,'paused',7,'{}',?,?)`, projectID, taskID, now, now); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/orchestration/resume", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("resume: %d body=%s", response.Code, response.Body.String())
	}
	assertTaskStatus(t, server, taskID, taskActionRequired)
}

func TestPauseOrchestrationJobCancelsActiveWorker(t *testing.T) {
	server := newTestServer(t)
	cancelled := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	server.orchestrationMu.Lock()
	server.orchestrationCancels["job"] = func() { cancel(); close(cancelled) }
	server.orchestrationMu.Unlock()
	server.cancelOrchestrationJob("job")
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("orchestration worker was not cancelled")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("orchestration cancellation callback was not invoked")
	}
}

func TestEnqueueTaskIsIdempotentForAnExistingJob(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	taskID := createTaskForTest(t, server.routes(), projectID, "Idempotent orchestration enqueue")
	task, err := server.taskByID(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	cfg := OrchestrationConfig{ProjectID: projectID, Enabled: true, AgentID: "claude-code", MainBranch: "main", VerificationCommands: []string{"true"}, MaxFixRounds: 1}
	first, err := server.enqueueTask(context.Background(), task, cfg)
	if err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	second, err := server.enqueueTask(context.Background(), task, cfg)
	if err != nil {
		t.Fatalf("second enqueue: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("idempotent enqueue returned %q, want %q", second.ID, first.ID)
	}
	var count int
	if err := server.db.QueryRow(`select count(*) from task_orchestration_jobs where task_id=?`, taskID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("job count=%d, want 1", count)
	}
}

func TestIndependentReviewIntentIsPersistedBeforeAgentStarts(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	taskID := createTaskForTest(t, server.routes(), projectID, "Persist review intent")
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into task_orchestration_jobs (id,project_id,task_id,queue_position,status,attempt,lease_token,policy_snapshot,created_at,updated_at) values ('job',?,?,1,'checking',2,7,'{}',?,?)`, projectID, taskID, now, now); err != nil {
		t.Fatal(err)
	}
	job := OrchestrationJob{ID: "job", ProjectID: projectID, TaskID: taskID, Attempt: 2, LeaseToken: 7}
	intentID, err := server.beginIndependentReviewIntent(context.Background(), job, "abc123")
	if err != nil {
		t.Fatalf("begin review intent: %v", err)
	}
	var phase, intentStatus, sha, verificationStatus string
	if err := server.db.QueryRow(`select intent.phase,intent.status,intent.reviewed_sha,verification.status from task_execution_intents intent join verification_runs verification on verification.id=intent.id where intent.id=?`, intentID).Scan(&phase, &intentStatus, &sha, &verificationStatus); err != nil {
		t.Fatal(err)
	}
	if phase != "review" || intentStatus != "running" || sha != "abc123" || verificationStatus != "running" {
		t.Fatalf("review intent phase=%q status=%q sha=%q verification=%q", phase, intentStatus, sha, verificationStatus)
	}
}

func TestWaitForOrchestrationJobShutdownWaitsForWorker(t *testing.T) {
	server := newTestServer(t)
	done := make(chan struct{})
	server.orchestrationMu.Lock()
	server.orchestrationDone["job"] = done
	server.orchestrationMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := server.waitForOrchestrationJobShutdown(ctx, "job"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait error=%v, want deadline exceeded", err)
	}
	close(done)
	if err := server.waitForOrchestrationJobShutdown(context.Background(), "job"); err != nil {
		t.Fatalf("wait after worker exit: %v", err)
	}
}

func TestOrchestrationDependencyAcceptsMergedPredecessor(t *testing.T) {
	server, projectID, conversationID := seedTaskConversation(t)
	predecessorID := createTaskForTest(t, server.routes(), projectID, "Integrated predecessor")
	dependentID := createTaskForTest(t, server.routes(), projectID, "Dependent task")
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into task_dependencies (task_id,predecessor_task_id,created_at) values (?,?,?)`, dependentID, predecessorID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`update tasks set status='awaiting_review' where id=?`, predecessorID); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into task_orchestration_jobs (id,project_id,task_id,queue_position,status,policy_snapshot,created_at,updated_at) values ('predecessor-job',?,?,1,'released_to_main','{}',?,?)`, projectID, predecessorID, now, now); err != nil {
		t.Fatal(err)
	}
	tx, err := server.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	err = server.validateTaskDispatchTx(context.Background(), tx, TaskRun{TaskID: dependentID, ConversationID: conversationID}, Conversation{ProjectID: projectID, ID: conversationID}, true)
	if err != nil {
		t.Fatalf("merged predecessor should unblock orchestrated task: %v", err)
	}
}

func TestOrchestrationAwaitingMainRejectsManualAndBatchReview(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	taskID := createTaskForTest(t, server.routes(), projectID, "Awaiting main merge")
	now := time.Now().UTC()
	if _, err := server.db.Exec(`update tasks set status='awaiting_review' where id=?`, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into task_orchestration_jobs (id,project_id,task_id,queue_position,status,policy_snapshot,created_at,updated_at) values ('job',?,?,1,'awaiting_main','{}',?,?)`, projectID, taskID, now, now); err != nil {
		t.Fatal(err)
	}

	manual := httptest.NewRecorder()
	server.routes().ServeHTTP(manual, httptest.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/review", bytes.NewBufferString(`{"action":"accept"}`)))
	if manual.Code != http.StatusConflict {
		t.Fatalf("manual review status=%d body=%s", manual.Code, manual.Body.String())
	}

	batch := httptest.NewRecorder()
	server.routes().ServeHTTP(batch, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/tasks/review-all", bytes.NewBufferString(`{}`)))
	if batch.Code != http.StatusOK {
		t.Fatalf("batch review status=%d body=%s", batch.Code, batch.Body.String())
	}
	var result struct{ Accepted, Skipped, Total int }
	if err := json.NewDecoder(batch.Body).Decode(&result); err != nil {
		t.Fatalf("decode batch review: %v", err)
	}
	if result.Accepted != 0 || result.Skipped != 1 || result.Total != 1 {
		t.Fatalf("batch review result=%+v", result)
	}
	assertTaskStatus(t, server, taskID, taskAwaitingReview)

	if _, err := server.db.Exec(`update task_orchestration_jobs set status='integrated_to_dev' where id='job'`); err != nil {
		t.Fatal(err)
	}
	legacyManual := httptest.NewRecorder()
	server.routes().ServeHTTP(legacyManual, httptest.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/review", bytes.NewBufferString(`{"action":"accept"}`)))
	if legacyManual.Code != http.StatusConflict {
		t.Fatalf("legacy manual review status=%d body=%s", legacyManual.Code, legacyManual.Body.String())
	}
}

func TestMergeTaskBranchToMainAndRecordsMerge(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-b", "main"}, {"config", "user.email", "test@example.com"}, {"config", "user.name", "Test User"}} {
		command := exec.Command("git", args...)
		command.Dir = repo
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("baseline\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("git", "add", "README.md")
	command.Dir = repo
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, output)
	}
	command = exec.Command("git", "commit", "-m", "baseline")
	command.Dir = repo
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, output)
	}
	branch := "自动编排-20260812-12345678"
	for _, args := range [][]string{{"checkout", "-b", branch}} {
		command := exec.Command("git", args...)
		command.Dir = repo
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "implemented.txt"), []byte("done\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "implemented.txt"}, {"commit", "-m", "task implementation"}, {"checkout", "main"}} {
		command := exec.Command("git", args...)
		command.Dir = repo
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "main-only.txt"), []byte("main change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "main-only.txt"}, {"commit", "-m", "main change"}} {
		command := exec.Command("git", args...)
		command.Dir = repo
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	readCommit := exec.Command("git", "rev-parse", branch)
	readCommit.Dir = repo
	commitOutput, err := readCommit.CombinedOutput()
	if err != nil {
		t.Fatalf("read task branch commit: %v: %s", err, commitOutput)
	}
	commit := strings.TrimSpace(string(commitOutput))
	if _, err := server.db.Exec(`update projects set path=?,git_branch='main' where id=?`, repo, projectID); err != nil {
		t.Fatal(err)
	}
	taskID := createTaskForTest(t, server.routes(), projectID, "Confirm merged branch")
	now := time.Now().UTC()
	if _, err := server.db.Exec(`update tasks set status='awaiting_review' where id=?`, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into task_orchestration_jobs (id,project_id,task_id,queue_position,status,policy_snapshot,created_at,updated_at) values ('job',?,?,1,'awaiting_main','{}',?,?)`, projectID, taskID, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into git_task_records (job_id,base_dev_sha,task_branch,worktree_path,integration_sha,created_at,updated_at) values ('job',?,'自动编排-20260812-12345678','',?, ?,?)`, commit, commit, now, now); err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/orchestration/merge-main", bytes.NewBufferString(`{}`)))
	if response.Code != http.StatusNoContent {
		t.Fatalf("merge task branch: %d body=%s", response.Code, response.Body.String())
	}
	readMain := exec.Command("git", "rev-parse", "main")
	readMain.Dir = repo
	mainOutput, err := readMain.CombinedOutput()
	if err != nil {
		t.Fatalf("read main commit: %v: %s", err, mainOutput)
	}
	mergedCommit := strings.TrimSpace(string(mainOutput))
	if mergedCommit == commit {
		t.Fatal("expected a merge commit for diverged main and task branches")
	}
	containsTaskCommit := exec.Command("git", "merge-base", "--is-ancestor", commit, "main")
	containsTaskCommit.Dir = repo
	if output, err := containsTaskCommit.CombinedOutput(); err != nil {
		t.Fatalf("main does not contain task commit: %v: %s", err, output)
	}
	var jobStatus string
	if err := server.db.QueryRow(`select status from task_orchestration_jobs where id='job'`).Scan(&jobStatus); err != nil {
		t.Fatal(err)
	}
	if jobStatus != "released_to_main" {
		t.Fatalf("job status=%q", jobStatus)
	}
	assertTaskStatus(t, server, taskID, taskDone)

	if _, err := server.db.Exec(`update tasks set status='awaiting_review',completed_at=null where id=?`, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`update task_orchestration_jobs set status='integrated_to_dev' where id='job'`); err != nil {
		t.Fatal(err)
	}
	legacy := httptest.NewRecorder()
	server.routes().ServeHTTP(legacy, httptest.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/orchestration/merge-main", bytes.NewBufferString(`{}`)))
	if legacy.Code != http.StatusNoContent {
		t.Fatalf("merge already integrated branch: %d body=%s", legacy.Code, legacy.Body.String())
	}
	assertTaskStatus(t, server, taskID, taskDone)
}

func TestMergeTaskBranchToMainAbortsConflictAndPreservesState(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "shared.txt", "baseline\n")
	runGitForTest(t, repo, "add", "shared.txt")
	runGitForTest(t, repo, "commit", "-m", "baseline")
	branch := "automatic-conflict-task"
	runGitForTest(t, repo, "checkout", "-b", branch)
	writeGitTestFile(t, repo, "shared.txt", "task change\n")
	runGitForTest(t, repo, "commit", "-am", "task change")
	commitOutput, err := exec.Command("git", "-C", repo, "rev-parse", branch).Output()
	if err != nil {
		t.Fatalf("read task commit: %v", err)
	}
	commit := strings.TrimSpace(string(commitOutput))
	runGitForTest(t, repo, "checkout", "main")
	writeGitTestFile(t, repo, "shared.txt", "main change\n")
	runGitForTest(t, repo, "commit", "-am", "main change")
	mainBeforeOutput, err := exec.Command("git", "-C", repo, "rev-parse", "main").Output()
	if err != nil {
		t.Fatalf("read main commit: %v", err)
	}
	mainBefore := strings.TrimSpace(string(mainBeforeOutput))
	if _, err := server.db.Exec(`update projects set path=?,git_branch='main' where id=?`, repo, projectID); err != nil {
		t.Fatal(err)
	}
	taskID := createTaskForTest(t, server.routes(), projectID, "Conflicting main merge")
	now := time.Now().UTC()
	if _, err := server.db.Exec(`update tasks set status='awaiting_review' where id=?`, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into task_orchestration_jobs (id,project_id,task_id,queue_position,status,policy_snapshot,created_at,updated_at) values ('conflict-job',?,?,1,'awaiting_main','{}',?,?)`, projectID, taskID, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into git_task_records (job_id,base_dev_sha,task_branch,worktree_path,integration_sha,created_at,updated_at) values ('conflict-job',?,'automatic-conflict-task','',?, ?,?)`, mainBefore, commit, now, now); err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/orchestration/merge-main", bytes.NewBufferString(`{}`)))
	if response.Code != http.StatusConflict {
		t.Fatalf("conflicting merge status=%d body=%s", response.Code, response.Body.String())
	}
	mainAfterOutput, err := exec.Command("git", "-C", repo, "rev-parse", "main").Output()
	if err != nil || strings.TrimSpace(string(mainAfterOutput)) != mainBefore {
		t.Fatalf("main changed after conflict: output=%q err=%v", mainAfterOutput, err)
	}
	statusOutput, err := exec.Command("git", "-C", repo, "status", "--porcelain").Output()
	if err != nil || strings.TrimSpace(string(statusOutput)) != "" {
		t.Fatalf("repository not clean after conflict: output=%q err=%v", statusOutput, err)
	}
	var jobStatus string
	if err := server.db.QueryRow(`select status from task_orchestration_jobs where id='conflict-job'`).Scan(&jobStatus); err != nil {
		t.Fatal(err)
	}
	if jobStatus != "awaiting_main" {
		t.Fatalf("job status=%q want awaiting_main", jobStatus)
	}
	assertTaskStatus(t, server, taskID, taskAwaitingReview)
}

func TestOrchestrationRejectsStaleLeaseStateWrites(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	taskID := createTaskForTest(t, server.routes(), projectID, "Fence stale worker")
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into project_orchestration_configs (project_id,enabled,main_branch,dev_branch,verification_commands,max_fix_rounds,frozen_reason,updated_at) values (?,0,'main','dev','[]',3,'',?)`, projectID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.Exec(`insert into task_orchestration_jobs (id,project_id,task_id,queue_position,status,lease_token,policy_snapshot,created_at,updated_at) values ('job',?,?,1,'implementing',9,'{}',?,?)`, projectID, taskID, now, now); err != nil {
		t.Fatal(err)
	}
	stale := OrchestrationJob{ID: "job", ProjectID: projectID, TaskID: taskID, LeaseToken: 8}
	if err := server.advanceOrchestrationJob(context.Background(), stale, orchestrationImplementing, orchestrationChecking); err == nil {
		t.Fatal("stale lease unexpectedly advanced the job")
	}
	server.failOrchestrationJob(context.Background(), stale, errors.New("stale failure"))
	var status, frozen string
	if err := server.db.QueryRow(`select status from task_orchestration_jobs where id='job'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := server.db.QueryRow(`select frozen_reason from project_orchestration_configs where project_id=?`, projectID).Scan(&frozen); err != nil {
		t.Fatal(err)
	}
	if status != orchestrationImplementing || frozen != "" {
		t.Fatalf("stale lease wrote state: status=%q frozen=%q", status, frozen)
	}
}

func TestOrchestrationLeaseAssertionRenewsCurrentLease(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	taskID := createTaskForTest(t, server.routes(), projectID, "Renew orchestration lease")
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into task_orchestration_jobs (id,project_id,task_id,queue_position,status,lease_token,policy_snapshot,created_at,updated_at) values ('job',?,?,1,'preparing',9,'{}',?,?)`, projectID, taskID, now, now); err != nil {
		t.Fatal(err)
	}
	expires := now.Add(time.Minute)
	if _, err := server.db.Exec(`insert into project_orchestration_leases (project_id,token,owner_id,expires_at,updated_at) values (?,?,?,?,?)`, projectID, 9, server.orchestrationOwner, expires, now); err != nil {
		t.Fatal(err)
	}
	if err := server.assertOrchestrationLease(context.Background(), OrchestrationJob{ID: "job", ProjectID: projectID, LeaseToken: 9}); err != nil {
		t.Fatalf("assert current lease: %v", err)
	}
	var renewed time.Time
	if err := server.db.QueryRow(`select expires_at from project_orchestration_leases where project_id=?`, projectID).Scan(&renewed); err != nil {
		t.Fatal(err)
	}
	if !renewed.After(now) {
		t.Fatalf("lease was not renewed: original=%s renewed=%s", expires, renewed)
	}
}

func TestReleaseOrchestrationLeasesClearsOnlyCurrentOwner(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into project_orchestration_leases (project_id,token,owner_id,expires_at,updated_at) values (?,?,?,?,?)`, projectID, 9, server.orchestrationOwner, now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	server.releaseOrchestrationLeases()
	var owner string
	var expires sql.NullTime
	if err := server.db.QueryRow(`select owner_id,expires_at from project_orchestration_leases where project_id=?`, projectID).Scan(&owner, &expires); err != nil {
		t.Fatal(err)
	}
	if owner != "" || expires.Valid {
		t.Fatalf("lease was not released: owner=%q expires=%v", owner, expires)
	}
}

func TestReleaseSnapshotBranchIsScopedToProject(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	for _, projectID := range []string{"project-a", "project-b"} {
		if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values (?,?,?,?,?,?,?)`, projectID, projectID, t.TempDir(), "wsl-local", "main", true, now); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []struct{ id, projectID string }{{"release-a", "project-a"}, {"release-b", "project-b"}} {
		if _, err := server.db.Exec(`insert into release_snapshots (id,project_id,dev_sha,branch,status,created_at) values (?,?,?,'release/same','awaiting_main',?)`, item.id, item.projectID, "same", now); err != nil {
			t.Fatalf("insert release snapshot for %s: %v", item.projectID, err)
		}
	}
}

func TestReleaseSnapshotMigrationScopesLegacyBranchConstraint(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	for _, projectID := range []string{"project-a", "project-b"} {
		if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values (?,?,?,?,?,?,?)`, projectID, projectID, t.TempDir(), "wsl-local", "main", true, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := server.db.Exec(`drop table release_snapshots;
create table release_snapshots (
	id text primary key,
	project_id text not null references projects(id) on delete cascade,
	dev_sha text not null,
	branch text not null unique,
	status text not null,
	created_at datetime not null,
	confirmed_at datetime
);`); err != nil {
		t.Fatalf("create legacy release schema: %v", err)
	}
	if err := server.migrateReleaseSnapshotBranchScope(context.Background()); err != nil {
		t.Fatalf("migrate legacy release schema: %v", err)
	}
	for _, item := range []struct{ id, projectID string }{{"release-a", "project-a"}, {"release-b", "project-b"}} {
		if _, err := server.db.Exec(`insert into release_snapshots (id,project_id,dev_sha,branch,status,created_at) values (?,?,?,'release/same','awaiting_main',?)`, item.id, item.projectID, "same", now); err != nil {
			t.Fatalf("migrated release schema rejected %s: %v", item.projectID, err)
		}
	}
}

func mustGitHead(t *testing.T, repo string) string {
	t.Helper()
	command := exec.Command("git", "rev-parse", "HEAD")
	command.Dir = repo
	output, err := command.Output()
	if err != nil {
		t.Fatalf("read git head: %v", err)
	}
	return strings.TrimSpace(string(output))
}

func TestListTasksLoadsWithSingleSQLiteConnection(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	createTaskForTest(t, server.routes(), projectID, "Load task board")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/tasks", nil).WithContext(ctx))
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("list tasks did not complete")
	}
	if response.Code != http.StatusOK {
		t.Fatalf("list tasks status: got=%d want=%d body=%s", response.Code, http.StatusOK, response.Body.String())
	}
}

func TestTaskDispatchCreatesSnapshotAndWaitsForReviewAfterRunCompletion(t *testing.T) {
	server, projectID, conversationID := seedTaskConversation(t)
	started := make(chan struct{})
	release := make(chan struct{})
	server.runner = runnerFunc(func(_ context.Context, _ AgentRunRequest, _ AgentRunSink) error {
		close(started)
		<-release
		return nil
	})
	taskID := createTaskForTest(t, server.routes(), projectID, "Implement task board")

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/dispatch", bytes.NewBufferString(`{}`)))
	if response.Code != http.StatusAccepted {
		t.Fatalf("dispatch status: %d body=%s", response.Code, response.Body.String())
	}
	var dispatched struct {
		RunID   string  `json:"runId"`
		TaskRun TaskRun `json:"taskRun"`
	}
	if err := json.NewDecoder(response.Body).Decode(&dispatched); err != nil {
		t.Fatalf("decode dispatch response: %v", err)
	}
	if dispatched.RunID == "" || dispatched.TaskRun.RunID != dispatched.RunID || dispatched.TaskRun.TaskID != taskID {
		t.Fatalf("unexpected dispatch response: %+v", dispatched)
	}
	if !strings.Contains(dispatched.TaskRun.PromptSnapshot, "Implement task board") || !strings.Contains(dispatched.TaskRun.PromptSnapshot, "Tests cover Implement task board.") {
		t.Fatalf("task run snapshot does not preserve task content: %q", dispatched.TaskRun.PromptSnapshot)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("task runner did not start")
	}
	assertTaskStatus(t, server, taskID, taskRunning)
	close(release)
	waitForConversationIdle(t, server, conversationID)
	assertTaskStatus(t, server, taskID, taskAwaitingReview)
	var taskRuns, messages, runs int
	if err := server.db.QueryRow(`select count(*) from task_runs where task_id=?`, taskID).Scan(&taskRuns); err != nil {
		t.Fatalf("count task runs: %v", err)
	}
	if err := server.db.QueryRow(`select count(*) from messages where conversation_id=?`, conversationID).Scan(&messages); err != nil {
		t.Fatalf("count task messages: %v", err)
	}
	if err := server.db.QueryRow(`select count(*) from runs where conversation_id=?`, conversationID).Scan(&runs); err != nil {
		t.Fatalf("count task runs: %v", err)
	}
	if taskRuns != 1 || messages != 1 || runs != 1 {
		t.Fatalf("dispatch did not create exactly one execution chain: taskRuns=%d messages=%d runs=%d", taskRuns, messages, runs)
	}
}

func TestTaskChangeRequestAndReopenFollowStateRules(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	taskID := createTaskForTest(t, server.routes(), projectID, "Review task behavior")
	if _, err := server.db.Exec(`update tasks set status=? where id=?`, taskAwaitingReview, taskID); err != nil {
		t.Fatalf("prepare task for changes: %v", err)
	}
	changes := httptest.NewRecorder()
	server.routes().ServeHTTP(changes, httptest.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/review", bytes.NewBufferString(`{"action":"request_changes","note":"Add coverage for interruption."}`)))
	if changes.Code != http.StatusOK {
		t.Fatalf("request changes status: %d body=%s", changes.Code, changes.Body.String())
	}
	assertTaskStatus(t, server, taskID, taskActionRequired)

	cancel := httptest.NewRecorder()
	server.routes().ServeHTTP(cancel, httptest.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/cancel", nil))
	if cancel.Code != http.StatusNotFound {
		t.Fatalf("cancel task route status: %d body=%s", cancel.Code, cancel.Body.String())
	}
	if _, err := server.db.Exec(`update tasks set status=? where id=?`, taskDone, taskID); err != nil {
		t.Fatalf("prepare completed task for reopen: %v", err)
	}

	reopen := httptest.NewRecorder()
	server.routes().ServeHTTP(reopen, httptest.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/reopen", nil))
	if reopen.Code != http.StatusOK {
		t.Fatalf("reopen task status: %d body=%s", reopen.Code, reopen.Body.String())
	}
	assertTaskStatus(t, server, taskID, taskTodo)

	invalid := httptest.NewRecorder()
	server.routes().ServeHTTP(invalid, httptest.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/reopen", nil))
	if invalid.Code != http.StatusConflict {
		t.Fatalf("reopen todo task status: %d body=%s", invalid.Code, invalid.Body.String())
	}
}

func TestAcceptingTaskDoesNotCreateStatusNotification(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	taskID := createTaskForTest(t, server.routes(), projectID, "Accepted task has no notification")
	if _, err := server.db.Exec(`update tasks set status=? where id=?`, taskAwaitingReview, taskID); err != nil {
		t.Fatalf("prepare task for acceptance: %v", err)
	}

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/review", bytes.NewBufferString(`{"action":"accept"}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("accept task status: %d body=%s", response.Code, response.Body.String())
	}
	assertTaskStatus(t, server, taskID, taskDone)

	var count int
	if err := server.db.QueryRow(`select count(*) from notifications where task_id=? and type='task.done'`, taskID).Scan(&count); err != nil {
		t.Fatalf("count accepted-task notifications: %v", err)
	}
	if count != 0 {
		t.Fatalf("accepted-task notification count = %d, want 0", count)
	}
}

func TestReviewAllTasksAcceptsAwaitingReviewTasks(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	first := createTaskForTest(t, server.routes(), projectID, "batch accept A")
	second := createTaskForTest(t, server.routes(), projectID, "batch accept B")
	done := createTaskForTest(t, server.routes(), projectID, "already done")
	for _, taskID := range []string{first, second} {
		if _, err := server.db.Exec(`update tasks set status=? where id=?`, taskAwaitingReview, taskID); err != nil {
			t.Fatalf("mark task awaiting review: %v", err)
		}
	}
	if _, err := server.db.Exec(`update tasks set status=? where id=?`, taskDone, done); err != nil {
		t.Fatalf("mark task done: %v", err)
	}

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/tasks/review-all", bytes.NewBufferString(`{}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("review-all status: %d body=%s", response.Code, response.Body.String())
	}
	var result struct {
		Accepted int `json:"accepted"`
		Skipped  int `json:"skipped"`
		Total    int `json:"total"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode review-all result: %v", err)
	}
	if result.Accepted != 2 || result.Skipped != 0 || result.Total != 2 {
		t.Fatalf("review-all result: accepted=%d skipped=%d total=%d want 2/0/2", result.Accepted, result.Skipped, result.Total)
	}
	assertTaskStatus(t, server, first, taskDone)
	assertTaskStatus(t, server, second, taskDone)
	// 已经 done 的任务不应被重新改动。
	assertTaskStatus(t, server, done, taskDone)

	// 仅批量验收的两项任务被标记 completed_at；基线任务虽为 done 但无 completed_at。
	var completedCount int
	if err := server.db.QueryRow(`select count(*) from tasks where status=? and completed_at is not null`, taskDone).Scan(&completedCount); err != nil {
		t.Fatalf("count completed tasks: %v", err)
	}
	if completedCount != 2 {
		t.Fatalf("completed task count = %d, want 2", completedCount)
	}

	var eventCount int
	if err := server.db.QueryRow(`select count(*) from task_events where type=?`, "task.accepted").Scan(&eventCount); err != nil {
		t.Fatalf("count accepted events: %v", err)
	}
	if eventCount != 2 {
		t.Fatalf("accepted event count = %d, want 2", eventCount)
	}

	var notificationCount int
	if err := server.db.QueryRow(`select count(*) from notifications where type='task.done'`).Scan(&notificationCount); err != nil {
		t.Fatalf("count done notifications: %v", err)
	}
	if notificationCount != 0 {
		t.Fatalf("done notification count = %d, want 0 (acceptance does not notify)", notificationCount)
	}
}

func TestReviewAllTasksSkipsTasksUnderActiveOrchestration(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	enableOrchestrationForTest(t, server, projectID)
	reviewable := createTaskForTest(t, server.routes(), projectID, "batch accept skipped check A")
	verifying := createTaskForTest(t, server.routes(), projectID, "batch accept skipped check B")
	for _, taskID := range []string{reviewable, verifying} {
		if _, err := server.db.Exec(`update tasks set status=? where id=?`, taskAwaitingReview, taskID); err != nil {
			t.Fatalf("mark task awaiting review: %v", err)
		}
	}
	now := time.Now().UTC()
	// 模拟一个仍在独立审查（checking）中的自动编排任务。
	if _, err := server.db.Exec(`insert into task_orchestration_jobs (id,project_id,task_id,queue_position,status,lease_token,policy_snapshot,created_at,updated_at) values ('job-verifying',?,?,1,'checking',7,'{}',?,?)`, projectID, verifying, now, now); err != nil {
		t.Fatalf("insert verifying orchestration job: %v", err)
	}

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/tasks/review-all", bytes.NewBufferString(`{}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("review-all status: %d body=%s", response.Code, response.Body.String())
	}
	var result struct {
		Accepted int `json:"accepted"`
		Skipped  int `json:"skipped"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode review-all result: %v", err)
	}
	if result.Accepted != 1 || result.Skipped != 1 {
		t.Fatalf("review-all result: accepted=%d skipped=%d want 1/1", result.Accepted, result.Skipped)
	}
	assertTaskStatus(t, server, reviewable, taskDone)
	assertTaskStatus(t, server, verifying, taskAwaitingReview)
}

func TestReviewAllTasksWithNoAwaitingReviewTasks(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	createTaskForTest(t, server.routes(), projectID, "no pending accept A")
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/tasks/review-all", bytes.NewBufferString(`{}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("review-all with none status: %d body=%s", response.Code, response.Body.String())
	}
	var result struct {
		Accepted int `json:"accepted"`
		Skipped  int `json:"skipped"`
		Total    int `json:"total"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode review-all result: %v", err)
	}
	if result.Accepted != 0 || result.Skipped != 0 || result.Total != 0 {
		t.Fatalf("review-all result: accepted=%d skipped=%d total=%d want 0/0/0", result.Accepted, result.Skipped, result.Total)
	}
}

func TestTaskAwaitingReviewCannotBeDispatchedUntilChangesAreRequested(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	taskID := createTaskForTest(t, server.routes(), projectID, "Review before redispatch")
	if _, err := server.db.Exec(`update tasks set status=? where id=?`, taskAwaitingReview, taskID); err != nil {
		t.Fatalf("prepare task for review: %v", err)
	}

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/dispatch", bytes.NewBufferString(`{}`)))
	if response.Code != http.StatusConflict {
		t.Fatalf("dispatch awaiting-review task status: %d body=%s", response.Code, response.Body.String())
	}
}

func TestTaskStopDelegatesToActiveRun(t *testing.T) {
	server, projectID, conversationID := seedTaskConversation(t)
	taskID := createTaskForTest(t, server.routes(), projectID, "Stop active task")
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values ('task-run-id',?,'running',?)`, conversationID, now); err != nil {
		t.Fatalf("insert active run: %v", err)
	}
	if _, err := server.db.Exec(`insert into task_runs (id,task_id,conversation_id,run_id,sequence,status,prompt_snapshot,acceptance_snapshot,failure_reason,created_at) values ('task-run',?,?, 'task-run-id',1,'running','prompt','criteria','',?)`, taskID, conversationID, now); err != nil {
		t.Fatalf("insert active task run: %v", err)
	}
	if _, err := server.db.Exec(`update tasks set status=? where id=?`, taskRunning, taskID); err != nil {
		t.Fatalf("mark task running: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	server.mu.Lock()
	server.cancels["task-run-id"] = cancel
	server.runContexts["task-run-id"] = conversationID
	server.mu.Unlock()

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/stop", nil))
	if response.Code != http.StatusAccepted {
		t.Fatalf("task stop status: %d body=%s", response.Code, response.Body.String())
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("task stop did not cancel the existing run context")
	}
}

func TestTaskStopRejectsActiveRunWhenConversationHasQueuedTask(t *testing.T) {
	server, projectID, conversationID := seedTaskConversation(t)
	activeTask := createTaskForTest(t, server.routes(), projectID, "Active task")
	queuedTask := createTaskForTest(t, server.routes(), projectID, "Queued task")
	now := time.Now().UTC()
	for _, run := range []struct{ id, taskID, status string }{{"active-run", activeTask, "running"}, {"queued-run", queuedTask, "queued"}} {
		if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values (?,?,?,?)`, run.id, conversationID, run.status, now); err != nil {
			t.Fatalf("insert run: %v", err)
		}
		if _, err := server.db.Exec(`insert into task_runs (id,task_id,conversation_id,run_id,sequence,status,prompt_snapshot,acceptance_snapshot,failure_reason,created_at) values (?,?,?,?,1,?,'prompt','criteria','',?)`, run.id+"-task", run.taskID, conversationID, run.id, run.status, now); err != nil {
			t.Fatalf("insert task run: %v", err)
		}
	}
	if _, err := server.db.Exec(`update tasks set status=? where id in (?,?)`, taskRunning, activeTask, queuedTask); err != nil {
		t.Fatalf("mark tasks active: %v", err)
	}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/tasks/"+activeTask+"/stop", nil))
	if response.Code != http.StatusConflict {
		t.Fatalf("stop active task with queue status: got=%d want=%d body=%s", response.Code, http.StatusConflict, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "当前会话还有排队的任务") {
		t.Fatalf("stop was rejected for the wrong reason: %s", response.Body.String())
	}
}

func TestRunStopRejectsStreamingSessionWithQueuedTask(t *testing.T) {
	server, projectID, conversationID := seedTaskConversation(t)
	activeTask := createTaskForTest(t, server.routes(), projectID, "Active task")
	queuedTask := createTaskForTest(t, server.routes(), projectID, "Queued task")
	now := time.Now().UTC()
	for _, run := range []struct{ id, taskID, status string }{{"active-run", activeTask, "running"}, {"queued-run", queuedTask, "queued"}} {
		if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values (?,?,?,?)`, run.id, conversationID, run.status, now); err != nil {
			t.Fatalf("insert run: %v", err)
		}
		if _, err := server.db.Exec(`insert into task_runs (id,task_id,conversation_id,run_id,sequence,status,prompt_snapshot,acceptance_snapshot,failure_reason,created_at) values (?,?,?,?,1,?,'prompt','criteria','',?)`, run.id+"-task", run.taskID, conversationID, run.id, run.status, now); err != nil {
			t.Fatalf("insert task run: %v", err)
		}
	}
	session := newQueuedAgentSession()
	server.mu.Lock()
	server.sessions[conversationID] = &activeAgentSession{agent: session, activeRunID: "active-run"}
	server.mu.Unlock()
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/runs/active-run/stop", nil))
	if response.Code != http.StatusConflict {
		t.Fatalf("stop run with queued task: got=%d want=%d body=%s", response.Code, http.StatusConflict, response.Body.String())
	}
	var errorPayload struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(response.Body).Decode(&errorPayload); err != nil {
		t.Fatalf("decode stop conflict: %v", err)
	}
	if errorPayload.Code != "active_runs_present" {
		t.Fatalf("stop conflict code=%q", errorPayload.Code)
	}
	select {
	case <-session.done:
		t.Fatal("queued session was stopped")
	default:
	}
}

func TestConversationReportsQueuedRunAsActive(t *testing.T) {
	server, _, conversationID := seedTaskConversation(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`update conversations set status='running' where id=?`, conversationID); err != nil {
		t.Fatalf("mark conversation running: %v", err)
	}
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values ('queued-run',?,'queued',?)`, conversationID, now); err != nil {
		t.Fatalf("insert queued run: %v", err)
	}

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/conversations/"+conversationID+"?limit=1", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("get conversation status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		ActiveRunID *string `json:"activeRunId"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatalf("decode conversation response: %v", err)
	}
	if payload.ActiveRunID == nil || *payload.ActiveRunID != "queued-run" {
		t.Fatalf("active run ID=%v, want queued-run", payload.ActiveRunID)
	}
}

func TestRunForceStopCancelsAllActiveConversationRuns(t *testing.T) {
	server, projectID, conversationID := seedTaskConversation(t)
	activeTask := createTaskForTest(t, server.routes(), projectID, "Active task")
	queuedTask := createTaskForTest(t, server.routes(), projectID, "Queued task")
	now := time.Now().UTC()
	for _, run := range []struct{ id, taskID, status string }{{"active-run", activeTask, "running"}, {"queued-run", queuedTask, "queued"}} {
		if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values (?,?,?,?)`, run.id, conversationID, run.status, now); err != nil {
			t.Fatalf("insert run: %v", err)
		}
		if _, err := server.db.Exec(`insert into task_runs (id,task_id,conversation_id,run_id,sequence,status,prompt_snapshot,acceptance_snapshot,failure_reason,created_at) values (?,?,?,?,1,?,'prompt','criteria','',?)`, run.id+"-task", run.taskID, conversationID, run.id, run.status, now); err != nil {
			t.Fatalf("insert task run: %v", err)
		}
	}
	if _, err := server.db.Exec(`update tasks set status=? where id in (?,?)`, taskRunning, activeTask, queuedTask); err != nil {
		t.Fatalf("mark tasks active: %v", err)
	}
	cancelled := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-ctx.Done()
		close(cancelled)
	}()
	session := newQueuedAgentSession()
	server.mu.Lock()
	server.cancels["active-run"] = cancel
	server.runContexts["active-run"] = conversationID
	server.runContexts["queued-run"] = conversationID
	server.streamingSetups["queued-run"] = &streamingSetup{}
	managed := &activeAgentSession{agent: session, activeRunID: "active-run", runIDs: map[string]struct{}{"active-run": {}, "queued-run": {}}}
	server.sessions[conversationID] = managed
	server.mu.Unlock()
	go server.watchStreamingSession(conversationID, managed)

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/runs/active-run/stop?force=true", nil))
	if response.Code != http.StatusAccepted {
		t.Fatalf("force stop status=%d body=%s", response.Code, response.Body.String())
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("force stop did not cancel the active run context")
	}
	select {
	case <-session.Done():
	case <-time.After(time.Second):
		t.Fatal("force stop did not stop the shared streaming session")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var remaining int
		if err := server.db.QueryRow(`select count(*) from runs where id in ('active-run','queued-run') and status in ('queued','running')`).Scan(&remaining); err != nil {
			t.Fatalf("count active runs: %v", err)
		}
		if remaining == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, runID := range []string{"active-run", "queued-run"} {
		var status string
		if err := server.db.QueryRow(`select status from runs where id=?`, runID).Scan(&status); err != nil {
			t.Fatalf("load %s status: %v", runID, err)
		}
		if status != "stopped" {
			t.Fatalf("%s status=%q, want stopped", runID, status)
		}
	}
	for _, taskID := range []string{activeTask, queuedTask} {
		assertTaskStatus(t, server, taskID, taskActionRequired)
	}
	var conversationStatus string
	if err := server.db.QueryRow(`select status from conversations where id=?`, conversationID).Scan(&conversationStatus); err != nil {
		t.Fatalf("load conversation status: %v", err)
	}
	if conversationStatus != "idle" {
		t.Fatalf("conversation status=%q, want idle", conversationStatus)
	}
	server.mu.Lock()
	_, activeCancel := server.cancels["active-run"]
	_, activeContext := server.runContexts["active-run"]
	_, queuedContext := server.runContexts["queued-run"]
	_, queuedSetup := server.streamingSetups["queued-run"]
	server.mu.Unlock()
	if activeCancel || activeContext || queuedContext || queuedSetup {
		t.Fatalf("force-stopped runs retained in-memory state: cancel=%t activeContext=%t queuedContext=%t queuedSetup=%t", activeCancel, activeContext, queuedContext, queuedSetup)
	}
}

func TestStreamingSessionUnexpectedExitFinalizesOutstandingRuns(t *testing.T) {
	server, _, conversationID := seedTaskConversation(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`update conversations set status='running' where id=?`, conversationID); err != nil {
		t.Fatalf("mark conversation running: %v", err)
	}
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values ('abandoned-run',?,'running',?)`, conversationID, now); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	session := newQueuedAgentSession()
	managed := &activeAgentSession{agent: session, activeRunID: "abandoned-run", runIDs: map[string]struct{}{"abandoned-run": {}}}
	server.mu.Lock()
	server.runContexts["abandoned-run"] = conversationID
	server.streamingSetups["abandoned-run"] = &streamingSetup{}
	server.sessions[conversationID] = managed
	server.mu.Unlock()
	go server.watchStreamingSession(conversationID, managed)

	// The process exited without emitting TurnFinished for its active turn.
	session.Stop()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var status string
		if err := server.db.QueryRow(`select status from runs where id='abandoned-run'`).Scan(&status); err != nil {
			t.Fatalf("load run: %v", err)
		}
		if status == "failed" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	var status, conversationStatus string
	if err := server.db.QueryRow(`select status from runs where id='abandoned-run'`).Scan(&status); err != nil {
		t.Fatalf("load final run: %v", err)
	}
	if status != "failed" {
		t.Fatalf("run status=%q, want failed", status)
	}
	if err := server.db.QueryRow(`select status from conversations where id=?`, conversationID).Scan(&conversationStatus); err != nil {
		t.Fatalf("load conversation: %v", err)
	}
	if conversationStatus != "idle" {
		t.Fatalf("conversation status=%q, want idle", conversationStatus)
	}
	server.mu.Lock()
	_, contextRetained := server.runContexts["abandoned-run"]
	_, setupRetained := server.streamingSetups["abandoned-run"]
	server.mu.Unlock()
	if contextRetained || setupRetained {
		t.Fatalf("unexpectedly exited run retained in-memory state: context=%t setup=%t", contextRetained, setupRetained)
	}
}

func TestRunForceStopKeepsWorkspaceUntilStreamingSessionExits(t *testing.T) {
	server, projectID, conversationID := seedTaskConversation(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`update conversations set status='running' where id=?`, conversationID); err != nil {
		t.Fatalf("mark conversation running: %v", err)
	}
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values ('active-run',?,'running',?)`, conversationID, now); err != nil {
		t.Fatalf("insert active run: %v", err)
	}
	releaseWorkspace, acquired := server.acquireProjectWorkspace(projectID, "conversation:"+conversationID)
	if !acquired {
		t.Fatal("acquire streaming workspace")
	}
	server.registerRunWorkspace("active-run", releaseWorkspace)
	session := newDelayedDoneAgentSession()
	managed := &activeAgentSession{agent: session, activeRunID: "active-run", runIDs: map[string]struct{}{"active-run": {}}}
	server.mu.Lock()
	server.sessions[conversationID] = managed
	server.mu.Unlock()
	go server.watchStreamingSession(conversationID, managed)

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/runs/active-run/stop?force=true", nil))
	if response.Code != http.StatusAccepted {
		t.Fatalf("force stop status=%d body=%s", response.Code, response.Body.String())
	}
	select {
	case <-session.stopped:
	case <-time.After(time.Second):
		t.Fatal("force stop did not request session shutdown")
	}
	var runStatus, conversationStatus string
	if err := server.db.QueryRow(`select status from runs where id='active-run'`).Scan(&runStatus); err != nil {
		t.Fatalf("load active run: %v", err)
	}
	if err := server.db.QueryRow(`select status from conversations where id=?`, conversationID).Scan(&conversationStatus); err != nil {
		t.Fatalf("load conversation: %v", err)
	}
	if runStatus != "running" || conversationStatus != "running" {
		t.Fatalf("session exited too early: run=%q conversation=%q", runStatus, conversationStatus)
	}
	if _, acquired := server.acquireProjectWorkspace(projectID, "run:replacement"); acquired {
		t.Fatal("workspace was released before the stopped session exited")
	}

	close(session.release)
	waitForConversationIdle(t, server, conversationID)
	if err := server.db.QueryRow(`select status from runs where id='active-run'`).Scan(&runStatus); err != nil {
		t.Fatalf("load stopped run: %v", err)
	}
	if runStatus != "stopped" {
		t.Fatalf("run status=%q, want stopped", runStatus)
	}
	releaseReplacement, acquired := server.acquireProjectWorkspace(projectID, "run:replacement")
	if !acquired {
		t.Fatal("workspace remained held after the stopped session exited")
	}
	releaseReplacement()
}

func TestTaskDetailIncludesLastRunAndFailureReason(t *testing.T) {
	server, projectID, conversationID := seedTaskConversation(t)
	taskID := createTaskForTest(t, server.routes(), projectID, "Failed task")
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values ('failed-run',?,'running',?)`, conversationID, now); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if _, err := server.db.Exec(`insert into task_runs (id,task_id,conversation_id,run_id,sequence,status,prompt_snapshot,acceptance_snapshot,failure_reason,created_at) values ('failed-task-run',?,?, 'failed-run',1,'running','prompt','criteria','',?)`, taskID, conversationID, now); err != nil {
		t.Fatalf("insert task run: %v", err)
	}
	if _, err := server.db.Exec(`update tasks set status=?,last_task_run_id=? where id=?`, taskRunning, "failed-task-run", taskID); err != nil {
		t.Fatalf("mark task running: %v", err)
	}
	server.finishRun("failed-run", conversationID, "failed", errors.New("permission denied"))
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/tasks/"+taskID, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("get task detail status: %d body=%s", response.Code, response.Body.String())
	}
	var detail TaskDetail
	if err := json.NewDecoder(response.Body).Decode(&detail); err != nil {
		t.Fatalf("decode task detail: %v", err)
	}
	if detail.LastRun == nil || detail.LastRun.ID != "failed-task-run" {
		t.Fatalf("last run missing from task detail: %+v", detail.LastRun)
	}
	if len(detail.Runs) != 1 || detail.Runs[0].FailureReason != "任务执行失败，请查看任务日志后重试。：permission denied" {
		t.Fatalf("failure reason was not preserved: %+v", detail.Runs)
	}
}

func TestTaskStatusNotificationLinksToTaskConversation(t *testing.T) {
	server, projectID, conversationID := seedTaskConversation(t)
	taskID := createTaskForTest(t, server.routes(), projectID, "Notification target")
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into task_runs (id,task_id,conversation_id,sequence,status,prompt_snapshot,acceptance_snapshot,failure_reason,created_at) values ('notification-task-run',?,?,1,'completed','prompt','criteria','',?)`, taskID, conversationID, now); err != nil {
		t.Fatalf("insert task run: %v", err)
	}

	server.notifyTaskStatusChange(context.Background(), taskID, taskDone)

	var gotConversationID, actionURL string
	if err := server.db.QueryRow(`select conversation_id,action_url from notifications where task_id=?`, taskID).Scan(&gotConversationID, &actionURL); err != nil {
		t.Fatalf("load notification: %v", err)
	}
	if gotConversationID != conversationID {
		t.Fatalf("notification conversation ID = %q, want %q", gotConversationID, conversationID)
	}
	wantURL := "/projects/" + projectID + "/conversations/" + conversationID
	if actionURL != wantURL {
		t.Fatalf("notification URL = %q, want %q", actionURL, wantURL)
	}
}

func TestTaskStatusNotificationUsesDescriptionForUntitledTaskAndDeduplicates(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/tasks", bytes.NewBufferString(`{"title":"","description":"修复通知中的空任务名称","acceptanceCriteria":"","priority":"normal"}`)))
	if response.Code != http.StatusCreated {
		t.Fatalf("create untitled task: %d body=%s", response.Code, response.Body.String())
	}
	var task Task
	if err := json.NewDecoder(response.Body).Decode(&task); err != nil {
		t.Fatalf("decode task: %v", err)
	}

	server.notifyTaskStatusChange(context.Background(), task.ID, taskDone)
	server.notifyTaskStatusChange(context.Background(), task.ID, taskDone)

	var count int
	var body string
	if err := server.db.QueryRow(`select count(*), max(body) from notifications where task_id=? and type='task.done'`, task.ID).Scan(&count, &body); err != nil {
		t.Fatalf("load notification: %v", err)
	}
	if count != 1 {
		t.Fatalf("notification count = %d, want 1", count)
	}
	if !strings.Contains(body, "修复通知中的空任务名称") || strings.Contains(body, "任务「」") {
		t.Fatalf("notification body = %q, want task description fallback", body)
	}
}

func TestOrchestrationNotificationsUseDescriptionForUntitledTask(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/tasks", bytes.NewBufferString(`{"title":"","description":"修复编排通知中的空任务名称","acceptanceCriteria":"","priority":"normal"}`)))
	if response.Code != http.StatusCreated {
		t.Fatalf("create untitled task: %d body=%s", response.Code, response.Body.String())
	}
	var task Task
	if err := json.NewDecoder(response.Body).Decode(&task); err != nil {
		t.Fatalf("decode task: %v", err)
	}
	job := OrchestrationJob{ProjectID: projectID, TaskID: task.ID}
	server.notifyOrchestrationNeedsHuman(context.Background(), job, errors.New("verification failed"))
	server.notifyOrchestrationRetry(context.Background(), job, errors.New("retrying"))
	server.notifyOrchestrationPaused(context.Background(), projectID, task.ID)
	server.notifyOrchestrationResumed(context.Background(), projectID, task.ID)

	rows, err := server.db.Query(`select body from notifications where task_id=? order by created_at,id`, task.ID)
	if err != nil {
		t.Fatalf("load orchestration notifications: %v", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			t.Fatalf("scan notification body: %v", err)
		}
		count++
		if !strings.Contains(body, "修复编排通知中的空任务名称") || strings.Contains(body, "任务「」") {
			t.Fatalf("notification body=%q, want task description fallback", body)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate orchestration notifications: %v", err)
	}
	if count != 4 {
		t.Fatalf("notification count=%d, want 4", count)
	}
}

func TestMigrateNotificationsAddsDedupeKeyToExistingTable(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`create table notifications (
		id text primary key,
		type text not null,
		project_id text not null,
		project_name text not null default '',
		conversation_id text not null default '',
		task_id text not null default '',
		title text not null,
		body text not null,
		priority text not null default 'normal',
		action_url text not null,
		dismissed integer not null default 0,
		created_at datetime not null
	)`); err != nil {
		t.Fatalf("create legacy notifications table: %v", err)
	}
	server := &Server{db: db}
	if err := server.migrateNotifications(context.Background()); err != nil {
		t.Fatalf("migrate notifications: %v", err)
	}
	if _, err := db.Exec(`insert into notifications (id,type,project_id,title,body,priority,action_url,created_at,dedupe_key) values ('first','task.done','project','title','body','normal','/','2026-01-01','same-event')`); err != nil {
		t.Fatalf("insert first deduplicated notification: %v", err)
	}
	if _, err := db.Exec(`insert into notifications (id,type,project_id,title,body,priority,action_url,created_at,dedupe_key) values ('second','task.done','project','title','body','normal','/','2026-01-01','same-event')`); err == nil {
		t.Fatal("duplicate dedupe key was accepted")
	}
}

func TestTaskUpdateRecordsAuditEvent(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	taskID := createTaskForTest(t, server.routes(), projectID, "Audited task")
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/api/tasks/"+taskID, bytes.NewBufferString(`{"title":"Renamed task","description":"Updated scope.","acceptanceCriteria":"Updated checks.","priority":"high","position":1}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("update task status: %d body=%s", response.Code, response.Body.String())
	}
	var events int
	if err := server.db.QueryRow(`select count(*) from task_events where task_id=? and type='task.updated'`, taskID).Scan(&events); err != nil {
		t.Fatalf("count update events: %v", err)
	}
	if events != 1 {
		t.Fatalf("expected one task.updated event, got %d", events)
	}
}

func TestStreamingTaskDispatchKeepsIndependentTaskRunsInOrder(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	session := newQueuedAgentSession()
	server.runner = queuedStreamingRunner{session: session}
	firstTask := createTaskForTest(t, server.routes(), projectID, "First queued task")
	secondTask := createTaskForTest(t, server.routes(), projectID, "Second queued task")
	for _, taskID := range []string{firstTask, secondTask} {
		response := httptest.NewRecorder()
		server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/dispatch", bytes.NewBufferString(`{}`)))
		if response.Code != http.StatusAccepted {
			t.Fatalf("dispatch %s status: %d body=%s", taskID, response.Code, response.Body.String())
		}
	}
	deadline := time.Now().Add(time.Second)
	for session.queuedCount() != 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if session.queuedCount() != 2 {
		t.Fatalf("expected two queued turns, got %d", session.queuedCount())
	}
	if !session.finishNext(nil) {
		t.Fatal("finish first queued task")
	}
	assertTaskStatus(t, server, firstTask, taskAwaitingReview)
	assertTaskStatus(t, server, secondTask, taskRunning)
	if !session.finishNext(nil) {
		t.Fatal("finish second queued task")
	}
	assertTaskStatus(t, server, secondTask, taskAwaitingReview)
	var distinctRuns int
	if err := server.db.QueryRow(`select count(distinct run_id) from task_runs where task_id in (?,?)`, firstTask, secondTask).Scan(&distinctRuns); err != nil {
		t.Fatalf("count task run identities: %v", err)
	}
	if distinctRuns != 2 {
		t.Fatalf("expected independent task run IDs, got %d", distinctRuns)
	}
}

func TestTaskStopRejectsQueuedStreamingTask(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	session := newQueuedAgentSession()
	server.runner = queuedStreamingRunner{session: session}
	firstTask := createTaskForTest(t, server.routes(), projectID, "First queued task")
	queuedTask := createTaskForTest(t, server.routes(), projectID, "Queued task to preserve")
	for _, taskID := range []string{firstTask, queuedTask} {
		response := httptest.NewRecorder()
		server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/dispatch", bytes.NewBufferString(`{}`)))
		if response.Code != http.StatusAccepted {
			t.Fatalf("dispatch %s status: %d body=%s", taskID, response.Code, response.Body.String())
		}
	}
	deadline := time.Now().Add(time.Second)
	for session.queuedCount() != 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/tasks/"+queuedTask+"/stop", nil))
	if response.Code != http.StatusConflict {
		t.Fatalf("stopping a queued task status: got=%d want=%d body=%s", response.Code, http.StatusConflict, response.Body.String())
	}
	if session.queuedCount() != 2 {
		t.Fatalf("stopping queued task must not affect session queue, got %d turns", session.queuedCount())
	}
}

func createTaskForTest(t *testing.T, handler http.Handler, projectID, title string) string {
	t.Helper()
	response := httptest.NewRecorder()
	body := `{"title":"` + title + `","description":"Implement ` + title + ` safely.","acceptanceCriteria":"Tests cover ` + title + `.","priority":"normal"}`
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/tasks", bytes.NewBufferString(body)))
	if response.Code != http.StatusCreated {
		t.Fatalf("create task %q status: %d body=%s", title, response.Code, response.Body.String())
	}
	var task struct{ ID string }
	if err := json.NewDecoder(response.Body).Decode(&task); err != nil {
		t.Fatalf("decode created task: %v", err)
	}
	if task.ID == "" {
		t.Fatalf("created task %q has no ID", title)
	}
	return task.ID
}

func seedTaskConversation(t *testing.T) (*Server, string, string) {
	t.Helper()
	server := newTestServer(t)
	now := time.Now().UTC()
	projectID, conversationID := "project", "conversation"
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values (?,?,?,?,?,?,?)`, projectID, "project", t.TempDir(), "wsl-local", "main", true, now); err != nil {
		t.Fatalf("insert task project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,claude_initialized,is_current,created_at) values (?,?,?,?,?,?,?)`, conversationID, projectID, "00000000-0000-4000-8000-000000000000", "idle", false, true, now); err != nil {
		t.Fatalf("insert task conversation: %v", err)
	}
	return server, projectID, conversationID
}

func enableOrchestrationForTest(t *testing.T, server *Server, projectID string) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into project_orchestration_configs (project_id,enabled,main_branch,dev_branch,verification_commands,max_fix_rounds,frozen_reason,updated_at) values (?,1,'main','dev','["true"]',3,'',?) on conflict(project_id) do update set enabled=1,verification_commands='["true"]',frozen_reason=''`, projectID, now); err != nil {
		t.Fatalf("enable orchestration: %v", err)
	}
}

func queuePositionForTest(t *testing.T, server *Server, taskID string) int {
	t.Helper()
	var position int
	if err := server.db.QueryRow(`select queue_position from task_orchestration_jobs where task_id=?`, taskID).Scan(&position); err != nil {
		t.Fatalf("load queue position for %s: %v", taskID, err)
	}
	return position
}

func TestEnqueueBatchAddsTasksInOrder(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	enableOrchestrationForTest(t, server, projectID)
	taskA := createTaskForTest(t, server.routes(), projectID, "batch A")
	taskB := createTaskForTest(t, server.routes(), projectID, "batch B")
	taskC := createTaskForTest(t, server.routes(), projectID, "batch C")
	body := fmt.Sprintf(`{"taskIds":["%s","%s","%s"]}`, taskA, taskB, taskC)
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/orchestration/enqueue-batch", bytes.NewBufferString(body)))
	if response.Code != http.StatusAccepted {
		t.Fatalf("enqueue-batch: %d body=%s", response.Code, response.Body.String())
	}
	if got := queuePositionForTest(t, server, taskA); got != 1 {
		t.Fatalf("taskA position=%d want 1", got)
	}
	if got := queuePositionForTest(t, server, taskB); got != 2 {
		t.Fatalf("taskB position=%d want 2", got)
	}
	if got := queuePositionForTest(t, server, taskC); got != 3 {
		t.Fatalf("taskC position=%d want 3", got)
	}
}

func TestEnqueueBatchRejectsDuplicates(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	enableOrchestrationForTest(t, server, projectID)
	taskA := createTaskForTest(t, server.routes(), projectID, "dup A")
	body := fmt.Sprintf(`{"taskIds":["%s","%s"]}`, taskA, taskA)
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/orchestration/enqueue-batch", bytes.NewBufferString(body)))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("duplicate enqueue-batch: %d want 400", response.Code)
	}
}

func TestEnqueueBatchRejectsIneligibleTaskAsConflict(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	enableOrchestrationForTest(t, server, projectID)
	taskA := createTaskForTest(t, server.routes(), projectID, "eligible")
	taskB := createTaskForTest(t, server.routes(), projectID, "ineligible")
	if _, err := server.db.Exec(`update tasks set status='running' where id=?`, taskB); err != nil {
		t.Fatalf("mark task running: %v", err)
	}
	body := fmt.Sprintf(`{"taskIds":["%s","%s"]}`, taskA, taskB)
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/orchestration/enqueue-batch", bytes.NewBufferString(body)))
	if response.Code != http.StatusConflict {
		t.Fatalf("ineligible enqueue-batch: %d want 409", response.Code)
	}
}

func TestEnqueueBatchRejectsUnknownTaskAsNotFound(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	enableOrchestrationForTest(t, server, projectID)
	body := `{"taskIds":["00000000-0000-0000-0000-000000000000"]}`
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/orchestration/enqueue-batch", bytes.NewBufferString(body)))
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown enqueue-batch: %d want 404", response.Code)
	}
}

func TestEnqueueBatchRejectsForeignTaskAsBadRequest(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	enableOrchestrationForTest(t, server, projectID)
	// Create a task under a different project.
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('other','other',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert other project: %v", err)
	}
	foreignTask := createTaskForTest(t, server.routes(), "other", "foreign")
	body := fmt.Sprintf(`{"taskIds":["%s"]}`, foreignTask)
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/orchestration/enqueue-batch", bytes.NewBufferString(body)))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("foreign enqueue-batch: %d want 400", response.Code)
	}
}

func TestDequeueRemovesQueuedTaskAndCompactsPositions(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	enableOrchestrationForTest(t, server, projectID)
	taskA := createTaskForTest(t, server.routes(), projectID, "dequeue A")
	taskB := createTaskForTest(t, server.routes(), projectID, "dequeue B")
	taskC := createTaskForTest(t, server.routes(), projectID, "dequeue C")
	now := time.Now().UTC()
	for i, id := range []string{taskA, taskB, taskC} {
		if _, err := server.db.Exec(`insert into task_orchestration_jobs (id,project_id,task_id,queue_position,status,policy_snapshot,created_at,updated_at) values (?,?,?,?,'queued','{}',?,?)`, fmt.Sprintf("job-%d", i), projectID, id, i+1, now, now); err != nil {
			t.Fatalf("insert job: %v", err)
		}
	}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/tasks/"+taskB+"/orchestration/dequeue", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("dequeue: %d body=%s", response.Code, response.Body.String())
	}
	var count int
	if err := server.db.QueryRow(`select count(*) from task_orchestration_jobs where task_id=?`, taskB).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("dequeued task still has a job")
	}
	if got := queuePositionForTest(t, server, taskA); got != 1 {
		t.Fatalf("taskA position=%d want 1 after compact", got)
	}
	if got := queuePositionForTest(t, server, taskC); got != 2 {
		t.Fatalf("taskC position=%d want 2 after compact", got)
	}
}

func TestDequeueRefusesRunningJob(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	enableOrchestrationForTest(t, server, projectID)
	taskA := createTaskForTest(t, server.routes(), projectID, "running dequeue")
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into task_orchestration_jobs (id,project_id,task_id,queue_position,status,policy_snapshot,created_at,updated_at) values ('job','project',?,1,'implementing','{}',?,?)`, taskA, now, now); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/tasks/"+taskA+"/orchestration/dequeue", nil))
	if response.Code != http.StatusConflict {
		t.Fatalf("dequeue implementing: %d want 409", response.Code)
	}
}

func TestDequeueTargetsActiveJobAmongStaleRecords(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	enableOrchestrationForTest(t, server, projectID)
	taskA := createTaskForTest(t, server.routes(), projectID, "finished task")
	now := time.Now().UTC()
	// A finished (integrated_to_dev) job: the task is not in the active queue,
	// so dequeue should report it as not queued rather than refusing.
	if _, err := server.db.Exec(`insert into task_orchestration_jobs (id,project_id,task_id,queue_position,status,policy_snapshot,created_at,updated_at) values ('stale','project',?,1,'integrated_to_dev','{}',?,?)`, taskA, now, now); err != nil {
		t.Fatalf("insert finished job: %v", err)
	}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/tasks/"+taskA+"/orchestration/dequeue", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("dequeue finished task: %d want 404 body=%s", response.Code, response.Body.String())
	}
}

func TestReorderReordersQueuedTasks(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	enableOrchestrationForTest(t, server, projectID)
	taskA := createTaskForTest(t, server.routes(), projectID, "reorder A")
	taskB := createTaskForTest(t, server.routes(), projectID, "reorder B")
	taskC := createTaskForTest(t, server.routes(), projectID, "reorder C")
	now := time.Now().UTC()
	for i, id := range []string{taskA, taskB, taskC} {
		if _, err := server.db.Exec(`insert into task_orchestration_jobs (id,project_id,task_id,queue_position,status,policy_snapshot,created_at,updated_at) values (?,?,?,?,'queued','{}',?,?)`, fmt.Sprintf("job-%d", i), projectID, id, i+1, now, now); err != nil {
			t.Fatalf("insert job: %v", err)
		}
	}
	body := fmt.Sprintf(`{"taskIds":["%s","%s","%s"]}`, taskC, taskA, taskB)
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/api/projects/"+projectID+"/orchestration/order", bytes.NewBufferString(body)))
	if response.Code != http.StatusNoContent {
		t.Fatalf("reorder: %d body=%s", response.Code, response.Body.String())
	}
	if got := queuePositionForTest(t, server, taskC); got != 1 {
		t.Fatalf("taskC position=%d want 1", got)
	}
	if got := queuePositionForTest(t, server, taskA); got != 2 {
		t.Fatalf("taskA position=%d want 2", got)
	}
	if got := queuePositionForTest(t, server, taskB); got != 3 {
		t.Fatalf("taskB position=%d want 3", got)
	}
}

func TestReorderRejectsPartialList(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	enableOrchestrationForTest(t, server, projectID)
	taskA := createTaskForTest(t, server.routes(), projectID, "partial A")
	taskB := createTaskForTest(t, server.routes(), projectID, "partial B")
	now := time.Now().UTC()
	for i, id := range []string{taskA, taskB} {
		if _, err := server.db.Exec(`insert into task_orchestration_jobs (id,project_id,task_id,queue_position,status,policy_snapshot,created_at,updated_at) values (?,?,?,?,'queued','{}',?,?)`, fmt.Sprintf("job-%d", i), projectID, id, i+1, now, now); err != nil {
			t.Fatalf("insert job: %v", err)
		}
	}
	body := fmt.Sprintf(`{"taskIds":["%s"]}`, taskA)
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/api/projects/"+projectID+"/orchestration/order", bytes.NewBufferString(body)))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("partial reorder: %d want 400", response.Code)
	}
}

func TestOrchestrationDispatchLinksIntentAndRunInOneTransaction(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	server.runner = runnerFunc(func(context.Context, AgentRunRequest, AgentRunSink) error { return nil })
	taskID := createTaskForTest(t, server.routes(), projectID, "intent linkage")
	var projectPath string
	if err := server.db.QueryRow(`select path from projects where id=?`, projectID).Scan(&projectPath); err != nil {
		t.Fatalf("load project path: %v", err)
	}
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into task_orchestration_jobs (id,project_id,task_id,queue_position,status,attempt,lease_token,policy_snapshot,created_at,updated_at) values ('job',?,?,1,'preparing',1,1,'{}',?,?)`, projectID, taskID, now, now); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	if _, err := server.db.Exec(`insert into task_execution_intents (id,job_id,phase,attempt,status,created_at,updated_at) values ('intent','job','implementation',1,'pending',?,?)`, now, now); err != nil {
		t.Fatalf("insert intent: %v", err)
	}

	result, status, err := server.dispatchTaskByIDInWorkspaceWithExecutionIntent(context.Background(), taskID, projectPath, "", "intent")
	if err != nil || status != http.StatusAccepted {
		t.Fatalf("dispatch result=%#v status=%d err=%v", result, status, err)
	}
	var runID, intentStatus string
	if err := server.db.QueryRow(`select run_id,status from task_execution_intents where id='intent'`).Scan(&runID, &intentStatus); err != nil {
		t.Fatalf("load intent: %v", err)
	}
	if runID != result.RunID || intentStatus != "started" {
		t.Fatalf("intent linkage: run=%q status=%q result=%q", runID, intentStatus, result.RunID)
	}
}

func assertTaskStatus(t *testing.T, server *Server, taskID, want string) {
	t.Helper()
	var status string
	if err := server.db.QueryRow(`select status from tasks where id=?`, taskID).Scan(&status); err != nil {
		t.Fatalf("read task status: %v", err)
	}
	if status != want {
		t.Fatalf("task status: got=%q want=%q", status, want)
	}
}

func containsArgument(args []string, value string) bool {
	return strings.Contains("\x00"+strings.Join(args, "\x00")+"\x00", "\x00"+value+"\x00")
}

func containsArguments(args []string, values ...string) bool {
	for index := 0; index+len(values) <= len(args); index++ {
		matched := true
		for offset, value := range values {
			if args[index+offset] != value {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func waitForConversationIdle(t *testing.T, server *Server, conversationID string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var status string
		if err := server.db.QueryRow(`select status from conversations where id=?`, conversationID).Scan(&status); err != nil {
			t.Fatalf("read conversation status: %v", err)
		}
		if status == "idle" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("conversation did not return to idle")
}

func waitForRunStatus(t *testing.T, server *Server, conversationID, want string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var status string
		err := server.db.QueryRow(`select status from runs where conversation_id=? order by created_at desc limit 1`, conversationID).Scan(&status)
		if err != nil {
			t.Fatalf("read run status: %v", err)
		}
		if status == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("run did not reach status %q", want)
}

func TestGitSnapshotParsesNULPathsAndMissingUpstream(t *testing.T) {
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "已修改\n文件.txt", "one\n")

	snapshot, err := newGitRunner().Snapshot(context.Background(), repo)
	if err != nil {
		t.Fatalf("read Git snapshot: %v", err)
	}
	if snapshot.Worktree.Untracked != 1 {
		t.Fatalf("untracked count: got=%d want=1", snapshot.Worktree.Untracked)
	}
	if snapshot.Head.Upstream != "" || snapshot.Head.Ahead != 0 || snapshot.Head.Behind != 0 {
		t.Fatalf("unexpected upstream summary: %#v", snapshot.Head)
	}
}

func TestGitDiffDoesNotRunTextconv(t *testing.T) {
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, ".gitattributes", "asset.bin diff=sentinel\n")
	writeGitTestFile(t, repo, "asset.bin", "before\n")
	runGitForTest(t, repo, "add", ".gitattributes", "asset.bin")
	runGitForTest(t, repo, "commit", "-m", "add binary asset")
	runGitForTest(t, repo, "config", "diff.sentinel.textconv", "sh -c 'touch textconv-ran; cat'")
	writeGitTestFile(t, repo, "asset.bin", "after\n")

	if _, err := newGitRunner().Diff(context.Background(), repo, "asset.bin", gitDiffWorktree); err != nil {
		t.Fatalf("read Git diff: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "textconv-ran")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("textconv was executed while reading diff")
	}
}

func TestGitLogAndBranchesUseMachineReadableFields(t *testing.T) {
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "readme.txt", "one\n")
	runGitForTest(t, repo, "add", "readme.txt")
	runGitForTest(t, repo, "commit", "-m", "initial commit")
	writeGitTestFile(t, repo, "readme.txt", "two\n")
	runGitForTest(t, repo, "commit", "-am", "second commit")

	runner := newGitRunner()
	commits, err := runner.Log(context.Background(), repo, "main", 10)
	if err != nil {
		t.Fatalf("read Git log: %v", err)
	}
	if len(commits) != 2 || commits[0].Subject != "second commit" || commits[0].OID == "" {
		t.Fatalf("unexpected commits: %#v", commits)
	}
	branches, err := runner.Branches(context.Background(), repo)
	if err != nil {
		t.Fatalf("read Git branches: %v", err)
	}
	if len(branches) != 1 || branches[0].Name != "main" || !branches[0].Current || branches[0].Remote {
		t.Fatalf("unexpected branches: %#v", branches)
	}
}

func TestGitChangesParseRenameDestinationAndSource(t *testing.T) {
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "old-name.txt", "content\n")
	runGitForTest(t, repo, "add", "old-name.txt")
	runGitForTest(t, repo, "commit", "-m", "add old name")
	runGitForTest(t, repo, "mv", "old-name.txt", "new-name.txt")

	changes, err := newGitRunner().Changes(context.Background(), repo)
	if err != nil {
		t.Fatalf("read renamed Git change: %v", err)
	}
	if len(changes) != 1 || !changes[0].Renamed || changes[0].Path != "new-name.txt" || changes[0].OriginalPath != "old-name.txt" {
		t.Fatalf("unexpected rename change: %#v", changes)
	}
}

func TestGitLogRejectsRevisionExpressions(t *testing.T) {
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "readme.txt", "one\n")
	runGitForTest(t, repo, "add", "readme.txt")
	runGitForTest(t, repo, "commit", "-m", "initial commit")
	writeGitTestFile(t, repo, "readme.txt", "two\n")
	runGitForTest(t, repo, "commit", "-am", "second commit")

	if _, err := newGitRunner().Log(context.Background(), repo, "HEAD^", 10); err == nil {
		t.Fatal("revision expression was accepted")
	}
}

func TestGitLogRejectsTagNamesThatWereNotEnumeratedAsBranches(t *testing.T) {
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "readme.txt", "one\n")
	runGitForTest(t, repo, "add", "readme.txt")
	runGitForTest(t, repo, "commit", "-m", "initial commit")
	runGitForTest(t, repo, "tag", "v1")

	if _, err := newGitRunner().Log(context.Background(), repo, "v1", 10); err == nil {
		t.Fatal("tag name was accepted as a client-selected log ref")
	}
}

func TestGitBranchUsesConstrainedRunnerEnvironment(t *testing.T) {
	repo := newTempGitRepository(t)
	otherRepo := newTempGitRepository(t)
	runGitForTest(t, otherRepo, "checkout", "-b", "other")
	t.Setenv("GIT_DIR", filepath.Join(otherRepo, ".git"))

	branch, ready := gitBranch(context.Background(), repo)
	if !ready || branch != "main" {
		t.Fatalf("Git branch used inherited repository override: branch=%q ready=%t", branch, ready)
	}
}

func TestWaitForGitCommandHasFinalBound(t *testing.T) {
	done := make(chan error)
	if waitForGitCommand(nil, done, time.Millisecond, time.Millisecond) {
		t.Fatal("wait unexpectedly completed")
	}
}

func TestGitCommandEnvironmentDisablesInheritedAskpassAndDangerousProtocols(t *testing.T) {
	env := gitCommandEnvironment([]string{"PATH=/usr/bin", "GIT_ASKPASS=/tmp/unsafe", "GIT_TERMINAL_PROMPT=1", "GIT_CONFIG_COUNT=1", "GIT_DIR=/tmp/other-repository", "GIT_WORK_TREE=/tmp/other-worktree", "GIT_INDEX_FILE=/tmp/other-index", "GIT_COMMON_DIR=/tmp/other-common", "GIT_OBJECT_DIRECTORY=/tmp/other-objects", "GIT_ALTERNATE_OBJECT_DIRECTORIES=/tmp/other-alternates"})
	values := map[string]string{}
	for _, entry := range env {
		parts := strings.SplitN(entry, "=", 2)
		values[parts[0]] = parts[1]
	}
	if values["GIT_ASKPASS"] != "" || values["GIT_TERMINAL_PROMPT"] != "0" || values["GIT_CONFIG_COUNT"] != "2" || values["GIT_CONFIG_KEY_0"] != "protocol.ext.allow" || values["GIT_CONFIG_VALUE_0"] != "never" || values["GIT_CONFIG_KEY_1"] != "protocol.file.allow" || values["GIT_CONFIG_VALUE_1"] != "never" {
		t.Fatalf("unsafe Git environment: %#v", values)
	}
	for _, key := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES"} {
		if _, found := values[key]; found {
			t.Fatalf("Git repository override %s was inherited: %#v", key, values)
		}
	}
}

func TestGitSnapshotIgnoresStderrWhenParsingPorcelain(t *testing.T) {
	git := writeFakeGit(t, "printf '# branch.oid (initial)\\000# branch.head main\\000? actual.txt\\000'\nprintf '? stderr-injection.txt\\000' >&2\n")
	t.Setenv("PATH", filepath.Dir(git)+string(os.PathListSeparator)+os.Getenv("PATH"))

	snapshot, err := newGitRunner().Snapshot(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("read Git snapshot: %v", err)
	}
	if snapshot.Worktree.Untracked != 1 {
		t.Fatalf("stderr must not be parsed as porcelain: %#v", snapshot)
	}
}

func TestGitValidationRejectsSpecialRefAndPathspec(t *testing.T) {
	if err := validateGitRef("@"); err == nil {
		t.Fatal("Git HEAD shorthand was accepted")
	}
	if err := validateGitPath(":(top)"); err == nil {
		t.Fatal("Git pathspec magic was accepted")
	}
}

func TestGitStageTreatsShortMagicFileNamesLiterally(t *testing.T) {
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "baseline.txt", "baseline\n")
	runGitForTest(t, repo, "add", "baseline.txt")
	runGitForTest(t, repo, "commit", "-m", "baseline")
	writeGitTestFile(t, repo, "ordinary.txt", "ordinary\n")
	writeGitTestFile(t, repo, ":!excluded.txt", "special\n")
	runGitForTest(t, repo, "add", "ordinary.txt")

	if err := newGitRunner().Stage(context.Background(), repo, []string{":!excluded.txt"}); err != nil {
		t.Fatalf("stage literal short-magic filename: %v", err)
	}
	staged, err := newGitRunner().runGit(context.Background(), repo, "diff", "--cached", "--name-only")
	if err != nil {
		t.Fatalf("read staged paths: %v", err)
	}
	if string(staged) != ":!excluded.txt\nordinary.txt\n" {
		t.Fatalf("staged paths=%q, want both literal selected and ordinary filenames", staged)
	}
	if err := newGitRunner().Unstage(context.Background(), repo, []string{":!excluded.txt"}); err != nil {
		t.Fatalf("unstage literal short-magic filename: %v", err)
	}
	staged, err = newGitRunner().runGit(context.Background(), repo, "diff", "--cached", "--name-only")
	if err != nil {
		t.Fatalf("read staged paths after unstage: %v", err)
	}
	if string(staged) != "ordinary.txt\n" {
		t.Fatalf("staged paths after unstage=%q, want the ordinary filename to remain staged", staged)
	}
}

func TestGitCommandTimeoutTerminatesItsProcessGroup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child-survived")
	git := writeFakeGit(t, "(sleep 0.2; touch \"$MARKER\") &\nsleep 5\n")
	t.Setenv("PATH", filepath.Dir(git)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MARKER", marker)

	_, err := (&gitCLIRunner{timeout: 40 * time.Millisecond, backend: &localGitBackend{timeout: 40 * time.Millisecond}}).runGit(context.Background(), t.TempDir(), "status")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Git child process survived cancellation: %v", err)
	}
}

func TestGitCommandStopsWhenCombinedOutputExceedsLimit(t *testing.T) {
	git := writeFakeGit(t, "head -c 1049600 /dev/zero\n")
	t.Setenv("PATH", filepath.Dir(git)+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := newGitRunner().runGit(context.Background(), t.TempDir(), "status")
	if !errors.Is(err, errGitOutputTooLarge) {
		t.Fatalf("output limit error: %v", err)
	}
}

func TestProjectWorkspaceLeaseExcludesOtherOwners(t *testing.T) {
	server := newTestServer(t)
	release, acquired := server.acquireProjectWorkspace("project", "run:active")
	if !acquired {
		t.Fatal("acquire project workspace")
	}
	defer release()
	if _, acquired := server.acquireProjectWorkspace("project", "git:fetch"); acquired {
		t.Fatal("another owner acquired the workspace lease")
	}
	sameOwnerRelease, acquired := server.acquireProjectWorkspace("project", "run:active")
	if !acquired {
		t.Fatal("the current owner could not share the workspace lease")
	}
	release()
	if _, acquired := server.acquireProjectWorkspace("project", "git:fetch"); acquired {
		t.Fatal("workspace lease was released before all current-owner holders exited")
	}
	sameOwnerRelease()
	otherRelease, acquired := server.acquireProjectWorkspace("project", "git:fetch")
	if !acquired {
		t.Fatal("workspace lease did not release after the final holder exited")
	}
	otherRelease()
}

func TestLocalizedErrorTextUsesChineseMessages(t *testing.T) {
	workspace := localizedHTTPErrorText(http.StatusConflict, errors.New("project workspace is occupied by another run or Git operation"))
	if workspace != "项目工作区正被其他 AI 任务或 Git 操作占用，请等待当前操作完成后重试。" {
		t.Fatalf("workspace error = %q", workspace)
	}

	dirtyWorktree := errorText(errors.New("project worktree is not clean"))
	if dirtyWorktree != "项目工作区存在未提交或未跟踪的更改。请提交、暂存或清理这些更改后重试任务编排。" {
		t.Fatalf("dirty worktree error = %q", dirtyWorktree)
	}

	missingShell := errorText(errors.New(`main baseline verification failed: ls: exec: "sh": executable file not found in %PATH%`))
	if missingShell != "自动编排无法执行验证命令：系统找不到 sh。请升级服务端到支持 Windows 命令解释器的版本后，恢复队列并重试。" {
		t.Fatalf("missing shell error = %q", missingShell)
	}

	unknown := localizedHTTPErrorText(http.StatusInternalServerError, errors.New("database connection refused"))
	if unknown != "服务内部错误，请稍后重试。：database connection refused" {
		t.Fatalf("unknown error = %q", unknown)
	}

	known := localizedHTTPErrorText(http.StatusBadRequest, errors.New("invalid JSON request"))
	if known != "请求内容不是有效的 JSON。" {
		t.Fatalf("known error = %q", known)
	}

	mixed := localizedHTTPErrorText(http.StatusBadRequest, errors.New("无法读取目录：permission denied"))
	if mixed != "请求参数无效，请检查后重试。：无法读取目录：permission denied" {
		t.Fatalf("mixed error = %q", mixed)
	}
	if code := httpErrorCode(errors.New("cannot stop a run while this conversation has other queued or running runs")); code != "active_runs_present" {
		t.Fatalf("stop conflict code = %q", code)
	}
}

func TestUpdateFailureSurfacesRealReason(t *testing.T) {
	// 更新失败必须把 CLI 的实际输出带给用户，而不是只给一个"请查看日志"的通用提示。
	// 带有明确中文"失败"标签的消息应原样展示，而不是被通用 fallback 前缀掩盖。
	real := localizedErrorText(errors.New("远程执行 claude update 失败：Process exited with status 1（[130] npm ERR! network timeout）"), "任务执行失败，请查看任务日志后重试。")
	if !strings.Contains(real, "npm ERR! network timeout") || strings.Contains(real, "查看任务日志") {
		t.Fatalf("update reason not surfaced: %q", real)
	}

	// tailUpdatOutput 应截取最后几行，避免超长输出刷屏。
	lines := []string{}
	for i := 1; i <= 30; i++ {
		lines = append(lines, "line")
	}
	tail := tailUpdateOutput(strings.Join(lines, "\n"))
	if strings.Count(tail, "line") != 8 {
		t.Fatalf("tailUpdateOutput kept %d lines, want 8: %q", strings.Count(tail, "line"), tail)
	}

	// ANSI 转义码（颜色、进度）和 \r 必须被剥离，否则会污染用户可见的报错。
	ansi := tailUpdateOutput("\x1b[31mnpm ERR!\x1b[0m network timeout\r\n")
	if !strings.Contains(ansi, "npm ERR! network timeout") || strings.Contains(ansi, "\x1b") || strings.Contains(ansi, "\r") {
		t.Fatalf("tailUpdateOutput did not strip ANSI/CR: %q", ansi)
	}

	// OSC-8 超链接（\x1b]8;;url\x1b\）的终止反斜杠不得残留。BEL 结尾同样干净。
	osc := tailUpdateOutput("\x1b]8;;https://x\x1b\\npm ERR!\x1b]8;;\x1b\\")
	if strings.Contains(osc, "\\") || !strings.Contains(osc, "npm ERR!") {
		t.Fatalf("OSC/ST backslash leaked or text lost: %q", osc)
	}

	// 凭据必须被脱敏：sk-…、Authorization: Bearer …、CODEX_HOME 路径不得原样带到 UI。
	redacted := tailUpdateOutput("auth: sk-0123456789abcdef\nAuthorization: Bearer abc123\nCODEX_HOME=/root/.codex")
	if strings.Contains(redacted, "sk-0123456789abcdef") || strings.Contains(redacted, "abc123") || strings.Contains(redacted, "/root/.codex") {
		t.Fatalf("credential not redacted: %q", redacted)
	}

	// 无输出的失败（如超时）不应留下悬空的 "（）"。
	if detail := updateOutputDetail("  \x1b[0m  \r  "); detail != "" {
		t.Fatalf("empty output should produce no detail, got %q", detail)
	}
	if detail := updateOutputDetail("npm ERR! timeout"); detail != "（npm ERR! timeout）" {
		t.Fatalf("non-empty output detail = %q", detail)
	}

	// 单行巨型输出也必须被字节封顶，避免撑爆错误消息。
	huge := strings.Repeat("x", 100_000)
	bigDetail := tailUpdateOutput("ok " + huge)
	if len(bigDetail) > maxUpdateOutputDetailBytes {
		t.Fatalf("tailUpdateOutput detail too large: %d bytes", len(bigDetail))
	}
	if len(tailUpdateOutput(huge+" end")) < 3 {
		t.Fatalf("byte cap should keep the tail, got?")
	}
}

func TestLocalizedErrorSuppressesInternalUpdateWraps(t *testing.T) {
	// 即使被包装成含"失败"的错误，涉及 Go 内部堆栈/实现的细节也不得外泄。
	wrapped := localizedErrorText(errors.New("update Codex 失败：panic: runtime error（goroutine 1 [running]）"), "任务执行失败，请查看任务日志后重试。")
	if strings.Contains(wrapped, "panic") || strings.Contains(wrapped, "goroutine") {
		t.Fatalf("internal detail leaked: %q", wrapped)
	}
	// 纯中文错误仍原样展示。
	if direct := localizedErrorText(errors.New("更新服务超时，请稍后再试。"), "fallback"); direct != "更新服务超时，请稍后再试。" {
		t.Fatalf("pure Chinese error not shown directly: %q", direct)
	}
}

// TestLocalizedErrorTextSilencesPipeClosed 验证：进程被停止/强杀后，stdout/stderr
// 管道被关闭会让 bufio.Scanner 拿到 os.ErrClosed（"file already closed"）。这是正常
// 停止，不是流错误——localizedErrorText 必须返回空字符串，让读循环据此跳过
// stream.error 上报，避免在对话历史里留下"流错误 / file already closed"。
func TestLocalizedErrorTextSilencesPipeClosed(t *testing.T) {
	// 直接的 os.ErrClosed。
	if text := localizedErrorText(os.ErrClosed, "fallback"); text != "" {
		t.Fatalf("os.ErrClosed should be silenced, got %q", text)
	}
	// 被 fmt.Errorf("%w") 包装后仍能识别（readOutputLoop 的 "read SSH stdout: %w" 形态）。
	wrapped := fmt.Errorf("read SSH stdout: %w", os.ErrClosed)
	if text := localizedErrorText(wrapped, "fallback"); text != "" {
		t.Fatalf("wrapped os.ErrClosed should be silenced, got %q", text)
	}
	// 仅凭错误文本 "file already closed" 也要识别（bufio.Scanner 可能丢失 unwrap 链）。
	if text := localizedErrorText(errors.New("read |0: file already closed"), "fallback"); text != "" {
		t.Fatalf("file already closed text should be silenced, got %q", text)
	}
	// errorText（读循环实际调用的出口）同样返回空。
	if text := errorText(os.ErrClosed); text != "" {
		t.Fatalf("errorText(os.ErrClosed) should be empty, got %q", text)
	}
}

// TestLocalizedHTTPErrorTextFallsBackOnPipeClosed 验证 HTTP 错误响应不会因为
// localizedErrorText 返回空而输出空 error——应回退到可读的中文 fallback。
func TestLocalizedHTTPErrorTextFallsBackOnPipeClosed(t *testing.T) {
	text := localizedHTTPErrorText(http.StatusInternalServerError, os.ErrClosed)
	if text == "" || strings.Contains(text, "file already closed") {
		t.Fatalf("HTTP error should fall back to readable text, got %q", text)
	}
}

// TestFinishStreamingRunTreatsPipeClosedAsStopped 验证：SSH 路径停止时
// readOutputLoop 会把 os.ErrClosed 作为 runErr 上报。即使 server 级 stopping
// 标志因时序未置位，finishStreamingRun 也必须把 os.ErrClosed 归为 stopped
// 而非 failed，避免停止被记成失败。
func TestFinishStreamingRunTreatsPipeClosedAsStopped(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,claude_initialized,is_current,created_at) values ('conversation','project','00000000-0000-4000-8000-000000000000','running',0,1,?)`, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values ('run','conversation','running',?)`, now); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	// 不注册 session，模拟 stopping 标志因时序未置位的边界。runErr 为 os.ErrClosed
	//（SSH readOutputLoop 的 "read SSH stdout: %w" 形态）。
	server.finishStreamingRun("run", "conversation", fmt.Errorf("read SSH stdout: %w", os.ErrClosed))

	var status string
	if err := server.db.QueryRow(`select status from runs where id='run'`).Scan(&status); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	if status != "stopped" {
		t.Fatalf("os.ErrClosed should be recorded as stopped, got %q", status)
	}
}

func TestRecoverGitPushWithUnknownResultNeedsAttention(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('push-project','Push project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert push project: %v", err)
	}
	if _, err := server.db.Exec(`insert into git_operations (id,project_id,type,status,request_summary,before_state,after_state,requested_at,started_at) values ('push-operation','push-project','push','running','origin/main','{}','{}',?,?)`, now, now); err != nil {
		t.Fatalf("insert running push: %v", err)
	}

	if err := server.recoverGitOperations(context.Background()); err != nil {
		t.Fatalf("recover Git operations: %v", err)
	}
	var status string
	if err := server.db.QueryRow(`select status from git_operations where id='push-operation'`).Scan(&status); err != nil {
		t.Fatalf("read recovered push: %v", err)
	}
	if status != gitOperationNeedsAttention {
		t.Fatalf("recovered push status: got=%q want=%q", status, gitOperationNeedsAttention)
	}
}

func TestRecoverGitCommitWithUnknownResultNeedsAttention(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('commit-project','Commit project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert commit project: %v", err)
	}
	if _, err := server.db.Exec(`insert into git_operations (id,project_id,type,status,request_summary,before_state,after_state,requested_at,started_at) values ('commit-operation','commit-project','commit','running','commit','{}','{}',?,?)`, now, now); err != nil {
		t.Fatalf("insert running commit: %v", err)
	}

	if err := server.recoverGitOperations(context.Background()); err != nil {
		t.Fatalf("recover Git operations: %v", err)
	}
	var status string
	if err := server.db.QueryRow(`select status from git_operations where id='commit-operation'`).Scan(&status); err != nil {
		t.Fatalf("read recovered commit: %v", err)
	}
	if status != gitOperationNeedsAttention {
		t.Fatalf("recovered commit status: got=%q want=%q", status, gitOperationNeedsAttention)
	}
}

func TestRecoverGitCommitWithVerifiedHeadSucceeds(t *testing.T) {
	server := newTestServer(t)
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "readme.txt", "one\n")
	runGitForTest(t, repo, "add", "readme.txt")
	runGitForTest(t, repo, "commit", "-m", "initial commit")
	head, err := newGitRunner().Snapshot(context.Background(), repo)
	if err != nil {
		t.Fatalf("read commit head: %v", err)
	}
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('verified-commit-project','Verified commit project',?,'wsl-local','main',1,?)`, repo, now); err != nil {
		t.Fatalf("insert Git project: %v", err)
	}
	if _, err := server.db.Exec(`insert into git_operations (id,project_id,type,status,request_summary,before_state,after_state,requested_at,started_at) values ('verified-commit-operation','verified-commit-project','commit','running','commit','{}',?, ?,?)`, `{"headOid":"`+head.Head.OID+`"}`, now, now); err != nil {
		t.Fatalf("insert running commit: %v", err)
	}

	if err := server.recoverGitOperations(context.Background()); err != nil {
		t.Fatalf("recover Git operations: %v", err)
	}
	var status string
	if err := server.db.QueryRow(`select status from git_operations where id='verified-commit-operation'`).Scan(&status); err != nil {
		t.Fatalf("read recovered commit: %v", err)
	}
	if status != gitOperationSucceeded {
		t.Fatalf("verified commit recovery status: got=%q want=%q", status, gitOperationSucceeded)
	}
}

func TestRecoverGitFetchWithVerifiedRemoteRefsSucceeds(t *testing.T) {
	server := newTestServer(t)
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "readme.txt", "one\n")
	runGitForTest(t, repo, "add", "readme.txt")
	runGitForTest(t, repo, "commit", "-m", "initial commit")
	head, err := newGitRunner().Snapshot(context.Background(), repo)
	if err != nil {
		t.Fatalf("read commit head: %v", err)
	}
	runGitForTest(t, repo, "update-ref", "refs/remotes/origin/main", head.Head.OID)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('verified-fetch-project','Verified fetch project',?,'wsl-local','main',1,?)`, repo, now); err != nil {
		t.Fatalf("insert Git project: %v", err)
	}
	if _, err := server.db.Exec(`insert into git_operations (id,project_id,type,status,request_summary,before_state,after_state,requested_at,started_at) values ('verified-fetch-operation','verified-fetch-project','fetch','running','origin','{}',?, ?,?)`, `{"remoteRefs":{"refs/remotes/origin/main":"`+head.Head.OID+`"}}`, now, now); err != nil {
		t.Fatalf("insert running fetch: %v", err)
	}

	if err := server.recoverGitOperations(context.Background()); err != nil {
		t.Fatalf("recover Git operations: %v", err)
	}
	var status string
	if err := server.db.QueryRow(`select status from git_operations where id='verified-fetch-operation'`).Scan(&status); err != nil {
		t.Fatalf("read recovered fetch: %v", err)
	}
	if status != gitOperationSucceeded {
		t.Fatalf("verified fetch recovery status: got=%q want=%q", status, gitOperationSucceeded)
	}
}

func TestProjectGitReadRoutesReportRepositoryState(t *testing.T) {
	server := newTestServer(t)
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "readme.txt", "one\n")
	runGitForTest(t, repo, "add", "readme.txt")
	runGitForTest(t, repo, "commit", "-m", "initial commit")
	writeGitTestFile(t, repo, "new.txt", "new\n")
	seedGitProjectForTest(t, server, "git-project", repo)

	handler := server.routes()
	for _, requestPath := range []string{
		"/api/projects/git-project/git/summary",
		"/api/projects/git-project/git/changes",
		"/api/projects/git-project/git/log?ref=HEAD&limit=10",
		"/api/projects/git-project/git/branches",
		"/api/projects/git-project/git/operations",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, requestPath, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", requestPath, response.Code, response.Body.String())
		}
	}

	summary := httptest.NewRecorder()
	handler.ServeHTTP(summary, httptest.NewRequest(http.MethodGet, "/api/projects/git-project/git/summary", nil))
	if !strings.Contains(summary.Body.String(), `"untracked":1`) {
		t.Fatalf("summary did not report untracked file: %s", summary.Body.String())
	}

	diff := httptest.NewRecorder()
	handler.ServeHTTP(diff, httptest.NewRequest(http.MethodGet, "/api/projects/git-project/git/diff?path=new.txt&stage=worktree", nil))
	if diff.Code != http.StatusOK {
		t.Fatalf("diff status=%d body=%s", diff.Code, diff.Body.String())
	}
}

func TestProjectGitReadRoutesHandleAnUnbornHead(t *testing.T) {
	server := newTestServer(t)
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "first.txt", "first\n")
	runGitForTest(t, repo, "add", "first.txt")
	seedGitProjectForTest(t, server, "git-project", repo)

	for _, requestPath := range []string{
		"/api/projects/git-project/git/summary",
		"/api/projects/git-project/git/changes",
		"/api/projects/git-project/git/log?ref=HEAD&limit=10",
		"/api/projects/git-project/git/branches",
		"/api/projects/git-project/git/operations",
	} {
		response := httptest.NewRecorder()
		server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, requestPath, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", requestPath, response.Code, response.Body.String())
		}
	}
}

func TestProjectGitDiffRejectsUntrustedPathAndLogReference(t *testing.T) {
	server := newTestServer(t)
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "readme.txt", "one\n")
	runGitForTest(t, repo, "add", "readme.txt")
	runGitForTest(t, repo, "commit", "-m", "initial commit")
	seedGitProjectForTest(t, server, "git-project", repo)

	handler := server.routes()
	for _, requestPath := range []string{
		"/api/projects/git-project/git/diff?path=/etc/passwd&stage=worktree",
		"/api/projects/git-project/git/diff?path=:(top)&stage=worktree",
		"/api/projects/git-project/git/log?ref=HEAD%5E&limit=10",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, requestPath, nil))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s status=%d body=%s", requestPath, response.Code, response.Body.String())
		}
	}
}

func TestProjectGitSummaryIssuesStateTokenAndStageRejectsStaleState(t *testing.T) {
	server := newTestServer(t)
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "readme.txt", "one\n")
	runGitForTest(t, repo, "add", "readme.txt")
	runGitForTest(t, repo, "commit", "-m", "initial commit")
	writeGitTestFile(t, repo, "feature.txt", "first\n")
	seedGitProjectForTest(t, server, "git-project", repo)

	summary := gitSummaryForTest(t, server, "git-project")
	if summary.StateToken == "" || summary.ObservedAt.IsZero() {
		t.Fatalf("summary has no state token: %#v", summary)
	}
	writeGitTestFile(t, repo, "feature.txt", "second\n")
	changed := httptest.NewRecorder()
	server.routes().ServeHTTP(changed, httptest.NewRequest(http.MethodPost, "/api/projects/git-project/git/stage", bytes.NewBufferString(`{"paths":["feature.txt"],"stateToken":"`+summary.StateToken+`"}`)))
	if changed.Code != http.StatusConflict {
		t.Fatalf("content-changed state status=%d body=%s", changed.Code, changed.Body.String())
	}

	summary = gitSummaryForTest(t, server, "git-project")
	stage := httptest.NewRecorder()
	server.routes().ServeHTTP(stage, httptest.NewRequest(http.MethodPost, "/api/projects/git-project/git/stage", bytes.NewBufferString(`{"paths":["feature.txt"],"stateToken":"`+summary.StateToken+`"}`)))
	if stage.Code != http.StatusAccepted {
		t.Fatalf("stage status=%d body=%s", stage.Code, stage.Body.String())
	}

	stale := httptest.NewRecorder()
	server.routes().ServeHTTP(stale, httptest.NewRequest(http.MethodPost, "/api/projects/git-project/git/unstage", bytes.NewBufferString(`{"paths":["feature.txt"],"stateToken":"`+summary.StateToken+`"}`)))
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale state status=%d body=%s", stale.Code, stale.Body.String())
	}
}

func TestProjectGitStagesAndUnstagesAllEligibleChanges(t *testing.T) {
	server := newTestServer(t)
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "readme.txt", "one\n")
	runGitForTest(t, repo, "add", "readme.txt")
	runGitForTest(t, repo, "commit", "-m", "initial commit")
	writeGitTestFile(t, repo, "readme.txt", "two\n")
	writeGitTestFile(t, repo, "new.txt", "new\n")
	seedGitProjectForTest(t, server, "git-project", repo)

	summary := gitSummaryForTest(t, server, "git-project")
	stage := httptest.NewRecorder()
	server.routes().ServeHTTP(stage, httptest.NewRequest(http.MethodPost, "/api/projects/git-project/git/stage-all", bytes.NewBufferString(`{"stateToken":"`+summary.StateToken+`"}`)))
	if stage.Code != http.StatusAccepted {
		t.Fatalf("stage all status=%d body=%s", stage.Code, stage.Body.String())
	}
	if snapshot := gitSummaryForTest(t, server, "git-project"); snapshot.Worktree.Staged != 2 {
		t.Fatalf("staged=%d, want 2", snapshot.Worktree.Staged)
	}

	summary = gitSummaryForTest(t, server, "git-project")
	unstage := httptest.NewRecorder()
	server.routes().ServeHTTP(unstage, httptest.NewRequest(http.MethodPost, "/api/projects/git-project/git/unstage-all", bytes.NewBufferString(`{"stateToken":"`+summary.StateToken+`"}`)))
	if unstage.Code != http.StatusAccepted {
		t.Fatalf("unstage all status=%d body=%s", unstage.Code, unstage.Body.String())
	}
	if snapshot := gitSummaryForTest(t, server, "git-project"); snapshot.Worktree.Staged != 0 || snapshot.Worktree.Untracked != 1 || snapshot.Worktree.Modified != 1 {
		t.Fatalf("unexpected worktree after unstage all: %#v", snapshot.Worktree)
	}
}

func TestProjectGitStagesAllConflictedChanges(t *testing.T) {
	server := newTestServer(t)
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "readme.txt", "one\n")
	runGitForTest(t, repo, "add", "readme.txt")
	runGitForTest(t, repo, "commit", "-m", "initial commit")
	runGitForTest(t, repo, "checkout", "-b", "feature")
	writeGitTestFile(t, repo, "readme.txt", "feature\n")
	runGitForTest(t, repo, "add", "readme.txt")
	runGitForTest(t, repo, "commit", "-m", "feature change")
	runGitForTest(t, repo, "checkout", "main")
	writeGitTestFile(t, repo, "readme.txt", "main\n")
	runGitForTest(t, repo, "add", "readme.txt")
	runGitForTest(t, repo, "commit", "-m", "main change")
	merge := exec.Command("git", "-C", repo, "merge", "feature")
	if err := merge.Run(); err == nil {
		t.Fatal("merge unexpectedly succeeded")
	}
	seedGitProjectForTest(t, server, "git-project", repo)

	summary := gitSummaryForTest(t, server, "git-project")
	if summary.Worktree.Conflicted != 1 {
		t.Fatalf("conflicted=%d, want 1", summary.Worktree.Conflicted)
	}
	stage := httptest.NewRecorder()
	server.routes().ServeHTTP(stage, httptest.NewRequest(http.MethodPost, "/api/projects/git-project/git/stage-all", bytes.NewBufferString(`{"stateToken":"`+summary.StateToken+`"}`)))
	if stage.Code != http.StatusAccepted {
		t.Fatalf("stage all conflicted file status=%d body=%s", stage.Code, stage.Body.String())
	}
	if snapshot := gitSummaryForTest(t, server, "git-project"); snapshot.Worktree.Conflicted != 0 || snapshot.Worktree.Staged != 1 {
		t.Fatalf("unexpected worktree after staging conflict: %#v", snapshot.Worktree)
	}
}

func TestProjectGitStagesMoreThanOneHundredChangesAtOnce(t *testing.T) {
	server := newTestServer(t)
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "readme.txt", "one\n")
	runGitForTest(t, repo, "add", "readme.txt")
	runGitForTest(t, repo, "commit", "-m", "initial commit")
	for index := 0; index < 101; index++ {
		writeGitTestFile(t, repo, fmt.Sprintf("generated/%03d.txt", index), "new\n")
	}
	seedGitProjectForTest(t, server, "git-project", repo)

	summary := gitSummaryForTest(t, server, "git-project")
	stage := httptest.NewRecorder()
	server.routes().ServeHTTP(stage, httptest.NewRequest(http.MethodPost, "/api/projects/git-project/git/stage-all", bytes.NewBufferString(`{"stateToken":"`+summary.StateToken+`"}`)))
	if stage.Code != http.StatusAccepted {
		t.Fatalf("stage all status=%d body=%s", stage.Code, stage.Body.String())
	}
	if snapshot := gitSummaryForTest(t, server, "git-project"); snapshot.Worktree.Staged != 101 {
		t.Fatalf("staged=%d, want 101", snapshot.Worktree.Staged)
	}
}

func TestProjectGitCommitCreatesCommitFromStagedChanges(t *testing.T) {
	server := newTestServer(t)
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "readme.txt", "one\n")
	runGitForTest(t, repo, "add", "readme.txt")
	runGitForTest(t, repo, "commit", "-m", "initial commit")
	writeGitTestFile(t, repo, "readme.txt", "two\n")
	runGitForTest(t, repo, "add", "readme.txt")
	seedGitProjectForTest(t, server, "git-project", repo)

	summary := gitSummaryForTest(t, server, "git-project")
	commit := httptest.NewRecorder()
	server.routes().ServeHTTP(commit, httptest.NewRequest(http.MethodPost, "/api/projects/git-project/git/commits", bytes.NewBufferString(`{"message":"commit from workbench","stateToken":"`+summary.StateToken+`"}`)))
	if commit.Code != http.StatusAccepted {
		t.Fatalf("commit status=%d body=%s", commit.Code, commit.Body.String())
	}
	if snapshot := gitSummaryForTest(t, server, "git-project"); snapshot.Worktree.Staged != 0 || snapshot.Head.OID == "" {
		t.Fatalf("unexpected worktree after commit: %#v", snapshot)
	}
	commits, err := newGitRunner().Log(context.Background(), repo, "HEAD", 2)
	if err != nil || len(commits) != 2 || commits[0].Subject != "commit from workbench" {
		t.Fatalf("commits=%#v err=%v", commits, err)
	}
	operations, err := server.listGitOperations(context.Background(), "git-project", 1)
	if err != nil || len(operations) != 1 || operations[0].Type != "commit" || operations[0].Status != gitOperationSucceeded {
		t.Fatalf("operations=%#v err=%v", operations, err)
	}
}

func TestProjectGitDiscardRestoresWorktreeOrAllChanges(t *testing.T) {
	server := newTestServer(t)
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "readme.txt", "one\n")
	runGitForTest(t, repo, "add", "readme.txt")
	runGitForTest(t, repo, "commit", "-m", "initial commit")
	writeGitTestFile(t, repo, "readme.txt", "two\n")
	seedGitProjectForTest(t, server, "git-project", repo)

	summary := gitSummaryForTest(t, server, "git-project")
	discard := httptest.NewRecorder()
	server.routes().ServeHTTP(discard, httptest.NewRequest(http.MethodPost, "/api/projects/git-project/git/discard", bytes.NewBufferString(`{"paths":["readme.txt"],"mode":"worktree","stateToken":"`+summary.StateToken+`"}`)))
	if discard.Code != http.StatusAccepted {
		t.Fatalf("discard worktree status=%d body=%s", discard.Code, discard.Body.String())
	}
	content, err := os.ReadFile(filepath.Join(repo, "readme.txt"))
	if err != nil || string(content) != "one\n" {
		t.Fatalf("worktree content=%q err=%v", content, err)
	}

	writeGitTestFile(t, repo, "readme.txt", "three\n")
	runGitForTest(t, repo, "add", "readme.txt")
	writeGitTestFile(t, repo, "readme.txt", "four\n")
	writeGitTestFile(t, repo, "new.txt", "keep me\n")
	summary = gitSummaryForTest(t, server, "git-project")
	discard = httptest.NewRecorder()
	server.routes().ServeHTTP(discard, httptest.NewRequest(http.MethodPost, "/api/projects/git-project/git/discard", bytes.NewBufferString(`{"mode":"all","includeUntracked":false,"stateToken":"`+summary.StateToken+`"}`)))
	if discard.Code != http.StatusAccepted {
		t.Fatalf("discard all status=%d body=%s", discard.Code, discard.Body.String())
	}
	content, err = os.ReadFile(filepath.Join(repo, "readme.txt"))
	if err != nil || string(content) != "one\n" {
		t.Fatalf("all-discard content=%q err=%v", content, err)
	}
	if _, err := os.Stat(filepath.Join(repo, "new.txt")); err != nil {
		t.Fatalf("untracked file was removed without explicit opt-in: %v", err)
	}
}

func TestProjectGitDiscardRemovesOnlyExplicitUntrackedPath(t *testing.T) {
	server := newTestServer(t)
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "readme.txt", "one\n")
	runGitForTest(t, repo, "add", "readme.txt")
	runGitForTest(t, repo, "commit", "-m", "initial commit")
	writeGitTestFile(t, repo, "remove.txt", "remove\n")
	writeGitTestFile(t, repo, "keep.txt", "keep\n")
	seedGitProjectForTest(t, server, "git-project", repo)

	summary := gitSummaryForTest(t, server, "git-project")
	discard := httptest.NewRecorder()
	server.routes().ServeHTTP(discard, httptest.NewRequest(http.MethodPost, "/api/projects/git-project/git/discard", bytes.NewBufferString(`{"paths":["remove.txt"],"mode":"worktree","includeUntracked":true,"stateToken":"`+summary.StateToken+`"}`)))
	if discard.Code != http.StatusAccepted {
		t.Fatalf("discard untracked status=%d body=%s", discard.Code, discard.Body.String())
	}
	if _, err := os.Stat(filepath.Join(repo, "remove.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("untracked file was not removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "keep.txt")); err != nil {
		t.Fatalf("unselected untracked file was removed: %v", err)
	}
}

func TestProjectGitDiscardAllHandlesAnUnbornHead(t *testing.T) {
	server := newTestServer(t)
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "first.txt", "first\n")
	runGitForTest(t, repo, "add", "first.txt")
	seedGitProjectForTest(t, server, "git-project", repo)

	summary := gitSummaryForTest(t, server, "git-project")
	discard := httptest.NewRecorder()
	server.routes().ServeHTTP(discard, httptest.NewRequest(http.MethodPost, "/api/projects/git-project/git/discard", bytes.NewBufferString(`{"mode":"all","includeUntracked":false,"stateToken":"`+summary.StateToken+`"}`)))
	if discard.Code != http.StatusAccepted {
		t.Fatalf("discard initial status=%d body=%s", discard.Code, discard.Body.String())
	}
	if _, err := os.Stat(filepath.Join(repo, "first.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("initial staged file remains: %v", err)
	}
	if snapshot := gitSummaryForTest(t, server, "git-project"); snapshot.Worktree.Staged != 0 || snapshot.Worktree.Untracked != 0 {
		t.Fatalf("unexpected initial worktree after discard: %#v", snapshot.Worktree)
	}
}

func TestProjectGitDiscardAllDeletesExplicitlySelectedUntrackedDirectory(t *testing.T) {
	server := newTestServer(t)
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "readme.txt", "one\n")
	runGitForTest(t, repo, "add", "readme.txt")
	runGitForTest(t, repo, "commit", "-m", "initial commit")
	writeGitTestFile(t, repo, "readme.txt", "two\n")
	writeGitTestFile(t, repo, "generated/entry.txt", "new\n")
	seedGitProjectForTest(t, server, "git-project", repo)

	summary := gitSummaryForTest(t, server, "git-project")
	discard := httptest.NewRecorder()
	server.routes().ServeHTTP(discard, httptest.NewRequest(http.MethodPost, "/api/projects/git-project/git/discard", bytes.NewBufferString(`{"mode":"all","includeUntracked":true,"stateToken":"`+summary.StateToken+`"}`)))
	if discard.Code != http.StatusAccepted {
		t.Fatalf("discard directory status=%d body=%s", discard.Code, discard.Body.String())
	}
	content, err := os.ReadFile(filepath.Join(repo, "readme.txt"))
	if err != nil || string(content) != "one\n" {
		t.Fatalf("tracked file was not restored: %q err=%v", content, err)
	}
	if _, err := os.Stat(filepath.Join(repo, "generated")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("untracked directory remains: %v", err)
	}
}

func TestRemoveUntrackedFromRootRejectsAnEscapingSymlink(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir()
	writeGitTestFile(t, repo, "nested/remove.txt", "inside\n")
	writeGitTestFile(t, outside, "remove.txt", "outside\n")
	if err := os.Rename(filepath.Join(repo, "nested"), filepath.Join(repo, "nested-real")); err != nil {
		t.Fatalf("rename nested directory: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "nested")); err != nil {
		t.Fatalf("create escaping symlink: %v", err)
	}

	backend := newLocalGitBackend()
	if err := backend.validateUntrackedRemoval(repo, []string{"nested/remove.txt"}); err == nil {
		t.Fatal("validation through escaping symlink unexpectedly succeeded")
	}
	if err := backend.removeUntracked(repo, []string{"nested/remove.txt"}); err == nil {
		t.Fatal("removal through escaping symlink unexpectedly succeeded")
	}
	content, err := os.ReadFile(filepath.Join(outside, "remove.txt"))
	if err != nil || string(content) != "outside\n" {
		t.Fatalf("outside file changed: content=%q err=%v", content, err)
	}
}

func TestGitOperationFailureClassifiesSafeErrorMessages(t *testing.T) {
	for _, test := range []struct {
		name, stderr, wantStatus, wantCode string
	}{
		{name: "identity", stderr: "Author identity unknown\nPlease tell me who you are", wantStatus: gitOperationFailed, wantCode: "identity_not_configured"},
		{name: "index lock", stderr: "fatal: Unable to create '.git/index.lock': File exists.", wantStatus: gitOperationFailed, wantCode: "repository_locked"},
		{name: "hook", stderr: "error: hook declined to update refs", wantStatus: gitOperationFailed, wantCode: "hook_rejected"},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, code, _ := gitOperationFailure(&gitCommandError{command: "commit", stderr: test.stderr})
			if status != test.wantStatus || code != test.wantCode {
				t.Fatalf("status=%q code=%q", status, code)
			}
		})
	}
	status, code, _ := gitOperationFailure(partiallyAppliedGitError(errors.New("remove failed")))
	if status != gitOperationNeedsAttention || code != "partial_result" {
		t.Fatalf("partial status=%q code=%q", status, code)
	}
}

func TestExecuteGitOperationNeedsAttentionWhenResultCannotBeVerified(t *testing.T) {
	server := newTestServer(t)
	repo := t.TempDir()
	seedGitProjectForTest(t, server, "git-project", repo)

	result, err := server.executeGitOperation(context.Background(), newGitRunner(), "git-project", repo, "stage", "stage file", GitSnapshot{}, func(GitRunner) error {
		return nil
	})
	if err != nil {
		t.Fatalf("execute operation: %v", err)
	}
	if result.Status != gitOperationNeedsAttention || result.ErrorMessage == "" {
		t.Fatalf("result=%#v", result)
	}
	operations, err := server.listGitOperations(context.Background(), "git-project", 1)
	if err != nil || len(operations) != 1 {
		t.Fatalf("operations=%#v err=%v", operations, err)
	}
	operation := operations[0]
	if operation.Status != gitOperationNeedsAttention || operation.ErrorCode != "result_unverified" || operation.AfterState != "{}" {
		t.Fatalf("operation=%#v", operation)
	}
}

func TestExecuteGitOperationReturnsNeedsAttentionWhenAuditCannotBeSaved(t *testing.T) {
	server := newTestServer(t)
	repo := newTempGitRepository(t)
	seedGitProjectForTest(t, server, "git-project", repo)

	result, err := server.executeGitOperation(context.Background(), newGitRunner(), "git-project", repo, "stage", "stage file", GitSnapshot{}, func(GitRunner) error {
		return server.db.Close()
	})
	if err != nil {
		t.Fatalf("execute operation: %v", err)
	}
	if result.Status != gitOperationNeedsAttention || result.ErrorMessage == "" {
		t.Fatalf("result=%#v", result)
	}
}

func TestRetryGitOperationUpdateRecordsNeedsAttention(t *testing.T) {
	server := newTestServer(t)
	repo := newTempGitRepository(t)
	seedGitProjectForTest(t, server, "git-project", repo)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into git_operations (id,project_id,type,status,request_summary,before_state,requested_at,started_at) values ('operation','git-project','stage','running','stage file','{}',?,?)`, now, now); err != nil {
		t.Fatalf("insert operation: %v", err)
	}

	server.retryGitOperationUpdate("operation", "{}", "audit_update_failed", "unable to record result", now)
	operations, err := server.listGitOperations(context.Background(), "git-project", 1)
	if err != nil || len(operations) != 1 {
		t.Fatalf("operations=%#v err=%v", operations, err)
	}
	if operation := operations[0]; operation.Status != gitOperationNeedsAttention || operation.ErrorCode != "audit_update_failed" {
		t.Fatalf("operation=%#v", operation)
	}
}

func TestProjectGitDiffReturnsUntrackedContentAndRejectsUnchangedPath(t *testing.T) {
	server := newTestServer(t)
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "readme.txt", "one\n")
	runGitForTest(t, repo, "add", "readme.txt")
	runGitForTest(t, repo, "commit", "-m", "initial commit")
	writeGitTestFile(t, repo, "new.txt", "untracked content\n")
	seedGitProjectForTest(t, server, "git-project", repo)

	for path, wantStatus := range map[string]int{"new.txt": http.StatusOK, "readme.txt": http.StatusBadRequest} {
		response := httptest.NewRecorder()
		server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/projects/git-project/git/diff?path="+path+"&stage=worktree", nil))
		if response.Code != wantStatus {
			t.Fatalf("%s status=%d body=%s", path, response.Code, response.Body.String())
		}
		if path == "new.txt" && !strings.Contains(response.Body.String(), "untracked content") {
			t.Fatalf("untracked diff did not include content: %s", response.Body.String())
		}
	}
}

type gitSummaryTestResponse struct {
	GitSnapshot
	StateToken string    `json:"stateToken"`
	ObservedAt time.Time `json:"observedAt"`
}

func gitSummaryForTest(t *testing.T, server *Server, projectID string) gitSummaryTestResponse {
	t.Helper()
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/git/summary", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("summary status=%d body=%s", response.Code, response.Body.String())
	}
	var summary gitSummaryTestResponse
	if err := json.Unmarshal(response.Body.Bytes(), &summary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	return summary
}

func seedGitProjectForTest(t *testing.T, server *Server, projectID, repo string) {
	t.Helper()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values (?,?,?,'wsl-local','main',1,?)`, projectID, "Git project", repo, time.Now().UTC()); err != nil {
		t.Fatalf("insert Git project: %v", err)
	}
}

func newTempGitRepository(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runGitForTest(t, repo, "init", "-b", "main")
	runGitForTest(t, repo, "config", "user.name", "Auto Test")
	runGitForTest(t, repo, "config", "user.email", "auto@example.test")
	return repo
}

func writeGitTestFile(t *testing.T, repo, path, content string) {
	t.Helper()
	fullPath := filepath.Join(repo, path)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatalf("create Git test directory: %v", err)
	}
	if err := os.WriteFile(fullPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write Git test file: %v", err)
	}
}

func runGitForTest(t *testing.T, repo string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repo}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func writeFakeGit(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatalf("write fake Git: %v", err)
	}
	return path
}

func TestTaskUpdateStatusViaPatchAllowsAllowedTransitions(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	taskID := createTaskForTest(t, server.routes(), projectID, "Draggable task")

	// todo -> action_required is allowed
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/api/tasks/"+taskID, bytes.NewBufferString(`{"title":"Draggable task","description":"Implement Draggable task safely.","acceptanceCriteria":"Tests cover Draggable task.","priority":"normal","position":1,"status":"action_required"}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("todo->action_required status: %d body=%s", response.Code, response.Body.String())
	}
	assertTaskStatus(t, server, taskID, taskActionRequired)

	// action_required -> todo is allowed
	response = httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/api/tasks/"+taskID, bytes.NewBufferString(`{"title":"Draggable task","description":"Implement Draggable task safely.","acceptanceCriteria":"Tests cover Draggable task.","priority":"normal","position":2,"status":"todo"}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("action_required->todo status: %d body=%s", response.Code, response.Body.String())
	}
	assertTaskStatus(t, server, taskID, taskTodo)
}

func TestTaskUpdateStatusViaPatchRejectsDisallowedTransitions(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	taskID := createTaskForTest(t, server.routes(), projectID, "Immutable task")

	// todo -> done is NOT allowed
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/api/tasks/"+taskID, bytes.NewBufferString(`{"title":"Immutable task","description":"Implement Immutable task safely.","acceptanceCriteria":"Tests cover Immutable task.","priority":"normal","position":1,"status":"done"}`)))
	if response.Code != http.StatusConflict {
		t.Fatalf("todo->done should be rejected, got %d body=%s", response.Code, response.Body.String())
	}

	// todo -> running is NOT allowed
	response = httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/api/tasks/"+taskID, bytes.NewBufferString(`{"title":"Immutable task","description":"Implement Immutable task safely.","acceptanceCriteria":"Tests cover Immutable task.","priority":"normal","position":1,"status":"running"}`)))
	if response.Code != http.StatusConflict {
		t.Fatalf("todo->running should be rejected, got %d body=%s", response.Code, response.Body.String())
	}
}

func TestTaskUpdateStatusViaPatchRejectsRunningTask(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	taskID := createTaskForTest(t, server.routes(), projectID, "Running task")
	if _, err := server.db.Exec(`update tasks set status=? where id=?`, taskRunning, taskID); err != nil {
		t.Fatalf("mark task running: %v", err)
	}

	// running -> anything is NOT allowed via PATCH
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/api/tasks/"+taskID, bytes.NewBufferString(`{"title":"Running task","description":"Implement Running task safely.","acceptanceCriteria":"Tests cover Running task.","priority":"normal","position":1,"status":"todo"}`)))
	if response.Code != http.StatusConflict {
		t.Fatalf("running->todo should be rejected, got %d body=%s", response.Code, response.Body.String())
	}
}

func TestTaskUpdatePositionReorderPreservesOtherFields(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	firstID := createTaskForTest(t, server.routes(), projectID, "First task")
	secondID := createTaskForTest(t, server.routes(), projectID, "Second task")
	thirdID := createTaskForTest(t, server.routes(), projectID, "Third task")

	// Set explicit positions
	for i, id := range []string{firstID, secondID, thirdID} {
		response := httptest.NewRecorder()
		server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/api/tasks/"+id, bytes.NewBufferString(`{"title":"Task `+id[:8]+`","description":"Implement Task `+id[:8]+` safely.","acceptanceCriteria":"Tests cover Task `+id[:8]+`.","priority":"normal","position":`+fmt.Sprintf("%d", (i+1)*10)+`}`)))
		if response.Code != http.StatusOK {
			t.Fatalf("set position for %s: %d body=%s", id, response.Code, response.Body.String())
		}
	}

	// Move second between first and third: position = 15
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/api/tasks/"+secondID, bytes.NewBufferString(`{"title":"Task `+secondID[:8]+`","description":"Implement Task `+secondID[:8]+` safely.","acceptanceCriteria":"Tests cover Task `+secondID[:8]+`.","priority":"normal","position":15}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("reorder second task: %d body=%s", response.Code, response.Body.String())
	}

	// Verify positions via list API
	listResponse := httptest.NewRecorder()
	server.routes().ServeHTTP(listResponse, httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/tasks", nil))
	if listResponse.Code != http.StatusOK {
		t.Fatalf("list tasks: %d", listResponse.Code)
	}
	var tasks []Task
	if err := json.NewDecoder(listResponse.Body).Decode(&tasks); err != nil {
		t.Fatalf("decode task list: %v", err)
	}
	if len(tasks) < 3 {
		t.Fatalf("expected at least 3 tasks, got %d", len(tasks))
	}
	// Tasks should be ordered by position
	for i := 1; i < len(tasks); i++ {
		if tasks[i].Position < tasks[i-1].Position {
			t.Fatalf("tasks not sorted by position: %v before %v", tasks[i-1].Position, tasks[i].Position)
		}
	}
}

func TestTaskUpdatePositionAllowsZero(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	taskID := createTaskForTest(t, server.routes(), projectID, "Move to first position")
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/api/tasks/"+taskID, bytes.NewBufferString(`{"title":"Move to first position","description":"Implement Move to first position safely.","acceptanceCriteria":"Tests cover Move to first position.","priority":"normal","position":0}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("move task to position zero: %d body=%s", response.Code, response.Body.String())
	}
	var position float64
	if err := server.db.QueryRow(`select position from tasks where id=?`, taskID).Scan(&position); err != nil {
		t.Fatalf("read task position: %v", err)
	}
	if position != 0 {
		t.Fatalf("expected position 0, got %v", position)
	}
}

func TestTaskPinningPersistsAndLeadsTaskList(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	firstID := createTaskForTest(t, server.routes(), projectID, "First task")
	pinnedID := createTaskForTest(t, server.routes(), projectID, "Pinned task")
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/api/tasks/"+pinnedID, bytes.NewBufferString(`{"title":"Pinned task","description":"Implement pinned queue ordering safely.","acceptanceCriteria":"Pinned tasks are first.","priority":"low","pinned":true,"position":999}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("pin task: %d body=%s", response.Code, response.Body.String())
	}
	listResponse := httptest.NewRecorder()
	server.routes().ServeHTTP(listResponse, httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/tasks", nil))
	if listResponse.Code != http.StatusOK {
		t.Fatalf("list tasks: %d body=%s", listResponse.Code, listResponse.Body.String())
	}
	var tasks []Task
	if err := json.NewDecoder(listResponse.Body).Decode(&tasks); err != nil {
		t.Fatalf("decode task list: %v", err)
	}
	if len(tasks) < 2 || tasks[0].ID != pinnedID || !tasks[0].Pinned {
		t.Fatalf("pinned task was not first: %#v", tasks)
	}
	if tasks[1].ID != firstID {
		t.Fatalf("unexpected second task: %q", tasks[1].ID)
	}
}

func TestDeleteTaskRemovesTerminalTask(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	taskID := createTaskForTest(t, server.routes(), projectID, "Delete me")
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/tasks/"+taskID, nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete task: %d body=%s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/tasks/"+taskID, nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("deleted task lookup: %d body=%s", response.Code, response.Body.String())
	}
}

func TestDeleteTaskRejectsRunningTask(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	taskID := createTaskForTest(t, server.routes(), projectID, "Running task")
	if _, err := server.db.Exec(`update tasks set status=? where id=?`, taskRunning, taskID); err != nil {
		t.Fatalf("mark task running: %v", err)
	}

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/tasks/"+taskID, nil))
	if response.Code != http.StatusConflict {
		t.Fatalf("delete running task: %d body=%s", response.Code, response.Body.String())
	}
	var status string
	if err := server.db.QueryRow(`select status from tasks where id=?`, taskID).Scan(&status); err != nil {
		t.Fatalf("read running task after delete: %v", err)
	}
	if status != taskRunning {
		t.Fatalf("running task status=%q, want %q", status, taskRunning)
	}
}

func TestCancelledTaskCanOnlyBeDeleted(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	taskID := createTaskForTest(t, server.routes(), projectID, "Legacy cancelled task")
	dependentID := createTaskForTest(t, server.routes(), projectID, "Task depending on cancelled task")
	if _, err := server.db.Exec(`update tasks set status=?,cancelled_at=? where id=?`, taskCancelled, time.Now().UTC(), taskID); err != nil {
		t.Fatalf("prepare legacy cancelled task: %v", err)
	}
	if _, err := server.db.Exec(`insert into task_dependencies (task_id,predecessor_task_id,created_at) values (?,?,?)`, dependentID, taskID, time.Now().UTC()); err != nil {
		t.Fatalf("add dependent task: %v", err)
	}

	reopen := httptest.NewRecorder()
	server.routes().ServeHTTP(reopen, httptest.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/reopen", nil))
	if reopen.Code != http.StatusConflict {
		t.Fatalf("reopen cancelled task: %d body=%s", reopen.Code, reopen.Body.String())
	}

	update := httptest.NewRecorder()
	server.routes().ServeHTTP(update, httptest.NewRequest(http.MethodPatch, "/api/tasks/"+taskID, bytes.NewBufferString(`{"title":"Legacy cancelled task","description":"Updated description","acceptanceCriteria":"","priority":"normal"}`)))
	if update.Code != http.StatusConflict {
		t.Fatalf("update cancelled task: %d body=%s", update.Code, update.Body.String())
	}

	addDependency := httptest.NewRecorder()
	server.routes().ServeHTTP(addDependency, httptest.NewRequest(http.MethodPost, "/api/tasks/"+taskID+"/dependencies", bytes.NewBufferString(`{"predecessorTaskId":"`+dependentID+`"}`)))
	if addDependency.Code != http.StatusConflict {
		t.Fatalf("add dependency to cancelled task: %d body=%s", addDependency.Code, addDependency.Body.String())
	}

	replaceDependencies := httptest.NewRecorder()
	server.routes().ServeHTTP(replaceDependencies, httptest.NewRequest(http.MethodPut, "/api/tasks/"+taskID+"/dependencies", bytes.NewBufferString(`{"predecessorTaskIds":[]}`)))
	if replaceDependencies.Code != http.StatusConflict {
		t.Fatalf("replace dependencies on cancelled task: %d body=%s", replaceDependencies.Code, replaceDependencies.Body.String())
	}

	deleteDependency := httptest.NewRecorder()
	server.routes().ServeHTTP(deleteDependency, httptest.NewRequest(http.MethodDelete, "/api/tasks/"+taskID+"/dependencies/"+dependentID, nil))
	if deleteDependency.Code != http.StatusConflict {
		t.Fatalf("delete dependency from cancelled task: %d body=%s", deleteDependency.Code, deleteDependency.Body.String())
	}

	deleteResponse := httptest.NewRecorder()
	server.routes().ServeHTTP(deleteResponse, httptest.NewRequest(http.MethodDelete, "/api/tasks/"+taskID, nil))
	if deleteResponse.Code != http.StatusNoContent {
		t.Fatalf("delete cancelled task: %d body=%s", deleteResponse.Code, deleteResponse.Body.String())
	}
	var dependencyCount int
	if err := server.db.QueryRow(`select count(*) from task_dependencies where task_id=? or predecessor_task_id=?`, taskID, taskID).Scan(&dependencyCount); err != nil {
		t.Fatalf("count removed dependencies: %v", err)
	}
	if dependencyCount != 0 {
		t.Fatalf("expected deleted task dependencies to be removed, got %d", dependencyCount)
	}
}

func TestTaskUpdateStatusDoesNotAffectUnrelatedFields(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	taskID := createTaskForTest(t, server.routes(), projectID, "Keep description intact")

	// Update only status — description must stay the same
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/api/tasks/"+taskID, bytes.NewBufferString(`{"title":"Keep description intact","description":"Implement Keep description intact safely.","acceptanceCriteria":"Tests cover Keep description intact.","priority":"normal","status":"action_required"}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("update status: %d body=%s", response.Code, response.Body.String())
	}

	var detail TaskDetail
	detailResponse := httptest.NewRecorder()
	server.routes().ServeHTTP(detailResponse, httptest.NewRequest(http.MethodGet, "/api/tasks/"+taskID, nil))
	if err := json.NewDecoder(detailResponse.Body).Decode(&detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail.Description != "Implement Keep description intact safely." {
		t.Fatalf("description changed unexpectedly: %q", detail.Description)
	}
	if detail.Status != taskActionRequired {
		t.Fatalf("status not updated: got %q want %q", detail.Status, taskActionRequired)
	}
}

func TestTaskUpdateInvalidStatusReturnsError(t *testing.T) {
	server, projectID, _ := seedTaskConversation(t)
	taskID := createTaskForTest(t, server.routes(), projectID, "Invalid status task")

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/api/tasks/"+taskID, bytes.NewBufferString(`{"title":"Invalid status task","description":"Implement Invalid status task safely.","acceptanceCriteria":"Tests cover Invalid status task.","priority":"normal","status":"nonexistent"}`)))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid status should be rejected, got %d body=%s", response.Code, response.Body.String())
	}
}

func TestParseContentPartsArrayFormat(t *testing.T) {
	raw := json.RawMessage(`[{"type":"text","text":"hello"},{"type":"text","text":"world"}]`)
	parts := parseContentParts(raw)
	if len(parts) != 2 || parts[0] != "hello" || parts[1] != "world" {
		t.Fatalf("expected [hello world], got %v", parts)
	}
}

func TestParseContentPartsStringFormat(t *testing.T) {
	raw := json.RawMessage(`"hello world"`)
	parts := parseContentParts(raw)
	if len(parts) != 1 || parts[0] != "hello world" {
		t.Fatalf("expected [hello world], got %v", parts)
	}
}

func TestParseContentPartsArraySkipsNonTextAndEmpty(t *testing.T) {
	raw := json.RawMessage(`[{"type":"tool_use","text":"ignored"},{"type":"text","text":""},{"type":"text","text":"valid"}]`)
	parts := parseContentParts(raw)
	if len(parts) != 1 || parts[0] != "valid" {
		t.Fatalf("expected [valid], got %v", parts)
	}
}

func TestParseContentPartsEmptyString(t *testing.T) {
	raw := json.RawMessage(`""`)
	parts := parseContentParts(raw)
	if len(parts) != 0 {
		t.Fatalf("expected empty, got %v", parts)
	}
}

func TestParseContentPartsInvalidJSON(t *testing.T) {
	raw := json.RawMessage(`not-json`)
	parts := parseContentParts(raw)
	if len(parts) != 0 {
		t.Fatalf("expected empty for invalid JSON, got %v", parts)
	}
}

func TestProjectRunnerStartStop(t *testing.T) {
	var broadcasted []LogEntry
	var mu sync.Mutex
	broadcast := func(e LogEntry) {
		mu.Lock()
		broadcasted = append(broadcasted, e)
		mu.Unlock()
	}

	tmpDir := t.TempDir()
	runner := newProjectRunner("test-project", "", "echo hello && sleep 3600", nil, broadcast)

	err := runner.Start(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	hasHello := false
	hasStarted := false
	for _, e := range broadcasted {
		if strings.Contains(e.Text, "hello") {
			hasHello = true
		}
		if strings.Contains(e.Text, "进程已启动") {
			hasStarted = true
		}
	}
	mu.Unlock()

	if !hasStarted {
		t.Fatal("expected process started system message")
	}
	if !hasHello {
		t.Fatal("expected hello in stdout")
	}

	snapshot := runner.StatusSnapshot()
	if snapshot.Status != RunStatusRunning {
		t.Fatalf("expected running, got %s", snapshot.Status)
	}

	if err := runner.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}

	time.Sleep(200 * time.Millisecond)
	snapshot = runner.StatusSnapshot()
	if snapshot.Status != RunStatusStopped {
		t.Fatalf("expected stopped after manual stop, got %s", snapshot.Status)
	}
}

func TestProjectRunnerRejectedStartKeepsCurrentConfig(t *testing.T) {
	tmpDir := t.TempDir()
	originalEnv := map[string]string{"PORT": "3000"}
	runner := newProjectRunner("test-project", "", "sleep 3600", originalEnv, func(LogEntry) {})
	if err := runner.Start(context.Background(), tmpDir); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		if runner.StatusSnapshot().Status == RunStatusRunning {
			_ = runner.Stop()
		}
	})

	if err := runner.StartWithConfig(context.Background(), tmpDir, "other", "echo replaced", map[string]string{"PORT": "4000"}, RunExecutionTargetAuto); err == nil {
		t.Fatal("expected a second start to be rejected")
	}

	runner.mu.RLock()
	workDir, command, port := runner.workDir, runner.command, runner.envVars["PORT"]
	runner.mu.RUnlock()
	if workDir != "" || command != "sleep 3600" || port != "3000" {
		t.Fatalf("rejected start changed configuration: workDir=%q command=%q PORT=%q", workDir, command, port)
	}
}

func TestProjectRunnerPathEscape(t *testing.T) {
	runner := newProjectRunner("test", "../../etc", "echo bad", nil, func(e LogEntry) {})
	err := runner.Start(context.Background(), "/tmp/test-project")
	if err == nil {
		t.Fatal("expected error for path escape")
	}
	if !strings.Contains(err.Error(), "工作目录不能超出项目路径") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestProjectRunnerRejectsSymlinkWorkDirEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic link setup requires elevated privileges on Windows")
	}
	projectPath := t.TempDir()
	outsidePath := t.TempDir()
	if err := os.Symlink(outsidePath, filepath.Join(projectPath, "outside")); err != nil {
		t.Fatalf("create symbolic link: %v", err)
	}
	runner := newProjectRunner("test", "outside", "echo bad", nil, func(LogEntry) {})
	err := runner.Start(context.Background(), projectPath)
	if err == nil || !strings.Contains(err.Error(), "工作目录不能超出项目路径") {
		t.Fatalf("expected symbolic-link escape rejection, got %v", err)
	}

	err = validateProjectRunConfig(Project{Path: projectPath}, RunConfig{WorkDir: "outside", Command: "echo bad"})
	if err == nil || !strings.Contains(err.Error(), "工作目录不能超出项目路径") {
		t.Fatalf("expected symbolic-link config rejection, got %v", err)
	}
}

func TestRunExecutionTargetAutoDetectsMountedWindowsProjects(t *testing.T) {
	target, err := resolveRunExecutionTarget("/mnt/d/projects/Programs/Floatory", RunExecutionTargetAuto)
	if err != nil {
		t.Fatalf("resolve target: %v", err)
	}
	if target != RunExecutionTargetWindows {
		t.Fatalf("target=%q, want windows", target)
	}

	path, ok := wslPathToWindowsPath("/mnt/d/projects/Programs/Floatory/src-tauri")
	if !ok || path != `D:\projects\Programs\Floatory\src-tauri` {
		t.Fatalf("Windows path=%q, ok=%v", path, ok)
	}
}

func TestRunExecutionTargetKeepsWSLProjectsAndAllowsOverride(t *testing.T) {
	target, err := resolveRunExecutionTarget("/home/tangmaoke/projects/milevia", RunExecutionTargetAuto)
	if err != nil {
		t.Fatalf("resolve WSL target: %v", err)
	}
	if target != RunExecutionTargetWSL {
		t.Fatalf("target=%q, want wsl", target)
	}

	target, err = resolveRunExecutionTarget("/mnt/d/projects/Programs/Floatory", RunExecutionTargetWSL)
	if err != nil || target != RunExecutionTargetWSL {
		t.Fatalf("WSL override target=%q, err=%v", target, err)
	}

	_, err = resolveRunExecutionTarget("/home/tangmaoke/projects/milevia", RunExecutionTargetWindows)
	if err == nil || !strings.Contains(err.Error(), "仅支持") {
		t.Fatalf("expected Windows target rejection, got %v", err)
	}
}

func TestWindowsExecutionTargetKeepsPublicValue(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows target behavior")
	}
	target, err := resolveRunExecutionTarget(`C:\\projects\\milevia`, RunExecutionTargetAuto)
	if err != nil || target != RunExecutionTargetWindows {
		t.Fatalf("target=%q err=%v, want windows", target, err)
	}
}

func TestWindowsPowerShellCommandUsesTextOutputFormat(t *testing.T) {
	cmd := newWindowsPowerShellCommand(context.Background(), "powershell.exe", "encoded-script")
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "-OutputFormat Text") {
		t.Fatalf("PowerShell output format must be text, got %q", args)
	}
	if !strings.Contains(args, "-EncodedCommand encoded-script") {
		t.Fatalf("encoded script missing from command: %q", args)
	}
}

func TestWindowsPowerShellRedirectedOutputSuppressesCLIXMLProgress(t *testing.T) {
	if _, err := os.Stat("/proc/sys/fs/binfmt_misc/WSLInterop"); err != nil {
		t.Skip("Windows interop is unavailable")
	}
	if _, err := os.Stat("/mnt/c/Windows"); err != nil {
		t.Skip("Windows system directory is unavailable")
	}
	powershellPath, err := windowsSystemExecutable("powershell.exe")
	if err != nil {
		t.Fatalf("find PowerShell: %v", err)
	}

	script := `$ProgressPreference = 'SilentlyContinue'; $ErrorActionPreference = 'Stop'; try { Write-Progress -Activity 'test' -Status 'hidden'; Write-Error 'AUTO_POWERSHELL_TEXT_OUTPUT' } catch { [Console]::Error.WriteLine($_.Exception.Message); exit 1 }`
	encoded := base64.StdEncoding.EncodeToString(utf16LE(script))
	output, err := newWindowsPowerShellCommand(context.Background(), powershellPath, encoded).CombinedOutput()
	if err == nil {
		t.Fatalf("expected PowerShell script to fail: %s", output)
	}
	text := string(output)
	if strings.Contains(text, "#< CLIXML") {
		t.Fatalf("PowerShell emitted CLIXML progress instead of plain output: %s", text)
	}
	if !strings.Contains(text, "AUTO_POWERSHELL_TEXT_OUTPUT") {
		t.Fatalf("expected plain PowerShell error text, got %q", text)
	}
}

func TestWindowsLaunchErrorPrefersLauncherStderr(t *testing.T) {
	runner := &projectRunner{
		windowsPIDMarker:  "__AUTO_WINDOWS_CHILD_test__=",
		windowsStartError: "无法启动 cmd.exe",
		exitCode:          1,
		hasExitCode:       true,
	}
	if got := runner.windowsLaunchError().Error(); got != "Windows 启动失败: 无法启动 cmd.exe" {
		t.Fatalf("launch error = %q", got)
	}
}

func TestWindowsLaunchErrorUsesExitCodeWithoutLauncherStderr(t *testing.T) {
	runner := &projectRunner{exitCode: 1, hasExitCode: true}
	if got := runner.windowsLaunchError().Error(); got != "Windows 启动器在返回进程 ID 前退出: code=1" {
		t.Fatalf("launch error = %q", got)
	}
}

func TestRunConfigDefaultsAndPersistsExecutionTarget(t *testing.T) {
	server, projectID := seedServerWithProject(t)
	handler := server.routes()

	saved := httptest.NewRecorder()
	handler.ServeHTTP(saved, httptest.NewRequest(http.MethodPut, "/api/projects/"+projectID+"/run/config", strings.NewReader(`{"workDir":"","command":"npm run dev","envVars":{},"executionTarget":"wsl"}`)))
	if saved.Code != http.StatusOK {
		t.Fatalf("save config: %d body=%s", saved.Code, saved.Body.String())
	}

	loaded := httptest.NewRecorder()
	handler.ServeHTTP(loaded, httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/run/config", nil))
	var cfg RunConfig
	if err := json.NewDecoder(loaded.Body).Decode(&cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if cfg.ExecutionTarget != RunExecutionTargetWSL {
		t.Fatalf("execution target=%q, want wsl", cfg.ExecutionTarget)
	}
}

func TestRemoteRunConfigAcceptsRemoteLinuxPaths(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,runner_id,git_branch,claude_ready,created_at) values ('remote-run','remote-run','/srv/apps/api','ssh-remote','ssh-remote','main',1,?)`, now); err != nil {
		t.Fatalf("insert remote project: %v", err)
	}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/api/projects/remote-run/run/config", strings.NewReader(`{"workDir":"/srv/apps/api/frontend","command":"npm run dev","envVars":{},"executionTarget":"auto"}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("save remote config: %d body=%s", response.Code, response.Body.String())
	}
	var cfg RunConfig
	if err := json.NewDecoder(response.Body).Decode(&cfg); err != nil {
		t.Fatalf("decode remote config: %v", err)
	}
	if cfg.WorkDir != "/srv/apps/api/frontend" || cfg.ExecutionTarget != RunExecutionTargetAuto {
		t.Fatalf("unexpected remote config: %#v", cfg)
	}
}

func TestWindowsProjectRunnerUsesMountedSystemToolsAndStopsChildTree(t *testing.T) {
	if _, err := os.Stat("/proc/sys/fs/binfmt_misc/WSLInterop"); err != nil {
		t.Skip("Windows interop is unavailable")
	}
	if _, err := os.Stat("/mnt/c/Windows"); err != nil {
		t.Skip("Windows system directory is unavailable")
	}
	if _, err := exec.LookPath("powershell.exe"); err == nil {
		t.Skip("requires a WSL environment where Windows executables are not on PATH")
	}

	runner := newProjectRunner("windows-process", "", "ping -t 127.0.0.1", nil, func(LogEntry) {})
	if err := runner.Start(context.Background(), "/mnt/c/Windows"); err != nil {
		t.Fatalf("start Windows runner: %v", err)
	}
	t.Cleanup(func() {
		if runner.StatusSnapshot().Status == RunStatusRunning {
			_ = runner.Stop()
		}
	})

	runner.mu.RLock()
	pid := runner.windowsPID
	runner.mu.RUnlock()
	if pid <= 0 {
		t.Fatalf("Windows child PID was not captured: %d", pid)
	}
	if err := runner.Stop(); err != nil {
		t.Fatalf("stop Windows runner: %v", err)
	}

	tasklistPath, err := windowsSystemExecutable("tasklist.exe")
	if err != nil {
		t.Fatalf("find tasklist: %v", err)
	}
	output, err := exec.Command(tasklistPath, "/FI", "PID eq "+strconv.Itoa(pid)).CombinedOutput()
	if err != nil {
		t.Fatalf("inspect Windows process: %v: %s", err, output)
	}
	if strings.Contains(string(output), strconv.Itoa(pid)) {
		t.Fatalf("Windows process tree still contains PID %d: %s", pid, output)
	}
}

func TestServerCloseStopsHTTPListener(t *testing.T) {
	server := newTestServer(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve listener: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release listener: %v", err)
	}

	listenDone := make(chan error, 1)
	go func() { listenDone <- server.Listen(addr) }()
	deadline := time.Now().Add(time.Second)
	listening := false
	for time.Now().Before(deadline) {
		client := &http.Client{Timeout: 100 * time.Millisecond}
		response, err := client.Get("http://" + addr + "/api/health")
		if err == nil {
			response.Body.Close()
			listening = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !listening {
		t.Fatal("HTTP server did not start listening")
	}

	server.Close()
	select {
	case err := <-listenDone:
		if err != nil {
			t.Fatalf("listener returned an unexpected error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("listener did not stop after server close")
	}
}

func TestProjectRunnerRingBuffer(t *testing.T) {
	rb := newRingBuffer(3)
	now := time.Now()
	rb.Append(LogEntry{Timestamp: now, Stream: "stdout", Text: "1"})
	rb.Append(LogEntry{Timestamp: now, Stream: "stdout", Text: "2"})
	rb.Append(LogEntry{Timestamp: now, Stream: "stdout", Text: "3"})
	rb.Append(LogEntry{Timestamp: now, Stream: "stdout", Text: "4"})

	recent := rb.Recent(3)
	if len(recent) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(recent))
	}
	if recent[0].Text != "2" || recent[1].Text != "3" || recent[2].Text != "4" {
		t.Fatalf("unexpected order: %v", recent)
	}
}

func TestProjectRunnerBoundsLogTextAndAssignsSequenceIDs(t *testing.T) {
	runner := newProjectRunner("logs", "", "", nil, func(LogEntry) {})
	runner.emitLog(LogEntry{Timestamp: time.Now(), Stream: "stdout", Text: "first"})
	runner.emitLog(LogEntry{Timestamp: time.Now(), Stream: "stderr", Text: strings.Repeat("好", projectRunLogMaxTextBytes)})

	logs := runner.logBuf.Recent(2)
	if len(logs) != 2 || logs[0].ID != 1 || logs[1].ID != 2 {
		t.Fatalf("expected ordered log IDs 1 and 2, got %#v", logs)
	}
	if len(logs[1].Text) > projectRunLogMaxTextBytes || !strings.HasSuffix(logs[1].Text, "... [truncated]") {
		t.Fatalf("log text was not bounded: bytes=%d text=%q", len(logs[1].Text), logs[1].Text)
	}
}

func TestBroadcastRunLogDoesNotBlockOnFullSubscriberQueue(t *testing.T) {
	server := newTestServer(t)
	sub := &runLogSubscriber{send: make(chan LogEntry, 1)}
	sub.send <- LogEntry{ID: 1}
	server.runLogSubMu.Lock()
	server.runLogSubscribers["project"] = map[*websocket.Conn]*runLogSubscriber{nil: sub}
	server.runLogSubMu.Unlock()

	done := make(chan struct{})
	go func() {
		server.broadcastRunLog("project", LogEntry{ID: 2})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("broadcast blocked on a full subscriber queue")
	}
	server.runLogSubMu.Lock()
	_, registered := server.runLogSubscribers["project"][nil]
	server.runLogSubMu.Unlock()
	if registered {
		t.Fatal("slow subscriber should be removed")
	}
}

func TestConversationEventDoesNotBlockOnFullSubscriberQueue(t *testing.T) {
	server := newTestServer(t)
	sub := &subscriber{send: make(chan []byte, 1)}
	sub.send <- []byte("already queued")
	server.mu.Lock()
	server.subscribers["conversation"] = map[*websocket.Conn]*subscriber{nil: sub}
	server.mu.Unlock()

	done := make(chan struct{})
	go func() {
		server.enqueueConversationEvent("conversation", []byte("next event"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("conversation broadcast blocked on a full subscriber queue")
	}
	server.mu.Lock()
	_, registered := server.subscribers["conversation"][nil]
	server.mu.Unlock()
	if registered {
		t.Fatal("slow conversation subscriber should be removed")
	}
	<-sub.send // The pre-existing buffered event remains readable after close.
	select {
	case _, ok := <-sub.send:
		if ok {
			t.Fatal("slow conversation subscriber queue should be closed")
		}
	case <-time.After(time.Second):
		t.Fatal("slow conversation subscriber queue was not closed")
	}
}

func TestRunLogSubscriptionReplaysHistoryBeforeLiveEntries(t *testing.T) {
	server := newTestServer(t)
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), time.Now().UTC()); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	runner := newProjectRunner("project", "", "", nil, func(entry LogEntry) { server.broadcastRunLog("project", entry) })
	server.runManagersMu.Lock()
	server.runManagers["project"] = runner
	server.runManagersMu.Unlock()
	runner.emitLog(LogEntry{Timestamp: time.Now(), Stream: "stdout", Text: "history"})

	sub := &runLogSubscriber{send: make(chan LogEntry, runLogSubscriberQueueSize)}
	if !server.registerRunLogSubscriber(context.Background(), "project", sub) {
		t.Fatal("register run log subscriber")
	}
	runner.emitLog(LogEntry{Timestamp: time.Now(), Stream: "stdout", Text: "live"})
	first := <-sub.send
	second := <-sub.send
	sub.close()
	if first.ID != 1 || second.ID != 2 || first.Text != "history" || second.Text != "live" {
		t.Fatalf("unexpected subscription order: %#v, %#v", first, second)
	}
}

func TestProjectRunnerRetireStopsAndRejectsStart(t *testing.T) {
	tmpDir := t.TempDir()
	runner := newProjectRunner("retired-project", "", "sleep 3600", nil, func(LogEntry) {})
	if err := runner.Start(context.Background(), tmpDir); err != nil {
		t.Fatalf("start runner: %v", err)
	}
	if err := runner.Retire(); err != nil {
		t.Fatalf("retire runner: %v", err)
	}
	if status := runner.StatusSnapshot().Status; status != RunStatusStopped && status != RunStatusFailed {
		t.Fatalf("retired runner still active: %s", status)
	}
	if err := runner.Start(context.Background(), tmpDir); err == nil {
		t.Fatal("expected retired runner to reject a new start")
	}
}

// seedServerWithProject 创建带有一个项目的 Server 用于测试。
func seedServerWithProject(t *testing.T) (*Server, string) {
	t.Helper()
	rootPath := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	config := Config{
		DatabasePath:   dbPath,
		AllowedRoot:    rootPath,
		ClaudePath:     "echo",
		PermissionMode: "acceptEdits",
		ControlURL:     "http://127.0.0.1:8080",
	}
	ctx := context.Background()
	server, err := NewWithRunner(ctx, config, runnerFunc(func(ctx context.Context, req AgentRunRequest, sink AgentRunSink) error {
		sink.Event("result", json.RawMessage(`{"status":"completed"}`))
		return nil
	}))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	t.Cleanup(func() { server.Close() })

	projectPath := filepath.Join(rootPath, "test-project")
	os.MkdirAll(filepath.Join(projectPath, ".git"), 0755)
	body := fmt.Sprintf(`{"path":"%s","name":"test-project","runner":"wsl-local"}`, projectPath)
	req := httptest.NewRequest(http.MethodPost, "/api/projects", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	server.routes().ServeHTTP(resp, req)
	if resp.Code != http.StatusCreated && resp.Code != http.StatusOK {
		t.Fatalf("create project: %d body=%s", resp.Code, resp.Body.String())
	}
	var project Project
	json.NewDecoder(resp.Body).Decode(&project)
	return server, project.ID
}

func TestRunConfigCRUD(t *testing.T) {
	server, projectID := seedServerWithProject(t)

	// 初始状态：无配置
	resp := httptest.NewRecorder()
	server.routes().ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/run/config", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("get config: %d", resp.Code)
	}

	// 保存配置
	body := bytes.NewBufferString(`{"workDir":"","command":"npm run dev","envVars":{"PORT":"3000"}}`)
	resp = httptest.NewRecorder()
	server.routes().ServeHTTP(resp, httptest.NewRequest(http.MethodPut, "/api/projects/"+projectID+"/run/config", body))
	if resp.Code != http.StatusOK {
		t.Fatalf("put config: %d body=%s", resp.Code, resp.Body.String())
	}

	// 验证已保存
	resp = httptest.NewRecorder()
	server.routes().ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/run/config", nil))
	var cfg RunConfig
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if cfg.Command != "npm run dev" {
		t.Fatalf("expected npm run dev, got %q", cfg.Command)
	}
	if cfg.EnvVars["PORT"] != "3000" {
		t.Fatalf("expected PORT=3000, got %v", cfg.EnvVars)
	}
}

func TestRunConfigRejectsInvalidEnvironmentVariables(t *testing.T) {
	server, projectID := seedServerWithProject(t)
	for name, body := range map[string]string{
		"empty key":     `{"workDir":"","command":"npm run dev","envVars":{"":"3000"}}`,
		"equals in key": `{"workDir":"","command":"npm run dev","envVars":{"PORT=DEV":"3000"}}`,
		"NUL in value":  `{"workDir":"","command":"npm run dev","envVars":{"PORT":"3000\u0000"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			response := httptest.NewRecorder()
			server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/api/projects/"+projectID+"/run/config", bytes.NewBufferString(body)))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestRunConfigRejectsEscapingWorkDir(t *testing.T) {
	server, projectID := seedServerWithProject(t)
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/api/projects/"+projectID+"/run/config", bytes.NewBufferString(`{"workDir":"../../outside","command":"npm run dev","envVars":{}}`)))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestStreamingTimeoutMarksSessionStopping(t *testing.T) {
	server := newTestServer(t)
	session := newQueuedAgentSession()
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,permission_mode,claude_initialized,is_current,created_at) values ('conversation','project','session','running','full_control',0,1,?)`, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	if _, err := server.db.Exec(`insert into runs (id,conversation_id,status,created_at) values ('run','conversation','running',?)`, now); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	server.mu.Lock()
	server.sessions["conversation"] = &activeAgentSession{agent: session, activeRunID: "run"}
	server.mu.Unlock()

	server.finishStreamingRun("run", "conversation", &claudeTurnStallError{})

	server.mu.Lock()
	managed := server.sessions["conversation"]
	server.mu.Unlock()
	if managed == nil || !managed.stopping {
		t.Fatal("timed-out streaming session remained reusable")
	}
	if managed.activeRunID != "" {
		t.Fatalf("active run id = %q, want empty", managed.activeRunID)
	}
	var status string
	if err := server.db.QueryRow(`select status from runs where id='run'`).Scan(&status); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	if status != "failed" {
		t.Fatalf("run status = %q, want failed", status)
	}
}

func TestProjectRunEndpointsRejectMissingProject(t *testing.T) {
	server := newTestServer(t)
	for _, test := range []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodGet, path: "/api/projects/missing/run/config"},
		{method: http.MethodPut, path: "/api/projects/missing/run/config", body: `{"command":"echo test"}`},
		{method: http.MethodGet, path: "/api/projects/missing/run/status"},
		{method: http.MethodPost, path: "/api/projects/missing/run/stop"},
	} {
		response := httptest.NewRecorder()
		server.routes().ServeHTTP(response, httptest.NewRequest(test.method, test.path, strings.NewReader(test.body)))
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s %s: expected 404, got %d body=%s", test.method, test.path, response.Code, response.Body.String())
		}
	}
}

func TestStartStopStatusAPI(t *testing.T) {
	server, projectID := seedServerWithProject(t)

	// 先保存配置
	cfgBody := bytes.NewBufferString(`{"workDir":"","command":"echo hello && sleep 3600","envVars":{}}`)
	resp := httptest.NewRecorder()
	server.routes().ServeHTTP(resp, httptest.NewRequest(http.MethodPut, "/api/projects/"+projectID+"/run/config", cfgBody))
	if resp.Code != http.StatusOK {
		t.Fatalf("put config: %d", resp.Code)
	}

	// 启动
	resp = httptest.NewRecorder()
	server.routes().ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/run/start", nil))
	if resp.Code != http.StatusAccepted {
		t.Fatalf("start: %d body=%s", resp.Code, resp.Body.String())
	}

	time.Sleep(300 * time.Millisecond)

	// 查询状态
	resp = httptest.NewRecorder()
	server.routes().ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/run/status", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("status: %d", resp.Code)
	}
	var st RunStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if st.Status != RunStatusRunning {
		t.Fatalf("expected running, got %s", st.Status)
	}

	// 停止
	resp = httptest.NewRecorder()
	server.routes().ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/run/stop", nil))
	if resp.Code != http.StatusAccepted {
		t.Fatalf("stop: %d body=%s", resp.Code, resp.Body.String())
	}

	time.Sleep(300 * time.Millisecond)

	// 查询状态
	resp = httptest.NewRecorder()
	server.routes().ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/run/status", nil))
	json.NewDecoder(resp.Body).Decode(&st)
	if st.Status != RunStatusStopped {
		t.Fatalf("expected stopped after manual stop, got %s", st.Status)
	}
}

func TestProjectRunStatusUsesNullForUnavailableLifecycleFields(t *testing.T) {
	server, projectID := seedServerWithProject(t)
	handler := server.routes()

	initial := httptest.NewRecorder()
	handler.ServeHTTP(initial, httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/run/status", nil))
	var initialStatus map[string]any
	if err := json.NewDecoder(initial.Body).Decode(&initialStatus); err != nil {
		t.Fatalf("decode initial status: %v", err)
	}
	for _, field := range []string{"startedAt", "pid", "exitCode"} {
		if initialStatus[field] != nil {
			t.Fatalf("initial %s=%v, want null", field, initialStatus[field])
		}
	}

	config := bytes.NewBufferString(`{"workDir":"","command":"sleep 3600","envVars":{}}`)
	saved := httptest.NewRecorder()
	handler.ServeHTTP(saved, httptest.NewRequest(http.MethodPut, "/api/projects/"+projectID+"/run/config", config))
	if saved.Code != http.StatusOK {
		t.Fatalf("save config: %d body=%s", saved.Code, saved.Body.String())
	}
	started := httptest.NewRecorder()
	handler.ServeHTTP(started, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/run/start", nil))
	if started.Code != http.StatusAccepted {
		t.Fatalf("start: %d body=%s", started.Code, started.Body.String())
	}

	status := httptest.NewRecorder()
	handler.ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/run/status", nil))
	var running RunStatusResponse
	if err := json.NewDecoder(status.Body).Decode(&running); err != nil {
		t.Fatalf("decode running status: %v", err)
	}
	if running.StartedAt == nil || running.PID == nil || running.ExitCode != nil {
		t.Fatalf("running status=%#v, want startedAt and pid only", running)
	}

	stopped := httptest.NewRecorder()
	handler.ServeHTTP(stopped, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/run/stop", nil))
	if stopped.Code != http.StatusAccepted {
		t.Fatalf("stop: %d body=%s", stopped.Code, stopped.Body.String())
	}
	final := httptest.NewRecorder()
	handler.ServeHTTP(final, httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/run/status", nil))
	var finished RunStatusResponse
	if err := json.NewDecoder(final.Body).Decode(&finished); err != nil {
		t.Fatalf("decode finished status: %v", err)
	}
	if finished.StartedAt == nil || finished.PID != nil || finished.ExitCode == nil {
		t.Fatalf("finished status=%#v, want startedAt and exitCode only", finished)
	}
}

func TestDeleteProjectStopsManagedProcess(t *testing.T) {
	server, projectID := seedServerWithProject(t)
	config := bytes.NewBufferString(`{"workDir":"","command":"sleep 3600","envVars":{}}`)
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/api/projects/"+projectID+"/run/config", config))
	if response.Code != http.StatusOK {
		t.Fatalf("save run config: %d body=%s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/run/start", nil))
	if response.Code != http.StatusAccepted {
		t.Fatalf("start project process: %d body=%s", response.Code, response.Body.String())
	}
	server.runManagersMu.RLock()
	runner := server.runManagers[projectID]
	server.runManagersMu.RUnlock()
	if runner == nil {
		t.Fatal("project runner was not registered")
	}
	response = httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/projects/"+projectID, nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete project: %d body=%s", response.Code, response.Body.String())
	}
	// Project deletion returns immediately (204); the managed process is retired
	// in the background, so wait for it to settle within a bounded window.
	deadline := time.Now().Add(10 * time.Second)
	for {
		status := runner.StatusSnapshot().Status
		if status == RunStatusStopped || status == RunStatusFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("project process still active after deletion: %s", status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	server.runManagersMu.RLock()
	_, remains := server.runManagers[projectID]
	server.runManagersMu.RUnlock()
	if remains {
		t.Fatal("deleted project still has a registered runner")
	}
}

func TestProjectRunManagerRejectsDeletedProject(t *testing.T) {
	server, projectID := seedServerWithProject(t)
	if _, err := server.db.Exec(`delete from projects where id=?`, projectID); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	if _, err := server.projectRunManagerForExistingProject(context.Background(), projectID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected deleted project to be rejected, got %v", err)
	}
	server.runManagersMu.RLock()
	_, registered := server.runManagers[projectID]
	server.runManagersMu.RUnlock()
	if registered {
		t.Fatal("deleted project created a run manager")
	}
}

func TestListProjectsKeepsRemotePresentation(t *testing.T) {
	server := newTestServer(t)
	server.runnerRegistry.register("ssh-remote", runnerFunc(func(context.Context, AgentRunRequest, AgentRunSink) error { return nil }), RunnerMeta{ID: "ssh-remote", Name: "build-host", Environment: "remote-linux"})
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('remote-project','remote-project','/srv/apps/api','ssh-remote','main',1,?)`, now); err != nil {
		t.Fatalf("insert remote project: %v", err)
	}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/projects", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("list projects: %d body=%s", response.Code, response.Body.String())
	}
	var projects []Project
	if err := json.NewDecoder(response.Body).Decode(&projects); err != nil {
		t.Fatalf("decode projects: %v", err)
	}
	if len(projects) != 1 || projects[0].PathDisplay != "build-host:/srv/apps/api" || projects[0].FullPath != "build-host:/srv/apps/api" || projects[0].Environment != "remote-linux" {
		t.Fatalf("unexpected remote project presentation: %#v", projects)
	}
}

func TestListProjectsExposesFullPathForLocalProjects(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('local-project','local-project','/home/dev/apps/my-service','wsl-local','main',1,?)`, now); err != nil {
		t.Fatalf("insert local project: %v", err)
	}
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/projects", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("list projects: %d body=%s", response.Code, response.Body.String())
	}
	var projects []Project
	if err := json.NewDecoder(response.Body).Decode(&projects); err != nil {
		t.Fatalf("decode projects: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("expected 1 project, got %#v", projects)
	}
	// 卡片正文展示简短目录名，FullPath 提供完整路径供悬停/查看
	if projects[0].PathDisplay != "my-service" || projects[0].FullPath != "/home/dev/apps/my-service" {
		t.Fatalf("unexpected local project presentation: %#v", projects[0])
	}
}

func TestProjectsAllowSamePathOnDifferentRunners(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('local','local','/srv/shared/app','wsl-local','main',1,?)`, now); err != nil {
		t.Fatalf("insert local project: %v", err)
	}
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('remote','remote','/srv/shared/app','ssh-remote','main',1,?)`, now); err != nil {
		t.Fatalf("insert same path on remote runner: %v", err)
	}
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('duplicate','duplicate','/srv/shared/app','ssh-remote','main',1,?)`, now); err == nil {
		t.Fatal("expected duplicate path on the same runner to be rejected")
	}
}

func TestCreateProjectDuplicateUsesSameRunner(t *testing.T) {
	server, projectID := seedServerWithProject(t)
	var path string
	if err := server.db.QueryRow(`select path from projects where id=?`, projectID).Scan(&path); err != nil {
		t.Fatalf("read project path: %v", err)
	}
	if _, err := server.db.Exec(`delete from projects where id=?`, projectID); err != nil {
		t.Fatalf("remove seeded project: %v", err)
	}
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('remote-first','remote',?,'ssh-remote','main',1,?)`, path, time.Now().UTC()); err != nil {
		t.Fatalf("insert remote project: %v", err)
	}

	body := fmt.Sprintf(`{"path":%q,"name":"local","runner":"wsl-local"}`, path)
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects", bytes.NewBufferString(body)))
	if response.Code != http.StatusCreated {
		t.Fatalf("create local project: %d body=%s", response.Code, response.Body.String())
	}
	var local Project
	if err := json.NewDecoder(response.Body).Decode(&local); err != nil {
		t.Fatalf("decode local project: %v", err)
	}

	response = httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects", bytes.NewBufferString(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("repeat local project: %d body=%s", response.Code, response.Body.String())
	}
	var existing Project
	if err := json.NewDecoder(response.Body).Decode(&existing); err != nil {
		t.Fatalf("decode existing project: %v", err)
	}
	if existing.ID != local.ID || existing.Runner != "wsl-local" {
		t.Fatalf("expected existing local project, got %#v", existing)
	}
}

func TestProjectGitCreateAndSwitchBranch(t *testing.T) {
	server := newTestServer(t)
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "readme.txt", "one\n")
	runGitForTest(t, repo, "add", "readme.txt")
	runGitForTest(t, repo, "commit", "-m", "initial commit")
	seedGitProjectForTest(t, server, "git-project", repo)

	summary := gitSummaryForTest(t, server, "git-project")
	if summary.Head.Branch != "main" {
		t.Fatalf("expected main branch, got %s", summary.Head.Branch)
	}

	create := httptest.NewRecorder()
	server.routes().ServeHTTP(create, httptest.NewRequest(http.MethodPost, "/api/projects/git-project/git/branches", bytes.NewBufferString(`{"name":"feature","startPoint":"main"}`)))
	if create.Code != http.StatusAccepted {
		t.Fatalf("create branch status=%d body=%s", create.Code, create.Body.String())
	}

	branches := httptest.NewRecorder()
	server.routes().ServeHTTP(branches, httptest.NewRequest(http.MethodGet, "/api/projects/git-project/git/branches", nil))
	if branches.Code != http.StatusOK {
		t.Fatalf("list branches status=%d body=%s", branches.Code, branches.Body.String())
	}
	var branchList []GitBranch
	if err := json.Unmarshal(branches.Body.Bytes(), &branchList); err != nil {
		t.Fatalf("decode branches: %v", err)
	}
	hasFeature := false
	for _, b := range branchList {
		if b.Name == "feature" {
			hasFeature = true
			break
		}
	}
	if !hasFeature {
		t.Fatalf("feature branch not found in %#v", branchList)
	}

	afterSummary := gitSummaryForTest(t, server, "git-project")
	if afterSummary.Head.Branch != "main" {
		t.Fatalf("expected still on main branch after create, got %s", afterSummary.Head.Branch)
	}
}

func TestProjectGitSwitchBranchRejectsDirtyWorktree(t *testing.T) {
	server := newTestServer(t)
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "readme.txt", "one\n")
	runGitForTest(t, repo, "add", "readme.txt")
	runGitForTest(t, repo, "commit", "-m", "initial commit")
	runGitForTest(t, repo, "switch", "-c", "other-branch")
	runGitForTest(t, repo, "switch", "main")
	writeGitTestFile(t, repo, "readme.txt", "two\n")
	seedGitProjectForTest(t, server, "git-project", repo)

	summary := gitSummaryForTest(t, server, "git-project")
	if summary.Worktree.Modified == 0 {
		t.Fatal("expected dirty worktree for this test")
	}

	switchResp := httptest.NewRecorder()
	server.routes().ServeHTTP(switchResp, httptest.NewRequest(http.MethodPost, "/api/projects/git-project/git/switch", bytes.NewBufferString(`{"branch":"other-branch","stateToken":"`+summary.StateToken+`"}`)))
	if switchResp.Code != http.StatusConflict {
		t.Fatalf("expected conflict for dirty worktree, got status=%d body=%s", switchResp.Code, switchResp.Body.String())
	}
	if !strings.Contains(switchResp.Body.String(), "commit or discard") && !strings.Contains(switchResp.Body.String(), "冲突") {
		t.Fatalf("expected dirty worktree rejection, got %s", switchResp.Body.String())
	}
}

func TestProjectGitSwitchBranchSucceedsOnCleanWorktree(t *testing.T) {
	server := newTestServer(t)
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "readme.txt", "one\n")
	runGitForTest(t, repo, "add", "readme.txt")
	runGitForTest(t, repo, "commit", "-m", "initial commit")
	runGitForTest(t, repo, "switch", "-c", "feature")
	writeGitTestFile(t, repo, "feature.txt", "new\n")
	runGitForTest(t, repo, "add", "feature.txt")
	runGitForTest(t, repo, "commit", "-m", "feature commit")
	seedGitProjectForTest(t, server, "git-project", repo)

	summary := gitSummaryForTest(t, server, "git-project")

	switchResp := httptest.NewRecorder()
	server.routes().ServeHTTP(switchResp, httptest.NewRequest(http.MethodPost, "/api/projects/git-project/git/switch", bytes.NewBufferString(`{"branch":"main","stateToken":"`+summary.StateToken+`"}`)))
	if switchResp.Code != http.StatusAccepted {
		t.Fatalf("clean switch status=%d body=%s", switchResp.Code, switchResp.Body.String())
	}

	afterSummary := gitSummaryForTest(t, server, "git-project")
	if afterSummary.Head.Branch != "main" {
		t.Fatalf("expected main after switch, got %s", afterSummary.Head.Branch)
	}
}

func TestProjectGitFetchAndPush(t *testing.T) {
	server := newTestServer(t)
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "readme.txt", "one\n")
	runGitForTest(t, repo, "add", "readme.txt")
	runGitForTest(t, repo, "commit", "-m", "initial commit")

	upstream := t.TempDir()
	runGitForTest(t, upstream, "init", "--bare", "-b", "main")
	runGitForTest(t, repo, "remote", "add", "origin", upstream)

	seedGitProjectForTest(t, server, "git-project", repo)

	summary := gitSummaryForTest(t, server, "git-project")

	fetchResp := httptest.NewRecorder()
	server.routes().ServeHTTP(fetchResp, httptest.NewRequest(http.MethodPost, "/api/projects/git-project/git/fetch", bytes.NewBufferString(`{"remote":"origin","stateToken":"`+summary.StateToken+`"}`)))
	if fetchResp.Code != http.StatusAccepted {
		t.Fatalf("fetch status=%d body=%s", fetchResp.Code, fetchResp.Body.String())
	}

	summary = gitSummaryForTest(t, server, "git-project")

	pushResp := httptest.NewRecorder()
	server.routes().ServeHTTP(pushResp, httptest.NewRequest(http.MethodPost, "/api/projects/git-project/git/push", bytes.NewBufferString(`{"remote":"origin","branch":"main","stateToken":"`+summary.StateToken+`"}`)))
	if pushResp.Code != http.StatusAccepted {
		t.Fatalf("push status=%d body=%s", pushResp.Code, pushResp.Body.String())
	}
}

func TestProjectGitPushRejectsNonFastForward(t *testing.T) {
	server := newTestServer(t)
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "readme.txt", "one\n")
	runGitForTest(t, repo, "add", "readme.txt")
	runGitForTest(t, repo, "commit", "-m", "initial commit")

	upstream := t.TempDir()
	runGitForTest(t, upstream, "init", "--bare", "-b", "main")
	runGitForTest(t, repo, "remote", "add", "origin", upstream)
	runGitForTest(t, repo, "push", "origin", "main")

	otherRepo := t.TempDir()
	runGitForTest(t, otherRepo, "clone", upstream, ".")
	writeGitTestFile(t, otherRepo, "other.txt", "from other\n")
	runGitForTest(t, otherRepo, "add", "other.txt")
	runGitForTest(t, otherRepo, "commit", "-m", "commit from other")
	runGitForTest(t, otherRepo, "push", "origin", "main")

	writeGitTestFile(t, repo, "readme.txt", "two\n")
	runGitForTest(t, repo, "add", "readme.txt")
	runGitForTest(t, repo, "commit", "-m", "conflicting commit")

	seedGitProjectForTest(t, server, "git-project", repo)
	summary := gitSummaryForTest(t, server, "git-project")

	pushResp := httptest.NewRecorder()
	server.routes().ServeHTTP(pushResp, httptest.NewRequest(http.MethodPost, "/api/projects/git-project/git/push", bytes.NewBufferString(`{"remote":"origin","branch":"main","stateToken":"`+summary.StateToken+`"}`)))
	if pushResp.Code != http.StatusAccepted {
		t.Fatalf("push response status=%d body=%s", pushResp.Code, pushResp.Body.String())
	}
	var pushResult gitOperationResult
	if err := json.Unmarshal(pushResp.Body.Bytes(), &pushResult); err != nil {
		t.Fatalf("decode push result: %v", err)
	}
	if pushResult.Status != "failed" {
		t.Fatalf("expected push rejection, got status=%s", pushResult.Status)
	}
}

func TestProjectGitCreateBranchValidatesName(t *testing.T) {
	server := newTestServer(t)
	repo := newTempGitRepository(t)
	writeGitTestFile(t, repo, "readme.txt", "one\n")
	runGitForTest(t, repo, "add", "readme.txt")
	runGitForTest(t, repo, "commit", "-m", "initial commit")
	seedGitProjectForTest(t, server, "git-project", repo)

	for _, name := range []string{"", "bad/name~", "-bad"} {
		create := httptest.NewRecorder()
		server.routes().ServeHTTP(create, httptest.NewRequest(http.MethodPost, "/api/projects/git-project/git/branches", bytes.NewBufferString(`{"name":"`+name+`"}`)))
		if create.Code != http.StatusBadRequest {
			t.Logf("name=%q status=%d body=%s", name, create.Code, create.Body.String())
		}
	}
}

// codexCapableStubRunner is a test double for an SSH runner that also runs
// Codex on the remote host. It implements both AgentRunner and CodexCapableRunner.
type codexCapableStubRunner struct {
	runnerFunc
	codexReady   bool
	codexVersion string
}

func (r codexCapableStubRunner) CodexReady(context.Context) bool     { return r.codexReady }
func (r codexCapableStubRunner) CodexVersion(context.Context) string { return r.codexVersion }
func (r codexCapableStubRunner) CodexCheckUpdate(context.Context) (bool, string, error) {
	return false, r.codexVersion, nil
}
func (r codexCapableStubRunner) CodexUpdate(context.Context) (string, string, error) {
	return r.codexVersion, r.codexVersion, nil
}

func TestListRunnersReportsCodexOnSSHCodexCapableRunner(t *testing.T) {
	server := newTestServer(t)
	stub := codexCapableStubRunner{
		runnerFunc:   runnerFunc(func(context.Context, AgentRunRequest, AgentRunSink) error { return nil }),
		codexReady:   true,
		codexVersion: "0.146.0",
	}
	server.runnerRegistry.register("ssh-codex", stub, RunnerMeta{ID: "ssh-codex", Name: "tencent-host", Environment: "remote-linux"})

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/runners", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("list runners: %d body=%s", response.Code, response.Body.String())
	}
	var raw []map[string]any
	if err := json.NewDecoder(response.Body).Decode(&raw); err != nil {
		t.Fatalf("decode runners: %v", err)
	}
	for _, entry := range raw {
		if entry["id"] != "ssh-codex" {
			continue
		}
		codex, ok := entry["codex"].(map[string]any)
		if !ok || codex["status"] != "ready" {
			t.Fatalf("expected ssh-codex codex ready, got %#v", entry["codex"])
		}
		if codex["version"] != "0.146.0" {
			t.Fatalf("expected codex version 0.146.0, got %v", codex["version"])
		}
		return
	}
	t.Fatal("ssh-codex runner not found in list")
}

func TestListRunnersReportsCodexUnavailableOnNonCodexRunner(t *testing.T) {
	server := newTestServer(t)
	server.runnerRegistry.register("ssh-plain", runnerFunc(func(context.Context, AgentRunRequest, AgentRunSink) error { return nil }), RunnerMeta{ID: "ssh-plain", Name: "plain-host", Environment: "remote-linux"})

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/runners", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("list runners: %d body=%s", response.Code, response.Body.String())
	}
	var raw []map[string]any
	if err := json.NewDecoder(response.Body).Decode(&raw); err != nil {
		t.Fatalf("decode runners: %v", err)
	}
	for _, entry := range raw {
		if entry["id"] != "ssh-plain" {
			continue
		}
		codex, _ := entry["codex"].(map[string]any)
		if codex["status"] != "unavailable" {
			t.Fatalf("expected ssh-plain codex unavailable, got %#v", codex)
		}
		return
	}
	t.Fatal("ssh-plain runner not found in list")
}

// TestListProjectInputHistory 验证项目级输入历史跨对话聚合，并压缩连续重复项。
func TestListProjectInputHistory(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	// 两个对话同属一个项目
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,claude_initialized,is_current,created_at) values ('c1','project','00000000-0000-4000-8000-000000000000','idle',1,1,?)`, now); err != nil {
		t.Fatalf("insert conversation c1: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,status,claude_initialized,is_current,created_at) values ('c2','project','00000000-0000-4000-8000-000000000001','idle',1,0,?)`, now); err != nil {
		t.Fatalf("insert conversation c2: %v", err)
	}
	// 依次插入用户消息：a, a, b, a（跨两个对话），连续的 a,a 应压缩为一条
	base := now
	inserts := []struct {
		conversation string
		content      string
	}{
		{"c1", "a"}, {"c1", "a"}, {"c1", "b"},
		{"c2", "b"}, {"c2", "c"},
	}
	for index, item := range inserts {
		at := base.Add(time.Duration(index) * time.Second)
		if _, err := server.db.Exec(`insert into messages (id,conversation_id,role,content,created_at) values (?,?,?,?,?)`, fmt.Sprintf("m%d", index), item.conversation, "user", item.content, at); err != nil {
			t.Fatalf("insert message %d: %v", index, err)
		}
	}

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/projects/project/input-history?limit=100", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var history []string
	if err := json.NewDecoder(response.Body).Decode(&history); err != nil {
		t.Fatalf("decode history: %v", err)
	}
	// 时间线：a, a, b, b, c
	// 同对话连续 a 被压缩；跨对话连续 b（c1 末尾 + c2 开头）也被压缩
	// 期望：a, b, c
	want := []string{"a", "b", "c"}
	if len(history) != len(want) {
		t.Fatalf("history=%v want=%v", history, want)
	}
	for index, value := range want {
		if history[index] != value {
			t.Fatalf("history=%v want=%v", history, want)
		}
	}
}
