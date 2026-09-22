package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// 工具可执行文件的路径解析。
//
// 这件事原先落在 `Config.ClaudePath` / `Config.CodexPath` 两个**不可变字段**上，
// 被直接读 18 处。那个形状有两个后果（见 docs/42 §14.B）：
//
//  1. 平台把工具装到自己的目录之后（托管 prefix **不在 PATH 上**），这 18 处全都不会
//     知道 —— 装完等于没装；
//  2. `Config` 是按值传进 runner 的，运行期改不了。
//
// 所以把"路径"换成一个可查询的解析器，所有读取都经它。

// agentPathResolver 回答"某个工具现在该用哪个可执行文件"。
type agentPathResolver struct {
	mu sync.RWMutex
	// override 来自环境变量（AUTO_CLAUDE_PATH / AUTO_CODEX_PATH），是部署方的显式覆盖。
	override map[string]string
	// recorded 是**实测过可用**的绝对路径：安装/升级成功后写回，启动时也从登记表载入。
	recorded map[string]string
}

// newAgentPathResolver 建立本地环境的路径解析器。
//
// 解析顺序固定为（**顺序不许换**）：
//
//	环境变量覆盖 > 登记表（实测可用）> PATH 查找 > 平台兜底候选
//
// 环境变量是显式覆盖；登记表是我们自己装出来的实测结果；PATH 是用户既有环境；
// 平台兜底只解决"GUI 进程继承了 npm 安装之前的陈旧 PATH"这一种已知情况。
func newAgentPathResolver(config Config) *agentPathResolver {
	override := map[string]string{}
	// 两个环境变量沿用既有名字，不加新开关。
	if path := strings.TrimSpace(config.ClaudePath); path != "" && path != agentCommandName("claude-code") {
		override["claude-code"] = path
	}
	if path := strings.TrimSpace(config.CodexPath); path != "" && path != agentCommandName("codex") {
		override["codex"] = path
	}
	return &agentPathResolver{override: override, recorded: map[string]string{}}
}

// Path 按固定顺序解析该工具的可执行文件。
//
// 全都没命中时返回目录里的命令名（例如 "claude"）而不是空串：这与改动前的
// `Config.ClaudePath` 缺省值一致，也让上层的错误信息里出现的是工具名而不是空白。
func (r *agentPathResolver) Path(agentID string) string {
	entry, ok := agentByID(agentID)
	if !ok {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	if path := r.override[agentID]; path != "" {
		return path
	}
	// 登记表的路径要**实测仍然存在**才采用：装完之后被用户删掉、或换了一台机器，
	// 记录还在而文件没了 —— 这时应继续往下找，而不是抱着一条死路径不放。
	if path := r.recorded[agentID]; path != "" && fileExists(path) {
		return path
	}
	if path, err := exec.LookPath(entry.CommandName); err == nil {
		return path
	}
	for _, candidate := range platformFallbackCandidates(entry) {
		if fileExists(candidate) {
			return candidate
		}
	}
	return entry.CommandName
}

// Remember 记下一次实测可用的路径（安装/升级成功后调用）。
func (r *agentPathResolver) Remember(agentID, path string) {
	if strings.TrimSpace(path) == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recorded[agentID] = path
}

// Forget 忘掉一条记录（登记表里那行被删掉时用）。
func (r *agentPathResolver) Forget(agentID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.recorded, agentID)
}

// load 用登记表的内容整体替换已记录项（启动时载入一次）。
func (r *agentPathResolver) load(recorded map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recorded = map[string]string{}
	for agentID, path := range recorded {
		if strings.TrimSpace(path) != "" {
			r.recorded[agentID] = path
		}
	}
}

// platformFallbackCandidates 给出平台兜底候选路径。
//
// 沿用 codex 原先那段硬编码的意图，但**不再只对 codex 生效**：npm 全局包的落点
// 对两个工具是一样的，没有理由只给其中一个兜底。
func platformFallbackCandidates(entry AgentCatalogEntry) []string {
	if runtime.GOOS != "windows" {
		return nil
	}
	appData := os.Getenv("APPDATA")
	if appData == "" {
		return nil
	}
	return []string{
		filepath.Join(appData, "npm", entry.CommandName+".cmd"),
		filepath.Join(appData, "npm", entry.CommandName+".exe"),
	}
}

