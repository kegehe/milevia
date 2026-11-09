//go:build windows

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func applyWorkflowActionForTest(t *testing.T, s *Server, id string, a any) {
	t.Helper()
	lease, d := decisionForTest(t, s, id, time.Now().UTC())
	if err := finishWorkerDecisionForTest(s, d, lease, workflowProposalForTest(d, a)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.applySupervisionAction(context.Background(), d.ID, lease); err != nil {
		t.Fatal(err)
	}
	var state, reason string
	if err := s.db.QueryRow(`select state,validation_error from supervision_decisions where id=?`, d.ID).Scan(&state, &reason); err != nil {
		t.Fatal(err)
	}
	if state == "invalid" {
		t.Fatalf("action rejected: %s", reason)
	}
}

func workflowPermitForTest(t *testing.T, s *Server, id string) (supervisionExecutionPermit, time.Time) {
	t.Helper()
	var decisionID string
	if err := s.db.QueryRow(`select id from supervision_decisions where session_id=? and state='pending'`, id).Scan(&decisionID); err != nil {
		t.Fatal(err)
	}
	d, err := loadSupervisionDecision(context.Background(), s.db, decisionID)
	if err != nil {
		t.Fatal(err)
	}
	current := supervisionSessionForTest(t, s, id)
	lease := supervisionLease{Owner: current.LeaseOwner, Epoch: current.LeaseEpoch}
	deadline, err := s.claimSupervisionDecisionLaunch(context.Background(), d.ID, lease, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	permit, err := s.prepareSupervisionExecution(context.Background(), d, lease, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return permit, deadline
}

func implementedWorkflowForTest(t *testing.T) (*Server, Project, string, supervisionSession, supervisionProjectContext, string) {
	t.Helper()
	s, p, taskID, ss, pc := supervisionPlanFixture(t)
	if _, err := s.db.Exec(`update supervision_sessions set scheduler_agent_id='claude-code' where id=?`, ss.ID); err != nil {
		t.Fatal(err)
	}
	applyWorkflowActionForTest(t, s, ss.ID, validSupervisionPlanActionForTest(ss, taskID, pc))
	applyWorkflowActionForTest(t, s, ss.ID, supervisionWorkflowAction{Type: "dispatch_implementation", SessionID: ss.ID, TaskID: taskID, RequirementsRevision: 1, PlanRevision: 1, ContextID: pc.ID, Instruction: "核对现有实现"})
	permit, _ := workflowPermitForTest(t, s, ss.ID)
	if err := s.finishSupervisionExecution(context.Background(), permit, supervisionSchedulerCLIReceipt{ContainerQuiet: true}, nil, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.completeSupervisionRole(context.Background(), permit, `{"summary":"已经核对源码，无需变更","edits":[]}`, nil); err != nil {
		t.Fatal(err)
	}
	return s, p, taskID, supervisionSessionForTest(t, s, ss.ID), pc, permit.Decision.ExecutionID
}

func addWorkflowEvidenceForTest(t *testing.T, s *Server, id, taskID, execution, kind, revision string, passed bool, details any) {
	t.Helper()
	if _, err := s.db.Exec(`insert into supervision_evidence(id,session_id,task_id,execution_id,kind,revision_id,requirements_revision,plan_revision,passed,details,created_at) values (?,?,?,?,?,?,1,1,?,?,?)`, uuid.NewString(), id, taskID, execution, kind, revision, passed, string(mustJSON(details)), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}

func TestSupervisionWorkflowAcceptanceRequiresLatestIndependentCompleteEvidence(t *testing.T) {
	for _, scenario := range []string{"pass", "missing_review", "later_failure", "different_check_success", "baseline_only", "truncated", "manual_pass", "manual_fail", "review_before_test"} {
		t.Run(scenario, func(t *testing.T) {
			s, _, taskID, ss, pc, execution := implementedWorkflowForTest(t)
			plan := validSupervisionPlanActionForTest(ss, taskID, pc).Plan
			check := supervisionCheck{ID: "first", AcceptanceIDs: []string{"behavior"}, Program: "node", Args: []string{"src/check.cjs"}, Directory: ".", TimeoutMillis: 30000}
			if scenario == "review_before_test" {
				addWorkflowEvidenceForTest(t, s, ss.ID, taskID, execution, "review", pc.RevisionID, true, map[string]any{})
			}
			if strings.HasPrefix(scenario, "manual_") {
				plan.Acceptance[0].Kind = "manual"
				addWorkflowEvidenceForTest(t, s, ss.ID, taskID, execution, "manual", pc.RevisionID, scenario == "manual_pass", map[string]string{"pointId": "behavior", "note": "实际人工操作记录"})
			} else {
				result := supervisionCheckResult{Check: check, ExitCode: 0}
				result.Check.Baseline = scenario == "baseline_only"
				result.Truncated = scenario == "truncated"
				addWorkflowEvidenceForTest(t, s, ss.ID, taskID, execution, "command", pc.RevisionID, true, result)
				if scenario == "later_failure" || scenario == "different_check_success" {
					result.ExitCode = 1
					addWorkflowEvidenceForTest(t, s, ss.ID, taskID, execution, "command", pc.RevisionID, false, result)
					if scenario == "different_check_success" {
						result.Check.ID = "renamed"
						result.Check.Args = []string{"src/other.cjs"}
						result.ExitCode = 0
						addWorkflowEvidenceForTest(t, s, ss.ID, taskID, execution, "command", pc.RevisionID, true, result)
					}
				}
			}
			if scenario != "missing_review" && scenario != "review_before_test" {
				addWorkflowEvidenceForTest(t, s, ss.ID, taskID, execution, "review", pc.RevisionID, true, map[string]any{})
			}
			tx, err := s.db.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			err = supervisionAcceptanceReadyTx(context.Background(), tx, ss.ID, taskID, pc.RevisionID, 1, 1, plan)
			wantPass := scenario == "pass" || scenario == "manual_pass"
			if (err == nil) != wantPass {
				t.Fatalf("acceptance=%v wantPass=%v", err, wantPass)
			}
		})
	}
}

func TestSupervisionWorkflowEnvironmentChangeAfterTestingInvalidatesAcceptance(t *testing.T) {
	s, p, taskID, ss, pc, execution := implementedWorkflowForTest(t)
	writeSupervisionSourceForTest(t, p.Path, "node_modules/pkg/dist/main.js", "module.exports=1;\n")
	check := supervisionCheck{ID: "tool-check", AcceptanceIDs: []string{"behavior"}, Program: "node", Args: []string{"--version"}, Directory: ".", TimeoutMillis: 30000}
	environment, err := supervisionCheckEnvironment(context.Background(), p.Path, check)
	if err != nil {
		t.Fatal(err)
	}
	result := supervisionCheckResult{Check: check, ExitCode: 0, Environment: environment}
	addWorkflowEvidenceForTest(t, s, ss.ID, taskID, execution, "command", pc.RevisionID, true, result)
	addWorkflowEvidenceForTest(t, s, ss.ID, taskID, execution, "review", pc.RevisionID, true, map[string]any{})
	if _, err := s.db.Exec(`update supervision_tasks set phase='accepted',evidence_freshness='fresh' where session_id=?`, ss.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.refreshSupervisionEvidenceEnvironments(context.Background(), ss.ID); err != nil {
		t.Fatal(err)
	}
	if current := supervisionSessionForTest(t, s, ss.ID); current.StateVersion != ss.StateVersion {
		t.Fatal("unchanged environment invalidated evidence")
	}
	writeSupervisionSourceForTest(t, p.Path, "node_modules/pkg/dist/main.js", "module.exports=2;\n")
	if err := s.refreshSupervisionEvidenceEnvironments(context.Background(), ss.ID); err != nil {
		t.Fatal(err)
	}
	var phase, freshness string
	if err := s.db.QueryRow(`select phase,evidence_freshness from supervision_tasks where session_id=?`, ss.ID).Scan(&phase, &freshness); err != nil || phase != "accepted" || freshness != "stale" {
		t.Fatalf("implementation facts lost: %s/%s %v", phase, freshness, err)
	}
	plan := validSupervisionPlanActionForTest(ss, taskID, pc).Plan
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	err = supervisionAcceptanceReadyTx(context.Background(), tx, ss.ID, taskID, pc.RevisionID, 1, 1, plan)
	if err == nil {
		t.Fatal("old environment evidence was accepted")
	}
	duplicate, err := supervisionDuplicateCheckTx(context.Background(), tx, &ss, supervisionWorkflowAction{TaskID: taskID, RequirementsRevision: 1, PlanRevision: 1, Checks: []supervisionCheck{check}}, pc.RevisionID)
	tx.Rollback()
	if err != nil || duplicate {
		t.Fatalf("stale environment prevented revalidation: %v %v", duplicate, err)
	}
	result.Environment, err = supervisionCheckEnvironment(context.Background(), p.Path, check)
	if err != nil {
		t.Fatal(err)
	}
	addWorkflowEvidenceForTest(t, s, ss.ID, taskID, execution, "command", pc.RevisionID, true, result)
	addWorkflowEvidenceForTest(t, s, ss.ID, taskID, execution, "review", pc.RevisionID, true, map[string]any{})
	if err := s.refreshSupervisionEvidenceEnvironments(context.Background(), ss.ID); err != nil {
		t.Fatal(err)
	}
	tx, err = s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := supervisionAcceptanceReadyTx(context.Background(), tx, ss.ID, taskID, pc.RevisionID, 1, 1, plan); err != nil {
		t.Fatal(err)
	}
}

func TestSupervisionWorkflowExternalDependencyWakesWaitAndInvalidatesEvidence(t *testing.T) {
	s, p, taskID, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	external := createTaskForTest(t, s.routes(), p.ID, "external prerequisite")
	if _, err := s.db.Exec(`insert into task_dependencies(task_id,predecessor_task_id,created_at) values (?,?,?)`, taskID, external, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	startSupervisionForTest(t, s, ss.ID, now)
	calls := 0
	w := supervisionWorkerForTest(s, func(_ context.Context, d supervisionDecision, lease supervisionLease) error {
		calls++
		return finishWorkerDecisionForTest(s, d, lease, decisionProposalForTest(d))
	})
	for i := 0; i < 2; i++ {
		if err := w.process(context.Background(), ss.ID); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("wait duplicated: %d", calls)
	}
	old := supervisionSessionForTest(t, s, ss.ID)
	if _, err := s.db.Exec(`update tasks set status='done',updated_at=? where id=?;update supervision_tasks set evidence_freshness='fresh' where session_id=?`, time.Now().UTC(), external, ss.ID); err != nil {
		t.Fatal(err)
	}
	if err := w.process(context.Background(), ss.ID); err != nil {
		t.Fatal(err)
	}
	current := supervisionSessionForTest(t, s, ss.ID)
	if calls != 2 || current.StateVersion <= old.StateVersion {
		t.Fatalf("not woken: calls=%d state=%+v", calls, current)
	}
	var freshness string
	if err := s.db.QueryRow(`select evidence_freshness from supervision_tasks where session_id=?`, ss.ID).Scan(&freshness); err != nil || freshness != "stale" {
		t.Fatalf("freshness=%s %v", freshness, err)
	}
}

func TestSupervisionWorkflowResumeRetainsImplementationAndCandidateFacts(t *testing.T) {
	s, p, taskID, ss, pc, _ := implementedWorkflowForTest(t)
	if _, err := s.db.Exec(`update supervision_tasks set phase='accepted',evidence_freshness='fresh' where session_id=?`, ss.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.requestSupervisionControl(context.Background(), ss.ID, ss.StateVersion, "pause", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.settleSupervisionControl(context.Background(), ss.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	paused := supervisionSessionForTest(t, s, ss.ID)
	if err := s.resumeSupervisionSession(context.Background(), ss.ID, paused.StateVersion, false, false); err != nil {
		t.Fatal(err)
	}
	var phase, freshness string
	if err := s.db.QueryRow(`select phase,evidence_freshness from supervision_tasks where session_id=? and task_id=?`, ss.ID, taskID).Scan(&phase, &freshness); err != nil || phase != "accepted" || freshness != "fresh" {
		t.Fatalf("restored=%s/%s %v", phase, freshness, err)
	}
	current := supervisionSessionForTest(t, s, ss.ID)
	if err := s.requestSupervisionControl(context.Background(), ss.ID, current.StateVersion, "pause", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.settleSupervisionControl(context.Background(), ss.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	writeSupervisionSourceForTest(t, p.Path, "src/main.go", "package changed\n")
	paused = supervisionSessionForTest(t, s, ss.ID)
	if err := s.resumeSupervisionSession(context.Background(), ss.ID, paused.StateVersion, false, false); !errors.Is(err, errSupervisionSnapshotChanged) {
		t.Fatalf("drift silently accepted: %v", err)
	}
	if err := s.resumeSupervisionSession(context.Background(), ss.ID, paused.StateVersion, true, false); err != nil {
		t.Fatal(err)
	}
	baseline, err := loadSupervisionProjectContext(context.Background(), s.db, ss.ID, "baseline")
	if err != nil || baseline.RevisionID != pc.RevisionID {
		t.Fatal("baseline replaced")
	}
	if err := s.db.QueryRow(`select phase,evidence_freshness from supervision_tasks where session_id=?`, ss.ID).Scan(&phase, &freshness); err != nil || phase != "accepted" || freshness != "stale" {
		t.Fatalf("drift forced reimplementation: %s/%s %v", phase, freshness, err)
	}
}

func TestSupervisionWorkflowRealBrowserArtifactsAndCleanup(t *testing.T) {
	for _, pass := range []bool{true, false} {
		t.Run(fmt.Sprint(pass), func(t *testing.T) {
			s, p, taskID, ss, now := supervisionFixture(t, defaultSupervisionBudget())
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := listener.Addr().(*net.TCPAddr).Port
			listener.Close()
			html := `<html><body><h1>Browser verification</h1><button id="increment" onclick="document.querySelector('#value').textContent=Number(document.querySelector('#value').textContent)+1">Increment</button><output id="value">0</output></body></html>`
			script := fmt.Sprintf("require('node:http').createServer((q,r)=>{r.setHeader('Content-Type','text/html');r.end(%q)}).listen(%d,'127.0.0.1');", html, port)
			writeSupervisionSourceForTest(t, p.Path, "src/server.cjs", script)
			if _, err := s.db.Exec(`update supervision_sessions set scheduler_agent_id='claude-code' where id=?`, ss.ID); err != nil {
				t.Fatal(err)
			}
			startSupervisionForTest(t, s, ss.ID, now)
			current := supervisionSessionForTest(t, s, ss.ID)
			pc, err := s.captureSupervisionProjectContext(context.Background(), ss.ID, current.StateVersion)
			if err != nil {
				t.Fatal(err)
			}
			plan := validSupervisionPlanActionForTest(current, taskID, pc)
			plan.Plan.Acceptance[0].Kind = "browser"
			applyWorkflowActionForTest(t, s, ss.ID, plan)
			expected := "2"
			if !pass {
				expected = "999"
			}
			check := supervisionCheck{ID: "browser-counter", AcceptanceIDs: []string{"behavior"}, Directory: ".", TimeoutMillis: 60000, Browser: &supervisionBrowserCheck{ServerProgram: "node", ServerArgs: []string{"src/server.cjs"}, URL: fmt.Sprintf("http://127.0.0.1:%d", port), Steps: []supervisionBrowserStep{{Action: "click", Selector: "#increment"}, {Action: "click", Selector: "#increment"}, {Action: "text", Selector: "#value", Value: expected}}}}
			applyWorkflowActionForTest(t, s, ss.ID, supervisionWorkflowAction{Type: "request_verification", SessionID: ss.ID, TaskID: taskID, RequirementsRevision: 1, PlanRevision: 1, ContextID: pc.ID, Checks: []supervisionCheck{check}})
			permit, deadline := workflowPermitForTest(t, s, ss.ID)
			if err := s.executeSupervisionVerification(context.Background(), permit, deadline); err != nil {
				t.Fatal(err)
			}
			var passed bool
			var raw string
			if err := s.db.QueryRow(`select passed,details from supervision_evidence where execution_id=?`, permit.Decision.ExecutionID).Scan(&passed, &raw); err != nil || passed != pass {
				t.Fatalf("browser=%v want=%v %v\n%s", passed, pass, err, raw)
			}
			var result supervisionCheckResult
			if err := json.Unmarshal([]byte(raw), &result); err != nil {
				t.Fatal(err)
			}
			if len(result.BrowserResult) == 0 || result.ScreenshotSHA256 == "" {
				t.Fatal("missing actual browser report/screenshot")
			}
			if output := os.Getenv("MILEVIA_SUPERVISION_BROWSER_SCREENSHOT"); pass && output != "" {
				data, err := os.ReadFile(filepath.Join(s.checkpointStorageRoot(), "supervision", "artifacts", permit.Decision.ExecutionID, "screenshot.png"))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(output, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			request := httptest.NewRecorder()
			s.routes().ServeHTTP(request, httptest.NewRequest(http.MethodGet, "/api/supervision/"+ss.ID+"/artifacts/"+permit.Decision.ExecutionID+"/screenshot.png", nil))
			if request.Code != 200 {
				t.Fatalf("artifact API: %d %s", request.Code, request.Body.String())
			}
			request = httptest.NewRecorder()
			s.routes().ServeHTTP(request, httptest.NewRequest(http.MethodDelete, "/api/supervision/"+ss.ID+"/artifacts", nil))
			if request.Code != http.StatusConflict {
				t.Fatal("active artifacts could be deleted")
			}
			current = supervisionSessionForTest(t, s, ss.ID)
			if err := s.requestSupervisionControl(context.Background(), ss.ID, current.StateVersion, "stop", time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			if err := s.settleSupervisionControl(context.Background(), ss.ID, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			request = httptest.NewRecorder()
			s.routes().ServeHTTP(request, httptest.NewRequest(http.MethodDelete, "/api/supervision/"+ss.ID+"/artifacts", nil))
			if request.Code != 204 {
				t.Fatalf("cleanup API: %d %s", request.Code, request.Body.String())
			}
			request = httptest.NewRecorder()
			s.routes().ServeHTTP(request, httptest.NewRequest(http.MethodGet, "/api/supervision/"+ss.ID+"/artifacts/"+permit.Decision.ExecutionID+"/screenshot.png", nil))
			if request.Code != http.StatusGone {
				t.Fatalf("cleanup marker: %d", request.Code)
			}
			if connection, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second); err == nil {
				connection.Close()
				t.Fatal("browser verification leaked its server")
			}
		})
	}
}
