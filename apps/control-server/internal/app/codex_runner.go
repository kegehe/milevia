package app

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const maxCodexDiagnosticBytes = 4 * 1024

const codexAdditionalStdinNotice = "Reading additional input from stdin..."

var (
	codexBearerPattern              = regexp.MustCompile(`(?i)\b(authorization\s*[:=]\s*)bearer\s+[^\s,;]+`)
	codexSensitiveAssignmentPattern = regexp.MustCompile(`(?i)\b([a-z0-9_.-]*(?:api[_-]?key|token|password|secret|authorization|credential)[a-z0-9_.-]*)\s*([:=])\s*[^\s,;]+`)
	codexHomePattern                = regexp.MustCompile(`(?i)\b(CODEX_HOME\s*[:=]\s*)[^\s,;]+`)
	codexAuthPathPattern            = regexp.MustCompile("(?i)(?:~|[a-z]:)?(?:[/\\\\][^ \\t\\r\\n\\\"'`/\\\\]+)*[/\\\\]\\.codex[/\\\\]auth\\.json")
	codexAPIKeyPattern              = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{8,}\b`)
)

// codexCLIRunner runs one non-interactive Codex turn per platform Run. Codex
// persists its own thread; the server stores the thread ID returned as JSONL.
type codexCLIRunner struct{ config Config }

// codexDefaultModelRunner 由能报告“CLI 默认模型”的 Codex runner 实现（本机 / WSL / SSH）。
// Codex 的 exec --json 事件流只含 token 用量、不含模型名，usage 追踪在 cli_managed
// （无档案模型）场景下只能从 CLI 自己的 config.toml 读取默认模型，见 seedRunUsageModel。
type codexDefaultModelRunner interface {
	codexDefaultModel(ctx context.Context) string
}

var codexProfileSkillsMu sync.Mutex

func newCodexCLIRunner(config Config) AgentRunner { return &codexCLIRunner{config: config} }

// codexCommandContext handles npm's Windows .cmd shim explicitly. CreateProcess
// cannot launch a batch file directly, while `codex` resolved through PATHEXT
// commonly points to codex.cmd.
func codexCommandContext(ctx context.Context, path string, args ...string) *exec.Cmd {
	if runtime.GOOS != "windows" {
		return exec.CommandContext(ctx, path, args...)
	}
	lower := strings.ToLower(path)
	if !strings.HasSuffix(lower, ".cmd") && !strings.HasSuffix(lower, ".bat") {
		return exec.CommandContext(ctx, path, args...)
	}
	// Pass the script path as the /c command and preserve each user argument as
	// a separate process argument. Building one hand-quoted command string would
	// mishandle prompts containing quotes or shell metacharacters.
	return exec.CommandContext(ctx, "cmd.exe", append([]string{"/d", "/c", `"` + path + `"`}, args...)...)
}

func (r *codexCLIRunner) Ready(parent context.Context) bool {
	// Readiness is an execution capability check, not an authentication check.
	// Codex may be authenticated by CC Switch, CODEX_HOME, environment
	// variables, or a managed API-key profile; all of those are valid without
	// `codex login status` succeeding.  The actual run will report auth errors
	// with the appropriate context if configuration is invalid.
	return r.BinaryReady()
}

// BinaryReady reports whether the Codex binary is present, independent of any
// CLI login state. A managed api_key profile injects its own credential, so it
// only needs the binary, not a persisted login.
func (r *codexCLIRunner) BinaryReady() bool {
	_, err := exec.LookPath(r.config.CodexPath)
	return err == nil
}

func (r *codexCLIRunner) Version(parent context.Context) string {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	cmd := codexCommandContext(ctx, r.config.CodexPath, "--version")
	configureProcessGroup(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), "codex-cli "))
}

func (r *codexCLIRunner) CheckUpdate(parent context.Context) (bool, string, error) {
	local := r.Version(parent)
	if local == "" {
		return false, "", errors.New("Codex CLI is not installed")
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "npm", "view", "@openai/codex", "version")
	configureProcessGroup(cmd)
	out, err := cmd.Output()
	if err != nil {
		return false, "", fmt.Errorf("query latest Codex version: %w", err)
	}
	latest := strings.TrimSpace(string(out))
	if latest == "" {
		return false, "", errors.New("latest Codex version is empty")
	}
	available, err := codexUpdateAvailable(local, latest)
	if err != nil {
		return false, latest, err
	}
	return available, latest, nil
}

type codexSemver struct {
	major int
	minor int
	patch int
	pre   []string
}

func codexUpdateAvailable(local, latest string) (bool, error) {
	localVersion, err := parseCodexSemver(local)
	if err != nil {
		return false, fmt.Errorf("parse local Codex version: %w", err)
	}
	latestVersion, err := parseCodexSemver(latest)
	if err != nil {
		return false, fmt.Errorf("parse latest Codex version: %w", err)
	}
	return compareCodexSemver(latestVersion, localVersion) > 0, nil
}

func parseCodexSemver(raw string) (codexSemver, error) {
	value := strings.TrimPrefix(strings.TrimSpace(raw), "v")
	value, _, _ = strings.Cut(value, "+")
	core, prerelease, hasPrerelease := strings.Cut(value, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return codexSemver{}, fmt.Errorf("invalid semantic version %q", raw)
	}
	parsed := codexSemver{}
	for index, target := range []*int{&parsed.major, &parsed.minor, &parsed.patch} {
		if parts[index] == "" || (len(parts[index]) > 1 && parts[index][0] == '0') {
			return codexSemver{}, fmt.Errorf("invalid semantic version %q", raw)
		}
		value, err := strconv.Atoi(parts[index])
		if err != nil || value < 0 {
			return codexSemver{}, fmt.Errorf("invalid semantic version %q", raw)
		}
		*target = value
	}
	if !hasPrerelease {
		return parsed, nil
	}
	if prerelease == "" {
		return codexSemver{}, fmt.Errorf("invalid semantic version %q", raw)
	}
	for _, identifier := range strings.Split(prerelease, ".") {
		if identifier == "" {
			return codexSemver{}, fmt.Errorf("invalid semantic version %q", raw)
		}
		for _, character := range identifier {
			if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || character == '-') {
				return codexSemver{}, fmt.Errorf("invalid semantic version %q", raw)
			}
		}
		if _, err := strconv.Atoi(identifier); err == nil && len(identifier) > 1 && identifier[0] == '0' {
			return codexSemver{}, fmt.Errorf("invalid semantic version %q", raw)
		}
		parsed.pre = append(parsed.pre, identifier)
	}
	return parsed, nil
}

func compareCodexSemver(left, right codexSemver) int {
	for _, pair := range [][2]int{{left.major, right.major}, {left.minor, right.minor}, {left.patch, right.patch}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	if len(left.pre) == 0 && len(right.pre) > 0 {
		return 1
	}
	if len(left.pre) > 0 && len(right.pre) == 0 {
		return -1
	}
	for index := 0; index < len(left.pre) && index < len(right.pre); index++ {
		leftNumber, leftErr := strconv.Atoi(left.pre[index])
		rightNumber, rightErr := strconv.Atoi(right.pre[index])
		if leftErr == nil && rightErr != nil {
			return -1
		}
		if leftErr != nil && rightErr == nil {
			return 1
		}
		if leftErr == nil && rightErr == nil {
			if leftNumber < rightNumber {
				return -1
			}
			if leftNumber > rightNumber {
				return 1
			}
			continue
		}
		if left.pre[index] < right.pre[index] {
			return -1
		}
		if left.pre[index] > right.pre[index] {
			return 1
		}
	}
	if len(left.pre) < len(right.pre) {
		return -1
	}
	if len(left.pre) > len(right.pre) {
		return 1
	}
	return 0
}

func (r *codexCLIRunner) Update(parent context.Context) (string, string, error) {
	previous := r.Version(parent)
	if previous == "" {
		return "", "", errors.New("Codex CLI is not installed")
	}
	recovery, recoveryErr := prepareNpmCLIRecovery(parent, r.config.CodexPath, codexNpmCLIInstall)
	ctx, cancel := context.WithTimeout(parent, r.config.agentUpdateTimeout())
	defer cancel()
	var out bytes.Buffer
	cmd := codexCommandContext(ctx, r.config.CodexPath, "update")
	configureProcessGroup(cmd)
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return r.finishFailedUpdate(previous, fmt.Errorf("update Codex 失败：%w%s", err, updateOutputDetail(out.String())), recovery, recoveryErr)
	}
	current := r.Version(context.Background())
	if current == "" {
		return r.finishFailedUpdate(previous, errors.New("update Codex 失败：更新后 Codex 未通过健康检查"), recovery, recoveryErr)
	}
	return previous, current, nil
}

func (r *codexCLIRunner) finishFailedUpdate(previous string, updateErr error, recovery npmCLIRecovery, recoveryErr error) (string, string, error) {
	if r.Version(context.Background()) != "" {
		return previous, "", updateErr
	}
	if recoveryErr != nil {
		return previous, "", fmt.Errorf("%w；自动回滚不可用：%v", updateErr, recoveryErr)
	}
	recovered, err := rollbackInterruptedNpmInstall(recovery.prefix, previous, recovery.install)
	if err != nil {
		return previous, "", fmt.Errorf("%w；自动回滚失败：%v", updateErr, err)
	}
	if current := r.Version(context.Background()); current != recovered {
		return previous, "", fmt.Errorf("%w；自动回滚失败：rollback health check failed (version %q)", updateErr, current)
	}
	return previous, recovered, fmt.Errorf("%w；已自动回滚到 Codex %s", updateErr, recovered)
}

var codexNpmCLIInstall = npmCLIInstall{
	scope: "@openai", packageName: "codex", commandName: "codex", binFile: "codex.js",
}

func rollbackInterruptedNpmCodexInstall(prefix, previous string) (string, error) {
	return rollbackInterruptedNpmInstall(prefix, previous, codexNpmCLIInstall)
}

func (r *codexCLIRunner) Run(ctx context.Context, request AgentRunRequest, sink AgentRunSink) error {
	policy, err := codexSandbox(request.PermissionMode)
	if err != nil {
		return err
	}
	args := []string{"exec"}
	var schemaPath string
	if len(request.OutputSchema) > 0 {
		var cleanup func()
		schemaPath, cleanup, err = writeCodexOutputSchema(request.OutputSchema)
		if err != nil {
			return err
		}
		defer cleanup()
		args = append(args, "--output-schema", schemaPath)
	}
	profileArgs, environment, closeProfile, err := r.profileLaunch(ctx, request.Profile)
	if err != nil {
		return err
	}
	defer closeProfile()
	args = append(args, profileArgs...)
	// MCP 注入：Codex 没有 --mcp-config，改由 -c 点号路径逐 server 注入；密钥只写变量名
	// （env_vars / env_http_headers），真值随进程环境提供，因此 argv 与配置文件都不含明文。
	args = append(args, request.CodexMCPArgs...)
	if len(request.MCPEnv) > 0 {
		environment = append(environment, request.MCPEnv...)
	}
	if request.Profile != nil && request.Profile.Model != "" {
		args = append(args, "-c", fmt.Sprintf("model=%q", request.Profile.Model))
	}
	if request.Resume {
		args = append(args, "resume", "-c", fmt.Sprintf("sandbox_mode=%q", policy), "--json", request.SessionID, request.Prompt)
	} else {
		args = append(args, "-c", fmt.Sprintf("sandbox_mode=%q", policy), "--json", "--color", "never", "-C", request.ProjectPath, "--sandbox", policy, request.Prompt)
	}
	cmd := codexCommandContext(context.Background(), r.config.CodexPath, args...)
	cmd.Dir = request.ProjectPath
	cmd.Env = environment
	configureProcessGroup(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open Codex stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("open Codex stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start Codex: %w", err)
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			terminateProcessGroup(cmd)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				forceTerminateProcessGroup(cmd)
			}
		case <-done:
		}
	}()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); readCodexJSONL(stdout, sink, request.ProjectPath) }()
	go func() { defer wg.Done(); readCodexStderr(stderr, sink) }()
	wg.Wait()
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("Codex exited: %w", err)
	}
	return nil
}

func writeCodexOutputSchema(input json.RawMessage) (string, func(), error) {
	schema, err := codexOutputSchema(input)
	if err != nil {
		return "", func() {}, err
	}
	file, err := os.CreateTemp("", "milevia-codex-schema-*.json")
	if err != nil {
		return "", func() {}, fmt.Errorf("create Codex output schema: %w", err)
	}
	path := file.Name()
	cleanup := func() { _ = os.Remove(path) }
	if _, err := file.Write(schema); err != nil {
		file.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("write Codex output schema: %w", err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("close Codex output schema: %w", err)
	}
	return path, cleanup, nil
}

// codexOutputSchema adapts the shared insight contracts for Codex's structured
// output API, whose response schema must have an object at its root. The
// insight parser already accepts the equivalent {"findings": [...]} envelope.
func codexOutputSchema(schema json.RawMessage) (json.RawMessage, error) {
	if !json.Valid(schema) {
		return nil, errors.New("invalid Codex output schema")
	}
	trimmed := bytes.TrimSpace(schema)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errors.New("Codex output schema root must be a JSON object")
	}
	var root struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(schema, &root); err != nil {
		return nil, errors.New("invalid Codex output schema")
	}
	if root.Type == "" || root.Type == "object" {
		return schema, nil
	}
	if root.Type != "array" {
		return nil, errors.New("Codex output schema root type must be object or array")
	}
	wrapped, err := json.Marshal(map[string]any{
		"type":                 "object",
		"properties":           map[string]json.RawMessage{"findings": schema},
		"required":             []string{"findings"},
		"additionalProperties": false,
	})
	if err != nil {
		return nil, fmt.Errorf("wrap Codex output schema: %w", err)
	}
	return wrapped, nil
}

func (r *codexCLIRunner) profileLaunch(ctx context.Context, profile *AgentRuntimeProfile) ([]string, []string, func(), error) {
	return r.profileLaunchWithSkills(ctx, profile, codexUserSkillsDir())
}

func (r *codexCLIRunner) profileLaunchWithSkills(_ context.Context, profile *AgentRuntimeProfile, skillsSource string) ([]string, []string, func(), error) {
	if strings.TrimSpace(skillsSource) == "" {
		skillsSource = codexUserSkillsDir()
	}
	environment := managedCLIEnvironment(profile, os.Environ())
	if profile == nil || profile.AuthMode != "api_key" {
		return nil, environment, func() {}, nil
	}
	// Always isolate CODEX_HOME for a managed key, including the official
	// endpoint. Otherwise a user's persisted model_provider could redirect this
	// process and its managed key to a different endpoint.
	dir, err := r.codexProfileHome(profile)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create isolated Codex provider directory: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, nil, fmt.Errorf("create isolated Codex provider directory: %w", err)
	}
	if err := provisionCodexProfileSkills(skillsSource, dir); err != nil {
		return nil, nil, nil, fmt.Errorf("make Codex skills available to isolated provider: %w", err)
	}
	// Codex uses this environment for provider authentication, but tool commands
	// must never inherit the managed API key.
	config := "[shell_environment_policy]\nexclude = [\"OPENAI_API_KEY\"]\n"
	args := []string{"-c", `shell_environment_policy.exclude=["OPENAI_API_KEY"]`}
	if profile.BaseURL != "" {
		// Codex custom providers are configuration-backed, not a generic
		// OPENAI_BASE_URL substitution. Keep the provider name and wire protocol
		// under our control.
		// The managed endpoint currently exposes the Responses HTTP stream but
		// does not provide a stable WebSocket upgrade path. Explicitly disable
		// WebSockets so a dropped upgrade cannot consume the turn retry budget
		// before falling back to HTTPS.
		config += "\nmodel_provider = \"milevia\"\n\n[model_providers.milevia]\nname = \"Milevia managed provider\"\nbase_url = \"" + strings.ReplaceAll(profile.BaseURL, "\"", "") + "\"\nwire_api = \"responses\"\nenv_key = \"OPENAI_API_KEY\"\nsupports_websockets = false\nstream_max_retries = 2\nrequest_max_retries = 2\n"
		args = append([]string{"-c", `model_provider="milevia"`}, args...)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(config), 0o600); err != nil {
		return nil, nil, nil, fmt.Errorf("write isolated Codex provider config: %w", err)
	}
	environment = managedCLIEnvironment(profile, environment, "CODEX_HOME="+dir)
	// This directory must survive the Run: Codex stores the thread rollout here,
	// and a later `exec resume` needs to see the same files. It is scoped by the
	// immutable profile revision, so credentials/configuration changes get a new
	// isolated home without mixing threads between profiles.
	return args, environment, func() {}, nil
}

// provisionCodexProfileSkills exposes the user's skills to a managed-profile
// CODEX_HOME without inheriting its config or authentication files. A symlink
// keeps the profile in sync with skill installs; on Windows installations that
// disallow symlink creation, a regular-file copy provides the same layout.
func provisionCodexProfileSkills(source, profileHome string) error {
	if sourceAbs, sourceErr := filepath.Abs(source); sourceErr == nil {
		if targetAbs, targetErr := filepath.Abs(filepath.Join(profileHome, "skills")); targetErr == nil && filepath.Clean(sourceAbs) == filepath.Clean(targetAbs) {
			return nil
		}
	}
	target := filepath.Join(profileHome, "skills")
	info, err := os.Stat(source)
	if errors.Is(err, os.ErrNotExist) {
		codexProfileSkillsMu.Lock()
		defer codexProfileSkillsMu.Unlock()
		if removeErr := os.RemoveAll(target); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return removeErr
		}
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("Codex skills path is not a directory: %s", source)
	}

	codexProfileSkillsMu.Lock()
	defer codexProfileSkillsMu.Unlock()
	if targetInfo, err := os.Lstat(target); err == nil {
		if targetInfo.Mode()&os.ModeSymlink != 0 {
			resolved, resolveErr := filepath.EvalSymlinks(target)
			if resolveErr == nil && !isWSLUncPath(source) {
				resolved, _ = filepath.Abs(resolved)
				expected, _ := filepath.Abs(source)
				if filepath.Clean(resolved) == filepath.Clean(expected) {
					return nil
				}
			}
			if err := os.Remove(target); err != nil {
				return err
			}
		} else if !targetInfo.IsDir() {
			if err := os.Remove(target); err != nil {
				return err
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !isWSLUncPath(source) {
		if _, err := os.Lstat(target); errors.Is(err, os.ErrNotExist) {
			if err := os.Symlink(source, target); err == nil {
				return nil
			}
		} else if err != nil {
			return err
		}
	}
	if err := syncCodexSkillTree(source, target); err != nil {
		_ = os.RemoveAll(target)
		return err
	}
	return nil
}

func syncCodexSkillTree(source, target string) error {
	if sourceAbs, sourceErr := filepath.Abs(source); sourceErr == nil {
		if targetAbs, targetErr := filepath.Abs(target); targetErr == nil && filepath.Clean(sourceAbs) == filepath.Clean(targetAbs) {
			return nil
		}
	}
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if existing, statErr := os.Lstat(target); statErr == nil && existing.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("Codex skills target must not be a symbolic link: %s", target)
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if err := os.MkdirAll(target, info.Mode().Perm()); err != nil {
		return err
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		sourcePath := filepath.Join(source, entry.Name())
		targetPath := filepath.Join(target, entry.Name())
		if entry.Type()&os.ModeSymlink != 0 {
			// Do not reproduce links from a user skill tree, but do remove a
			// stale copy from an earlier sync.
			if err := os.RemoveAll(targetPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			continue
		}
		if entry.IsDir() {
			if existing, statErr := os.Lstat(targetPath); statErr == nil {
				if existing.Mode()&os.ModeSymlink != 0 || !existing.IsDir() {
					if err := os.RemoveAll(targetPath); err != nil {
						return err
					}
				}
			} else if !errors.Is(statErr, os.ErrNotExist) {
				return statErr
			}
			if err := syncCodexSkillTree(sourcePath, targetPath); err != nil {
				return err
			}
			continue
		}
		fileInfo, err := entry.Info()
		if err != nil {
			return err
		}
		if !fileInfo.Mode().IsRegular() {
			if err := os.RemoveAll(targetPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			continue
		}
		if existing, statErr := os.Lstat(targetPath); statErr == nil && (existing.Mode()&os.ModeSymlink != 0 || existing.IsDir()) {
			if err := os.RemoveAll(targetPath); err != nil {
				return err
			}
		} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		input, err := os.Open(sourcePath)
		if err != nil {
			return err
		}
		output, err := os.OpenFile(targetPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fileInfo.Mode().Perm())
		if err != nil {
			_ = input.Close()
			return err
		}
		_, err = io.Copy(output, input)
		closeOutputErr := output.Close()
		closeInputErr := input.Close()
		if err != nil {
			return err
		}
		if closeOutputErr != nil {
			return closeOutputErr
		}
		if closeInputErr != nil {
			return closeInputErr
		}
	}
	targetEntries, err := os.ReadDir(target)
	if err != nil {
		return err
	}
	present := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		present[entry.Name()] = struct{}{}
	}
	for _, entry := range targetEntries {
		if _, ok := present[entry.Name()]; !ok {
			if err := os.RemoveAll(filepath.Join(target, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *codexCLIRunner) codexProfileHome(profile *AgentRuntimeProfile) (string, error) {
	root := r.config.DataDir
	if root == "" {
		var err error
		root, err = os.UserConfigDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(root, "milevia")
	}
	identity := "default"
	if profile != nil && profile.RevisionID != "" {
		identity = profile.RevisionID
	} else if profile != nil {
		digest := sha256.Sum256([]byte(profile.AgentID + "\x00" + profile.BaseURL + "\x00" + profile.Model))
		identity = fmt.Sprintf("anonymous-%x", digest[:8])
	}
	// Revision IDs are UUIDs today, but keep the path safe if that contract
	// changes; the digest also avoids placing endpoint/key material in a path.
	if profile != nil && profile.RevisionID != "" {
		digest := sha256.Sum256([]byte(identity))
		identity = fmt.Sprintf("revision-%x", digest[:8])
	}
	return filepath.Join(root, "codex", "profiles", identity), nil
}

// codexConfigModelPattern 匹配 config.toml 顶层 `model = "..."` / `model = '...'`。
// Codex 的 config.toml 形如：
//
//	model_provider = "custom"
//	model = "gpt-5.6-terra"
//	model_reasoning_effort = "high"
//
//	[model_providers.custom]
//	...
//
// 只有第一个 [section] 之前的顶层 model 才是当前生效模型；provider 段内同名键不读。
var codexConfigModelPattern = regexp.MustCompile(`^[ \t]*model[ \t]*=[ \t]*("([^"]*)"|'([^']*)')`)

