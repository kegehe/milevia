package app

// 插件管理：把各 CLI 自己的插件生命周期能力代理成一套统一接口。
//
// 三条纪律（与 docs/65 对应）：
//  1. **CLI 是唯一事实源** —— 列表、启用状态、范围、版本都来自 CLI 的 JSON 输出；
//     磁盘扫描只在 CLI 不支持查询时兜底，并且明确标注「状态未知」。
//  2. **不发明插件格式** —— 安装、卸载、启停、市场全部交给 CLI 自己的子命令。
//  3. **不替用户确认** —— marketplace 声明的待执行命令必须由用户看过 sha256 后显式接受；
//     这里只透传 --accept-command，绝不自动追加 -y。

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

const (
	pluginScopeUser    = "user"
	pluginScopeProject = "project"
	pluginScopeLocal   = "local"

	pluginSourceCLI  = "cli"
	pluginSourceScan = "scan"

	pluginListTimeout     = 30 * time.Second
	pluginDetailsTimeout  = 20 * time.Second
	pluginMutationTimeout = 5 * time.Minute
	pluginCacheTTL        = 30 * time.Second
	// pluginListFlightTimeout 是单飞 leader 的上限：list(30s) + marketplaces(30s) +
	// 能力探测 --help(10s)，再加一点余量。它脱开请求取消，只受这个期限约束。
	pluginListFlightTimeout = 2*pluginListTimeout + 30*time.Second
	pluginCapsTTL           = 10 * time.Minute
	pluginOutputLimit       = 256 * 1024
	// 插件清单只用来补全名称/版本/描述，没有理由超过这个大小。
	pluginManifestSizeLimit = 128 * 1024
)

// PluginCapabilities 说明某台机器上的某个 CLI 实际支持哪些插件动作。
// 前端只渲染 true 的动作，不显示"点了才知道不支持"的假按钮。
type PluginCapabilities struct {
	List         bool `json:"list"`
	Details      bool `json:"details"`
	Install      bool `json:"install"`
	Uninstall    bool `json:"uninstall"`
	Enable       bool `json:"enable"`
	Disable      bool `json:"disable"`
	Update       bool `json:"update"`
	Marketplaces bool `json:"marketplaces"`
	// Scopes 是该 CLI 真正支持的安装范围。Codex 的市场与安装都是全局配置，
	// 前端据此只渲染 user，避免"选了项目却装到全局"的误导。
	Scopes []string `json:"scopes"`
}

// PluginComponents 是插件内各类组件的数量。来自 CLI 的 JSON 或插件目录清点。
type PluginComponents struct {
	Skills     int `json:"skills"`
	Commands   int `json:"commands"`
	Agents     int `json:"agents"`
	Hooks      int `json:"hooks"`
	MCPServers int `json:"mcpServers"`
	LSPServers int `json:"lspServers"`
}

