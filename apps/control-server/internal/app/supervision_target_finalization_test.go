package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

func TestSupervisionTargetFinalizationClearBindsOldReceiptToNewGrantAndRetries(t *testing.T) {
	target, control, _ := targetRuntimeFixture(t)
	ctx := context.Background()
	spec := targetexec.FinalizationSpec{ID: "final", SessionID: control.SessionID, WorkspaceID: control.WorkspaceID, OwnerEpoch: control.OwnerEpoch, AuthorizationVersion: control.Version, BindingSHA256: strings.Repeat("a", 64), RevisionID: strings.Repeat("b", 64), RulesHash: strings.Repeat("c", 64), EnvironmentHash: strings.Repeat("d", 64)}
	if _, _, err := target.store.BeginFinalization(spec, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := target.store.SealFinalization(spec, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	request := targetRequest(control, "finalize.clear")
	request.Parameters = mustJSON(supervisionTargetFinalization{Spec: spec})
	if _, err := target.handle(ctx, request); !errors.Is(err, targetexec.ErrAuthority) {
		t.Fatal("original grant reopened handoff", err)
	}
	control.Version++
	control.OwnerEpoch++
	control.AuthorizedUntil = time.Now().Add(30 * time.Second)
	if err := target.store.ApplyControl(control, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	request = targetRequest(control, "finalize.clear")
	for _, change := range []func(*targetexec.FinalizationSpec){
		func(p *targetexec.FinalizationSpec) { p.BindingSHA256 = strings.Repeat("e", 64) },
		func(p *targetexec.FinalizationSpec) { p.AuthorizationVersion = control.Version },
		func(p *targetexec.FinalizationSpec) { p.WorkspaceID = "foreign" },
	} {
		forged := spec
		change(&forged)
		request.Parameters = mustJSON(supervisionTargetFinalization{Spec: forged})
		if _, err := target.handle(ctx, request); !errors.Is(err, targetexec.ErrConflict) {
			t.Fatal("modified original receipt was accepted", err)
		}
	}
	request.Parameters = mustJSON(supervisionTargetFinalization{Spec: spec})
	for range 2 {
		if _, err := target.handle(ctx, request); err != nil {
			t.Fatal("new grant could not clear/retry original handoff", err)
		}
	}
	if err := target.store.CheckGrant(control.SessionID, control.WorkspaceID, control.OwnerEpoch, control.Version, time.Now().UTC()); err != nil {
		t.Fatal("cleared handoff did not permit current authority", err)
	}
	next := spec
	next.ID, next.OwnerEpoch, next.AuthorizationVersion = "new-final", control.OwnerEpoch, control.Version
	if _, _, err := target.store.BeginFinalization(next, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := target.handle(ctx, request); !errors.Is(err, targetexec.ErrAuthority) {
		t.Fatal("late clear erased a different active handoff", err)
	}
}
