package app

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestParseSkillFrontmatter(t *testing.T) {
	cases := []struct {
		name        string
		content     string
		wantName    string
		wantDesc    string
	}{
		{
			name: "normal",
			content: `---
name: frontend-design
description: Create distinctive interfaces. Use this skill when building web components.
license: Complete terms in LICENSE.txt
---

This skill guides creation of frontend interfaces.
`,
			wantName: "frontend-design",
			wantDesc: "Create distinctive interfaces. Use this skill when building web components.",
		},
		{
			name: "multiline description with double quotes",
			content: "---\nname: \"audit\"\ndescription: \"Audit and improve CLAUDE.md files.\"\n---\n",
			wantName: "audit",
			wantDesc: "Audit and improve CLAUDE.md files.",
		},
		{
			name: "nested block ignored",
			content: "---\ntool:\n  name: bash\n  description: run shell\nname: bash-runner\ndescription: Run shell commands\n---\n",
			// 嵌套块后的顶层 name/description 仍需正确取到；嵌套块内 description 不应污染。
			wantName: "bash-runner",
			wantDesc: "Run shell commands",
		},
		{
			name:     "no frontmatter",
			content:  "# Just a header\nno frontmatter here\n",
			wantName: "",
			wantDesc: "",
		},
		{
			name:     "empty frontmatter, desc missing",
			content:  "---\nname: foo\n---\n",
			wantName: "foo",
			wantDesc: "",
		},
		{
			name: "unknown key before name",
			content: "---\nmodel: claude-4\nname: zed\ndescription: whatever\n---\n",
			wantName: "zed",
			wantDesc: "whatever",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotName, gotDesc := parseSkillFrontmatter(c.content)
			if gotName != c.wantName {
				t.Errorf("name: got %q want %q", gotName, c.wantName)
			}
			if gotDesc != c.wantDesc {
				t.Errorf("description: got %q want %q", gotDesc, c.wantDesc)
			}
		})
	}
}

