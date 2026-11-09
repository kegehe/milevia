//go:build windows

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSupervisionWorkflowRecoveryHonorsControlsAndRequiresDriftReview(t *testing.T) {
	for _, control := range []string{"pause", "stop", "drift"} {
		t.Run(control, func(t *testing.T) {
			s, p, taskID, ss, _ := supervisionPlanFixture(t)
			ctx := context.Background()
			if control != "drift" {
				if err := s.requestSupervisionControl(ctx, ss.ID, ss.StateVersion, control, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
				if control == "pause" {
					if err := s.settleSupervisionControl(ctx, ss.ID, time.Now().UTC()); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				writeSupervisionSourceForTest(t, p.Path, "src/main.go", "package changed\n")
			}
			if err := s.recoverWorkspaceAdmissions(ctx); err != nil {
				t.Fatal(err)
			}
			if err := s.recoverSupervisionSessions(ctx, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			w := supervisionWorkerForTest(s, func(context.Context, supervisionDecision, supervisionLease) error {
				t.Fatal("recovery executed a model")
				return nil
			})
			current, err := w.maintain(ctx, ss.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch control {
			case "pause":
				if current.State != "paused" || current.Control != "pause" || current.RecoveryState != "ready" {
					t.Fatalf("lost pause: %+v", current)
				}
				assertExecutionClaimForTest(t, s, taskID, "supervision", ss.ID)
			case "stop":
				if current.State != "stopped" || current.RecoveryState != "ready" {
					t.Fatalf("stop not settled: %+v", current)
				}
				assertExecutionClaimForTest(t, s, taskID, "", "")
			case "drift":
				if current.State != "needs_input" || current.Reason != "recovery_review_required" {
					t.Fatalf("no drift review: %+v", current)
				}
				if err := s.resumeSupervisionSession(ctx, ss.ID, current.StateVersion, false, false); !errors.Is(err, errSupervisionSnapshotChanged) {
					t.Fatalf("accepted drift silently: %v", err)
				}
				if err := s.resumeSupervisionSession(ctx, ss.ID, current.StateVersion, true, false); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestSupervisionWorkflowRecoveryStopsPersistedOwnedContainerAndFencesOldPermit(t *testing.T) {
	s, taskID, permit, now, _ := supervisionExecutionForTest(t)
	process, err := startSupervisionContainedProcess(supervisionProcessCommandForTest(t, "Start-Sleep -Seconds 45"), permit.Container)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = process.terminate(); _ = process.wait(); process.close() }()
	ctx := context.Background()
	if err := s.registerSupervisionProcess(ctx, permit, process.identity(), now); err != nil {
		t.Fatal(err)
	}
	if err := s.resumeSupervisionProcess(ctx, permit, process.resume, now); err != nil {
		t.Fatal(err)
	}
	ss := supervisionSessionForTest(t, s, permit.Decision.SessionID)
	if err := s.requestSupervisionControl(ctx, ss.ID, ss.StateVersion, "stop", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.recoverWorkspaceAdmissions(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.recoverSupervisionSessions(ctx, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	w := supervisionWorkerForTest(s, nil)
	deadline := time.Now().Add(8 * time.Second)
	for {
		current, err := w.maintain(ctx, ss.ID)
		if err != nil && !errors.Is(err, errWorkspaceNotQuiescent) {
			t.Fatal(err)
		}
		if current.State == "stopped" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("owned container did not settle: %+v %v", current, err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if quiet, err := process.quiescent(); err != nil || !quiet {
		t.Fatalf("container leaked: %v %v", quiet, err)
	}
	if err := s.finishSupervisionExecution(ctx, permit, supervisionSchedulerCLIReceipt{ContainerQuiet: true}, nil, time.Now().UTC()); !errors.Is(err, errSupervisionLease) {
		t.Fatalf("old permit accepted: %v", err)
	}
	assertExecutionClaimForTest(t, s, taskID, "", "")
	var outcome string
	if err := s.db.QueryRow(`select outcome_state from supervision_run_links where run_id=?`, permit.RunID).Scan(&outcome); err != nil || outcome != "unknown" {
		t.Fatalf("invented outcome: %s %v", outcome, err)
	}
}

func supervisionHTTPForTest(t *testing.T, s *Server, method, path string, body any, want int) *httptest.ResponseRecorder {
	t.Helper()
	payload := ""
	if body != nil {
		payload = string(mustJSON(body))
	}
	request := httptest.NewRequest(method, path, strings.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	s.routes().ServeHTTP(response, request)
	if response.Code != want {
		t.Fatalf("%s %s: status=%d want=%d body=%s", method, path, response.Code, want, response.Body.String())
	}
	return response
}

func TestSupervisionWorkflowHTTPManualEvidenceRequiresNewIndependentReview(t *testing.T) {
	s, _, taskID, ss, pc, execution := implementedWorkflowForTest(t)
	plan := validSupervisionPlanActionForTest(ss, taskID, pc).Plan
	plan.Acceptance[0].Kind = "manual"
	planJSON := mustJSON(plan)
	if _, err := s.db.Exec(`update supervision_plan_versions set plan_json=?,semantic_hash=? where session_id=? and task_id=? and revision=1`, string(planJSON), supervisionSHA256(planJSON), ss.ID, taskID); err != nil {
		t.Fatal(err)
	}
	addWorkflowEvidenceForTest(t, s, ss.ID, taskID, execution, "review", pc.RevisionID, true, map[string]any{})
	base := "/api/supervision/" + ss.ID
	body := map[string]any{"stateVersion": ss.StateVersion, "taskId": taskID, "pointId": "behavior", "note": "用户实际执行设备操作并确认结果", "passed": true}
	response := supervisionHTTPForTest(t, s, http.MethodPost, base+"/manual-evidence", body, http.StatusAccepted)
	if !json.Valid(response.Body.Bytes()) {
		t.Fatal("manual evidence acknowledgement must be valid JSON")
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	err = supervisionAcceptanceReadyTx(context.Background(), tx, ss.ID, taskID, pc.RevisionID, 1, 1, plan)
	tx.Rollback()
	if err == nil {
		t.Fatal("manual evidence reused earlier review")
	}
	addWorkflowEvidenceForTest(t, s, ss.ID, taskID, execution, "review", pc.RevisionID, true, map[string]any{})
	tx, err = s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := supervisionAcceptanceReadyTx(context.Background(), tx, ss.ID, taskID, pc.RevisionID, 1, 1, plan); err != nil {
		t.Fatal(err)
	}
}

func TestSupervisionWorkflowHTTPHistorySourceAndAmendmentControls(t *testing.T) {
	for _, scenario := range []string{"deferred", "immediate", "paused"} {
		t.Run(scenario, func(t *testing.T) {
			s, p, taskID, ss, pc, _ := implementedWorkflowForTest(t)
			ctx := context.Background()
			base := "/api/supervision/" + ss.ID
			if scenario == "deferred" {
				supervisionHTTPForTest(t, s, http.MethodGet, "/api/projects/"+p.ID+"/supervision/capabilities", nil, http.StatusOK)
				draftTask := createTaskForTest(t, s.routes(), p.ID, "another independent todo task")
				supervisionHTTPForTest(t, s, http.MethodPost, "/api/projects/"+p.ID+"/supervision", map[string]any{"taskIds": []string{draftTask}, "schedulerAgentId": "claude-code"}, http.StatusCreated)
			}
			supervisionHTTPForTest(t, s, http.MethodGet, "/api/projects/"+p.ID+"/supervision", nil, http.StatusOK)
			supervisionHTTPForTest(t, s, http.MethodGet, base+"?taskId="+taskID, nil, http.StatusOK)
			page := supervisionHTTPForTest(t, s, http.MethodGet, base+"/process?afterMessage=0&afterEvent=0", nil, http.StatusOK)
			var result struct {
				AfterMessage int64 `json:"afterMessage"`
				AfterEvent   int64 `json:"afterEvent"`
			}
			if err := json.Unmarshal(page.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			supervisionHTTPForTest(t, s, http.MethodGet, fmt.Sprintf("%s/process?afterMessage=%d&afterEvent=%d", base, result.AfterMessage, result.AfterEvent), nil, http.StatusOK)
			source := supervisionHTTPForTest(t, s, http.MethodGet, base+"/source?revisionId="+pc.RevisionID+"&path=src/main.go", nil, http.StatusOK)
			if !strings.Contains(source.Body.String(), "package baseline") {
				t.Fatal("source missing")
			}
			supervisionHTTPForTest(t, s, http.MethodGet, base+"/source?revisionId=foreign&path=src/main.go", nil, http.StatusNotFound)
			supervisionHTTPForTest(t, s, http.MethodGet, base+"/changes", nil, http.StatusOK)
			_, old := decisionForTest(t, s, ss.ID, time.Now().UTC())
			ss = supervisionSessionForTest(t, s, ss.ID)
			if scenario == "paused" {
				supervisionHTTPForTest(t, s, http.MethodPost, base+"/control", map[string]any{"stateVersion": ss.StateVersion, "action": "pause"}, http.StatusAccepted)
				ss = supervisionSessionForTest(t, s, ss.ID)
			}
			supervisionHTTPForTest(t, s, http.MethodPost, base+"/instructions", map[string]any{"stateVersion": ss.StateVersion, "taskId": taskID, "content": "增加异常输入验收", "immediate": scenario != "deferred"}, http.StatusAccepted)
			if err := s.applySupervisionAmendments(ctx, ss.ID); err != nil {
				t.Fatal(err)
			}
			current := supervisionSessionForTest(t, s, ss.ID)
			if scenario == "paused" {
				if current.State != "paused" || current.Control != "pause" {
					t.Fatalf("instruction resumed user pause: %+v", current)
				}
			} else if current.State != "running" || current.Control != "" {
				t.Fatalf("instruction not applied: %+v", current)
			}
			var revision int
			var phase, jobState string
			if err := s.db.QueryRow(`select requirements_revision,phase from supervision_tasks where session_id=? and task_id=?`, ss.ID, taskID).Scan(&revision, &phase); err != nil || revision != 2 || phase != "pending" {
				t.Fatalf("requirements=%d/%s %v", revision, phase, err)
			}
			if err := s.db.QueryRow(`select state from supervision_work_items where id=?`, old.WorkItemID).Scan(&jobState); err != nil || jobState != "suspended" {
				t.Fatalf("old work replayable: %s %v", jobState, err)
			}
		})
	}
}
