package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestSupervisionPlanAndEditsUseManifestTargetOS(t *testing.T) {
	makePlan := func(scope ...string) supervisionTaskPlan {
		return supervisionTaskPlan{Goal: "target path validation", Scope: scope, Acceptance: []supervisionAcceptancePoint{{ID: "check", Requirement: "expected contents", Kind: "command", Verification: "run checks", Critical: true}}, Environment: []string{"target runtime"}, Review: []string{"compare actual changes"}}
	}
	for _, targetOS := range []string{"linux", "windows"} {
		t.Run(targetOS, func(t *testing.T) {
			manifest := supervisionWorkspaceManifest{Policy: supervisionSnapshotPolicy, TargetOS: targetOS}
			plan := makePlan("src/A.go", "src/a.go")
			err := validateSupervisionPlan(&plan, manifest)
			if (err != nil) != (targetOS == "windows") {
				t.Fatal("controller OS changed target duplicate semantics", targetOS, err)
			}
			for _, name := range []string{"CON", "nul.txt", "COM¹.log", "LPT9", "dir/PRN"} {
				plan := makePlan(name)
				err := validateSupervisionPlan(&plan, manifest)
				if (err != nil) != (targetOS == "windows") {
					t.Fatal("controller OS changed target reserved-name semantics", targetOS, name, err)
				}
			}
			plan = makePlan("src")
			pc := supervisionProjectContext{Manifest: manifest}
			err = validateSupervisionEdits([]supervisionEdit{{Path: "src/A.go", Content: "upper"}, {Path: "src/a.go", Content: "lower"}}, plan, pc)
			if (err != nil) != (targetOS == "windows") {
				t.Fatal("controller OS changed target edit identity", targetOS, err)
			}
		})
	}
	manifest := supervisionWorkspaceManifest{TargetOS: "unknown-os"}
	plan := makePlan("src/main.go")
	if err := validateSupervisionPlan(&plan, manifest); err == nil {
		t.Fatal("unknown target path semantics accepted")
	}
}

func supervisionPlanFixture(t *testing.T) (*Server, Project, string, supervisionSession, supervisionProjectContext) {
	t.Helper()
	s, p, taskID, ss, now := supervisionFixture(t, defaultSupervisionBudget())
	writeSupervisionSourceForTest(t, p.Path, "src/main.go", "package baseline\n")
	ss = startSupervisionForTest(t, s, ss.ID, now)
	projectContext, err := s.captureSupervisionProjectContext(context.Background(), ss.ID, ss.StateVersion)
	if err != nil {
		t.Fatal(err)
	}
	return s, p, taskID, supervisionSessionForTest(t, s, ss.ID), projectContext
}

func validSupervisionPlanActionForTest(ss supervisionSession, taskID string, pc supervisionProjectContext) supervisionRecordPlanAction {
	return supervisionRecordPlanAction{Type: "record_plan", SessionID: ss.ID, TaskID: taskID, RequirementsRevision: 1, ContextID: pc.ID, RevisionID: pc.RevisionID, RulesHash: pc.RulesHash,
		Plan: supervisionTaskPlan{Goal: "Complete the selected task", Scope: []string{"src"}, DependsOn: []string{},
			Acceptance:  []supervisionAcceptancePoint{{ID: "behavior", Requirement: "Expected behavior and error handling", Kind: "command", Verification: "Run relevant behavior and failure-path checks", Critical: true}},
			Environment: []string{"Project toolchain available"}, Review: []string{"Review behavior, scope and regression evidence"}, Limitations: []string{}}}
}

func supervisionPlanProposalForTest(d supervisionDecision, action supervisionRecordPlanAction) string {
	raw, _ := json.Marshal(map[string]any{"schemaVersion": 1, "decisionId": d.ID, "expectedStateVersion": d.ExpectedStateVersion, "basisSeq": d.BasisSeq, "reason": "Plan based on selected task and current inputs", "action": action})
	return string(raw)
}

func proposeSupervisionPlanForTest(t *testing.T, s *Server, ssID string, action supervisionRecordPlanAction) (supervisionLease, supervisionDecision) {
	t.Helper()
	lease, d := decisionForTest(t, s, ssID, time.Now().UTC())
	if err := finishWorkerDecisionForTest(s, d, lease, supervisionPlanProposalForTest(d, action)); err != nil {
		t.Fatal(err)
	}
	return lease, d
}

