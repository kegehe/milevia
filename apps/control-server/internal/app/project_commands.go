package app

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// 常用命令目录（见 docs/37）：让"新增常用命令"从自由输入变成从 CLI 真实可用的
// 命令里选。
//
// 三条硬事实决定了这里的形状（均为 2026-09-15 在 Claude Code 2.1.266 上的实测）：
//  1. 权威目录只存在于 CLI 自己手里 —— `system/init` 事件的 `slash_commands` 字段。
//     47 条里有 17 条是编译进 CLI 二进制的能力，磁盘上没有文件（文件扫描稳定漏掉），
//     所以"静态表 + 扫描"不能替代它。
//  2. 拿到它不需要花钱也不需要凭据：起一个 stream-json 会话、写一条本地命令
//     （/context）、读第一行 init 即可，`num_turns=0`、`total_cost_usd=0`；把凭据换成
//     假的、Base URL 指向黑洞，init 依旧完整。因此探针**不注入任何 profile 凭据**。
//  3. Codex 的 `codex exec` 不解析斜杠命令（斜杠只存在于它的 TUI 层），所以对 Codex
//     会话只提供"自定义 shell 命令"，目录恒为空。

// AgentCommandOption 是命令选择器里的一项。
type AgentCommandOption struct {
	// Name 是命令名（不含前导 `/`）。子目录形式的自定义命令用 `:` 连接，如 nested:deep。
	Name string `json:"name"`
	// Label 是给用户看的中文名；缺失时前端回落到 Name。
	Label string `json:"label,omitempty"`
	// Description 是中文说明。内置命令来自本文件的静态表，自定义命令来自 frontmatter。
	Description string `json:"description,omitempty"`
	// ArgumentHint 是 frontmatter 里的参数提示（如 "[路径]"）；仅自定义命令有。
	ArgumentHint string `json:"argumentHint,omitempty"`
	// Group 是来源分组：builtin | skill | project | user | plugin | other。
	Group string `json:"group"`
	// Recommended 标记策展出的"建议先用这些"的条目（docs/37 §3.1 的策展规则）。
	Recommended bool `json:"recommended,omitempty"`
	// TerminalOnly 说明该命令的交互绑在本地终端（CLI 自己把这类归入
	// terminal_slash_commands，并建议弱化 UI）；桌面端保留但需要标注。
	TerminalOnly bool `json:"terminalOnly,omitempty"`
}

// ProjectCommandsView 是 GET /api/projects/{id}/commands 的响应。
// 形状对齐 conversation_models.go 的 ConversationModelsView：目录 + 如实说明来源。
type ProjectCommandsView struct {
	ProjectID string `json:"projectId"`
	AgentID   string `json:"agentId"`
	Env       string `json:"env"`
	// Source 说明这份目录是怎么来的：run（从真实运行采样）| probe（主动探针）
	// | scan（文件扫描）| static（静态兜底）。
	Source string `json:"source"`
	// Authoritative 为 true 时表示目录来自 CLI 本身，可用于判断"某条命令当前
	// CLI 是否提供"。static/scan 下前端不得据此判定命令失效（会误报）。
	Authoritative bool                 `json:"authoritative"`
	Commands      []AgentCommandOption `json:"commands"`
	// ClaudeVersion 是产生这份目录的 CLI 版本（仅 run/probe 有）。
	ClaudeVersion string `json:"claudeCodeVersion,omitempty"`
	RefreshedAt   string `json:"refreshedAt,omitempty"`
	// CustomAllowed 表示是否仍提供"自定义 shell 命令"入口。命令目录只能覆盖 CLI
	// 命令；"让 AI 跑 pnpm test" 这类请求是提示词、无法枚举，必须留着。
	CustomAllowed bool `json:"customAllowed"`
	// Note 如实说明目录来源或降级原因。
	Note string `json:"note,omitempty"`
}

// ---------------------------------------------------------------------------
// CLI 命令目录（init 事件）
// ---------------------------------------------------------------------------

// claudeCommandCatalog 是一次观测到的 CLI 命令目录。
type claudeCommandCatalog struct {
	names    []string
	terminal map[string]bool
	skills   map[string]bool
	version  string
}

