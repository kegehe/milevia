package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNormalizeRunCommandsMigratesLegacySingleCommand 覆盖升级路径：老库只有 command
// 列，读出来必须变成一条（可编辑、可继续跑的）命令，而不是让用户重填。
func TestNormalizeRunCommandsMigratesLegacySingleCommand(t *testing.T) {
	cfg := RunConfig{Command: "  npm run dev  "}
	normalizeRunCommands(&cfg)

	if len(cfg.Commands) != 1 {
		t.Fatalf("commands=%#v, want one migrated entry", cfg.Commands)
	}
	if cfg.Commands[0].ID != legacyRunCommandID || cfg.Commands[0].Command != "npm run dev" {
		t.Fatalf("unexpected migrated command: %#v", cfg.Commands[0])
	}
	if cfg.SelectedCommandID != legacyRunCommandID {
		t.Fatalf("selected=%q, want legacy ID", cfg.SelectedCommandID)
	}

	// 幂等：再跑一遍不应该又复制出一条，也不应该改动任何字段。
	before := cfg
	normalizeRunCommands(&cfg)
	if len(cfg.Commands) != 1 || cfg.SelectedCommandID != before.SelectedCommandID || cfg.Command != before.Command {
		t.Fatalf("normalize is not idempotent: %#v", cfg)
	}
}

// TestNormalizeRunCommandsDropsBlankRowsAndFixesSelection 覆盖编辑期的不完整状态：
// 前端"添加一条"点出来的空行不落库；选中项失效时回落到第一条；Command 恒等于所选命令。
func TestNormalizeRunCommandsDropsBlankRowsAndFixesSelection(t *testing.T) {
	cfg := RunConfig{
		Command: "stale",
		Commands: []RunCommand{
			{ID: "a", Name: "开发环境", Command: "npm run dev"},
			{ID: "", Name: "", Command: "", EnvVars: map[string]string{}},
			{ID: "b", Name: "预发", Command: "npm run dev:staging", EnvVars: map[string]string{"API": "https://staging"}},
		},
		SelectedCommandID: "missing",
	}
	normalizeRunCommands(&cfg)

	if len(cfg.Commands) != 2 {
		t.Fatalf("commands=%#v, want blank row dropped", cfg.Commands)
	}
	if cfg.SelectedCommandID != "a" {
		t.Fatalf("selected=%q, want fallback to the first command", cfg.SelectedCommandID)
	}
	if cfg.Command != "npm run dev" {
		t.Fatalf("command mirror=%q, want the selected command text", cfg.Command)
	}
	if cfg.Commands[1].EnvVars["API"] != "https://staging" {
		t.Fatalf("per-command env vars lost: %#v", cfg.Commands[1])
	}
}

// TestNormalizeRunCommandsKeepsNamedCommandWithoutBody 只填了名称没填命令的行要留下，
// 交给校验给出"第 N 条不能为空"，而不是被当成空行悄悄丢掉用户的输入。
func TestNormalizeRunCommandsKeepsNamedCommandWithoutBody(t *testing.T) {
	cfg := RunConfig{Commands: []RunCommand{{ID: "a", Name: "生产"}}}
	normalizeRunCommands(&cfg)

	if len(cfg.Commands) != 1 {
		t.Fatalf("commands=%#v, want the named row kept", cfg.Commands)
	}
	if err := validateRunCommands(cfg.Commands); err == nil || !strings.Contains(err.Error(), "第 1 条启动命令不能为空") {
		t.Fatalf("validate error=%v, want a readable empty-command error", err)
	}
}

// TestResolveRunCommandPrefersExplicitSelection 显式指定的 ID 最高优先，且对不上时报错
// 而不是悄悄换一条 —— 启动"生产环境"跑成"开发环境"比启动失败更糟。
func TestResolveRunCommandPrefersExplicitSelection(t *testing.T) {
	cfg := RunConfig{
		Commands:          []RunCommand{{ID: "a", Command: "npm run dev"}, {ID: "b", Command: "npm run build"}},
		SelectedCommandID: "a",
	}

	selected, err := cfg.resolveRunCommand("b")
	if err != nil || selected.Command != "npm run build" {
		t.Fatalf("explicit selection: command=%q err=%v", selected.Command, err)
	}

	selected, err = cfg.resolveRunCommand("")
	if err != nil || selected.Command != "npm run dev" {
		t.Fatalf("fallback to persisted selection: command=%q err=%v", selected.Command, err)
	}

	if _, err := cfg.resolveRunCommand("gone"); err == nil {
		t.Fatal("expected a stale command ID to be rejected")
	}

	empty := RunConfig{}
	if _, err := empty.resolveRunCommand(""); err == nil || !strings.Contains(err.Error(), "请先配置启动命令") {
		t.Fatalf("empty config error=%v", err)
	}
}

