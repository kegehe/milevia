package app

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSupervisionQoderAndCursorHaveIndependentFailClosedStreams(t *testing.T) {
	qoderInit := `{"type":"system","subtype":"init","session_id":"qoder-session","qodercli_version":"1.1.82","permissionMode":"dont_ask","tools":[],"mcp_servers":[],"plugins":[],"agents":[],"skills":[]}`
	cursorInit := `{"type":"system","subtype":"init","session_id":"cursor-session","permissionMode":"default"}`
	for _, tc := range []struct {
		name, contract, frames string
		accepted               bool
	}{
		{"qoder_success", supervisionQoderContract, qoderInit, true},
		{"qoder_tools", supervisionQoderContract, strings.Replace(qoderInit, `"tools":[]`, `"tools":["Bash"]`, 1), false},
		{"qoder_skills", supervisionQoderContract, strings.Replace(qoderInit, `"skills":[]`, `"skills":["canary"]`, 1), false},
		{"qoder_agents", supervisionQoderContract, strings.Replace(qoderInit, `"agents":[]`, `"agents":["agent"]`, 1), false},
		{"qoder_wrong_version", supervisionQoderContract, strings.Replace(qoderInit, `1.1.82`, `1.1.83`, 1), false},
		{"qoder_wrong_permissions", supervisionQoderContract, strings.Replace(qoderInit, `dont_ask`, `default`, 1), false},
		{"qoder_cursor_init", supervisionQoderContract, cursorInit, false},
		{"cursor_success", supervisionCursorContract, cursorInit, true},
		{"cursor_missing_session", supervisionCursorContract, strings.Replace(cursorInit, `"cursor-session"`, `""`, 1), false},
		{"cursor_extra_tools", supervisionCursorContract, strings.TrimSuffix(cursorInit, `}`) + `,"tools":["shell"]}`, false},
		{"cursor_tool_event", supervisionCursorContract, cursorInit + "\n" + `{"type":"tool_call","subtype":"started","tool_call":{"shellToolCall":{"command":"must-not-run"}}}`, false},
		{"cursor_tool_content", supervisionCursorContract, cursorInit + "\n" + `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read"}]}}`, false},
		{"cursor_hook_event", supervisionCursorContract, cursorInit + "\n" + `{"type":"system","subtype":"hook_started"}`, false},
		{"claude_cursor_init", supervisionSchedulerCLIContract, cursorInit, false},
		{"codebuddy_cursor_init", supervisionCodebuddyContract, cursorInit, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stream := &supervisionSchedulerStream{sink: &claudeOutputTestSink{}, cancel: cancel, contract: tc.contract}
			_, _ = stream.Write([]byte(tc.frames + "\n" + `{"type":"result","subtype":"success","is_error":false,"result":"{}"}` + "\n"))
			stream.flush()
			if (stream.failure() == nil) != tc.accepted || tc.accepted && (!stream.initialized || !stream.completed || stream.output != "{}") || !tc.accepted && ctx.Err() == nil {
				t.Fatal("incorrect protocol boundary", stream.failure(), stream.initialized, stream.completed)
			}
		})
	}
}

func TestSupervisionIsolatedCLIEnvironmentDropsAmbientAutomationAndPreloads(t *testing.T) {
	for _, name := range []string{"QODER_MCP_CONFIG", "QODERCN_CONFIG_DIR", "CURSOR_AGENT_CLI_LOCAL_MODE", "CURSOR_API_KEY", "NODE_OPTIONS", "NODE_PATH", "GOOGLE_APPLICATION_CREDENTIALS", "GEMINI_API_KEY", "AGENT_CLI_CREDENTIAL_STORE", "AUTO_CONTROL_URL"} {
		t.Setenv(name, "ambient-canary")
	}
	home := t.TempDir()
	environment := supervisionIsolatedCLIEnvironment(home, "CURSOR_API_KEY=explicit-test-key")
	values := map[string]string{}
	for _, item := range environment {
		name, value, _ := strings.Cut(item, "=")
		if strings.Contains(value, "ambient-canary") {
			t.Fatal("ambient automation reached controlled subprocess", name)
		}
		values[strings.ToUpper(name)] = value
	}
	if values["HOME"] != home || values["USERPROFILE"] != home || values["CURSOR_API_KEY"] != "explicit-test-key" || values["NODE_OPTIONS"] != "" || values["AUTO_CONTROL_URL"] != "" {
		t.Fatal("isolated environment lost its bound home or explicit connection")
	}
}

