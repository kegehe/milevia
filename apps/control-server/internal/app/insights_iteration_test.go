package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestInsightIterationDetailsReachEditedCardAndTask(t *testing.T) {
	s := newTestServer(t)
	p := insightTestProject(t, s)
	runner := &insightScriptRunner{outputs: []string{
		`{"findings":[{"type":"optimization","title":"补齐安装说明","summary":"新用户缺少启动指引","evidence":"README 未说明启动入口","acceptanceCriteria":"按文档可完成首次启动"}]}`,
		`{"findings":[{"index":1,"confirmed":true,"reason":"README 缺少启动命令；package.json 有 dev 脚本","duplicateOf":""}]}`,
	}}
	s.runner = runner
	insertSyncInsightScan(t, s, p)
	f := firstInsightFinding(t, s, p)
	if !strings.Contains(f.Evidence, "package.json") || f.AcceptanceCriteria != "按文档可完成首次启动" {
		t.Fatalf("scan lost details: %+v", f)
	}
	for _, req := range runner.capturedRequests() {
		for _, want := range []string{"README", "当前", "验收", "文档"} {
			if !strings.Contains(req.Prompt, want) {
				t.Fatalf("analysis/review lost project context or criteria: %q", want)
			}
		}
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/api/projects/"+p+"/insights/"+f.ID,
		strings.NewReader(`{"evidence":"更新后的文档位置","acceptanceCriteria":"在干净环境按步骤启动成功"}`)))
	f = insightDecode[InsightFinding](t, rec, http.StatusOK)
	if f.Evidence != "更新后的文档位置" || f.AcceptanceCriteria != "在干净环境按步骤启动成功" {
		t.Fatalf("edit lost details: %+v", f)
	}
	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/projects/"+p+"/insights", nil))
	listed := insightDecode[insightsResponse](t, rec, http.StatusOK)
	if len(listed.Findings) != 1 || listed.Findings[0].Evidence != f.Evidence {
		t.Fatalf("list lost details: %+v", listed.Findings)
	}
	s.runner = &insightScriptRunner{outputs: []string{`{"findings":[{"index":1,"status":"valid","reason":"仍缺少说明"}]}`}}
	runSyncInsightVerify(t, s, p, []string{f.ID})
	f, _ = s.loadInsightFinding(context.Background(), p, f.ID)
	if f.VerificationResult != insightVerifyValid || f.Evidence != "更新后的文档位置" {
		t.Fatalf("reverification lost original details: %+v", f)
	}
	task, converted, err := s.convertInsightToTask(context.Background(), p, f)
	if err != nil || !converted || !strings.Contains(task.Description, f.Evidence) || !strings.Contains(task.Description, f.AcceptanceCriteria) {
		t.Fatalf("task lost evidence or acceptance: task=%+v converted=%v err=%v", task, converted, err)
	}
}

func TestInsightIterationSuppressesRewordedFinding(t *testing.T) {
	s := newTestServer(t)
	p := insightTestProject(t, s)
	s.runner = &insightScriptRunner{outputs: []string{
		`[{"type":"bug","title":"保存失败后丢失草稿","summary":"网络错误时输入被清空","evidence":"测试代码依据","acceptanceCriteria":"按复现步骤检查预期结果"}]`,
		`{"findings":[{"index":1,"confirmed":true,"reason":"错误分支清空了输入"}]}`,
	}}
	insertSyncInsightScan(t, s, p)
	original := firstInsightFinding(t, s, p)
	ref := "finding:" + original.ID
	runner := &insightScriptRunner{outputs: []string{
		`[{"type":"optimization","title":"发送异常应保留输入","summary":"请求失败需要保留未发送的内容","evidence":"测试代码依据","acceptanceCriteria":"按复现步骤检查预期结果"}]`,
		fmt.Sprintf(`{"findings":[{"index":1,"confirmed":true,"reason":"同一错误分支","duplicateOf":%q}]}`, ref),
	}}
	s.runner = runner
	scan := insertSyncInsightScan(t, s, p)
	var count, suppressed int
	if err := s.db.QueryRow(`select count(*) from project_insights where project_id=?`, p).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`select suppressed_count from project_insight_scans where id=?`, scan).Scan(&suppressed); err != nil {
		t.Fatal(err)
	}
	if count != 1 || suppressed != 1 {
		t.Fatalf("reworded duplicate was not suppressed: findings=%d suppressed=%d", count, suppressed)
	}
	for _, req := range runner.capturedRequests() {
		if !strings.Contains(req.Prompt, ref) || !strings.Contains(req.Prompt, original.Title) {
			t.Fatal("history must accompany both discovery and independent verification")
		}
	}
}

