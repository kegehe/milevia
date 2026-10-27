package app

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"
)

type gitConflictSuggestAgent struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// conflictSuggestionLocalRunner 只为已有本机执行面的工具返回独立 runner。
// 不能把这些工具的请求送给 Claude，也不能用本机 CLI 读取 WSL / SSH 项目。
func (s *Server) conflictSuggestionLocalRunner(project Project, agentID string) (AgentRunner, error) {
	target := s.resolveAgentTargetEnv(project.Runner, project.Path)
	nativeTarget := s.resolveAgentTargetEnv(s.localRunnerID(), "")
	// WSL 的 /mnt/<盘符>/ 项目按项目约定使用 Windows 工具，不能仅凭
	// runner=wsl-local 就误用 WSL 进程里的本机 CLI。
	local := isLocalRunnerID(project.Runner) && target == nativeTarget
	switch agentID {
	case "codebuddy":
		if !local {
			return nil, errors.New(codebuddyLocalOnlyMessage)
		}
		return s.codebuddyRunner, nil
	case "qoder":
		if !local {
			return nil, errors.New(qoderLocalOnlyMessage)
		}
		return s.qoderRunner, nil
	case "cursor":
		if !local {
			return nil, errors.New(cursorLocalOnlyMessage)
		}
		return s.cursorRunner, nil
	case "opencode":
		if target == agentTargetEnvWindows && nativeTarget != agentTargetEnvWindows {
			return nil, errors.New(opencodeWindowsBridgeMessage)
		}
		return nil, nil
	default:
		return nil, nil // 其余工具复用现有的一次性只读分析链路。
	}
}

func (s *Server) conflictSuggestionAgents(project Project) []gitConflictSuggestAgent {
	agents := make([]gitConflictSuggestAgent, 0)
	for _, entry := range agentCatalog() {
		if !entry.SupportsConflictSuggestions {
			continue
		}
		if _, err := s.conflictSuggestionLocalRunner(project, entry.ID); err != nil {
			continue
		}
		agents = append(agents, gitConflictSuggestAgent{ID: entry.ID, Name: entry.Name})
	}
	return agents
}

// runConflictSuggestionAgent 不改变优化建议扫描的能力开关。CodeBuddy / Qoder
// 只提供 StartSession，因此为冲突建议创建临时只读会话，首轮完成或取消就关闭。
func (s *Server) runConflictSuggestionAgent(ctx context.Context, project Project, agentID, prompt string) (string, error) {
	entry, ok := agentByID(agentID)
	if !ok || !entry.SupportsConflictSuggestions {
		return "", errors.New("unsupported agent")
	}
	runner, err := s.conflictSuggestionLocalRunner(project, agentID)
	if err != nil {
		return "", err
	}
	if entry.SupportsProjectInsights {
		return s.runReadOnlyAgentWithSchema(ctx, project, agentID, prompt, conflictSuggestionOutputSchema, 0, nil)
	}
	if runner == nil || !runner.Ready(ctx) {
		return "", fmt.Errorf("%s 不可用，请先在 CLI 工具页安装或登录", entry.Name)
	}
	request := AgentRunRequest{
		SessionID: uuid.NewString(), ProjectPath: project.Path,
		AgentID: agentID, Prompt: prompt, PermissionMode: "read_only",
		RunID: uuid.NewString(), SkipSessionID: true,
		Model: "", // 这些工具暂不支持受管档案，沿用各 CLI 自己的模型配置。
	}
	sink := &conflictSuggestionTurnSink{finished: make(chan error, 1)}
	if streaming, ok := runner.(StreamingAgentRunner); ok {
		session, startErr := streaming.StartSession(ctx, AgentSessionRequest{
			SessionID: request.SessionID, ProjectPath: request.ProjectPath,
			PermissionMode: request.PermissionMode, Model: request.Model,
		})
		if startErr != nil {
			return "", startErr
		}
		defer session.Stop()
		if err := session.Send(request, sink); err != nil {
			return "", err
		}
		select {
		case err = <-sink.finished:
		case err = <-session.Done():
			// result 与进程退出可以同时就绪；已经完成的回合优先。
			select {
			case err = <-sink.finished:
			default:
				if err == nil {
					err = fmt.Errorf("%s 会话在返回合并建议前结束", entry.Name)
				}
			}
		case <-ctx.Done():
			return "", ctx.Err()
		}
	} else {
		err = runner.Run(ctx, request, sink)
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", err
	}
	return sink.String(), nil
}

type conflictSuggestionTurnSink struct {
	orchestrationReviewSink
	finished chan error
	once     sync.Once
}

func (*conflictSuggestionTurnSink) TurnStarted() {}
func (sink *conflictSuggestionTurnSink) TurnFinished(err error) {
	sink.once.Do(func() { sink.finished <- err })
}
