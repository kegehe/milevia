package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
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

// blockingInsightRunner 阻塞在 Run 直到 ctx 取消，模拟真实 agent 长时间运行中，
// 供端到端取消测试使用（HTTP 触发扫描 → HTTP 取消 → worker 收到 ctx 取消 → 置 cancelled）。
type blockingInsightRunner struct{}

func (r *blockingInsightRunner) Ready(context.Context) bool     { return true }
func (r *blockingInsightRunner) Version(context.Context) string { return "" }
func (r *blockingInsightRunner) CheckUpdate(context.Context) (bool, string, error) {
	return false, "", nil
}
func (r *blockingInsightRunner) Update(context.Context) (string, string, error) {
	return "", "", nil
}
func (r *blockingInsightRunner) Run(ctx context.Context, _ AgentRunRequest, _ AgentRunSink) error {
	<-ctx.Done()
	return ctx.Err()
}

func insightTestProject(t *testing.T, server *Server) string {
	t.Helper()
	return insightTestProjectID(t, server, "insight-project")
}

// insightTestProjectID 与 insightTestProject 相同，但使用指定项目 id（供多项目测试）。
func insightTestProjectID(t *testing.T, server *Server, projectID string) string {
	t.Helper()
	projectPath := t.TempDir()
	runner := newGitRunner()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "insights-test@example.invalid"},
		{"config", "user.name", "Insights Test"},
		{"commit", "--allow-empty", "-m", "initial"},
	} {
		if _, err := runner.runGit(context.Background(), projectPath, args...); err != nil {
			t.Fatalf("initialize insight test repository (%s): %v", strings.Join(args, " "), err)
		}
	}
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values (?,?,?,'windows','main',1,?)`, projectID, projectID, projectPath, time.Now().UTC().Format("2006-01-02 15:04:05")); err != nil {
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

func TestTriggerInsightScanRejectsOccupiedWorkspace(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	release, acquired := server.acquireProjectWorkspace(projectID, "run:active")
	if !acquired {
		t.Fatal("acquire test workspace lease")
	}
	defer release()

	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/insights/scan", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d want %d (body %s)", rec.Code, http.StatusConflict, rec.Body.String())
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
	var fileHint string
	if err := server.db.QueryRow(`select file_hint from project_insights where scan_id=? and title=?`, scanID, "列表加载很慢").Scan(&fileHint); err != nil {
		t.Fatalf("load file hint: %v", err)
	}
	if fileHint != "src/List.tsx" {
		t.Errorf("file hint: got %q want src/List.tsx", fileHint)
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

// Pass A 首轮输出非法（模型 end_turn 未给 JSON 数组，见运行期真因）时，应自动用
// 修正 prompt 重试一次；重试返回合法数组则该次扫描照常完成，而非直接判 failed。
func TestRunInsightScanRetriesBadPassA(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	// 输出序：Pass A 首轮（非法）→ Pass A 重试（合法）→ Pass B（核实）。
	badA := `我通读了项目，针对各个子系统做了分析：
1. **列表加载慢** —— 建议优化分页，具体见源码。
["这是","随手写的","字符串数组","不满足契约"]
以上就是我发现的问题，我稍后再整理成正式结果。`
	goodA := `[{"type":"optimization","severity":"normal","title":"列表分页","summary":"列表页数据量大时可加分页避免卡顿","fileHint":"src/List.tsx"}]`
	passB := `{"findings":[{"index":1,"confirmed":true,"reason":""}]}`
	stub := &insightScriptRunner{outputs: []string{badA, goodA, passB}}
	server.runner = stub

	scanID := insertSyncInsightScan(t, server, projectID)

	var status string
	if err := server.db.QueryRow(`select status from project_insight_scans where id=?`, scanID).Scan(&status); err != nil {
		t.Fatalf("scan row: %v", err)
	}
	if status != insightScanCompleted {
		t.Fatalf("scan status: got %q want %q", status, insightScanCompleted)
	}
	// 应触发 3 趟 agent 运行：Pass A 首轮 + Pass A 重试 + Pass B。
	reqs := stub.capturedRequests()
	if len(reqs) != 3 {
		t.Fatalf("agent runs: got %d want 3 (A, A-retry, B)", len(reqs))
	}
	// 首轮与重试用不同 prompt（重试必须走修正 prompt）。
	if reqs[0].Prompt == reqs[1].Prompt {
		t.Errorf("retry prompt should differ from first Pass A prompt")
	}
	// 重试 prompt 必须含强约束表白（"必须是且仅是一个 JSON 数组"）。
	if !strings.Contains(reqs[1].Prompt, "JSON 数组") {
		t.Errorf("repair prompt missing strict JSON-array instruction")
	}
	// 产物应落库。
	var n int
	if err := server.db.QueryRow(`select count(*) from project_insights where scan_id=?`, scanID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("persisted findings: got %d want 1", n)
	}
}

// 首轮与重试都输出非法 JSON 时，重试不应无限循环，最终仍判 failed。
func TestRunInsightScanBadJSONFailsAfterRetry(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	server.runner = &insightScriptRunner{outputs: []string{`not json`, `still not json`, ``}}
	scanID := insertSyncInsightScan(t, server, projectID)

	var status string
	if err := server.db.QueryRow(`select status from project_insight_scans where id=?`, scanID).Scan(&status); err != nil {
		t.Fatalf("scan row: %v", err)
	}
	if status != insightScanFailed {
		t.Fatalf("scan status: got %q want %q", status, insightScanFailed)
	}
	if got := stubCalls(server.runner); got != 2 {
		t.Errorf("agent runs: got %d want 2 (two Pass A attempts, no Pass B)", got)
	}
}

// stubCalls 返回 *insightScriptRunner 已消费的输出个数（同步辅助）。
func stubCalls(runner AgentRunner) int {
	sr, ok := runner.(*insightScriptRunner)
	if !ok {
		return -1
	}
	sr.mu.Lock()
	defer sr.mu.Unlock()
	return sr.calls
}

// Pass B 核实输出非法（模型再次没给 JSON）时，应同样用修正 prompt 重试一次。
func TestRunInsightScanRetriesBadPassB(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	passA := `[{"type":"bug","severity":"high","title":"悬浮球重挂","summary":"取消截图后悬浮球又冒出来","fileHint":"src-tauri/src/commands.rs"}]`
	badB := `我逐条核对了候选：
