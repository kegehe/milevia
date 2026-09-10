package app

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestMCPToolMatchesGlob(t *testing.T) {
	cases := []struct {
		pattern string
		tool    string
		want    bool
	}{
		{"mcp__github__*", "mcp__github__create_issue", true},
		{"mcp__github__*", "mcp__gitlab__create_issue", false},
		{"mcp__*", "mcp__github__create_issue", true},
		{"mcp__github__create_issue", "mcp__github__create_issue", true},
		{"", "mcp__github__create_issue", false},
		{"Bash", "mcp__github__create_issue", false},
	}
	for _, tc := range cases {
		if got := mcpToolMatchesGlob(tc.pattern, tc.tool); got != tc.want {
			t.Fatalf("mcpToolMatchesGlob(%q,%q)=%v want %v", tc.pattern, tc.tool, got, tc.want)
		}
	}
	if !mcpAutoApproveAllows([]string{"Bash", "mcp__github__*"}, "mcp__github__list_repos", nil) {
		t.Fatal("expected auto approve to match MCP glob")
	}
	if mcpAutoApproveAllows([]string{"mcp__github__*"}, "Bash", nil) {
		t.Fatal("expected Bash not to match MCP glob")
	}
	// Bash 形态必须看命令本身：`Bash` 放行任意命令，`Bash(git status*)` 只放行该前缀。
	bashInput := json.RawMessage(`{"command":"git status --short"}`)
	if !mcpAutoApproveAllows([]string{"Bash"}, "Bash", bashInput) {
		t.Fatal("plain Bash pattern should allow any Bash call")
	}
	if !mcpAutoApproveAllows([]string{"Bash(git status*)"}, "Bash", bashInput) {
		t.Fatal("Bash(<prefix>*) should allow a matching command")
	}
	if mcpAutoApproveAllows([]string{"Bash(rm -rf*)"}, "Bash", bashInput) {
		t.Fatal("Bash(<prefix>*) must not allow a different command")
	}
	if mcpAutoApproveAllows([]string{"Bash(git status)"}, "Bash", bashInput) {
		t.Fatal("Bash(<exact>) must not allow a longer command")
	}
	if !mcpAutoApproveAllows([]string{"Bash(git status)", "Bash(git commit:*)"}, "Bash", json.RawMessage(`{"command":"git status"}`)) {
		t.Fatal("Bash(<exact>) should allow the identical command")
	}
	if !mcpAutoApproveAllows([]string{"Bash(git commit:*)"}, "Bash", json.RawMessage(`{"command":"git commit -m x"}`)) {
		t.Fatal("Bash(<prefix>:*) should allow a matching command")
	}
	if mcpAutoApproveAllows([]string{"Bash(git commit:*)"}, "Bash", nil) {
		t.Fatal("Bash pattern must not allow when tool input is missing")
	}
	if mcpAutoApproveAllows([]string{"Bash"}, "mcp__github__x", nil) {
		t.Fatal("Bash pattern must not allow an MCP tool")
	}
}

func TestIsApprovableToolName(t *testing.T) {
	for _, name := range []string{"Bash", "mcp__github__create_issue", "mcp__x__y"} {
		if !isApprovableToolName(name) {
			t.Fatalf("%q should be approvable", name)
		}
	}
	for _, name := range []string{"Read", "Write", "", "bash"} {
		if isApprovableToolName(name) {
			t.Fatalf("%q should not be approvable", name)
		}
	}
}

func TestVersionAtLeast(t *testing.T) {
	cases := []struct {
		version             string
		major, minor, patch int
		want                bool
	}{
		{"2.1.266", 2, 1, 246, true},
		{"2.1.246", 2, 1, 246, true},
		{"2.1.245", 2, 1, 246, false},
		{"2.0.9", 2, 1, 246, false},
		{"3.0.0", 2, 1, 246, true},
		{"2.1.0", 2, 1, 0, true},
		{"", 2, 1, 246, false},
		{"not-a-version", 2, 1, 246, false},
	}
	for _, tc := range cases {
		if got := versionAtLeast(tc.version, tc.major, tc.minor, tc.patch); got != tc.want {
			t.Fatalf("versionAtLeast(%q,%d.%d.%d)=%v want %v", tc.version, tc.major, tc.minor, tc.patch, got, tc.want)
		}
	}
}

