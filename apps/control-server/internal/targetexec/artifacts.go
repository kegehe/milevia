package targetexec

import "time"

// Allocation survives target crashes. Unknown/unfinished output keeps its full
// reservation until an explicit, quiescent cleanup; restart never frees space
// merely because the writing process disappeared.
type ArtifactAllocation struct {
	SessionID   string `json:"sessionId"`
	ExecutionID string `json:"executionId"`
	Bytes       int64  `json:"bytes"`
	Limit       int64  `json:"limit"`
	State       string `json:"state"` // reserved, settled, cleaning, cleared
}

const MaxArtifactStorageBytes int64 = 2 << 30

func (s *Store) ReserveArtifacts(sessionID, executionID string, bytes int64, now time.Time) error {
	return s.ReserveArtifactsWithAvailable(sessionID, executionID, bytes, MaxArtifactStorageBytes, now)
}

// available excludes the caller's filesystem control reserve. Count all
// unfinished allocations under the same admission mutex so concurrent workspaces
// cannot each spend the same free-space observation.
func (s *Store) ReserveArtifactsWithAvailable(sessionID, executionID string, bytes, available int64, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	session, ok := s.facts.Sessions[sessionID]
	execution, exists := session.Executions[executionID]
	if !ok || !exists || execution.Spec.Role != "test" || execution.State != "starting" && execution.State != "held" && execution.State != "running" {
		return ErrAuthority
	}
	if err := s.authorize(session, execution.Spec, now); err != nil {
		return err
	}
	if bytes <= 0 || bytes > MaxArtifactStorageBytes {
		return ErrLimit
	}
	if prior, exists := s.facts.Artifacts[executionID]; exists {
		if prior.SessionID == sessionID && prior.Limit == bytes && prior.State == "reserved" {
			return nil
		}
		return ErrConflict
	}
	var total, pending int64
	for _, allocation := range s.facts.Artifacts {
		total += allocation.Bytes
		if allocation.State == "reserved" {
			pending += allocation.Bytes
		}
	}
	if total > MaxArtifactStorageBytes-bytes || available < bytes || pending > available-bytes {
		return ErrLimit
	}
	if s.facts.Artifacts == nil {
		s.facts.Artifacts = map[string]ArtifactAllocation{}
	}
	s.facts.Artifacts[executionID] = ArtifactAllocation{SessionID: sessionID, ExecutionID: executionID, Bytes: bytes, Limit: bytes, State: "reserved"}
	return s.save()
}

// Only the trusted output driver settles actual size after proving containment
// quiet. A failed size check leaves the full reservation and fails acceptance.
func (s *Store) SettleArtifacts(sessionID, executionID string, bytes int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	prior, exists := s.facts.Artifacts[executionID]
	if !exists || prior.SessionID != sessionID || bytes < 0 || bytes > prior.Limit {
		return ErrConflict
	}
	if prior.State == "settled" && prior.Bytes == bytes {
		return nil
	}
	if prior.State != "reserved" {
		return ErrConflict
	}
	prior.State, prior.Bytes = "settled", bytes
	s.facts.Artifacts[executionID] = prior
	return s.saveControl()
}

// Cleanup is two phase: a lost response or crash cannot make bytes reusable
// before the target filesystem driver actually deletes them. No running session
// may lose evidence; cleanup is explicit and limited to released sessions.
func (s *Store) BeginArtifactCleanup(sessionID, executionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	session, ok := s.facts.Sessions[sessionID]
	execution, exists := session.Executions[executionID]
	prior, allocated := s.facts.Artifacts[executionID]
	if !ok || !exists || !allocated || prior.SessionID != sessionID || !session.Released || execution.State != "quiescent" {
		return ErrNotQuiet
	}
	if prior.State == "cleaning" || prior.State == "cleared" {
		return nil
	}
	prior.State = "cleaning"
	s.facts.Artifacts[executionID] = prior
	return s.saveControl()
}

func (s *Store) FinishArtifactCleanup(sessionID, executionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	prior, ok := s.facts.Artifacts[executionID]
	if !ok || prior.SessionID != sessionID || prior.State != "cleaning" && prior.State != "cleared" {
		return ErrConflict
	}
	prior.State, prior.Bytes = "cleared", 0
	s.facts.Artifacts[executionID] = prior
	return s.saveControl()
}

func validateArtifactAllocations(facts Facts) error {
	var total int64
	for id, allocation := range facts.Artifacts {
		session, ok := facts.Sessions[allocation.SessionID]
		execution, exists := session.Executions[id]
		if !ok || !exists || execution.Spec.Role != "test" || allocation.ExecutionID != id || allocation.Limit <= 0 || allocation.Limit > MaxArtifactStorageBytes || allocation.Bytes < 0 || allocation.Bytes > allocation.Limit {
			return ErrProtocol
		}
		switch allocation.State {
		case "reserved", "settled":
		case "cleaning":
			if !session.Released || execution.State != "quiescent" {
				return ErrProtocol
			}
		case "cleared":
			if !session.Released || allocation.Bytes != 0 || execution.State != "quiescent" {
				return ErrProtocol
			}
		default:
			return ErrProtocol
		}
		total += allocation.Bytes
		if total > MaxArtifactStorageBytes {
			return ErrProtocol
		}
	}
	return nil
}
