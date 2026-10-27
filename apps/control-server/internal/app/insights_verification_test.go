package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestInsightVerifyRepairMapsSubsetIndexesAndPreservesRejections(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	stub := &insightScriptRunner{outputs: []string{
		`{"findings":[{"type":"bug","title":"候选甲","summary":"甲"},{"type":"bug","title":"候选乙","summary":"乙"},{"type":"bug","title":"候选丙","summary":"丙"},{"type":"bug","title":"候选丁","summary":"丁"}]}`,
		`{"findings":[{"index":3,"confirmed":true,"reason":"丙存在"},{"index":1,"confirmed":false,"reason":"甲已实现"}]}`,
		`{"findings":[{"index":2,"confirmed":false,"reason":"丁已修复"},{"index":1,"confirmed":true,"reason":"乙存在"}]}`,
	}}
	server.runner = stub
	scanID := insertSyncInsightScan(t, server, projectID)
	var status, titles, rejectedJSON string
	if err := server.db.QueryRow(`select status,rejected_json from project_insight_scans where id=?`, scanID).Scan(&status, &rejectedJSON); err != nil {
		t.Fatal(err)
	}
	if err := server.db.QueryRow(`select group_concat(title,',') from (select title from project_insights where scan_id=? order by title)`, scanID).Scan(&titles); err != nil {
		t.Fatal(err)
	}
	if status != insightScanCompleted || !strings.Contains(titles, "候选乙") || !strings.Contains(titles, "候选丙") || strings.Contains(titles, "候选甲") || strings.Contains(titles, "候选丁") {
		t.Fatalf("status=%q accepted=%q", status, titles)
	}
	var rejected []InsightRejection
	if err := json.Unmarshal([]byte(rejectedJSON), &rejected); err != nil {
		t.Fatal(err)
	}
	if len(rejected) != 2 || rejected[0].Title != "候选甲" || rejected[0].Reason != "甲已实现" || rejected[1].Title != "候选丁" || rejected[1].Reason != "丁已修复" {
		t.Fatalf("rejections were mapped incorrectly: %+v", rejected)
	}
	reqs := stub.capturedRequests()
	if len(reqs) != 3 {
		t.Fatalf("agent calls=%d, want A + B + one repair", len(reqs))
	}
	for _, detail := range []string{`1. {"title":"候选乙"`, `2. {"title":"候选丁"`, "恰有 2 条", "1 到 2", "没有此前的分析上下文"} {
		if !strings.Contains(reqs[2].Prompt, detail) {
			t.Fatalf("repair missing %q: %s", detail, reqs[2].Prompt)
		}
	}
	if strings.Contains(reqs[2].Prompt, "候选甲") || strings.Contains(reqs[2].Prompt, "候选丙") {
		t.Fatal("repair repeated candidates that already had valid verdicts")
	}
}

