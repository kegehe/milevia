package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type claudeTurnTestSink struct {
	mu       sync.Mutex
	finished []error
}

type claudeOutputTestSink struct {
	events []json.RawMessage
	texts  []string
}

func (sink *claudeOutputTestSink) Event(_ string, payload json.RawMessage) {
	sink.events = append(sink.events, append(json.RawMessage(nil), payload...))
}
func (sink *claudeOutputTestSink) AssistantText(text, _ string) {
	sink.texts = append(sink.texts, text)
}
func (*claudeOutputTestSink) SessionIdentified(string) {}
func (*claudeOutputTestSink) SessionInitialized()      {}

func TestIndependentReviewClaudeRequestUsesExecutableReadOnlyMode(t *testing.T) {
	tools := orchestrationReviewReadOnlyTools("claude-code")
	if len(tools) == 0 {
		t.Fatal("Claude independent review must restrict tools")
	}
	runner := &claudeCLIRunner{config: Config{PermissionMode: "acceptEdits"}}
	args, err := runner.args(AgentRunRequest{
		SessionID:      "review-session",
		Prompt:         "review prompt",
		PermissionMode: "read_only",
		SkipSessionID:  true,
		ReadOnlyTools:  tools,
		PromptViaStdin: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--permission-mode default") || !strings.Contains(joined, "--allowedTools") {
		t.Fatalf("independent review args do not use executable read-only mode: %q", args)
	}
	if strings.Contains(joined, "--permission-mode plan") || strings.Contains(joined, "--session-id") || strings.Contains(joined, "review-session") || strings.Contains(joined, "review prompt") {
		t.Fatalf("independent review args still use waiting/session mode: %q", args)
	}
}

func TestClaudeOutputRedactsCredentialsBeforeEmitting(t *testing.T) {
	const secret = "sk-claude-test-secret-value-12345"
	runner := &claudeCLIRunner{}
	sink := &claudeOutputTestSink{}
	runner.readOutput(strings.NewReader(`{"type":"assistant","api_key":"`+secret+`","message":{"content":[{"type":"text","text":"Authorization: Bearer `+secret+`"}]}}`+"\n"), sink)
	runner.readStderr(strings.NewReader("OPENAI_API_KEY="+secret+"\n"), sink)
	for _, payload := range sink.events {
		if strings.Contains(string(payload), secret) {
			t.Fatalf("credential leaked in event: %s", payload)
		}
	}
	for _, text := range sink.texts {
		if strings.Contains(text, secret) {
			t.Fatalf("credential leaked in assistant text: %s", text)
		}
	}
	if len(sink.texts) != 1 || !strings.Contains(sink.texts[0], "[REDACTED]") {
		t.Fatalf("assistant text was not redacted: %#v", sink.texts)
	}
}

func (*claudeTurnTestSink) Event(string, json.RawMessage) {}
func (*claudeTurnTestSink) AssistantText(string, string)  {}
func (*claudeTurnTestSink) SessionIdentified(string)      {}
func (*claudeTurnTestSink) SessionInitialized()           {}
func (*claudeTurnTestSink) TurnStarted()                  {}
func (sink *claudeTurnTestSink) TurnFinished(err error) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.finished = append(sink.finished, err)
}

func (sink *claudeTurnTestSink) errors() []error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]error(nil), sink.finished...)
}

func newClaudeSessionForTimerTest(timeout time.Duration) *claudeCLISession {
	return &claudeCLISession{
		cmd:                    &exec.Cmd{},
		processDone:            make(chan struct{}),
		turnIdleTimeout:        timeout,
		initialResponseTimeout: timeout,
		toolResultTimeout:      timeout,
	}
}

func newClaudeSessionForPhaseTimerTest(initial, afterToolResult, idle time.Duration) *claudeCLISession {
	return &claudeCLISession{
		cmd:                    &exec.Cmd{},
		processDone:            make(chan struct{}),
		turnIdleTimeout:        idle,
		initialResponseTimeout: initial,
		toolResultTimeout:      afterToolResult,
	}
}

func startTimerTestTurn(session *claudeCLISession, turn *claudeSessionTurn, queued ...*claudeSessionTurn) {
	session.mu.Lock()
	session.current = turn
	session.queued = queued
	turn.waitPhase = claudeTurnWaitingInitialResponse
	turn.lastEvent = "user_prompt"
	session.startTurnTimerLocked(turn)
	session.mu.Unlock()
}