func TestResolveMCPPlaceholders(t *testing.T) {
	if got := resolveMCPPlaceholders("npx", agentTargetEnvWindows, `C:\proj`); got != "npx" {
		t.Fatalf("unrelated value changed: %q", got)
	}
	if got := resolveMCPPlaceholders(`${PROJECT_DIR}/docs`, agentTargetEnvWindows, `D:\proj`); got != `D:\proj/docs` {
		t.Fatalf("windows resolve = %q", got)
	}
	if got := resolveMCPPlaceholders(`${PROJECT_DIR}/docs`, agentTargetEnvWSL, `D:\proj`); got != "/mnt/d/proj/docs" {
		t.Fatalf("wsl resolve = %q", got)
	}
	// 其它 ${VAR} 占位符保持原样，交给 CLI 按进程环境展开。
	if got := resolveMCPPlaceholders("${HOME}/.config", agentTargetEnvWindows, `D:\proj`); got != "${HOME}/.config" {
		t.Fatalf("home placeholder should be preserved: %q", got)
	}
}

func TestMCPSecretEnvName(t *testing.T) {
	// 同一引用恒得同名；连字符等非字母数字字符替换为下划线（shell 变量名不允许连字符）。
	name := mcpSecretEnvName("sec_0f8fad5b-d9cb-469f-a165-70867728950e")
	if name != "MCP_SEC_0f8fad5b_d9cb_469f_a165_70867728950e" {
		t.Fatalf("unexpected env name: %q", name)
	}
	if name != mcpSecretEnvName("sec_0f8fad5b-d9cb-469f-a165-70867728950e") {
		t.Fatal("env name must be deterministic for the same reference")
	}
	for _, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_') {
			t.Fatalf("env name has an unsafe character: %q", name)
		}
	}
}

func TestResolveMCPValuesModes(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	ref, err := s.profileSecrets.Store(s.db, ctx, "tok-123")
	if err != nil {
		t.Fatalf("store secret: %v", err)
	}
	values := map[string]string{"TOKEN": ref, "LOG": "info"}

	// useEnvRefs=true（Windows/WSL）：占位符 + env 增项，返回值不含明文。
	out, additions, ok := s.resolveMCPValues(ctx, values, true, agentTargetEnvWindows, "")
	if !ok {
		t.Fatal("expected env-ref resolution to succeed")
	}
	if out["LOG"] != "info" {
		t.Fatalf("plain value changed: %q", out["LOG"])
	}
	if out["TOKEN"] != "${"+mcpSecretEnvName(ref)+"}" {
		t.Fatalf("expected placeholder, got %q", out["TOKEN"])
	}
	if len(additions) != 1 || additions[0] != mcpSecretEnvName(ref)+"=tok-123" {
		t.Fatalf("unexpected env additions: %v", additions)
	}

	// useEnvRefs=false（SSH 远端无 env 通道）：内联明文，无增项。
	out, additions, ok = s.resolveMCPValues(ctx, values, false, agentTargetEnvWindows, "")
	if !ok {
		t.Fatal("expected inline resolution to succeed")
	}
	if out["TOKEN"] != "tok-123" {
		t.Fatalf("expected inline plaintext, got %q", out["TOKEN"])
	}
	if len(additions) != 0 {
		t.Fatalf("inline mode must not produce env additions: %v", additions)
	}

	// 解密失败的引用应整体放弃该 server。
	if _, _, ok := s.resolveMCPValues(ctx, map[string]string{"TOKEN": "sec_missing"}, true, agentTargetEnvWindows, ""); ok {
		t.Fatal("expected missing secret reference to fail resolution")
	}

	// 非密钥值里的 ${PROJECT_DIR} 必须解析（docs/34 §5.3）：否则预览对了、注入却留字面量。
	resolved, _, ok := s.resolveMCPValues(ctx, map[string]string{"ROOT": "${PROJECT_DIR}/docs"}, false, agentTargetEnvWSL, `D:\proj`)
	if !ok {
		t.Fatal("expected placeholder resolution to succeed")
	}
	if resolved["ROOT"] != "/mnt/d/proj/docs" {
		t.Fatalf("env placeholder not resolved: %q", resolved["ROOT"])
	}
	// 未选项目时项目路径为空，占位符原样保留（由上层决定是否提示），不静默变成空串。
	kept, _, _ := s.resolveMCPValues(ctx, map[string]string{"ROOT": "${PROJECT_DIR}/docs"}, false, agentTargetEnvWindows, "")
	if kept["ROOT"] != "${PROJECT_DIR}/docs" {
		t.Fatalf("empty project path must keep placeholder, got %q", kept["ROOT"])
	}
}