// agentCommandName 返回目录里声明的命令名；未知工具返回空串。
func agentCommandName(agentID string) string {
	if entry, ok := agentByID(agentID); ok {
		return entry.CommandName
	}
	return ""
}

// agentBinary 是 Server 侧的取值入口（mcp_import / project_commands 等非 runner 处使用）。
func (s *Server) agentBinary(agentID string) string {
	if s.paths != nil {
		if path := s.paths.Path(agentID); path != "" {
			return path
		}
	}
	return agentCommandName(agentID)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// ── 安装登记表 ──────────────────────────────────────────────────────────────
//
// 为什么需要它（docs/42 §14.B）：托管 prefix 不在 PATH 上，`exec.LookPath` 永远找不到它。
// 所以安装/升级成功后必须把**实测到的绝对路径**记下来，否则下次启动时这 18 处读取
// 又会回到"找不到"。同时它让界面能显示"安装位置"，也留下一条审计线索。

func (s *Server) migrateAgentInstallations(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `create table if not exists agent_installations (
		runner_id text not null,
		agent_id text not null,
		binary_path text not null,
		install_kind text not null default '',
		prefix text not null default '',
		version text not null default '',
		source text not null default '',
		installed_at datetime not null,
		updated_at datetime not null,
		primary key (runner_id, agent_id)
	)`)
	return err
}

// agentInstallation 是登记表的一行。
type agentInstallation struct {
	RunnerID    string    `json:"runnerId"`
	AgentID     string    `json:"agentId"`
	BinaryPath  string    `json:"binaryPath"`
	InstallKind string    `json:"installKind"`
	Prefix      string    `json:"prefix"`
	Version     string    `json:"version"`
	Source      string    `json:"source"`
	InstalledAt time.Time `json:"installedAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// loadAgentInstallations 读某个 Runner 上的全部登记项。
func (s *Server) loadAgentInstallations(ctx context.Context, runnerID string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `select agent_id,binary_path from agent_installations
		where runner_id=? and binary_path<>''`, runnerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var agentID, binaryPath string
		if err := rows.Scan(&agentID, &binaryPath); err != nil {
			return nil, err
		}
		out[agentID] = binaryPath
	}
	return out, rows.Err()
}

// recordAgentInstallation 写入/更新一条登记项，并同步给解析器。
//
// binary_path 与 install_kind / prefix **必须成对记录**：只有路径而没有 prefix 时，
// 后续"升级"会去找系统 npm 的全局位置，而不是这个工具实际所在的托管位置。
func (s *Server) recordAgentInstallation(ctx context.Context, installation agentInstallation) error {
	now := time.Now().UTC()
	if installation.InstalledAt.IsZero() {
		installation.InstalledAt = now
	}
	installation.UpdatedAt = now
	if _, err := s.db.ExecContext(ctx, `insert into agent_installations
		(runner_id,agent_id,binary_path,install_kind,prefix,version,source,installed_at,updated_at)
		values (?,?,?,?,?,?,?,?,?)
		on conflict(runner_id,agent_id) do update set
			binary_path=excluded.binary_path,
			install_kind=excluded.install_kind,
			prefix=excluded.prefix,
			version=excluded.version,
			source=excluded.source,
			updated_at=excluded.updated_at`,
		installation.RunnerID,
		installation.AgentID,
		installation.BinaryPath,
		installation.InstallKind,
		installation.Prefix,
		installation.Version,
		installation.Source,
		installation.InstalledAt,
		installation.UpdatedAt,
	); err != nil {
		return err
	}
	if s.paths != nil && isLocalRunnerID(installation.RunnerID) {
		s.paths.Remember(installation.AgentID, installation.BinaryPath)
	}
	return nil
}

// listAgentInstallations 读登记项（含元数据），供界面展示"安装位置"与排查。
func (s *Server) listAgentInstallations(ctx context.Context, runnerID string) ([]agentInstallation, error) {
	rows, err := s.db.QueryContext(ctx, `select runner_id,agent_id,binary_path,install_kind,prefix,version,source,installed_at,updated_at
		from agent_installations where runner_id=? order by agent_id`, runnerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []agentInstallation{}
	for rows.Next() {
		var item agentInstallation
		if err := rows.Scan(&item.RunnerID, &item.AgentID, &item.BinaryPath, &item.InstallKind,
			&item.Prefix, &item.Version, &item.Source, &item.InstalledAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
