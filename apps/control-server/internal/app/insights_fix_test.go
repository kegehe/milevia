package app

// 本轮修复引入的行为的回归测试（租约粒度 / 剔除原因 / 分批复核 / 不再提示 / 任务关联 /
// 列表防护）。与 insights_test.go 同 package，共用其中已有的测试脚手架。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// nthBatchRunner 按调用次序给出罐装输出，指定的序号直接返回错误（模拟该批 agent 失败）。
type nthBatchRunner struct {
	mu       sync.Mutex
	calls    int
	failAt   int // 1-based
	answered [][]string
}

func (r *nthBatchRunner) Ready(context.Context) bool     { return true }
func (r *nthBatchRunner) Version(context.Context) string { return "1.0" }
func (r *nthBatchRunner) CheckUpdate(context.Context) (bool, string, error) {
	return false, "", nil
}
func (r *nthBatchRunner) Update(context.Context) (string, string, error) { return "", "", nil }

func (r *nthBatchRunner) Run(_ context.Context, req AgentRunRequest, sink AgentRunSink) error {
	r.mu.Lock()
	r.calls++
	call := r.calls
	r.mu.Unlock()
	if call == r.failAt {
		return errors.New("stub batch failure")
	}
	ids := insightPromptCandidateIDs(req.Prompt)
	r.mu.Lock()
	r.answered = append(r.answered, ids)
	r.mu.Unlock()
	verdicts := make([]string, 0, len(ids))
	for _, id := range ids {
		verdicts = append(verdicts, fmt.Sprintf(`{"id":%q,"status":"valid","reason":"仍在"}`, id))
	}
	sink.AssistantText(`{"findings":[`+strings.Join(verdicts, ",")+`]}`, "")
	return nil
}

// insightPromptCandidateIDs 从再验证 prompt 的候选清单里取出 id，跳过输出契约里的示例 id。
func insightPromptCandidateIDs(prompt string) []string {
	var ids []string
	rest := prompt
	for {
		i := strings.Index(rest, `{"id":`)
		if i < 0 {
			return ids
		}
		rest = rest[i+len(`{"id":`):]
		if !strings.HasPrefix(rest, `"`) {
			continue
		}
		rest = rest[1:]
		end := strings.Index(rest, `"`)
		if end < 0 {
			return ids
		}
		id := rest[:end]
		rest = rest[end:]
		if id == "candidate-id" {
			continue
		}
		ids = append(ids, id)
	}
}

// 扫描期间不再独占项目工作区：只读分析可与用户操作并行，互斥只保留在"复核版本 + 写库"。
func TestInsightScanDoesNotHoldWorkspaceLeaseWhileAnalyzing(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	gate := make(chan struct{})
	entered := make(chan struct{})
	server.runner = &gatedInsightRunner{entered: entered, gate: gate,
		outputs: []string{
			`[{"type":"bug","severity":"high","title":"a","summary":"s"}]`,
			`{"findings":[{"index":1,"confirmed":true,"reason":""}]}`,
		}}

	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/insights/scan", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("trigger scan: got %d (%s)", rec.Code, rec.Body.String())
	}
	<-entered // Pass A 正在跑

	// 会话运行（project_shared）用的 key 就是 projectID：此时必须能取到。
	release, acquired := server.acquireWorkspace(projectID, "run:concurrent")
	if !acquired {
		t.Fatal("扫描进行中仍独占项目工作区 —— 修复未生效")
	}
	release()

	close(gate)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		if err := server.db.QueryRow(`select status from project_insight_scans where project_id=? order by created_at desc limit 1`, projectID).Scan(&status); err == nil && status != insightScanRunning {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("scan did not finish")
}

