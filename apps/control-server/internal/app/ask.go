package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// 会话内提问（Ask User）。
//
// 为什么单独成文件：提问与审批共享"阻塞等人"的形状，但语义完全不同 ——
// 审批是一次二元裁决，提问是多题结构化作答。两者共用一个 waiter 会让重放、
// 手机通知与卡片渲染三处都长出条件分支（见 docs/51 §8）。
//
// 通道只有一条：模型调用注入的 MCP 工具 mcp__milevia__ask_user，本服务阻塞等桌面端
// 作答，再把答案作为工具结果返回。原生 AskUserQuestion 不做，原因见 docs/51 §2。

const (
	askStatusAnswered  = "answered"
	askStatusSkipped   = "skipped"
	askStatusExpired   = "expired"
	askStatusCancelled = "cancelled"

	// 这些上限写进 docs/51 §4.6。它们的意义不是"防攻击"，而是让提问不会比审批更
	// 廉价：没有上限时，一个绕圈的模型可以每 10 秒弹一次卡片。
	askMaxQuestions   = 4
	askMaxOptions     = 8
	askMaxAnswerBytes = 4 << 10
	askMaxPerRun      = 5

	// interactionServerName 是内置 MCP server 的名字，工具名为 ask_user，
	// 在 CLI 侧呈现为 mcp__milevia__ask_user。
	interactionServerName = "milevia"
	interactionServerID   = "milevia-interaction"
	interactionToolName   = "ask_user"
	interactionMCPPath    = "/api/internal/mcp/interaction"

	askMaxRequestBody = 1 << 20
)

type askOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type askQuestion struct {
	ID          string      `json:"id"`
	Header      string      `json:"header"`
	Question    string      `json:"question"`
	Kind        string      `json:"kind,omitempty"`
	Options     []askOption `json:"options,omitempty"`
	MultiSelect bool        `json:"multiSelect,omitempty"`
	Placeholder string      `json:"placeholder,omitempty"`
	Min         *float64    `json:"min,omitempty"`
	Max         *float64    `json:"max,omitempty"`
	Step        *float64    `json:"step,omitempty"`
	Unit        string      `json:"unit,omitempty"`
}

type askWaiter struct {
	askID          string
	runID          string
	conversationID string
	questions      []askQuestion
	result         chan askOutcome
}

type askOutcome struct {
	status  string
	answers map[string]string
	notes   string
}

// askBudget 记录一个会话当前 run 已发起的提问次数。
//
// 以会话为键、内部记 runID：run 换了自动归零，不需要在每条 run 收尾路径上再挂一次
// 清理 —— 那种清理最容易在新增收尾路径时被漏掉。
type askBudget struct {
	runID string
	count int
}

// beginAsk 发起一次提问并阻塞等待结果。
//
// 返回的 status 一定是四个终态之一；工具结果文案由调用方拼装。
func (s *Server) beginAsk(ctx context.Context, conversationID, runID string, questions []askQuestion) (askOutcome, error) {
	s.mu.Lock()
	if existing := s.askForConversationLocked(conversationID); existing != nil {
		s.mu.Unlock()
		// 并发提问只可能来自子代理。让后来者拿到一句明确的话，比让它再挂一个等待更可预测。
		return askOutcome{}, errors.New("当前会话已有一个未回答的问题，请等它结束后再提问")
	}
	budget := s.askBudgets[conversationID]
	if budget == nil || budget.runID != runID {
		budget = &askBudget{runID: runID}
		s.askBudgets[conversationID] = budget
	}
	if budget.count >= askMaxPerRun {
		s.mu.Unlock()
		return askOutcome{}, fmt.Errorf("本轮提问次数已用完（上限 %d 次），请自行决策并说明依据", askMaxPerRun)
	}
	budget.count++

	waiter := &askWaiter{
		askID:          uuid.NewString(),
		runID:          runID,
		conversationID: conversationID,
		questions:      questions,
		result:         make(chan askOutcome, 1),
	}
	s.asks[waiter.askID] = waiter
	s.mu.Unlock()

	pending := map[string]any{
		"askId":     waiter.askID,
		"source":    "mcp",
		"status":    "pending",
		"questions": questions,
	}
	s.appendEvent(runID, conversationID, "ask.pending", mustJSON(pending))
	s.notifyAskPending(conversationID, questions)

	timeout := s.config.UserAskTimeout
	if timeout <= 0 {
		timeout = defaultUserAskTimeout
	}

	select {
	case outcome := <-waiter.result:
		s.appendAskTerminal(waiter, outcome)
		return outcome, nil
	case <-ctx.Done():
		if s.settleAsk(waiter, askStatusCancelled) {
			return askOutcome{status: askStatusCancelled}, nil
		}
		return s.drainSettledOutcome(waiter, askStatusCancelled), nil
	case <-time.After(timeout):
		if s.settleAsk(waiter, askStatusExpired) {
			return askOutcome{status: askStatusExpired}, nil
		}
		return s.drainSettledOutcome(waiter, askStatusExpired), nil
	}
}

