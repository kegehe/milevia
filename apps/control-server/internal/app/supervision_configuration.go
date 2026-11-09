package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

type supervisionRoleConfig struct {
	AgentID         string `json:"agentId"`
	ProfileID       string `json:"profileId,omitempty"`
	Model           string `json:"model,omitempty"`
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
}
type supervisionRoleConfigs map[string]supervisionRoleConfig

func (ss supervisionSession) roleConfig(role string) supervisionRoleConfig {
	if role == "" || role == "test" {
		role = "scheduler"
	}
	value := ss.Roles[role]
	if value.AgentID == "" {
		value.AgentID = ss.SchedulerAgentID
	}
	return value
}

func validateSupervisionRoles(roles supervisionRoleConfigs, defaults ...string) error {
	for role, value := range roles {
		if role != "scheduler" && role != "implementation" && role != "review" {
			return errors.New("仅可配置调度、实施、独立审查角色")
		}
		if value.AgentID != "" && !validProfileAgent(value.AgentID) {
			return errors.New("角色 CLI 必须来自现有 CLI 目录")
		}
		model, err := normalizeModelOverride(value.Model)
		if err != nil || model != value.Model || len(value.ProfileID) > 200 {
			return errors.New("模型或认证档案标识无效")
		}
		agent := value.AgentID
		if agent == "" {
			agent = roles["scheduler"].AgentID
		}
		if agent == "" && len(defaults) > 0 {
			agent = defaults[0]
		}
		if agent == "" {
			agent = "claude-code"
		}
		if !supervisionProfileAllowed(agent, value.ProfileID) {
			return errors.New("该 CLI 使用目标环境登录与配置，不支持平台认证档案")
		}
		valid := value.ReasoningEffort == ""
		for _, option := range supervisionReasoningOptions(agent) {
			valid = valid || option == value.ReasoningEffort
		}
		if !valid {
			return errors.New("无效的角色思考强度")
		}
	}
	return nil
}

func (s *Server) freezeSupervisionRoleRoutesTx(ctx context.Context, tx *sql.Tx, ss *supervisionSession, runnerID string) error {
	var routes []map[string]string
	if err := json.Unmarshal(ss.RouteSnapshot, &routes); err != nil {
		return err
	}
	kept := routes[:0]
	for _, route := range routes {
		if route["role"] == "" {
			kept = append(kept, route)
		}
	}
	routes = kept
	for _, role := range []string{"scheduler", "implementation", "review"} {
		value := ss.roleConfig(role)
		// 没有显式档案时继续复用启动时固化的项目路由。
		if value.ProfileID == "" {
			found := false
			for _, route := range routes {
				if route["role"] == "" && route["agentId"] == value.AgentID {
					found = true
					break
				}
			}
			if found {
				continue
			}
		}
		var requested *string
		if value.ProfileID != "" {
			requested = &value.ProfileID
		}
		entry, known := agentByID(value.AgentID)
		if !known {
			return errors.New("未知的托管 CLI")
		}
		if !entry.SupportsProfiles {
			if requested != nil {
				return errors.New("该 CLI 不支持平台认证档案")
			}
			routes = append(routes, map[string]string{"role": role, "agentId": value.AgentID, "mode": "cli_managed"})
			continue
		}
		selected, err := s.profileRouteForNewConversationTx(supervisionProfileContext(ctx), tx, requested, runnerID, value.AgentID, ss.ProjectID)
		if err != nil {
			return err
		}
		mode := "cli_managed"
		if selected.ProfileRevisionID != "" {
			mode = "pinned"
		}
		if selected.RouteRevisionID != "" {
			if err := tx.QueryRowContext(ctx, `select mode from project_agent_route_revisions where id=?`, selected.RouteRevisionID).Scan(&mode); err != nil {
				return err
			}
		}
		routes = append(routes, map[string]string{"role": role, "agentId": value.AgentID, "mode": mode, "profileRevisionId": selected.ProfileRevisionID, "routeRevisionId": selected.RouteRevisionID})
	}
	ss.RouteSnapshot = mustJSON(routes)
	for _, route := range routes {
		revision := route["profileRevisionId"]
		if revision == "" {
			continue
		}
		var targetID string
		err := tx.QueryRowContext(ctx, `select target_id from supervision_profile_targets where revision_id=?`, revision).Scan(&targetID)
		if errors.Is(err, sql.ErrNoRows) {
			project, projectErr := supervisionProjectTx(ctx, tx, ss.ProjectID)
			if projectErr != nil {
				return projectErr
			}
			if !s.supervisionProjectRoute(project).local() {
				return errors.New("跨端托管请使用绑定实际目标的托管认证档案，或使用目标 CLI 已有登录")
			}
			continue
		}
		if err != nil {
			return err
		}
		project, err := supervisionProjectTx(ctx, tx, ss.ProjectID)
		if err != nil {
			return err
		}
		expected, err := s.supervisionProfileDestinationTx(ctx, tx, project)
		if err != nil {
			return err
		}
		if targetID != expected {
			return errors.New("认证档案与项目实际执行目标不一致")
		}
	}
	return nil
}

