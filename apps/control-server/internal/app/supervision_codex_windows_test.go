//go:build windows

package app

import (
	"context"
	"encoding/json"
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

// 真实已安装 CLI、真实 Windows 挂起/归组、假 Responses 服务。验证权限和输入
// 隔离，不把假服务结果计为模型端到端通过。
func TestSupervisionCodexInstalledPermissionAndInputContract(t *testing.T) {
	if os.Getenv("MILEVIA_RUN_SUPERVISION_CODEX_CONTRACT") != "1" {
		t.Skip("explicit installed Codex protocol test")
	}
	for _, mode := range []string{"existing_env", "existing_auth_file", "managed_key", "malicious_tool", "code_mode_model"} {
		t.Run(mode, func(t *testing.T) { testSupervisionCodexInstalledContract(t, mode) })
	}
}

func testSupervisionCodexInstalledContract(t *testing.T, mode string) {
	binary, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	original := filepath.Join(root, "original")
	if err := os.Mkdir(original, 0700); err != nil {
		t.Fatal(err)
	}
	const canary = "UNAUTHORIZED_GLOBAL_RULE_CANARY"
	const secret = "milevia-contract-fake-key-1234567890"
	if err := os.WriteFile(filepath.Join(original, "AGENTS.md"), []byte(canary), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(original, "auth.json"), []byte(`{"OPENAI_API_KEY":"`+secret+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	requests := []map[string]any{}
	marker := filepath.Join(root, "unauthorized-tool.txt")
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(req.Body, 2<<20))
		if err != nil {
			t.Error(err)
			return
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		requests = append(requests, body)
		count := len(requests)
		mu.Unlock()
		if req.Header.Get("Authorization") != "Bearer "+secret {
			t.Error("existing authentication was not reused")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if mode == "malicious_tool" && count == 1 {
			command := "powershell -NoProfile -NonInteractive -Command \"Set-Content -LiteralPath '" + marker + "' -Value 'bad'\""
			arguments, _ := json.Marshal(map[string]string{"cmd": command})
			writeSupervisionCodexTool(w, "exec_command", string(arguments))
			return
		}
		writeSupervisionCodexFinal(w, "done")
	}))
	defer provider.Close()
	config := "model = \"gpt-5.4\"\nmodel_provider = \"probe\"\ncli_auth_credentials_store = \"file\"\n[model_providers.probe]\nname = \"Local contract probe\"\nwire_api = \"responses\"\nenv_key = \"MILEVIA_CODEX_DECOY_KEY\"\nbase_url = " + supervisionTOMLString(provider.URL+"/v1") + "\n"
	if mode == "existing_auth_file" {
		config = strings.ReplaceAll(config, "env_key = \"MILEVIA_CODEX_DECOY_KEY\"", "requires_openai_auth = true")
	}
	if err := os.WriteFile(filepath.Join(original, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", original)
	t.Setenv("MILEVIA_CODEX_DECOY_KEY", secret)
	request := schedulerCLIRequestForTest()
	request.Model, request.ReasoningEffort = "gpt-5.4", ""
	if mode == "code_mode_model" {
		request.Model = "gpt-6.1-sol"
	}
	if mode == "managed_key" {
		request.Profile = &AgentRuntimeProfile{AgentID: "codex", AuthMode: "api_key", Secret: secret, BaseURL: provider.URL + "/v1", Env: map[string]string{"CODEX_HOME": original, "AUTO_APPROVAL_TOKEN": "forbidden-token"}}
	}
	request.Container = supervisionJobPrefix + uuid.NewString()
	resolved, resumed := false, false
	request.Resolved = func(info supervisionCLIInfo) error {
		resolved = info.AgentID == "codex" && info.Version == "0.166.0" && info.Contract == supervisionCodexContract && strings.EqualFold(filepath.Ext(info.Path), ".exe")
		return nil
	}
	request.Handshake = func(identity supervisionProcessIdentity, resume func() error) error {
		if !resolved || identity.Container != request.Container {
			t.Error("process bypassed frozen specification")
		}
		resumed = true
		return resume()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	receipt, err := (&codexCLIRunner{config: Config{CodexPath: binary}}).runSupervisionScheduler(ctx, request, &claudeOutputTestSink{})
	if err != nil {
		t.Fatalf("native Codex contract: %v; receipt=%+v", err, receipt)
	}
	if !resumed || !receipt.Initialized || !receipt.ProcessWaited || !receipt.ContainerQuiet || receipt.Output != "done" {
		t.Fatalf("incomplete protocol: %+v", receipt)
	}
	mu.Lock()
	defer mu.Unlock()
	wantRequests := 1
	if mode == "malicious_tool" {
		wantRequests = 2
	}
	if len(requests) != wantRequests {
		t.Fatalf("unexpected provider requests: %d", len(requests))
	}
	raw, _ := json.Marshal(requests)
	if strings.Contains(string(raw), canary) || strings.Contains(string(raw), secret) {
		t.Fatal("unauthorized rule or authentication leaked into model input")
	}
	tools, ok := requests[0]["tools"].([]any)
	if !ok && requests[0]["tools"] != nil {
		t.Fatal("tools declaration has unexpected format")
	}
	for _, tool := range tools {
		entry := tool.(map[string]any)
		if entry["name"] != "request_user_input" {
			t.Fatalf("unexpected execution tool: %v", entry["name"])
		}
	}
	if !strings.Contains(string(raw), "schemaVersion") || !strings.Contains(string(raw), "全局调度") {
		t.Fatal("authorized observation or fixed role instructions missing")
	}
	auth, err := os.ReadFile(filepath.Join(original, "auth.json"))
	if err != nil || string(auth) != `{"OPENAI_API_KEY":"`+secret+`"}` {
		t.Fatal("source authentication changed")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("model-supplied unregistered tool created a file")
	}
}

func writeSupervisionCodexFinal(w http.ResponseWriter, text string) {
	response := map[string]any{"id": "resp_contract", "object": "response", "created_at": 0, "status": "in_progress", "model": "gpt-5.4", "output": []any{}}
	writeCodexPOCEvent(w, "response.created", map[string]any{"type": "response.created", "response": response})
	item := map[string]any{"id": "msg_contract", "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}
	writeCodexPOCEvent(w, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item})
	item["status"] = "completed"
	item["content"] = []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}
	writeCodexPOCEvent(w, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
	response["status"], response["output"] = "completed", []any{item}
	writeCodexPOCEvent(w, "response.completed", map[string]any{"type": "response.completed", "response": response})
}

func writeSupervisionCodexTool(w http.ResponseWriter, name, arguments string) {
	response := map[string]any{"id": "resp_contract_tool", "object": "response", "created_at": 0, "status": "in_progress", "model": "gpt-5.4", "output": []any{}}
	writeCodexPOCEvent(w, "response.created", map[string]any{"type": "response.created", "response": response})
	item := map[string]any{"id": "tool_contract", "type": "function_call", "call_id": "call_contract", "name": name, "arguments": "", "status": "in_progress"}
	writeCodexPOCEvent(w, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item})
	item["arguments"], item["status"] = arguments, "completed"
	writeCodexPOCEvent(w, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
	response["status"], response["output"] = "completed", []any{item}
	writeCodexPOCEvent(w, "response.completed", map[string]any{"type": "response.completed", "response": response})
}

func TestSupervisionCodexInstalledWorkerPersistsAndSettles(t *testing.T) {
	if os.Getenv("MILEVIA_RUN_SUPERVISION_CODEX_LIVE") != "1" {
		t.Skip("explicit real Codex worker experiment")
	}
	binary, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	s, _, _, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	s.codexRunner = &codexCLIRunner{config: Config{CodexPath: binary}}
	if err := s.updateSupervisionRoles(context.Background(), ss.ID, ss.StateVersion, supervisionRoleConfigs{"scheduler": {AgentID: "codex"}}); err != nil {
		t.Fatal(err)
	}
	startSupervisionForTest(t, s, ss.ID, now)
	lease, decision := decisionForTest(t, s, ss.ID, now)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	if err := s.executeSupervisionWorkerDecision(ctx, decision, lease); err != nil {
		t.Fatal(err)
	}
	var raw, state, output string
	if err := s.db.QueryRow(`select s.spec,e.state,d.output from supervision_execution_specs s join supervision_executions e on e.execution_id=s.execution_id join supervision_work_items w on w.execution_id=e.execution_id join supervision_decisions d on d.id=w.decision_id where e.execution_id=?`, decision.ExecutionID).Scan(&raw, &state, &output); err != nil {
		t.Fatal(err)
	}
	var spec supervisionExecutionSpec
	if json.Unmarshal([]byte(raw), &spec) != nil || spec.CLI.AgentID != "codex" || spec.CLI.Contract != supervisionCodexContract || state != "quiescent" || output == "" {
		t.Fatalf("actual specification or settlement missing: %s %s", raw, state)
	}
	t.Logf("actual CLI=%s version=%s contract=%s, frozen specification and quiescent process", spec.CLI.AgentID, spec.CLI.Version, spec.CLI.Contract)
}

func TestSupervisionCodexInstalledCancellationSettlesContainer(t *testing.T) {
	if os.Getenv("MILEVIA_RUN_SUPERVISION_CODEX_CONTRACT") != "1" {
		t.Skip("explicit installed Codex cancellation test")
	}
	binary, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	request := schedulerCLIRequestForTest()
	request.Model, request.ReasoningEffort = "gpt-5.4", ""
	request.Profile = &AgentRuntimeProfile{AgentID: "codex", AuthMode: "api_key", Secret: "unused-fake-key", BaseURL: "http://127.0.0.1:1/v1"}
	request.Container = supervisionJobPrefix + uuid.NewString()
	request.Handshake = func(_ supervisionProcessIdentity, resume func() error) error { return resume() }
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sink := &cancelSchedulerOnInitSink{cancel: cancel}
	receipt, err := (&codexCLIRunner{config: Config{CodexPath: binary}}).runSupervisionScheduler(ctx, request, sink)
	if err == nil || !receipt.ProcessStarted || !receipt.ProcessWaited || !receipt.ContainerQuiet {
		t.Fatalf("canceled execution not settled: %+v %v", receipt, err)
	}
	quiet, err := supervisionContainerQuiescent(request.Container)
	if err != nil || !quiet {
		t.Fatalf("cancellation left container active: %t %v", quiet, err)
	}
}