func TestSupervisionCursorTrustedHookActuallyDeniesEveryTool(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	dir := t.TempDir()
	if err := prepareSupervisionCursorIsolation(dir, supervisionCLIInfo{Path: node}); err != nil {
		t.Fatal(err)
	}
	var hooks struct {
		Hooks map[string][]struct {
			Command string `json:"command"`
			Matcher string `json:"matcher"`
		} `json:"hooks"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".cursor", "hooks.json"))
	if err != nil || json.Unmarshal(raw, &hooks) != nil || len(hooks.Hooks) != 1 || len(hooks.Hooks["preToolUse"]) != 1 || hooks.Hooks["preToolUse"][0].Matcher != "*" {
		t.Fatal("trusted deny gate not installed", err)
	}
	for _, tool := range []string{"Shell", "Read", "Write", "WebFetch", "Mcp", "UnknownFutureTool"} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cmd := exec.CommandContext(ctx, node, filepath.Join(dir, "deny-tools.cjs"))
		cmd.Stdin = strings.NewReader(string(mustJSON(map[string]any{"tool_name": tool, "tool_input": map[string]string{"path": "sensitive-canary"}})))
		output, err := cmd.Output()
		cancel()
		var verdict map[string]any
		if err != nil || json.Unmarshal(output, &verdict) != nil || verdict["permission"] != "deny" || strings.Contains(string(output), "sensitive-canary") {
			t.Fatal("trusted gate did not deny tool", tool, err)
		}
	}
}

func TestSupervisionQoderCredentialReferenceRefreshesOriginalWithoutConfigTree(t *testing.T) {
	source, home := t.TempDir(), t.TempDir()
	t.Setenv("QODER_CONFIG_DIR", source)
	t.Setenv("QODER_CONFIG_DIR_NAME", "")
	t.Setenv("QODERCN_CONFIG_DIR", "")
	for name, body := range map[string]string{".keychain-salt": "test-salt", "qoder-cli-credentials.json": "fake-encrypted-login", "settings.json": `{"hooks":{"must-not-copy":true}}`, "hooks.json": `{"must-not-copy":true}`} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := prepareSupervisionQoderLogin(home); err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(home, ".qoder")
	entries, err := os.ReadDir(private)
	if err != nil || len(entries) != 2 {
		t.Fatal("copied user resources instead of referencing credentials", len(entries), err)
	}
	if err := os.WriteFile(filepath.Join(private, "qoder-cli-credentials.json"), []byte("fake-refreshed-login"), 0600); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(filepath.Join(source, "qoder-cli-credentials.json"))
	if err != nil || string(actual) != "fake-refreshed-login" {
		t.Fatal("credential refresh was lost in private copy", err)
	}
}

type supervisionPendingAdapterForTest struct {
	AgentRunner
	agent string
	path  string
}

func (a *supervisionPendingAdapterForTest) supervisionCLIInfo(context.Context, bool) (supervisionCLIInfo, error) {
	return supervisionCLIInfo{AgentID: a.agent, Path: a.path, Version: "fixture", Contract: "pending-fixture", ValidationPending: true}, nil
}
func (*supervisionPendingAdapterForTest) runSupervisionScheduler(context.Context, supervisionSchedulerCLIRequest, AgentRunSink) (supervisionSchedulerCLIReceipt, error) {
	panic("pending capability must never dispatch")
}

func TestSupervisionUnverifiedAdapterIsVisibleButCannotStart(t *testing.T) {
	s, project, _, _, _ := supervisionFixture(t, defaultSupervisionBudget())
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, agent := range []string{"qoder", "cursor"} {
		adapter := &supervisionPendingAdapterForTest{AgentRunner: unavailableRunner{}, agent: agent, path: path}
		if agent == "qoder" {
			s.qoderRunner = adapter
		} else {
			s.cursorRunner = adapter
		}
		c := s.supervisionCapabilityForProject(context.Background(), project, agent, true)
		if c.Available || c.State != "validation_pending" || c.Reason == "" {
			t.Fatal("unverified tool advertised as production ready", agent, c)
		}
	}
}

func TestSupervisionQoderInstalledEntryResolvesWithoutCredentialsOrModel(t *testing.T) {
	binary := os.Getenv("MILEVIA_SUPERVISION_QODER_PROTOCOL_ENTRY")
	if binary == "" {
		t.Skip("set actual Qoder npm entry for no-model installation check")
	}
	info, err := supervisionQoderNode(binary)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	home := t.TempDir()
	cmd := exec.CommandContext(ctx, info.Path, info.Entrypoint, "--version")
	cmd.Dir, cmd.Env = home, supervisionIsolatedCLIEnvironment(home)
	raw, err := runProbeCommand(ctx, cmd)
	if err != nil || agentVersionFromOutput(string(raw)) != supervisionQoderVersion || !info.ValidationPending || info.EntrypointSHA256 == "" {
		t.Fatal("installed Qoder not independently resolved", runtime.GOOS, err)
	}
}
