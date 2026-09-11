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
	"runtime/debug"
	"strconv"
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

// insightVerifyBatchSize 是 Pass B 与再验证共用的"单趟 agent 一次核验条数"上限：
// 超过时分批串行跑，避免单趟 prompt 过大、agent 通读过多文件导致超时/上下文偏紧。
// 再验证在 ≤20 条的常见区间还会用 insightReverifyChunkSize 进一步切小批，见其注释。
const insightVerifyBatchSize = 20

// insightScanPassTimeout 是单趟只读 agent（Pass A 发现 / Pass B 核实）的执行上限。
// 超时后进程会被终止，扫描置 failed 并明确提示"分析超时"（见 runReadOnlyAgent 的
// scanCtx.Err() 分支），避免用户面对无解释的"项目分析失败"。
const insightScanPassTimeout = 20 * time.Minute

// insightScanRunTimeout 是一次完整扫描（Pass A → 可能的修正重试 → Pass B 分批 ×
// 可能的修正重试 → 落库）的总预算。单趟上限是"每趟"的：最坏情况（Pass A 重试 +
// Pass B 两批各重试）会累计到 6 趟，没有总预算就会拖到 2 小时。
//
// 取值口径：**必须宽于"成功扫描的合理最坏情况"**——预算若落在合法慢扫描的耗时区间里，
// 它自己就成了失败来源（跑满 30 分钟然后把结果全丢掉，是最差的组合）。两趟各十几分钟的
// 扫描是正常的，故取 45 分钟（与 insightVerifyRunTimeout 一致），仍远低于理论最坏 120 分钟。
// 注意：扫描期间**不持有**项目工作区租约，因此放宽这个值不会让任何项目的占用时间变长，
// 只是让慢扫描有机会跑完（代价是更长时间窗口内工作区可能被改动、结果被判作废）。
const insightScanRunTimeout = 45 * time.Minute

// insightPublishLeaseWait 是发布结果（落库）前等待项目工作区空闲的上限；实际等待还会被
// 本次运行的剩余总预算夹住（取二者较小值）。结果已经算好、只差写不进去——等一小会儿
// 好过把整趟分析的成果丢掉，但也不能无限等：等待本身就意味着项目正被别的任务改写，
// 拖得越久结果越可能过期，用户也越久看不到结论。超时则明确告知本次结果未写入。
const insightPublishLeaseWait = 2 * time.Minute

// insightFindingsListLimit 是列表接口单次返回的建议条数上限（有效 / 已失效 / 已忽略
// 各自独立计数）。建议跨扫描去重累积，理论上可无限增长；这里兜底防止响应体失控，
// 触顶时响应带 truncated 标记，前端如实展示"仅显示最近 N 条"。
const insightFindingsListLimit = 500

// insightHistoryPromptLimit 是喂给发现 agent 的「历史已报告清单」条数上限。
// 超过时只给最近这些条（按创建时间倒序），避免 prompt 随项目历史无限膨胀；
// 去重判定不受影响——指纹集合始终来自全部历史行，且最终由 DB 唯一索引兜底。
const insightHistoryPromptLimit = 100

// insightHistorySummaryLimit 是「历史已报告」清单里每条说明的截断长度（够模型辨认同一
// 问题即可，不必整段回灌）。截断按 rune 计，避免中文被切半。
const insightHistorySummaryLimit = 80

// insightRejectionReasonLimit 是单条「被核实剔除」原因写入 scan 行的截断长度。
const insightRejectionReasonLimit = 200

// 再验证可以分批，但整个请求必须有总上限，避免单批超时累计成无限后台任务。
// 批次之间彼此独立发布（见 runInsightFindingsVerifyRun），因此这个总预算被触顶时
// 只影响尚未跑完的尾批，已发布的批次不受影响。
const insightVerifyRunTimeout = 45 * time.Minute

// insightPersistenceTimeout 为服务关闭后写入任务终态预留时间。
const insightPersistenceTimeout = 10 * time.Second

// insightReverifyChunkSize 返回一次复核中单趟只读 agent 一次核验的建议条数。
// 复核进度以"批"为粒度落库（processed_count / run.message），而单趟 agent 必须整批
// 返回后才给出判定——若把整份清单（常见 ≤20 条）全装进一趟，复核全程（可能数分钟到
// 十几分钟）进度会一直停在 0/N，再瞬间跳到完成，用户看到的就是"没有进度"。这里对
// ≤20 条（原单趟装完的区间）切成约 3 批，让进度出现可见的中间点；超过 20 条维持原
// insightVerifyBatchSize 上限（那些清单本来就有批次间进度点，不再额外拆出 agent 会话，
// 也避免 prompt 无谓变小）。小清单（≤6 条）不切，省掉无谓的会话开销。
//
// 批次独立发布（见 runInsightFindingsVerifyRun）之后，"切小批"还多了一层作用：
// 单批失败只影响该批，切得越细，一次失败牵连的建议越少。
func insightReverifyChunkSize(n int) int {
	if n > insightVerifyBatchSize {
		return insightVerifyBatchSize
	}
	if n <= 6 {
		return n
	}
	chunk := (n + 2) / 3 // ceil(n/3)：目标约 3 批，兼顾进度粒度与 agent 会话数
	if chunk < 2 {
		chunk = 2
	}
	if chunk > n {
		chunk = n
	}
	return chunk
}

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

// scanRequest POST /insights/scan 的可选 body：选定 Agent 与分析方向（agent + theme + types）。
// theme 空串 = 全面分析；types 空/全选 = 全查。两者都可省略，缺省等价于原全量扫描。
type scanRequest struct {
	Agent string   `json:"agent"`
	Theme string   `json:"theme"`
	Types []string `json:"types"`
}

// scanOpts 本次扫描的 Agent 与方向，供 prompt 组装与入库。
type scanOpts struct {
	Agent string
	Theme string
	Types []string // 已归一化、去重；空 = 全查
}

func normalizeScanAgent(agent string) string {
	if agent == "claude-code" || agent == "codex" {
		return agent
	}
	return ""
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
	return scanOpts{Agent: normalizeScanAgent(req.Agent), Theme: normalizeScanTheme(req.Theme), Types: normalizeScanTypes(req.Types)}
}

// insightFindingStatus* 是 project_insights.status 的取值。当前只有两态：
// open（在有效列表中）与 dismissed（用户点了「不再提示」，折叠展示、不再被扫描上报）。
// 已失效（verification_result='invalid'）是独立的一维状态，不占用 status。
const (
	insightStatusOpen      = "open"
	insightStatusDismissed = "dismissed"
)

// insightSuppressionSuperseded 是 project_insight_suppressions.reason 的取值：
// 建议被用户编辑（title/summary 变了），编辑前的旧指纹记入该表，永不再次上报。
const insightSuppressionSuperseded = "superseded"

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
	// Rejected 是本次扫描中「第 2 轮独立核实判为不成立」而被丢弃的候选，附 AI 给出的
	// 判定依据。规则 2（必须核实）的可审计性来源：用户能看到被剔除的是什么、为什么。
	Rejected    []InsightRejection `json:"rejected,omitempty"`
	CreatedAt   time.Time          `json:"createdAt"`
	StartedAt   *time.Time         `json:"startedAt,omitempty"`
	CompletedAt *time.Time         `json:"completedAt,omitempty"`
}

// InsightRejection 一条被核实环节剔除的候选发现（不落 project_insights）。
type InsightRejection struct {
	Title  string `json:"title"`
	Reason string `json:"reason,omitempty"`
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

	// LinkedTaskStatus 是该建议已转成的任务的当前状态（todo/running/done/…），
	// 仅在本项目存在同指纹任务时非空，由 listInsights 计算，不落库。
	// 用于在卡片上提示"已转为任务"，并阻止为同一问题重复建任务。
	LinkedTaskStatus string `json:"linkedTaskStatus,omitempty"`
	LinkedTaskTitle  string `json:"linkedTaskTitle,omitempty"`
}

// insightTaskTerminal 判断任务是否已到终态（终态意味着"该问题已被处理过一轮"，
// 此时允许再次为同一建议建任务；未完任务则视为重复，拒绝重复建任务）。
func insightTaskTerminal(status string) bool {
	return status == taskDone || status == taskCancelled
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
	DefaultAgent    string           `json:"defaultAgent"`
	Scan            *InsightScan     `json:"scan"`
	Findings        []InsightFinding `json:"findings"`
	Events          []InsightEvent   `json:"events"`
	HasScan         bool             `json:"hasScan"`
	SuppressedCount int              `json:"suppressedCount"`
	OpenCount       int              `json:"openCount"` // 当前有效建议总数（== len(Findings)），供前端区分"本次新增"
	// Invalidated 是经验证已失效、从有效列表隐藏的建议（折叠展示，含 AI 判断依据）。
	Invalidated []InsightFinding        `json:"invalidated,omitempty"`
	Verification *InsightVerificationRun `json:"verification,omitempty"`
	// Dismissed 是用户点了「不再提示」的建议（折叠展示，可恢复）。它们不再出现在
	// 有效列表中，也不会被后续扫描再次上报；手动删除或恢复会解除这一状态。
	Dismissed []InsightFinding `json:"dismissed,omitempty"`
	// Truncated 为 true 表示列表触到了 insightFindingsListLimit，前端据此提示
	// "仅显示最近 N 条"，避免用户以为建议凭空消失。
	Truncated bool `json:"truncated,omitempty"`
	// FindingsLimit 是列表上限，供前端在 truncated 时展示准确文案。
	FindingsLimit int `json:"findingsLimit,omitempty"`
}

