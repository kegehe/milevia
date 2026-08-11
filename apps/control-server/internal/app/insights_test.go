package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// insightScriptRunner 注入到 server.runner / server.codexRunner，按调用次序逐步输出
// 罐装文本（Pass A 发现 → Pass B 核实），并记录每次收到的 AgentRunRequest 供断言。
type insightScriptRunner struct {
	mu       sync.Mutex
	calls    int
	outputs  []string
	requests []AgentRunRequest
}

func (r *insightScriptRunner) Ready(context.Context) bool { return true }
func (r *insightScriptRunner) Version(context.Context) string {
	return "1.0"
}
func (r *insightScriptRunner) CheckUpdate(context.Context) (bool, string, error) {
	return false, "", nil
}
func (r *insightScriptRunner) Update(context.Context) (string, string, error) {
	return "", "", nil
}

// Run 模拟 agent：把下一个罐装输出喂给 sink.AssistantText，并记录请求。
func (r *insightScriptRunner) Run(_ context.Context, req AgentRunRequest, sink AgentRunSink) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, req)
	if r.calls < len(r.outputs) && r.outputs[r.calls] != "" {
		sink.AssistantText(r.outputs[r.calls], "")
	}
	r.calls++
	return nil
}

// capturedRequests 返回已记录请求副本（并发安全）。
func (r *insightScriptRunner) capturedRequests() []AgentRunRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]AgentRunRequest, len(r.requests))
	copy(out, r.requests)
	return out
}

