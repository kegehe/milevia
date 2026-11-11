package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Explicit opt-in: uses local Claude credentials and real model calls on a temporary project.
func TestInsightLiveAcceptance(t *testing.T) {
	if os.Getenv("MILEVIA_TEST_LIVE_INSIGHTS") != "1" {
		t.Skip("set MILEVIA_TEST_LIVE_INSIGHTS=1 to run the real-model acceptance check")
	}
	s := newTestServer(t)
	p := insightTestProject(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	project, err := s.getProjectByID(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	s.runner = newClaudeCLIRunner(s.config, nil)
	files := map[string]string{
		"README.md":  "# 轻记账\n面向个人的离线浏览器记账工具。当前已有记录列表和总额展示；总额应是所有记录金额之和，空列表为0。方向是帮助用户了解自己的支出，可考虑按月份或分类查看，不做云同步、团队协作、社交或服务端。无构建工具，直接通过静态服务器打开 index.html。后续功能需保持轻量，以当前代码证据判断是否值得做。\n",
		"totals.js":  "export function sumExpenses(items) { return items.reduce((sum, item) => sum + item.amount, 1); }\n",
		"main.js":    "import { sumExpenses } from './totals.js';\nconst expenses = [{amount: 10, category: '餐饮', date: '2026-11-01'}, {amount: 20, category: '交通', date: '2026-11-02'}];\ndocument.querySelector('#total').textContent = String(sumExpenses(expenses));\ndocument.querySelector('#records').textContent = expenses.map(item => `${item.date} ${item.category} ${item.amount}`).join('\\n');\n",
		"index.html": "<!doctype html><html lang=\"zh-CN\"><meta charset=\"utf-8\"><title>轻记账</title><h1>支出记录</h1><p>总额：<span id=\"total\"></span></p><pre id=\"records\"></pre><script type=\"module\" src=\"main.js\"></script></html>\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(project.Path, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	scan := func() {
		id := "live-" + time.Now().Format("150405.000000000")
		if _, err := s.db.Exec(`insert into project_insight_scans(id,project_id,status,created_at,started_at) values (?,?,'running',?,?)`, id, p, time.Now().UTC(), time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		s.runProjectInsightScan(ctx, p, id, scanOpts{Agent: "claude-code", Types: []string{insightBug, insightFeature}})
		var status, message string
		var count int
		if err := s.db.QueryRow(`select status,coalesce(error,''),findings_count from project_insight_scans where id=?`, id).Scan(&status, &message, &count); err != nil {
			t.Fatal(err)
		}
		t.Logf("live scan status=%s new=%d error=%s", status, count, message)
		if status != insightScanCompleted {
			t.Fatalf("scan failed: %s", message)
		}
	}
	scan()
	rows, err := s.db.Query(`select `+insightFindingColumns+` from project_insights where project_id=?`, p)
	if err != nil {
		t.Fatal(err)
	}
	var bug InsightFinding
	var features []InsightFinding
	for rows.Next() {
		f, err := scanInsightFinding(rows.Scan)
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		t.Logf("live finding type=%s title=%s evidence=%s acceptance=%s", f.Type, f.Title, f.Evidence, f.AcceptanceCriteria)
		if strings.TrimSpace(f.Evidence) == "" || strings.TrimSpace(f.AcceptanceCriteria) == "" {
			rows.Close()
			t.Fatal("empty details")
		}
		if f.Type == insightBug && strings.Contains(f.Evidence+f.Summary+f.FileHint, "totals") && strings.Contains(f.Title+f.Summary, "空列表") {
			bug = f
		}
		if f.Type == insightFeature {
			features = append(features, f)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	if bug.ID == "" {
		t.Fatal("known total bug was not found")
	}
	if err := os.WriteFile(filepath.Join(project.Path, "totals.js"), []byte("export function sumExpenses(items) { return items.reduce((sum, item) => sum + item.amount, 0); }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	targets, err := s.resolveVerifyTargets(ctx, p, []string{bug.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range targets {
		s.setInsightVerification(ctx, p, f.ID, insightVerifyPending, "", time.Now().UTC())
	}
	s.runInsightFindingsVerify(ctx, p, targets)
	current, exists := s.loadInsightFinding(ctx, p, bug.ID)
	t.Logf("live repaired bug verification=%s reason=%s", current.VerificationResult, current.VerificationNote)
	if !exists || current.VerificationResult != insightVerifyInvalid {
		t.Fatal("fixed issue did not leave active list")
	}
	scan()
	current, _ = s.loadInsightFinding(ctx, p, bug.ID)
	if current.VerificationResult != insightVerifyInvalid {
		t.Fatal("fixed issue was revived")
	}
	t.Logf("live feature suggestions=%d (zero is allowed)", len(features))
}