type verifyInsightsRequest struct {
	Agent      string   `json:"agent"`
	FindingIDs []string `json:"findingIds"`
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

var (
	insightCandidatesOutputSchema = json.RawMessage(`{"type":"array","items":{"type":"object","properties":{"type":{"type":"string","enum":["bug","style","optimization","feature"]},"severity":{"type":"string","enum":["low","normal","high"]},"title":{"type":"string"},"summary":{"type":"string"},"fileHint":{"type":"string"}},"required":["type","severity","title","summary","fileHint"],"additionalProperties":false}}`)
	insightVerifyOutputSchema     = json.RawMessage(`{"type":"object","properties":{"findings":{"type":"array","items":{"type":"object","properties":{"index":{"type":"integer","minimum":1},"confirmed":{"type":"boolean"},"reason":{"type":"string"}},"required":["index","confirmed","reason"],"additionalProperties":false}}},"required":["findings"],"additionalProperties":false}`)
	insightReverifyOutputSchema   = json.RawMessage(`{"type":"object","properties":{"findings":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"status":{"type":"string","enum":["valid","invalid","uncertain"]},"reason":{"type":"string"}},"required":["id","status","reason"],"additionalProperties":false}}},"required":["findings"],"additionalProperties":false}`)
)

// rawInsightParse 兼容多种 agent 输出形态：裸数组、{findings:[...]} 包裹、
// Markdown 代码块（```json ... ```）、以及前后夹杂说明文字的 JSON。
func parseInsightCandidates(raw string) ([]pendingInsight, error) {
	values := extractInsightJSONValues(raw)
	if len(values) == 0 {
		return nil, errors.New("analysis returned empty output")
	}
	// Prefer the last complete value matching the result contract. This avoids
	// losing a valid final response to an earlier incomplete example or an
	// unrelated JSON value in the model's explanation.
	for index := len(values) - 1; index >= 0; index-- {
		var asSlice []pendingInsight
		if err := json.Unmarshal([]byte(values[index]), &asSlice); err == nil {
			return asSlice, nil
		}
		var wrapper map[string]json.RawMessage
		if err := json.Unmarshal([]byte(values[index]), &wrapper); err != nil || wrapper["findings"] == nil {
			continue
		}
		var findings []pendingInsight
		if err := json.Unmarshal(wrapper["findings"], &findings); err == nil {
			return findings, nil
		}
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
	values := extractInsightJSONValues(raw)
	if len(values) == 0 {
		return ""
	}
	return values[len(values)-1]
}

// extractInsightJSONValues returns complete top-level objects and arrays in
// encounter order. A malformed opening bracket is skipped so a later valid
// final response remains recoverable.
func extractInsightJSONValues(raw string) []string {
	s := strings.TrimSpace(stripInsightCodeFences(raw))
	const maxTry = 128
	values := make([]string, 0, 1)
	for try := 0; try < maxTry; try++ {
		_, index := insightJSONFirstDelim(s)
		if index < 0 {
			break
		}
		var candidate json.RawMessage
		decoder := json.NewDecoder(strings.NewReader(s[index:]))
		if err := decoder.Decode(&candidate); err != nil || !json.Valid(candidate) {
			s = s[index+1:]
			continue
		}
		values = append(values, string(candidate))
		s = s[index+len(candidate):]
	}
	return values
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

// projectRuntimeProfile resolves the selected agent's project route into a
// runtime profile. Insights must follow the same per-agent routing as new
// conversations: projects can configure Claude and Codex independently.
// A legacy default is consulted only when it belongs to agentID (inside
// profileRouteForNewConversationTx); no matching route/profile falls back to
// the CLI's own credentials.
func (s *Server) projectRuntimeProfile(ctx context.Context, project Project, agentID string) (*AgentRuntimeProfile, error) {
	runnerID := project.RunnerID
	if runnerID == "" {
		runnerID = project.Runner
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // harmless after commit; cleans up error paths
	selection, err := s.profileRouteForNewConversationTx(ctx, tx, nil, runnerID, agentID, project.ID)
	if err != nil {
		return nil, err
	}
	revisionID := selection.ProfileRevisionID
	if revisionID == "" && selection.RouteRevisionID != "" {
		var mode string
		if err := tx.QueryRowContext(ctx, `select mode from project_agent_route_revisions where id=? and agent_id=?`, selection.RouteRevisionID, agentID).Scan(&mode); err != nil {
			return nil, err
		}
		if mode == "pool" {
			revisionID, err = s.selectPoolProfileRevisionTx(ctx, tx, selection.RouteRevisionID, runnerID, agentID)
			if err != nil {
				return nil, err
			}
		}
	}
	profile, err := s.runtimeProfileTx(ctx, tx, revisionID, runnerID, agentID)
	if err != nil {
		return nil, err
	}
	// Pool selection advances credential_pool_state in this transaction. A
	// rollback here would make round-robin selection restart at the same member.
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return profile, nil
}

// runReadOnlyAgent 封装「选 runner → 解析项目代理路由档案 → sink → 超时 → Run →
// 收集助手文本」，供 Pass A（发现）/ Pass B（核实）复用。严格只读：Claude 用
// `plan`、Codex 用 `read_only`，绕开 HTTP 层的 `validAgentPolicy`（见 docs/25 §5.2）。
// 选 runner 与 startMessage（app.go:3556）同一语义：SSH 走 runnerRegistry、本机按
// 目标环境；`agentClaudeRunnerFor/codexRunnerFor` 对 remote 返回 nil，故兜底 s.runner。
// progress 非空时把 agent 的实时工具动作（读取/搜索…）作为分析进度回调给调用方。
func (s *Server) runReadOnlyAgent(ctx context.Context, project Project, agentID, prompt string, progress func(level, message string)) (string, error) {
	return s.runReadOnlyAgentWithSchema(ctx, project, agentID, prompt, nil, progress)
}

func (s *Server) runReadOnlyAgentWithSchema(ctx context.Context, project Project, agentID, prompt string, outputSchema json.RawMessage, progress func(level, message string)) (string, error) {
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
			if agentID == "codex" {
				// Never run a Codex request through the Claude runner. This can
				// happen when a cross-environment runner (for example WSL) is not
				// available; returning an explicit error is safer than silently
				// launching the wrong CLI.
				return "", fmt.Errorf("没有可用的 Codex 分析运行器")
			}
			runner = s.runner // Claude 的兼容回退：目标 runner 缺失时用服务端 runner
		}
		if runner == nil {
			return "", fmt.Errorf("没有可用的分析运行器")
		}
	}

	profile, err := s.projectRuntimeProfile(ctx, project, agentID)
	if err != nil {
		return "", fmt.Errorf("resolve project agent profile: %w", err)
	}
	if agentID == "codex" {
		ready := false
		if capable, ok := runner.(CodexCapableRunner); ok {
			// SSH and cross-environment runners probe Codex on the target host.
			ready = capable.CodexReady(ctx)
		} else if local, ok := runner.(*codexCLIRunner); ok && profile != nil && profile.AuthMode == "api_key" {
			// Managed API-key profiles do not need persisted CLI login.
			ready = local.BinaryReady()
		} else {
			ready = runner.Ready(ctx)
		}
		if !ready {
			return "", errors.New("Codex CLI is unavailable or not logged in")
		}
	}
	cleanupQuota, err := s.reserveInsightQuota(ctx, project, agentID, profile)
	if err != nil {
		return "", fmt.Errorf("reserve analysis quota: %w", err)
	}
	defer cleanupQuota()

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
		OutputSchema:   outputSchema,
	}, sink); err != nil {
		// 区分"跑满单趟上限被杀"（agent 进程被终止，cmd.Wait 的报错不含 deadline
		// 语义，这里显式看 scanCtx.Err()）与真正的运行失败，让用户拿到准确原因。
		if scanCtx.Err() == context.DeadlineExceeded {
			// scanCtx 是从本次运行的 ctx 派生出来的，两者的 deadline 都可能触发。父 ctx
			// 也已结束时说明是**整趟运行的总预算**用尽（不是这一趟跑满 20 分钟）——
			// 报错必须区分，否则用户会按"单趟超时"去重试，而其实该缩小范围。
			if ctx.Err() != nil {
				return "", fmt.Errorf("分析超时：已超出本次运行的总时长上限（%d 分钟），已中止。可缩小分析范围（选择聚焦主题或更少的查找类型）后重试",
					int(insightScanRunTimeout.Minutes()))
			}
			return "", fmt.Errorf("分析超时：超过 %d 分钟未完成。项目可能较大或模型处理较慢，请稍后重试或更换更快的模型档案",
				int(insightScanPassTimeout.Minutes()))
		}
		return "", err
	}
	return strings.TrimSpace(sink.text.String()), nil
}