// parseClaudeCommandCatalog 解析 init 事件里的命令目录。不是 init 事件、或没有
// slash_commands 字段时返回 ok=false（调用方按"没拿到目录"处理，不报错）。
func parseClaudeCommandCatalog(payload []byte) (claudeCommandCatalog, bool) {
	var init struct {
		Type          string   `json:"type"`
		Subtype       string   `json:"subtype"`
		SlashCommands []string `json:"slash_commands"`
		TerminalOnly  []string `json:"terminal_slash_commands"`
		Skills        []string `json:"skills"`
		ClaudeVersion string   `json:"claude_code_version"`
	}
	if err := json.Unmarshal(payload, &init); err != nil {
		return claudeCommandCatalog{}, false
	}
	if init.Type != "" && init.Type != "system" {
		return claudeCommandCatalog{}, false
	}
	if init.Subtype != "" && init.Subtype != "init" {
		return claudeCommandCatalog{}, false
	}
	if len(init.SlashCommands) == 0 {
		return claudeCommandCatalog{}, false
	}
	catalog := claudeCommandCatalog{
		names:    init.SlashCommands,
		terminal: map[string]bool{},
		skills:   map[string]bool{},
		version:  strings.TrimSpace(init.ClaudeVersion),
	}
	for _, name := range init.TerminalOnly {
		catalog.terminal[name] = true
	}
	for _, name := range init.Skills {
		catalog.skills[name] = true
	}
	return catalog, true
}

// claudeCommandCatalogRunner 由能观测到 CLI 命令目录的 runner 实现。
// 本机 claudeCLIRunner 走探针；WSL / SSH 见 docs/37 的 Phase 2。
type claudeCommandCatalogRunner interface {
	claudeCommandCatalog(ctx context.Context, projectPath string) (claudeCommandCatalog, error)
}

// claudeCommandProbeTimeout 是探针的整体上限。真机实测（2026-09-16，Claude Code 2.1.266）
// 常态 2.8–3.1 秒；同一台机器在高负载（并行跑全量测试）时出现过一次超过 6 秒，故留到 10 秒
// ——仍在前端 GET 的 15 秒超时之内。超时即降级，不阻塞用户界面。
const claudeCommandProbeTimeout = 10 * time.Second

// probeClaudeCommandCatalog 起一个一次性 stream-json 会话，读到 init 事件后立即收工。
//
// 三个必须守住的点（都来自实测）：
//   - **必须写一条 stdin**：不写任何消息时 CLI 不会产生 init（空 stdin 保持 18 秒输出 0 字节）；
//   - **只能用本地命令**：/context 由 CLI 本地应答（model=<synthetic>、num_turns=0、成本 0），
//     写成普通提示词就会真的调用模型；
//   - **不注入 profile 凭据**：init 在任何 API 调用之前产生，假 key 也能拿到完整目录，
//     因此这里用 os.Environ()，绝不触发凭据解密。
//
// --no-session-persistence 保证探针不在用户历史里留下可 resume 的会话。
// newProbeSessionID 生成探针用的一次性会话 id。抽成变量是为了让真机测试能用固定 id
// 在进程列表里认出这次探针产生的进程（见 project_commands_live_test.go）。
var newProbeSessionID = uuid.NewString

