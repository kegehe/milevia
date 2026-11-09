package targetexec

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const maxFactsBytes = 32 << 20
const maxAdmittedFactsBytes = maxFactsBytes - (8 << 20)

type Store struct {
	mu        sync.Mutex
	dir       string
	lock      *os.File
	facts     Facts
	closed    bool
	committed []byte
	// Zero uses the production payload limit. Kept separate from the hard
	// limit so revocation and quiescence can still be persisted at capacity.
	admissionLimit int
	grantDeadlines map[string]time.Time
	turnDeadlines  map[string]time.Time
	leaseDeadlines map[string]time.Time
}

// Open uses a process lifetime filesystem lock, not the presence of a lock file.
// Unknown versions and incomplete durable facts fail closed, never reset identity.
func Open(dir string) (*Store, error) {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absolute, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(absolute, "target.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(lock); err != nil {
		lock.Close()
		return nil, err
	}
	s := &Store{dir: absolute, lock: lock}
	f, err := os.Open(filepath.Join(absolute, "facts.json"))
	if errors.Is(err, os.ErrNotExist) {
		s.facts = Facts{Protocol: ProtocolVersion, TargetID: uuid.NewString(), Sessions: map[string]Session{}}
		if err := s.persist(); err != nil {
			s.Close()
			return nil, err
		}
		return s, nil
	}
	if err != nil {
		s.Close()
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxFactsBytes+1))
	f.Close()
	if err != nil || len(raw) > maxFactsBytes || json.Unmarshal(raw, &s.facts) != nil || s.facts.Protocol != ProtocolVersion || !validID(s.facts.TargetID) || s.facts.Sessions == nil {
		s.Close()
		return nil, ErrProtocol
	}
	for sessionID, session := range s.facts.Sessions {
		if session.Control.SessionID != sessionID || session.Control.TargetID != s.facts.TargetID || session.Control.Protocol != ProtocolVersion || session.Executions == nil {
			s.Close()
			return nil, ErrProtocol
		}
		for id, execution := range session.Executions {
			if execution.Spec.ExecutionID != id || execution.Spec.SessionID != sessionID || execution.Spec.TargetID != s.facts.TargetID || execution.Spec.Protocol != ProtocolVersion || execution.Digest != specDigest(execution.Spec) {
				s.Close()
				return nil, ErrProtocol
			}
			// A restart does not prove a running process or its output has ended.
			if execution.State == "starting" || execution.State == "held" || execution.State == "running" {
				execution.State = "unknown"
				session.Executions[id] = execution
			}
			if execution.State == "prepared" {
				execution.State, execution.Interrupted = "quiescent", true
				session.Executions[id] = execution
			}
			if execution.State != "prepared" && execution.State != "unknown" && execution.State != "quiescent" {
				s.Close()
				return nil, ErrProtocol
			}
		}
		if !session.Released && session.Control.State != "stopped" {
			session.RecoveryRequired = true
		}
		if err := recoverFinalization(&session); err != nil {
			s.Close()
			return nil, err
		}
		s.facts.Sessions[sessionID] = session
	}
	if err := validateEventLogs(s.facts); err != nil {
		s.Close()
		return nil, err
	}
	for id, workspace := range s.facts.Workspaces {
		if id == "" || workspace.ID != id || !filepath.IsAbs(workspace.Path) || filepath.Clean(workspace.Path) != workspace.Path {
			s.Close()
			return nil, ErrProtocol
		}
	}
	if err := recoverMutations(&s.facts); err != nil {
		s.Close()
		return nil, err
	}
	if err := recoverFinalizationJournal(&s.facts); err != nil {
		s.Close()
		return nil, err
	}
	if err := validateArtifactAllocations(s.facts); err != nil {
		s.Close()
		return nil, err
	}
	if err := s.persist(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// RegisterWorkspace is called only after the target filesystem driver resolves
// symlinks, canonical path and physical identity. Clients cannot invent an ID.
func (s *Store) RegisterWorkspace(workspace Workspace) error {
	if workspace.ID == "" || !filepath.IsAbs(workspace.Path) || filepath.Clean(workspace.Path) != workspace.Path {
		return ErrConflict
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if s.facts.Workspaces == nil {
		s.facts.Workspaces = map[string]Workspace{}
	}
	if prior, ok := s.facts.Workspaces[workspace.ID]; ok {
		if prior != workspace {
			return ErrConflict
		}
		return nil
	}
	s.facts.Workspaces[workspace.ID] = workspace
	return s.save()
}

func (s *Store) Workspace(id string) (Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Workspace{}, ErrClosed
	}
	workspace, ok := s.facts.Workspaces[id]
	if !ok {
		return Workspace{}, ErrConflict
	}
	return workspace, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.lock == nil {
		return nil
	}
	err := s.lock.Close()
	s.lock = nil
	return err
}

func (s *Store) persist() error {
	return s.persistLimit(maxFactsBytes)
}

func (s *Store) persistLimit(limit int) error {
	raw, err := json.Marshal(s.facts)
	if err != nil {
		return err
	}
	if len(raw) > limit {
		return ErrLimit
	}
	f, err := os.CreateTemp(s.dir, "facts-*.tmp")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err := replaceDurable(name, filepath.Join(s.dir, "facts.json")); err != nil {
		return err
	}
	s.committed = raw
	return nil
}

func (s *Store) save() error {
	limit := s.admissionLimit
	if limit <= 0 || limit > maxAdmittedFactsBytes {
		limit = maxAdmittedFactsBytes
	}
	return s.saveLimit(limit)
}

func (s *Store) saveControl() error { return s.saveLimit(maxFactsBytes) }

func (s *Store) saveLimit(limit int) error {
	if err := s.persistLimit(limit); err != nil {
		if errors.Is(err, ErrLimit) && len(s.committed) > 0 {
			// A refused admission must not leave an in-memory execution/event
			// that was never durable. Capacity exhaustion leaves the trusted
			// stop/query/release path open; real I/O faults still fail closed.
			var prior Facts
			if json.Unmarshal(s.committed, &prior) == nil {
				s.facts = prior
				return err
			}
		}
		s.closed = true
		return err
	}
	return nil
}

func (s *Store) Facts() (Facts, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Facts{}, ErrClosed
	}
	// Do not return shared maps to transport handlers or callers.
	raw, err := json.Marshal(s.facts)
	if err != nil {
		return Facts{}, err
	}
	var out Facts
	err = json.Unmarshal(raw, &out)
	return out, err
}

