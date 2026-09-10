package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

// ---------------------------------------------------------------------------
// 测试脚手架
// ---------------------------------------------------------------------------

func insertTestProject(t *testing.T, s *Server, projectID, projectPath string) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(),
		`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values (?,?,?,?,?,?,?)`,
		projectID, projectID, projectPath, s.localRunnerID(), "main", true, time.Now().UTC()); err != nil {
		t.Fatalf("insert project: %v", err)
	}
}

func insertTestMCPServerURL(t *testing.T, s *Server, name, scope, projectID, transport, rawURL string, agents []string) string {
	t.Helper()
	ctx := context.Background()
	environmentsJSON, _ := marshalStringSlice([]string{string(agentTargetEnvWindows)})
	agentsJSON, _ := marshalStringSlice(agents)
	id := "mcp_url_" + name
	if _, err := s.db.ExecContext(ctx, `insert into mcp_servers (`+mcpServerColumns+`) values (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, name, name, "", transport, "", "[]", "{}", "",
		rawURL, "{}",
		scope, projectID, environmentsJSON, agentsJSON,
		true, "[]", 20, 60, "manual", time.Now().UTC(), time.Now().UTC()); err != nil {
		t.Fatalf("insert mcp server with url: %v", err)
	}
	return id
}

func withURLParam(request *http.Request, key, value string) *http.Request {
	routeCtx := chi.NewRouteContext()
	routeCtx.URLParams.Add(key, value)
	return request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeCtx))
}

// ---------------------------------------------------------------------------
// Codex 项目级 TOML 子集
// ---------------------------------------------------------------------------

func TestParseCodexProjectMCPConfig(t *testing.T) {
	raw := `
# 项目级 Codex 配置
model = "gpt-5"

[mcp_servers.context7]
command = "npx"          # 官方示例
args = [
  "-y",
  "@upstash/context7-mcp",   # 行尾注释
]
env_vars = ["LOCAL_TOKEN"]

[mcp_servers.context7.env]
MY_ENV_VAR = "MY_ENV_VALUE"
SHARED = 'literal\value'

[mcp_servers.figma]
url = "https://mcp.figma.com/mcp"
bearer_token_env_var = "FIGMA_OAUTH_TOKEN"
http_headers = { "X-Figma-Region" = "us-east-1", "X-Trace" = "1" }

[mcp_servers."my.server"]
command = "uvx"
args = ["mcp-server-fetch"]
enabled = false

[mcp_servers.inline]
command = "node"
env = { "KEY" = "value", "OTHER" = "x" }

[mcp_servers.empty]
enabled = true

[mcp_servers.context7.tools.search]
approval_mode = "approve"
`
	servers, warnings := parseCodexProjectMCPConfig(raw)

	if len(servers) != 4 {
		t.Fatalf("expected 4 servers, got %d (%v)", len(servers), sortedKeysOfServers(servers))
	}
	context7, ok := servers["context7"]
	if !ok {
		t.Fatal("context7 should be parsed")
	}
	if context7.Command != "npx" {
		t.Fatalf("context7 command = %q", context7.Command)
	}
	if len(context7.Args) != 2 || context7.Args[1] != "@upstash/context7-mcp" {
		t.Fatalf("multiline args with inline comment parsed wrong: %#v", context7.Args)
	}
	if context7.Env["MY_ENV_VAR"] != "MY_ENV_VALUE" {
		t.Fatalf("env sub-table not applied: %#v", context7.Env)
	}
	// 字面字符串不做转义处理。
	if context7.Env["SHARED"] != `literal\value` {
		t.Fatalf("literal string decoded wrong: %q", context7.Env["SHARED"])
	}
	if !hasWarning(warnings["context7"], "env_vars") {
		t.Fatalf("expected env_vars warning, got %v", warnings["context7"])
	}

	figma := servers["figma"]
	if figma.URL != "https://mcp.figma.com/mcp" {
		t.Fatalf("figma url = %q", figma.URL)
	}
	if figma.Headers["X-Figma-Region"] != "us-east-1" || len(figma.Headers) != 2 {
		t.Fatalf("inline http_headers parsed wrong: %#v", figma.Headers)
	}
	if !hasWarning(warnings["figma"], "FIGMA_OAUTH_TOKEN") {
		t.Fatalf("expected bearer_token_env_var warning, got %v", warnings["figma"])
	}

	// 带引号的表名。
	if servers["my.server"].Command != "uvx" {
		t.Fatalf("quoted section name not handled: %#v", servers["my.server"])
	}
	if !hasWarning(warnings["my.server"], "enabled = false") {
		t.Fatalf("expected disabled warning, got %v", warnings["my.server"])
	}

	if servers["inline"].Env["OTHER"] != "x" {
		t.Fatalf("inline env table parsed wrong: %#v", servers["inline"].Env)
	}
	// 没有 command/url 的表（含仅子表）应被丢弃。
	if _, exists := servers["empty"]; exists {
		t.Fatal("table without command/url should be dropped")
	}
	// 顶层非 mcp_servers 的表不应污染结果。
	if len(servers) != 4 {
		t.Fatalf("unexpected server count %d", len(servers))
	}
}

func hasWarning(list []string, fragment string) bool {
	for _, item := range list {
		if strings.Contains(item, fragment) {
			return true
		}
	}
	return false
}

func TestTOMLValueHelpers(t *testing.T) {
	if got := strings.TrimSpace(stripTOMLComment(`command = "npx" # comment`)); got != `command = "npx"` {
		t.Fatalf("strip comment = %q", got)
	}
	if got := stripTOMLComment(`x = "a # b"`); got != `x = "a # b"` {
		t.Fatalf("hash inside string must survive: %q", got)
	}
	key, value, ok := splitTOMLAssignment(`args = ["a", "b"]`)
	if !ok || key != "args" || value != `["a", "b"]` {
		t.Fatalf("assignment split wrong: %q %q %v", key, value, ok)
	}
	if !tomlValueBalanced(`["a", "b"]`) {
		t.Fatal("single-line array should be balanced")
	}
	if tomlValueBalanced("[\n \"a\",") {
		t.Fatal("unterminated array should be unbalanced")
	}
	if got, ok := parseTOMLString(`"a\tb\"c"`); !ok || got != "a\tb\"c" {
		t.Fatalf("basic string escapes wrong: %q %v", got, ok)
	}
	if _, ok := parseTOMLString(`not-a-string`); ok {
		t.Fatal("bare token must not parse as string")
	}
}

