package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"time"

	"github.com/google/uuid"
)

const supervisionExecutionProtocol = 1

func verifySupervisionResolvedProcessTx(ctx context.Context, tx *sql.Tx, permit supervisionExecutionPermit, identity supervisionProcessIdentity) error {
	var raw string
	err := tx.QueryRowContext(ctx, `select spec from supervision_execution_specs where execution_id=?`, permit.Decision.ExecutionID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // 旧执行及固定测试执行器沿用原身份与容器核对，不补造规格。
	}
	if err != nil {
		return err
	}
	var spec supervisionExecutionSpec
	if json.Unmarshal([]byte(raw), &spec) != nil || spec.ProtocolVersion != supervisionExecutionProtocol || spec.CLI.AgentID != permit.AgentID || spec.Role != permit.Role || spec.CLI.Path != identity.Executable {
		return errors.New("实际托管进程与持久化执行协议或程序路径不符")
	}
	if spec.SessionID != "" && (spec.SessionID != permit.Decision.SessionID || spec.ExecutionID != permit.Decision.ExecutionID || spec.WorkItemID != permit.Decision.WorkItemID || spec.Attempt != permit.Decision.Attempt || spec.LeaseEpoch != permit.Lease.Epoch) {
		return errors.New("实际托管进程与持久化工作项或授权代际不符")
	}
	return nil
}

// 只保存执行事实及非敏感配置。授权令牌、档案 Env 和认证正文不得进入此记录。
type supervisionExecutionSpec struct {
	ProtocolVersion    int                                `json:"protocolVersion"`
	TargetID           string                             `json:"targetId"`
	SessionID          string                             `json:"sessionId"`
	ExecutionID        string                             `json:"executionId"`
	WorkItemID         string                             `json:"workItemId"`
	Attempt            int                                `json:"attempt"`
	LeaseEpoch         int64                              `json:"leaseEpoch"`
	Workspace          supervisionExecutionWorkspace      `json:"workspace"`
	Authentication     supervisionExecutionAuthentication `json:"authentication"`
	CLI                supervisionCLIInfo                 `json:"cli"`
	Role               string                             `json:"role"`
	InputSHA256        string                             `json:"inputSha256"`
	InstructionsSHA256 string                             `json:"instructionsSha256"`
	Model              string                             `json:"model,omitempty"`
	ReasoningEffort    string                             `json:"reasoningEffort,omitempty"`
}

type supervisionExecutionWorkspace struct {
	AdmissionID       string `json:"admissionId"`
	Epoch             int64  `json:"epoch"`
	DirectoryIdentity string `json:"directoryIdentity"`
}

type supervisionExecutionAuthentication struct {
	Source            string `json:"source"`
	ProfileRevisionID string `json:"profileRevisionId,omitempty"`
	RouteRevisionID   string `json:"routeRevisionId,omitempty"`
}

