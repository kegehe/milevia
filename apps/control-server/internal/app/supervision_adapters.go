package app

import (
	"context"
	"database/sql"
	"errors"
)

var errSupervisionCLINotInstalled = errors.New("目标 CLI 未安装")

// 与普通 Run 分开：普通交互的成功退出不具备托管所需的权限与收口保证。
// 通过现有 CLI 目录和管理 Runner 取得适配器，不维护另一份工具清单。
type supervisionCLIAdapter interface {
	AgentRunner
	supervisionCLIInfo(context.Context, bool) (supervisionCLIInfo, error)
	runSupervisionScheduler(context.Context, supervisionSchedulerCLIRequest, AgentRunSink) (supervisionSchedulerCLIReceipt, error)
}

type supervisionCLIInfo struct {
	AgentID                  string `json:"agentId"`
	Path                     string `json:"path"`
	Version                  string `json:"version,omitempty"`
	Contract                 string `json:"contract"`
	EffectiveModel           string `json:"effectiveModel,omitempty"`
	EffectiveReasoningEffort string `json:"effectiveReasoningEffort,omitempty"`
	Entrypoint               string `json:"entrypoint,omitempty"`
	EntrypointSHA256         string `json:"entrypointSha256,omitempty"`
	RuntimeVersion           string `json:"runtimeVersion,omitempty"`
	ModelSettingsSHA256      string `json:"modelSettingsSha256,omitempty"`
	ValidationPending        bool   `json:"validationPending,omitempty"`
}

var errSupervisionValidationPending = errors.New("受控适配代码已接入，真实账号下的权限及多任务验收尚未完成")

func requireSupervisionValidatedCLI(info supervisionCLIInfo) error {
	if info.ValidationPending {
		return errSupervisionValidationPending
	}
	return nil
}

type supervisionTarget struct {
	TargetID    string `json:"targetId,omitempty"`
	RunnerID    string `json:"runnerId"`
	Environment string `json:"environment"`
	Local       bool   `json:"local"`
	State       string `json:"state"`
	Reason      string `json:"reason,omitempty"`
}

func supervisionProjectTx(ctx context.Context, tx *sql.Tx, projectID string) (Project, error) {
	var p Project
	err := tx.QueryRowContext(ctx, `select id,path,coalesce(nullif(runner_id,''),runner) from projects where id=?`, projectID).Scan(&p.ID, &p.Path, &p.Runner)
	return p, err
}

func (s *Server) supervisionProjectTarget(p Project) supervisionTarget {
	id := p.Runner
	if p.RunnerID != "" {
		id = p.RunnerID
	}
	if id == "" {
		id = s.localRunnerID()
	}
	environment := s.resolveAgentTargetEnv(id, p.Path)
	target := supervisionTarget{RunnerID: id, Environment: string(environment), State: "adapter_pending"}
	// 本机挂载路径也可能要求在另一环境执行，不能只判断 Runner ID。
	target.Local = newSupervisionTargetRoute(id, environment).local()
	if !target.Local {
		if _, ok := s.runnerRegistry.get(id); !ok {
			target.State, target.Reason = "environment_unavailable", "项目目标 Runner 尚未连接或注册"
		} else {
			target.State = "ready" // CLI/target health are still re-probed before start.
		}
		return target
	}
	if err := supervisionLocalProcessCapability(); err != nil {
		target.Reason = err.Error()
		return target
	}
	target.State = "ready"
	return target
}

func (s *Server) supervisionAdapter(agentID string) (supervisionCLIAdapter, error) {
	entry, known := agentByID(agentID)
	if !known {
		return nil, errors.New("未知的托管 CLI")
	}
	backend, _ := s.agentBackend(s.localRunnerMeta(), entry)
	if entry.ID == "claude-code" {
		// 与普通本机执行的选择一致；管理注册表不应替代当前执行 Runner。
		backend = s.agentClaudeRunnerFor(s.resolveAgentTargetEnv(s.localRunnerID(), ""))
	}
	adapter, ok := backend.(supervisionCLIAdapter)
	if !ok || adapter == nil {
		return nil, errors.New("该 CLI 的受控资料输入及托管执行适配尚待实现")
	}
	return adapter, nil
}

func supervisionProfileAllowed(agentID, profileID string) bool {
	entry, known := agentByID(agentID)
	return known && (profileID == "" || entry.SupportsProfiles)
}