// ---------------------------------------------------------------------------
// 自动放行白名单
// ---------------------------------------------------------------------------

func TestNormalizeMCPAutoApprovePatterns(t *testing.T) {
	valid, err := normalizeMCPAutoApprovePatterns([]string{" mcp__github__* ", "mcp__github__list_repos", "Bash", "Bash(git *)", "mcp__github__*"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(valid) != 4 {
		t.Fatalf("expected dedupe to keep 4 patterns, got %v", valid)
	}
	for _, pattern := range []string{"mcp__*", "*", "mcp__github", "Read", ""} {
		if pattern == "" {
			continue
		}
		if _, err := normalizeMCPAutoApprovePatterns([]string{pattern}); err == nil {
			t.Fatalf("pattern %q should be rejected", pattern)
		}
	}
}

// ---------------------------------------------------------------------------
// 审计
// ---------------------------------------------------------------------------

func TestSplitMCPToolName(t *testing.T) {
	server, tool := splitMCPToolName("mcp__github__create_issue")
	if server != "github" || tool != "create_issue" {
		t.Fatalf("split = %q %q", server, tool)
	}
	server, tool = splitMCPToolName("mcp__onlyserver")
	if server != "onlyserver" || tool != "" {
		t.Fatalf("fallback split = %q %q", server, tool)
	}
}

func TestSummarizeMCPToolInputRedactsSecrets(t *testing.T) {
	raw := json.RawMessage(`{"path":"/tmp/x","GITHUB_TOKEN":"ghp_secret","nested":"ok"}`)
	summary := summarizeMCPToolInput(raw)
	if strings.Contains(summary, "ghp_secret") {
		t.Fatalf("secret value leaked into audit summary: %s", summary)
	}
	if !strings.Contains(summary, "/tmp/x") || !strings.Contains(summary, `"***"`) {
		t.Fatalf("unexpected summary: %s", summary)
	}
	long := summarizeMCPToolInput(json.RawMessage(`"` + strings.Repeat("a", 2000) + `"`))
	if len([]rune(long)) > mcpAuditArgsLimit+1 {
		t.Fatalf("summary not truncated: %d", len([]rune(long)))
	}
}

func TestMCPToolResultStatus(t *testing.T) {
	status, _ := mcpToolResultStatus(json.RawMessage(`{"is_error":true,"content":"boom"}`))
	if status != "error" {
		t.Fatalf("is_error should map to error, got %q", status)
	}
	status, detail := mcpToolResultStatus(json.RawMessage(`{"exit_code":2}`))
	if status != "error" || !strings.Contains(detail, "2") {
		t.Fatalf("nonzero exit code should map to error, got %q %q", status, detail)
	}
	if status, _ := mcpToolResultStatus(json.RawMessage(`{"content":"fine"}`)); status != "ok" {
		t.Fatalf("plain response should map to ok, got %q", status)
	}
	if status, _ := mcpToolResultStatus(nil); status != "ok" {
		t.Fatalf("empty response should map to ok, got %q", status)
	}
}

func TestMCPCallAuditRoundTrip(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	projectID := "proj-audit"
	insertTestProject(t, s, projectID, t.TempDir())
	conversationID := "conv-audit"
	if _, err := s.db.ExecContext(ctx, `insert into conversations (id,project_id,claude_session_id,status,created_at) values (?,?,?,?,?)`,
		conversationID, projectID, "sess-audit", "idle", time.Now().UTC()); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}

	s.recordMCPCallStart(ctx, conversationID, "run-1", "toolu_1", "mcp__github__create_issue", "allow", json.RawMessage(`{"title":"hi","GITHUB_TOKEN":"ghp_x"}`))
	s.recordMCPCallFinish(ctx, conversationID, "run-1", "toolu_1", "mcp__github__create_issue", "ok", "")
	// 非 MCP 工具不入库。
	s.recordMCPCallStart(ctx, conversationID, "run-1", "toolu_bash", "Bash", "allow", json.RawMessage(`{"command":"ls"}`))

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/mcp/audit?projectId="+projectID, nil)
	s.listMCPCallAudit(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("audit list status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response mcpAuditResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode audit response: %v", err)
	}
	if response.Total != 1 || len(response.Entries) != 1 {
		t.Fatalf("expected exactly one MCP audit row, got %+v", response)
	}
	entry := response.Entries[0]
	if entry.ServerName != "github" || entry.ToolName != "create_issue" {
		t.Fatalf("unexpected audit entry: %+v", entry)
	}
	if entry.Decision != "allow" || entry.Status != "ok" {
		t.Fatalf("decision/status not recorded: %+v", entry)
	}
	if strings.Contains(entry.ArgsPreview, "ghp_x") {
		t.Fatalf("audit row leaked a secret: %s", entry.ArgsPreview)
	}
	if entry.ProjectID != projectID {
		t.Fatalf("project filter join failed: %+v", entry)
	}

	// 清空后应为空。
	clearRecorder := httptest.NewRecorder()
	s.clearMCPCallAudit(clearRecorder, httptest.NewRequest(http.MethodDelete, "/api/mcp/audit", nil))
	if clearRecorder.Code != http.StatusNoContent {
		t.Fatalf("clear audit status = %d", clearRecorder.Code)
	}
	var remaining int
	if err := s.db.QueryRowContext(ctx, `select count(*) from mcp_call_audit`).Scan(&remaining); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("expected audit cleared, %d rows left", remaining)
	}
}

