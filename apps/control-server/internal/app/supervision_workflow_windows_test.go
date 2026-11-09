//go:build windows

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func workflowProposalForTest(d supervisionDecision, action any) string {
	return string(mustJSON(map[string]any{"schemaVersion": 1, "decisionId": d.ID, "expectedStateVersion": d.ExpectedStateVersion, "basisSeq": d.BasisSeq, "reason": "依据任务和真实证据推进", "action": action}))
}

// 模型替身只提供结构化决策和文件内容；测试通过生产执行器启动真实 Node 进程。
func TestSupervisionWorkflowAppliesFilesRunsRealCheckReviewsAndReleases(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node unavailable")
	}
	s, p, taskID, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	writeSupervisionSourceForTest(t, p.Path, "src/math.cjs", "module.exports=(a,b)=>a-b;\n")
	writeSupervisionSourceForTest(t, p.Path, "src/check.cjs", "const assert=require('node:assert/strict');assert.equal(require('./math.cjs')(2,3),5);console.log('behavior checked');\n")
	if _, err := s.db.Exec(`update supervision_sessions set scheduler_agent_id='claude-code' where id=?`, ss.ID); err != nil {
		t.Fatal(err)
	}
	startSupervisionForTest(t, s, ss.ID, now)
	ctx := context.Background()
	var schedulerCalls, roleCalls int
	w := supervisionWorkerForTest(s, func(ctx context.Context, d supervisionDecision, lease supervisionLease) error {
		var role string
		if err := s.db.QueryRow(`select role from supervision_work_items where execution_id=?`, d.ExecutionID).Scan(&role); err != nil {
			return err
		}
		if role != "scheduler" {
			roleCalls++
			deadline, err := s.claimSupervisionDecisionLaunch(ctx, d.ID, lease, time.Now().UTC())
			if err != nil {
				return err
			}
			permit, err := s.prepareSupervisionExecution(ctx, d, lease, time.Now().UTC())
			if err != nil {
				return err
			}
			if role == "test" {
				if err := s.executeSupervisionVerification(ctx, permit, deadline); err != nil {
					return err
				}
				before := supervisionSessionForTest(t, s, ss.ID)
				if err := s.executeSupervisionVerification(ctx, permit, deadline); err == nil {
					return fmt.Errorf("same verification execution was started twice")
				}
				after := supervisionSessionForTest(t, s, ss.ID)
				if after.UsedTurns != before.UsedTurns || after.ReservedTurns != before.ReservedTurns {
					return fmt.Errorf("duplicate verification consumed budget")
				}
				return nil
			}
			raw := ""
			if role == "implementation" {
				raw = string(mustJSON(supervisionImplementationResult{Summary: "修复加法", Edits: []supervisionEdit{{Path: "src/math.cjs", BaseSHA256: supervisionSHA256([]byte("module.exports=(a,b)=>a-b;\n")), Content: "module.exports=(a,b)=>a+b;\n"}}}))
			} else {
				input, _, err := s.supervisionSchedulerInput(ctx, d.ExecutionID)
				if err != nil {
					return err
				}
				var reviewInput supervisionRoleInput
				if err := json.Unmarshal(input, &reviewInput); err != nil {
					return err
				}
				currentCheck, baselineCheck := false, false
				for _, source := range reviewInput.Sources {
					currentCheck = currentCheck || source.Path == "src/check.cjs" && source.Content != "" && !source.Truncated
				}
				for _, source := range reviewInput.BaselineSources {
					baselineCheck = baselineCheck || source.Path == "src/check.cjs" && source.Content != "" && !source.Truncated
				}
				if !currentCheck || !baselineCheck || reviewInput.ManifestTruncated || reviewInput.ManifestTotal != len(reviewInput.ProjectContext.Manifest.Entries) {
					return fmt.Errorf("review lost verification source outside edit scope or falsely truncated manifest")
				}
				if len(reviewInput.WorkspaceChanges) != 1 || reviewInput.WorkspaceChanges[0].Path != "src/math.cjs" || len(reviewInput.Plan.Plan.Scope) != 1 || reviewInput.Plan.Plan.Scope[0] != "src/math.cjs" {
					return fmt.Errorf("review context broadened write scope or lost batch changes")
				}
				raw = string(mustJSON(supervisionReviewResult{Verdict: "pass", Summary: "已核对源码及实际测试证据", Points: []supervisionReviewPoint{{ID: "behavior", Passed: true, Reason: "2+3真实命令断言通过"}}, Blockers: []string{}}))
			}
			if err := s.finishSupervisionExecution(ctx, permit, supervisionSchedulerCLIReceipt{ContainerQuiet: true}, nil, time.Now().UTC()); err != nil {
				return err
			}
			return s.completeSupervisionRole(ctx, permit, raw, nil)
		}
		schedulerCalls++
		current := supervisionSessionForTest(t, s, ss.ID)
		pc, err := loadSupervisionProjectContext(ctx, s.db, ss.ID, "current")
		if err != nil {
			return err
		}
		var phase string
		if err := s.db.QueryRow(`select phase from supervision_tasks where session_id=?`, ss.ID).Scan(&phase); err != nil {
			return err
		}
		action := supervisionWorkflowAction{SessionID: ss.ID, TaskID: taskID, RequirementsRevision: 1, PlanRevision: 1, ContextID: pc.ID, Instruction: "依据当前任务和证据实施或审查"}
		var proposal any = action
		switch phase {
		case "pending":
			plan := validSupervisionPlanActionForTest(current, taskID, pc)
			plan.Plan.Scope = []string{"src/math.cjs"}
			proposal = plan
		case "ready":
			action.Type = "dispatch_implementation"
			proposal = action
		case "implemented":
			action.Type = "request_verification"
			action.Checks = []supervisionCheck{{ID: "behavior-real", AcceptanceIDs: []string{"behavior"}, Program: "node", Args: []string{"src/check.cjs"}, Directory: ".", TimeoutMillis: 30000}}
			proposal = action
		case "verified":
			action.Type = "request_review"
			proposal = action
		case "reviewed":
			action.Type = "record_acceptance"
			proposal = action
		default:
			return fmt.Errorf("unexpected phase %s", phase)
		}
		return finishWorkerDecisionForTest(s, d, lease, workflowProposalForTest(d, proposal))
	})
	for i := 0; i < 12; i++ {
		if err := w.process(ctx, ss.ID); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		if supervisionTerminal(supervisionSessionForTest(t, s, ss.ID).State) {
			break
		}
	}
	current := supervisionSessionForTest(t, s, ss.ID)
	if current.State != "completed" || schedulerCalls != 5 || roleCalls != 3 {
		t.Fatalf("state=%+v scheduler=%d roles=%d", current, schedulerCalls, roleCalls)
	}
	data, err := os.ReadFile(filepath.Join(p.Path, "src", "math.cjs"))
	if err != nil || string(data) != "module.exports=(a,b)=>a+b;\n" {
		t.Fatalf("files=%q %v", data, err)
	}
	var status string
	if err := s.db.QueryRow(`select status from tasks where id=?`, taskID).Scan(&status); err != nil || status != "done" {
		t.Fatalf("status=%s %v", status, err)
	}
	var details string
	if err := s.db.QueryRow(`select details from supervision_evidence where session_id=? and kind='command'`, ss.ID).Scan(&details); err != nil {
		t.Fatal(err)
	}
	var evidence supervisionCheckResult
	if err := json.Unmarshal([]byte(details), &evidence); err != nil || evidence.ExitCode != 0 || evidence.Stdout != "behavior checked\n" {
		t.Fatalf("actual check=%s %v", details, err)
	}
	assertExecutionClaimForTest(t, s, taskID, "", "")
	var admissions int
	if err := s.db.QueryRow(`select count(*) from workspace_admissions where owner_id=?`, ss.ID).Scan(&admissions); err != nil || admissions != 0 {
		t.Fatalf("admissions=%d %v", admissions, err)
	}
}