// drainSettledOutcome 处理"超时/取消与作答同时发生"的竞态：裁决方刚把 waiter 摘掉并
// 写进通道，这里必须把那个结果读出来，不能丢掉用户的答案。
//
// ⚠️ 终态事件必须在这里补落。裁决方（answerAsk / resolveRunAsks）只负责摘 waiter 与
// 投递结果，谁摘到 waiter 谁落事件；一旦 beginAsk 的 select 同时命中超时分支，settleAsk
// 会落空、结果又被这里读走 —— 事件就一条都没有，卡片永久停在"等待你的回答"，而模型其实
// 已经拿到答案继续跑了。走 fallback 时同样要落，返回给模型什么状态，时间线就记什么状态。
func (s *Server) drainSettledOutcome(waiter *askWaiter, fallback string) askOutcome {
	select {
	case outcome := <-waiter.result:
		s.appendAskTerminal(waiter, outcome)
		return outcome
	case <-time.After(50 * time.Millisecond):
		outcome := askOutcome{status: fallback}
		s.appendAskTerminal(waiter, outcome)
		return outcome
	}
}

// settleAsk 在超时/取消路径上摘掉 waiter 并落终态事件。
//
// 只有真正摘到 waiter 的那一方才有权落事件，避免"用户刚好答了"与"刚好超时"
// 同时写出两条终态。
func (s *Server) settleAsk(waiter *askWaiter, status string) bool {
	s.mu.Lock()
	current, ok := s.asks[waiter.askID]
	if ok && current == waiter {
		delete(s.asks, waiter.askID)
	}
	s.mu.Unlock()
	if !ok || current != waiter {
		return false
	}
	s.appendAskTerminal(waiter, askOutcome{status: status})
	return true
}

func (s *Server) appendAskTerminal(waiter *askWaiter, outcome askOutcome) {
	payload := map[string]any{"askId": waiter.askID, "status": outcome.status}
	if len(outcome.answers) > 0 {
		payload["answers"] = outcome.answers
	}
	if outcome.notes != "" {
		payload["notes"] = outcome.notes
	}
	s.appendEvent(waiter.runID, waiter.conversationID, "ask."+outcome.status, mustJSON(payload))
}

func (s *Server) askForConversationLocked(conversationID string) *askWaiter {
	for _, waiter := range s.asks {
		if waiter.conversationID == conversationID {
			return waiter
		}
	}
	return nil
}

// resolveRunAsks 把某个 run 名下所有待答问题置为取消。
//
// 与 resolveRunApprovals 成对调用：run 停止/失败/进程退出时，不能让卡片永远停在
// "等待你的回答"。
func (s *Server) resolveRunAsks(runID string) {
	s.mu.Lock()
	waiters := make([]*askWaiter, 0)
	for askID, waiter := range s.asks {
		if waiter.runID == runID {
			delete(s.asks, askID)
			waiters = append(waiters, waiter)
		}
	}
	s.mu.Unlock()
	for _, waiter := range waiters {
		select {
		case waiter.result <- askOutcome{status: askStatusCancelled}:
		default:
		}
	}
}