func insightTestProject(t *testing.T, server *Server) string {
	t.Helper()
	projectID := "insight-project"
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values (?,?,?,'windows','main',1,?)`, projectID, projectID, t.TempDir(), time.Now().UTC().Format("2006-01-02 15:04:05")); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	return projectID
}

// insightQueryString 从响应体解码成目标类型的通用辅助。
func insightDecode[T any](t *testing.T, recorder *httptest.ResponseRecorder, wantStatus int) T {
	t.Helper()
	if recorder.Code != wantStatus {
		t.Fatalf("status: got %d want %d (body %s)", recorder.Code, wantStatus, recorder.Body.String())
	}
	var out T
	if err := json.Unmarshal(recorder.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode body: %v (%s)", err, recorder.Body.String())
	}
	return out
}

// insertSyncInsightScan 直接插入一条 running 扫描行并同步执行 runProjectInsightScan，
// 避免经 HTTP 触发时的后台 goroutine 与同步调用竞态（测试用脚本化 runner 需确定性）。
// 返回 scanID。
func insertSyncInsightScan(t *testing.T, server *Server, projectID string) string {
	t.Helper()
	return insertSyncInsightScanOpts(t, server, projectID, scanOpts{})
}

// insertSyncInsightScanOpts 同上，但携带指定扫描方向 opts。
func insertSyncInsightScanOpts(t *testing.T, server *Server, projectID string, opts scanOpts) string {
	t.Helper()
	scanID := "scan-" + projectID + fmt.Sprintf("-%d", time.Now().UnixNano())
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	if _, err := server.db.Exec(`insert into project_insight_scans (id,project_id,status,theme,focus_types,created_at,started_at) values (?,?,'running',?,?,?,?)`, scanID, projectID, opts.Theme, strings.Join(opts.Types, ","), now, now); err != nil {
		t.Fatalf("insert running scan: %v", err)
	}
	server.runProjectInsightScan(context.Background(), projectID, scanID, opts)
	return scanID
}

func TestParseInsightCandidates(t *testing.T) {
	// 裸数组形态。
	items, err := parseInsightCandidates(`[{"type":"bug","title":"a","summary":"b"}]`)
	if err != nil {
		t.Fatalf("parse array: %v", err)
	}
	if len(items) != 1 || items[0].Type != "bug" {
		t.Errorf("array parse: %+v", items)
	}
	// {findings:[...]} 包裹形态。
	items, err = parseInsightCandidates(`{"findings":[{"type":"feature","title":"c","summary":"d"}]}`)
	if err != nil {
		t.Fatalf("parse object: %v", err)
	}
	if len(items) != 1 || items[0].Type != "feature" {
		t.Errorf("object parse: %+v", items)
	}
	// 坏输入。
	if _, err := parseInsightCandidates(``); err == nil {
		t.Error("expected error for empty input")
	}
	if _, err := parseInsightCandidates(`not json`); err == nil {
		t.Error("expected error for non-json input")
	}
	// 真实 agent 常带 Markdown 代码围栏与前缀散文——必须能解析。
	fenced := "好的，这是分析结果：\n```json\n[{\"type\":\"bug\",\"severity\":\"high\",\"title\":\"围栏\",\"summary\":\"说明\"}]\n```\n希望对你有帮助。"
	items, err = parseInsightCandidates(fenced)
	if err != nil {
		t.Fatalf("parse fenced+prose: %v\nraw=%q", err, fenced)
	}
	if len(items) != 1 || items[0].Type != "bug" || items[0].Title != "围栏" {
		t.Errorf("fenced parse: %+v", items)
	}
	// 字符串内容里含括号不应干扰配对。
	nested := `{"findings":[{"type":"feature","title":"含括号(x)与}引号","summary":"a{b"}]}`
	items, err = parseInsightCandidates(nested)
	if err != nil {
		t.Fatalf("nested-brace parse: %v", err)
	}
	if len(items) != 1 || items[0].Title != "含括号(x)与}引号" {
		t.Errorf("nested parse: %+v", items)
	}
	// 真实 Claude 会先叙述"我要分析…"（可能引用示例 JSON），最后才给最终答案——取最后一个合法 JSON。
	narration := "我将分析项目。输出结构形如 [{\"type\":\"bug\",\"title\":\"示例\"}]。\n这是最终发现：\n[{\"type\":\"optimization\",\"severity\":\"normal\",\"title\":\"最终建议\",\"summary\":\"s\"}]"
	items, err = parseInsightCandidates(narration)
	if err != nil {
		t.Fatalf("narration+final parse: %v\nraw=%q", err, narration)
	}
	if len(items) != 1 || items[0].Type != "optimization" || items[0].Title != "最终建议" {
		t.Errorf("narration parse picked wrong json: %+v", items)
	} else {
		t.Log("narration parse ok")
	}
}

func TestTriggerInsightScanRejectsConcurrent(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	server.runner = &insightScriptRunner{
		outputs: []string{`[]`, `{"findings":[]}`},
	}

	// 模拟已有 running 扫描行（DB 兜底）+ 内存占用。
	if _, err := server.db.Exec(`insert into project_insight_scans (id,project_id,status,created_at,started_at) values (?,?,'running',?,?)`, "existing", projectID, time.Now().UTC().Format("2006-01-02 15:04:05"), time.Now().UTC().Format("2006-01-02 15:04:05")); err != nil {
		t.Fatalf("insert running scan: %v", err)
	}
	server.insightMu.Lock()
	server.insightActive[projectID] = true
	server.insightMu.Unlock()

	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/insights/scan", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d want %d", rec.Code, http.StatusConflict)
	}
}

func TestTriggerInsightScanAccepts(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	server.runner = &insightScriptRunner{outputs: []string{`[]`, `{"findings":[]}`}}

	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/insights/scan", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status: got %d want %d (body %s)", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	var scan InsightScan
	if err := json.Unmarshal(rec.Body.Bytes(), &scan); err != nil {
		t.Fatalf("decode scan: %v", err)
	}
	if scan.Status != insightScanRunning {
		t.Fatalf("scan status: got %q want %q", scan.Status, insightScanRunning)
	}
}

