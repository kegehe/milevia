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
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSupervisionCodebuddyInstalledPermissionAndInputContract(t *testing.T) {
	if os.Getenv("MILEVIA_RUN_SUPERVISION_CODEBUDDY_CONTRACT") != "1" {
		t.Skip("explicit installed CodeBuddy isolation experiment")
	}
	root := t.TempDir()
	original := filepath.Join(root, ".codebuddy")
	if err := os.Mkdir(original, 0700); err != nil {
		t.Fatal(err)
	}
	const canary = "UNAUTHORIZED_CODEBUDDY_RULE_CANARY"
	const token = "milevia-codebuddy-fake-token"
	for _, path := range []string{filepath.Join(original, "CODEBUDDY.md"), filepath.Join(root, "AGENTS.md"), filepath.Join(root, "CODEBUDDY.md")} {
		if err := os.WriteFile(path, []byte(canary), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TEMP", root)
	t.Setenv("TMP", root)
	t.Setenv("USERPROFILE", root)
	t.Setenv("HOME", root)
	t.Setenv("CODEBUDDY_CONFIG_DIR", original)
	var mu sync.Mutex
	var requests []map[string]any
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
		if err != nil {
			t.Error(err)
			return
		}
		var body map[string]any
		if json.Unmarshal(raw, &body) != nil {
			t.Error("invalid provider body")
			return
		}
		mu.Lock()
		requests = append(requests, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range []map[string]any{
			{"id": "milevia-test", "object": "chat.completion.chunk", "model": "hy4-preview", "choices": []any{map[string]any{"index": 0, "delta": map[string]string{"role": "assistant", "content": "done"}, "finish_reason": nil}}},
			{"id": "milevia-test", "object": "chat.completion.chunk", "model": "hy4-preview", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 1, "total_tokens": 11}},
		} {
			raw, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", raw)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer provider.Close()
	t.Setenv("CODEBUDDY_BASE_URL", provider.URL)
	t.Setenv("CODEBUDDY_AUTH_TOKEN", token)
	request := schedulerCLIRequestForTest()
	request.Model, request.ReasoningEffort = "hy4-preview", ""
	request.Container = supervisionJobPrefix + uuid.NewString()
	resolved := false
	request.Resolved = func(info supervisionCLIInfo) error {
		resolved = info.AgentID == "codebuddy" && info.Version == "2.177.0" && info.Contract == supervisionCodebuddyContract && filepath.Base(info.Path) == "node.exe" && info.Entrypoint != "" && len(info.EntrypointSHA256) == 64 && info.RuntimeVersion != ""
		if !resolved {
			t.Error("runtime and entrypoint facts missing")
		}
		return nil
	}
	request.Handshake = func(identity supervisionProcessIdentity, resume func() error) error {
		if !resolved || filepath.Base(identity.Executable) != "node.exe" {
			t.Error("unfrozen Node execution")
		}
		return resume()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	receipt, err := (&codebuddyCLIRunner{}).runSupervisionScheduler(ctx, request, &claudeOutputTestSink{})
	if err != nil || receipt.Output != "done" || !receipt.Initialized || !receipt.ProcessWaited || !receipt.ContainerQuiet {
		t.Fatalf("CodeBuddy controlled protocol: %+v %v", receipt, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 1 {
		t.Fatalf("provider requests=%d", len(requests))
	}
	raw, _ := json.Marshal(requests)
	if strings.Contains(string(raw), canary) || strings.Contains(string(raw), token) {
		t.Fatal("unauthorized context or credentials reached model input")
	}
	tools, ok := requests[0]["tools"].([]any)
	if (ok && len(tools) != 0) || (!ok && requests[0]["tools"] != nil) {
		t.Fatal("provider received execution tools")
	}
	if !strings.Contains(string(raw), "schemaVersion") || !strings.Contains(string(raw), "全局调度") {
		t.Fatal("authorized input or fixed role missing")
	}
}

func TestSupervisionCodebuddyInstalledWorkerPersistsAndSettles(t *testing.T) {
	if os.Getenv("MILEVIA_RUN_SUPERVISION_CODEBUDDY_LIVE") != "1" {
		t.Skip("explicit real CodeBuddy worker experiment")
	}
	if _, err := exec.LookPath("codebuddy"); err != nil {
		t.Fatal(err)
	}
	s, _, _, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	s.codebuddyRunner = &codebuddyCLIRunner{}
	if err := s.updateSupervisionRoles(context.Background(), ss.ID, ss.StateVersion, supervisionRoleConfigs{"scheduler": {AgentID: "codebuddy"}}); err != nil {
		t.Fatal(err)
	}
	startSupervisionForTest(t, s, ss.ID, now)
	lease, decision := decisionForTest(t, s, ss.ID, now)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	if err := s.executeSupervisionWorkerDecision(ctx, decision, lease); err != nil {
		t.Fatal(err)
	}
	var raw, state string
	if err := s.db.QueryRow(`select s.spec,e.state from supervision_execution_specs s join supervision_executions e on e.execution_id=s.execution_id where e.execution_id=?`, decision.ExecutionID).Scan(&raw, &state); err != nil {
		t.Fatal(err)
	}
	var spec supervisionExecutionSpec
	if json.Unmarshal([]byte(raw), &spec) != nil || spec.CLI.AgentID != "codebuddy" || spec.CLI.Entrypoint == "" || state != "quiescent" {
		t.Fatalf("missing actual runtime specification: %s %s", raw, state)
	}
}
