package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"
)

// TestValidProfileAgentReadsCatalogNotHardcodedPair 是这一组整理的核心自证用例。
//
// 它把「目录里存在、但旧硬编码里没有」的工具塞进目录，然后断言校验函数放行。
// 如果哪天有人把 validProfileAgent 写回 `id == "claude-code" || id == "codex"`，
// 这条用例会立刻变红 —— 而那正是本次要消灭的东西。
func TestValidProfileAgentReadsCatalogNotHardcodedPair(t *testing.T) {
	withExtraCatalogEntry(t, AgentCatalogEntry{
		ID: "gemini-cli", Name: "Gemini CLI", Vendor: "Google",
		InstallKind: InstallKindNpmGlobal, NpmPackage: "@google/gemini-cli",
		CommandName: "gemini", BinPath: "bin/index.js",
		VersionArgs:           []string{"--version"},
		SupportsInstall:       true,
		PermissionModes:       []string{"read_only"},
		DefaultPermissionMode: "read_only",
	})

	if !validProfileAgent("gemini-cli") {
		t.Fatal("validProfileAgent rejected an agent that exists in the catalog; it is not reading the catalog")
	}
	if !validAgentPolicy("gemini-cli", "read_only") {
		t.Fatal("validAgentPolicy rejected a mode declared by the catalog entry")
	}
	// 反向：目录里没有的仍然要被拒，否则上面那条就只是"永远返回 true"。
	if validProfileAgent("no-such-agent") {
		t.Fatal("validProfileAgent accepted an agent absent from the catalog")
	}
	if validAgentPolicy("no-such-agent", "read_only") {
		t.Fatal("validAgentPolicy accepted an agent absent from the catalog")
	}
}

// TestSupportedAgentIDsTracksCatalog 断言名单本身也来自目录（而不是另一处硬编码）。
func TestSupportedAgentIDsTracksCatalog(t *testing.T) {
	withExtraCatalogEntry(t, AgentCatalogEntry{
		ID: "aider", Name: "Aider", Vendor: "Aider",
		InstallKind: InstallKindNpmGlobal, NpmPackage: "aider-chat",
		CommandName: "aider", BinPath: "bin/aider.js",
		PermissionModes:       []string{"read_only"},
		DefaultPermissionMode: "read_only",
	})

	found := false
	for _, id := range supportedAgentIDs() {
		if id == "aider" {
			found = true
		}
	}
	if !found {
		t.Fatal("supportedAgentIDs did not include an agent present in the catalog")
	}
	if len(supportedAgentIDs()) != len(agentCatalog()) {
		t.Fatal("supportedAgentIDs and agentCatalog disagree on the tool count")
	}
}

// TestAgentDisplayNameDoesNotFallBackToAnotherTool 防的是"未知工具被显示成 Claude Code"。
//
// 前端那些 `agentID === "codex" ? "Codex" : "Claude Code"` 的二元三元式就是这么错的：
// 任何不认识的值都会落到 Claude。这里要求 Go 侧不重复这个错。
func TestAgentDisplayNameDoesNotFallBackToAnotherTool(t *testing.T) {
	if got := agentDisplayName("codex"); got != "Codex" {
		t.Fatalf("agentDisplayName(codex) = %q, want %q", got, "Codex")
	}
	if got := agentDisplayName("who-knows"); got != "who-knows" {
		t.Fatalf("unknown agent was given a display name %q; it must echo the id instead of claiming another tool", got)
	}
}

