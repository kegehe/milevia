package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

type supervisionTargetCapture struct {
	RevisionID string                       `json:"revisionId"`
	RulesHash  string                       `json:"rulesHash"`
	Manifest   supervisionWorkspaceManifest `json:"manifest"`
	Rules      []supervisionSourceRule      `json:"rules"`
}
type supervisionTargetRead struct {
	RevisionID string   `json:"revisionId"`
	RulesHash  string   `json:"rulesHash"`
	Paths      []string `json:"paths"`
}

type supervisionTargetBlobRequest struct {
	RevisionID string `json:"revisionId"`
	RulesHash  string `json:"rulesHash"`
	SHA256     string `json:"sha256"`
	Offset     int64  `json:"offset"`
	Limit      int    `json:"limit"`
}

type supervisionTargetBlobPage struct {
	RevisionID string `json:"revisionId"`
	RulesHash  string `json:"rulesHash"`
	SHA256     string `json:"sha256"`
	Offset     int64  `json:"offset"`
	Total      int64  `json:"total"`
	Data       []byte `json:"data"`
}
type supervisionTargetMutation struct {
	Spec  targetexec.MutationSpec `json:"spec"`
	Plan  supervisionTaskPlan     `json:"plan"`
	Edits []supervisionEdit       `json:"edits"`
}

func (r *supervisionTargetRuntime) authorizedWorkspace(request targetexec.Request) (WorkspaceAdmission, error) {
	var err error
	if request.Method == "snapshot.reconcile" || request.Method == "blob.reconcile" {
		err = r.store.CheckReconciliationRead(request.SessionID, request.WorkspaceID, request.OwnerEpoch, request.AuthorizationVersion)
	} else {
		err = r.store.CheckGrant(request.SessionID, request.WorkspaceID, request.OwnerEpoch, request.AuthorizationVersion, time.Now().UTC())
	}
	if err != nil {
		return WorkspaceAdmission{}, err
	}
	return r.workspace(request.WorkspaceID)
}