func TestSplitSecretKeys(t *testing.T) {
	public, keys := splitSecretKeys(map[string]string{"LOG": "info", "TOKEN": "sec_abc"})
	if len(keys) != 1 || keys[0] != "TOKEN" {
		t.Fatalf("secret keys = %v", keys)
	}
	if public["LOG"] != "info" {
		t.Fatalf("public map = %v", public)
	}
	if _, leaked := public["TOKEN"]; leaked {
		t.Fatal("secret reference leaked into public map")
	}
}

func TestMergePublicEnvPreservesSecretRefs(t *testing.T) {
	current := map[string]string{"TOKEN": "sec_abc", "LOG": "info"}
	merged := mergePublicEnv(current, map[string]string{"LEVEL": "debug"})
	if merged["TOKEN"] != "sec_abc" {
		t.Fatal("secret ref should be preserved")
	}
	if _, ok := merged["LOG"]; ok {
		t.Fatal("non-secret keys should be replaced when provided")
	}
	if merged["LEVEL"] != "debug" {
		t.Fatal("provided key missing")
	}
}

func insertTestMCPServer(t *testing.T, s *Server, name, scope, projectID, transport, command string, envRaw map[string]string, environments, agents []string) string {
	t.Helper()
	ctx := context.Background()
	argsJSON, _ := marshalStringSlice([]string{"-y", "@modelcontextprotocol/server-github"})
	envJSON, _ := marshalStringMap(envRaw)
	environmentsJSON, _ := marshalStringSlice(environments)
	agentsJSON, _ := marshalStringSlice(agents)
	id := "mcp_test_" + name + "_" + scope
	if _, err := s.db.ExecContext(ctx, `insert into mcp_servers (`+mcpServerColumns+`) values (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, name, name, "", transport, command, argsJSON, envJSON, "",
		"", "{}",
		scope, projectID, environmentsJSON, agentsJSON,
		true, "[]", 20, 60, "manual", time.Now().UTC(), time.Now().UTC()); err != nil {
		t.Fatalf("insert mcp server: %v", err)
	}
	return id
}

func TestPrepareMCPInjectionWindows(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	projectID := "proj-mcp"
	projectPath := t.TempDir()
	if _, err := s.db.ExecContext(ctx, `insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values (?,?,?,?,?,?,?)`,
		projectID, "P", projectPath, s.localRunnerID(), "main", true, time.Now().UTC()); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	secretRef, err := s.profileSecrets.Store(s.db, ctx, "super-secret-token")
	if err != nil {
		t.Fatalf("store secret: %v", err)
	}
	insertTestMCPServer(t, s, "github", mcpScopeGlobal, "", mcpTransportStdio, "npx",
		map[string]string{"GITHUB_PERSONAL_ACCESS_TOKEN": secretRef, "LOG_LEVEL": "info"},
		[]string{"windows"}, []string{"claude-code"})

	injection := s.prepareMCPInjection(ctx, projectID, projectPath, "claude-code", s.localRunnerID(), "run-abc")
	defer injection.done()
	if injection.JSON == "" {
		t.Fatal("expected MCP JSON to be generated")
	}
	if !injection.Strict {
		t.Fatal("expected strict mode by default")
	}
	if injection.LocalPath == "" {
		t.Fatal("expected a local config path")
	}
	raw, err := os.ReadFile(injection.LocalPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	content := string(raw)
	if !strings.Contains(content, `"mcpServers"`) || !strings.Contains(content, `"github"`) {
		t.Fatalf("unexpected config: %s", content)
	}
	if strings.Contains(content, mcpSecretRefPrefix) {
		t.Fatal("config must not contain secret references")
	}
	// Windows/WSL：密钥不落盘——文件里只有 ${MCP_SEC_*} 占位符，真值走进程环境。
	if strings.Contains(content, "super-secret-token") {
		t.Fatal("runtime config must not contain the plaintext secret on Windows/WSL")
	}
	if !strings.Contains(content, "${MCP_SEC_") {
		t.Fatalf("expected a ${MCP_SEC_*} placeholder in runtime config: %s", content)
	}
	envName := ""
	for _, item := range injection.Env {
		if name, value, found := strings.Cut(item, "="); found && value == "super-secret-token" {
			envName = name
		}
	}
	if envName == "" {
		t.Fatalf("expected a secret env addition carrying the plaintext, got %v", injection.Env)
	}
	if !strings.HasPrefix(envName, "MCP_SEC_") {
		t.Fatalf("secret env name should use the MCP_SEC_ prefix, got %q", envName)
	}
	// 模拟 CLI 按进程环境展开 ${VAR}：应还原出密钥。
	expanded := strings.ReplaceAll(content, "${"+envName+"}", "super-secret-token")
	if !strings.Contains(expanded, "super-secret-token") {
		t.Fatal("placeholder should expand to the secret via the injected env")
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("config is not valid JSON: %v", err)
	}
	// 清理后文件应被删除。
	injection.done()
	if _, err := os.Stat(injection.LocalPath); !os.IsNotExist(err) {
		t.Fatalf("expected runtime config to be removed, stat err=%v", err)
	}
}

func TestPrepareMCPInjectionFiltersEnvironmentAndAgent(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	projectID := "proj-filter"
	projectPath := t.TempDir()
	if _, err := s.db.ExecContext(ctx, `insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values (?,?,?,?,?,?,?)`,
		projectID, "P", projectPath, s.localRunnerID(), "main", true, time.Now().UTC()); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	// 仅适用于 ssh 远端 → 在 windows 目标下不注入。
	insertTestMCPServer(t, s, "remote-only", mcpScopeGlobal, "", mcpTransportStdio, "npx", nil, []string{"remote-linux"}, []string{"claude-code"})
	injection := s.prepareMCPInjection(ctx, projectID, projectPath, "claude-code", s.localRunnerID(), "run-1")
	if injection.JSON != "" {
		t.Fatalf("expected no injection for non-matching environment, got %s", injection.JSON)
	}
	// 仅适用于 codex → P0 的 Claude 路径不注入。
	insertTestMCPServer(t, s, "codex-only", mcpScopeGlobal, "", mcpTransportStdio, "npx", nil, []string{"windows"}, []string{"codex"})
	injection = s.prepareMCPInjection(ctx, projectID, projectPath, "claude-code", s.localRunnerID(), "run-2")
	if injection.JSON != "" {
		t.Fatalf("expected no injection for non-matching agent, got %s", injection.JSON)
	}
}

func TestMCPProjectScopeOverridesGlobal(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	projectID := "proj-override"
	if _, err := s.db.ExecContext(ctx, `insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values (?,?,?,?,?,?,?)`,
		projectID, "P", t.TempDir(), s.localRunnerID(), "main", true, time.Now().UTC()); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	insertTestMCPServer(t, s, "shared", mcpScopeGlobal, "", mcpTransportStdio, "global-cmd", nil, []string{"windows"}, []string{"claude-code"})
	insertTestMCPServer(t, s, "shared", mcpScopeProject, projectID, mcpTransportStdio, "project-cmd", nil, []string{"windows"}, []string{"claude-code"})
	selected, err := s.selectMCPServers(ctx, projectID, "claude-code", agentTargetEnvWindows)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(selected) != 1 || selected[0].Scope != mcpScopeProject || selected[0].Command != "project-cmd" {
		t.Fatalf("expected project scope to win, got %+v", selected)
	}
}

func TestMCPApprovalHandlerAcceptsMCPTools(t *testing.T) {
	s := newTestServer(t)
	// 非 Bash 且非 MCP 的工具名仍应被拒绝（校验没有被放得过宽）。
	if isApprovableToolName("Read") {
		t.Fatal("Read must stay rejected by the approval channel")
	}
	// 自动放行命中时不应产生 pending：直接验证判定逻辑本身。
	s.setMCPAutoApprove("conv-1", []string{"mcp__github__*"})
	if !mcpAutoApproveAllows(s.runMCPAutoApprove("conv-1"), "mcp__github__create_issue", nil) {
		t.Fatal("expected auto approve to allow the MCP tool")
	}
	s.clearMCPAutoApprove("conv-1")
	if len(s.runMCPAutoApprove("conv-1")) != 0 {
		t.Fatal("expected auto approve to be cleared")
	}
}

// 回归：Codex 的密钥环境变量名必须按 server 区分。
//
// 早期实现用 header 名（如 Authorization）派生变量名，两个 server 配同名 header 时会得到
// 同一个变量名；buildCodexInjection 会按变量名去重，于是后一个 server 的密钥被丢弃、它的
// bearer_token_env_var 指向第一个 server 的值 —— 凭据串到别的 server 上。
func TestCodexHeaderSecretEnvNamesAreServerScoped(t *testing.T) {
	s := &Server{}
	ctx := context.Background()
	argsA, envA, ok := s.codexHeaderArgs(ctx, "mcp_servers.alpha", map[string]string{"Authorization": "Bearer tokA"}, agentTargetEnvWindows, "")
	if !ok || len(envA) != 1 {
		t.Fatalf("alpha auth env = %#v ok=%v", envA, ok)
	}
	argsB, envB, ok := s.codexHeaderArgs(ctx, "mcp_servers.beta", map[string]string{"Authorization": "Bearer tokB"}, agentTargetEnvWindows, "")
	if !ok || len(envB) != 1 {
		t.Fatalf("beta auth env = %#v ok=%v", envB, ok)
	}
	nameA, _, _ := strings.Cut(envA[0], "=")
	nameB, _, _ := strings.Cut(envB[0], "=")
	if nameA == nameB {
		t.Fatalf("两个 server 的 Authorization 变量名相同（%q），按名去重会丢掉后者", nameA)
	}
	if envA[0] != nameA+"=tokA" || envB[0] != nameB+"=tokB" {
		t.Fatalf("密钥未各归其位：%q / %q", envA[0], envB[0])
	}
	if !strings.Contains(strings.Join(argsA, " "), nameA) || !strings.Contains(strings.Join(argsB, " "), nameB) {
		t.Fatalf("bearer_token_env_var 未指向各自变量：%v / %v", argsA, argsB)
	}

	// 非 Authorization 的密钥型 header 同样要按 server 区分。
	_, genericA, _ := s.codexHeaderArgs(ctx, "mcp_servers.alpha", map[string]string{"X-Api-Key": "keyA"}, agentTargetEnvWindows, "")
	_, genericB, _ := s.codexHeaderArgs(ctx, "mcp_servers.beta", map[string]string{"X-Api-Key": "keyB"}, agentTargetEnvWindows, "")
	if len(genericA) != 1 || len(genericB) != 1 {
		t.Fatalf("generic header env = %#v / %#v", genericA, genericB)
	}
	if genericA[0] == genericB[0] {
		t.Fatalf("两个 server 的 X-Api-Key 变量名冲突：%q", genericA[0])
	}
}
