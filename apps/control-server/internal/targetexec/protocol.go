// Package targetexec owns durable target-side authority and execution facts.
// It does not decide plans, acceptance criteria or Task.done.
package targetexec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

const ProtocolVersion = 1

var (
	ErrProtocol  = errors.New("unsupported target execution protocol")
	ErrAuthority = errors.New("target execution authorization is stale or revoked")
	ErrConflict  = errors.New("target execution identity conflicts with durable facts")
	ErrNotQuiet  = errors.New("target resources are not confirmed quiescent")
	ErrClosed    = errors.New("target execution store is closed or requires recovery")
	ErrCursor    = errors.New("target execution event cursor is unavailable")
	ErrLimit     = errors.New("target execution payload exceeds protocol limit")
)

type Control struct {
	Protocol         int       `json:"protocol"`
	TargetID         string    `json:"targetId"`
	SessionID        string    `json:"sessionId"`
	OwnerEpoch       int64     `json:"ownerEpoch"`
	Version          int64     `json:"version"`
	State            string    `json:"state"` // running, paused, stopped
	DrainExecutionID string    `json:"drainExecutionId,omitempty"`
	WorkspaceID      string    `json:"workspaceId"` // target filesystem identity, never a controller path
	GuardID          string    `json:"guardId,omitempty"`
	AuthorizedUntil  time.Time `json:"authorizedUntil"`
}

type ExecutionSpec struct {
	Protocol             int       `json:"protocol"`
	TargetID             string    `json:"targetId"`
	SessionID            string    `json:"sessionId"`
	ExecutionID          string    `json:"executionId"`
	WorkItemID           string    `json:"workItemId"`
	Attempt              int       `json:"attempt"`
	OwnerEpoch           int64     `json:"ownerEpoch"`
	AuthorizationVersion int64     `json:"authorizationVersion"`
	WorkspaceID          string    `json:"workspaceId"`
	Role                 string    `json:"role"`
	AgentID              string    `json:"agentId"`
	ProfileRevisionID    string    `json:"profileRevisionId,omitempty"`
	ProfileSHA256        string    `json:"profileSha256,omitempty"`
	Contract             string    `json:"contract"`
	CLIPath              string    `json:"cliPath,omitempty"`
	InstallationPath     string    `json:"installationPath,omitempty"`
	CLIVersion           string    `json:"cliVersion,omitempty"`
	EntrypointSHA256     string    `json:"entrypointSha256,omitempty"`
	RuntimeVersion       string    `json:"runtimeVersion,omitempty"`
	ModelSettingsSHA256  string    `json:"modelSettingsSha256,omitempty"`
	Model                string    `json:"model,omitempty"`
	ReasoningEffort      string    `json:"reasoningEffort,omitempty"`
	InputSHA256          string    `json:"inputSha256"`
	InstructionsSHA256   string    `json:"instructionsSha256"`
	PreparedAt           time.Time `json:"preparedAt"`
	TurnDeadline         time.Time `json:"turnDeadline"`
}

type Execution struct {
	Spec         ExecutionSpec   `json:"spec"`
	Digest       string          `json:"digest"`
	Container    string          `json:"container"`
	State        string          `json:"state"` // prepared, starting, held, running, unknown, quiescent
	LeaseUntil   time.Time       `json:"leaseUntil"`
	Process      ProcessIdentity `json:"process"`
	Usage        Usage           `json:"usage"`
	ResultSHA256 string          `json:"resultSha256,omitempty"`
	Interrupted  bool            `json:"interrupted"`
}

type ProcessIdentity struct {
	Container  string `json:"container"`
	PID        int    `json:"pid"`
	Birth      string `json:"birth"`
	Executable string `json:"executable"`
}

type Usage struct {
	Sequence            int64 `json:"sequence"`
	InputTokens         int64 `json:"inputTokens"`
	OutputTokens        int64 `json:"outputTokens"`
	CacheReadTokens     int64 `json:"cacheReadTokens"`
	CacheCreationTokens int64 `json:"cacheCreationTokens"`
	ActiveMillis        int64 `json:"activeMillis"`
}

type Session struct {
	Control          Control              `json:"control"`
	Executions       map[string]Execution `json:"executions"`
	Claimed          bool                 `json:"claimed"`
	Released         bool                 `json:"released"`
	RecoveryRequired bool                 `json:"recoveryRequired"`
	Finalization     *Finalization        `json:"finalization,omitempty"`
}

type Facts struct {
	Protocol      int                           `json:"protocol"`
	TargetID      string                        `json:"targetId"`
	Sessions      map[string]Session            `json:"sessions"`
	Events        map[string]EventLog           `json:"events,omitempty"`
	Workspaces    map[string]Workspace          `json:"workspaces,omitempty"`
	Mutations     map[string]Mutation           `json:"mutations,omitempty"`
	Finalizations map[string]Finalization       `json:"finalizations,omitempty"`
	Artifacts     map[string]ArtifactAllocation `json:"artifacts,omitempty"`
}

type Workspace struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

// Events are bounded target facts. Payloads must already be redacted by the CLI
// adapter; credentials and raw process environments never enter this journal.
type Event struct {
	Sequence    int64           `json:"sequence"`
	ExecutionID string          `json:"executionId"`
	Key         string          `json:"key"`
	Type        string          `json:"type"`
	Payload     json.RawMessage `json:"payload"`
	SHA256      string          `json:"sha256"`
}

type EventLog struct {
	NextSequence  int64             `json:"nextSequence"`
	FirstSequence int64             `json:"firstSequence"`
	Events        []Event           `json:"events"`
	Keys          map[string]string `json:"keys"`
	Bytes         int64             `json:"bytes"`
}

type EventPage struct {
	Events        []Event `json:"events"`
	FirstSequence int64   `json:"firstSequence"`
	NextSequence  int64   `json:"nextSequence"`
	More          bool    `json:"more"`
}

func specDigest(spec ExecutionSpec) string {
	raw, _ := json.Marshal(spec)
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func validID(value string) bool {
	if len(value) == 0 || len(value) > 128 || value == "." || value == ".." {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func validHash(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	raw, err := hex.DecodeString(value)
	return err == nil && len(raw) == sha256.Size
}
