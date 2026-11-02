package app

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// remoteInteractionOperations 是挂在 `/api/...` 自身、而不是项目或会话前缀下的手机操作。
//
// 提问作答、工具确认、交接轮询、配置档案名单都复用桌面端已有的 handler。
// 档案名单单独走一份不含密钥的查询：listAgentProfiles 会把 options_json 里的环境变量
// 原样交出去，那份不能进手机，也不能经过云端中继。
func (s *Server) remoteInteractionOperations() map[string]remoteOperation {
	return map[string]remoteOperation{
		"interaction.answerAsk": {
			Method:     http.MethodPost,
			Scope:      relayScopeAPI,
			Path:       "/api/asks/{askID}",
			PathParams: []string{"askID"},
			Handle:     s.answerAsk,
		},
		"interaction.resolveApproval": {
			Method:     http.MethodPost,
			Scope:      relayScopeAPI,
			Path:       "/api/approvals/{approvalID}",
			PathParams: []string{"approvalID"},
			Handle:     s.resolveApproval,
		},
		"handoff.status": {
			Method:     http.MethodGet,
			Scope:      relayScopeAPI,
			Path:       "/api/conversation-handoffs/{handoffID}",
			Query:      true,
			PathParams: []string{"handoffID"},
			Handle:     s.getConversationHandoff,
		},
		"runner.profileChoices": {
			Method:     http.MethodGet,
			Scope:      relayScopeAPI,
			Path:       "/api/runners/{runnerID}/profile-choices",
			Query:      true,
			PathParams: []string{"runnerID"},
			MaxBytes:   64 << 10,
			Label:      "配置档案",
			Handle:     s.listMobileProfileChoices,
		},
	}
}

// mobileProfileChoice 是新建会话时可选的档案。只有名字、模型和状态，没有密钥、
// Base URL 或 options。桌面端新建会话同样只展示 cli_managed 且启用的档案。
type mobileProfileChoice struct {
	ID        string `json:"id"`
	AgentID   string `json:"agentId"`
	Name      string `json:"name"`
	Model     string `json:"model,omitempty"`
	Enabled   bool   `json:"enabled"`
	IsDefault bool   `json:"isDefault"`
	AuthMode  string `json:"authMode"`
	State     string `json:"state"`
}

func (s *Server) listMobileProfileChoices(w http.ResponseWriter, r *http.Request) {
	runnerID := chi.URLParam(r, "runnerID")
	rows, err := s.db.QueryContext(r.Context(), `select p.id,p.agent_id,p.name,p.enabled,p.is_default,r.model,r.auth_mode,r.state
		from agent_profiles p join agent_profile_revisions r on r.id=p.current_revision_id
		where p.runner_id=? order by p.agent_id,p.is_default desc,p.name`, runnerID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	choices := []mobileProfileChoice{}
	for rows.Next() {
		var choice mobileProfileChoice
		var enabled, isDefault int
		if err := rows.Scan(&choice.ID, &choice.AgentID, &choice.Name, &enabled, &isDefault, &choice.Model, &choice.AuthMode, &choice.State); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		choice.Enabled = enabled != 0
		choice.IsDefault = isDefault != 0
		choices = append(choices, choice)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, choices)
}
