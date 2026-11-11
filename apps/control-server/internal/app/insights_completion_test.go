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

func insightCompleteOutput(title string) string {
	data, _ := json.Marshal(map[string]any{"findings": []pendingInsight{{Type: insightBug, Title: title, Summary: "实际影响", Evidence: "代码路径依据", AcceptanceCriteria: "复现后检查预期结果"}}})
	return string(data)
}

func TestInsightCompletionScanContract(t *testing.T) {
	for _, raw := range []string{`null`, `{"findings":null}`, `[null]`, `[1]`, `[{"type":"unknown","title":"x","summary":"s"}]`, `[{"type":"bug","title":" ","summary":"s"}]`, `[{"type":"bug","title":"x","summary":" "}]`, `[{"type":"bug","title":"x","summary":"s"}]`, `[{"type":"bug","title":"x","summary":"s","evidence":" ","acceptanceCriteria":"check"}]`} {
		if _, err := parseInsightScanCandidates(raw, scanOpts{}); err == nil {
			t.Fatalf("invalid scan output accepted: %s", raw)
		}
	}
	for _, raw := range []string{`[]`, `{"findings":[]}`} {
		if got, err := parseInsightScanCandidates(raw, scanOpts{}); err != nil || len(got) != 0 {
			t.Fatalf("empty array rejected: %s %v", raw, err)
		}
	}
	if got, err := parseInsightScanCandidates(insightCompleteOutput("问题"), scanOpts{Types: []string{insightFeature}}); err != nil || len(got) != 0 {
		t.Fatalf("valid scoped zero result: %v %v", got, err)
	}
}

func TestInsightReviewFinalCandidateContract(t *testing.T) {
	for _, suffix := range []string{
		`[1]`, `{"findings":[1]}`, `{"findings":null}`, `{"findings":{}}`, `{"findings":"invalid"}`,
		`{"findings":[1`, `{"findings":`, `[`, `{"findings":[]`, `{"findings" : []`,
		`{"type":"bug","title":"x","summary":"s"}`, `[{"type":"bug","title":"x","summary":"s"}`,
	} {
		if _, err := parseInsightScanCandidates("[]\n"+suffix, scanOpts{}); err == nil {
			t.Errorf("malformed final result fell back to an earlier empty example: %s", suffix)
		}
	}
	for _, suffix := range []string{"", `{"debug":"unrelated"}`} {
		items, err := parseInsightScanCandidates("[]\n"+insightCompleteOutput("最终候选")+"\n"+suffix, scanOpts{})
		if err != nil || len(items) != 1 || items[0].Title != "最终候选" {
			t.Fatalf("valid final contract lost: items=%+v err=%v", items, err)
		}
	}
}

func TestInsightCompletionRetrySharesOneBudget(t *testing.T) {
	s := newTestServer(t)
	p := insightTestProject(t, s)
	missing := `[{"type":"bug","title":"问题","summary":"影响"}]`
	for _, tc := range []struct {
		name            string
		outputs         []string
		status          string
		calls, findings int
	}{
		{"missing twice", []string{missing, missing}, insightScanFailed, 2, 0},
		{"bad json then missing", []string{"bad json", missing}, insightScanFailed, 2, 0},
		{"invalid final result twice", []string{"[]\n{\"findings\":[1]}", "[]\n{\"findings\":[1]}"}, insightScanFailed, 2, 0},
		{"repair complete", []string{missing, insightCompleteOutput("问题"), `{"findings":[{"index":1,"confirmed":true}]}`}, insightScanCompleted, 3, 1},
		{"zero", []string{`{"findings":[]}`}, insightScanCompleted, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.db.Exec(`delete from project_insights where project_id=?`, p); err != nil {
				t.Fatal(err)
			}
			r := &insightScriptRunner{outputs: tc.outputs}
			s.runner = r
			id := insertSyncInsightScan(t, s, p)
			var status string
			var count int
			if err := s.db.QueryRow(`select status,findings_count from project_insight_scans where id=?`, id).Scan(&status, &count); err != nil {
				t.Fatal(err)
			}
			if status != tc.status || count != tc.findings || len(r.capturedRequests()) != tc.calls {
				t.Fatalf("status=%s count=%d calls=%d", status, count, len(r.capturedRequests()))
			}
			if tc.calls > 1 && !strings.Contains(r.capturedRequests()[1].Prompt, "验收") {
				t.Fatal("repair lost contract")
			}
		})
	}
}