func waitForTurnErrors(t *testing.T, sink *claudeTurnTestSink, count int) []error {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if errors := sink.errors(); len(errors) == count {
			return errors
		}
		time.Sleep(5 * time.Millisecond)
	}
	return sink.errors()
}

func TestClaudeSessionFailsStalledCurrentAndQueuedTurns(t *testing.T) {
	session := newClaudeSessionForTimerTest(20 * time.Millisecond)
	firstSink, secondSink := &claudeTurnTestSink{}, &claudeTurnTestSink{}
	first := &claudeSessionTurn{sink: firstSink}
	second := &claudeSessionTurn{sink: secondSink}
	startTimerTestTurn(session, first, second)

	firstErrors := waitForTurnErrors(t, firstSink, 1)
	secondErrors := waitForTurnErrors(t, secondSink, 1)
	if len(firstErrors) != 1 || !errors.Is(firstErrors[0], errClaudeTurnIdleTimeout) {
		t.Fatalf("first turn errors=%v", firstErrors)
	}
	if len(secondErrors) != 1 || !errors.Is(secondErrors[0], errClaudeTurnIdleTimeout) {
		t.Fatalf("queued turn errors=%v", secondErrors)
	}
	var currentStall *claudeTurnStallError
	if !errors.As(firstErrors[0], &currentStall) {
		t.Fatalf("current error=%T, want *claudeTurnStallError", firstErrors[0])
	}
	var queuedStall *claudeQueuedTurnCancelledError
	if !errors.As(secondErrors[0], &queuedStall) {
		t.Fatalf("queued error=%T, want *claudeQueuedTurnCancelledError", secondErrors[0])
	}

	session.mu.Lock()
	defer session.mu.Unlock()
	if !session.stopped || session.current != nil || len(session.queued) != 0 {
		t.Fatalf("session was not fully stopped: stopped=%v current=%v queued=%d", session.stopped, session.current, len(session.queued))
	}
}

func TestClaudeSessionOutputExtendsTurnIdleTimeout(t *testing.T) {
	session := newClaudeSessionForTimerTest(75 * time.Millisecond)
	sink := &claudeTurnTestSink{}
	turn := &claudeSessionTurn{sink: sink}
	startTimerTestTurn(session, turn)

	time.Sleep(45 * time.Millisecond)
	session.noteStreamEvent("assistant", json.RawMessage(`[{"type":"text","text":"仍在处理"}]`))
	time.Sleep(45 * time.Millisecond)
	if errors := sink.errors(); len(errors) != 0 {
		t.Fatalf("turn timed out despite recent output: %v", errors)
	}
	turnErrors := waitForTurnErrors(t, sink, 1)
	if len(turnErrors) != 1 || !errors.Is(turnErrors[0], errClaudeTurnIdleTimeout) {
		t.Fatalf("turn errors=%v", turnErrors)
	}
}

func TestClaudeSessionToolResultUsesShortContinuationTimeout(t *testing.T) {
	session := newClaudeSessionForPhaseTimerTest(200*time.Millisecond, 20*time.Millisecond, 200*time.Millisecond)
	sink := &claudeTurnTestSink{}
	turn := &claudeSessionTurn{sink: sink}
	startTimerTestTurn(session, turn)

	session.noteStreamEvent("assistant", json.RawMessage(`[{"type":"tool_use","name":"Read"}]`))
	session.noteStreamEvent("user", json.RawMessage(`[{"type":"tool_result"}]`))

	errs := waitForTurnErrors(t, sink, 1)
	if len(errs) != 1 || !errors.Is(errs[0], errClaudeTurnIdleTimeout) {
		t.Fatalf("turn errors=%v", errs)
	}
	var stall *claudeTurnStallError
	if !errors.As(errs[0], &stall) {
		t.Fatalf("stall error=%T, want *claudeTurnStallError", errs[0])
	}
	if stall.phase != claudeTurnWaitingAfterToolResult || stall.toolName != "Read" || stall.lastEvent != "user.tool_result" {
		t.Fatalf("stall details=%+v", stall)
	}
}

