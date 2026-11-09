package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const authOutputLimit = 64 * 1024

var authANSI = regexp.MustCompile("\x1b\\[[0-?]*[ -/]*[@-~]")
var authCount = regexp.MustCompile(`(?im)(?:^|[│└┌●	 ])([0-9]+) (credentials?|environment variables?)[	 ]*$`)

func cleanAuthOutput(output string) string {
	return strings.TrimSpace(authANSI.ReplaceAllString(strings.ReplaceAll(output, "\r", ""), ""))
}

// Only explicit positive evidence establishes an account session. Local key
// records (including OAuth records without an identity check) are configured.
func parseCLIAuthState(kind, stdout, stderr string, exitCode int) agentAuthState {
	if len(stdout) > authOutputLimit || len(stderr) > authOutputLimit || (exitCode != 0 && exitCode != 1) {
		return agentAuthUnreadable
	}
	stdout, stderr = cleanAuthOutput(stdout), cleanAuthOutput(stderr)
	switch kind {
	case "claude-json":
		var v struct {
			LoggedIn *bool  `json:"loggedIn"`
			Method   string `json:"authMethod"`
		}
		if json.Unmarshal([]byte(stdout), &v) != nil || v.LoggedIn == nil {
			return agentAuthUnreadable
		}
		switch v.Method {
		case "api_key", "api_key_helper", "third_party", "oauth_token":
			// An externally supplied token does not prove an account session.
			if *v.LoggedIn && exitCode == 0 {
				return agentAuthConfigured
			}
		case "claude.ai":
			if *v.LoggedIn && exitCode == 0 {
				return agentAuthLoggedIn
			}
		case "none", "":
			if !*v.LoggedIn {
				return agentAuthLoggedOut
			}
		}
		return agentAuthUnreadable
	case "qoder-json":
		var v struct {
			LoggedIn    *bool  `json:"logged_in"`
			AuthSource  string `json:"auth_source"`
			LoginMethod string `json:"login_method"`
		}
		if json.Unmarshal([]byte(stdout), &v) == nil && v.LoggedIn != nil {
			if !*v.LoggedIn {
				return agentAuthLoggedOut
			}
			if exitCode != 0 {
				return agentAuthUnreadable
			}
			if tokenAuthSource(v.AuthSource + " " + v.LoginMethod) {
				return agentAuthConfigured
			}
			return agentAuthLoggedIn
		}
		if exitCode == 0 {
			return qoderAuthState(stdout)
		}
		return agentAuthUnreadable
	case "opencode-list":
		if exitCode != 0 {
			return agentAuthUnreadable
		}
		counts := authCount.FindAllStringSubmatch(stdout, -1)
		if len(counts) == 0 {
			return agentAuthUnreadable
		}
		for _, c := range counts {
			n, err := strconv.Atoi(c[1])
			if err != nil {
				return agentAuthUnreadable
			}
			if n > 0 {
				return agentAuthConfigured
			}
		}
		return agentAuthLoggedOut
	case "cursor-json":
		if exitCode != 0 {
			return agentAuthUnreadable
		}
		var v struct {
			Status        string `json:"status"`
			Authenticated *bool  `json:"isAuthenticated"`
			Access        *bool  `json:"hasAccessToken"`
			Refresh       *bool  `json:"hasRefreshToken"`
			User          struct {
				Email string `json:"email"`
				ID    string `json:"userId"`
			} `json:"userInfo"`
		}
		if json.Unmarshal([]byte(stdout), &v) != nil {
			return parseCLIAuthState("cursor-text", stdout, stderr, exitCode)
		}
		if v.Authenticated == nil || v.Access == nil || v.Refresh == nil {
			return agentAuthUnreadable
		}
		if v.Status == "authenticated" && *v.Authenticated && *v.Access && *v.Refresh {
			if v.User.Email != "" || v.User.ID != "" {
				return agentAuthLoggedIn
			}
			return agentAuthConfigured
		}
		if v.Status == "partially-authenticated" && *v.Access {
			return agentAuthConfigured
		}
		if v.Status == "unauthenticated" && !*v.Authenticated && !*v.Access && !*v.Refresh {
			return agentAuthLoggedOut
		}
		return agentAuthUnreadable
	case "codex-text", "cursor-text":
		for _, line := range strings.Split(stdout+"\n"+stderr, "\n") {
			line = strings.ToLower(strings.Trim(strings.TrimSpace(line), "│●✓✔ "))
			if line == "not logged in" || line == "not authenticated" || line == "logged out" || line == "not signed in" || line == "not logged in. run 'agent login' to authenticate." {
				return agentAuthLoggedOut
			}
			if exitCode != 0 {
				continue
			}
			if strings.HasPrefix(line, "logged in using an api key") || strings.HasPrefix(line, "authenticated using api key") || strings.HasPrefix(line, "logged in with api key") {
				return agentAuthConfigured
			}
			if line == "logged in using chatgpt" || strings.HasPrefix(line, "logged in as ") || strings.HasPrefix(line, "authenticated as ") {
				return agentAuthLoggedIn
			}
		}
	}
	return agentAuthUnreadable
}