// ---------------------------------------------------------------------------
// 项目设置与注入状态
// ---------------------------------------------------------------------------

func TestProjectMCPAllowMcpJsonControlsStrictMode(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	projectID := "proj-allow"
	projectPath := t.TempDir()
	insertTestProject(t, s, projectID, projectPath)
	insertTestMCPServer(t, s, "fs", mcpScopeGlobal, "", mcpTransportStdio, "npx", nil,
		[]string{string(agentTargetEnvWindows)}, []string{"claude-code"})

	injection := s.prepareMCPInjection(ctx, projectID, projectPath, "claude-code", s.localRunnerID(), "run-1")
	defer injection.done()
	if !injection.Strict {
		t.Fatal("strict mode must be on by default")
	}

	if err := s.setProjectMCPAllowMcpJson(ctx, projectID, true); err != nil {
		t.Fatalf("allow mcp.json: %v", err)
	}
	injection2 := s.prepareMCPInjection(ctx, projectID, projectPath, "claude-code", s.localRunnerID(), "run-2")
	defer injection2.done()
	if injection2.Strict {
		t.Fatal("strict mode should be off after allowing project .mcp.json")
	}

	// 注入状态快照应反映最近一次注入。
	recorder := httptest.NewRecorder()
	request := withURLParam(httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/mcp/status", nil), "projectID", projectID)
	s.getProjectMCPStatus(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status endpoint = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var status projectMCPStatus
	if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.ProjectID != projectID || status.ServerCount != 1 || status.StrictMode {
		t.Fatalf("unexpected injection status: %+v", status)
	}
	if status.Servers[0].Name != "fs" {
		t.Fatalf("injection status should list the injected server: %+v", status.Servers)
	}
	if status.RunKey != "run-2" {
		t.Fatalf("expected the latest run key, got %q", status.RunKey)
	}
}

func TestProjectMcpJSONServerNames(t *testing.T) {
	dir := t.TempDir()
	if names := projectMcpJSONServerNames(dir); len(names) != 0 {
		t.Fatalf("missing file should yield no names, got %v", names)
	}
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(`{"mcpServers":{"zeta":{},"alpha":{}}}`), 0o600); err != nil {
		t.Fatalf("write .mcp.json: %v", err)
	}
	names := projectMcpJSONServerNames(dir)
	if len(names) != 2 || names[0] != "alpha" || names[1] != "zeta" {
		t.Fatalf("unexpected names: %v", names)
	}
}

// ---------------------------------------------------------------------------
// 审批 hook 设置
// ---------------------------------------------------------------------------

func TestMCPApprovalHooksSettingsJSONIncludesPostToolUse(t *testing.T) {
	raw := mcpApprovalHooksSettingsJSON(`sh -c "curl -s -X POST http://127.0.0.1:1/a"`, nil)
	var decoded struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("hook settings is not valid JSON: %v (%s)", err, raw)
	}
	if len(decoded.Hooks["PreToolUse"]) != 1 || decoded.Hooks["PreToolUse"][0].Matcher != mcpToolHookMatcher {
		t.Fatalf("PreToolUse matcher wrong: %s", raw)
	}
	if len(decoded.Hooks["PostToolUse"]) != 1 || decoded.Hooks["PostToolUse"][0].Matcher != mcpAuditHookMatcher {
		t.Fatalf("PostToolUse matcher wrong: %s", raw)
	}
	// hook 命令里的引号必须被正确转义（这是弃用字符串拼接的原因）。
	if decoded.Hooks["PostToolUse"][0].Hooks[0].Command != `sh -c "curl -s -X POST http://127.0.0.1:1/a"` {
		t.Fatalf("hook command round-trip failed: %q", decoded.Hooks["PostToolUse"][0].Hooks[0].Command)
	}
}