func TestClaudeSessionUserReplayDoesNotExtendInitialResponseTimeout(t *testing.T) {
	session := newClaudeSessionForPhaseTimerTest(20*time.Millisecond, 200*time.Millisecond, 200*time.Millisecond)
	sink := &claudeTurnTestSink{}
	turn := &claudeSessionTurn{sink: sink}
	startTimerTestTurn(session, turn)

	time.Sleep(10 * time.Millisecond)
	session.noteStreamEvent("user", json.RawMessage(`[{"type":"text","text":"回放提示词"}]`))
	errs := waitForTurnErrors(t, sink, 1)
	var stall *claudeTurnStallError
	if len(errs) != 1 || !errors.As(errs[0], &stall) || stall.phase != claudeTurnWaitingInitialResponse {
		t.Fatalf("turn errors=%v", errs)
	}
}

func TestClaudeSessionModelContinuationRestoresLongTimeout(t *testing.T) {
	session := newClaudeSessionForPhaseTimerTest(200*time.Millisecond, 20*time.Millisecond, 75*time.Millisecond)
	sink := &claudeTurnTestSink{}
	turn := &claudeSessionTurn{sink: sink}
	startTimerTestTurn(session, turn)

	session.noteStreamEvent("assistant", json.RawMessage(`[{"type":"tool_use","name":"Read"}]`))
	session.noteStreamEvent("user", json.RawMessage(`[{"type":"tool_result"}]`))
	time.Sleep(10 * time.Millisecond)
	session.noteStreamEvent("assistant", json.RawMessage(`[{"type":"text","text":"继续处理"}]`))
	time.Sleep(45 * time.Millisecond)
	if errs := sink.errors(); len(errs) != 0 {
		t.Fatalf("model continuation did not restore the long timeout: %v", errs)
	}
	errs := waitForTurnErrors(t, sink, 1)
	var stall *claudeTurnStallError
	if len(errs) != 1 || !errors.As(errs[0], &stall) || stall.phase != claudeTurnWaitingForActivity {
		t.Fatalf("turn errors=%v", errs)
	}
}

func TestClaudeSessionToolExecutionKeepsLongTimeout(t *testing.T) {
	session := newClaudeSessionForPhaseTimerTest(20*time.Millisecond, 20*time.Millisecond, 75*time.Millisecond)
	sink := &claudeTurnTestSink{}
	turn := &claudeSessionTurn{sink: sink}
	startTimerTestTurn(session, turn)

	session.noteStreamEvent("assistant", json.RawMessage(`[{"type":"tool_use","name":"Bash"}]`))
	time.Sleep(45 * time.Millisecond)
	if errs := sink.errors(); len(errs) != 0 {
		t.Fatalf("tool execution timed out before long timeout: %v", errs)
	}
	errs := waitForTurnErrors(t, sink, 1)
	var stall *claudeTurnStallError
	if len(errs) != 1 || !errors.As(errs[0], &stall) || stall.phase != claudeTurnWaitingForToolResult || stall.toolName != "Bash" {
		t.Fatalf("turn errors=%v", errs)
	}
}

func TestClaudeSessionResultCancelsTurnIdleTimeout(t *testing.T) {
	session := newClaudeSessionForTimerTest(20 * time.Millisecond)
	sink := &claudeTurnTestSink{}
	turn := &claudeSessionTurn{sink: sink}
	startTimerTestTurn(session, turn)

	session.finishCurrent(nil)
	time.Sleep(50 * time.Millisecond)
	turnErrors := sink.errors()
	if len(turnErrors) != 1 || turnErrors[0] != nil {
		t.Fatalf("result completion errors=%v", turnErrors)
	}
}

func TestClaudeSessionResultEventCancelsTimerBeforeCompletion(t *testing.T) {
	session := newClaudeSessionForTimerTest(20 * time.Millisecond)
	sink := &claudeTurnTestSink{}
	turn := &claudeSessionTurn{sink: sink}
	startTimerTestTurn(session, turn)

	session.noteStreamEvent("result", nil)
	time.Sleep(50 * time.Millisecond)
	if errs := sink.errors(); len(errs) != 0 {
		t.Fatalf("result event allowed a timeout before completion: %v", errs)
	}
	session.finishCurrent(nil)
	if errs := sink.errors(); len(errs) != 1 || errs[0] != nil {
		t.Fatalf("result completion errors=%v", errs)
	}
}