1. 确实在 cleanup_screenshot_session_inner 里无条件 orb.show()，属实。
以上就是我的核实结论，整理后再给正式输出。`
	goodB := `{"findings":[{"index":1,"confirmed":true,"reason":""}]}`
	stub := &insightScriptRunner{outputs: []string{passA, badB, goodB}}
	server.runner = stub

	scanID := insertSyncInsightScan(t, server, projectID)

	var status string
	if err := server.db.QueryRow(`select status from project_insight_scans where id=?`, scanID).Scan(&status); err != nil {
		t.Fatalf("scan row: %v", err)
	}
	if status != insightScanCompleted {
		t.Fatalf("scan status: got %q want %q", status, insightScanCompleted)
	}
	reqs := stub.capturedRequests()
	if len(reqs) != 3 {
		t.Fatalf("agent runs: got %d want 3 (A, B, B-retry)", len(reqs))
	}
	// B 首轮与重试用不同 prompt（重试必须走修正 prompt）。
	if reqs[1].Prompt == reqs[2].Prompt {
		t.Errorf("verify retry prompt should differ from first verify prompt")
	}
	if !strings.Contains(reqs[2].Prompt, "JSON 对象") {
		t.Errorf("verify repair prompt missing strict JSON-object instruction")
	}
	if !strings.Contains(reqs[2].Prompt, "悬浮球重挂") || !strings.Contains(reqs[2].Prompt, "src-tauri/src/commands.rs") {
		t.Errorf("verify repair prompt must repeat full candidate details: %q", reqs[2].Prompt)
	}
	// 产物应落库。
	var n int
	if err := server.db.QueryRow(`select count(*) from project_insights where scan_id=?`, scanID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("persisted findings: got %d want 1", n)
	}
}

// Pass B 首轮与重试都失败时，整个扫描最终判 failed，且不落任何发现。
func TestRunInsightScanBadJSONFailsAfterVerifyRetry(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	passA := `[{"type":"bug","severity":"high","title":"真问题","summary":"存在"}]`
	stub := &insightScriptRunner{outputs: []string{passA, `no json`, `still no json`}}
	server.runner = stub

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
		t.Errorf("after failed verify retry: status=%q findings=%d want %q/0", status, n, insightScanFailed)
	}
	if got := stubCalls(server.runner); got != 3 {
		t.Errorf("agent runs: got %d want 3 (A, B, B-retry)", got)
	}
}

// Pass B 的 agent 进程本身运行失败（非解析失败）时，不应重试、不应误报"未返回有效结果"，
// 而应提示"建议核实失败"（区分环境失败与输出失败两类错误）。
func TestRunInsightScanVerifyRunnerErrorFailsWithSuggestionMessage(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	passA := `[{"type":"bug","severity":"high","title":"真问题","summary":"存在"}]`
	calls := 0
	server.runner = runnerFunc(func(_ context.Context, _ AgentRunRequest, sink AgentRunSink) error {
		calls++
		if calls == 1 {
			sink.AssistantText(passA, "") // Pass A 首轮成功
			return nil
		}
		return fmt.Errorf("claude process crashed") // Pass B runner 运行失败
	})

	scanID := insertSyncInsightScan(t, server, projectID)

	var status, errMsg string
	if err := server.db.QueryRow(`select status,error from project_insight_scans where id=?`, scanID).Scan(&status, &errMsg); err != nil {
		t.Fatalf("scan row: %v", err)
	}
	if status != insightScanFailed {
		t.Fatalf("scan status: got %q want %q", status, insightScanFailed)
	}
	if !strings.Contains(errMsg, "建议核实失败") {
		t.Errorf("error should read as verify-runner failure, got %q", errMsg)
	}
	// runner 失败不应触发 B 重试：只应跑 1 次 A + 1 次 B。
	if calls != 2 {
		t.Errorf("agent runs: got %d want 2 (A, B-first); B must not retry on runner error", calls)
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
	// 验证数量钳制：超过上限时截断，并让每个 Pass B 批次使用局部索引。
	var items []string
	for i := 0; i < 60; i++ {
		items = append(items, fmt.Sprintf(`{"type":"feature","severity":"normal","title":"功能 %d","summary":"描述 %d"}`, i, i))
	}
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	passA := "[" + strings.Join(items, ",") + "]"
	outputs := []string{passA}
	for start := 0; start < insightFindingsCap; start += insightVerifyBatchSize {
		end := min(start+insightVerifyBatchSize, insightFindingsCap)
		var confirm []string
		for i := start; i < end; i++ {
			confirm = append(confirm, fmt.Sprintf(`{"index":%d,"confirmed":true}`, i-start+1))
		}
		outputs = append(outputs, `{"findings":[`+strings.Join(confirm, ",")+`]}`)
	}
	server.runner = &insightScriptRunner{outputs: outputs}
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

func TestPassBMissingFindingFailsScan(t *testing.T) {
	// Pass B 漏掉候选时，不得把未核实建议当作已确认落库。
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	passA := `[{"type":"bug","severity":"high","title":"已核实","summary":"s1"},{"type":"feature","severity":"normal","title":"未覆盖","summary":"s2"}]`
	passB := `{"findings":[{"index":1,"confirmed":true,"reason":""}]}`
	server.runner = &insightScriptRunner{outputs: []string{passA, passB}}
	scanID := insertSyncInsightScan(t, server, projectID)

	var status string
	if err := server.db.QueryRow(`select status from project_insight_scans where id=?`, scanID).Scan(&status); err != nil {
		t.Fatalf("scan status: %v", err)
	}
	var n int
	if err := server.db.QueryRow(`select count(*) from project_insights where scan_id=?`, scanID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if status != insightScanFailed || n != 0 {
		t.Errorf("incomplete Pass B: status/count = %q/%d want failed/0", status, n)
	}
}

func TestValidateInsightVerdictRejectsInvalidIndexes(t *testing.T) {
	verdict := func(indexes ...int) insightVerifyVerdict {
		out := insightVerifyVerdict{}
		for _, index := range indexes {
			out.Findings = append(out.Findings, struct {
				Index     int  `json:"index"`
				Confirmed bool `json:"confirmed"`
			}{Index: index, Confirmed: true})
		}
		return out
	}
	for name, result := range map[string]insightVerifyVerdict{
		"duplicate":    verdict(1, 1),
		"out of range": verdict(1, 3),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateInsightVerdict(result, 2); err == nil {
				t.Fatal("expected invalid verdict error")
			}
		})
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

func TestAddInsightToTaskCreatesTaskAndRemovesFinding(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	passA := `[{"type":"bug","severity":"high","title":"列表加载很慢","summary":"列表页打开要等好几秒","fileHint":"src/List.tsx"}]`
	passB := `{"findings":[{"index":1,"confirmed":true}]}`
	server.runner = &insightScriptRunner{outputs: []string{passA, passB}}
	_ = insertSyncInsightScan(t, server, projectID)

	findingID := firstInsightFindingID(t, server, projectID)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/insights/"+findingID+"/to-task", strings.NewReader(`{}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("to-task status: got %d want %d (body %s)", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var task Task
	if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil {
		t.Fatalf("decode task: %v (%s)", err, rec.Body.String())
	}
	if task.ProjectID != projectID || task.Status != taskTodo {
		t.Errorf("task project/status: %+v", task)
	}
	// 标题 = 建议标题；说明含可执行提示词；优先级按严重度映射。
	if task.Title != "列表加载很慢" {
		t.Errorf("task title: got %q want %q", task.Title, "列表加载很慢")
	}
	if !strings.Contains(task.Description, "类型：有 bug") || !strings.Contains(task.Description, "严重度：高") ||
		!strings.Contains(task.Description, "说明：列表页打开要等好几秒") || !strings.Contains(task.Description, "相关位置：src/List.tsx") ||
		!strings.Contains(task.Description, "请据此排查并修复") {
		t.Errorf("task description should carry user-facing prompt, got:\n%s", task.Description)
	}
	if task.Priority != "high" {
		t.Errorf("task priority: got %q want %q", task.Priority, "high")
	}
	// 建议应从列表删除（硬删）。
	var n int
	if err := server.db.QueryRow(`select count(*) from project_insights where id=?`, findingID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("finding should be removed after to-task: got %d want 0", n)
	}
	rec2 := httptest.NewRecorder()
	server.routes().ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/insights", nil))
	resp := insightDecode[insightsResponse](t, rec2, http.StatusOK)
	if len(resp.Findings) != 0 {
		t.Errorf("list after to-task: got %d findings want 0", len(resp.Findings))
	}
	// 任务应记录 task.created 事件（与 createTask 一致）。
	var eventType string
	if err := server.db.QueryRow(`select type from task_events where task_id=? order by created_at desc limit 1`, task.ID).Scan(&eventType); err != nil {
		t.Fatalf("load task event: %v", err)
	}
	if eventType != "task.created" {
		t.Errorf("task event type: got %q want %q", eventType, "task.created")
	}
}

func TestAddInsightToTaskMapsSeverityToPriority(t *testing.T) {
	for severity, wantPriority := range map[string]string{
		insightSeverityHigh:   "high",
		insightSeverityNormal: "normal",
		insightSeverityLow:    "low",
	} {
		t.Run(severity, func(t *testing.T) {
			if got := insightTaskPriority(severity); got != wantPriority {
				t.Errorf("insightTaskPriority(%q): got %q want %q", severity, got, wantPriority)
			}
		})
	}
}

func TestAddInsightToTaskNotFound(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/insights/nope/to-task", strings.NewReader(`{}`)))
	if rec.Code != http.StatusNotFound {
		t.Errorf("to-task unknown finding: got %d want %d", rec.Code, http.StatusNotFound)
	}
}

