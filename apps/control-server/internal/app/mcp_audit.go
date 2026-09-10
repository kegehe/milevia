package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// MCP 调用审计（P2）。
//
// 审计记录「哪个会话在何时调用了哪个 MCP 工具的哪个工具、参数摘要是什么、有没有被批准、
// 最终执行成功还是失败」。写入分两次：
//
//   - PreToolUse（审批 hook）：决策确定时写入一行，status=pending。自动放行也会记录
//     （decision=auto_allow），因为它同样是一次真实调用，只是没有弹审批。
//   - PostToolUse（同一个 hook 命令，按 hook_event_name 分流）：按 tool_use_id 回填
//     status 与耗时。
//
// 之所以复用同一个 hook 端点，是因为 hook 助手（cmd/approval-helper）已经把 stdin 原样
// 转发到该端点，PostToolUse 的负载里带 `hook_event_name`，服务端据此分流即可，无需改动
// 已部署的助手二进制。
//
// 表内不存明文参数：只存截断后的参数摘要，命中密钥特征的键值会被替换为 ***。

const (
	// mcpAuditRetention 是审计表的保留条数上限（超出部分在读接口被清理）。
	mcpAuditRetention = 2000
	// mcpAuditArgsLimit 是参数摘要的最大长度。
	mcpAuditArgsLimit = 600
	// mcpAuditErrorLimit 是失败原因的最大长度。
	mcpAuditErrorLimit = 400
)

func migrateMCPCallAudit(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `create table if not exists mcp_call_audit (
		id              text primary key,
		tool_use_id     text not null default '',
		conversation_id text not null default '',
		run_id          text not null default '',
		server_name     text not null default '',
		tool_name       text not null default '',
		args_preview    text not null default '',
		decision        text not null default '',
		status          text not null default '',
		error_text      text not null default '',
		duration_ms     integer not null default 0,
		created_at      datetime not null,
		updated_at      datetime not null
	)`); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `create index if not exists mcp_call_audit_created on mcp_call_audit(created_at)`); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `create index if not exists mcp_call_audit_tool_use on mcp_call_audit(tool_use_id)`); err != nil {
		return err
	}
	return nil
}

// splitMCPToolName 从 `mcp__<server>__<tool>` 拆出 server 与 tool 段。
// 名称里若缺少分隔段，则回退为「整串作为 tool、server 留空」，保证审计不丢记录。
func splitMCPToolName(toolName string) (string, string) {
	rest, ok := strings.CutPrefix(toolName, "mcp__")
	if !ok {
		return "", toolName
	}
	server, tool, found := strings.Cut(rest, "__")
	if !found {
		return rest, ""
	}
	return server, tool
}

// summarizeMCPToolInput 生成可安全入库的参数摘要：命中密钥特征的键值替换为 ***，再截断。
func summarizeMCPToolInput(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return ""
	}
	var decoded map[string]any
	if json.Unmarshal(raw, &decoded) != nil {
		return truncateAuditText(trimmed, mcpAuditArgsLimit)
	}
	redacted := make(map[string]any, len(decoded))
	for key, value := range decoded {
		if looksLikeSecretKey(key) {
			redacted[key] = "***"
			continue
		}
		redacted[key] = value
	}
	encoded, err := json.Marshal(redacted)
	if err != nil {
		return truncateAuditText(trimmed, mcpAuditArgsLimit)
	}
	return truncateAuditText(string(encoded), mcpAuditArgsLimit)
}