// answerAsk 是桌面端的作答入口。
func (s *Server) answerAsk(w http.ResponseWriter, r *http.Request) {
	askID := chi.URLParam(r, "askID")
	var input struct {
		Action  string            `json:"action"`
		Answers map[string]string `json:"answers"`
		Notes   string            `json:"notes"`
	}
	if !decode(w, r, &input) {
		return
	}

	s.mu.Lock()
	waiter, ok := s.asks[askID]
	if ok {
		delete(s.asks, askID)
	}
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusConflict, errors.New("这个问题已经结束，无法再作答"))
		return
	}

	outcome := askOutcome{status: askStatusAnswered, answers: map[string]string{}, notes: strings.TrimSpace(input.Notes)}
	if input.Action == "skip" {
		outcome = askOutcome{status: askStatusSkipped}
	} else {
		known := make(map[string]bool, len(waiter.questions))
		for _, question := range waiter.questions {
			known[question.ID] = true
		}
		for id, answer := range input.Answers {
			// 未知题目直接忽略而不是报错：前端可能带着上一版的问题结构提交。
			if !known[id] {
				continue
			}
			answer = strings.TrimSpace(answer)
			if answer == "" {
				continue
			}
			if len(answer) > askMaxAnswerBytes {
				answer = truncateBytesHead(answer, askMaxAnswerBytes) + "…"
			}
			outcome.answers[id] = answer
		}
		if len(outcome.answers) == 0 && outcome.notes == "" {
			outcome.status = askStatusSkipped
		}
	}

	select {
	case waiter.result <- outcome:
	default:
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": outcome.status})
}

// notifyAskPending 与提问解耦：通知失败绝不能让提问失败。
func (s *Server) notifyAskPending(conversationID string, questions []askQuestion) {
	if len(questions) == 0 {
		return
	}
	var projectID, projectName string
	if err := s.db.QueryRowContext(s.runtimeCtx,
		`select c.project_id, coalesce(p.name,'') from conversations c left join projects p on p.id=c.project_id where c.id=?`,
		conversationID).Scan(&projectID, &projectName); err != nil {
		return
	}
	header := questions[0].Header
	if header == "" {
		header = "需要你回答"
	}
	s.broadcastNotification(NotificationEvent{
		ID:             uuid.NewString(),
		Type:           "ask.pending",
		ProjectID:      projectID,
		ProjectName:    projectName,
		ConversationID: conversationID,
		Title:          "有一个问题在等你回答",
		Body:           truncateRunes(header+"："+questions[0].Question, 120),
		Priority:       "high",
		ActionURL:      "/projects/" + projectID + "/conversations/" + conversationID,
		DedupeKey:      "ask:" + conversationID,
	})
}

// askAllowedForPolicy 决定某个权限档要不要注入提问能力。
//
// 只读档不注入：codebuddy 在 plan 档下想放行 MCP 调用只能把 DeferExecuteTool 一起放开
// （等于放开所有延迟工具），而只读会话本来就无法执行动作，问了也做不了（docs/51 §3.2）。
func askAllowedForPolicy(policy string) bool {
	switch policy {
	case "read_only", "plan":
		return false
	default:
		return true
	}
}

// interactionMCPServer 生成内置提问 server 的注入定义。
//
// 头部即凭据：CLI 拿不到桌面会话，只能用 per-session / per-run 的 approval token
// （与审批 hook 同一套，见 resolveInteractionIdentity）。
func (s *Server) interactionMCPServer(identity mcpInteractionIdentity) storedMCPServer {
	headers := map[string]string{"X-Auto-Approval-Token": identity.ApprovalToken}
	if identity.ConversationID != "" {
		headers["X-Auto-Conversation-ID"] = identity.ConversationID
	}
	if identity.RunID != "" {
		headers["X-Auto-Run-ID"] = identity.RunID
	}
	budget := s.config.UserAskTimeout
	if budget <= 0 {
		budget = defaultUserAskTimeout
	}
	server := storedMCPServer{headersRaw: headers}
	server.ID = interactionServerID
	server.Name = interactionServerName
	server.DisplayName = "Milevia 提问"
	server.Description = "会话内提问通道：模型调用后会等你在会话页作答。"
	controlURL := strings.TrimRight(strings.TrimSpace(s.config.ControlURL), "/")
	if controlURL == "" {
		controlURL = "http://127.0.0.1:8080"
	}
	server.Transport = mcpTransportHTTP
	server.URL = controlURL + interactionMCPPath
	server.Headers = headers
	server.Enabled = true
	server.StartupTimeoutSec = 10
	// 客户端工具超时必须比服务端等待预算长：反过来会先被 CLI 掐断，
	// 模型看到的是"工具失败"，而不是"用户没回答"（docs/51 §4.5）。
	server.ToolTimeoutSec = int(budget.Seconds()) + 60
	return server
}

