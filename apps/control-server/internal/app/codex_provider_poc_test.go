package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestCodexCustomProviderConnectivityPOC validates the exact custom-provider
// configuration used by a future API Key Profile. It intentionally returns a
// local 400 after observing the first request, so it never invokes a model or
// permits a tool call. Tool-environment isolation is a separate POC and must
// pass before this provider path can be enabled for real credentials.
func TestCodexCustomProviderConnectivityPOC(t *testing.T) {
	if os.Getenv("MILEVIA_RUN_CODEX_PROVIDER_POC") != "1" {
		t.Skip("set MILEVIA_RUN_CODEX_PROVIDER_POC=1 to run the installed Codex custom-provider POC")
	}
	codexPath, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("Codex is not installed")
	}
	const decoyKey = "sk-milevia-codex-provider-decoy-1234567890"
	var mu sync.Mutex
	requests := 0
	authorization := ""
	requestBody := ""
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 2<<20))
		mu.Lock()
		requests++
		authorization = r.Header.Get("authorization")
		requestBody = string(body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Milevia Codex provider POC complete","type":"invalid_request_error"}}`))
	}))
	defer provider.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, codexPath, "exec", "--ignore-user-config", "--ignore-rules", "--ephemeral", "--strict-config", "--json",
		"-c", `model_provider="milevia_poc"`,
		"-c", `model_providers.milevia_poc.name="Milevia POC"`,
		"-c", `model_providers.milevia_poc.base_url="`+provider.URL+`/v1"`,
		"-c", `model_providers.milevia_poc.wire_api="responses"`,
		"-c", `model_providers.milevia_poc.env_key="OPENAI_API_KEY"`,
		"-m", "milevia-poc-model", "Reply with one word.",
	)
	cmd.Env = codexPOCEnvironment(t.TempDir(), decoyKey)
	_ = cmd.Run() // The local provider deliberately returns HTTP 400.
	mu.Lock()
	defer mu.Unlock()
	if requests != 1 {
		t.Fatalf("provider requests=%d, want 1", requests)
	}
	if authorization != "Bearer "+decoyKey {
		t.Fatalf("provider authentication did not use the configured API key")
	}
	if !strings.Contains(requestBody, `"model":"milevia-poc-model"`) || !strings.Contains(requestBody, `"tools"`) {
		t.Fatal("provider request did not contain the configured model and tool contract")
	}
}

func codexPOCEnvironment(home, apiKey string) []string {
	environment := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"CODEX_HOME=" + home,
		"TMPDIR=" + home,
		"OPENAI_API_KEY=" + apiKey,
	}
	for _, name := range []string{"LANG", "LC_ALL", "SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT", "USERPROFILE", "TEMP", "TMP"} {
		if value := os.Getenv(name); value != "" {
			environment = append(environment, name+"="+value)
		}
	}
	return environment
}

func TestCodexPOCEnvironmentDoesNotInheritCredentials(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "other-secret")
	environment := strings.Join(codexPOCEnvironment(t.TempDir(), "codex-secret"), "\n")
	if strings.Contains(environment, "other-secret") || strings.Contains(environment, "ANTHROPIC_API_KEY=") {
		t.Fatalf("Codex POC environment inherited another credential: %q", environment)
	}
}

// TestCodexToolEnvironmentIsolationPOC verifies that the configured shell
// environment policy removes the API key before Codex executes a model-issued
// command. It is opt-in because it launches the installed CLI, but it only
// speaks to the in-process mock Responses endpoint with a fake key.
func TestCodexToolEnvironmentIsolationPOC(t *testing.T) {
	if os.Getenv("MILEVIA_RUN_CODEX_ISOLATION_POC") != "1" {
		t.Skip("set MILEVIA_RUN_CODEX_ISOLATION_POC=1 to run the installed Codex tool-environment POC")
	}
	codexPath, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("Codex is not installed")
	}
	const decoyKey = "sk-milevia-codex-isolation-decoy-1234567890"
	var mu sync.Mutex
	requests := 0
	toolResult := ""
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 2<<20))
		mu.Lock()
		requests++
		current := requests
		if current > 1 {
			toolResult = string(body)
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		if current == 1 {
			writeCodexPOCToolUse(w)
			return
		}
		writeCodexPOCFinal(w)
	}))
	defer provider.Close()

	home := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, codexPath, "exec", "--ignore-user-config", "--ignore-rules", "--ephemeral", "--strict-config", "--json",
		"-c", `model_provider="milevia_poc"`,
		"-c", `model_providers.milevia_poc.name="Milevia POC"`,
		"-c", `model_providers.milevia_poc.base_url="`+provider.URL+`/v1"`,
		"-c", `model_providers.milevia_poc.wire_api="responses"`,
		"-c", `model_providers.milevia_poc.env_key="OPENAI_API_KEY"`,
		"-c", `shell_environment_policy.exclude=["OPENAI_API_KEY"]`,
		"-m", "milevia-poc-model", "Run the requested command, then answer done.",
	)
	cmd.Dir = home
	cmd.Env = codexPOCEnvironment(home, decoyKey)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Codex isolation POC failed: %v\n%s", err, redactAgentText(string(output)))
	}
	mu.Lock()
	defer mu.Unlock()
	if requests < 2 {
		t.Fatalf("provider requests=%d, want tool-result follow-up", requests)
	}
	if strings.Contains(toolResult, decoyKey) {
		t.Fatal("API key leaked into Codex command environment")
	}
	if strings.Contains(string(output), decoyKey) {
		t.Fatal("API key leaked into Codex output")
	}
}

func writeCodexPOCEvent(w http.ResponseWriter, typ string, data any) {
	payload, _ := json.Marshal(data)
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, payload)
	if flush, ok := w.(http.Flusher); ok {
		flush.Flush()
	}
}

func writeCodexPOCToolUse(w http.ResponseWriter) {
	response := map[string]any{"id": "resp_milevia_poc_1", "object": "response", "created_at": 0, "status": "in_progress", "model": "milevia-poc-model", "output": []any{}}
	item := map[string]any{"id": "fc_milevia_poc_1", "type": "function_call", "status": "in_progress", "call_id": "call_milevia_poc_1", "name": "exec_command", "arguments": ""}
	arguments := `{"cmd":"env"}`
	writeCodexPOCEvent(w, "response.created", map[string]any{"type": "response.created", "response": response})
	writeCodexPOCEvent(w, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item})
	writeCodexPOCEvent(w, "response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "item_id": item["id"], "output_index": 0, "delta": arguments})
	item["arguments"] = arguments
	writeCodexPOCEvent(w, "response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "item_id": item["id"], "output_index": 0, "arguments": arguments})
	writeCodexPOCEvent(w, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
	response["status"] = "completed"
	response["output"] = []any{item}
	writeCodexPOCEvent(w, "response.completed", map[string]any{"type": "response.completed", "response": response})
}

func writeCodexPOCFinal(w http.ResponseWriter) {
	response := map[string]any{"id": "resp_milevia_poc_2", "object": "response", "created_at": 0, "status": "completed", "model": "milevia-poc-model", "output": []any{map[string]any{"id": "msg_milevia_poc_1", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "done", "annotations": []any{}}}}}}
	writeCodexPOCEvent(w, "response.created", map[string]any{"type": "response.created", "response": response})
	writeCodexPOCEvent(w, "response.completed", map[string]any{"type": "response.completed", "response": response})
}
