package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCLIAuthEvidence(t *testing.T) {
	cases := []struct {
		name, kind, out, err string
		code                 int
		want                 agentAuthState
	}{
		{"claude account", "claude-json", `{"loggedIn":true,"authMethod":"claude.ai"}`, "", 0, agentAuthLoggedIn},
		{"claude key", "claude-json", `{"loggedIn":true,"authMethod":"api_key"}`, "", 0, agentAuthConfigured},
		{"claude token", "claude-json", `{"loggedIn":true,"authMethod":"oauth_token"}`, "", 0, agentAuthConfigured},
		{"claude out", "claude-json", `{"loggedIn":false,"authMethod":"none"}`, "", 1, agentAuthLoggedOut},
		{"claude future", "claude-json", `{"loggedIn":true,"authMethod":"future"}`, "", 0, agentAuthUnreadable},
		{"qoder account", "qoder-json", `{"logged_in":true,"auth_source":"credentials","login_method":"browser"}`, "", 0, agentAuthLoggedIn},
		{"qoder pat", "qoder-json", `{"logged_in":true,"auth_source":"environment_token"}`, "", 0, agentAuthConfigured},
		{"qoder out", "qoder-json", `{"logged_in":false}`, "", 0, agentAuthLoggedOut},
		{"qoder text", "qoder-json", "Account: fixture\nLogin Method: browser", "", 0, agentAuthLoggedIn},
		{"qoder text pat", "qoder-json", "Account: fixture\nLogin Method: Personal Access Token", "", 0, agentAuthConfigured},
		{"qoder no identity", "qoder-json", "Login Method: browser", "", 0, agentAuthUnreadable},
		{"opencode key", "opencode-list", "└ 1 credential", "", 0, agentAuthConfigured},
		{"opencode env only", "opencode-list", "\x1b[90m└ 0 credentials\x1b[0m\n└ 1 environment variable", "", 0, agentAuthConfigured},
		{"opencode empty", "opencode-list", "└ 0 credentials\n└ 0 environment variables", "", 0, agentAuthLoggedOut},
		{"opencode prose", "opencode-list", "provider api_key", "", 0, agentAuthUnreadable},
		{"codex stderr out", "codex-text", "", "Not logged in", 1, agentAuthLoggedOut},
		{"codex account", "codex-text", "", "Logged in using ChatGPT", 0, agentAuthLoggedIn},
		{"codex key", "codex-text", "Logged in using an API key - fixture", "", 0, agentAuthConfigured},
		{"codex failed positive", "codex-text", "Logged in using ChatGPT", "", 1, agentAuthUnreadable},
		{"cursor out", "cursor-json", `{"status":"unauthenticated","isAuthenticated":false,"hasAccessToken":false,"hasRefreshToken":false}`, "", 0, agentAuthLoggedOut},
		{"cursor unverified", "cursor-json", `{"status":"authenticated","isAuthenticated":true,"hasAccessToken":true,"hasRefreshToken":true}`, "", 0, agentAuthConfigured},
		{"cursor account", "cursor-json", `{"status":"authenticated","isAuthenticated":true,"hasAccessToken":true,"hasRefreshToken":true,"userInfo":{"email":"fixture@example.test"}}`, "", 0, agentAuthLoggedIn},
		{"cursor partial", "cursor-json", `{"status":"partially-authenticated","isAuthenticated":false,"hasAccessToken":true,"hasRefreshToken":false}`, "", 0, agentAuthConfigured},
		{"cursor old text", "cursor-json", "Not logged in", "", 0, agentAuthLoggedOut},
		{"unknown", "cursor-json", "new format", "", 0, agentAuthUnreadable},
		{"timeout", "codex-text", "Not logged in", "", 124, agentAuthUnreadable},
		{"oversize", "codex-text", strings.Repeat("x", authOutputLimit+1), "", 0, agentAuthUnreadable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseCLIAuthState(c.kind, c.out, c.err, c.code); got != c.want {
				t.Fatalf("got %s, want %s", got, c.want)
			}
		})
	}
}

