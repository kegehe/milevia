package app

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

type DiscoveredPlugin struct {
	Name        string `json:"name"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
	Agent       string `json:"agent"`
	Source      string `json:"source"`
	Status      string `json:"status"` // discovered/configured does not assert activation
	// Enabled / StateKnown 只在 Source=="cli" 时有意义：状态来自 CLI 自己的输出，
	// 磁盘扫描兜底时 StateKnown=false，前端不能把"未知"画成"已禁用"。
	Enabled    bool   `json:"enabled"`
	StateKnown bool   `json:"stateKnown"`
	Managed    bool   `json:"managed"`
	Scope      string `json:"scope,omitempty"`
}

// OpenCode declares npm/file plugins in JSONC configuration rather than
// requiring a marketplace manifest. Only read the plugin field, never expose
// provider configuration or credentials through this endpoint.
func configuredOpenCodePlugins(paths []string) []DiscoveredPlugin {
	var plugins []DiscoveredPlugin
	seen := map[string]bool{}
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		clean, err := supervisionJSONC(raw)
		if err != nil {
			continue
		}
		var config struct {
			Plugin []string `json:"plugin"`
		}
		if json.Unmarshal(clean, &config) != nil {
			continue
		}
		for _, name := range config.Plugin {
			name = strings.TrimSpace(name)
			// URL userinfo and query strings may carry credentials. Such entries
			// are deliberately not copied into the user-facing catalog.
			if name == "" || len(name) > 256 || strings.ContainsAny(name, "\r\n?#") || strings.Contains(name, "://") || seen[name] {
				continue
			}
			seen[name] = true
			plugins = append(plugins, DiscoveredPlugin{Name: name, Agent: "opencode", Source: "config", Status: "configured"})
		}
	}
	return plugins
}

func scanPluginManifests(ctx context.Context, root, agent, manifest string) []DiscoveredPlugin {
	plugins := []DiscoveredPlugin{}
	visited := 0
	_ = filepath.WalkDir(root, func(p string, entry fs.DirEntry, walkErr error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		visited++
		if visited > 20000 {
			return fs.SkipAll
		}
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(filepath.ToSlash(p), "/"+manifest) || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() > 128<<10 {
			return nil
		}
		var plugin DiscoveredPlugin
		data, err := os.ReadFile(p)
		if err != nil || json.Unmarshal(data, &plugin) != nil || plugin.Name == "" {
			return nil
		}
		plugin.Agent, plugin.Source, plugin.Status = agent, "manifest", "discovered"
		// 清单是插件作者写的：启用状态、管理归属与安装范围只能来自 CLI，
		// 否则一个手写字段就能把「状态未知」伪装成「已启用」。
		plugin.Enabled, plugin.StateKnown = false, false
		plugin.Managed, plugin.Scope = false, ""
		plugins = append(plugins, plugin)
		return nil
	})
	return plugins
}

func (s *Server) listPlugins(w http.ResponseWriter, r *http.Request) {
	// CLI 冷启动可能较慢（WSL/SSH 还要跨端），给它留出与列表查询同级的预算；
	// 30 秒缓存让连续刷新不再重复拉进程。
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	project, err := s.getProjectByID(r.Context(), chi.URLParam(r, "projectID"))
	if err != nil {
		writeProjectResolveError(w, err)
		return
	}
	if conversationID := r.URL.Query().Get("conversationId"); conversationID != "" {
		workspace, resolveErr := s.resolveRequestWorkspace(ctx, project.ID, conversationID)
		if resolveErr != nil {
			writeError(w, http.StatusConflict, resolveErr)
			return
		}
		project.Path = workspace.Workspace.Path
	}
	agentID := r.URL.Query().Get("agentId")
	plugins := []DiscoveredPlugin{}
	target := s.resolveAgentTargetEnv(project.Runner, project.Path)
	seen := map[string]bool{}
	managed := map[string]bool{}
	// CLI 查询超时与"这个 CLI 不支持插件子命令"是两回事：前者只是没读到，不能
	// 让面板继续宣称"状态来自 CLI"，也不能把它画成"不支持"。
	cliTimedOut := false
	// 支持插件子命令的 CLI：清单与启用状态以 CLI 输出为准（远端也一样，走同一条跨端通道）。
	for _, id := range []string{"claude-code", "codex", "qoder"} {
		if agentID != "" && agentID != id {
			continue
		}
		if _, ok := pluginAdapterFor(id); !ok {
			continue
		}
		view, listErr := s.pluginListWith(ctx, project.Runner, id, project.Path, false, false)
		if listErr != nil {
			// 单飞 leader 跑在脱开请求的限时 ctx 上，所以超时可能表现为
			// DeadlineExceeded 而不是本请求 ctx 结束。
			if ctx.Err() != nil || errors.Is(listErr, context.DeadlineExceeded) {
				cliTimedOut = true
			}
			continue // 读不到就退回磁盘扫描，不硬失败
		}
		managed[id] = true
		for _, record := range view.Plugins {
			name := firstNonEmpty(record.DisplayName, record.Name, record.ID)
			key := id + "\x00" + name
			if name == "" || seen[key] {
				continue
			}
			seen[key] = true
			plugins = append(plugins, DiscoveredPlugin{
				Name: name, Version: record.Version, Description: record.Description,
				Agent: id, Source: "cli", Status: "configured",
				Enabled: record.Enabled, StateKnown: record.StateKnown, Managed: true,
				Scope: record.Scope,
			})
		}
	}
	// 磁盘扫描只补 CLI 不支持查询的工具；远端插件目录不可直读。
	if target != agentTargetEnvRemote {
		home := homeDir()
		if target == agentTargetEnvWSL {
			h, distro := s.wslUserHomeDistro()
			if h == "" || distro == "" {
				home = ""
			} else {
				home = wslToUncPath(h, distro)
			}
		}
		for _, dir := range pluginDirectories {
			if agentID != "" && agentID != dir.agent {
				continue
			}
			if managed[dir.agent] {
				continue
			}
			var found []DiscoveredPlugin
			if home != "" {
				found = append(found, scanPluginManifests(ctx, filepath.Join(home, dir.root), dir.agent, dir.manifest)...)
			}
			found = append(found, scanPluginManifests(ctx, filepath.Join(project.Path, dir.root), dir.agent, dir.manifest)...)
			for _, plugin := range found {
				key := plugin.Agent + "\x00" + plugin.Name
				if seen[key] {
					continue
				}
				seen[key] = true
				plugins = append(plugins, plugin)
			}
		}
		if (agentID == "" || agentID == "opencode") && !managed["opencode"] {
			configDir := filepath.Join(home, ".config", "opencode")
			if target != agentTargetEnvWSL {
				if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
					configDir = filepath.Join(xdg, "opencode")
				}
			}
			var configs []string
			if home != "" {
				configs = append(configs, filepath.Join(configDir, "opencode.json"), filepath.Join(configDir, "opencode.jsonc"))
			}
			for _, name := range []string{"opencode.json", "opencode.jsonc", ".opencode/opencode.json", ".opencode/opencode.jsonc"} {
				configs = append(configs, filepath.Join(project.Path, filepath.FromSlash(name)))
			}
			if target != agentTargetEnvWSL {
				if extra := strings.TrimSpace(os.Getenv("OPENCODE_CONFIG_DIR")); extra != "" {
					configs = append(configs, filepath.Join(extra, "opencode.json"), filepath.Join(extra, "opencode.jsonc"))
				}
				if extra := strings.TrimSpace(os.Getenv("OPENCODE_CONFIG")); extra != "" {
					configs = append(configs, extra)
				}
			}
			for _, plugin := range configuredOpenCodePlugins(configs) {
				key := plugin.Agent + "\x00" + plugin.Name
				if seen[key] {
					continue
				}
				seen[key] = true
				plugins = append(plugins, plugin)
			}
		}
	}
	sort.Slice(plugins, func(i, j int) bool { return plugins[i].Agent+plugins[i].Name < plugins[j].Agent+plugins[j].Name })
	note := "清单与启用状态来自 CLI；不支持插件子命令的工具只列出磁盘上发现的插件。"
	if cliTimedOut {
		note = "部分 CLI 查询超时：超时的工具只列出磁盘上发现的插件，启用状态未知。"
	}
	if target == agentTargetEnvRemote && len(managed) == 0 {
		note = "远端暂无法读取插件清单；远端 Skill 来源仍可查看。"
	}
	writeJSON(w, http.StatusOK, map[string]any{"plugins": plugins, "note": note})
}