func TestSSHAgentSessionUsesToolResultContinuationTimeout(t *testing.T) {
	session := &sshAgentSession{
		processDone:            make(chan struct{}),
		turnIdleTimeout:        200 * time.Millisecond,
		initialResponseTimeout: 200 * time.Millisecond,
		toolResultTimeout:      20 * time.Millisecond,
	}
	sink := &claudeTurnTestSink{}
	turn := &claudeSessionTurn{sink: sink}
	session.mu.Lock()
	session.current = turn
	turn.waitPhase = claudeTurnWaitingInitialResponse
	turn.lastEvent = "user_prompt"
	session.startTurnTimerLocked(turn)
	session.mu.Unlock()

	session.noteStreamEvent("assistant", json.RawMessage(`[{"type":"tool_use","name":"Read"}]`))
	session.noteStreamEvent("user", json.RawMessage(`[{"type":"tool_result"}]`))
	errs := waitForTurnErrors(t, sink, 1)
	var stall *claudeTurnStallError
	if len(errs) != 1 || !errors.As(errs[0], &stall) || stall.phase != claudeTurnWaitingAfterToolResult || stall.toolName != "Read" {
		t.Fatalf("SSH turn errors=%v", errs)
	}
}

func TestConfigFromEnvParsesClaudeTurnIdleTimeout(t *testing.T) {
	t.Setenv("AUTO_CLAUDE_TURN_IDLE_TIMEOUT", "45m")
	t.Setenv("AUTO_CLAUDE_INITIAL_RESPONSE_TIMEOUT", "6m")
	t.Setenv("AUTO_CLAUDE_TOOL_RESULT_TIMEOUT", "7m")
	t.Setenv("AUTO_AGENT_UPDATE_TIMEOUT", "16m")
	config := ConfigFromEnv()
	if got := config.ClaudeTurnIdleTimeout; got != 45*time.Minute {
		t.Fatalf("ClaudeTurnIdleTimeout=%s, want 45m", got)
	}
	if got := config.ClaudeInitialResponseTimeout; got != 6*time.Minute {
		t.Fatalf("ClaudeInitialResponseTimeout=%s, want 6m", got)
	}
	if got := config.ClaudeToolResultTimeout; got != 7*time.Minute {
		t.Fatalf("ClaudeToolResultTimeout=%s, want 7m", got)
	}
	if got := config.AgentUpdateTimeout; got != 16*time.Minute {
		t.Fatalf("AgentUpdateTimeout=%s, want 16m", got)
	}
}

func TestConfigFromEnvFallsBackForInvalidClaudeTurnIdleTimeout(t *testing.T) {
	for _, value := range []string{"invalid", "0", "-1m"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("AUTO_CLAUDE_TURN_IDLE_TIMEOUT", value)
			t.Setenv("AUTO_CLAUDE_INITIAL_RESPONSE_TIMEOUT", value)
			t.Setenv("AUTO_CLAUDE_TOOL_RESULT_TIMEOUT", value)
			t.Setenv("AUTO_AGENT_UPDATE_TIMEOUT", value)
			config := ConfigFromEnv()
			if got := config.ClaudeTurnIdleTimeout; got != defaultClaudeTurnIdleTimeout {
				t.Fatalf("ClaudeTurnIdleTimeout=%s, want %s", got, defaultClaudeTurnIdleTimeout)
			}
			if got := config.ClaudeInitialResponseTimeout; got != defaultClaudeInitialResponseTimeout {
				t.Fatalf("ClaudeInitialResponseTimeout=%s, want %s", got, defaultClaudeInitialResponseTimeout)
			}
			if got := config.ClaudeToolResultTimeout; got != defaultClaudeToolResultTimeout {
				t.Fatalf("ClaudeToolResultTimeout=%s, want %s", got, defaultClaudeToolResultTimeout)
			}
			if got := config.AgentUpdateTimeout; got != defaultAgentUpdateTimeout {
				t.Fatalf("AgentUpdateTimeout=%s, want %s", got, defaultAgentUpdateTimeout)
			}
		})
	}
}