func probeClaudeCommandCatalog(ctx context.Context, claudePath, projectPath string) (claudeCommandCatalog, error) {
	args := []string{
		"-p", "--verbose",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--session-id", newProbeSessionID(),
		"--no-session-persistence",
	}
	cmd := exec.Command(claudePath, args...)
	cmd.Dir = projectPath
	cmd.Env = os.Environ()
	configureProcessGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return claudeCommandCatalog{}, fmt.Errorf("open Claude stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return claudeCommandCatalog{}, fmt.Errorf("open Claude stdout: %w", err)
	}
	// stderr 要收着：探针失败时（未登录、CLI 启动即崩、参数不被支持）真实原因都在这里，
	// 丢掉它就只能报一句"没读到 init"，与既有一次性运行的做法一致（stderrCapture）。
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return claudeCommandCatalog{}, fmt.Errorf("open Claude stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return claudeCommandCatalog{}, fmt.Errorf("start Claude: %w", err)
	}
	// 尽快把探针进程收进作业对象：Windows 上真正的 CLI 是 .cmd 包装器拉起的孙进程，
	// 只按直接子进程的 PID 收尾并不可靠（见 probe_job_windows.go 的说明）。
	processJob, jobErr := newProbeProcessJob()
	if jobErr == nil {
		if err := processJob.assign(cmd.Process); err != nil {
			processJob.close()
			processJob = nil
		}
	}
	var stderrTail = &stderrCapture{}
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 64*1024), 64*1024)
		scanner.Split(wslStderrSplit)
		for scanner.Scan() {
			stderrTail.append(scanner.Text())
		}
	}()

	lines := make(chan []byte, 4)
	stop := make(chan struct{})
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(stdout)
		// init 事件带 tools/skills/plugins 等数组，单行可达数 KB；给足上限。
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			select {
			case lines <- line:
			case <-stop:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	defer func() {
		close(stop)
		// 关掉 stdin 只是"顺手让 CLI 有机会自己退"，**收尾不能依赖它**：Windows 上探针的
		// 直接子进程是 npm 的 .cmd 包装器、真正的 CLI 是孙进程，包装器随时可能先退出。
		//
		// 三层收尾，全部在后台做（Windows 上 taskkill 自身可能要几百毫秒到数秒，同步做会把
		// 最坏耗时推到十几秒——实测过一次 14.6s，贴着前端 GET 的 15s 超时）：
		//   1. 关闭作业对象句柄：作业内所有进程（含孙进程）一起结束，这是唯一不依赖
		//      "包装器还活着"的办法；
		//   2. taskkill /T /F：作业对象没能覆盖时（创建/加入失败）尽力收整棵树；
		//   3. 直接 Kill：至少让直接子进程消失。
		_ = stdin.Close()
		go func() {
			processJob.close()
			terminateProcessGroup(cmd)
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		}()
	}()

	// 触发 init 的本地命令：无副作用、零成本、输出不会被解析。
	message := `{"type":"user","session_id":"","parent_tool_use_id":null,"message":{"role":"user","content":[{"type":"text","text":"/context"}]}}` + "\n"
	if _, err := io.WriteString(stdin, message); err != nil {
		return claudeCommandCatalog{}, fmt.Errorf("write Claude stdin: %w", err)
	}

	for {
		select {
		case line, ok := <-lines:
			if !ok {
				// stderr 读完了才判定，否则会把还没读到的原因丢掉。
				select {
				case <-stderrDone:
				case <-time.After(200 * time.Millisecond):
				}
				return claudeCommandCatalog{}, fmt.Errorf("Claude exited before the init event%s", claudeStderrDetail(stderrTail.tail()))
			}
			if catalog, ok := parseClaudeCommandCatalog(line); ok {
				return catalog, nil
			}
		case <-ctx.Done():
			return claudeCommandCatalog{}, ctx.Err()
		}
	}
}

// claudeCommandCatalog 实现 claudeCommandCatalogRunner（本机 runner）。
func (r *claudeCLIRunner) claudeCommandCatalog(ctx context.Context, projectPath string) (claudeCommandCatalog, error) {
	if r.config.ClaudePath == "" {
		return claudeCommandCatalog{}, errors.New("Claude CLI path is not configured")
	}
	return probeClaudeCommandCatalog(ctx, r.config.ClaudePath, projectPath)
}

// ---------------------------------------------------------------------------
// 内置命令与技能的中文说明（静态表）
// ---------------------------------------------------------------------------

// claudeBuiltinCommandLabels 是内置命令的中文说明。
//
// 为什么是静态表：init 事件只给命令名、不给描述；`/help` 在 print 模式下不可用
// （实测返回 "/help isn't available in this environment."），所以内置命令没有任何
// 机器可读的描述来源。这与 claudeModelCatalog() 的取舍一致：静态表 + 未收录时回落
// 显示命令名 + 在 Note 里如实说明。
//
// 升级 Claude Code 大版本后需要人工复核；表中没有的命令不会消失，只是没有中文说明。
var claudeBuiltinCommandLabels = map[string]AgentCommandOption{
	"clear":           {Label: "清空会话", Description: "清空当前对话上下文（应用本地处理，旧会话保留在历史里）"},
	"compact":         {Label: "压缩上下文", Description: "把当前上下文压缩成摘要后继续，省 token"},
	"context":         {Label: "查看上下文占用", Description: "列出上下文各部分的 token 占用"},
	"usage":           {Label: "查看用量", Description: "查看本次会话的用量与限额"},
	"model":           {Label: "切换模型", Description: "切换本次会话使用的模型（底部模型栏已有等价入口）"},
	"effort":          {Label: "设置推理强度", Description: "调整本轮工作的推理强度"},
	"fast":            {Label: "快速模式", Description: "切换快速模式"},
	"init":            {Label: "初始化项目文档", Description: "为当前项目生成/更新 CLAUDE.md"},
	"config":          {Label: "配置", Description: "查看或修改 Claude Code 配置"},
	"mcp":             {Label: "MCP 服务器", Description: "查看与管理 MCP 服务器连接"},
	"agents":          {Label: "子代理", Description: "管理子代理"},
	"list-agents":     {Label: "列出子代理", Description: "列出可用的子代理"},
	"rename":          {Label: "重命名会话", Description: "给当前会话改一个名字"},
	"recap":           {Label: "回顾会话", Description: "回顾本次会话做了什么"},
	"security-review": {Label: "安全审查", Description: "对当前改动做一次安全审查"},
	"autocompact":     {Label: "自动压缩设置", Description: "调整自动压缩的触发阈值"},
	"insights":        {Label: "项目洞察", Description: "查看项目洞察（应用侧已有等价入口）"},
	"reload-plugins":  {Label: "重载插件", Description: "重新加载插件（仅本地终端可用）"},
	"reload-skills":   {Label: "重载技能", Description: "重新加载技能"},
	"color":           {Label: "终端配色", Description: "调整终端配色（仅本地终端可用）"},
	"doctor":          {Label: "安装体检", Description: "检查 Claude Code 安装与配置（仅本地终端可用）"},
	"heapdump":        {Label: "导出堆快照", Description: "导出进程堆快照用于排查"},
}

