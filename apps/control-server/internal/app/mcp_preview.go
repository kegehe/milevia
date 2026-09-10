package app

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
)

// 「按环境预览」接口（docs/34 §10.3）。
//
// 表单里允许写 ${PROJECT_DIR} 这类占位符，但占位符在不同环境解析成完全不同的路径
//（Windows 是 C:\…，WSL 是 /mnt/c/…，SSH 是远端路径）。保存前先让用户看清「这条配置在
// 目标环境里到底长什么样」，可以挡掉绝大部分「本地能跑、远端跑不起来」的配置错误。
//
// 该接口直接接收表单字段而非 serverID —— 预览发生在保存之前。

// mcpPreviewInput 是一次预览请求的表单快照。
type mcpPreviewInput struct {
	Transport   string            `json:"transport"`
	Command     string            `json:"command"`
	Args        []string          `json:"args"`
	Env         map[string]string `json:"env"`
	URL         string            `json:"url"`
	Headers     map[string]string `json:"headers"`
	Environment string            `json:"environment"`
	ProjectID   string            `json:"projectId"`
}

// mcpPreviewResult 是解析后的形态。
//
// 密钥不在此接口处理：表单里已保存的凭据只以「键名」形式存在（值不回显），故预览天然
// 不涉及明文，也不会把 sec_ 引用解出来。
type mcpPreviewResult struct {
	Environment string            `json:"environment"`
	ProjectPath string            `json:"projectPath"`
	Command     string            `json:"command"`
	Args        []string          `json:"args"`
	Env         map[string]string `json:"env"`
	URL         string            `json:"url"`
	Headers     map[string]string `json:"headers"`
	Notes       []string          `json:"notes"`
}

func (s *Server) previewMCPServer(w http.ResponseWriter, r *http.Request) {
	var input mcpPreviewInput
	if !decode(w, r, &input) {
		return
	}
	environment := strings.TrimSpace(input.Environment)
	// 兼容前端可能传入的旧字面量。
	if environment == "remote" {
		environment = string(agentTargetEnvRemote)
	}
	if environment == "" {
		environment = string(agentTargetEnvWindows)
	}
	if !mcpValidEnvironments[environment] {
		writeError(w, http.StatusBadRequest, errors.New("未知的目标环境"))
		return
	}
	target := agentTargetEnv(environment)

	projectPath := ""
	if projectID := strings.TrimSpace(input.ProjectID); projectID != "" {
		project, err := s.getProjectByID(r.Context(), projectID)
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("project not found"))
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		projectPath = project.Path
	}

	mentionsProjectDir := false
	resolve := func(value string) string {
		if !strings.Contains(value, "${PROJECT_DIR}") {
			return value
		}
		mentionsProjectDir = true
		// 没选项目就原样保留，由下面的 note 说明原因（而不是静默替换成空串）。
		return resolveMCPPlaceholders(value, target, projectPath)
	}

	result := mcpPreviewResult{
		Environment: environment,
		ProjectPath: projectPath,
		Command:     resolve(input.Command),
		URL:         resolve(input.URL),
		Args:        []string{},
		Env:         map[string]string{},
		Headers:     map[string]string{},
		Notes:       []string{},
	}
	for _, arg := range input.Args {
		result.Args = append(result.Args, resolve(arg))
	}
	for key, value := range input.Env {
		result.Env[key] = resolve(value)
	}
	for key, value := range input.Headers {
		result.Headers[key] = resolve(value)
	}

	if mentionsProjectDir && projectPath == "" {
		result.Notes = append(result.Notes, "未选择项目：${PROJECT_DIR} 无法解析，已原样保留。")
	}
	if projectPath != "" {
		switch target {
		case agentTargetEnvWSL:
			result.Notes = append(result.Notes, "WSL 环境按 /mnt/<盘符>/ 形态解析项目路径。")
		case agentTargetEnvRemote:
			result.Notes = append(result.Notes, "SSH 远端使用远端路径形态；stdio 型 server 依赖远端已安装对应运行时。")
		}
	}
	if containsUnresolvedPlaceholder(result) {
		result.Notes = append(result.Notes, "其余 ${VAR} 形态（如 ${HOME}）由 CLI 在目标环境按进程环境展开，此处不做替换。")
	}
	writeJSON(w, http.StatusOK, result)
}

// containsUnresolvedPlaceholder 报告解析结果里是否还剩未处理的 ${...} 占位符。
func containsUnresolvedPlaceholder(result mcpPreviewResult) bool {
	values := []string{result.Command, result.URL}
	values = append(values, result.Args...)
	for _, value := range result.Env {
		values = append(values, value)
	}
	for _, value := range result.Headers {
		values = append(values, value)
	}
	for _, value := range values {
		if strings.Contains(value, "${") {
			return true
		}
	}
	return false
}
