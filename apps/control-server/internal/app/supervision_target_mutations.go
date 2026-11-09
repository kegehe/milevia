package app

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

// Edits remain a controller decision. Only the authenticated target service may
// apply them, against its own filesystem identity, grant and durable journal.
// A lost reply leaves the local journal unknown; this path never replays edits.
func (s *Server) applySupervisionTargetEdits(ctx context.Context, source supervisionWorkspaceSource, permit supervisionExecutionPermit, job supervisionRoleJob, plan supervisionTaskPlan, pc supervisionProjectContext, before supervisionCollectedContext, edits []supervisionEdit) (revision string, resultErr error) {
	if source.Control == nil || permit.Role != "implementation" || job.Kind != "implementation" || job.ExecutionID != permit.Decision.ExecutionID || job.TaskID != permit.TaskID || job.InputRevision != pc.RevisionID || len(edits) == 0 {
		return "", targetexec.ErrConflict
	}
	c := *source.Control
	spec := targetexec.MutationSpec{ID: "edit-" + job.ExecutionID, SessionID: c.SessionID, ExecutionID: job.ExecutionID, WorkspaceID: c.WorkspaceID, OwnerEpoch: c.OwnerEpoch, AuthorizationVersion: c.Version, InputRevision: pc.RevisionID, RulesHash: pc.RulesHash}
	for _, edit := range edits {
		file := targetexec.FileChange{Path: edit.Path, BeforeSHA256: edit.BaseSHA256, Delete: edit.Delete}
		if !edit.Delete {
			file.AfterSHA256 = supervisionSHA256([]byte(edit.Content))
		}
		spec.Files = append(spec.Files, file)
	}
	err := s.withSupervisionTx(ctx, c.SessionID, time.Now().UTC(), func(tx *sql.Tx, admissions map[string]WorkspaceAdmission, ss *supervisionSession) error {
		if err := source.verifyTx(ctx, s, tx, ss, admissions[ss.AdmissionID]); err != nil {
			return err
		}
		state, err := checkSupervisionExecutionTx(ctx, tx, ss, permit)
		if err != nil {
			return err
		}
		if state != "quiescent" || ss.StateVersion != permit.Decision.ExpectedStateVersion {
			return errSupervisionState
		}
		if err := validateSupervisionEdits(edits, plan, pc); err != nil {
			return err
		}
		var raw string
		if err := tx.QueryRowContext(ctx, `select spec from supervision_target_executions where execution_id=? and session_id=? and target_id=?`, job.ExecutionID, ss.ID, c.TargetID).Scan(&raw); err != nil {
			return err
		}
		var execution targetexec.ExecutionSpec
		if json.Unmarshal([]byte(raw), &execution) != nil || execution.TargetID != c.TargetID || execution.SessionID != spec.SessionID || execution.ExecutionID != job.ExecutionID || execution.Role != permit.Role || execution.WorkspaceID != spec.WorkspaceID || execution.OwnerEpoch != spec.OwnerEpoch || execution.AuthorizationVersion != spec.AuthorizationVersion {
			return targetexec.ErrConflict
		}
		if _, err := tx.ExecContext(ctx, `insert into supervision_mutations(execution_id,session_id,state,edits,created_at) values (?,?,'applying',?,?)`, job.ExecutionID, ss.ID, string(mustJSON(edits)), time.Now().UTC()); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `insert into supervision_target_mutations(execution_id,session_id,target_id,spec) values (?,?,?,?)`, job.ExecutionID, ss.ID, c.TargetID, string(mustJSON(spec)))
		return err
	})
	if err != nil {
		return "", err
	}
	defer func() {
		settle, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resultErr = errors.Join(resultErr, s.finishSupervisionMutation(settle, c.SessionID, job.ExecutionID, revision, edits, resultErr))
	}()
	link, err := source.connect(ctx, s)
	if err != nil {
		return "", err
	}
	defer link.Close()
	request := targetexec.Request{ID: uuid.NewString(), Method: "mutate", SessionID: c.SessionID, ExecutionID: job.ExecutionID, WorkspaceID: c.WorkspaceID, OwnerEpoch: c.OwnerEpoch, AuthorizationVersion: c.Version, Parameters: mustJSON(supervisionTargetMutation{Spec: spec, Plan: plan, Edits: edits})}
	response, err := link.Call(ctx, request)
	if err != nil {
		return "", err
	}
	var receipt targetexec.Mutation
	if json.Unmarshal(response.Data, &receipt) != nil || !supervisionTargetMutationCompleted(spec, receipt) {
		return "", targetexec.ErrConflict
	}
	// Independently read durable facts instead of accepting an output message as
	// proof. The target must retain the original ID even after response loss.
	response, err = link.Call(ctx, targetexec.Request{ID: uuid.NewString(), Method: "mutation.query", SessionID: c.SessionID, WorkspaceID: c.WorkspaceID, MutationID: spec.ID})
	if err != nil {
		return "", err
	}
	var persisted targetexec.Mutation
	if json.Unmarshal(response.Data, &persisted) != nil || string(mustJSON(persisted)) != string(mustJSON(receipt)) {
		return "", targetexec.ErrConflict
	}
	after, err := source.collect(ctx, s)
	if err != nil {
		return "", err
	}
	if err := validateSupervisionMutationResult(before, after, edits); err != nil {
		return "", err
	}
	if after.RevisionID != receipt.OutputRevision {
		return "", errSupervisionSnapshotChanged
	}
	err = s.withSupervisionTx(ctx, c.SessionID, time.Now().UTC(), func(tx *sql.Tx, admissions map[string]WorkspaceAdmission, ss *supervisionSession) error {
		if err := source.verifyTx(ctx, s, tx, ss, admissions[ss.AdmissionID]); err != nil {
			return err
		}
		if _, err := checkSupervisionExecutionTx(ctx, tx, ss, permit); err != nil {
			return err
		}
		if ss.StateVersion != permit.Decision.ExpectedStateVersion {
			return errSupervisionState
		}
		res, err := tx.ExecContext(ctx, `update supervision_target_mutations set receipt=? where execution_id=? and session_id=? and target_id=? and spec=? and receipt=''`, string(mustJSON(receipt)), job.ExecutionID, ss.ID, c.TargetID, string(mustJSON(spec)))
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil || n != 1 {
			return errors.Join(err, targetexec.ErrConflict)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return after.RevisionID, nil
}

func supervisionTargetMutationCompleted(spec targetexec.MutationSpec, receipt targetexec.Mutation) bool {
	if string(mustJSON(receipt.Spec)) != string(mustJSON(spec)) || receipt.Digest != supervisionSHA256(mustJSON(spec)) || receipt.State != "completed" || len(receipt.OutputRevision) != 64 || len(receipt.Files) != len(spec.Files) {
		return false
	}
	if _, err := hex.DecodeString(receipt.OutputRevision); err != nil {
		return false
	}
	for _, state := range receipt.Files {
		if state != "applied" {
			return false
		}
	}
	return true
}
