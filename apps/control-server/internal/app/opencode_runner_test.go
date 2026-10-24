package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestOpenCodeExecArgs(t *testing.T) {
	tests := []struct {
		name    string
		request AgentRunRequest
		want    []string
	}{
		{
			name:    "workspace write continues session with model",
			request: AgentRunRequest{Prompt: "hello", PermissionMode: "workspace_write", Resume: true, SessionID: "ses-1", Model: "opencode/gpt-5.1-codex"},
			want:    []string{"run", "--format", "json", "--auto", "--session", "ses-1", "--model", "opencode/gpt-5.1-codex"},
		},
		{
			name:    "read only uses plan agent without auto",
			request: AgentRunRequest{Prompt: "analyze", PermissionMode: "read_only"},
			want:    []string{"run", "--format", "json", "--agent", "plan"},
		},
		{
			name:    "full control auto approves",
			request: AgentRunRequest{Prompt: "fix it", PermissionMode: "full_control", ReasoningEffort: "high"},
			want:    []string{"run", "--format", "json", "--auto", "--variant", "high"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := opencodeExecArgs(test.request)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, "\x00") != strings.Join(test.want, "\x00") {
				t.Fatalf("args = %#v, want %#v", got, test.want)
			}
		})
	}
	if _, err := opencodeExecArgs(AgentRunRequest{Prompt: "x", PermissionMode: "approval_required"}); err == nil {
		t.Fatal("approval_required should be rejected")
	}
	if _, err := opencodeExecArgs(AgentRunRequest{Prompt: "  ", PermissionMode: "workspace_write"}); err == nil {
		t.Fatal("empty prompt should be rejected")
	}
	if _, err := opencodeExecArgs(AgentRunRequest{Prompt: "x", PermissionMode: "workspace_write", Model: "foo&bar"}); err == nil {
		t.Fatal("model with cmd metacharacters should be rejected")
	}
	if _, err := opencodeExecArgs(AgentRunRequest{Prompt: "x", PermissionMode: "workspace_write", ReasoningEffort: "high&rm"}); err == nil {
		t.Fatal("variant with cmd metacharacters should be rejected")
	}
	if _, err := opencodeExecArgs(AgentRunRequest{Prompt: "x", PermissionMode: "workspace_write", Resume: true, SessionID: "ses&calc"}); err == nil {
		t.Fatal("session ID with cmd metacharacters should be rejected")
	}
	if _, err := opencodeExecArgs(AgentRunRequest{Prompt: "x", PermissionMode: "workspace_write", Resume: true}); err == nil {
		t.Fatal("resume without a session ID should be rejected")
	}
}

