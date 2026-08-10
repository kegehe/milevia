package app

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"
)

// Skill 描述一处已安装的 Claude Code / Codex 技能，供对话页「Skill」区域展示与引用。
// 发现以文件系统扫描为唯一真实来源（不依赖 CLI 子命令，见 docs/22）。
type Skill struct {
	// Name 是技能的标识符（frontmatter name 或目录名），如 "frontend-design"。
	Name string `json:"name"`
	// Description 是 frontmatter 的 description，用于预览/悬浮提示；缺失时回退为目录名。
	Description string `json:"description"`
	// Agent 标识技能所属 CLI："claude-code" 或 "codex"。
	Agent string `json:"agent"`
	// Env 标识技能运行的环境："windows"、"wsl" 或 "remote-linux"。
	Env string `json:"env"`
	// Source 标识技能来源："user"、"project" 或 "plugin"。
	Source string `json:"source"`
	// BackgroundSource 可选来源描述（如 "claude-plugins-official/external_plugins/discord"）。
	BackgroundSource string `json:"backgroundSource,omitempty"`
}

const (
	skillAgentClaude        = "claude-code"
	skillAgentCodex         = "codex"
	skillSourceUser         = "user"
	skillSourceProject      = "project"
	skillSourcePlugin       = "plugin"
	skillFile               = "SKILL.md"
	skillFrontmatterDelim   = "---"
	// remoteSkillOutputMarker 是远端 find+cat 输出中，分隔「路径」与「下一文件」的 ASCII 哨兵。
	// 用长 ASCII 串（而非 NUL 字节）以兼容远端 /bin/sh（dash/busybox）对 printf 的处理；
	// 该串几乎不可能出现在路径或 SKILL.md 正文中。
	remoteSkillOutputMarker = "###MILEVIA-SKILL-DELIM###"
)

// skillScanRoot 描述一次本地目录扫描：在哪、扫哪个 CLI、来源、环境与优先级。
type skillScanRoot struct {
	absPath string // 包含 <name>/SKILL.md 的直接父目录（skills 根），或插件树根
	agent   string // 该 root 下的技能所属 CLI
	source  string // "user"、"project" 或 "plugin"
	env     string // "windows"、"wsl" 或 "remote-linux"
	// pluginRoot 为 true 表示 absPath 是 ~/.claude/plugins 树，需递归进入名为 skills 的目录。
	pluginRoot bool
	// priority 用于同名去重：数值越大越优先（project > plugin > user）。
	priority int
}

// skillScan 记录扫描到的 skill 与其来源，用于解析 frontmatter 后输出 Skill。
type skillScan struct {
	skillFile string // SKILL.md 的绝对路径
	nameHint  string // 目录名（description/name 缺失时的回退）
	root      skillScanRoot
}

// parseSkillFrontmatter 解析 SKILL.md 首对 --- 之间的 YAML，仅取 name/description。
// 手写轻量解析，不引入 yaml 依赖：`key: value` 拆键值并去引号；
// 遇到 `key:`（多行标量 / 嵌套块）跳过后续缩进行；忽略一切陌生键；不因未知键而失败。
func parseSkillFrontmatter(content string) (name, description string) {
	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 64*1024), 64*1024)

	first := true
	inNested := false
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if first {
			if trimmed != skillFrontmatterDelim {
				return "", "" // 首行不是 ---，无 frontmatter
			}
			first = false
			continue
		}
		if trimmed == skillFrontmatterDelim {
			break // frontmatter 结束
		}
		if trimmed == "" {
			continue
		}
		if line != trimmed && inNested {
			continue // 嵌套块内的缩进行：忽略
		}
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		key := strings.TrimSpace(line[:colon])
		value := strings.TrimSpace(line[colon+1:])
		if value == "" {
			inNested = true // 空值 / 嵌套块开始
			continue
		}
		inNested = false
		value = strings.Trim(value, `"'`)
		switch key {
		case "name":
			if name == "" {
				name = value
			}
		case "description":
			if description == "" {
				description = value
			}
		}
	}
	return name, description
}