func (s *Server) recordSupervisionExecutionSpec(ctx context.Context, permit supervisionExecutionPermit, input json.RawMessage, info supervisionCLIInfo) error {
	if err := requireSupervisionValidatedCLI(info); err != nil {
		return err
	}
	if info.AgentID != permit.AgentID || info.Contract == "" || info.Version == "" || !filepath.IsAbs(info.Path) {
		return errors.New("托管 CLI 未返回与授权匹配的路径、版本和协议")
	}
	var event Event
	err := s.withSupervisionTx(ctx, permit.Decision.SessionID, time.Now().UTC(), func(tx *sql.Tx, admissions map[string]WorkspaceAdmission, ss *supervisionSession) error {
		state, err := checkSupervisionExecutionTx(ctx, tx, ss, permit)
		if err != nil {
			return err
		}
		if state != "starting" || ss.Control != "" || ss.StateVersion != permit.Decision.ExpectedStateVersion {
			return errSupervisionState
		}
		if err := s.verifySupervisionTargetProfileTx(ctx, tx, permit); err != nil {
			return err
		}
		var targetID, expectedInput string
		authentication := supervisionExecutionAuthentication{Source: "cli_managed"}
		if err := tx.QueryRowContext(ctx, `select r.agent_runtime_id,i.input_json,r.agent_profile_revision_id,r.project_agent_route_revision_id from runs r join supervision_scheduler_inputs i on i.execution_id=? where r.id=?`, permit.Decision.ExecutionID, permit.RunID).Scan(&targetID, &expectedInput, &authentication.ProfileRevisionID, &authentication.RouteRevisionID); err != nil {
			return err
		}
		if authentication.ProfileRevisionID != "" {
			if permit.Profile == nil || permit.Profile.RevisionID != authentication.ProfileRevisionID || permit.Profile.AgentID != permit.AgentID {
				return errors.New("实际认证档案与持久化执行路由不符")
			}
			authentication.Source = "profile"
		} else if permit.Profile != nil {
			return errors.New("执行未授权临时认证档案")
		}
		admission, exists := admissions[ss.AdmissionID]
		if !exists || admission.OwnerID != ss.ID || admission.Epoch != ss.AdmissionEpoch || admission.State != "owned" || admission.RecoveryState != "ready" {
			return errWorkspaceAdmissionStale
		}
		if err := verifyWorkspaceAdmissionPath(admission); err != nil {
			return err
		}
		if string(input) != expectedInput {
			return errors.New("实际 CLI 输入与持久化工作项不一致")
		}
		if targetID == "" {
			targetID = s.localRunnerID()
		}
		spec := supervisionExecutionSpec{ProtocolVersion: supervisionExecutionProtocol, TargetID: targetID, CLI: info, Role: permit.Role, InputSHA256: supervisionSHA256(input), InstructionsSHA256: supervisionSHA256([]byte(supervisionRolePrompt(permit.Role))), Model: runModel(permit.Model, permit.Profile), ReasoningEffort: permit.ReasoningEffort}
		if info.EffectiveModel != "" {
			if spec.Model != "" && spec.Model != info.EffectiveModel {
				return errors.New("实际 CLI 模型与授权角色配置不符")
			}
			spec.Model = info.EffectiveModel
		}
		if info.EffectiveReasoningEffort != "" {
			if spec.ReasoningEffort != "" && spec.ReasoningEffort != info.EffectiveReasoningEffort {
				return errors.New("实际 CLI 思考强度与授权角色配置不符")
			}
			spec.ReasoningEffort = info.EffectiveReasoningEffort
		}
		spec.SessionID, spec.ExecutionID, spec.WorkItemID = ss.ID, permit.Decision.ExecutionID, permit.Decision.WorkItemID
		spec.Attempt, spec.LeaseEpoch, spec.Authentication = permit.Decision.Attempt, permit.Lease.Epoch, authentication
		spec.Workspace = supervisionExecutionWorkspace{AdmissionID: admission.ID, Epoch: admission.Epoch, DirectoryIdentity: admission.DirectoryIdentity}
		raw, err := json.Marshal(spec)
		if err != nil {
			return err
		}
		var previous string
		err = tx.QueryRowContext(ctx, `select spec from supervision_execution_specs where execution_id=?`, permit.Decision.ExecutionID).Scan(&previous)
		if err == nil {
			if previous != string(raw) {
				return errors.New("同一执行身份的目标、CLI协议或输入发生变化")
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := tx.ExecContext(ctx, `insert into supervision_execution_specs(execution_id,spec,created_at) values (?,?,?)`, permit.Decision.ExecutionID, string(raw), time.Now().UTC()); err != nil {
			return err
		}
		event = Event{ID: uuid.NewString(), ConversationID: permit.ConversationID, RunID: permit.RunID, Type: "supervision.runtime", Payload: raw, CreatedAt: time.Now().UTC()}
		if _, err := tx.ExecContext(ctx, `insert into events(id,conversation_id,run_id,type,payload,created_at) values (?,?,?,?,?,?)`, event.ID, event.ConversationID, event.RunID, event.Type, string(raw), event.CreatedAt); err != nil {
			return err
		}
		if s.remoteRelayConfigured() {
			if err := s.enqueueRemoteEventTxWithConversation(ctx, tx, event.ID, "", event.RunID, event.ConversationID, event.Type, event.Payload, event.CreatedAt); err != nil {
				return err
			}
		}
		return appendSupervisionEventTx(ctx, tx, ss, "execution.spec_resolved", map[string]any{"executionId": permit.Decision.ExecutionID, "spec": spec}, time.Now().UTC())
	})
	if err == nil && event.ID != "" {
		s.enqueueConversationEvent(event.ConversationID, mustJSON(event))
		s.wakeRemoteOutbox()
	}
	return err
}