// 落库时项目被别的任务占用：等待后仍拿不到租约则如实失败（结果不落库 + 明确文案）。
func TestInsightPublishFailsWhenWorkspaceBusy(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	server.runner = &insightScriptRunner{outputs: []string{
		`[{"type":"bug","severity":"high","title":"a","summary":"s"}]`,
		`{"findings":[{"index":1,"confirmed":true,"reason":""}]}`,
	}}
	release, acquired := server.acquireProjectWorkspace(projectID, "run:blocker")
	if !acquired {
		t.Fatal("acquire blocker lease")
	}
	defer release()

	// 用短预算的 ctx 驱动扫描：落库阶段等租约的时长会被剩余预算夹住，因而很快就
	// 放弃写入（真实运行时预算由 triggerInsightScan 的 insightScanRunTimeout 给）。
	scanID := "scan-busy-" + projectID
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	if _, err := server.db.Exec(`insert into project_insight_scans (id,project_id,status,created_at,started_at) values (?,?,'running',?,?)`, scanID, projectID, now, now); err != nil {
		t.Fatalf("insert scan: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	server.runProjectInsightScan(ctx, projectID, scanID, scanOpts{})

	var status, errMsg string
	if err := server.db.QueryRow(`select status,error from project_insight_scans where id=?`, scanID).Scan(&status, &errMsg); err != nil {
		t.Fatalf("scan row: %v", err)
	}
	if status != insightScanFailed {
		t.Fatalf("scan status: got %q want %q (error %q)", status, insightScanFailed, errMsg)
	}
	if !strings.Contains(errMsg, "工作区") {
		t.Fatalf("scan error should mention the busy workspace, got %q", errMsg)
	}
	var findings int
	if err := server.db.QueryRow(`select count(*) from project_insights where project_id=?`, projectID).Scan(&findings); err != nil {
		t.Fatalf("count findings: %v", err)
	}
	if findings != 0 {
		t.Fatalf("expected no findings persisted, got %d", findings)
	}
}

// 被第 2 轮核实剔除的候选（含 AI 依据）落进 scan 行并在响应中可见 —— 规则 2 的可审计性。
func TestInsightScanRecordsRejectionReasons(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	passA := `[{"type":"bug","severity":"high","title":"真问题","summary":"s1"},{"type":"feature","severity":"normal","title":"伪需求","summary":"s2"}]`
	const reason = "项目已实现该功能，属于伪需求"
	passB := `{"findings":[{"index":1,"confirmed":true,"reason":""},{"index":2,"confirmed":false,"reason":"` + reason + `"}]}`
	server.runner = &insightScriptRunner{outputs: []string{passA, passB}}
	scanID := insertSyncInsightScan(t, server, projectID)

	var rejectedJSON string
	if err := server.db.QueryRow(`select coalesce(rejected_json,'') from project_insight_scans where id=?`, scanID).Scan(&rejectedJSON); err != nil {
		t.Fatalf("scan rejected_json: %v", err)
	}
	if !strings.Contains(rejectedJSON, reason) {
		t.Fatalf("rejected_json should carry the reason, got %q", rejectedJSON)
	}
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/insights", nil))
	resp := insightDecode[insightsResponse](t, rec, http.StatusOK)
	if resp.Scan == nil || len(resp.Scan.Rejected) != 1 {
		t.Fatalf("expected 1 rejection in scan, got %+v", resp.Scan)
	}
	if resp.Scan.Rejected[0].Title != "伪需求" || resp.Scan.Rejected[0].Reason != reason {
		t.Fatalf("rejection content: %+v", resp.Scan.Rejected[0])
	}
}

// 复核按批独立发布：某批失败只影响该批与后续批次，此前已发布的批次保留。
func TestInsightReverifyBatchFailureKeepsEarlierBatches(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	var items []string
	for i := 0; i < 12; i++ {
		items = append(items, fmt.Sprintf(`{"type":"bug","severity":"normal","title":"建议%02d","summary":"s%d"}`, i+1, i+1))
	}
	findings := seedInsightFindings(t, server, projectID, "["+strings.Join(items, ",")+"]")
	if len(findings) != 12 {
		t.Fatalf("seeded findings: got %d want 12", len(findings))
	}
	chunk := insightReverifyChunkSize(len(findings))
	if chunk != 4 {
		t.Fatalf("chunk size: got %d want 4", chunk)
	}

	// 第 1 批成功，第 2 批失败，第 3 批成功。
	runner := &nthBatchRunner{failAt: 2}
	server.runner = runner
	targets, err := server.resolveVerifyTargets(context.Background(), projectID, nil)
	if err != nil {
		t.Fatalf("resolve verify targets: %v", err)
	}
	now := time.Now().UTC()
	for _, f := range targets {
		server.setInsightVerification(context.Background(), projectID, f.ID, insightVerifyPending, "", now)
	}
	server.runInsightFindingsVerify(context.Background(), projectID, targets)

	runner.mu.Lock()
	answered := len(runner.answered)
	runner.mu.Unlock()
	if answered == 0 {
		t.Fatal("no batch was successfully answered; test cannot prove anything")
	}

	var valid, failed int
	if err := server.db.QueryRow(`select
		sum(case when verification_result='valid' then 1 else 0 end),
		sum(case when verification_result='failed' then 1 else 0 end)
		from project_insights where project_id=?`, projectID).Scan(&valid, &failed); err != nil {
		t.Fatalf("sum verification: %v", err)
	}
	// 已成功发布的批次必须保留（这正是修复点：过去会被整轮作废）。
	if valid != chunk*2 {
		t.Fatalf("成功批次的结论应保留，got valid=%d want %d", valid, chunk*2)
	}
	if failed != chunk {
		t.Fatalf("失败批次应为可重试失败，got failed=%d want %d", failed, chunk)
	}
}

// 「不再提示」：建议从有效列表移入折叠区，且后续扫描不再上报它。
func TestInsightDismissHidesAndSuppressesRescan(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	passA := `[{"type":"bug","severity":"high","title":"噪声建议","summary":"用户不想再看到"}]`
	server.runner = &insightScriptRunner{outputs: []string{passA, `{"findings":[{"index":1,"confirmed":true,"reason":""}]}`}}
	insertSyncInsightScan(t, server, projectID)
	f := firstInsightFinding(t, server, projectID)

	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/projects/"+projectID+"/insights/"+f.ID+"/dismiss", strings.NewReader("{}")))
	if rec.Code != http.StatusOK {
		t.Fatalf("dismiss: got %d (%s)", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/insights", nil))
	resp := insightDecode[insightsResponse](t, rec, http.StatusOK)
	if len(resp.Findings) != 0 {
		t.Fatalf("dismissed finding should leave the open list, got %d", len(resp.Findings))
	}
	if len(resp.Dismissed) != 1 || resp.Dismissed[0].ID != f.ID {
		t.Fatalf("dismissed list: %+v", resp.Dismissed)
	}
	// 复核 / 批量转任务的目标集合同样不含它。
	targets, err := server.resolveVerifyTargets(context.Background(), projectID, nil)
	if err != nil {
		t.Fatalf("resolve verify targets: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("dismissed finding should not be a verify target, got %d", len(targets))
	}
	toTask, err := server.resolveToTaskTargets(context.Background(), projectID, nil)
	if err != nil {
		t.Fatalf("resolve to-task targets: %v", err)
	}
	if len(toTask) != 0 {
		t.Fatalf("dismissed finding should not be a to-task target, got %d", len(toTask))
	}

	// 再次扫描：同一条建议不得被重新上报（指纹被抑制）。
	server.runner = &insightScriptRunner{outputs: []string{passA, `{"findings":[{"index":1,"confirmed":true,"reason":""}]}`}}
	scanID := insertSyncInsightScan(t, server, projectID)
	var scanFindings, suppressed int
	if err := server.db.QueryRow(`select findings_count,suppressed_count from project_insight_scans where id=?`, scanID).Scan(&scanFindings, &suppressed); err != nil {
		t.Fatalf("scan counts: %v", err)
	}
	if scanFindings != 0 || suppressed != 1 {
		t.Fatalf("re-scan of dismissed finding: findings=%d suppressed=%d want 0/1", scanFindings, suppressed)
	}

	// 恢复后重新回到有效列表。
	rec = httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/projects/"+projectID+"/insights/"+f.ID+"/dismiss", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("restore: got %d (%s)", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/insights", nil))
	resp = insightDecode[insightsResponse](t, rec, http.StatusOK)
	if len(resp.Findings) != 1 || len(resp.Dismissed) != 0 {
		t.Fatalf("restored finding should be back in the open list: findings=%d dismissed=%d", len(resp.Findings), len(resp.Dismissed))
	}
}

// 同一问题已转成任务且任务未完时不再重复建任务；任务到终态后可以再次转换。
func TestConvertInsightToTaskSkipsWhileLinkedTaskOpen(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	passA := `[{"type":"bug","severity":"high","title":"重复问题","summary":"同一个问题"}]`
	server.runner = &insightScriptRunner{outputs: []string{passA, `{"findings":[{"index":1,"confirmed":true,"reason":""}]}`}}
	insertSyncInsightScan(t, server, projectID)
	f := firstInsightFinding(t, server, projectID)

	task, converted, err := server.convertInsightToTask(context.Background(), projectID, f)
	if err != nil || !converted {
		t.Fatalf("first convert: converted=%v err=%v", converted, err)
	}
	var fingerprint string
	if err := server.db.QueryRow(`select coalesce(source_insight_fingerprint,'') from tasks where id=?`, task.ID).Scan(&fingerprint); err != nil {
		t.Fatalf("task fingerprint: %v", err)
	}
	if fingerprint != insightFingerprint(f.Title, f.Summary) {
		t.Fatalf("task should record the source fingerprint, got %q", fingerprint)
	}

	// 同一问题被再次发现（硬删后重新上报）→ 已有未完成任务 → 跳过。
	server.runner = &insightScriptRunner{outputs: []string{passA, `{"findings":[{"index":1,"confirmed":true,"reason":""}]}`}}
	insertSyncInsightScan(t, server, projectID)
	again := firstInsightFinding(t, server, projectID)
	if _, converted, err := server.convertInsightToTask(context.Background(), projectID, again); err != nil || converted {
		t.Fatalf("second convert while task open: converted=%v err=%v want false/nil", converted, err)
	}
	// 建议本身仍在（跳过不等于删除）。
	var count int
	if err := server.db.QueryRow(`select count(*) from project_insights where project_id=?`, projectID).Scan(&count); err != nil {
		t.Fatalf("count findings: %v", err)
	}
	if count != 1 {
		t.Fatalf("skipped conversion must keep the finding, got %d rows", count)
	}

	// 任务进入终态后可再次转换。
	if _, err := server.db.Exec(`update tasks set status=? where id=?`, taskDone, task.ID); err != nil {
		t.Fatalf("complete task: %v", err)
	}
	if _, converted, err := server.convertInsightToTask(context.Background(), projectID, again); err != nil || !converted {
		t.Fatalf("convert after task done: converted=%v err=%v want true/nil", converted, err)
	}
}

// 建议卡片能带上"已转为任务"的状态标注。
func TestListInsightsAnnotatesLinkedTask(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	passA := `[{"type":"bug","severity":"high","title":"关联问题","summary":"同一个问题"}]`
	server.runner = &insightScriptRunner{outputs: []string{passA, `{"findings":[{"index":1,"confirmed":true,"reason":""}]}`}}
	insertSyncInsightScan(t, server, projectID)
	f := firstInsightFinding(t, server, projectID)
	if _, converted, err := server.convertInsightToTask(context.Background(), projectID, f); err != nil || !converted {
		t.Fatalf("convert: converted=%v err=%v", converted, err)
	}
	// 重新上报同一问题（硬删后扫描会再次发现）。
	server.runner = &insightScriptRunner{outputs: []string{passA, `{"findings":[{"index":1,"confirmed":true,"reason":""}]}`}}
	insertSyncInsightScan(t, server, projectID)

	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/insights", nil))
	resp := insightDecode[insightsResponse](t, rec, http.StatusOK)
	if len(resp.Findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(resp.Findings))
	}
	if resp.Findings[0].LinkedTaskStatus != taskTodo {
		t.Fatalf("linked task status: got %q want %q", resp.Findings[0].LinkedTaskStatus, taskTodo)
	}
}

// 编辑建议后，编辑前的原文不会被下次扫描当成新建议再报一次。
func TestUpdateInsightFindingSuppressesPreviousFingerprint(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	oldPassA := `[{"type":"bug","severity":"high","title":"旧写法","summary":"旧说明"}]`
	server.runner = &insightScriptRunner{outputs: []string{oldPassA, `{"findings":[{"index":1,"confirmed":true,"reason":""}]}`}}
	insertSyncInsightScan(t, server, projectID)
	f := firstInsightFinding(t, server, projectID)

	body := strings.NewReader(`{"title":"新写法","summary":"新说明","type":"optimization"}`)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/api/projects/"+projectID+"/insights/"+f.ID, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: got %d (%s)", rec.Code, rec.Body.String())
	}
	updated := insightDecode[InsightFinding](t, rec, http.StatusOK)
	if updated.Type != insightOptimization {
		t.Fatalf("type should be editable, got %q", updated.Type)
	}

	// 扫描重新报告"旧写法"：应被抑制（旧指纹已记入 suppressions）。
	server.runner = &insightScriptRunner{outputs: []string{oldPassA, `{"findings":[{"index":1,"confirmed":true,"reason":""}]}`}}
	scanID := insertSyncInsightScan(t, server, projectID)
	var scanFindings, suppressed int
	if err := server.db.QueryRow(`select findings_count,suppressed_count from project_insight_scans where id=?`, scanID).Scan(&scanFindings, &suppressed); err != nil {
		t.Fatalf("scan counts: %v", err)
	}
	if scanFindings != 0 || suppressed != 1 {
		t.Fatalf("previous fingerprint should be suppressed: findings=%d suppressed=%d", scanFindings, suppressed)
	}
	var count int
	if err := server.db.QueryRow(`select count(*) from project_insights where project_id=?`, projectID).Scan(&count); err != nil {
		t.Fatalf("count findings: %v", err)
	}
	if count != 1 {
		t.Fatalf("edited finding must stay the only row, got %d", count)
	}
}