func writeNpmClaudeInstall(t *testing.T, dir, version string, executable bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"version":"`+version+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	mode := os.FileMode(0o644)
	if executable {
		mode = 0o755
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "claude.exe"), []byte("test binary"), mode); err != nil {
		t.Fatal(err)
	}
}

func TestRollbackInterruptedNpmClaudeInstall(t *testing.T) {
	prefix := t.TempDir()
	packageRoot := filepath.Join(prefix, "lib", "node_modules", "@anthropic-ai")
	current := filepath.Join(packageRoot, "claude-code")
	backup := filepath.Join(packageRoot, ".claude-code-previous")
	writeNpmClaudeInstall(t, current, "2.1.224", false)
	writeNpmClaudeInstall(t, backup, "2.1.220", true)

	recovered, err := rollbackInterruptedNpmClaudeInstall(prefix, "2.1.220")
	if err != nil {
		t.Fatalf("rollback interrupted install: %v", err)
	}
	if recovered != "2.1.220" {
		t.Fatalf("recovered version=%q, want 2.1.220", recovered)
	}
	if version, err := claudePackageVersion(current); err != nil || version != "2.1.220" {
		t.Fatalf("active package version=%q err=%v, want 2.1.220", version, err)
	}
	if info, err := os.Stat(filepath.Join(current, "bin", "claude.exe")); err != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("active binary is not executable: info=%v err=%v", info, err)
	}
	link := filepath.Join(prefix, "bin", "claude")
	if target, err := os.Readlink(link); err != nil || target != "../lib/node_modules/@anthropic-ai/claude-code/bin/claude.exe" {
		t.Fatalf("recovered CLI link target=%q err=%v", target, err)
	}
	entries, err := os.ReadDir(packageRoot)
	if err != nil {
		t.Fatal(err)
	}
	foundInterrupted := false
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".claude-code-interrupted-") {
			foundInterrupted = true
		}
	}
	if !foundInterrupted {
		t.Fatal("interrupted package was not retained as a rollback backup")
	}
}

