package app

import (
	"encoding/base64"
	"strings"
	"testing"
)

// 本轮（2026-09-15）修掉的模板缺陷的回归测试。
//
// 背景：模板目录原来只声明「启动命令是什么」，既不声明需要什么运行时、也不声明要填哪个
// 凭据，而且 github 一条指向的 npm 包已于 2025-04 归档弃用 —— 用户点进去必然失败，且失败
// 发生在保存之后。这里把「模板必须自洽」固化成不变量。

func findMCPPreset(id string) (mcpPreset, bool) {
	for _, preset := range mcpPresetCatalog() {
		if preset.ID == id {
			return preset, true
		}
	}
	return mcpPreset{}, false
}

// TestMCPPresetCatalogIsWellFormed 守住模板的基本形状：字段与传输类型自洽、Name 是合法
// server key、ID 与 name 都不重复。模板写错在这里就失败，而不是等用户点了才发现。
func TestMCPPresetCatalogIsWellFormed(t *testing.T) {
	presets := mcpPresetCatalog()
	if len(presets) == 0 {
		t.Fatal("模板目录为空")
	}
	seenID := map[string]bool{}
	seenName := map[string]bool{}
	for _, preset := range presets {
		switch {
		case preset.ID == "" || preset.Name == "" || preset.DisplayName == "":
			t.Errorf("模板缺少 id / name / displayName：%+v", preset)
			continue
		case seenID[preset.ID]:
			t.Errorf("模板 id 重复：%s", preset.ID)
			continue
		case seenName[preset.Name]:
			t.Errorf("模板 name 重复（会撞 MCP server key）：%s", preset.Name)
			continue
		}
		seenID[preset.ID] = true
		seenName[preset.Name] = true

		if !mcpServerNamePattern.MatchString(preset.Name) {
			t.Errorf("模板 %s 的 name 非法（须匹配 %s）：%s", preset.ID, mcpServerNamePattern, preset.Name)
		}
		switch preset.Transport {
		case mcpTransportStdio:
			if strings.TrimSpace(preset.Command) == "" {
				t.Errorf("stdio 模板 %s 未声明启动命令", preset.ID)
			}
			if preset.URL != "" {
				t.Errorf("stdio 模板 %s 不应带 url", preset.ID)
			}
		case mcpTransportHTTP, mcpTransportSSE:
			if strings.TrimSpace(preset.URL) == "" {
				t.Errorf("%s 模板 %s 未声明 url", preset.Transport, preset.ID)
			}
			if preset.Command != "" {
				t.Errorf("%s 模板 %s 不应带启动命令", preset.Transport, preset.ID)
			}
		default:
			t.Errorf("模板 %s 传输类型非法：%s", preset.ID, preset.Transport)
		}
		if len(preset.Environments) == 0 {
			t.Errorf("模板 %s 未声明适用环境", preset.ID)
		}

		// 面向用户的三个字段：目录卡片全靠它们才读得懂（用户不懂 MCP，只看得懂服务名与一句人话）。
		if preset.Summary == "" || preset.Category == "" || preset.Icon == "" {
			t.Errorf("模板 %s 缺少 Summary / Category / Icon：%+v", preset.ID, preset)
		}
		if preset.Description != preset.Summary {
			t.Errorf("模板 %s 的 Description 与 Summary 不一致（同一句话，两处口径会漂移）", preset.ID)
		}
		switch preset.Category {
		case mcpPresetCategoryCommon, mcpPresetCategoryLocal:
		default:
			t.Errorf("模板 %s 的分组未知：%s", preset.ID, preset.Category)
		}
		if preset.Category == mcpPresetCategoryLocal && preset.Transport != mcpTransportStdio {
			t.Errorf("「%s」分组的条目 %s 应为 stdio，实际 %s", mcpPresetCategoryLocal, preset.ID, preset.Transport)
		}

		// 依赖声明必须能被运行时检查真的检查得了，且要有可读名称。
		for _, require := range preset.Requires {
			if !mcpRuntimeCommandPattern.MatchString(require.Command) {
				t.Errorf("模板 %s 的依赖命令名无法被运行时检查支持：%s", preset.ID, require.Command)
			}
			if require.Label == "" {
				t.Errorf("模板 %s 的依赖 %s 缺少可读名称", preset.ID, require.Command)
			}
		}

		for _, credential := range preset.Credentials {
			if credential.Key == "" || credential.Label == "" || credential.Description == "" {
				t.Errorf("模板 %s 的凭据声明不完整：%+v", preset.ID, credential)
				continue
			}
			// Authorization 头必须带 scheme（`Bearer <token>`）。让用户在向导里只粘 token、
			// 由 ValuePrefix 补前缀，用户就不必记格式 —— 漏了会表现为 401 而不是「没填」。
			if credential.Target == mcpPresetCredentialHeader && strings.EqualFold(credential.Key, "Authorization") && credential.ValuePrefix == "" {
				t.Errorf("模板 %s 的 Authorization 凭据未声明 ValuePrefix（用户粘裸 token 会 401）", preset.ID)
			}
			if credential.Target == mcpPresetCredentialEnv && credential.ValuePrefix != "" {
				t.Errorf("模板 %s 的环境变量凭据 %s 不该有 ValuePrefix（前缀是 HTTP 头概念）", preset.ID, credential.Key)
			}
			switch credential.Target {
			case mcpPresetCredentialEnv:
				// 声明成环境变量的凭据必须真的出现在启动参数里（如 docker 的 `-e KEY`），
				// 否则用户照着提示填了，却没有任何东西去消费它。
				if !strings.Contains(strings.Join(preset.Args, " "), credential.Key) {
					t.Errorf("模板 %s 声明了环境变量凭据 %s，但启动参数里没有引用它", preset.ID, credential.Key)
				}
			case mcpPresetCredentialHeader:
				// 请求头由用户在表单里填，模板只需要是 http/sse 形态。
				if preset.Transport == mcpTransportStdio {
					t.Errorf("模板 %s 声明了请求头凭据 %s，但它是 stdio 形态", preset.ID, credential.Key)
				}
			default:
				t.Errorf("模板 %s 的凭据 %s 落点非法：%s", preset.ID, credential.Key, credential.Target)
			}
		}
	}
}