// sinceSeq 增量拉取的游标语义：游标只对**它所属的那次扫描**有效。客户端手上的游标
// 可能属于上一次扫描，而新扫描的 seq 从 1 重新开始——若只按 seq 过滤，新扫描开头的事件
// （seq 小于旧游标）会被永久漏掉。因此 sinceScan 与当前扫描不符时必须返回全量。
func TestListInsightsIgnoresCursorFromAnotherScan(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	server.runner = &insightScriptRunner{outputs: []string{`[]`, `{"findings":[]}`}}
	oldScanID := insertSyncInsightScan(t, server, projectID)

	// 新的 running 扫描，带若干事件（seq 从 1 开始）。
	// created_at 用"现在 +1 秒"：created_at 只存到秒，若与上一条扫描落在同一秒，
	// listInsights 的 `order by created_at desc limit 1` 就不确定取哪条（测试会闪断）。
	newScanID := "scan-new-" + projectID
	now := time.Now().UTC().Add(time.Second).Format("2006-01-02 15:04:05")
	if _, err := server.db.Exec(`insert into project_insight_scans (id,project_id,status,created_at,started_at) values (?,?,'running',?,?)`, newScanID, projectID, now, now); err != nil {
		t.Fatalf("insert running scan: %v", err)
	}
	for i := 1; i <= 3; i++ {
		server.appendInsightEvent(context.Background(), newScanID, "info", fmt.Sprintf("新扫描事件 %d", i))
	}

	// 用旧扫描的 id + 一个大游标请求：不得据此过滤掉新扫描的事件。
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/projects/%s/insights?sinceScan=%s&sinceSeq=99", projectID, oldScanID), nil))
	resp := insightDecode[insightsResponse](t, rec, http.StatusOK)
	if resp.Scan == nil || resp.Scan.ID != newScanID {
		t.Fatalf("scan: %+v", resp.Scan)
	}
	if len(resp.Events) != 3 {
		t.Fatalf("cursor from another scan must not filter events, got %d want 3", len(resp.Events))
	}
	// 游标属于当前扫描时正常增量：只返回更新的那一条。
	lastSeq := resp.Events[len(resp.Events)-1].Seq
	server.appendInsightEvent(context.Background(), newScanID, "info", "又一条")
	rec = httptest.NewRecorder()
	server.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/projects/%s/insights?sinceScan=%s&sinceSeq=%d", projectID, newScanID, lastSeq), nil))
	incremental := insightDecode[insightsResponse](t, rec, http.StatusOK)
	if len(incremental.Events) != 1 || incremental.Events[0].Message != "又一条" {
		t.Fatalf("matching cursor should return only newer events, got %+v", incremental.Events)
	}
}

