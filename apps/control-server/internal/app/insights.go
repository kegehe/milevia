package app

// 项目主动优化建议（Insights）—— 见 docs/25-项目主动优化建议实现方案.md
//
// 在项目真实目录上跑两次只读 agent（发现 → 独立核实），产出用户可读的
// 优化建议逻辑卡片。核心诉求：
//  1. 主动：AI 自行通读项目，不等用户撞见问题。
//  2. 用户可读：卡片是"界面能看到/用到"的逻辑描述，而非代码级诊断。
//  3. 不重复（规则 1）与必须核实（规则 2），见 docs/25 §1.2/§4。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// truncateInsightLog 把原始 agent 输出截断到安全长度，便于日志排查。
// 截断按字节数限制，但回退到 UTF-8 字符边界，避免把多字节字符（中文）切半
// 产生 U+FFFD 替换符。
func truncateInsightLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	// 从 cut 处回退：只要 s[cut] 是 UTF-8 尾随字节（10xxxxxx），就继续向前找字符起始字节。
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…<truncated>"
}

// 发现类型枚举。
const (
	insightBug          = "bug"
	insightStyle        = "style"
	insightOptimization = "optimization"
	insightFeature      = "feature"
)

// 严重度枚举。
const (
	insightSeverityLow    = "low"
	insightSeverityNormal = "normal"
	insightSeverityHigh   = "high"
)

// 扫描状态枚举。
const (
	insightScanRunning   = "running"
	insightScanCompleted = "completed"
	insightScanFailed    = "failed"
	insightScanCancelled = "cancelled"
)

// 建议再验证（re-verify）结果状态枚举。验证状态存于 project_insights 行本身
// （verification_result/note/verified_at），由 GET /insights 轮询带回，不新建
// 独立验证任务表。值与前端 InsightVerificationResult 一一对应。
const (
	insightVerifyPending = "pending" // 验证中（后台 agent 在跑）
	insightVerifyValid   = "valid"   // 确认仍存在，可继续处理
	insightVerifyInvalid = "invalid" // 已失效（已修复/已实现/伪建议），从有效列表隐藏
	insightVerifyFailed  = "failed"  // 验证失败（agent 运行或输出解析失败），可重试
)

// 每条扫描产出的发现数量上限（与服务端数量钳一致的硬顶）。
const insightFindingsCap = 30

// Pass B 与再验证使用同一批大小，保证候选数量不会在第二轮重新膨胀。
const insightVerifyBatchSize = 20

// insightVerifyBatchSize 单趟再验证 agent 一次核验的建议数上限。超过时分批串行跑，
// 避免单趟 prompt 过大、agent 通读过多文件导致超时/上下文偏紧。
// insightScanPassTimeout 是单趟只读 agent（Pass A 发现 / Pass B 核实）的执行上限。
// 超时后进程会被终止，扫描置 failed 并明确提示"分析超时"（见 runReadOnlyAgent 的
// scanCtx.Err() 分支），避免用户面对无解释的"项目分析失败"。
const insightScanPassTimeout = 20 * time.Minute

// 再验证可以分批，但整个请求必须有总上限，避免单批超时累计成无限后台任务。
const insightVerifyRunTimeout = 30 * time.Minute

// insightPersistenceTimeout 为服务关闭后写入任务终态预留时间。
const insightPersistenceTimeout = 10 * time.Second

// 主题方向枚举（空串 = 全面分析）。
const (
	insightThemeSecurity  = "security"
	insightThemePerf      = "performance"
	insightThemeUX        = "ux"
	insightThemeArch      = "architecture"
	insightThemeStability = "stability"
)

// insightThemeLabels 供 prompt 组装：主题 → 用户可读的中文标签。
var insightThemeLabels = map[string]string{
	insightThemeSecurity:  "安全",
	insightThemePerf:      "性能",
	insightThemeUX:        "UX",
	insightThemeArch:      "架构",
	insightThemeStability: "稳定性",
}

// scanRequest POST /insights/scan 的可选 body：选定方向（theme + types）。
// theme 空串 = 全面分析；types 空/全选 = 全查。两者都可省略，缺省等价于原全量扫描。
type scanRequest struct {
	Theme string   `json:"theme"`
	Types []string `json:"types"`
}

// scanOpts 本次扫描的方向，供 prompt 组装与入库。
type scanOpts struct {
	Theme string
	Types []string // 已归一化、去重；空 = 全查
}

// normalizeScanTheme 把请求里的主题归一到白名单；非法/空 → ""（全面分析）。
func normalizeScanTheme(t string) string {
	if _, ok := insightThemeLabels[t]; ok {
		return t
	}
	return ""
}

// normalizeScanTypes 归一化请求里的类型列表：逐项 white-list、去重；空/全选 → nil（全查）。
func normalizeScanTypes(types []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range types {
		valid := false
		for _, known := range insightTypeOrder {
			if t == known {
				valid = true
				break
			}
		}
		if !valid || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	// 全四个都被勾选等价于全查 → 归 nil，让 prompt 走"覆盖四类"原文。
	if len(out) == len(insightTypeOrder) {
		return nil
	}
	return out
}

// buildScanOpts 把 scanRequest 解析为规范化 scanOpts（供入库与 prompt 消费）。
func buildScanOpts(req scanRequest) scanOpts {
	return scanOpts{Theme: normalizeScanTheme(req.Theme), Types: normalizeScanTypes(req.Types)}
}

// InsightScan 一条项目分析扫描（含两趟 agent 运行）的状态行。
type InsightScan struct {
	ID              string     `json:"id"`
	ProjectID       string     `json:"projectId"`
	Status          string     `json:"status"`
	Error           string     `json:"error,omitempty"`
	Agent           string     `json:"agent"`
	Theme           string     `json:"theme,omitempty"`      // 本次扫描聚焦主题（''=全面分析）
	FocusTypes      []string   `json:"focusTypes,omitempty"` // 本次扫描限定查找的类型（空=全查）
	FindingsCount   int        `json:"findingsCount"`
	SuppressedCount int        `json:"suppressedCount"`
	CreatedAt       time.Time  `json:"createdAt"`
	StartedAt       *time.Time `json:"startedAt,omitempty"`
	CompletedAt     *time.Time `json:"completedAt,omitempty"`
}

// InsightFinding 一条用户可读的优化建议卡片。
// VerificationResult/Note/VerifiedAt 为「建议再验证」（re-verify）的落库状态，
// ”=未验证、pending=验证中、valid=确认仍存在、invalid=已失效、failed=验证失败。
type InsightFinding struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"projectId"`
	ScanID    string    `json:"scanId"`
	Type      string    `json:"type"`
	Severity  string    `json:"severity"`
	Title     string    `json:"title"`
	Summary   string    `json:"summary"`
	FileHint  string    `json:"fileHint,omitempty"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"-"`

	VerificationResult string     `json:"verificationResult,omitempty"`
	VerificationNote   string     `json:"verificationNote,omitempty"`
	VerifiedAt         *time.Time `json:"verifiedAt,omitempty"`
}

// InsightVerificationRun 是一次异步再验证任务的可观察状态。
type InsightVerificationRun struct {
	ID             string     `json:"id"`
	ProjectID      string     `json:"projectId"`
	Status         string     `json:"status"`
	Error          string     `json:"error,omitempty"`
	Message        string     `json:"message,omitempty"`
	TotalCount     int        `json:"totalCount"`
	ProcessedCount int        `json:"processedCount"`
	CreatedAt      time.Time  `json:"createdAt"`
	StartedAt      *time.Time `json:"startedAt,omitempty"`
	CompletedAt    *time.Time `json:"completedAt,omitempty"`
}

// InsightEvent 一条分析过程中的进度消息。用于“分析信息”滚动展示：
// 由扫描里程碑（开始/各轮/完成/失败）与 agent 实时的工具动作（读取/搜索…）产生。
// level 取值与前端一一对应：info | success | warn | error。
type InsightEvent struct {
	ID      string    `json:"id"`
	Seq     int       `json:"seq"`
	Ts      time.Time `json:"ts"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
}

// insightsResponse `GET /api/projects/{id}/insights` 的荷载。
type insightsResponse struct {
	Scan            *InsightScan     `json:"scan"`
	Findings        []InsightFinding `json:"findings"`
	Events          []InsightEvent   `json:"events"`
	HasScan         bool             `json:"hasScan"`
	SuppressedCount int              `json:"suppressedCount"`
	OpenCount       int              `json:"openCount"` // 当前有效建议总数（== len(Findings)），供前端区分"本次新增"
	// Invalidated 是经验证已失效、从有效列表隐藏的建议（折叠展示，含 AI 判断依据）。
	Invalidated  []InsightFinding        `json:"invalidated,omitempty"`
	Verification *InsightVerificationRun `json:"verification,omitempty"`
}

// pendingInsight 是 Pass A 产出的候选发现（尚未核实/去重），也是扫描用的内部表示。
type pendingInsight struct {
	Type      string `json:"type"`
	Severity  string `json:"severity"`
	Title     string `json:"title"`
	Summary   string `json:"summary"`
	FileHint  string `json:"fileHint,omitempty"`
	Confirmed bool   `json:"confirmed"`
}

// rawInsightParse 兼容多种 agent 输出形态：裸数组、{findings:[...]} 包裹、
// Markdown 代码块（```json ... ```）、以及前后夹杂说明文字的 JSON。
func parseInsightCandidates(raw string) ([]pendingInsight, error) {
	candidates := extractInsightJSON(raw)
	if candidates == "" {
		return nil, errors.New("analysis returned empty output")
	}
	// 尝试裸数组。
	var asSlice []pendingInsight
	if err := json.Unmarshal([]byte(candidates), &asSlice); err == nil {
		return asSlice, nil
	}
	// 尝试对象包裹 { "findings": [...] }。
	var asObj struct {
		Findings []pendingInsight `json:"findings"`
	}
	if err := json.Unmarshal([]byte(candidates), &asObj); err == nil {
		return asObj.Findings, nil
	}
	return nil, errors.New("analysis output is not a valid JSON array")
}

// insightJSONFirstDelim 定位可解析 JSON 的起始游标：剥掉 Markdown 代码围栏后，
// 找到第一个 [ 或 {。
func insightJSONFirstDelim(s string) (byte, int) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '[' || c == '{' {
			return c, i
		}
	}
	return 0, -1
}

// extractInsightJSON 从可能混杂说明文字/多段 agent 输出的文本中抽取**最外层** JSON：
//   - 剥去 Markdown 代码围栏（```json ... ```）
//   - 取最后一个完整的顶层 [/{ … ]/}（Claude 会先叙述"我要分析…"再给最终 JSON，
//     因此最终答案通常在后部；逐个候选并校验 JSON 合法性，取最后一个合法者）。
func extractInsightJSON(raw string) string {
	s := stripInsightCodeFences(raw)
	s = strings.TrimSpace(s)
	const maxTry = 64
	lastValid := ""
	for try := 0; try < maxTry; try++ {
		open, idx := insightJSONFirstDelim(s)
		if idx < 0 {
			break
		}
		closeTok := byte(']')
		if open == '{' {
			closeTok = '}'
		}
		// 括号配对，忽略字符串内括号与转义。
		depth := 0
		inStr := false
		escaped := false
		end := -1
		for i := idx; i < len(s); i++ {
			c := s[i]
			if inStr {
				if escaped {
					escaped = false
				} else if c == '\\' {
					escaped = true
				} else if c == '"' {
					inStr = false
				}
				continue
			}
			switch c {
			case '"':
				inStr = true
			case open:
				depth++
			case closeTok:
				depth--
				if depth == 0 {
					end = i + 1
				}
			}
			if end >= 0 {
				break
			}
		}
		if end < 0 {
			break
		}
		candidate := s[idx:end]
		// 校验确实是合法 JSON；若是，记录为候选，继续看是否还有更靠后的 JSON。
		if json.Valid([]byte(candidate)) {
			lastValid = candidate
		}
		s = s[end:]
	}
	return lastValid
}

