package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

// The inbox and cursor commit with the displayed messages/events. A connection
// retry can neither append the same assistant output twice nor acknowledge a
// frame whose transcript failed to persist. Receiving history grants no new
// execution authority and does not apply a decision or acceptance result.
func (s *Server) migrateSupervisionTargetEvents(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `create table if not exists supervision_profile_targets (
	 revision_id text primary key references agent_profile_revisions(id) on delete restrict, target_id text not null, connection_key text not null
	);
	create trigger if not exists supervision_profile_targets_immutable before update on supervision_profile_targets begin select raise(abort,'supervision profile destinations are immutable'); end;
	create table if not exists supervision_target_cursors (
	 session_id text primary key references supervision_sessions(id) on delete cascade,
	 target_id text not null, sequence integer not null default 0 check(sequence>=0)
	);
	create table if not exists supervision_target_executions (
	 execution_id text primary key references supervision_executions(execution_id) on delete cascade,
	 session_id text not null references supervision_sessions(id) on delete cascade,
	 target_id text not null, spec text not null, native_session_id text not null default ''
	);
	create table if not exists supervision_target_inbox (
	 session_id text not null references supervision_target_cursors(session_id) on delete cascade,
	 sequence integer not null check(sequence>0), event_key text not null,
	 execution_id text not null references supervision_target_executions(execution_id) on delete cascade,
	 sha256 text not null, primary key(session_id,sequence), unique(session_id,event_key)
	);
	create table if not exists supervision_target_usage (
	 execution_id text primary key references supervision_target_executions(execution_id) on delete cascade,
	 session_id text not null references supervision_sessions(id) on delete cascade, counters text not null
	);
	create table if not exists supervision_target_resolutions (
	 execution_id text primary key references supervision_executions(execution_id) on delete cascade,
	 session_id text not null references supervision_sessions(id) on delete cascade,
	 resolution text not null
	);
	create table if not exists supervision_target_releases (
	 session_id text primary key references supervision_sessions(id) on delete cascade,
	 target_id text not null, workspace_id text not null, admission_epoch integer not null,
	 receipt text not null
	);
	create table if not exists supervision_target_mutations (
	 execution_id text primary key references supervision_mutations(execution_id) on delete cascade,
	 session_id text not null references supervision_sessions(id) on delete cascade,
	 target_id text not null, spec text not null, receipt text not null default ''
	);
	create table if not exists supervision_target_finalizations (
	 id text primary key, session_id text not null references supervision_sessions(id) on delete cascade,
	 state text not null check(state in ('pending','committed','abandoned')),
	 state_version integer not null, admission_epoch integer not null,
	 spec text not null, checks text not null, receipt text not null default ''
	);
	create unique index if not exists supervision_target_finalizations_active on supervision_target_finalizations(session_id) where state='pending'
	;`)
	return err
}