// TestMCPPresetCatalogHasNoArchivedPackages 守住这一轮修掉的缺陷：模板不得再指向已归档的
// MCP 参考实现包。@modelcontextprotocol/server-github 于 2025-04 归档弃用（官方迁至
// github/github-mcp-server），旧包硬 pin SDK 1.0.1，协议协商停在 2024-11-05。
func TestMCPPresetCatalogHasNoArchivedPackages(t *testing.T) {
	archived := []string{
		"@modelcontextprotocol/server-github",
		"@modelcontextprotocol/server-postgres",
		"@modelcontextprotocol/server-slack",
		"@modelcontextprotocol/server-gitlab",
		"@modelcontextprotocol/server-puppeteer",
		"@modelcontextprotocol/server-redis",
		"@modelcontextprotocol/server-sentry",
	}
	for _, preset := range mcpPresetCatalog() {
		joined := strings.Join(append([]string{preset.Command}, preset.Args...), " ")
		for _, pkg := range archived {
			if strings.Contains(joined, pkg) {
				t.Errorf("模板 %s 仍指向已归档的包 %s", preset.ID, pkg)
			}
		}
	}
}

// TestMCPPresetCatalogHasNoPlaceholderEndpoints 模板里不该出现 example.com 这类占位地址：
// 它会被用户当成可用模板点进去，然后必然连接失败。
func TestMCPPresetCatalogHasNoPlaceholderEndpoints(t *testing.T) {
	for _, preset := range mcpPresetCatalog() {
		if strings.Contains(preset.URL, "example.com") {
			t.Errorf("模板 %s 指向占位地址 %s", preset.ID, preset.URL)
		}
		// 示例模板的存在本身就是缺陷：它没有任何真实用途。
		if strings.Contains(preset.DisplayName, "示例") {
			t.Errorf("模板 %s 的显示名仍带「示例」：%s", preset.ID, preset.DisplayName)
		}
	}
}

