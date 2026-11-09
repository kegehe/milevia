package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestSupervisionCodexStreamRejectsExecutionAndInvalidOrdering(t *testing.T) {
	init := "{\"type\":\"thread.started\",\"thread_id\":\"test-thread\"}\n{\"type\":\"turn.started\"}\n"
	answer := "{\"type\":\"item.completed\",\"item\":{\"id\":\"answer\",\"type\":\"agent_message\",\"text\":\"{}\"}}\n"
	end := "{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}"
	for _, tc := range []struct {
		name, raw string
		ok        bool
	}{
		{"native_result", init + answer + end, true},
		{"reasoning", init + "{\"type\":\"item.completed\",\"item\":{\"id\":\"reason\",\"type\":\"reasoning\",\"text\":\"checking\"}}\n" + answer + end, true},
		{"no_thread", answer + end, false},
		{"no_answer", init + end, false},
		{"duplicate_answer", init + answer + answer + end, false},
		{"after_terminal", init + answer + end + "\n" + answer, false},
		{"invalid_json", init + "garbage", false},
		{"failure", init + "{\"type\":\"turn.failed\",\"error\":{\"message\":\"secret\"}}", false},
		{"command", init + "{\"type\":\"item.started\",\"item\":{\"id\":\"tool\",\"type\":\"command_execution\",\"command\":\"danger\"}}", false},
		{"mcp", init + "{\"type\":\"item.completed\",\"item\":{\"id\":\"tool\",\"type\":\"mcp_tool_call\"}}", false},
		{"file_write", init + "{\"type\":\"item.completed\",\"item\":{\"id\":\"tool\",\"type\":\"file_change\"}}", false},
		{"tool_disguised_as_text", init + "{\"type\":\"item.completed\",\"item\":{\"id\":\"tool\",\"type\":\"reasoning\",\"command\":\"danger\"}}", false},
		{"commentary_before_answer", init + "{\"type\":\"item.completed\",\"item\":{\"id\":\"commentary\",\"type\":\"agent_message\",\"phase\":\"commentary\",\"text\":\"Checking\"}}\n" + answer + end, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := newSupervisionCodexStream(&claudeOutputTestSink{}, cancel)
			for offset := 0; offset < len(tc.raw); offset += 7 {
				end := min(offset+7, len(tc.raw))
				if _, err := s.Write([]byte(tc.raw[offset:end])); err != nil {
					break
				}
			}
			s.flush()
			if (s.failure() == nil) != tc.ok || (!tc.ok && ctx.Err() == nil) || (tc.ok && (!s.initialized || !s.completed || s.output != "{}")) {
				t.Fatalf("valid=%t initialized=%t completed=%t error=%v", tc.ok, s.initialized, s.completed, s.failure())
			}
		})
	}
}

func TestSupervisionCodexFailureRetainsRedactedCauseWithoutAcceptingResult(t *testing.T) {
	for _, kind := range []string{"error", "turn.failed"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sink := &claudeOutputTestSink{}
			stream := newSupervisionCodexStream(sink, cancel)
			message := "401 Unauthorized Authorization: Bearer sk-testsecret123456789012345678901234567890 " + strings.Repeat("x", maxCodexDiagnosticBytes+100)
			event := map[string]any{"type": kind, "message": message, "error": map[string]string{"message": message}}
			_, _ = stream.Write(append([]byte("{\"type\":\"thread.started\",\"thread_id\":\"t\"}\n{\"type\":\"turn.started\"}\n"), mustJSON(event)...))
			stream.flush()
			err := stream.failure()
			if err == nil || !strings.Contains(err.Error(), "401 Unauthorized") || strings.Contains(err.Error(), "sk-testsecret") || len(err.Error()) > maxCodexDiagnosticBytes+256 || ctx.Err() == nil || stream.completed || stream.output != "" {
				t.Fatal("native failure lost its safe diagnostic or became a result", err)
			}
			if supervisionTransientError(err.Error()) {
				t.Fatal("authentication failure became a transient retry")
			}
		})
	}
}

func TestSupervisionCodexConnectionWhitelistAndTOMLInjection(t *testing.T) {
	config := map[string]any{
		"model": "selected-model", "model_provider": "custom", "profile": "selected",
		"profiles":               map[string]any{"selected": map[string]any{"model": "profile-model", "developer_instructions": "UNAUTHORIZED_PROFILE_RULE"}},
		"developer_instructions": "UNAUTHORIZED_RULE", "model_instructions_file": "UNAUTHORIZED_FILE",
		"hooks": map[string]any{"command": "UNAUTHORIZED_HOOK"}, "mcp_servers": map[string]any{"command": "UNAUTHORIZED_MCP"},
		"model_providers": map[string]any{"custom": map[string]any{
			"name": "Custom", "base_url": "https://example.invalid/v1", "env_key": "KEY",
			"model_catalog_url": "UNAUTHORIZED_CATALOG", "wire_api": "responses", "http_headers": map[string]any{"X-Test": "a\"\n[mcp_servers.bad]\ncommand='oops'"},
		}},
	}
	result, err := supervisionCodexConnectionConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result, "UNAUTHORIZED") || !strings.Contains(result, `"model" = "profile-model"`) || !strings.Contains(result, `\n[mcp_servers.bad]\n`) || !strings.Contains(result, `"supports_websockets" = false`) {
		t.Fatalf("connection whitelist or string encoding failed: %s", result)
	}
	config["model_provider"] = "missing"
	if _, err := supervisionCodexConnectionConfig(config); err == nil {
		t.Fatal("silently changed missing provider")
	}
	config["model_provider"] = "custom"
	config["cli_auth_credentials_store"] = "keyring"
	if _, err := supervisionCodexConnectionConfig(config); err == nil {
		t.Fatal("silently changed authentication store")
	}
}

func TestSupervisionCodexRequiresBoundedAuthorizedExecution(t *testing.T) {
	runner := &codexCLIRunner{}
	request := schedulerCLIRequestForTest()
	if _, err := runner.runSupervisionScheduler(context.Background(), request, &claudeOutputTestSink{}); err == nil {
		t.Fatal("unbounded execution accepted")
	}
	args := strings.Join(supervisionCodexArgs(request, []string{"shell_tool", "hooks", "plugins"}), "\n")
	for _, forbidden := range []string{"--dangerously", "--resume", "--add-dir", "--enable"} {
		if strings.Contains(args, forbidden) {
			t.Fatalf("unexpected authority: %s", forbidden)
		}
	}
	if !strings.Contains(args, "--disable\nshell_tool") || !strings.HasSuffix(args, "\n-") {
		t.Fatal("missing closed tool or stdin contract")
	}
	// 参数不承载凭据；角色协议可解析为一个独立 TOML 字符串。
	encoded := supervisionTOMLString("a\n\"b\"")
	var decoded string
	if json.Unmarshal([]byte(encoded), &decoded) != nil || decoded != "a\n\"b\"" {
		t.Fatal("invalid fixed instruction encoding")
	}
}
