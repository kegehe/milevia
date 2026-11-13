package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

func TestPluginActionResultParsesShownCommand(t *testing.T) {
	result := pluginActionResultFromOutput(pluginCommandOutput{
		Stdout: `{"ok":false,"shownCommand":{"command":"npm exec evil@1.0.0","sha256":"deadbeef"}}`,
		Err:    errors.New("exit status 1"),
	}, "安装插件失败")
	if result.OK || !result.NeedsConfirmation {
		t.Fatalf("应当要求确认：%+v", result)
	}
	if result.Command != "npm exec evil@1.0.0" || result.CommandSHA256 != "deadbeef" {
		t.Fatalf("命令或 sha256 解析不对：%+v", result)
	}

	// 普通失败不能被误判成"待确认"，否则前端会展示一个空命令。
	result = pluginActionResultFromOutput(pluginCommandOutput{Stderr: "network unreachable", Err: errors.New("exit 1")}, "安装插件失败")
	if result.NeedsConfirmation || result.OK {
		t.Fatalf("普通失败被误判：%+v", result)
	}
	if result.Message != "network unreachable" {
		t.Fatalf("失败信息不对：%+v", result)
	}
}

func TestExtractJSONToleratesPromptLines(t *testing.T) {
	raw := "Updating marketplace…\n{\"installed\":[],\"available\":[]}\n"
	got, ok := extractJSON(raw)
	if !ok {
		t.Fatal("应当取到 JSON")
	}
	var payload map[string]any
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatalf("取到的不是 JSON：%v", err)
	}
	if _, ok := payload["installed"]; !ok {
		t.Fatalf("解析结果缺字段：%v", payload)
	}
}

func TestParsePluginMarketplacesDialects(t *testing.T) {
	claude, err := parsePluginMarketplaces(pluginDialectClaude, []byte(`[{"name":"demo","source":"github","repo":"owner/repo","installLocation":"/tmp/demo"}]`))
	if err != nil || len(claude) != 1 {
		t.Fatalf("Claude 市场解析失败：%v %+v", err, claude)
	}
	if claude[0].Name != "demo" || claude[0].Source != "owner/repo" || claude[0].InstallLocation != "/tmp/demo" {
		t.Fatalf("Claude 市场字段不对：%+v", claude[0])
	}

	codex, err := parsePluginMarketplaces(pluginDialectCodex, []byte(`{"marketplaces":[{"name":"local","root":"/tmp/market"}]}`))
	if err != nil || len(codex) != 1 {
		t.Fatalf("Codex 市场解析失败：%v %+v", err, codex)
	}
	if codex[0].Name != "local" || codex[0].InstallLocation != "/tmp/market" {
		t.Fatalf("Codex 市场字段不对：%+v", codex[0])
	}

	qoder, err := parsePluginMarketplaces(pluginDialectQoder, []byte(`[{"name":"q","source":{"source":"github","repo":"o/r"},"installLocation":"/tmp/q"}]`))
	if err != nil || len(qoder) != 1 {
		t.Fatalf("Qoder 市场解析失败：%v %+v", err, qoder)
	}
	if qoder[0].Name != "q" || qoder[0].Kind != "github" || qoder[0].Source != "o/r" {
		t.Fatalf("Qoder 市场字段不对：%+v", qoder[0])
	}
}

func TestFilterDisabledPluginScans(t *testing.T) {
	disabled := []string{filepath.FromSlash("/plugins/off")}
	scans := []skillScan{
		{skillFile: filepath.FromSlash("/plugins/off/skills/a/SKILL.md"), nameHint: "a"},
		{skillFile: filepath.FromSlash("/plugins/on/skills/b/SKILL.md"), nameHint: "b"},
		{skillFile: filepath.FromSlash("/home/user/.claude/skills/c/SKILL.md"), nameHint: "c"},
	}
	got := filterDisabledPluginScans(scans, disabled)
	if len(got) != 2 {
		t.Fatalf("应当只剩 2 条，得到 %d：%+v", len(got), got)
	}
	for _, scan := range got {
		if scan.nameHint == "a" {
			t.Fatal("被禁用插件的技能没有被过滤")
		}
	}
}