func TestInsightIterationActiveTaskAndTerminalRediscovery(t *testing.T) {
	for _, terminal := range []string{taskDone, taskCancelled} {
		t.Run(terminal, func(t *testing.T) {
			s := newTestServer(t)
			p := insightTestProject(t, s)
			output := []string{`[{"type":"bug","title":"错误时丢失草稿","summary":"请求错误分支清空输入","evidence":"测试代码依据","acceptanceCriteria":"按复现步骤检查预期结果"}]`, `{"findings":[{"index":1,"confirmed":true,"reason":"仍存在"}]}`}
			s.runner = &insightScriptRunner{outputs: output}
			insertSyncInsightScan(t, s, p)
			f := firstInsightFinding(t, s, p)
			task, ok, err := s.convertInsightToTask(context.Background(), p, f)
			if err != nil || !ok {
				t.Fatalf("convert: %v %v", ok, err)
			}
			s.runner = &insightScriptRunner{outputs: output}
			activeScan := insertSyncInsightScan(t, s, p)
			var count, suppressed int
			if err := s.db.QueryRow(`select count(*) from project_insights where project_id=?`, p).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRow(`select suppressed_count from project_insight_scans where id=?`, activeScan).Scan(&suppressed); err != nil {
				t.Fatal(err)
			}
			if count != 0 || suppressed != 1 {
				t.Fatalf("active task was reported as a new suggestion: count=%d suppressed=%d", count, suppressed)
			}
			if _, err := s.db.Exec(`update tasks set status=? where id=?`, terminal, task.ID); err != nil {
				t.Fatal(err)
			}
			s.runner = &insightScriptRunner{outputs: output}
			insertSyncInsightScan(t, s, p)
			if got := firstInsightFinding(t, s, p); got.Title != f.Title {
				t.Fatalf("terminal task prevented rediscovery: %+v", got)
			}
		})
	}
}