func TestRunInsightScanPersistsConfirmedFindings(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	passA := `[{"type":"bug","severity":"high","title":"列表加载很慢","summary":"列表页打开要等好几秒","fileHint":"src/List.tsx"},{"type":"feature","severity":"normal","title":"可加导出按钮","summary":"在列表页加一个导出功能","fileHint":""}]`
	passB := `{"findings":[{"index":1,"confirmed":true,"reason":""},{"index":2,"confirmed":true,"reason":""}]}`
	stub := &insightScriptRunner{outputs: []string{passA, passB}}
	server.runner = stub

	scanID := insertSyncInsightScan(t, server, projectID)

	// 断言两趟权限模式与项目路径。
	reqs := stub.capturedRequests()
	if len(reqs) != 2 {
		t.Fatalf("expected 2 agent runs, got %d", len(reqs))
	}
	if reqs[0].PermissionMode != "plan" || reqs[0].ProjectPath == "" {
		t.Errorf("pass A policy/path: %q / %q", reqs[0].PermissionMode, reqs[0].ProjectPath)
	}
	if reqs[1].PermissionMode != "plan" {
		t.Errorf("pass B policy: %q", reqs[1].PermissionMode)
	}

	// 断言落库 + 计数。
	var count, suppressed, status string
	if err := server.db.QueryRow(`select status,findings_count,suppressed_count from project_insight_scans where id=?`, scanID).Scan(&status, &count, &suppressed); err != nil {
		t.Fatalf("scan row: %v", err)
	}
	if status != insightScanCompleted {
		t.Fatalf("scan status: %q", status)
	}
	if count != "2" || suppressed != "0" {
		t.Errorf("count/suppressed: got %s/%s want 2/0", count, suppressed)
	}
	var fingerprints int
	if err := server.db.QueryRow(`select count(*) from project_insights where scan_id=?`, scanID).Scan(&fingerprints); err != nil {
		t.Fatalf("findings count: %v", err)
	}
	if fingerprints != 2 {
		t.Errorf("persisted findings: got %d want 2", fingerprints)
	}
}