func truncateAuditText(text string, limit int) string {
	text = strings.TrimSpace(text)
	if len(text) <= limit {
		return text
	}
	// 按 rune 截断，避免把多字节字符切成半个。
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

// ---------------------------------------------------------------------------
// 审批参数上限（approval.* 事件）
// ---------------------------------------------------------------------------

const (
	// approvalToolInputTotalLimit 是 approval.* 事件里 toolInput 的整体字节上限。
	approvalToolInputTotalLimit = 32 * 1024
	// approvalToolInputValueLimit 是单个字符串参数值的上限（按 rune 计）。
	approvalToolInputValueLimit = 1024
	// approvalToolInputKeepLimit 是「必须保留原文」的键的独立上限，放宽一档。
	approvalToolInputKeepLimit = 16 * 1024
)

// approvalToolInputKeepKeys 是无论如何都要保留原文的键。
//
// 前端靠 `toolInput.command` 把审批横幅锚定到对应的工具卡片
// （apps/web/src/lib/timeline.ts），截断它会让横幅定位失败、用户无法对上是哪条命令。
var approvalToolInputKeepKeys = map[string]bool{"command": true}

// truncateApprovalToolInput 把审批参数压到可安全落事件的体积。
//
// 文档要求「toolInput 设上限 / 截断后再落事件与前端展示」（docs/34 §9）：MCP 工具参数可能
// 极大（例如整份文件内容），原样写进会话事件会撑大数据库并拖慢前端。两层限制叠加：
// 单个字符串值超限即截断；整体仍超限则退化为一条说明性标记（保留键之外的信息全部丢弃）。
func truncateApprovalToolInput(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || len(raw) <= approvalToolInputValueLimit {
		return raw
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		// 非对象或非法 JSON：结构不可解析，只在整体超限时降级。
		if len(raw) > approvalToolInputTotalLimit {
			return approvalToolInputMarker(len(raw))
		}
		return raw
	}
	trimmed := make(map[string]any, len(decoded))
	for key, value := range decoded {
		trimmed[key] = truncateApprovalValue(key, value)
	}
	encoded, err := json.Marshal(trimmed)
	if err != nil || len(encoded) > approvalToolInputTotalLimit {
		return approvalToolInputMarker(len(raw))
	}
	return encoded
}

// truncateApprovalValue 对单个参数值施加长度上限。
//
// 只处理字符串：数字、布尔与短结构本身不构成体积问题，嵌套结构里的长字符串由整体上限兜底。
//
// 上限按 **rune** 计（与 truncateAuditText 一致）。早期实现用 `len(text)` 的字节长度判定是否
// 需要截断，再交给按 rune 截断的 truncateAuditText —— 中文内容会落在「字节超限、rune 未超限」
// 的区间里，于是实测未截断却仍被追加「（已截断）」说明。两处口径必须统一。
func truncateApprovalValue(key string, value any) any {
	text, ok := value.(string)
	if !ok {
		return value
	}
	limit := approvalToolInputValueLimit
	if approvalToolInputKeepKeys[key] {
		limit = approvalToolInputKeepLimit
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + fmt.Sprintf("（已截断，原始 %d 字符）", len(runes))
}

// approvalToolInputMarker 是整体降级标记：结构过于庞大时不写入原文。
func approvalToolInputMarker(originalBytes int) json.RawMessage {
	encoded, err := json.Marshal(map[string]any{
		"_truncated":     true,
		"_originalBytes": originalBytes,
		"_note":          "参数过大，已省略正文，以免撑大会话事件与界面。",
	})
	if err != nil {
		return json.RawMessage(`{"_truncated":true}`)
	}
	return encoded
}

// mcpAuditWriteContext 保证审计写入不受请求上下文取消影响：客户端断开或审批超时后，
// 我们仍然希望把「这次调用发生了什么」记下来。
func mcpAuditWriteContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx != nil && ctx.Err() == nil {
		return ctx, func() {}
	}
	return context.WithTimeout(context.Background(), 5*time.Second)
}

// recordMCPCallStart 记录一次 MCP 工具调用进入审批/放行判定。
func (s *Server) recordMCPCallStart(ctx context.Context, conversationID, runID, toolUseID, toolName, decision string, toolInput json.RawMessage) {
	if !strings.HasPrefix(toolName, "mcp__") {
		return
	}
	ctx, cancel := mcpAuditWriteContext(ctx)
	defer cancel()
	server, tool := splitMCPToolName(toolName)
	preview := summarizeMCPToolInput(toolInput)
	now := time.Now().UTC()
	if toolUseID != "" {
		res, err := s.db.ExecContext(ctx, `update mcp_call_audit set decision=?,args_preview=?,conversation_id=?,run_id=?,updated_at=? where tool_use_id=?`,
			decision, preview, conversationID, runID, now, toolUseID)
		if err == nil {
			if affected, _ := res.RowsAffected(); affected > 0 {
				return
			}
		}
	}
	_, _ = s.db.ExecContext(ctx, `insert into mcp_call_audit
		(id,tool_use_id,conversation_id,run_id,server_name,tool_name,args_preview,decision,status,error_text,duration_ms,created_at,updated_at)
		values (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"audit_"+uuid.NewString(), toolUseID, conversationID, runID, server, tool, preview, decision, "pending", "", 0, now, now)
}

// recordMCPCallFinish 回填一次 MCP 工具调用的执行结果。
func (s *Server) recordMCPCallFinish(ctx context.Context, conversationID, runID, toolUseID, toolName, status, errorText string) {
	if !strings.HasPrefix(toolName, "mcp__") {
		return
	}
	ctx, cancel := mcpAuditWriteContext(ctx)
	defer cancel()
	errorText = truncateAuditText(errorText, mcpAuditErrorLimit)
	now := time.Now().UTC()
	if toolUseID != "" {
		res, err := s.db.ExecContext(ctx, `update mcp_call_audit
			set status=?,error_text=?,updated_at=?,
			    duration_ms=cast((julianday(?)-julianday(created_at))*86400000 as integer)
			where tool_use_id=?`,
			status, errorText, now, now, toolUseID)
		if err == nil {
			if affected, _ := res.RowsAffected(); affected > 0 {
				return
			}
		}
	}
	server, tool := splitMCPToolName(toolName)
	_, _ = s.db.ExecContext(ctx, `insert into mcp_call_audit
		(id,tool_use_id,conversation_id,run_id,server_name,tool_name,args_preview,decision,status,error_text,duration_ms,created_at,updated_at)
		values (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"audit_"+uuid.NewString(), toolUseID, conversationID, runID, server, tool, "", "", status, errorText, 0, now, now)
}

// mcpToolResultStatus 从 PostToolUse 的 tool_response 判断执行结果。
// 判定依据按可靠性排序：显式 is_error / error 字段 → 文本里的错误前缀 → 默认成功。
func mcpToolResultStatus(raw json.RawMessage) (string, string) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return "ok", ""
	}
	var decoded struct {
		IsError  *bool           `json:"is_error"`
		Error    string          `json:"error"`
		Content  json.RawMessage `json:"content"`
		ExitCode *int            `json:"exit_code"`
	}
	if json.Unmarshal(raw, &decoded) == nil {
		if decoded.IsError != nil && *decoded.IsError {
			return "error", truncateAuditText(string(decoded.Content), mcpAuditErrorLimit)
		}
		if strings.TrimSpace(decoded.Error) != "" {
			return "error", truncateAuditText(decoded.Error, mcpAuditErrorLimit)
		}
		if decoded.ExitCode != nil && *decoded.ExitCode != 0 {
			return "error", truncateAuditText("exit code "+strconv.Itoa(*decoded.ExitCode), mcpAuditErrorLimit)
		}
	}
	return "ok", ""
}

