package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

const acpAuthFixture = `
const fs = require('node:fs');
const readline = require('node:readline');
const mode = process.env.MILEVIA_AUTH_FIXTURE;
if (mode === 'descendant' || mode === 'hang-descendant') {
 const child = require('node:child_process').spawn(process.execPath, ['-e','setInterval(()=>{},1000)'], {stdio:'ignore',windowsHide:true});
 fs.writeFileSync(process.env.MILEVIA_AUTH_PID_FILE, JSON.stringify([process.pid, child.pid]));
}
if (process.env.DISABLE_AUTOUPDATER !== '1' || !process.argv.includes('--acp') ||
    !process.argv.includes('--strict-mcp-config') || !process.argv.includes('--no-session-persistence')) process.exit(70);
if (mode === 'exit') process.exit(71);
readline.createInterface({input: process.stdin}).on('line', line => {
 const request = JSON.parse(line);
 if (request.method !== 'initialize' && request.method !== '_codebuddy.ai/getUserInfo') process.exit(72);
 if (mode === 'hang' || mode === 'hang-descendant') return;
 if (mode === 'overflow') return process.stdout.write('x'.repeat(300000));
 if (request.method === 'initialize') {
  if (request.id !== 1 || request.params.protocolVersion !== 1) process.exit(73);
  return process.stdout.write(JSON.stringify({jsonrpc:'2.0',id:1,result:{protocolVersion:1}})+'\n');
 }
 if (mode === 'unsupported') return process.stdout.write(JSON.stringify({jsonrpc:'2.0',id:2,error:{code:-32601,message:'SECRET_TOKEN'}})+'\n');
 if (mode === 'malformed') return process.stdout.write('not json SECRET_TOKEN\n');
 if (mode === 'null') return process.stdout.write(JSON.stringify({jsonrpc:'2.0',id:2,result:{userInfo:null}})+'\n');
 if (mode === 'wrong-id') return process.stdout.write(JSON.stringify({jsonrpc:'2.0',id:3,result:{}})+'\n');
 let result = mode === 'empty' ? {} : {userInfo:{userId:'real-account-id',userName:'SECRET_USERNAME',accessToken:'SECRET_TOKEN'}};
 if (mode === 'anonymous') result.userInfo.userId = 'anonymous_test-key';
 if (mode === 'no-token') delete result.userInfo.accessToken;
 const response = JSON.stringify({jsonrpc:'2.0',id:2,result})+'\n';
 // Exercise streaming across arbitrary chunk boundaries.
 process.stdout.write(response.slice(0,19));
 setTimeout(() => process.stdout.write(response.slice(19)), 5);
});
`