func (s *Store) Identity() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", ErrClosed
	}
	return s.facts.TargetID, nil
}

func (s *Store) Session(id string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Session{}, ErrClosed
	}
	session, ok := s.facts.Sessions[id]
	if !ok {
		return Session{}, ErrConflict
	}
	raw, err := json.Marshal(session)
	if err != nil {
		return Session{}, err
	}
	var out Session
	err = json.Unmarshal(raw, &out)
	return out, err
}

func (s *Store) CheckGrant(sessionID, workspaceID string, epoch, version int64, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	session, ok := s.facts.Sessions[sessionID]
	control := session.Control
	if !ok || session.Released || !session.Claimed || session.RecoveryRequired || session.Finalization != nil || control.State != "running" || control.WorkspaceID != workspaceID || control.OwnerEpoch != epoch || control.Version != version || !control.AuthorizedUntil.After(now) || !s.grantLive(sessionID) {
		return ErrAuthority
	}
	return nil
}

// Reconciliation may read the protected workspace without granting any new
// execution or write permission. It requires the exact current paused control,
// no drain exception and durable quiescence of every old execution. Recheck this
// at each snapshot/blob boundary, including after collecting the filesystem.
func (s *Store) CheckReconciliationRead(sessionID, workspaceID string, epoch, version int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	session, ok := s.facts.Sessions[sessionID]
	c := session.Control
	if !ok || session.Released || !session.Claimed || session.RecoveryRequired || c.State != "paused" || c.DrainExecutionID != "" || c.WorkspaceID != workspaceID || c.OwnerEpoch != epoch || c.Version != version {
		return ErrAuthority
	}
	for _, execution := range session.Executions {
		if execution.State != "quiescent" {
			return ErrNotQuiet
		}
	}
	for _, mutation := range s.facts.Mutations {
		if s.workspacesOverlap(mutation.Spec.WorkspaceID, workspaceID) && (mutation.State == "prepared" || mutation.State == "applying") {
			return ErrNotQuiet
		}
	}
	return nil
}