// TestMergedRunEnvVarsOverridesGlobal 命令专属变量叠加在全局之上（同名覆盖），
// 这是"同一个入口、不同环境参数"的实现基础。
func TestMergedRunEnvVarsOverridesGlobal(t *testing.T) {
	global := map[string]string{"PORT": "3000", "SHARED": "global"}
	command := RunCommand{EnvVars: map[string]string{"PORT": "4000"}}

	merged := mergedRunEnvVars(global, command)
	if merged["PORT"] != "4000" || merged["SHARED"] != "global" {
		t.Fatalf("merged=%v", merged)
	}
	if global["PORT"] != "3000" {
		t.Fatalf("global env vars must not be mutated: %v", global)
	}
	if len(mergedRunEnvVars(nil, RunCommand{})) != 0 {
		t.Fatal("want an empty (non-nil) map for a command without env vars")
	}
}

// TestRunConfigMultiCommandCRUD 端到端覆盖 HTTP 层：保存多条命令、选中项持久化、
// 老式单命令请求体仍能读回成一条命令。
func TestRunConfigMultiCommandCRUD(t *testing.T) {
	server, projectID := seedServerWithProject(t)
	handler := server.routes()

	body := `{"workDir":"","command":"","envVars":{"PORT":"3000"},"commands":[{"id":"dev","name":"开发环境","command":"npm run dev"},{"id":"staging","name":"预发","command":"npm run dev:staging","envVars":{"API":"https://staging"}}],"selectedCommandId":"staging"}`
	saved := httptest.NewRecorder()
	handler.ServeHTTP(saved, httptest.NewRequest(http.MethodPut, "/api/projects/"+projectID+"/run/config", bytes.NewBufferString(body)))
	if saved.Code != http.StatusOK {
		t.Fatalf("save config: %d body=%s", saved.Code, saved.Body.String())
	}

	loaded := httptest.NewRecorder()
	handler.ServeHTTP(loaded, httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/run/config", nil))
	var cfg RunConfig
	if err := json.NewDecoder(loaded.Body).Decode(&cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if len(cfg.Commands) != 2 || cfg.SelectedCommandID != "staging" {
		t.Fatalf("unexpected config: %#v", cfg)
	}
	// command 是所选命令的镜像，随选中项走。
	if cfg.Command != "npm run dev:staging" {
		t.Fatalf("command mirror=%q", cfg.Command)
	}
	if cfg.Commands[1].EnvVars["API"] != "https://staging" {
		t.Fatalf("per-command env vars lost: %#v", cfg.Commands[1])
	}
}

// TestRunConfigRejectsCommandWithoutBody 保存"只填了名称没填命令"的行要报错，
// 而不是存下来一个启动必失败的配置。
func TestRunConfigRejectsCommandWithoutBody(t *testing.T) {
	server, projectID := seedServerWithProject(t)
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/api/projects/"+projectID+"/run/config", bytes.NewBufferString(`{"workDir":"","command":"","envVars":{},"commands":[{"id":"a","name":"生产","command":""}]}`)))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "第 1 条启动命令不能为空") {
		t.Fatalf("unexpected error body: %s", response.Body.String())
	}
}

// TestStartProjectRunRejectsUnknownCommandID 启动时带的命令 ID 已不存在（另一个页面刚删了
// 它）要明确报错，不能退回随便一条。
func TestStartProjectRunRejectsUnknownCommandID(t *testing.T) {
	server, projectID := seedServerWithProject(t)

	saved := httptest.NewRecorder()
	server.routes().ServeHTTP(saved, httptest.NewRequest(http.MethodPut, "/api/projects/"+projectID+"/run/config", bytes.NewBufferString(`{"workDir":"","command":"","envVars":{},"commands":[{"id":"dev","name":"开发","command":"echo dev"}],"selectedCommandId":"dev"}`)))
	if saved.Code != http.StatusOK {
		t.Fatalf("save config: %d body=%s", saved.Code, saved.Body.String())
	}

	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/run/start", bytes.NewBufferString(`{"selectedCommandId":"gone"}`)))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "已不存在") {
		t.Fatalf("unexpected error body: %s", response.Body.String())
	}
}