func TestSupervisionWorkflowInstalledCLIEndToEnd(t *testing.T) {
	if os.Getenv("MILEVIA_RUN_SUPERVISION_WORKFLOW_LIVE") != "1" {
		t.Skip("explicit live CLI experiment")
	}
	binary, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal(err)
	}
	s, p, taskID, ss, _ := supervisionFixture(t, defaultSupervisionBudget())
	writeSupervisionSourceForTest(t, p.Path, "src/math.cjs", "module.exports=(a,b)=>a-b;\n")
	writeSupervisionSourceForTest(t, p.Path, "src/check.cjs", "const assert=require('node:assert/strict');const add=require('./math.cjs');assert.equal(add(2,3),5);assert.equal(add(-2,3),1);console.log('behavior checked');\n")
	if _, err = s.db.Exec(`update tasks set title='修复加法函数',description='修复src/math.cjs的加法函数，使2+3=5和-2+3=1。保留src/check.cjs中的实际断言，必须运行node src/check.cjs验证，独立审查后验收。无其他需求。' where id=?;update supervision_sessions set scheduler_agent_id='claude-code' where id=?`, taskID, ss.ID); err != nil {
		t.Fatal(err)
	}
	s.runner = &claudeCLIRunner{config: Config{ClaudePath: binary}}
	supervisionHTTPForTest(t, s, http.MethodPost, "/api/supervision/"+ss.ID+"/start", map[string]any{"stateVersion": ss.StateVersion}, http.StatusAccepted)
	w := supervisionWorkerForTest(s, s.executeSupervisionWorkerDecision)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	for i := 0; i < 22; i++ {
		if err := w.process(ctx, ss.ID); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		current := supervisionSessionForTest(t, s, ss.ID)
		t.Logf("round %d: %s %s turns=%d", i, current.State, current.Reason, current.UsedTurns)
		if supervisionTerminal(current.State) {
			break
		}
		if current.State == "needs_input" || current.State == "paused" {
			t.Fatalf("blocked: %+v", current)
		}
	}
	current := supervisionSessionForTest(t, s, ss.ID)
	if current.State != "completed" {
		t.Fatalf("not completed: %+v", current)
	}
	var checks int
	if err := s.db.QueryRow(`select count(*) from supervision_evidence where session_id=? and kind='command' and passed=1`, ss.ID).Scan(&checks); err != nil || checks < 1 {
		t.Fatalf("no real passing check: %d %v", checks, err)
	}
	content, err := os.ReadFile(filepath.Join(p.Path, "src", "math.cjs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("actual implemented source: %s", content)
	assertExecutionClaimForTest(t, s, taskID, "", "")
}