func TestOpenCodeEnvironmentOverridesParentPermission(t *testing.T) {
	t.Setenv("OPENCODE_PERMISSION", `{"*":"allow"}`)
	environment, err := opencodeProcessEnvironment(AgentRunRequest{
		PermissionMode: "read_only",
		MCPEnv:         []string{"OPENCODE_PERMISSION=" + `{"*":"allow"}`, "MCP_SEC_OTHER=secret"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	value := ""
	for _, item := range environment {
		if strings.HasPrefix(item, "OPENCODE_PERMISSION=") {
			count++
			value = strings.TrimPrefix(item, "OPENCODE_PERMISSION=")
		}
	}
	if count != 1 {
		t.Fatalf("OPENCODE_PERMISSION appears %d times, want exactly 1", count)
	}
	if !strings.Contains(value, `"*":"deny"`) {
		t.Fatalf("platform permission did not override the parent env: %s", value)
	}
}

func TestOpenCodePermissionJSON(t *testing.T) {
	readOnly, err := opencodePermissionJSON("read_only")
	if err != nil {
		t.Fatal(err)
	}
	var readOnlyValue map[string]any
	if err := json.Unmarshal([]byte(readOnly), &readOnlyValue); err != nil {
		t.Fatal(err)
	}
	if readOnlyValue["*"] != "deny" || readOnlyValue["edit"] != "deny" || readOnlyValue["shell"] != "deny" {
		t.Fatalf("read_only permission = %s", readOnly)
	}

	workspace, err := opencodePermissionJSON("workspace_write")
	if err != nil {
		t.Fatal(err)
	}
	var workspaceValue map[string]any
	if err := json.Unmarshal([]byte(workspace), &workspaceValue); err != nil {
		t.Fatal(err)
	}
	if workspaceValue["*"] != "allow" || workspaceValue["external_directory"] != "deny" || workspaceValue["question"] != "deny" {
		t.Fatalf("workspace_write permission = %s", workspace)
	}
	if _, ok := workspaceValue["shell"].(map[string]any); !ok {
		t.Fatalf("workspace_write shell rules missing: %s", workspace)
	}

	fullControl, err := opencodePermissionJSON("full_control")
	if err != nil {
		t.Fatal(err)
	}
	var fullValue map[string]any
	if err := json.Unmarshal([]byte(fullControl), &fullValue); err != nil {
		t.Fatal(err)
	}
	if fullValue["question"] != "deny" {
		t.Fatalf("full_control must deny headless questions: %s", fullControl)
	}

	if _, err := opencodePermissionJSON("approval_required"); err == nil {
		t.Fatal("approval_required should be rejected")
	}
}

func TestReadOpenCodeJSONL(t *testing.T) {
	sink := &codexPayloadSink{}
	completion := newOpenCodeCompletion()
	input := strings.Join([]string{
		`{"type":"step_start","sessionID":"ses-1","part":{}}`,
		`{"type":"text","sessionID":"ses-1","part":{"id":"p1","text":"hello"}}`,
		`{"type":"tool_use","sessionID":"ses-1","part":{"id":"call-1","tool":"bash","state":{"status":"completed","input":{"command":"echo hi"},"output":"hi\n"}}}`,
		`{"type":"text","sessionID":"ses-1","part":{"id":"p2","text":"done"}}`,
		`{"type":"step_finish","sessionID":"ses-1","part":{"reason":"stop"}}`,
	}, "\n") + "\n"
	readOpenCodeJSONL(strings.NewReader(input), sink, completion)

	if !completion.finished() {
		t.Fatal("step_finish stop did not finish the turn")
	}
	if len(sink.sessions) != 1 || sink.sessions[0] != "ses-1" {
		t.Fatalf("sessions = %#v", sink.sessions)
	}
	if sink.initialized != 1 {
		t.Fatalf("initialized = %d", sink.initialized)
	}
	if strings.Join(sink.texts, "|") != "hello|done" {
		t.Fatalf("texts = %#v", sink.texts)
	}
	if !containsString(sink.events, "assistant") || !containsString(sink.events, "user") {
		t.Fatalf("events = %#v", sink.events)
	}
	var sawToolUse, sawToolResult bool
	for _, payload := range sink.payloads {
		if strings.Contains(payload, `"type":"tool_use"`) && strings.Contains(payload, `"name":"Bash"`) && strings.Contains(payload, `"command":"echo hi"`) {
			sawToolUse = true
		}
		if strings.Contains(payload, `"type":"tool_result"`) && strings.Contains(payload, `"tool_use_id":"call-1"`) && strings.Contains(payload, `"content":"hi\n"`) {
			sawToolResult = true
		}
	}
	if !sawToolUse || !sawToolResult {
		t.Fatalf("tool payloads missing: %#v", sink.payloads)
	}
}

func TestReadOpenCodeJSONLRealFixture(t *testing.T) {
	sink := &codexPayloadSink{}
	completion := newOpenCodeCompletion()
	// 2026-10-22 从 opencode-ai@1.19.3 真机抓取的事件形状（截断输出内容）：
	// 第一个 step 调 read，step_finish reason=tool-calls；第二个 step 输出文本后 stop。
	input := strings.Join([]string{
		`{"type":"step_start","sessionID":"ses_real","part":{"id":"prt_1","messageID":"msg_1","type":"step-start"}}`,
		`{"type":"tool_use","sessionID":"ses_real","part":{"type":"tool","tool":"read","callID":"call_1","state":{"status":"completed","input":{"filePath":"/tmp/a.txt"},"output":"<path>/tmp/a.txt</path>\n<content>\n1: hello-from-file\n</content>","metadata":{"exit":0},"title":"a.txt"}}}`,
		`{"type":"step_finish","sessionID":"ses_real","part":{"reason":"tool-calls","type":"step-finish"}}`,
		`{"type":"step_start","sessionID":"ses_real","part":{"id":"prt_2","messageID":"msg_2","type":"step-start"}}`,
		`{"type":"text","sessionID":"ses_real","part":{"id":"prt_3","messageID":"msg_2","type":"text","text":"hello-from-file"}}`,
		`{"type":"step_finish","sessionID":"ses_real","part":{"reason":"stop","type":"step-finish"}}`,
	}, "\n") + "\n"
	readOpenCodeJSONL(strings.NewReader(input), sink, completion)
	if !completion.finished() {
		t.Fatal("final stop was not recognized")
	}
	if strings.Join(sink.texts, "|") != "hello-from-file" {
		t.Fatalf("texts = %#v", sink.texts)
	}
	var sawRead, sawResult bool
	for _, payload := range sink.payloads {
		if strings.Contains(payload, `"name":"Read"`) && strings.Contains(payload, `"path":"/tmp/a.txt"`) {
			sawRead = true
		}
		if strings.Contains(payload, `"type":"tool_result"`) && strings.Contains(payload, "hello-from-file") && strings.Contains(payload, `"is_error":false`) {
			sawResult = true
		}
	}
	if !sawRead || !sawResult {
		t.Fatalf("real fixture was not mapped to timeline events: %#v", sink.payloads)
	}
}

func TestReadOpenCodeJSONLHandlesTextSnapshots(t *testing.T) {
	t.Run("longer snapshot replaces shorter", func(t *testing.T) {
		sink := &recordingSink{}
		completion := newOpenCodeCompletion()
		input := strings.Join([]string{
			`{"type":"text","part":{"id":"same","text":"hel"}}`,
			`{"type":"text","part":{"id":"same","text":"hello"}}`,
			`{"type":"step_finish","part":{"reason":"stop"}}`,
		}, "\n") + "\n"
		readOpenCodeJSONL(strings.NewReader(input), sink, completion)
		if strings.Join(sink.texts, "|") != "hello" {
			t.Fatalf("texts = %#v", sink.texts)
		}
	})
	t.Run("shorter snapshot replaces longer", func(t *testing.T) {
		sink := &recordingSink{}
		completion := newOpenCodeCompletion()
		input := strings.Join([]string{
			`{"type":"text","part":{"id":"same","text":"hello"}}`,
			`{"type":"text","part":{"id":"same","text":"hel"}}`,
			`{"type":"step_finish","part":{"reason":"stop"}}`,
		}, "\n") + "\n"
		readOpenCodeJSONL(strings.NewReader(input), sink, completion)
		if strings.Join(sink.texts, "|") != "hel" {
			t.Fatalf("texts = %#v", sink.texts)
		}
	})
	t.Run("whitespace across parts is preserved", func(t *testing.T) {
		sink := &recordingSink{}
		completion := newOpenCodeCompletion()
		input := strings.Join([]string{
			`{"type":"text","part":{"id":"a","text":"hello"}}`,
			`{"type":"text","part":{"id":"b","text":" world"}}`,
			`{"type":"step_finish","part":{"reason":"stop"}}`,
		}, "\n") + "\n"
		readOpenCodeJSONL(strings.NewReader(input), sink, completion)
		if strings.Join(sink.texts, "|") != "hello world" {
			t.Fatalf("texts = %#v", sink.texts)
		}
	})
}

func TestReadOpenCodeJSONLReportsErrorWhenNoStop(t *testing.T) {
	sink := &recordingSink{}
	completion := newOpenCodeCompletion()
	readOpenCodeJSONL(strings.NewReader(`{"type":"error","error":{"message":"authentication failed"}}`+"\n"), sink, completion)
	if !completion.finished() {
		t.Fatal("error without step_finish should still finish the reader with a failure")
	}
	if failure := completion.failure(); failure == nil || !strings.Contains(failure.Error(), "authentication failed") {
		t.Fatalf("failure = %v", failure)
	}
	if !containsString(sink.events, "stream.error") {
		t.Fatalf("events = %#v", sink.events)
	}
}

func TestOpenCodeErrorMessageNested(t *testing.T) {
	got := opencodeErrorMessage(json.RawMessage(`{"error":{"name":"APIError","data":{"message":"bad api key"}}}`))
	if got != "bad api key" {
		t.Fatalf("nested error message = %q", got)
	}
	if got := opencodeErrorMessage(json.RawMessage(`"plain error"`)); got != "plain error" {
		t.Fatalf("plain error message = %q", got)
	}
}

func TestReadOpenCodeJSONLEStepFinishUsesDiagnostic(t *testing.T) {
	completion := newOpenCodeCompletion()
	completion.noteDiagnostic("provider rejected the request")
	readOpenCodeJSONL(strings.NewReader(`{"type":"step_finish","part":{"reason":"error"}}`+"\n"), &recordingSink{}, completion)
	if completion.failure() == nil || !strings.Contains(completion.failure().Error(), "provider rejected") {
		t.Fatalf("failure = %v", completion.failure())
	}
}

func TestReadOpenCodeJSONLMissingStopFails(t *testing.T) {
	completion := newOpenCodeCompletion()
	readOpenCodeJSONL(strings.NewReader(`{"type":"text","part":{"id":"p","text":"partial"}}`+"\n"), &recordingSink{}, completion)
	if completion.failure() == nil || !strings.Contains(completion.failure().Error(), "未返回结束事件") {
		t.Fatalf("failure = %v", completion.failure())
	}
}

func TestReadOpenCodeJSONLEmptyOutputFails(t *testing.T) {
	completion := newOpenCodeCompletion()
	readOpenCodeJSONL(strings.NewReader(`{"type":"step_start","sessionID":"ses-empty"}`+"\n"), &recordingSink{}, completion)
	if completion.failure() == nil || !strings.Contains(completion.failure().Error(), "未返回任何输出") {
		t.Fatalf("failure = %v", completion.failure())
	}
}

func TestAgentAuthListHasCredentials(t *testing.T) {
	if agentAuthListHasCredentials("", nil) {
		t.Fatal("empty output should not be treated as logged in")
	}
	if agentAuthListHasCredentials("0 credentials", nil) {
		t.Fatal("0 credentials should not be treated as logged in")
	}
	if agentAuthListHasCredentials("No providers configured", nil) {
		t.Fatal("no providers should not be treated as logged in")
	}
	if !agentAuthListHasCredentials("anthropic\nopencode", nil) {
		t.Fatal("provider lines should be treated as logged in")
	}
	if agentAuthListHasCredentials("NOT LOGGED IN", nil) {
		t.Fatal("not logged in marker should not be treated as logged in")
	}
}

func TestOpenCodeToolMapping(t *testing.T) {
	if got := opencodeToolName("bash"); got != "Bash" {
		t.Fatalf("tool name = %q", got)
	}
	if got := opencodeToolName("mystery"); got != "mystery" {
		t.Fatalf("unknown tool name = %q", got)
	}
	if got := opencodeOutputText(json.RawMessage(`"hello"`)); got != "hello" {
		t.Fatalf("string output = %q", got)
	}
	if got := opencodeOutputText(json.RawMessage(`{"a":1}`)); got != `{"a":1}` {
		t.Fatalf("object output = %q", got)
	}
	input, ok := opencodeToolInput(json.RawMessage(`{"filePath":"/a/b.txt"}`)).(map[string]any)
	if !ok || input["path"] != "/a/b.txt" || input["file_path"] != "/a/b.txt" {
		t.Fatalf("filePath aliases missing: %#v", input)
	}
	longContent := strings.Repeat("x", opencodeMaxToolInputBytes+100)
	largeInput, _ := opencodeToolInput(json.RawMessage(`{"filePath":"/a/b.txt","content":"` + longContent + `"}`)).(map[string]any)
	content, _ := largeInput["content"].(string)
	if len(content) > opencodeMaxToolInputBytes+64 || !strings.Contains(content, "已截断") {
		t.Fatalf("long tool input was not truncated: len=%d", len(content))
	}
	metadata := json.RawMessage(`{"exitCode":1}`)
	if !opencodeToolFailed("completed", metadata) {
		t.Fatal("non-zero exit code should be failed")
	}
	if opencodeToolFailed("completed", json.RawMessage(`{"exitCode":0}`)) {
		t.Fatal("zero exit code should not be failed")
	}
}

func TestRefreshConversationForRunPicksUpNativeSession(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('refresh-project','P',?,'windows-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,agent_id,agent_session_id,agent_runtime_id,execution_policy,status,permission_mode,model_override,title,last_activity_at,claude_initialized,agent_initialized,is_current,created_at) values ('refresh-conversation','refresh-project','legacy','opencode','ses_new','windows-local','read_only','idle','read_only','opencode/gpt-5.1-codex','t',?,0,1,1,?)`, now, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	stale := Conversation{ID: "refresh-conversation", AgentSessionID: "stale", AgentInitialized: false}
	refreshed := server.refreshConversationForRun(context.Background(), stale)
	if refreshed.sessionID() != "ses_new" || !refreshed.initialized() {
		t.Fatalf("session was not refreshed: %+v", refreshed)
	}
	if refreshed.ModelOverride != "opencode/gpt-5.1-codex" || refreshed.executionPolicy() != "read_only" {
		t.Fatalf("model/policy were not refreshed: %+v", refreshed)
	}

	// Handoff 的只读策略是 startMessage 在内存里强制、并未写回 conversations；
	// 刷新不得用持久化的 workspace_write 覆盖它。
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,agent_id,agent_session_id,agent_runtime_id,execution_policy,status,permission_mode,model_override,title,last_activity_at,created_at,is_current) values ('handoff-conversation','refresh-project','legacy-handoff','codex','ses_old','windows-local','workspace_write','running','workspace_write','','t',?,?,0)`, now, now); err != nil {
		t.Fatalf("insert handoff conversation: %v", err)
	}
	handoff := server.refreshConversationForRun(context.Background(), Conversation{ID: "handoff-conversation", Handoff: true, ExecutionPolicy: "read_only"})
	if handoff.executionPolicy() != "read_only" {
		t.Fatalf("handoff read-only policy was overwritten: %+v", handoff)
	}
}

// fakeOpenCodeAgentRunner 只服务 Insights 的选择/参数断言，不启动任何进程。
type fakeOpenCodeAgentRunner struct {
	ready   bool
	request AgentRunRequest
	output  string
}

func (r *fakeOpenCodeAgentRunner) Ready(context.Context) bool             { return r.ready }
func (r *fakeOpenCodeAgentRunner) Version(context.Context) string         { return "1.19.3" }
func (r *fakeOpenCodeAgentRunner) OpenCodeReady(context.Context) bool     { return r.ready }
func (r *fakeOpenCodeAgentRunner) OpenCodeVersion(context.Context) string { return "1.19.3" }
func (r *fakeOpenCodeAgentRunner) CheckUpdate(context.Context) (bool, string, error) {
	return false, "", nil
}
func (r *fakeOpenCodeAgentRunner) Update(context.Context) (string, string, error) {
	return "", "", nil
}
func (r *fakeOpenCodeAgentRunner) Run(_ context.Context, request AgentRunRequest, sink AgentRunSink) error {
	r.request = request
	if r.output != "" {
		sink.AssistantText(r.output, "")
	}
	return nil
}

func TestRunReadOnlyAgentWithSchemaUsesOpenCode(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	project, err := server.getProjectByID(context.Background(), projectID)
	if err != nil {
		t.Fatalf("load project: %v", err)
	}
	runner := &fakeOpenCodeAgentRunner{ready: true, output: `{"findings":[]}`}
	server.opencodeRunner = runner

	text, err := server.runReadOnlyAgentWithSchema(context.Background(), project, "opencode", "analyze the project", nil, 0, nil)
	if err != nil {
		t.Fatalf("OpenCode insight run failed: %v", err)
	}
	if text != `{"findings":[]}` {
		t.Fatalf("insight text = %q", text)
	}
	if runner.request.AgentID != "opencode" || runner.request.PermissionMode != "read_only" {
		t.Fatalf("insight request = %+v, want opencode/read_only", runner.request)
	}
	if runner.request.ReadOnlyTools != nil || runner.request.PromptViaStdin {
		t.Fatalf("OpenCode must not reuse Claude-only knobs: %+v", runner.request)
	}
}

func TestCatalogExposesOpenCode(t *testing.T) {
	entry, ok := agentByID("opencode")
	if !ok {
		t.Fatal("opencode missing from catalog")
	}
	if entry.NpmPackage != "opencode-ai" || entry.CommandName != "opencode" || entry.BinPath != "bin/opencode.exe" {
		t.Fatalf("unexpected package metadata: %+v", entry)
	}
	if !entry.SupportsInstall || !entry.SupportsLogin || !entry.RunnableInProject || !entry.SupportsProjectInsights {
		t.Fatalf("opencode capability flags are incomplete: %+v", entry)
	}
	if entry.SupportsProfiles {
		t.Fatal("opencode must not advertise managed profiles before the profiles table accepts it")
	}
	if entry.DefaultPermissionMode != "workspace_write" || len(entry.PermissionModes) != 3 {
		t.Fatalf("unexpected permission surface: %+v", entry)
	}
	if len(entry.UpdateArgs) != 1 || entry.UpdateArgs[0] != "upgrade" {
		t.Fatalf("opencode update subcommand = %#v, want upgrade", entry.UpdateArgs)
	}
	if got := agentLoginCommand(entry); got != "opencode auth login" {
		t.Fatalf("login command = %q", got)
	}
}

func TestPrepareMCPInjectionForOpenCode(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	projectID := "proj-opencode"
	projectPath := t.TempDir()
	if _, err := s.db.ExecContext(ctx, `insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values (?,?,?,?,?,?,?)`,
		projectID, "P", projectPath, s.localRunnerID(), "main", true, time.Now().UTC()); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	secretRef, err := s.profileSecrets.Store(s.db, ctx, "mcp-secret")
	if err != nil {
		t.Fatalf("store secret: %v", err)
	}
	insertTestMCPServer(t, s, "github", mcpScopeGlobal, "", mcpTransportStdio, "npx",
		map[string]string{"GITHUB_PERSONAL_ACCESS_TOKEN": secretRef, "LOG_LEVEL": "info"},
		[]string{"windows"}, []string{"opencode"})

	injection := s.prepareMCPInjection(ctx, projectID, projectPath, "opencode", s.localRunnerID(), "run-opencode")
	if injection.OpenCodeConfig == "" {
		t.Fatal("expected OpenCode config to be generated")
	}
	if injection.JSON != "" || injection.LocalPath != "" || injection.Strict {
		t.Fatalf("opencode injection must not use --mcp-config: %+v", injection)
	}
	var value struct {
		MCP map[string]map[string]any `json:"mcp"`
	}
	if err := json.Unmarshal([]byte(injection.OpenCodeConfig), &value); err != nil {
		t.Fatalf("decode OpenCode config: %v\n%s", err, injection.OpenCodeConfig)
	}
	server := value.MCP["github"]
	if server["type"] != "local" || server["enabled"] != true {
		t.Fatalf("unexpected OpenCode MCP entry: %#v", server)
	}
	command, ok := server["command"].([]any)
	if !ok || len(command) == 0 || command[0] != "npx" {
		t.Fatalf("unexpected command: %#v", server["command"])
	}
	environment, ok := server["environment"].(map[string]any)
	if !ok || environment["GITHUB_PERSONAL_ACCESS_TOKEN"] != "mcp-secret" {
		t.Fatalf("secret must be inlined for OpenCode: %#v", server["environment"])
	}
	if len(injection.Env) != 0 {
		t.Fatalf("OpenCode injection should not use MCPEnv: %#v", injection.Env)
	}
	if !strings.Contains(injection.Note, "strict") {
		t.Fatalf("note should disclose the missing strict equivalent: %q", injection.Note)
	}
}

func TestOpenCodeMCPServerEntry(t *testing.T) {
	local, ok := openCodeMCPServerEntry(map[string]any{
		"type":    "stdio",
		"command": "npx",
		"args":    []string{"-y", "server"},
		"env":     map[string]string{"A": "B"},
	})
	if !ok || local["type"] != "local" || local["enabled"] != true {
		t.Fatalf("local entry = %#v, ok=%v", local, ok)
	}
	command, ok := local["command"].([]string)
	if !ok || strings.Join(command, " ") != "npx -y server" {
		t.Fatalf("local command = %#v", local["command"])
	}
	if _, ok := local["environment"].(map[string]string); !ok {
		t.Fatalf("local environment missing: %#v", local)
	}

	remote, ok := openCodeMCPServerEntry(map[string]any{
		"type":    "http",
		"url":     "https://example.com/mcp",
		"headers": map[string]string{"Authorization": "Bearer x"},
	})
	if !ok || remote["type"] != "remote" || remote["url"] != "https://example.com/mcp" {
		t.Fatalf("remote entry = %#v, ok=%v", remote, ok)
	}
	if _, ok := remote["headers"].(map[string]string); !ok {
		t.Fatalf("remote headers missing: %#v", remote)
	}

	if _, ok := openCodeMCPServerEntry(map[string]any{"type": "stdio"}); ok {
		t.Fatal("stdio without command should be rejected")
	}
}

func TestBuildSSHOpenCodeCommand(t *testing.T) {
	request := AgentRunRequest{
		ProjectPath:    "/srv/app",
		Prompt:         "fix it",
		PermissionMode: "workspace_write",
		Resume:         true,
		SessionID:      "ses-1",
		Model:          "opencode/gpt-5.1-codex",
		OpenCodeConfig: `{"mcp":{}}`,
	}
	command, err := buildSSHOpenCodeCommand(request, "/tmp/milevia-opencode-x.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"trap 'rm -f /tmp/milevia-opencode-x.json' EXIT",
		`export OPENCODE_CONFIG_CONTENT="$(cat '/tmp/milevia-opencode-x.json')"`,
		"export OPENCODE_PERMISSION=",
		"export OPENCODE_DISABLE_AUTOUPDATE=true",
		"cd '/srv/app' || exit 1",
		"opencode 'run' '--format' 'json' '--auto' '--session' 'ses-1' '--model' 'opencode/gpt-5.1-codex'",
		"status=$?",
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("command missing %q:\n%s", want, command)
		}
	}
	if strings.Contains(command, "exec opencode") {
		t.Fatalf("exec would skip the cleanup trap:\n%s", command)
	}
}

func TestWSLForwardsOpenCodeEnvironment(t *testing.T) {
	_, wslenv := wslBuildEnv([]string{
		"OPENCODE_CONFIG_CONTENT={}",
		"OPENCODE_PERMISSION={}",
		"OPENCODE_DISABLE_AUTOUPDATE=true",
	})
	for _, name := range []string{"OPENCODE_CONFIG_CONTENT/u", "OPENCODE_PERMISSION/u", "OPENCODE_DISABLE_AUTOUPDATE/u"} {
		if !strings.Contains(wslenv, name) {
			t.Fatalf("WSLENV missing %q: %s", name, wslenv)
		}
	}
}

func writeFakeOpenCodeCLI(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		path := filepath.Join(dir, "opencode.cmd")
		script := strings.Join([]string{
			"@echo off",
			"set /p PROMPT=",
			`echo {"type":"step_start","sessionID":"fake-session","part":{}}`,
			`echo {"type":"text","part":{"id":"p","text":"%PROMPT%"}}`,
			`echo {"type":"step_finish","part":{"reason":"stop"}}`,
		}, "\r\n") + "\r\n"
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	path := filepath.Join(dir, "opencode")
	script := strings.Join([]string{
		"#!/bin/sh",
		"IFS= read -r PROMPT || true",
		`printf '{"type":"step_start","sessionID":"fake-session","part":{}}\n'`,
		`printf '{"type":"text","part":{"id":"p","text":"%s"}}\n' "$PROMPT"`,
		`printf '{"type":"step_finish","part":{"reason":"stop"}}\n'`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOpenCodeRunnerUsesFakeCLI(t *testing.T) {
	script := writeFakeOpenCodeCLI(t)
	paths := &agentPathResolver{recorded: map[string]string{"opencode": script}}
	runner := &opencodeCLIRunner{paths: paths}
	sink := &recordingSink{}
	err := runner.Run(context.Background(), AgentRunRequest{
		ProjectPath:    t.TempDir(),
		Prompt:         "hello",
		PermissionMode: "workspace_write",
	}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(sink.texts, "|") != "hello" {
		t.Fatalf("texts = %#v", sink.texts)
	}
	if len(sink.sessions) != 1 || sink.sessions[0] != "fake-session" {
		t.Fatalf("sessions = %#v", sink.sessions)
	}
}