// Wire timestamps bind durable facts. Live grants also need a local monotonic
// bound: a backwards wall-clock adjustment must not turn 30 seconds into hours.
// These bounds never survive process restart; recovery requires new authority.
func localDeadline(until, observed time.Time) time.Time {
	now := time.Now()
	remaining := min(until.Sub(observed), until.Sub(now))
	return now.Add(remaining)
}

func (s *Store) grantLive(id string) bool {
	deadline, ok := s.grantDeadlines[id]
	return ok && time.Now().Before(deadline)
}

func (s *Store) setGrantDeadline(id string, until time.Time) {
	if s.grantDeadlines == nil {
		s.grantDeadlines = map[string]time.Time{}
	}
	s.grantDeadlines[id] = until
}

func (s *Store) turnDeadline(sessionID, executionID string) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return time.Time{}, ErrClosed
	}
	session, ok := s.facts.Sessions[sessionID]
	if _, exists := session.Executions[executionID]; !ok || !exists {
		return time.Time{}, ErrConflict
	}
	deadline, exists := s.turnDeadlines[executionID]
	if !exists {
		return time.Time{}, ErrAuthority
	}
	return deadline, nil
}

// ApplyControl persists a revoke even when no execution is known. Delayed
// prepare/start/renew messages therefore cannot resurrect an old authorization.
func (s *Store) ApplyControl(control Control, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if control.Protocol != ProtocolVersion {
		return ErrProtocol
	}
	if control.TargetID != s.facts.TargetID || !validID(control.SessionID) || control.OwnerEpoch <= 0 || control.Version <= 0 || control.WorkspaceID == "" {
		return ErrAuthority
	}
	if control.State != "running" && control.State != "paused" && control.State != "stopped" {
		return ErrAuthority
	}
	if (control.State == "running" || control.DrainExecutionID != "") && (!control.AuthorizedUntil.After(now) || control.AuthorizedUntil.After(now.Add(30*time.Second))) {
		return ErrAuthority
	}
	current, exists := s.facts.Sessions[control.SessionID]
	if exists {
		prior := current.Control
		if prior.Protocol == control.Protocol && prior.TargetID == control.TargetID && prior.SessionID == control.SessionID && prior.OwnerEpoch == control.OwnerEpoch && prior.Version == control.Version && prior.State == control.State && prior.WorkspaceID == control.WorkspaceID && prior.GuardID == control.GuardID && prior.DrainExecutionID == control.DrainExecutionID && !control.AuthorizedUntil.After(prior.AuthorizedUntil) {
			return nil
		}
		if current.Released || current.Control.State == "stopped" || control.OwnerEpoch < current.Control.OwnerEpoch || control.Version <= current.Control.Version || current.Control.WorkspaceID != control.WorkspaceID || current.Control.GuardID != control.GuardID {
			return ErrAuthority
		}
	} else {
		current.Executions = map[string]Execution{}
	}
	if control.DrainExecutionID != "" {
		execution, ok := current.Executions[control.DrainExecutionID]
		if control.State != "paused" || !exists || control.OwnerEpoch != current.Control.OwnerEpoch || current.RecoveryRequired || !ok || (execution.State != "starting" && execution.State != "held" && execution.State != "running") {
			return ErrAuthority
		}
	}
	if control.State == "running" {
		for id, other := range s.facts.Sessions {
			if id != control.SessionID && other.Claimed && !other.Released && s.workspacesOverlap(other.Control.WorkspaceID, control.WorkspaceID) {
				return ErrNotQuiet
			}
		}
		current.Claimed = true
	}
	for id, execution := range current.Executions {
		if execution.State == "prepared" {
			execution.State = "quiescent"
			execution.Interrupted = true
			current.Executions[id] = execution
		}
	}
	current.Control = control
	current.RecoveryRequired = false
	s.facts.Sessions[control.SessionID] = current
	deadline := localDeadline(control.AuthorizedUntil, now)
	var err error
	if !exists {
		err = s.save()
	} else {
		err = s.saveControl()
	}
	if err == nil {
		s.setGrantDeadline(control.SessionID, deadline)
	}
	return err
}