func isolateACPAuthFixture(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for _, variable := range []string{"CODEBUDDY_API_KEY", "CODEBUDDY_AUTH_TOKEN", "ACC_PRODUCT_CONFIG_V2", "ACC_PRODUCT_CONFIG_V3", "ACC_PRODUCT_CONFIG_PATH"} {
		t.Setenv(variable, "")
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEBUDDY_CONFIG_DIR", filepath.Join(home, ".codebuddy"))
	return home
}

func TestACPAuthProbeProtocolAndCredentialStates(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js unavailable")
	}
	tests := []struct {
		name, mode, setup string
		want              agentAuthState
		wantErr           bool
	}{
		{"account", "account", "", agentAuthLoggedIn, false},
		{"logged-out", "empty", "", agentAuthLoggedOut, false},
		{"anonymous-key", "anonymous", "", agentAuthConfigured, false},
		{"key-with-custom-account-id", "account", "key", agentAuthConfigured, false},
		{"settings-key", "account", "settings-key", agentAuthConfigured, false},
		{"helper-without-account", "empty", "helper", agentAuthConfigured, false},
		{"unrestored-credentials", "empty", "stored", agentAuthUnreadable, true},
		{"explicit-logout", "empty", "logout", agentAuthLoggedOut, false},
		{"broken-settings", "account", "broken", agentAuthUnreadable, true},
		{"unsupported-method", "unsupported", "", agentAuthUnreadable, true},
		{"malformed", "malformed", "", agentAuthUnreadable, true},
		{"null-is-not-logout", "null", "", agentAuthUnreadable, true},
		{"wrong-response-id", "wrong-id", "", agentAuthUnreadable, true},
		{"missing-token", "no-token", "", agentAuthUnreadable, true},
		{"output-limit", "overflow", "", agentAuthUnreadable, true},
		{"early-exit", "exit", "", agentAuthUnreadable, true},
		{"bounded-timeout", "hang", "", agentAuthUnreadable, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := isolateACPAuthFixture(t)
			t.Setenv("MILEVIA_AUTH_FIXTURE", tc.mode)
			fixture := filepath.Join(t.TempDir(), "fake cli.cjs")
			if err := os.WriteFile(fixture, []byte(acpAuthFixture), 0600); err != nil {
				t.Fatal(err)
			}
			settings := filepath.Join(home, ".codebuddy", "settings.json")
			if err := os.MkdirAll(filepath.Dir(settings), 0700); err != nil {
				t.Fatal(err)
			}
			switch tc.setup {
			case "key":
				t.Setenv("CODEBUDDY_API_KEY", "SECRET_TOKEN")
			case "settings-key", "helper", "broken":
				content := `{"env":{"CODEBUDDY_API_KEY":"SECRET_TOKEN"}}`
				if tc.setup == "helper" {
					content = `{"apiKeyHelper":"echo SECRET_TOKEN"}`
				}
				if tc.setup == "broken" {
					content = "invalid json"
				}
				if err := os.WriteFile(settings, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			case "stored", "logout":
				root := filepath.Join(home, ".local", "share")
				if runtime.GOOS == "windows" {
					root = filepath.Join(home, "AppData", "Local")
				}
				if runtime.GOOS == "darwin" {
					root = filepath.Join(home, "Library", "Application Support")
				}
				auth := filepath.Join(root, "CodeBuddyExtension", "Data", "Public", "auth")
				if err := os.MkdirAll(auth, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(auth, "fixture.info"), []byte("SECRET_TOKEN"), 0600); err != nil {
					t.Fatal(err)
				}
				if tc.setup == "logout" {
					if err := os.WriteFile(filepath.Join(auth, "fixture.info.logged-out"), nil, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			entry, _ := agentByID("codebuddy")
			config := acpAuthProbeConfig(entry, fixture, 18*time.Second, "")
			if tc.mode == "hang" {
				config = strings.Replace(config, "18000", "200", 1)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			out, stderr, err := runProbeCommandStreamsLimit(ctx, exec.CommandContext(ctx, node, "-e", acpAuthProbeScript, config), 4096)
			if err != nil {
				t.Fatalf("probe transport: %v (stderr bytes=%d)", err, len(stderr))
			}
			if strings.Contains(string(out)+string(stderr), "SECRET_") || strings.Contains(string(out), "real-account-id") {
				t.Fatal("probe exposed account secrets")
			}
			state, err := acpAuthState(string(out))
			if state != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("state=%s err=%v want %s err=%v; output=%s", state, err, tc.want, tc.wantErr, out)
			}
		})
	}
}

func TestACPAuthCatalogAndTargetIsolation(t *testing.T) {
	entry, _ := agentByID("codebuddy")
	if entry.AuthStatusProtocol != authProtocolACPUserInfo {
		t.Fatal("CodeBuddy ACP protocol missing from catalog")
	}
	shell := newFakeCrossShell()
	shell.outputs[entry.CommandName] = "2.181.0"
	shell.outputs["node"] = "v24.21.0"
	shell.runFunc = func(script string) (string, error) {
		if !strings.Contains(script, "command -v 'codebuddy'") || !strings.Contains(script, "node -e ") || !strings.Contains(script, "getUserInfo") {
			t.Fatal("target probe does not use catalog CLI and ACP")
		}
		return `{"authStatus":"configured"}`, nil
	}
	state, err := (&catalogAgentBackend{entry: entry, shell: shell}).LoginStatus(context.Background())
	if state != agentAuthConfigured || err != nil {
		t.Fatalf("state=%s err=%v", state, err)
	}
	shell.failing[entry.CommandName] = true
	if state, err = (&catalogAgentBackend{entry: entry, shell: shell}).LoginStatus(context.Background()); state != agentAuthUnreadable || err == nil {
		t.Fatal("unavailable target must remain unknown")
	}
	if len(shell.ranScripts()) != 1 {
		t.Fatal("probe bypassed native target validation")
	}
	shell.failing[entry.CommandName] = false
	shell.runFunc = func(string) (string, error) { return "SECRET_TOKEN", errors.New("SECRET_TOKEN") }
	if _, err = (&catalogAgentBackend{entry: entry, shell: shell}).LoginStatus(context.Background()); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatal("target error exposed secrets")
	}
}

func TestACPAuthConfiguredReadingIsCachedWithoutClaimingAccountLogin(t *testing.T) {
	entry, _ := agentByID("codebuddy")
	runner := &fakeAuthRunner{state: agentAuthConfigured}
	server := &Server{runtimeCtx: context.Background()}
	read := server.agentAuthFor(context.Background(), "local", entry, runner, false)
	if read.state != agentAuthConfigured || !read.readable() || read.loggedIn() || read.note == "" {
		t.Fatalf("bad configured reading: %+v", read)
	}
	server.agentAuthFor(context.Background(), "local", entry, runner, false)
	if runner.probeCount() != 1 {
		t.Fatal("configured reading was not cached")
	}
	server.agentAuthFor(context.Background(), "local", entry, runner, true)
	if runner.probeCount() != 2 {
		t.Fatal("manual refresh did not probe")
	}
}

func TestACPAuthStateRejectsRawErrorAndUnknownShape(t *testing.T) {
	for _, raw := range []string{"SECRET_TOKEN", `{"authStatus":"unexpected","reason":"SECRET_TOKEN"}`, "{}", strings.Repeat("x", 5000)} {
		state, err := acpAuthState(raw)
		if state != agentAuthUnreadable || err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("unsafe result state=%s err=%v", state, err)
		}
	}
}

func TestACPAuthBudgetLeavesCleanupTime(t *testing.T) {
	entry, _ := agentByID("codebuddy")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	config, err := boundedACPAuthConfig(ctx, entry, "fixture", "")
	var decoded struct {
		Timeout int64 `json:"timeoutMs"`
	}
	if err != nil || json.Unmarshal([]byte(config), &decoded) != nil || decoded.Timeout <= 0 || decoded.Timeout > 2000 {
		t.Fatalf("invalid remaining budget: %s %v", config, err)
	}
	expired, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if _, err := boundedACPAuthConfig(expired, entry, "fixture", ""); err == nil {
		t.Fatal("started a probe with no cleanup budget")
	}
}

func TestACPAuthConfiguredWireAndForceRefresh(t *testing.T) {
	shell := newFakeCrossShell()
	shell.outputs["codebuddy"] = "2.181.0"
	shell.outputs["node"] = "v24.21.0"
	result := `{"authStatus":"configured"}`
	shell.runFunc = func(string) (string, error) { return result, nil }
	server := &Server{runnerRegistry: newRunnerRegistry(), runtimeCtx: context.Background(), runnerUpdating: map[runnerAgentKey]bool{}}
	meta := RunnerMeta{ID: "auth-target", Name: "目标环境", Environment: "remote-linux"}
	server.runnerRegistry.register(meta.ID, &crossShellOnlyRunner{shell}, meta)
	call := func(force bool) map[string]any {
		path := "/api/runners/auth-target/agents/codebuddy/login-status"
		if force {
			path += "?force=true"
		}
		request := httptest.NewRequest(http.MethodGet, path, nil)
		route := chi.NewRouteContext()
		route.URLParams.Add("runnerID", meta.ID)
		route.URLParams.Add("agentID", "codebuddy")
		request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, route))
		response := httptest.NewRecorder()
		server.agentLoginStatus(response, request)
		var body map[string]any
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &body) != nil {
			t.Fatalf("status endpoint: %d", response.Code)
		}
		return body
	}
	body := call(false)
	if body["authStatus"] != agentAuthConfigured || body["loggedIn"] != false || body["note"] == "" {
		t.Fatalf("configured wire: %v", body)
	}
	result = `{"authStatus":"logged_in"}`
	if call(false)["authStatus"] != agentAuthConfigured {
		t.Fatal("cache not preserved")
	}
	if body = call(true); body["authStatus"] != agentAuthLoggedIn || body["loggedIn"] != true {
		t.Fatalf("forced wire: %v", body)
	}
}

func TestCodebuddyLoginStatusInstalled(t *testing.T) {
	if os.Getenv("MILEVIA_TEST_CODEBUDDY_AUTH") != "1" {
		t.Skip("explicit installed CLI auth probe")
	}
	runner := &codebuddyCLIRunner{paths: newAgentPathResolver(Config{})}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	state, err := runner.LoginStatus(ctx)
	if err != nil || state != agentAuthLoggedIn {
		t.Fatalf("installed CodeBuddy account probe: state=%s err=%v", state, err)
	}
	t.Log("installed CodeBuddy account session detected without a model prompt")
}
