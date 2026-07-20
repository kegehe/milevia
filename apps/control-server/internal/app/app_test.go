package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type runnerFunc func(context.Context, AgentRunRequest, AgentRunSink) error

func (fn runnerFunc) Run(ctx context.Context, request AgentRunRequest, sink AgentRunSink) error {
	return fn(ctx, request, sink)
}

func (runnerFunc) Ready(context.Context) bool { return true }

type idleAgentSession struct {
	done chan error
	once sync.Once
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
	initialized int
}

func (sink *recordingSink) Event(eventType string, _ json.RawMessage) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.events = append(sink.events, eventType)
}

func (sink *recordingSink) AssistantText(content string) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.texts = append(sink.texts, content)
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
func (sink *turnRecordingSink) AssistantText(string)          {}
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
	mainStep := json.RawMessage(`{"type":"assistant","message":{"id":"main-step","model":"claude-sonnet","usage":{"input_tokens":48000},"content":[{"type":"tool_use","id":"tool-1"}]}}`)
	server.collectUsageEvent("run", "conversation", "assistant", mainStep)
	server.collectUsageEvent("run", "conversation", "assistant", mainStep)
	subagentStep := json.RawMessage(`{"type":"assistant","parent_tool_use_id":"tool-1","message":{"id":"subagent-step","model":"claude-haiku","usage":{"input_tokens":1200},"content":[{"type":"tool_use","id":"tool-2"}]}}`)
	server.collectUsageEvent("run", "conversation", "assistant", subagentStep)
	server.collectUsageEvent("run", "conversation", "assistant", subagentStep)
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
	if usage.Context.ContextInputTokens != 48000 || usage.Context.ContextWindow != 200000 || usage.Context.EstimatedCostUSD != 0.1234 || len(usage.Models) != 2 {
		t.Fatalf("unexpected context or models: %#v models=%#v", usage.Context, usage.Models)
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
	handler.ServeHTTP(preflight, request)
	if preflight.Code != http.StatusNoContent || preflight.Header().Get("Access-Control-Allow-Methods") != "GET, POST, PATCH, DELETE, OPTIONS" {
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
	if err != nil || !strings.Contains(content, "请在当前项目中执行以下命令") || !strings.Contains(content, "pnpm test") {
		t.Fatalf("command shortcut render failed: content=%q err=%v", content, err)
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

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/projects/project/conversations", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("list conversation history status: %d body=%s", response.Code, response.Body.String())
	}
	var history []Conversation
	if err := json.NewDecoder(response.Body).Decode(&history); err != nil {
		t.Fatalf("decode conversation history: %v", err)
	}
	if len(history) != 3 || history[0].ID != "current" || history[1].ID != "recent" || history[1].Preview != "最近会话最后回复" {
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
	if !columns["title"] || !columns["last_activity_at"] {
		t.Fatalf("history columns missing after migration: %#v", columns)
	}
	var title string
	if err := server.db.QueryRow(`select title from conversations where id='conversation'`).Scan(&title); err != nil {
		t.Fatalf("read migrated title: %v", err)
	}
	if title != "修复历史会话标题" {
		t.Fatalf("unexpected migrated title: %q", title)
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
		if _, err := server.db.Exec(`insert into messages (id,conversation_id,role,content,created_at) values (?,?,?,?,?)`, fmt.Sprintf("user-%d", index), "conversation", "user", fmt.Sprintf("prompt-%d", index), createdAt); err != nil {
			t.Fatalf("insert user message %d: %v", index, err)
		}
	}
	if _, err := server.db.Exec(`insert into messages (id,conversation_id,role,content,created_at) values ('assistant','conversation','assistant','ignore me',?)`, now.Add(103*time.Second)); err != nil {
		t.Fatalf("insert assistant message: %v", err)
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
	script := "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\"}'\nIFS= read -r line\nwhile :; do sleep 1; done\n"
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
	script := "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\"}'\nprintf '%s\\n' '{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"runner output\"}]}}'\n"
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
	if sink.initialized != 1 || len(sink.texts) != 1 || sink.texts[0] != "runner output" || !containsArgument(sink.events, "system") || !containsArgument(sink.events, "assistant") {
		t.Fatalf("unexpected Claude stream: initialized=%d texts=%q events=%q", sink.initialized, sink.texts, sink.events)
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
		sink.AssistantText("ready")
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
	config := server.config
	server.Close()
	recovered, err := New(context.Background(), config)
	if err != nil {
		t.Fatalf("restart server: %v", err)
	}
	t.Cleanup(recovered.Close)
	var runningStatus, queuedStatus, conversationStatus, eventType, shortcutStatus string
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
	if runningStatus != "interrupted" || queuedStatus != "interrupted" || shortcutStatus != "interrupted" || conversationStatus != "idle" || eventType != "run.interrupted" || interruptedEvents != 2 {
		t.Fatalf("unexpected recovery state: running=%q queued=%q shortcut=%q conversation=%q event=%q events=%d", runningStatus, queuedStatus, shortcutStatus, conversationStatus, eventType, interruptedEvents)
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