func (s *Store) authorize(session Session, spec ExecutionSpec, now time.Time) error {
	c := session.Control
	draining := c.State == "paused" && c.DrainExecutionID == spec.ExecutionID
	if spec.TargetID != s.facts.TargetID || spec.SessionID != c.SessionID || !session.Claimed || session.Released || session.RecoveryRequired || session.Finalization != nil || (c.State != "running" && !draining) || spec.OwnerEpoch != c.OwnerEpoch || (!draining && spec.AuthorizationVersion != c.Version) || spec.WorkspaceID != c.WorkspaceID || !c.AuthorizedUntil.After(now) || !spec.TurnDeadline.After(now) || !s.grantLive(c.SessionID) {
		return ErrAuthority
	}
	if deadline, exists := s.turnDeadlines[spec.ExecutionID]; exists && !time.Now().Before(deadline) {
		return ErrAuthority
	}
	return nil
}

// Prepare is durable and idempotent by execution identity plus immutable digest.
// The returned state is authoritative; repeating it never launches a process.
func (s *Store) Prepare(spec ExecutionSpec, now time.Time) (Execution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Execution{}, ErrClosed
	}
	if spec.Protocol != ProtocolVersion {
		return Execution{}, ErrProtocol
	}
	if !validID(spec.ExecutionID) || !validID(spec.SessionID) || !validID(spec.WorkItemID) || spec.Attempt <= 0 || spec.PreparedAt.IsZero() || spec.PreparedAt.After(now) || !spec.TurnDeadline.After(spec.PreparedAt) || spec.TurnDeadline.Sub(spec.PreparedAt) > 30*time.Minute || !validHash(spec.InputSHA256) || !validHash(spec.InstructionsSHA256) || spec.Contract == "" || spec.AgentID == "" {
		return Execution{}, ErrAuthority
	}
	switch spec.Role {
	case "scheduler", "implementation", "review", "test":
	default:
		return Execution{}, ErrAuthority
	}
	session, exists := s.facts.Sessions[spec.SessionID]
	if !exists {
		return Execution{}, ErrAuthority
	}
	if prior, ok := session.Executions[spec.ExecutionID]; ok {
		if prior.Digest != specDigest(spec) {
			return Execution{}, ErrConflict
		}
		return prior, nil
	}
	if err := s.authorize(session, spec, now); err != nil {
		return Execution{}, err
	}
	if session.Control.State != "running" || spec.AuthorizationVersion != session.Control.Version {
		return Execution{}, ErrAuthority
	}
	// Work item attempts and directory aliases share the same target protection.
	for _, other := range s.facts.Sessions {
		for _, execution := range other.Executions {
			if execution.Spec.ExecutionID == spec.ExecutionID {
				return Execution{}, ErrConflict
			}
			if execution.State != "quiescent" && (execution.Spec.WorkItemID == spec.WorkItemID || s.workspacesOverlap(execution.Spec.WorkspaceID, spec.WorkspaceID)) {
				return Execution{}, ErrNotQuiet
			}
		}
	}
	for _, mutation := range s.facts.Mutations {
		if s.workspacesOverlap(mutation.Spec.WorkspaceID, spec.WorkspaceID) && (mutation.State == "prepared" || mutation.State == "applying") {
			return Execution{}, ErrNotQuiet
		}
	}
	execution := Execution{Spec: spec, Digest: specDigest(spec), Container: containerName(uuid.NewString()), State: "prepared", LeaseUntil: minDeadline(session.Control.AuthorizedUntil, spec.TurnDeadline)}
	session.Executions[spec.ExecutionID] = execution
	s.facts.Sessions[spec.SessionID] = session
	if err := s.save(); err != nil {
		return Execution{}, err
	}
	if s.turnDeadlines == nil {
		s.turnDeadlines, s.leaseDeadlines = map[string]time.Time{}, map[string]time.Time{}
	}
	s.turnDeadlines[spec.ExecutionID] = localDeadline(spec.TurnDeadline, now)
	s.leaseDeadlines[spec.ExecutionID] = localDeadline(execution.LeaseUntil, now)
	return execution, nil
}

