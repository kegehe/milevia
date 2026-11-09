package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

// Interpret a target path using the target platform, never the controller's
// filepath rules. Linux paths must remain valid when the controller is Windows.
func supervisionTargetAbsolutePath(targetOS, value string) bool {
	if value == "" || len(value) > 4096 || strings.ContainsAny(value, "\x00\r\n") {
		return false
	}
	switch targetOS {
	case "linux":
		return path.IsAbs(value)
	case "windows":
		p := strings.ReplaceAll(value, `/`, `\`)
		if len(p) >= 3 && ((p[0] >= 'A' && p[0] <= 'Z') || (p[0] >= 'a' && p[0] <= 'z')) && p[1:3] == `:\` {
			return true
		}
		if strings.HasPrefix(p, `\\`) && !strings.HasPrefix(p, `\\?\`) && !strings.HasPrefix(p, `\\.\`) {
			parts := strings.Split(strings.TrimPrefix(p, `\\`), `\`)
			return len(parts) >= 3 && parts[0] != "" && parts[1] != "" && parts[2] != ""
		}
	}
	return false
}

// Preflight and commit use the same short transaction checks. Configuration
// probes must not reach a target on the authority of a forged/stale permit.
func (s *Server) supervisionTargetDispatchBasisTx(ctx context.Context, tx *sql.Tx, admissions map[string]WorkspaceAdmission, ss *supervisionSession, permit supervisionExecutionPermit) (targetexec.Control, targetexec.Input, string, error) {
	var empty targetexec.Control
	var input targetexec.Input
	state, err := checkSupervisionExecutionTx(ctx, tx, ss, permit)
	if err != nil {
		return empty, input, "", err
	}
	if state != "prepared" && state != "starting" || ss.Control != "" || ss.StateVersion != permit.Decision.ExpectedStateVersion {
		return empty, input, "", errSupervisionState
	}
	if err := s.verifySupervisionTargetProfileTx(ctx, tx, permit); err != nil {
		return empty, input, "", err
	}
	if err := requireSupervisionLease(ss, permit.Lease, time.Now().UTC()); err != nil {
		return empty, input, "", err
	}
	if err := requireSupervisionNoPendingDrift(ctx, tx, ss.ID); err != nil {
		return empty, input, "", err
	}
	var role, taskID string
	if err := tx.QueryRowContext(ctx, `select role,task_id from supervision_work_items where execution_id=? and session_id=?`, permit.Decision.ExecutionID, ss.ID).Scan(&role, &taskID); err != nil {
		return empty, input, "", err
	}
	config := ss.roleConfig(role)
	if role != permit.Role || taskID != permit.TaskID || config.AgentID != permit.AgentID || config.Model != permit.Model || config.ReasoningEffort != permit.ReasoningEffort {
		return empty, input, "", targetexec.ErrConflict
	}
	intent, err := loadSupervisionTargetControl(ctx, tx, ss.ID)
	if err != nil {
		return empty, input, "", err
	}
	control := intent.Control
	a, ok := admissions[ss.AdmissionID]
	if !ok || a.OwnerID != ss.ID || a.Epoch != ss.AdmissionEpoch || a.State != "owned" || a.RecoveryState != "ready" || control.WorkspaceID != a.DirectoryIdentity || control.State != "running" || control.OwnerEpoch != permit.Lease.Epoch || intent.Acknowledged != control.Version || intent.Wire == nil {
		return empty, input, "", targetexec.ErrAuthority
	}
	if _, err := s.supervisionWorkspaceSourceTx(ctx, tx, ss, a); err != nil {
		return empty, input, "", err
	}
	var targetOS, raw string
	if err := tx.QueryRowContext(ctx, `select b.target_os,i.input_json from runs r join supervision_target_bindings b on b.runner_id=? join supervision_scheduler_inputs i on i.execution_id=? where r.id=? and r.agent_runtime_id=? and b.target_id=? and b.protocol=?`, supervisionTargetConnectionKey(intent), permit.Decision.ExecutionID, permit.RunID, intent.RunnerID, control.TargetID, targetexec.ProtocolVersion).Scan(&targetOS, &raw); err != nil {
		return empty, input, "", err
	}
	input, err = supervisionTargetExecutionInputTx(ctx, tx, permit, json.RawMessage(raw))
	return control, input, targetOS, err
}

type supervisionTargetResolvedExecution struct {
	Spec                     targetexec.ExecutionSpec `json:"spec"`
	RequestedModel           string                   `json:"requestedModel"`
	RequestedReasoningEffort string                   `json:"requestedReasoningEffort"`
}

func supervisionTargetCLIExecutionSpec(permit supervisionExecutionPermit, control targetexec.Control, input targetexec.Input) targetexec.ExecutionSpec {
	spec := targetexec.ExecutionSpec{Protocol: targetexec.ProtocolVersion, TargetID: control.TargetID, SessionID: permit.Decision.SessionID, ExecutionID: permit.Decision.ExecutionID, WorkItemID: permit.Decision.WorkItemID, Attempt: permit.Decision.Attempt, OwnerEpoch: permit.Lease.Epoch, AuthorizationVersion: control.Version, WorkspaceID: control.WorkspaceID, Role: permit.Role, AgentID: permit.AgentID, Model: runModel(permit.Model, permit.Profile), ReasoningEffort: permit.ReasoningEffort, InputSHA256: supervisionSHA256(input.Observation), InstructionsSHA256: supervisionSHA256([]byte(input.Instructions))}
	if permit.Profile != nil && permit.Role != "test" {
		spec.ProfileRevisionID = permit.Profile.RevisionID
		spec.ProfileSHA256 = supervisionTargetProfileFingerprint(permit.Profile)
	}
	return spec
}

func supervisionTargetInstallationPath(ctx context.Context, q sqlQueryer, permit supervisionExecutionPermit) (string, error) {
	intent, err := loadSupervisionTargetControl(ctx, q, permit.Decision.SessionID)
	if err != nil {
		return "", err
	}
	installationRunner := intent.RunnerID
	if key := supervisionTargetConnectionKey(intent); strings.HasPrefix(key, supervisionTargetRoutePrefix) {
		route, err := loadSupervisionTargetRoute(ctx, q, key)
		if err != nil || route.RunnerID != intent.RunnerID {
			return "", errors.Join(targetexec.ErrConflict, err)
		}
		installationRunner = supervisionTargetInstallationRunner(route)
	}
	var path string
	err = q.QueryRowContext(ctx, `select coalesce(i.binary_path,'') from runs r left join agent_installations i on i.runner_id=? and i.agent_id=? where r.id=? and r.agent_runtime_id=?`, installationRunner, permit.AgentID, permit.RunID, intent.RunnerID).Scan(&path)
	return path, err
}

func (s *Server) recordSupervisionTargetResolution(ctx context.Context, permit supervisionExecutionPermit, expected targetexec.Control, input targetexec.Input, info supervisionCLIInfo, observedInstallation ...string) (targetexec.ExecutionSpec, error) {
	var resolved supervisionTargetResolvedExecution
	if err := requireSupervisionValidatedCLI(info); err != nil {
		return resolved.Spec, err
	}
	if permit.Role == "test" {
		return resolved.Spec, targetexec.ErrConflict
	}
	err := s.withSupervisionTx(ctx, permit.Decision.SessionID, time.Now().UTC(), func(tx *sql.Tx, admissions map[string]WorkspaceAdmission, ss *supervisionSession) error {
		control, frozen, targetOS, err := s.supervisionTargetDispatchBasisTx(ctx, tx, admissions, ss, permit)
		if err != nil {
			return err
		}
		if control != expected {
			return targetexec.ErrAuthority
		}
		installation, err := supervisionTargetInstallationPath(ctx, tx, permit)
		if err != nil {
			return err
		}
		if len(observedInstallation) > 1 || len(observedInstallation) == 1 && observedInstallation[0] != installation || installation != "" && !supervisionTargetAbsolutePath(targetOS, installation) {
			return targetexec.ErrConflict
		}
		if string(frozen.Observation) != string(input.Observation) || frozen.Instructions != input.Instructions || info.AgentID != permit.AgentID || info.Contract == "" || info.Version == "" || !supervisionTargetAbsolutePath(targetOS, info.Path) {
			return targetexec.ErrConflict
		}
		spec := supervisionTargetCLIExecutionSpec(permit, control, frozen)
		spec.InstallationPath = installation
		resolved = supervisionTargetResolvedExecution{Spec: spec, RequestedModel: spec.Model, RequestedReasoningEffort: spec.ReasoningEffort}
		if info.EffectiveModel != "" {
			if spec.Model != "" && spec.Model != info.EffectiveModel {
				return targetexec.ErrConflict
			}
			spec.Model = info.EffectiveModel
		}
		if info.EffectiveReasoningEffort != "" {
			if spec.ReasoningEffort != "" && spec.ReasoningEffort != info.EffectiveReasoningEffort {
				return targetexec.ErrConflict
			}
			spec.ReasoningEffort = info.EffectiveReasoningEffort
		}
		spec.Contract, spec.CLIPath, spec.CLIVersion = info.Contract, info.Path, info.Version
		spec.EntrypointSHA256, spec.RuntimeVersion = info.EntrypointSHA256, info.RuntimeVersion
		spec.ModelSettingsSHA256 = info.ModelSettingsSHA256
		resolved.Spec = spec
		var prior string
		err = tx.QueryRowContext(ctx, `select resolution from supervision_target_resolutions where execution_id=?`, spec.ExecutionID).Scan(&prior)
		if err == nil {
			if prior != string(mustJSON(resolved)) {
				return targetexec.ErrConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.ExecContext(ctx, `insert into supervision_target_resolutions(execution_id,session_id,resolution) values (?,?,?)`, spec.ExecutionID, ss.ID, string(mustJSON(resolved)))
		return err
	})
	return resolved.Spec, err
}

func (s *Server) resolveSupervisionTargetExecution(ctx context.Context, link *supervisionTargetLink, permit supervisionExecutionPermit) (targetexec.ExecutionSpec, error) {
	var empty targetexec.ExecutionSpec
	if link == nil || link.Client == nil || permit.Role == "test" {
		return empty, targetexec.ErrConflict // Target credentials are not copied here.
	}
	var control targetexec.Control
	var input targetexec.Input
	var installation string
	err := s.withSupervisionTx(ctx, permit.Decision.SessionID, time.Now().UTC(), func(tx *sql.Tx, admissions map[string]WorkspaceAdmission, ss *supervisionSession) error {
		var err error
		var targetOS string
		control, input, targetOS, err = s.supervisionTargetDispatchBasisTx(ctx, tx, admissions, ss, permit)
		if err != nil {
			return err
		}
		installation, err = supervisionTargetInstallationPath(ctx, tx, permit)
		if err == nil && installation != "" && !supervisionTargetAbsolutePath(targetOS, installation) {
			return targetexec.ErrConflict
		}
		return err
	})
	if err != nil {
		return empty, err
	}
	if control.TargetID != link.TargetID {
		return empty, targetexec.ErrAuthority
	}
	spec := supervisionTargetCLIExecutionSpec(permit, control, input)
	spec.InstallationPath = installation
	if err := validateSupervisionTargetCLIInput(spec, input); err != nil {
		return empty, err
	}
	response, err := link.Call(ctx, targetexec.Request{ID: uuid.NewString(), Method: "execution.resolve", TargetID: spec.TargetID, SessionID: spec.SessionID, ExecutionID: spec.ExecutionID, WorkspaceID: spec.WorkspaceID, OwnerEpoch: spec.OwnerEpoch, AuthorizationVersion: spec.AuthorizationVersion, Spec: &spec, Input: &input, Credentials: supervisionTargetCredentials(permit.Profile)})
	if err != nil {
		return empty, err
	}
	var info supervisionCLIInfo
	if json.Unmarshal(response.Data, &info) != nil {
		return empty, targetexec.ErrProtocol
	}
	return s.recordSupervisionTargetResolution(ctx, permit, control, input, info, installation)
}