// TestMCPGitHubPresetsPointAtOfficialServer 固定 GitHub 模板的修复结论：两种形态都必须
// 指向官方实现，且各自声明运行时依赖与凭据。
func TestMCPGitHubPresetsPointAtOfficialServer(t *testing.T) {
	remote, ok := findMCPPreset("github")
	if !ok {
		t.Fatal("缺少 github 模板")
	}
	if remote.Transport != mcpTransportHTTP {
		t.Errorf("github 模板应为 http 形态，实际 %s", remote.Transport)
	}
	if remote.URL != mcpGitHubRemoteURL {
		t.Errorf("github 模板地址应为官方托管地址 %s，实际 %s", mcpGitHubRemoteURL, remote.URL)
	}
	if remote.DocsURL != mcpGitHubDocsURL {
		t.Errorf("github 模板应指向官方仓库 %s，实际 %s", mcpGitHubDocsURL, remote.DocsURL)
	}
	if len(remote.Credentials) != 1 || remote.Credentials[0].Key != "Authorization" {
		t.Errorf("github 模板应声明 Authorization 凭据，实际 %+v", remote.Credentials)
	}
	if remote.Credentials[0].DocsURL == "" {
		t.Error("github 模板的凭据应给出申请 Token 的地址")
	}
	// http 形态与环境无关，必须覆盖全部三种环境。
	if len(remote.Environments) != 3 {
		t.Errorf("http 形态的 github 模板应适用于全部环境，实际 %v", remote.Environments)
	}

	local, ok := findMCPPreset("github-local")
	if !ok {
		t.Fatal("缺少 github-local 模板")
	}
	if local.Transport != mcpTransportStdio || local.Command != "docker" {
		t.Errorf("github-local 模板应为 stdio + docker，实际 %s / %s", local.Transport, local.Command)
	}
	if !strings.Contains(strings.Join(local.Args, " "), "ghcr.io/github/github-mcp-server") {
		t.Errorf("github-local 模板应使用官方镜像，实际 %v", local.Args)
	}
	if len(local.Requires) != 1 || local.Requires[0].Command != "docker" {
		t.Errorf("github-local 模板应声明 docker 依赖，实际 %+v", local.Requires)
	}
}

// TestMCPStdioPresetsDeclareRuntimeRequires 凡是 stdio 形态的模板，都必须声明它的启动命令
// 来自哪个运行时 —— 这是「保存前就能知道跑不起来」的前提。
func TestMCPStdioPresetsDeclareRuntimeRequires(t *testing.T) {
	for _, preset := range mcpPresetCatalog() {
		if preset.Transport != mcpTransportStdio {
			continue
		}
		declared := false
		for _, require := range preset.Requires {
			if require.Command == preset.Command {
				declared = true
				break
			}
		}
		if !declared {
			t.Errorf("stdio 模板 %s 的命令 %s 未在 requires 里声明", preset.ID, preset.Command)
		}
	}
}

// TestMCPCommonPresetsAreOneClick 锁住目录的**筛选标准**（2026-09-15 定）。
//
// 「常用服务」分组是给不懂 MCP 的用户走的**主路径**，所以它必须真的满足「点一次浏览器授权
// 就能用」：不支持 OAuth、或者还要用户先在目标环境装个运行时，都不配放在这个位置 ——
// 放进去就等于把「依赖 + 申请 Token」两道门槛又塞回给用户。
func TestMCPCommonPresetsAreOneClick(t *testing.T) {
	count := 0
	for _, preset := range mcpPresetCatalog() {
		if preset.Category != mcpPresetCategoryCommon {
			continue
		}
		count++
		if !preset.OAuth {
			t.Errorf("「常用服务」条目 %s 不支持浏览器授权，用户还得自己去申请凭据", preset.ID)
		}
		if len(preset.Requires) > 0 {
			t.Errorf("「常用服务」条目 %s 需要先在目标环境装东西（%+v），不属于一键路径", preset.ID, preset.Requires)
		}
		if preset.Transport != mcpTransportHTTP && preset.Transport != mcpTransportSSE {
			t.Errorf("「常用服务」条目 %s 应为远程形态，实际 %s", preset.ID, preset.Transport)
		}
		// 远程地址必须附官方文档，用户才有地方核对「这个地址是不是官方的」。
		if preset.DocsURL == "" {
			t.Errorf("「常用服务」条目 %s 未给出官方文档地址", preset.ID)
		}
	}
	if count < 5 {
		t.Errorf("「常用服务」只有 %d 条，目录太薄 —— 用户找不到自己在用的服务就会放弃", count)
	}
}