func TestCLIAuthCrossProtocolAndFallback(t *testing.T) {
	e := mustAgent(t, "qoder")
	shell := newFakeCrossShell()
	calls := 0
	shell.runFunc = func(script string) (string, error) {
		calls++
		if calls == 1 {
			return "\n__MILEVIA_AUTH_STDERR__\nunknown option -o\n__MILEVIA_AUTH_CODE__=1\n", nil
		}
		return "Account: fixture\n\n__MILEVIA_AUTH_STDERR__\n\n__MILEVIA_AUTH_CODE__=0\n", nil
	}
	if state, err := crossCLIAuthStatus(context.Background(), e, shell); err != nil || state != agentAuthLoggedIn || calls != 2 {
		t.Fatalf("fallback %s %v %d", state, err, calls)
	}
	for _, output := range []string{"fixture-secret", "\n__MILEVIA_AUTH_STDERR__\nfixture-secret\n__MILEVIA_AUTH_CODE__=124", strings.Repeat("x", 2*authOutputLimit+513)} {
		shell.runFunc = func(string) (string, error) { return output, nil }
		state, err := crossCLIAuthStatus(context.Background(), e, shell)
		if state != agentAuthUnreadable || err == nil || strings.Contains(err.Error(), "fixture-secret") {
			t.Fatalf("unknown leaked output: %s %v", state, err)
		}
	}
	script := crossAuthCommand(&wslAgentRunner{}, e, e.AuthStatusArgs)
	if !strings.Contains(script, "/mnt/*") || !strings.Contains(script, "timeout -k 1s 16s") {
		t.Fatal("WSL boundary or deadline lost")
	}
}

func TestCLIAuthEnvironmentCredentials(t *testing.T) {
	for _, id := range []string{"qoder", "cursor"} {
		e := mustAgent(t, id)
		for _, name := range authEnvironmentNames(e) {
			t.Setenv(name, "")
		}
		t.Setenv(authEnvironmentNames(e)[0], "fixture-secret")
		state, err := localCLIAuthStatus(context.Background(), e, "definitely-not-installed")
		if err != nil || state != agentAuthConfigured {
			t.Fatalf("%s env credential: %s %v", id, state, err)
		}
	}
}

func TestCLIAuthLocalExitStreamsAndPrivacy(t *testing.T) {
	if os.Getenv("MILEVIA_AUTH_LOCAL_FIXTURE") == "1" {
		fmt.Fprintln(os.Stderr, "Not logged in")
		os.Exit(1)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MILEVIA_AUTH_LOCAL_FIXTURE", "1")
	e := mustAgent(t, "codex")
	e.AuthStatusArgs = []string{"-test.run=^TestCLIAuthLocalExitStreamsAndPrivacy$"}
	state, err := localCLIAuthStatus(context.Background(), e, exe)
	if err != nil || state != agentAuthLoggedOut {
		t.Fatalf("exit 1 stderr %s %v", state, err)
	}
}

func TestCLIAuthCrossScriptRunsAndPreservesExitOne(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		if git, gitErr := exec.LookPath("git"); gitErr == nil {
			candidate := filepath.Join(filepath.Dir(filepath.Dir(git)), "usr", "bin", "sh.exe")
			if fileExists(candidate) {
				sh, err = candidate, nil
				t.Setenv("PATH", filepath.Dir(candidate)+string(os.PathListSeparator)+os.Getenv("PATH"))
			}
		}
	}
	if err != nil {
		t.Skip("sh unavailable")
	}
	e := AgentCatalogEntry{CommandName: "sh", AuthStatusKind: "codex-text", AuthStatusArgs: []string{"-c", "printf 'Not logged in\\n' >&2; exit 1"}}
	shell := newFakeCrossShell()
	shell.runFunc = func(script string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		data, err := exec.CommandContext(ctx, sh, "-c", script).CombinedOutput()
		return string(data), err
	}
	state, err := crossCLIAuthStatus(context.Background(), e, shell)
	if err != nil || state != agentAuthLoggedOut {
		t.Fatalf("shell protocol: %s %v", state, err)
	}
}