func TestPluginPathKeysAndScopes(t *testing.T) {
	if pluginNameFromID("alpha@demo") != "alpha" {
		t.Fatal("插件名应当取 @ 之前")
	}
	if pluginMarketplaceFromID("alpha@demo") != "demo" {
		t.Fatal("市场名应当取 @ 之后")
	}
	if normalizePluginScope("PROJECT") != pluginScopeProject || normalizePluginScope("") != pluginScopeUser {
		t.Fatal("范围归一化不对")
	}
	// 路径分隔符跟随运行平台：这个用例要在 Windows 与类 Unix 上都能跑。
	root := "C:" + string(filepath.Separator) + "Plugins" + string(filepath.Separator) + "A"
	skill := filepath.Join(root, "skills", "x", "SKILL.md")
	sibling := "C:" + string(filepath.Separator) + "Plugins" + string(filepath.Separator) + "AB" + string(filepath.Separator) + "skills" + string(filepath.Separator) + "x" + string(filepath.Separator) + "SKILL.md"
	if !pathWithinPlugin(skill, strings.ToLower(root)) {
		t.Fatal("Windows 路径应当忽略大小写并识别包含关系")
	}
	if pathWithinPlugin(sibling, root) {
		t.Fatal("前缀相同的兄弟目录不该被误判为包含")
	}
}

// TestPluginCommandArgs 是跨平台可跑的参数拼装断言（假 CLI 夹具只在类 Unix 上跑）。
func TestPluginCommandArgs(t *testing.T) {
	installCases := []struct {
		name     string
		dialect  pluginDialect
		selector string
		scope    string
		source   string
		accept   string
		want     string
	}{
		{"claude qualified", pluginDialectClaude, "alpha@demo", "user", "", "", "plugin install alpha@demo --scope user --json"},
		{"claude bare with source", pluginDialectClaude, "alpha", "project", "owner/repo", "", "plugin install alpha --scope project --json --marketplace owner/repo"},
		{"claude accept", pluginDialectClaude, "alpha@demo", "user", "", "deadbeef", "plugin install alpha@demo --scope user --json --accept-command deadbeef"},
		{"qoder ignores marketplace", pluginDialectQoder, "alpha", "local", "owner/repo", "", "plugins install alpha --scope local --json"},
		{"codex keeps marketplace name", pluginDialectCodex, "alpha", "user", "demo", "", "plugin add alpha --marketplace demo"},
		{"codex qualified", pluginDialectCodex, "alpha@demo", "user", "", "", "plugin add alpha@demo"},
	}
	for _, testCase := range installCases {
		got := strings.Join(pluginInstallArgs(testCase.dialect, testCase.selector, testCase.scope, testCase.source, testCase.accept), " ")
		if got != testCase.want {
			t.Errorf("%s: got %q want %q", testCase.name, got, testCase.want)
		}
	}

	record := PluginRecord{ID: "alpha@demo", Scope: pluginScopeProject}
	updateCases := []struct {
		name    string
		dialect pluginDialect
		scope   string
		accept  string
		want    string
	}{
		{"claude scoped", pluginDialectClaude, pluginScopeProject, "", "plugin update alpha@demo --scope project --json"},
		{"claude accept", pluginDialectClaude, pluginScopeUser, "deadbeef", "plugin update alpha@demo --scope user --json --accept-command deadbeef"},
		{"qoder scoped", pluginDialectQoder, pluginScopeLocal, "", "plugins update alpha@demo --scope local"},
	}
	for _, testCase := range updateCases {
		record.Scope = testCase.scope
		args, err := pluginUpdateArgs(testCase.dialect, record, testCase.accept)
		if err != nil {
			t.Fatalf("%s: %v", testCase.name, err)
		}
		if got := strings.Join(args, " "); got != testCase.want {
			t.Errorf("%s: got %q want %q", testCase.name, got, testCase.want)
		}
	}
	if _, err := pluginUpdateArgs(pluginDialectCodex, record, ""); err == nil {
		t.Fatal("Codex 没有升级子命令，应当报错")
	}
}