func TestSupervisionPlanAppliesAtomicallyAndReplayDoesNotCreateAnotherVersion(t *testing.T) {
	s, _, taskID, ss, pc := supervisionPlanFixture(t)
	action := validSupervisionPlanActionForTest(ss, taskID, pc)
	lease, d := proposeSupervisionPlanForTest(t, s, ss.ID, action)
	for i := 0; i < 3; i++ {
		handled, err := s.applySupervisionPlanDecision(context.Background(), d.ID, lease)
		if err != nil || !handled {
			t.Fatalf("apply=%v err=%v", handled, err)
		}
	}
	var count, revision int
	var phase, taskState, decisionState string
	if err := s.db.QueryRow(`select count(*) from supervision_plan_versions where session_id=?`, ss.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("plan count=%d %v", count, err)
	}
	if err := s.db.QueryRow(`select plan_revision,phase from supervision_tasks where session_id=? and task_id=?`, ss.ID, taskID).Scan(&revision, &phase); err != nil || revision != 1 || phase != "ready" {
		t.Fatalf("plan not ready: %d %s %v", revision, phase, err)
	}
	if err := s.db.QueryRow(`select status from tasks where id=?`, taskID).Scan(&taskState); err != nil || taskState != taskTodo {
		t.Fatalf("planning changed task outcome: %s %v", taskState, err)
	}
	if err := s.db.QueryRow(`select state from supervision_decisions where id=?`, d.ID).Scan(&decisionState); err != nil || decisionState != "applied" {
		t.Fatalf("decision=%s %v", decisionState, err)
	}
	_, next := decisionForTest(t, s, ss.ID, time.Now().UTC())
	raw, _, err := s.supervisionSchedulerInput(context.Background(), next.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	var input supervisionSchedulerInput
	if err := json.Unmarshal(raw, &input); err != nil || len(input.Plans) != 1 || input.Plans[0].Revision != 1 || input.ProjectContext.ID != pc.ID {
		t.Fatalf("next observation lacks versioned plan: %v", err)
	}
	assertExecutionClaimForTest(t, s, taskID, "supervision", ss.ID)
}

func TestSupervisionPlanRejectsForeignScopeStaleVersionsAndMissingAcceptance(t *testing.T) {
	for name, change := range map[string]func(*supervisionRecordPlanAction){
		"foreign_task":            func(a *supervisionRecordPlanAction) { a.TaskID = "outside-task" },
		"requirements":            func(a *supervisionRecordPlanAction) { a.RequirementsRevision++ },
		"plan_revision":           func(a *supervisionRecordPlanAction) { a.ExpectedPlanRevision++ },
		"code_revision":           func(a *supervisionRecordPlanAction) { a.RevisionID = "other" },
		"rules_revision":          func(a *supervisionRecordPlanAction) { a.RulesHash = "other" },
		"context":                 func(a *supervisionRecordPlanAction) { a.ContextID = "other" },
		"traversal":               func(a *supervisionRecordPlanAction) { a.Plan.Scope = []string{"../outside"} },
		"whole_project":           func(a *supervisionRecordPlanAction) { a.Plan.Scope = []string{"."} },
		"hidden_sensitive_input":  func(a *supervisionRecordPlanAction) { a.Plan.Scope = []string{".env"} },
		"self_dependency":         func(a *supervisionRecordPlanAction) { a.Plan.DependsOn = []string{a.TaskID} },
		"new_external_dependency": func(a *supervisionRecordPlanAction) { a.Plan.DependsOn = []string{"outside-task"} },
		"no_critical":             func(a *supervisionRecordPlanAction) { a.Plan.Acceptance[0].Critical = false },
		"no_verification":         func(a *supervisionRecordPlanAction) { a.Plan.Acceptance[0].Verification = "" },
		"no_review":               func(a *supervisionRecordPlanAction) { a.Plan.Review = nil },
	} {
		t.Run(name, func(t *testing.T) {
			s, p, taskID, ss, pc := supervisionPlanFixture(t)
			// 用显式排除项证明计划不能将其当作已覆盖输入。
			if name == "hidden_sensitive_input" {
				writeSupervisionSourceForTest(t, p.Path, ".env", "PRIVATE_VALUE=hidden")
				// 该项在首次基线之前建立；重新建 fixture 会丢失动作状态，直接重新采集
				// 将如实进入漂移保护，因此这个分支单独用纯领域校验。
				manifest := pc.Manifest
				manifest.Entries = append(manifest.Entries, supervisionSnapshotEntry{Path: ".env", Kind: "excluded", Gap: "sensitive_path"})
				a := validSupervisionPlanActionForTest(ss, taskID, pc)
				change(&a)
				if err := validateSupervisionPlan(&a.Plan, manifest); err == nil {
					t.Fatal("sensitive scope accepted")
				}
				return
			}
			action := validSupervisionPlanActionForTest(ss, taskID, pc)
			change(&action)
			lease, d := proposeSupervisionPlanForTest(t, s, ss.ID, action)
			if handled, err := s.applySupervisionPlanDecision(context.Background(), d.ID, lease); err != nil || !handled {
				t.Fatalf("handled=%v err=%v", handled, err)
			}
			var state string
			var count int
			if err := s.db.QueryRow(`select state from supervision_decisions where id=?`, d.ID).Scan(&state); err != nil || state != "invalid" {
				t.Fatalf("invalid action accepted: %s %v", state, err)
			}
			if err := s.db.QueryRow(`select count(*) from supervision_plan_versions where session_id=?`, ss.ID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("rejected plan persisted: %d %v", count, err)
			}
		})
	}
}

func TestSupervisionPlanStorageFailureDoesNotCommitHalfPlanOrConsumeProposal(t *testing.T) {
	s, _, taskID, ss, pc := supervisionPlanFixture(t)
	lease, d := proposeSupervisionPlanForTest(t, s, ss.ID, validSupervisionPlanActionForTest(ss, taskID, pc))
	before := supervisionSessionForTest(t, s, ss.ID)
	if _, err := s.db.Exec(`create trigger reject_action_result before insert on supervision_action_results begin select raise(abort,'simulated action commit failure');end`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.applySupervisionPlanDecision(context.Background(), d.ID, lease); err == nil {
		t.Fatal("persistence error hidden as valid or rejected action")
	}
	var count, revision int
	var state string
	if err := s.db.QueryRow(`select count(*) from supervision_plan_versions where session_id=?`, ss.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("half plan committed: %d %v", count, err)
	}
	if err := s.db.QueryRow(`select plan_revision from supervision_tasks where session_id=? and task_id=?`, ss.ID, taskID).Scan(&revision); err != nil || revision != 0 {
		t.Fatalf("task revision advanced: %d %v", revision, err)
	}
	if err := s.db.QueryRow(`select state from supervision_decisions where id=?`, d.ID).Scan(&state); err != nil || state != "proposed" {
		t.Fatalf("proposal lost: %s %v", state, err)
	}
	if after := supervisionSessionForTest(t, s, ss.ID); after.StateVersion != before.StateVersion || after.EventSeq != before.EventSeq {
		t.Fatal("rollback leaked plan event or version")
	}
	if _, err := s.db.Exec(`drop trigger reject_action_result`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.applySupervisionPlanDecision(context.Background(), d.ID, lease); err != nil {
		t.Fatalf("same proposal cannot retry after storage recovers: %v", err)
	}
}

func TestSupervisionPlanSemanticReplayAndAcceptanceWeakeningCannotLoopOrSkipReview(t *testing.T) {
	s, _, taskID, ss, pc := supervisionPlanFixture(t)
	action := validSupervisionPlanActionForTest(ss, taskID, pc)
	lease, d := proposeSupervisionPlanForTest(t, s, ss.ID, action)
	if _, err := s.applySupervisionPlanDecision(context.Background(), d.ID, lease); err != nil {
		t.Fatal(err)
	}
	for revision := int64(1); revision <= 3; revision++ {
		action.ExpectedPlanRevision = revision
		action.Plan.Acceptance[0].Verification = fmt.Sprintf("Changed verification specification %d", revision)
		lease, d = proposeSupervisionPlanForTest(t, s, ss.ID, action)
		if _, err := s.applySupervisionPlanDecision(context.Background(), d.ID, lease); err != nil {
			t.Fatal(err)
		}
		var status, phase string
		if err := s.db.QueryRow(`select status from supervision_plan_versions where session_id=? and task_id=? and revision=?`, ss.ID, taskID, revision+1).Scan(&status); err != nil || status != "review_required" {
			t.Fatalf("changed acceptance bypassed review: %s %v", status, err)
		}
		if err := s.db.QueryRow(`select phase from supervision_tasks where session_id=? and task_id=?`, ss.ID, taskID).Scan(&phase); err != nil || phase != "planning" {
			t.Fatalf("unreviewed plan marked ready: %s %v", phase, err)
		}
	}
	if got := supervisionSessionForTest(t, s, ss.ID); got.State != "needs_input" || got.Reason != "no_progress" {
		t.Fatalf("plan-only loop not stopped: %+v", got)
	}
}

func TestSupervisionPlanDriftAndControlInvalidateLateApplication(t *testing.T) {
	for _, mode := range []string{"drift", "pause"} {
		t.Run(mode, func(t *testing.T) {
			s, p, taskID, ss, pc := supervisionPlanFixture(t)
			lease, d := proposeSupervisionPlanForTest(t, s, ss.ID, validSupervisionPlanActionForTest(ss, taskID, pc))
			if mode == "drift" {
				writeSupervisionSourceForTest(t, p.Path, "src/main.go", "package external\n")
			} else {
				if err := s.requestSupervisionControl(context.Background(), ss.ID, ss.StateVersion, "pause", time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.applySupervisionPlanDecision(context.Background(), d.ID, lease); err == nil {
				t.Fatal("late plan applied despite changed facts/control")
			}
			var count int
			if err := s.db.QueryRow(`select count(*) from supervision_plan_versions where session_id=?`, ss.ID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("late plan persisted: %d %v", count, err)
			}
		})
	}
}

func TestSupervisionPlanCanonicalFieldsAndDependencyCycleAreRejected(t *testing.T) {
	action := supervisionRecordPlanAction{}
	if err := decodeSupervisionAction(json.RawMessage(`{"type":"record_plan","TaskId":"alias"}`), &action); err == nil {
		t.Fatal("case alias accepted")
	}
	if err := decodeSupervisionAction(json.RawMessage(`{"type":"record_plan","plan":{"goal":"x","unexpected":true}}`), &action); err == nil {
		t.Fatal("unknown nested field accepted")
	}
	s, p, first, ss, _ := supervisionFixture(t, defaultSupervisionBudget())
	second := createTaskForTest(t, s.routes(), p.ID, "second selected task")
	if _, err := s.db.Exec(`insert into supervision_tasks(session_id,task_id,position,task_snapshot,dependency_snapshot) values (?,?,1,'{}','[]')`, ss.ID, second); err != nil {
		t.Fatal(err)
	}
	ss = startSupervisionForTest(t, s, ss.ID, time.Now().UTC())
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`update supervision_tasks set dependency_snapshot=? where session_id=? and task_id=?`, fmt.Sprintf(`[{"taskId":%q}]`, first), ss.ID, second); err != nil {
		t.Fatal(err)
	}
	if err := validateSupervisionPlanDependenciesTx(context.Background(), tx, ss.ID, first, []string{second}); !errors.Is(err, errSupervisionPlanInvalid) {
		t.Fatalf("cycle accepted: %v", err)
	}
}

func TestSupervisionWorkerAppliesPlanAndCorrectsRejectedPlanWithinSameBudget(t *testing.T) {
	s, _, taskID, ss, pc := supervisionPlanFixture(t)
	calls := 0
	w := supervisionWorkerForTest(s, func(_ context.Context, d supervisionDecision, lease supervisionLease) error {
		calls++
		action := validSupervisionPlanActionForTest(ss, taskID, pc)
		if calls == 1 {
			action.ExpectedPlanRevision = 5
		}
		return finishWorkerDecisionForTest(s, d, lease, supervisionPlanProposalForTest(d, action))
	})
	if err := w.process(context.Background(), ss.ID); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("rejected plan was not corrected: %d calls", calls)
	}
	var revision int
	if err := s.db.QueryRow(`select plan_revision from supervision_tasks where session_id=? and task_id=?`, ss.ID, taskID).Scan(&revision); err != nil || revision != 1 {
		t.Fatalf("worker plan revision=%d err=%v", revision, err)
	}
	if got := supervisionSessionForTest(t, s, ss.ID); got.UsedTurns != 2 || got.ReservedTurns != 0 {
		t.Fatalf("correction budget=%+v", got)
	}
}

func TestSupervisionPlanRejectsExcludedNewPathsAndFileAncestors(t *testing.T) {
	_, _, taskID, ss, pc := supervisionPlanFixture(t)
	for _, name := range []string{".env", "config/.env.local", "new/node_modules/input.js", "new/.aws/config", "keys/new.pem", "src/main.go/child.go", "src./file.go", "src /file.go"} {
		t.Run(name, func(t *testing.T) {
			action := validSupervisionPlanActionForTest(ss, taskID, pc)
			action.Plan.Scope = []string{name}
			if err := validateSupervisionPlan(&action.Plan, pc.Manifest); err == nil {
				t.Fatal("unsafe new scope accepted")
			}
		})
	}
	action := validSupervisionPlanActionForTest(ss, taskID, pc)
	action.Plan.Scope = []string{"new/source.go"}
	if err := validateSupervisionPlan(&action.Plan, pc.Manifest); err != nil {
		t.Fatalf("valid new source rejected: %v", err)
	}
}

func TestSupervisionPlanReviewRequirementSurvivesUnrelatedRevision(t *testing.T) {
	s, _, taskID, ss, pc := supervisionPlanFixture(t)
	action := validSupervisionPlanActionForTest(ss, taskID, pc)
	for revision := int64(0); revision < 3; revision++ {
		action.ExpectedPlanRevision = revision
		if revision == 1 {
			action.Plan.Acceptance[0].Verification = "Changed acceptance check"
		}
		if revision == 2 {
			action.Plan.Goal = "Clarified goal with unchanged revised acceptance"
		}
		lease, d := proposeSupervisionPlanForTest(t, s, ss.ID, action)
		if _, err := s.applySupervisionPlanDecision(context.Background(), d.ID, lease); err != nil {
			t.Fatal(err)
		}
	}
	var status string
	if err := s.db.QueryRow(`select status from supervision_plan_versions where session_id=? and task_id=? and revision=3`, ss.ID, taskID).Scan(&status); err != nil || status != "review_required" {
		t.Fatalf("unreviewed acceptance bypassed by unrelated revision: %s %v", status, err)
	}
}

func TestSupervisionPlanSemanticReplayDoesNotAdvanceBusinessVersion(t *testing.T) {
	s, _, taskID, ss, pc := supervisionPlanFixture(t)
	action := validSupervisionPlanActionForTest(ss, taskID, pc)
	lease, d := proposeSupervisionPlanForTest(t, s, ss.ID, action)
	if _, err := s.applySupervisionPlanDecision(context.Background(), d.ID, lease); err != nil {
		t.Fatal(err)
	}
	action.ExpectedPlanRevision = 1
	lease, d = proposeSupervisionPlanForTest(t, s, ss.ID, action)
	before := supervisionSessionForTest(t, s, ss.ID)
	if _, err := s.applySupervisionPlanDecision(context.Background(), d.ID, lease); err != nil {
		t.Fatal(err)
	}
	after := supervisionSessionForTest(t, s, ss.ID)
	var count int
	if err := s.db.QueryRow(`select count(*) from supervision_plan_versions where session_id=?`, ss.ID).Scan(&count); err != nil || count != 1 || after.StateVersion != before.StateVersion {
		t.Fatalf("semantic replay created progress: count=%d versions=%d/%d err=%v", count, before.StateVersion, after.StateVersion, err)
	}
}

func TestSupervisionPlanMigrationAndTerminalCascadeKeepReferentialIntegrity(t *testing.T) {
	s, p, taskID, ss, pc := supervisionPlanFixture(t)
	lease, d := proposeSupervisionPlanForTest(t, s, ss.ID, validSupervisionPlanActionForTest(ss, taskID, pc))
	if _, err := s.applySupervisionPlanDecision(context.Background(), d.ID, lease); err != nil {
		t.Fatal(err)
	}
	if err := s.migrateSupervision(context.Background()); err != nil {
		t.Fatalf("repeated migration lost existing context/plan: %v", err)
	}
	ss = supervisionSessionForTest(t, s, ss.ID)
	if err := s.requestSupervisionControl(context.Background(), ss.ID, ss.StateVersion, "stop", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.settleSupervisionControl(context.Background(), ss.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`delete from projects where id=?`, p.ID); err != nil {
		t.Fatalf("terminal plan/context blocks cascade: %v", err)
	}
	for _, table := range []string{"supervision_plan_versions", "supervision_project_contexts", "supervision_context_state", "supervision_context_captures", "supervision_action_results"} {
		var count int
		if err := s.db.QueryRow(`select count(*) from ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("orphan in %s: %d %v", table, count, err)
		}
	}
	rows, err := s.db.Query(`pragma foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign key violation after terminal project deletion")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
