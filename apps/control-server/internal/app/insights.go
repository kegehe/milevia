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
func truncateInsightLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…<truncated>"
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

// 每条扫描产出的发现数量上限（与服务端数量钳一致的硬顶）。
const insightFindingsCap = 50

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
}

// insightsResponse `GET /api/projects/{id}/insights` 的荷载。
type insightsResponse struct {
	Scan            *InsightScan     `json:"scan"`
	Findings        []InsightFinding `json:"findings"`
	HasScan         bool             `json:"hasScan"`
	SuppressedCount int              `json:"suppressedCount"`
	OpenCount       int              `json:"openCount"` // 当前有效建议总数（== len(Findings)），供前端区分"本次新增"
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
func (s *Server) runReadOnlyAgent(ctx context.Context, project Project, agentID, prompt string) (string, error) {
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

	sink := &orchestrationReviewSink{}
	scanCtx, cancel := context.WithTimeout(ctx, 20*time.Minute)
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
		return "", err
	}
	return strings.TrimSpace(sink.text.String()), nil
}

// insightReadOnlyTools 返回只读分析的放行工具清单。仅 claude 需要（它依赖
// --allowedTools 限制写入）；codex 由 read_only sandbox 保证，返回 nil。
func insightReadOnlyTools(agentID string) []string {
	if agentID != "claude-code" {
		return nil
	}
	return []string{"Read", "Glob", "Grep", "List", "ReadMultiToolInfo"}
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

// buildVerifyPrompt 组装 Pass B 的核实 prompt：逐条到项目里查证候选是否真实存在。
// 同样以"立即执行"的强指令开头，避免 agent 反问澄清。
func buildVerifyPrompt(projectPath string, candidates []pendingInsight) string {
	items := make([]string, 0, len(candidates))
	for i, c := range candidates {
		items = append(items, fmt.Sprintf("%d. {\"title\":%q,\"summary\":%q,\"type\":%q,\"severity\":%q,\"fileHint\":%q}",
			i+1, c.Title, c.Summary, c.Type, c.Severity, c.FileHint))
	}
	return fmt.Sprintf(`执行一个只读核验任务，立即开始，不要询问、不要确认、不要前言，直接输出唯一的结果。

核验对象目录：%s

对下面每条候选，到目录里读取相关文件/结构，判断它是否真实存在（是否伪需求、假 bug、已实现/已具备）：

候选清单：
%s

	硬性输出要求：你的整个回复只能包含一个 JSON 对象，不允许有任何前言、注释、Markdown 围栏、或对象之后的任何文字。结构：
{"findings":[{"index":1,"confirmed":true,"reason":""},{"index":2,"confirmed":false,"reason":"项目当前已具备"}]}
confirmed=true 表示确认真实存在；false 表示伪需求/假 bug/已具备。每条对应候选的 index；无法证实的判 false。`, projectPath, strings.Join(items, "\n"))
}

// buildVerifyRepairPrompt 组装 Pass B 核实输出非法后的修正重试 prompt。
// 仅复述候选与硬性输出契约，要求"整个回复只能是那个 JSON 对象"。
func buildVerifyRepairPrompt(projectPath string, candidates []pendingInsight) string {
	return fmt.Sprintf(`重新执行刚才的只读核验，直接给出结果，别再说思考过程。

上一轮你只做了查证却未在回复里给出唯一结果，这不可接受。

对象目录：%s
候选编号：%s

这一次的硬性要求（必须严格遵守）：
你的整段回复必须且仅是一个 JSON 对象，前面、后面、中间都不允许任何说明、思考、标题、示例或 Markdown 围栏。结构必须是：
{"findings":[{"index":1,"confirmed":true,"reason":""},{"index":2,"confirmed":false,"reason":"项目当前已具备"}]}
每条 findings 的 index 对应候选编号；confirmed=true 表示确认真实存在、false 表示伪需求/假 bug/已具备；无法证实的判 false。每条候选都必须有一条。

现在只输出那个 JSON 对象：`, projectPath, insightCandidateIDs(candidates))
}

// insightCandidateIDs 渲染候选的编号清单（供核实 prompt 指代）。
func insightCandidateIDs(candidates []pendingInsight) string {
	parts := make([]string, 0, len(candidates))
	for i := range candidates {
		parts = append(parts, fmt.Sprintf("%d", i+1))
	}
	return strings.Join(parts, "、")
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

func (s *Server) runInsightVerify(ctx context.Context, project Project, agentID string, candidates []pendingInsight) ([]struct {
	Index     int  `json:"index"`
	Confirmed bool `json:"confirmed"`
}, error) {
	cc := func(prompt string) (string, error) {
		return s.runReadOnlyAgent(ctx, project, agentID, prompt)
	}
	// 首轮。
	textB, err := cc(buildVerifyPrompt(project.Path, candidates))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errInsightVerifyRunner, err)
	}
	verdict, vErr := decodeInsightVerdict(textB)
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

// runProjectInsightScan 后台执行一次完整扫描（Pass A 发现 → Pass B 核实 → 去重落库）。
// opts 携带本次方向（theme + types），写进 scan 行并由 Pass A prompt 消费。
// 任一 pass 失败或 JSON 解析失败都将 scan 置为 failed（不 panic、不留半截数据）。
func (s *Server) runProjectInsightScan(ctx context.Context, projectID, scanID string, opts scanOpts) {
	markFailed := func(errMsg string) {
		_, _ = s.db.ExecContext(ctx, `update project_insight_scans set status=?,error=?,completed_at=? where id=?`,
			insightScanFailed, errMsg, time.Now().UTC(), scanID)
	}

	project, err := s.getProjectByID(ctx, projectID)
	if err != nil {
		markFailed("无法加载项目，请重试")
		return
	}

	// 决定用 claude 还是 codex：跟随项目当前会话的 agentId。
	agentID := "claude-code"
	_ = s.db.QueryRowContext(ctx, `select agent_id from conversations where project_id=? and is_current=true`, projectID).Scan(&agentID)
	if agentID != "codex" {
		agentID = "claude-code"
	}

	// 历史已报告指纹 + 清单（规则 1）：供 prompt 提示与落库去重。
	prevRows, err := s.db.QueryContext(ctx, `select title,summary from project_insights where project_id=?`, projectID)
	if err != nil {
		markFailed("读取历史建议失败，请重试")
		return
	}
	var alreadySurfaced []string
	seenFP := map[string]struct{}{}
	for prevRows.Next() {
		var t, sm string
		if prevRows.Scan(&t, &sm) == nil {
			alreadySurfaced = append(alreadySurfaced, t)
			seenFP[insightFingerprint(t, sm)] = struct{}{}
		}
	}
	prevRows.Close()

	// repo_sha：项目 HEAD（非 git / 出错为空串）。
	repoSHA := ""
	if out, err := s.gitOutput(ctx, project.Path, "rev-parse", "HEAD"); err == nil {
		repoSHA = strings.TrimSpace(out)
	}

	// Pass A：发现。模型常在通读项目时过度叙述，把最终 JSON 数组留到回合末尾，可能在
	// 输出前就 end_turn——此时 transcript 里只有散文 + 随手写的字符串数组残片，
	// parseInsightCandidates 必然失败（这正是「分析代理未返回有效结果」的真因）。
	// 补救：用一条强约束的"只输出 JSON"修正 prompt 重试一次；仍失败才判 scan failed。
	textA, err := s.runReadOnlyAgent(ctx, project, agentID, buildInsightScanPrompt(project.Path, repoSHA, alreadySurfaced, opts))
	if err != nil {
		log.Printf("[insights] project=%s Pass A runner error: %v", projectID, err)
		markFailed("项目分析失败，请重试")
		return
	}
	candidates, err := parseInsightCandidates(textA)
	if err != nil {
		log.Printf("[insights] project=%s Pass A parse failed on first attempt; raw(len=%d): %q",
			projectID, len(textA), truncateInsightLog(textA, 300))
		// 修正 prompt 重试一次（同项目同方向，但明确"只输出数组"。）
		textA, err = s.runReadOnlyAgent(ctx, project, agentID, buildInsightRepairPrompt(project.Path, repoSHA, opts))
		if err != nil {
			log.Printf("[insights] project=%s Pass A retry runner error: %v", projectID, err)
			markFailed("项目分析失败，请重试")
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

	// Pass B：独立核实。
	verified := map[int]bool{}
	if len(candidates) > 0 {
		verdict, bErr := s.runInsightVerify(ctx, project, agentID, candidates)
		if bErr != nil {
			log.Printf("[insights] project=%s Pass B failed: %v", projectID, bErr)
			// 区分 agent 进程运行失败（环境问题，提示"建议核实失败"）与解析失败（"未返回有效结果"）。
			if errors.Is(bErr, errInsightVerifyRunner) {
				markFailed("建议核实失败，请重试")
			} else {
				markFailed("核实代理未返回有效结果，请重试")
			}
			return
		}
		for _, v := range verdict {
			verified[v.Index] = v.Confirmed
		}
	}

	// 归一化 + 去重（规则 1）+ 落库。
	now := time.Now().UTC()
	var accepted []fingerprintedFact
	suppressed := 0
	for idx, c := range candidates {
		// 规则 2：仅丢弃 Pass B 明确判定为 false 的候选；Pass B 未覆盖（map 缺失）的按确认处理——
		// 宁可放过个别伪需求，也不让 Pass B 漏判静默吞掉真实发现。
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
			(id,project_id,scan_id,type,severity,title,summary,fingerprint,status,created_at,updated_at)
			values (?,?,?,?,?,?,?,?,?,?,?)
			on conflict(project_id,fingerprint) do nothing`,
			f.ID, f.ProjectID, f.ScanID, f.Type, f.Severity, f.Title, f.Summary, f.fingerprint, f.Status, now, now)
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
	s.insightActive[projectID] = true
	s.insightWG.Add(1)
	s.insightMu.Unlock()

	scanID := uuid.NewString()
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(r.Context(), `insert into project_insight_scans
		(id,project_id,status,agent,theme,focus_types,findings_count,suppressed_count,created_at,started_at)
		values (?,?,?,?,?,?,0,0,?,?)`,
		scanID, projectID, insightScanRunning, "", opts.Theme, strings.Join(opts.Types, ","), now, now); err != nil {
		s.insightMu.Lock()
		delete(s.insightActive, projectID)
		s.insightMu.Unlock()
		s.insightWG.Done()
		writeError(w, http.StatusInternalServerError, errors.New("启动分析失败，请重试"))
		return
	}

	scan := InsightScan{ID: scanID, ProjectID: projectID, Status: insightScanRunning, Agent: "", Theme: opts.Theme, FocusTypes: opts.Types, FindingsCount: 0, SuppressedCount: 0, CreatedAt: now}
	writeJSON(w, http.StatusAccepted, scan)

	// 后台执行扫描。Done 在扫描 goroutine 内调用，使 insightWG 精确跟踪扫描生命周期，
	// 供 Close() 等待（而非在 HTTP handler 返回时就 Done，那会让 Close 立即通过）。
	go func() {
		defer s.insightWG.Done()
		defer func() {
			s.insightMu.Lock()
			delete(s.insightActive, projectID)
			s.insightMu.Unlock()
		}()
		s.runProjectInsightScan(s.runtimeCtx, projectID, scanID, opts)
	}()
}

func (s *Server) listInsights(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("projectID")
	if !s.projectExists(r.Context(), projectID) {
		http.NotFound(w, r)
		return
	}

	resp := insightsResponse{Findings: []InsightFinding{}}
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

		// 规则 1：发现跨扫描去重累积。展示的是本项目"当前仍有效的建议全集"
		//（union 而非只看最新一次扫描），否则"仅命中重复的新扫描"会让既有建议从视图消失。
		rows, err := s.db.QueryContext(r.Context(), `select id,project_id,scan_id,type,severity,title,summary,file_hint,status,created_at
			from project_insights where project_id=? order by
				case severity when 'high' then 0 when 'normal' then 1 else 2 end, created_at asc`, projectID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, errors.New("读取分析结果失败，请重试"))
			return
		}
		defer rows.Close()
		for rows.Next() {
			var f InsightFinding
			if rows.Scan(&f.ID, &f.ProjectID, &f.ScanID, &f.Type, &f.Severity, &f.Title, &f.Summary, &f.FileHint, &f.Status, &f.CreatedAt) == nil {
				resp.Findings = append(resp.Findings, f)
			}
		}
		if err := rows.Err(); err != nil {
			writeError(w, http.StatusInternalServerError, errors.New("读取分析结果失败，请重试"))
			return
		}
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

// loadInsightFinding 按 id + projectID 取一条 finding（防越权）。不存在返回 false。
func (s *Server) loadInsightFinding(ctx context.Context, projectID, findingID string) (InsightFinding, bool) {
	var f InsightFinding
	var fileHint sql.NullString
	err := s.db.QueryRowContext(ctx, `select id,project_id,scan_id,type,severity,title,summary,file_hint,status,created_at
		from project_insights where id=? and project_id=?`, findingID, projectID).
		Scan(&f.ID, &f.ProjectID, &f.ScanID, &f.Type, &f.Severity, &f.Title, &f.Summary, &fileHint, &f.Status, &f.CreatedAt)
	if err != nil {
		return f, false
	}
	f.FileHint = fileHint.String
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
	// 预检：新指纹是否与项目内其他 finding 撞车（UNIQUE(project_id,fingerprint)）。
	var dupCount int
	if err := s.db.QueryRowContext(r.Context(), `select count(*) from project_insights where project_id=? and fingerprint=? and id<>?`, projectID, newFP, findingID).Scan(&dupCount); err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("保存失败，请重试"))
		return
	}
	if dupCount > 0 {
		writeError(w, http.StatusConflict, errors.New("与另一条建议重复，无法保存"))
		return
	}
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(r.Context(), `update project_insights set title=?,summary=?,severity=?,fingerprint=?,updated_at=? where id=? and project_id=?`,
		title, summary, sev, newFP, now, findingID, projectID); err != nil {
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