// codexModelFromConfig 从 codex config.toml 文本中提取顶层生效的 model 名。
// 找不到（使用官方 provider 内建默认、文件不存在或不可解析）时返回空串。
func codexModelFromConfig(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			break // 进入 provider 段后不再属于顶层配置
		}
		if match := codexConfigModelPattern.FindStringSubmatch(trimmed); match != nil {
			if match[2] != "" {
				return match[2]
			}
			return match[3]
		}
	}
	return ""
}

// codexDefaultModel 返回本机 cli_managed（无档案模型）Codex 将使用的默认模型。
// 生效的 CODEX_HOME 取环境变量，否则为 ~/.codex（与 codex CLI 的解析一致）。
// 读取或解析失败返回空串，调用方回退到仅显示工具名。
func (r *codexCLIRunner) codexDefaultModel(ctx context.Context) string {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		home = filepath.Join(userHome, ".codex")
	}
	data, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		return ""
	}
	return codexModelFromConfig(data)
}

func codexSandbox(policy string) (string, error) {
	switch policy {
	case "read_only":
		return "read-only", nil
	case "workspace_write":
		return "workspace-write", nil
	case "full_control":
		return "danger-full-access", nil
	default:
		return "", fmt.Errorf("unsupported Codex execution policy: %s", policy)
	}
}