// TestPluginPathParamDecodesPercentEncoding 钉住 "%40" 的查找路径。
// 前端用 encodeURIComponent 拼插件 ID，chi 在 RawPath 上匹配且不解码，漏了这一步详情必然 404。
func TestPluginPathParamDecodesPercentEncoding(t *testing.T) {
	var got string
	router := chi.NewRouter()
	router.Get("/plugins/{pluginID}", func(w http.ResponseWriter, r *http.Request) {
		got = pluginPathParam(r, "pluginID")
		w.WriteHeader(http.StatusNoContent)
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/plugins/alpha%40demo", nil))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("路由未命中：%d", recorder.Code)
	}
	if got != "alpha@demo" {
		t.Fatalf("路径参数未解码：%q", got)
	}
}

// TestFindPluginRecordDisambiguates 同一插件同时装在多个范围时，必须按 scope/project 选中正确记录。
func TestFindPluginRecordDisambiguates(t *testing.T) {
	records := []PluginRecord{
		{ID: "alpha@demo", Scope: pluginScopeUser},
		{ID: "alpha@demo", Scope: pluginScopeProject, ProjectPath: `/work/a`},
		{ID: "alpha@demo", Scope: pluginScopeProject, ProjectPath: `/work/b`},
	}
	if record, ok := findPluginRecord(records, "alpha@demo", "project", `/work/b/`); !ok || record.ProjectPath != `/work/b` {
		t.Fatalf("未按项目消歧：%+v %v", record, ok)
	}
	if record, ok := findPluginRecord(records, "alpha@demo", "user", ""); !ok || record.Scope != pluginScopeUser {
		t.Fatalf("未按范围消歧：%+v %v", record, ok)
	}
	if _, ok := findPluginRecord(records, "alpha@demo", "local", ""); ok {
		t.Fatal("不存在的范围不该命中")
	}
	if record, ok := findPluginRecord(records, "alpha@demo", "", ""); !ok || record.Scope != pluginScopeUser {
		t.Fatalf("无范围时应取第一条：%+v %v", record, ok)
	}
	// 指定了项目但没有任何记录匹配时不能退回第一条，否则会操作到别的项目的安装。
	if _, ok := findPluginRecord(records, "alpha@demo", "project", `/work/c`); ok {
		t.Fatal("项目路径不匹配时不该命中其他记录")
	}
}

// TestParseQoderPluginListShapes 钉住 Qoder 的两种输出：纯列表是数组，
// 带 --available 是 {installed, available} 信封，且 available 的 source 是对象。
func TestParseQoderPluginListShapes(t *testing.T) {
	entries, available, envelope, err := parseQoderPluginList([]byte(`[{"id":"alpha@qoder-marketplace","name":"alpha","scope":"user","enabled":true,"installPath":"/opt/alpha"}]`))
	if err != nil || envelope || len(entries) != 1 || len(available) != 0 {
		t.Fatalf("数组形状解析不对：%v %v %v %v", err, entries, available, envelope)
	}

	raw := `{"installed":[{"id":"Presentations@qoder-marketplace","name":"Presentations","scope":"user","enabled":true,"installPath":"C:\\\\Users\\\\x"}],"available":[{"pluginId":"better-harness@qoder-plugins","name":"better-harness","marketplaceName":"qoder-plugins","version":"0.2.20","description":"build","source":{"source":"archive","url":"https://example.com/x.zip"}}]}`
	entries, available, envelope, err = parseQoderPluginList([]byte(raw))
	if err != nil || !envelope || len(entries) != 1 || len(available) != 1 {
		t.Fatalf("信封形状解析不对：%v %v %v %v", err, entries, available, envelope)
	}
	if available[0].PluginID != "better-harness@qoder-plugins" || available[0].MarketplaceName != "qoder-plugins" {
		t.Fatalf("available 字段不对：%+v", available[0])
	}

	records, view := qoderPluginView(entries, available, envelope)
	if len(records) != 1 || records[0].ID != "Presentations@qoder-marketplace" || !records[0].StateKnown {
		t.Fatalf("installed 未转成记录：%+v", records)
	}
	if len(view) != 1 || view[0].Installed {
		t.Fatalf("available 转换不对：%+v", view)
	}

	// 已安装的同 ID 出现在 available 里时要标记，前端据此隐藏。
	_, view = qoderPluginView(
		[]qoderPluginEntry{{ID: "same@q", Name: "same", Scope: "user", InstallPath: "/opt/same"}},
		[]qoderAvailablePlugin{{PluginID: "same@q", Name: "same"}},
		true,
	)
	if len(view) != 1 || !view[0].Installed {
		t.Fatalf("已安装的 available 项未标记：%+v", view)
	}
}

// TestPluginCapabilitiesHelpShrink 钉住帮助只能收缩「依赖子命令」的动作；
// 详情由本地清单补全，Qoder/Codex 的帮助里没有 details 也必须保留。
func TestPluginCapabilitiesHelpShrink(t *testing.T) {
	qoder, _ := pluginAdapterFor("qoder")
	qoderCaps := pluginCapabilitiesFromHelp("Usage: qodercli plugins\n\nCommands:\n  list\n  install\n  enable\n  disable\n  update\n  marketplace\n", qoder.Capabilities())
	if !qoderCaps.Details {
		t.Fatal("详情不该被帮助文本收缩")
	}
	if !qoderCaps.List || !qoderCaps.Install || !qoderCaps.Enable || !qoderCaps.Marketplaces {
		t.Fatalf("Qoder 能力位被过度收缩：%+v", qoderCaps)
	}

	codex, _ := pluginAdapterFor("codex")
	codexCaps := pluginCapabilitiesFromHelp("Usage: codex plugin\n\nCommands:\n  add\n  list\n  marketplace  Add, list, upgrade, or remove\n\nOptions:\n  --enable <FEATURE>\n  --disable <FEATURE>\n", codex.Capabilities())
	if !codexCaps.Details || !codexCaps.Install || !codexCaps.Uninstall {
		t.Fatalf("Codex 应保留详情/安装/卸载：%+v", codexCaps)
	}
	if codexCaps.Enable || codexCaps.Disable || codexCaps.Update {
		t.Fatalf("帮助里的 --enable/upgrade 字眼不能扩张能力：%+v", codexCaps)
	}

	fallback := qoder.Capabilities()
	if got := pluginCapabilitiesFromHelp("no command table here", fallback); !got.Details || !got.Enable {
		t.Fatalf("读不到子命令表时应保留适配器声明：%+v", got)
	}
}

// TestPluginRecordBelongsToProjectWSL 钉住 UNC 与发行版内路径的等价比较。
func TestPluginRecordBelongsToProjectWSL(t *testing.T) {
	unc := `\\wsl.localhost\Ubuntu\home\user\proj`
	if !pluginRecordBelongsToProject(PluginRecord{Scope: pluginScopeProject, ProjectPath: `/home/user/proj`}, unc) {
		t.Fatal("WSL 发行版内路径应当匹配 UNC 项目")
	}
	if !pluginRecordBelongsToProject(PluginRecord{Scope: pluginScopeProject, ProjectPath: unc}, unc) {
		t.Fatal("UNC 记录应当匹配 UNC 项目")
	}
	if pluginRecordBelongsToProject(PluginRecord{Scope: pluginScopeProject, ProjectPath: `/home/user/other`}, unc) {
		t.Fatal("其他项目不该匹配")
	}
	if !pluginRecordBelongsToProject(PluginRecord{Scope: pluginScopeUser}, unc) {
		t.Fatal("user 范围不受项目路径影响")
	}
}

// TestScanPluginManifestsIgnoresAuthorState 钉住：清单文件不能自带启用状态。
func TestScanPluginManifestsIgnoresAuthorState(t *testing.T) {
	root := t.TempDir()
	manifestDir := filepath.Join(root, "evil", ".claude-plugin")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := `{"name":"evil-plugin","version":"1.0.0","enabled":true,"stateKnown":true,"managed":true,"scope":"user"}`
	if err := os.WriteFile(filepath.Join(manifestDir, "plugin.json"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	plugins := scanPluginManifests(context.Background(), root, "claude-code", ".claude-plugin/plugin.json")
	if len(plugins) != 1 {
		t.Fatalf("应当扫描到 1 个插件：%+v", plugins)
	}
	got := plugins[0]
	if got.Enabled || got.StateKnown || got.Managed || got.Scope != "" {
		t.Fatalf("清单里的状态字段不应被采信：%+v", got)
	}
	if got.Source != "manifest" || got.Status != "discovered" {
		t.Fatalf("来源/状态标记不对：%+v", got)
	}
}

// TestScanPluginListLabelsProjectScope 钉住兜底扫描的根目录归属。
func TestScanPluginListLabelsProjectScope(t *testing.T) {
	projectDir := t.TempDir()
	manifestDir := filepath.Join(projectDir, ".cursor", "plugins", "probe", ".cursor-plugin")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "plugin.json"), []byte(`{"name":"zz-milevia-scan-probe","version":"9.9.9","description":"probe"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	server := &Server{runnerRegistry: newRunnerRegistry()}
	view, err := server.scanPluginList(context.Background(), server.localRunnerID(), "cursor", projectDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range view.Plugins {
		if record.Name != "zz-milevia-scan-probe" {
			continue
		}
		if record.Scope != pluginScopeProject || record.ProjectPath != projectDir {
			t.Fatalf("项目级插件被标错范围：%+v", record)
		}
		if record.StateKnown || record.Enabled {
			t.Fatalf("扫描记录不应声称启用状态：%+v", record)
		}
		return
	}
	t.Fatalf("没有扫描到项目插件：%+v", view.Plugins)
}

func TestPluginScopesMatchCLIReality(t *testing.T) {
	claude, _ := pluginAdapterFor("claude-code")
	codex, _ := pluginAdapterFor("codex")
	if !pluginScopeSupported(claude, "project") || !pluginScopeSupported(claude, "local") {
		t.Fatal("Claude 支持三种范围")
	}
	if pluginScopeSupported(codex, "project") || !pluginScopeSupported(codex, "user") {
		t.Fatal("Codex 只有全局范围，不能显示成可选项目范围")
	}
	if err := validatePluginName("市场名称", "-rf", false); err == nil {
		t.Fatal("前导 '-' 的名称应当被拒绝")
	}
	if err := validatePluginName("市场名称", "", true); err != nil {
		t.Fatalf("允许空名称时不应报错：%v", err)
	}
}

// TestPluginListAndInstallViaFakeCLI 用假 CLI 跑通「列表 → 市场 → 安装参数」整条链。
// 只在类 Unix 上跑：夹具是一个 #!/bin/sh 脚本，Windows 不适用。
func TestPluginListAndInstallViaFakeCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("假 CLI 夹具是 shell 脚本，Windows 不适用")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	script := filepath.Join(dir, "claude")
	body := `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_CLI_LOG"
case "$*" in
  *"marketplace list"*) printf '%s\n' '[{"name":"demo","source":"github","repo":"owner/repo","installLocation":"/tmp/demo"}]' ;;
  *"plugin --help"*) printf '%s\n' 'Usage: claude plugin <command>' 'Commands:' '  list' '  install' '  uninstall' '  enable' '  disable' '  update' '  details' '  marketplace' ;;
  *"plugin install"*) printf '%s\n' '{"ok":true}' ;;
  *"plugin disable"*) printf '%s\n' '{"ok":true}' ;;
  *"plugin uninstall"*) printf '%s\n' '{"ok":true}' ;;
  *"plugin list"*) printf '%s\n' '[{"id":"alpha@demo","version":"1.2.3","scope":"user","enabled":true,"installPath":"/nonexistent/alpha","installedAt":"2026-11-01T00:00:00Z"}]' ;;
  *) printf '%s\n' '{}' ;;
