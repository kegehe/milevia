package app

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

// Network I/O stays outside controller transactions/admission mutexes. Every
// chunk is tied to the same target snapshot, and the final capture must still
// match; an interrupted or changed transfer cannot become a complete CAS input.
func collectSupervisionTargetWorkspace(ctx context.Context, link *supervisionTargetLink, control targetexec.Control, renew ...func(context.Context) error) (supervisionCollectedContext, error) {
	return collectSupervisionTargetWorkspaceMode(ctx, link, control, false, renew...)
}

func collectSupervisionTargetWorkspaceMode(ctx context.Context, link *supervisionTargetLink, control targetexec.Control, reconcile bool, renew ...func(context.Context) error) (supervisionCollectedContext, error) {
	out := supervisionCollectedContext{Blobs: map[string][]byte{}}
	if link == nil || link.Client == nil || link.TargetID != control.TargetID || !reconcile && control.State != "running" || reconcile && (control.State != "paused" || control.DrainExecutionID != "") {
		return out, targetexec.ErrAuthority
	}
	nextRenewal := time.Now().Add(8 * time.Second)
	call := func(method string, parameters any) (targetexec.Response, error) {
		if reconcile {
			method += ".reconcile"
		}
		if !reconcile && len(renew) == 1 && renew[0] != nil && !time.Now().Before(nextRenewal) {
			// Only the domain owner can grant more time after rechecking the
			// current budget, lease and control version. Source transfer alone
			// must never mint another unconditional 30-second authorization.
			if err := renew[0](ctx); err != nil {
				return targetexec.Response{}, err
			}
			nextRenewal = time.Now().Add(8 * time.Second)
		}
		return link.Call(ctx, targetexec.Request{ID: uuid.NewString(), Method: method, SessionID: control.SessionID, WorkspaceID: control.WorkspaceID, OwnerEpoch: control.OwnerEpoch, AuthorizationVersion: control.Version, Parameters: mustJSON(parameters)})
	}
	response, err := call("snapshot", nil)
	if err != nil {
		return out, err
	}
	var capture supervisionTargetCapture
	if json.Unmarshal(response.Data, &capture) != nil || capture.Manifest.Policy != supervisionSnapshotPolicy || capture.Manifest.TargetOS != link.Artifact.OS || capture.Manifest.DirectoryIdentity != control.WorkspaceID || capture.RevisionID != supervisionSHA256(mustJSON(capture.Manifest)) || capture.RulesHash != supervisionSHA256(mustJSON(capture.Rules)) || len(capture.Manifest.Entries) > supervisionSnapshotEntryLimit {
		return out, targetexec.ErrProtocol
	}
	out.RevisionID, out.RulesHash, out.Manifest, out.Rules = capture.RevisionID, capture.RulesHash, capture.Manifest, capture.Rules
	var total int64
	for _, entry := range capture.Manifest.Entries {
		if entry.Kind != "file" || entry.Gap != "" {
			continue
		}
		if _, exists := out.Blobs[entry.SHA256]; exists {
			continue
		}
		if entry.Size < 0 || entry.Size > supervisionSnapshotFileLimit || total+entry.Size > supervisionSnapshotTotalLimit {
			return supervisionCollectedContext{}, targetexec.ErrLimit
		}
		body := make([]byte, 0, int(entry.Size))
		for {
			response, err = call("blob", supervisionTargetBlobRequest{RevisionID: capture.RevisionID, RulesHash: capture.RulesHash, SHA256: entry.SHA256, Offset: int64(len(body)), Limit: 256 << 10})
			if err != nil {
				return supervisionCollectedContext{}, err
			}
			var page supervisionTargetBlobPage
			if json.Unmarshal(response.Data, &page) != nil || page.RevisionID != capture.RevisionID || page.RulesHash != capture.RulesHash || page.SHA256 != entry.SHA256 || page.Offset != int64(len(body)) || page.Total != entry.Size || len(page.Data) > 256<<10 || int64(len(body)+len(page.Data)) > entry.Size {
				return supervisionCollectedContext{}, targetexec.ErrProtocol
			}
			body = append(body, page.Data...)
			if int64(len(body)) == entry.Size {
				break
			}
			if len(page.Data) == 0 {
				return supervisionCollectedContext{}, targetexec.ErrProtocol
			}
		}
		if supervisionSHA256(body) != entry.SHA256 {
			return supervisionCollectedContext{}, targetexec.ErrProtocol
		}
		out.Blobs[entry.SHA256] = body
		total += entry.Size
	}
	response, err = call("snapshot", nil)
	if err != nil {
		return supervisionCollectedContext{}, err
	}
	var after supervisionTargetCapture
	if json.Unmarshal(response.Data, &after) != nil {
		return supervisionCollectedContext{}, targetexec.ErrProtocol
	}
	if string(mustJSON(after)) != string(mustJSON(capture)) {
		return supervisionCollectedContext{}, errors.Join(errSupervisionSnapshotChanged, targetexec.ErrConflict)
	}
	return out, nil
}