func (r *supervisionTargetRuntime) handleFiles(ctx context.Context, request targetexec.Request) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if request.Method == "mutation.query" {
		mutation, err := r.store.Mutation(request.MutationID)
		if err != nil || mutation.Spec.SessionID != request.SessionID || mutation.Spec.WorkspaceID != request.WorkspaceID {
			return nil, targetexec.ErrConflict
		}
		return json.Marshal(mutation)
	}
	if request.Method == "mutate" {
		var payload supervisionTargetMutation
		if json.Unmarshal(request.Parameters, &payload) != nil || payload.Spec.SessionID != request.SessionID || payload.Spec.ExecutionID != request.ExecutionID || payload.Spec.WorkspaceID != request.WorkspaceID || payload.Spec.OwnerEpoch != request.OwnerEpoch || payload.Spec.AuthorizationVersion != request.AuthorizationVersion {
			return nil, targetexec.ErrConflict
		}
		if prior, err := r.store.Mutation(payload.Spec.ID); err == nil {
			raw, _ := json.Marshal(prior.Spec)
			expected, _ := json.Marshal(payload.Spec)
			if string(raw) != string(expected) {
				return nil, targetexec.ErrConflict
			}
			return json.Marshal(prior)
		}
	}
	admission, err := r.authorizedWorkspace(request)
	if err != nil {
		return nil, err
	}
	collected, err := collectSupervisionWorkspace(ctx, admission, r.storage)
	if err != nil {
		return nil, err
	}
	switch request.Method {
	case "blob", "blob.reconcile":
		// Controller CAS transfer is distinct from the model's bounded read
		// action. Never widen read_context to fit a multi-megabyte source file.
		var query supervisionTargetBlobRequest
		if json.Unmarshal(request.Parameters, &query) != nil || query.RevisionID != collected.RevisionID || query.RulesHash != collected.RulesHash || query.Offset < 0 || query.Limit < 1 || query.Limit > 256<<10 {
			return nil, targetexec.ErrConflict
		}
		body, ok := collected.Blobs[query.SHA256]
		if !ok || query.Offset > int64(len(body)) || supervisionSHA256(body) != query.SHA256 {
			return nil, targetexec.ErrConflict
		}
		end := min(int64(len(body)), query.Offset+int64(query.Limit))
		if _, err := r.authorizedWorkspace(request); err != nil {
			return nil, err
		}
		return json.Marshal(supervisionTargetBlobPage{RevisionID: collected.RevisionID, RulesHash: collected.RulesHash, SHA256: query.SHA256, Offset: query.Offset, Total: int64(len(body)), Data: body[query.Offset:end]})
	case "environment":
		var payload struct {
			Plan   supervisionTaskPlan `json:"plan"`
			Checks []supervisionCheck  `json:"checks"`
		}
		if json.Unmarshal(request.Parameters, &payload) != nil {
			return nil, targetexec.ErrConflict
		}
		if err := validateSupervisionPlan(&payload.Plan, collected.Manifest); err != nil {
			return nil, err
		}
		for _, check := range payload.Checks {
			if err := validateSupervisionCheck(check, payload.Plan, collected.Manifest); err != nil {
				return nil, err
			}
		}
		environments, err := collectSupervisionTargetEnvironments(ctx, admission.OriginalPath, payload.Checks)
		if err != nil {
			return nil, err
		}
		if _, err := r.authorizedWorkspace(request); err != nil {
			return nil, err
		}
		return json.Marshal(environments)
	case "snapshot", "snapshot.reconcile":
		if _, err := r.authorizedWorkspace(request); err != nil {
			return nil, err
		}
		return json.Marshal(supervisionTargetCapture{RevisionID: collected.RevisionID, RulesHash: collected.RulesHash, Manifest: collected.Manifest, Rules: collected.Rules})
	case "read":
		var read supervisionTargetRead
		if json.Unmarshal(request.Parameters, &read) != nil || read.RevisionID != collected.RevisionID || read.RulesHash != collected.RulesHash || len(read.Paths) == 0 || len(read.Paths) > 100 {
			return nil, errSupervisionSnapshotChanged
		}
		sources := []map[string]string{}
		size := 0
		for _, name := range read.Paths {
			found := false
			for _, entry := range collected.Manifest.Entries {
				if entry.Path != name {
					continue
				}
				content, ok := collected.Blobs[entry.SHA256]
				if !ok || entry.Kind != "file" || entry.Gap != "" {
					return nil, errors.New("目标源码未覆盖或不可读取")
				}
				size += len(content)
				if size > 512<<10 {
					return nil, errors.New("目标源码正文超限，请分批读取")
				}
				sources = append(sources, map[string]string{"path": name, "sha256": entry.SHA256, "content": string(content)})
				found = true
				break
			}
			if !found {
				return nil, errors.New("目标源码不存在")
			}
		}
		if _, err := r.authorizedWorkspace(request); err != nil {
			return nil, err
		}
		return json.Marshal(sources)
	case "mutate":
		var mutation supervisionTargetMutation
		if json.Unmarshal(request.Parameters, &mutation) != nil || mutation.Spec.SessionID != request.SessionID || mutation.Spec.ExecutionID != request.ExecutionID || mutation.Spec.WorkspaceID != request.WorkspaceID || mutation.Spec.OwnerEpoch != request.OwnerEpoch || mutation.Spec.AuthorizationVersion != request.AuthorizationVersion {
			return nil, targetexec.ErrConflict
		}
		if mutation.Spec.InputRevision != collected.RevisionID || mutation.Spec.RulesHash != collected.RulesHash || len(mutation.Edits) != len(mutation.Spec.Files) {
			return nil, errSupervisionSnapshotChanged
		}
		pc := supervisionProjectContext{RevisionID: collected.RevisionID, RulesHash: collected.RulesHash, Manifest: collected.Manifest, Rules: collected.Rules}
		if err := validateSupervisionPlan(&mutation.Plan, collected.Manifest); err != nil {
			return nil, err
		}
		if err := validateSupervisionEdits(mutation.Edits, mutation.Plan, pc); err != nil {
			return nil, err
		}
		for i, edit := range mutation.Edits {
			file := mutation.Spec.Files[i]
			hash := ""
			if !edit.Delete {
				hash = supervisionSHA256([]byte(edit.Content))
			}
			if file.Path != edit.Path || file.BeforeSHA256 != edit.BaseSHA256 || file.AfterSHA256 != hash || file.Delete != edit.Delete {
				return nil, targetexec.ErrConflict
			}
		}
		_, fresh, err := r.store.BeginMutation(mutation.Spec, time.Now().UTC())
		if err != nil {
			return nil, err
		}
		if !fresh {
			return nil, targetexec.ErrConflict
		}
		defer r.store.AbandonMutation(mutation.Spec.ID)
		root, err := os.OpenRoot(admission.OriginalPath)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		for i, edit := range mutation.Edits {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			err := r.store.ApplyMutationFile(mutation.Spec.ID, i, time.Now().UTC(), func(targetexec.FileChange) error { return applySupervisionTargetFile(root, admission, edit) })
			if err != nil {
				return nil, err
			}
		}
		after, err := collectSupervisionWorkspace(ctx, admission, r.storage)
		if err != nil {
			return nil, err
		}
		if err := validateSupervisionMutationResult(collected, after, mutation.Edits); err != nil {
			return nil, err
		}
		if err := r.store.FinishMutation(mutation.Spec.ID, after.RevisionID, time.Now().UTC()); err != nil {
			return nil, err
		}
		result, err := r.store.Mutation(mutation.Spec.ID)
		if err != nil {
			return nil, err
		}
		return json.Marshal(result)
	default:
		return nil, targetexec.ErrProtocol
	}
}

func applySupervisionTargetFile(root *os.Root, admission WorkspaceAdmission, edit supervisionEdit) error {
	if err := verifyWorkspaceAdmissionPath(admission); err != nil {
		return err
	}
	if err := supervisionRootPathSafe(root, edit.Path); err != nil {
		return err
	}
	name := filepath.FromSlash(edit.Path)
	mode := os.FileMode(0644)
	info, err := root.Lstat(name)
	if err == nil {
		if !info.Mode().IsRegular() {
			return errSupervisionSnapshotChanged
		}
		raw, err := root.ReadFile(name)
		if err != nil || supervisionSHA256(raw) != edit.BaseSHA256 {
			return errSupervisionSnapshotChanged
		}
		mode = info.Mode().Perm()
	} else if !errors.Is(err, os.ErrNotExist) || edit.BaseSHA256 != "" {
		return errSupervisionSnapshotChanged
	}
	if edit.Delete {
		return root.Remove(name)
	}
	if err := root.MkdirAll(filepath.Dir(name), 0755); err != nil {
		return err
	}
	temp := filepath.Join(filepath.Dir(name), ".milevia-write-"+uuid.NewString())
	f, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, writeErr := f.Write([]byte(edit.Content))
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		root.Remove(temp)
		return err
	}
	if err := root.Rename(temp, name); err != nil {
		root.Remove(temp)
		return err
	}
	return nil
}
