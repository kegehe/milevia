package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestCursorExecArgs(t *testing.T) {
	tests := []struct {
		name    string
		request AgentRunRequest
		want    []string
	}{
		{
			name:    "read only asks without force",
			request: AgentRunRequest{Prompt: "analyze", PermissionMode: "read_only"},
			want:    []string{"-p", "--output-format", "stream-json", "--trust", "--mode", "ask"},
		},
		{
			name:    "workspace write runs the default agent mode with force",
			request: AgentRunRequest{Prompt: "fix it", PermissionMode: "workspace_write"},
			want:    []string{"-p", "--output-format", "stream-json", "--trust", "--force"},
		},
		{
			name:    "full control disables the sandbox",
			request: AgentRunRequest{Prompt: "fix it", PermissionMode: "full_control"},
			want:    []string{"-p", "--output-format", "stream-json", "--trust", "--force", "--sandbox", "disabled"},
		},
		{
			name:    "resume and model are appended",
			request: AgentRunRequest{Prompt: "more", PermissionMode: "workspace_write", Resume: true, SessionID: "chat-1", Model: "gpt-5"},
			want:    []string{"-p", "--output-format", "stream-json", "--trust", "--force", "--resume", "chat-1", "--model", "gpt-5"},
		},
		{
			name:    "empty permission mode defaults to workspace write",
			request: AgentRunRequest{Prompt: "hello"},
			want:    []string{"-p", "--output-format", "stream-json", "--trust", "--force"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := cursorExecArgs(test.request)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, "\x00") != strings.Join(test.want, "\x00") {
				t.Fatalf("args = %#v, want %#v", got, test.want)
			}
		})
	}

	if _, err := cursorExecArgs(AgentRunRequest{Prompt: "x", PermissionMode: "approval_required"}); err == nil {
		t.Fatal("approval_required should be rejected")
	}
	if _, err := cursorExecArgs(AgentRunRequest{Prompt: "  ", PermissionMode: "workspace_write"}); err == nil {
		t.Fatal("empty prompt should be rejected")
	}
	if _, err := cursorExecArgs(AgentRunRequest{Prompt: "x", PermissionMode: "workspace_write", Model: "foo&bar"}); err == nil {
		t.Fatal("model with cmd metacharacters should be rejected")
	}
	if _, err := cursorExecArgs(AgentRunRequest{Prompt: "x", PermissionMode: "workspace_write", Resume: true, SessionID: "chat&calc"}); err == nil {
		t.Fatal("session ID with cmd metacharacters should be rejected")
	}
	if _, err := cursorExecArgs(AgentRunRequest{Prompt: "x", PermissionMode: "workspace_write", Resume: true}); err == nil {
		t.Fatal("resume without a session ID should be rejected")
	}
}

