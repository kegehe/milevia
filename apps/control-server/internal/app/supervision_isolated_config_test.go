package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

func TestSupervisionQoderProjectsTargetModelWithoutAutomation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("QODER_CONFIG_DIR", dir)
	t.Setenv("QODERCN_CONFIG_DIR", "")
	t.Setenv("QODER_CONFIG_DIR_NAME", "")
	t.Setenv("QODER_MODEL", "")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"model":{"name":"target-default","reasoningEffort":"high","preferences":{"target-default":{"contextWindow":200000}}},"hooks":{"canary":"must-not-run"},"mcpServers":{"canary":{}},"security":{"approvalMode":"yolo"}}`)
	model, projection, err := supervisionQoderModelSettings("")
	if err != nil || model != "target-default" || strings.Contains(string(mustJSON(projection)), "canary") || !strings.Contains(string(mustJSON(projection)), "200000") {
		t.Fatal("target model/preferences not isolated", model, err)
	}
	args := supervisionQoderArgs(supervisionSchedulerCLIRequest{Model: model}, supervisionCLIInfo{Entrypoint: "qodercli.js"}, t.TempDir(), projection)
	for i, arg := range args {
		if arg == "--settings" {
			var settings map[string]any
			if json.Unmarshal([]byte(args[i+1]), &settings) != nil || settings["disableAllHooks"] != true || settings["model"] == nil || strings.Contains(args[i+1], "canary") {
				t.Fatal("model projection weakened isolation")
			}
		}
	}
	t.Setenv("QODER_MODEL", "environment-choice")
	if model, _, err := supervisionQoderModelSettings(""); err != nil || model != "environment-choice" {
		t.Fatal("native environment selection lost", model, err)
	}
	if model, _, err := supervisionQoderModelSettings("explicit-choice"); err != nil || model != "explicit-choice" {
		t.Fatal("role selection lost", model, err)
	}
	for _, body := range []string{`{"providers":{"custom":{"apiKey":"secret-canary"}}}`, `{"modelConfigs":{"aliases":{"custom":"other"}}}`, `{"model":{"unknown":"secret-canary"}}`, `{"model":{"name":123}}`, `[]`, `null`, `{"model":`} {
		write(body)
		if _, _, err := supervisionQoderModelSettings(""); err == nil || strings.Contains(err.Error(), "secret-canary") {
			t.Fatal("unknown config silently changed route or leaked content", err)
		}
	}
}

func TestSupervisionCursorProjectsModelParametersAndPreservesDenyGate(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	t.Setenv("CURSOR_CONFIG_DIR", dir)
	config := filepath.Join(dir, "cli-config.json")
	if err := os.WriteFile(config, []byte(`{"selectedModel":{"modelId":"target-model","parameters":[{"id":"thinking","value":"high"}]},"modelParameters":{"target-model":[{"id":"thinking","value":"high"}]},"maxMode":true,"permissions":{"allow":["Shell(*)"]},"statusLine":{"command":"canary-must-not-run"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	model, projection, err := supervisionCursorModelSettings("")
	if err != nil || model != "target-model" || strings.Contains(string(mustJSON(projection)), "canary") {
		t.Fatal("model selection lost or automation copied", model, err)
	}
	if err := prepareSupervisionCursorIsolation(home, supervisionCLIInfo{Path: filepath.Join(home, "node")}, projection); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".cursor", "cli-config.json"))
	var actual struct {
		SelectedModel struct {
			ModelID    string           `json:"modelId"`
			Parameters []map[string]any `json:"parameters"`
		} `json:"selectedModel"`
		Permissions struct{ Allow, Deny []string } `json:"permissions"`
		MaxMode     bool                           `json:"maxMode"`
	}
	if err != nil || json.Unmarshal(raw, &actual) != nil || actual.SelectedModel.ModelID != model || len(actual.SelectedModel.Parameters) != 1 || !actual.MaxMode || len(actual.Permissions.Allow) != 0 || len(actual.Permissions.Deny) == 0 || strings.Contains(string(raw), "canary") {
		t.Fatal("projection lost model parameters or weakened deny gate", err)
	}
	for _, body := range []string{`{"bedrock":{"enabled":true}}`, `{"awsAuthRefresh":"secret-canary"}`, `{"selectedModel":{}}`, `[]`} {
		if err := os.WriteFile(config, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := supervisionCursorModelSettings(""); err == nil || strings.Contains(err.Error(), "secret-canary") {
			t.Fatal("unsupported route was silently ignored", err)
		}
	}
	if err := os.WriteFile(config, []byte(`{"model":{"modelId":"legacy-choice"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if model, projection, err := supervisionCursorModelSettings(""); err != nil || model != "" || projection["model"] == nil {
		t.Fatal("legacy native model was overridden by auto", model, err)
	}
}

func TestSupervisionUnverifiedCLIRejectedByExecutionReceipts(t *testing.T) {
	info := supervisionCLIInfo{AgentID: "qoder", ValidationPending: true}
	var s *Server // The gate must run before any transaction or dispatch.
	if err := s.recordSupervisionExecutionSpec(context.Background(), supervisionExecutionPermit{}, nil, info); !errors.Is(err, errSupervisionValidationPending) {
		t.Fatal(err)
	}
	if _, err := s.recordSupervisionTargetResolution(context.Background(), supervisionExecutionPermit{}, targetexec.Control{}, targetexec.Input{}, info); !errors.Is(err, errSupervisionValidationPending) {
		t.Fatal(err)
	}
	spec, input := supervisionResolutionInput()
	adapter := supervisionResolutionAdapter{run: func(_ context.Context, r supervisionSchedulerCLIRequest, _ AgentRunSink) (supervisionSchedulerCLIReceipt, error) {
		return supervisionSchedulerCLIReceipt{ContainerQuiet: true}, r.Resolved(supervisionCLIInfo{AgentID: spec.AgentID, Path: "/bin/fixture", Version: "1", Contract: "fixture", ValidationPending: true})
	}}
	if _, err := resolveSupervisionTargetCLI(context.Background(), adapter, spec, input); !errors.Is(err, errSupervisionValidationPending) {
		t.Fatal("pending remote resolution was accepted", err)
	}
}
