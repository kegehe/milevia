package targetexec

import (
	"encoding/json"
	"time"
)

type FinalizationSpec struct {
	ID                   string `json:"id"`
	SessionID            string `json:"sessionId"`
	WorkspaceID          string `json:"workspaceId"`
	OwnerEpoch           int64  `json:"ownerEpoch"`
	AuthorizationVersion int64  `json:"authorizationVersion"`
	// Controller binding includes accepted task subset, requirements/plans,
	// dependency facts and evidence watermarks. The target never decides done.
	BindingSHA256   string `json:"bindingSha256"`
	RevisionID      string `json:"revisionId"`
	RulesHash       string `json:"rulesHash"`
	EnvironmentHash string `json:"environmentHash"`
}

type Finalization struct {
	Spec       FinalizationSpec `json:"spec"`
	Digest     string           `json:"digest"`
	State      string           `json:"state"` // collecting, sealed, unknown
	ObservedAt time.Time        `json:"observedAt"`
}

func finalizationDigest(spec FinalizationSpec) string {
	raw, _ := json.Marshal(spec)
	return hashBytes(raw)
}
func validFinalization(spec FinalizationSpec) bool {
	return validID(spec.ID) && validID(spec.SessionID) && spec.WorkspaceID != "" && spec.OwnerEpoch > 0 && spec.AuthorizationVersion > 0 && validHash(spec.BindingSHA256) && validHash(spec.RevisionID) && validHash(spec.RulesHash) && validHash(spec.EnvironmentHash)
}

func (s *Store) BeginFinalization(spec FinalizationSpec, now time.Time) (Finalization, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Finalization{}, false, ErrClosed
	}
	if !validFinalization(spec) {
		return Finalization{}, false, ErrConflict
	}
	if prior, exists := s.facts.Finalizations[spec.ID]; exists {
		if prior.Digest != finalizationDigest(spec) {
			return Finalization{}, false, ErrConflict
		}
		return prior, false, nil
	}
	session, ok := s.facts.Sessions[spec.SessionID]
	control := session.Control
	if !ok {
		return Finalization{}, false, ErrAuthority
	}
	if prior := session.Finalization; prior != nil {
		if prior.Spec.ID == spec.ID {
			if prior.Digest != finalizationDigest(spec) {
				return Finalization{}, false, ErrConflict
			}
			return *prior, false, nil
		}
		return Finalization{}, false, ErrConflict
	}
	if session.Released || session.RecoveryRequired || !session.Claimed || control.State != "running" || control.Version != spec.AuthorizationVersion || control.OwnerEpoch != spec.OwnerEpoch || control.WorkspaceID != spec.WorkspaceID || !control.AuthorizedUntil.After(now) || !s.grantLive(spec.SessionID) {
		return Finalization{}, false, ErrAuthority
	}
	for _, execution := range session.Executions {
		if execution.State != "quiescent" {
			return Finalization{}, false, ErrNotQuiet
		}
	}
	for _, mutation := range s.facts.Mutations {
		if mutation.Spec.SessionID == spec.SessionID && (mutation.State == "prepared" || mutation.State == "applying") {
			return Finalization{}, false, ErrNotQuiet
		}
	}
	finalization := Finalization{Spec: spec, Digest: finalizationDigest(spec), State: "collecting"}
	session.Finalization = &finalization
	s.facts.Sessions[spec.SessionID] = session
	if s.facts.Finalizations == nil {
		s.facts.Finalizations = map[string]Finalization{}
	}
	s.facts.Finalizations[spec.ID] = finalization
	if err := s.save(); err != nil {
		return Finalization{}, false, err
	}
	return finalization, true, nil
}

