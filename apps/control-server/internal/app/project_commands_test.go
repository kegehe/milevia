package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 常用命令目录（docs/37）的后端回归。这里锁住的都是"看起来能跑、其实会静默错"的性质：
//   - 目录必须来自 CLI 自己的 init 事件（它才是唯一完整来源）；
//   - 拿不到目录时必须降级成静态候选并如实说明，且**不得**声称权威——否则前端会把
//     有效命令误判为失效；
//   - Codex 会话不得拿到斜杠命令的运行入口（它的 exec 不解析斜杠）；
//   - 探针不是每条请求都拉起，且真实运行的观测优先于探针。

const testInitPayload = `{"type":"system","subtype":"init","model":"claude-opus-5",
 "slash_commands":["mycmd","nested:deep","deep-research","clear","compact","context","model","doctor"],
 "terminal_slash_commands":["doctor"],
 "skills":["deep-research"],
 "claude_code_version":"2.1.266"}`

func TestParseClaudeCommandCatalog(t *testing.T) {
	catalog, ok := parseClaudeCommandCatalog([]byte(testInitPayload))
	if !ok {
		t.Fatal("init event with slash_commands must parse")
	}
	if catalog.version != "2.1.266" {
		t.Fatalf("version = %q", catalog.version)
	}
	if len(catalog.names) != 8 {
		t.Fatalf("names = %v", catalog.names)
	}
	if !catalog.skills["deep-research"] || catalog.skills["compact"] {
		t.Fatalf("skills set wrong: %v", catalog.skills)
	}
	if !catalog.terminal["doctor"] || catalog.terminal["compact"] {
		t.Fatalf("terminal set wrong: %v", catalog.terminal)
	}
}

func TestParseClaudeCommandCatalogRejectsOtherEvents(t *testing.T) {
	cases := map[string]string{
		"assistant":          `{"type":"assistant","message":{"content":[]}}`,
		"no slash commands":  `{"type":"system","subtype":"init","model":"x"}`,
		"empty slash list":   `{"type":"system","subtype":"init","slash_commands":[]}`,
		"other system event": `{"type":"system","subtype":"thinking_tokens","slash_commands":["a"]}`,
		"not json":           `hello`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			if _, ok := parseClaudeCommandCatalog([]byte(payload)); ok {
				t.Fatalf("%s must not be treated as a command catalog", name)
			}
		})
	}
}

func TestBuildCommandOptionsMarksSourcesAndDropsStaleCustomCommands(t *testing.T) {
	catalog, _ := parseClaudeCommandCatalog([]byte(testInitPayload))
	scanned := []scannedCommand{
		{name: "mycmd", description: "统计 TODO", argumentHint: "[路径]", source: skillSourceProject},
		// CLI 目录里没有它（例如 frontmatter 关掉了调用），权威目录下必须丢弃。
		{name: "ghost", description: "不该出现", source: skillSourceProject},
	}
	byName := map[string]AgentCommandOption{}
	for _, option := range buildCommandOptions(catalog, scanned, true) {
		byName[option.Name] = option
	}
	if _, ok := byName["ghost"]; ok {
		t.Fatal("权威目录下不得出现 CLI 未列出的命令")
	}
	project := byName["mycmd"]
	if project.Group != skillSourceProject || project.Description != "统计 TODO" || project.ArgumentHint != "[路径]" {
		t.Fatalf("项目自定义命令未按扫描结果标注：%+v", project)
	}
	skill := byName["deep-research"]
	if skill.Group != "skill" || skill.Label == "" {
		t.Fatalf("技能命令应有 skill 分组与中文名：%+v", skill)
	}
	compact := byName["compact"]
	if compact.Group != "builtin" || compact.Label == "" || !compact.Recommended {
		t.Fatalf("内置命令应有中文名并进入建议组：%+v", compact)
	}
	if !byName["doctor"].TerminalOnly {
		t.Fatalf("terminal_slash_commands 里的命令必须带 terminalOnly：%+v", byName["doctor"])
	}
	if byName["context"].TerminalOnly {
		t.Fatal("非终端命令不得被标成 terminalOnly")
	}
}

func TestBuildCommandOptionsFallsBackToStaticCandidates(t *testing.T) {
	options := buildCommandOptions(claudeCommandCatalog{}, []scannedCommand{{name: "only-mine", source: skillSourceUser}}, false)
	byName := map[string]AgentCommandOption{}
	for _, option := range options {
		byName[option.Name] = option
	}
	// 静态兜底必须同时包含内置命令、技能命令与扫描到的自定义命令，否则用户在冷启动时
	// 会看到一个几乎空的选择器。
	for _, name := range []string{"compact", "code-review", "only-mine"} {
		if _, ok := byName[name]; !ok {
			t.Fatalf("静态兜底缺少 %s", name)
		}
	}
	if byName["only-mine"].Group != skillSourceUser {
		t.Fatalf("自定义命令来源分组错误：%+v", byName["only-mine"])
	}
}

