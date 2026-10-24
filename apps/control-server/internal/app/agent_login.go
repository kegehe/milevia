package app

import (
	"context"
	"errors"
	"fmt"
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
		info = agentLoginInfo{Message: fmt.Sprintf("无法发起 %s 登录：%v。请打开官方登录页完成授权。", entry.Name, err)}
	} else if info.AuthURL == "" && info.UserCode == "" {
		// 管理 runner 的 Login 不真正拉起交互式 TUI（无头环境无法驱动），如实给可直接
		// 操作的指引，不说"已经启动流程"这类对其运行状态无法保证的话。
		//
		// ⚠️ 工具名与命令名都从**目录**取，不写死：写死的那一版在新增第二个需要登录的
		// 工具时，会把用户指向另一个 CLI（而这句话是"点进去之后唯一的指引"）。
		info.Message = fmt.Sprintf("%s 需要登录后才能使用。请在目标环境的终端里运行 %s 完成登录授权，然后回到本页点「检查登录状态」。",
			entry.Name, agentLoginCommand(entry))
	}
	writeJSON(w, http.StatusOK, info)
}

// agentLoginCommand 是“去终端里登录”的完整命令。目录没有声明 LoginArgs 时只给命令名，
// 与改造前一致；声明了（如 opencode auth login）就连子命令一起给出，避免用户只看到
// 一个打开 TUI 的裸命令。
func agentLoginCommand(entry AgentCatalogEntry) string {
	command := strings.TrimSpace(entry.CommandName)
	if args := strings.TrimSpace(strings.Join(entry.LoginArgs, " ")); args != "" {
		command += " " + args
	}
	return command
}

// agentLoginStatus 是 GET /api/runners/{runnerID}/agents/{agentID}/login-status。
//
// 回三件事：authStatus（三态，见 agent_auth.go）、note（判断不了时的一句话）、
// loggedIn（**给旧客户端留的兼容字段**：桌面端/手机端可能落后一个版本，而它们只认得
// 这个 bool）。新界面只读 authStatus —— bool 表达不了"判断不了"，而"判断不了"被念成
// "未登录"正是这一轮要修的那件事。
//
// ?force=true 绕过读数缓存：手动点「检查登录状态」走这条，页面进入时那次自动探测不带。
func (s *Server) agentLoginStatus(w http.ResponseWriter, r *http.Request) {
	runnerID := chi.URLParam(r, "runnerID")
	agentID := chi.URLParam(r, "agentID")
	entry, known := agentByID(agentID)
	if !known {
		writeError(w, http.StatusBadRequest, errors.New("不支持的工具"+agentID))
		return
	}
	if !entry.SupportsLogin {
		// 平台连登录入口都不给的工具，"登录状态"这个读数就没有意义。回 501 而不是
		// 一个假的 false —— 后者会让调用方以为"它只是没登录"。
		//
		// 判定放在解析后端**之前**：这条结论与执行环境无关（哪台机器上都一样），
		// 先解析只会把 501 变成一个随环境变化的 404。
		writeError(w, http.StatusNotImplemented, fmt.Errorf("%s 不在平台内登录的支持范围内", entry.Name))
		return
	}
	_, _, backend, err := s.resolveAgent(r.Context(), runnerID, agentID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	read := s.agentAuthFor(r.Context(), runnerID, entry, backend, forceRefresh(r))
	body := map[string]any{
		"authStatus": read.state,
		"loggedIn":   read.loggedIn(),
		"note":       read.note,
	}
	// 零值不写出去：那会让前端把它念成"1月1日 08:00 读到的"（同 runnerAgentsView.ProbedAt
	// 那条注释里的理由）。零值只出现在"调用方没等到"那一档。
	if !read.at.IsZero() {
		body["probedAt"] = read.at
	}
	writeJSON(w, http.StatusOK, body)
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