// 假 CLI 只做一件事：从 stdin 读一行提示词，再把它作为 assistant/result 回显。
// 如果参数里混进位置提示词（旧实现的 `-`），stdin 就不会被读取，测试立刻暴露。
func writeFakeCursorCLI(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		path := filepath.Join(dir, "cursor-agent.cmd")
		script := strings.Join([]string{
			"@echo off",
			"set /p PROMPT=",
			`echo {"type":"system","subtype":"init","session_id":"fake-cursor"}`,
			`echo {"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"%PROMPT%"}]},"session_id":"fake-cursor"}`,
			`echo {"type":"result","subtype":"success","result":"%PROMPT%","session_id":"fake-cursor","is_error":false}`,
		}, "\r\n") + "\r\n"
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	path := filepath.Join(dir, "cursor-agent")
	script := strings.Join([]string{
		"#!/bin/sh",
		"IFS= read -r PROMPT || true",
		`printf '{"type":"system","subtype":"init","session_id":"fake-cursor"}\n'`,
		`printf '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"%s"}]},"session_id":"fake-cursor"}\n' "$PROMPT"`,
		`printf '{"type":"result","subtype":"success","result":"%s","session_id":"fake-cursor","is_error":false}\n' "$PROMPT"`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCursorRunnerDeliversPromptViaStdin(t *testing.T) {
	script := writeFakeCursorCLI(t)
	runner := &cursorCLIRunner{paths: &agentPathResolver{override: map[string]string{"cursor": script}}}
	sink := &recordingSink{}
	err := runner.Run(context.Background(), AgentRunRequest{
		ProjectPath:    t.TempDir(),
		Prompt:         "hello from stdin",
		PermissionMode: "workspace_write",
	}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(sink.texts, "|") != "hello from stdin" {
		t.Fatalf("texts = %#v", sink.texts)
	}
	if len(sink.sessions) != 1 || sink.sessions[0] != "fake-cursor" {
		t.Fatalf("sessions = %#v", sink.sessions)
	}
}

func TestReadCursorJSONL(t *testing.T) {
	sink := &codexPayloadSink{}
	completion := newOpenCodeCompletion()
	input := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"chat-1"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hello"}]},"session_id":"chat-1"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":"ok"}]},"session_id":"chat-1"}`,
		`{"type":"result","subtype":"success","result":"hello","session_id":"chat-1","is_error":false}`,
	}, "\n") + "\n"
	readCursorJSONL(strings.NewReader(input), sink, completion)

	if !completion.finished() {
		t.Fatal("result success did not finish the turn")
	}
	if failure := completion.failure(); failure != nil {
		t.Fatalf("unexpected failure: %v", failure)
	}
	if len(sink.sessions) != 1 || sink.sessions[0] != "chat-1" {
		t.Fatalf("sessions = %#v", sink.sessions)
	}
	if sink.initialized != 1 {
		t.Fatalf("initialized = %d", sink.initialized)
	}
	if strings.Join(sink.texts, "|") != "hello" {
		t.Fatalf("texts = %#v", sink.texts)
	}
	for _, eventType := range []string{"system", "assistant", "user", "result"} {
		if !containsString(sink.events, eventType) {
			t.Fatalf("event %q was not forwarded: %#v", eventType, sink.events)
		}
	}
}

func TestReadCursorJSONLUsesResultTextWhenNoAssistantFrame(t *testing.T) {
	sink := &recordingSink{}
	completion := newOpenCodeCompletion()
	readCursorJSONL(strings.NewReader(`{"type":"result","subtype":"success","result":"final answer","session_id":"chat-2"}`+"\n"), sink, completion)
	if failure := completion.failure(); failure != nil {
		t.Fatalf("unexpected failure: %v", failure)
	}
	if strings.Join(sink.texts, "|") != "final answer" {
		t.Fatalf("texts = %#v", sink.texts)
	}
}

func TestReadCursorJSONLResultErrorFinishesWithFailure(t *testing.T) {
	completion := newOpenCodeCompletion()
	readCursorJSONL(strings.NewReader(`{"type":"result","subtype":"error","result":"permission denied","is_error":true}`+"\n"), &recordingSink{}, completion)
	if !completion.finished() {
		t.Fatal("error result did not finish the turn")
	}
	if failure := completion.failure(); failure == nil || !strings.Contains(failure.Error(), "permission denied") {
		t.Fatalf("failure = %v", failure)
	}
}

func TestReadCursorJSONLMissingResultFails(t *testing.T) {
	completion := newOpenCodeCompletion()
	readCursorJSONL(strings.NewReader(`{"type":"assistant","message":{"content":[{"type":"text","text":"partial"}]}}`+"\n"), &recordingSink{}, completion)
	if completion.failure() == nil || !strings.Contains(completion.failure().Error(), "未返回结束事件") {
		t.Fatalf("failure = %v", completion.failure())
	}
}

func TestReadCursorJSONLEmptyOutputFails(t *testing.T) {
	completion := newOpenCodeCompletion()
	readCursorJSONL(strings.NewReader(`{"type":"system","subtype":"init","session_id":"chat-empty"}`+"\n"), &recordingSink{}, completion)
	if completion.failure() == nil || !strings.Contains(completion.failure().Error(), "未返回任何输出") {
		t.Fatalf("failure = %v", completion.failure())
	}
}

func TestReadCursorJSONLNonJSONLineBecomesDiagnostic(t *testing.T) {
	sink := &codexPayloadSink{}
	completion := newOpenCodeCompletion()
	readCursorJSONL(strings.NewReader("starting up\n"+`{"type":"result","subtype":"success","result":"ok"}`+"\n"), sink, completion)
	if failure := completion.failure(); failure != nil {
		t.Fatalf("unexpected failure: %v", failure)
	}
	if !containsString(sink.events, "stderr") {
		t.Fatalf("non-JSON line was not surfaced as a diagnostic: %#v", sink.events)
	}
}

func cursorPayloadAt(sink *codexPayloadSink, eventType string, index int) json.RawMessage {
	seen := 0
	for position, current := range sink.events {
		if current != eventType {
			continue
		}
		if seen == index {
			return json.RawMessage(sink.payloads[position])
		}
		seen++
	}
	return nil
}

type cursorDecodedToolPayload struct {
	Message struct {
		Content []struct {
			Type      string         `json:"type"`
			ID        string         `json:"id"`
			Name      string         `json:"name"`
			Input     map[string]any `json:"input"`
			ToolUseID string         `json:"tool_use_id"`
			Content   string         `json:"content"`
			IsError   bool           `json:"is_error"`
		} `json:"content"`
	} `json:"message"`
}

func decodeCursorToolPayload(t *testing.T, raw json.RawMessage) cursorDecodedToolPayload {
	t.Helper()
	var payload cursorDecodedToolPayload
	if len(raw) == 0 {
		t.Fatal("missing payload")
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if len(payload.Message.Content) != 1 {
		t.Fatalf("content = %#v", payload.Message.Content)
	}
	return payload
}

func TestReadCursorJSONLTranslatesToolCalls(t *testing.T) {
	sink := &codexPayloadSink{}
	completion := newOpenCodeCompletion()
	input := strings.Join([]string{
		`{"type":"tool_call","subtype":"started","call_id":"call-1","session_id":"chat-tool","tool_call":{"tool":{"case":"shellToolCall","value":{"args":{"command":"git status"}}}}}`,
		`{"type":"tool_call","subtype":"completed","call_id":"call-1","session_id":"chat-tool","tool_call":{"tool":{"case":"shellToolCall","value":{"args":{"command":"git status"},"result":{"result":{"case":"success","value":{"exitCode":0,"stdout":"clean\n","stderr":""}}}}}}}`,
		`{"type":"result","subtype":"success","result":"done","session_id":"chat-tool"}`,
	}, "\n") + "\n"
	readCursorJSONL(strings.NewReader(input), sink, completion)
	if failure := completion.failure(); failure != nil {
		t.Fatalf("unexpected failure: %v", failure)
	}
	if containsString(sink.events, "tool_call") {
		t.Fatalf("raw tool_call should not be forwarded: %#v", sink.events)
	}

	use := decodeCursorToolPayload(t, cursorPayloadAt(sink, "assistant", 0))
	toolUse := use.Message.Content[0]
	if toolUse.Type != "tool_use" || toolUse.ID != "call-1" || toolUse.Name != "终端命令" {
		t.Fatalf("tool_use = %#v", toolUse)
	}
	if command, _ := toolUse.Input["command"].(string); command != "git status" {
		t.Fatalf("command = %#v", toolUse.Input["command"])
	}

	result := decodeCursorToolPayload(t, cursorPayloadAt(sink, "user", 0))
	toolResult := result.Message.Content[0]
	if toolResult.Type != "tool_result" || toolResult.ToolUseID != "call-1" || toolResult.Content != "clean" || toolResult.IsError {
		t.Fatalf("tool_result = %#v", toolResult)
	}
}

func TestReadCursorJSONLBackfillsToolUseForOrphanResult(t *testing.T) {
	sink := &codexPayloadSink{}
	completion := newOpenCodeCompletion()
	input := `{"type":"tool_call","subtype":"completed","call_id":"call-2","tool_call":{"tool":{"case":"editToolCall","value":{"args":{"path":"a.txt"},"result":{"result":{"case":"success","value":{"path":"a.txt","diffString":"@@ -1 +1 @@\n-old\n+new"}}}}}}}` + "\n" +
		`{"type":"result","subtype":"success","result":"done"}` + "\n"
	readCursorJSONL(strings.NewReader(input), sink, completion)
	if failure := completion.failure(); failure != nil {
		t.Fatalf("unexpected failure: %v", failure)
	}
	use := decodeCursorToolPayload(t, cursorPayloadAt(sink, "assistant", 0))
	if toolUse := use.Message.Content[0]; toolUse.Type != "tool_use" || toolUse.ID != "call-2" || toolUse.Name != "文件修改" {
		t.Fatalf("tool_use = %#v", toolUse)
	}
	result := decodeCursorToolPayload(t, cursorPayloadAt(sink, "user", 0))
	if toolResult := result.Message.Content[0]; toolResult.ToolUseID != "call-2" || !strings.Contains(toolResult.Content, "-old") || toolResult.IsError {
		t.Fatalf("tool_result = %#v", toolResult)
	}
}

func TestReadCursorJSONLToolFailureUsesExitCode(t *testing.T) {
	sink := &codexPayloadSink{}
	completion := newOpenCodeCompletion()
	input := `{"type":"tool_call","subtype":"completed","call_id":"call-3","tool_call":{"tool":{"case":"shellToolCall","value":{"args":{"command":"false"},"result":{"result":{"case":"success","value":{"exitCode":1,"stdout":"","stderr":"boom"}}}}}}}` + "\n" +
		`{"type":"result","subtype":"success","result":"done"}` + "\n"
	readCursorJSONL(strings.NewReader(input), sink, completion)
	if failure := completion.failure(); failure != nil {
		t.Fatalf("unexpected failure: %v", failure)
	}
	result := decodeCursorToolPayload(t, cursorPayloadAt(sink, "user", 0))
	toolResult := result.Message.Content[0]
	if !toolResult.IsError || !strings.Contains(toolResult.Content, "boom") || !strings.Contains(toolResult.Content, "退出码 1") {
		t.Fatalf("tool_result = %#v", toolResult)
	}
}

func TestReadCursorJSONLTruncatesLargeToolOutput(t *testing.T) {
	output := strings.Repeat("x", cursorMaxToolOutputBytes+1024)
	line, err := json.Marshal(map[string]any{
		"type":    "tool_call",
		"subtype": "completed",
		"call_id": "call-4",
		"tool_call": map[string]any{"tool": map[string]any{"case": "readToolCall", "value": map[string]any{
			"args": map[string]any{"path": "big.txt"},
			"result": map[string]any{"result": map[string]any{
				"case": "success",
				"value": map[string]any{
					"output": map[string]any{"case": "content", "value": output},
				},
			}},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	sink := &codexPayloadSink{}
	completion := newOpenCodeCompletion()
	readCursorJSONL(strings.NewReader(string(line)+"\n"+`{"type":"result","subtype":"success","result":"done"}`+"\n"), sink, completion)
	result := decodeCursorToolPayload(t, cursorPayloadAt(sink, "user", 0))
	toolResult := result.Message.Content[0]
	if !strings.Contains(toolResult.Content, "... [TRUNCATED]") {
		t.Fatalf("large output was not truncated: %d bytes", len(toolResult.Content))
	}
	if len(toolResult.Content) > cursorMaxToolOutputBytes+64 {
		t.Fatalf("truncated output is still %d bytes", len(toolResult.Content))
	}
}

func TestCursorUpdateAvailableFromDateVersions(t *testing.T) {
	tests := []struct {
		name   string
		local  string
		latest string
		want   bool
	}{
		{name: "newer day with leading zero", local: "2026.11.02-abc1234", latest: "2026.11.03-abc1234", want: true},
		{name: "compact month and day", local: "2026.11.2-abc1234", latest: "2026.11.10-abc1234", want: true},
		{name: "numeric day comparison", local: "2026.11.10-abc1234", latest: "2026.11.2-abc1234", want: false},
		{name: "same day rebuild is not an update", local: "2026.11.10-aaa", latest: "2026.11.10-zzz", want: false},
		{name: "newer year", local: "2025.12.31-abc", latest: "2026.1.1-abc", want: true},
		{name: "fallback to semver", local: "1.2.3", latest: "1.3.0", want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := cursorUpdateAvailableFrom(test.local, test.latest)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("cursorUpdateAvailableFrom(%q, %q) = %v, want %v", test.local, test.latest, got, test.want)
			}
		})
	}
	if _, ok := parseCursorVersion("2026.13.1-abc"); ok {
		t.Fatal("invalid month should not be parsed as a date version")
	}
	if _, ok := parseCursorVersion("2026.10.32-abc"); ok {
		t.Fatal("invalid day should not be parsed as a date version")
	}
}

func TestCursorLatestVersionPatternAcceptsDateVariants(t *testing.T) {
	entry, _ := agentByID("cursor")
	pattern, err := regexp.Compile(entry.LatestVersionPattern)
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]string{
		"lab/2026.10.19-6bbf2c4":        "2026.10.19-6bbf2c4",
		"lab/2026.11.2-abc123":          "2026.11.2-abc123",
		"lab/2026.11.02-abc123":         "2026.11.02-abc123",
		"lab/2026.11.2-12-34-56-abcdef": "2026.11.2-12-34-56-abcdef",
	}
	for input, want := range tests {
		match := pattern.FindStringSubmatch(input)
		if len(match) < 2 || match[1] != want {
			t.Fatalf("pattern match for %q = %#v, want %q", input, match, want)
		}
	}
}

func TestCatalogExposesCursor(t *testing.T) {
	entry, ok := agentByID("cursor")
	if !ok {
		t.Fatal("cursor is missing from the agent catalog")
	}
	if entry.Name != "Cursor CLI" || entry.Vendor != "Anysphere" {
		t.Fatalf("cursor name/vendor = %q/%q", entry.Name, entry.Vendor)
	}
	if entry.CommandName != "cursor-agent" || !containsString(entry.CommandAliases, "agent") {
		t.Fatalf("cursor command/aliases = %q/%v", entry.CommandName, entry.CommandAliases)
	}
	if entry.InstallKind != installKindNative {
		t.Fatalf("cursor install kind = %q, want native", entry.InstallKind)
	}
	if entry.SupportsInstall {
		t.Fatal("cursor is distributed by the official installer; the platform must not claim it can install it")
	}
	if !entry.SupportsLogin || !entry.RunnableInProject {
		t.Fatalf("cursor capability flags incomplete: %+v", entry)
	}
	if entry.SupportsProjectInsights {
		t.Fatal("cursor has no read-only analysis protocol yet; insights must stay off")
	}
	if entry.SupportsProfiles {
		t.Fatal("cursor profiles would hit the agent_profiles CHECK constraint; SupportsProfiles must stay false")
	}
	if entry.LatestVersionURL == "" || entry.LatestVersionPattern == "" {
		t.Fatal("cursor needs an official version source and extraction pattern")
	}
	// 平台不代装原生包，管理页的「手动安装」块只能把用户送去官方说明 ——
	// 没有 DocsURL 时那一行不会出现（前端有意不猜地址）。
	if entry.DocsURL == "" {
		t.Fatal("cursor needs a DocsURL for the manual-install guidance")
	}
	if len(entry.UpdateArgs) == 0 {
		t.Fatal("cursor update must use the CLI's own update subcommand")
	}
}