func TestClaudeUpdateRollsBackInterruptedNpmInstall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture uses POSIX shell scripts and symlinks")
	}
	prefix := t.TempDir()
	packageRoot := filepath.Join(prefix, "lib", "node_modules", "@anthropic-ai")
	active := filepath.Join(packageRoot, "claude-code")
	writeNpmClaudeInstall(t, active, "2.1.220", true)
	claudeScript := `#!/bin/sh
case "$1" in
  --version) echo "2.1.220 (Claude Code)" ;;
  auth) exit 0 ;;
  update)
    mv "$TEST_PACKAGE_ROOT/claude-code" "$TEST_PACKAGE_ROOT/.claude-code-previous"
    mkdir -p "$TEST_PACKAGE_ROOT/claude-code/bin"
    printf '{"version":"2.1.224"}' > "$TEST_PACKAGE_ROOT/claude-code/package.json"
    printf 'interrupted update' > "$TEST_PACKAGE_ROOT/claude-code/bin/claude.exe"
    mv "$TEST_PREFIX/bin/claude" "$TEST_PREFIX/bin/.claude-interrupted"
    exit 1
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(active, "bin", "claude.exe"), []byte(claudeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../lib/node_modules/@anthropic-ai/claude-code/bin/claude.exe", filepath.Join(prefix, "bin", "claude")); err != nil {
		t.Fatal(err)
	}
	npmScript := "#!/bin/sh\nprintf '%s\\n' \"$TEST_PREFIX\"\n"
	if err := os.WriteFile(filepath.Join(prefix, "bin", "npm"), []byte(npmScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_PREFIX", prefix)
	t.Setenv("TEST_PACKAGE_ROOT", packageRoot)
	t.Setenv("PATH", filepath.Join(prefix, "bin")+string(os.PathListSeparator)+"/usr/bin:/bin")

	runner := &claudeCLIRunner{config: Config{ClaudePath: "claude", AgentUpdateTimeout: time.Minute}}
	previous, current, err := runner.Update(context.Background())
	if err == nil || !strings.Contains(err.Error(), "已自动回滚到 Claude Code 2.1.220") {
		t.Fatalf("update error=%v, want rollback result", err)
	}
	if previous != "2.1.220" || current != "2.1.220" {
		t.Fatalf("update versions previous=%q current=%q", previous, current)
	}
	if got := runner.Version(context.Background()); got != "2.1.220" {
		t.Fatalf("recovered CLI version=%q, want 2.1.220", got)
	}
}

func TestClaudeSessionIdleTimeoutFailsRunsAndReleasesConversation(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','project',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,agent_id,status,claude_initialized,agent_initialized,is_current,created_at) values ('conversation','project','session','claude-code','running',1,1,1,?)`, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	for _, run := range []struct {
		id     string
		status string
	}{{"first", "running"}, {"second", "queued"}} {
		if _, err := server.db.Exec(`insert into runs (id,conversation_id,agent_id,agent_runtime_id,execution_policy,status,created_at) values (?, 'conversation', 'claude-code', 'wsl-local', 'full_control', ?, ?)`, run.id, run.status, now); err != nil {
			t.Fatalf("insert %s run: %v", run.id, err)
		}
	}

	session := newClaudeSessionForTimerTest(20 * time.Millisecond)
	first := &claudeSessionTurn{sink: &agentRunSink{server: server, runID: "first", conversationID: "conversation", agentID: "claude-code", streaming: true}}
	second := &claudeSessionTurn{sink: &agentRunSink{server: server, runID: "second", conversationID: "conversation", agentID: "claude-code", streaming: true}}
	startTimerTestTurn(session, first, second)

	completed := false
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var firstStatus, secondStatus, conversationStatus string
		err := server.db.QueryRowContext(context.Background(), `select status from runs where id='first'`).Scan(&firstStatus)
		if err == nil {
			err = server.db.QueryRowContext(context.Background(), `select status from runs where id='second'`).Scan(&secondStatus)
		}
		if err == nil {
			err = server.db.QueryRowContext(context.Background(), `select status from conversations where id='conversation'`).Scan(&conversationStatus)
		}
		if err == nil && firstStatus == "failed" && secondStatus == "failed" && conversationStatus == "idle" {
			completed = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if completed {
		var rawPayload []byte
		if err := server.db.QueryRow(`select payload from events where run_id='first' and type='run.failed' order by created_at desc limit 1`).Scan(&rawPayload); err != nil {
			t.Fatalf("read run.failed event: %v", err)
		}
		var details map[string]string
		if err := json.Unmarshal(rawPayload, &details); err != nil {
			t.Fatalf("decode run.failed payload: %v", err)
		}
		if details["stallPhase"] != string(claudeTurnWaitingInitialResponse) || details["lastEvent"] != "user_prompt" {
			t.Fatalf("unexpected stall details: %#v", details)
		}
		var queuedPayload []byte
		if err := server.db.QueryRow(`select payload from events where run_id='second' and type='run.failed' order by created_at desc limit 1`).Scan(&queuedPayload); err != nil {
			t.Fatalf("read queued run.failed event: %v", err)
		}
		if err := json.Unmarshal(queuedPayload, &details); err != nil {
			t.Fatalf("decode queued run.failed payload: %v", err)
		}
		if details["stallPhase"] != "queued" || details["lastEvent"] != "not_started" || details["previousStallPhase"] != string(claudeTurnWaitingInitialResponse) {
			t.Fatalf("unexpected queued stall details: %#v", details)
		}
		return
	}

	var firstStatus, secondStatus, conversationStatus string
	if err := server.db.QueryRow(`select status from runs where id='first'`).Scan(&firstStatus); err != nil {
		t.Fatalf("read first run: %v", err)
	}
	if err := server.db.QueryRow(`select status from runs where id='second'`).Scan(&secondStatus); err != nil {
		t.Fatalf("read second run: %v", err)
	}
	if err := server.db.QueryRow(`select status from conversations where id='conversation'`).Scan(&conversationStatus); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	t.Fatalf("timeout statuses: first=%q second=%q conversation=%q", firstStatus, secondStatus, conversationStatus)
}

