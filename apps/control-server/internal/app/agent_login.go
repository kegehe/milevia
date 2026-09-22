package app

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// 登录集成：管理页对「需要登录」的工具发起的登录入口（阶段 2）。
//
// codebuddy 的登录是交互式 TUI + 浏览器授权，官方未给出文档化的「无头返回设备码 URL」
// 命令，所以本层先提供可调用的登录适配面：
//   - 发起登录（POST …/login）：在目标环境如实启动 CLI 登录流程，尽力捕获授权链接，
//     捕获不到就把官方授权的可操作指引透出给页面（docs/CodeBuddy 方案 §阶段2 的降级）。
//   - 查登录态（GET …/login-status）：报该工具当前是否已登录。
//
// 设备码/浏览器的端到端回调、登录成功后的自动识别，都依赖对已安装 codebuddy 的真机
// 输出校准，属**待校准接缝** —— 本层不假装实现，而是把能确定的部分（启动登录、透出
// 输出与指引、如实上报「暂无法自动判别」）做成结构，保留扩展点。

// loginAgentRunner 是可实现"发起该工具自己的登录"的可选接口。
type loginAgentRunner interface {
	Login(context.Context) (agentLoginInfo, error)
}

// authStateRunner 是可实现"报告该工具是否已登录"的可选接口。
type authStateRunner interface {
	LoginStatus(context.Context) bool
}

// agentLoginInfo 描述一次登录发起的可呈现结果。
type agentLoginInfo struct {
	// AuthURL 尽力捕获的授权链接；空表示未捕获到，交给 Message 指引。
	AuthURL string `json:"authUrl,omitempty"`
	// UserCode 尽力捕获的用户码；空表示未捕获到。
	UserCode string `json:"userCode,omitempty"`
	// Message 给用户的可操作指引（授权链接/验证码捕获不到时的兜底）。
	Message string `json:"message"`
}

// runAgentLogin 是 POST /api/runners/{runnerID}/agents/{agentID}/login。
func (s *Server) runAgentLogin(w http.ResponseWriter, r *http.Request) {
	runnerID := chi.URLParam(r, "runnerID")
	agentID := chi.URLParam(r, "agentID")
	entry, ok := agentByID(agentID)
	if !ok {
		writeError(w, http.StatusBadRequest, errors.New("不支持的工具"+agentID))
		return
	}
	if !entry.SupportsLogin {
		writeError(w, http.StatusNotImplemented, errors.New("该工具不支持平台内登录"))
		return
	}
	_, _, backend, err := s.resolveAgent(r.Context(), runnerID, agentID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	loginR, ok := backend.(loginAgentRunner)
	if !ok {
		writeError(w, http.StatusNotImplemented, errors.New("该工具尚未接通平台内登录"))
		return
	}
	info, err := loginR.Login(r.Context())
	if err != nil {
		info = agentLoginInfo{Message: "无法发起 CodeBuddy 登录：" + err.Error() + "。请打开官方登录页完成授权。"}
	} else if info.AuthURL == "" && info.UserCode == "" {
		// 管理 runner 的 Login 不真正拉起交互式 TUI（无头环境无法驱动），如实给可直接
		// 操作的指引，不说"已经启动流程"这类对其运行状态无法保证的话。
		info.Message = "CodeBuddy 需要登录后才能使用。请在目标环境的终端运行 codebuddy，按提示选择站点并在浏览器完成授权，授权完成后在本页点「检查登录状态」。"
	}
	writeJSON(w, http.StatusOK, info)
}

// agentLoginStatus 是 GET /api/runners/{runnerID}/agents/{agentID}/login-status。
func (s *Server) agentLoginStatus(w http.ResponseWriter, r *http.Request) {
	runnerID := chi.URLParam(r, "runnerID")
	agentID := chi.URLParam(r, "agentID")
	_, _, backend, err := s.resolveAgent(r.Context(), runnerID, agentID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	loggedIn := false
	if stateR, ok := backend.(authStateRunner); ok {
		loggedIn = stateR.LoginStatus(r.Context())
	}
	writeJSON(w, http.StatusOK, map[string]any{"loggedIn": loggedIn})
}

// captureDeviceURLConditional 尽力从登录输出里提取首段 http(s) 链接；提取不到返回空。
// 供管理员在真机校准登录输出时改为真实调用（当前仅保留提取逻辑）。
func captureDeviceURLConditional(output string) string {
	markers := []string{"https://", "http://"}
	for _, prefix := range markers {
		idx := strings.Index(strings.ToLower(output), prefix)
		if idx < 0 {
			continue
		}
		end := idx + len(prefix)
		for end < len(output) {
			c := output[end]
			if c == ' ' || c == '\n' || c == '\r' || c == ')' || c == ']' || c == '\t' {
				break
			}
			end++
		}
		if url := output[idx:end]; url != "" {
			return url
		}
	}
	return ""
}
