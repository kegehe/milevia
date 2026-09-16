package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
)

// 会话级模型选择：底部模型栏的"可选择"能力。
//
// 设计要点（见 docs/36）：
//   - 模型是"执行参数"而非凭据，因此落在 conversations.model_override 单列上，
//     不新建 agent profile revision；
//   - 优先级只有一处判定（runModel：会话覆盖 > 档案模型 > CLI 默认），算完作为
//     AgentRunRequest.Model 下传，runner 不再读 Profile.Model；
//   - 切换复用会话退役机制：model 是 conversationSessionConfig 的字段，变化即退役
//     长驻进程，下一条消息带新 --model --resume 重启。

const maxModelOverrideLength = 128

// modelOverridePattern 限定模型名的字符集。模型名最终会成为 CLI argv（SSH 路径还会经
// shellQuote 拼进远端命令），这里做保守白名单：字母数字与 . _ : - / @ +——覆盖
// "claude-opus-5"、"gpt-5.6-sol"、"openai/gpt-4o"、"us.anthropic.x-v1:0" 这类真实取值，
// 同时挡掉空白、引号、$、反引号与控制字符。
var modelOverridePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/+-]*$`)

// normalizeModelOverride 校验并规范化用户填入的模型名；空串合法（= 清除覆盖，
// 回到"跟随项目配置 / CLI 默认"）。
func normalizeModelOverride(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	if utf8.RuneCountInString(value) > maxModelOverrideLength {
		return "", fmt.Errorf("模型名过长（最多 %d 个字符）", maxModelOverrideLength)
	}
	if !modelOverridePattern.MatchString(value) {
		return "", errors.New("模型名含有不支持的字符（只允许字母、数字与 . _ : - / @ +）")
	}
	return value, nil
}

// AgentModelOption 是模型选择器里的一项。
type AgentModelOption struct {
	ID          string `json:"id"`
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
	// Alias 标记"永远指向最新版本"的别名（如 Claude 的 opus / sonnet）。
	Alias bool `json:"alias,omitempty"`
}

// ConversationModelsView 是 GET /api/conversations/{id}/models 的响应。
type ConversationModelsView struct {
	ConversationID string `json:"conversationId"`
	AgentID        string `json:"agentId"`
	// Selected 是会话级覆盖（空 = 未设置）。
	Selected string `json:"selected"`
	// Effective 是"跟随配置时会实际使用的模型"，未知时为空串。
	Effective string `json:"effective"`
	// Source 说明 effective 的来源：override | profile | cli_default。
	Source        string             `json:"source"`
	Models        []AgentModelOption `json:"models"`
	CustomAllowed bool               `json:"customAllowed"`
	// Note 如实说明目录来源或降级原因（例如探测失败回退静态表）。
	Note string `json:"note,omitempty"`
}

// ---------------------------------------------------------------------------
// 模型目录
// ---------------------------------------------------------------------------

// claudeModelCatalog 是 Claude Code 的静态目录。
//
// Claude Code 没有"列出模型"的子命令（与 Codex 的 `codex debug models` 不同），且
// `--model` 本身接受任意字符串（第三方网关尤其如此），所以这里只是**候选项建议**：
//  1. 别名 —— 永远指向最新版本，跨版本稳定，是日常最该用的；
//  2. 具体版本 id —— 需要钉住版本时用，取自本机 CLI 自身支持的型号
//     （`strings <claude 二进制> | grep -o 'claude-\(opus\|sonnet\|haiku\|fable\)-[0-9.-]*'`）。
//
// 升级 Claude Code 大版本后需要人工复核第 2 组；第 1 组不受影响。
func claudeModelCatalog() []AgentModelOption {
	return []AgentModelOption{
		{ID: "opus", Label: "opus（最新 Opus）", Alias: true, Description: "别名，始终指向最新 Opus"},
		{ID: "sonnet", Label: "sonnet（最新 Sonnet）", Alias: true, Description: "别名，始终指向最新 Sonnet"},
		{ID: "haiku", Label: "haiku（最新 Haiku）", Alias: true, Description: "别名，始终指向最新 Haiku，响应最快"},
		{ID: "fable", Label: "fable（最新 Fable）", Alias: true, Description: "别名，始终指向最新 Fable"},
		{ID: "claude-opus-5", Label: "claude-opus-5", Description: "Opus 5"},
		{ID: "claude-sonnet-5", Label: "claude-sonnet-5", Description: "Sonnet 5"},
		{ID: "claude-haiku-4-5", Label: "claude-haiku-4-5", Description: "Haiku 4.5"},
		{ID: "claude-fable-5-1", Label: "claude-fable-5-1", Description: "Fable 5.1"},
		{ID: "claude-fable-5", Label: "claude-fable-5", Description: "Fable 5"},
	}
}

// codexModelCatalogRunner 由能列出 Codex 模型目录的 runner 实现（本机 / WSL / SSH）。
// 目录来自 `codex debug models`（JSON），属 CLI 的调试子命令，因此调用方必须容忍失败
// 并回退到静态表。
type codexModelCatalogRunner interface {
	codexModelCatalog(ctx context.Context) ([]AgentModelOption, error)
}

type codexModelCatalogPayload struct {
	Models []struct {
		Slug        string `json:"slug"`
		DisplayName string `json:"display_name"`
		Description string `json:"description"`
		Visibility  string `json:"visibility"`
	} `json:"models"`
}

// parseCodexModelCatalog 解析 `codex debug models` 的 JSON。visibility 非 "list" 的
// 型号（hide）是内部/实验型号，不作为候选项。解析失败返回 nil，由调用方降级。
func parseCodexModelCatalog(data []byte) []AgentModelOption {
	var payload codexModelCatalogPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil
	}
	options := make([]AgentModelOption, 0, len(payload.Models))
	for _, entry := range payload.Models {
		if entry.Slug == "" {
			continue
		}
		if entry.Visibility != "" && entry.Visibility != "list" {
			continue
		}
		label := entry.DisplayName
		if label == "" {
			label = entry.Slug
		}
		options = append(options, AgentModelOption{ID: entry.Slug, Label: label, Description: entry.Description})
	}
	if len(options) == 0 {
		return nil
	}
	return options
}

// modelCatalogProbeTimeout 限制单次 `codex debug models` 的探测时间：本机是毫秒级，
// WSL/SSH 需要拉起远端进程，仍应远小于用户能接受的等待。
const modelCatalogProbeTimeout = 8 * time.Second

// modelCatalogTTL 是目录缓存有效期。目录随 CLI 版本变化，且探测要拉起进程，故做单条缓存。
const modelCatalogTTL = 10 * time.Minute

// conversationModels 返回该会话的模型目录与当前生效信息，供底部模型选择器使用。
func (s *Server) conversationModels(w http.ResponseWriter, r *http.Request) {
	conversationID := chi.URLParam(r, "conversationID")
	var conversation Conversation
	var projectRunner, projectPath string
	err := s.db.QueryRowContext(r.Context(), `select c.id,c.project_id,c.claude_session_id,c.agent_id,c.agent_session_id,c.agent_runtime_id,c.agent_profile_revision_id,c.execution_policy,c.status,c.permission_mode,c.model_override,c.title,c.last_activity_at,c.claude_initialized,c.agent_initialized,c.is_current,c.created_at,p.path,coalesce(nullif(p.runner_id,''),p.runner) from conversations c join projects p on p.id=c.project_id where c.id=?`, conversationID).Scan(&conversation.ID, &conversation.ProjectID, &conversation.ClaudeSessionID, &conversation.AgentID, &conversation.AgentSessionID, &conversation.AgentRuntimeID, &conversation.AgentProfileRevisionID, &conversation.ExecutionPolicy, &conversation.Status, &conversation.PermissionMode, &conversation.ModelOverride, &conversation.Title, &conversation.LastActivityAt, &conversation.ClaudeInitialized, &conversation.AgentInitialized, &conversation.IsCurrent, &conversation.CreatedAt, &projectPath, &projectRunner)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("conversation not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	view := ConversationModelsView{
		ConversationID: conversation.ID,
		AgentID:        conversation.AgentID,
		Selected:       conversation.ModelOverride,
		CustomAllowed:  true,
		Models:         claudeModelCatalog(),
		Note:           "候选项为内置建议；Claude Code 的 --model 接受任意模型名，可直接输入。",
	}

	// 跟随配置时实际会用的模型：优先读档案 revision 的 model（只读，不触发凭据解密，
	// 也不做凭据池轮转选择——那是 run 路径的事）。读不到就留给运行后的用量数据展示。
	if conversation.ModelOverride != "" {
		view.Source = "override"
		view.Effective = conversation.ModelOverride
	} else if model := s.profileModel(r.Context(), conversation.AgentProfileRevisionID, projectRunner, conversation.AgentID); model != "" {
		view.Source = "profile"
		view.Effective = model
	} else {
		view.Source = "cli_default"
	}

	if conversation.AgentID == "codex" {
		options, note := s.codexModelCatalogFor(r.Context(), projectRunner, projectPath)
		if len(options) > 0 {
			view.Models = options
		} else {
			view.Models = codexFallbackModelCatalog()
		}
		if note != "" {
			view.Note = note
		}
		if view.Source == "cli_default" {
			// cli_managed 时 Codex 的真实默认模型可由 runner 报告（config.toml 的 model 键）。
			if runner := s.modelCatalogRunnerFor(projectRunner, projectPath); runner != nil {
				if resolver, ok := runner.(codexDefaultModelRunner); ok {
					probeCtx, cancel := context.WithTimeout(r.Context(), codexDefaultModelProbeTimeout)
					view.Effective = resolver.codexDefaultModel(probeCtx)
					cancel()
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, view)
}

// profileModel 只读地取出档案 revision 上的模型名。取不到（无档案、档案已不可用、
// 会话走凭据池尚未选定成员）时返回空串，调用方据此显示"跟随 CLI 默认"。
func (s *Server) profileModel(ctx context.Context, revisionID, runnerID, agentID string) string {
	if revisionID == "" {
		return ""
	}
	var model string
	err := s.db.QueryRowContext(ctx, `select r.model from agent_profile_revisions r join agent_profiles p on p.id=r.profile_id where r.id=? and p.runner_id=? and p.agent_id=?`, revisionID, runnerID, agentID).Scan(&model)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(model)
}

// codexDefaultModelProbeTimeout 比 usage 预置用的 5s 略短：这里在请求内同步等待用户。
const codexDefaultModelProbeTimeout = 5 * time.Second

// modelCatalogRunnerFor 返回可为该会话探测 Codex 目录的 runner，路由与 startMessage 的
// runner 选择保持一致（SSH 走注册表，其余按项目目标环境解析）。
func (s *Server) modelCatalogRunnerFor(projectRunner, projectPath string) AgentRunner {
	if strings.HasPrefix(projectRunner, "ssh-") {
		if runner, ok := s.runnerRegistry.get(projectRunner); ok {
			return runner
		}
		return nil
	}
	return s.codexRunnerFor(s.resolveAgentTargetEnv(projectRunner, projectPath))
}

// codexModelCatalogFor 返回 Codex 目录与一句来源说明。探测失败时返回 nil，由调用方
// 回退静态表（note 说明降级原因）。带单条缓存，避免反复打开选择器时重复拉起进程。
func (s *Server) codexModelCatalogFor(ctx context.Context, projectRunner, projectPath string) ([]AgentModelOption, string) {
	runner := s.modelCatalogRunnerFor(projectRunner, projectPath)
	if runner == nil {
		return nil, "未能解析该项目对应的 Codex runner，已回退到内置模型列表。"
	}
	capable, ok := runner.(codexModelCatalogRunner)
	if !ok {
		return nil, "该运行环境暂不支持读取 Codex 模型目录，已回退到内置模型列表。"
	}

	cacheKey := projectRunner + "|" + projectPath
	s.modelCatalogMu.Lock()
	if s.modelCatalogKey == cacheKey && !s.modelCatalogAt.IsZero() && time.Since(s.modelCatalogAt) < modelCatalogTTL {
		options, note := s.modelCatalogOptions, s.modelCatalogNote
		s.modelCatalogMu.Unlock()
		return options, note
	}
	s.modelCatalogMu.Unlock()

	probeCtx, cancel := context.WithTimeout(ctx, modelCatalogProbeTimeout)
	defer cancel()
	options, err := capable.codexModelCatalog(probeCtx)
	note := "模型目录来自 Codex CLI 的 `codex debug models`。"
	if err != nil || len(options) == 0 {
		options, note = nil, "读取 Codex 模型目录失败，已回退到内置模型列表；可在下方直接输入模型名。"
	}

	s.modelCatalogMu.Lock()
	s.modelCatalogKey = cacheKey
	s.modelCatalogAt = time.Now()
	s.modelCatalogOptions = options
	s.modelCatalogNote = note
	s.modelCatalogMu.Unlock()
	return options, note
}

// codexFallbackModelCatalog 是探测失败时的内置兜底列表。刻意保持短：它只是"给几个
// 起点"，真实可用的型号以 CLI 目录或用户自填为准（第三方网关的型号无法穷举）。
func codexFallbackModelCatalog() []AgentModelOption {
	return []AgentModelOption{
		{ID: "gpt-5.5", Label: "gpt-5.5"},
		{ID: "gpt-5.2", Label: "gpt-5.2"},
		{ID: "gpt-5.1-codex", Label: "gpt-5.1-codex"},
	}
}

// ---------------------------------------------------------------------------
// 切换会话模型
// ---------------------------------------------------------------------------

// modelSwitchRetireTimeout 等待长驻会话退出的上限。切换模型必须重启进程，而退役是异步的；
// 给一个短上限把"一次点击就完成"做成常态，超时则如实让用户重试（不假报成功）。
const modelSwitchRetireTimeout = 2 * time.Second

// awaitSessionRetired 等待会话从 s.sessions 移除（watcher 确认进程已退出后才移除）。
// 会话仍在但不是 stopping（例如有审批挂起）时立刻返回 false，不空等。
func (s *Server) awaitSessionRetired(conversationID string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		s.mu.Lock()
		session := s.sessions[conversationID]
		stopping := session != nil && session.stopping
		s.mu.Unlock()
		if session == nil {
			return true
		}
		if !stopping || !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// setConversationModel 设置或清除会话级模型覆盖。流程与 setConversationPermissionMode
// 一致：仅空闲会话可切，切换前先退役长驻会话（模型是进程启动参数，改了必须重启）。
func (s *Server) setConversationModel(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Model string `json:"model"`
	}
	if !decode(w, r, &input) {
		return
	}
	model, err := normalizeModelOverride(input.Model)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if s.isOrchestrationConversation(r.Context(), chi.URLParam(r, "conversationID")) {
		writeError(w, http.StatusConflict, errors.New("automatic orchestration conversations are read-only"))
		return
	}
	// 与清空会话、激活会话、改权限保持同一把顺序锁。
	s.projectLifecycleMu.Lock()
	defer s.projectLifecycleMu.Unlock()

	conversationID := chi.URLParam(r, "conversationID")
	var conversation Conversation
	err = s.db.QueryRowContext(r.Context(), `select id,project_id,claude_session_id,agent_id,agent_session_id,agent_runtime_id,agent_profile_revision_id,execution_policy,status,permission_mode,model_override,title,last_activity_at,claude_initialized,agent_initialized,is_current,created_at from conversations where id=$1`, conversationID).Scan(&conversation.ID, &conversation.ProjectID, &conversation.ClaudeSessionID, &conversation.AgentID, &conversation.AgentSessionID, &conversation.AgentRuntimeID, &conversation.AgentProfileRevisionID, &conversation.ExecutionPolicy, &conversation.Status, &conversation.PermissionMode, &conversation.ModelOverride, &conversation.Title, &conversation.LastActivityAt, &conversation.ClaudeInitialized, &conversation.AgentInitialized, &conversation.IsCurrent, &conversation.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("conversation not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if conversation.ModelOverride == model {
		// 幂等：重复设同一个值不必重启长驻会话。
		writeJSON(w, http.StatusOK, conversation)
		return
	}
	if conversation.Status != "idle" {
		writeError(w, http.StatusConflict, errors.New("请先停止当前任务，再切换模型"))
		return
	}
	// 模型是长驻进程的启动参数，改配置前必须让旧进程退役。retireForConfiguration 在
	// 「刚发起退役」与「退不掉」（例如有审批挂起）两种情况下都返回 false（其注释写明调用方
	// 应在 watcher 清理后重试），所以这里等它真正从 s.sessions 消失再落库——否则用户点一次
	// 换模型只会拿到 409，得再点一次才生效。等待只读 s.mu，不碰 projectLifecycleMu，
	// 因此与 watcher 的清理路径不构成死锁。
	if !s.sessionManager.retireForConfiguration(conversationID) && !s.awaitSessionRetired(conversationID, modelSwitchRetireTimeout) {
		writeError(w, http.StatusConflict, errors.New("会话正在重启，请稍后重试"))
		return
	}
	result, err := s.db.ExecContext(r.Context(), `update conversations set model_override=? where id=? and status='idle'`, model, conversationID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	updated, err := result.RowsAffected()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if updated != 1 {
		// 等待退役期间会话可能已开始新任务或被删除，两种情况都在这里兜住；文案不要只说"先停止任务"。
		writeError(w, http.StatusConflict, errors.New("会话状态已变化（可能已开始新任务或被删除），请刷新后重试"))
		return
	}
	conversation.ModelOverride = model
	writeJSON(w, http.StatusOK, conversation)
}