// stripInsightCodeFences 去掉 Markdown 的 ``` ... ``` 代码块围栏标记。
func stripInsightCodeFences(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	inFence := false
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") {
			inFence = !inFence
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// projectRuntimeProfile 兑现「随项目默认」取舍：把项目默认档案解析为运行时
// Profile 注入 `AgentRunRequest.Profile`（model/baseUrl/受管 Key 与项目 AI 配置
// 一致，而非 CLI 裸默认）。无默认档案时返回 nil（退化为 CLI 默认）。
func (s *Server) projectRuntimeProfile(ctx context.Context, project Project, agentID string) (*AgentRuntimeProfile, error) {
	if project.DefaultProfileID == "" {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // 只读解析，提交与否无关紧要
	profile, err := s.runtimeProfileTx(ctx, tx, project.DefaultProfileID, project.RunnerID, agentID)
	if err != nil {
		return nil, err
	}
	return profile, nil
}

// runReadOnlyAgent 封装「选 runner → 解析项目默认档案 → sink → 超时 → Run →
// 收集助手文本」，供 Pass A（发现）/ Pass B（核实）复用。严格只读：Claude 用
// `plan`、Codex 用 `read_only`，绕开 HTTP 层的 `validAgentPolicy`（见 docs/25 §5.2）。
// 选 runner 与 startMessage（app.go:3556）同一语义：SSH 走 runnerRegistry、本机按
// 目标环境；`agentClaudeRunnerFor/codexRunnerFor` 对 remote 返回 nil，故兜底 s.runner。
// progress 非空时把 agent 的实时工具动作（读取/搜索…）作为分析进度回调给调用方。
func (s *Server) runReadOnlyAgent(ctx context.Context, project Project, agentID, prompt string, progress func(level, message string)) (string, error) {
	var runner AgentRunner
	policy := "plan"
	isSSH := strings.HasPrefix(project.Runner, "ssh-")
	target := s.resolveAgentTargetEnv(project.Runner, project.Path)
	switch {
	case isSSH:
		r, ok := s.runnerRegistry.get(project.Runner)
		if !ok {
			return "", fmt.Errorf("SSH 连接不可用")
		}
		runner = r
		if agentID == "codex" {
			policy = "read_only"
		}
	default:
		if agentID == "codex" {
			runner = s.codexRunnerFor(target)
			policy = "read_only"
		} else {
			runner = s.agentClaudeRunnerFor(target)
		}
		if runner == nil {
			runner = s.runner // 兜底：remote/windowsAgentRunner 缺失时用服务端 runner
		}
		if runner == nil {
			return "", fmt.Errorf("没有可用的分析运行器")
		}
	}

	profile, err := s.projectRuntimeProfile(ctx, project, agentID)
	if err != nil {
		return "", fmt.Errorf("resolve project default profile: %w", err)
	}

	sink := &insightLiveSink{onProgress: progress}
	scanCtx, cancel := context.WithTimeout(ctx, insightScanPassTimeout)
	defer cancel()
	if err := runner.Run(scanCtx, AgentRunRequest{
		SessionID:      uuid.NewString(),
		ProjectPath:    project.Path,
		Prompt:         prompt,
		PermissionMode: policy,
		RunID:          uuid.NewString(),
		AgentID:        agentID,
		Profile:        profile,
		SkipSessionID:  true, // 一次性只读分析：避免 plan 模式带 --session-id 走"待命"
		// 只读执行（claude）：default + 仅放行只读工具，能执行但不改文件、不挂审批。
		// codex 走其自身 read_only sandbox，不设此项。
		ReadOnlyTools:  insightReadOnlyTools(agentID),
		PromptViaStdin: len(insightReadOnlyTools(agentID)) > 0, // 用了 --allowedTools 就需 stdin 传 prompt
	}, sink); err != nil {
		// 区分"跑满单趟上限被杀"（agent 进程被终止，cmd.Wait 的报错不含 deadline
		// 语义，这里显式看 scanCtx.Err()）与真正的运行失败，让用户拿到准确原因。
		if scanCtx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("分析超时：超过 %d 分钟未完成。项目可能较大或模型处理较慢，请稍后重试或更换更快的模型档案",
				int(insightScanPassTimeout.Minutes()))
		}
		return "", err
	}
	return strings.TrimSpace(sink.text.String()), nil
}

// insightLiveSink 在收集助手文本（orchestrationReviewSink）之外，额外把 agent 实时
// 的工具动作抽成进度消息。Event 回调运行在 runner 的输出读 goroutine 上，与扫描
// goroutine 并发追加进度事件；消息去重 + 节流，避免刷屏（3 秒内同一条/同一类只报一次）。
type insightLiveSink struct {
	orchestrationReviewSink
	onProgress   func(level, message string)
	lastActivity time.Time
	lastMessage  string
}

func (sink *insightLiveSink) Event(eventType string, payload json.RawMessage) {
	sink.orchestrationReviewSink.Event(eventType, payload)
	if sink.onProgress == nil {
		return
	}
	message, ok := parseInsightToolActivity(eventType, payload)
	if !ok || message == sink.lastMessage || time.Since(sink.lastActivity) < 3*time.Second {
		return
	}
	sink.lastMessage = message
	sink.lastActivity = time.Now()
	sink.onProgress("info", "正在"+message)
}

// parseInsightToolActivity 从 runner 事件里尽力提取一条“当前在做什么”的短信息。
// 目前可靠识别 claude 的 assistant 事件中的 tool_use（Read/Glob/Grep/List 等，正是
// 只读分析实际放行的工具）；codex/ssh 事件形态不同，解析不了返回 ("", false)，调用方忽略。
func parseInsightToolActivity(eventType string, payload json.RawMessage) (string, bool) {
	if eventType != "assistant" {
		return "", false
	}
	var envelope struct {
		Message struct {
			Content []struct {
				Type  string `json:"type"`
				Name  string `json:"name"`
				Input struct {
					FilePath string `json:"file_path"`
					Path     string `json:"path"`
					Pattern  string `json:"pattern"`
				} `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(payload, &envelope) != nil {
		return "", false
	}
	for _, block := range envelope.Message.Content {
		if block.Type != "tool_use" || block.Name == "" {
			continue
		}
		target := block.Input.FilePath
		if target == "" {
			target = block.Input.Path
		}
		switch block.Name {
		case "Read", "List":
			if target != "" {
				return "读取 " + target, true
			}
			return "读取文件", true
		case "Glob":
			if block.Input.Pattern != "" {
				return "搜索文件 " + block.Input.Pattern, true
			}
			return "搜索文件", true
		case "Grep":
			if block.Input.Pattern != "" {
				return "检索代码 " + block.Input.Pattern, true
			}
			return "检索代码", true
		default:
			return "", false
		}
	}
	return "", false
}

// appendInsightEvent 追加一条分析进度事件（seq 自增）。单连接 SQLite 串行化所有写入，
// 用子查询取 max(seq)+1 一步完成，跨 goroutine（扫描 goroutine + sink 读 goroutine）
// 并发追加也不会撞 seq。
func (s *Server) appendInsightEvent(ctx context.Context, scanID, level, message string) {
	if message == "" || scanID == "" {
		return
	}
	_, err := s.db.ExecContext(ctx, `insert into project_insight_events (id,scan_id,seq,ts,level,message)
		select ?, ?, coalesce(max(seq),0)+1, ?, ?, ? from project_insight_events where scan_id=?`,
		uuid.NewString(), scanID, time.Now().UTC(), level, message, scanID)
	if err != nil {
		log.Printf("[insights] append progress event scan=%s: %v", scanID, err)
	}
}

// loadInsightEvents 返回某次扫描的全部进度事件（按 seq 升序）。
func (s *Server) loadInsightEvents(ctx context.Context, scanID string) []InsightEvent {
	rows, err := s.db.QueryContext(ctx, `select id,seq,ts,level,message from project_insight_events where scan_id=? order by seq asc`, scanID)
	if err != nil {
		log.Printf("[insights] load progress events scan=%s: %v", scanID, err)
		return nil
	}
	defer rows.Close()
	var out []InsightEvent
	for rows.Next() {
		var e InsightEvent
		if rows.Scan(&e.ID, &e.Seq, &e.Ts, &e.Level, &e.Message) == nil {
			out = append(out, e)
		}
	}
	if out == nil {
		out = []InsightEvent{}
	}
	return out
}

// insightReadOnlyTools 返回只读分析的放行工具清单。仅 claude 需要（它依赖
// --allowedTools 限制写入）；codex 由 read_only sandbox 保证，返回 nil。
// 只放行当前版本实际存在的只读工具（Read/Glob/Grep）；List/ReadMultiToolInfo
// 不是本版本的工具名，白名单里写不存在的名字无意义。
func insightReadOnlyTools(agentID string) []string {
	if agentID != "claude-code" {
		return nil
	}
	return []string{"Read", "Glob", "Grep"}
}

// insightReadOnlyDenyTools 是只读分析对可写/可执行工具的"硬拒"清单。
// --allowedTools 仅表达白名单，实测在本版本不限制非白名单工具（模型仍可调用
// PowerShell 跑 git diff、写文件），因此配合 --settings permissions.deny 把这些
// 工具从模型工具集里真正移除，保证"只读分析"物理上只读、也不会因 git 式全量
// 探索把单次扫描拖到超时。与 insightReadOnlyTools 配套：白名单声明意图，
// deny 清单真正落地。工具名与 claude init 事件里的 tools 列表一一对应。
var insightReadOnlyDenyTools = []string{
	// Shell：跑命令（git diff、改文件…）。跨端命名都拒。
	"Bash", "PowerShell",
	// 文件写入。
	"Write", "Edit", "MultiEdit", "NotebookEdit",
	// 子代理/技能/工作流/消息：可间接执行任意操作。
	"Task", "Workflow", "Skill", "Agent", "SendMessage",
	// 定时任务。
	"CronCreate", "CronDelete", "CronList", "ScheduleWakeup",
	// 项目/设计同步、通知/上报。
	"DesignSync", "EnterWorktree", "ExitWorktree", "PushNotification", "ReportFindings",
	// 任务管理。
	"TaskCreate", "TaskGet", "TaskList", "TaskOutput", "TaskStop", "TaskUpdate",
	// 网络检索：分析聚焦项目代码，避免跑题拖慢。
	"WebFetch", "WebSearch",
}

// insightReadOnlySettingsJSON 组装只读执行的 --settings JSON（permissions.deny 硬拒
// 清单），供 claude 本地/WSL/SSH 只读路径共用。失败返回错误（理论上不会发生）。
func insightReadOnlySettingsJSON() (string, error) {
	settings, err := json.Marshal(map[string]any{
		"permissions": map[string]any{
			"deny": insightReadOnlyDenyTools,
		},
	})
	if err != nil {
		return "", fmt.Errorf("encode Claude read-only settings: %w", err)
	}
	return string(settings), nil
}

// insightTypeLabels 供 prompt 组装：类型 → 用户可读的中文标签。
var insightTypeLabels = map[string]string{
	insightBug:          "有 bug",
	insightStyle:        "样式问题",
	insightOptimization: "可优化项",
	insightFeature:      "可新增功能",
}

var insightTypeOrder = []string{insightBug, insightStyle, insightOptimization, insightFeature}

// buildInsightScanPrompt 组装 Pass A 的发现 prompt（docs/25 §5.2）。
// opts 携带本次方向：theme（”=全面）+ types（空=全查）。
func buildInsightScanPrompt(projectPath, repoSHA string, alreadySurfaced []string, opts scanOpts) string {
	// 类型段：按 opts.Types 过滤；空 = 覆盖全部四类（原全量语义）。
	var types []string
	if len(opts.Types) == 0 {
		for _, t := range insightTypeOrder {
			types = append(types, fmt.Sprintf("%s —— %s", t, insightTypeLabels[t]))
		}
	} else {
		for _, t := range opts.Types {
			types = append(types, fmt.Sprintf("%s —— %s", t, insightTypeLabels[t]))
		}
	}
	// 主题段：非空时强调视角。
	themeLine := "不限定主题，全面审视。"
	if opts.Theme != "" {
		themeLine = fmt.Sprintf("聚焦主题：%s。请从「%s」的视角审视项目，优先发现这类相关问题。",
			insightThemeLabels[opts.Theme], insightThemeLabels[opts.Theme])
	}
	prev := "（本次为首次分析，无历史记录）"
	if len(alreadySurfaced) > 0 {
		prev = "\n- " + strings.Join(alreadySurfaced, "\n- ")
	}
	return fmt.Sprintf(`立即执行一个只读分析任务，不要询问任何细节，不要让我确认目标，直接开始。

分析对象已完整指定为：项目根目录 %s
（当前 git 提交：%s）

%s

任务：通读整个项目，主动找出值得用户知道的发现，覆盖以下类别：
- %s

硬性输出要求：你的整个回复只能包含一个 JSON 数组，不允许有任何前言、注释、Markdown 围栏、或数组之后的任何文字。数组里每条为：
{"type":"bug|style|optimization|feature","title":"一句话标题","summary":"面向用户的两三句说明","severity":"low|normal|high","fileHint":"（可选）最相关文件路径，否则留空"}

约束：
1. title 和 summary 用用户能看懂的表述（界面能看到/用到），禁止代码标识符、行号、堆栈。
2. 【不重复】以下为本项目前几批已报告过的建议。凡已在清单里、或经核对项目当前已实现/具备的，一律不得再报（宁缺毋滥，最多 30 条）：
%s
3. 每条都先到项目里核实确有其事再报；拿不准的不要报。
4. 你看不到运行中的浏览器，样式问题依据代码结构与组件树推断即可。`,
		projectPath, repoSHA, themeLine, strings.Join(types, "\n- "), prev)
}

// buildInsightRepairPrompt 组装 Pass A 解析失败后的修正重试 prompt。与首轮同方向
// （theme + types），但对输出契约做最强约束：不得叙述、不得展示过程、不得贴示例、
// 不得中途改口，整段回复就是且仅是那个 JSON 数组。首轮失败的真因是模型把 JSON 数组
// 留到最后却在输出前 end_turn，因此这里明确"宁可少报也必须在回复里给出数组"。
func buildInsightRepairPrompt(projectPath, repoSHA string, opts scanOpts) string {
	var types []string
	if len(opts.Types) == 0 {
		for _, t := range insightTypeOrder {
			types = append(types, fmt.Sprintf("%s —— %s", t, insightTypeLabels[t]))
		}
	} else {
		for _, t := range opts.Types {
			types = append(types, fmt.Sprintf("%s —— %s", t, insightTypeLabels[t]))
		}
	}
	themeLine := "不限定主题，全面审视。"
	if opts.Theme != "" {
		themeLine = fmt.Sprintf("聚焦主题：%s。请从「%s」的视角审视项目，优先发现这类相关问题。",
			insightThemeLabels[opts.Theme], insightThemeLabels[opts.Theme])
	}
	return fmt.Sprintf(`针对同一项目补交上一轮缺失的结果，直接给出，别再说思考过程、别再重新通读全目录。

上一轮你只做了大量分析却未在回复里给出结果，这不可接受。你现在已经掌握项目结构，只需快速核对后用 JSON 数组把发现交出来，尽量少做重复探索。

对象：%s（git %s）。%s
覆盖类别：%s

这一次的硬性要求（必须严格遵守）：
1. 你的整段回复必须是且仅是一个 JSON 数组，前面、后面、中间都不允许有任何说明文字、思考、标题、示例或 Markdown 围栏。
2. 数组元素格式：{"type":"bug|style|optimization|feature","title":"一句话标题","summary":"面向用户的两三句说明","severity":"low|normal|high","fileHint":"（可选）最相关文件路径，否则留空"}
3. 宁缺毋滥：只报你直接核实过的发现；拿不稳的不报，最少可以只报 1 条，但必须给出数组。
4. title 用用户能看懂的表述；禁止代码标识符、行号、堆栈。

现在只输出那个 JSON 数组：`, projectPath, repoSHA, themeLine, strings.Join(types, "\n- "))
}

func buildInsightRepairPromptWithHistory(projectPath, repoSHA string, alreadySurfaced []string, opts scanOpts) string {
	prompt := buildInsightRepairPrompt(projectPath, repoSHA, opts)
	if len(alreadySurfaced) == 0 {
		return prompt
	}
	return prompt + "\n\nDo not report these previously surfaced suggestions again:\n- " + strings.Join(alreadySurfaced, "\n- ")
}

// buildVerifyPrompt 组装 Pass B 的核实 prompt：逐条到项目里查证候选是否真实存在。
// 同样以"立即执行"的强指令开头，避免 agent 反问澄清。
func buildVerifyPrompt(projectPath string, candidates []pendingInsight) string {
	items := insightVerifyCandidateDetails(candidates)
	return fmt.Sprintf(`执行一个只读核验任务，立即开始，不要询问、不要确认、不要前言，直接输出唯一的结果。

核验对象目录：%s

对下面每条候选，到目录里读取相关文件/结构，判断它是否真实存在（是否伪需求、假 bug、已实现/已具备）：

候选清单：
%s

	硬性输出要求：你的整个回复只能包含一个 JSON 对象，不允许有任何前言、注释、Markdown 围栏、或对象之后的任何文字。结构：
{"findings":[{"index":1,"confirmed":true,"reason":""},{"index":2,"confirmed":false,"reason":"项目当前已具备"}]}
confirmed=true 表示确认真实存在；false 表示伪需求/假 bug/已具备。每条对应候选的 index；无法证实的判 false。`, projectPath, items)
}

// buildVerifyRepairPrompt 组装 Pass B 核实输出非法后的修正重试 prompt。
// 仅复述候选与硬性输出契约，要求"整个回复只能是那个 JSON 对象"。
func buildVerifyRepairPrompt(projectPath string, candidates []pendingInsight) string {
	return fmt.Sprintf(`重新执行刚才的只读核验，直接给出结果，别再说思考过程。

上一轮你只做了查证却未在回复里给出唯一结果，这不可接受。

对象目录：%s
候选清单：
%s

这一次的硬性要求（必须严格遵守）：
你的整段回复必须且仅是一个 JSON 对象，前面、后面、中间都不允许任何说明、思考、标题、示例或 Markdown 围栏。结构必须是：
{"findings":[{"index":1,"confirmed":true,"reason":""},{"index":2,"confirmed":false,"reason":"项目当前已具备"}]}
每条 findings 的 index 对应候选编号；confirmed=true 表示确认真实存在、false 表示伪需求/假 bug/已具备；无法证实的判 false。每条候选都必须有一条。

现在只输出那个 JSON 对象：`, projectPath, insightVerifyCandidateDetails(candidates))
}

// insightVerifyCandidateDetails 在初次核实与修正重试之间复用完整候选信息。
// 修正重试始终是新 agent 会话，只有编号会让模型失去判定对象。
func insightVerifyCandidateDetails(candidates []pendingInsight) string {
	items := make([]string, 0, len(candidates))
	for i, c := range candidates {
		items = append(items, fmt.Sprintf("%d. {\"title\":%q,\"summary\":%q,\"type\":%q,\"severity\":%q,\"fileHint\":%q}",
			i+1, c.Title, c.Summary, c.Type, c.Severity, c.FileHint))
	}
	return strings.Join(items, "\n")
}

// insightVerifyVerdict 是 Pass B 核实结果的解码目标。
type insightVerifyVerdict struct {
	Findings []struct {
		Index     int  `json:"index"`
		Confirmed bool `json:"confirmed"`
	} `json:"findings"`
}

// runInsightVerify 跑一趟 Pass B 核实并解码结果。与 Pass A 同样地，模型可能在输出
// JSON 前 end_turn 或给散文/错误的形状——先抽最外层 JSON，非法则用强约束修正 prompt
// 重试一次，仍失败才返回错误。
//
// 返回的错误分两类，供调用方给出准确提示：
//   - 包装 errInsightVerifyRunner 的：agent 进程本身跑失败（启动/超时/退出），
//     属环境问题，重试无用；
//   - 其余：核实结果解析失败（模型没给合法 JSON 对象）。这二者在 UI 提示文案上应区别。
var errInsightVerifyRunner = errors.New("核实代理运行失败")

func (s *Server) runInsightVerify(ctx context.Context, project Project, agentID string, candidates []pendingInsight, progress func(level, message string)) ([]struct {
	Index     int  `json:"index"`
	Confirmed bool `json:"confirmed"`
}, error) {
	cc := func(prompt string) (string, error) {
		return s.runReadOnlyAgent(ctx, project, agentID, prompt, progress)
	}
	// 首轮。
	textB, err := cc(buildVerifyPrompt(project.Path, candidates))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errInsightVerifyRunner, err)
	}
	verdict, vErr := decodeInsightVerdict(textB)
	if vErr == nil {
		vErr = validateInsightVerdict(verdict, len(candidates))
	}
	if vErr == nil {
		return verdict.Findings, nil
	}
	log.Printf("[insights] project=%s Pass B parse failed on first attempt: %v", project.ID, vErr)
	// 修正 prompt 重试一次。
	textB, err = cc(buildVerifyRepairPrompt(project.Path, candidates))
	if err != nil {
		return nil, fmt.Errorf("%w（重试）: %v", errInsightVerifyRunner, err)
	}
	verdict, vErr = decodeInsightVerdict(textB)
	if vErr == nil {
		vErr = validateInsightVerdict(verdict, len(candidates))
	}
	if vErr != nil {
		return nil, fmt.Errorf("核实代理未返回有效结果: %v (raw len=%d)", vErr, len(textB))
	}
	return verdict.Findings, nil
}

// decodeInsightVerdict 从 agent 输出抽最外层 JSON 并解码为核实对象。
func decodeInsightVerdict(text string) (insightVerifyVerdict, error) {
	vJSON := extractInsightJSON(text)
	if vJSON == "" {
		return insightVerifyVerdict{}, errors.New("empty/non-json")
	}
	var verdict insightVerifyVerdict
	if err := json.Unmarshal([]byte(vJSON), &verdict); err != nil {
		return insightVerifyVerdict{}, err
	}
	return verdict, nil
}

// runInsightVerifyBatches keeps the initial confirmation pass within the same
// prompt budget as re-verification. Each verdict has local indexes, so convert
// them to the original candidate index before returning.
func (s *Server) runInsightVerifyBatches(ctx context.Context, project Project, agentID string, candidates []pendingInsight, progress func(level, message string)) (map[int]bool, error) {
	verified := make(map[int]bool, len(candidates))
	for start := 0; start < len(candidates); start += insightVerifyBatchSize {
		end := min(start+insightVerifyBatchSize, len(candidates))
		if progress != nil && len(candidates) > insightVerifyBatchSize {
			progress("info", fmt.Sprintf("第 2 轮：核实候选 %d-%d/%d…", start+1, end, len(candidates)))
		}
		verdict, err := s.runInsightVerify(ctx, project, agentID, candidates[start:end], progress)
		if err != nil {
			return nil, err
		}
		for _, item := range verdict {
			verified[start+item.Index] = item.Confirmed
		}
	}
	return verified, nil
}

func validateInsightVerdict(verdict insightVerifyVerdict, candidateCount int) error {
	if len(verdict.Findings) != candidateCount {
		return fmt.Errorf("incomplete verdict: got %d findings, want %d", len(verdict.Findings), candidateCount)
	}
	seen := make(map[int]struct{}, candidateCount)
	for _, finding := range verdict.Findings {
		if finding.Index < 1 || finding.Index > candidateCount {
			return fmt.Errorf("invalid candidate index: %d", finding.Index)
		}
		if _, ok := seen[finding.Index]; ok {
			return fmt.Errorf("duplicate candidate index: %d", finding.Index)
		}
		seen[finding.Index] = struct{}{}
	}
	return nil
}

// ─── 建议再验证（re-verify）───────────────────────────────────────────────
//
// 扫描时的 Pass B 只保证「报告当下」候选真实存在；此后项目可能被其它任务迭代
// 修改（bug 已修复、功能已实现、优化已落地），或原分析因上下文限制判断不准。
// 再验证让用户对**既有建议**手动发起新一轮只读核验，确认其描述在当前代码里是否
// 仍然成立，结果写回 project_insights 行。一次可核验一批建议（POST /insights/verify
// 带 findingIds，缺省 = 全部有效建议），前端按 verificationResult 轮询展示。

// buildVerifyExistingPrompt 组装再验证 prompt：把每条既有建议（含 id）交给只读
// agent，逐条读取相关代码判断是否仍然成立。输出契约与 Pass B 一致：整段回复
// 必须且仅是一个 JSON 对象。
func buildVerifyExistingPrompt(projectPath, repoSHA string, targets []InsightFinding) string {
	return buildInsightReverifyPrompt(projectPath, repoSHA, targets, false)
}

// buildVerifyExistingRepairPrompt 是再验证输出非法后的修正重试 prompt。
// 注意：重试运行在一个全新的 agent 会话里（runReadOnlyAgent 每次新建 SessionID），
// 因此必须把候选**完整详情**再贴一遍，否则 agent 不知道每条 id 指向什么。
func buildVerifyExistingRepairPrompt(projectPath string, targets []InsightFinding) string {
	return buildInsightReverifyPrompt(projectPath, "", targets, true)
}

// insightVerifyExistingVerdict 是再验证 agent 输出的解码目标。
// Exists 用 *bool：若 agent 违约输出缺少 exists 字段的条目，缺省应判"无法确定"
// （标 failed 可重试）而非误判 invalid 把仍有效的建议隐藏。
// buildInsightReverifyPrompt 使用三态结论。只有能指出直接依据的 invalid 才会隐藏建议；
// 无法确认必须返回 uncertain，由服务端保留建议并标记可重试。
func buildInsightReverifyPrompt(projectPath, repoSHA string, targets []InsightFinding, repair bool) string {
	items := make([]string, 0, len(targets))
	for i, f := range targets {
		items = append(items, fmt.Sprintf("%d. {\"id\":%q,\"title\":%q,\"type\":%q,\"severity\":%q,\"summary\":%q,\"fileHint\":%q}",
			i+1, f.ID, f.Title, f.Type, f.Severity, f.Summary, f.FileHint))
	}
	repairLine := ""
	if repair {
		repairLine = "The previous response was not valid JSON. Repeat the verification and return only the required JSON object."
	}
	return fmt.Sprintf(`Run a read-only verification now. Do not ask questions and do not change files.
Project directory: %s
Git revision: %s
%s

For every candidate, inspect the relevant code and return exactly one JSON object:
{"findings":[{"id":"candidate-id","status":"valid","reason":"direct evidence"},{"id":"candidate-id","status":"invalid","reason":"direct evidence that it is fixed or implemented"},{"id":"candidate-id","status":"uncertain","reason":"evidence is insufficient"}]}

status meanings:
- valid: the issue or missing feature still exists.
- invalid: it is fixed, implemented, or inapplicable. Use this only with direct evidence from the project.
- uncertain: relevant evidence is insufficient. Never use invalid merely because you cannot confirm it.

Return one entry for every candidate and no text outside the JSON object.
Candidates:
%s`, projectPath, repoSHA, repairLine, strings.Join(items, "\n"))
}

type insightVerifyExistingVerdict struct {
	Findings []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		// Exists 兼容旧版 agent 输出；新协议使用 status 三态。
		Exists *bool  `json:"exists"`
		Reason string `json:"reason"`
	} `json:"findings"`
}

// decodeInsightVerifyExisting 从 agent 输出抽最外层 JSON 并解码为再验证对象。
func decodeInsightVerifyExisting(text string) (insightVerifyExistingVerdict, error) {
	vJSON := extractInsightJSON(text)
	if vJSON == "" {
		return insightVerifyExistingVerdict{}, errors.New("empty/non-json")
	}
	var verdict insightVerifyExistingVerdict
	if err := json.Unmarshal([]byte(vJSON), &verdict); err != nil {
		return insightVerifyExistingVerdict{}, err
	}
	return verdict, nil
}

// setInsightVerification 写一条建议的验证状态/说明/时间。note 超长截断；写入失败
// 仅记日志（验证是后台尽力而为，失败会在下次轮询/重试中体现，不回滚整批）。
func (s *Server) setInsightVerification(ctx context.Context, projectID, findingID, result, note string, now time.Time) {
	note = truncateInsightLog(strings.TrimSpace(note), 300)
	if _, err := s.db.ExecContext(ctx, `update project_insights set verification_result=?,verification_note=?,verified_at=? where id=? and project_id=?`,
		result, note, now, findingID, projectID); err != nil {
		log.Printf("[insights] project=%s set verification %s on %s: %v", projectID, result, findingID, err)
	}
}

// setInsightVerificationIfUnchanged avoids overwriting a user edit that happened
// after verification started. Verification writes intentionally do not touch
// updated_at, which remains the content revision owned by PATCH.
func (s *Server) setInsightVerificationIfUnchanged(ctx context.Context, projectID string, finding InsightFinding, result, note string, now time.Time) bool {
	note = truncateInsightLog(strings.TrimSpace(note), 300)
	res, err := s.db.ExecContext(ctx, `update project_insights set verification_result=?,verification_note=?,verified_at=? where id=? and project_id=? and updated_at=?`,
		result, note, now, finding.ID, projectID, finding.UpdatedAt)
	if err != nil {
		log.Printf("[insights] project=%s set conditional verification %s on %s: %v", projectID, result, finding.ID, err)
		return false
	}
	changed, err := res.RowsAffected()
	return err == nil && changed == 1
}

// setInsightVerificationPendingIfUnchanged starts a run only when the finding
// still has the content revision selected for it.
func setInsightVerificationPendingIfUnchanged(ctx context.Context, tx *sql.Tx, projectID string, finding InsightFinding, now time.Time) (bool, error) {
	res, err := tx.ExecContext(ctx, `update project_insights set verification_result=?,verification_note='',verified_at=? where id=? and project_id=? and updated_at=?`,
		insightVerifyPending, now, finding.ID, projectID, finding.UpdatedAt)
	if err != nil {
		return false, err
	}
	changed, err := res.RowsAffected()
	return err == nil && changed == 1, err
}

// resolveVerifyTargets 解析待验证建议：指定 ids → 逐条校验归属（防越权，任一非法
// 即报错）；空 ids → 全部"有效"（未失效）建议。返回空列表表示无建议可验证。
type insightReverifyResult struct {
	status string
	reason string
}

func normalizeInsightReverifyStatus(status string, legacyExists *bool) (string, bool) {
	switch status {
	case "valid", "invalid", "uncertain":
		return status, true
	case "":
		if legacyExists == nil {
			return "", false
		}
		if *legacyExists {
			return "valid", true
		}
		return "invalid", true
	default:
		return "", false
	}
}

// runInsightFindingsVerify collects all batches first, then publishes their results only
// if the repository still matches the version observed before verification. This prevents
// a late batch or a concurrent edit from making earlier, stale conclusions authoritative.
func (s *Server) runInsightFindingsVerify(ctx context.Context, projectID string, targets []InsightFinding) {
	s.runInsightFindingsVerifyRun(ctx, projectID, "", targets)
}

func (s *Server) runInsightFindingsVerifyRun(ctx context.Context, projectID, verificationID string, targets []InsightFinding) {
	if len(targets) == 0 {
		return
	}
	verifyCtx, cancel := context.WithTimeout(ctx, insightVerifyRunTimeout)
	defer cancel()
	// failAll 把复核整体置为失败（或取消）并复原目标建议。ctx 被取消（用户停止 /
	// 项目删除）时按"已取消"记录复核运行，建议写回 failed（可重试）——复核结果枚举
	// 没有 cancelled 值，且取消不等于 AI 判定，复原为可重试的失败最接近语义。
	failAll := func(note string) {
		status := insightScanFailed
		if errors.Is(verifyCtx.Err(), context.Canceled) {
			status = insightScanCancelled
			note = "验证已取消"
		}
		now := time.Now().UTC()
		persistInsightWrite(func(persistCtx context.Context) {
			for _, f := range targets {
				// 已失效建议是 AI 上次确认的终局判定：复验失败不应把它"复活"进有效列表，
				// 保持失效（它可能在请求时已被置为 pending，这里写回 invalid 复原）。
				if f.VerificationResult == insightVerifyInvalid {
					s.setInsightVerificationIfUnchanged(persistCtx, projectID, f, insightVerifyInvalid, note, now)
					continue
				}
				s.setInsightVerificationIfUnchanged(persistCtx, projectID, f, insightVerifyFailed, note, now)
			}
			s.finishInsightVerificationRun(persistCtx, verificationID, status, note, len(targets))
		})
	}
	project, err := s.getProjectByID(verifyCtx, projectID)
	if err != nil {
		failAll("无法加载项目，请重试")
		return
	}
	revision, err := s.insightWorkspaceRevision(verifyCtx, project)
	if err != nil {
		failAll("读取项目版本状态失败，请重试")
		return
	}
	// 非 Git 项目（Available=false）仍可复核：insightWorkspaceUnchanged 对非 Git 恒返回
	// unchanged，复核正常推进，只是没有版本一致性对账（repoSHA 为空，run 记录不写 revision）。
	if revision.Available {
		s.setInsightVerificationRunRevision(verifyCtx, verificationID, revision.RepoSHA)
	}
	agentID := s.currentInsightAgent(verifyCtx, projectID)
	results := make(map[string]insightReverifyResult, len(targets))

	for start := 0; start < len(targets); start += insightVerifyBatchSize {
		end := min(start+insightVerifyBatchSize, len(targets))
		batch := targets[start:end]
		unchanged, revisionErr := s.insightWorkspaceUnchanged(verifyCtx, project, revision)
		if revisionErr != nil || !unchanged {
			failAll("项目代码在验证中发生变化，已丢弃本轮结果，请重新验证")
			return
		}
		batchResults, batchErr := s.runInsightReverifyBatch(verifyCtx, project, agentID, revision.RepoSHA, batch)
		if batchErr != nil {
			log.Printf("[insights] project=%s re-verify batch failed: %v", projectID, batchErr)
			failAll(insightRunErrorMessage("建议验证失败", batchErr))
			return
		}
		for id, result := range batchResults {
			results[id] = result
		}
		s.updateInsightVerificationRun(verifyCtx, verificationID, fmt.Sprintf("正在核实 %d/%d 条建议", end, len(targets)), end)
	}

	unchanged, revisionErr := s.insightWorkspaceUnchanged(verifyCtx, project, revision)
	if revisionErr != nil || !unchanged {
		failAll("项目代码在验证中发生变化，已丢弃本轮结果，请重新验证")
		return
	}
	now := time.Now().UTC()
	persistInsightWrite(func(persistCtx context.Context) {
		for _, f := range targets {
			result := results[f.ID]
			// 已失效建议复验：仅 valid 判定才把它恢复进有效列表；invalid/uncertain 保持
			// 失效，避免一次失败的复验推翻此前 AI 已确认的失效结论。
			if f.VerificationResult == insightVerifyInvalid && result.status != "valid" {
				reason := result.reason
				if result.status == "uncertain" {
					reason = "AI 无法确认：" + reason
				}
				s.setInsightVerificationIfUnchanged(persistCtx, projectID, f, insightVerifyInvalid, reason, now)
				continue
			}
			dbStatus := insightVerifyFailed
			if result.status == "valid" {
				dbStatus = insightVerifyValid
			} else if result.status == "invalid" {
				dbStatus = insightVerifyInvalid
			}
			if result.status == "uncertain" {
				result.reason = "AI 无法确认：" + result.reason
			}
			s.setInsightVerificationIfUnchanged(persistCtx, projectID, f, dbStatus, result.reason, now)
		}
		s.finishInsightVerificationRun(persistCtx, verificationID, insightScanCompleted, "验证完成", len(targets))
	})
}

// persistInsightWrite 让扫描/复核 worker 被取消后仍能写入终态。
// Server.Close 会在关闭数据库前等待 insight worker，因此该有界后台上下文可安全使用。
func persistInsightWrite(write func(context.Context)) {
	persistCtx, cancel := context.WithTimeout(context.Background(), insightPersistenceTimeout)
	defer cancel()
	write(persistCtx)
}

func (s *Server) runInsightReverifyBatch(ctx context.Context, project Project, agentID, repoSHA string, targets []InsightFinding) (map[string]insightReverifyResult, error) {
	cc := func(prompt string) (string, error) {
		return s.runReadOnlyAgent(ctx, project, agentID, prompt, nil)
	}
	text, err := cc(buildInsightReverifyPrompt(project.Path, repoSHA, targets, false))
	if err != nil {
		return nil, err
	}
	verdict, err := decodeInsightVerifyExisting(text)
	if err != nil {
		text, err = cc(buildInsightReverifyPrompt(project.Path, repoSHA, targets, true))
		if err != nil {
			return nil, err
		}
		verdict, err = decodeInsightVerifyExisting(text)
		if err != nil {
			return nil, fmt.Errorf("verification agent returned invalid JSON: %w", err)
		}
	}
	targetIDs := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		targetIDs[target.ID] = struct{}{}
	}
	results := make(map[string]insightReverifyResult, len(targets))
	for _, item := range verdict.Findings {
		if _, ok := targetIDs[item.ID]; !ok {
			return nil, errors.New("AI 返回了未知建议")
		}
		if _, duplicate := results[item.ID]; duplicate {
			return nil, errors.New("AI 重复返回了同一条建议")
		}
		status, ok := normalizeInsightReverifyStatus(item.Status, item.Exists)
		if !ok {
			return nil, errors.New("AI 未给出有效的验证判定")
		}
		results[item.ID] = insightReverifyResult{status: status, reason: item.Reason}
	}
	if len(results) != len(targets) {
		return nil, errors.New("AI 未给出全部建议的验证判定")
	}
	return results, nil
}

func (s *Server) updateInsightVerificationRun(ctx context.Context, verificationID, message string, processed int) {
	if verificationID == "" {
		return
	}
	if _, err := s.db.ExecContext(ctx, `update project_insight_verification_runs set message=?,processed_count=? where id=? and status='running'`, message, processed, verificationID); err != nil {
		log.Printf("[insights] update verification run %s: %v", verificationID, err)
	}
}

func (s *Server) setInsightVerificationRunRevision(ctx context.Context, verificationID, repoSHA string) {
	if verificationID == "" || repoSHA == "" {
		return
	}
	if _, err := s.db.ExecContext(ctx, `update project_insight_verification_runs set repo_sha=? where id=? and status='running'`, repoSHA, verificationID); err != nil {
		log.Printf("[insights] set verification run revision %s: %v", verificationID, err)
	}
}

func (s *Server) finishInsightVerificationRun(ctx context.Context, verificationID, status, message string, processed int) {
	if verificationID == "" {
		return
	}
	if _, err := s.db.ExecContext(ctx, `update project_insight_verification_runs set status=?,error=?,message=?,processed_count=?,completed_at=? where id=?`,
		status, func() string {
			if status == insightScanFailed {
				return message
			}
			return ""
		}(), message, processed, time.Now().UTC(), verificationID); err != nil {
		log.Printf("[insights] finish verification run %s: %v", verificationID, err)
	}
}

func (s *Server) resolveVerifyTargets(ctx context.Context, projectID string, ids []string) ([]InsightFinding, error) {
	if len(ids) == 0 {
		rows, err := s.db.QueryContext(ctx, `select `+insightFindingColumns+` from project_insights
			where project_id=? and coalesce(verification_result,'')<>'invalid' order by created_at asc`, projectID)
		if err != nil {
			return nil, errors.New("读取待验证建议失败，请重试")
		}
		defer rows.Close()
		var out []InsightFinding
		for rows.Next() {
			if f, err := scanInsightFinding(rows.Scan); err == nil {
				out = append(out, f)
			}
		}
		if err := rows.Err(); err != nil {
			return nil, errors.New("读取待验证建议失败，请重试")
		}
		return out, nil
	}
	// 显式 ids：逐条校验归属（防越权，任一非法即报错），并去重避免同一条被
	// 重复写/重复出现在 agent prompt 里。
	var out []InsightFinding
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		f, ok := s.loadInsightFinding(ctx, projectID, id)
		if !ok {
			return nil, errors.New("建议不存在或已被删除，请刷新后重试")
		}
		out = append(out, f)
	}
	return out, nil
}

// verifyInsightFindings POST /api/projects/{projectID}/insights/verify
// 对既有建议发起新一轮 AI 核验。可选 body {"findingIds":[...]}，缺省 = 全部有效建议。
// 异步执行：先把目标置 pending 并返回 202，后台跑只读 agent 后逐条写回
// valid/invalid/failed。单项目互斥：扫描/验证进行中返回 409。
func (s *Server) verifyInsightFindings(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("projectID")
	if !s.projectExists(r.Context(), projectID) {
		http.NotFound(w, r)
		return
	}
	var req struct {
		FindingIDs []string `json:"findingIds"`
	}
	if !decodeOptional(w, r, &req) {
		return
	}
	targets, err := s.resolveVerifyTargets(r.Context(), projectID, req.FindingIDs)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if len(targets) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("没有可验证的建议"))
		return
	}
	now := time.Now().UTC()
	verificationID := uuid.NewString()

	s.insightMu.Lock()
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	if closing {
		s.insightMu.Unlock()
		writeError(w, http.StatusServiceUnavailable, errors.New("服务正在关闭，请稍后再试"))
		return
	}
	if s.insightActive[projectID] {
		s.insightMu.Unlock()
		writeError(w, http.StatusConflict, errors.New("项目正在分析/验证中，请稍候再试"))
		return
	}
	// 先创建取消句柄并随 insightActive 一起登记：让 cancel 端点也能命中"启动中"瞬间。
	// runCancel 由 goroutine 的 defer 调用；早退路径（工作区/事务失败）显式调用，幂等。
	runCtx, runCancel := context.WithCancel(s.runtimeCtx)
	s.insightActive[projectID] = true
	s.insightCancels[projectID] = runCancel
	s.insightWG.Add(1)
	s.insightMu.Unlock()

	// 整个 worker 生命周期持有租约。应用内 Git、文件和 agent 写入使用同一租约，
	// 因而无法在最终版本检查后、复核结果提交前修改工作区。
	releaseWorkspace, acquired := s.acquireProjectWorkspace(projectID, "insight-verify:"+verificationID)
	if !acquired {
		runCancel()
		s.insightMu.Lock()
		delete(s.insightActive, projectID)
		delete(s.insightCancels, projectID)
		s.insightMu.Unlock()
		s.insightWG.Done()
		writeError(w, http.StatusConflict, errors.New("项目工作区正在被其他任务修改，请稍候再试"))
		return
	}
	workerStarted := false
	defer func() {
		if !workerStarted {
			releaseWorkspace()
		}
	}()
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		runCancel()
		s.insightMu.Lock()
		delete(s.insightActive, projectID)
		delete(s.insightCancels, projectID)
		s.insightMu.Unlock()
		s.insightWG.Done()
		writeError(w, http.StatusInternalServerError, errors.New("启动建议验证失败，请重试"))
		return
	}
	defer tx.Rollback() //nolint:errcheck

	// Start only findings that are still at the content revision selected above.
	// This transaction keeps pending rows and the observable run record in sync.
	pendingTargets := make([]InsightFinding, 0, len(targets))
	ids := make([]string, 0, len(targets))
	for _, f := range targets {
		pending, err := setInsightVerificationPendingIfUnchanged(r.Context(), tx, projectID, f, now)
		if err != nil {
			runCancel()
			s.insightMu.Lock()
			delete(s.insightActive, projectID)
			delete(s.insightCancels, projectID)
			s.insightMu.Unlock()
			s.insightWG.Done()
			writeError(w, http.StatusInternalServerError, errors.New("启动建议验证失败，请重试"))
			return
		}
		if pending {
			pendingTargets = append(pendingTargets, f)
			ids = append(ids, f.ID)
		}
	}
	if len(pendingTargets) == 0 {
		runCancel()
		s.insightMu.Lock()
		delete(s.insightActive, projectID)
		delete(s.insightCancels, projectID)
		s.insightMu.Unlock()
		s.insightWG.Done()
		writeError(w, http.StatusConflict, errors.New("建议已被更新，请刷新后重新验证"))
		return
	}
	if _, err := tx.ExecContext(r.Context(), `insert into project_insight_verification_runs
		(id,project_id,status,message,total_count,processed_count,created_at,started_at)
		values (?,?,'running',?, ?,0,?,?)`,
		verificationID, projectID, "正在准备验证", len(pendingTargets), now, now); err != nil {
		runCancel()
		s.insightMu.Lock()
		delete(s.insightActive, projectID)
		delete(s.insightCancels, projectID)
		s.insightMu.Unlock()
		s.insightWG.Done()
		writeError(w, http.StatusInternalServerError, errors.New("启动建议验证失败，请重试"))
		return
	}
	if err := tx.Commit(); err != nil {
		runCancel()
		s.insightMu.Lock()
		delete(s.insightActive, projectID)
		delete(s.insightCancels, projectID)
		s.insightMu.Unlock()
		s.insightWG.Done()
		writeError(w, http.StatusInternalServerError, errors.New("启动建议验证失败，请重试"))
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"findingIds": ids, "verificationId": verificationID})

	// 后台执行。Done 在 goroutine 内调用，使 insightWG 精确跟踪验证生命周期，
	// 供 Close() 等待（与 triggerInsightScan 同语义）。runCtx 已在登记 active 时创建：
	// 项目被删除/用户停止时取消它，避免 agent 继续跑完无用功。
	workerStarted = true
	go func() {
		defer s.insightWG.Done()
		defer releaseWorkspace()
		defer runCancel()
		defer func() {
			s.insightMu.Lock()
			delete(s.insightActive, projectID)
			delete(s.insightCancels, projectID)
			s.insightMu.Unlock()
		}()
		s.runInsightFindingsVerifyRun(runCtx, projectID, verificationID, pendingTargets)
	}()
}

type fingerprintedFact struct {
	InsightFinding
	id          string
	fingerprint string
}

var insightFpStripRE = regexp.MustCompile(`[\s\p{P}]+`)

// insightFingerprint 把 title+summary 归一化为去重指纹（规则 1）：去空白/标点、转小写。
func insightFingerprint(title, summary string) string {
	return strings.ToLower(insightFpStripRE.ReplaceAllString(title+summary, ""))
}

// validateAndNormalize 钳制单个候选：合法 type/severity、非空 title、附指纹。
func validateAndNormalize(c pendingInsight) (fingerprintedFact, bool) {
	validType := false
	for _, t := range insightTypeOrder {
		if c.Type == t {
			validType = true
			break
		}
	}
	if !validType {
		return fingerprintedFact{}, false
	}
	if strings.TrimSpace(c.Title) == "" {
		return fingerprintedFact{}, false
	}
	sev := c.Severity
	if sev != insightSeverityLow && sev != insightSeverityNormal && sev != insightSeverityHigh {
		sev = insightSeverityNormal
	}
	return fingerprintedFact{
		InsightFinding: InsightFinding{
			ProjectID: "", ScanID: "", Type: c.Type, Severity: sev,
			Title: strings.TrimSpace(c.Title), Summary: strings.TrimSpace(c.Summary),
			FileHint: strings.TrimSpace(c.FileHint), Status: "open",
		},
		fingerprint: insightFingerprint(c.Title, c.Summary),
	}, true
}

// insightRunErrorMessage 把 Pass A / Pass B 的 agent 运行失败转成写入 scan.error
// 的用户可读文案：prefix 区分失败阶段（"项目分析失败" / "建议核实失败"），原因部分
// 经 errorText 本地化 + 脱敏 + 截断，避免把内部实现细节抛给用户；拿不到可读原因时
// 回退到通用的"<prefix>，请重试"。
func insightRunErrorMessage(prefix string, err error) string {
	if err == nil {
		return prefix + "，请重试"
	}
	msg := redactAgentText(errorText(err))
	// errorText 对未翻译的英文错误会包一层"任务执行失败，请查看任务日志后重试。"——
	// 该提示面向对话/任务页，对优化建议扫描无意义，剥掉让原因直接可见。
	const fallbackPrefix = "任务执行失败，请查看任务日志后重试。："
	msg = strings.TrimPrefix(msg, fallbackPrefix)
	msg = truncateInsightLog(strings.TrimSpace(msg), 240)
	if msg == "" || msg == "任务执行失败，请查看任务日志后重试。" {
		return prefix + "，请重试"
	}
	// 超时信息已自带完整原因（"分析超时：…"），无需再叠"项目分析失败："前缀。
	if strings.HasPrefix(msg, "分析超时") {
		return msg
	}
	return prefix + "：" + msg
}

// currentInsightAgent 返回项目当前会话的 agentId（决定扫描/再验证用 claude 还是
// codex）；无当前会话或缺省值时回退 claude-code。
func (s *Server) currentInsightAgent(ctx context.Context, projectID string) string {
	agentID := "claude-code"
	_ = s.db.QueryRowContext(ctx, `select agent_id from conversations where project_id=? and is_current=true`, projectID).Scan(&agentID)
	if agentID != "codex" {
		agentID = "claude-code"
	}
	return agentID
}

type insightWorkspaceRevision struct {
	RepoSHA   string
	Signature string
	Available bool
}

// insightWorkspaceRevision 对 Git 工作区创建一个可比较的版本标识。HEAD 之外还包含
// 未提交 diff、未跟踪文件内容与工作区状态，防止扫描/验证跨越用户正在进行的修改后仍然
// 落库旧结论。通过项目对应的 Git runner 执行，确保 SSH 项目在远端工作区取快照。
// 非 Git 项目保留可用性为 false；该场景仍可分析，但无法提供 Git 级版本一致性保证。
func (s *Server) insightWorkspaceRevision(ctx context.Context, project Project) (insightWorkspaceRevision, error) {
	runner, repo, err := s.insightGitRunner(ctx, project)
	if err != nil {
		return insightWorkspaceRevision{}, fmt.Errorf("resolve project Git runner: %w", err)
	}
	headBytes, err := runner.runGit(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		return insightWorkspaceRevision{}, nil
	}
	statusBytes, err := runner.runGit(ctx, repo, "status", "--porcelain=v2", "--untracked-files=all")
	if err != nil {
		return insightWorkspaceRevision{}, fmt.Errorf("read Git workspace status: %w", err)
	}
	diffBytes, err := runner.runGit(ctx, repo, "diff", "--no-ext-diff", "--binary", "HEAD")
	if err != nil {
		return insightWorkspaceRevision{}, fmt.Errorf("read Git workspace diff: %w", err)
	}
	// git diff HEAD 不会包含未跟踪文件内容；仅把它们的路径写进 status 会漏掉
	// “同一个新文件被继续修改”的场景，因此额外取得每个未跟踪文件的 Git 内容哈希。
	untrackedBytes, err := runner.runGit(ctx, repo, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return insightWorkspaceRevision{}, fmt.Errorf("list untracked files: %w", err)
	}
	untrackedHash, err := insightUntrackedContentHash(ctx, runner, repo, untrackedBytes)
	if err != nil {
		return insightWorkspaceRevision{}, err
	}
	head := strings.TrimSpace(string(headBytes))
	digest := sha256.Sum256([]byte(head + "\x00" + string(statusBytes) + "\x00" + string(diffBytes) + "\x00" + untrackedHash))
	return insightWorkspaceRevision{RepoSHA: head, Signature: fmt.Sprintf("%x", digest[:]), Available: true}, nil
}

// insightGitRunner 与 runReadOnlyAgent 使用相同的 runner 判定：只有 ssh-* 项目在
// 远端执行 Git；其余本地/WSL/兼容旧 runner 标识均直接使用本机 Git。
func (s *Server) insightGitRunner(ctx context.Context, project Project) (GitRunner, string, error) {
	if !strings.HasPrefix(project.Runner, "ssh-") {
		return newGitRunner(), project.Path, nil
	}
	runner, ok := s.runnerRegistry.get(project.Runner)
	if !ok {
		return nil, "", &runnerOfflineError{RunnerID: project.Runner}
	}
	sshR, ok := runner.(*sshRunner)
	if !ok {
		return nil, "", errors.New("runner is not an SSH runner")
	}
	repo, err := sshR.canonicalProjectPath(ctx, project.Path)
	if err != nil {
		return nil, "", err
	}
	return newSSHGitRunner(sshR.client, repo), repo, nil
}

// insightUntrackedContentHash 将未跟踪文件按路径分批交给 git hash-object，避免把可能很大的
// 文件内容传回控制端；同一 GitRunner 同时支持本地和 SSH 后端。
func insightUntrackedContentHash(ctx context.Context, runner GitRunner, repo string, rawPaths []byte) (string, error) {
	paths := strings.FieldsFunc(string(rawPaths), func(r rune) bool { return r == '\x00' })
	if len(paths) == 0 {
		return "", nil
	}
	var b strings.Builder
	for start := 0; start < len(paths); start += 100 {
		end := min(start+100, len(paths))
		args := append([]string{"hash-object", "--"}, paths[start:end]...)
		output, err := runner.runGit(ctx, repo, args...)
		if err != nil {
			return "", fmt.Errorf("hash untracked files: %w", err)
		}
		hashes := strings.Fields(string(output))
		if len(hashes) != end-start {
			return "", errors.New("hash untracked files: unexpected Git output")
		}
		for index, hash := range hashes {
			b.WriteString(paths[start+index])
			b.WriteByte('\x00')
			b.WriteString(hash)
			b.WriteByte('\x00')
		}
	}
	return b.String(), nil
}

func (s *Server) insightWorkspaceUnchanged(ctx context.Context, project Project, before insightWorkspaceRevision) (bool, error) {
	if !before.Available {
		return true, nil
	}
	after, err := s.insightWorkspaceRevision(ctx, project)
	if err != nil {
		return false, err
	}
	return after.Available && after.Signature == before.Signature, nil
}

// runProjectInsightScan 后台执行一次完整扫描（Pass A 发现 → Pass B 核实 → 去重落库）。
// opts 携带本次方向（theme + types），写进 scan 行并由 Pass A prompt 消费。
// 任一 pass 失败或 JSON 解析失败都将 scan 置为 failed（不 panic、不留半截数据）。
func (s *Server) runProjectInsightScan(ctx context.Context, projectID, scanID string, opts scanOpts) {
	// emit 追加一条本次扫描的进度事件（供前端“分析信息”滚动展示）。
	emit := func(level, message string) { s.appendInsightEvent(ctx, scanID, level, message) }
	// markFailed 把本次扫描置为终态。ctx 被取消（用户停止 / 项目删除）时按"已取消"
	// 记录而不是 failed，让前端能区分"主动停止"与"分析失败"；写入走后台上下文，
	// 保证取消后（worker 的 ctx 已不可用）仍能落库。无取消时维持原失败语义。
	markFailed := func(errMsg string) {
		status := insightScanFailed
		level := "error"
		eventMsg := "分析失败：" + errMsg
		if errors.Is(ctx.Err(), context.Canceled) {
			status = insightScanCancelled
			level = "warn"
			errMsg = "已取消"
			eventMsg = "分析已取消"
		}
		persistInsightWrite(func(persistCtx context.Context) {
			s.appendInsightEvent(persistCtx, scanID, level, eventMsg)
			if _, err := s.db.ExecContext(persistCtx, `update project_insight_scans set status=?,error=?,completed_at=? where id=? and status=?`,
				status, errMsg, time.Now().UTC(), scanID, insightScanRunning); err != nil {
				log.Printf("[insights] mark scan %s %s: %v", scanID, status, err)
			}
		})
	}

	project, err := s.getProjectByID(ctx, projectID)
	if err != nil {
		markFailed("无法加载项目，请重试")
		return
	}

	// 决定用 claude 还是 codex：跟随项目当前会话的 agentId。
	agentID := s.currentInsightAgent(ctx, projectID)

	emit("info", "开始优化建议分析…")

	// 历史已报告指纹 + 清单（规则 1）：供 prompt 提示与落库去重。
	prevRows, err := s.db.QueryContext(ctx, `select title,summary from project_insights where project_id=? and coalesce(verification_result,'')<>'invalid'`, projectID)
	if err != nil {
		markFailed("读取历史建议失败，请重试")
		return
	}
	var alreadySurfaced []string
	seenFP := map[string]struct{}{}
	for prevRows.Next() {
		var t, sm string
		if err := prevRows.Scan(&t, &sm); err != nil {
			prevRows.Close()
			markFailed("读取历史建议失败，请重试")
			return
		}
		alreadySurfaced = append(alreadySurfaced, t)
		seenFP[insightFingerprint(t, sm)] = struct{}{}
	}
	if err := prevRows.Err(); err != nil {
		prevRows.Close()
		markFailed("读取历史建议失败，请重试")
		return
	}
	if err := prevRows.Close(); err != nil {
		markFailed("读取历史建议失败，请重试")
		return
	}

	workspaceRevision, err := s.insightWorkspaceRevision(ctx, project)
	if err != nil {
		markFailed("读取项目版本状态失败，请重新发起分析")
		return
	}
	// 非 Git 项目（Available=false）仍可分析：版本一致性检查在 insightWorkspaceUnchanged
	// 中对非 Git 恒返回 unchanged，分析可正常推进，只是不做跨阶段版本对账（repoSHA 为空）。
	if !workspaceRevision.Available {
		emit("info", "当前项目不是 Git 仓库，分析将跳过版本一致性校验")
	}
	repoSHA := workspaceRevision.RepoSHA

	// Pass A：发现。模型常在通读项目时过度叙述，把最终 JSON 数组留到回合末尾，可能在
	// 输出前就 end_turn——此时 transcript 里只有散文 + 随手写的字符串数组残片，
	// parseInsightCandidates 必然失败（这正是「分析代理未返回有效结果」的真因）。
	// 补救：用一条强约束的"只输出 JSON"修正 prompt 重试一次；仍失败才判 scan failed。
	emit("info", "第 1 轮：通读项目代码，收集候选发现…")
	textA, err := s.runReadOnlyAgent(ctx, project, agentID, buildInsightScanPrompt(project.Path, repoSHA, alreadySurfaced, opts), emit)
	if err != nil {
		log.Printf("[insights] project=%s Pass A runner error: %v", projectID, err)
		markFailed(insightRunErrorMessage("项目分析失败", err))
		return
	}
	candidates, err := parseInsightCandidates(textA)
	if err != nil {
		log.Printf("[insights] project=%s Pass A parse failed on first attempt; raw(len=%d): %q",
			projectID, len(textA), truncateInsightLog(textA, 300))
		// 修正 prompt 重试一次（同项目同方向，但明确"只输出数组"。）
		emit("warn", "首轮输出不规范，正在要求 AI 补交结果…")
		textA, err = s.runReadOnlyAgent(ctx, project, agentID, buildInsightRepairPromptWithHistory(project.Path, repoSHA, alreadySurfaced, opts), emit)
		if err != nil {
			log.Printf("[insights] project=%s Pass A retry runner error: %v", projectID, err)
			markFailed(insightRunErrorMessage("项目分析失败", err))
			return
		}
		candidates, err = parseInsightCandidates(textA)
		if err != nil {
			log.Printf("[insights] project=%s Pass A parse failed after retry; raw (len=%d): %q",
				projectID, len(textA), truncateInsightLog(textA, 1200))
			markFailed("分析代理未返回有效结果，请重试")
			return
		}
	}
	if len(candidates) > insightFindingsCap {
		candidates = candidates[:insightFindingsCap]
	}
	log.Printf("[insights] project=%s Pass A candidates=%d sample=%q", projectID, len(candidates), truncateInsightLog(textA, 500))
	emit("success", fmt.Sprintf("第 1 轮完成，收集到 %d 条候选发现", len(candidates)))

	// Pass B：独立核实。先确认工作区未在 Pass A 中变化，避免两个阶段分析不同版本。
	unchanged, revisionErr := s.insightWorkspaceUnchanged(ctx, project, workspaceRevision)
	if revisionErr != nil || !unchanged {
		markFailed("项目代码在分析中发生变化，已丢弃本轮结果，请重新发起")
		return
	}

	verified := map[int]bool{}
	if len(candidates) > 0 {
		emit("info", fmt.Sprintf("第 2 轮：逐项核实 %d 条候选…", len(candidates)))
		verdict, bErr := s.runInsightVerifyBatches(ctx, project, agentID, candidates, emit)
		if bErr != nil {
			log.Printf("[insights] project=%s Pass B failed: %v", projectID, bErr)
			// 区分 agent 进程运行失败（环境问题，提示"建议核实失败"）与解析失败（"未返回有效结果"）。
			if errors.Is(bErr, errInsightVerifyRunner) {
				markFailed(insightRunErrorMessage("建议核实失败", bErr))
			} else {
				markFailed("核实代理未返回有效结果，请重试")
			}
			return
		}
		confirmed := 0
		for _, isConfirmed := range verdict {
			if isConfirmed {
				confirmed++
			}
		}
		verified = verdict
		emit("success", fmt.Sprintf("核实完成，确认 %d 条有效", confirmed))
	}

	// 归一化 + 去重（规则 1）+ 落库。
	now := time.Now().UTC()
	emit("info", "正在去重并写入建议…")
	var accepted []fingerprintedFact
	suppressed := 0
	for idx, c := range candidates {
		// 规则 2：仅丢弃 Pass B 明确判定为 false 的候选。Pass B 已在 runInsightVerify 里
		// 用 validateInsightVerdict 强制逐条判定（漏判/越界/重复都会让核实失败并终止扫描），
		// 因此走到这里时 verified 应覆盖全部候选；此处"未覆盖按确认"仅是防御性兜底，
		// 防止未来放宽校验时误吞真实发现。
		if confirmed, ok := verified[idx+1]; ok && !confirmed {
			continue
		}
		norm, ok := validateAndNormalize(c)
		if !ok {
			continue
		}
		if _, dup := seenFP[norm.fingerprint]; dup {
			suppressed++
			continue
		}
		seenFP[norm.fingerprint] = struct{}{}
		accepted = append(accepted, norm)
	}
	log.Printf("[insights] project=%s Post-B candidates=%d accepted=%d suppressed=%d", projectID, len(candidates), len(accepted), suppressed)
	unchanged, revisionErr = s.insightWorkspaceUnchanged(ctx, project, workspaceRevision)
	if revisionErr != nil || !unchanged {
		markFailed("项目代码在分析中发生变化，已丢弃本轮结果，请重新发起")
		return
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		markFailed("写入分析结果失败，请重试")
		return
	}
	defer tx.Rollback() //nolint:errcheck
	inserted := 0
	for idx := range accepted {
		f := &accepted[idx]
		f.ID = uuid.NewString()
		f.ProjectID = projectID
		f.ScanID = scanID
		f.CreatedAt = now
		res, err := tx.ExecContext(ctx, `insert into project_insights
			(id,project_id,scan_id,type,severity,title,summary,file_hint,fingerprint,status,created_at,updated_at)
			values (?,?,?,?,?,?,?,?,?,?,?,?)
			on conflict(project_id,fingerprint) do update set
				scan_id=excluded.scan_id,type=excluded.type,severity=excluded.severity,title=excluded.title,
				summary=excluded.summary,file_hint=excluded.file_hint,status=excluded.status,
				verification_result='',verification_note='',verified_at=null,updated_at=excluded.updated_at
			where project_insights.verification_result='invalid'`,
			f.ID, f.ProjectID, f.ScanID, f.Type, f.Severity, f.Title, f.Summary, f.FileHint, f.fingerprint, f.Status, now, now)
		if err != nil {
			_ = tx.Rollback()
			markFailed("写入分析结果失败，请重试")
			return
		}
		if n, _ := res.RowsAffected(); n == 1 {
			inserted++
		} else {
			suppressed++
		}
	}
	if _, err := tx.ExecContext(ctx, `update project_insight_scans
		set status=?,agent=?,theme=?,focus_types=?,findings_count=?,suppressed_count=?,completed_at=?,repo_sha=? where id=?`,
		insightScanCompleted, agentID, opts.Theme, strings.Join(opts.Types, ","), inserted, suppressed, now, repoSHA, scanID); err != nil {
		_ = tx.Rollback()
		markFailed("更新扫描状态失败，请重试")
		return
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		markFailed("写入分析结果失败，请重试")
		return
	}
	emit("success", fmt.Sprintf("分析完成，新增 %d 条建议，忽略 %d 条重复", inserted, suppressed))
}

// triggerInsightScan POST /api/projects/{id}/insights/scan
// 单项目互斥：已有 running 扫描返回 409；否则插入 running 扫描行并后台执行。
func (s *Server) triggerInsightScan(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("projectID")
	if !s.projectExists(r.Context(), projectID) {
		http.NotFound(w, r)
		return
	}
	// 可选 body：{theme, types}（省略等价于全量扫描）。非法/空归一化，不报 400。
	var req scanRequest
	if !decodeOptional(w, r, &req) {
		return
	}
	opts := buildScanOpts(req)
	scanID := uuid.NewString()

	s.insightMu.Lock()
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	if closing {
		s.insightMu.Unlock()
		writeError(w, http.StatusServiceUnavailable, errors.New("服务正在关闭，请稍后再试"))
		return
	}
	if s.insightActive[projectID] {
		s.insightMu.Unlock()
		writeError(w, http.StatusConflict, errors.New("项目正在分析中，请稍候再试"))
		return
	}
	// 兜底：进程恢复后若 DB 残留上次未完成的 running 行，也视为占用。
	var runningExists int
	if err := s.db.QueryRowContext(r.Context(), `select exists(select 1 from project_insight_scans where project_id=? and status='running')`, projectID).Scan(&runningExists); err == nil && runningExists == 1 {
		s.insightMu.Unlock()
		writeError(w, http.StatusConflict, errors.New("项目正在分析中，请稍候再试"))
		return
	}
	// 先创建取消句柄并随 insightActive 一起登记，再解锁：让 cancel 端点也能命中
	// "启动中"这一瞬间（置 active 到 goroutine 就绪之间），避免启动窗口内取消被当作
	// no-op。runCancel 由 goroutine 的 defer 调用；若中途早退（工作区/落库失败）则在此
	// 显式调用，幂等无副作用。
	runCtx, runCancel := context.WithCancel(s.runtimeCtx)
	s.insightActive[projectID] = true
	s.insightCancels[projectID] = runCancel
	s.insightWG.Add(1)
	s.insightMu.Unlock()

	// 整个只读扫描持有工作区租约，使最终工作区指纹检查与 Milevia 发起的所有写入串行。
	releaseWorkspace, acquired := s.acquireProjectWorkspace(projectID, "insight-scan:"+scanID)
	if !acquired {
		runCancel()
		s.insightMu.Lock()
		delete(s.insightActive, projectID)
		delete(s.insightCancels, projectID)
		s.insightMu.Unlock()
		s.insightWG.Done()
		writeError(w, http.StatusConflict, errors.New("项目工作区正在被其他任务修改，请稍候再试"))
		return
	}
	workerStarted := false
	defer func() {
		if !workerStarted {
			releaseWorkspace()
		}
	}()
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(r.Context(), `insert into project_insight_scans
		(id,project_id,status,agent,theme,focus_types,findings_count,suppressed_count,created_at,started_at)
		values (?,?,?,?,?,?,0,0,?,?)`,
		scanID, projectID, insightScanRunning, "", opts.Theme, strings.Join(opts.Types, ","), now, now); err != nil {
		runCancel()
		s.insightMu.Lock()
		delete(s.insightActive, projectID)
		delete(s.insightCancels, projectID)
		s.insightMu.Unlock()
		s.insightWG.Done()
		writeError(w, http.StatusInternalServerError, errors.New("启动分析失败，请重试"))
		return
	}

	scan := InsightScan{ID: scanID, ProjectID: projectID, Status: insightScanRunning, Agent: "", Theme: opts.Theme, FocusTypes: opts.Types, FindingsCount: 0, SuppressedCount: 0, CreatedAt: now}
	writeJSON(w, http.StatusAccepted, scan)

	// 后台执行扫描。Done 在扫描 goroutine 内调用，使 insightWG 精确跟踪扫描生命周期，
	// 供 Close() 等待（而非在 HTTP handler 返回时就 Done，那会让 Close 立即通过）。
	// runCtx 已在登记 active 时创建：项目被删除/用户停止时取消它，避免 agent
	// 继续跑完剩余阶段做无用功。
	workerStarted = true
	go func() {
		defer s.insightWG.Done()
		defer releaseWorkspace()
		defer runCancel()
		defer func() {
			s.insightMu.Lock()
			delete(s.insightActive, projectID)
			delete(s.insightCancels, projectID)
			s.insightMu.Unlock()
		}()
		s.runProjectInsightScan(runCtx, projectID, scanID, opts)
	}()
}

// cancelInsightScan POST /api/projects/{projectID}/insights/cancel
// 停止该项目运行中的优化建议扫描或复核（若有）。取消通过 context 传播给 worker
// goroutine：agent 进程被终止，扫描/复核记录置为 cancelled（见 runProjectInsightScan
// 的 markFailed 与 runInsightFindingsVerifyRun 的 failAll 对 context.Canceled 的处理）。
// 无运行中的任务时幂等返回 202（cancelled=false），避免双连击报错。
func (s *Server) cancelInsightScan(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("projectID")
	if !s.projectExists(r.Context(), projectID) {
		http.NotFound(w, r)
		return
	}
	s.insightMu.Lock()
	cancel := s.insightCancels[projectID]
	running := s.insightActive[projectID]
	s.insightMu.Unlock()
	cancelled := running && cancel != nil
	if cancelled {
		cancel()
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"cancelled": cancelled})
}

// cancelInsightRun 取消该项目运行中的优化建议扫描/复核（若有）。供项目删除时中止
// 无用的 agent 运行；goroutine 收到取消后会中止 agent 并自行清理 insightActive。
func (s *Server) cancelInsightRun(projectID string) {
	s.insightMu.Lock()
	cancel := s.insightCancels[projectID]
	s.insightMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *Server) loadRunningInsightVerification(ctx context.Context, projectID string) *InsightVerificationRun {
	row := s.db.QueryRowContext(ctx, `select id,project_id,status,error,message,total_count,processed_count,created_at,started_at,completed_at
		from project_insight_verification_runs where project_id=? and status='running' order by created_at desc limit 1`, projectID)
	var run InsightVerificationRun
	var startedAt, completedAt sql.NullTime
	if err := row.Scan(&run.ID, &run.ProjectID, &run.Status, &run.Error, &run.Message, &run.TotalCount, &run.ProcessedCount, &run.CreatedAt, &startedAt, &completedAt); err != nil {
		return nil
	}
	if startedAt.Valid {
		run.StartedAt = &startedAt.Time
	}
	if completedAt.Valid {
		run.CompletedAt = &completedAt.Time
	}
	return &run
}

func (s *Server) listInsights(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("projectID")
	if !s.projectExists(r.Context(), projectID) {
		http.NotFound(w, r)
		return
	}

	resp := insightsResponse{Findings: []InsightFinding{}, Events: []InsightEvent{}}
	resp.Verification = s.loadRunningInsightVerification(r.Context(), projectID)
	row := s.db.QueryRowContext(r.Context(), `select id,project_id,status,error,agent,theme,focus_types,findings_count,suppressed_count,created_at,started_at,completed_at
		from project_insight_scans where project_id=? order by created_at desc limit 1`, projectID)
	var scan InsightScan
	var errMsg sql.NullString
	var themeStr, focusStr sql.NullString
	var startedAt, completedAt sql.NullTime
	err := row.Scan(&scan.ID, &scan.ProjectID, &scan.Status, &errMsg, &scan.Agent, &themeStr, &focusStr, &scan.FindingsCount, &scan.SuppressedCount, &scan.CreatedAt, &startedAt, &completedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, errors.New("读取分析结果失败，请重试"))
		return
	}
	if err == nil {
		scan.Error = errMsg.String
		scan.Theme = themeStr.String
		if len(focusStr.String) > 0 {
			scan.FocusTypes = strings.Split(focusStr.String, ",")
		}
		if startedAt.Valid {
			scan.StartedAt = &startedAt.Time
		}
		if completedAt.Valid {
			scan.CompletedAt = &completedAt.Time
		}
		if scan.Status == insightScanRunning {
			scan.Agent = ""
		}
		resp.Scan = &scan
		resp.HasScan = true
		resp.SuppressedCount = scan.SuppressedCount
		resp.Events = s.loadInsightEvents(r.Context(), scan.ID)

		// 规则 1：发现跨扫描去重累积。展示的是本项目"当前仍有效的建议全集"
		//（union 而非只看最新一次扫描），否则"仅命中重复的新扫描"会让既有建议从视图消失。
		// 经验证已失效（verification_result='invalid'）的建议不在此列，单独折叠返回。
		rows, err := s.db.QueryContext(r.Context(), `select `+insightFindingColumns+` from project_insights where project_id=? and coalesce(verification_result,'')<>'invalid' order by
			case severity when 'high' then 0 when 'normal' then 1 else 2 end, created_at asc`, projectID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, errors.New("读取分析结果失败，请重试"))
			return
		}
		for rows.Next() {
			if f, err := scanInsightFinding(rows.Scan); err == nil {
				resp.Findings = append(resp.Findings, f)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			writeError(w, http.StatusInternalServerError, errors.New("读取分析结果失败，请重试"))
			return
		}
		rows.Close()

		// 已失效建议（验证判定不再成立）：按最近验证时间倒序返回，供前端折叠展示原因。
		invRows, err := s.db.QueryContext(r.Context(), `select `+insightFindingColumns+` from project_insights where project_id=? and coalesce(verification_result,'')='invalid' order by coalesce(verified_at,created_at) desc`, projectID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, errors.New("读取分析结果失败，请重试"))
			return
		}
		for invRows.Next() {
			if f, err := scanInsightFinding(invRows.Scan); err == nil {
				resp.Invalidated = append(resp.Invalidated, f)
			}
		}
		if err := invRows.Err(); err != nil {
			invRows.Close()
			writeError(w, http.StatusInternalServerError, errors.New("读取分析结果失败，请重试"))
			return
		}
		invRows.Close()
		resp.OpenCount = len(resp.Findings)
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.OpenCount = len(resp.Findings)
	writeJSON(w, http.StatusOK, resp)
}

// insightUpdateRequest PATCH /insights/{findingID} 的可选 body：仅允许改 title/summary/severity。
type insightUpdateRequest struct {
	Title    *string `json:"title"`
	Summary  *string `json:"summary"`
	Severity *string `json:"severity"`
}

// insightFindingColumns 是查询 project_insights 全部展示列的前缀（含再验证三列）。
// 供 listInsights / loadInsightFinding / resolveVerifyTargets 共用，保证列序一致。
const insightFindingColumns = `id,project_id,scan_id,type,severity,title,summary,file_hint,status,created_at,
	coalesce(verification_result,''),coalesce(verification_note,''),verified_at,updated_at`

// scanInsightFinding 把一行 project_insights（insightFindingColumns 顺序）扫成 InsightFinding。
func scanInsightFinding(scan func(dest ...any) error) (InsightFinding, error) {
	var f InsightFinding
	var fileHint, vr, vn sql.NullString
	var verifiedAt sql.NullTime
	if err := scan(&f.ID, &f.ProjectID, &f.ScanID, &f.Type, &f.Severity, &f.Title, &f.Summary, &fileHint, &f.Status, &f.CreatedAt, &vr, &vn, &verifiedAt, &f.UpdatedAt); err != nil {
		return f, err
	}
	f.FileHint = fileHint.String
	f.VerificationResult = vr.String
	f.VerificationNote = vn.String
	if verifiedAt.Valid {
		f.VerifiedAt = &verifiedAt.Time
	}
	return f, nil
}

// loadInsightFinding 按 id + projectID 取一条 finding（防越权）。不存在返回 false。
func (s *Server) loadInsightFinding(ctx context.Context, projectID, findingID string) (InsightFinding, bool) {
	row := s.db.QueryRowContext(ctx, `select `+insightFindingColumns+` from project_insights where id=? and project_id=?`, findingID, projectID)
	f, err := scanInsightFinding(row.Scan)
	if err != nil {
		return f, false
	}
	return f, true
}

// deleteInsightFinding DELETE /api/projects/{projectID}/insights/{findingID}
// 硬删除：移除该行（含指纹），下次扫描同问题会被重新报告。校验 project_id 防越权。
func (s *Server) deleteInsightFinding(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("projectID")
	findingID := r.PathValue("findingID")
	if !s.projectExists(r.Context(), projectID) {
		http.NotFound(w, r)
		return
	}
	if _, ok := s.loadInsightFinding(r.Context(), projectID, findingID); !ok {
		http.NotFound(w, r)
		return
	}
	if _, err := s.db.ExecContext(r.Context(), `delete from project_insights where id=? and project_id=?`, findingID, projectID); err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("删除失败，请重试"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// updateInsightFinding PATCH /api/projects/{projectID}/insights/{findingID}
// 编辑 title/summary/severity（至少传一个）。编辑后重算指纹；若新指纹与项目内其他 finding
// 撞 UNIQUE 索引，返回 409。返回更新后的 InsightFinding。
func (s *Server) updateInsightFinding(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("projectID")
	findingID := r.PathValue("findingID")
	if !s.projectExists(r.Context(), projectID) {
		http.NotFound(w, r)
		return
	}
	f, ok := s.loadInsightFinding(r.Context(), projectID, findingID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	var input insightUpdateRequest
	if !decodeOptional(w, r, &input) {
		return
	}
	if input.Title == nil && input.Summary == nil && input.Severity == nil {
		writeError(w, http.StatusBadRequest, errors.New("请至少提供 title/summary/severity 之一"))
		return
	}
	// 合并输入：未提供的字段沿用原值。
	title := f.Title
	if input.Title != nil {
		title = strings.TrimSpace(*input.Title)
	}
	summary := f.Summary
	if input.Summary != nil {
		summary = strings.TrimSpace(*input.Summary)
	}
	sev := f.Severity
	if input.Severity != nil {
		sev = strings.TrimSpace(*input.Severity)
	}
	if sev != insightSeverityLow && sev != insightSeverityNormal && sev != insightSeverityHigh {
		sev = insightSeverityNormal
	}
	if title == "" {
		writeError(w, http.StatusBadRequest, errors.New("标题不能为空"))
		return
	}
	newFP := insightFingerprint(title, summary)
	now := time.Now().UTC()
	// 指纹查重与更新放进同一事务：单连接 SQLite 下事务内串行，避免"查→改"两段式
	// 在并发编辑/扫描插入同一指纹时误判，导致 UNIQUE 冲突落到 500 而非明确的 409。
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("保存失败，请重试"))
		return
	}
	defer tx.Rollback() //nolint:errcheck
	// 预检：新指纹是否与项目内其他 finding 撞车（UNIQUE(project_id,fingerprint)）。
	var dupCount int
	if err := tx.QueryRowContext(r.Context(), `select count(*) from project_insights where project_id=? and fingerprint=? and id<>?`, projectID, newFP, findingID).Scan(&dupCount); err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("保存失败，请重试"))
		return
	}
	if dupCount > 0 {
		writeError(w, http.StatusConflict, errors.New("与另一条建议重复，无法保存"))
		return
	}
	// 编辑即改变建议内容：上一轮验证（valid/invalid/failed）随之失效，重置为未验证。
	if _, err := tx.ExecContext(r.Context(), `update project_insights set title=?,summary=?,severity=?,fingerprint=?,verification_result='',verification_note='',verified_at=NULL,updated_at=? where id=? and project_id=?`,
		title, summary, sev, newFP, now, findingID, projectID); err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("保存失败，请重试"))
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("保存失败，请重试"))
		return
	}
	updated, ok := s.loadInsightFinding(r.Context(), projectID, findingID)
	if !ok {
		writeError(w, http.StatusInternalServerError, errors.New("保存失败，请重试"))
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// ─── 建议转任务（添加到任务）────────────────────────────────────────────────
//
// 优化建议必须转成任务后再下发，不能直接发送到对话。该端点把一条建议组织成任务
// （标题=建议标题、说明=可执行提示词、优先级按严重度映射），并在同一事务内把这条
// 建议从列表中删除（硬删），原子生效。
//
// 【规则 · 勿改】转任务后对该建议执行"硬删"，因此下次扫描会重新报告同一问题（指纹
// 不复存在）。这是有意为之：任务尚未真正修复前，问题仍可能存在，必须允许再次被
// 扫描发现并再次处置。不要改成软删/标记已处理/仅隐藏，否则该问题会被指纹去重吞掉、
// 永远不再上报。与手动"删除"按钮（deleteInsightFinding）语义一致。见 docs/25 §4.1/§6.7。

// insightSeverityLabels 供任务说明组装：严重度 → 用户可读的中文标签。
var insightSeverityLabels = map[string]string{
	insightSeverityLow:    "低",
	insightSeverityNormal: "普通",
	insightSeverityHigh:   "高",
}

// insightTaskPriority 把建议严重度映射为任务优先级（high→高, normal→普通, low→低）。
func insightTaskPriority(severity string) string {
	switch severity {
	case insightSeverityHigh:
		return "high"
	case insightSeverityLow:
		return "low"
	default:
		return "normal"
	}
}

// insightFindingLabel 归一化返回一条建议的类型/严重度中文标签；非法值回退默认标签。
func insightFindingLabel(labels map[string]string, value, fallback string) string {
	if label := labels[value]; label != "" {
		return label
	}
	return labels[fallback]
}

// buildInsightTaskDescription 把一条优化建议组织成任务说明：用户可读的现象描述 + 明确
// 行动指令。与原"发送到对话"提示词同构，改为服务端拼装，前端只负责调端点。
// summary 超长时先截断（保留结尾的行动指令行，避免 12000 说明上限把指令整段切掉）。
func buildInsightTaskDescription(f InsightFinding) string {
	summary := f.Summary
	if len([]rune(summary)) > 6000 {
		summary = truncateInsightRunes(summary, 6000) + "…（内容过长已截断）"
	}
	lines := []string{
		fmt.Sprintf("【优化建议】%s", f.Title),
		fmt.Sprintf("类型：%s　严重度：%s",
			insightFindingLabel(insightTypeLabels, f.Type, insightOptimization),
			insightFindingLabel(insightSeverityLabels, f.Severity, insightSeverityNormal)),
		fmt.Sprintf("说明：%s", summary),
	}
	if hint := strings.TrimSpace(f.FileHint); hint != "" {
		lines = append(lines, "相关位置："+hint)
	}
	lines = append(lines, "", "请据此排查并修复上述问题/实现上述功能。")
	return strings.Join(lines, "\n")
}

// truncateInsightRunes 按 rune 数截断（中文安全），超长截断。
func truncateInsightRunes(s string, limit int) string {
	runes := []rune(strings.TrimSpace(s))
	if len(runes) <= limit {
		return string(runes)
	}
	return string(runes[:limit])
}

// addInsightToTask POST /api/projects/{projectID}/insights/{findingID}/to-task
// 把一条优化建议转成任务（title/description/priority 由建议派生），随后硬删除该建议，
// 使其从建议列表消失。创建任务与删除建议在同一个事务内，避免只建任务没删建议或反之。
// 返回创建的 Task（201）。
func (s *Server) addInsightToTask(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("projectID")
	findingID := r.PathValue("findingID")
	if !s.projectExists(r.Context(), projectID) {
		http.NotFound(w, r)
		return
	}
	f, ok := s.loadInsightFinding(r.Context(), projectID, findingID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	// 与 UI 一致：建议复核中或已被判失效时不允许转任务。复核未出结果前转任务可能为
	// 不存在的问题建任务；已失效则是为已修复/伪问题建任务，浪费一次任务执行。
	if f.VerificationResult == insightVerifyPending {
		writeError(w, http.StatusConflict, errors.New("该建议正在复核中，请稍后再试"))
		return
	}
	if f.VerificationResult == insightVerifyInvalid {
		writeError(w, http.StatusConflict, errors.New("该建议已失效，请刷新后重试"))
		return
	}
	// 防御：正常流水线（扫描/PATCH）不会产出空白标题，但这是数据写入点，空标题任务
	// 会在任务板显示空白。与批量共用 convertInsightToTask，此处显式返回 400。
	if strings.TrimSpace(f.Title) == "" {
		writeError(w, http.StatusBadRequest, errors.New("建议标题为空，无法转任务"))
		return
	}
	task, converted, err := s.convertInsightToTask(r.Context(), projectID, f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// 并发防重：另一请求（双标签页/重复点击）已先转走并硬删该建议 → 视为已被处理。
	if !converted {
		writeError(w, http.StatusConflict, errors.New("该建议已被处理，请刷新后重试"))
		return
	}
	writeJSON(w, http.StatusCreated, task)
}

// convertInsightToTask 在同一事务里把一条建议转成任务并硬删建议，返回 (task, converted, err)：
//   - converted=true：任务已创建、建议已删除。
//   - converted=false, err=nil：建议已不存在或被并发处理（含复核中突变），未创建任务，调用方按跳过处理。
//   - err != nil：真实失败（已包装用户可读信息）。
func (s *Server) convertInsightToTask(ctx context.Context, projectID string, f InsightFinding) (Task, bool, error) {
	now := time.Now().UTC()
	task := Task{
		ID:                 uuid.NewString(),
		ProjectID:          projectID,
		Title:              truncateInsightRunes(f.Title, 120),
		Description:        truncateInsightRunes(buildInsightTaskDescription(f), 12000),
		Priority:           insightTaskPriority(f.Severity),
		Position:           0,
		Status:             taskTodo,
		CreatedAt:          now,
		UpdatedAt:          now,
		DependsOn:          []TaskDependency{},
		BlockedBy:          []TaskBlocker{},
		Blocks:             []TaskDependency{},
	}
	// 防御：正常流水线不会产出空白标题，但这是新的数据写入点，空标题任务会在任务板显示空白。
	if task.Title == "" {
		return Task{}, false, errors.New("建议标题为空，无法转任务")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, false, errors.New("创建任务失败，请重试")
	}
	defer tx.Rollback() //nolint:errcheck
	if err := tx.QueryRowContext(ctx, `select coalesce(max(position),0)+1 from tasks where project_id=?`, projectID).Scan(&task.Position); err != nil {
		return Task{}, false, errors.New("创建任务失败，请重试")
	}
	if _, err := tx.ExecContext(ctx, `insert into tasks (id,project_id,title,description,priority,pinned,position,status,created_at,updated_at) values (?,?,?,?,?,?,?,?,?,?)`,
		task.ID, task.ProjectID, task.Title, task.Description, task.Priority, false, task.Position, task.Status, task.CreatedAt, task.UpdatedAt); err != nil {
		return Task{}, false, errors.New("创建任务失败，请重试")
	}
	if err := recordTaskEventTx(ctx, tx, task.ID, "", "task.created", map[string]string{"status": task.Status}, now); err != nil {
		return Task{}, false, errors.New("创建任务失败，请重试")
	}
	// 建议已转为任务：从列表删除（硬删）。
	// 【规则 · 勿改】硬删 = 下次扫描会重新报告同一问题（指纹消失）。这是有意的：
	// 任务未真正修复前问题仍可能存在，必须允许再被发现。勿改为软删/已处理/仅隐藏。
	// WHERE 追加防护：批量转换时复核可能刚把该建议置为 pending 或判为 invalid，
	// 跳过避免为未定论/已失效问题建任务（单条端点同样受益于这层并发防护）。
	res, err := tx.ExecContext(ctx, `delete from project_insights where id=? and project_id=? and coalesce(verification_result,'')<>'pending' and coalesce(verification_result,'')<>'invalid'`, f.ID, projectID)
	if err != nil {
		return Task{}, false, errors.New("删除建议失败，请重试")
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return Task{}, false, errors.New("删除建议失败，请重试")
	}
	// 并发防重：删除影响 0 行说明该建议已被其它请求先转成任务（双标签页/重复请求），
	// 回滚本次任务创建，避免一条建议建出多个任务。
	if deleted != 1 {
		return Task{}, false, nil
	}
	if err := tx.Commit(); err != nil {
		return Task{}, false, errors.New("创建任务失败，请重试")
	}
	return task, true, nil
}

// resolveToTaskTargets 解析批量转任务的目标建议：缺省 = 全部未失效建议（已失效单独折叠展示，
// 复核中保留 —— 由 convertInsightToTask 的 pending 删除防护在事务内判定为"跳过"并计数）。
// 显式 ids 逐条校验归属并跳过已失效/不存在的；不存在的 id 忽略。批量是尽力而为，一条不该阻塞其余。
func (s *Server) resolveToTaskTargets(ctx context.Context, projectID string, ids []string) ([]InsightFinding, error) {
	if len(ids) == 0 {
		rows, err := s.db.QueryContext(ctx, `select `+insightFindingColumns+` from project_insights
			where project_id=? and coalesce(verification_result,'')<>'invalid'
			order by created_at asc`, projectID)
		if err != nil {
			return nil, errors.New("读取建议失败，请重试")
		}
		defer rows.Close()
		var out []InsightFinding
		for rows.Next() {
			if f, err := scanInsightFinding(rows.Scan); err == nil {
				out = append(out, f)
			}
		}
		if err := rows.Err(); err != nil {
			return nil, errors.New("读取建议失败，请重试")
		}
		return out, nil
	}
	var out []InsightFinding
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		f, ok := s.loadInsightFinding(ctx, projectID, id)
		if !ok {
			continue
		}
		if f.VerificationResult == insightVerifyInvalid {
			continue
		}
		out = append(out, f)
	}
	return out, nil
}

// addInsightToTasks POST /api/projects/{projectID}/insights/to-task
// 一键把当前全部有效建议添加为任务（等价于逐条点击"添加到任务"）。可选 body
// {"findingIds":[...]}，缺省 = 全部有效建议；复核中（pending）与已失效（invalid）自动跳过。
// 逐条独立事务：单条失败不影响其余，返回 {created, skipped, failed, tasks}（201）。
func (s *Server) addInsightToTasks(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("projectID")
	if !s.projectExists(r.Context(), projectID) {
		http.NotFound(w, r)
		return
	}
	var req struct {
		FindingIDs []string `json:"findingIds"`
	}
	if !decodeOptional(w, r, &req) {
		return
	}
	targets, err := s.resolveToTaskTargets(r.Context(), projectID, req.FindingIDs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	createdTasks := make([]Task, 0, len(targets))
	created, skipped, failed := 0, 0, 0
	for _, f := range targets {
		task, converted, convErr := s.convertInsightToTask(r.Context(), projectID, f)
		if convErr != nil {
			failed++
			continue
		}
		if !converted {
			skipped++
			continue
		}
		created++
		createdTasks = append(createdTasks, task)
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"created": created,
		"skipped": skipped,
		"failed":  failed,
		"tasks":   createdTasks,
	})
}