func TestInsightCompletionCrossBatchAndRepairContext(t *testing.T) {
	s := newTestServer(t)
	p := insightTestProject(t, s)
	project, err := s.getProjectByID(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	var candidates []pendingInsight
	var first []string
	for i := 1; i <= 22; i++ {
		candidates = append(candidates, pendingInsight{Type: insightBug, Title: fmt.Sprintf("独立问题%d", i), Summary: "不同范围"})
		if i <= 20 {
			first = append(first, fmt.Sprintf(`{"index":%d,"confirmed":true}`, i))
		}
	}
	r := &insightScriptRunner{outputs: []string{`{"findings":[` + strings.Join(first, ",") + `]}`, `{"findings":[{"index":2,"confirmed":true}]}`, `{"findings":[{"index":1,"confirmed":false,"reason":"与本轮编号1重复"}]}`}}
	s.runner = r
	got, err := s.runInsightVerifyBatches(context.Background(), project, "claude-code", candidates, nil)
	if err != nil || got[21].Confirmed || !got[22].Confirmed {
		t.Fatalf("batch outcome: %+v %v", got, err)
	}
	reqs := r.capturedRequests()
	if len(reqs) != 3 {
		t.Fatalf("unexpected calls %d", len(reqs))
	}
	for _, index := range []int{1, 2} {
		_, refs, ok := strings.Cut(reqs[index].Prompt, "本轮此前已确认候选")
		if !ok || !strings.Contains(refs, "独立问题1") {
			t.Fatal("later batch/repair lost prior context")
		}
	}
	_, refs, _ := strings.Cut(reqs[2].Prompt, "本轮此前已确认候选")
	if !strings.Contains(refs, "独立问题22") {
		t.Fatal("missing-item repair lost retained confirmations")
	}
	var large []insightCurrentReference
	for i := 0; i < 30; i++ {
		large = append(large, insightCurrentReference{i + 1, pendingInsight{Type: insightBug, Title: strings.Repeat("\x00", 1000), Summary: strings.Repeat("中", 2000), FileHint: strings.Repeat("路径", 1000)}})
	}
	prompt := insightCurrentPrompt(large)
	if len(prompt) > 32<<10 || strings.Count(prompt, `"本轮编号"`) != 30 {
		t.Fatalf("reference budget: %d", len(prompt))
	}
}

func insightCompletionSeed(t *testing.T, s *Server, p, id, title string) InsightFinding {
	t.Helper()
	now := time.Now().UTC()
	if _, err := s.db.Exec(`insert or ignore into project_insight_scans (id,project_id,status,created_at) values (?,?,'completed',?)`, "completion-"+p, p, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`insert into project_insights (id,project_id,scan_id,type,title,summary,fingerprint,created_at,updated_at) values (?,?,?,'bug',?,'影响',?,?,?)`, id, p, "completion-"+p, title, insightFingerprint(title, "影响"), now, now); err != nil {
		t.Fatal(err)
	}
	f, ok := s.loadInsightFinding(context.Background(), p, id)
	if !ok {
		t.Fatal("missing seed")
	}
	return f
}

func insightCompletionEdit(t *testing.T, s *Server, p string, f InsightFinding, title string, want int) InsightFinding {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"title": title})
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/api/projects/"+p+"/insights/"+f.ID, strings.NewReader(string(body))))
	if rec.Code != want {
		t.Fatalf("edit %s: %d %s", title, rec.Code, rec.Body.String())
	}
	current, ok := s.loadInsightFinding(context.Background(), p, f.ID)
	if !ok {
		t.Fatal("edited finding disappeared")
	}
	return current
}