// TestCatalogEntriesAreSelfConsistent 是目录自身的不变量。
//
// 新增工具时最容易漏的是"权限模式列表与默认值不同步"（默认值不在列表里），
// 那种错会表现为"新工具建会话时被服务端拒绝"，且报错完全指不到目录。
func TestCatalogEntriesAreSelfConsistent(t *testing.T) {
	ids := map[string]bool{}
	for _, entry := range agentCatalog() {
		if entry.ID == "" || entry.Name == "" || entry.Vendor == "" {
			t.Fatalf("catalog entry %+v is missing id/name/vendor", entry)
		}
		if ids[entry.ID] {
			t.Fatalf("duplicate catalog id %q", entry.ID)
		}
		ids[entry.ID] = true

		if entry.InstallKind == "" {
			t.Fatalf("%s: installKind is empty", entry.ID)
		}
		if entry.InstallKind == InstallKindNpmGlobal {
			if entry.NpmPackage == "" || entry.CommandName == "" || entry.BinPath == "" {
				t.Fatalf("%s: npm-global entry needs npmPackage/commandName/binPath", entry.ID)
			}
			if len(entry.VersionArgs) == 0 {
				t.Fatalf("%s: npm-global entry needs versionArgs to probe its version", entry.ID)
			}
		}
		if len(entry.PermissionModes) == 0 {
			t.Fatalf("%s: permissionModes is empty", entry.ID)
		}
		// 就绪判据必须二选一且写明确：漏填会让 probeAgent 走进默认分支，
		// 而"默认按版本判"对只该查二进制的工具（Codex）是错的，且不会报错。
		if entry.Readiness != readinessVersion && entry.Readiness != readinessBinary {
			t.Fatalf("%s: readiness = %q，必须是 %q 或 %q", entry.ID, entry.Readiness, readinessVersion, readinessBinary)
		}
		if !validAgentPolicy(entry.ID, entry.DefaultPermissionMode) {
			t.Fatalf("%s: defaultPermissionMode %q is not among permissionModes %v", entry.ID, entry.DefaultPermissionMode, entry.PermissionModes)
		}
		if entry.SupportsProjectInsights && !entry.RunnableInProject {
			t.Fatalf("%s: insights-capable agent must be runnable in a project", entry.ID)
		}
		if entry.SupportsInstall && entry.MinRuntimeVersion == "" {
			t.Fatalf("%s: supportsInstall without minRuntimeVersion leaves no way to gate an unusable install", entry.ID)
		}
		if entry.SupportsInstall && len(entry.UpdateArgs) == 0 {
			// 跨端升级用目录里的子命令（Claude/Codex 是 update、OpenCode 是 upgrade）。
			// 漏填会让跨端升级去跑裸命令，而裸命令通常只打印帮助并成功退出。
			t.Fatalf("%s: supportsInstall without updateArgs makes cross-end updates run the bare command", entry.ID)
		}
		// 不可安装的工具不能声明运行时依赖，否则界面会提示"先装 Node"却给不出动作。
		if !entry.SupportsInstall && len(entry.Requires) > 0 {
			t.Fatalf("%s: declares requires but does not support install", entry.ID)
		}
	}
	if len(ids) == 0 {
		t.Fatal("agent catalog is empty")
	}
}

func TestInsightAgentSupportIsExplicit(t *testing.T) {
	for _, agentID := range []string{"claude-code", "codex", "opencode"} {
		entry, ok := agentByID(agentID)
		if !ok || !entry.SupportsProjectInsights {
			t.Fatalf("%s must be explicitly enabled for optimization insights", agentID)
		}
	}
	for _, agentID := range []string{"codebuddy", "qoder", "cursor"} {
		entry, ok := agentByID(agentID)
		if !ok || entry.SupportsProjectInsights {
			t.Fatalf("%s must stay out of optimization insights until its read-only analysis protocol is implemented", agentID)
		}
	}
}

// TestCatalogJSONCarriesNoSecretFields 是目录的对外形状断言：它会被 GET /api/agents
// 整份发给前端，所以字段一旦混进内部信息（路径、命令行、环境变量名）就是泄漏面。
func TestCatalogJSONCarriesNoSecretFields(t *testing.T) {
	raw, err := json.Marshal(agentCatalog())
	if err != nil {
		t.Fatalf("marshal catalog: %v", err)
	}
	payload := string(raw)
	for _, forbidden := range []string{"baseUrl", "apiKey", "secret", "token", "env"} {
		if strings.Contains(payload, forbidden) {
			t.Fatalf("catalog JSON exposes %q: %s", forbidden, payload)
		}
	}
	for _, required := range []string{`"id"`, `"name"`, `"vendor"`, `"permissionModes"`, `"minRuntimeVersion"`} {
		if !strings.Contains(payload, required) {
			t.Fatalf("catalog JSON is missing %s: %s", required, payload)
		}
	}
}

// TestPermissionModesCoversEveryCatalogAgent 断言偏好设置里的派生视图覆盖目录里的
// 每个工具 —— 包括还没有专属历史列的工具（那一档必须回落到目录声明的默认值）。
func TestPermissionModesCoversEveryCatalogAgent(t *testing.T) {
	withExtraCatalogEntry(t, AgentCatalogEntry{
		ID: "brand-new", Name: "Brand New", Vendor: "Nobody",
		InstallKind:           InstallKindNpmGlobal,
		NpmPackage:            "brand-new",
		CommandName:           "brand-new",
		BinPath:               "bin/index.js",
		VersionArgs:           []string{"--version"},
		PermissionModes:       []string{"read_only"},
		DefaultPermissionMode: "read_only",
	})

	modes := defaultAppPreferences().agentPermissionModes()
	if modes["brand-new"] != "read_only" {
		t.Fatalf("agent without a legacy column got %q, want the catalog default %q", modes["brand-new"], "read_only")
	}
	if modes["claude-code"] != defaultClaudePermission || modes["codex"] != defaultCodexPermission {
		t.Fatalf("legacy columns did not reach the derived view: %v", modes)
	}
}