func TestRunInsightScanBadJSONFails(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	server.runner = &insightScriptRunner{outputs: []string{`not json`, ``}}
	scanID := insertSyncInsightScan(t, server, projectID)

	var status string
	var n int
	if err := server.db.QueryRow(`select status from project_insight_scans where id=?`, scanID).Scan(&status); err != nil {
		t.Fatalf("scan row: %v", err)
	}
	if err := server.db.QueryRow(`select count(*) from project_insights where scan_id=?`, scanID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if status != insightScanFailed || n != 0 {
		t.Errorf("after bad JSON: status=%q findings=%d want %q/0", status, n, insightScanFailed)
	}
}

func TestUnconfirmedFindingsDoNotPersist(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	passA := `[{"type":"bug","severity":"high","title":"假 bug","summary":"并不存在"}]`
	passB := `{"findings":[{"index":1,"confirmed":false,"reason":"项目里没有该问题"}]}`
	server.runner = &insightScriptRunner{outputs: []string{passA, passB}}
	scanID := insertSyncInsightScan(t, server, projectID)

	var n int
	if err := server.db.QueryRow(`select count(*) from project_insights where scan_id=?`, scanID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("unconfirmed findings persisted: got %d want 0", n)
	}
}

func TestRule1DedupByFingerprint(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	passA1 := `[{"type":"bug","severity":"high","title":"列表加载很慢","summary":"列表页打开很慢","fileHint":""}]`
	passA2 := `[{"type":"bug","severity":"high","title":"列表加载很慢","summary":"列表页打开很慢","fileHint":""}]`
	passB := `{"findings":[{"index":1,"confirmed":true,"reason":""}]}`

	// 第一批。
	server.runner = &insightScriptRunner{outputs: []string{passA1, passB}}
	scan1 := insertSyncInsightScan(t, server, projectID)

	// 第二批：同指纹 → 应被规则 1 跳过。
	server.runner = &insightScriptRunner{outputs: []string{passA2, passB}}
	scan2 := insertSyncInsightScan(t, server, projectID)

	// 全库指纹唯一（规则 1 兜底）。
	var distinct, total int
	if err := server.db.QueryRow(`select count(distinct fingerprint) from project_insights where project_id=?`, projectID).Scan(&distinct); err != nil {
		t.Fatalf("distinct: %v", err)
	}
	if err := server.db.QueryRow(`select count(*) from project_insights where project_id=?`, projectID).Scan(&total); err != nil {
		t.Fatalf("total: %v", err)
	}
	if distinct != 1 || total != 1 {
		t.Errorf("after dedup: distinct=%d total=%d want 1/1", distinct, total)
	}
	// 第二次扫描的 suppressed_count 应为 1。
	var suppressed int
	if err := server.db.QueryRow(`select suppressed_count from project_insight_scans where id=?`, scan2).Scan(&suppressed); err != nil {
		t.Fatalf("suppressed: %v", err)
	}
	if suppressed != 1 {
		t.Errorf("second scan suppressed: got %d want 1", suppressed)
	}
	// 归一化：大小写/空白差异应归同一指纹。
	if insightFingerprint(" 列表加载很慢 ", " 列表页打开很慢 ") != insightFingerprint("列表加载很慢", "列表页打开很慢") {
		t.Error("fingerprint normalization should be case/whitespace insensitive")
	}
	_ = scan1
}

func TestListInsightsReturnsScanAndFindings(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	passA := `[{"type":"bug","severity":"high","title":"高严重度","summary":"s"},{"type":"feature","severity":"low","title":"低严重度","summary":"t"}]`
	passB := `{"findings":[{"index":1,"confirmed":true},{"index":2,"confirmed":true}]}`
	server.runner = &insightScriptRunner{outputs: []string{passA, passB}}
	_ = insertSyncInsightScan(t, server, projectID)

	recList := httptest.NewRecorder()
	server.routes().ServeHTTP(recList, httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/insights", nil))
	resp := insightDecode[insightsResponse](t, recList, http.StatusOK)
	if !resp.HasScan || resp.Scan == nil || resp.Scan.Status != insightScanCompleted {
		t.Fatalf("list scan: %+v", resp.Scan)
	}
	if len(resp.Findings) != 2 {
		t.Fatalf("findings: got %d want 2", len(resp.Findings))
	}
	// severity 排序：high 在 low 前。
	if resp.Findings[0].Severity != insightSeverityHigh || resp.Findings[1].Severity != insightSeverityLow {
		t.Errorf("findings not sorted by severity: %s, %s", resp.Findings[0].Severity, resp.Findings[1].Severity)
	}
}

func TestInsightFingerprintNormalization(t *testing.T) {
	a := insightFingerprint("列表加载很慢", "列表页打开很慢")
	b := insightFingerprint("  列表加载很慢  ", "列表页打开很慢")
	c := insightFingerprint("列表加载很慢", "列表页打开很慢。")
	// 末尾句号被标点归移除。
	if a != b || a != c {
		t.Errorf("fingerprint should be whitespace/punct-insensitive: %q %q %q", a, b, c)
	}
	if a == "" {
		t.Error("fingerprint should not be empty")
	}
}

func TestProjectRuntimeProfileNilWithoutDefault(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	project, err := server.getProjectByID(context.Background(), projectID)
	if err != nil {
		t.Fatalf("load project: %v", err)
	}
	profile, err := server.projectRuntimeProfile(context.Background(), project, "claude-code")
	if err != nil {
		t.Fatalf("runtime profile: %v", err)
	}
	if profile != nil {
		t.Errorf("expected nil profile, got %+v", profile)
	}
}

func TestInsightFindingsCap(t *testing.T) {
	// 验证数量钳制：超过 50 条时截断到 50。
	var items []string
	for i := 0; i < 60; i++ {
		items = append(items, fmt.Sprintf(`{"type":"feature","severity":"normal","title":"功能 %d","summary":"描述 %d"}`, i, i))
	}
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	passA := "[" + strings.Join(items, ",") + "]"
	// Pass B 全确认。
	var confirm []string
	for i := 0; i < 60; i++ {
		confirm = append(confirm, fmt.Sprintf(`{"index":%d,"confirmed":true}`, i+1))
	}
	passB := `{"findings":[` + strings.Join(confirm, ",") + `]}`
	server.runner = &insightScriptRunner{outputs: []string{passA, passB}}
	scanID := insertSyncInsightScan(t, server, projectID)

	var n int
	if err := server.db.QueryRow(`select count(*) from project_insights where scan_id=?`, scanID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n > insightFindingsCap {
		t.Errorf("findings exceeded cap: got %d want <=%d", n, insightFindingsCap)
	}
}

func TestTriggerInsightScanPersistsThemeAndFocusTypes(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	server.runner = &insightScriptRunner{outputs: []string{`[]`, `{"findings":[]}`}}

	body := strings.NewReader(`{"theme":"security","types":["bug","style"]}`)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/insights/scan", body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status: got %d want %d (body %s)", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	var scan InsightScan
	if err := json.Unmarshal(rec.Body.Bytes(), &scan); err != nil {
		t.Fatalf("decode scan: %v", err)
	}
	if scan.Theme != "security" || len(scan.FocusTypes) != 2 || scan.FocusTypes[0] != "bug" || scan.FocusTypes[1] != "style" {
		t.Fatalf("response theme/focus: %q %v", scan.Theme, scan.FocusTypes)
	}
	// DB 行也应持久化（逗号串）。
	var themeStr, focusStr string
	if err := server.db.QueryRow(`select theme,focus_types from project_insight_scans where id=?`, scan.ID).Scan(&themeStr, &focusStr); err != nil {
		t.Fatalf("scan row: %v", err)
	}
	if themeStr != "security" || focusStr != "bug,style" {
		t.Errorf("persisted theme/focus: %q/%q want security/bug,style", themeStr, focusStr)
	}
}

func TestTriggerInsightScanBodyOptionalDefaultsToAll(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	server.runner = &insightScriptRunner{outputs: []string{`[]`, `{"findings":[]}`}}
	// 无 body → 等价于全量（theme=''、focus_types=''）。
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/insights/scan", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status: got %d want %d", rec.Code, http.StatusAccepted)
	}
	var scan InsightScan
	_ = json.Unmarshal(rec.Body.Bytes(), &scan)
	var focusStr string
	if err := server.db.QueryRow(`select focus_types from project_insight_scans where id=?`, scan.ID).Scan(&focusStr); err != nil {
		t.Fatalf("scan row: %v", err)
	}
	if focusStr != "" {
		t.Errorf("no-body focus_types: got %q want empty", focusStr)
	}
}

func TestTriggerInsightScanNormalizesInvalidDirection(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	server.runner = &insightScriptRunner{outputs: []string{`[]`, `{"findings":[]}`}}
	// 非法 theme 回退 ''；非法 type 丢弃；全选归全查（focus_types=''）。
	body := strings.NewReader(`{"theme":"bogus","types":["bug","notAType","style","optimization","feature"]}`)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/insights/scan", body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status: got %d want %d (body %s)", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	var scan InsightScan
	_ = json.Unmarshal(rec.Body.Bytes(), &scan)
	if scan.Theme != "" || len(scan.FocusTypes) != 0 {
		t.Errorf("normalize: theme=%q focus=%v want ''/nil", scan.Theme, scan.FocusTypes)
	}
}

func TestBuildInsightScanPromptHonorsDirection(t *testing.T) {
	opts := scanOpts{Theme: "security", Types: []string{"bug", "style"}}
	p := buildInsightScanPrompt("/x", "abc", nil, opts)
	if !strings.Contains(p, "聚焦主题：安全") || !strings.Contains(p, "从「安全」的视角") {
		t.Errorf("prompt should mention security theme, got:\n%s", p)
	}
	if !strings.Contains(p, "bug —— 有 bug") || !strings.Contains(p, "style —— 样式问题") {
		t.Errorf("prompt should list bug & style types")
	}
	if strings.Contains(p, "feature —— 可新增功能") {
		t.Errorf("prompt should NOT list feature type when not selected")
	}
	// 全查（opts.Types==nil）保持四类都在。
	all := buildInsightScanPrompt("/x", "abc", nil, scanOpts{})
	for _, ty := range insightTypeOrder {
		if !strings.Contains(all, ty+" —— "+insightTypeLabels[ty]) {
			t.Errorf("all-types prompt missing %s", ty)
		}
	}
}

func TestNormalizeScanTypes(t *testing.T) {
	if got := normalizeScanTypes([]string{"bug"}); len(got) != 1 || got[0] != "bug" {
		t.Errorf("basic: %v", got)
	}
	// 去重 + 丢弃非法。
	if got := normalizeScanTypes([]string{"bug", "bug", "nope", "style"}); len(got) != 2 {
		t.Errorf("dedupe/filter: %v", got)
	}
	// 全选 → nil（全查）。
	if got := normalizeScanTypes([]string{"bug", "style", "optimization", "feature"}); got != nil {
		t.Errorf("full selection should be nil: %v", got)
	}
	if got := normalizeScanTypes(nil); got != nil {
		t.Errorf("nil should stay nil: %v", got)
	}
}

// firstInsightFindingID 取项目下第一条 finding 的 id（测试落库后断言用）。
func firstInsightFindingID(t *testing.T, server *Server, projectID string) string {
	t.Helper()
	var id string
	if err := server.db.QueryRow(`select id from project_insights where project_id=? limit 1`, projectID).Scan(&id); err != nil {
		t.Fatalf("load finding id: %v", err)
	}
	return id
}

func TestPassBUncoveredTreatedAsConfirmed(t *testing.T) {
	// Pass B 只返回 index=1 的判定（confirmed=true），漏掉 index=2 ——
	// 被漏掉的候选应按"确认"处理并落库，而非静默丢弃。
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	passA := `[{"type":"bug","severity":"high","title":"已核实","summary":"s1"},{"type":"feature","severity":"normal","title":"未覆盖","summary":"s2"}]`
	passB := `{"findings":[{"index":1,"confirmed":true,"reason":""}]}`
	server.runner = &insightScriptRunner{outputs: []string{passA, passB}}
	scanID := insertSyncInsightScan(t, server, projectID)

	var n int
	if err := server.db.QueryRow(`select count(*) from project_insights where scan_id=?`, scanID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("Pass B uncovered should be treated as confirmed: got %d want 2", n)
	}
}

func TestDeleteInsightFindingHardDeletes(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	passA := `[{"type":"bug","severity":"high","title":"待删除","summary":"s"}]`
	passB := `{"findings":[{"index":1,"confirmed":true}]}`
	server.runner = &insightScriptRunner{outputs: []string{passA, passB}}
	scanID := insertSyncInsightScan(t, server, projectID)

	findingID := firstInsightFindingID(t, server, projectID)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/projects/"+projectID+"/insights/"+findingID, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status: got %d want %d", rec.Code, http.StatusNoContent)
	}
	// 行已不在库。
	var n int
	if err := server.db.QueryRow(`select count(*) from project_insights where id=?`, findingID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("finding should be hard-deleted: got %d want 0", n)
	}
	// listInsights 不再返回它。
	rec2 := httptest.NewRecorder()
	server.routes().ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/insights", nil))
	resp := insightDecode[insightsResponse](t, rec2, http.StatusOK)
	if len(resp.Findings) != 0 {
		t.Errorf("list after delete: got %d findings want 0", len(resp.Findings))
	}
	_ = scanID
}

