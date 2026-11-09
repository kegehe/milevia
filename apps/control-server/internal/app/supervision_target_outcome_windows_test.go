//go:build windows

package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

func TestSupervisionTargetOutcomeRequiresDurableQuietAndExactlyBoundResult(t *testing.T) {
	for _, scenario := range []string{"success", "failed_before_start", "message_only", "wrong_digest", "duplicate_result"} {
		t.Run(scenario, func(t *testing.T) {
			s, permit, store, spec := supervisionTargetInboxFixture(t)
			ctx := context.Background()
			current, err := store.Prepare(spec, time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			failed := scenario == "failed_before_start"
			receipt := supervisionSchedulerCLIReceipt{AgentID: spec.AgentID, CLIPath: spec.CLIPath, CLIVersion: spec.CLIVersion, Contract: spec.Contract, InputSHA256: spec.InputSHA256, Initialized: !failed, ProcessStarted: !failed, ProcessWaited: !failed, ContainerQuiet: true, Output: "actual target output"}
			message := ""
			if failed {
				message = "target authentication failed"
				receipt.Output = ""
			}
			payload, err := supervisionTargetPayload(mustJSON(map[string]any{"receipt": receipt, "failed": failed, "error": message}))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.AppendEvent(spec.SessionID, spec.ExecutionID, "result", "execution.result", payload); err != nil {
				t.Fatal(err)
			}
			if scenario == "duplicate_result" {
				if _, err := store.AppendEvent(spec.SessionID, spec.ExecutionID, "second-result", "execution.result", payload); err != nil {
					t.Fatal(err)
				}
			}
			if scenario != "message_only" {
				if !failed {
					if _, err := store.ClaimStart(spec.SessionID, spec.ExecutionID, current.Digest, time.Now().UTC()); err != nil {
						t.Fatal(err)
					}
					if err := store.RegisterHeld(spec.SessionID, spec.ExecutionID, targetexec.ProcessIdentity{Container: current.Container, PID: 4242, Birth: "fixture-birth", Executable: spec.CLIPath}, time.Now().UTC()); err != nil {
						t.Fatal(err)
					}
					if err := store.Resume(spec.SessionID, spec.ExecutionID, time.Now().UTC(), func() error { return nil }); err != nil {
						t.Fatal(err)
					}
				}
				digest := supervisionSHA256(payload)
				if scenario == "wrong_digest" {
					digest = strings.Repeat("e", 64)
				}
				if err := store.RecordQuiet(spec.SessionID, spec.ExecutionID, current.Digest, digest, failed, targetexec.Usage{Sequence: 1, InputTokens: 12, OutputTokens: 3, ActiveMillis: 20}); err != nil {
					t.Fatal(err)
				}
			}
			link := supervisionTargetControlTestLink(t, &supervisionTargetRuntime{store: store, storage: t.TempDir(), config: Config{WorkspaceGuardDir: t.TempDir()}})
			outcome, err := s.readSupervisionTargetOutcome(ctx, link, permit)
			if scenario == "message_only" || scenario == "wrong_digest" || scenario == "duplicate_result" {
				if err == nil {
					t.Fatal("unproven remote result accepted", scenario)
				}
				var state string
				if err := s.db.QueryRow(`select state from supervision_executions where execution_id=?`, spec.ExecutionID).Scan(&state); err != nil || state != "prepared" {
					t.Fatal("unproven result changed execution lifecycle", state, err)
				}
				return
			}
			if err != nil || outcome.Receipt != receipt || (outcome.RunError != nil) != failed {
				t.Fatal("target outcome lost actual receipt/failure", outcome, err)
			}
			if err := s.finishSupervisionExecution(ctx, permit, receipt, nil, time.Now().UTC()); err == nil {
				t.Fatal("controller-local empty container was accepted as target quiet")
			}
			if err := s.consumeSupervisionTargetUsage(ctx, spec.SessionID, spec.TargetID, outcome.Execution); err != nil {
				t.Fatal(err)
			}
			supplied := outcome
			supplied.Receipt.Output, supplied.RunError = "caller-invented output", nil
			if err := s.finishSupervisionTargetExecution(ctx, permit, supplied, time.Now().UTC()); err != nil {
				t.Fatal("proven target result failed to settle", err)
			}
			if err := s.finishSupervisionTargetExecution(ctx, permit, outcome, time.Now().UTC()); err != nil {
				t.Fatal("target settlement was not idempotent", err)
			}
			var state, status string
			if err := s.db.QueryRow(`select e.state,r.status from supervision_executions e join runs r on r.id=e.run_id where e.execution_id=?`, spec.ExecutionID).Scan(&state, &status); err != nil {
				t.Fatal(err)
			}
			want := "completed"
			if failed {
				want = "failed"
			}
			if state != "quiescent" || status != want || errors.Is(outcome.RunError, context.Canceled) {
				t.Fatal("remote failure/success misclassified", state, status, outcome.RunError)
			}
			var done int
			if err := s.db.QueryRow(`select count(*) from tasks where project_id=(select project_id from supervision_sessions where id=?) and status='done'`, spec.SessionID).Scan(&done); err != nil || done != 0 {
				t.Fatal("execution receipt bypassed task acceptance", done, err)
			}
		})
	}
}