// ---------------------------------------------------------------------------
// requiresUserInteraction 与过度授权提示（docs/34 §8.5 / §9 / §10.4）
// ---------------------------------------------------------------------------

func hasFlagCode(flags []mcpToolFlag, code string) bool {
	for _, flag := range flags {
		if flag.Code == code {
			return true
		}
	}
	return false
}

func TestMCPToolRequiresInteraction(t *testing.T) {
	cases := []struct {
		name string
		meta string
		want bool
	}{
		{"bool true", `{"anthropic/requiresUserInteraction":true}`, true},
		{"bool false", `{"anthropic/requiresUserInteraction":false}`, false},
		{"string true", `{"anthropic/requiresUserInteraction":"true"}`, true},
		{"snake case alias", `{"anthropic/requires_user_interaction":true}`, true},
		{"unrelated meta", `{"other":"x"}`, false},
		{"empty meta", ``, false},
		{"invalid json", `{`, false},
	}
	for _, tc := range cases {
		if got := mcpToolRequiresInteraction(json.RawMessage(tc.meta)); got != tc.want {
			t.Fatalf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestBuildProbeOutcomeCapturesRequiresInteraction(t *testing.T) {
	initMsg := jsonrpcMessage{Result: json.RawMessage(`{"protocolVersion":"2024-11-05"}`)}
	toolsMsg := jsonrpcMessage{Result: json.RawMessage(`{"tools":[
		{"name":"interactive_login","_meta":{"anthropic/requiresUserInteraction":true}},
		{"name":"plain_read","description":"Read a file from the project."}
	]}`)}
	out, err := buildProbeOutcome(initMsg, toolsMsg)
	if err != nil {
		t.Fatalf("buildProbeOutcome: %v", err)
	}
	if len(out.Tools) != 2 {
		t.Fatalf("tools = %#v", out.Tools)
	}
	if !out.Tools[0].RequiresInteraction {
		t.Fatal("_meta 标记未被解析")
	}
	if out.Tools[1].RequiresInteraction {
		t.Fatal("未标记的工具不应被视为需要交互")
	}
	if !hasFlagCode(out.Tools[0].Flags, "requires_interaction") {
		t.Fatalf("缺少 requires_interaction 标注：%#v", out.Tools[0].Flags)
	}
	if out.Tools[0].Flags[0].Note == "" {
		t.Fatal("requires_interaction 必须带可操作说明")
	}
	// 普通工具不应被误标。
	if len(out.Tools[1].Flags) != 0 {
		t.Fatalf("普通工具出现误报：%#v", out.Tools[1].Flags)
	}
}

func TestFlagMCPToolMetadataDetectsBroadScope(t *testing.T) {
	flagged := mcpToolInfo{Name: "grant", Description: "Requires administrator access to the repository."}
	if !hasFlagCode(flagMCPToolMetadata(flagged), "broad_scope") {
		t.Fatalf("未识别过度授权描述：%#v", flagMCPToolMetadata(flagged))
	}
	if !hasFlagCode(flagMCPToolMetadata(mcpToolInfo{Name: "scope", Description: "Uses the *:* scope."}), "broad_scope") {
		t.Fatal("未识别 *:* 特征")
	}
	benign := mcpToolInfo{Name: "read_file", Description: "Read a file from the project directory."}
	if flags := flagMCPToolMetadata(benign); len(flags) != 0 {
		t.Fatalf("普通描述不应告警：%#v", flags)
	}
}

// ---------------------------------------------------------------------------
// 审批参数上限（docs/34 §9）
// ---------------------------------------------------------------------------

func TestTruncateApprovalToolInputPreservesCommand(t *testing.T) {
	long := strings.Repeat("x", 5000)
	raw := json.RawMessage(`{"command":"` + long + `","description":"` + long + `"}`)
	out := truncateApprovalToolInput(raw)
	var decoded map[string]string
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("截断结果不是合法 JSON：%v (%s)", err, out)
	}
	// command 必须逐字保留：前端靠它把审批横幅锚定到工具卡片。
	if decoded["command"] != long {
		t.Fatalf("command 被截断，锚定会失效：len=%d", len(decoded["command"]))
	}
	if len(decoded["description"]) >= len(long) {
		t.Fatalf("超长值未被截断：len=%d", len(decoded["description"]))
	}
	if !strings.Contains(decoded["description"], "已截断") {
		t.Fatalf("截断说明缺失：%q", decoded["description"])
	}
}

func TestTruncateApprovalToolInputPassesSmallInputThrough(t *testing.T) {
	raw := json.RawMessage(`{"path":"/tmp/a.txt"}`)
	if got := truncateApprovalToolInput(raw); string(got) != string(raw) {
		t.Fatalf("小输入应原样透传：%s", got)
	}
}

// 回归：截断判定与「已截断」说明必须同为 rune 口径。早期实现用字节长度判定是否截断、
// 又交给按 rune 截断的 helper，中文内容会落在「字节超限但 rune 未超限」的区间，实测未截断
// 却仍被追加「（已截断）」说明。原测试只用 ASCII，覆盖不到该区间。
func TestTruncateApprovalToolInputCJKValueNotFalselyAnnotated(t *testing.T) {
	// 700 个汉字 = 2100 字节（> 1024 字节上限），但只有 700 个 rune（< 1024 rune 上限）。
	cjk := strings.Repeat("中", 700)
	raw := json.RawMessage(`{"description":"` + cjk + `"}`)
	out := truncateApprovalToolInput(raw)
	var decoded map[string]string
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("截断结果不是合法 JSON：%v (%s)", err, out)
	}
	if decoded["description"] != cjk {
		t.Fatalf("rune 未超限的值不应被截断：len=%d", len([]rune(decoded["description"])))
	}
	if strings.Contains(decoded["description"], "已截断") {
		t.Fatalf("未被截断的值不应带「已截断」说明：%q", decoded["description"])
	}
}

// 回归：rune 确实超限时必须截断，且说明里的原始字符数按 rune 计。
func TestTruncateApprovalToolInputCJKValueTruncatedByRune(t *testing.T) {
	cjk := strings.Repeat("中", 2000) // 2000 rune > 1024
	raw := json.RawMessage(`{"description":"` + cjk + `"}`)
	out := truncateApprovalToolInput(raw)
	var decoded map[string]string
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("截断结果不是合法 JSON：%v (%s)", err, out)
	}
	if !strings.Contains(decoded["description"], "已截断") {
		t.Fatalf("超限值缺少截断说明：%q", decoded["description"])
	}
	if !strings.Contains(decoded["description"], "原始 2000 字符") {
		t.Fatalf("截断说明里的原始字符数应按 rune 计：%q", decoded["description"])
	}
	if got := strings.Count(decoded["description"], "中"); got != 1024 {
		t.Fatalf("保留的 rune 数 = %d want 1024", got)
	}
}

