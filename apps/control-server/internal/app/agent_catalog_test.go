package app

import (
	"encoding/json"
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
		CommandName: "gemini", BinFile: "index.js",
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
		CommandName: "aider", BinFile: "aider.js",
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
			if entry.NpmPackage == "" || entry.CommandName == "" || entry.BinFile == "" {
				t.Fatalf("%s: npm-global entry needs npmPackage/commandName/binFile", entry.ID)
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
		if entry.SupportsInstall && entry.MinRuntimeVersion == "" {
			t.Fatalf("%s: supportsInstall without minRuntimeVersion leaves no way to gate an unusable install", entry.ID)
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
		BinFile:               "index.js",
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