func insightCompletionState(t *testing.T, s *Server, p string) *insightDedupState {
	t.Helper()
	d, err := loadInsightDedupState(context.Background(), s.db, p)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestInsightCompletionAliasEditingAndConflicts(t *testing.T) {
	s := newTestServer(t)
	p := insightTestProject(t, s)
	ctx := context.Background()
	f := insightCompletionSeed(t, s, p, "first", "A")
	a := insightFingerprint("A", "影响")
	f = insightCompletionEdit(t, s, p, f, "B", 200)
	b := insightFingerprint("B", "影响")
	f = insightCompletionEdit(t, s, p, f, "C", 200)
	c := insightFingerprint("C", "影响")
	d := insightCompletionState(t, s, p)
	if d.aliases[a] != c || d.aliases[b] != c || !d.suppressed(a) {
		t.Fatalf("not flattened: %+v", d)
	}
	f = insightCompletionEdit(t, s, p, f, "A", 200)
	d = insightCompletionState(t, s, p)
	if d.aliases[a] != "" || d.aliases[b] != a || d.aliases[c] != a {
		t.Fatalf("revert produced chain: %+v", d.aliases)
	}
	x := insightCompletionSeed(t, s, p, "other", "X")
	x = insightCompletionEdit(t, s, p, x, "Y", 200)
	f = insightCompletionEdit(t, s, p, f, "X", 409)
	if f.Title != "A" {
		t.Fatal("conflict changed finding")
	}
	if _, err := s.db.Exec(`update project_insights set verification_result='invalid' where id=?`, x.ID); err != nil {
		t.Fatal(err)
	}
	f = insightCompletionEdit(t, s, p, f, "X", 200)
	d = insightCompletionState(t, s, p)
	xfp := insightFingerprint("X", "影响")
	if d.aliases[xfp] != "" || d.aliases[a] != xfp || d.aliases[b] != xfp {
		t.Fatalf("expired target not detached: %+v", d.aliases)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareInsightAliasEdit(ctx, tx, p, xfp, "rollback-target", time.Now().UTC()); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	tx.Rollback()
	if insightCompletionState(t, s, p).aliases[xfp] != "" {
		t.Fatal("rollback leaked alias")
	}
	if _, err := s.db.Exec(`update project_insights set verification_result='invalid',status='dismissed' where id=?`, f.ID); err != nil {
		t.Fatal(err)
	}
	if !insightCompletionState(t, s, p).suppressed(a) {
		t.Fatal("explicit dismissal lost alias protection")
	}
	if _, err := s.db.Exec(`update project_insights set status='open' where id=?`, f.ID); err != nil {
		t.Fatal(err)
	}
	if insightCompletionState(t, s, p).suppressed(a) {
		t.Fatal("invalid finding permanently suppressed old spelling")
	}
}

func TestInsightCompletionAliasTaskLifecycle(t *testing.T) {
	for _, terminal := range []string{taskDone, taskCancelled} {
		t.Run(terminal, func(t *testing.T) {
			s := newTestServer(t)
			p := insightTestProject(t, s)
			ctx := context.Background()
			f := insightCompletionSeed(t, s, p, "original", "A")
			a := insightFingerprint("A", "影响")
			f = insightCompletionEdit(t, s, p, f, "B", 200)
			task, ok, err := s.convertInsightToTask(ctx, p, f)
			if err != nil || !ok {
				t.Fatalf("convert: %v %v", ok, err)
			}
			if !insightCompletionState(t, s, p).suppressed(a) {
				t.Fatal("active task did not suppress old spelling")
			}
			if _, err := s.db.Exec(`update tasks set status=? where id=?`, terminal, task.ID); err != nil {
				t.Fatal(err)
			}
			if insightCompletionState(t, s, p).suppressed(a) {
				t.Fatal("terminal task permanently suppressed old spelling")
			}
			// Actual publication of an old spelling clears its expired mapping.
			raw := `[{"type":"bug","title":"A","summary":"影响","evidence":"code","acceptanceCriteria":"check"}]`
			s.runner = &insightScriptRunner{outputs: []string{raw, `{"findings":[{"index":1,"confirmed":true}]}`}}
			id := insertSyncInsightScan(t, s, p)
			var count int
			if err := s.db.QueryRow(`select findings_count from project_insight_scans where id=?`, id).Scan(&count); err != nil || count != 1 {
				t.Fatalf("rediscovery %d %v", count, err)
			}
			if insightCompletionState(t, s, p).aliases[a] != "" {
				t.Fatal("republishing old spelling retained stale alias")
			}
			insightCompletionSeed(t, s, p, "later-B", "B")
			if insightCompletionState(t, s, p).root(a) != a {
				t.Fatal("expired mapping reactivated after target returned")
			}
		})
	}
}

func TestInsightCompletionEarlierTaskAndStaleConversion(t *testing.T) {
	s := newTestServer(t)
	p := insightTestProject(t, s)
	ctx := context.Background()
	f := insightCompletionSeed(t, s, p, "original", "A")
	a := insightFingerprint("A", "影响")
	f = insightCompletionEdit(t, s, p, f, "B", 200)
	now := time.Now().UTC()
	for i, state := range []string{taskRunning, taskDone} {
		fp := a
		if i == 1 {
			fp = insightFingerprint("B", "影响")
		}
		if _, err := s.db.Exec(`insert into tasks (id,project_id,title,status,source_insight_fingerprint,created_at,updated_at) values (?,?,?,?,?,?,?)`, fmt.Sprint("old-task-", i), p, state, state, fp, now.Add(time.Duration(i)*time.Hour), now); err != nil {
			t.Fatal(err)
		}
	}
	f = insightCompletionEdit(t, s, p, f, "C", 200)
	d := insightCompletionState(t, s, p)
	if !d.activeTask(a) || !d.activeTask(insightFingerprint("C", "影响")) {
		t.Fatal("old-spelling task omitted")
	}
	linked := s.loadInsightLinkedTasks(ctx, p)
	if linked[insightFingerprint("C", "影响")].Status != taskRunning {
		t.Fatal("new terminal task hid older active task")
	}
	if _, ok, err := s.convertInsightToTask(ctx, p, f); err != nil || ok {
		t.Fatalf("duplicated alias task %v %v", ok, err)
	}
	if _, err := s.db.Exec(`update tasks set status='done' where project_id=?`, p); err != nil {
		t.Fatal(err)
	}
	stale := f
	f = insightCompletionEdit(t, s, p, f, "D", 200)
	if _, ok, err := s.convertInsightToTask(ctx, p, stale); err != nil || ok {
		t.Fatalf("converted stale content: %v %v", ok, err)
	}
	current, exists := s.loadInsightFinding(ctx, p, f.ID)
	if !exists || current.Title != "D" {
		t.Fatal("stale conversion removed new card")
	}
	if _, err := s.db.Exec(`delete from project_insights where id=?`, f.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.convertInsightToTask(ctx, p, f); err != nil || ok {
		t.Fatalf("converted deleted card: %v %v", ok, err)
	}
}

func TestInsightCompletionLegacySuppressionAndIsolation(t *testing.T) {
	s := newTestServer(t)
	p := insightTestProject(t, s)
	other := insightTestProjectID(t, s, "other-insight-project")
	for _, row := range []struct{ fp, reason, canonical string }{{"legacy", insightSuppressionSuperseded, ""}, {"unknown", "custom", ""}, {"old", insightSuppressionSuperseded, "root"}} {
		if _, err := s.db.Exec(`insert into project_insight_suppressions (project_id,fingerprint,reason,canonical_fingerprint,created_at) values (?,?,?,?,?)`, p, row.fp, row.reason, row.canonical, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`insert into tasks (id,project_id,title,status,source_insight_fingerprint,created_at,updated_at) values ('foreign',?,'foreign','running','root',?,?)`, other, time.Now().UTC(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	d := insightCompletionState(t, s, p)
	if d.suppressed("legacy") || d.suppressed("old") || !d.suppressed("unknown") {
		t.Fatalf("legacy or cross-project semantics: %+v", d)
	}
}

func TestInsightReviewMigratesLegacyColumns(t *testing.T) {
	s := newTestServer(t)
	p := insightTestProject(t, s)
	f := insightCompletionSeed(t, s, p, "legacy", "保留旧建议")
	if _, err := s.db.Exec(`insert into project_insight_suppressions(project_id,fingerprint,reason,created_at) values (?,'old',?,?)`, p, insightSuppressionSuperseded, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`alter table project_insights drop column evidence`,
		`alter table project_insights drop column acceptance_criteria`,
		`drop index insight_suppressions_canonical`,
		`alter table project_insight_suppressions drop column canonical_fingerprint`,
	} {
		if _, err := s.db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	for i := 0; i < 2; i++ {
		reopened, err := New(context.Background(), s.config)
		if err != nil {
			t.Fatal(err)
		}
		current, exists := reopened.loadInsightFinding(context.Background(), p, f.ID)
		d, err := loadInsightDedupState(context.Background(), reopened.db, p)
		reopened.Close()
		if err != nil || !exists || current.Title != f.Title || current.Evidence != "" || current.AcceptanceCriteria != "" || d.suppressed("old") {
			t.Fatalf("legacy migration/restart %d: finding=%+v exists=%v err=%v", i, current, exists, err)
		}
	}
}

type insightCompletionHookRunner struct {
	*insightScriptRunner
	before func()
}

func (r *insightCompletionHookRunner) Run(ctx context.Context, req AgentRunRequest, sink AgentRunSink) error {
	if r.before != nil {
		hook := r.before
		r.before = nil
		hook()
	}
	return r.insightScriptRunner.Run(ctx, req, sink)
}

func TestInsightCompletionPublishRechecksNewAlias(t *testing.T) {
	s := newTestServer(t)
	p := insightTestProject(t, s)
	r := &insightCompletionHookRunner{insightScriptRunner: &insightScriptRunner{outputs: []string{
		`[{"type":"bug","title":"A","summary":"影响","evidence":"code","acceptanceCriteria":"check"}]`,
		`{"findings":[{"index":1,"confirmed":true}]}`,
	}}}
	r.before = func() {
		f := insightCompletionSeed(t, s, p, "concurrent", "A")
		insightCompletionEdit(t, s, p, f, "B", http.StatusOK)
	}
	s.runner = r
	id := insertSyncInsightScan(t, s, p)
	var status string
	var count, suppressed, cards int
	if err := s.db.QueryRow(`select status,findings_count,suppressed_count from project_insight_scans where id=?`, id).Scan(&status, &count, &suppressed); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`select count(*) from project_insights where project_id=?`, p).Scan(&cards); err != nil {
		t.Fatal(err)
	}
	if status != insightScanCompleted || count != 0 || suppressed != 1 || cards != 1 {
		t.Fatalf("publication missed new alias: status=%s new=%d suppressed=%d cards=%d", status, count, suppressed, cards)
	}
}

func TestInsightCompletionPublishRollbackAndRevival(t *testing.T) {
	s := newTestServer(t)
	p := insightTestProject(t, s)
	f := insightCompletionSeed(t, s, p, "original", "A")
	f = insightCompletionEdit(t, s, p, f, "B", http.StatusOK)
	if _, err := s.db.Exec(`update project_insights set verification_result='invalid' where id=?`, f.ID); err != nil {
		t.Fatal(err)
	}
	a, b := insightFingerprint("A", "影响"), insightFingerprint("B", "影响")
	if _, err := s.db.Exec(`create trigger fail_insight_completion before insert on project_insights begin select raise(abort,'injected publication failure'); end`); err != nil {
		t.Fatal(err)
	}
	outputs := []string{`[{"type":"bug","title":"A","summary":"影响","evidence":"code","acceptanceCriteria":"check"}]`, `{"findings":[{"index":1,"confirmed":true}]}`}
	s.runner = &insightScriptRunner{outputs: outputs}
	id := insertSyncInsightScan(t, s, p)
	var status string
	if err := s.db.QueryRow(`select status from project_insight_scans where id=?`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != insightScanFailed || insightCompletionState(t, s, p).aliases[a] != b {
		t.Fatal("failed publication did not roll back alias detachment")
	}
	if _, err := s.db.Exec(`drop trigger fail_insight_completion`); err != nil {
		t.Fatal(err)
	}
	// A stored invalid card exercises the upsert/revival path as well as insertion.
	old := insightCompletionSeed(t, s, p, "old-A", "A")
	if _, err := s.db.Exec(`update project_insights set verification_result='invalid' where id=?`, old.ID); err != nil {
		t.Fatal(err)
	}
	s.runner = &insightScriptRunner{outputs: outputs}
	id = insertSyncInsightScan(t, s, p)
	var count int
	if err := s.db.QueryRow(`select status,findings_count from project_insight_scans where id=?`, id).Scan(&status, &count); err != nil {
		t.Fatal(err)
	}
	current, exists := s.loadInsightFinding(context.Background(), p, old.ID)
	if status != insightScanCompleted || count != 1 || !exists || current.VerificationResult != "" || insightCompletionState(t, s, p).aliases[a] != "" {
		t.Fatal("revival failed to detach expired alias atomically")
	}
}

type insightCompletionHookBody struct {
	*strings.Reader
	before func()
}

func (b *insightCompletionHookBody) Read(data []byte) (int, error) {
	if b.before != nil {
		hook := b.before
		b.before = nil
		hook()
	}
	return b.Reader.Read(data)
}

func TestInsightCompletionConcurrentEditConflict(t *testing.T) {
	s := newTestServer(t)
	p := insightTestProject(t, s)
	f := insightCompletionSeed(t, s, p, "original", "A")
	body := &insightCompletionHookBody{Reader: strings.NewReader(`{"title":"stale"}`), before: func() {
		insightCompletionEdit(t, s, p, f, "B", http.StatusOK)
	}}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/api/projects/"+p+"/insights/"+f.ID, body))
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale edit: %d %s", rec.Code, rec.Body.String())
	}
	current, exists := s.loadInsightFinding(context.Background(), p, f.ID)
	d := insightCompletionState(t, s, p)
	if !exists || current.Title != "B" || d.aliases[insightFingerprint("A", "影响")] != insightFingerprint("B", "影响") || d.aliases[insightFingerprint("B", "影响")] != "" {
		t.Fatal("stale edit overwrote content or left orphaned aliases")
	}
}

func TestInsightCompletionFoldedRowLimitNotice(t *testing.T) {
	s := newTestServer(t)
	p := insightTestProject(t, s)
	f := insightCompletionSeed(t, s, p, "first", "first")
	// Compact rows exceed the row limit without necessarily reaching the byte limit.
	for _, status := range []string{insightStatusOpen, insightStatusDismissed} {
		if _, err := s.db.Exec(`delete from project_insights where project_id=? and id<>?`, p, f.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`with recursive n(i) as (select 1 union all select i+1 from n where i<?)
			insert into project_insights(id,project_id,scan_id,type,title,summary,fingerprint,status,verification_result,created_at,updated_at)
			select 'row-'||i,?,?,'bug','x','s','fp-'||i,?,'invalid',?,? from n`, insightFindingsListLimit+1, p, f.ScanID, status, time.Now().UTC(), time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/projects/"+p+"/insights", nil))
		resp := insightDecode[insightsResponse](t, rec, http.StatusOK)
		if !resp.FoldedTruncated {
			t.Fatalf("folded row limit hidden for status=%s", status)
		}
	}
}