// reserveInsightQuota creates a short-lived internal run so read-only insight
// calls use the same quota admission as ordinary conversations. The run is
// deliberately not exposed to the conversation history and is removed after
// the agent call, while quota_reservations remains auditable during execution.
func (s *Server) reserveInsightQuota(ctx context.Context, project Project, agentID string, profile *AgentRuntimeProfile) (func(), error) {
	if profile == nil {
		return func() {}, nil
	}
	conversationID := "insight-quota-" + uuid.NewString()
	runID := "insight-quota-" + uuid.NewString()
	now := time.Now().UTC()
	routeRevisionID := ""
	runnerID := project.RunnerID
	if runnerID == "" {
		runnerID = project.Runner
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	cleanupTx := func() { _ = tx.Rollback() }
	defer cleanupTx()
	_ = tx.QueryRowContext(ctx, `select current_revision_id from project_agent_routes where project_id=? and agent_id=?`, project.ID, agentID).Scan(&routeRevisionID)
	if _, err := tx.ExecContext(ctx, `insert into conversations
		(id,project_id,claude_session_id,agent_id,agent_session_id,agent_runtime_id,agent_profile_revision_id,project_agent_route_revision_id,execution_policy,status,permission_mode,title,last_activity_at,claude_initialized,agent_initialized,is_current,created_at)
		values (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		conversationID, project.ID, conversationID, agentID, "", runnerID, profile.RevisionID, routeRevisionID,
		"read_only", "active", "read_only", "Insight quota", now, 0, 0, 0, now); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `insert into runs
		(id,conversation_id,agent_id,agent_runtime_id,agent_profile_revision_id,project_agent_route_revision_id,execution_policy,status,created_at)
		values (?,?,?,?,?,?,?,?,?)`, runID, conversationID, agentID, runnerID, profile.RevisionID, routeRevisionID, "read_only", "running", now); err != nil {
		return nil, err
	}
	if err := s.reserveProfileQuotaTx(ctx, tx, runID, profile.RevisionID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cleanup, cleanupErr := s.db.BeginTx(cleanupCtx, nil)
		if cleanupErr != nil {
			log.Printf("[insights] begin quota cleanup %s: %v", runID, cleanupErr)
			return
		}
		if cleanupErr = s.releaseQuotaReservations(cleanupCtx, cleanup, runID); cleanupErr == nil {
			_, cleanupErr = cleanup.ExecContext(cleanupCtx, `update runs set status='completed',completed_at=? where id=?`, time.Now().UTC(), runID)
		}
		if cleanupErr == nil {
			_, cleanupErr = cleanup.ExecContext(cleanupCtx, `delete from runs where id=?`, runID)
		}
		if cleanupErr == nil {
			_, cleanupErr = cleanup.ExecContext(cleanupCtx, `delete from conversations where id=?`, conversationID)
		}
		if cleanupErr != nil {
			_ = cleanup.Rollback()
			log.Printf("[insights] cleanup quota run %s: %v", runID, cleanupErr)
			return
		}
		if cleanupErr = cleanup.Commit(); cleanupErr != nil {
			log.Printf("[insights] commit quota cleanup %s: %v", runID, cleanupErr)
		}
	}, nil
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
	// 避免重复加进行时前缀：部分活动文案自身已含“正在”（如 Codex 的
	// “模型正在分析项目”），再加“正在”会拼出“正在模型正在分析项目”。
	if strings.Contains(message, "正在") {
		sink.onProgress("info", message)
	} else {
		sink.onProgress("info", "正在"+message)
	}
}

// parseInsightToolActivity 从 runner 事件里尽力提取一条“当前在做什么”的短信息。
// 识别 Claude assistant 事件中的 tool_use（Read/Glob/Grep/List 等）以及 Codex 的
// turn/item 事件；SSH 使用相同的事件格式时也能获得粗粒度进度，未知事件返回 ("", false)。
func parseInsightToolActivity(eventType string, payload json.RawMessage) (string, bool) {
	// Codex emits structured item events rather than Claude's assistant/tool_use
	// envelope. Surface those events as coarse-grained progress so a long
	// read-only scan does not look stalled while the model is working.
	if eventType == "turn.started" {
		return "模型正在分析项目", true
	}
	if eventType == "item.started" || eventType == "item.completed" {
		var envelope struct {
			Item struct {
				Type string `json:"type"`
			} `json:"item"`
		}
		if err := json.Unmarshal(payload, &envelope); err != nil {
			return "", false
		}
		switch envelope.Item.Type {
		case "command_execution":
			if eventType == "item.started" {
				return "执行只读检查", true
			}
			return "完成只读检查", true
		case "file_change":
			return "读取文件变更", true
		}
		return "", false
	}
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
//
// 每次追加都广播一次项目状态失效信号：项目总览卡片的「优化建议分析中」徽标与副标题
// 进度文案都来自 /api/projects/statuses，没有这条广播就只能等 30s 兜底轮询。事件本身
// 已被 sink 节流（同一动作/同类 3 秒最多一条），因此广播频率与进度更新频率一致。
func (s *Server) appendInsightEvent(ctx context.Context, scanID, level, message string) {
	if message == "" || scanID == "" {
		return
	}
	if _, err := s.db.ExecContext(ctx, `insert into project_insight_events (id,scan_id,seq,ts,level,message)
		select ?, ?, coalesce(max(seq),0)+1, ?, ?, ? from project_insight_events where scan_id=?`,
		uuid.NewString(), scanID, time.Now().UTC(), level, message, scanID); err != nil {
		log.Printf("[insights] append progress event scan=%s: %v", scanID, err)
		return
	}
	// 读一下所属项目再广播；项目已被删除时静默（事件本身会随级联删除消失）。
	var projectID string
	if err := s.db.QueryRowContext(ctx, `select project_id from project_insight_scans where id=?`, scanID).Scan(&projectID); err == nil {
		s.broadcastStateEvent(stEvProjects, projectID)
	}
}

// loadInsightEvents 返回某次扫描的进度事件（按 seq 升序）。sinceSeq > 0 时只返回
// seq 大于它的增量部分——前端 2s 轮询据此只取新事件，避免每次把整份日志重传。
func (s *Server) loadInsightEvents(ctx context.Context, scanID string, sinceSeq int) []InsightEvent {
	query := `select id,seq,ts,level,message from project_insight_events where scan_id=?`
	args := []any{scanID}
	if sinceSeq > 0 {
		query += ` and seq>?`
		args = append(args, sinceSeq)
	}
	query += ` order by seq asc`
	rows, err := s.db.QueryContext(ctx, query, args...)
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
// deny 清单真正落地。
//
// 维护口径（重要）：deny 清单必须覆盖"当前 CLI 版本里所有可写/可执行/可产生副作用的
// 工具"，即"allow 之外的一切"。核对方式：跑一次 `claude -p --output-format stream-json`
// 读 init 事件里的 tools 数组，与本清单逐项比对（实测 2.1.266 的列表为：
// Task/CronCreate/CronDelete/CronList/DesignSync/Edit/EnterWorktree/ExitWorktree/
// Glob/Grep/ListAgents/LSP/NotebookEdit/PowerShell/PushNotification/Read/ReportFindings/
// ScheduleWakeup/SendMessage/Skill/TaskOutput/TaskStop/WebFetch/WebSearch/Workflow/Write）。
// 其中 Read/Glob/Grep 是 allow 的三个只读工具，其余全部在此 deny；LSP/ListAgents 虽是
// 只读工具，但分析不需要它们，一并拒掉以保持"allow 之外一律不可用"这一不变量。
// 注意：枚举式硬拒对"未来版本新增的工具"没有免疫力——若 CLI 升级新增了可执行工具，
// 它会默认可用。CLI 自 2.1 起提供真正的白名单 `--tools`，但远端（WSL/SSH）侧版本无法
// 在本地核实，贸然加上会让旧版远端 CLI 直接报错，故暂未启用；升级远端后应改为
// `--tools "Read,Glob,Grep"` 这种正向白名单，届时本清单可以退役。
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
	// 任务管理（含读取/停止他人任务）。
	"TaskCreate", "TaskGet", "TaskList", "TaskOutput", "TaskStop", "TaskUpdate",
	// 网络检索：分析聚焦项目代码，避免跑题拖慢。
	"WebFetch", "WebSearch",
	// 只读但分析用不到的工具：拒掉以维持"allow 之外一律不可用"。
	"LSP", "ListAgents",
	// MCP 工具：只读分析不注入 MCP；这里再加一条 glob 兜底——即使将来某条只读路径
	// 漏掉注入控制，deny 规则也会拦下所有 server 的全部 MCP 工具（deny 优先级最高，
	// 且 hook 返回 allow 不覆盖 deny）。见 docs/34 §8.3。
	"mcp__*",
}

// insightReadOnlySettingsJSON 组装只读执行的 --settings JSON（permissions.deny
// 硬拒清单）。本地 claude runner（insights + orchestration 独立审查共用）、WSL 与
// SSH 三条只读路径都消费它，因此清单是两者共同的下界：
// 对 orchestration 的 `Read/Glob/Grep/List/ReadMultiToolInfo` 白名单而言，本清单
// 新增的项都不在其 allow 内——即"同样只读、只是更难以绕过"。失败返回错误（理论上不会发生）。
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
// Reason 承载模型给出的判定依据：confirmed=false 时它就是"为什么剔除"，
// 会被写进 scan 行的 rejected 列表展示给用户（规则 2 的可审计性）。
type insightVerifyVerdict struct {
	Findings []struct {
		Index     int    `json:"index"`
		Confirmed bool   `json:"confirmed"`
		Reason    string `json:"reason"`
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

// insightVerifyOutcome 是一条候选的核实结论（含模型给出的判定依据）。
type insightVerifyOutcome struct {
	Index     int
	Confirmed bool
	Reason    string
}

func (s *Server) runInsightVerify(ctx context.Context, project Project, agentID string, candidates []pendingInsight, progress func(level, message string)) ([]insightVerifyOutcome, error) {
	cc := func(prompt string) (string, error) {
		return s.runReadOnlyAgentWithSchema(ctx, project, agentID, prompt, insightVerifyOutputSchema, progress)
	}
	decode := func(text string) ([]insightVerifyOutcome, error) {
		verdict, err := decodeInsightVerdict(text)
		if err != nil {
			return nil, err
		}
		if err := validateInsightVerdict(verdict, len(candidates)); err != nil {
			return nil, err
		}
		out := make([]insightVerifyOutcome, 0, len(verdict.Findings))
		for _, item := range verdict.Findings {
			out = append(out, insightVerifyOutcome{
				Index:     item.Index,
				Confirmed: item.Confirmed,
				Reason:    truncateInsightLog(strings.TrimSpace(item.Reason), insightRejectionReasonLimit),
			})
		}
		return out, nil
	}
	// 首轮。
	textB, err := cc(buildVerifyPrompt(project.Path, candidates))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errInsightVerifyRunner, err)
	}
	outcomes, vErr := decode(textB)
	if vErr == nil {
		return outcomes, nil
	}
	log.Printf("[insights] project=%s Pass B parse failed on first attempt: %v", project.ID, vErr)
	// 修正 prompt 重试一次。
	textB, err = cc(buildVerifyRepairPrompt(project.Path, candidates))
	if err != nil {
		return nil, fmt.Errorf("%w（重试）: %v", errInsightVerifyRunner, err)
	}
	outcomes, vErr = decode(textB)
	if vErr != nil {
		return nil, fmt.Errorf("核实代理未返回有效结果: %v (raw len=%d)", vErr, len(textB))
	}
	return outcomes, nil
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
func (s *Server) runInsightVerifyBatches(ctx context.Context, project Project, agentID string, candidates []pendingInsight, progress func(level, message string)) (map[int]insightVerifyOutcome, error) {
	verified := make(map[int]insightVerifyOutcome, len(candidates))
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
			verified[start+item.Index] = item
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
// 即报错）；空 ids → 全部"有效"（未失效且未被「不再提示」）建议。返回空列表表示无建议可验证。
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

// runInsightFindingsVerify 见 runInsightFindingsVerifyRun。
func (s *Server) runInsightFindingsVerify(ctx context.Context, projectID string, targets []InsightFinding) {
	s.runInsightFindingsVerifyRun(ctx, projectID, "", "", targets)
}

// insightVerifyAbortNote 把运行上下文的终止原因转成用户可读文案；未终止返回空串。
func insightVerifyAbortNote(ctx context.Context) string {
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		return "验证已取消"
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "验证超时，未完成部分请重新验证"
	default:
		return ""
	}
}

// runInsightFindingsVerifyRun 对既有建议跑一轮只读复核。
//
// 批次彼此独立发布：每批跑完就立刻在租约内复核版本并落库，某批失败或被中止只影响它
// 自己与尚未开始的后续批次，此前已发布的结论一律保留。原实现是"所有批次先收集、最后
// 一次性发布"，任何一批出错都会把此前已成功的批次一起作废——用户付了费却什么都拿不到。
//
// 版本一致性仍然成立：每批发布前都会确认工作区自本次复核开始起未被修改，因此所有已发布
// 的结论都针对同一个版本；一旦发现变化，立即停止发布剩余批次并把它们标为可重试的失败。
func (s *Server) runInsightFindingsVerifyRun(ctx context.Context, projectID, verificationID, agentID string, targets []InsightFinding) {
	if len(targets) == 0 {
		return
	}
	verifyCtx, cancel := context.WithTimeout(ctx, insightVerifyRunTimeout)
	defer cancel()

	// 已失效建议是 AI 上次确认的终局判定：复验失败/无结论一律保持失效，不复活进有效列表。
	fallbackStatus := func(f InsightFinding) string {
		if f.VerificationResult == insightVerifyInvalid {
			return insightVerifyInvalid
		}
		return insightVerifyFailed
	}
	// persistFailures 把一批建议写回"失败"（已失效的保持失效）。不结束复核运行记录。
	persistFailures := func(batch []InsightFinding, note string) {
		now := time.Now().UTC()
		persistInsightWrite(func(persistCtx context.Context) {
			for _, f := range batch {
				s.setInsightVerificationIfUnchanged(persistCtx, projectID, f, fallbackStatus(f), note, now)
			}
		})
	}
	// failAll 把全部目标置为失败（或取消）并结束复核运行。仅用于批次尚未开始时就能
	// 确定整趟跑不下去的失败（取项目失败、版本快照失败等）。
	failAll := func(note string) {
		status := insightScanFailed
		if errors.Is(verifyCtx.Err(), context.Canceled) {
			status = insightScanCancelled
			note = "验证已取消"
		}
		persistInsightWrite(func(persistCtx context.Context) {
			for _, f := range targets {
				s.setInsightVerificationIfUnchanged(persistCtx, projectID, f, fallbackStatus(f), note, time.Now().UTC())
			}
			s.finishInsightVerificationRun(persistCtx, verificationID, status, note, len(targets))
		})
	}
	// 复核 worker 跑在裸 goroutine 里（net/http 的 panic 兜底覆盖不到），一次 panic 会
	// 带走整个控制服务。这里兜底：记录堆栈、把本轮置为可重试的失败，绝不让进程退出。
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[insights] project=%s re-verify panic: %v\n%s", projectID, r, debug.Stack())
			failAll("验证内部错误，请重试")
		}
	}()
	project, err := s.getProjectByID(verifyCtx, projectID)
	if err != nil {
		failAll("无法加载项目，请重试")
		return
	}
	revision, revisionErr := s.insightWorkspaceRevision(verifyCtx, project)
	if revisionErr != nil {
		if verifyCtx.Err() != nil {
			failAll("读取项目版本状态失败，请重试")
			return
		}
		// 复核同样不因版本快照读不出而整体失败：降级为非 Git 语义继续推进（原因进日志）。
		log.Printf("[insights] project=%s verify: read workspace revision failed, continue without version check: %v", projectID, revisionErr)
	}
	// 非 Git 项目（Available=false）仍可复核：insightWorkspaceUnchanged 对非 Git 恒返回
	// unchanged，复核正常推进，只是没有版本一致性对账（repoSHA 为空，run 记录不写 revision）。
	if revision.Available {
		s.setInsightVerificationRunRevision(verifyCtx, verificationID, revision.RepoSHA)
	}
	agentID = normalizeScanAgent(agentID)
	if agentID == "" {
		agentID = s.currentInsightAgent(verifyCtx, projectID)
	}
	// publishBatch 在租约内复核版本并落库一批结论。返回 false 表示未写入（附原因）。
	publishBatch := func(batch []InsightFinding, results map[string]insightReverifyResult, fallbackNote string) (bool, string) {
		ok, reason := s.publishInsightResult(verifyCtx, project, revision, "insight-verify:"+verificationID,
			func() {
				s.updateInsightVerificationRunMessage(verifyCtx, verificationID, "结果已就绪，正在等待项目空闲以写入…")
			},
			func(publishCtx context.Context) error {
			now := time.Now().UTC()
			for _, f := range batch {
				result, hasResult := results[f.ID]
				if !hasResult {
					s.setInsightVerificationIfUnchanged(publishCtx, projectID, f, fallbackStatus(f), fallbackNote, now)
					continue
				}
				// 已失效建议复验：仅 valid 判定才把它恢复进有效列表；invalid/uncertain 保持
				// 失效，避免一次失败的复验推翻此前 AI 已确认的失效结论。
				if f.VerificationResult == insightVerifyInvalid && result.status != "valid" {
					reason := result.reason
					if result.status == "uncertain" {
						reason = "AI 无法确认：" + reason
					}
					s.setInsightVerificationIfUnchanged(publishCtx, projectID, f, insightVerifyInvalid, reason, now)
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
				s.setInsightVerificationIfUnchanged(publishCtx, projectID, f, dbStatus, result.reason, now)
			}
			return nil
		})
		return ok, reason
	}

	// 按批推进进度：见 insightReverifyChunkSize 说明。进入/完成每批都落库一条消息，
	// 避免整趟 agent（可能数分钟）期间 run 一直停留在"正在准备验证 · 0/N"。
	chunk := insightReverifyChunkSize(len(targets))
	batches := (len(targets) + chunk - 1) / chunk
	processed := 0
	failedBatches := 0
	abortNote := ""
	for start := 0; start < len(targets); start += chunk {
		end := min(start+chunk, len(targets))
		batch := targets[start:end]
		batchNo := start/chunk + 1
		// 单批（小清单）不写"第 1/1 批"这类噪声，只留建议区间/进行中的动作。
		batchPrefix := ""
		if batches > 1 {
			batchPrefix = fmt.Sprintf("第 %d/%d 批：", batchNo, batches)
		}
		if note := insightVerifyAbortNote(verifyCtx); note != "" {
			abortNote = note
			break
		}
		startMsg := fmt.Sprintf("正在核实建议 %d-%d/%d", start+1, end, len(targets))
		if len(targets) == 1 {
			startMsg = "正在核实该条建议"
		}
		s.updateInsightVerificationRun(verifyCtx, verificationID, batchPrefix+startMsg, processed)
		// 早退：工作区已变则后续批次不再开跑，避免继续消耗 agent 调用（已发布的批次保留）。
		if unchanged, revisionErr := s.insightWorkspaceUnchanged(verifyCtx, project, revision); revisionErr != nil || !unchanged {
			abortNote = "项目代码在验证中发生变化，已丢弃本轮结果，请重新验证"
			break
		}
		// 整批返回前进度数不会动（agent 一次只回整批判定），把实时工具动作（读取/
		// 检索哪个文件）滚动写进 run.message，避免长时间盯着 0/N 误以为卡住。
		activity := func(level, message string) {
			if verifyCtx.Err() != nil {
				return
			}
			if batches > 1 {
				message = fmt.Sprintf("第 %d/%d 批：%s", batchNo, batches, message)
			}
			s.updateInsightVerificationRunMessage(verifyCtx, verificationID, message)
		}
		batchResults, batchErr := s.runInsightReverifyBatch(verifyCtx, project, agentID, revision.RepoSHA, batch, activity)
		if batchErr != nil {
			log.Printf("[insights] project=%s re-verify batch %d/%d failed: %v", projectID, batchNo, batches, batchErr)
			// 整趟被取消/超时：中止剩余批次（各批是独立的 agent 调用，继续跑没有意义）。
			if note := insightVerifyAbortNote(verifyCtx); note != "" {
				abortNote = note
				break
			}
			// 本批自身失败（agent 报错 / 输出不合契约）：只标记本批，继续跑后续批次。
			failedBatches++
			persistFailures(batch, insightRunErrorMessage("建议验证失败", batchErr))
			processed = end
			continue
		}
		if published, reason := publishBatch(batch, batchResults, "结果未能写入项目，请重新验证"); !published {
			// 发布失败（已取消 / 超预算 / 租约等不到 / 工作区已变 / 写库失败）：本批结果
			// 未落库，且工作区状态已不可信，中止剩余批次，避免继续花 agent 调用去产出
			// 同样写不进去的结论。reason 由 publishInsightResult 保证非空。
			persistFailures(batch, reason)
			abortNote = reason
			processed = end
			break
		}
		processed = end
		s.updateInsightVerificationRun(verifyCtx, verificationID,
			fmt.Sprintf("%s已完成 %d/%d 条建议", batchPrefix, end, len(targets)), end)
	}

	if abortNote == "" {
		// 正常跑完所有批次：成功批次已逐批落库。
		if failedBatches == 0 {
			persistInsightWrite(func(persistCtx context.Context) {
				s.finishInsightVerificationRun(persistCtx, verificationID, insightScanCompleted, "验证完成", len(targets))
			})
			return
		}
		note := fmt.Sprintf("验证完成：%d/%d 批成功，%d 批失败（失败的建议已标记为可重试）", batches-failedBatches, batches, failedBatches)
		persistInsightWrite(func(persistCtx context.Context) {
			s.finishInsightVerificationRun(persistCtx, verificationID, insightScanFailed, note, len(targets))
		})
		return
	}

	// 中止：剩余未处理的批次统一置为可重试的失败，已发布的批次保持已发布。
	if remaining := targets[processed:]; len(remaining) > 0 {
		persistFailures(remaining, abortNote)
	}
	status := insightScanFailed
	if errors.Is(verifyCtx.Err(), context.Canceled) {
		status = insightScanCancelled
	}
	persistInsightWrite(func(persistCtx context.Context) {
		s.finishInsightVerificationRun(persistCtx, verificationID, status, abortNote, processed)
	})
}

// persistInsightWrite 让扫描/复核 worker 被取消后仍能写入终态。
// Server.Close 会在关闭数据库前等待 insight worker，因此该有界后台上下文可安全使用。
func persistInsightWrite(write func(context.Context)) {
	persistCtx, cancel := context.WithTimeout(context.Background(), insightPersistenceTimeout)
	defer cancel()
	write(persistCtx)
}

func (s *Server) runInsightReverifyBatch(ctx context.Context, project Project, agentID, repoSHA string, targets []InsightFinding, progress func(level, message string)) (map[string]insightReverifyResult, error) {
	cc := func(prompt string) (string, error) {
		return s.runReadOnlyAgentWithSchema(ctx, project, agentID, prompt, insightReverifyOutputSchema, progress)
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
		return
	}
	s.broadcastInsightVerificationRun(ctx, verificationID)
}

// updateInsightVerificationRunMessage 只滚动复核 run 的 message 不动 processed_count，
// 供单趟 agent 运行中实时转发工具动作（读取/检索…）。status='running' 守卫保证 run
// 结束后（比如用户停止）残留的进度回调不会把终态消息冲掉。
func (s *Server) updateInsightVerificationRunMessage(ctx context.Context, verificationID, message string) {
	if verificationID == "" || message == "" {
		return
	}
	if _, err := s.db.ExecContext(ctx, `update project_insight_verification_runs set message=? where id=? and status='running'`, message, verificationID); err != nil {
		log.Printf("[insights] update verification run message %s: %v", verificationID, err)
		return
	}
	s.broadcastInsightVerificationRun(ctx, verificationID)
}

// broadcastInsightVerificationRun 广播复核进度变化，让项目总览卡片的进度文案实时跟上
// （同 appendInsightEvent 的理由：状态来自 /projects/statuses，不广播就靠 30s 兜底）。
func (s *Server) broadcastInsightVerificationRun(ctx context.Context, verificationID string) {
	if verificationID == "" {
		return
	}
	var projectID string
	if err := s.db.QueryRowContext(ctx, `select project_id from project_insight_verification_runs where id=?`, verificationID).Scan(&projectID); err == nil {
		s.broadcastStateEvent(stEvProjects, projectID)
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
		return
	}
	s.broadcastInsightVerificationRun(ctx, verificationID)
}

// acquireWorkspaceWait 在 wait 时间内反复尝试获取工作区租约（见 acquireWorkspace：
// 同一 key 同时只允许一个 owner）。用于「结果已算好、只差落库」的场景——此时不该因为
// 项目正被别的任务占用就把整趟分析的成果丢掉，但也不能无限等，故带上限。
// ctx 取消（用户停止 / 项目删除）立即返回 false。
func (s *Server) acquireWorkspaceWait(ctx context.Context, key, owner string, wait time.Duration) (func(), bool) {
	deadline := time.Now().Add(wait)
	for {
		if release, ok := s.acquireWorkspace(key, owner); ok {
			return release, true
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			return nil, false
		}
		select {
		case <-ctx.Done():
			return nil, false
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// publishInsightResult 在项目工作区租约内发布一次结果：等待租约（上限
// insightPublishLeaseWait，且不超过本次运行的剩余预算）→ 确认工作区自 revision 起
// 未被修改 → 执行 publish 写库。
//
// 这是「只读分析不独占工作区」方案的收口点：分析阶段允许用户继续在项目里工作，
// 代价是工作区一旦变化，本次结果就不再成立。租约在这里只覆盖"最终版本复核 + 写库"
// 这一小段，使二者对所有 Milevia 发起的写入是原子的——不会出现"复核通过后有写入
// 溜进来、结果仍被当成有效"的窗口。
//
// onWait 只在"第一次没抢到租约、确实需要等"时回调一次，供调用方告诉用户"结果已算好，
// 正在等项目空闲以写入"——顺利的情况下不该出现这句话。
//
// 返回 ok=false 表示没能发布，reason 为用户可读原因（已取消 / 预算用尽 / 工作区被占用 /
// 工作区已变化 / 版本状态读不出 / 写库失败）。reason 一定非空，调用方可直接展示。
func (s *Server) publishInsightResult(ctx context.Context, project Project, revision insightWorkspaceRevision, owner string, onWait func(), publish func(context.Context) error) (bool, string) {
	// 先判上下文：已取消/已超预算就没必要再去碰工作区（否则会落到下面那些"看起来
	// 像是工作区问题"的分支，把真实原因盖掉）。
	if errors.Is(ctx.Err(), context.Canceled) {
		return false, "已取消"
	}
	if ctx.Err() != nil {
		return false, "已超出本次运行的时间预算，结果未写入，请重新发起"
	}
	wait := insightPublishLeaseWait
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < wait {
			wait = remaining
		}
	}
	if wait <= 0 {
		return false, "已超出本次运行的时间预算，结果未写入，请重新发起"
	}
	// 先试一次：空闲时直接拿到，不打扰用户。
	release, acquired := s.acquireWorkspace(project.ID, owner)
	if !acquired {
		if onWait != nil {
			onWait()
		}
		release, acquired = s.acquireWorkspaceWait(ctx, project.ID, owner, wait)
	}
	if !acquired {
		if errors.Is(ctx.Err(), context.Canceled) {
			return false, "已取消"
		}
		// 项目工作区被别的任务占用，且剩余时间不够继续等。文案要说清是占用问题，
		// 而不是让用户以为模型分析出了问题。
		return false, "项目工作区正被其他任务占用，未能在剩余时间内写入结果，请稍后重新发起"
	}
	defer release()
	unchanged, revisionErr := s.insightWorkspaceUnchanged(ctx, project, revision)
	if revisionErr != nil {
		// 读不出快照与"工作区变了"是两回事，别混成同一句话。上下文已结束时优先报真实原因。
		if errors.Is(ctx.Err(), context.Canceled) {
			return false, "已取消"
		}
		if ctx.Err() != nil {
			return false, "已超出本次运行的时间预算，结果未写入，请重新发起"
		}
		log.Printf("[insights] project=%s publish: read workspace revision failed: %v", project.ID, revisionErr)
		return false, "无法读取项目版本状态，本次结果未写入，请重新发起"
	}
	if !unchanged {
		return false, "项目代码在处理过程中发生变化，已丢弃本轮结果，请重新发起"
	}
	if err := publish(ctx); err != nil {
		log.Printf("[insights] project=%s publish: %v", project.ID, err)
		return false, "写入分析结果失败，请重试"
	}
	return true, ""
}

func (s *Server) resolveVerifyTargets(ctx context.Context, projectID string, ids []string) ([]InsightFinding, error) {
	if len(ids) == 0 {
		rows, err := s.db.QueryContext(ctx, `select `+insightFindingColumns+` from project_insights
			where project_id=? and coalesce(verification_result,'')<>'invalid' and coalesce(status,?)<>?
			order by created_at asc`, projectID, insightStatusOpen, insightStatusDismissed)
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
	// 重复写/重复出现在 agent prompt 里。「已忽略」在此与无 ids 分支同样跳过：调用方
	// 不该因为传了 id 就能复核一条用户已明确表示不再提示的建议。已失效的仍允许核对
	// （那是"重新验证"的正当用法——复核判 valid 才会把它恢复进有效列表）。
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
		if f.Status == insightStatusDismissed {
			continue
		}
		out = append(out, f)
	}
	return out, nil
}

// verifyInsightFindings POST /api/projects/{projectID}/insights/verify
// 对既有建议发起新一轮 AI 核验。可选 body {"agent":"codex","findingIds":[...]}，缺省 = 全部有效建议。
// 异步执行：先把目标置 pending 并返回 202，后台跑只读 agent 后逐条写回
// valid/invalid/failed。单项目互斥：扫描/验证进行中返回 409。
func (s *Server) verifyInsightFindings(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("projectID")
	if !s.projectExists(r.Context(), projectID) {
		http.NotFound(w, r)
		return
	}
	var req verifyInsightsRequest
	if !decodeOptional(w, r, &req) {
		return
	}
	agentID := normalizeScanAgent(req.Agent)
	if agentID == "" {
		agentID = s.currentInsightAgent(r.Context(), projectID)
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

	// 准入检查（不是长期占用）：复核全程只读、不独占工作区，用户可以在复核期间继续用
	// 项目；但若此刻项目正被别的任务改写，结论几乎必然在落库时被判作废，所以此时直接
	// 让用户稍后再来。检查后立即释放：真正的互斥只发生在每批"版本复核 + 写库"那一小段
	// （见 publishInsightResult）。
	releaseAdmission, acquired := s.acquireProjectWorkspace(projectID, "insight-verify:"+verificationID)
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
	releaseAdmission()
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

	// 让项目总览卡片的进度文案立刻亮起，而不是等 30s 兜底轮询。
	s.broadcastStateEvent(stEvProjects, projectID)

	// 后台执行。Done 在 goroutine 内调用，使 insightWG 精确跟踪验证生命周期，
	// 供 Close() 等待（与 triggerInsightScan 同语义）。runCtx 已在登记 active 时创建：
	// 项目被删除/用户停止时取消它，避免 agent 继续跑完无用功。
	go func() {
		defer s.insightWG.Done()
		defer runCancel()
		defer func() {
			s.insightMu.Lock()
			delete(s.insightActive, projectID)
			delete(s.insightCancels, projectID)
			s.insightMu.Unlock()
		}()
		// 兜底：worker 跑在裸 goroutine 里，panic 若逃逸会带走整个控制服务。
		// runInsightFindingsVerifyRun 内部已自兜底（并写下终态），这里防的是它的外层。
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[insights] project=%s verify worker panic: %v\n%s", projectID, r, debug.Stack())
			}
		}()
		s.runInsightFindingsVerifyRun(runCtx, projectID, verificationID, agentID, pendingTargets)
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

// insightSurfacedLine 组装一行喂给发现 agent 的「历史已报告」条目。带上说明（截断）
// 才能让模型识别"换个措辞的同一问题"；只给标题时它很容易把同一件事换个说法再报一次。
func insightSurfacedLine(title, summary string) string {
	title = strings.TrimSpace(title)
	summary = truncateInsightLog(strings.TrimSpace(summary), insightHistorySummaryLimit)
	if summary == "" {
		return title
	}
	return title + " —— " + summary
}

// insightRejectionSummary 把被剔除候选压成一行短文案（最多 limit 条），供进度事件展示。
func insightRejectionSummary(rejected []InsightRejection, limit int) string {
	parts := make([]string, 0, min(limit, len(rejected)))
	for i, r := range rejected {
		if i >= limit {
			break
		}
		if r.Reason == "" {
			parts = append(parts, r.Title)
			continue
		}
		parts = append(parts, r.Title+"（"+r.Reason+"）")
	}
	if len(rejected) > limit {
		parts = append(parts, fmt.Sprintf("…另 %d 条", len(rejected)-limit))
	}
	return strings.Join(parts, "；")
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

// insightRevisionFailureReason 把读取工作区版本快照失败的底层错误压成一段简短可读的原因，
// 用于写日志与进度事件附注；取不到可读原因时返回空串，调用方应省略附注。
func insightRevisionFailureReason(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, errGitOutputTooLarge):
		return "Git 输出超过大小限制（工作区改动或未跟踪文件过多）"
	case errors.Is(err, context.DeadlineExceeded):
		return "Git 命令执行超时"
	}
	var commandErr *gitCommandError
	if errors.As(err, &commandErr) {
		if detail := strings.TrimSpace(commandErr.stderr); detail != "" {
			return "Git " + commandErr.command + "：" + truncateInsightLog(redactAgentText(detail), 160)
		}
		return "Git " + commandErr.command + " 执行失败"
	}
	// 其它错误（git 可执行文件缺失、路径不可访问等）取最里层的系统错误描述。
	leaf := err
	for {
		next := errors.Unwrap(leaf)
		if next == nil {
			break
		}
		leaf = next
	}
	text := strings.TrimSpace(redactAgentText(leaf.Error()))
	if text == "" {
		return ""
	}
	return truncateInsightLog(text, 160)
}

// currentInsightAgent 返回项目当前会话的 agentId（决定扫描/再验证用 claude
// 还是 codex）；无当前会话或缺省值时回退 claude-code。它不能依赖浏览器窗口的活跃 Tab。
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
	// 扫描 worker 跑在裸 goroutine 里（net/http 的 panic 兜底覆盖不到），一次 panic 会带走
	// 整个控制服务（所有项目）。这里兜底：记录堆栈 + 把本次扫描置为可重试的失败。
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[insights] project=%s scan panic: %v\n%s", projectID, r, debug.Stack())
			markFailed("分析内部错误，请重试")
		}
	}()

	project, err := s.getProjectByID(ctx, projectID)
	if err != nil {
		markFailed("无法加载项目，请重试")
		return
	}

	// 新客户端会明确指定 Agent；旧客户端未指定时沿用当前会话的兼容行为。
	agentID := normalizeScanAgent(opts.Agent)
	if agentID == "" {
		agentID = s.currentInsightAgent(ctx, projectID)
	}

	emit("info", "开始优化建议分析…")

	// 历史已报告指纹 + 清单（规则 1）：供 prompt 提示与落库去重。
	// 指纹集合来自全部历史行（含用户点了「不再提示」的，见 insightStatusDismissed）；
	// 喂给模型的清单只取最近 insightHistoryPromptLimit 条，避免 prompt 随历史无限膨胀。
	// 「已失效」行不参与：它们允许被重新发现（见落库处的复活语义）。
	prevRows, err := s.db.QueryContext(ctx, `select title,summary from project_insights
		where project_id=? and (coalesce(verification_result,'')<>'invalid' or status=?)
		order by created_at desc`, projectID, insightStatusDismissed)
	if err != nil {
		markFailed("读取历史建议失败，请重试")
		return
	}
	var alreadySurfaced []string
	promptListFull := false
	seenFP := map[string]struct{}{}
	for prevRows.Next() {
		var t, sm string
		if err := prevRows.Scan(&t, &sm); err != nil {
			prevRows.Close()
			markFailed("读取历史建议失败，请重试")
			return
		}
		seenFP[insightFingerprint(t, sm)] = struct{}{}
		if len(alreadySurfaced) < insightHistoryPromptLimit {
			alreadySurfaced = append(alreadySurfaced, insightSurfacedLine(t, sm))
		} else {
			promptListFull = true
		}
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
	// 「永不再报告」的指纹（用户编辑建议后遗留的旧指纹，见 project_insight_suppressions）：
	// 它们没有对应的建议行可查，只能从这张表补齐，否则编辑前的原文会被再报一次。
	suppRows, err := s.db.QueryContext(ctx, `select fingerprint from project_insight_suppressions where project_id=?`, projectID)
	if err != nil {
		markFailed("读取历史建议失败，请重试")
		return
	}
	for suppRows.Next() {
		var fingerprint string
		if err := suppRows.Scan(&fingerprint); err != nil {
			suppRows.Close()
			markFailed("读取历史建议失败，请重试")
			return
		}
		if fingerprint != "" {
			seenFP[fingerprint] = struct{}{}
		}
	}
	if err := suppRows.Err(); err != nil {
		suppRows.Close()
		markFailed("读取历史建议失败，请重试")
		return
	}
	suppRows.Close()
	if promptListFull {
		emit("info", fmt.Sprintf("历史建议较多，已只把最近 %d 条交给分析器（去重仍覆盖全部历史）", insightHistoryPromptLimit))
	}

	workspaceRevision, revisionErr := s.insightWorkspaceRevision(ctx, project)
	if revisionErr != nil {
		if ctx.Err() != nil {
			markFailed("读取项目版本状态失败，请重新发起分析")
			return
		}
		// Git 版本快照读不出来（命令超时 / 输出超限 / Git 报错）时不阻断分析：与非 Git
		// 项目同一路径继续，只是失去跨阶段版本对账。底层原因写日志并随事件可见，避免
		// 用户再遇到「读取项目版本状态失败」时无从排查。
		log.Printf("[insights] project=%s read workspace revision failed, continue without version check: %v", projectID, revisionErr)
		if reason := insightRevisionFailureReason(revisionErr); reason != "" {
			emit("warn", "无法读取 Git 版本状态，本次分析将跳过版本一致性校验："+reason)
		} else {
			emit("warn", "无法读取 Git 版本状态，本次分析将跳过版本一致性校验")
		}
	} else if !workspaceRevision.Available {
		// 非 Git 项目（Available=false）仍可分析：版本一致性检查在 insightWorkspaceUnchanged
		// 中对非 Git 恒返回 unchanged，分析可正常推进，只是不做跨阶段版本对账（repoSHA 为空）。
		emit("info", "当前项目不是 Git 仓库，分析将跳过版本一致性校验")
	}
	repoSHA := workspaceRevision.RepoSHA

	// Pass A：发现。模型常在通读项目时过度叙述，把最终 JSON 数组留到回合末尾，可能在
	// 输出前就 end_turn——此时 transcript 里只有散文 + 随手写的字符串数组残片，
	// parseInsightCandidates 必然失败（这正是「分析代理未返回有效结果」的真因）。
	// 补救：用一条强约束的"只输出 JSON"修正 prompt 重试一次；仍失败才判 scan failed。
	emit("info", "第 1 轮：通读项目代码，收集候选发现…")
	textA, err := s.runReadOnlyAgentWithSchema(ctx, project, agentID, buildInsightScanPrompt(project.Path, repoSHA, alreadySurfaced, opts), insightCandidatesOutputSchema, emit)
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
		textA, err = s.runReadOnlyAgentWithSchema(ctx, project, agentID, buildInsightRepairPromptWithHistory(project.Path, repoSHA, alreadySurfaced, opts), insightCandidatesOutputSchema, emit)
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

	verified := map[int]insightVerifyOutcome{}
	var rejected []InsightRejection
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
		verified = verdict
		// 被核实剔除的候选连同 AI 给出的依据一并记下：规则 2（必须核实）唯一的可审计
		// 来源，用户需要看到"哪几条被剔除、为什么"，否则无从发现模型误判。
		for idx, c := range candidates {
			outcome, ok := verified[idx+1]
			if !ok || outcome.Confirmed {
				continue
			}
			rejected = append(rejected, InsightRejection{
				Title:  truncateInsightRunes(c.Title, 60),
				Reason: truncateInsightRunes(strings.TrimSpace(outcome.Reason), insightRejectionReasonLimit),
			})
		}
		confirmed := 0
		for _, outcome := range verified {
			if outcome.Confirmed {
				confirmed++
			}
		}
		emit("success", fmt.Sprintf("核实完成，确认 %d 条有效", confirmed))
		if len(rejected) > 0 {
			summary := fmt.Sprintf("核实剔除 %d 条未通过的候选：%s", len(rejected), insightRejectionSummary(rejected, 5))
			emit("warn", summary)
		}
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
		if outcome, ok := verified[idx+1]; ok && !outcome.Confirmed {
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
	log.Printf("[insights] project=%s Post-B candidates=%d accepted=%d suppressed=%d rejected=%d", projectID, len(candidates), len(accepted), suppressed, len(rejected))

	// 落库。只读分析阶段并不持有工作区租约（用户可以在分析期间继续用项目），因此这里在
	// 租约内先复核"工作区自扫描开始起未被修改"，再写库——publishInsightResult 负责等待
	// 租约、复核版本、执行写入。工作区已变或租约等不到时，本次结果作废并明确告知用户。
	rejectedJSON, err := json.Marshal(rejected)
	if err != nil {
		log.Printf("[insights] project=%s marshal rejections: %v", projectID, err)
		rejectedJSON = []byte("[]")
	}
	inserted := 0
	published, publishReason := s.publishInsightResult(ctx, project, workspaceRevision, "insight-publish:"+scanID,
		func() { emit("info", "分析结果已就绪，正在等待项目空闲以写入…") },
		func(publishCtx context.Context) error {
			tx, err := s.db.BeginTx(publishCtx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback() //nolint:errcheck
			inserted = 0
			for idx := range accepted {
				f := &accepted[idx]
				f.ID = uuid.NewString()
				f.ProjectID = projectID
				f.ScanID = scanID
				f.CreatedAt = now
				// 复活语义：已失效（verification_result='invalid'）的建议允许被重新发现、
				// 重新进入有效列表（问题可能确实没修好）。但用户显式点了「不再提示」
				// （status='dismissed'）的不复活——那是用户的决定，不是 AI 的判定。
				res, err := tx.ExecContext(publishCtx, `insert into project_insights
					(id,project_id,scan_id,type,severity,title,summary,file_hint,fingerprint,status,created_at,updated_at)
					values (?,?,?,?,?,?,?,?,?,?,?,?)
					on conflict(project_id,fingerprint) do update set
						scan_id=excluded.scan_id,type=excluded.type,severity=excluded.severity,title=excluded.title,
						summary=excluded.summary,file_hint=excluded.file_hint,status=excluded.status,
						verification_result='',verification_note='',verified_at=null,updated_at=excluded.updated_at
					where project_insights.verification_result='invalid' and project_insights.status<>?`,
					f.ID, f.ProjectID, f.ScanID, f.Type, f.Severity, f.Title, f.Summary, f.FileHint, f.fingerprint, insightStatusOpen, now, now, insightStatusDismissed)
				if err != nil {
					return err
				}
				if n, _ := res.RowsAffected(); n == 1 {
					inserted++
				} else {
					suppressed++
				}
			}
			// completed_at 取当前时刻而非上面的 now：等待租约可能耗掉一段时间（上限
			// insightPublishLeaseWait），用户看到的"完成时间"应该是真正写完的时间。
			if _, err := tx.ExecContext(publishCtx, `update project_insight_scans
				set status=?,agent=?,theme=?,focus_types=?,findings_count=?,suppressed_count=?,completed_at=?,repo_sha=?,rejected_json=? where id=?`,
				insightScanCompleted, agentID, opts.Theme, strings.Join(opts.Types, ","), inserted, suppressed, time.Now().UTC(), repoSHA, string(rejectedJSON), scanID); err != nil {
				return err
			}
			return tx.Commit()
		})
	if !published {
		// publishReason 一定非空且已区分取消/超预算/工作区占用/工作区变化/写库失败，
		// markFailed 会在上下文被取消时按"已取消"收尾。
		markFailed(publishReason)
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
	if opts.Agent == "" {
		// 兼容未传 agent 的旧客户端，同时让扫描记录保存实际使用的 Agent。
		opts.Agent = s.currentInsightAgent(r.Context(), projectID)
	}
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
	// no-op。总预算 insightScanRunTimeout 从这一刻开始计，保证扫描不会无限期跑下去
	// （单趟上限是"每趟"的，没有总预算时多趟 + 重试会累计成小时级）。
	runCtx, runCancel := context.WithTimeout(s.runtimeCtx, insightScanRunTimeout)
	s.insightActive[projectID] = true
	s.insightCancels[projectID] = runCancel
	s.insightWG.Add(1)
	s.insightMu.Unlock()

	// 准入检查（不是长期占用）：分析全程只读、不独占工作区，用户可以在分析期间继续
	// 用项目；但若此刻项目正被别的任务改写，分析出的结论几乎必然在落库时被判作废，
	// 因此此时直接告诉用户稍后再来，而不是让他白等一场。检查后立即释放，真正的互斥
	// 只发生在"最终版本复核 + 写库"那一小段（见 publishInsightResult）。
	releaseAdmission, acquired := s.acquireProjectWorkspace(projectID, "insight-scan:"+scanID)
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
	releaseAdmission()
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(r.Context(), `insert into project_insight_scans
		(id,project_id,status,agent,theme,focus_types,findings_count,suppressed_count,created_at,started_at)
		values (?,?,?,?,?,?,0,0,?,?)`,
		scanID, projectID, insightScanRunning, opts.Agent, opts.Theme, strings.Join(opts.Types, ","), now, now); err != nil {
		runCancel()
		s.insightMu.Lock()
		delete(s.insightActive, projectID)
		delete(s.insightCancels, projectID)
		s.insightMu.Unlock()
		s.insightWG.Done()
		writeError(w, http.StatusInternalServerError, errors.New("启动分析失败，请重试"))
		return
	}

	scan := InsightScan{ID: scanID, ProjectID: projectID, Status: insightScanRunning, Agent: opts.Agent, Theme: opts.Theme, FocusTypes: opts.Types, FindingsCount: 0, SuppressedCount: 0, CreatedAt: now}
	writeJSON(w, http.StatusAccepted, scan)

	// 让项目总览卡片的「优化建议分析中」徽标立刻亮起，而不是等 30s 兜底轮询。
	s.broadcastStateEvent(stEvProjects, projectID)

	// 后台执行扫描。Done 在扫描 goroutine 内调用，使 insightWG 精确跟踪扫描生命周期，
	// 供 Close() 等待（而非在 HTTP handler 返回时就 Done，那会让 Close 立即通过）。
	// runCtx 已在登记 active 时创建：项目被删除/用户停止时取消它，避免 agent
	// 继续跑完剩余阶段做无用功。
	go func() {
		defer s.insightWG.Done()
		defer runCancel()
		defer func() {
			s.insightMu.Lock()
			delete(s.insightActive, projectID)
			delete(s.insightCancels, projectID)
			s.insightMu.Unlock()
		}()
		// 兜底：worker 跑在裸 goroutine 里，panic 若逃逸会带走整个控制服务。
		// runProjectInsightScan 内部已自兜底（并写下扫描终态），这里防的是它的外层。
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[insights] project=%s scan worker panic: %v\n%s", projectID, r, debug.Stack())
			}
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

	resp := insightsResponse{DefaultAgent: s.currentInsightAgent(r.Context(), projectID), Findings: []InsightFinding{}, Events: []InsightEvent{}}
	resp.Verification = s.loadRunningInsightVerification(r.Context(), projectID)
	row := s.db.QueryRowContext(r.Context(), `select id,project_id,status,error,agent,theme,focus_types,findings_count,suppressed_count,created_at,started_at,completed_at,coalesce(rejected_json,'')
		from project_insight_scans where project_id=? order by created_at desc limit 1`, projectID)
	var scan InsightScan
	var errMsg sql.NullString
	var themeStr, focusStr, rejectedJSON sql.NullString
	var startedAt, completedAt sql.NullTime
	err := row.Scan(&scan.ID, &scan.ProjectID, &scan.Status, &errMsg, &scan.Agent, &themeStr, &focusStr, &scan.FindingsCount, &scan.SuppressedCount, &scan.CreatedAt, &startedAt, &completedAt, &rejectedJSON)
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
		// 被核实剔除的候选（含 AI 给出的依据）：规则 2 的可审计性来源，只在完成态才有内容。
		if raw := strings.TrimSpace(rejectedJSON.String); raw != "" && raw != "[]" {
			var rejected []InsightRejection
			if json.Unmarshal([]byte(raw), &rejected) == nil {
				scan.Rejected = rejected
			}
		}
		if startedAt.Valid {
			scan.StartedAt = &startedAt.Time
		}
		if completedAt.Valid {
			scan.CompletedAt = &completedAt.Time
		}
		resp.Scan = &scan
		resp.HasScan = true
		resp.SuppressedCount = scan.SuppressedCount
		// 进度事件只在扫描进行中才有意义（前端也只在 running 时渲染日志）。扫描结束后
		// 不再随每次 GET 把整份日志重传一遍。
		//
		// 轮询可带 sinceScan + sinceSeq 只取增量。**必须带上 sinceScan**：客户端手上的
		// 游标可能属于上一次扫描，而新扫描的 seq 从 1 重新开始，若只按 seq 过滤会把新扫描
		// 开头的事件永久漏掉（它们的 seq 小于旧游标）。id 不匹配就当作首次拉取，返回全量。
		if scan.Status == insightScanRunning {
			sinceSeq := 0
			if r.URL.Query().Get("sinceScan") == scan.ID {
				if parsed, convErr := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("sinceSeq"))); convErr == nil && parsed > 0 {
					sinceSeq = parsed
				}
			}
			resp.Events = s.loadInsightEvents(r.Context(), scan.ID, sinceSeq)
		}

		// 规则 1：发现跨扫描去重累积。展示的是本项目"当前仍有效的建议全集"
		//（union 而非只看最新一次扫描），否则"仅命中重复的新扫描"会让既有建议从视图消失。
		// 经验证已失效（verification_result='invalid'）的建议不在此列，单独折叠返回；
		// 用户点了「不再提示」（status='dismissed'）的同样不进有效列表，另作折叠。
		// 排序按严重度优先、同级取较新者，配合 insightFindingsListLimit 的截断语义：
		// 触顶时丢掉的是"较旧的次要建议"，而不是任意条目。
		rows, err := s.db.QueryContext(r.Context(), `select `+insightFindingColumns+` from project_insights
			where project_id=? and coalesce(verification_result,'')<>'invalid' and coalesce(status,?)<>? order by
			case severity when 'high' then 0 when 'normal' then 1 else 2 end, created_at desc limit ?`,
			projectID, insightStatusOpen, insightStatusDismissed, insightFindingsListLimit+1)
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
		resp.FindingsLimit = insightFindingsListLimit
		if len(resp.Findings) > insightFindingsListLimit {
			resp.Findings = resp.Findings[:insightFindingsListLimit]
			resp.Truncated = true
		}

		// 已失效建议（验证判定不再成立）：按最近验证时间倒序返回，供前端折叠展示原因。
		invRows, err := s.db.QueryContext(r.Context(), `select `+insightFindingColumns+` from project_insights
			where project_id=? and coalesce(verification_result,'')='invalid' and coalesce(status,?)<>?
			order by coalesce(verified_at,created_at) desc limit ?`,
			projectID, insightStatusOpen, insightStatusDismissed, insightFindingsListLimit)
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

		// 用户点了「不再提示」的建议：折叠展示，可恢复（恢复后重新进入有效列表）。
		disRows, err := s.db.QueryContext(r.Context(), `select `+insightFindingColumns+` from project_insights
			where project_id=? and coalesce(status,?)=? order by created_at desc limit ?`,
			projectID, insightStatusOpen, insightStatusDismissed, insightFindingsListLimit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, errors.New("读取分析结果失败，请重试"))
			return
		}
		for disRows.Next() {
			if f, err := scanInsightFinding(disRows.Scan); err == nil {
				resp.Dismissed = append(resp.Dismissed, f)
			}
		}
		if err := disRows.Err(); err != nil {
			disRows.Close()
			writeError(w, http.StatusInternalServerError, errors.New("读取分析结果失败，请重试"))
			return
		}
		disRows.Close()

		// 建议已转成任务的状态标注：让卡片能显示"已转为任务 · 进行中"，并阻止为同一
		// 问题重复建任务（见 convertInsightToTask 的重复防护）。
		linked := s.loadInsightLinkedTasks(r.Context(), projectID)
		for i := range resp.Findings {
			if task, ok := linked[insightFingerprint(resp.Findings[i].Title, resp.Findings[i].Summary)]; ok {
				resp.Findings[i].LinkedTaskStatus = task.Status
				resp.Findings[i].LinkedTaskTitle = task.Title
			}
		}
		resp.OpenCount = len(resp.Findings)
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.OpenCount = len(resp.Findings)
	writeJSON(w, http.StatusOK, resp)
}

// insightLinkedTask 是建议指纹到任务状态的投影（用于卡片标注与重复建任务防护）。
type insightLinkedTask struct {
	Status string
	Title  string
}

// loadInsightLinkedTasks 返回本项目"由优化建议转成的任务"的指纹 → 任务状态映射。
// 同一指纹可能有多个历史任务（完成后又转了一次），这里取**最新**的那个：转换在
// "同指纹已有未完成任务"时被拒绝（见 convertInsightToTask），因此同一指纹最多只会
// 有一个未完成任务，且它必然是最新的——取最新即等价于"要看的那个任务"。
func (s *Server) loadInsightLinkedTasks(ctx context.Context, projectID string) map[string]insightLinkedTask {
	rows, err := s.db.QueryContext(ctx, `select source_insight_fingerprint,status,title from tasks
		where project_id=? and coalesce(source_insight_fingerprint,'')<>'' order by created_at desc`, projectID)
	if err != nil {
		log.Printf("[insights] project=%s load linked tasks: %v", projectID, err)
		return nil
	}
	defer rows.Close()
	out := map[string]insightLinkedTask{}
	for rows.Next() {
		var fingerprint, status, title string
		if err := rows.Scan(&fingerprint, &status, &title); err != nil {
			continue
		}
		if _, seen := out[fingerprint]; seen {
			continue // DESC 顺序：先到的是最新的，后续更旧的一律忽略
		}
		out[fingerprint] = insightLinkedTask{Status: status, Title: title}
	}
	return out
}

// insightUpdateRequest PATCH /insights/{findingID} 的可选 body：只允许改
// title/summary/severity/type 这四个展示字段（fingerprint 由 title+summary 派生）。
type insightUpdateRequest struct {
	Title    *string `json:"title"`
	Summary  *string `json:"summary"`
	Severity *string `json:"severity"`
	Type     *string `json:"type"`
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
	if input.Title == nil && input.Summary == nil && input.Severity == nil && input.Type == nil {
		writeError(w, http.StatusBadRequest, errors.New("请至少提供 title/summary/severity/type 之一"))
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
	// 类型可改：模型把"bug"报成"optimization"时，用户应能就地纠正，而不是删掉重报
	// （删掉重报会让同一问题在下次扫描里以原类型再次出现）。非法值归一为 optimization。
	itemType := f.Type
	if input.Type != nil {
		itemType = normalizeInsightTypeOrDefault(strings.TrimSpace(*input.Type))
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
	// 顺带把用户「不再提示」的旧指纹记进 superseded 指纹表：否则下次扫描会把编辑前的
	// 原文当成一条新建议再报一次，与编辑后的卡片并存。
	oldFP := insightFingerprint(f.Title, f.Summary)
	if _, err := tx.ExecContext(r.Context(), `update project_insights set title=?,summary=?,severity=?,type=?,fingerprint=?,verification_result='',verification_note='',verified_at=NULL,updated_at=? where id=? and project_id=?`,
		title, summary, sev, itemType, newFP, now, findingID, projectID); err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("保存失败，请重试"))
		return
	}
	if oldFP != "" && oldFP != newFP {
		if _, err := tx.ExecContext(r.Context(), `insert into project_insight_suppressions (project_id,fingerprint,reason,created_at) values (?,?,?,?)
			on conflict(project_id,fingerprint) do nothing`, projectID, oldFP, insightSuppressionSuperseded, now); err != nil {
			writeError(w, http.StatusInternalServerError, errors.New("保存失败，请重试"))
			return
		}
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

// normalizeInsightTypeOrDefault 把任意字符串归一到四类枚举之一（非法值回退 optimization）。
func normalizeInsightTypeOrDefault(value string) string {
	for _, known := range insightTypeOrder {
		if value == known {
			return value
		}
	}
	return insightOptimization
}

// setInsightDismissed POST /api/projects/{projectID}/insights/{findingID}/dismiss
// 把一条建议标为「不再提示」：从有效列表移入折叠区，且后续扫描不再上报它。
// 与"删除"的区别：删除是硬删（下次扫描会重新报告），不再提示是用户对该问题的明确
// 处置（保留记录、可恢复、不再打扰）。
func (s *Server) setInsightDismissed(w http.ResponseWriter, r *http.Request) {
	s.updateInsightDismissed(w, r, insightStatusDismissed)
}

// clearInsightDismissed DELETE /api/projects/{projectID}/insights/{findingID}/dismiss
// 恢复一条被「不再提示」的建议，让它重新回到有效列表（并再次参与复核/转任务）。
func (s *Server) clearInsightDismissed(w http.ResponseWriter, r *http.Request) {
	s.updateInsightDismissed(w, r, insightStatusOpen)
}

func (s *Server) updateInsightDismissed(w http.ResponseWriter, r *http.Request, status string) {
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
	if _, err := s.db.ExecContext(r.Context(), `update project_insights set status=? where id=? and project_id=?`, status, findingID, projectID); err != nil {
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
//
// 与之配套的重复防护（不是去重的替代）：任务本身记下来源指纹
// （tasks.source_insight_fingerprint），同一指纹已存在"未完成任务"时拒绝再次转换——
// 问题仍可被重新发现、仍可再次处置（等该任务到终态即可），但不会为同一个问题反复
// 堆出多个任务。删除/编辑建议不受影响。

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
	// 与 UI 一致：建议复核中、已被判失效、或已被用户「不再提示」时不允许转任务。复核未出
	// 结果前转任务可能为不存在的问题建任务；已失效则是为已修复/伪问题建任务；已忽略则是
	// 用户已明确表示不需要处理，转任务违背其意图。
	if f.VerificationResult == insightVerifyPending {
		writeError(w, http.StatusConflict, errors.New("该建议正在复核中，请稍后再试"))
		return
	}
	if f.VerificationResult == insightVerifyInvalid {
		writeError(w, http.StatusConflict, errors.New("该建议已失效，请刷新后重试"))
		return
	}
	if f.Status == insightStatusDismissed {
		writeError(w, http.StatusConflict, errors.New("该建议已设为不再提示，请先恢复后再转任务"))
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
	// 并发防重：另一请求（双标签页/重复点击）已先转走并硬删该建议，或同一问题已有未完成
	// 任务 → 区分文案，让用户知道是"已被处理"还是"已有一个未完成任务"。
	if !converted {
		if linked, ok := s.loadInsightLinkedTasks(r.Context(), projectID)[insightFingerprint(f.Title, f.Summary)]; ok && !insightTaskTerminal(linked.Status) {
			writeError(w, http.StatusConflict, errors.New("该问题已转为任务且尚未完成（「"+linked.Title+"」），请先在任务板处理"))
			return
		}
		writeError(w, http.StatusConflict, errors.New("该建议已被处理，请刷新后重试"))
		return
	}
	writeJSON(w, http.StatusCreated, task)
}

// convertInsightToTask 在同一事务里把一条建议转成任务并硬删建议，返回 (task, converted, err)：
//   - converted=true：任务已创建、建议已删除。
//   - converted=false, err=nil：建议已不存在、被并发处理（含复核中/已失效/已忽略突变），或
//     同一问题已存在未完成任务，未创建任务，调用方按跳过处理。
//   - err != nil：真实失败（已包装用户可读信息）。
func (s *Server) convertInsightToTask(ctx context.Context, projectID string, f InsightFinding) (Task, bool, error) {
	now := time.Now().UTC()
	fingerprint := insightFingerprint(f.Title, f.Summary)
	task := Task{
		ID:          uuid.NewString(),
		ProjectID:   projectID,
		Title:       truncateInsightRunes(f.Title, 120),
		Description: truncateInsightRunes(buildInsightTaskDescription(f), 12000),
		Priority:    insightTaskPriority(f.Severity),
		Position:    0,
		Status:      taskTodo,
		CreatedAt:   now,
		UpdatedAt:   now,
		DependsOn:   []TaskDependency{},
		BlockedBy:   []TaskBlocker{},
		Blocks:      []TaskDependency{},
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
	// 重复防护：同一建议指纹已有"未完成任务"时不再新建。问题的再次发现完全不受影响
	// （扫描仍会重新报告它），只是不再为同一个问题反复堆任务；等该任务进入终态后即可
	// 再次转换。这里在事务内查询，与并发转换串行。
	var openLinked int
	if err := tx.QueryRowContext(ctx, `select count(*) from tasks where project_id=? and source_insight_fingerprint=?
		and status not in (?,?)`, projectID, fingerprint, taskDone, taskCancelled).Scan(&openLinked); err != nil {
		return Task{}, false, errors.New("创建任务失败，请重试")
	}
	if openLinked > 0 {
		return Task{}, false, nil
	}
	if err := tx.QueryRowContext(ctx, `select coalesce(max(position),0)+1 from tasks where project_id=?`, projectID).Scan(&task.Position); err != nil {
		return Task{}, false, errors.New("创建任务失败，请重试")
	}
	if _, err := tx.ExecContext(ctx, `insert into tasks (id,project_id,title,description,priority,pinned,position,status,created_at,updated_at,source_insight_fingerprint) values (?,?,?,?,?,?,?,?,?,?,?)`,
		task.ID, task.ProjectID, task.Title, task.Description, task.Priority, false, task.Position, task.Status, task.CreatedAt, task.UpdatedAt, fingerprint); err != nil {
		return Task{}, false, errors.New("创建任务失败，请重试")
	}
	if err := s.recordTaskEventTx(ctx, tx, task.ID, "", "task.created", map[string]string{"status": task.Status}, now); err != nil {
		return Task{}, false, errors.New("创建任务失败，请重试")
	}
	// 建议已转为任务：从列表删除（硬删）。
	// 【规则 · 勿改】硬删 = 下次扫描会重新报告同一问题（指纹消失）。这是有意的：
	// 任务未真正修复前问题仍可能存在，必须允许再被发现。勿改为软删/已处理/仅隐藏。
	// WHERE 追加防护：批量转换时复核可能刚把该建议置为 pending 或判为 invalid、用户
	// 也可能刚点了「不再提示」，跳过避免为未定论/已失效/已忽略的问题建任务。
	res, err := tx.ExecContext(ctx, `delete from project_insights where id=? and project_id=?
		and coalesce(verification_result,'')<>'pending' and coalesce(verification_result,'')<>'invalid'
		and coalesce(status,?)<>?`, f.ID, projectID, insightStatusOpen, insightStatusDismissed)
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

// resolveToTaskTargets 解析批量转任务的目标建议：缺省 = 全部未失效且未被「不再提示」的建议
// （已失效/已忽略单独折叠展示，复核中保留 —— 由 convertInsightToTask 的 pending 删除防护在
// 事务内判定为"跳过"并计数）。显式 ids 逐条校验归属并跳过已失效/已忽略/不存在的；不存在的
// id 忽略。批量是尽力而为，一条不该阻塞其余。
func (s *Server) resolveToTaskTargets(ctx context.Context, projectID string, ids []string) ([]InsightFinding, error) {
	if len(ids) == 0 {
		rows, err := s.db.QueryContext(ctx, `select `+insightFindingColumns+` from project_insights
			where project_id=? and coalesce(verification_result,'')<>'invalid' and coalesce(status,?)<>?
			order by created_at asc`, projectID, insightStatusOpen, insightStatusDismissed)
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
		if f.VerificationResult == insightVerifyInvalid || f.Status == insightStatusDismissed {
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