// 跨项目越权：用项目 B 的 findingID 打项目 A 的端点 → 404。
func TestAddInsightToTaskRejectsCrossProjectFinding(t *testing.T) {
	server := newTestServer(t)
	projectA := insightTestProject(t, server)
	// 再建一个项目 B（id 不同，同样初始化 Git 仓库），在其中落一条 finding。
	projectB := insightTestProjectID(t, server, "insight-project-b")
	passA := `[{"type":"bug","severity":"high","title":"B 的建议","summary":"s"}]`
	passB := `{"findings":[{"index":1,"confirmed":true}]}`
	server.runner = &insightScriptRunner{outputs: []string{passA, passB}}
	_ = insertSyncInsightScan(t, server, projectB)
	findingInB := firstInsightFindingID(t, server, projectB)

	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectA+"/insights/"+findingInB+"/to-task", strings.NewReader(`{}`)))
	if rec.Code != http.StatusNotFound {
		t.Errorf("cross-project to-task: got %d want %d (body %s)", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

// 复核中 / 已失效的建议不允许转任务（与 UI 隐藏按钮一致，API 层同样拦截）。
func TestAddInsightToTaskRejectsPendingVerification(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	f := seedInsightFinding(t, server, projectID, `[{"type":"bug","severity":"high","title":"待复核","summary":"s"}]`)
	server.setInsightVerification(context.Background(), projectID, f.ID, insightVerifyPending, "", time.Now().UTC())

	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/insights/"+f.ID+"/to-task", strings.NewReader(`{}`)))
	if rec.Code != http.StatusConflict {
		t.Errorf("pending to-task: got %d want %d (body %s)", rec.Code, http.StatusConflict, rec.Body.String())
	}
	var tasks int
	if err := server.db.QueryRow(`select count(*) from tasks where project_id=?`, projectID).Scan(&tasks); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if tasks != 0 {
		t.Errorf("pending to-task should not create task: got %d want 0", tasks)
	}
}

func TestAddInsightToTaskRejectsInvalidatedFinding(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	f := seedInsightFinding(t, server, projectID, `[{"type":"bug","severity":"high","title":"已失效","summary":"s"}]`)
	server.setInsightVerification(context.Background(), projectID, f.ID, insightVerifyInvalid, "已修复", time.Now().UTC())

	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/insights/"+f.ID+"/to-task", strings.NewReader(`{}`)))
	if rec.Code != http.StatusConflict {
		t.Errorf("invalidated to-task: got %d want %d (body %s)", rec.Code, http.StatusConflict, rec.Body.String())
	}
}

// 长 summary 的说明先截断（保留结尾行动指令行），且标记"已截断"。
func TestBuildInsightTaskDescriptionTruncatesLongSummary(t *testing.T) {
	long := strings.Repeat("超长说明内容。", 3000) // 约 21000 rune
	f := InsightFinding{Type: insightBug, Severity: insightSeverityHigh, Title: "T", Summary: long, FileHint: "src/List.tsx"}
	desc := buildInsightTaskDescription(f)
	if !strings.Contains(desc, "…（内容过长已截断）") {
		t.Error("truncated summary should carry a marker")
	}
	if !strings.HasSuffix(desc, "请据此排查并修复上述问题/实现上述功能。") {
		t.Error("action instruction line must survive truncation")
	}
	if !strings.Contains(desc, "相关位置：src/List.tsx") {
		t.Error("file hint should be preserved")
	}
}

// 同一建议连续转两次任务：第一次成功并删掉建议，第二次应 404（避免一条建议建出多个任务）。
func TestAddInsightToTaskSecondAttemptNotFound(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	passA := `[{"type":"bug","severity":"high","title":"列表加载很慢","summary":"列表页打开要等好几秒"}]`
	passB := `{"findings":[{"index":1,"confirmed":true}]}`
	server.runner = &insightScriptRunner{outputs: []string{passA, passB}}
	_ = insertSyncInsightScan(t, server, projectID)
	findingID := firstInsightFindingID(t, server, projectID)

	rec1 := httptest.NewRecorder()
	server.routes().ServeHTTP(rec1, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/insights/"+findingID+"/to-task", strings.NewReader(`{}`)))
	if rec1.Code != http.StatusCreated {
		t.Fatalf("first to-task: got %d want %d (body %s)", rec1.Code, http.StatusCreated, rec1.Body.String())
	}
	var tasksAfterFirst int
	if err := server.db.QueryRow(`select count(*) from tasks where project_id=?`, projectID).Scan(&tasksAfterFirst); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if tasksAfterFirst != 1 {
		t.Errorf("tasks after first to-task: got %d want 1", tasksAfterFirst)
	}

	rec2 := httptest.NewRecorder()
	server.routes().ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/insights/"+findingID+"/to-task", strings.NewReader(`{}`)))
	if rec2.Code != http.StatusNotFound {
		t.Errorf("second to-task: got %d want %d (body %s)", rec2.Code, http.StatusNotFound, rec2.Body.String())
	}
	var tasksAfterSecond int
	if err := server.db.QueryRow(`select count(*) from tasks where project_id=?`, projectID).Scan(&tasksAfterSecond); err != nil {
		t.Fatalf("count tasks after second: %v", err)
	}
	if tasksAfterSecond != 1 {
		t.Errorf("tasks after second to-task: got %d want 1 (no duplicate task)", tasksAfterSecond)
	}
}

// insightToTaskBatchResult 批量转任务的返回体（created/skipped/failed/tasks）。
type insightToTaskBatchResult struct {
	Created int `json:"created"`
	Skipped int `json:"skipped"`
	Failed  int `json:"failed"`
}

// seedInsightFindings 跑一次含多条候选的扫描，返回全部已落库 finding（按 rowid 序）。
// Pass B 判定按 passA 的候选条数逐一确认（validateInsightVerdict 要求 verdict 条数精确匹配）。
func seedInsightFindings(t *testing.T, server *Server, projectID, passA string) []InsightFinding {
	t.Helper()
	var items []struct{}
	if err := json.Unmarshal([]byte(passA), &items); err != nil {
		t.Fatalf("parse passA: %v", err)
	}
	verdicts := make([]string, 0, len(items))
	for i := range items {
		verdicts = append(verdicts, fmt.Sprintf(`{"index":%d,"confirmed":true}`, i+1))
	}
	passB := `{"findings":[` + strings.Join(verdicts, ",") + `]}`
	server.runner = &insightScriptRunner{outputs: []string{passA, passB}}
	_ = insertSyncInsightScan(t, server, projectID)
	var out []InsightFinding
	for _, id := range insightFindingIDs(t, server, projectID) {
		f, _ := server.loadInsightFinding(context.Background(), projectID, id)
		out = append(out, f)
	}
	return out
}

// 一键把全部有效建议转任务：3 条全部建任务并硬删建议。
func TestAddInsightToTasksConvertsAllValid(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	passA := `[
		{"type":"bug","severity":"high","title":"列表加载很慢","summary":"列表页打开要等好几秒"},
		{"type":"optimization","severity":"normal","title":"接口可加缓存","summary":"每次请求都打全量数据"},
		{"type":"feature","severity":"low","title":"加导出按钮","summary":"用户常要导出数据"}
	]`
	_ = seedInsightFindings(t, server, projectID, passA)

	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/insights/to-task", strings.NewReader(`{}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("to-task all status: got %d want %d (body %s)", rec.Code, http.StatusCreated, rec.Body.String())
	}
	res := insightDecode[insightToTaskBatchResult](t, rec, http.StatusCreated)
	if res.Created != 3 || res.Skipped != 0 || res.Failed != 0 {
		t.Errorf("batch result: got %+v want created=3 skipped=0 failed=0", res)
	}
	var tasks, findings int
	if err := server.db.QueryRow(`select count(*) from tasks where project_id=?`, projectID).Scan(&tasks); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if err := server.db.QueryRow(`select count(*) from project_insights where project_id=?`, projectID).Scan(&findings); err != nil {
		t.Fatalf("count findings: %v", err)
	}
	if tasks != 3 || findings != 0 {
		t.Errorf("after batch: tasks=%d findings=%d want 3/0", tasks, findings)
	}
	// 任务标题逐一取自建议标题。
	titles := map[string]bool{}
	rows, err := server.db.Query(`select title from tasks where project_id=?`, projectID)
	if err != nil {
		t.Fatalf("query task titles: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var title string
		if rows.Scan(&title) == nil {
			titles[title] = true
		}
	}
	for _, want := range []string{"列表加载很慢", "接口可加缓存", "加导出按钮"} {
		if !titles[want] {
			t.Errorf("task titles missing %q: %v", want, titles)
		}
	}
}

// 复核中 / 已失效的建议自动跳过，只转剩余有效建议。复核中的计入 skipped（前端据此提示），
// 已失效的单独折叠展示、不参与批量。
func TestAddInsightToTasksSkipsPendingAndInvalid(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	passA := `[
		{"type":"bug","severity":"high","title":"A","summary":"s"},
		{"type":"optimization","severity":"normal","title":"B","summary":"s"},
		{"type":"feature","severity":"low","title":"C","summary":"s"}
	]`
	findings := seedInsightFindings(t, server, projectID, passA)
	if len(findings) != 3 {
		t.Fatalf("seeded findings: got %d want 3", len(findings))
	}
	server.setInsightVerification(context.Background(), projectID, findings[0].ID, insightVerifyPending, "", time.Now().UTC())
	server.setInsightVerification(context.Background(), projectID, findings[1].ID, insightVerifyInvalid, "已修复", time.Now().UTC())

	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/insights/to-task", strings.NewReader(`{}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("to-task all status: got %d want %d (body %s)", rec.Code, http.StatusCreated, rec.Body.String())
	}
	res := insightDecode[insightToTaskBatchResult](t, rec, http.StatusCreated)
	if res.Created != 1 || res.Skipped != 1 || res.Failed != 0 {
		t.Errorf("batch result: got %+v want created=1 skipped=1 failed=0", res)
	}
	var tasks, findingsLeft int
	if err := server.db.QueryRow(`select count(*) from tasks where project_id=?`, projectID).Scan(&tasks); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if err := server.db.QueryRow(`select count(*) from project_insights where project_id=?`, projectID).Scan(&findingsLeft); err != nil {
		t.Fatalf("count findings: %v", err)
	}
	if tasks != 1 || findingsLeft != 2 {
		t.Errorf("after batch: tasks=%d findings=%d want 1/2 (pending+invalid kept)", tasks, findingsLeft)
	}
	// 有效列表只剩复核中的那条，已失效仍折叠展示。
	rec2 := httptest.NewRecorder()
	server.routes().ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/insights", nil))
	resp := insightDecode[insightsResponse](t, rec2, http.StatusOK)
	if len(resp.Findings) != 1 || len(resp.Invalidated) != 1 {
		t.Errorf("list after batch: findings=%d invalidated=%d want 1/1", len(resp.Findings), len(resp.Invalidated))
	}
}

// 显式 findingIds 只转换指定建议；跨项目/不存在的 id 忽略。
func TestAddInsightToTasksExplicitIDs(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	passA := `[
		{"type":"bug","severity":"high","title":"A","summary":"s"},
		{"type":"optimization","severity":"normal","title":"B","summary":"s"},
		{"type":"feature","severity":"low","title":"C","summary":"s"}
	]`
	findings := seedInsightFindings(t, server, projectID, passA)
	if len(findings) != 3 {
		t.Fatalf("seeded findings: got %d want 3", len(findings))
	}
	body := fmt.Sprintf(`{"findingIds":[%q,%q,"nope"]}`, findings[0].ID, findings[2].ID)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/insights/to-task", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("to-task ids status: got %d want %d (body %s)", rec.Code, http.StatusCreated, rec.Body.String())
	}
	res := insightDecode[insightToTaskBatchResult](t, rec, http.StatusCreated)
	if res.Created != 2 || res.Skipped != 0 || res.Failed != 0 {
		t.Errorf("batch result: got %+v want created=2 skipped=0 failed=0", res)
	}
	var tasks, findingsLeft int
	if err := server.db.QueryRow(`select count(*) from tasks where project_id=?`, projectID).Scan(&tasks); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if err := server.db.QueryRow(`select count(*) from project_insights where project_id=?`, projectID).Scan(&findingsLeft); err != nil {
		t.Fatalf("count findings: %v", err)
	}
	if tasks != 2 || findingsLeft != 1 {
		t.Errorf("after explicit batch: tasks=%d findings=%d want 2/1", tasks, findingsLeft)
	}
}

// 没有可转的建议：created=0 返回成功（一键场景下 UI 直接提示），不报错。
func TestAddInsightToTasksEmptyProject(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/insights/to-task", strings.NewReader(`{}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("to-task empty status: got %d want %d (body %s)", rec.Code, http.StatusCreated, rec.Body.String())
	}
	res := insightDecode[insightToTaskBatchResult](t, rec, http.StatusCreated)
	if res.Created != 0 || res.Skipped != 0 {
		t.Errorf("batch result on empty project: got %+v want created=0 skipped=0", res)
	}
}

