package app

import "path/filepath"

// Native directories and documented compatibility directories. Keep one table
// for local, WSL and SSH discovery; project roots override user/plugin roots.
type skillDirectory struct {
	agent, user, project string
}

var skillDirectories = []skillDirectory{
	{"claude-code", ".claude/skills", ".claude/skills"},
	{"codex", ".codex/skills", ".codex/skills"},
	{"codex", ".agents/skills", ".agents/skills"},
	{"codebuddy", ".codebuddy/skills", ".codebuddy/skills"},
	{"qoder", ".qoder/skills", ".qoder/skills"},
	{"opencode", ".claude/skills", ".claude/skills"},
	{"opencode", ".agents/skills", ".agents/skills"},
	{"opencode", ".config/opencode/skills", ".opencode/skills"},
	{"cursor", ".claude/skills", ".claude/skills"},
	{"cursor", ".agents/skills", ".agents/skills"},
	{"cursor", ".cursor/skills", ".cursor/skills"},
}

func skillRootsForPaths(home, projectPath, env, codexSkills, openCodeConfig string) []skillScanRoot {
	var roots []skillScanRoot
	add := func(p, agent, source string, plugin bool) {
		if p == "" {
			return
		}
		roots = append(roots, skillScanRoot{absPath: p, agent: agent, source: source, env: env, pluginRoot: plugin, recursive: agent == skillAgentCodex, priority: len(roots)})
	}
	if home != "" {
		for _, dir := range skillDirectories {
			p := filepath.Join(home, filepath.FromSlash(dir.user))
			if dir.agent == "codex" && dir.user == ".codex/skills" && codexSkills != "" {
				p = codexSkills
			}
			if dir.agent == "opencode" && dir.user == ".config/opencode/skills" && openCodeConfig != "" {
				p = filepath.Join(openCodeConfig, "opencode", "skills")
			}
			add(p, dir.agent, skillSourceUser, false)
		}
		for _, dir := range pluginDirectories {
			add(filepath.Join(home, dir.root), dir.agent, skillSourcePlugin, true)
		}
	}
	if projectPath != "" {
		for _, dir := range skillDirectories {
			add(filepath.Join(projectPath, filepath.FromSlash(dir.project)), dir.agent, skillSourceProject, false)
		}
	}
	return roots
}

var pluginDirectories = []struct{ agent, root, manifest string }{
	{"claude-code", ".claude/plugins", ".claude-plugin/plugin.json"},
	{"codex", ".codex/plugins", ".codex-plugin/plugin.json"},
	{"codebuddy", ".codebuddy/plugins", ".codebuddy-plugin/plugin.json"},
	{"qoder", ".qoder/plugins", ".qoder-plugin/plugin.json"},
	{"cursor", ".cursor/plugins", ".cursor-plugin/plugin.json"},
}
