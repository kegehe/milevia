package app

import (
	"context"
	"encoding/json"
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
		plugins = append(plugins, plugin)
		return nil
	})
	return plugins
}

func (s *Server) listPlugins(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
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
	if target == agentTargetEnvRemote {
		// The API makes the limitation explicit instead of claiming activation.
		writeJSON(w, http.StatusOK, map[string]any{"plugins": plugins, "note": "SSH 插件清单暂不扫描；远端 Skill 来源仍可查看。"})
		return
	}
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
		if home != "" {
			plugins = append(plugins, scanPluginManifests(ctx, filepath.Join(home, dir.root), dir.agent, dir.manifest)...)
		}
		plugins = append(plugins, scanPluginManifests(ctx, filepath.Join(project.Path, dir.root), dir.agent, dir.manifest)...)
	}
	if agentID == "" || agentID == "opencode" {
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
		plugins = append(plugins, configuredOpenCodePlugins(configs)...)
	}
	seen := map[string]bool{}
	unique := plugins[:0]
	for _, plugin := range plugins {
		key := plugin.Agent + "\x00" + plugin.Name + "\x00" + plugin.Version + "\x00" + plugin.Status
		if !seen[key] {
			seen[key] = true
			unique = append(unique, plugin)
		}
	}
	plugins = unique
	sort.Slice(plugins, func(i, j int) bool { return plugins[i].Agent+plugins[i].Name < plugins[j].Agent+plugins[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{"plugins": plugins, "note": "仅列出磁盘上发现的插件；是否启用由 CLI 配置决定。"})
}