func TestTruncateApprovalToolInputMarkersOversizedPayload(t *testing.T) {
	parts := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		parts = append(parts, fmt.Sprintf(`"k%d":"%s"`, i, strings.Repeat("y", 4000)))
	}
	raw := json.RawMessage("{" + strings.Join(parts, ",") + "}")
	out := truncateApprovalToolInput(raw)
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("降级标记不是合法 JSON：%v (%s)", err, out)
	}
	if decoded["_truncated"] != true {
		t.Fatalf("整体超限时未降级：%s", out)
	}
	if len(out) > approvalToolInputTotalLimit {
		t.Fatalf("降级后仍然过大：%d", len(out))
	}
}

// ---------------------------------------------------------------------------
// permissions.allow 兜底（docs/34 §8.4）
// ---------------------------------------------------------------------------

func TestMCPPermissionsAllowFiltersUnanchoredPatterns(t *testing.T) {
	got := mcpPermissionsAllow([]string{"mcp__github__*", "mcp__*", "Bash(npm run test)", "mcp__github__create_issue", "  mcp__fs__read  ", "garbage"})
	want := []string{"mcp__github__*", "Bash(npm run test)", "mcp__github__create_issue", "mcp__fs__read"}
	if len(got) != len(want) {
		t.Fatalf("allow 模式 = %#v want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("allow 模式 = %#v want %#v", got, want)
		}
	}
}

