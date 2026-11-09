//go:build windows

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// This runs the actual installed native CLI inside a real Windows Job. Only
// its provider is local/synthetic: no model account or paid requests are used.
func TestSupervisionOpenCodeInstalledNativeControlledProvider(t *testing.T) {
	binary := os.Getenv("MILEVIA_SUPERVISION_OPENCODE_PROTOCOL_BINARY")
	if binary == "" {
		t.Skip("set MILEVIA_SUPERVISION_OPENCODE_PROTOCOL_BINARY to verified 1.19.5 native executable")
	}
	var mu sync.Mutex
	var requests []map[string]any
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&request) != nil {
			http.Error(w, "invalid body", 400)
			return
		}
		mu.Lock()
		requests = append(requests, request)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := map[string]any{"id": "fake-response", "object": "chat.completion.chunk", "created": 1, "model": "fixture", "choices": []any{map[string]any{"index": 0, "delta": map[string]string{"role": "assistant", "content": `{"schemaVersion":1,"protocolProbe":true}`}, "finish_reason": nil}}}
		raw, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", raw)
		end := map[string]any{"id": "fake-response", "object": "chat.completion.chunk", "created": 1, "model": "fixture", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}}
		raw, _ = json.Marshal(end)
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", raw)
	}))
	defer provider.Close()
	configHome, dataHome := t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("OPENCODE_CONFIG", filepath.Join(t.TempDir(), "absent.json"))
	t.Setenv("OPENCODE_AUTH_CONTENT", `{}`)
	marker := filepath.Join(t.TempDir(), "unexpected-plugin-executed")
	plugin := filepath.Join(t.TempDir(), "plugin.mjs")
	js := fmt.Sprintf("import fs from 'node:fs';fs.writeFileSync(%s,'executed');export const Canary=async()=>({});", mustJSON(marker))
	if err := os.WriteFile(plugin, []byte(js), 0600); err != nil {
		t.Fatal(err)
	}
	configuration := map[string]any{"model": "fixture/model", "plugin": []string{filepath.ToSlash(plugin)}, "instructions": []string{"private-original-instruction"}, "mcp": map[string]any{"canary": map[string]any{"type": "local", "command": []string{"must-not-execute"}}}, "provider": map[string]any{"fixture": map[string]any{"npm": "@ai-sdk/openai-compatible", "options": map[string]string{"baseURL": provider.URL + "/v1", "apiKey": "fake-key"}, "models": map[string]any{"model": map[string]any{"name": "fixture", "limit": map[string]int{"context": 32000, "output": 4096}}}}}}
	t.Setenv("OPENCODE_CONFIG_CONTENT", string(mustJSON(configuration)))
	runner := newOpenCodeManageRunner(Config{OpenCodePath: binary}, newAgentPathResolver(Config{OpenCodePath: binary}))
	request := schedulerCLIRequestForTest()
	request.Model, request.ReasoningEffort, request.Profile = "fixture/model", "", nil
	request.Container = supervisionJobPrefix + uuid.NewString()
	var identity supervisionProcessIdentity
	request.Handshake = func(id supervisionProcessIdentity, resume func() error) error { identity = id; return resume() }
	var resolved supervisionCLIInfo
	request.Resolved = func(info supervisionCLIInfo) error { resolved = info; return nil }
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	receipt, err := runner.runSupervisionScheduler(ctx, request, &claudeOutputTestSink{})
	if err != nil || !receipt.Initialized || !receipt.ProcessStarted || !receipt.ProcessWaited || !receipt.ContainerQuiet || !strings.Contains(receipt.Output, `"protocolProbe":true`) || identity.PID <= 0 || resolved.Contract != supervisionOpenCodeContract {
		t.Fatalf("native OpenCode protocol failed: %+v %v", receipt, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("original plugin executed", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 1 {
		t.Fatalf("expected one controlled model step; got %d provider requests", len(requests))
	}
	for _, request := range requests {
		tools, ok := request["tools"].([]any)
		if (ok && len(tools) > 0) || (!ok && request["tools"] != nil) {
			t.Fatal("native CLI exposed tools to provider")
		}
		raw := string(mustJSON(request))
		if strings.Contains(raw, "private-original-instruction") || strings.Contains(raw, "must-not-execute") || strings.Contains(raw, "fake-key") {
			t.Fatal("external instructions or MCP entered model input")
		}
		if !strings.Contains(raw, "全局调度 Agent") {
			t.Fatal("fixed role instructions not supplied to provider")
		}
	}
	if quiet, err := supervisionContainerQuiescent(request.Container); err != nil || !quiet {
		t.Fatal("native OpenCode resources not quiet", err)
	}
}