// claudeSkillCommandLabels 是 CLI 内置技能（bundled skills）的中文说明。
//
// 这些技能编译在 claude 二进制里，磁盘上没有文件（~/.claude/skills 可能压根不存在），
// 所以文件扫描与 Skill 面板都看不到它们，只能由 init 目录发现——本表只负责给中文名。
var claudeSkillCommandLabels = map[string]AgentCommandOption{
	"deep-research":            {Label: "深度调研", Description: "多来源检索并交叉验证，产出带引用的调研结论"},
	"design":                   {Label: "设计", Description: "在 Claude Design 项目里做设计并同步回代码"},
	"design-sync":              {Label: "同步设计系统", Description: "把本地组件库与设计系统项目对齐"},
	"dataviz":                  {Label: "数据可视化", Description: "按设计规范产出图表"},
	"update-config":            {Label: "改配置", Description: "修改 settings.json / hooks 等配置"},
	"verify":                   {Label: "验证改动", Description: "实际跑一遍，确认改动真的生效（而不是只看测试通过）"},
	"debug":                    {Label: "调试", Description: "定位并修复失败"},
	"code-review":              {Label: "代码审查", Description: "审查当前改动、PR 或指定分支的差异"},
	"simplify":                 {Label: "简化重构", Description: "复查改动里的重复与冗余并简化"},
	"batch":                    {Label: "批量改动", Description: "把同一改动批量套用到多处"},
	"fewer-permission-prompts": {Label: "减少权限询问", Description: "分析历史调用，给出可加白名单的只读命令"},
	"loop":                     {Label: "定时循环", Description: "按间隔重复执行某个任务"},
	"claude-api":               {Label: "Claude API 参考", Description: "查阅 Claude API / SDK 的模型与用法"},
	"workflow-authoring":       {Label: "编写工作流", Description: "编写多代理工作流脚本"},
	"run":                      {Label: "运行本项目", Description: "启动并驱动本项目的应用，验证改动"},
	"run-skill-generator":      {Label: "生成技能", Description: "为项目生成新的技能"},
}

// claudeRecommendedCommands 是策展出的"建议先用这些"。清单刻意短：命令目录有 40+
// 条，平铺会让人无从下手；其余条目在"技能 / 项目自定义 / 其它"分组里仍可搜索到。
var claudeRecommendedCommands = []string{
	"compact", "context", "code-review", "verify", "simplify", "security-review", "init", "recap",
}