func TestMCPApprovalHooksSettingsJSONWritesPermissionsAllow(t *testing.T) {
	raw := mcpApprovalHooksSettingsJSON("hook-cmd", []string{"mcp__github__*", "Bash(npm run test)", "mcp__*"})
	var decoded struct {
		Permissions struct {
			Allow []string `json:"allow"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("settings 不是合法 JSON：%v (%s)", err, raw)
	}
	// 只保留合法规则：mcp__* 无锚点写法必须被剔除。
	if len(decoded.Permissions.Allow) != 2 {
		t.Fatalf("allow = %#v（应剔除无锚点写法）", decoded.Permissions.Allow)
	}
	if decoded.Permissions.Allow[0] != "mcp__github__*" || decoded.Permissions.Allow[1] != "Bash(npm run test)" {
		t.Fatalf("allow = %#v", decoded.Permissions.Allow)
	}
	// 没有白名单时不应凭空造出 permissions 段。
	if plain := mcpApprovalHooksSettingsJSON("hook-cmd", nil); strings.Contains(plain, "permissions") {
		t.Fatalf("空白名单不应写出 permissions：%s", plain)
	}
	// 全是非法模式时同样不应写出 permissions 段。
	if only := mcpApprovalHooksSettingsJSON("hook-cmd", []string{"mcp__*"}); strings.Contains(only, "permissions") {
		t.Fatalf("非法模式不应写出 permissions：%s", only)
	}
}

// ---------------------------------------------------------------------------
// 按环境预览（docs/34 §10.3）
// ---------------------------------------------------------------------------

func TestPreviewMCPServerResolvesProjectDirPerEnvironment(t *testing.T) {
	server := newTestServer(t)
	insertTestProject(t, server, "proj-preview", `C:/dev/proj`)

	body := `{"transport":"stdio","command":"npx","args":["--root","${PROJECT_DIR}"],"env":{"ROOT":"${PROJECT_DIR}"},"environment":"wsl","projectId":"proj-preview"}`
	request := httptest.NewRequest(http.MethodPost, "/api/mcp/preview", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	server.previewMCPServer(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var result mcpPreviewResult
	if err := json.NewDecoder(recorder.Body).Decode(&result); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if result.Command != "npx" {
		t.Fatalf("command = %q", result.Command)
	}
	// WSL 下 Windows 盘符路径应解析为 /mnt/<盘符>/ 形态。
	if len(result.Args) != 2 || result.Args[1] != "/mnt/c/dev/proj" {
		t.Fatalf("args = %#v", result.Args)
	}
	if result.Env["ROOT"] != "/mnt/c/dev/proj" {
		t.Fatalf("env ROOT = %q", result.Env["ROOT"])
	}
}

func TestPreviewMCPServerNotesUnresolvedProjectDir(t *testing.T) {
	server := newTestServer(t)
	body := `{"transport":"stdio","command":"npx","args":["${PROJECT_DIR}","${HOME}"],"environment":"windows"}`
	request := httptest.NewRequest(http.MethodPost, "/api/mcp/preview", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	server.previewMCPServer(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var result mcpPreviewResult
	if err := json.NewDecoder(recorder.Body).Decode(&result); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	// 未选项目时占位符原样保留，并且必须给出说明。
	if result.Args[0] != "${PROJECT_DIR}" {
		t.Fatalf("未选项目时应保留占位符：%#v", result.Args)
	}
	joined := strings.Join(result.Notes, " | ")
	if !strings.Contains(joined, "未选择项目") {
		t.Fatalf("notes = %#v", result.Notes)
	}
	if !strings.Contains(joined, "${HOME}") {
		t.Fatalf("应说明其余占位符交给 CLI 展开：%#v", result.Notes)
	}
}

func TestPreviewMCPServerRejectsUnknownEnvironment(t *testing.T) {
	server := newTestServer(t)
	body := `{"transport":"stdio","command":"npx","environment":"plan9"}`
	request := httptest.NewRequest(http.MethodPost, "/api/mcp/preview", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	server.previewMCPServer(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