func readCodexJSONL(reader io.Reader, sink AgentRunSink, projectPath string) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for scanner.Scan() {
		line, err := sanitizeCodexJSONL(scanner.Bytes())
		if err != nil {
			sink.Event("stream.error", mustJSON(map[string]string{"error": errorText(err)}))
			continue
		}
		var event struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
			Item     struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Changes []struct {
					Path string `json:"path"`
					Kind string `json:"kind"`
				} `json:"changes"`
			} `json:"item"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			sink.Event("stream.error", mustJSON(map[string]string{"error": errorText(err)}))
			continue
		}
		// When running locally, enrich file_change items with the actual file
		// content or git diff so the UI can show what changed. SSH runners
		// pass an empty projectPath and skip this — the files are remote.
		emitted := line
		if projectPath != "" && event.Type == "item.completed" && event.Item.Type == "file_change" && len(event.Item.Changes) > 0 {
			if enriched := enrichCodexFileChange(line, projectPath); enriched != nil {
				emitted = enriched
			}
		}
		sink.Event(event.Type, emitted)
		if event.Type == "thread.started" && event.ThreadID != "" {
			sink.SessionIdentified(event.ThreadID)
			sink.SessionInitialized()
		}
		if event.Type == "item.completed" && event.Item.Type == "agent_message" && strings.TrimSpace(event.Item.Text) != "" {
			sink.AssistantText(event.Item.Text, "")
		}
	}
	if err := scanner.Err(); err != nil {
		// errorText 对停止时管道关闭（os.ErrClosed）返回空，据此跳过上报，
		// 避免正常停止在对话历史里留下"流错误 / file already closed"。
		if text := errorText(err); text != "" {
			sink.Event("stream.error", mustJSON(map[string]string{"error": text}))
		}
	}
}

func readCodexStderr(reader io.Reader, sink AgentRunSink) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	// 同 claude readStderr：解码 wsl.exe 的 UTF-16LE 主机侧警告；非 WSL 路径行为不变。
	scanner.Split(wslStderrSplit)
	for scanner.Scan() {
		if text := strings.TrimSpace(scanner.Text()); text != "" {
			// codex exec emits this informational line when its /dev/null stdin is
			// non-interactive. The prompt is already supplied through argv.
			if text == codexAdditionalStdinNotice {
				continue
			}
			// Codex 0.148 emits a bare `!` on stderr as a progress/status
			// marker in JSON mode. It is not an actionable diagnostic.
			if strings.Trim(text, "!?.:;,-_~ \t\r\n") == "" {
				continue
			}
			sink.Event("stderr", mustJSON(map[string]string{"message": codexDiagnostic(text)}))
		}
	}
}

// sanitizeAgentJSONL removes credentials before a CLI event reaches any
// parser, persistence sink, or client-facing stream. Both supported CLIs use
// JSONL output, so this must remain independent of a particular CLI schema.
func sanitizeAgentJSONL(line []byte) (json.RawMessage, error) {
	var payload any
	if err := json.Unmarshal(line, &payload); err != nil {
		return nil, err
	}
	sanitized, err := json.Marshal(redactAgentJSONValue(payload, ""))
	if err != nil {
		return nil, err
	}
	return json.RawMessage(sanitized), nil
}

// sanitizeCodexJSONL is kept for existing callers and tests.
func sanitizeCodexJSONL(line []byte) (json.RawMessage, error) {
	return sanitizeAgentJSONL(line)
}

func redactAgentJSONValue(value any, field string) any {
	switch typed := value.(type) {
	case map[string]any:
		redacted := make(map[string]any, len(typed))
		for key, item := range typed {
			redacted[key] = redactAgentJSONValue(item, key)
		}
		return redacted
	case []any:
		redacted := make([]any, len(typed))
		for index, item := range typed {
			redacted[index] = redactAgentJSONValue(item, field)
		}
		return redacted
	case string:
		if isSensitiveAgentField(field) {
			return "[REDACTED]"
		}
		return redactAgentText(typed)
	default:
		return value
	}
}

func isSensitiveAgentField(field string) bool {
	field = strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(field, "_", ""), "-", ""))
	return strings.Contains(field, "apikey") || strings.Contains(field, "token") || strings.Contains(field, "password") || strings.Contains(field, "secret") || strings.Contains(field, "authorization") || strings.Contains(field, "credential") || strings.Contains(field, "codexhome")
}

func redactAgentText(value string) string {
	value = codexBearerPattern.ReplaceAllString(value, "${1}[REDACTED]")
	value = codexSensitiveAssignmentPattern.ReplaceAllString(value, "${1}${2}[REDACTED]")
	value = codexHomePattern.ReplaceAllString(value, "${1}[REDACTED_PATH]")
	value = codexAuthPathPattern.ReplaceAllString(value, "[REDACTED_PATH]")
	return codexAPIKeyPattern.ReplaceAllString(value, "[REDACTED]")
}

// redactCodexText is kept for existing callers that redact Codex-specific
// file diffs. The implementation is deliberately shared with Claude output.
func redactCodexText(value string) string { return redactAgentText(value) }

func codexDiagnostic(value string) string {
	value = redactCodexText(value)
	if len(value) <= maxCodexDiagnosticBytes {
		return value
	}
	return value[:maxCodexDiagnosticBytes] + "... [TRUNCATED]"
}

// maxCodexFileDiffBytes bounds the amount of file content/diff we attach to
// file_change events so a single huge file cannot blow up the event payload.
const maxCodexFileDiffBytes = 64 * 1024

// enrichCodexFileChange attaches the actual file content or git diff to a
// file_change item so the UI can show what Codex modified. It re-serializes
// the payload with an added "diff" field on each change. Returns nil if the
// payload cannot be processed.
func enrichCodexFileChange(line json.RawMessage, projectPath string) json.RawMessage {
	var payload map[string]any
	if err := json.Unmarshal(line, &payload); err != nil {
		return nil
	}
	itemRaw, ok := payload["item"]
	if !ok {
		return nil
	}
	item, ok := itemRaw.(map[string]any)
	if !ok {
		return nil
	}
	changesRaw, ok := item["changes"].([]any)
	if !ok {
		return nil
	}
	absProject, err := filepath.Abs(projectPath)
	if err != nil {
		return nil
	}
	gitDir := filepath.Join(absProject, ".git")
	for _, changeRaw := range changesRaw {
		change, ok := changeRaw.(map[string]any)
		if !ok {
			continue
		}
		path, _ := change["path"].(string)
		if path == "" {
			continue
		}
		kind, _ := change["kind"].(string)
		diff := codexFileChangeDiff(absProject, gitDir, path, kind)
		if diff != "" {
			// Redact sensitive values (API keys, tokens, auth paths) that may
			// appear in file contents or git diff output. The original JSONL
			// was sanitized by sanitizeCodexJSONL, but the diff we just read
			// from disk has never been through redaction.
			change["diff"] = redactCodexText(diff)
		}
	}
	result, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return result
}

// codexFileChangeDiff returns the content to display for a single file change:
//   - add: the full file content (if small enough)
//   - modify: a git diff against HEAD
//   - delete: a placeholder marker
//
// The path may be relative or absolute; the resolved path (with symlinks
// evaluated) must stay inside the project root to prevent directory-traversal
// and symlink-based information leaks.
func codexFileChangeDiff(projectRoot, gitDir, changePath, kind string) string {
	cleanRoot := filepath.Clean(projectRoot)
	// On Windows, EvalSymlinks can return a canonical path with a different
	// casing or 8.3 component than filepath.Abs. Resolve the root too so the
	// containment check compares like-for-like paths.
	if resolvedRoot, err := filepath.EvalSymlinks(cleanRoot); err == nil {
		cleanRoot = resolvedRoot
	}
	var absPath string
	if filepath.IsAbs(changePath) {
		absPath = filepath.Clean(changePath)
	} else {
		absPath = filepath.Clean(filepath.Join(cleanRoot, changePath))
	}
	// Evaluate symlinks so a symlink pointing outside the project root cannot
	// leak arbitrary file contents through the diff field.
	if resolved, err := filepath.EvalSymlinks(absPath); err == nil {
		absPath = resolved
	}
	if !isWithinPath(absPath, cleanRoot) {
		return ""
	}
	relPath, err := filepath.Rel(cleanRoot, absPath)
	if err != nil {
		return ""
	}
	switch kind {
	case "add":
		content, err := os.ReadFile(absPath)
		if err != nil {
			return ""
		}
		if !isProbablyText(content) {
			return "（二进制文件，无法预览）"
		}
		if len(content) > maxCodexFileDiffBytes {
			return truncateUTF8(string(content), maxCodexFileDiffBytes) + "\n... [已截断]"
		}
		return string(content)
	case "modify":
		return codexGitDiff(gitDir, projectRoot, relPath)
	case "delete":
		return "（文件已删除）"
	default:
		return codexGitDiff(gitDir, projectRoot, relPath)
	}
}

// isWithinPath reports whether target is equal to or nested inside root.
// It handles the root-"/" edge case where appending a separator would
// produce "//" and break the prefix check.
func isWithinPath(target, root string) bool {
	target = filepath.Clean(target)
	root = filepath.Clean(root)
	if runtime.GOOS == "windows" {
		target = strings.ToLower(target)
		root = strings.ToLower(root)
	}
	relative, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

// codexGitDiff runs `git diff HEAD -- <path>` in the project root and returns
// the patch output. Returns an empty string if git is unavailable, the
// repository has no HEAD (freshly initialised), or the diff is empty.
func codexGitDiff(gitDir, projectRoot, relPath string) string {
	if _, err := os.Stat(gitDir); err != nil {
		return ""
	}
	cmd := exec.Command("git", "-C", projectRoot, "diff", "HEAD", "--", relPath)
	// Isolate from user/system gitconfig: diff.external and other hooks could
	// otherwise execute arbitrary commands or alter diff output.
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
	)
	configureProcessGroup(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	if len(out) > maxCodexFileDiffBytes {
		return truncateUTF8(string(out), maxCodexFileDiffBytes) + "\n... [已截断]"
	}
	return string(out)
}

// truncateUTF8 cuts the string to at most maxBytes, backing up to the last
// valid UTF-8 rune boundary so we never split a multi-byte character.
func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	// Walk backward from the boundary to find a valid rune start.
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// isProbablyText checks whether the byte slice looks like text (no NUL bytes
// in the first 512 bytes), mirroring how git and file(1) distinguish text
// from binary.
func isProbablyText(data []byte) bool {
	limit := len(data)
	if limit > 512 {
		limit = 512
	}
	for i := 0; i < limit; i++ {
		if data[i] == 0 {
			return false
		}
	}
	return true
}