func tokenAuthSource(source string) bool {
	s := strings.ToLower(source)
	return strings.Contains(s, "token") || strings.Contains(s, "api_key") || strings.Contains(s, "api key") || strings.Contains(s, "pat") || strings.Contains(s, "environment")
}

func authProbeResult(entry AgentCatalogEntry, stdout, stderr string, exitCode int) (agentAuthState, error) {
	if exitCode == 0 && stdout == "__MILEVIA_CONFIGURED__" {
		return agentAuthConfigured, nil
	}
	state := parseCLIAuthState(entry.AuthStatusKind, stdout, stderr, exitCode)
	if state == agentAuthUnreadable {
		// Never put account data, keys or raw process errors in the HTTP response.
		return state, errors.New("认证查询未返回可识别状态，请检查 CLI 版本与目标环境")
	}
	return state, nil
}

func localCLIAuthStatus(parent context.Context, entry AgentCatalogEntry, binary string) (agentAuthState, error) {
	for _, name := range authEnvironmentNames(entry) {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return agentAuthConfigured, nil
		}
	}
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("", "milevia-auth-cli-")
	if err != nil {
		return agentAuthUnreadable, errors.New("无法创建认证查询目录")
	}
	defer os.RemoveAll(dir)
	run := func(args []string) (string, string, int) {
		resolved, err := exec.LookPath(binary)
		if err != nil {
			return "", "", -1
		}
		cmd := codexCommandContext(ctx, resolved, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "DISABLE_AUTOUPDATER=1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "OPENCODE_DISABLE_AUTOUPDATE=true", "NO_COLOR=1")
		stdout, stderr, err := runProbeCommandStreamsLimit(ctx, cmd, authOutputLimit)
		code := 0
		if err != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) || ctx.Err() != nil {
				return "", "", -1
			}
			code = ee.ExitCode()
		}
		return string(stdout), string(stderr), code
	}
	out, errOut, code := run(entry.AuthStatusArgs)
	if entry.AuthStatusKind == "qoder-json" && qoderNeedsTextFallback(out, errOut) && ctx.Err() == nil {
		out, errOut, code = run([]string{"status"})
	}
	if entry.AuthStatusKind == "cursor-json" && qoderNeedsTextFallback(out, errOut) && ctx.Err() == nil {
		out, errOut, code = run([]string{"status"})
	}
	return authProbeResult(entry, out, errOut, code)
}

func authEnvironmentNames(entry AgentCatalogEntry) []string {
	switch entry.AuthStatusKind {
	case "qoder-json":
		return []string{"QODER_PERSONAL_ACCESS_TOKEN"}
	case "cursor-json":
		return []string{"CURSOR_API_KEY", "CURSOR_AUTH_TOKEN"}
	}
	return nil
}

func qoderNeedsTextFallback(stdout, stderr string) bool {
	s := strings.ToLower(stdout + "\n" + stderr)
	return strings.Contains(s, "unknown option") || strings.Contains(s, "unknown argument") || strings.Contains(s, "unsupported output format")
}

// Cross probes return both streams plus an exit code even for the normal
// logged-out exit 1. GNU timeout bounds the target-side process tree; output
// files are private and bounded before being brought back to the server.
func crossAuthCommand(shell crossShellRunner, entry AgentCatalogEntry, args []string) string {
	var alternatives []string
	for _, name := range agentCommandNames(entry) {
		parts := []string{shellQuote(name)}
		for _, arg := range args {
			parts = append(parts, shellQuote(arg))
		}
		command := strings.Join(parts, " ")
		if _, wsl := shell.(*wslAgentRunner); wsl {
			// Skip a Windows shim and still try a native alias on this target.
			alternatives = append(alternatives, "auth_cmd=$(command -v "+shellQuote(name)+" 2>/dev/null || true)\ncase \"$auth_cmd\" in \"\"|/mnt/*) ;; *) "+command+"; exit $?;; esac")
			continue
		}
		alternatives = append(alternatives, "if command -v "+shellQuote(name)+" >/dev/null 2>&1; then "+command+"; exit $?; fi")
	}
	command := strings.Join(alternatives, "\n") + "\nexit 127"
	for _, name := range authEnvironmentNames(entry) {
		command = "if [ -n \"$" + name + "\" ]; then printf '%s' '__MILEVIA_CONFIGURED__'; exit 0; fi\n" + command
	}
	return `set +e
umask 077
command -v timeout >/dev/null 2>&1 || exit 125
auth_dir=$(mktemp -d) || exit 125
auth_pid=
auth_cleanup() {
 if [ -n "$auth_pid" ]; then
  kill -TERM "$auth_pid" 2>/dev/null
  wait "$auth_pid" 2>/dev/null
  kill -KILL -"$auth_pid" 2>/dev/null
 fi
 rm -rf -- "$auth_dir"
}
trap auth_cleanup EXIT
trap 'exit 125' HUP INT TERM
cd "$auth_dir" || exit 125
ulimit -f 128
DISABLE_AUTOUPDATER=1 CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 OPENCODE_DISABLE_AUTOUPDATE=true NO_COLOR=1 timeout -k 1s 16s sh -c ` + shellQuote(command) + ` >out 2>err &
auth_pid=$!
wait "$auth_pid"
auth_code=$?
kill -KILL -"$auth_pid" 2>/dev/null
auth_pid=
head -c 65537 "$auth_dir/out"
printf '\n__MILEVIA_AUTH_STDERR__\n'
head -c 65537 "$auth_dir/err"
printf '\n__MILEVIA_AUTH_CODE__='
printf '%s\n' "$auth_code"
`
}