// withExtraCatalogEntry 临时给目录追加一条，并在用例结束时还原。
//
// 目录是包级变量，而"证明某函数读的是目录"只能靠改目录看行为是否跟着变。
// 还原写在 Cleanup 里，保证失败路径也回滚（否则会污染同包其它用例）。
func withExtraCatalogEntry(t *testing.T, entry AgentCatalogEntry) {
	t.Helper()
	original := agentCatalogEntries
	agentCatalogEntries = append(append([]AgentCatalogEntry{}, original...), entry)
	t.Cleanup(func() { agentCatalogEntries = original })
}

// TestCatalogBinPathIsTheNpmBinTarget 钉住一条**只在最末端才暴露**的约定。
//
// BinPath 的消费方式是被拼到包根之后：npmCLIInstall.binaryPath =
// `<prefix>/lib/node_modules/<scope>/<pkg>/<BinPath>`。所以它必须**逐字等于**
// `npm view <pkg> bin` 里那一条目标 —— 少一层、多一层都是不存在的路径。
//
// 历史教训有两条：2026-10-09 codebuddy 照抄完整目标 `bin/codebuddy`，而当时字段语义是
// "bin/ 下的文件名"，于是拼成 `<包根>/bin/bin/codebuddy`；2026-10-21 接 qoder 时它的 bin
// 目标在 `bundle/`，旧语义根本表达不出来。
//
// 它不在安装路径上暴露（那条自检用的是 `$prefix/bin/<命令名>`），只在跨端升级的
// 「确认来源」与「回滚」上用（cross_npm_update.go / ssh_runner.go）——症状是"升级失败
// 时回滚不了"，所以特别需要一条断言把它钉住。
func TestCatalogBinPathIsTheNpmBinTarget(t *testing.T) {
	// npm 上**实测**的 bin 目标（`npm view <pkg> bin`）。新增工具时必须先查一次并记进
	// 这张表 —— 缺一条就红，是为了让"照抄 `bin/X`"这个错在下一次也犯不出来。
	npmBinTargets := map[string]string{
		"claude-code": "bin/claude.exe",
		"codex":       "bin/codex.js",
		"codebuddy":   "./bin/codebuddy",
		"qoder":       "bundle/qoder-npm-dispatcher.cjs",
		"opencode":    "bin/opencode.exe",
	}
	for _, entry := range agentCatalog() {
		if entry.InstallKind != InstallKindNpmGlobal {
			continue
		}
		target, known := npmBinTargets[entry.ID]
		if !known {
			t.Fatalf("%s: 先跑 `npm view %s bin` 并把结果记进这张表", entry.ID, entry.NpmPackage)
		}
		// 比之前先 Clean：npm 对 codebuddy 报的是 `./bin/codebuddy`，落点本身等价。
		if path.Clean(entry.BinPath) != path.Clean(target) {
			t.Fatalf("%s: BinPath = %q，want %q（npm bin 目标原样）", entry.ID, entry.BinPath, target)
		}
	}
}

// TestListAgentsEndpointExposesQoder 是"接口层真的把它交出去"的断言。
//
// 目录里登记了、handler 却没发出去，是这类接入最典型的静默失败：编译通过、目录测试
// 也通过，前端却永远看不到这个工具。这里走真实路由，只断言对外可观察的东西。
func TestListAgentsEndpointExposesQoder(t *testing.T) {
	server := newTestServer(t)
	recorder := httptest.NewRecorder()
	server.routes().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/agents", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/agents 返回 %d：%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	// 落点与内部路径是**不许**出现在这份响应里的（它是给前端的目录，不是诊断读数）。
	for _, forbidden := range []string{"binPath", "qoder-npm-dispatcher", "binFile"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("目录响应泄漏了内部字段 %q：%s", forbidden, body)
		}
	}
	var entries []AgentCatalogEntry
	if err := json.Unmarshal([]byte(body), &entries); err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	var qoder *AgentCatalogEntry
	for index := range entries {
		if entries[index].ID == "qoder" {
			qoder = &entries[index]
		}
	}
	if qoder == nil {
		t.Fatalf("GET /api/agents 没有 qoder：%s", body)
	}
	if qoder.Name != "Qoder CLI" || qoder.CommandName != "qoder" || qoder.NpmPackage != "@qoder-ai/qodercli" {
		t.Fatalf("qoder 条目的名字/命令/包名不对：%+v", *qoder)
	}
	if !qoder.SupportsInstall || !qoder.SupportsLogin || !qoder.RunnableInProject {
		t.Fatalf("qoder 能力开关不完整（安装/登录/可运行都要为 true）：%+v", *qoder)
	}
	if qoder.SupportsProjectInsights {
		t.Fatal("洞察协议还没接，目录不能声称支持")
	}
}
