package app

import (
	"context"
	"errors"
	"testing"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

func TestSupervisionTargetCLIInputBindsDecisionAndRoleBeforeLaunch(t *testing.T) {
	for _, role := range []string{"scheduler", "implementation", "review"} {
		t.Run(role, func(t *testing.T) {
			spec := targetexec.ExecutionSpec{SessionID: "session", ExecutionID: "execution", WorkItemID: "work", Attempt: 2, OwnerEpoch: 3, WorkspaceID: "directory", Role: role}
			observation := supervisionRoleInput{
				SchemaVersion:  1,
				Decision:       supervisionDecision{ID: "decision", SessionID: spec.SessionID, ExecutionID: spec.ExecutionID, WorkItemID: spec.WorkItemID, Attempt: spec.Attempt, LeaseEpoch: spec.OwnerEpoch},
				Job:            supervisionRoleJob{ExecutionID: spec.ExecutionID, TaskID: "task", Kind: role, RequirementsRevision: 2, PlanRevision: 3, InputRevision: "revision"},
				Task:           supervisionTask{TaskID: "task", RequirementsRevision: 2},
				Plan:           supervisionPlanVersion{TaskID: "task", RequirementsRevision: 2, Revision: 3},
				ProjectContext: supervisionProjectContext{RevisionID: "revision", Manifest: supervisionWorkspaceManifest{DirectoryIdentity: spec.WorkspaceID}},
			}
			check := func(body supervisionRoleInput, prompt string) error {
				input := targetexec.Input{Observation: mustJSON(body), Instructions: prompt}
				// Even a rehashed observation must match the execution envelope.
				bound := spec
				bound.InputSHA256, bound.InstructionsSHA256 = supervisionSHA256(input.Observation), supervisionSHA256([]byte(input.Instructions))
				return validateSupervisionTargetCLIInput(bound, input)
			}
			if err := check(observation, supervisionRolePrompt(role)); err != nil {
				t.Fatal("matching input rejected", err)
			}
			for _, field := range []string{"session", "execution", "work", "attempt", "epoch", "instructions", "schema", "role", "task", "plan", "revision", "directory"} {
				if role == "scheduler" && (field == "role" || field == "task" || field == "plan" || field == "revision") {
					continue
				}
				changed, prompt := observation, supervisionRolePrompt(role)
				switch field {
				case "session":
					changed.Decision.SessionID = "other"
				case "execution":
					changed.Decision.ExecutionID = "other"
				case "work":
					changed.Decision.WorkItemID = "other"
				case "attempt":
					changed.Decision.Attempt++
				case "epoch":
					changed.Decision.LeaseEpoch++
				case "instructions":
					prompt = "unapproved role instructions"
				case "schema":
					changed.SchemaVersion++
				case "role":
					changed.Job.Kind = "test"
				case "task":
					changed.Task.TaskID = "other"
				case "plan":
					changed.Plan.Revision++
				case "revision":
					changed.ProjectContext.RevisionID = "other"
				case "directory":
					changed.ProjectContext.Manifest.DirectoryIdentity = "other"
				}
				if err := check(changed, prompt); !errors.Is(err, targetexec.ErrConflict) {
					t.Fatal("mismatched observation accepted", field, err)
				}
			}
		})
	}
}

func TestSupervisionTargetRuntimeRejectsRehashedForeignInputBeforeResolvingCLI(t *testing.T) {
	runtime, control, _ := targetRuntimeFixture(t)
	input := targetexec.Input{Instructions: supervisionRolePrompt("scheduler"), Observation: mustJSON(supervisionSchedulerInput{SchemaVersion: 1, Decision: supervisionDecision{ID: "d", SessionID: "another-session", ExecutionID: "execution", WorkItemID: "work", Attempt: 1, LeaseEpoch: 1}})}
	spec := targetexec.ExecutionSpec{Role: "scheduler", AgentID: "codex", SessionID: control.SessionID, ExecutionID: "execution", WorkItemID: "work", Attempt: 1, OwnerEpoch: 1, WorkspaceID: control.WorkspaceID, InputSHA256: supervisionSHA256(input.Observation), InstructionsSHA256: supervisionSHA256([]byte(input.Instructions))}
	resumed := false
	receipt, err := runtime.Run(context.Background(), targetexec.Execution{Spec: spec}, input, func(_ targetexec.ProcessIdentity, _ func() error) error { resumed = true; return nil })
	if !errors.Is(err, targetexec.ErrConflict) || !receipt.Quiet || resumed {
		t.Fatal("foreign input reached CLI resolution or process launch", receipt, err)
	}
}