// discoverLocalSkillRoots 依据目标环境返回需要本地扫描的根目录列表（含来源与优先级）。
// windows 环境：用户级 + 项目级 + 插件树均在本机可直读；wsl 环境：项目级外，
// 也把 WSL 用户级与插件树按 UNC 纳入（os.ReadDir/os.Stat 对 UNC 可靠，仅 EvalSymlinks 不可靠）。
func (s *Server) discoverLocalSkillRoots(target agentTargetEnv, projectPath string) []skillScanRoot {
	env := string(target)
	roots := []skillScanRoot{}
	add := func(abs string, agent, source string, pluginRoot bool) {
		if abs == "" {
			return
		}
		roots = append(roots, skillScanRoot{
			absPath: abs, agent: agent, source: source, env: env, pluginRoot: pluginRoot,
			// 优先级递增装配：后 add 的（project）数值更大，同名去重时胜出。
			priority: len(roots),
		})
	}

	if target == agentTargetEnvWSL {
		// WSL 用户级 + 插件树：把 WSL Linux home 转成可读的 UNC 后纳入扫描。
		// wslHome/wslDistro 未探测到（无 WSL）时跳过，退化为「仅项目级」。
		if s.wslHome != "" && s.wslDistro != "" {
			home := wslToUncPath(s.wslHome, s.wslDistro)
			add(filepath.Join(home, ".claude", "skills"), skillAgentClaude, skillSourceUser, false)
			add(filepath.Join(home, ".codex", "skills"), skillAgentCodex, skillSourceUser, false)
			add(filepath.Join(home, ".claude", "plugins"), skillAgentClaude, skillSourcePlugin, true)
		}
		add(filepath.Join(projectPath, ".claude", "skills"), skillAgentClaude, skillSourceProject, false)
		add(filepath.Join(projectPath, ".codex", "skills"), skillAgentCodex, skillSourceProject, false)
		return roots
	}

	// windows（本机）环境：用户级 + 项目级 + 插件树。
	add(filepath.Join(homeDir(), ".claude", "skills"), skillAgentClaude, skillSourceUser, false)
	add(filepath.Join(homeDir(), ".codex", "skills"), skillAgentCodex, skillSourceUser, false)
	add(filepath.Join(homeDir(), ".claude", "plugins"), skillAgentClaude, skillSourcePlugin, true)
	add(filepath.Join(projectPath, ".claude", "skills"), skillAgentClaude, skillSourceProject, false)
	add(filepath.Join(projectPath, ".codex", "skills"), skillAgentCodex, skillSourceProject, false)
	return roots
}

func homeDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return ""
}

// scanLocalSkills 对本地根目录执行扫描，返回原始技能记录（未解析 frontmatter）。
func scanLocalSkills(ctx context.Context, roots []skillScanRoot) []skillScan {
	var scans []skillScan
	for _, root := range roots {
		if root.pluginRoot {
			scans = append(scans, scanPluginTree(ctx, root)...)
			continue
		}
		entries, err := os.ReadDir(root.absPath)
		if err != nil {
			continue // 目录不存在或不可读：静默跳过
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			skillMD := filepath.Join(root.absPath, entry.Name(), skillFile)
			if st, err := os.Stat(skillMD); err != nil || st.IsDir() {
				continue
			}
			scans = append(scans, skillScan{skillFile: skillMD, nameHint: entry.Name(), root: root})
		}
	}
	return scans
}

// scanPluginTree 递归扫描插件树，进入所有名为 skills 的目录收集其下 <name>/SKILL.md。
// 跳过 .git / .hg / node_modules 等目录，避免误扫。
func scanPluginTree(ctx context.Context, root skillScanRoot) []skillScan {
	var scans []skillScan
	var walk func(dir string, inSkillsDir bool)
	walk = func(dir string, inSkillsDir bool) {
		select {
		case <-ctx.Done():
			return
		default:
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			name := entry.Name()
			if name == ".git" || name == ".hg" || name == "node_modules" {
				continue
			}
			child := filepath.Join(dir, name)
			childIsSkills := inSkillsDir || strings.EqualFold(name, "skills")
			if childIsSkills {
				// <skills>/<name>/SKILL.md（本特性关心的形态）
				if st, err := os.Stat(filepath.Join(child, skillFile)); err == nil && !st.IsDir() {
					scans = append(scans, skillScan{skillFile: filepath.Join(child, skillFile), nameHint: name, root: root})
					continue // skill 目录内不再下钻，避免把子技能目录也当顶层
				}
			}
			if childIsSkills {
				walk(child, true)
			} else {
				walk(child, false)
			}
		}
	}
	walk(root.absPath, false)
	return scans
}