func TestScanLocalCommandsReadsFrontmatterAndNamespaces(t *testing.T) {
	projectPath := t.TempDir()
	commandsDir := filepath.Join(projectPath, ".claude", "commands")
	if err := os.MkdirAll(filepath.Join(commandsDir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCommandFileForTest(t, filepath.Join(commandsDir, "mycmd.md"), "---\ndescription: 统计 TODO\nargument-hint: \"[路径]\"\n---\n正文\n")
	writeCommandFileForTest(t, filepath.Join(commandsDir, "nested", "deep.md"), "---\ndescription: 嵌套命令\n---\n正文\n")
	writeCommandFileForTest(t, filepath.Join(commandsDir, "notes.txt"), "不是命令")
	// 没有 frontmatter 的命令文件也是合法命令，只是没有说明。
	writeCommandFileForTest(t, filepath.Join(commandsDir, "bare.md"), "直接是正文\n")

	scanned := map[string]scannedCommand{}
	for _, command := range scanLocalCommands(projectPath) {
		scanned[command.name] = command
	}
	// 只断言项目级这几条：用户级 ~/.claude/commands 属于本机环境，测试不该依赖它。
	mine, ok := scanned["mycmd"]
	if !ok {
		t.Fatalf("未扫到 mycmd：%v", scanned)
	}
	if mine.description != "统计 TODO" || mine.argumentHint != "[路径]" || mine.source != skillSourceProject {
		t.Fatalf("frontmatter 解析错误：%+v", mine)
	}
	if _, ok := scanned["nested:deep"]; !ok {
		t.Fatalf("子目录命令应带上 `:` 命名空间：%v", scanned)
	}
	if _, ok := scanned["bare"]; !ok {
		t.Fatalf("没有 frontmatter 的命令文件也应被扫到：%v", scanned)
	}
	if _, ok := scanned["notes"]; ok {
		t.Fatal("非 .md 文件不得当成命令")
	}
}

func TestSlashCommandName(t *testing.T) {
	cases := map[string]string{
		"/compact":        "compact",
		"  /model opus  ": "model",
		"pnpm test":       "",
		"请在当前项目中执行 `ls`":  "",
	}
	for input, want := range cases {
		if got := slashCommandName(input); got != want {
			t.Fatalf("slashCommandName(%q) = %q, want %q", input, got, want)
		}
	}
}

// stubCommandCatalogRunner 模拟"能探测目录的 Claude runner"。runnerFunc 已满足
// AgentRunner 的其余方法，这里只覆盖探测本身并记调用次数。
type stubCommandCatalogRunner struct {
	runnerFunc
	catalog claudeCommandCatalog
	err     error
	calls   int
}

func (r *stubCommandCatalogRunner) claudeCommandCatalog(context.Context, string) (claudeCommandCatalog, error) {
	r.calls++
	return r.catalog, r.err
}

func TestProjectCommandsUsesProbeThenCaches(t *testing.T) {
	server := newTestServer(t)
	projectID := createCommandTestProject(t, server, t.TempDir())
	runner := &stubCommandCatalogRunner{catalog: mustCommandCatalog(t)}
	server.runner = runner

	view := projectCommandsView(t, server, projectID, "")
	if !view.Authoritative || view.Source != "probe" {
		t.Fatalf("首次请求应来自探针：%+v", view)
	}
	if view.ClaudeVersion != "2.1.266" {
		t.Fatalf("应带出 CLI 版本：%+v", view)
	}
	if runner.calls != 1 {
		t.Fatalf("探针调用次数 = %d, want 1", runner.calls)
	}
	// 第二次请求命中缓存：目录只随 CLI 版本变化，不该每次打开选择器都拉起进程。
	projectCommandsView(t, server, projectID, "")
	if runner.calls != 1 {
		t.Fatalf("第二次请求又拉起了探针：calls = %d", runner.calls)
	}
	// refresh=1 忽略 TTL。
	projectCommandsView(t, server, projectID, "refresh=1")
	if runner.calls != 2 {
		t.Fatalf("refresh=1 应重新探测：calls = %d", runner.calls)
	}
}

func TestProjectCommandsProbeFailureFallsBackToStatic(t *testing.T) {
	server := newTestServer(t)
	projectID := createCommandTestProject(t, server, t.TempDir())
	server.runner = &stubCommandCatalogRunner{err: errors.New("claude not logged in")}

	view := projectCommandsView(t, server, projectID, "")
	if view.Authoritative {
		t.Fatal("探针失败时不得声称目录权威——否则前端会把有效命令误判为失效")
	}
	if view.Source != "static" || len(view.Commands) == 0 {
		t.Fatalf("应降级为静态候选：%+v", view)
	}
	if view.Note == "" {
		t.Fatal("降级必须如实说明原因")
	}
}

func TestProjectCommandsProbeDisabledDoesNotSpawn(t *testing.T) {
	server := newTestServer(t)
	projectID := createCommandTestProject(t, server, t.TempDir())
	runner := &stubCommandCatalogRunner{catalog: mustCommandCatalog(t)}
	server.runner = runner

	// 会话页加载时的徽标判定走 probe=0：只是打开一个项目，不该拉起 CLI。
	view := projectCommandsView(t, server, projectID, "probe=0")
	if runner.calls != 0 {
		t.Fatalf("probe=0 不得拉起探针：calls = %d", runner.calls)
	}
	if view.Authoritative {
		t.Fatal("未探测时目录不权威")
	}
}

func TestProjectCommandsForCodexHasNoSlashCommands(t *testing.T) {
	server := newTestServer(t)
	projectID := createCommandTestProject(t, server, t.TempDir())
	server.runner = &stubCommandCatalogRunner{catalog: mustCommandCatalog(t)}

	view := projectCommandsView(t, server, projectID, "agentId=codex")
	if len(view.Commands) != 0 {
		t.Fatalf("Codex 不应提供命令目录：%+v", view.Commands)
	}
	if !view.CustomAllowed {
		t.Fatal("Codex 仍必须保留自定义 shell 命令入口")
	}
	if view.Note == "" {
		t.Fatal("必须说明为什么没有目录")
	}
}

func TestObserveCommandCatalogFromRunWinsOverProbe(t *testing.T) {
	server := newTestServer(t)
	projectID := createCommandTestProject(t, server, t.TempDir())
	server.runner = &stubCommandCatalogRunner{catalog: mustCommandCatalog(t)}
	projectCommandsView(t, server, projectID, "")

	// 真实运行的观测比探针更新，必须覆盖缓存（否则新装的插件命令要等 TTL 才可见）。
	server.observeCommandCatalog(projectID, []byte(`{"type":"system","subtype":"init","slash_commands":["compact","brand-new"],"skills":[],"claude_code_version":"2.1.267"}`))

	view := projectCommandsView(t, server, projectID, "")
	if view.Source != "run" || view.ClaudeVersion != "2.1.267" {
		t.Fatalf("运行观测未生效：%+v", view)
	}
	found := false
	for _, command := range view.Commands {
		if command.Name == "brand-new" {
			found = true
		}
	}
	if !found {
		t.Fatalf("运行观测到的命令未出现：%+v", view.Commands)
	}
}

func TestObserveCommandCatalogIgnoresUnrelatedEvents(t *testing.T) {
	server := newTestServer(t)
	// 事件热路径上的采样必须廉价且精确：非 init 事件、或空 projectID 都不该写入缓存。
	server.observeCommandCatalog("project", []byte(`{"type":"assistant","message":{"content":[]}}`))
	server.observeCommandCatalog("", []byte(`{"type":"system","subtype":"init","slash_commands":["compact"]}`))
	if _, ok := server.cachedCommandCatalog(commandCatalogCacheKey("project", "claude-code"), 0); ok {
		t.Fatal("不该写入缓存")
	}
}

func TestProjectCommandsRefreshFailureKeepsLastKnownCatalog(t *testing.T) {
	server := newTestServer(t)
	projectID := createCommandTestProject(t, server, t.TempDir())
	runner := &stubCommandCatalogRunner{catalog: mustCommandCatalog(t)}
	server.runner = runner
	projectCommandsView(t, server, projectID, "")

	// 让缓存"过期"，再把探针弄失败：这一次刷新拿不到新目录，但手里那份是**真观测到的**，
	// 比退回内置候选准确得多，所以应当继续用它，并在说明里如实写明没能刷新。
	server.commandCatalogMu.Lock()
	server.commandCatalogEntry.observed = time.Now().Add(-2 * commandCatalogTTL)
	server.commandCatalogMu.Unlock()
	runner.err = errors.New("claude probe timed out")

	view := projectCommandsView(t, server, projectID, "refresh=1")
	if !view.Authoritative || len(view.Commands) == 0 {
		t.Fatalf("过期目录仍应可用：%+v", view)
	}
	if !strings.Contains(view.Note, "观测到的目录") {
		t.Fatalf("必须如实说明这是旧目录而非刷新成功：%q", view.Note)
	}
}

// TestRunShortcutRejectsSlashCommandForCodex 锁住 docs/37 §3.6 的唯一硬拦截：
// Codex 的 exec 不解析斜杠命令，`/compact` 会被当成普通提示词交给模型。
func TestRunShortcutRejectsSlashCommandForCodex(t *testing.T) {
	server := newTestServer(t)
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','demo',?,'wsl-local','main',1,?)`, t.TempDir(), now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := server.db.Exec(`insert into conversations (id,project_id,claude_session_id,agent_id,status,claude_initialized,is_current,created_at) values ('conversation','project','00000000-0000-4000-8000-000000000001','codex','idle',0,1,?)`, now); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	handler := server.routes()
	create := httptest.NewRecorder()
	handler.ServeHTTP(create, httptest.NewRequest(http.MethodPost, "/api/shortcuts", bytes.NewBufferString(`{"name":"压缩上下文","description":"","kind":"command_request","template":"/compact","scope":"local","defaultAction":"confirm","groupName":"常用命令","enabled":true}`)))
	if create.Code != http.StatusCreated {
		t.Fatalf("create shortcut status: %d body=%s", create.Code, create.Body.String())
	}
	var shortcut Shortcut
	if err := json.NewDecoder(create.Body).Decode(&shortcut); err != nil {
		t.Fatalf("decode shortcut: %v", err)
	}

	run := httptest.NewRecorder()
	handler.ServeHTTP(run, httptest.NewRequest(http.MethodPost, "/api/conversations/conversation/shortcuts/"+shortcut.ID+"/run", bytes.NewBufferString(`{"variables":{},"action":"confirm"}`)))
	if run.Code != http.StatusBadRequest {
		t.Fatalf("Codex 会话上的斜杠命令必须被拦下：status=%d body=%s", run.Code, run.Body.String())
	}
	if !strings.Contains(run.Body.String(), "Codex") {
		t.Fatalf("错误信息应说明原因：%s", run.Body.String())
	}
}

// TestMigrateLegacyReviewShortcut 锁住 v1 种子数据的定向修复：只改与种子完全一致的
// 那一行，用户自建/改过的条目不动，且只跑一次。
func TestMigrateLegacyReviewShortcut(t *testing.T) {
	server := newTestServer(t)
	// 新库启动时 migrate 已经跑过并留下标记；这里模拟"老库带着种子行升级上来"的场景。
	if _, err := server.db.Exec(`delete from app_metadata where key='common_shortcuts_review_fix_v1'`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seed := func(id, name, template string) {
		t.Helper()
		if _, err := server.db.Exec(`insert into shortcuts (id,name,description,kind,template,scope,default_action,group_name,pinned,enabled,sort_order,created_at,updated_at) values (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, name, "", "command_request", template, "local", "confirm", "常用命令", 1, 1, 0, now, now); err != nil {
			t.Fatalf("insert shortcut %s: %v", id, err)
		}
	}
	seed("legacy", "代码审查", "/review")
	seed("custom", "我自己写的", "/review") // 同名模板但不同名：必须原样保留
	if err := server.migrateLegacyReviewShortcut(context.Background(), now); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	templateOf := func(id string) string {
		t.Helper()
		var template string
		if err := server.db.QueryRow(`select template from shortcuts where id=?`, id).Scan(&template); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		return template
	}
	if got := templateOf("legacy"); got != "/code-review" {
		t.Fatalf("种子命令未被修好：%q", got)
	}
	if got := templateOf("custom"); got != "/review" {
		t.Fatalf("用户自建条目被误改：%q", got)
	}
	var audits int
	if err := server.db.QueryRow(`select count(*) from shortcut_audit_events where shortcut_id='legacy' and action='updated'`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Fatalf("修复必须留审计：audits = %d", audits)
	}
	// 幂等：再跑一次不得重复修（标记行已存在）。
	if err := server.migrateLegacyReviewShortcut(context.Background(), now); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	var marker int
	if err := server.db.QueryRow(`select count(*) from app_metadata where key='common_shortcuts_review_fix_v1'`).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if marker != 1 {
		t.Fatalf("修复标记应只有一行：%d", marker)
	}
}

func mustCommandCatalog(t *testing.T) claudeCommandCatalog {
	t.Helper()
	catalog, ok := parseClaudeCommandCatalog([]byte(testInitPayload))
	if !ok {
		t.Fatal("test payload must parse")
	}
	return catalog
}

// createCommandTestProject 建一个走本机 runner 的项目，让 agentClaudeRunnerFor 能解析到
// 测试替换掉的 server.runner（与 resolveAgentTargetEnv 的平台判定保持一致）。
func createCommandTestProject(t *testing.T, server *Server, projectPath string) string {
	t.Helper()
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values ('project','demo',?,?,?,1,?)`,
		projectPath, server.localRunnerID(), "main", time.Now().UTC()); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	return "project"
}

func projectCommandsView(t *testing.T, server *Server, projectID, query string) ProjectCommandsView {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/commands?"+query, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("commands status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	var view ProjectCommandsView
	if err := json.NewDecoder(recorder.Body).Decode(&view); err != nil {
		t.Fatalf("decode view: %v", err)
	}
	return view
}

func writeCommandFileForTest(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
