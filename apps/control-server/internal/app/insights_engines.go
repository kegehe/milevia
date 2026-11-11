package app

import (
	"database/sql"
	"errors"
	"net/http"
)

// insightEngines 从目标电脑读取目录及当前项目的真实运行环境。
// 使用项目作用域，手机无需访问同源 /api/agents 或传入任意 runnerID。
func (s *Server) insightEngines(w http.ResponseWriter, r *http.Request) {
	project, err := s.getProjectByID(r.Context(), r.PathValue("projectID"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("项目不存在"))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if project.Runner == "wsl-local" {
		s.ensureWSLRunner()
	}
	meta, ok := s.runnerRegistry.getMeta(project.Runner)
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("当前项目的运行环境不可用"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": agentCatalog(),
		"runner": map[string]any{
			"id": meta.ID, "name": meta.Name, "environment": meta.Environment,
			"local": isLocalRunnerID(meta.ID), "agents": s.probeAgents(r.Context(), meta),
		},
	})
}