// TestStartProjectRunUsesSelectedCommand 选中第二条启动时，runner 里生效的必须是第二条
// 的命令与环境变量（命令专属变量覆盖全局同名项）。
func TestStartProjectRunUsesSelectedCommand(t *testing.T) {
	server, projectID := seedServerWithProject(t)

	saved := httptest.NewRecorder()
	server.routes().ServeHTTP(saved, httptest.NewRequest(http.MethodPut, "/api/projects/"+projectID+"/run/config", bytes.NewBufferString(`{"workDir":"","command":"","envVars":{"PORT":"3000"},"commands":[{"id":"dev","name":"开发","command":"echo dev"},{"id":"staging","name":"预发","command":"echo staging","envVars":{"PORT":"4000"}}],"selectedCommandId":"dev"}`)))
	if saved.Code != http.StatusOK {
		t.Fatalf("save config: %d body=%s", saved.Code, saved.Body.String())
	}

	started := httptest.NewRecorder()
	server.routes().ServeHTTP(started, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/run/start", bytes.NewBufferString(`{"selectedCommandId":"staging"}`)))
	if started.Code != http.StatusAccepted {
		t.Fatalf("start: %d body=%s", started.Code, started.Body.String())
	}
	t.Cleanup(func() {
		stop := httptest.NewRecorder()
		server.routes().ServeHTTP(stop, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/run/stop", nil))
	})

	status := httptest.NewRecorder()
	server.routes().ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/run/status", nil))
	var snapshot RunStatusResponse
	if err := json.NewDecoder(status.Body).Decode(&snapshot); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if snapshot.Command != "echo staging" {
		t.Fatalf("running command=%q, want the selected one", snapshot.Command)
	}

	runner, err := server.projectRunManagerForExistingProject(context.Background(), projectID)
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	local, ok := runner.(*projectRunner)
	if !ok {
		t.Fatalf("unexpected runner type %T", runner)
	}
	local.mu.RLock()
	port := local.envVars["PORT"]
	local.mu.RUnlock()
	if port != "4000" {
		t.Fatalf("PORT=%q, want the command-specific override", port)
	}
}

// TestLoadRunConfigMigratesLegacyRowWithoutCommands 直接写老形态的行（只有 command 列），
// 读出来必须是一条可用的命令 —— 升级后老项目不该丢命令。
func TestLoadRunConfigMigratesLegacyRowWithoutCommands(t *testing.T) {
	server, projectID := seedServerWithProject(t)
	if _, err := server.db.Exec(`insert into project_run_configs (project_id, work_dir, command, env_vars, execution_target, commands, selected_command_id, updated_at) values (?, '', 'npm run dev', '{}', 'auto', '[]', '', ?)`, projectID, "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	cfg, err := server.loadRunConfig(context.Background(), projectID)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if len(cfg.Commands) != 1 || cfg.Commands[0].Command != "npm run dev" || cfg.SelectedCommandID != legacyRunCommandID {
		t.Fatalf("legacy row not migrated: %#v", cfg)
	}
}

// TestSaveRunConfigClearsCommandsWhenEmptied 清空所有命令后不能再被"旧 command 迁移"复活：
// 用户明确删掉的东西不该回来。
func TestSaveRunConfigClearsCommandsWhenEmptied(t *testing.T) {
	server, projectID := seedServerWithProject(t)

	saved := httptest.NewRecorder()
	server.routes().ServeHTTP(saved, httptest.NewRequest(http.MethodPut, "/api/projects/"+projectID+"/run/config", bytes.NewBufferString(`{"workDir":"","command":"","envVars":{},"commands":[],"selectedCommandId":""}`)))
	if saved.Code != http.StatusOK {
		t.Fatalf("save config: %d body=%s", saved.Code, saved.Body.String())
	}

	cfg, err := server.loadRunConfig(context.Background(), projectID)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if len(cfg.Commands) != 0 || cfg.Command != "" {
		t.Fatalf("cleared config resurrected: %#v", cfg)
	}
	if _, err := cfg.resolveRunCommand(""); err == nil || !strings.Contains(err.Error(), "请先配置启动命令") {
		t.Fatalf("start error=%v, want the missing-command message", err)
	}
}