func TestInsightVerifyIncompleteRepairFailsInsteadOfRejectingMissingCandidates(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	stub := &insightScriptRunner{outputs: []string{
		`[{"type":"bug","title":"甲","summary":"甲"},{"type":"bug","title":"乙","summary":"乙"},{"type":"bug","title":"丙","summary":"丙"}]`,
		`{"findings":[{"index":1,"confirmed":true}]}`,
		`{"findings":[{"index":1,"confirmed":true}]}`,
	}}
	server.runner = stub
	scanID := insertSyncInsightScan(t, server, projectID)
	var status, message, rejected string
	if err := server.db.QueryRow(`select status,error,rejected_json from project_insight_scans where id=?`, scanID).Scan(&status, &message, &rejected); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := server.db.QueryRow(`select count(*) from project_insights where scan_id=?`, scanID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if status != insightScanFailed || !strings.Contains(message, "核实结论数量不完整") || count != 0 {
		t.Fatalf("incomplete verification was treated as success: status=%q error=%q findings=%d", status, message, count)
	}
	if strings.Contains(rejected, "保守排除") || len(stub.capturedRequests()) != 3 {
		t.Fatalf("missing verdicts must not become rejections or trigger unlimited retries: rejected=%q calls=%d", rejected, len(stub.capturedRequests()))
	}
}

func TestInsightVerifyMalformedIndexesRetryWholeBatch(t *testing.T) {
	for _, bad := range []string{
		`{"findings":[{"index":1,"confirmed":true},{"index":1,"confirmed":false}]}`,
		`{"findings":[{"index":1,"confirmed":true},{"index":3,"confirmed":true}]}`,
		`{"findings":[{"index":1},{"index":2,"confirmed":true}]}`,
	} {
		t.Run(bad, func(t *testing.T) {
			server := newTestServer(t)
			projectID := insightTestProject(t, server)
			stub := &insightScriptRunner{outputs: []string{bad, `{"findings":[{"index":1,"confirmed":true},{"index":2,"confirmed":false,"reason":"已修复"}]}`}}
			server.runner = stub
			project, err := server.getProjectByID(context.Background(), projectID)
			if err != nil {
				t.Fatal(err)
			}
			got, err := server.runInsightVerify(context.Background(), project, "claude-code", []pendingInsight{{Title: "甲"}, {Title: "乙"}}, nil)
			if err != nil || len(got) != 2 || !got[0].Confirmed || got[1].Confirmed {
				t.Fatalf("repair outcomes=%+v err=%v", got, err)
			}
			reqs := stub.capturedRequests()
			if len(reqs) != 2 || !strings.Contains(reqs[1].Prompt, `1. {"title":"甲"`) || !strings.Contains(reqs[1].Prompt, `2. {"title":"乙"`) {
				t.Fatalf("invalid first result must retry the entire batch: %+v", reqs)
			}
		})
	}
}

func TestDecodeInsightVerdictDoesNotInventFalseVerdicts(t *testing.T) {
	for _, input := range []string{
		`{}`, `{"findings":null}`, `{"findings":[{"index":1}]}`, `{"findings":[{"index":1,"confirmed":null}]}`,
	} {
		if got, err := decodeInsightVerdict(input); err == nil {
			t.Fatalf("missing fields must not default to a negative verdict: input=%s got=%+v", input, got)
		}
	}
	got, err := decodeInsightVerdict(`{"findings":[{"index":1,"confirmed":false,"reason":"已实现"}]}`)
	if err != nil || len(got.Findings) != 1 || got.Findings[0].Confirmed || got.Findings[0].Reason != "已实现" {
		t.Fatalf("explicit negative verdict must remain valid: %+v err=%v", got, err)
	}
}

func TestInsightVerifyBatchesMapsRepairIndexesAcrossBatchBoundary(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	project, err := server.getProjectByID(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	var candidates []pendingInsight
	var firstBatch []string
	for index := 1; index <= insightVerifyBatchSize+2; index++ {
		candidates = append(candidates, pendingInsight{Title: fmt.Sprintf("候选%d", index)})
		if index <= insightVerifyBatchSize {
			firstBatch = append(firstBatch, fmt.Sprintf(`{"index":%d,"confirmed":true}`, index))
		}
	}
	stub := &insightScriptRunner{outputs: []string{
		`{"findings":[` + strings.Join(firstBatch, ",") + `]}`,
		`{"findings":[{"index":2,"confirmed":false,"reason":"尾项已修复"}]}`,
		`{"findings":[{"index":1,"confirmed":true}]}`,
	}}
	server.runner = stub
	got, err := server.runInsightVerifyBatches(context.Background(), project, "claude-code", candidates, nil)
	if err != nil || len(got) != len(candidates) {
		t.Fatalf("batch results=%+v err=%v", got, err)
	}
	for index := 1; index <= insightVerifyBatchSize+1; index++ {
		if got[index].Index != index || !got[index].Confirmed {
			t.Fatalf("candidate %d mapped incorrectly: %+v", index, got[index])
		}
	}
	last := got[insightVerifyBatchSize+2]
	if last.Index != insightVerifyBatchSize+2 || last.Confirmed || last.Reason != "尾项已修复" {
		t.Fatalf("last candidate mapped incorrectly: %+v", last)
	}
	if len(stub.capturedRequests()) != 3 {
		t.Fatalf("calls=%d, want two batches plus one repair", len(stub.capturedRequests()))
	}
}

// Check every missing-candidate pattern, including no returned verdicts and a
// complete first response. Both responses deliberately use reversed order.
func TestInsightVerifyRepairsEveryMissingSubset(t *testing.T) {
	server := newTestServer(t)
	projectID := insightTestProject(t, server)
	project, err := server.getProjectByID(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	candidates := []pendingInsight{{Title: "甲"}, {Title: "乙"}, {Title: "丙"}, {Title: "丁"}}
	for mask := 0; mask < 1<<len(candidates); mask++ {
		t.Run(fmt.Sprintf("returned_%04b", mask), func(t *testing.T) {
			var initial []string
			var missing []int
			for index := range candidates {
				if mask&(1<<index) != 0 {
					initial = append([]string{fmt.Sprintf(`{"index":%d,"confirmed":%t,"reason":"candidate %d"}`, index+1, index%2 == 0, index+1)}, initial...)
				} else {
					missing = append(missing, index)
				}
			}
			var repair []string
			for local, original := range missing {
				repair = append([]string{fmt.Sprintf(`{"index":%d,"confirmed":%t,"reason":"candidate %d"}`, local+1, original%2 == 0, original+1)}, repair...)
			}
			stub := &insightScriptRunner{outputs: []string{
				`{"findings":[` + strings.Join(initial, ",") + `]}`,
				`{"findings":[` + strings.Join(repair, ",") + `]}`,
			}}
			server.runner = stub
			got, err := server.runInsightVerify(context.Background(), project, "claude-code", candidates, nil)
			if err != nil || len(got) != len(candidates) {
				t.Fatalf("outcomes=%+v err=%v", got, err)
			}
			byIndex := make(map[int]insightVerifyOutcome, len(got))
			for _, item := range got {
				if _, duplicate := byIndex[item.Index]; duplicate {
					t.Fatalf("duplicate result index: %+v", got)
				}
				byIndex[item.Index] = item
			}
			for index := range candidates {
				item, ok := byIndex[index+1]
				if !ok || item.Confirmed != (index%2 == 0) || item.Reason != fmt.Sprintf("candidate %d", index+1) {
					t.Fatalf("candidate %d mapped incorrectly: %+v", index+1, got)
				}
			}
			wantCalls := 1
			if len(missing) > 0 {
				wantCalls = 2
			}
			if gotCalls := len(stub.capturedRequests()); gotCalls != wantCalls {
				t.Fatalf("calls=%d want %d", gotCalls, wantCalls)
			}
		})
	}
}
