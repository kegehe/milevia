package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// 单个工具的 HTTP 入口（泛化路由 /api/runners/{runnerID}/agents/{agentID}/...）。
//
// 存在意义：把"某个工具"从 URL 参数一路带到后端，于是前端不必再维护
// `agentID === "codex" ? "codex" : "claude"` 这种把工具名映射成旧路径段的写法
// （那段映射本身就是"清单散落"的一种表现 —— 新增工具时它会把新工具指向 Claude 的路径）。
//
// 旧的 /claude/* 与 /codex/* 路由保留为薄委托，等前端切过去后再删。

// resolveAgent 解析 {runnerID} + {agentID}，并选出能执行该工具的后端。
// 返回的 error 文案已经可以直接给用户看。
func (s *Server) resolveAgent(ctx context.Context, runnerID, agentID string) (RunnerMeta, AgentCatalogEntry, AgentRunner, error) {
	entry, ok := agentByID(agentID)
	if !ok {
		return RunnerMeta{}, AgentCatalogEntry{}, nil, fmt.Errorf("不支持的工具 %s", agentID)
	}
	if runnerID == "wsl-local" {
		// 与 runnerStatus 一致：WSL 可能因冷启动超时未注册，按需补一次探测。
		s.ensureWSLRunner()
	}
	meta, ok := s.runnerRegistry.getMeta(runnerID)
	if !ok {
		if !isLocalRunnerID(runnerID) {
			return RunnerMeta{}, AgentCatalogEntry{}, nil, errors.New("runner not found")
		}
		// 本机 runner 在极端情况下可能未注册。工具操作仍按"本机"语义处理：
		// agentBackend 对本机 Codex 直接取 codexRunner，不依赖注册表；
		// 对本机 Claude 会返回 nil，下面照常报错。
		meta = RunnerMeta{ID: runnerID, Name: runnerID, Environment: "local"}
	}
	backend, unsupportedReason := s.agentBackend(meta, entry)
	if unsupportedReason != "" {
		return RunnerMeta{}, AgentCatalogEntry{}, nil, errors.New(unsupportedReason)
	}
	if backend == nil {
		return RunnerMeta{}, AgentCatalogEntry{}, nil, errors.New("runner not found")
	}
	return meta, entry, backend, nil
}

// checkAgentStatus 是 GET 泛化路由的处理器：查某个工具在某 Runner 上是否有新版。
func (s *Server) checkAgentStatus(w http.ResponseWriter, r *http.Request) {
	s.checkAgentStatusFor(w, r, chi.URLParam(r, "runnerID"), chi.URLParam(r, "agentID"))
}

func (s *Server) checkAgentStatusFor(w http.ResponseWriter, r *http.Request, runnerID, agentID string) {
	_, _, backend, err := s.resolveAgent(r.Context(), runnerID, agentID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	s.checkAgentUpdate(w, r, backend)
}

// updateAgentStatus 是 POST 泛化路由的处理器：安装或升级某个工具。
func (s *Server) updateAgentStatus(w http.ResponseWriter, r *http.Request) {
	s.updateAgentStatusFor(w, r, chi.URLParam(r, "runnerID"), chi.URLParam(r, "agentID"))
}

// installAgentStatus 是 POST /api/runners/{runnerID}/agents/{agentID}/install。
func (s *Server) installAgentStatus(w http.ResponseWriter, r *http.Request) {
	s.installAgentFor(w, r, chi.URLParam(r, "runnerID"), chi.URLParam(r, "agentID"))
}

func (s *Server) updateAgentStatusFor(w http.ResponseWriter, r *http.Request, runnerID, agentID string) {
	meta, entry, backend, err := s.resolveAgent(r.Context(), runnerID, agentID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	s.updateAgent(w, r, meta.ID, entry.ID, entry.Name, backend)
}

// ── 过渡路由 ────────────────────────────────────────────────────────────────
//
// 下列四个 handler 只做一件事：把旧的 /claude/*、/codex/* 路径委托给泛化实现。
// 它们内部一律不再各自判断工具（原先 checkClaudeUpdate / checkCodexUpdate 各有一份
// "这个 runner 支不支持该工具"的判断）。前端切到泛化路由后即可整体删除。

func (s *Server) checkClaudeUpdate(w http.ResponseWriter, r *http.Request) {
	s.checkAgentStatusFor(w, r, chi.URLParam(r, "runnerID"), "claude-code")
}

func (s *Server) checkCodexUpdate(w http.ResponseWriter, r *http.Request) {
	s.checkAgentStatusFor(w, r, chi.URLParam(r, "runnerID"), "codex")
}

func (s *Server) updateClaude(w http.ResponseWriter, r *http.Request) {
	s.updateAgentStatusFor(w, r, chi.URLParam(r, "runnerID"), "claude-code")
}

func (s *Server) updateCodex(w http.ResponseWriter, r *http.Request) {
	s.updateAgentStatusFor(w, r, chi.URLParam(r, "runnerID"), "codex")
}