// PluginRecord 是插件列表/详情共用的一条记录。
type PluginRecord struct {
	ID string `json:"id"`
	// Key 在同 ID 多范围安装时区分记录（id|scope|projectPath），前端用它做列表 key
	// 和操作定位；ID 本身不唯一（实测 Claude 同一插件可同时存在于 user 与 project）。
	Key         string `json:"key"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
	Description string `json:"description,omitempty"`
	Version     string `json:"version,omitempty"`
	Marketplace string `json:"marketplace,omitempty"`
	Scope       string `json:"scope"`
	Enabled     bool   `json:"enabled"`
	// StateKnown 为 false 表示这份记录来自磁盘兜底扫描，CLI 没有给出启用状态。
	StateKnown  bool             `json:"stateKnown"`
	CanDisable  bool             `json:"canDisable"`
	InstallPath string           `json:"installPath,omitempty"`
	InstalledAt string           `json:"installedAt,omitempty"`
	LastUpdated string           `json:"lastUpdated,omitempty"`
	ProjectPath string           `json:"projectPath,omitempty"`
	Components  PluginComponents `json:"components"`
}

// PluginDetails 在列表记录之上补充 token 开销与清单路径。
type PluginDetails struct {
	PluginRecord
	TokenCost    string `json:"tokenCost,omitempty"`
	ManifestPath string `json:"manifestPath,omitempty"`
}

// AvailablePlugin 是市场里可安装但尚未安装的插件。
type AvailablePlugin struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
	Version      string `json:"version,omitempty"`
	Marketplace  string `json:"marketplace,omitempty"`
	Installed    bool   `json:"installed"`
	InstallCount int    `json:"installCount,omitempty"`
}

// PluginMarketplace 是已配置的市场。
type PluginMarketplace struct {
	Name            string `json:"name"`
	Kind            string `json:"kind,omitempty"`
	Source          string `json:"source,omitempty"`
	InstallLocation string `json:"installLocation,omitempty"`
	LastUpdated     string `json:"lastUpdated,omitempty"`
}

// PluginListView 是 GET /api/runners/{runnerID}/plugins 的响应。
type PluginListView struct {
	RunnerID     string              `json:"runnerId"`
	AgentID      string              `json:"agentId"`
	Env          string              `json:"env"`
	Source       string              `json:"source"`
	Capabilities PluginCapabilities  `json:"capabilities"`
	Plugins      []PluginRecord      `json:"plugins"`
	Available    []AvailablePlugin   `json:"available,omitempty"`
	Marketplaces []PluginMarketplace `json:"marketplaces"`
	Note         string              `json:"note,omitempty"`
}

// PluginActionResult 是一次写操作的结果。NeedsConfirmation 为 true 时 Message 里
// 是 marketplace 声明的待执行命令，调用方必须让用户确认后再带 acceptCommandSha256 重试。
type PluginActionResult struct {
	OK                bool   `json:"ok"`
	Message           string `json:"message,omitempty"`
	NeedsConfirmation bool   `json:"needsConfirmation,omitempty"`
	Command           string `json:"command,omitempty"`
	CommandSHA256     string `json:"commandSha256,omitempty"`
	Output            string `json:"output,omitempty"`
}

// ── 适配器 ───────────────────────────────────────────────────────────────────

type pluginDialect int

const (
	pluginDialectClaude pluginDialect = iota
	pluginDialectCodex
	pluginDialectQoder
)

type pluginAdapter interface {
	Capabilities() PluginCapabilities
	List(ctx context.Context, exec *pluginExec, cwd string, includeAvailable bool) ([]PluginRecord, []AvailablePlugin, error)
	Details(ctx context.Context, exec *pluginExec, cwd string, record PluginRecord) (PluginDetails, error)
	Install(ctx context.Context, exec *pluginExec, cwd string, request pluginInstallRequest) (PluginActionResult, error)
	SetEnabled(ctx context.Context, exec *pluginExec, cwd string, record PluginRecord, enabled bool) (PluginActionResult, error)
	Update(ctx context.Context, exec *pluginExec, cwd string, record PluginRecord, acceptCommandSHA256 string) (PluginActionResult, error)
	Uninstall(ctx context.Context, exec *pluginExec, cwd string, record PluginRecord) (PluginActionResult, error)
	Marketplaces(ctx context.Context, exec *pluginExec, cwd string) ([]PluginMarketplace, error)
	MarketplaceAdd(ctx context.Context, exec *pluginExec, cwd, source, scope string) (PluginActionResult, error)
	MarketplaceRemove(ctx context.Context, exec *pluginExec, cwd, name string) (PluginActionResult, error)
	MarketplaceUpdate(ctx context.Context, exec *pluginExec, cwd, name string) (PluginActionResult, error)
}

type cliPluginAdapter struct {
	agentID string
	dialect pluginDialect
}

func pluginAdapterFor(agentID string) (pluginAdapter, bool) {
	switch agentID {
	case "claude-code":
		return &cliPluginAdapter{agentID: agentID, dialect: pluginDialectClaude}, true
	case "codex":
		return &cliPluginAdapter{agentID: agentID, dialect: pluginDialectCodex}, true
	case "qoder":
		return &cliPluginAdapter{agentID: agentID, dialect: pluginDialectQoder}, true
	}
	return nil, false
}

func (a *cliPluginAdapter) Capabilities() PluginCapabilities {
	scopes := []string{pluginScopeUser, pluginScopeProject, pluginScopeLocal}
	switch a.dialect {
	case pluginDialectClaude:
		return PluginCapabilities{List: true, Details: true, Install: true, Uninstall: true, Enable: true, Disable: true, Update: true, Marketplaces: true, Scopes: scopes}
	case pluginDialectQoder:
		return PluginCapabilities{List: true, Details: true, Install: true, Uninstall: true, Enable: true, Disable: true, Update: true, Marketplaces: true, Scopes: scopes}
	default: // Codex 目前没有 enable/disable/update 子命令，市场与安装是全局配置。
		return PluginCapabilities{List: true, Details: true, Install: true, Uninstall: true, Marketplaces: true, Scopes: []string{pluginScopeUser}}
	}
}

func (a *cliPluginAdapter) verb() string {
	if a.dialect == pluginDialectQoder {
		return "plugins"
	}
	return "plugin"
}

func pluginAdapterVerb(adapter pluginAdapter) string {
	if typed, ok := adapter.(*cliPluginAdapter); ok {
		return typed.verb()
	}
	return "plugin"
}

// ── 命令行执行（本机 / WSL / SSH）────────────────────────────────────────────

type pluginExec struct {
	server   *Server
	runnerID string
	agentID  string
}

type pluginCommandOutput struct {
	Stdout string
	Stderr string
	Err    error
}

func (o pluginCommandOutput) detail() string {
	for _, text := range []string{o.Stderr, o.Stdout} {
		trimmed := strings.TrimSpace(text)
		if trimmed == "" {
			continue
		}
		if index := strings.IndexByte(trimmed, '\n'); index >= 0 {
			trimmed = strings.TrimSpace(trimmed[:index])
		}
		if len(trimmed) > 400 {
			trimmed = trimmed[:400] + "…"
		}
		return trimmed
	}
	if o.Err != nil {
		return o.Err.Error()
	}
	return ""
}

func (e *pluginExec) crossEnvironment() crossEnvironment {
	return e.server.crossEnvironmentFor(e.runnerID)
}

func (e *pluginExec) run(ctx context.Context, args []string, cwd string, timeout time.Duration) pluginCommandOutput {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if env := e.crossEnvironment(); env != nil {
		script, err := e.crossScript(env, args, cwd)
		if err != nil {
			return pluginCommandOutput{Err: err}
		}
		stdout, stderr, err := env.runWithLimit(runCtx, script, pluginOutputLimit)
		return pluginCommandOutput{Stdout: truncatePluginOutput(stdout), Stderr: truncatePluginOutput(stderr), Err: err}
	}
	if !isLocalRunnerID(e.runnerID) {
		return pluginCommandOutput{Err: errors.New("该执行环境暂不支持插件管理")}
	}
	binary := e.server.agentBinary(e.agentID)
	if strings.TrimSpace(binary) == "" {
		return pluginCommandOutput{Err: errors.New("找不到该 CLI 的可执行文件")}
	}
	cmd := exec.CommandContext(runCtx, binary, args...)
	// 与包内其它 CLI 启动一致：Windows 上隐藏控制台窗口；POSIX 上独立进程组，
	// 超时（pluginMutationTimeout 可达 5 分钟）时能连同 npm 式子孙一起结束。
	configureProcessGroup(cmd)
	// 超时/取消时结束整个进程组（安装可能拉起 npm 子孙），并用 WaitDelay 兜住继承了
	// 输出管道的后代：只杀直接子进程时，Wait 会一直等那个写端关闭。
	cmd.Cancel = func() error {
		forceTerminateProcessGroup(cmd)
		return nil
	}
	cmd.WaitDelay = 5 * time.Second
	if strings.TrimSpace(cwd) != "" {
		cmd.Dir = cwd
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if errors.Is(err, exec.ErrWaitDelay) && runCtx.Err() == nil {
		// CLI 已经退出（成功），只是它派生的后代还占着输出管道；WaitDelay 到点后关闭
		// 管道并返回这个错误。命令本身没有失败，不能报成安装/查询失败。
		err = nil
	}
	return pluginCommandOutput{Stdout: truncatePluginOutput(stdout.String()), Stderr: truncatePluginOutput(stderr.String()), Err: err}
}

func (e *pluginExec) crossScript(env crossEnvironment, args []string, cwd string) (string, error) {
	linuxCwd := strings.TrimSpace(cwd)
	if linuxCwd != "" {
		// WSL 项目的路径以 UNC 存储，必须换成发行版内路径才能 cd。
		// 但 CLI 自己报出来的 project 路径已经是 `/home/...` 形式（record.ProjectPath），
		// 这类绝对 Linux 路径可以直接用；不判断就会把它清空，命令会在 $HOME 里执行。
		if _, ok := env.(*wslCrossEnvironment); ok && !strings.HasPrefix(linuxCwd, "/") {
			if converted, valid := uncToWslPath(linuxCwd, ""); valid {
				linuxCwd = converted
			} else {
				linuxCwd = ""
			}
		}
	}
	var script strings.Builder
	script.WriteString(crossManagedPathPrefix())
	script.WriteString("\n")
	if linuxCwd != "" {
		script.WriteString("cd " + shellQuote(linuxCwd) + " || { echo '无法进入项目目录：" + strings.ReplaceAll(linuxCwd, "'", "") + "' >&2; exit 97; }\n")
	}
	entry, ok := agentByID(e.agentID)
	if !ok {
		return "", fmt.Errorf("不支持的工具 %s", e.agentID)
	}
	names := agentCommandNames(entry)
	if len(names) == 0 {
		return "", fmt.Errorf("找不到 %s 的命令名", e.agentID)
	}
	var lookups []string
	for _, name := range names {
		lookups = append(lookups, "command -v "+shellQuote(name)+" 2>/dev/null")
	}
	script.WriteString("Q=$(" + strings.Join(lookups, " || ") + " || true)\n")
	script.WriteString(`if [ -z "$Q" ]; then echo ` + shellQuote("目标环境里找不到 "+entry.Name+" 的命令") + ` >&2; exit 127; fi` + "\n")
	quoted := make([]string, 0, len(args)+1)
	quoted = append(quoted, `"$Q"`)
	for _, arg := range args {
		quoted = append(quoted, shellQuote(arg))
	}
	script.WriteString(strings.Join(quoted, " "))
	return script.String(), nil
}

func truncatePluginOutput(value string) string {
	if len(value) <= pluginOutputLimit {
		return value
	}
	return value[:pluginOutputLimit] + "\n…（输出已截断）"
}

// pluginJSONParseError 区分"CLI 没给 JSON"与"输出被截断所以 JSON 不完整"。
// 后者不是 CLI 的问题：报成"没有返回 JSON"会让人去查 CLI，而真正要做的是缩小清单。
func pluginJSONParseError(raw, what string) error {
	if strings.Contains(raw, "…（输出已截断）") {
		return fmt.Errorf("无法解析%s：CLI 输出超过 %d KiB 上限，请缩小清单后重试", what, pluginOutputLimit/1024)
	}
	return fmt.Errorf("无法解析%s：CLI 没有返回 JSON", what)
}

// extractJSON 从 CLI 输出里取出第一段可解析的 JSON。部分 CLI 会先打印提示行。
func extractJSON(raw string) ([]byte, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, false
	}
	if json.Valid([]byte(trimmed)) {
		return []byte(trimmed), true
	}
	lines := strings.Split(trimmed, "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		line := strings.TrimSpace(lines[index])
		if (strings.HasPrefix(line, "{") || strings.HasPrefix(line, "[")) && json.Valid([]byte(line)) {
			return []byte(line), true
		}
	}
	for index, char := range trimmed {
		if char == '{' || char == '[' {
			candidate := trimmed[index:]
			if json.Valid([]byte(candidate)) {
				return []byte(candidate), true
			}
		}
	}
	return nil, false
}

func pluginCommandError(prefix string, output pluginCommandOutput) error {
	detail := output.detail()
	if detail == "" {
		return fmt.Errorf("%s", prefix)
	}
	return fmt.Errorf("%s：%s", prefix, detail)
}

// ── 列表与解析 ───────────────────────────────────────────────────────────────

type claudeListPlugin struct {
	ID             string `json:"id"`
	Version        string `json:"version"`
	Scope          string `json:"scope"`
	Enabled        bool   `json:"enabled"`
	InstallPath    string `json:"installPath"`
	InstalledAt    string `json:"installedAt"`
	LastUpdated    string `json:"lastUpdated"`
	ProjectPath    string `json:"projectPath"`
	ProjectEnabled bool   `json:"projectEnabled"`
}

type claudeAvailablePlugin struct {
	PluginID        string `json:"pluginId"`
	Name            string `json:"name"`
	Description     string `json:"description"`
	Version         string `json:"version"`
	MarketplaceName string `json:"marketplaceName"`
	InstallCount    int    `json:"installCount"`
}

type qoderPluginResource struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type qoderPluginEntry struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Source          string `json:"source"`
	Version         string `json:"version"`
	Scope           string `json:"scope"`
	Enabled         bool   `json:"enabled"`
	CanDisable      bool   `json:"canDisable"`
	InstallPath     string `json:"installPath"`
	InstalledAt     string `json:"installedAt"`
	LastUpdated     string `json:"lastUpdated"`
	DisplayName     string `json:"displayName"`
	Description     string `json:"description"`
	MarketplaceName string `json:"marketplaceName"`
	PluginID        string `json:"pluginId"`
	Installed       bool   `json:"installed"`
	ProjectPath     string `json:"projectPath"`
	Resources       struct {
		Skills   []qoderPluginResource `json:"skills"`
		Agents   []qoderPluginResource `json:"agents"`
		MCP      []qoderPluginResource `json:"mcpServers"`
		Commands []qoderPluginResource `json:"commands"`
		Hooks    []qoderPluginResource `json:"hooks"`
	} `json:"resources"`
}

// qoderAvailablePlugin 是 Qoder `plugins list --json --available` 的 available 条目。
// 它的 source 是对象（archive/url/github），与 installed 条目的字符串 source 不同，
// 因此不能复用 qoderPluginEntry；这里只取界面需要的字段，其余忽略。
type qoderAvailablePlugin struct {
	PluginID        string `json:"pluginId"`
	Name            string `json:"name"`
	MarketplaceName string `json:"marketplaceName"`
	Version         string `json:"version"`
	Description     string `json:"description"`
	Installed       bool   `json:"installed"`
}

type codexPluginEntry struct {
	PluginID        string `json:"pluginId"`
	Name            string `json:"name"`
	MarketplaceName string `json:"marketplaceName"`
	Version         string `json:"version"`
	Installed       bool   `json:"installed"`
	Enabled         bool   `json:"enabled"`
	Source          struct {
		Source string `json:"source"`
		Path   string `json:"path"`
	} `json:"source"`
}

func (a *cliPluginAdapter) List(ctx context.Context, exec *pluginExec, cwd string, includeAvailable bool) ([]PluginRecord, []AvailablePlugin, error) {
	switch a.dialect {
	case pluginDialectClaude:
		return a.listClaude(ctx, exec, cwd, includeAvailable)
	case pluginDialectQoder:
		return a.listQoder(ctx, exec, cwd, includeAvailable)
	default:
		return a.listCodex(ctx, exec, cwd, includeAvailable)
	}
}

func (a *cliPluginAdapter) listClaude(ctx context.Context, exec *pluginExec, cwd string, includeAvailable bool) ([]PluginRecord, []AvailablePlugin, error) {
	args := []string{"plugin", "list", "--json"}
	if includeAvailable {
		args = append(args, "--available")
	}
	output := exec.run(ctx, args, cwd, pluginListTimeout)
	if output.Err != nil {
		return nil, nil, pluginCommandError("读取插件列表失败", output)
	}
	raw, ok := extractJSON(output.Stdout)
	if !ok {
		return nil, nil, pluginJSONParseError(output.Stdout, "插件列表")
	}
	var installed []claudeListPlugin
	var available []claudeAvailablePlugin
	var envelope struct {
		Installed []claudeListPlugin      `json:"installed"`
		Available []claudeAvailablePlugin `json:"available"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && (envelope.Installed != nil || envelope.Available != nil) {
		installed, available = envelope.Installed, envelope.Available
	} else if err := json.Unmarshal(raw, &installed); err != nil {
		return nil, nil, fmt.Errorf("无法解析插件列表：%w", err)
	}
	descriptions := map[string]string{}
	for _, item := range available {
		descriptions[item.PluginID] = item.Description
		descriptions[item.Name] = item.Description
	}
	records := make([]PluginRecord, 0, len(installed))
	for _, item := range installed {
		record := PluginRecord{
			ID:          item.ID,
			Name:        pluginNameFromID(item.ID),
			Version:     item.Version,
			Marketplace: pluginMarketplaceFromID(item.ID),
			Scope:       normalizePluginScope(item.Scope),
			Enabled:     item.Enabled,
			StateKnown:  true,
			CanDisable:  true,
			InstallPath: item.InstallPath,
			InstalledAt: item.InstalledAt,
			LastUpdated: item.LastUpdated,
			ProjectPath: item.ProjectPath,
			Description: descriptions[item.ID],
		}
		// 远端安装路径不可直读：enrichPluginRecord 读不到清单时会保留 CLI 给的字段，
		// 组件数量则以 CLI 提供的信息为准（Claude 不提供则为 0）。
		enrichPluginRecord(&record)
		if exec.crossEnvironment() != nil {
			record.InstallPath = item.InstallPath
		}
		record.Key = pluginRecordKey(record)
		records = append(records, record)
	}
	return records, availablePluginsFromClaude(available, installed), nil
}

