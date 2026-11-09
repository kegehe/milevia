package app

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

type supervisionTargetFinalization struct {
	Spec   targetexec.FinalizationSpec `json:"spec"`
	Checks []supervisionCheck          `json:"checks"`
}

type supervisionTargetEnvironments struct {
	Checks []map[string]string `json:"checks"`
	SHA256 string              `json:"sha256"`
}

const supervisionTargetEnvironmentCheckLimit = 4096

func collectSupervisionTargetEnvironments(ctx context.Context, root string, checks []supervisionCheck) (supervisionTargetEnvironments, error) {
	if len(checks) > supervisionTargetEnvironmentCheckLimit {
		return supervisionTargetEnvironments{}, targetexec.ErrLimit
	}
	environments := map[string]string{}
	observations := map[string]string{}
	specs := map[string]supervisionCheckEnvironmentSpec{}
	for _, check := range checks {
		key := supervisionCheckSemanticHash(check)
		if _, exists := environments[key]; exists {
			continue
		}
		spec, err := resolveSupervisionCheckEnvironment(check)
		if err != nil {
			return supervisionTargetEnvironments{}, err
		}
		observationKey := supervisionSHA256(mustJSON(spec))
		hash, exists := observations[observationKey]
		if !exists {
			hash, err = spec.collect(ctx, root)
			if err != nil {
				return supervisionTargetEnvironments{}, err
			}
			observations[observationKey], specs[observationKey] = hash, spec
		}
		environments[key] = hash
	}
	// Reobserve each distinct runtime once. A binary/dependency change while
	// processing a large batch cannot hide behind an early cached observation.
	for key, spec := range specs {
		hash, err := spec.collect(ctx, root)
		if err != nil {
			return supervisionTargetEnvironments{}, err
		}
		if hash != observations[key] {
			return supervisionTargetEnvironments{}, errSupervisionSnapshotChanged
		}
	}
	keys := make([]string, 0, len(environments))
	for key := range environments {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	ordered := []map[string]string{}
	for _, key := range keys {
		ordered = append(ordered, map[string]string{"check": key, "environment": environments[key]})
	}
	return supervisionTargetEnvironments{Checks: ordered, SHA256: supervisionSHA256(mustJSON(ordered))}, nil
}

// Final observations are collected on the project host. The durable receipt
// freezes further target writes; the controller still owns version comparison,
// accepted subset, Task.done and the separate releasing transaction.
func (r *supervisionTargetRuntime) handleFinalization(ctx context.Context, request targetexec.Request) (json.RawMessage, error) {
	var payload supervisionTargetFinalization
	if json.Unmarshal(request.Parameters, &payload) != nil || payload.Spec.SessionID != request.SessionID || payload.Spec.WorkspaceID != request.WorkspaceID {
		return nil, targetexec.ErrConflict
	}
	if request.Method == "finalize.clear" {
		// The receipt belongs to the old grant. Requiring its frozen epoch and
		// version to equal the new request would make clearing impossible after
		// recovery. Bind the exact prior spec, then let the store authenticate
		// the strictly newer current authorization carried by the request.
		prior, err := r.store.Finalization(payload.Spec.ID)
		if err != nil {
			return nil, err
		}
		if prior.Spec != payload.Spec {
			return nil, targetexec.ErrConflict
		}
		return nil, r.store.ClearFinalization(request.SessionID, payload.Spec.ID, request.OwnerEpoch, request.AuthorizationVersion)
	}
	if payload.Spec.OwnerEpoch != request.OwnerEpoch || payload.Spec.AuthorizationVersion != request.AuthorizationVersion {
		return nil, targetexec.ErrConflict
	}
	finalization, fresh, err := r.store.BeginFinalization(payload.Spec, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if !fresh {
		return json.Marshal(finalization)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	admission, err := r.workspace(payload.Spec.WorkspaceID)
	if err != nil {
		return nil, err
	}
	before, err := collectSupervisionWorkspace(ctx, admission, r.storage)
	if err != nil {
		return nil, err
	}
	environments, err := collectSupervisionTargetEnvironments(ctx, admission.OriginalPath, payload.Checks)
	if err != nil {
		return nil, err
	}
	after, err := collectSupervisionWorkspace(ctx, admission, r.storage)
	if err != nil {
		return nil, err
	}
	if before.RevisionID != after.RevisionID || before.RulesHash != after.RulesHash || after.RevisionID != payload.Spec.RevisionID || after.RulesHash != payload.Spec.RulesHash || environments.SHA256 != payload.Spec.EnvironmentHash {
		return nil, errSupervisionSnapshotChanged
	}
	if err := r.store.SealFinalization(payload.Spec, time.Now().UTC()); err != nil {
		return nil, err
	}
	session, err := r.store.Session(request.SessionID)
	if err != nil {
		return nil, err
	}
	return json.Marshal(session.Finalization)
}