func (s *Store) workspacesOverlap(leftID, rightID string) bool {
	if leftID == rightID {
		return true
	}
	left, leftOK := s.facts.Workspaces[leftID]
	right, rightOK := s.facts.Workspaces[rightID]
	if !leftOK || !rightOK {
		return false
	}
	within := func(root, name string) bool {
		relative, err := filepath.Rel(root, name)
		return err == nil && (relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
	}
	return within(left.Path, right.Path) || within(right.Path, left.Path)
}

// ClaimStart commits before the process driver creates a held container.
// A lost start response is queried, never retried as another process start.
func (s *Store) ClaimStart(sessionID, executionID, digest string, now time.Time) (Execution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Execution{}, ErrClosed
	}
	session := s.facts.Sessions[sessionID]
	execution, exists := session.Executions[executionID]
	if !exists || execution.Digest != digest {
		return Execution{}, ErrConflict
	}
	if err := s.authorize(session, execution.Spec, now); err != nil {
		return Execution{}, err
	}
	if execution.State != "prepared" {
		return execution, ErrConflict
	}
	if !execution.LeaseUntil.After(now) || !time.Now().Before(s.leaseDeadlines[executionID]) {
		return Execution{}, ErrAuthority
	}
	execution.State = "starting"
	session.Executions[executionID] = execution
	s.facts.Sessions[sessionID] = session
	return execution, s.saveControl()
}

// RegisterHeld persists a concrete process identity before any CLI instruction.
// Resume authorization must be checked again under the driver/control gate.
func (s *Store) RegisterHeld(sessionID, executionID string, identity ProcessIdentity, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	session := s.facts.Sessions[sessionID]
	execution, exists := session.Executions[executionID]
	if !exists || execution.State != "starting" || identity.Container != execution.Container || identity.PID <= 0 || identity.Birth == "" || identity.Executable == "" {
		return ErrConflict
	}
	authorityErr := s.authorize(session, execution.Spec, now)
	execution.Process = identity
	execution.State = "held"
	session.Executions[executionID] = execution
	s.facts.Sessions[sessionID] = session
	return errors.Join(authorityErr, s.saveControl())
}

// Resume shares the same local mutex as revoke. The callback may only perform
// the bounded local resume primitive; it must not wait for output or the network.
func (s *Store) Resume(sessionID, executionID string, now time.Time, resume func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	session := s.facts.Sessions[sessionID]
	execution, exists := session.Executions[executionID]
	if !exists || execution.State != "held" || resume == nil {
		return ErrConflict
	}
	if err := s.authorize(session, execution.Spec, now); err != nil {
		return err
	}
	if !execution.LeaseUntil.After(now) || !time.Now().Before(s.leaseDeadlines[executionID]) {
		return ErrAuthority
	}
	execution.State = "running"
	session.Executions[executionID] = execution
	s.facts.Sessions[sessionID] = session
	if err := s.saveControl(); err != nil {
		return err
	}
	return resume()
}

// Renew preserves the original turn deadline and cumulative usage. A control
// message with a newer authorization version is required to change permissions.
func (s *Store) Renew(sessionID, executionID string, epoch, version int64, until, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	session := s.facts.Sessions[sessionID]
	execution, exists := session.Executions[executionID]
	if !exists || execution.State != "running" || epoch != session.Control.OwnerEpoch || version != session.Control.Version || epoch != execution.Spec.OwnerEpoch || !execution.LeaseUntil.After(now) || !time.Now().Before(s.leaseDeadlines[executionID]) {
		return ErrAuthority
	}
	if err := s.authorize(session, execution.Spec, now); err != nil {
		return err
	}
	if !until.After(now) || until.After(now.Add(30*time.Second)) || until.After(session.Control.AuthorizedUntil) {
		return ErrAuthority
	}
	if !until.After(execution.LeaseUntil) {
		return nil
	}
	execution.LeaseUntil = minDeadline(until, execution.Spec.TurnDeadline)
	session.Executions[executionID] = execution
	s.facts.Sessions[sessionID] = session
	deadline := minDeadline(localDeadline(execution.LeaseUntil, now), s.turnDeadlines[executionID])
	if err := s.saveControl(); err != nil {
		return err
	}
	s.leaseDeadlines[executionID] = deadline
	return nil
}