// Persist the destination and immutable target spec before sending start. The
// controller's existing execution/Run is retained across reconnects; it is never
// replaced by a Run invented from a returned target frame.
func (s *Server) stageSupervisionTargetExecution(ctx context.Context, permit supervisionExecutionPermit, spec targetexec.ExecutionSpec) error {
	return s.withSupervisionTx(ctx, permit.Decision.SessionID, time.Now().UTC(), func(tx *sql.Tx, admissions map[string]WorkspaceAdmission, ss *supervisionSession) error {
		control, frozenInput, _, err := s.supervisionTargetDispatchBasisTx(ctx, tx, admissions, ss, permit)
		if err != nil {
			return err
		}
		a := admissions[ss.AdmissionID]
		targetID := control.TargetID
		if control.TargetID != spec.TargetID || control.WorkspaceID != spec.WorkspaceID || control.OwnerEpoch != spec.OwnerEpoch || control.Version != spec.AuthorizationVersion || control.State != "running" {
			return targetexec.ErrAuthority
		}
		if spec.Protocol != targetexec.ProtocolVersion || spec.TargetID != targetID || spec.SessionID != ss.ID || spec.ExecutionID != permit.Decision.ExecutionID || spec.WorkItemID != permit.Decision.WorkItemID || spec.Attempt != permit.Decision.Attempt || spec.OwnerEpoch != permit.Lease.Epoch || spec.AuthorizationVersion < 1 || spec.WorkspaceID != a.DirectoryIdentity || spec.Role != permit.Role || spec.AgentID != permit.AgentID || spec.InputSHA256 != supervisionSHA256(frozenInput.Observation) || spec.InstructionsSHA256 != supervisionSHA256([]byte(frozenInput.Instructions)) || spec.PreparedAt.IsZero() || !spec.TurnDeadline.After(spec.PreparedAt) || spec.TurnDeadline.Sub(spec.PreparedAt) > time.Duration(ss.Budget.MaxTurnMillis)*time.Millisecond {
			return targetexec.ErrConflict
		}
		if permit.Role == "test" {
			if spec.Contract != supervisionTargetVerificationContract || spec.ProfileRevisionID != "" || spec.ProfileSHA256 != "" || spec.CLIPath != "" || spec.InstallationPath != "" || spec.CLIVersion != "" || spec.EntrypointSHA256 != "" || spec.RuntimeVersion != "" || spec.ModelSettingsSHA256 != "" || spec.Model != "" || spec.ReasoningEffort != "" {
				return targetexec.ErrConflict
			}
		} else {
			installation, err := supervisionTargetInstallationPath(ctx, tx, permit)
			if err != nil {
				return err
			}
			if spec.InstallationPath != installation {
				return targetexec.ErrConflict
			}
			var raw string
			err = tx.QueryRowContext(ctx, `select resolution from supervision_target_resolutions where execution_id=? and session_id=?`, spec.ExecutionID, ss.ID).Scan(&raw)
			if errors.Is(err, sql.ErrNoRows) {
				return targetexec.ErrConflict // No CLI dispatch without target resolution.
			} else if err != nil {
				return err
			} else {
				var resolved supervisionTargetResolvedExecution
				bound := spec
				bound.PreparedAt, bound.TurnDeadline = time.Time{}, time.Time{}
				if json.Unmarshal([]byte(raw), &resolved) != nil || resolved.Spec != bound || resolved.RequestedModel != runModel(permit.Model, permit.Profile) || resolved.RequestedReasoningEffort != permit.ReasoningEffort {
					return targetexec.ErrConflict
				}
			}
		}
		raw := string(mustJSON(spec))
		var previous string
		err = tx.QueryRowContext(ctx, `select spec from supervision_target_executions where execution_id=?`, spec.ExecutionID).Scan(&previous)
		if err == nil {
			if previous != raw {
				return targetexec.ErrConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := tx.ExecContext(ctx, `insert into supervision_target_cursors(session_id,target_id) values (?,?) on conflict(session_id) do nothing`, ss.ID, targetID); err != nil {
			return err
		}
		var bound string
		if err := tx.QueryRowContext(ctx, `select target_id from supervision_target_cursors where session_id=?`, ss.ID).Scan(&bound); err != nil {
			return err
		}
		if bound != targetID {
			return targetexec.ErrConflict
		}
		_, err = tx.ExecContext(ctx, `insert into supervision_target_executions(execution_id,session_id,target_id,spec) values (?,?,?,?)`, spec.ExecutionID, ss.ID, targetID, raw)
		return err
	})
}

func (s *Server) supervisionTargetCursor(ctx context.Context, sessionID, targetID string) (int64, error) {
	var sequence int64
	err := s.db.QueryRowContext(ctx, `select sequence from supervision_target_cursors where session_id=? and target_id=?`, sessionID, targetID).Scan(&sequence)
	return sequence, err
}

func (s *Server) readSupervisionTargetEvents(ctx context.Context, link *supervisionTargetLink, sessionID string) (bool, error) {
	after, err := s.supervisionTargetCursor(ctx, sessionID, link.TargetID)
	if err != nil {
		return false, err
	}
	response, err := link.Call(ctx, targetexec.Request{ID: uuid.NewString(), Method: "events", SessionID: sessionID, After: after, Limit: 100})
	if err != nil {
		return false, err // A missing prefix is never acknowledged as complete.
	}
	if response.Events == nil {
		return false, targetexec.ErrProtocol
	}
	err = s.consumeSupervisionTargetEvents(ctx, sessionID, link.TargetID, after, *response.Events)
	return response.Events.More, err
}

func (s *Server) consumeSupervisionTargetEvents(ctx context.Context, sessionID, targetID string, after int64, page targetexec.EventPage) error {
	if err := targetexec.ValidateEventPage(page, after, 100); err != nil {
		return err
	}
	var published []Event
	err := s.withSupervisionTx(ctx, sessionID, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, _ *supervisionSession) error {
		var bound string
		var cursor int64
		if err := tx.QueryRowContext(ctx, `select target_id,sequence from supervision_target_cursors where session_id=?`, sessionID).Scan(&bound, &cursor); err != nil {
			return err
		}
		if bound != targetID || after > cursor {
			return targetexec.ErrConflict
		}
		for _, frame := range page.Events {
			if frame.Sequence <= cursor {
				var digest, key, executionID string
				if err := tx.QueryRowContext(ctx, `select sha256,event_key,execution_id from supervision_target_inbox where session_id=? and sequence=?`, sessionID, frame.Sequence).Scan(&digest, &key, &executionID); err != nil {
					return err
				}
				if digest != frame.SHA256 || key != frame.Key || executionID != frame.ExecutionID {
					return targetexec.ErrConflict
				}
				continue
			}
			if frame.Sequence != cursor+1 {
				return targetexec.ErrCursor
			}
			var conversationID, runID, nativeID string
			if err := tx.QueryRowContext(ctx, `select r.conversation_id,e.run_id,t.native_session_id from supervision_target_executions t join supervision_executions e on e.execution_id=t.execution_id join runs r on r.id=e.run_id where t.execution_id=? and t.session_id=? and t.target_id=? and e.session_id=?`, frame.ExecutionID, sessionID, targetID, sessionID).Scan(&conversationID, &runID, &nativeID); err != nil {
				return err
			}
			// Redact again at the controller boundary. The target digest remains
			// bound to the received bytes; sensitive raw bytes are not persisted.
			payload, err := supervisionTargetPayload(frame.Payload)
			if err != nil {
				return err
			}
			eventID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("supervision-target:%s:%s:%d", targetID, sessionID, frame.Sequence))).String()
			event := Event{ID: eventID, ConversationID: conversationID, RunID: runID, Type: "supervision.target." + frame.Type, Payload: payload, CreatedAt: time.Now().UTC()}
			switch frame.Type {
			case "cli.event":
				var source struct {
					Type    string          `json:"type"`
					Payload json.RawMessage `json:"payload"`
				}
				if json.Unmarshal(payload, &source) != nil || source.Type == "" || len(source.Type) > 128 || !json.Valid(source.Payload) || isThinkingTokensEvent(source.Type, source.Payload) {
					return targetexec.ErrProtocol
				}
				event.Type, event.Payload = source.Type, source.Payload
			case "assistant.message":
				var body struct {
					Content string `json:"content"`
					Parent  string `json:"parentToolUseId"`
				}
				if json.Unmarshal(payload, &body) != nil {
					return targetexec.ErrProtocol
				}
				message := Message{ID: eventID, ConversationID: conversationID, RunID: runID, Role: "assistant", Content: body.Content, ParentToolUseID: body.Parent, CreatedAt: event.CreatedAt}
				if _, err := tx.ExecContext(ctx, `insert into messages(id,conversation_id,run_id,role,content,parent_tool_use_id,created_at) values (?,?,?,'assistant',?,?,?)`, message.ID, conversationID, runID, message.Content, message.ParentToolUseID, message.CreatedAt); err != nil {
					return err
				}
				event.Type, event.Payload = "assistant.message", mustJSON(message)
			case "cli.session":
				var body struct {
					ID string `json:"sessionId"`
				}
				if json.Unmarshal(payload, &body) != nil || body.ID == "" || len(body.ID) > 128 || nativeID != "" && nativeID != body.ID {
					return targetexec.ErrConflict
				}
				if _, err := tx.ExecContext(ctx, `update supervision_target_executions set native_session_id=? where execution_id=?;update runs set agent_run_id=? where id=?;update conversations set agent_session_id=?,claude_session_id=? where id=?`, body.ID, frame.ExecutionID, body.ID, runID, body.ID, body.ID, conversationID); err != nil {
					return err
				}
			case "cli.initialized":
				if _, err := tx.ExecContext(ctx, `update conversations set agent_initialized=1,claude_initialized=1 where id=?`, conversationID); err != nil {
					return err
				}
			case "verification.output":
				event.Type = frame.Type
			case "execution.configuration", "execution.result", "verification.result":
				// Result history is visible but cannot settle a Run, write Task.done
				// or replace the independent durable quiescence/receipt checks.
			default:
				return targetexec.ErrProtocol
			}
			if _, err := tx.ExecContext(ctx, `insert into events(id,conversation_id,run_id,type,payload,created_at) values (?,?,?,?,?,?);insert into supervision_target_inbox(session_id,sequence,event_key,execution_id,sha256) values (?,?,?,?,?)`, event.ID, conversationID, runID, event.Type, string(event.Payload), event.CreatedAt, sessionID, frame.Sequence, frame.Key, frame.ExecutionID, frame.SHA256); err != nil {
				return err
			}
			if s.remoteRelayConfigured() {
				if err := s.enqueueRemoteEventTxWithConversation(ctx, tx, event.ID, "", runID, conversationID, event.Type, event.Payload, event.CreatedAt); err != nil {
					return err
				}
			}
			published = append(published, event)
			cursor = frame.Sequence
		}
		_, err := tx.ExecContext(ctx, `update supervision_target_cursors set sequence=? where session_id=? and target_id=?`, cursor, sessionID, targetID)
		return err
	})
	if err == nil {
		for _, event := range published {
			s.enqueueConversationEvent(event.ConversationID, mustJSON(event))
		}
		if len(published) > 0 {
			s.wakeRemoteOutbox()
		}
	}
	return err
}