func availablePluginsFromClaude(available []claudeAvailablePlugin, installed []claudeListPlugin) []AvailablePlugin {
	installedIDs := map[string]bool{}
	for _, item := range installed {
		installedIDs[item.ID] = true
	}
	out := make([]AvailablePlugin, 0, len(available))
	for _, item := range available {
		out = append(out, AvailablePlugin{
			ID:           item.PluginID,
			Name:         item.Name,
			Description:  item.Description,
			Version:      item.Version,
			Marketplace:  item.MarketplaceName,
			Installed:    installedIDs[item.PluginID],
			InstallCount: item.InstallCount,
		})
	}
	return out
}

func (a *cliPluginAdapter) listQoder(ctx context.Context, exec *pluginExec, cwd string, includeAvailable bool) ([]PluginRecord, []AvailablePlugin, error) {
	args := []string{"plugins", "list", "--json"}
	if includeAvailable {
		args = append(args, "--available")
	}
	output := exec.run(ctx, args, cwd, pluginListTimeout)
	if output.Err != nil {
		return nil, nil, pluginCommandError("读取插件列表失败", output)
	}
	raw, ok := extractJSON(output.Stdout)
	if !ok {
		return nil, nil, pluginJSONParseError(output.Stdout, "插件列表")
	}
	entries, availableEntries, envelopeShape, err := parseQoderPluginList(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("无法解析插件列表：%w", err)
	}
	records, available := qoderPluginView(entries, availableEntries, envelopeShape)
	sort.SliceStable(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	sort.SliceStable(available, func(i, j int) bool { return available[i].ID < available[j].ID })
	return records, available, nil
}

// qoderPluginView 把两种解析结果归一成列表记录与可安装项。
func qoderPluginView(entries []qoderPluginEntry, availableEntries []qoderAvailablePlugin, envelopeShape bool) ([]PluginRecord, []AvailablePlugin) {
	records := []PluginRecord{}
	available := []AvailablePlugin{}
	for _, item := range entries {
		id := firstNonEmpty(item.ID, item.PluginID)
		if id == "" {
			continue
		}
		name := firstNonEmpty(item.Name, pluginNameFromID(id))
		marketplace := firstNonEmpty(item.MarketplaceName, pluginMarketplaceFromID(id))
		// 数组形状下没有单独的 available 段，只能靠 installPath 判断是否已安装。
		if !envelopeShape && item.InstallPath == "" && !item.Installed {
			available = append(available, AvailablePlugin{
				ID: id, Name: name, Description: item.Description, Version: item.Version,
				Marketplace: marketplace, Installed: false,
			})
			continue
		}
		record := PluginRecord{
			ID: id, Name: name, DisplayName: item.DisplayName, Description: item.Description,
			Version: item.Version, Marketplace: marketplace,
			Scope: normalizePluginScope(item.Scope), Enabled: item.Enabled, StateKnown: true,
			CanDisable: item.CanDisable, InstallPath: item.InstallPath,
			InstalledAt: item.InstalledAt, LastUpdated: item.LastUpdated,
			ProjectPath: item.ProjectPath,
		}
		record.Components = PluginComponents{
			Skills: len(item.Resources.Skills), Commands: len(item.Resources.Commands),
			Agents: len(item.Resources.Agents), Hooks: len(item.Resources.Hooks),
			MCPServers: len(item.Resources.MCP),
		}
		enrichPluginRecord(&record)
		record.Key = pluginRecordKey(record)
		records = append(records, record)
	}
	installedIDs := map[string]bool{}
	for _, record := range records {
		installedIDs[record.ID] = true
	}
	for _, item := range availableEntries {
		id := firstNonEmpty(item.PluginID, item.Name)
		if id == "" {
			continue
		}
		available = append(available, AvailablePlugin{
			ID: id, Name: firstNonEmpty(item.Name, pluginNameFromID(id)),
			Description: item.Description, Version: item.Version,
			Marketplace: firstNonEmpty(item.MarketplaceName, pluginMarketplaceFromID(id)),
			Installed:   item.Installed || installedIDs[id],
		})
	}
	return records, available
}

// parseQoderPluginList 兼容 Qoder 的两种输出形状：不带 --available 时是数组；
// 带 --available 时是 {installed, available} 信封。帮助里写着 --available 却返回
// 数组的旧版本也接受，避免形状变化直接变成 502。
func parseQoderPluginList(raw []byte) ([]qoderPluginEntry, []qoderAvailablePlugin, bool, error) {
	var envelope struct {
		Installed []qoderPluginEntry     `json:"installed"`
		Available []qoderAvailablePlugin `json:"available"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && (envelope.Installed != nil || envelope.Available != nil) {
		return envelope.Installed, envelope.Available, true, nil
	}
	var entries []qoderPluginEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, nil, false, err
	}
	return entries, nil, false, nil
}

func (a *cliPluginAdapter) listCodex(ctx context.Context, exec *pluginExec, cwd string, includeAvailable bool) ([]PluginRecord, []AvailablePlugin, error) {
	output := exec.run(ctx, []string{"plugin", "list", "--json"}, cwd, pluginListTimeout)
	if output.Err != nil {
		return nil, nil, pluginCommandError("读取插件列表失败", output)
	}
	raw, ok := extractJSON(output.Stdout)
	if !ok {
		return nil, nil, pluginJSONParseError(output.Stdout, "插件列表")
	}
	var payload struct {
		Installed []codexPluginEntry `json:"installed"`
		Available []codexPluginEntry `json:"available"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, nil, fmt.Errorf("无法解析插件列表：%w", err)
	}
	records := []PluginRecord{}
	for _, item := range payload.Installed {
		id := firstNonEmpty(item.PluginID, item.Name)
		record := PluginRecord{
			ID: id, Name: firstNonEmpty(item.Name, pluginNameFromID(id)),
			Version: item.Version, Marketplace: item.MarketplaceName,
			Scope: pluginScopeUser, Enabled: item.Enabled, StateKnown: true,
			CanDisable: false, InstallPath: item.Source.Path,
		}
		enrichPluginRecord(&record)
		record.Key = pluginRecordKey(record)
		records = append(records, record)
	}
	available := []AvailablePlugin{}
	for _, item := range payload.Available {
		id := firstNonEmpty(item.PluginID, item.Name)
		available = append(available, AvailablePlugin{
			ID: id, Name: firstNonEmpty(item.Name, pluginNameFromID(id)),
			Version: item.Version, Marketplace: item.MarketplaceName, Installed: item.Installed,
		})
	}
	return records, available, nil
}

// ── 详情 ─────────────────────────────────────────────────────────────────────

var claudeTokenCostPattern = regexp.MustCompile(`(?i)always-on:[^\n]*?~?\s*([\d,]+)\s*tok`)

func (a *cliPluginAdapter) Details(ctx context.Context, exec *pluginExec, cwd string, record PluginRecord) (PluginDetails, error) {
	details := PluginDetails{PluginRecord: record}
	enrichPluginRecord(&details.PluginRecord)
	details.ManifestPath = findPluginManifest(record.InstallPath)
	if a.dialect != pluginDialectClaude {
		return details, nil
	}
	output := exec.run(ctx, []string{"plugin", "details", record.Name}, cwd, pluginDetailsTimeout)
	if output.Err == nil {
		if match := claudeTokenCostPattern.FindStringSubmatch(output.Stdout); len(match) == 2 {
			details.TokenCost = strings.ReplaceAll(match[1], ",", "") + " tok"
		}
	}
	return details, nil
}

// ── 写操作 ───────────────────────────────────────────────────────────────────

type pluginInstallRequest struct {
	AgentID             string `json:"agentId"`
	Plugin              string `json:"plugin"`
	Marketplace         string `json:"marketplace,omitempty"`
	Scope               string `json:"scope"`
	ProjectID           string `json:"projectId,omitempty"`
	AcceptCommandSHA256 string `json:"acceptCommandSha256,omitempty"`
}

func (a *cliPluginAdapter) Install(ctx context.Context, exec *pluginExec, cwd string, request pluginInstallRequest) (PluginActionResult, error) {
	scope := normalizePluginScope(request.Scope)
	if a.dialect == pluginDialectCodex && scope != pluginScopeUser {
		return PluginActionResult{}, errors.New("Codex 插件只能安装在用户范围")
	}
	selector := strings.TrimSpace(request.Plugin)
	source := strings.TrimSpace(request.Marketplace)
	// 选择器里已经带 @marketplace 时不能再给 --marketplace：Claude 会直接拒绝
	// （"Name the marketplace once"），Codex 的帮助也写明该参数只用于裸插件名。
	// Codex 的 --marketplace 只认市场名，不能换成 source；只有 Claude 需要解析。
	if strings.Contains(selector, "@") {
		source = ""
	} else if a.dialect == pluginDialectClaude && source != "" {
		source = a.marketplaceSource(ctx, exec, cwd, source)
	}
	args := pluginInstallArgs(a.dialect, selector, scope, source, request.AcceptCommandSHA256)
	output := exec.run(ctx, args, cwd, pluginMutationTimeout)
	return pluginActionResultFromOutput(output, "安装插件失败"), nil
}

// pluginInstallArgs 拼装安装参数。只有裸插件名才接受 marketplaceSource。
func pluginInstallArgs(dialect pluginDialect, selector, scope, marketplaceSource, acceptCommandSHA256 string) []string {
	switch dialect {
	case pluginDialectClaude:
		args := []string{"plugin", "install", selector, "--scope", scope, "--json"}
		if marketplaceSource != "" {
			args = append(args, "--marketplace", marketplaceSource)
		}
		if acceptCommandSHA256 != "" {
			args = append(args, "--accept-command", acceptCommandSHA256)
		}
		return args
	case pluginDialectQoder:
		return []string{"plugins", "install", selector, "--scope", scope, "--json"}
	default:
		args := []string{"plugin", "add", selector}
		if marketplaceSource != "" {
			args = append(args, "--marketplace", marketplaceSource)
		}
		return args
	}
}

// marketplaceSource 把市场名换成 CLI 需要的 source（owner/repo、URL 或路径）。
// Claude 的 --marketplace 只认 source，传名字会被当成一条新的市场来源。
func (a *cliPluginAdapter) marketplaceSource(ctx context.Context, exec *pluginExec, cwd, name string) string {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "://") {
		return name
	}
	list, err := a.Marketplaces(ctx, exec, cwd)
	if err != nil {
		return name
	}
	for _, market := range list {
		if market.Name == name && strings.TrimSpace(market.Source) != "" {
			return market.Source
		}
	}
	return name
}

