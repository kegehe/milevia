package targetexec

import (
	"encoding/json"
	"io/fs"
	"strings"
	"time"
)

// MutationSpec contains content hashes, never mutable filesystem paths supplied
// as absolute controller paths. File contents remain in the immutable role input
// and output store; recovery inspects facts and never blindly replays the batch.
type MutationSpec struct {
	ID                   string       `json:"id"`
	SessionID            string       `json:"sessionId"`
	ExecutionID          string       `json:"executionId"`
	OwnerEpoch           int64        `json:"ownerEpoch"`
	AuthorizationVersion int64        `json:"authorizationVersion"`
	WorkspaceID          string       `json:"workspaceId"`
	InputRevision        string       `json:"inputRevision"`
	RulesHash            string       `json:"rulesHash"`
	Files                []FileChange `json:"files"`
}

type FileChange struct {
	Path         string `json:"path"`
	BeforeSHA256 string `json:"beforeSha256"`
	AfterSHA256  string `json:"afterSha256"`
	Delete       bool   `json:"delete"`
}

type Mutation struct {
	Spec           MutationSpec `json:"spec"`
	Digest         string       `json:"digest"`
	State          string       `json:"state"` // prepared, applying, unknown, completed
	Files          []string     `json:"files"` // pending, writing, applied, unknown
	OutputRevision string       `json:"outputRevision,omitempty"`
}

func mutationDigest(spec MutationSpec) string { raw, _ := json.Marshal(spec); return hashBytes(raw) }

func validateMutationSpec(spec MutationSpec) error {
	if !validID(spec.ID) || !validID(spec.SessionID) || !validID(spec.ExecutionID) || spec.WorkspaceID == "" || !validHash(spec.InputRevision) || !validHash(spec.RulesHash) || len(spec.Files) == 0 || len(spec.Files) > 100 {
		return ErrConflict
	}
	seen := map[string]bool{}
	for _, file := range spec.Files {
		if !fs.ValidPath(file.Path) || file.Path == "." || strings.ContainsAny(file.Path, "\\:\x00") || seen[file.Path] || file.BeforeSHA256 != "" && !validHash(file.BeforeSHA256) || file.Delete && (file.BeforeSHA256 == "" || file.AfterSHA256 != "") || !file.Delete && !validHash(file.AfterSHA256) {
			return ErrConflict
		}
		seen[file.Path] = true
	}
	return nil
}

func (s *Store) authorizeMutation(spec MutationSpec, now time.Time) error {
	session, ok := s.facts.Sessions[spec.SessionID]
	execution, exists := session.Executions[spec.ExecutionID]
	if !ok || !exists || session.Control.State != "running" || execution.State != "quiescent" || execution.Interrupted || execution.Spec.Role != "implementation" || spec.OwnerEpoch != execution.Spec.OwnerEpoch || spec.AuthorizationVersion != session.Control.Version || spec.WorkspaceID != execution.Spec.WorkspaceID {
		return ErrAuthority
	}
	return s.authorize(session, execution.Spec, now)
}

func cloneMutation(mutation Mutation) Mutation {
	mutation.Spec.Files = append([]FileChange(nil), mutation.Spec.Files...)
	mutation.Files = append([]string(nil), mutation.Files...)
	return mutation
}

// BeginMutation is committed before the first file is replaced. Existing IDs
// return facts with fresh=false, even after response loss or process restart.
func (s *Store) BeginMutation(spec MutationSpec, now time.Time) (mutation Mutation, fresh bool, err error) {
	if err = validateMutationSpec(spec); err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Mutation{}, false, ErrClosed
	}
	if prior, ok := s.facts.Mutations[spec.ID]; ok {
		if prior.Digest != mutationDigest(spec) {
			return Mutation{}, false, ErrConflict
		}
		return cloneMutation(prior), false, nil
	}
	if err = s.authorizeMutation(spec, now); err != nil {
		return
	}
	for _, prior := range s.facts.Mutations {
		if prior.Spec.ExecutionID == spec.ExecutionID {
			return Mutation{}, false, ErrConflict
		}
		if s.workspacesOverlap(prior.Spec.WorkspaceID, spec.WorkspaceID) && (prior.State == "prepared" || prior.State == "applying") {
			return Mutation{}, false, ErrNotQuiet
		}
	}
	if s.facts.Mutations == nil {
		s.facts.Mutations = map[string]Mutation{}
	}
	mutation = Mutation{Spec: spec, Digest: mutationDigest(spec), State: "prepared", Files: make([]string, len(spec.Files))}
	for i := range mutation.Files {
		mutation.Files[i] = "pending"
	}
	s.facts.Mutations[spec.ID] = cloneMutation(mutation)
	if err = s.save(); err != nil {
		return Mutation{}, false, err
	}
	return mutation, true, nil
}