// 未知项目：批量端点 404。
func TestAddInsightToTasksUnknownProject(t *testing.T) {
	server := newTestServer(t)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/projects/nope/insights/to-task", strings.NewReader(`{}`)))
	if rec.Code != http.StatusNotFound {
		t.Errorf("to-task unknown project: got %d want %d (body %s)", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

// 同一批建议连续转两次：第二次无剩余目标 → created=0，不产生重复任务。
func TestAddInsightToTasksSecondRunIdempotent(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	passA := `[
		{"type":"bug","severity":"high","title":"A","summary":"s"},
		{"type":"optimization","severity":"normal","title":"B","summary":"s"}
	]`
	_ = seedInsightFindings(t, server, projectID, passA)

	for i, want := range []int{2, 0} {
		rec := httptest.NewRecorder()
		server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/insights/to-task", strings.NewReader(`{}`)))
		if rec.Code != http.StatusCreated {
			t.Fatalf("to-task run %d status: got %d want %d (body %s)", i+1, rec.Code, http.StatusCreated, rec.Body.String())
		}
		res := insightDecode[insightToTaskBatchResult](t, rec, http.StatusCreated)
		if res.Created != want {
			t.Errorf("batch run %d created: got %d want %d", i+1, res.Created, want)
		}
	}
	var tasks int
	if err := server.db.QueryRow(`select count(*) from tasks where project_id=?`, projectID).Scan(&tasks); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if tasks != 2 {
		t.Errorf("tasks after two runs: got %d want 2 (no duplicates)", tasks)
	}
}

// 转换事务内的删除防护（并发复核竞态）：调用方用转换前已读取的旧快照（此时仍 valid），
// 但落库前该建议已被并发复核判为 invalid → 转换被跳过，不建任务。逐条端点与批量共用
// convertInsightToTask，批量场景下 resolver 先排除 invalid、这里专门测删除防护兜底。
func TestConvertInsightToTaskSkipsFindingInvalidatedMidway(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	f := seedInsightFinding(t, server, projectID, `[{"type":"bug","severity":"high","title":"真问题","summary":"s"}]`)
	// 用旧快照 f 调用转换；落库前该建议已被并发复核判为 invalid。
	server.setInsightVerification(context.Background(), projectID, f.ID, insightVerifyInvalid, "已修复", time.Now().UTC())

	task, converted, err := server.convertInsightToTask(context.Background(), projectID, f)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if converted {
		t.Errorf("expected skip for invalidated finding, but converted %q", task.Title)
	}
	var tasks, findings int
	if err := server.db.QueryRow(`select count(*) from tasks where project_id=?`, projectID).Scan(&tasks); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if err := server.db.QueryRow(`select count(*) from project_insights where id=?`, f.ID).Scan(&findings); err != nil {
		t.Fatalf("count findings: %v", err)
	}
	if tasks != 0 || findings != 1 {
		t.Errorf("after skip: tasks=%d findings=%d want 0/1 (invalidated kept, no task)", tasks, findings)
	}
}

// 同上，但复核把建议置为 pending（批量场景计入 skipped 的来源）：转换防护同样跳过。
func TestConvertInsightToTaskSkipsFindingPendingMidway(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	f := seedInsightFinding(t, server, projectID, `[{"type":"bug","severity":"high","title":"真问题","summary":"s"}]`)
	server.setInsightVerification(context.Background(), projectID, f.ID, insightVerifyPending, "", time.Now().UTC())

	task, converted, err := server.convertInsightToTask(context.Background(), projectID, f)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if converted {
		t.Errorf("expected skip for pending finding, but converted %q", task.Title)
	}
	var tasks, findings int
	if err := server.db.QueryRow(`select count(*) from tasks where project_id=?`, projectID).Scan(&tasks); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if err := server.db.QueryRow(`select count(*) from project_insights where id=?`, f.ID).Scan(&findings); err != nil {
		t.Fatalf("count findings: %v", err)
	}
	if tasks != 0 || findings != 1 {
		t.Errorf("after skip: tasks=%d findings=%d want 0/1 (pending kept, no task)", tasks, findings)
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

func TestRunInsightScanEmitsProgressEvents(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	passA := `[{"type":"bug","severity":"high","title":"列表加载很慢","summary":"列表页打开要等好几秒"},{"type":"feature","severity":"normal","title":"可加导出按钮","summary":"在列表页加一个导出功能"}]`
	passB := `{"findings":[{"index":1,"confirmed":true},{"index":2,"confirmed":false}]}`
	server.runner = &insightScriptRunner{outputs: []string{passA, passB}}
	scanID := insertSyncInsightScan(t, server, projectID)

	events := server.loadInsightEvents(context.Background(), scanID)
	if len(events) == 0 {
		t.Fatal("expected progress events, got none")
	}
	// 里程碑齐全且按 seq 升序。
	joined := ""
	for i, e := range events {
		if e.Seq != i+1 {
			t.Errorf("event seq: got %d want %d", e.Seq, i+1)
		}
		if e.ID == "" || e.Message == "" {
			t.Errorf("event %d missing id/message", i)
		}
		joined += e.Message
	}
	for _, want := range []string{"开始优化建议分析", "第 1 轮", "候选发现", "第 2 轮", "核实完成", "写入建议", "分析完成"} {
		if !strings.Contains(joined, want) {
			t.Errorf("progress events missing %q: %s", want, joined)
		}
	}
	// 成功类里程碑使用 success 级。
	var last InsightEvent
	for _, e := range events {
		if strings.Contains(e.Message, "分析完成") {
			last = e
		}
	}
	if last.Level != "success" {
		t.Errorf("completion event level: got %q want success", last.Level)
	}
}

func TestListInsightsIncludesProgressEvents(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	passA := `[{"type":"bug","severity":"high","title":"a","summary":"s"}]`
	passB := `{"findings":[{"index":1,"confirmed":true}]}`
	server.runner = &insightScriptRunner{outputs: []string{passA, passB}}
	_ = insertSyncInsightScan(t, server, projectID)

	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/insights", nil))
	resp := insightDecode[insightsResponse](t, rec, http.StatusOK)
	if resp.Scan == nil || resp.Scan.Status != insightScanCompleted {
		t.Fatalf("scan: %+v", resp.Scan)
	}
	if len(resp.Events) == 0 {
		t.Fatal("expected events in list response")
	}
	if !strings.Contains(resp.Events[len(resp.Events)-1].Message, "分析完成") {
		t.Errorf("last event should be completion, got %q", resp.Events[len(resp.Events)-1].Message)
	}
}

func TestProjectStatusesReportsInsightsRunning(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	server.runner = &insightScriptRunner{outputs: []string{`[]`, `{"findings":[]}`}}

	// 无运行扫描时 insightsRunning=0。
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/projects/statuses", nil))
	items := insightDecode[[]projectStatusItem](t, rec, http.StatusOK)
	if len(items) == 0 || items[0].InsightsRunning != 0 {
		t.Fatalf("initial insightsRunning: got %d want 0", items[0].InsightsRunning)
	}

	// 插入 running 扫描 + 一条进度事件 → insightsRunning=1 且 message 取最新事件。
	scanID := "scan-status-" + projectID
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	if _, err := server.db.Exec(`insert into project_insight_scans (id,project_id,status,created_at,started_at) values (?,?,'running',?,?)`, scanID, projectID, now, now); err != nil {
		t.Fatalf("insert running scan: %v", err)
	}
	server.appendInsightEvent(context.Background(), scanID, "info", "第 1 轮：通读项目代码，收集候选发现…")
	server.appendInsightEvent(context.Background(), scanID, "info", "正在读取 src/App.tsx")

	rec = httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/projects/statuses", nil))
	items = insightDecode[[]projectStatusItem](t, rec, http.StatusOK)
	if len(items) == 0 || items[0].InsightsRunning != 1 {
		t.Fatalf("running insightsRunning: got %d want 1", items[0].InsightsRunning)
	}
	if items[0].InsightsMessage != "正在读取 src/App.tsx" {
		t.Errorf("insightsMessage: got %q want %q", items[0].InsightsMessage, "正在读取 src/App.tsx")
	}
}

func TestParseInsightToolActivity(t *testing.T) {
	cases := []struct {
		name      string
		eventType string
		payload   string
		wantMsg   string
		wantOK    bool
	}{
		{name: "read with path", eventType: "assistant", payload: `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"src/App.tsx"}}]}}`, wantMsg: "读取 src/App.tsx", wantOK: true},
		{name: "glob pattern", eventType: "assistant", payload: `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Glob","input":{"pattern":"**/*.tsx"}}]}}`, wantMsg: "搜索文件 **/*.tsx", wantOK: true},
		{name: "grep pattern", eventType: "assistant", payload: `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"useEffect"}}]}}`, wantMsg: "检索代码 useEffect", wantOK: true},
		{name: "non-tool content", eventType: "assistant", payload: `{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}`, wantMsg: "", wantOK: false},
		{name: "non-assistant event", eventType: "result", payload: `{"type":"result","status":"completed"}`, wantMsg: "", wantOK: false},
		{name: "malformed", eventType: "assistant", payload: `not json`, wantMsg: "", wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, ok := parseInsightToolActivity(tc.eventType, json.RawMessage(tc.payload))
			if ok != tc.wantOK || msg != tc.wantMsg {
				t.Errorf("parse: got (%q,%v) want (%q,%v)", msg, ok, tc.wantMsg, tc.wantOK)
			}
		})
	}
}

func TestInsightRunErrorMessage(t *testing.T) {
	cases := []struct {
		name     string
		prefix   string
		err      error
		contains []string // 全部需包含
		excludes []string // 全部需不含
	}{
		{
			name:     "timeout stays un-prefixed and self-explanatory",
			prefix:   "项目分析失败",
			err:      errors.New("分析超时：超过 20 分钟未完成。项目可能较大或模型处理较慢"),
			contains: []string{"分析超时：超过 20 分钟未完成"},
			excludes: []string{"项目分析失败：分析超时"},
		},
		{
			name:     "runner error strips task-log fallback wrapper",
			prefix:   "项目分析失败",
			err:      fmt.Errorf("Claude exited: %w", errors.New("exit status 1")),
			contains: []string{"Claude exited"},
			excludes: []string{"任务执行失败，请查看任务日志后重试。"},
		},
		{
			name:     "verify runner error keeps its stage prefix",
			prefix:   "建议核实失败",
			err:      fmt.Errorf("核实代理运行失败: %w", errors.New("connection reset")),
			contains: []string{"建议核实失败：核实代理运行失败"},
		},
		{
			name:     "nil error falls back to generic retry hint",
			prefix:   "项目分析失败",
			err:      nil,
			contains: []string{"项目分析失败，请重试"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := insightRunErrorMessage(tc.prefix, tc.err)
			for _, want := range tc.contains {
				if !strings.Contains(got, want) {
					t.Errorf("insightRunErrorMessage(%q, %v) = %q, want to contain %q", tc.prefix, tc.err, got, want)
				}
			}
			for _, banned := range tc.excludes {
				if strings.Contains(got, banned) {
					t.Errorf("insightRunErrorMessage(%q, %v) = %q, must not contain %q", tc.prefix, tc.err, got, banned)
				}
			}
		})
	}
}

// ─── 建议再验证（re-verify）───────────────────────────────────────────────

// runSyncInsightVerify 把指定建议置 pending 并同步执行再验证，避免经 HTTP 触发时的
// 后台 goroutine 与断言竞态。返回解析出的待验证建议。
func runSyncInsightVerify(t *testing.T, server *Server, projectID string, ids []string) []InsightFinding {
	t.Helper()
	targets, err := server.resolveVerifyTargets(context.Background(), projectID, ids)
	if err != nil {
		t.Fatalf("resolve verify targets: %v", err)
	}
	if len(targets) == 0 {
		t.Fatalf("resolve verify targets returned none for %v", ids)
	}
	now := time.Now().UTC()
	for _, f := range targets {
		server.setInsightVerification(context.Background(), projectID, f.ID, insightVerifyPending, "", now)
	}
	server.runInsightFindingsVerify(context.Background(), projectID, targets)
	return targets
}

// seedInsightFinding 跑一次单发现扫描并返回该 finding（供再验证测试复用）。
func seedInsightFinding(t *testing.T, server *Server, projectID, passA string) InsightFinding {
	t.Helper()
	server.runner = &insightScriptRunner{outputs: []string{passA, `{"findings":[{"index":1,"confirmed":true}]}`}}
	_ = insertSyncInsightScan(t, server, projectID)
	return firstInsightFinding(t, server, projectID)
}

// firstInsightFinding 取项目下第一条 finding（含验证列）。
func firstInsightFinding(t *testing.T, server *Server, projectID string) InsightFinding {
	t.Helper()
	row := server.db.QueryRow(`select `+insightFindingColumns+` from project_insights where project_id=? limit 1`, projectID)
	f, err := scanInsightFinding(row.Scan)
	if err != nil {
		t.Fatalf("load finding: %v", err)
	}
	return f
}

// insightFindingIDs 按 rowid 顺序取项目下全部 finding id。
func insightFindingIDs(t *testing.T, server *Server, projectID string) []string {
	t.Helper()
	rows, err := server.db.Query(`select id from project_insights where project_id=? order by rowid asc`, projectID)
	if err != nil {
		t.Fatalf("query ids: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

// insightVerificationRow 读取一条 finding 的验证三列。
func insightVerificationRow(t *testing.T, server *Server, findingID string) (result string, note sql.NullString, verifiedAt sql.NullTime) {
	t.Helper()
	if err := server.db.QueryRow(`select verification_result,verification_note,verified_at from project_insights where id=?`, findingID).Scan(&result, &note, &verifiedAt); err != nil {
		t.Fatalf("scan verification: %v", err)
	}
	return
}

func TestVerifyInsightFindingMarksValid(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	f := seedInsightFinding(t, server, projectID, `[{"type":"bug","severity":"high","title":"列表加载很慢","summary":"列表页打开要等好几秒","fileHint":"src/List.tsx"}]`)

	// 再验证返回 exists:true → 保持有效，reason 写入 note。
	server.runner = &insightScriptRunner{outputs: []string{`{"findings":[{"id":"` + f.ID + `","exists":true,"reason":"src/List.tsx 里仍未做分页"}]}`}}
	runSyncInsightVerify(t, server, projectID, []string{f.ID})

	result, note, verifiedAt := insightVerificationRow(t, server, f.ID)
	if result != insightVerifyValid {
		t.Errorf("verification_result: got %q want %q", result, insightVerifyValid)
	}
	if !strings.Contains(note.String, "分页") {
		t.Errorf("verification_note should carry reason: %q", note.String)
	}
	if !verifiedAt.Valid {
		t.Error("verified_at should be set")
	}
	// 仍出现在有效列表，invalidated 为空。
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/insights", nil))
	resp := insightDecode[insightsResponse](t, rec, http.StatusOK)
	if len(resp.Findings) != 1 || len(resp.Invalidated) != 0 {
		t.Errorf("after valid verify: findings=%d invalidated=%d want 1/0", len(resp.Findings), len(resp.Invalidated))
	}
	if resp.Findings[0].VerificationResult != insightVerifyValid {
		t.Errorf("list finding should carry verificationResult: %+v", resp.Findings[0])
	}
}

func TestVerifyInsightFindingMarksInvalidAndHides(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	f := seedInsightFinding(t, server, projectID, `[{"type":"bug","severity":"high","title":"列表加载很慢","summary":"列表页打开要等好几秒","fileHint":"src/List.tsx"}]`)

	server.runner = &insightScriptRunner{outputs: []string{`{"findings":[{"id":"` + f.ID + `","exists":false,"reason":"已加分页，位于 src/List.tsx"}]}`}}
	runSyncInsightVerify(t, server, projectID, []string{f.ID})

	result, note, _ := insightVerificationRow(t, server, f.ID)
	if result != insightVerifyInvalid {
		t.Fatalf("verification_result: got %q want %q", result, insightVerifyInvalid)
	}
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/insights", nil))
	resp := insightDecode[insightsResponse](t, rec, http.StatusOK)
	if len(resp.Findings) != 0 || len(resp.Invalidated) != 1 {
		t.Errorf("after invalid verify: findings=%d invalidated=%d want 0/1", len(resp.Findings), len(resp.Invalidated))
	}
	if resp.Invalidated[0].ID != f.ID || !strings.Contains(resp.Invalidated[0].VerificationNote, "分页") {
		t.Errorf("invalidated card: %+v", resp.Invalidated[0])
	}
	if resp.OpenCount != 0 {
		t.Errorf("openCount: got %d want 0", resp.OpenCount)
	}
	_ = note
}

func TestVerifyInsightFindingRunnerErrorMarksFailed(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	f := seedInsightFinding(t, server, projectID, `[{"type":"bug","severity":"high","title":"真问题","summary":"存在"}]`)

	// 再验证时 runner 直接失败（环境问题）→ 不重试、标 failed，note 提示"建议验证失败"。
	calls := 0
	server.runner = runnerFunc(func(_ context.Context, _ AgentRunRequest, sink AgentRunSink) error {
		calls++
		return fmt.Errorf("claude process crashed")
	})
	runSyncInsightVerify(t, server, projectID, []string{f.ID})

	result, note, _ := insightVerificationRow(t, server, f.ID)
	if result != insightVerifyFailed {
		t.Errorf("verification_result: got %q want %q", result, insightVerifyFailed)
	}
	if !strings.Contains(note.String, "建议验证失败") {
		t.Errorf("note should mention verify failure: %q", note.String)
	}
	if calls != 1 {
		t.Errorf("agent runs: got %d want 1 (no retry on runner error)", calls)
	}
}

func TestVerifyInsightFindingBadJSONRetries(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	f := seedInsightFinding(t, server, projectID, `[{"type":"bug","severity":"high","title":"真问题","summary":"存在"}]`)

	bad := "我核对了代码，结论稍后给出。"
	good := `{"findings":[{"id":"` + f.ID + `","exists":true,"reason":"确认仍在"}]}`
	server.runner = &insightScriptRunner{outputs: []string{bad, good}}
	runSyncInsightVerify(t, server, projectID, []string{f.ID})

	result, _, _ := insightVerificationRow(t, server, f.ID)
	if result != insightVerifyValid {
		t.Errorf("verification_result: got %q want %q", result, insightVerifyValid)
	}
	if got := stubCalls(server.runner); got != 2 {
		t.Errorf("agent runs: got %d want 2 (verify + retry)", got)
	}
}

func TestVerifyInsightFindingMissingResultMarksFailed(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	f := seedInsightFinding(t, server, projectID, `[{"type":"bug","severity":"high","title":"真问题","summary":"存在"}]`)

	// agent 给了合法 JSON 却漏掉该 id → 标 failed，不能静默留在 pending。
	server.runner = &insightScriptRunner{outputs: []string{`{"findings":[]}`}}
	runSyncInsightVerify(t, server, projectID, []string{f.ID})

	result, note, _ := insightVerificationRow(t, server, f.ID)
	if result != insightVerifyFailed {
		t.Errorf("verification_result: got %q want %q", result, insightVerifyFailed)
	}
	if !strings.Contains(note.String, "未给出") {
		t.Errorf("note should explain missing result: %q", note.String)
	}
}

// 已失效建议复验失败（agent 运行失败）时保持失效，不得"复活"进有效列表。
func TestReverifyInvalidatedFindingFailureStaysInvalid(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	f := seedInsightFinding(t, server, projectID, `[{"type":"bug","severity":"high","title":"列表加载很慢","summary":"列表页打开要等好几秒"}]`)
	// 第一次复验判失效。
	server.runner = &insightScriptRunner{outputs: []string{`{"findings":[{"id":"` + f.ID + `","exists":false,"reason":"已加分页"}]}`}}
	runSyncInsightVerify(t, server, projectID, []string{f.ID})

	// 第二次复验：agent 进程运行失败 → 必须保持失效，不能因 failed 被列进有效列表。
	calls := 0
	server.runner = runnerFunc(func(_ context.Context, _ AgentRunRequest, _ AgentRunSink) error {
		calls++
		return fmt.Errorf("claude process crashed")
	})
	runSyncInsightVerify(t, server, projectID, []string{f.ID})

	result, _, _ := insightVerificationRow(t, server, f.ID)
	if result != insightVerifyInvalid {
		t.Fatalf("after failed re-verify: result=%q want %q (must stay invalid)", result, insightVerifyInvalid)
	}
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/insights", nil))
	resp := insightDecode[insightsResponse](t, rec, http.StatusOK)
	if len(resp.Findings) != 0 || len(resp.Invalidated) != 1 {
		t.Errorf("after failed re-verify: findings=%d invalidated=%d want 0/1", len(resp.Findings), len(resp.Invalidated))
	}
	if calls != 1 {
		t.Errorf("agent runs: got %d want 1", calls)
	}
}

// 已失效建议复验判定 valid 时应恢复进有效列表（唯一允许"复活"的路径）。
func TestReverifyInvalidatedFindingValidRestoresIt(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	f := seedInsightFinding(t, server, projectID, `[{"type":"bug","severity":"high","title":"列表加载很慢","summary":"列表页打开要等好几秒"}]`)
	server.runner = &insightScriptRunner{outputs: []string{`{"findings":[{"id":"` + f.ID + `","exists":false,"reason":"已加分页"}]}`}}
	runSyncInsightVerify(t, server, projectID, []string{f.ID})

	server.runner = &insightScriptRunner{outputs: []string{`{"findings":[{"id":"` + f.ID + `","status":"valid","reason":"问题仍在"}]}`}}
	runSyncInsightVerify(t, server, projectID, []string{f.ID})

	result, _, _ := insightVerificationRow(t, server, f.ID)
	if result != insightVerifyValid {
		t.Fatalf("after valid re-verify: result=%q want %q", result, insightVerifyValid)
	}
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/insights", nil))
	resp := insightDecode[insightsResponse](t, rec, http.StatusOK)
	if len(resp.Findings) != 1 || len(resp.Invalidated) != 0 {
		t.Errorf("after valid re-verify: findings=%d invalidated=%d want 1/0", len(resp.Findings), len(resp.Invalidated))
	}
}

// cancelInsightRun 应取消项目对应的运行上下文；无句柄时静默。
func TestCancelInsightRun(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	runCtx, runCancel := context.WithCancel(context.Background())
	server.insightMu.Lock()
	server.insightCancels[projectID] = runCancel
	server.insightMu.Unlock()
	done := make(chan struct{})
	go func() {
		<-runCtx.Done()
		close(done)
	}()
	server.cancelInsightRun(projectID)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelInsightRun did not cancel the run context")
	}
	// 无句柄的项目调用不 panic。
	server.cancelInsightRun("no-such-project")
}

// cancelInsightScan HTTP 端点：运行中返回 202+cancelled=true 并触发取消；
// 无运行任务幂等返回 202+cancelled=false；未知项目 404。
func TestCancelInsightScanHTTP(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)

	// 运行中的扫描：注入 cancel 句柄，命中后 runCtx 应被取消。
	runCtx, runCancel := context.WithCancel(context.Background())
	server.insightMu.Lock()
	server.insightActive[projectID] = true
	server.insightCancels[projectID] = runCancel
	server.insightMu.Unlock()

	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/insights/cancel", nil))
	body := insightDecode[map[string]any](t, rec, http.StatusAccepted)
	if cancelled, _ := body["cancelled"].(bool); !cancelled {
		t.Fatalf("running cancel: got cancelled=%v want true (body %s)", body["cancelled"], rec.Body.String())
	}
	select {
	case <-runCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("cancel endpoint did not cancel the run context")
	}

	// 模拟 worker 收尾清理（真实 goroutine 的 defer 会删除 active/cancel 句柄）。
	server.insightMu.Lock()
	delete(server.insightActive, projectID)
	delete(server.insightCancels, projectID)
	server.insightMu.Unlock()

	// 无运行任务：幂等 202 + cancelled=false，双连击不报错。
	rec2 := httptest.NewRecorder()
	server.routes().ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/insights/cancel", nil))
	body2 := insightDecode[map[string]any](t, rec2, http.StatusAccepted)
	if cancelled, _ := body2["cancelled"].(bool); cancelled {
		t.Fatalf("idle cancel: got cancelled=true want false (body %s)", rec2.Body.String())
	}

	// 未知项目：404。
	rec3 := httptest.NewRecorder()
	server.routes().ServeHTTP(rec3, httptest.NewRequest(http.MethodPost, "/api/projects/nope/insights/cancel", nil))
	if rec3.Code != http.StatusNotFound {
		t.Fatalf("unknown project cancel: got %d want %d", rec3.Code, http.StatusNotFound)
	}
}

// 端到端取消：HTTP 触发扫描（阻塞 runner 模拟 agent 运行中）→ HTTP 取消 →
// worker 收到 ctx 取消中止 agent → 扫描置 cancelled 并清理 active/句柄。
// 同时覆盖"取消句柄随 active 一起登记"的竞态修复：触发返回后立即取消必须命中。
func TestCancelInsightScanEndToEnd(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	server.runner = &blockingInsightRunner{}

	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/insights/scan", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start scan: got %d want %d (body %s)", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	var scan InsightScan
	if err := json.Unmarshal(rec.Body.Bytes(), &scan); err != nil {
		t.Fatalf("decode scan: %v", err)
	}
	if scan.Status != insightScanRunning {
		t.Fatalf("scan status: got %q want %q", scan.Status, insightScanRunning)
	}

	// 触发刚返回即取消：必须命中（竞态修复的核心断言）。
	recCancel := httptest.NewRecorder()
	server.routes().ServeHTTP(recCancel, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/insights/cancel", nil))
	if recCancel.Code != http.StatusAccepted {
		t.Fatalf("cancel: got %d want %d (body %s)", recCancel.Code, http.StatusAccepted, recCancel.Body.String())
	}

	// 等 worker 收尾：扫描置 cancelled 且 active/句柄均已清理（goroutine 的 defer 完成）。
	deadline := time.Now().Add(5 * time.Second)
	for {
		var status string
		if err := server.db.QueryRow(`select status from project_insight_scans where id=?`, scan.ID).Scan(&status); err != nil {
			t.Fatalf("read scan status: %v", err)
		}
		server.insightMu.Lock()
		active := server.insightActive[projectID]
		_, hasCancel := server.insightCancels[projectID]
		server.insightMu.Unlock()
		if status == insightScanCancelled && !active && !hasCancel {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("scan did not settle as cancelled: status=%q active=%v hasCancel=%v", status, active, hasCancel)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestVerifyInsightFindingUndeterminedVerdictMarksFailed(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	f := seedInsightFinding(t, server, projectID, `[{"type":"bug","severity":"high","title":"真问题","summary":"存在"}]`)

	// agent 返回了该 id 却缺 exists 字段（违约输出）：必须标 failed，不能默认 false
	// 把仍有效的建议误判为已失效隐藏。
	server.runner = &insightScriptRunner{outputs: []string{`{"findings":[{"id":"` + f.ID + `","reason":"..."}]}`}}
	runSyncInsightVerify(t, server, projectID, []string{f.ID})

	result, _, _ := insightVerificationRow(t, server, f.ID)
	if result != insightVerifyFailed {
		t.Errorf("verification_result: got %q want %q (undetermined verdict must not hide as invalid)", result, insightVerifyFailed)
	}
}

func TestVerifyInsightFindingBatch(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	passA := `[{"type":"bug","severity":"high","title":"A","summary":"sa"},{"type":"feature","severity":"normal","title":"B","summary":"sb"}]`
	passB := `{"findings":[{"index":1,"confirmed":true},{"index":2,"confirmed":true}]}`
	server.runner = &insightScriptRunner{outputs: []string{passA, passB}}
	_ = insertSyncInsightScan(t, server, projectID)
	ids := insightFindingIDs(t, server, projectID)
	if len(ids) != 2 {
		t.Fatalf("seeded findings: got %d want 2", len(ids))
	}

	server.runner = &insightScriptRunner{outputs: []string{`{"findings":[{"id":"` + ids[0] + `","exists":true,"reason":"仍在"},{"id":"` + ids[1] + `","exists":false,"reason":"已实现"}]}`}}
	runSyncInsightVerify(t, server, projectID, ids)

	r0, _, _ := insightVerificationRow(t, server, ids[0])
	r1, _, _ := insightVerificationRow(t, server, ids[1])
	if r0 != insightVerifyValid || r1 != insightVerifyInvalid {
		t.Errorf("batch results: got %q/%q want valid/invalid", r0, r1)
	}
}

func TestVerifyInsightFindingsSplitsLargeBatch(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	targets := make([]InsightFinding, insightVerifyBatchSize+1)
	for i := range targets {
		targets[i] = InsightFinding{ID: fmt.Sprintf("finding-%d", i), Title: fmt.Sprintf("Finding %d", i)}
	}
	response := func(batch []InsightFinding) string {
		items := make([]string, 0, len(batch))
		for _, finding := range batch {
			items = append(items, fmt.Sprintf(`{"id":%q,"exists":true,"reason":"still applies"}`, finding.ID))
		}
		return `{"findings":[` + strings.Join(items, ",") + `]}`
	}
	runner := &insightScriptRunner{outputs: []string{
		response(targets[:insightVerifyBatchSize]),
		response(targets[insightVerifyBatchSize:]),
	}}
	server.runner = runner

	server.runInsightFindingsVerify(context.Background(), projectID, targets)

	requests := runner.capturedRequests()
	if len(requests) != 2 {
		t.Fatalf("agent runs: got %d want 2", len(requests))
	}
	firstNext := fmt.Sprintf("finding-%d", insightVerifyBatchSize)
	if !strings.Contains(requests[0].Prompt, "finding-0") || strings.Contains(requests[0].Prompt, firstNext) {
		t.Errorf("first prompt has wrong batch: %q", requests[0].Prompt)
	}
	if !strings.Contains(requests[1].Prompt, firstNext) || strings.Contains(requests[1].Prompt, "finding-0") {
		t.Errorf("second prompt has wrong batch: %q", requests[1].Prompt)
	}
}

func TestTriggerVerifyInsightRejectsConcurrent(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	f := seedInsightFinding(t, server, projectID, `[{"type":"bug","severity":"high","title":"真问题","summary":"存在"}]`)

	server.insightMu.Lock()
	server.insightActive[projectID] = true
	server.insightMu.Unlock()

	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/insights/verify", strings.NewReader(`{"findingIds":["`+f.ID+`"]}`)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d want %d", rec.Code, http.StatusConflict)
	}
}

func TestTriggerVerifyInsightRejectsOccupiedWorkspace(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	f := seedInsightFinding(t, server, projectID, `[{"type":"bug","severity":"high","title":"真问题","summary":"存在"}]`)
	release, acquired := server.acquireProjectWorkspace(projectID, "run:active")
	if !acquired {
		t.Fatal("acquire test workspace lease")
	}
	defer release()

	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/insights/verify", strings.NewReader(`{"findingIds":["`+f.ID+`"]}`)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d want %d (body %s)", rec.Code, http.StatusConflict, rec.Body.String())
	}
}

func TestTriggerVerifyInsightUnknownTarget(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	body := strings.NewReader(`{"findingIds":["nope"]}`)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/insights/verify", body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want %d (body %s)", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestTriggerVerifyInsightNoTargets(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/insights/verify", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want %d (body %s)", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

// 走真实 HTTP 端点的 happy path：POST /insights/verify → 202 + findingIds 响应 +
// handler 同步写 pending（返回前完成）；后台 goroutine 启动并被 blocking runner 挡住，
// 待断言 pending 后再放行，轮询等它跑完（避免污染后续测试）。
func TestTriggerVerifyInsightAcceptsAndPersistsPending(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	f := seedInsightFinding(t, server, projectID, `[{"type":"bug","severity":"high","title":"真问题","summary":"存在"}]`)

	var mu sync.Mutex
	calls := 0
	started := make(chan struct{})
	release := make(chan struct{})
	server.runner = runnerFunc(func(_ context.Context, _ AgentRunRequest, sink AgentRunSink) error {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			close(started)
		}
		<-release
		return nil
	})

	body := strings.NewReader(`{"findingIds":["` + f.ID + `"]}`)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/insights/verify", body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status: got %d want %d (body %s)", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	var resp struct {
		FindingIDs     []string `json:"findingIds"`
		VerificationID string   `json:"verificationId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v (%s)", err, rec.Body.String())
	}
	if len(resp.FindingIDs) != 1 || resp.FindingIDs[0] != f.ID {
		t.Errorf("response findingIds: %v", resp.FindingIDs)
	}
	var verificationStatus string
	if resp.VerificationID == "" || server.db.QueryRow(`select status from project_insight_verification_runs where id=?`, resp.VerificationID).Scan(&verificationStatus) != nil || verificationStatus != insightScanRunning {
		t.Errorf("verification run: id=%q status=%q", resp.VerificationID, verificationStatus)
	}
	// handler 在返回 202 前同步写 pending。
	result, _, _ := insightVerificationRow(t, server, f.ID)
	if result != insightVerifyPending {
		t.Errorf("verification_result after trigger: got %q want %q", result, insightVerifyPending)
	}

	// 等 goroutine 进入 agent 运行再放行,随后轮询等它跑完（输出为空→标 failed）。
	<-started
	var repoSHA string
	if err := server.db.QueryRow(`select repo_sha from project_insight_verification_runs where id=?`, resp.VerificationID).Scan(&repoSHA); err != nil {
		t.Fatalf("read verification repo SHA: %v", err)
	}
	if repoSHA == "" {
		t.Fatal("verification run did not persist its repository revision")
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, _, _ := insightVerificationRow(t, server, f.ID)
		if r != insightVerifyPending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("verify goroutine did not finish in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestUpdateInsightFindingResetsVerification(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	f := seedInsightFinding(t, server, projectID, `[{"type":"bug","severity":"high","title":"原标题","summary":"原说明"}]`)

	server.runner = &insightScriptRunner{outputs: []string{`{"findings":[{"id":"` + f.ID + `","exists":true,"reason":"仍在"}]}`}}
	runSyncInsightVerify(t, server, projectID, []string{f.ID})

	// 编辑 → 验证状态被重置为未验证（内容已变，上一轮验证失效）。
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/api/projects/"+projectID+"/insights/"+f.ID, strings.NewReader(`{"title":"新标题"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status: got %d (body %s)", rec.Code, rec.Body.String())
	}
	result, note, verifiedAt := insightVerificationRow(t, server, f.ID)
	// verification_note 为 not-null default ''，空串时 NullString.Valid 恒为 true，
	// 因此用 String 判空；verified_at 是 nullable，重置后应为 NULL（Valid=false）。
	if result != "" || note.String != "" || verifiedAt.Valid {
		t.Errorf("after edit: result=%q note=%q verified.valid=%v want '', '', false", result, note.String, verifiedAt.Valid)
	}
}

func TestVerifyExistingPromptBuild(t *testing.T) {
	f := InsightFinding{ID: "abc", Title: "列表加载很慢", Summary: "列表页打开要等好几秒", Type: insightBug, Severity: insightSeverityHigh, FileHint: "src/List.tsx"}
	p := buildVerifyExistingPrompt("/x", "sha123", []InsightFinding{f})
	if !strings.Contains(p, "列表加载很慢") || !strings.Contains(p, "abc") || !strings.Contains(p, "sha123") {
		t.Errorf("prompt should carry finding id/title/repo sha, got:\n%s", p)
	}
	if !strings.Contains(p, `"status":"valid"`) || !strings.Contains(p, `"status":"uncertain"`) {
		t.Errorf("prompt should explain both verdicts, got:\n%s", p)
	}
	// 修正重试跑在全新 agent 会话里，必须自包含候选详情（不能只给 id）。
	rp := buildVerifyExistingRepairPrompt("/x", []InsightFinding{f})
	if !strings.Contains(rp, "列表加载很慢") || !strings.Contains(rp, "abc") || !strings.Contains(rp, "src/List.tsx") {
		t.Errorf("repair prompt must repeat full candidate details (new session has no context), got:\n%s", rp)
	}
}

func TestVerifyRepairPromptIncludesCompleteCandidates(t *testing.T) {
	candidates := []pendingInsight{{Type: insightBug, Severity: insightSeverityHigh, Title: "列表加载很慢", Summary: "列表页打开要等好几秒", FileHint: "src/List.tsx"}}
	prompt := buildVerifyRepairPrompt("/project", candidates)
	for _, want := range []string{"列表加载很慢", "列表页打开要等好几秒", "src/List.tsx"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("repair prompt missing candidate detail %q: %s", want, prompt)
		}
	}
}

func TestVerifyInsightFindingUncertainVerdictMarksFailed(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	f := seedInsightFinding(t, server, projectID, `[{"type":"bug","severity":"high","title":"真问题","summary":"存在"}]`)
	server.runner = &insightScriptRunner{outputs: []string{`{"findings":[{"id":"` + f.ID + `","status":"uncertain","reason":"无法读取相关实现"}]}`}}
	runSyncInsightVerify(t, server, projectID, []string{f.ID})

	result, note, _ := insightVerificationRow(t, server, f.ID)
	if result != insightVerifyFailed || !strings.Contains(note.String, "无法确认") {
		t.Errorf("uncertain verdict should be retryable: result=%q note=%q", result, note.String)
	}
}

func TestRunInsightScanReactivatesRediscoveredInvalidFinding(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	passA := `[{"type":"bug","severity":"high","title":"列表加载很慢","summary":"列表页打开要等好几秒","fileHint":"src/List.tsx"}]`
	passB := `{"findings":[{"index":1,"confirmed":true}]}`
	server.runner = &insightScriptRunner{outputs: []string{passA, passB}}
	_ = insertSyncInsightScan(t, server, projectID)
	f := firstInsightFinding(t, server, projectID)
	server.runner = &insightScriptRunner{outputs: []string{`{"findings":[{"id":"` + f.ID + `","status":"invalid","reason":"已修复"}]}`}}
	runSyncInsightVerify(t, server, projectID, []string{f.ID})

	server.runner = &insightScriptRunner{outputs: []string{passA, passB}}
	scanID := insertSyncInsightScan(t, server, projectID)
	var count int
	var result, findingScanID string
	if err := server.db.QueryRow(`select count(*),max(verification_result),max(scan_id) from project_insights where project_id=?`, projectID).Scan(&count, &result, &findingScanID); err != nil {
		t.Fatalf("load rediscovered finding: %v", err)
	}
	if count != 1 || result != "" || findingScanID != scanID {
		t.Errorf("rediscovered invalid finding: count=%d result=%q scan=%q want 1/empty/%q", count, result, findingScanID, scanID)
	}
}

func TestResolveVerifyTargetsDedupesExplicitIDs(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	f := seedInsightFinding(t, server, projectID, `[{"type":"bug","severity":"high","title":"真问题","summary":"存在"}]`)

	// 同一 id 传两次 → 只验证一次（去重），不重复出现在 agent prompt。
	targets, err := server.resolveVerifyTargets(context.Background(), projectID, []string{f.ID, f.ID})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(targets) != 1 || targets[0].ID != f.ID {
		t.Errorf("duplicate ids should dedupe: got %d targets (%v)", len(targets), targets)
	}
}

func TestInsightUntrackedContentHashTracksContentChanges(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	runner := newGitRunner()
	if _, err := runner.runGit(ctx, repo, "init"); err != nil {
		t.Fatalf("init Git repository: %v", err)
	}
	path := "draft.txt"
	if err := os.WriteFile(filepath.Join(repo, path), []byte("first"), 0o600); err != nil {
		t.Fatalf("write untracked file: %v", err)
	}
	paths, err := runner.runGit(ctx, repo, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		t.Fatalf("list untracked files: %v", err)
	}
	first, err := insightUntrackedContentHash(ctx, runner, repo, paths)
	if err != nil {
		t.Fatalf("hash first content: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, path), []byte("second"), 0o600); err != nil {
		t.Fatalf("rewrite untracked file: %v", err)
	}
	second, err := insightUntrackedContentHash(ctx, runner, repo, paths)
	if err != nil {
		t.Fatalf("hash second content: %v", err)
	}
	if first == second {
		t.Fatal("untracked content hash did not change after file content changed")
	}
}

func TestInsightVerifyCancelledRunPersistsCancelled(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	f := seedInsightFinding(t, server, projectID, `[{"type":"bug","title":"needs review","summary":"still exists"}]`)
	now := time.Now().UTC()
	server.setInsightVerification(context.Background(), projectID, f.ID, insightVerifyPending, "", now)
	verificationID := "cancelled-verify"
	if _, err := server.db.Exec(`insert into project_insight_verification_runs (id,project_id,status,message,total_count,created_at) values (?,?,'running',?,1,?)`, verificationID, projectID, "running", now); err != nil {
		t.Fatalf("insert verification run: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	server.runInsightFindingsVerifyRun(ctx, projectID, verificationID, []InsightFinding{f})

	result, _, _ := insightVerificationRow(t, server, f.ID)
	if result != insightVerifyFailed {
		t.Fatalf("verification result after cancellation: got %q want %q", result, insightVerifyFailed)
	}
	var status string
	if err := server.db.QueryRow(`select status from project_insight_verification_runs where id=?`, verificationID).Scan(&status); err != nil {
		t.Fatalf("read verification run: %v", err)
	}
	if status != insightScanCancelled {
		t.Fatalf("verification run after cancellation: got %q want %q", status, insightScanCancelled)
	}
}

func TestInsightScanCancelledRunPersistsCancelled(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	scanID := "cancelled-scan"
	now := time.Now().UTC()
	if _, err := server.db.Exec(`insert into project_insight_scans (id,project_id,status,created_at,started_at) values (?,?,'running',?,?)`, scanID, projectID, now, now); err != nil {
		t.Fatalf("insert scan: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	server.runProjectInsightScan(ctx, projectID, scanID, scanOpts{})

	var status, scanErr string
	if err := server.db.QueryRow(`select status,error from project_insight_scans where id=?`, scanID).Scan(&status, &scanErr); err != nil {
		t.Fatalf("read scan: %v", err)
	}
	if status != insightScanCancelled || scanErr != "已取消" {
		t.Fatalf("cancelled scan: status=%q error=%q", status, scanErr)
	}
}

func TestInsightPendingOnlyAppliesToSelectedRevision(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	f := seedInsightFinding(t, server, projectID, `[{"type":"bug","title":"needs review","summary":"still exists"}]`)
	targets, err := server.resolveVerifyTargets(context.Background(), projectID, []string{f.ID})
	if err != nil || len(targets) != 1 {
		t.Fatalf("resolve target: targets=%d err=%v", len(targets), err)
	}
	if _, err := server.db.Exec(`update project_insights set title=?,updated_at=? where id=?`, "edited after selection", time.Now().UTC().Add(time.Second), f.ID); err != nil {
		t.Fatalf("edit selected finding: %v", err)
	}
	tx, err := server.db.Begin()
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	applied, err := setInsightVerificationPendingIfUnchanged(context.Background(), tx, projectID, targets[0], time.Now().UTC())
	if err != nil {
		tx.Rollback()
		t.Fatalf("set pending conditionally: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit pending transaction: %v", err)
	}
	if applied {
		t.Fatal("stale target revision was marked pending")
	}
	result, _, _ := insightVerificationRow(t, server, f.ID)
	if result != "" {
		t.Fatalf("stale target verification result: got %q want empty", result)
	}
}

func TestRunInsightScanAllowsNonGitProject(t *testing.T) {
	server := newTestServer(t)
	projectID := "non-git-insight-project"
	if _, err := server.db.Exec(`insert into projects (id,name,path,runner,git_branch,claude_ready,created_at) values (?,?,?,'windows','非 Git 目录',1,?)`, projectID, projectID, t.TempDir(), time.Now().UTC()); err != nil {
		t.Fatalf("insert non-Git project: %v", err)
	}
	runner := &insightScriptRunner{outputs: []string{`[]`}}
	server.runner = runner
	scanID := insertSyncInsightScan(t, server, projectID)
	var status, scanErr string
	var findings int
	if err := server.db.QueryRow(`select status,error,findings_count from project_insight_scans where id=?`, scanID).Scan(&status, &scanErr, &findings); err != nil {
		t.Fatalf("read scan: %v", err)
	}
	if status != insightScanCompleted || scanErr != "" {
		t.Fatalf("non-Git scan should complete instead of failing: status=%q error=%q", status, scanErr)
	}
	if findings != 0 {
		t.Fatalf("non-Git empty scan should produce no findings: got %d", findings)
	}
	if calls := stubCalls(runner); calls != 1 {
		t.Fatalf("non-Git scan should run the agent once: got %d calls", calls)
	}
}

// TestTruncateInsightLogKeepsValidUTF8 验证字节截断会回退到 UTF-8 字符边界：
// 中文错误信息按字节截断不会把多字节字符切半产生 U+FFFD 替换符。
func TestTruncateInsightLogKeepsValidUTF8(t *testing.T) {
	s := "优化建议分析错误（中文错误信息，验证字节截断不会切半字符）"
	got := truncateInsightLog(s, 40) // 40 字节会落在某个中文字符中间
	if !utf8.ValidString(got) {
		t.Fatalf("truncate produced invalid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "…<truncated>") {
		t.Fatalf("truncate missing suffix: %q", got)
	}
	// 截掉后缀后的主体必须是原字符串的合法前缀。
	body := strings.TrimSuffix(got, "…<truncated>")
	if !strings.HasPrefix(s, body) {
		t.Fatalf("truncate body %q is not a prefix of %q", body, s)
	}
	// 纯 ASCII 输入行为不变。
	if got := truncateInsightLog("abcdefghij", 5); got != "abcde…<truncated>" {
		t.Fatalf("ascii truncate: %q", got)
	}
}
