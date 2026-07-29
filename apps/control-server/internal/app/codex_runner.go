package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

const maxCodexDiagnosticBytes = 4 * 1024

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
	if _, err := exec.LookPath(r.config.CodexPath); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, r.config.CodexPath, "login", "status").Run() == nil
}

func (r *codexCLIRunner) Version(parent context.Context) string {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, r.config.CodexPath, "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), "codex-cli "))
}

func (r *codexCLIRunner) CheckUpdate(context.Context) (bool, string, error) {
	return false, "", errors.New("Codex CLI does not provide a non-interactive update check")
}

func (r *codexCLIRunner) Update(parent context.Context) (string, string, error) {
	previous := r.Version(parent)
	if previous == "" {
		return "", "", errors.New("Codex CLI is not installed")
	}
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, r.config.CodexPath, "update").Run(); err != nil {
		return previous, "", fmt.Errorf("update Codex: %w", err)
	}
	return previous, r.Version(parent), nil
}

func (r *codexCLIRunner) Run(ctx context.Context, request AgentRunRequest, sink AgentRunSink) error {
	policy, err := codexSandbox(request.PermissionMode)
	if err != nil {
		return err
	}
	args := []string{"exec"}
	if request.Resume {
		args = append(args, "resume", "-c", fmt.Sprintf("sandbox_mode=%q", policy), "--json", request.SessionID, request.Prompt)
	} else {
		args = append(args, "-c", fmt.Sprintf("sandbox_mode=%q", policy), "--json", "--color", "never", "-C", request.ProjectPath, "--sandbox", policy, request.Prompt)
	}
	cmd := exec.Command(r.config.CodexPath, args...)
	cmd.Dir = request.ProjectPath
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
	go func() { defer wg.Done(); readCodexJSONL(stdout, sink) }()
	go func() { defer wg.Done(); readCodexStderr(stderr, sink) }()
	wg.Wait()
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("Codex exited: %w", err)
	}
	return nil
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

func readCodexJSONL(reader io.Reader, sink AgentRunSink) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for scanner.Scan() {
		line, err := sanitizeCodexJSONL(scanner.Bytes())
		if err != nil {
			sink.Event("stream.error", mustJSON(map[string]string{"error": codexDiagnostic(err.Error())}))
			continue
		}
		var event struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
			Item     struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			sink.Event("stream.error", mustJSON(map[string]string{"error": codexDiagnostic(err.Error())}))
			continue
		}
		sink.Event(event.Type, line)
		if event.Type == "thread.started" && event.ThreadID != "" {
			sink.SessionIdentified(event.ThreadID)
			sink.SessionInitialized()
		}
		if event.Type == "item.completed" && event.Item.Type == "agent_message" && strings.TrimSpace(event.Item.Text) != "" {
			sink.AssistantText(event.Item.Text, "")
		}
	}
	if err := scanner.Err(); err != nil {
		sink.Event("stream.error", mustJSON(map[string]string{"error": codexDiagnostic(err.Error())}))
	}
}

func readCodexStderr(reader io.Reader, sink AgentRunSink) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for scanner.Scan() {
		if text := strings.TrimSpace(scanner.Text()); text != "" {
			sink.Event("stderr", mustJSON(map[string]string{"message": codexDiagnostic(text)}))
		}
	}
}

func sanitizeCodexJSONL(line []byte) (json.RawMessage, error) {
	var payload any
	if err := json.Unmarshal(line, &payload); err != nil {
		return nil, err
	}
	sanitized, err := json.Marshal(redactCodexJSONValue(payload, ""))
	if err != nil {
		return nil, err
	}
	return json.RawMessage(sanitized), nil
}

func redactCodexJSONValue(value any, field string) any {
	switch typed := value.(type) {
	case map[string]any:
		redacted := make(map[string]any, len(typed))
		for key, item := range typed {
			redacted[key] = redactCodexJSONValue(item, key)
		}
		return redacted
	case []any:
		redacted := make([]any, len(typed))
		for index, item := range typed {
			redacted[index] = redactCodexJSONValue(item, field)
		}
		return redacted
	case string:
		if isSensitiveCodexField(field) {
			return "[REDACTED]"
		}
		return redactCodexText(typed)
	default:
		return value
	}
}

func isSensitiveCodexField(field string) bool {
	field = strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(field, "_", ""), "-", ""))
	return strings.Contains(field, "apikey") || strings.Contains(field, "token") || strings.Contains(field, "password") || strings.Contains(field, "secret") || strings.Contains(field, "authorization") || strings.Contains(field, "credential") || strings.Contains(field, "codexhome")
}

func redactCodexText(value string) string {
	value = codexBearerPattern.ReplaceAllString(value, "${1}[REDACTED]")
	value = codexSensitiveAssignmentPattern.ReplaceAllString(value, "${1}${2}[REDACTED]")
	value = codexHomePattern.ReplaceAllString(value, "${1}[REDACTED_PATH]")
	value = codexAuthPathPattern.ReplaceAllString(value, "[REDACTED_PATH]")
	return codexAPIKeyPattern.ReplaceAllString(value, "[REDACTED]")
}

func codexDiagnostic(value string) string {
	value = redactCodexText(value)
	if len(value) <= maxCodexDiagnosticBytes {
		return value
	}
	return value[:maxCodexDiagnosticBytes] + "... [TRUNCATED]"
}