// Lease renewal changes no permissions and cannot restore an expired or revoked
// grant. The transport must authenticate the owning controller before calling.
func (s *Store) RenewAuthorization(sessionID string, epoch, version int64, until, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	session, exists := s.facts.Sessions[sessionID]
	c := session.Control
	if !exists || !session.Claimed || session.Released || session.RecoveryRequired || (c.State != "running" && !(c.State == "paused" && c.DrainExecutionID != "")) || epoch != c.OwnerEpoch || version != c.Version || !c.AuthorizedUntil.After(now) || !until.After(now) || until.After(now.Add(30*time.Second)) || !s.grantLive(sessionID) {
		return ErrAuthority
	}
	if !until.After(c.AuthorizedUntil) {
		return nil
	}
	session.Control.AuthorizedUntil = until
	s.facts.Sessions[sessionID] = session
	deadline := localDeadline(until, now)
	if err := s.saveControl(); err != nil {
		return err
	}
	s.setGrantDeadline(sessionID, deadline)
	return nil
}

// RecordQuiet is called only by the trusted process/output/resource driver. A
// CLI success event and the disappearance of its parent PID are insufficient.
func (s *Store) RecordQuiet(sessionID, executionID, digest, resultHash string, interrupted bool, usage Usage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	session := s.facts.Sessions[sessionID]
	execution, exists := session.Executions[executionID]
	if !exists || execution.Digest != digest || resultHash != "" && !validHash(resultHash) {
		return ErrConflict
	}
	if execution.State == "quiescent" {
		if execution.ResultSHA256 != resultHash || execution.Interrupted != interrupted || execution.Usage != usage {
			return ErrConflict
		}
		return nil
	}
	if usage.Sequence < execution.Usage.Sequence || usage.InputTokens < execution.Usage.InputTokens || usage.OutputTokens < execution.Usage.OutputTokens || usage.CacheReadTokens < execution.Usage.CacheReadTokens || usage.CacheCreationTokens < execution.Usage.CacheCreationTokens || usage.ActiveMillis < execution.Usage.ActiveMillis || (usage.Sequence == execution.Usage.Sequence && usage != execution.Usage) {
		return ErrConflict
	}
	execution.State = "quiescent"
	execution.ResultSHA256 = resultHash
	execution.Interrupted = interrupted
	execution.Usage = usage
	session.Executions[executionID] = execution
	s.facts.Sessions[sessionID] = session
	return s.saveControl()
}

func (s *Store) DueStops(now time.Time) ([]Execution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	var stops []Execution
	for _, session := range s.facts.Sessions {
		for _, execution := range session.Executions {
			if execution.State == "quiescent" || execution.State == "prepared" {
				continue
			}
			if execution.State == "unknown" || s.authorize(session, execution.Spec, now) != nil || !execution.LeaseUntil.After(now) || !time.Now().Before(s.leaseDeadlines[execution.Spec.ExecutionID]) {
				stops = append(stops, execution)
			}
		}
	}
	return stops, nil
}

func (s *Store) Release(sessionID string, epoch, version int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	session, exists := s.facts.Sessions[sessionID]
	if !exists || session.Control.OwnerEpoch != epoch || session.Control.Version != version || session.Control.State != "stopped" {
		return ErrAuthority
	}
	if session.Released {
		return nil
	}
	for _, execution := range session.Executions {
		if execution.State != "quiescent" {
			return ErrNotQuiet
		}
	}
	for _, mutation := range s.facts.Mutations {
		if mutation.Spec.SessionID == sessionID && (mutation.State == "prepared" || mutation.State == "applying") {
			return ErrNotQuiet
		}
	}
	session.Released = true
	s.facts.Sessions[sessionID] = session
	return s.saveControl()
}

func minDeadline(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (s *Store) Disconnect() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	for id, session := range s.facts.Sessions {
		if session.Released {
			continue
		}
		session.RecoveryRequired = true
		for executionID, execution := range session.Executions {
			if execution.State == "prepared" {
				execution.State = "quiescent"
				execution.Interrupted = true
				session.Executions[executionID] = execution
			}
		}
		s.facts.Sessions[id] = session
	}
	for id, mutation := range s.facts.Mutations {
		if mutation.State == "prepared" || mutation.State == "applying" {
			mutation.State = "unknown"
			for i, state := range mutation.Files {
				if state == "writing" {
					mutation.Files[i] = "unknown"
				}
			}
			s.facts.Mutations[id] = mutation
		}
	}
	return s.saveControl()
}