// ---------------------------------------------------------------------------
// HTTP handler：GET / DELETE /api/mcp/audit
// ---------------------------------------------------------------------------

type mcpAuditView struct {
	ID             string    `json:"id"`
	ServerName     string    `json:"serverName"`
	ToolName       string    `json:"toolName"`
	ConversationID string    `json:"conversationId"`
	RunID          string    `json:"runId"`
	ProjectID      string    `json:"projectId,omitempty"`
	ArgsPreview    string    `json:"argsPreview,omitempty"`
	Decision       string    `json:"decision,omitempty"`
	Status         string    `json:"status,omitempty"`
	Error          string    `json:"error,omitempty"`
	DurationMs     int64     `json:"durationMs"`
	CreatedAt      time.Time `json:"createdAt"`
}

type mcpAuditResponse struct {
	Entries []mcpAuditView `json:"entries"`
	Total   int            `json:"total"`
}

func (s *Server) listMCPCallAudit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// 保留上限内的记录：读接口顺手清理，避免写路径每次都跑一遍子查询。
	if _, err := s.db.ExecContext(ctx, `delete from mcp_call_audit where id not in (
		select id from mcp_call_audit order by created_at desc, id desc limit ?)`, mcpAuditRetention); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	clauses := []string{}
	args := []any{}
	joined := false
	if projectID := strings.TrimSpace(r.URL.Query().Get("projectId")); projectID != "" {
		joined = true
		clauses = append(clauses, `c.project_id=?`)
		args = append(args, projectID)
	}
	if serverName := strings.TrimSpace(r.URL.Query().Get("serverName")); serverName != "" {
		clauses = append(clauses, `a.server_name=?`)
		args = append(args, serverName)
	}
	if decision := strings.TrimSpace(r.URL.Query().Get("decision")); decision != "" {
		clauses = append(clauses, `a.decision=?`)
		args = append(args, decision)
	}
	where := ""
	if len(clauses) > 0 {
		where = " where " + strings.Join(clauses, " and ")
	}
	from := " from mcp_call_audit a"
	if joined {
		from += " left join conversations c on c.id=a.conversation_id"
	}

	var total int
	if err := s.db.QueryRowContext(ctx, `select count(*)`+from+where, args...).Scan(&total); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	limit := 100
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= 500 {
			limit = parsed
		}
	}
	offset := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("offset")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed >= 0 {
			offset = parsed
		}
	}
	projectExpr := "''"
	if joined {
		projectExpr = "coalesce(c.project_id,'')"
	}
	query := `select a.id,a.server_name,a.tool_name,a.conversation_id,a.run_id,` + projectExpr + `,
		a.args_preview,a.decision,a.status,a.error_text,a.duration_ms,a.created_at` + from + where +
		` order by a.created_at desc, a.id desc limit ? offset ?`
	rows, err := s.db.QueryContext(ctx, query, append(append([]any{}, args...), limit, offset)...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	entries := []mcpAuditView{}
	for rows.Next() {
		var entry mcpAuditView
		if err := rows.Scan(&entry.ID, &entry.ServerName, &entry.ToolName, &entry.ConversationID, &entry.RunID,
			&entry.ProjectID, &entry.ArgsPreview, &entry.Decision, &entry.Status, &entry.Error, &entry.DurationMs, &entry.CreatedAt); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, mcpAuditResponse{Entries: entries, Total: total})
}

func (s *Server) clearMCPCallAudit(w http.ResponseWriter, r *http.Request) {
	projectID := strings.TrimSpace(r.URL.Query().Get("projectId"))
	if projectID != "" {
		if _, err := s.db.ExecContext(r.Context(), `delete from mcp_call_audit where conversation_id in (
			select id from conversations where project_id=?)`, projectID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	} else {
		if _, err := s.db.ExecContext(r.Context(), `delete from mcp_call_audit`); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