// TestMCPLocalPresetsAreHonestAboutDependencies 锁住「本机运行」分组的诚实性：既然它要用户
// 先装东西，就必须把要装什么说清楚（requires 非空且带可读名称），不能只给一条命令。
func TestMCPLocalPresetsAreHonestAboutDependencies(t *testing.T) {
	for _, preset := range mcpPresetCatalog() {
		if preset.Category != mcpPresetCategoryLocal {
			continue
		}
		if len(preset.Requires) == 0 {
			t.Errorf("「本机运行」条目 %s 未声明任何运行时依赖", preset.ID)
		}
		for _, require := range preset.Requires {
			if require.Label == "" {
				t.Errorf("条目 %s 的依赖 %s 缺少可读名称（用户看不懂 npx 是什么）", preset.ID, require.Command)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 运行时检查
// ---------------------------------------------------------------------------

func TestMCPRuntimeProbeScriptAlwaysSucceeds(t *testing.T) {
	for _, command := range []string{"npx", "uvx", "docker"} {
		script := mcpRuntimeProbeScript(command)
		if !strings.Contains(script, "command -v "+command) {
			t.Errorf("脚本未检查 %s：%s", command, script)
		}
		// `||` 兜底让退出码恒为 0 —— 否则「确实没装」会与「通道坏了」混为一谈。
		if !strings.Contains(script, "|| echo "+mcpRuntimeMissingMarker) {
			t.Errorf("脚本缺少退出码兜底：%s", script)
		}
	}
}

func TestMCPRuntimeItemFromOutput(t *testing.T) {
	cases := []struct {
		name   string
		output string
		found  bool
		path   string
	}{
		{"命中", "/usr/local/bin/npx\n", true, "/usr/local/bin/npx"},
		{"哨兵", mcpRuntimeMissingMarker + "\n", false, ""},
		{"哨兵夹在中间", "noise\n" + mcpRuntimeMissingMarker + "\n", false, ""},
		{"空输出", "  \n", false, ""},
		{"多行只取首行", "/usr/bin/docker\nwarning: something\n", true, "/usr/bin/docker"},
	}
	for _, tc := range cases {
		got := mcpRuntimeItemFromOutput("npx", tc.output)
		if got.Found != tc.found || got.Path != tc.path {
			t.Errorf("%s：got found=%v path=%q，want found=%v path=%q", tc.name, got.Found, got.Path, tc.found, tc.path)
		}
		if got.Command != "npx" {
			t.Errorf("%s：结论未回填命令名：%+v", tc.name, got)
		}
	}
}

func TestMCPRuntimeCommandPatternRejectsShellMetacharacters(t *testing.T) {
	allowed := []string{"npx", "uvx", "docker", "node", "python3", "my-tool", "a.b_c+d", "NPM"}
	for _, command := range allowed {
		if !mcpRuntimeCommandPattern.MatchString(command) {
			t.Errorf("应被接受：%s", command)
		}
	}
	// 这些形态会被拼进远端 / WSL 的 shell，必须拒掉。
	rejected := []string{
		"", "npx; rm -rf /", "npx && curl evil", "$(id)", "`id`",
		"--flag", "a b", "a|b", "a>b", "a\\b", "'x'", "npx\nuvx", "-npx",
	}
	for _, command := range rejected {
		if mcpRuntimeCommandPattern.MatchString(command) {
			t.Errorf("应被拒绝：%q", command)
		}
	}
}

func TestResolveMCPDraftValues(t *testing.T) {
	got := resolveMCPDraftValues(map[string]string{
		"ROOT":  "${PROJECT_DIR}/docs",
		"OTHER": "${HOME}/x",
		"PLAIN": "v",
	}, agentTargetEnvWindows, `C:\proj`)
	if got["ROOT"] != `C:\proj/docs` {
		t.Errorf("草稿值未按目标环境解析占位符：%q", got["ROOT"])
	}
	if got["OTHER"] != "${HOME}/x" {
		t.Errorf("非 PROJECT_DIR 的占位符应原样保留（交给 CLI 展开）：%q", got["OTHER"])
	}
	if got["PLAIN"] != "v" {
		t.Errorf("普通值被改动：%q", got["PLAIN"])
	}

	// 未选项目时原样保留 —— 静默变成空串会造出一条缺路径的命令。
	kept := resolveMCPDraftValues(map[string]string{"ROOT": "${PROJECT_DIR}/docs"}, agentTargetEnvWindows, "")
	if kept["ROOT"] != "${PROJECT_DIR}/docs" {
		t.Errorf("未选项目时应原样保留：%q", kept["ROOT"])
	}
	if resolveMCPDraftValues(nil, agentTargetEnvWindows, "x") != nil {
		t.Error("空输入应返回 nil，不要造一个空 map")
	}
}

// decodeRemoteProbeScript 解出 `printf '%s' <b64> | base64 -d | sh` 里的脚本体。
//
// 远端探测命令是 base64 包起来的（密钥不进 argv），所以对它断言**必须先解码** ——
// 直接对命令串找 "cd " 永远为假，会写出一个「看起来在验、其实恒真/恒假」的断言。
func decodeRemoteProbeScript(t *testing.T, command string) string {
	t.Helper()
	// 命令形态是 `printf '%s' '<b64>' | base64 -d | sh` —— 单引号段有两处（`%s` 与 base64），
	// 所以**按段试解**而不是取「第一个引号到最后一个引号」。
	for _, part := range strings.Split(command, "'") {
		decoded, err := base64.StdEncoding.DecodeString(part)
		if err == nil && len(decoded) > 0 {
			return string(decoded)
		}
	}
	t.Fatalf("未在命令里找到可解码的 base64 段：%s", command)
	return ""
}

// TestBuildRemoteStdioProbeCommandSkipsEmptyCwd 锁住「没有工作目录就别拼 cd」这条：
// `cd ”` 会让整个远端探测脚本以「没有那个文件或目录」失败，而错误指向 cwd，与真正的原因无关 ——
// 草稿态试连常常没有项目，很容易踩到。
func TestBuildRemoteStdioProbeCommandSkipsEmptyCwd(t *testing.T) {
	without := decodeRemoteProbeScript(t, buildRemoteStdioProbeCommand(mcpProbeRequest{Command: "npx", Args: []string{"-y", "x"}}))
	if strings.Contains(without, "cd ") {
		t.Errorf("未指定工作目录时不应拼 cd：%s", without)
	}
	with := decodeRemoteProbeScript(t, buildRemoteStdioProbeCommand(mcpProbeRequest{Command: "npx", ProjectPath: "/srv/app"}))
	if !strings.Contains(with, "cd ") {
		t.Errorf("指定了工作目录时应保留 cd：%s", with)
	}
}

func TestMCPConnectionIDFromRunner(t *testing.T) {
	cases := []struct {
		name     string
		runnerID string
		explicit string
		want     string
	}{
		{"从 runnerID 反推", "ssh-abc123", "", "abc123"},
		{"显式优先", "ssh-abc123", "explicit", "explicit"},
		{"空 runner", "", "", ""},
		{"本机 runner 不当成连接", "local", "", ""},
		{"只有前缀", "ssh-", "", ""},
		{"多余空白", "  ssh-abc123  ", "", "abc123"},
	}
	for _, tc := range cases {
		if got := mcpConnectionIDFromRunner(tc.runnerID, tc.explicit); got != tc.want {
			t.Errorf("%s：got %q，want %q", tc.name, got, tc.want)
		}
	}
}
