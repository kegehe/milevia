package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

func newCodexCLIRunner(config Config) AgentRunner { return &codexCLIRunner{config: config} }

func (r *codexCLIRunner) Ready(parent context.Context) bool {
	if !r.BinaryReady() {
		return false
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.config.CodexPath, "login", "status")
	configureProcessGroup(cmd)
	return cmd.Run() == nil
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
	cmd := exec.CommandContext(ctx, r.config.CodexPath, "--version")
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
	cmd := exec.CommandContext(ctx, r.config.CodexPath, "update")
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
	profileArgs, environment, closeProfile, err := r.profileLaunch(ctx, request.Profile)
	if err != nil {
		return err
	}
	defer closeProfile()
	args = append(args, profileArgs...)
	if request.Profile != nil && request.Profile.Model != "" {
		args = append(args, "-c", fmt.Sprintf("model=%q", request.Profile.Model))
	}
	if request.Resume {
		args = append(args, "resume", "-c", fmt.Sprintf("sandbox_mode=%q", policy), "--json", request.SessionID, request.Prompt)
	} else {
		args = append(args, "-c", fmt.Sprintf("sandbox_mode=%q", policy), "--json", "--color", "never", "-C", request.ProjectPath, "--sandbox", policy, request.Prompt)
	}
	cmd := exec.Command(r.config.CodexPath, args...)
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

func (r *codexCLIRunner) profileLaunch(_ context.Context, profile *AgentRuntimeProfile) ([]string, []string, func(), error) {
	return nil, managedCLIEnvironment(profile, os.Environ()), func() {}, nil
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
	for scanner.Scan() {
		if text := strings.TrimSpace(scanner.Text()); text != "" {
			// codex exec emits this informational line when its /dev/null stdin is
			// non-interactive. The prompt is already supplied through argv.
			if text == codexAdditionalStdinNotice {
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
	if target == root {
		return true
	}
	if root == string(filepath.Separator) {
		return strings.HasPrefix(target, root)
	}
	return strings.HasPrefix(target, root+string(filepath.Separator))
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
