//go:build windows

package app

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestSupervisionTargetReleaseRetainsAdmissionUntilTargetConfirmedAndRecoversLostACK(t *testing.T) {
	for _, lostACK := range []bool{false, true} {
		t.Run(map[bool]string{false: "disconnected", true: "lost-release-ack"}[lostACK], func(t *testing.T) {
			s, permit, target := supervisionTargetControlFixture(t)
			ctx := context.Background()
			link := supervisionTargetControlTestLink(t, target)
			if err := s.deliverSupervisionTargetControl(ctx, link, permit.Decision.SessionID); err != nil {
				t.Fatal(err)
			}
			// Business termination and local Run settlement have committed; no
			// target execution was ever started, but its claim still protects the
			// directory until the independent target release is confirmed.
			if err := s.withSupervisionTx(ctx, permit.Decision.SessionID, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
				if _, err := tx.ExecContext(ctx, `update supervision_executions set state='quiescent' where execution_id=?`, permit.Decision.ExecutionID); err != nil {
					return err
				}
				if err := s.settleSupervisionRunFactsTx(ctx, tx, permit.Decision.ExecutionID, permit.RunID, permit.ConversationID, "interrupted", "unknown", time.Now().UTC()); err != nil {
					return err
				}
				ss.State, ss.Control, ss.ClockAt = "stopped", "stop", nil
				ss.StateVersion++
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			unavailable := errors.New("test target disconnected")
			s.supervisionTargets = newSupervisionTargetConnections(ctx, func(context.Context, string) (*supervisionTargetLink, error) { return nil, unavailable })
			ss := supervisionSessionForTest(t, s, permit.Decision.SessionID)
			if err := s.releaseTerminalSupervision(ctx, ss); !errors.Is(err, unavailable) {
				t.Fatal("disconnection falsely released target", err)
			}
			var state string
			if err := s.db.QueryRow(`select state from workspace_admissions where id=?`, ss.AdmissionID).Scan(&state); err != nil || state != "releasing" {
				t.Fatal("failed target release did not retain releasing", state, err)
			}
			if err := s.releaseWorkspaceAdmission(ctx, ss.AdmissionID, ss.ID, ss.AdmissionEpoch); !errors.Is(err, errWorkspaceNotQuiescent) {
				t.Fatal("local release bypassed target obligation", err)
			}
			if lostACK {
				if err := s.deliverSupervisionTargetControl(ctx, link, ss.ID); err != nil {
					t.Fatal(err)
				}
				actual, err := target.store.Session(ss.ID)
				if err != nil {
					t.Fatal(err)
				}
				if err := target.store.Release(ss.ID, actual.Control.OwnerEpoch, actual.Control.Version); err != nil {
					t.Fatal(err)
				}
				// The target committed release, while the controller has no proof.
				var count int
				if err := s.db.QueryRow(`select count(*) from supervision_target_releases where session_id=?`, ss.ID).Scan(&count); err != nil || count != 0 {
					t.Fatal("lost ACK fixture already had local proof", count, err)
				}
			}
			s.supervisionTargets.Close()
			s.supervisionTargets = newSupervisionTargetConnections(ctx, func(context.Context, string) (*supervisionTargetLink, error) { return link, nil })
			if err := s.releaseTerminalSupervision(ctx, ss); err != nil {
				t.Fatal("release retry failed to reconcile original target", err)
			}
			actual, err := target.store.Session(ss.ID)
			if err != nil || !actual.Released || actual.Control.State != "stopped" {
				t.Fatal("target protection was not durably released", err)
			}
			var admissions, proofs int
			if err := s.db.QueryRow(`select (select count(*) from workspace_admissions where id=?),(select count(*) from supervision_target_releases where session_id=? and admission_epoch=?)`, ss.AdmissionID, ss.ID, ss.AdmissionEpoch).Scan(&admissions, &proofs); err != nil || admissions != 0 || proofs != 1 {
				t.Fatal("target release proof and local ownership disagreed", admissions, proofs, err)
			}
			s.supervisionTargets.Close()
			if err := s.releaseSupervisionTargetProtection(ctx, ss.ID); err != nil {
				t.Fatal("confirmed irreversible release depended on reconnecting", err)
			}
		})
	}
}