func (s *Server) updateSupervisionRoles(ctx context.Context, id string, version int64, roles supervisionRoleConfigs) error {
	observed, err := loadSupervisionSession(ctx, s.db, id)
	if err != nil {
		return err
	}
	if err := requireSupervisionVersion(&observed, version); err != nil {
		return err
	}
	if err := validateSupervisionRoles(roles, observed.SchedulerAgentID); err != nil {
		return err
	}
	var observedProject Project
	if observed.State != "draft" {
		observedProject, err = s.getProjectByID(ctx, observed.ProjectID)
		if err != nil {
			return err
		}
		preview := observed
		preview.Roles = roles
		if value := roles["scheduler"]; value.AgentID != "" {
			preview.SchedulerAgentID = value.AgentID
		}
		for _, role := range []string{"scheduler", "implementation", "review"} {
			capability := s.supervisionCapabilityForProject(ctx, observedProject, preview.roleConfig(role).AgentID, false)
			if !capability.Available {
				return errors.New(capability.Reason)
			}
		}
	}
	return s.withSupervisionRequestTx(ctx, id, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		if err := validateSupervisionRoles(roles, ss.SchedulerAgentID); err != nil {
			return err
		}
		if err := requireSupervisionVersion(ss, version); err != nil {
			return err
		}
		if ss.State != "draft" && ss.State != "paused" && ss.State != "needs_input" {
			return errors.New("请先暂停托管并等待执行收敛，再调整角色配置")
		}
		if ss.Control == "stop" || ss.RecoveryState != "ready" {
			return errSupervisionState
		}
		if ss.State != "draft" {
			if err := supervisionRunsQuiescentTx(ctx, tx, id); err != nil {
				return err
			}
		}
		ss.Roles = roles
		if value := roles["scheduler"]; value.AgentID != "" {
			ss.SchedulerAgentID = value.AgentID
		}
		if ss.State != "draft" {
			project, err := supervisionProjectTx(ctx, tx, ss.ProjectID)
			if err != nil {
				return err
			}
			if project.Path != observedProject.Path || s.supervisionProjectRoute(project) != s.supervisionProjectRoute(observedProject) {
				return errWorkspaceAdmissionStale
			}
			var runnerID string
			if err := tx.QueryRowContext(ctx, `select coalesce(nullif(runner_id,''),runner) from projects where id=?`, ss.ProjectID).Scan(&runnerID); err != nil {
				return err
			}
			if err := s.freezeSupervisionRoleRoutesTx(ctx, tx, ss, runnerID); err != nil {
				return err
			}
			if err := suspendSupervisionQueuedTx(ctx, tx, ss, false, time.Now().UTC()); err != nil {
				return err
			}
			ss.LeaseOwner = ""
			ss.LeaseExpiresAt = nil
			ss.LeaseEpoch++
		}
		ss.StateVersion++
		return appendSupervisionEventTx(ctx, tx, ss, "roles.configured", roles, time.Now().UTC())
	})
}

func (s *Server) supervisionRolesHTTP(w http.ResponseWriter, r *http.Request) {
	var input struct {
		StateVersion int64                  `json:"stateVersion"`
		Roles        supervisionRoleConfigs `json:"roles"`
	}
	if !decode(w, r, &input) {
		return
	}
	if err := s.updateSupervisionRoles(r.Context(), chi.URLParam(r, "supervisionID"), input.StateVersion, input.Roles); err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Controlled adapters have a stricter launch contract than ordinary conversations.
func supervisionReasoningOptions(agent string) []string {
	switch agent {
	case "codebuddy", "qoder", "cursor":
		return []string{}
	}
	return reasoningEffortOptions(agent)
}