func (a *cliPluginAdapter) SetEnabled(ctx context.Context, exec *pluginExec, cwd string, record PluginRecord, enabled bool) (PluginActionResult, error) {
	action := "enable"
	label := "启用插件失败"
	if !enabled {
		action = "disable"
		label = "禁用插件失败"
	}
	var args []string
	switch a.dialect {
	case pluginDialectClaude:
		args = []string{"plugin", action, record.ID, "--scope", normalizePluginScope(record.Scope), "--json"}
	case pluginDialectQoder:
		args = []string{"plugins", action, record.ID, "--scope", normalizePluginScope(record.Scope)}
	default:
		return PluginActionResult{}, errors.New("该 CLI 不支持启用/禁用插件")
	}
	output := exec.run(ctx, args, cwd, pluginMutationTimeout)
	return pluginActionResultFromOutput(output, label), nil
}

func (a *cliPluginAdapter) Update(ctx context.Context, exec *pluginExec, cwd string, record PluginRecord, acceptCommandSHA256 string) (PluginActionResult, error) {
	args, err := pluginUpdateArgs(a.dialect, record, acceptCommandSHA256)
	if err != nil {
		return PluginActionResult{}, err
	}
	output := exec.run(ctx, args, cwd, pluginMutationTimeout)
	return pluginActionResultFromOutput(output, "升级插件失败"), nil
}

func pluginUpdateArgs(dialect pluginDialect, record PluginRecord, acceptCommandSHA256 string) ([]string, error) {
	switch dialect {
	case pluginDialectClaude:
		// 同 ID 多范围安装时，缺 --scope 会落到 auto-detect 的另一个范围上。
		args := []string{"plugin", "update", record.ID, "--scope", normalizePluginScope(record.Scope), "--json"}
		if acceptCommandSHA256 != "" {
			args = append(args, "--accept-command", acceptCommandSHA256)
		}
		return args, nil
	case pluginDialectQoder:
		return []string{"plugins", "update", record.ID, "--scope", normalizePluginScope(record.Scope)}, nil
	default:
		return nil, errors.New("该 CLI 不支持升级插件")
	}
}

func (a *cliPluginAdapter) Uninstall(ctx context.Context, exec *pluginExec, cwd string, record PluginRecord) (PluginActionResult, error) {
	var args []string
	switch a.dialect {
	case pluginDialectClaude:
		args = []string{"plugin", "uninstall", record.ID, "--scope", normalizePluginScope(record.Scope), "--json"}
	case pluginDialectQoder:
		args = []string{"plugins", "uninstall", record.ID, "--scope", normalizePluginScope(record.Scope)}
	default:
		args = []string{"plugin", "remove", record.ID}
	}
	output := exec.run(ctx, args, cwd, pluginMutationTimeout)
	return pluginActionResultFromOutput(output, "卸载插件失败"), nil
}

func pluginActionResultFromOutput(output pluginCommandOutput, failure string) PluginActionResult {
	message := strings.TrimSpace(output.Stdout)
	if message == "" {
		message = strings.TrimSpace(output.Stderr)
	}
	result := PluginActionResult{OK: output.Err == nil, Message: message}
	if command, sha := pluginShownCommand(output.Stdout); sha != "" {
		// CLI 拒绝执行 marketplace 声明的命令，并把命令与 sha256 报给了调用方。
		// 这里只把它们原样交出去，等用户确认后再带 --accept-command 重试。
		result.OK = false
		result.NeedsConfirmation = true
		result.Command = command
		result.CommandSHA256 = sha
		result.Message = "该插件声明了待执行命令，确认后才能继续"
		return result
	}
	if output.Err != nil {
		detail := output.detail()
		if detail == "" {
			detail = failure
		}
		result.Message = detail
		result.Output = message
	}
	return result
}

// pluginShownCommand 从 CLI 的 JSON 输出里取 shownCommand.command / sha256。
// 目前只有 Claude 使用这一协议；取不到就返回空串，调用方照常报错。
func pluginShownCommand(raw string) (string, string) {
	data, ok := extractJSON(raw)
	if !ok {
		return "", ""
	}
	var payload any
	if json.Unmarshal(data, &payload) != nil {
		return "", ""
	}
	return findShownCommand(payload)
}

func findShownCommand(value any) (string, string) {
	switch typed := value.(type) {
	case map[string]any:
		if raw, ok := typed["shownCommand"]; ok {
			if shown, ok := raw.(map[string]any); ok {
				sha, _ := shown["sha256"].(string)
				command, _ := shown["command"].(string)
				if sha != "" {
					return command, sha
				}
			}
		}
		for _, child := range typed {
			if command, sha := findShownCommand(child); sha != "" {
				return command, sha
			}
		}
	case []any:
		for _, child := range typed {
			if command, sha := findShownCommand(child); sha != "" {
				return command, sha
			}
		}
	}
	return "", ""
}

// ── 市场 ─────────────────────────────────────────────────────────────────────

func (a *cliPluginAdapter) Marketplaces(ctx context.Context, exec *pluginExec, cwd string) ([]PluginMarketplace, error) {
	args := []string{a.verb(), "marketplace", "list", "--json"}
	output := exec.run(ctx, args, cwd, pluginListTimeout)
	if output.Err != nil {
		return nil, pluginCommandError("读取插件市场失败", output)
	}
	raw, ok := extractJSON(output.Stdout)
	if !ok {
		return nil, pluginJSONParseError(output.Stdout, "插件市场")
	}
	return parsePluginMarketplaces(a.dialect, raw)
}