func crossCLIAuthStatus(parent context.Context, entry AgentCatalogEntry, shell crossShellRunner) (agentAuthState, error) {
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	run := func(args []string) (string, string, int) {
		out, err := shell.crossRun(ctx, crossAuthCommand(shell, entry, args))
		if err != nil || len(out) > 2*authOutputLimit+512 {
			return "", "", -1
		}
		p := strings.LastIndex(out, "\n__MILEVIA_AUTH_CODE__=")
		if p < 0 {
			return "", "", -1
		}
		code, err := strconv.Atoi(strings.TrimSpace(out[p+len("\n__MILEVIA_AUTH_CODE__="):]))
		stdout, stderr, found := strings.Cut(out[:p], "\n__MILEVIA_AUTH_STDERR__\n")
		if err != nil || !found {
			return "", "", -1
		}
		return stdout, stderr, code
	}
	out, errOut, code := run(entry.AuthStatusArgs)
	if entry.AuthStatusKind == "qoder-json" && qoderNeedsTextFallback(out, errOut) && ctx.Err() == nil {
		out, errOut, code = run([]string{"status"})
	}
	if entry.AuthStatusKind == "cursor-json" && qoderNeedsTextFallback(out, errOut) && ctx.Err() == nil {
		out, errOut, code = run([]string{"status"})
	}
	return authProbeResult(entry, out, errOut, code)
}

func (r *claudeCLIRunner) LoginStatus(ctx context.Context) (agentAuthState, error) {
	e, _ := agentByID("claude-code")
	return localCLIAuthStatus(ctx, e, r.claudeBinary())
}
func (r *codexCLIRunner) LoginStatus(ctx context.Context) (agentAuthState, error) {
	e, _ := agentByID("codex")
	return localCLIAuthStatus(ctx, e, r.codexBinary())
}
func terminalLogin(ctx context.Context, runner AgentRunner) (agentLoginInfo, error) {
	if runner.Version(ctx) == "" {
		return agentLoginInfo{}, fmt.Errorf("目标 CLI 未安装或不可执行")
	}
	return agentLoginInfo{}, nil
}
func (r *claudeCLIRunner) Login(ctx context.Context) (agentLoginInfo, error) {
	return terminalLogin(ctx, r)
}
func (r *codexCLIRunner) Login(ctx context.Context) (agentLoginInfo, error) {
	return terminalLogin(ctx, r)
}
func (r *wslAgentRunner) LoginStatus(ctx context.Context) (agentAuthState, error) {
	e, _ := agentByID("claude-code")
	return crossCLIAuthStatus(ctx, e, r)
}
func (r *sshRunner) LoginStatus(ctx context.Context) (agentAuthState, error) {
	e, _ := agentByID("claude-code")
	return crossCLIAuthStatus(ctx, e, r)
}
func (r *wslAgentRunner) Login(ctx context.Context) (agentLoginInfo, error) {
	return terminalLogin(ctx, r)
}
func (r *sshRunner) Login(ctx context.Context) (agentLoginInfo, error) { return terminalLogin(ctx, r) }
func (a codexRunnerAdapter) LoginStatus(ctx context.Context) (agentAuthState, error) {
	shell, ok := a.inner.(crossShellRunner)
	if !ok {
		return agentAuthUnreadable, errors.New("目标环境尚未接通认证查询")
	}
	e, _ := agentByID("codex")
	return crossCLIAuthStatus(ctx, e, shell)
}
func (a codexRunnerAdapter) Login(ctx context.Context) (agentLoginInfo, error) {
	return terminalLogin(ctx, a)
}