esac
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_CLI_LOG", logPath)

	server := &Server{
		config:         Config{},
		paths:          &agentPathResolver{override: map[string]string{"claude-code": script}, recorded: map[string]string{}},
		runnerRegistry: newRunnerRegistry(),
	}
	runnerID := server.localRunnerID()
	view, err := server.pluginList(context.Background(), runnerID, "claude-code", "", false)
	if err != nil {
		t.Fatalf("pluginList 失败：%v", err)
	}
	if view.Source != pluginSourceCLI || len(view.Plugins) != 1 {
		t.Fatalf("插件列表不对：%+v", view)
	}
	record := view.Plugins[0]
	if record.ID != "alpha@demo" || record.Name != "alpha" || record.Version != "1.2.3" || !record.Enabled || !record.StateKnown {
		t.Fatalf("插件字段不对：%+v", record)
	}
	if record.Key != "alpha@demo|user|" {
		t.Fatalf("记录 key 应当由 id|scope|projectPath 组成：%q", record.Key)
	}
	if len(view.Marketplaces) != 1 || view.Marketplaces[0].Name != "demo" {
		t.Fatalf("市场列表不对：%+v", view.Marketplaces)
	}
	if !view.Capabilities.Install || !view.Capabilities.Enable || !view.Capabilities.Uninstall {
		t.Fatalf("能力位不对：%+v", view.Capabilities)
	}

	// 写操作：确认参数按 CLI 真实语法拼装，且绝不自动追加 -y。
	adapter, _ := pluginAdapterFor("claude-code")
	exec := &pluginExec{server: server, runnerID: runnerID, agentID: "claude-code"}
	if result, err := adapter.Install(context.Background(), exec, "", pluginInstallRequest{Plugin: "beta@demo", Scope: "user"}); err != nil || !result.OK {
		t.Fatalf("安装失败：%v %+v", err, result)
	}
	// 已经带 @market 的选择器不能再追加 --marketplace，否则 Claude 会报"市场只能命名一次"。
	if result, err := adapter.Install(context.Background(), exec, "", pluginInstallRequest{Plugin: "beta@demo", Marketplace: "demo", Scope: "user"}); err != nil || !result.OK {
		t.Fatalf("限定选择器安装失败：%v %+v", err, result)
	}
	// 裸插件名才需要把市场名解析成来源（owner/repo）。
	if result, err := adapter.Install(context.Background(), exec, "", pluginInstallRequest{Plugin: "gamma", Marketplace: "demo", Scope: "user"}); err != nil || !result.OK {
		t.Fatalf("裸名安装失败：%v %+v", err, result)
	}
	if result, err := adapter.SetEnabled(context.Background(), exec, "", record, false); err != nil || !result.OK {
		t.Fatalf("禁用失败：%v %+v", err, result)
	}
	if result, err := adapter.Update(context.Background(), exec, "", record, ""); err != nil || !result.OK {
		t.Fatalf("升级失败：%v %+v", err, result)
	}
	if result, err := adapter.Update(context.Background(), exec, "", record, "deadbeef"); err != nil || !result.OK {
		t.Fatalf("确认升级失败：%v %+v", err, result)
	}
	if result, err := adapter.Uninstall(context.Background(), exec, "", record); err != nil || !result.OK {
		t.Fatalf("卸载失败：%v %+v", err, result)
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(raw)
	if strings.Contains(log, "-y") || strings.Contains(log, "--yes") {
		t.Fatalf("自动确认了命令：\n%s", log)
	}
	for _, want := range []string{
		"plugin install beta@demo --scope user --json",
		"plugin install gamma --scope user --json --marketplace owner/repo",
		"plugin disable alpha@demo --scope user --json",
		"plugin update alpha@demo --scope user --json",
		"plugin update alpha@demo --scope user --json --accept-command deadbeef",
		"plugin uninstall alpha@demo --scope user --json",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("调用参数缺少 %q：\n%s", want, log)
		}
	}
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, "install beta@demo") && strings.Contains(line, "--marketplace") {
			t.Fatalf("带 @market 的选择器不该再传 --marketplace：%s", line)
		}
	}
}