// ApplyMutationFile shares the target control gate. Only bounded local file I/O
// belongs in apply; no network, model calls or controller database transactions.
// A revoke waits for this file, then prevents every subsequent file.
func (s *Store) ApplyMutationFile(id string, index int, now time.Time, apply func(FileChange) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	mutation, ok := s.facts.Mutations[id]
	if !ok || index < 0 || index >= len(mutation.Files) || apply == nil || mutation.State != "prepared" && mutation.State != "applying" || mutation.Files[index] != "pending" {
		return ErrConflict
	}
	for i := 0; i < index; i++ {
		if mutation.Files[i] != "applied" {
			return ErrConflict
		}
	}
	if err := s.authorizeMutation(mutation.Spec, now); err != nil {
		return err
	}
	mutation.State, mutation.Files[index] = "applying", "writing"
	s.facts.Mutations[id] = mutation
	if err := s.saveControl(); err != nil {
		return err
	}
	applyErr := apply(mutation.Spec.Files[index])
	if applyErr != nil {
		mutation.State, mutation.Files[index] = "unknown", "unknown"
	} else {
		mutation.Files[index] = "applied"
	}
	s.facts.Mutations[id] = mutation
	if err := s.saveControl(); err != nil {
		return err
	}
	return applyErr
}

func (s *Store) FinishMutation(id, revision string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	mutation, ok := s.facts.Mutations[id]
	if !ok {
		return ErrConflict
	}
	if mutation.State == "completed" {
		if mutation.OutputRevision != revision {
			return ErrConflict
		}
		return nil
	}
	if !validHash(revision) || s.authorizeMutation(mutation.Spec, now) != nil {
		return ErrAuthority
	}
	for _, state := range mutation.Files {
		if state != "applied" {
			return ErrNotQuiet
		}
	}
	mutation.State, mutation.OutputRevision = "completed", revision
	s.facts.Mutations[id] = mutation
	return s.saveControl()
}

// AbandonMutation records partial/unknown facts after the writer has closed all
// file handles. It neither rolls back user files nor makes a replay eligible.
func (s *Store) AbandonMutation(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	mutation, ok := s.facts.Mutations[id]
	if !ok {
		return ErrConflict
	}
	if mutation.State == "completed" || mutation.State == "unknown" {
		return nil
	}
	mutation.State = "unknown"
	for i, state := range mutation.Files {
		if state == "writing" {
			mutation.Files[i] = "unknown"
		}
	}
	s.facts.Mutations[id] = mutation
	return s.saveControl()
}

func (s *Store) Mutation(id string) (Mutation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Mutation{}, ErrClosed
	}
	mutation, ok := s.facts.Mutations[id]
	if !ok {
		return Mutation{}, ErrConflict
	}
	return cloneMutation(mutation), nil
}

func recoverMutations(facts *Facts) error {
	for id, mutation := range facts.Mutations {
		if id != mutation.Spec.ID || validateMutationSpec(mutation.Spec) != nil || mutation.Digest != mutationDigest(mutation.Spec) || len(mutation.Files) != len(mutation.Spec.Files) {
			return ErrProtocol
		}
		session, ok := facts.Sessions[mutation.Spec.SessionID]
		if !ok {
			return ErrProtocol
		}
		if _, ok := session.Executions[mutation.Spec.ExecutionID]; !ok {
			return ErrProtocol
		}
		if mutation.State != "prepared" && mutation.State != "applying" && mutation.State != "unknown" && mutation.State != "completed" {
			return ErrProtocol
		}
		for i, state := range mutation.Files {
			if state != "pending" && state != "writing" && state != "applied" && state != "unknown" {
				return ErrProtocol
			}
			if mutation.State == "completed" && (state != "applied" || !validHash(mutation.OutputRevision)) {
				return ErrProtocol
			}
			if state == "writing" {
				mutation.Files[i] = "unknown"
			}
		}
		if mutation.State == "applying" || mutation.State == "prepared" {
			mutation.State = "unknown"
		}
		facts.Mutations[id] = mutation
	}
	return nil
}