// isRecommendedCommand 报告命令是否属于建议组。
func isRecommendedCommand(name string) bool {
	for _, candidate := range claudeRecommendedCommands {
		if candidate == name {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 自定义命令文件扫描（补描述与来源分组）
// ---------------------------------------------------------------------------

// scannedCommand 是文件系统里扫到的一条自定义命令。
type scannedCommand struct {
	// name 是命令名：项目/用户级按相对路径生成（子目录用 `:` 连接），插件级带插件名前缀。
	name         string
	description  string
	argumentHint string
	source       string // project | user | plugin
}

// commandScanRoot 描述一次自定义命令扫描的根目录。
type commandScanRoot struct {
	dir    string
	source string
	// prefix 非空时作为命令名前缀（插件命令在 CLI 里形如 plugin:command）。
	prefix string
	// subdirs 表示是否进入子目录（子目录只影响命名空间，不改变来源）。
	subdirs bool
}

// localCommandScanRoots 返回本机自定义命令的扫描根。
// 顺序即优先级：user < plugin < project，同名后者覆盖前者（与 skills.go 一致）。
func localCommandScanRoots(projectPath string) []commandScanRoot {
	roots := []commandScanRoot{}
	if home := homeDir(); home != "" {
		roots = append(roots, commandScanRoot{dir: filepath.Join(home, ".claude", "commands"), source: skillSourceUser, subdirs: true})
	}
	if home := homeDir(); home != "" {
		roots = append(roots, pluginCommandScanRoots(filepath.Join(home, ".claude", "plugins"))...)
	}
	if projectPath != "" {
		roots = append(roots, commandScanRoot{dir: filepath.Join(projectPath, ".claude", "commands"), source: skillSourceProject, subdirs: true})
	}
	return roots
}

// pluginCommandScanRoots 找出插件树里名为 commands 的目录。
// 插件命令在 CLI 里形如 `plugin-name:command`，但插件名前缀由 CLI 决定（市场名/目录名
// 都可能），此处不臆造前缀——只用于给目录里已有的名字补描述，prefix 留空。
func pluginCommandScanRoots(pluginRoot string) []commandScanRoot {
	var roots []commandScanRoot
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if depth > 8 {
			return
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
			if strings.EqualFold(name, "commands") {
				roots = append(roots, commandScanRoot{dir: child, source: skillSourcePlugin, subdirs: true})
				continue
			}
			walk(child, depth+1)
		}
	}
	walk(pluginRoot, 0)
	return roots
}

// scanLocalCommands 扫描本地自定义命令。目录不存在或不可读时静默跳过（与 skills.go 一致）。
func scanLocalCommands(projectPath string) []scannedCommand {
	byName := map[string]scannedCommand{}
	order := []string{}
	for _, root := range localCommandScanRoots(projectPath) {
		for _, command := range scanCommandRoot(root) {
			if _, seen := byName[command.name]; !seen {
				order = append(order, command.name)
			}
			byName[command.name] = command // 后者覆盖前者：user < plugin < project
		}
	}
	out := make([]scannedCommand, 0, len(order))
	for _, name := range order {
		out = append(out, byName[name])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// scanCommandRoot 扫描单个根目录下的 *.md，按相对路径生成命令名。
func scanCommandRoot(root commandScanRoot) []scannedCommand {
	if root.dir == "" {
		return nil
	}
	var out []scannedCommand
	var walk func(dir, prefix string)
	walk = func(dir, prefix string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() {
				if !root.subdirs || name == ".git" || name == "node_modules" {
					continue
				}
				walk(filepath.Join(dir, name), prefix+name+":")
				continue
			}
			if !strings.HasSuffix(strings.ToLower(name), ".md") {
				continue
			}
			commandName := prefix + strings.TrimSuffix(name, filepath.Ext(name))
			description, argumentHint := parseCommandFrontmatter(readFileBestEffort(filepath.Join(dir, name)))
			out = append(out, scannedCommand{name: commandName, description: description, argumentHint: argumentHint, source: root.source})
		}
	}
	walk(root.dir, root.prefix)
	return out
}

// parseCommandFrontmatter 解析命令文件的 frontmatter，取 description 与 argument-hint。
// 与 parseSkillFrontmatter 同样的轻量手写解析（不引入 yaml 依赖）：只认 `key: value`，
// 忽略缩进的嵌套块与未知键，不因陌生键而失败。
func parseCommandFrontmatter(content string) (description, argumentHint string) {
	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 64*1024), 64*1024)
	first := true
	inNested := false
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if first {
			if trimmed != skillFrontmatterDelim {
				return "", ""
			}
			first = false
			continue
		}
		if trimmed == skillFrontmatterDelim {
			break
		}
		if trimmed == "" {
			continue
		}
		if line != trimmed && inNested {
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		key := strings.TrimSpace(line[:colon])
		value := strings.TrimSpace(line[colon+1:])
		if value == "" {
			inNested = true
			continue
		}
		inNested = false
		value = strings.Trim(value, `"'`)
		switch key {
		case "description":
			if description == "" {
				description = value
			}
		case "argument-hint":
			if argumentHint == "" {
				argumentHint = value
			}
		}
	}
	return description, argumentHint
}

// ---------------------------------------------------------------------------
// 目录组装
// ---------------------------------------------------------------------------

// buildCommandOptions 把 CLI 目录、文件扫描与静态表合并成选择器的条目列表。
//
// authoritative 为 true 时以 CLI 目录为准：CLI 没列出的自定义命令会被丢弃
// （文件的 frontmatter 可以把命令设为不可调用，此时 CLI 不列、我们也不该提供）。
func buildCommandOptions(catalog claudeCommandCatalog, scanned []scannedCommand, authoritative bool) []AgentCommandOption {
	scannedByName := map[string]scannedCommand{}
	for _, command := range scanned {
		scannedByName[command.name] = command
	}

	names := catalog.names
	if !authoritative {
		// 没有 CLI 目录时，用静态表 + 扫描结果拼一份"候选建议"，并在 Note 里说明。
		// 排序保证同一环境下响应稳定（map 迭代顺序随机）。
		seen := map[string]bool{}
		names = nil
		for name := range claudeBuiltinCommandLabels {
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
		for name := range claudeSkillCommandLabels {
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
		for _, command := range scanned {
			if !seen[command.name] {
				seen[command.name] = true
				names = append(names, command.name)
			}
		}
		sort.Strings(names)
	}

	options := make([]AgentCommandOption, 0, len(names))
	for _, name := range names {
		option := AgentCommandOption{Name: name, Group: "other"}
		if scannedCommand, ok := scannedByName[name]; ok {
			option.Group = scannedCommand.source
			option.Description = scannedCommand.description
			option.ArgumentHint = scannedCommand.argumentHint
		}
		if catalog.skills[name] {
			option.Group = "skill"
		}
		if builtin, ok := claudeBuiltinCommandLabels[name]; ok {
			if option.Description == "" {
				option.Description = builtin.Description
			}
			option.Label = builtin.Label
			if option.Group == "other" {
				option.Group = "builtin"
			}
		}
		if skill, ok := claudeSkillCommandLabels[name]; ok {
			if option.Description == "" {
				option.Description = skill.Description
			}
			if option.Label == "" {
				option.Label = skill.Label
			}
			if option.Group == "other" {
				option.Group = "skill"
			}
		}
		if option.Group == "other" && !authoritative {
			// 静态拼装时其它来源未知，统一归到内置候选，避免把 CLI 自己的命令
			// 说成"其它"。权威目录下则如实保留 other（通常是 CLI 内部命令）。
			option.Group = "builtin"
		}
		option.TerminalOnly = catalog.terminal[name]
		option.Recommended = isRecommendedCommand(name)
		options = append(options, option)
	}
	return options
}

// ---------------------------------------------------------------------------
// 目录缓存
// ---------------------------------------------------------------------------

// commandCatalogTTL 是目录缓存有效期。目录只随 CLI 版本与项目配置变化，而探测要拉起
// 进程，故按 (项目, agent) 缓存一小段时间（与 modelCatalogTTL 同思路）。
const commandCatalogTTL = 10 * time.Minute

// commandCatalogEntry 是缓存的一份目录。
type commandCatalogEntry struct {
	key      string
	catalog  claudeCommandCatalog
	source   string // run | probe
	note     string
	observed time.Time
}

// cachedCommandCatalog 读取仍然新鲜的缓存；没有或已过期时返回 ok=false。
func (s *Server) cachedCommandCatalog(key string, ttl time.Duration) (commandCatalogEntry, bool) {
	s.commandCatalogMu.Lock()
	defer s.commandCatalogMu.Unlock()
	if s.commandCatalogEntry == nil || s.commandCatalogEntry.key != key {
		return commandCatalogEntry{}, false
	}
	if ttl > 0 && time.Since(s.commandCatalogEntry.observed) > ttl {
		return commandCatalogEntry{}, false
	}
	return *s.commandCatalogEntry, true
}

// storeCommandCatalog 写入缓存。来自真实运行的观测永远覆盖缓存（它比探针更新），
// 因此调用方需要区分：run 观测直接用 TTL=0 写，探针只在不比现有观测旧时写。
func (s *Server) storeCommandCatalog(entry commandCatalogEntry) {
	s.commandCatalogMu.Lock()
	defer s.commandCatalogMu.Unlock()
	if entry.source == "probe" && s.commandCatalogEntry != nil && s.commandCatalogEntry.key == entry.key && s.commandCatalogEntry.source == "run" && time.Since(s.commandCatalogEntry.observed) < commandCatalogTTL {
		return // 真实运行的观测更可信，探针不覆盖它
	}
	copied := entry
	s.commandCatalogEntry = &copied
}

// slashCommandsMarker 用于在事件热路径上做零分配的子串判断，避免为每条 system 事件
// 都构造一个 string。
var slashCommandsMarker = []byte(`"slash_commands"`)

// observeCommandCatalog 从真实运行的 init 事件里采样命令目录。
//
// 这是目录的常态来源：项目跑过一次之后就再也不用起探针。所有 runner（本机 / WSL /
// SSH 长驻会话）的事件都汇聚到 agentRunSink.Event，因此这里能覆盖全部环境。
// 调用点在事件热路径上，故先做一次零成本的字节子串判断再反序列化。
func (s *Server) observeCommandCatalog(projectID string, payload json.RawMessage) {
	if projectID == "" || !bytes.Contains(payload, slashCommandsMarker) {
		return
	}
	catalog, ok := parseClaudeCommandCatalog(payload)
	if !ok {
		return
	}
	s.storeCommandCatalog(commandCatalogEntry{
		key:      commandCatalogCacheKey(projectID, "claude-code"),
		catalog:  catalog,
		source:   "run",
		note:     "目录来自最近一次运行的 Claude Code 会话。",
		observed: time.Now(),
	})
}

// commandCatalogCacheKey 是目录缓存的键。项目决定 cwd 与自定义命令，agent 决定语义，
// 二者足以定位一份目录（环境由项目自身决定，不额外入键）。
func commandCatalogCacheKey(projectID, agentID string) string {
	return projectID + "|" + agentID
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

// projectCommands 返回命令选择器需要的目录与来源说明。
//
// 查询参数：
//   - agentId  默认 claude-code；codex 返回空目录（其 exec 路径不支持斜杠命令）
//   - refresh=1 忽略 TTL 重新探测（用户点"刷新"）
//   - probe=0  不允许起探针：只用缓存/扫描/静态表。会话页加载时的徽标判定走这条，
//     避免仅仅打开一个项目就拉起 CLI 进程。
func (s *Server) projectCommands(w http.ResponseWriter, r *http.Request) {
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
	// agentId 只区分 Codex 与"其余（= Claude）"。收敛成这两个取值再入缓存键，避免任意
	// 查询串（?agentId=xxx）各占一条缓存、各拉起一次探针。
	agentID := strings.TrimSpace(r.URL.Query().Get("agentId"))
	if agentID != "codex" {
		agentID = "claude-code"
	}
	target := s.resolveAgentTargetEnv(project.Runner, project.Path)

	view := ProjectCommandsView{
		ProjectID:     project.ID,
		AgentID:       agentID,
		Env:           string(target),
		Commands:      []AgentCommandOption{},
		CustomAllowed: true,
	}
	if agentID == "codex" {
		// Codex 的斜杠命令只存在于它的 TUI 层，`codex exec` 会把 `/status` 当普通提示词
		// 传给模型（实测）。因此不提供目录，只保留"自定义 shell 命令"这条路。
		view.Source = "static"
		view.Note = "Codex 在非交互模式下不支持斜杠命令，请使用自定义 shell 命令。"
		writeJSON(w, http.StatusOK, view)
		return
	}

	key := commandCatalogCacheKey(project.ID, agentID)
	refresh := r.URL.Query().Get("refresh") == "1"
	probeAllowed := r.URL.Query().Get("probe") != "0"

	entry, cached := s.cachedCommandCatalog(key, commandCatalogTTL)
	probeNote := ""
	if refresh || !cached {
		probed, ok, reason := s.probeCommandCatalog(r.Context(), project, target, key, probeAllowed, refresh)
		if ok {
			entry, cached = probed, true
		} else if stale, ok := s.cachedCommandCatalog(key, 0); ok {
			// 探测失败但手里有过期的目录时，用它而不是退回静态表：一份真观测到的目录
			// （可能只是旧了几分钟）比"内置候选"准确得多，只要如实说明它没能刷新。
			entry, cached = stale, true
			probeNote = commandCatalogStaleNote(reason, stale)
		} else {
			probeNote = commandCatalogFallbackNote(reason)
		}
	}

	scanned := scanLocalCommands(project.Path)
	if cached {
		view.Source = entry.source
		view.Note = entry.note
		if probeNote != "" {
			view.Note = probeNote
		}
		view.ClaudeVersion = entry.catalog.version
		view.RefreshedAt = entry.observed.UTC().Format(time.RFC3339)
		view.Authoritative = true
		view.Commands = buildCommandOptions(entry.catalog, scanned, true)
	} else {
		view.Source = "static"
		view.Note = probeNote
		if view.Note == "" {
			view.Note = "未能读取 Claude CLI 的命令目录，这里列出的是内置候选；在项目中跑一次或点「刷新」可获得完整目录。"
		}
		view.Commands = buildCommandOptions(claudeCommandCatalog{}, scanned, false)
	}
	writeJSON(w, http.StatusOK, view)
}

// commandCatalogFailureReason 把"没能探测"的原因翻成一个不带标点的从句，供下面两句拼接。
func commandCatalogFailureReason(reason string) string {
	switch reason {
	case "disabled":
		return "本次没有主动探测命令目录"
	case "no-runner":
		return "未能解析该项目对应的 Claude runner"
	case "unsupported":
		return "该运行环境暂不支持主动读取命令目录（见 docs/37 的分期）"
	default:
		return "读取 Claude CLI 的命令目录失败"
	}
}

// commandCatalogFallbackNote 是完全没有目录可用时的说明。
func commandCatalogFallbackNote(reason string) string {
	return commandCatalogFailureReason(reason) + "，这里列出的是内置候选；在项目里跑一次或点「刷新」可获得完整目录。"
}

// commandCatalogStaleNote 是"探测失败但用过期的旧目录顶替"时的说明：不假装刷新成功。
func commandCatalogStaleNote(reason string, entry commandCatalogEntry) string {
	version := entry.catalog.version
	if version == "" {
		version = "版本未知"
	}
	return fmt.Sprintf("%s，展示的是 %s 观测到的目录（Claude Code %s）；点「刷新」可重试。",
		commandCatalogFailureReason(reason), entry.observed.Local().Format("01-02 15:04"), version)
}

// probeCommandCatalog 尝试主动探测目录。任何失败都不是错误：返回 ok=false 与原因，
// 让调用方降级到扫描 + 静态表（与 codexModelCatalog 的降级策略一致）。
// refresh=true 表示用户显式要求重取，此时连缓存复查也跳过——否则"刷新"会永远返回旧目录。
func (s *Server) probeCommandCatalog(ctx context.Context, project Project, target agentTargetEnv, key string, allowed, refresh bool) (commandCatalogEntry, bool, string) {
	if !allowed {
		return commandCatalogEntry{}, false, "disabled"
	}
	runner := s.agentClaudeRunnerFor(target)
	if runner == nil {
		return commandCatalogEntry{}, false, "no-runner"
	}
	capable, ok := runner.(claudeCommandCatalogRunner)
	if !ok {
		// WSL / SSH 的探针见 docs/37 Phase 2；在此之前由真实运行采样（run）补全。
		return commandCatalogEntry{}, false, "unsupported"
	}
	// 串行化探针：并发请求进来时第二个请求在拿到锁后先复查缓存，避免重复拉起 CLI。
	s.commandProbeMu.Lock()
	defer s.commandProbeMu.Unlock()
	if !refresh {
		if entry, ok := s.cachedCommandCatalog(key, commandCatalogTTL); ok {
			return entry, true, ""
		}
	}
	probeCtx, cancel := context.WithTimeout(ctx, claudeCommandProbeTimeout)
	defer cancel()
	catalog, err := capable.claudeCommandCatalog(probeCtx, project.Path)
	if err != nil {
		log.Printf("probe Claude command catalog for project %s: %v", project.ID, err)
		return commandCatalogEntry{}, false, "failed"
	}
	entry := commandCatalogEntry{
		key:      key,
		catalog:  catalog,
		source:   "probe",
		note:     "目录来自项目内的 Claude CLI 探针（不消耗 token）。",
		observed: time.Now(),
	}
	s.storeCommandCatalog(entry)
	return entry, true, ""
}

// slashCommandName 从模板里取出斜杠命令名（不含 `/` 与参数）。不是斜杠命令时返回空串。
// 渲染后的模板才会走到这里，因此参数（如 `/model opus`）只取首段。
func slashCommandName(template string) string {
	trimmed := strings.TrimSpace(template)
	if !strings.HasPrefix(trimmed, "/") {
		return ""
	}
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return ""
	}
	return strings.TrimPrefix(fields[0], "/")
}