// WSL 项目的 project 路径有两种形态：项目表里是 UNC，CLI 报出来的 record.ProjectPath
// 已经是 /home/... 。后者不能被当成"无法换算"清空，否则插件写入会在 $HOME 里执行。
func TestPluginCrossScriptKeepsLinuxProjectPath(t *testing.T) {
	env := &wslCrossEnvironment{runner: &wslAgentRunner{distro: "Ubuntu"}}
	exec := &pluginExec{agentID: "claude-code"}

	script, err := exec.crossScript(env, []string{"plugin", "list", "--json"}, "/home/user/proj")
	if err != nil {
		t.Fatalf("crossScript: %v", err)
	}
	if !strings.Contains(script, "cd '/home/user/proj'") {
		t.Fatalf("linux project path was dropped from the script:\n%s", script)
	}

	script, err = exec.crossScript(env, []string{"plugin", "list", "--json"}, `\\wsl.localhost\Ubuntu\home\user\proj`)
	if err != nil {
		t.Fatalf("crossScript: %v", err)
	}
	if !strings.Contains(script, "cd '/home/user/proj'") {
		t.Fatalf("UNC project path was not converted:\n%s", script)
	}

	// Windows 本地路径在 WSL 里无法 cd：保持原来的保守行为（不拼 cd）。
	script, err = exec.crossScript(env, []string{"plugin", "list", "--json"}, `C:\projects\demo`)
	if err != nil {
		t.Fatalf("crossScript: %v", err)
	}
	if strings.Contains(script, "cd '") {
		t.Fatalf("windows path must not be used as a WSL cwd:\n%s", script)
	}
}