// interactionApprovalToolName 是审批 hook 里要直接放行的那一个工具名。
//
// Claude 把 MCP 工具命名为 mcp__<server>__<tool>；这是平台自己的提问通道，必须先于
// 要不要弹审批的判断放行，否则用户要先批准、再回答，两次交互。
func interactionApprovalToolName() string {
	return "mcp__" + interactionServerName + "__" + interactionToolName
}

// ---- 问题结构校验 ----

func parseAskQuestions(raw json.RawMessage) ([]askQuestion, error) {
	var payload struct {
		Questions []askQuestion `json:"questions"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("参数不是合法 JSON：%w", err)
	}
	if len(payload.Questions) == 0 {
		return nil, errors.New("至少需要一道问题")
	}
	if len(payload.Questions) > askMaxQuestions {
		return nil, fmt.Errorf("一次最多 %d 道问题", askMaxQuestions)
	}
	seen := map[string]bool{}
	out := make([]askQuestion, 0, len(payload.Questions))
	for index, question := range payload.Questions {
		question.ID = strings.TrimSpace(question.ID)
		if question.ID == "" {
			return nil, fmt.Errorf("第 %d 道问题缺少 id", index+1)
		}
		if seen[question.ID] {
			return nil, fmt.Errorf("问题 id %q 重复", question.ID)
		}
		seen[question.ID] = true
		question.Question = strings.TrimSpace(question.Question)
		if question.Question == "" {
			return nil, fmt.Errorf("问题 %q 缺少正文", question.ID)
		}
		switch question.Kind {
		case "":
			question.Kind = "choice"
		case "choice", "text", "number":
		default:
			return nil, fmt.Errorf("问题 %q 的 kind 只能是 choice / text / number", question.ID)
		}
		switch question.Kind {
		case "choice":
			if len(question.Options) < 2 || len(question.Options) > askMaxOptions {
				return nil, fmt.Errorf("问题 %q 需要 2~%d 个选项", question.ID, askMaxOptions)
			}
			for i, option := range question.Options {
				if strings.TrimSpace(option.Label) == "" {
					return nil, fmt.Errorf("问题 %q 的第 %d 个选项缺少 label", question.ID, i+1)
				}
			}
		case "text":
			question.Options = nil
			question.MultiSelect = false
		case "number":
			if question.Min == nil || question.Max == nil {
				return nil, fmt.Errorf("问题 %q 是数字题，必须给 min 与 max", question.ID)
			}
			if *question.Min > *question.Max {
				return nil, fmt.Errorf("问题 %q 的 min 大于 max", question.ID)
			}
			question.Options = nil
			question.MultiSelect = false
		}
		out = append(out, question)
	}
	return out, nil
}

// ---- MCP 端点 ----

type interactionIdentity struct {
	runID          string
	conversationID string
}

// resolveInteractionIdentity 与 waitForApproval 的凭据判定同源：会话维度优先，
// run 维度兜底（一次性运行的 CLI 没有常驻会话）。
func (s *Server) resolveInteractionIdentity(r *http.Request) (interactionIdentity, error) {
	token := r.Header.Get("X-Auto-Approval-Token")
	conversationID := r.Header.Get("X-Auto-Conversation-ID")
	runID := r.Header.Get("X-Auto-Run-ID")
	if token == "" || (conversationID == "" && runID == "") {
		return interactionIdentity{}, errors.New("缺少鉴权信息")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if conversationID != "" {
		session := s.sessions[conversationID]
		if session == nil || session.approvalToken != token {
			return interactionIdentity{}, errors.New("会话未激活")
		}
		return interactionIdentity{runID: session.activeRunID, conversationID: conversationID}, nil
	}
	if s.runTokens[runID] != token {
		return interactionIdentity{}, errors.New("run 未激活")
	}
	conversation, ok := s.runContexts[runID]
	if !ok {
		return interactionIdentity{}, errors.New("run 不存在")
	}
	return interactionIdentity{runID: runID, conversationID: conversation}, nil
}

func (s *Server) interactionMCP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Auto-Approval-Token") == "" {
		writeError(w, http.StatusUnauthorized, errors.New("缺少鉴权信息"))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, askMaxRequestBody+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if len(body) > askMaxRequestBody {
		writeError(w, http.StatusRequestEntityTooLarge, errors.New("请求体过大"))
		return
	}

	var request struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, mcpError(nil, -32700, "无法解析 JSON-RPC 请求"))
		return
	}
	// 通知没有 id：按规范回 202 空体。
	if len(request.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	reply := func(result any) {
		writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}

	// 握手阶段（initialize / tools/list）只校验"带了 token"，不解析会话归属：
	// CLI 进程刚起来就发 initialize，此刻 StartSession 还没返回、会话行尚未登记，
	// 严格校验会把每一次握手都判成 401，表现是"这个 MCP server 根本没连上"。
	// 真正需要归属的是 tools/call，那一定发生在回合里。
	switch request.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(request.Params, &params)
		version := params.ProtocolVersion
		if version == "" {
			version = "2025-06-18"
		}
		reply(map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": interactionServerName, "version": "1.0.0"},
		})
	case "tools/list":
		reply(map[string]any{"tools": []any{interactionToolDefinition()}})
	case "tools/call":
		identity, err := s.resolveInteractionIdentity(r)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, mcpError(request.ID, -32001, err.Error()))
			return
		}
		s.handleInteractionToolCall(w, identity, request.ID, request.Params, reply)
	case "ping":
		reply(map[string]any{})
	default:
		writeJSON(w, http.StatusOK, mcpError(request.ID, -32601, "未知方法 "+request.Method))
	}
}

func (s *Server) handleInteractionToolCall(w http.ResponseWriter, identity interactionIdentity, id json.RawMessage, params json.RawMessage, reply func(any)) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		writeJSON(w, http.StatusOK, mcpError(id, -32602, "参数不是合法 JSON"))
		return
	}
	if call.Name != interactionToolName {
		writeJSON(w, http.StatusOK, mcpError(id, -32602, "未知工具 "+call.Name))
		return
	}
	if identity.runID == "" || identity.conversationID == "" {
		reply(toolError("当前没有进行中的回合，无法提问。"))
		return
	}
	questions, err := parseAskQuestions(call.Arguments)
	if err != nil {
		reply(toolError(err.Error()))
		return
	}
	outcome, err := s.beginAsk(rContext(s), identity.conversationID, identity.runID, questions)
	if err != nil {
		reply(toolError(err.Error()))
		return
	}
	reply(toolText(askResultText(questions, outcome)))
}

// rContext 返回服务级 ctx：提问的等待不应被 HTTP 请求的取消拖走 —— 客户端断线
// （CLI 侧超时/重连）不代表用户不答了，真正的取消源是 run 停止。
func rContext(s *Server) context.Context {
	return s.runtimeCtx
}

func interactionToolDefinition() map[string]any {
	return map[string]any{
		"name": interactionToolName,
		"description": "向用户提问并等待回答。当某个选择会实质影响结果、且无法从仓库或对话里确定时使用；" +
			"能自行判断时不要滥用，也不要把它当权限申请（权限由审批流程处理）。" +
			"调用会阻塞直到用户作答或超时；超时后请明确写出你所采用的假设。",
		"inputSchema": map[string]any{
			"type":     "object",
			"required": []string{"questions"},
			"properties": map[string]any{
				"questions": map[string]any{
					"type":     "array",
					"minItems": 1,
					"maxItems": askMaxQuestions,
					"items": map[string]any{
						"type":     "object",
						"required": []string{"id", "header", "question"},
						"properties": map[string]any{
							"id":       map[string]any{"type": "string", "description": "本题的稳定标识，答案以它为键。"},
							"header":   map[string]any{"type": "string", "description": "卡片上的短标签，不超过 12 字。"},
							"question": map[string]any{"type": "string", "description": "完整问题，以问号结尾。"},
							"kind": map[string]any{
								"type":        "string",
								"enum":        []string{"choice", "text", "number"},
								"description": "choice（默认）从选项里选；text 自由输入；number 数值。",
							},
							"options": map[string]any{
								"type":        "array",
								"maxItems":    askMaxOptions,
								"description": "kind=choice 时必填，2~8 项。",
								"items": map[string]any{
									"type":     "object",
									"required": []string{"label"},
									"properties": map[string]any{
										"label":       map[string]any{"type": "string"},
										"description": map[string]any{"type": "string"},
									},
								},
							},
							"multiSelect": map[string]any{"type": "boolean", "description": "仅 choice：是否可多选。"},
							"placeholder": map[string]any{"type": "string", "description": "仅 text：输入框占位。"},
							"min":         map[string]any{"type": "number", "description": "仅 number：最小值。"},
							"max":         map[string]any{"type": "number", "description": "仅 number：最大值。"},
							"step":        map[string]any{"type": "number", "description": "仅 number：步长。"},
							"unit":        map[string]any{"type": "string", "description": "仅 number：单位。"},
						},
					},
				},
			},
		},
	}
}

// askResultText 是模型真正读到的东西。措辞要求：
//   - 已答的逐题列出，用 header 而不是 id（模型更容易引用）；
//   - 没答的单独一行，避免模型把"没答"当成"答了空字符串"；
//   - 超时/跳过时明确禁止臆造答案，但允许带假设继续（见 docs/51 §4.3）。
func askResultText(questions []askQuestion, outcome askOutcome) string {
	if outcome.status == askStatusExpired || outcome.status == askStatusCancelled {
		return "用户没有回答问题（等待超时）。不要猜测用户的选择；如果必须继续，请明确写出你采用的假设。"
	}
	if outcome.status == askStatusSkipped {
		return "用户跳过了这次提问。不要猜测用户的选择；如果必须继续，请明确写出你采用的假设。"
	}
	var answered, unanswered []string
	for _, question := range questions {
		label := question.Header
		if label == "" {
			label = question.ID
		}
		answer := strings.TrimSpace(outcome.answers[question.ID])
		if answer == "" {
			unanswered = append(unanswered, label)
			continue
		}
		answered = append(answered, "- "+label+"："+answer)
	}
	var builder strings.Builder
	if len(answered) > 0 {
		builder.WriteString("用户已回答：\n")
		builder.WriteString(strings.Join(answered, "\n"))
	}
	if outcome.notes != "" {
		if builder.Len() > 0 {
			builder.WriteString("\n")
		}
		builder.WriteString("用户补充：")
		builder.WriteString(outcome.notes)
	}
	if len(unanswered) > 0 {
		if builder.Len() > 0 {
			builder.WriteString("\n")
		}
		builder.WriteString("用户没有回答：")
		builder.WriteString(strings.Join(unanswered, "、"))
	}
	return builder.String()
}

func toolText(text string) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
}

func toolError(text string) map[string]any {
	result := toolText(text)
	result["isError"] = true
	return result
}

func mcpError(id json.RawMessage, code int, message string) map[string]any {
	var rawID any
	if len(id) > 0 {
		_ = json.Unmarshal(id, &rawID)
	}
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      rawID,
		"error":   map[string]any{"code": code, "message": message},
	}
}

// reconcilePendingAsks 在启动时把没有终态的提问置为取消。
//
// waiter 全在内存里，控制服务重启后不会有人再作答；不清理的话，那张卡会永远显示
// "等待你的回答"。审批靠 5 分钟超时自愈，提问的等待预算更长，所以启动时要补一刀。
func (s *Server) reconcilePendingAsks(ctx context.Context) {
	cutoff := time.Now().UTC().Add(-7 * 24 * time.Hour)
	rows, err := s.db.QueryContext(ctx,
		"select id, conversation_id, run_id, payload from events where type='ask.pending' and created_at>?", cutoff)
	if err != nil {
		log.Printf("reconcile pending asks: %v", err)
		return
	}
	type pendingAsk struct {
		eventID        string
		conversationID string
		runID          string
		payload        string
	}
	pending := make([]pendingAsk, 0)
	for rows.Next() {
		var item pendingAsk
		if err := rows.Scan(&item.eventID, &item.conversationID, &item.runID, &item.payload); err == nil {
			pending = append(pending, item)
		}
	}
	rows.Close()
	for _, item := range pending {
		var payload struct {
			AskID string `json:"askId"`
		}
		if json.Unmarshal([]byte(item.payload), &payload) != nil || payload.AskID == "" {
			continue
		}
		var settled int
		if err := s.db.QueryRowContext(ctx,
			"select count(*) from events where type like 'ask.%' and type<>'ask.pending' and payload like ?",
			"%"+payload.AskID+"%").Scan(&settled); err == nil && settled > 0 {
			continue
		}
		s.appendEvent(item.runID, item.conversationID, "ask.cancelled",
			mustJSON(map[string]any{"askId": payload.AskID, "status": askStatusCancelled, "reason": "restart"}))
	}
}