func TestDeleteInsightFindingNotFound(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/projects/"+projectID+"/insights/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("delete unknown finding: got %d want %d", rec.Code, http.StatusNotFound)
	}
}

func TestUpdateInsightFindingRewritesFingerprint(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	passA := `[{"type":"bug","severity":"high","title":"原标题","summary":"原说明"}]`
	passB := `{"findings":[{"index":1,"confirmed":true}]}`
	server.runner = &insightScriptRunner{outputs: []string{passA, passB}}
	_ = insertSyncInsightScan(t, server, projectID)

	findingID := firstInsightFindingID(t, server, projectID)
	body := strings.NewReader(`{"title":"新标题","summary":"新说明","severity":"low"}`)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/api/projects/"+projectID+"/insights/"+findingID, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("update status: got %d want %d (body %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	var updated InsightFinding
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if updated.Title != "新标题" || updated.Summary != "新说明" || updated.Severity != "low" {
		t.Errorf("updated fields: %+v", updated)
	}
	// 指纹已重算。
	var fp string
	if err := server.db.QueryRow(`select fingerprint from project_insights where id=?`, findingID).Scan(&fp); err != nil {
		t.Fatalf("load fp: %v", err)
	}
	if fp != insightFingerprint("新标题", "新说明") {
		t.Errorf("fingerprint not rewritten: got %q", fp)
	}
}

func TestUpdateInsightFindingConflictOnDuplicateFingerprint(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	// 两条不同 finding。
	passA := `[{"type":"bug","severity":"high","title":"A","summary":"sa"},{"type":"bug","severity":"normal","title":"B","summary":"sb"}]`
	passB := `{"findings":[{"index":1,"confirmed":true},{"index":2,"confirmed":true}]}`
	server.runner = &insightScriptRunner{outputs: []string{passA, passB}}
	_ = insertSyncInsightScan(t, server, projectID)

	// 取第二条，把它的标题/说明改成与第一条完全相同 → 撞指纹。
	// 同一扫描内两条 created_at 相同，用 rowid 保证稳定顺序，避免 flaky。
	var firstID, secondID string
	rows, err := server.db.Query(`select id from project_insights where project_id=? order by rowid asc`, projectID)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	for rows.Next() {
		if firstID == "" {
			rows.Scan(&firstID)
		} else {
			rows.Scan(&secondID)
		}
	}
	rows.Close()
	body := strings.NewReader(`{"title":"A","summary":"sa"}`)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/api/projects/"+projectID+"/insights/"+secondID, body))
	if rec.Code != http.StatusConflict {
		t.Errorf("duplicate fingerprint should 409: got %d want %d (body %s)", rec.Code, http.StatusConflict, rec.Body.String())
	}
}

func TestUpdateInsightFindingNotFound(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	body := strings.NewReader(`{"title":"x"}`)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/api/projects/"+projectID+"/insights/nope", body))
	if rec.Code != http.StatusNotFound {
		t.Errorf("update unknown finding: got %d want %d", rec.Code, http.StatusNotFound)
	}
}

func TestListInsightsOpenCount(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	passA := `[{"type":"bug","severity":"high","title":"a","summary":"s"},{"type":"feature","severity":"low","title":"b","summary":"t"}]`
	passB := `{"findings":[{"index":1,"confirmed":true},{"index":2,"confirmed":true}]}`
	server.runner = &insightScriptRunner{outputs: []string{passA, passB}}
	_ = insertSyncInsightScan(t, server, projectID)

	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/insights", nil))
	resp := insightDecode[insightsResponse](t, rec, http.StatusOK)
	if resp.OpenCount != len(resp.Findings) {
		t.Errorf("openCount: got %d want %d", resp.OpenCount, len(resp.Findings))
	}
	if resp.OpenCount != 2 {
		t.Errorf("openCount value: got %d want 2", resp.OpenCount)
	}
}