// 写操作之后，在途的单飞可能已经把旧清单发给后来者：invalidate 必须摘掉匹配的登记，
// 但只限同一 runner+agent，别的环境不能被牵连。
func TestInvalidatePluginCacheDropsMatchingInFlightLists(t *testing.T) {
	s := &Server{}
	match := keyForPluginList("runner-a", "claude-code", "/p", false, false)
	otherAgent := keyForPluginList("runner-a", "codex", "/p", false, false)
	otherRunner := keyForPluginList("runner-b", "claude-code", "/p", false, false)
	s.pluginFlights = map[string]*pluginListFlight{
		match:       {done: make(chan struct{})},
		otherAgent:  {done: make(chan struct{})},
		otherRunner: {done: make(chan struct{})},
	}
	s.pluginCache = map[string]pluginCacheEntry{
		match:       {at: time.Now()},
		otherRunner: {at: time.Now()},
	}
	s.invalidatePluginCache("runner-a", "claude-code")
	if _, ok := s.pluginFlights[match]; ok {
		t.Fatal("matching in-flight list survived invalidation")
	}
	if _, ok := s.pluginFlights[otherAgent]; !ok {
		t.Fatal("unrelated agent's flight was dropped")
	}
	if _, ok := s.pluginFlights[otherRunner]; !ok {
		t.Fatal("unrelated runner's flight was dropped")
	}
	if _, ok := s.pluginCache[match]; ok {
		t.Fatal("matching cache entry survived invalidation")
	}
}

// 输出被截断时的报错必须指出"超过上限"，不能误导成 CLI 没有返回 JSON。
func TestPluginJSONParseErrorDistinguishesTruncation(t *testing.T) {
	truncated := `{"plugins":[` + "\n…（输出已截断）"
	if err := pluginJSONParseError(truncated, "插件列表"); !strings.Contains(err.Error(), "上限") {
		t.Fatalf("truncated output misreported: %v", err)
	}
	if err := pluginJSONParseError("not json", "插件列表"); !strings.Contains(err.Error(), "没有返回 JSON") {
		t.Fatalf("non-JSON output misreported: %v", err)
	}
}

// 请求已经取消时不应再拉起 CLI，也不应留下 flight。
func TestPluginListWithRejectsCanceledRequestBeforeSpawn(t *testing.T) {
	s := &Server{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.pluginListWith(ctx, "runner-a", "claude-code", "/p", false, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled request err = %v, want context.Canceled", err)
	}
	if len(s.pluginFlights) != 0 {
		t.Fatalf("canceled request registered %d flights", len(s.pluginFlights))
	}
}