func (s *Store) SealFinalization(spec FinalizationSpec, observedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	session := s.facts.Sessions[spec.SessionID]
	prior := session.Finalization
	control := session.Control
	if prior == nil || prior.Digest != finalizationDigest(spec) {
		return ErrConflict
	}
	if prior.State == "sealed" {
		return nil
	}
	if prior.State != "collecting" || session.Released || session.RecoveryRequired || control.State != "running" || control.Version != spec.AuthorizationVersion || control.OwnerEpoch != spec.OwnerEpoch || !control.AuthorizedUntil.After(observedAt) || !s.grantLive(spec.SessionID) {
		return ErrAuthority
	}
	prior.State, prior.ObservedAt = "sealed", observedAt
	s.facts.Sessions[spec.SessionID] = session
	s.facts.Finalizations[spec.ID] = *prior
	return s.saveControl()
}

// Only an explicit newer running authorization may abandon a stale receipt.
// Reconnect/lease renewal alone cannot reopen writes after finalization.
func (s *Store) ClearFinalization(sessionID, id string, epoch, version int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	session, ok := s.facts.Sessions[sessionID]
	prior := session.Finalization
	if !ok || session.Control.State != "running" || session.Released || session.RecoveryRequired || session.Control.OwnerEpoch != epoch || session.Control.Version != version || !s.grantLive(sessionID) {
		return ErrAuthority
	}
	if prior == nil {
		// No active handoff plus the original durable journal proves a prior
		// clear. A lost ACK can be retried without reopening or erasing facts.
		journal, exists := s.facts.Finalizations[id]
		if !exists || journal.Spec.SessionID != sessionID || journal.Spec.WorkspaceID != session.Control.WorkspaceID || version <= journal.Spec.AuthorizationVersion {
			return ErrAuthority
		}
		return nil
	}
	if prior.Spec.ID != id || version <= prior.Spec.AuthorizationVersion {
		return ErrAuthority
	}
	if prior.State == "collecting" {
		prior.State = "unknown"
		s.facts.Finalizations[prior.Spec.ID] = *prior
	}
	session.Finalization = nil
	s.facts.Sessions[sessionID] = session
	return s.saveControl()
}

func (s *Store) Finalization(id string) (Finalization, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Finalization{}, ErrClosed
	}
	prior, exists := s.facts.Finalizations[id]
	if !exists {
		return Finalization{}, ErrConflict
	}
	return prior, nil
}

func recoverFinalizationJournal(facts *Facts) error {
	if facts.Finalizations == nil {
		facts.Finalizations = map[string]Finalization{}
	}
	for id, prior := range facts.Finalizations {
		session, exists := facts.Sessions[prior.Spec.SessionID]
		if !exists || id != prior.Spec.ID || !validFinalization(prior.Spec) || prior.Digest != finalizationDigest(prior.Spec) || prior.Spec.WorkspaceID != session.Control.WorkspaceID {
			return ErrProtocol
		}
		if prior.State != "collecting" && prior.State != "sealed" && prior.State != "unknown" {
			return ErrProtocol
		}
		if prior.State == "sealed" && prior.ObservedAt.IsZero() {
			return ErrProtocol
		}
		if prior.State == "collecting" {
			prior.State = "unknown"
			facts.Finalizations[id] = prior
		}
	}
	for _, session := range facts.Sessions {
		if session.Finalization == nil {
			continue
		}
		prior := *session.Finalization
		if journal, exists := facts.Finalizations[prior.Spec.ID]; exists {
			if journal != prior {
				return ErrProtocol
			}
		} else {
			facts.Finalizations[prior.Spec.ID] = prior
		}
	}
	return nil
}

func recoverFinalization(session *Session) error {
	prior := session.Finalization
	if prior == nil {
		return nil
	}
	if !validFinalization(prior.Spec) || prior.Spec.SessionID != session.Control.SessionID || prior.Spec.WorkspaceID != session.Control.WorkspaceID || prior.Digest != finalizationDigest(prior.Spec) {
		return ErrProtocol
	}
	if prior.State != "collecting" && prior.State != "sealed" && prior.State != "unknown" {
		return ErrProtocol
	}
	if prior.State == "sealed" && prior.ObservedAt.IsZero() {
		return ErrProtocol
	}
	if prior.State == "collecting" {
		prior.State = "unknown"
	}
	return nil
}
