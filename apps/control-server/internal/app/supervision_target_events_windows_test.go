//go:build windows

package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

func supervisionTargetInboxFixture(t *testing.T) (*Server, supervisionExecutionPermit, *targetexec.Store, targetexec.ExecutionSpec) {
	t.Helper()
	s, _, permit, _, _ := supervisionExecutionForTest(t)
	ctx := context.Background()
	store, err := targetexec.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	targetID, err := store.Identity()
	if err != nil {
		t.Fatal(err)
	}
	ss := supervisionSessionForTest(t, s, permit.Decision.SessionID)
	var workspace targetexec.Workspace
	if err := s.db.QueryRow(`select directory_identity,canonical_path from workspace_admissions where id=?`, ss.AdmissionID).Scan(&workspace.ID, &workspace.Path); err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterWorkspace(workspace); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	control := targetexec.Control{Protocol: 1, TargetID: targetID, SessionID: ss.ID, OwnerEpoch: permit.Lease.Epoch, Version: 1, State: "running", WorkspaceID: workspace.ID, AuthorizedUntil: now.Add(30 * time.Second)}
	if err := store.ApplyControl(control, now); err != nil {
		t.Fatal(err)
	}
	input, _, err := s.supervisionSchedulerInput(ctx, permit.Decision.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	spec := targetexec.ExecutionSpec{Protocol: 1, TargetID: targetID, SessionID: ss.ID, ExecutionID: permit.Decision.ExecutionID, WorkItemID: permit.Decision.WorkItemID, Attempt: permit.Decision.Attempt, OwnerEpoch: permit.Lease.Epoch, AuthorizationVersion: 1, WorkspaceID: workspace.ID, Role: permit.Role, AgentID: permit.AgentID, Contract: "test-controlled-v1", CLIPath: "/usr/bin/native-test", CLIVersion: "1.0", InputSHA256: supervisionSHA256(input), InstructionsSHA256: supervisionSHA256([]byte(supervisionRolePrompt(permit.Role))), PreparedAt: now, TurnDeadline: now.Add(time.Minute)}
	var runnerID string
	if err := s.db.QueryRow(`select agent_runtime_id from runs where id=?`, permit.RunID).Scan(&runnerID); err != nil {
		t.Fatal(err)
	}
	if err := s.recordSupervisionTargetBinding(ctx, runnerID, &supervisionTargetLink{TargetID: targetID, Artifact: targetexec.HelperArtifact{OS: "linux", Arch: "amd64", SHA256: strings.Repeat("a", 64)}}); err != nil {
		t.Fatal(err)
	}
	if err := s.stageSupervisionTargetControl(ctx, ss.ID, runnerID, targetID, workspace.ID); err != nil {
		t.Fatal(err)
	}
	// The actual target control above was applied successfully. Persist its ACK
	// and resolve fixed target metadata before admitting any CLI execution.
	if _, err := s.db.Exec(`update supervision_target_controls set wire_control=?,acknowledged_version=1 where session_id=?`, string(mustJSON(control)), ss.ID); err != nil {
		t.Fatal(err)
	}
	intent, err := loadSupervisionTargetControl(ctx, s.db, ss.ID)
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := s.supervisionTargetExecutionInput(ctx, permit)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.recordSupervisionTargetResolution(ctx, permit, intent.Control, frozen, supervisionCLIInfo{AgentID: permit.AgentID, Contract: spec.Contract, Path: spec.CLIPath, Version: spec.CLIVersion}); err != nil {
		t.Fatal(err)
	}
	if err := s.stageSupervisionTargetExecution(ctx, permit, spec); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Prepare(spec, now); err != nil {
		t.Fatal(err)
	}
	return s, permit, store, spec
}

func appendSupervisionInboxFrame(t *testing.T, store *targetexec.Store, spec targetexec.ExecutionSpec, key, kind string, payload any) {
	t.Helper()
	if _, err := store.AppendEvent(spec.SessionID, spec.ExecutionID, key, kind, mustJSON(payload)); err != nil {
		t.Fatal(err)
	}
}

func TestSupervisionTargetInboxPersistsTranscriptAndCursorAtomicallyAcrossReplay(t *testing.T) {
	s, permit, store, spec := supervisionTargetInboxFixture(t)
	ctx := context.Background()
	appendSupervisionInboxFrame(t, store, spec, "session", "cli.session", map[string]string{"sessionId": "native-target-session"})
	appendSupervisionInboxFrame(t, store, spec, "init", "cli.initialized", map[string]any{})
	appendSupervisionInboxFrame(t, store, spec, "text-1", "assistant.message", map[string]string{"content": "identical real output"})
	appendSupervisionInboxFrame(t, store, spec, "text-2", "assistant.message", map[string]string{"content": "identical real output"})
	appendSupervisionInboxFrame(t, store, spec, "usage", "cli.event", map[string]any{"type": "turn.completed", "payload": map[string]any{"usage": map[string]int{"input_tokens": 12, "output_tokens": 4}}})
	appendSupervisionInboxFrame(t, store, spec, "result", "execution.result", map[string]any{"receipt": map[string]string{"output": "historical result"}, "failed": false})
	page, err := store.EventsAfter(spec.SessionID, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.consumeSupervisionTargetEvents(ctx, spec.SessionID, spec.TargetID, 0, page); err != nil {
		t.Fatal(err)
	}
	if err := s.consumeSupervisionTargetEvents(ctx, spec.SessionID, spec.TargetID, 0, page); err != nil {
		t.Fatal("replayed first page", err)
	}
	page, err = store.EventsAfter(spec.SessionID, 2, 100) // overlap the committed prefix
	if err != nil {
		t.Fatal(err)
	}
	if err := s.consumeSupervisionTargetEvents(ctx, spec.SessionID, spec.TargetID, 2, page); err != nil {
		t.Fatal(err)
	}
	if err := s.consumeSupervisionTargetEvents(ctx, spec.SessionID, spec.TargetID, 2, page); err != nil {
		t.Fatal(err)
	}
	cursor, err := s.supervisionTargetCursor(ctx, spec.SessionID, spec.TargetID)
	if err != nil || cursor != 6 {
		t.Fatal("lost durable cursor", cursor, err)
	}
	var messages, events, frames int
	if err := s.db.QueryRow(`select count(*) from messages where run_id=? and role='assistant'`, permit.RunID).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`select count(*) from events where run_id=?`, permit.RunID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`select count(*) from supervision_target_inbox where session_id=?`, spec.SessionID).Scan(&frames); err != nil {
		t.Fatal(err)
	}
	if messages != 2 || events != 6 || frames != 6 {
		t.Fatal("replay duplicated or text equality merged output", messages, events, frames)
	}
	var nativeID, state string
	var initialized bool
	if err := s.db.QueryRow(`select c.agent_session_id,c.agent_initialized,e.state from conversations c join runs r on r.conversation_id=c.id join supervision_executions e on e.run_id=r.id where r.id=?`, permit.RunID).Scan(&nativeID, &initialized, &state); err != nil {
		t.Fatal(err)
	}
	if nativeID != "native-target-session" || !initialized || state != "prepared" {
		t.Fatal("history changed execution authority", nativeID, initialized, state)
	}
	// A valid, rehashed alternate frame at an old sequence cannot replace the
	// already consumed event, even though its own checksum verifies.
	// Reuse the original journal's next frame at the old cursor: checksum does
	// not bind sequence, while the controller's inbox does.
	forged := targetexec.EventPage{FirstSequence: 1, NextSequence: 4, Events: []targetexec.Event{page.Events[1]}}
	forged.Events[0].Sequence = 3
	if err := targetexec.ValidateEventPage(forged, 2, 100); err != nil {
		t.Fatal("invalid replay fixture", err)
	}
	if err := s.consumeSupervisionTargetEvents(ctx, spec.SessionID, spec.TargetID, 2, forged); !errors.Is(err, targetexec.ErrConflict) {
		t.Fatal("consumed sequence accepted different event", err)
	}
}

func TestSupervisionTargetInboxRollsBackWholePageOnInvalidTail(t *testing.T) {
	s, permit, store, spec := supervisionTargetInboxFixture(t)
	appendSupervisionInboxFrame(t, store, spec, "good", "assistant.message", map[string]string{"content": "must roll back"})
	appendSupervisionInboxFrame(t, store, spec, "bad", "unexpected.event", map[string]string{"content": "unknown protocol"})
	page, err := store.EventsAfter(spec.SessionID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.consumeSupervisionTargetEvents(context.Background(), spec.SessionID, spec.TargetID, 0, page); !errors.Is(err, targetexec.ErrProtocol) {
		t.Fatal(err)
	}
	cursor, err := s.supervisionTargetCursor(context.Background(), spec.SessionID, spec.TargetID)
	if err != nil || cursor != 0 {
		t.Fatal("failed transaction acknowledged a frame", cursor, err)
	}
	var count int
	if err := s.db.QueryRow(`select count(*) from messages where run_id=? and role='assistant'`, permit.RunID).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed page left partial output", count, err)
	}
	page.Events[0].Payload = json.RawMessage(`{"content":"tampered"}`)
	if err := s.consumeSupervisionTargetEvents(context.Background(), spec.SessionID, spec.TargetID, 0, page); !errors.Is(err, targetexec.ErrProtocol) {
		t.Fatal("tampered page entered database", err)
	}
}

func TestSupervisionTargetExecutionIntentRejectsChangedDestinationOrInput(t *testing.T) {
	s, permit, _, spec := supervisionTargetInboxFixture(t)
	for _, change := range []string{"target", "workspace", "input", "epoch", "version", "deadline", "model"} {
		altered := spec
		switch change {
		case "target":
			altered.TargetID = "other-target"
		case "workspace":
			altered.WorkspaceID = "other-directory"
		case "input":
			altered.InputSHA256 = strings.Repeat("b", 64)
		case "epoch":
			altered.OwnerEpoch++
		case "version":
			altered.AuthorizationVersion++
		case "deadline":
			altered.TurnDeadline = altered.TurnDeadline.Add(time.Second)
		case "model":
			altered.Model = "other-model"
		}
		if err := s.stageSupervisionTargetExecution(context.Background(), permit, altered); err == nil {
			t.Fatal("immutable dispatch changed", change)
		}
	}
	if err := s.stageSupervisionTargetExecution(context.Background(), permit, spec); err != nil {
		t.Fatal("identical dispatch could not be queried", err)
	}
}

func TestSupervisionTargetUsageAppliesCumulativeWatermarkWithoutRecharging(t *testing.T) {
	s, permit, _, spec := supervisionTargetInboxFixture(t)
	ctx := context.Background()
	state := targetexec.Execution{Spec: spec, State: "quiescent", Usage: targetexec.Usage{Sequence: 1, InputTokens: 12, OutputTokens: 4, CacheReadTokens: 3, ActiveMillis: 250}}
	for i := 0; i < 3; i++ {
		if err := s.consumeSupervisionTargetUsage(ctx, spec.SessionID, spec.TargetID, state); err != nil {
			t.Fatal(err)
		}
	}
	newer := state
	newer.Usage.Sequence = 2
	newer.Usage.InputTokens = 20
	newer.Usage.ActiveMillis = 300
	if err := s.consumeSupervisionTargetUsage(ctx, spec.SessionID, spec.TargetID, newer); err != nil {
		t.Fatal(err)
	}
	if err := s.consumeSupervisionTargetUsage(ctx, spec.SessionID, spec.TargetID, state); err != nil {
		t.Fatal("late old watermark should have no effect", err)
	}
	for _, kind := range []string{"same-watermark", "regression", "negative", "spec", "running"} {
		invalid := newer
		switch kind {
		case "same-watermark":
			invalid.Usage.InputTokens++
		case "regression":
			invalid.Usage.Sequence++
			invalid.Usage.InputTokens = 1
		case "negative":
			invalid.Usage.Sequence++
			invalid.Usage.CacheReadTokens = -1
		case "spec":
			invalid.Spec.WorkspaceID = "other"
		case "running":
			invalid.State = "running"
		}
		if err := s.consumeSupervisionTargetUsage(ctx, spec.SessionID, spec.TargetID, invalid); err == nil {
			t.Fatal("invalid usage changed ledger", kind)
		}
	}
	var input, output, cache, duration int64
	if err := s.db.QueryRow(`select input_tokens,output_tokens,cache_read_tokens,duration_ms from run_usage where run_id=?`, permit.RunID).Scan(&input, &output, &cache, &duration); err != nil {
		t.Fatal(err)
	}
	if input != 20 || output != 4 || cache != 3 || duration != 300 {
		t.Fatal("cumulative usage was summed or rolled back", input, output, cache, duration)
	}
	var executionState string
	if err := s.db.QueryRow(`select state from supervision_executions where execution_id=?`, spec.ExecutionID).Scan(&executionState); err != nil || executionState != "prepared" {
		t.Fatal("usage was treated as completion authority", executionState, err)
	}
}