// makeSkillTree 在 base 下造一个 <base>/<dir>/<name>/SKILL.md，写入合法 frontmatter。
func makeSkillTree(t *testing.T, base, parentSource, name, desc string, plugins bool) {
	t.Helper()
	dir := filepath.Join(base, parentSource)
	if plugins {
		dir = filepath.Join(base, "plugins", "marketplaces", "org", "external_plugins", "someplugin", "skills")
		// 再塞一个 .git，验证被跳过
		if err := os.MkdirAll(filepath.Join(base, "plugins", "marketplaces", "org", ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dir = filepath.Join(dir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: " + desc + "\n---\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanAndResolveLocalSkills(t *testing.T) {
	base := t.TempDir()
	proj := filepath.Join(base, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}

	// 用户级：claude 一个、codex 一个。
	makeSkillTree(t, base, "user/.claude/skills", "user-skill", "Uses the user skill", false)
	makeSkillTree(t, base, "user/.codex/skills", "codex-user", "A codex user skill", false)
	// 项目级 claude，与用户级同名，应覆盖。
	makeSkillTree(t, proj, ".claude/skills", "user-skill", "PROJECT version of user-skill", false)
	makeSkillTree(t, proj, ".claude/skills", "proj-skill", "A project skill", false)
	// 插件树。
	makeSkillTree(t, base, "", "plugin-skill", "A plugin skill", true)

	roots := []skillScanRoot{}
	add := func(abs, agent, source string, plugin bool, prio int) {
		roots = append(roots, skillScanRoot{absPath: abs, agent: agent, source: source, env: "windows", pluginRoot: plugin, priority: prio})
	}
	// 优先级：user(低) < plugin(中) < project(高)。
	add(filepath.Join(base, "user/.claude/skills"), skillAgentClaude, skillSourceUser, false, 0)
	add(filepath.Join(base, "user/.codex/skills"), skillAgentCodex, skillSourceUser, false, 1)
	add(filepath.Join(base, "plugins"), skillAgentClaude, skillSourcePlugin, true, 2)
	add(filepath.Join(proj, ".claude/skills"), skillAgentClaude, skillSourceProject, false, 3)
	add(filepath.Join(proj, ".codex/skills"), skillAgentCodex, skillSourceProject, false, 4)

	scans := scanLocalSkills(context.Background(), roots)
	skills := resolveSkillScans(scans)

	// 断言：user-skill 项目版胜出；proj-skill、plugin-skill、codex-user 均在；plugin 不出现。
	byName := map[string]Skill{}
	for _, sk := range skills {
		byName[sk.Name] = sk
	}
	if got := byName["user-skill"]; got.Source != skillSourceProject || got.Description != "PROJECT version of user-skill" {
		t.Errorf("user-skill: got %+v want project source with project desc", got)
	}
	if sk, ok := byName["proj-skill"]; !ok || sk.Source != skillSourceProject {
		t.Errorf("proj-skill missing or wrong source: %+v", sk)
	}
	if sk, ok := byName["plugin-skill"]; !ok || sk.Source != skillSourcePlugin || sk.Agent != skillAgentClaude {
		t.Errorf("plugin-skill missing or wrong: %+v", sk)
	}
	if sk, ok := byName["codex-user"]; !ok || sk.Agent != skillAgentCodex || sk.Source != skillSourceUser {
		t.Errorf("codex-user missing or wrong: %+v", sk)
	}
	// 插件树不应出现 .git 目录名。
	for _, sk := range skills {
		if sk.Name == ".git" {
			t.Fatalf("plugin tree leaked .git as a skill: %+v", sk)
		}
	}
}

// TestDiscoverLocalSkillRootsWSL 验证 WSL 分支装配的扫描根：
// 有 wslDistro/wslHome 时包含 用户级(user) + 插件树(plugin) + 项目级(project)，
// 顺序与优先级与 windows 分支语义一致（user < plugin < project）；无 wslHome 时退化为仅项目级。
func TestDiscoverLocalSkillRootsWSL(t *testing.T) {
	// 本用例硬编码反斜杠 UNC 期望（filepath.Join 在 Windows 下保留 \\wsl$\ 前缀），
	// 仅 Windows 可跑；Linux 上 Join 用 / 会得到混合分隔符，导致断言失败。
	if runtime.GOOS != "windows" {
		t.Skip("仅验证 Windows 本机的 WSL UNC 技能根装配")
	}
	proj := `\\wsl$\Ubuntu\home\tangmaoke\work\app`
	assertRoots := func(t *testing.T, roots []skillScanRoot, want []string) {
		t.Helper()
		// 只比对 absPath，其它字段在每个分支断言。
		if len(roots) != len(want) {
			t.Fatalf("root count: got %d %v, want %d (user+plugin+project)", len(roots), roots, len(want))
		}
		for i, r := range roots {
			if !strings.EqualFold(r.absPath, want[i]) {
				t.Errorf("root[%d].absPath = %q, want %q", i, r.absPath, want[i])
			}
		}
	}

	// 有 WSL home：用户级 + 插件 + 项目级。
	s := &Server{wslDistro: "Ubuntu", wslHome: "/home/tangmaoke"}
	roots := s.discoverLocalSkillRoots(agentTargetEnvWSL, proj)
	assertRoots(t, roots, []string{
		`\\wsl$\Ubuntu\home\tangmaoke\.claude\skills`,
		`\\wsl$\Ubuntu\home\tangmaoke\.codex\skills`,
		`\\wsl$\Ubuntu\home\tangmaoke\.claude\plugins`,
		`\\wsl$\Ubuntu\home\tangmaoke\work\app\.claude\skills`,
		`\\wsl$\Ubuntu\home\tangmaoke\work\app\.codex\skills`,
	})
	// 来源 / 优先级 / 环境逐项校验。
	type wantRoot struct {
		source string
		agent  string
		plugin bool
	}
	wants := []wantRoot{
		{skillSourceUser, skillAgentClaude, false},
		{skillSourceUser, skillAgentCodex, false},
		{skillSourcePlugin, skillAgentClaude, true},
		{skillSourceProject, skillAgentClaude, false},
		{skillSourceProject, skillAgentCodex, false},
	}
	for i, r := range roots {
		if r.source != wants[i].source || r.agent != wants[i].agent || r.pluginRoot != wants[i].plugin || r.env != "wsl" {
			t.Errorf("root[%d] meta wrong: got source=%q agent=%q plugin=%v env=%q, want source=%q agent=%q plugin=%v env=wsl",
				i, r.source, r.agent, r.pluginRoot, r.env, wants[i].source, wants[i].agent, wants[i].plugin)
		}
	}
	// 优先级严格递增，保证同名去重 project 胜出。
	for i := 1; i < len(roots); i++ {
		if roots[i].priority <= roots[i-1].priority {
			t.Errorf("priority not increasing at %d: %d <= %d", i, roots[i].priority, roots[i-1].priority)
		}
	}

	// 无 WSL home：退化为仅项目级（回归现状）。
	s0 := &Server{}
	roots0 := s0.discoverLocalSkillRoots(agentTargetEnvWSL, proj)
	if len(roots0) != 2 || roots0[0].source != skillSourceProject || roots0[1].source != skillSourceProject {
		t.Fatalf("no-WSL-home roots: got %+v, want only project roots", roots0)
	}
}

func TestFilterSkillsByAgent(t *testing.T) {
	all := []Skill{
		{Name: "a", Agent: skillAgentClaude},
		{Name: "b", Agent: skillAgentCodex},
		{Name: "c", Agent: skillAgentClaude},
	}
	got := filterSkillsByAgent(all, skillAgentClaude)
	want := []Skill{{Name: "a", Agent: skillAgentClaude}, {Name: "c", Agent: skillAgentClaude}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("filter: got %+v want %+v", got, want)
	}
	if n := len(filterSkillsByAgent(all, "")); n != 3 {
		t.Errorf("empty agent filter should pass all, got %d", n)
	}
}

func TestParseRemoteSkillOutput(t *testing.T) {
	marker := remoteSkillOutputMarker
	home := "/home/usr"
	proj := "/home/usr/work/app"
	output := "" +
		marker + "/home/usr/.claude/skills/alpha/SKILL.md\n---\nname: alpha\ndescription: Alpha skill\n---\nbody\n" +
		marker + "/home/usr/work/app/.codex/skills/beta/SKILL.md\n---\nname: beta\ndescription: Beta codex skill\n---\n"
	skills := parseRemoteSkillOutput(output, home, proj, "")
	byName := map[string]Skill{}
	for _, sk := range skills {
		byName[sk.Name] = sk
	}
	if sk, ok := byName["alpha"]; !ok || sk.Agent != skillAgentClaude || sk.Source != skillSourceUser || sk.Env != "remote-linux" {
		t.Errorf("alpha: %+v", sk)
	}
	if sk, ok := byName["beta"]; !ok || sk.Agent != skillAgentCodex || sk.Source != skillSourceProject {
		t.Errorf("beta: %+v", sk)
	}
	// 按 agentID 过滤。
	codexOnly := parseRemoteSkillOutput(output, home, proj, skillAgentCodex)
	if len(codexOnly) != 1 || codexOnly[0].Name != "beta" {
		t.Errorf("codex-only filter: %+v", codexOnly)
	}
}

// TestParseRemoteSkillOutputProjectWins 验证同名技能"后者覆盖前者"：
// 远端命令顺序 user → plugin → project，故 user 级 alpha 应被 project 级 alpha 覆盖（source=project）。
func TestParseRemoteSkillOutputProjectWins(t *testing.T) {
	home := "/home/u"
	proj := "/home/u/work/app"
	marker := remoteSkillOutputMarker
	output := "" +
		marker + "/home/u/.claude/skills/alpha/SKILL.md\n---\nname: alpha\ndescription: USER version\n---\n" +
		marker + "/home/u/.claude/plugins/p/skills/alpha/SKILL.md\n---\nname: alpha\ndescription: PLUGIN version\n---\n" +
		marker + "/home/u/work/app/.claude/skills/alpha/SKILL.md\n---\nname: alpha\ndescription: PROJECT version\n---\n"
	skills := parseRemoteSkillOutput(output, home, proj, "")
	var alpha *Skill
	for i := range skills {
		if skills[i].Name == "alpha" {
			alpha = &skills[i]
		}
	}
	if alpha == nil {
		t.Fatal("alpha skill missing")
	}
	if alpha.Source != skillSourceProject || alpha.Description != "PROJECT version" {
		t.Errorf("expected project to win, got source=%q desc=%q", alpha.Source, alpha.Description)
	}
}

func TestSkillAgentAndSourceForRemotePath(t *testing.T) {
	home := "/home/u"
	proj := "/home/u/work/app"
	if got := skillAgentForRemotePath("/home/u/.codex/skills/x/SKILL.md"); got != skillAgentCodex {
		t.Errorf("codex path agent: got %q", got)
	}
	if got := skillAgentForRemotePath("/home/u/.claude/plugins/p/skills/x/SKILL.md"); got != skillAgentClaude {
		t.Errorf("plugin path agent: got %q", got)
	}
	if got := skillSourceForRemotePath("/home/u/.claude/plugins/p/skills/x/SKILL.md", home, proj); got != skillSourcePlugin {
		t.Errorf("plugin source: got %q", got)
	}
	if got := skillSourceForRemotePath(proj+"/.claude/skills/x/SKILL.md", home, proj); got != skillSourceProject {
		t.Errorf("project source: got %q", got)
	}
	if got := skillSourceForRemotePath(home+"/.claude/skills/x/SKILL.md", home, proj); got != skillSourceUser {
		t.Errorf("user source: got %q", got)
	}
}

// TestDiscoverSkillsForProjectLocalWindows 验证本地 windows 分支 + agentId 过滤，
// 走完整的 discoverSkillsForProject（resolveAgentTargetEnv -> 扫描 -> 去重 -> 过滤）。
// 用零值 Server：本地路径不触 DB，也不触 runnerRegistry。
func TestDiscoverSkillsForProjectLocalWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("仅验证 windows 本机分支")
	}
	proj := t.TempDir()
	// 项目内同时放 claude 与 codex 的项目级技能。
	makeSkillTree(t, proj, ".claude/skills", "claude-proj", "A claude project skill", false)
	makeSkillTree(t, proj, ".codex/skills", "codex-proj", "A codex project skill", false)

	s := &Server{}
	p := Project{Runner: "windows-local", Path: proj}

	// 本地 windows 分支会合并真实用户/插件技能 + 项目级技能，因此不断言精确数量，
	// 只断言：项目级 claude 技能存在、agentId 过滤正确（claude 结果不含 codex 技能）。
	claudeSkills := s.discoverSkillsForProject(context.Background(), p, skillAgentClaude)
	byName := map[string]Skill{}
	for _, sk := range claudeSkills {
		byName[sk.Name] = sk
	}
	owned, ok := byName["claude-proj"]
	if !ok || owned.Agent != skillAgentClaude || owned.Source != skillSourceProject || owned.Env != "windows" {
		t.Fatalf("claude project skill missing/wrong: %+v", owned)
	}
	for name, sk := range byName {
		if sk.Agent != skillAgentClaude {
			t.Errorf("claude-agent query returned non-claude skill %q (%s)", name, sk.Agent)
		}
	}
	if _, has := byName["codex-proj"]; has {
		t.Errorf("claude-agent query must not include codex project skill")
	}

	codexSkills := s.discoverSkillsForProject(context.Background(), p, skillAgentCodex)
	byNameCodex := map[string]Skill{}
	for _, sk := range codexSkills {
		byNameCodex[sk.Name] = sk
	}
	if owned, ok := byNameCodex["codex-proj"]; !ok || owned.Agent != skillAgentCodex {
		t.Fatalf("codex project skill missing/wrong: %+v", owned)
	}
	for name, sk := range byNameCodex {
		if sk.Agent != skillAgentCodex {
			t.Errorf("codex-agent query returned non-codex skill %q (%s)", name, sk.Agent)
		}
	}

	all := s.discoverSkillsForProject(context.Background(), p, "")
	byNameAll := map[string]Skill{}
	for _, sk := range all {
		byNameAll[sk.Name] = sk
	}
	if _, ok := byNameAll["claude-proj"]; !ok {
		t.Errorf("empty agent query should include claude project skill")
	}
	if _, ok := byNameAll["codex-proj"]; !ok {
		t.Errorf("empty agent query should include codex project skill")
	}
}