// resolveSkillScans 解析 frontmatter、按优先级去重，输出按 agent 分组、名称排序的 Skill 列表。
func resolveSkillScans(scans []skillScan) []Skill {
	type parsed struct {
		name, desc string
		root       skillScanRoot
	}
	// 优先保留优先级高（数值大）的同 (agent,name) 记录。
	best := map[[2]string]parsed{}
	for _, sc := range scans {
		name, desc := parseSkillFrontmatter(readFileBestEffort(sc.skillFile))
		if name == "" {
			name = sc.nameHint
		}
		if desc == "" {
			desc = sc.nameHint
		}
		key := [2]string{sc.root.agent, name}
		cur := parsed{name: name, desc: desc, root: sc.root}
		if existing, ok := best[key]; !ok || cur.root.priority > existing.root.priority {
			best[key] = cur
		}
	}

	out := make([]Skill, 0, len(best))
	for key, p := range best {
		out = append(out, Skill{
			Name:        p.name,
			Description: p.desc,
			Agent:       key[0],
			Env:         p.root.env,
			Source:      p.root.source,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Agent != out[j].Agent {
			return out[i].Agent < out[j].Agent
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// discoverSkillsForProject 返回当前环境中可用的全部技能。失败时返回空列表（不硬失败）。
func (s *Server) discoverSkillsForProject(ctx context.Context, project Project, agentID string) []Skill {
	target := s.resolveAgentTargetEnv(project.Runner, project.Path)

	// SSH 远端：走 sshRunner 执行远程 find+cat。
	if target == agentTargetEnvRemote {
		return s.discoverSkillsRemote(ctx, project, agentID)
	}

	roots := s.discoverLocalSkillRoots(target, project.Path)
	scans := scanLocalSkills(ctx, roots)
	all := resolveSkillScans(scans)
	return filterSkillsByAgent(all, agentID)
}

// filterSkillsByAgent 按 agentID 过滤；空 agentID 表示不过滤。
func filterSkillsByAgent(skills []Skill, agentID string) []Skill {
	if agentID == "" {
		return skills
	}
	out := skills[:0]
	for _, sk := range skills {
		if sk.Agent == agentID {
			out = append(out, sk)
		}
	}
	return out
}

// listSkills 返回项目中当前环境可用的技能。`?agentId=` 可选过滤（claude-code / codex）。
// 实时扫描、不做缓存；目录缺失 / 远端未就绪时返回空列表，不报错。
func (s *Server) listSkills(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	project, err := s.getProjectByID(r.Context(), projectID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("项目不存在"))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	skills := s.discoverSkillsForProject(r.Context(), project, r.URL.Query().Get("agentId"))
	if skills == nil {
		skills = []Skill{}
	}
	writeJSON(w, http.StatusOK, skills)
}

// discoverSkillsRemote 在 SSH 远端执行 find+cat，将 frontmatter 拉回本机解析。
func (s *Server) discoverSkillsRemote(ctx context.Context, project Project, agentID string) []Skill {
	runner, ok := s.runnerRegistry.get(project.Runner)
	if !ok {
		return nil
	}
	sshR, ok := runner.(*sshRunner)
	if !ok {
		return nil
	}
	home := strings.TrimSpace(remoteHome(ctx, sshR))
	if home == "" {
		return nil
	}
	proj := filepath.ToSlash(project.Path)

	// 用户 + 项目两类根；按 agentID 定向裁剪命令。顺序与本地优先级一致：
	// user < plugin < project，故 project 排最后；parseRemoteSkillOutput 同名"后者覆盖前者"，
	// 使 project 级 over 插件 over 用户级，与 discoverLocalSkillRoots 的 priority 语义一致。
	var cmds []string
	if agentID == "" || agentID == skillAgentClaude {
		cmds = append(cmds,
			skillFindAndCat(shellQuotePresent(filepath.Join(home, ".claude", "skills"))),
			skillFindPlugins(shellQuotePresent(filepath.Join(home, ".claude", "plugins"))),
			skillFindAndCat(shellQuotePresent(filepath.Join(proj, ".claude", "skills"))),
		)
	}
	if agentID == "" || agentID == skillAgentCodex {
		cmds = append(cmds,
			skillFindAndCat(shellQuotePresent(filepath.Join(home, ".codex", "skills"))),
			skillFindAndCat(shellQuotePresent(filepath.Join(proj, ".codex", "skills"))),
		)
	}
	if len(cmds) == 0 {
		return nil
	}
	// execCommand 用 CombinedOutput：即使远端某个 find 因目录不存在返回非零退出码，
	// out 仍包含已捕获的输出（见 ssh_runner.execCommand）。这里忽略 err，避免目录缺失
	// 时把所有（可能有效的）技能一并丢弃。
	script := strings.Join(cmds, "\n")
	out, _ := sshR.client.execCommand(ctx, script)
	return parseRemoteSkillOutput(string(out), home, proj, agentID)
}

// remoteHome 查询远端 $HOME。
func remoteHome(ctx context.Context, sshR *sshRunner) string {
	out, err := sshR.client.execCommand(ctx, `printf '%s' "$HOME"`)
	if err != nil {
		return ""
	}
	return string(out)
}

// shellQuotePresent 对远端路径做 shellQuote；空路径返回空串。
func shellQuotePresent(p string) string {
	if p == "" {
		return ""
	}
	return shellQuote(p)
}

// skillFindAndCat 返回「find <root> -name SKILL.md 并回显 marker+路径+内容」的远端命令。
func skillFindAndCat(root string) string {
	if root == "" {
		return "true"
	}
	return `find ` + root + ` -type f -name SKILL.md -exec printf '` + remoteSkillOutputMarker + `%s\n' {} \; -exec cat {} \; 2>/dev/null`
}

// skillFindPlugins 返回插件树内 skills 目录的 find+cat 远端命令。
func skillFindPlugins(pluginRoot string) string {
	if pluginRoot == "" {
		return "true"
	}
	return `find ` + pluginRoot + ` -type d \( -name .git -o -name .hg -o -name node_modules \) -prune -o -type d -name skills -print 2>/dev/null | while IFS= read -r d; do find "$d" -type f -name SKILL.md -exec printf '` + remoteSkillOutputMarker + `%s\n' {} \; -exec cat {} \; 2>/dev/null; done`
}

// parseRemoteSkillOutput 解析远端 find+cat 的输出（每条以 marker+路径开头，随后是文件内容，
// 直到下一个 marker 或末尾），逐文件解析 frontmatter。
func parseRemoteSkillOutput(output, home, projectPath, agentID string) []Skill {
	// 远端命令顺序 user → plugin → project（见 discoverSkillsRemote），同名"后者覆盖前者"，
	// 使 project 优先级最高，与本地 discoverLocalSkillRoots 一致。
	skills := []Skill{}
	index := map[string]int{} // agent:name → skills 下标
	for _, block := range strings.Split(output, remoteSkillOutputMarker) {
		block = strings.TrimPrefix(block, "\n")
		if block == "" {
			continue
		}
		nl := strings.IndexByte(block, '\n')
		if nl < 0 {
			continue
		}
		filePath := strings.TrimSpace(block[:nl])
		content := block[nl+1:]
		if filePath == "" {
			continue
		}
		name, description := parseSkillFrontmatter(content)
		if name == "" {
			name = filepath.Base(filepath.Dir(filepath.ToSlash(filePath)))
		}
		if description == "" {
			description = name
		}
		agent := skillAgentForRemotePath(filePath)
		if agentID != "" && agent != agentID {
			continue
		}
		source := skillSourceForRemotePath(filePath, home, projectPath)
		key := agent + ":" + name
		skill := Skill{Name: name, Description: description, Agent: agent, Env: "remote-linux", Source: source}
		if i, ok := index[key]; ok {
			skills[i] = skill // 后者覆盖前者
			continue
		}
		index[key] = len(skills)
		skills = append(skills, skill)
	}
	sort.Slice(skills, func(i, j int) bool {
		if skills[i].Agent != skills[j].Agent {
			return skills[i].Agent < skills[j].Agent
		}
		return skills[i].Name < skills[j].Name
	})
	return skills
}

// skillAgentForRemotePath 依据远端路径判断技能所属 CLI。
func skillAgentForRemotePath(filePath string) string {
	if strings.Contains(filepath.ToSlash(filePath), "/.codex/") {
		return skillAgentCodex
	}
	return skillAgentClaude
}

// skillSourceForRemotePath 依据远端路径判断技能来源（user/project/plugin）。
func skillSourceForRemotePath(filePath, home, projectPath string) string {
	p := filepath.ToSlash(filePath)
	if strings.Contains(p, "/plugins/") {
		return skillSourcePlugin
	}
	if proj := strings.TrimSuffix(filepath.ToSlash(projectPath), "/") + "/"; strings.HasPrefix(p, proj) {
		return skillSourceProject
	}
	return skillSourceUser
}

// readFileBestEffort 读取文件，失败返回空串；用于不可靠的 SKILL.md 读取。
func readFileBestEffort(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}