func parsePluginMarketplaces(dialect pluginDialect, raw []byte) ([]PluginMarketplace, error) {
	switch dialect {
	case pluginDialectClaude:
		var entries []struct {
			Name            string `json:"name"`
			Source          any    `json:"source"`
			Repo            string `json:"repo"`
			InstallLocation string `json:"installLocation"`
		}
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, err
		}
		out := make([]PluginMarketplace, 0, len(entries))
		for _, item := range entries {
			out = append(out, PluginMarketplace{
				Name: item.Name, Kind: pluginSourceKind(item.Source, item.Repo),
				Source: pluginSourceText(item.Source, item.Repo), InstallLocation: item.InstallLocation,
			})
		}
		return out, nil
	case pluginDialectQoder:
		var entries []struct {
			Name            string `json:"name"`
			Source          any    `json:"source"`
			InstallLocation string `json:"installLocation"`
			LastUpdated     string `json:"lastUpdated"`
		}
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, err
		}
		out := make([]PluginMarketplace, 0, len(entries))
		for _, item := range entries {
			out = append(out, PluginMarketplace{
				Name: item.Name, Kind: pluginSourceKind(item.Source, ""),
				Source: pluginSourceText(item.Source, ""), InstallLocation: item.InstallLocation,
				LastUpdated: item.LastUpdated,
			})
		}
		return out, nil
	default:
		var payload struct {
			Marketplaces []struct {
				Name string `json:"name"`
				Root string `json:"root"`
			} `json:"marketplaces"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return nil, err
		}
		out := make([]PluginMarketplace, 0, len(payload.Marketplaces))
		for _, item := range payload.Marketplaces {
			out = append(out, PluginMarketplace{Name: item.Name, Kind: "local", Source: item.Root, InstallLocation: item.Root})
		}
		return out, nil
	}
}

func pluginSourceKind(source any, repo string) string {
	if repo != "" {
		return "github"
	}
	switch typed := source.(type) {
	case string:
		return typed
	case map[string]any:
		if value, ok := typed["source"].(string); ok {
			return value
		}
	}
	return ""
}

func pluginSourceText(source any, repo string) string {
	if repo != "" {
		return repo
	}
	switch typed := source.(type) {
	case string:
		return typed
	case map[string]any:
		if value, ok := typed["url"].(string); ok && value != "" {
			return value
		}
		if value, ok := typed["repo"].(string); ok && value != "" {
			return value
		}
		if value, ok := typed["path"].(string); ok && value != "" {
			return value
		}
		encoded, err := json.Marshal(typed)
		if err == nil {
			return string(encoded)
		}
	}
	return ""
}

func (a *cliPluginAdapter) MarketplaceAdd(ctx context.Context, exec *pluginExec, cwd, source, scope string) (PluginActionResult, error) {
	// Codex 的市场来源是全局配置，没有 --scope；Claude/Qoder 支持按范围声明。
	args := []string{a.verb(), "marketplace", "add", source}
	switch a.dialect {
	case pluginDialectCodex:
		args = append(args, "--json")
	default:
		args = append(args, "--scope", normalizePluginScope(scope))
		if a.dialect == pluginDialectClaude {
			args = append(args, "--json")
		}
	}
	output := exec.run(ctx, args, cwd, pluginMutationTimeout)
	return pluginActionResultFromOutput(output, "添加插件市场失败"), nil
}

func (a *cliPluginAdapter) MarketplaceRemove(ctx context.Context, exec *pluginExec, cwd, name string) (PluginActionResult, error) {
	args := []string{a.verb(), "marketplace", "remove", name}
	if a.dialect == pluginDialectQoder {
		args = []string{"plugins", "marketplace", "remove", name}
	} else if a.dialect == pluginDialectClaude {
		args = append(args, "--json")
	}
	output := exec.run(ctx, args, cwd, pluginMutationTimeout)
	return pluginActionResultFromOutput(output, "移除插件市场失败"), nil
}

func (a *cliPluginAdapter) MarketplaceUpdate(ctx context.Context, exec *pluginExec, cwd, name string) (PluginActionResult, error) {
	var args []string
	switch a.dialect {
	case pluginDialectClaude:
		args = []string{"plugin", "marketplace", "update", "--json"}
		if name != "" {
			args = append(args, name)
		}
	case pluginDialectQoder:
		args = []string{"plugins", "marketplace", "update"}
		if name != "" {
			args = append(args, name)
		}
	default:
		args = []string{"plugin", "marketplace", "upgrade"}
		if name != "" {
			args = append(args, name)
		}
	}
	output := exec.run(ctx, args, cwd, pluginMutationTimeout)
	return pluginActionResultFromOutput(output, "刷新插件市场失败"), nil
}

// ── 磁盘信息补全 ─────────────────────────────────────────────────────────────

// pluginRecordKey 用 id/scope/projectPath 三者定位一条安装记录。
// Windows 路径不含 "|"，其余平台的路径理论上可能包含；仅用于前端 key 与操作定位，
// 不参与 CLI 调用。
func pluginRecordKey(record PluginRecord) string {
	return record.ID + "|" + record.Scope + "|" + record.ProjectPath
}

func pluginNameFromID(id string) string {
	if index := strings.IndexByte(id, '@'); index > 0 {
		return id[:index]
	}
	return id
}

func pluginMarketplaceFromID(id string) string {
	if index := strings.IndexByte(id, '@'); index >= 0 && index+1 < len(id) {
		return id[index+1:]
	}
	return ""
}

func normalizePluginScope(scope string) string {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case pluginScopeProject:
		return pluginScopeProject
	case pluginScopeLocal:
		return pluginScopeLocal
	default:
		return pluginScopeUser
	}
}

func pluginManifestCandidates(base string) []string {
	return []string{
		filepath.Join(base, ".claude-plugin", "plugin.json"),
		filepath.Join(base, ".codex-plugin", "plugin.json"),
		filepath.Join(base, ".qoder-plugin", "plugin.json"),
		filepath.Join(base, ".codebuddy-plugin", "plugin.json"),
		filepath.Join(base, ".cursor-plugin", "plugin.json"),
		filepath.Join(base, "plugin.json"),
	}
}

func findPluginManifest(base string) string {
	if strings.TrimSpace(base) == "" {
		return ""
	}
	for _, candidate := range pluginManifestCandidates(base) {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate
		}
	}
	return ""
}

func enrichPluginRecord(record *PluginRecord) {
	if strings.TrimSpace(record.InstallPath) == "" {
		return
	}
	manifest := findPluginManifest(record.InstallPath)
	if manifest != "" {
		// 清单是别人仓库里的文件，超过 128KB 的只可能是打包产物或恶意大文件；
		// 直接跳过，避免列表热路径把整份内容读进内存。
		if info, err := os.Stat(manifest); err == nil && info.Size() <= pluginManifestSizeLimit {
			if data, err := os.ReadFile(manifest); err == nil {
				var payload struct {
					Name        string `json:"name"`
					Version     string `json:"version"`
					Description string `json:"description"`
					DisplayName string `json:"displayName"`
				}
				if json.Unmarshal(data, &payload) == nil {
					if record.Name == "" {
						record.Name = payload.Name
					}
					if record.DisplayName == "" {
						record.DisplayName = payload.DisplayName
					}
					if record.Description == "" {
						record.Description = payload.Description
					}
					if record.Version == "" {
						record.Version = payload.Version
					}
				}
			}
		}
	}
	if record.Components == (PluginComponents{}) {
		record.Components = countPluginComponents(record.InstallPath)
	}
}

func countPluginComponents(base string) PluginComponents {
	var counts PluginComponents
	if strings.TrimSpace(base) == "" {
		return counts
	}
	visited := 0
	_ = filepath.WalkDir(base, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		visited++
		if visited > 20000 {
			return fs.SkipAll
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "node_modules" || entry.Name() == ".hg" {
				return filepath.SkipDir
			}
			return nil
		}
		name := entry.Name()
		parent := strings.ToLower(filepath.Base(filepath.Dir(path)))
		switch {
		case strings.EqualFold(name, "SKILL.md"):
			counts.Skills++
		case parent == "commands" && strings.EqualFold(filepath.Ext(name), ".md"):
			counts.Commands++
		case parent == "agents" && strings.EqualFold(filepath.Ext(name), ".md"):
			counts.Agents++
		case strings.EqualFold(name, "hooks.json"):
			counts.Hooks++
		case strings.EqualFold(name, ".mcp.json") || strings.EqualFold(name, "mcp.json"):
			if servers, ok := countJSONMapEntries(path, "mcpServers"); ok {
				counts.MCPServers += servers
			}
		case strings.EqualFold(name, ".lsp.json") || strings.EqualFold(name, "lsp.json"):
			if servers, ok := countJSONMapEntries(path, ""); ok {
				counts.LSPServers += servers
			}
		}
		return nil
	})
	return counts
}

func countJSONMapEntries(path, key string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 512*1024 {
		return 0, false
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(data, &payload) != nil {
		return 0, false
	}
	if key == "" {
		return len(payload), true
	}
	raw, ok := payload[key]
	if !ok {
		return 0, false
	}
	var entries map[string]json.RawMessage
	if json.Unmarshal(raw, &entries) != nil {
		return 0, false
	}
	return len(entries), true
}

// ── 能力探测与缓存 ───────────────────────────────────────────────────────────

type pluginCapabilitiesCache struct {
	at    time.Time
	value PluginCapabilities
}

func (s *Server) pluginCapabilities(ctx context.Context, runnerID, agentID string) PluginCapabilities {
	adapter, ok := pluginAdapterFor(agentID)
	if !ok {
		return PluginCapabilities{}
	}
	key := runnerID + "\x00" + agentID
	s.pluginCapsMu.Lock()
	if entry, ok := s.pluginCaps[key]; ok && time.Since(entry.at) < pluginCapsTTL {
		value := entry.value
		s.pluginCapsMu.Unlock()
		return value
	}
	s.pluginCapsMu.Unlock()

	fallback := adapter.Capabilities()
	exec := &pluginExec{server: s, runnerID: runnerID, agentID: agentID}
	output := exec.run(ctx, []string{pluginAdapterVerb(adapter), "--help"}, "", 10*time.Second)
	value := fallback
	if output.Err == nil {
		value = pluginCapabilitiesFromHelp(output.Stdout+"\n"+output.Stderr, fallback)
	}
	s.pluginCapsMu.Lock()
	if s.pluginCaps == nil {
		s.pluginCaps = map[string]pluginCapabilitiesCache{}
	}
	s.pluginCaps[key] = pluginCapabilitiesCache{at: time.Now(), value: value}
	s.pluginCapsMu.Unlock()
	return value
}

// pluginCapabilitiesFromHelp 用 --help 收缩适配器声明的能力：帮助只说明 CLI
// 有没有这个子命令，因此只能把动作缩到 false，不能扩张。
//
// Details 是例外：它由适配器在本地补全（清单路径、组件数），不依赖 details
// 子命令；Qoder/Codex 的帮助里没有 details，但详情仍然可用，不能一起被判没。
func pluginCapabilitiesFromHelp(help string, fallback PluginCapabilities) PluginCapabilities {
	help = strings.ToLower(help)
	// 帮助输出缺项不等于 CLI 不支持：只有明确读到子命令表时才收缩能力。
	if !strings.Contains(help, "commands:") && !strings.Contains(help, "usage:") {
		return fallback
	}
	return PluginCapabilities{
		List:         strings.Contains(help, " list") && fallback.List,
		Details:      fallback.Details,
		Install:      (strings.Contains(help, "install") || strings.Contains(help, " add")) && fallback.Install,
		Uninstall:    (strings.Contains(help, "uninstall") || strings.Contains(help, "remove")) && fallback.Uninstall,
		Enable:       strings.Contains(help, "enable") && fallback.Enable,
		Disable:      strings.Contains(help, "disable") && fallback.Disable,
		Update:       (strings.Contains(help, "update") || strings.Contains(help, "upgrade")) && fallback.Update,
		Marketplaces: strings.Contains(help, "marketplace") && fallback.Marketplaces,
		// 范围由适配器决定，帮助文本无法判断，原样保留。
		Scopes: fallback.Scopes,
	}
}

type pluginCacheEntry struct {
	at   time.Time
	view PluginListView
}

func (s *Server) invalidatePluginCache(runnerID, agentID string) {
	prefix := runnerID + "\x00" + agentID + "\x00"
	s.pluginCacheMu.Lock()
	for key := range s.pluginCache {
		if strings.HasPrefix(key, prefix) {
			delete(s.pluginCache, key)
		}
	}
	// 正在进行中的列表查询可能在写操作之后完成并把旧结果写回缓存；
	// 递增代数让这些回填失效（见 loadPluginList 的比对）。
	s.pluginCacheEpoch++
	s.pluginCacheMu.Unlock()

	// 在途的单飞如果在写操作之前就读完了清单，会继续把旧状态发给新加入的请求；
	// 摘掉登记，让后续请求重新查询。老的 leader 完成时只删自己那条（见 pluginListWith）。
	s.pluginFlightMu.Lock()
	for key := range s.pluginFlights {
		if strings.HasPrefix(key, prefix) {
			delete(s.pluginFlights, key)
		}
	}
	s.pluginFlightMu.Unlock()
}

type pluginListFlight struct {
	done chan struct{}
	view PluginListView
	err  error
}

func (s *Server) pluginList(ctx context.Context, runnerID, agentID, projectPath string, includeAvailable bool) (PluginListView, error) {
	return s.pluginListWith(ctx, runnerID, agentID, projectPath, includeAvailable, true)
}

// pluginListWith 的 withMarketplaces=false 给"只关心启用状态"的场景（技能过滤）用：
// 市场列表是另一次 CLI 调用，和插件是否启用无关。
func (s *Server) pluginListWith(ctx context.Context, runnerID, agentID, projectPath string, includeAvailable, withMarketplaces bool) (PluginListView, error) {
	// 请求已经结束时不要再拉起新一轮 CLI（调用方常常是只按 agent 循环的列表接口，
	// 客户端走了还继续跑就是纯浪费）。
	if err := ctx.Err(); err != nil {
		return PluginListView{}, err
	}
	adapter, ok := pluginAdapterFor(agentID)
	// 没有路径解析器的 Server 不知道工具装在哪（测试夹具 / 未完成初始化的实例），
	// 退回磁盘扫描，绝不在 PATH 上猜一个同名命令去执行。
	if !ok || s.paths == nil {
		return s.scanPluginList(ctx, runnerID, agentID, projectPath)
	}
	if runnerID == "wsl-local" && s.runnerRegistry != nil {
		s.ensureWSLRunner()
	}
	key := keyForPluginList(runnerID, agentID, projectPath, includeAvailable, withMarketplaces)
	s.pluginCacheMu.Lock()
	if entry, ok := s.pluginCache[key]; ok && time.Since(entry.at) < pluginCacheTTL {
		value := entry.view
		s.pluginCacheMu.Unlock()
		return value, nil
	}
	epoch := s.pluginCacheEpoch
	s.pluginCacheMu.Unlock()

	// 同一份清单常常被两处同时要（技能过滤与插件列表），冷启动时不该各拉一个 CLI 进程。
	// 同 key 的并发请求合并成一次执行，其余等待结果。
	s.pluginFlightMu.Lock()
	if s.pluginFlights == nil {
		s.pluginFlights = map[string]*pluginListFlight{}
	}
	if flight, ok := s.pluginFlights[key]; ok {
		s.pluginFlightMu.Unlock()
		select {
		case <-flight.done:
			return flight.view, flight.err
		case <-ctx.Done():
			return PluginListView{}, ctx.Err()
		}
	}
	flight := &pluginListFlight{done: make(chan struct{})}
	s.pluginFlights[key] = flight
	s.pluginFlightMu.Unlock()

	// 查询放到后台执行：
	//   - 等待者各有各的预算，leader 的客户端断开不能连坐其他人（结果还会进缓存，
	//     让后来者直接命中，而不是让一条已断开请求白跑一遍 CLI）；
	//   - 后台执行绑定 runtimeCtx：服务关闭就取消，不留下跑满 90 秒的孤儿 CLI；
	//   - leader 自己也按自己的 ctx 等待，客户端走了就立刻返回。
	// 摘登记与 close(done) 必须走 defer：任何 panic 若跳过这两步，同 key 的后续请求
	// 会一直等一条永不关闭的 flight。这里不能重新 panic —— 后台 goroutine 的 panic
	// 会直接结束进程，记录成错误交给等待者即可。
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				flight.view, flight.err = PluginListView{}, fmt.Errorf("插件清单查询异常: %v", recovered)
				log.Printf("[plugin] %v", flight.err)
			}
			s.pluginFlightMu.Lock()
			// 期间可能被 invalidatePluginCache 摘掉并换成了新 flight，只删自己那条。
			if s.pluginFlights[key] == flight {
				delete(s.pluginFlights, key)
			}
			s.pluginFlightMu.Unlock()
			close(flight.done)
		}()
		baseCtx := s.runtimeCtx
		if baseCtx == nil {
			baseCtx = context.Background()
		}
		leaderCtx, cancel := context.WithTimeout(baseCtx, pluginListFlightTimeout)
		defer cancel()
		flight.view, flight.err = s.loadPluginList(leaderCtx, adapter, runnerID, agentID, projectPath, includeAvailable, withMarketplaces, epoch)
	}()

	select {
	case <-flight.done:
		return flight.view, flight.err
	case <-ctx.Done():
		return PluginListView{}, ctx.Err()
	}
}

func (s *Server) loadPluginList(ctx context.Context, adapter pluginAdapter, runnerID, agentID, projectPath string, includeAvailable, withMarketplaces bool, epoch uint64) (PluginListView, error) {
	exec := &pluginExec{server: s, runnerID: runnerID, agentID: agentID}
	records, available, err := adapter.List(ctx, exec, projectPath, includeAvailable)
	if err != nil {
		return PluginListView{}, err
	}
	var marketplaces []PluginMarketplace
	note := ""
	if withMarketplaces {
		var marketErr error
		marketplaces, marketErr = adapter.Marketplaces(ctx, exec, projectPath)
		if marketErr != nil {
			note = marketErr.Error()
		}
	}
	view := PluginListView{
		RunnerID: runnerID, AgentID: agentID, Env: s.pluginEnvironmentLabel(runnerID),
		Source: pluginSourceCLI, Capabilities: s.pluginCapabilities(ctx, runnerID, agentID),
		Plugins: records, Available: available, Marketplaces: marketplaces, Note: note,
	}
	if view.Plugins == nil {
		view.Plugins = []PluginRecord{}
	}
	if view.Marketplaces == nil {
		view.Marketplaces = []PluginMarketplace{}
	}
	s.pluginCacheMu.Lock()
	if s.pluginCache == nil {
		s.pluginCache = map[string]pluginCacheEntry{}
	}
	// 写操作已经发生过（epoch 变了）就不要再把这份旧清单写回缓存。
	if s.pluginCacheEpoch == epoch {
		now := time.Now()
		// TTL 只在读时判断，没人再访问的 key 永远不会被删除；而 key 里含 projectPath
		// （每个会话 worktree 都会产生新 key），长期运行会单调增长。写入时顺带清过期项。
		for existing, entry := range s.pluginCache {
			if now.Sub(entry.at) >= pluginCacheTTL {
				delete(s.pluginCache, existing)
			}
		}
		s.pluginCache[keyForPluginList(runnerID, agentID, projectPath, includeAvailable, withMarketplaces)] = pluginCacheEntry{at: now, view: view}
	}
	s.pluginCacheMu.Unlock()
	return view, nil
}

func keyForPluginList(runnerID, agentID, projectPath string, includeAvailable, withMarketplaces bool) string {
	return runnerID + "\x00" + agentID + "\x00" + projectPath + "\x00" + strconv.FormatBool(includeAvailable) + "\x00" + strconv.FormatBool(withMarketplaces)
}

func (s *Server) pluginEnvironmentLabel(runnerID string) string {
	if s.runnerRegistry != nil {
		if meta, ok := s.runnerRegistry.getMeta(runnerID); ok && strings.TrimSpace(meta.Environment) != "" {
			return meta.Environment
		}
	}
	return string(s.resolveAgentTargetEnv(runnerID, ""))
}

// scanPluginList 是 CLI 不支持查询时的兜底：只报"磁盘上发现了什么"，状态明确未知。
func (s *Server) scanPluginList(ctx context.Context, runnerID, agentID, projectPath string) (PluginListView, error) {
	if !isLocalRunnerID(runnerID) {
		return PluginListView{
			RunnerID: runnerID, AgentID: agentID, Env: s.pluginEnvironmentLabel(runnerID),
			Source: pluginSourceScan, Capabilities: PluginCapabilities{},
			Plugins: []PluginRecord{}, Marketplaces: []PluginMarketplace{},
			Note: "该 CLI 没有插件管理命令，且远端清单暂不扫描。",
		}, nil
	}
	records := []PluginRecord{}
	seen := map[string]bool{}
	home := homeDir()
	type scanRoot struct {
		path        string
		scope       string
		projectPath string
	}
	for _, dir := range pluginDirectories {
		if agentID != "" && agentID != dir.agent {
			continue
		}
		var roots []scanRoot
		if home != "" {
			roots = append(roots, scanRoot{path: filepath.Join(home, dir.root), scope: pluginScopeUser})
		}
		if projectPath != "" {
			roots = append(roots, scanRoot{path: filepath.Join(projectPath, dir.root), scope: pluginScopeProject, projectPath: projectPath})
		}
		for _, root := range roots {
			for _, plugin := range scanPluginManifests(ctx, root.path, dir.agent, dir.manifest) {
				key := plugin.Agent + "\x00" + plugin.Name + "\x00" + plugin.Version + "\x00" + root.scope + "\x00" + pluginPathKey(root.projectPath)
				if seen[key] {
					continue
				}
				seen[key] = true
				record := PluginRecord{
					ID: plugin.Name, Name: plugin.Name, Version: plugin.Version,
					Description: plugin.Description, Scope: root.scope, ProjectPath: root.projectPath,
					StateKnown: false, CanDisable: false,
				}
				record.Key = pluginRecordKey(record)
				records = append(records, record)
			}
		}
	}
	sort.SliceStable(records, func(i, j int) bool { return records[i].Name < records[j].Name })
	return PluginListView{
		RunnerID: runnerID, AgentID: agentID, Env: s.pluginEnvironmentLabel(runnerID),
		Source: pluginSourceScan, Capabilities: PluginCapabilities{},
		Plugins: records, Marketplaces: []PluginMarketplace{},
		Note: "该 CLI 没有提供插件管理命令；这里只列出磁盘上发现的插件，启用状态未知。",
	}, nil
}

// ── HTTP 处理器 ──────────────────────────────────────────────────────────────

// PluginAgentInfo 是插件页可选工具的目录项：Managed=false 表示只能只读扫描。
type PluginAgentInfo struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Managed bool   `json:"managed"`
}

// listPluginAgents 是 GET /api/plugin-agents：插件页的工具清单只从这里取。
func (s *Server) listPluginAgents(w http.ResponseWriter, r *http.Request) {
	seen := map[string]bool{}
	out := []PluginAgentInfo{}
	for _, dir := range pluginDirectories {
		if seen[dir.agent] {
			continue
		}
		seen[dir.agent] = true
		entry, ok := agentByID(dir.agent)
		if !ok {
			continue
		}
		_, managed := pluginAdapterFor(dir.agent)
		out = append(out, PluginAgentInfo{ID: dir.agent, Name: entry.Name, Managed: managed})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Managed != out[j].Managed {
			return out[i].Managed
		}
		return out[i].Name < out[j].Name
	})
	writeJSON(w, http.StatusOK, out)
}

func pluginQueryAgent(r *http.Request) string {
	agentID := strings.TrimSpace(r.URL.Query().Get("agentId"))
	if agentID == "" {
		agentID = "claude-code"
	}
	return agentID
}

// pluginPathParam 取路径参数并做百分号解码。前端用 encodeURIComponent 传插件 ID，
// 其中的 "@" 会变成 "%40"；chi 在 RawPath 上匹配、URLParam 不做解码，直接比对会查不到。
func pluginPathParam(r *http.Request, name string) string {
	value := chi.URLParam(r, name)
	if decoded, err := url.PathUnescape(value); err == nil {
		return decoded
	}
	return value
}

// findPluginRecord 按 id 定位记录。同一插件可以同时装在 user 与 project 两个范围，
// 只按 id 取第一条会把操作落到错误的范围上；scope 与 projectPath 用来消歧。
func findPluginRecord(records []PluginRecord, id, scope, recordProjectPath string) (PluginRecord, bool) {
	var candidates []PluginRecord
	wantedScope := ""
	if strings.TrimSpace(scope) != "" {
		wantedScope = normalizePluginScope(scope)
	}
	for _, item := range records {
		if item.ID != id {
			continue
		}
		if wantedScope != "" && item.Scope != wantedScope {
			continue
		}
		candidates = append(candidates, item)
	}
	if len(candidates) == 0 {
		return PluginRecord{}, false
	}
	if recordProjectPath != "" {
		want := pluginPathKey(recordProjectPath)
		for _, item := range candidates {
			if pluginPathKey(item.ProjectPath) == want {
				return item, true
			}
		}
		// 指定了项目却没有任何记录匹配：这条记录可能已在 CLI 侧被卸载或换了项目。
		// 退回第一条会把操作落到另一个项目的安装上，宁可让调用方重新拉列表。
		return PluginRecord{}, false
	}
	return candidates[0], true
}

func (s *Server) pluginProjectPath(ctx context.Context, runnerID, projectID string) (string, error) {
	if strings.TrimSpace(projectID) == "" {
		return "", nil
	}
	project, err := s.getProjectByID(ctx, projectID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", errors.New("项目不存在")
		}
		return "", err
	}
	if project.Runner != "" && project.Runner != runnerID {
		return "", fmt.Errorf("项目不在该执行环境上（项目 runner=%s）", project.Runner)
	}
	return project.Path, nil
}

// listRunnerPlugins 是 GET /api/runners/{runnerID}/plugins。
func (s *Server) listRunnerPlugins(w http.ResponseWriter, r *http.Request) {
	runnerID := chi.URLParam(r, "runnerID")
	agentID := pluginQueryAgent(r)
	projectPath, err := s.pluginProjectPath(r.Context(), runnerID, r.URL.Query().Get("projectId"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if _, ok := agentByID(agentID); !ok {
		writeError(w, http.StatusBadRequest, fmt.Errorf("不支持的工具 %s", agentID))
		return
	}
	view, err := s.pluginList(r.Context(), runnerID, agentID, projectPath, r.URL.Query().Get("available") == "1")
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) pluginDetailsHandler(w http.ResponseWriter, r *http.Request) {
	runnerID := chi.URLParam(r, "runnerID")
	agentID := pluginQueryAgent(r)
	adapter, ok := pluginAdapterFor(agentID)
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Errorf("该 CLI 不支持插件详情"))
		return
	}
	projectPath, err := s.pluginProjectPath(r.Context(), runnerID, r.URL.Query().Get("projectId"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	pluginID := pluginPathParam(r, "pluginID")
	// 详情与写操作只需要记录本身，不需要市场清单，别为它们多跑一次 CLI。
	view, err := s.pluginListWith(r.Context(), runnerID, agentID, projectPath, false, false)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	record, found := findPluginRecord(view.Plugins, pluginID, r.URL.Query().Get("scope"), r.URL.Query().Get("recordProjectPath"))
	if !found {
		writeError(w, http.StatusNotFound, errors.New("插件不存在"))
		return
	}
	details, err := adapter.Details(r.Context(), &pluginExec{server: s, runnerID: runnerID, agentID: agentID}, projectPath, record)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, details)
}

func (s *Server) installRunnerPlugin(w http.ResponseWriter, r *http.Request) {
	runnerID := chi.URLParam(r, "runnerID")
	var request pluginInstallRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("请求格式无效"))
		return
	}
	if err := validatePluginSelector(request.Plugin); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := validatePluginName("市场名称", request.Marketplace, true); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	agentID := firstNonEmpty(request.AgentID, pluginQueryAgent(r))
	adapter, ok := pluginAdapterFor(agentID)
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Errorf("该 CLI 不支持插件安装"))
		return
	}
	scope := normalizePluginScope(request.Scope)
	projectPath, err := s.pluginProjectPath(r.Context(), runnerID, request.ProjectID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if scope != pluginScopeUser && projectPath == "" {
		writeError(w, http.StatusBadRequest, errors.New("项目范围操作需要选择一个项目"))
		return
	}
	if !pluginScopeSupported(adapter, scope) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("该 CLI 不支持 %s 范围的插件安装", scope))
		return
	}
	s.pluginMutationMu.Lock()
	defer s.pluginMutationMu.Unlock()
	result, err := adapter.Install(r.Context(), &pluginExec{server: s, runnerID: runnerID, agentID: agentID}, projectPath, request)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.invalidatePluginCache(runnerID, agentID)
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) setRunnerPluginEnabled(w http.ResponseWriter, r *http.Request) {
	runnerID := chi.URLParam(r, "runnerID")
	pluginID := pluginPathParam(r, "pluginID")
	// 启用/禁用是两条路径，状态以路径为准；请求体只承载范围与项目。
	enabled := !strings.HasSuffix(r.URL.Path, "/disable")
	var request struct {
		AgentID           string `json:"agentId"`
		Scope             string `json:"scope"`
		ProjectID         string `json:"projectId,omitempty"`
		RecordProjectPath string `json:"recordProjectPath,omitempty"`
	}
	_ = json.NewDecoder(r.Body).Decode(&request)
	s.runPluginMutation(w, r, runnerID, firstNonEmpty(request.AgentID, pluginQueryAgent(r)), pluginID, request.Scope, request.ProjectID, request.RecordProjectPath, func(adapter pluginAdapter, exec *pluginExec, projectPath string, record PluginRecord) (PluginActionResult, error) {
		return adapter.SetEnabled(r.Context(), exec, projectPath, record, enabled)
	})
}

func (s *Server) updateRunnerPlugin(w http.ResponseWriter, r *http.Request) {
	runnerID := chi.URLParam(r, "runnerID")
	pluginID := pluginPathParam(r, "pluginID")
	var request struct {
		AgentID             string `json:"agentId"`
		Scope               string `json:"scope"`
		ProjectID           string `json:"projectId,omitempty"`
		RecordProjectPath   string `json:"recordProjectPath,omitempty"`
		AcceptCommandSHA256 string `json:"acceptCommandSha256,omitempty"`
	}
	_ = json.NewDecoder(r.Body).Decode(&request)
	s.runPluginMutation(w, r, runnerID, firstNonEmpty(request.AgentID, pluginQueryAgent(r)), pluginID, request.Scope, request.ProjectID, request.RecordProjectPath, func(adapter pluginAdapter, exec *pluginExec, projectPath string, record PluginRecord) (PluginActionResult, error) {
		return adapter.Update(r.Context(), exec, projectPath, record, request.AcceptCommandSHA256)
	})
}

func (s *Server) uninstallRunnerPlugin(w http.ResponseWriter, r *http.Request) {
	runnerID := chi.URLParam(r, "runnerID")
	pluginID := pluginPathParam(r, "pluginID")
	s.runPluginMutation(w, r, runnerID, pluginQueryAgent(r), pluginID, r.URL.Query().Get("scope"), r.URL.Query().Get("projectId"), r.URL.Query().Get("recordProjectPath"), func(adapter pluginAdapter, exec *pluginExec, projectPath string, record PluginRecord) (PluginActionResult, error) {
		return adapter.Uninstall(r.Context(), exec, projectPath, record)
	})
}

func (s *Server) runPluginMutation(w http.ResponseWriter, r *http.Request, runnerID, agentID, pluginID, scope, projectID, recordProjectPath string, mutate func(pluginAdapter, *pluginExec, string, PluginRecord) (PluginActionResult, error)) {
	adapter, ok := pluginAdapterFor(agentID)
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Errorf("该 CLI 不支持插件管理"))
		return
	}
	if strings.TrimSpace(scope) != "" && !pluginScopeSupported(adapter, scope) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("该 CLI 不支持 %s 范围的插件操作", normalizePluginScope(scope)))
		return
	}
	projectPath, err := s.pluginProjectPath(r.Context(), runnerID, projectID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	view, err := s.pluginListWith(r.Context(), runnerID, agentID, projectPath, false, false)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	record, found := findPluginRecord(view.Plugins, pluginID, scope, recordProjectPath)
	if !found {
		writeError(w, http.StatusNotFound, errors.New("插件不存在"))
		return
	}
	// 项目范围的插件本身带着安装时记录的项目路径；未显式选项目时沿用它，
	// 这样开关/升级/卸载不必再让用户挑一次项目。
	if projectPath == "" && record.ProjectPath != "" && record.Scope != pluginScopeUser {
		projectPath = record.ProjectPath
	}
	s.pluginMutationMu.Lock()
	defer s.pluginMutationMu.Unlock()
	result, err := mutate(adapter, &pluginExec{server: s, runnerID: runnerID, agentID: agentID}, projectPath, record)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.invalidatePluginCache(runnerID, agentID)
	writeJSON(w, http.StatusOK, result)
}

func validatePluginSelector(value string) error {
	return validatePluginName("插件标识", value, false)
}

// validatePluginName 校验交给 CLI 的纯名称参数（插件 ID、市场名）。这些值会被拼进
// 子进程参数，禁止前导 "-"（会被当成新选项）与控制字符。
func validatePluginName(label, value string, allowEmpty bool) error {
	value = strings.TrimSpace(value)
	if value == "" {
		if allowEmpty {
			return nil
		}
		return fmt.Errorf("%s不能为空", label)
	}
	if len(value) > 512 {
		return fmt.Errorf("%s过长", label)
	}
	if strings.HasPrefix(value, "-") || strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("%s无效", label)
	}
	return nil
}

// pluginScopeSupported 判断该 CLI 是否真的支持某个安装范围。
// Codex 的市场与安装都是全局配置，前端不应把它显示成可选项目范围。
func pluginScopeSupported(adapter pluginAdapter, scope string) bool {
	wanted := normalizePluginScope(scope)
	for _, item := range adapter.Capabilities().Scopes {
		if item == wanted {
			return true
		}
	}
	return false
}

// ── 市场 HTTP ────────────────────────────────────────────────────────────────

func (s *Server) addPluginMarketplace(w http.ResponseWriter, r *http.Request) {
	runnerID := chi.URLParam(r, "runnerID")
	var request struct {
		AgentID   string `json:"agentId"`
		Source    string `json:"source"`
		Scope     string `json:"scope"`
		ProjectID string `json:"projectId,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("请求格式无效"))
		return
	}
	if err := validatePluginName("市场来源", request.Source, false); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	agentID := firstNonEmpty(request.AgentID, pluginQueryAgent(r))
	adapter, ok := pluginAdapterFor(agentID)
	if !ok {
		writeError(w, http.StatusBadRequest, errors.New("该 CLI 不支持插件市场"))
		return
	}
	projectPath, err := s.pluginProjectPath(r.Context(), runnerID, request.ProjectID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	scope := normalizePluginScope(request.Scope)
	if scope != pluginScopeUser && projectPath == "" {
		writeError(w, http.StatusBadRequest, errors.New("项目范围操作需要选择一个项目"))
		return
	}
	if !pluginScopeSupported(adapter, scope) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("该 CLI 不支持 %s 范围的插件市场", scope))
		return
	}
	s.pluginMutationMu.Lock()
	defer s.pluginMutationMu.Unlock()
	result, err := adapter.MarketplaceAdd(r.Context(), &pluginExec{server: s, runnerID: runnerID, agentID: agentID}, projectPath, request.Source, scope)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.invalidatePluginCache(runnerID, agentID)
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) removePluginMarketplace(w http.ResponseWriter, r *http.Request) {
	runnerID := chi.URLParam(r, "runnerID")
	agentID := pluginQueryAgent(r)
	adapter, ok := pluginAdapterFor(agentID)
	if !ok {
		writeError(w, http.StatusBadRequest, errors.New("该 CLI 不支持插件市场"))
		return
	}
	name := pluginPathParam(r, "name")
	if err := validatePluginName("市场名称", name, false); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	projectPath, err := s.pluginProjectPath(r.Context(), runnerID, r.URL.Query().Get("projectId"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.pluginMutationMu.Lock()
	defer s.pluginMutationMu.Unlock()
	result, err := adapter.MarketplaceRemove(r.Context(), &pluginExec{server: s, runnerID: runnerID, agentID: agentID}, projectPath, name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.invalidatePluginCache(runnerID, agentID)
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) updatePluginMarketplaces(w http.ResponseWriter, r *http.Request) {
	runnerID := chi.URLParam(r, "runnerID")
	var request struct {
		AgentID   string `json:"agentId"`
		Name      string `json:"name"`
		ProjectID string `json:"projectId,omitempty"`
	}
	_ = json.NewDecoder(r.Body).Decode(&request)
	// 空名称表示刷新全部市场；非空时同样要做参数校验。
	if err := validatePluginName("市场名称", request.Name, true); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	agentID := firstNonEmpty(request.AgentID, pluginQueryAgent(r))
	adapter, ok := pluginAdapterFor(agentID)
	if !ok {
		writeError(w, http.StatusBadRequest, errors.New("该 CLI 不支持插件市场"))
		return
	}
	projectPath, err := s.pluginProjectPath(r.Context(), runnerID, request.ProjectID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.pluginMutationMu.Lock()
	defer s.pluginMutationMu.Unlock()
	result, err := adapter.MarketplaceUpdate(r.Context(), &pluginExec{server: s, runnerID: runnerID, agentID: agentID}, projectPath, request.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.invalidatePluginCache(runnerID, agentID)
	writeJSON(w, http.StatusOK, result)
}

// ── 技能面板联动 ─────────────────────────────────────────────────────────────

// disabledPluginPaths 返回当前环境下处于禁用状态的插件安装路径。
// 读失败时返回 nil —— 读不到不等于没有，不能因此把技能的可用性改口。
func (s *Server) disabledPluginPaths(ctx context.Context, project Project, agentID string) []string {
	// 没有路径解析器时无法确定该拉起哪个可执行文件；按"读不到"处理，不能误判成"全部启用"。
	if s.paths == nil {
		return nil
	}
	agents := []string{agentID}
	if agentID == "" {
		agents = []string{"claude-code", "codex", "qoder"}
	}
	state := map[string]bool{}
	for _, id := range agents {
		if _, ok := pluginAdapterFor(id); !ok {
			continue
		}
		// 技能过滤只关心启用状态，不需要市场清单。
		view, err := s.pluginListWith(ctx, project.Runner, id, project.Path, false, false)
		if err != nil {
			continue
		}
		for _, plugin := range view.Plugins {
			if plugin.InstallPath == "" || !plugin.StateKnown {
				continue
			}
			// 别的项目的 project/local 安装不在本项目的技能树里，不能拿来当禁用根。
			if !pluginRecordBelongsToProject(plugin, project.Path) {
				continue
			}
			// WSL 侧 CLI 报出的是发行版内路径（/home/...），而本地技能扫描读的是 UNC；
			// 不换算就永远匹配不上，禁用插件的技能会继续显示为可用。
			installPath := plugin.InstallPath
			if s.resolveAgentTargetEnv(project.Runner, project.Path) == agentTargetEnvWSL {
				if _, distro := s.wslUserHomeDistro(); distro != "" && strings.HasPrefix(filepath.ToSlash(installPath), "/") {
					installPath = wslToUncPath(installPath, distro)
				}
			}
			key := pluginPathKey(installPath)
			// 同一路径被多条记录指向时，只要有一条说启用就不算禁用。
			if plugin.Enabled {
				state[key] = true
				continue
			}
			if _, ok := state[key]; !ok {
				state[key] = false
			}
		}
	}
	var disabled []string
	for path, enabled := range state {
		if !enabled {
			disabled = append(disabled, path)
		}
	}
	sort.Strings(disabled)
	return disabled
}

func pluginPathKey(path string) string {
	trimmed := filepath.ToSlash(filepath.Clean(strings.TrimSpace(path)))
	if runtimeIsWindowsPath(trimmed) {
		return strings.ToLower(trimmed)
	}
	return trimmed
}

// pluginProjectKeys 返回同一个项目可比较的路径键。WSL 项目的库内路径是 UNC，
// 而 WSL 里运行的 CLI 报出的是发行版内路径（/home/...），只比较一种形态会把
// 本项目自己的安装当成"别的项目"。
func pluginProjectKeys(projectPath string) []string {
	keys := []string{pluginPathKey(projectPath)}
	if linuxPath, ok := uncToWslPath(strings.TrimSpace(projectPath), ""); ok {
		keys = append(keys, pluginPathKey(linuxPath))
	}
	return keys
}

// pluginRecordBelongsToProject 判断一条安装记录是否属于当前项目的技能树。
// user 范围没有项目归属；其他项目 / 其他环境的 project/local 安装要排除。
func pluginRecordBelongsToProject(plugin PluginRecord, projectPath string) bool {
	if plugin.Scope == pluginScopeUser || plugin.ProjectPath == "" {
		return true
	}
	key := pluginPathKey(plugin.ProjectPath)
	for _, projectKey := range pluginProjectKeys(projectPath) {
		if key == projectKey {
			return true
		}
	}
	return false
}

func runtimeIsWindowsPath(path string) bool {
	return len(path) >= 2 && path[1] == ':'
}

// pathWithinPlugin 判断 skillFile 是否落在某个插件安装路径下。
func pathWithinPlugin(skillFile, pluginPath string) bool {
	file := pluginPathKey(skillFile)
	root := pluginPathKey(pluginPath)
	if root == "" {
		return false
	}
	return file == root || strings.HasPrefix(file, strings.TrimSuffix(root, "/")+"/")
}

// pluginPathInAny 判断文件是否落在任一插件安装路径下。
func pluginPathInAny(file string, roots []string) bool {
	for _, root := range roots {
		if pathWithinPlugin(file, root) {
			return true
		}
	}
	return false
}

// filterDisabledPluginScans 丢弃落在已禁用插件目录下的技能扫描结果。
func filterDisabledPluginScans(scans []skillScan, disabled []string) []skillScan {
	if len(disabled) == 0 {
		return scans
	}
	out := scans[:0]
	for _, scan := range scans {
		if !pluginPathInAny(scan.skillFile, disabled) {
			out = append(out, scan)
		}
	}
	return out
}