// 落库失败时给出的是具体原因（不再出现空错误文案）。
func TestInsightPublishFailureReportsReason(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	passA := `[{"type":"bug","severity":"high","title":"a","summary":"s"}]`
	server.runner = &insightScriptRunner{outputs: []string{passA, `{"findings":[{"index":1,"confirmed":true,"reason":""}]}`}}

	// 让写库必然失败：先把 project_insights 换成一张只读/冲突的表代价太大，这里改用
	// "租约被长期占用"这一可稳定复现的失败路径，断言 error 非空且可读。
	release, acquired := server.acquireProjectWorkspace(projectID, "run:blocker")
	if !acquired {
		t.Fatal("acquire blocker lease")
	}
	defer release()
	scanID := "scan-reason-" + projectID
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	if _, err := server.db.Exec(`insert into project_insight_scans (id,project_id,status,created_at,started_at) values (?,?,'running',?,?)`, scanID, projectID, now, now); err != nil {
		t.Fatalf("insert scan: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	server.runProjectInsightScan(ctx, projectID, scanID, scanOpts{})

	var status, errMsg string
	if err := server.db.QueryRow(`select status,error from project_insight_scans where id=?`, scanID).Scan(&status, &errMsg); err != nil {
		t.Fatalf("scan row: %v", err)
	}
	if status != insightScanFailed {
		t.Fatalf("scan status: got %q want %q", status, insightScanFailed)
	}
	if strings.TrimSpace(errMsg) == "" {
		t.Fatal("failed scan must carry a non-empty, user-readable reason")
	}
	if strings.Contains(errMsg, "分析失败：\n") || strings.HasSuffix(errMsg, "：") {
		t.Fatalf("reason looks malformed: %q", errMsg)
	}
}

func TestInsightScanCancelDoesNotPublish(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	server.runner = &blockingInsightRunner{}
	scanID := "scan-cancel-" + projectID
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	if _, err := server.db.Exec(`insert into project_insight_scans (id,project_id,status,created_at,started_at) values (?,?,'running',?,?)`, scanID, projectID, now, now); err != nil {
		t.Fatalf("insert scan: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.runProjectInsightScan(ctx, projectID, scanID, scanOpts{})
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("scan did not stop after cancel")
	}
	var status string
	if err := server.db.QueryRow(`select status from project_insight_scans where id=?`, scanID).Scan(&status); err != nil {
		t.Fatalf("scan status: %v", err)
	}
	if status != insightScanCancelled {
		t.Fatalf("scan status: got %q want %q", status, insightScanCancelled)
	}
	var findings int
	if err := server.db.QueryRow(`select count(*) from project_insights where project_id=?`, projectID).Scan(&findings); err != nil {
		t.Fatalf("count findings: %v", err)
	}
	if findings != 0 {
		t.Fatalf("cancelled scan must not publish findings, got %d", findings)
	}
}