func TestParseClaudeMessage(t *testing.T) {
	cases := []struct {
		name      string
		msg       string // JSON for the message field
		wantParts []string
	}{
		{
			"object with content array",
			`{"content":[{"type":"text","text":"hello"},{"type":"tool_use","name":"Read"}]}`,
			[]string{"hello"},
		},
		{
			"object with content plain string",
			`{"content":"直接文本"}`,
			[]string{"直接文本"},
		},
		{
			"object empty content",
			`{"content":[]}`,
			nil,
		},
		{
			"plain string message",
			`"整段文本作为 message 字符串"`,
			[]string{"整段文本作为 message 字符串"},
		},
		{
			"plain empty string message",
			`""`,
			nil,
		},
		{
			"missing message field",
			``,
			nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var raw json.RawMessage
			if c.msg != "" {
				raw = json.RawMessage(c.msg)
			} else {
				raw = nil
			}
			parts, _ := parseClaudeMessage(raw)
			if len(parts) != len(c.wantParts) {
				t.Fatalf("parseClaudeMessage(%q) = %#v; want %#v", c.msg, parts, c.wantParts)
			}
			for i := range parts {
				if parts[i] != c.wantParts[i] {
					t.Errorf("part[%d] = %q; want %q", i, parts[i], c.wantParts[i])
				}
			}
		})
	}
}

// TestClaudeReadOutputAcceptsPlainStringMessage 验证 readOutput（一次性 Run 路径）能
// 接受 message 为纯字符串的 assistant 行——即此前 "cannot unmarshal string into ... .message"
// 导致整行丢弃的形态——并照常把文本交给 AssistantText，且不产生 stream.error。
func TestClaudeReadOutputAcceptsPlainStringMessage(t *testing.T) {
	runner := &claudeCLIRunner{}
	sink := &claudeOutputTestSink{}
	// 旧的 struct{Content json.RawMessage} 声明会拒绝此行的 "message":"..." 字符串形态。
	runner.readOutput(strings.NewReader(`{"type":"assistant","parent_tool_use_id":"t1","message":"纯字符串消息正文"}`+"\n"), sink)
	if len(sink.texts) != 1 {
		t.Fatalf("expected 1 assistant text, got %#v", sink.texts)
	}
	if sink.texts[0] != "纯字符串消息正文" {
		t.Errorf("assistant text = %q; want 纯字符串消息正文", sink.texts[0])
	}
	for _, e := range sink.events {
		var errEv struct{ Error string `json:"error"` }
		if err := json.Unmarshal(e, &errEv); err == nil && errEv.Error != "" {
			t.Fatalf("unexpected stream.error event: %s", string(e))
		}
	}
}

// errClosedReader 在 Read 时返回 os.ErrClosed，模拟进程被停止/强杀后 stdout/stderr
// 管道被关闭、bufio.Scanner 拿到 "file already closed" 的形态。
type errClosedReader struct{}

func (errClosedReader) Read([]byte) (int, error) { return 0, os.ErrClosed }

// TestClaudeReadOutputSilencesPipeClosed 验证：停止时管道关闭（os.ErrClosed）不应
// 产生 stream.error 事件。此前该路径会在对话历史里留下"流错误 / file already closed"。
func TestClaudeReadOutputSilencesPipeClosed(t *testing.T) {
	runner := &claudeCLIRunner{}

	// readOutput（一次性 Run 路径）。
	sink := &claudeOutputTestSink{}
	runner.readOutput(errClosedReader{}, sink)
	for _, e := range sink.events {
		var errEv struct{ Error string `json:"error"` }
		if err := json.Unmarshal(e, &errEv); err == nil && errEv.Error != "" {
			t.Fatalf("readOutput emitted stream.error on pipe close: %s", string(e))
		}
	}

	// readStderr（一次性 Run 路径）。
	sink = &claudeOutputTestSink{}
	runner.readStderr(errClosedReader{}, sink)
	for _, e := range sink.events {
		var errEv struct{ Error string `json:"error"` }
		if err := json.Unmarshal(e, &errEv); err == nil && errEv.Error != "" {
			t.Fatalf("readStderr emitted stream.error on pipe close: %s", string(e))
		}
	}
}

// TestCodexReadOutputSilencesPipeClosed 同理验证 codex 一次性读循环。
func TestCodexReadOutputSilencesPipeClosed(t *testing.T) {
	sink := &claudeOutputTestSink{}
	readCodexJSONL(errClosedReader{}, sink, "")
	for _, e := range sink.events {
		var errEv struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(e, &errEv); err == nil && errEv.Error != "" {
			t.Fatalf("readCodexJSONL emitted stream.error on pipe close: %s", string(e))
		}
	}
}
