package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

var errSupervisionTargetResolved = errors.New("target execution configuration resolved before launch")

// Resolve through the same native launch preparation used by the real CLI.
// In particular Codex/OpenCode may select a default model from target-side
// configuration. Returning from Resolved aborts before any model process starts.
// No credentials/configuration body is returned, copied or persisted.
func resolveSupervisionTargetCLI(ctx context.Context, adapter supervisionCLIAdapter, spec targetexec.ExecutionSpec, input targetexec.Input, profiles ...*AgentRuntimeProfile) (supervisionCLIInfo, error) {
	var info supervisionCLIInfo
	var profile *AgentRuntimeProfile
	if len(profiles) == 1 {
		profile = profiles[0]
	}
	if len(profiles) > 1 {
		return info, targetexec.ErrProtocol
	}
	if err := validateSupervisionTargetCLIInput(spec, input); err != nil {
		return info, err
	}
	if adapter == nil {
		return info, targetexec.ErrProtocol
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	sink := &supervisionTargetResolutionSink{}
	resolved, handshake := 0, false
	receipt, err := adapter.runSupervisionScheduler(ctx, supervisionSchedulerCLIRequest{
		Input: input.Observation, Profile: profile, SystemPrompt: input.Instructions, Model: spec.Model, ReasoningEffort: spec.ReasoningEffort,
		Container: supervisionJobPrefix + uuid.NewString(),
		Resolved: func(actual supervisionCLIInfo) error {
			resolved++
			info = actual
			return errSupervisionTargetResolved
		},
		Handshake: func(_ supervisionProcessIdentity, _ func() error) error {
			handshake = true
			return targetexec.ErrAuthority // Never resume a process for resolution.
		},
	}, sink)
	if resolved != 1 || handshake || receipt.ProcessStarted || receipt.Initialized || receipt.Output != "" || !receipt.ContainerQuiet || sink.observed.Load() || !errors.Is(err, errSupervisionTargetResolved) {
		if err != nil && resolved == 0 && !handshake && !receipt.ProcessStarted && !sink.observed.Load() {
			return supervisionCLIInfo{}, err
		}
		return supervisionCLIInfo{}, targetexec.ErrProtocol
	}
	if info.AgentID != spec.AgentID || info.Path == "" || info.Version == "" || info.Contract == "" || spec.Model != "" && info.EffectiveModel != "" && spec.Model != info.EffectiveModel || spec.ReasoningEffort != "" && info.EffectiveReasoningEffort != "" && spec.ReasoningEffort != info.EffectiveReasoningEffort {
		return supervisionCLIInfo{}, targetexec.ErrConflict
	}
	if err := requireSupervisionValidatedCLI(info); err != nil {
		return supervisionCLIInfo{}, err
	}
	return info, nil
}

type supervisionTargetResolutionSink struct{ observed atomic.Bool }

func (s *supervisionTargetResolutionSink) Event(string, json.RawMessage) { s.observed.Store(true) }
func (s *supervisionTargetResolutionSink) AssistantText(string, string)  { s.observed.Store(true) }
func (s *supervisionTargetResolutionSink) SessionIdentified(string)      { s.observed.Store(true) }
func (s *supervisionTargetResolutionSink) SessionInitialized()           { s.observed.Store(true) }

func (r *supervisionTargetRuntime) resolveExecution(ctx context.Context, request targetexec.Request) (json.RawMessage, error) {
	if request.Spec == nil || request.Input == nil {
		return nil, targetexec.ErrProtocol
	}
	spec := *request.Spec
	if spec.TargetID != request.TargetID || spec.SessionID != request.SessionID || spec.ExecutionID != request.ExecutionID || spec.WorkspaceID != request.WorkspaceID || spec.OwnerEpoch != request.OwnerEpoch || spec.AuthorizationVersion != request.AuthorizationVersion {
		return nil, targetexec.ErrConflict
	}
	if err := r.store.CheckGrant(spec.SessionID, spec.WorkspaceID, spec.OwnerEpoch, spec.AuthorizationVersion, time.Now().UTC()); err != nil {
		return nil, err
	}
	if _, err := r.workspace(spec.WorkspaceID); err != nil {
		return nil, err
	}
	profile, err := supervisionTargetRuntimeProfile(spec, request.Credentials)
	if err != nil {
		return nil, err
	}
	adapter, err := r.executionAdapter(spec.AgentID, spec.InstallationPath)
	if err != nil {
		return nil, err
	}
	info, err := resolveSupervisionTargetCLI(ctx, adapter, spec, *request.Input, profile)
	if err != nil {
		if profile != nil && profile.Secret != "" {
			return nil, errors.New(strings.ReplaceAll(err.Error(), profile.Secret, "[REDACTED]"))
		}
		return nil, err
	}
	// Probing can take time. A concurrent revocation cannot turn a result into
	// a reusable preparation under a now-stale grant.
	if err := r.store.CheckGrant(spec.SessionID, spec.WorkspaceID, spec.OwnerEpoch, spec.AuthorizationVersion, time.Now().UTC()); err != nil {
		return nil, err
	}
	return json.Marshal(info)
}