func TestInsightIterationManualTaskReferenceExpires(t *testing.T) {
	s := newTestServer(t)
	p := insightTestProject(t, s)
	now := time.Now().UTC()
	if _, err := s.db.Exec(`insert into tasks (id,project_id,title,description,status,created_at,updated_at) values (?,?,?,?,?,?,?)`,
		"manual-task", p, "补充启动文档", "说明启动命令与环境要求", taskTodo, now, now); err != nil {
		t.Fatal(err)
	}
	history, err := s.loadInsightScanHistory(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	ref, ok := history.References["task:manual-task"]
	if !ok || !strings.Contains(strings.Join(history.Lines, "\n"), "说明启动命令") {
		t.Fatalf("manual task was omitted: %+v", history)
	}
	s.runner = &insightScriptRunner{outputs: []string{
		`[{"type":"feature","title":"为首次启动提供操作指引","summary":"需要说明如何启动及环境要求","evidence":"测试代码依据","acceptanceCriteria":"按复现步骤检查预期结果"}]`,
		`{"findings":[{"index":1,"confirmed":true,"reason":"已有未完成任务覆盖同一目标","duplicateOf":"task:manual-task"}]}`,
	}}
	scan := insertSyncInsightScan(t, s, p)
	var suppressed, findings int
	if err := s.db.QueryRow(`select suppressed_count,findings_count from project_insight_scans where id=?`, scan).Scan(&suppressed, &findings); err != nil || suppressed != 1 || findings != 0 {
		t.Fatalf("manual task duplicate was not suppressed: suppressed=%d findings=%d err=%v", suppressed, findings, err)
	}
	for _, state := range []string{"active", "edited", "done", "other-project"} {
		t.Run(state, func(t *testing.T) {
			if state == "edited" {
				if _, err := s.db.Exec(`update tasks set description='changed scope' where id='manual-task'`); err != nil {
					t.Fatal(err)
				}
			}
			if state == "done" {
				if _, err := s.db.Exec(`update tasks set status=? where id='manual-task'`, taskDone); err != nil {
					t.Fatal(err)
				}
			}
			tx, err := s.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			project := p
			if state == "other-project" {
				project = "other-project"
			}
			applies, err := insightReferenceStillApplies(context.Background(), tx, project, ref)
			if err != nil || applies != (state == "active") {
				t.Fatalf("state=%s stillApplies=%v err=%v", state, applies, err)
			}
		})
	}
}

func TestInsightIterationUnknownReferenceRetriesWithHistory(t *testing.T) {
	s := newTestServer(t)
	p := insightTestProject(t, s)
	project, err := s.getProjectByID(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	history := insightScanHistory{Lines: []string{`{"reference":"task:known","title":"已有任务"}`},
		References: map[string]insightHistoryReference{"task:known": {Kind: "task", ID: "known"}}}
	bad := `{"findings":[{"index":1,"confirmed":true,"reason":"duplicate","duplicateOf":"task:invented"}]}`
	runner := &insightScriptRunner{outputs: []string{bad, bad}}
	s.runner = runner
	_, err = s.runInsightVerify(context.Background(), project, "claude-code", []pendingInsight{{Title: "候选"}}, nil, history)
	if err == nil || !strings.Contains(err.Error(), "unknown duplicate reference") {
		t.Fatalf("invented reference should not suppress a suggestion: %v", err)
	}
	requests := runner.capturedRequests()
	if len(requests) != 2 || !strings.Contains(requests[1].Prompt, "task:known") {
		t.Fatal("repair must preserve duplicate-reference context and retry only once")
	}
}

func TestInsightIterationEditedFindingReferenceDoesNotSuppress(t *testing.T) {
	s := newTestServer(t)
	p := insightTestProject(t, s)
	s.runner = &insightScriptRunner{outputs: []string{`[{"type":"bug","title":"草稿丢失","summary":"错误分支清空输入","evidence":"测试代码依据","acceptanceCriteria":"按复现步骤检查预期结果"}]`, `{"findings":[{"index":1,"confirmed":true}]}`}}
	insertSyncInsightScan(t, s, p)
	f := firstInsightFinding(t, s, p)
	history, err := s.loadInsightScanHistory(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	// Editing evidence changes the content revision even if title/summary stay the same.
	if _, err := s.db.Exec(`update project_insights set evidence='changed',updated_at=? where id=?`, f.UpdatedAt.Add(time.Second), f.ID); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if applies, err := insightReferenceStillApplies(context.Background(), tx, p, history.References["finding:"+f.ID]); err != nil || applies {
		t.Fatalf("edited reference must be re-evaluated: applies=%v err=%v", applies, err)
	}
}

func TestInsightIterationRichResponseFitsMobileRelay(t *testing.T) {
	resp := insightsResponse{OpenCount: 500, FindingsLimit: 500}
	for i := 0; i < 500; i++ {
		f := InsightFinding{ID: fmt.Sprint(i), Title: "建议", Summary: "说明", Evidence: strings.Repeat("据", 2000), AcceptanceCriteria: strings.Repeat("验", 2000)}
		resp.Findings = append(resp.Findings, f)
		resp.Invalidated = append(resp.Invalidated, f)
		resp.Dismissed = append(resp.Dismissed, f)
	}
	capInsightResponse(&resp)
	body, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(rpcResponse{OK: true, Status: http.StatusOK, Data: body})
	if err != nil || len(encoded) >= 320<<10 || !resp.Truncated || !resp.FoldedTruncated || resp.OpenCount != 500 || resp.FindingsLimit != len(resp.Findings) || len(resp.Findings) == 0 {
		t.Fatalf("invalid bounded response: bytes=%d cards=%d total=%d truncated=%v folded=%v err=%v", len(encoded), len(resp.Findings), resp.OpenCount, resp.Truncated, resp.FoldedTruncated, err)
	}
	if len([]rune(resp.Findings[0].Evidence)) != 2000 {
		t.Fatal("response must retain whole evidence rather than silently shortening cards")
	}
}

func TestInsightIterationHistoryBudgetKeepsBothSourcesAndAllFingerprints(t *testing.T) {
	s := newTestServer(t)
	p := insightTestProject(t, s)
	now := time.Now().UTC()
	if _, err := s.db.Exec(`insert into project_insight_scans (id,project_id,status,created_at) values ('history-scan',?,'completed',?)`, p, now); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 0; i < 100; i++ {
		id, title := fmt.Sprint(i), strings.Repeat("标题", 50)+fmt.Sprint(i)
		summary := strings.Repeat("证", 200)
		if _, err := tx.Exec(`insert into project_insights (id,project_id,scan_id,type,title,summary,fingerprint,created_at,updated_at) values (?,?,'history-scan','bug',?,?,?,?,?)`,
			"finding-"+id, p, title, summary, insightFingerprint(title, summary), now, now); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`insert into tasks (id,project_id,title,description,status,created_at,updated_at) values (?,?,?,?,'todo',?,?)`,
			"task-"+id, p, title, strings.Repeat("任", 600), now, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	history, err := s.loadInsightScanHistory(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	findingRefs, taskRefs := 0, 0
	for _, ref := range history.References {
		if ref.Kind == "finding" {
			findingRefs++
		} else if ref.Kind == "task" {
			taskRefs++
		}
	}
	if !history.Truncated || len(history.Fingerprints) != 100 || findingRefs == 0 || taskRefs == 0 || len(strings.Join(history.Lines, "\n")) > 2*insightHistorySectionBytes {
		t.Fatalf("unbounded or incomplete history: truncated=%v fingerprints=%d findings=%d tasks=%d", history.Truncated, len(history.Fingerprints), findingRefs, taskRefs)
	}
}