func TestCLIAuthQueryWithoutLoginEntry(t *testing.T) {
	e := mustAgent(t, "qoder")
	e.SupportsLogin = false
	r := &fakeAuthRunner{state: agentAuthLoggedIn}
	got := (&Server{}).probeAgentAuth(context.Background(), e, r)
	if got.state != agentAuthLoggedIn || r.probeCount() != 1 {
		t.Fatal("query incorrectly requires login entry")
	}
}

func TestCLISelectedRuntimeAndEngineEnforcement(t *testing.T) {
	e := mustAgent(t, "claude-code")
	if err := checkSelectedRuntimeGate("20.0.0", e, "latest"); err == nil {
		t.Fatal("latest Claude requires Node 22")
	}
	if err := checkSelectedRuntimeGate("20.0.0", e, "2.0.0"); err != nil {
		t.Fatal("historical package must use its engines", err)
	}
	script := crossAgentInstallScript("/node/npm", "/prefix", e.NpmPackage+"@2.0.0", e.CommandName)
	if !strings.Contains(script, "--engine-strict=true --force=false") {
		t.Fatal("actual package engines not enforced")
	}
}

func TestCLIInstalledRuntimeUsesSelectedPackageMetadata(t *testing.T) {
	e := mustAgent(t, "claude-code")
	record := agentInstallation{Prefix: t.TempDir()}
	install := agentNpmCLIInstall(e)
	path := filepath.Join(install.packageRoot(record.Prefix), install.packageName, "package.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ engine, want string }{{">=18.0.0", "18.0.0"}, {">=22", "22.0.0"}, {"^20 || >=22", ""}} {
		if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"name":"%s","engines":{"node":"%s"}}`, e.NpmPackage, c.engine)), 0600); err != nil {
			t.Fatal(err)
		}
		gotEntry, _ := installedRuntimeEntry(e, record)
		if got := gotEntry.MinRuntimeVersion; got != c.want {
			t.Fatalf("%s: %s", c.engine, got)
		}
	}
}

func TestCLIClaudeNativeDiscovery(t *testing.T) {
	e := mustAgent(t, "claude-code")
	paths := platformFallbackCandidates(e)
	if !strings.Contains(strings.Join(paths, "\n"), ".local") {
		t.Fatal("native Claude installation omitted")
	}
}

func TestCodeBuddySuggestedModels(t *testing.T) {
	output := "  --model <model>    Model for the current session. Please provide the model ID. Currently supported: (hy4free, hy4std, hy4free, invalid id)\n --fallback-model <model> fallback"
	got := parseNativeModelCatalog("codebuddy", output)
	if len(got) != 2 || got[0].ID != "hy4free" || got[1].ID != "hy4std" {
		t.Fatalf("choices %+v", got)
	}
	if len(parseNativeModelCatalog("codebuddy", "Other models (sonnet, opus)")) != 0 {
		t.Fatal("unrelated help accepted")
	}
}

func TestCLIInstalledIsolatedCursorStatus(t *testing.T) {
	path := os.Getenv("MILEVIA_TEST_CURSOR_BINARY")
	if path == "" {
		t.Skip("explicit isolated CLI probe")
	}
	home := t.TempDir()
	for _, name := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "XDG_CONFIG_HOME", "XDG_DATA_HOME"} {
		t.Setenv(name, home)
	}
	for _, name := range []string{"CURSOR_API_KEY", "CURSOR_AUTH_TOKEN"} {
		t.Setenv(name, "")
	}
	e := mustAgent(t, "cursor")
	state, err := localCLIAuthStatus(context.Background(), e, path)
	if err != nil || state != agentAuthLoggedOut {
		t.Fatalf("isolated installed Cursor: %s %v", state, err)
	}
}

func TestCLIInstalledReadOnlyManagement(t *testing.T) {
	if os.Getenv("MILEVIA_TEST_CLI_MANAGEMENT") != "1" {
		t.Skip("explicit installed CLI read-only probes")
	}
	for _, id := range []string{"claude-code", "codex"} {
		e := mustAgent(t, id)
		state, err := localCLIAuthStatus(context.Background(), e, e.CommandName)
		if err != nil || state == agentAuthUnreadable {
			t.Fatalf("%s: %s %v", id, state, err)
		}
		t.Logf("%s authentication state: %s", id, state)
	}
	e := mustAgent(t, "codebuddy")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := codebuddyCommand(ctx, (&codebuddyCLIRunner{paths: newAgentPathResolver(Config{})}).codebuddyBinary(), "--help")
	out, _, err := runProbeCommandStreamsLimit(ctx, cmd, 1<<20)
	if err != nil || len(parseNativeModelCatalog(e.ID, string(out))) == 0 {
		t.Fatal("installed CodeBuddy model suggestions unavailable")
	}
}

func TestCLIManagementCursorKeepsSessionBoundary(t *testing.T) {
	s := &Server{runnerRegistry: newRunnerRegistry()}
	shell := newFakeCrossShell()
	shell.outputs["cursor-agent"] = "2026.11.05-86047df"
	meta := RunnerMeta{ID: "ssh-management", Environment: "remote-linux"}
	s.runnerRegistry.register(meta.ID, &crossShellOnlyRunner{shell}, meta)
	entry := mustAgent(t, "cursor")
	backend, reason := s.agentManagementBackend(meta, entry)
	if backend == nil || reason != "" || backend.Version(context.Background()) != "2026.11.05-86047df" {
		t.Fatalf("management: %v %s", backend, reason)
	}
	if backend, _ := s.agentBackend(meta, entry); backend != nil {
		t.Fatal("management opened remote sessions")
	}
	if status := s.probeAgent(context.Background(), meta, entry); status.Status != agentStatusUnsupported {
		t.Fatal("session catalog advertises remote Cursor")
	}
}

func TestCLIManagementCursorDiagnosisUsesManagementBackend(t *testing.T) {
	s := newInstallTestServer(t)
	s.runnerRegistry = newRunnerRegistry()
	shell := newFakeCrossShell()
	shell.outputs["cursor-agent"] = "2026.11.05-86047df"
	meta := RunnerMeta{ID: "ssh-management", Environment: "remote-linux"}
	s.runnerRegistry.register(meta.ID, &crossShellOnlyRunner{shell}, meta)
	report := s.buildCrossAgentDiagnosis(context.Background(), meta, mustAgent(t, "cursor"))
	if report.Status != diagnosisOK || report.Version != "2026.11.05-86047df" {
		t.Fatalf("management diagnosis: %+v", report)
	}
}

func TestCLIUpdateUnknownHealthNeverUsesCacheOrRollsBack(t *testing.T) {
	for _, updateErr := range []error{nil, errors.New("update failed")} {
		calls, runs := 0, []string{}
		version := func(context.Context) (string, error) {
			calls++
			if calls == 1 {
				return "1.2.3", nil
			}
			return "", errors.New("probe unknown")
		}
		run := func(_ context.Context, script string) (string, error) {
			runs = append(runs, script)
			if strings.Contains(script, "npm prefix -g") {
				return "/fixture", nil
			}
			return "", updateErr
		}
		_, current, err := runCrossCLIUpdateChecked(context.Background(), "fixture", "tool", []string{"update"}, "Tool", npmCLIInstall{commandName: "tool"}, run, version)
		if err == nil || current != "" || !strings.Contains(err.Error(), "未自动回滚") {
			t.Fatalf("unknown health accepted: %s %v", current, err)
		}
		for _, script := range runs {
			if strings.Contains(script, "install -g") {
				t.Fatal("unknown triggered rollback")
			}
		}
	}
}

func TestCLIUpdatePreflightFailureMakesNoChanges(t *testing.T) {
	runs := 0
	_, _, err := runCrossCLIUpdateChecked(context.Background(), "fixture", "tool", []string{"update"}, "Tool", npmCLIInstall{},
		func(context.Context, string) (string, error) { runs++; return "", nil },
		func(context.Context) (string, error) { return "", errors.New("probe failed") })
	if err == nil || runs != 0 {
		t.Fatalf("preflight allowed mutation: %v %d", err, runs)
	}
}

func TestCLIUpdateRollbackMustVerifyVersion(t *testing.T) {
	calls := 0
	version := func(context.Context) (string, error) {
		calls++
		if calls == 1 {
			return "", nil
		}
		return "", errors.New("probe failed")
	}
	_, current, err := finishCrossNpmUpdateChecked("1.2.3", "Tool", version,
		func(context.Context, string) (string, error) { return "", nil }, errors.New("update failed"), remoteNpmCLIRecovery{}, nil)
	if err == nil || current != "" || !strings.Contains(err.Error(), "已执行回滚但无法确认版本") {
		t.Fatalf("rollback falsely verified %s %v", current, err)
	}
}

func TestCLIManagementCursorUpdateUsesInstalledAlias(t *testing.T) {
	shell := newFakeCrossShell()
	shell.outputs["agent"] = "2026.11.04-old"
	shell.runFunc = func(script string) (string, error) {
		if script != "'agent' 'update'" {
			t.Fatalf("wrong native update %s", script)
		}
		shell.mu.Lock()
		shell.outputs["agent"] = "2026.11.05-new"
		shell.mu.Unlock()
		return "", nil
	}
	b := &catalogAgentBackend{entry: mustAgent(t, "cursor"), shell: shell}
	previous, current, err := b.Update(context.Background())
	if err != nil || previous != "2026.11.04-old" || current != "2026.11.05-new" {
		t.Fatalf("alias update %s %s %v", previous, current, err)
	}
}

func TestCursorModelFormat(t *testing.T) {
	out := "warning\nAvailable models\n\nmodel-a - Model A (current, default)\nmodel-b - Model B\nwarning\ninvalid id - Nope\nTip: use --model\nmodel-c - Ignore"
	got := parseNativeModelCatalog("cursor", out)
	if len(got) != 2 || got[0].ID != "model-a" || got[1].ID != "model-b" {
		t.Fatalf("models %+v", got)
	}
	if got := parseNativeModelCatalog("cursor", "Authentication required\nmodel-a - Fake"); len(got) != 0 {
		t.Fatal("diagnostic accepted as models")
	}
}

func TestCursorLoginURLAndBoundedOutput(t *testing.T) {
	for _, url := range []string{"http://cursor.com/login", "https://cursor.com.evil.test/login", "https://evilcursor.com/login", "https://user@cursor.com/login", "https://cursor.com:123/login", "https://cursor.com/login#fragment"} {
		if cursorAuthURL(url) != "" {
			t.Fatalf("accepted %s", url)
		}
	}
	b := &browserLogin{ready: make(chan struct{})}
	w := &browserLoginOutput{owner: b}
	_, _ = w.Write([]byte(strings.Repeat("x", 20000)))
	if len(w.tail) > 16384 {
		t.Fatal("unbounded output")
	}
	_, _ = w.Write([]byte("\nhttps://cursor.com/login?code=fixture"))
	if b.url != "" {
		t.Fatal("published partial URL")
	}
	_, _ = w.Write([]byte("\n"))
	if b.url != "https://cursor.com/login?code=fixture" {
		t.Fatal("URL lost")
	}
}

func TestCursorBrowserLoginLifecycle(t *testing.T) {
	if os.Getenv("MILEVIA_BROWSER_LOGIN_FIXTURE") == "1" {
		fmt.Println("https://cursor.com/login?code=fixture")
		time.Sleep(30 * time.Second)
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b := &browserLogin{}
	defer b.stop()
	count := 0
	var dir string
	makeCommand := func(ctx context.Context) *exec.Cmd {
		count++
		cmd := exec.CommandContext(ctx, exe, "-test.run=^TestCursorBrowserLoginLifecycle$")
		cmd.Env = append(os.Environ(), "MILEVIA_BROWSER_LOGIN_FIXTURE=1")
		return cmd
	}
	info, err := b.start(context.Background(), makeCommand)
	if err != nil || info.AuthURL == "" {
		t.Fatalf("start %v %+v", err, info)
	}
	b.mu.Lock()
	dir = b.cmd.Dir
	b.mu.Unlock()
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = b.start(context.Background(), makeCommand) }()
	}
	wg.Wait()
	if count != 1 {
		t.Fatalf("duplicate authorization: %d", count)
	}
	b.stop()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("temp dir retained: %v", err)
	}
	if _, err := b.start(context.Background(), makeCommand); err == nil {
		t.Fatal("closed controller restarted")
	}
}
