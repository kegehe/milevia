package app

import (
	"context"
	"fmt"
	"time"
)

// channelHealthTimeout 是"这台机器现在还连得上吗"这个问题自身的上限。
//
// 它只管**这一问**；各 runner 的实现可以再收窄（SSH 那条是 10 秒）。
const channelHealthTimeout = 30 * time.Second

// channelHealthRunner 是"能回答自己这条执行通道现在通不通"的 runner。
//
// 存在的理由：工具探测失败有两种完全不同的原因 —— "那个工具没装"与
// "这台机器现在联系不上"。前者该说「未安装」，后者该说「无法检测」
// （docs/42 §5.2 的三档）。注册表只能回答"这台机器在不在清单里"，
// 回答不了"现在还连得上吗" —— 于是连不上的 SSH 会被说成"工具没装"，
// 用户被支去装一个装不上的东西。
//
// 没有实现该接口的 runner 一律按"通道可用"处理：本机与 WSL 各自有它们的
// 通道语义（WSL 走自己的读数缓存与保活），不在这里另开一套判据。
type channelHealthRunner interface {
	// ChannelHealth 报告通道当前是否可用；不可用时第二个返回值是给用户看的原因。
	ChannelHealth(ctx context.Context) (bool, string)
}

// probeChannelHealth 报告这台机器的执行通道当前是否可用，并给出原因。
//
// 空原因 = 通道可用（或这个 runner 不支持这项检查）。
//
// ⚠️ 调用方**必须**在拿它与工具探测结果**之前**问一次：通道已经断了的时候，
// 后面的每一次工具探测都会失败，而那些失败会被读成"工具没装"——
// 这正是这个检查要挡掉的误判。
func (s *Server) probeChannelHealth(ctx context.Context, meta RunnerMeta) (bool, string) {
	runner, ok := s.runnerRegistry.get(meta.ID)
	if !ok {
		// 这台机器不在注册表里：它压根没准备好（WSL 没装 / SSH 没连上），
		// "通道可用"是错的，但那是调用方（resolveProbeTarget）要回答的另一件事。
		return true, ""
	}
	checker, ok := runner.(channelHealthRunner)
	if !ok {
		return true, ""
	}
	healthCtx, cancel := context.WithTimeout(ctx, channelHealthTimeout)
	defer cancel()
	return checker.ChannelHealth(healthCtx)
}

// channelUnavailableStatuses 是"通道坏了"时每个工具的状态。
//
// 关键在于**理由**：不能写"未安装"（那是把"联系不上"说成"没装"），
// 也不能笼统写一个"不可用"（用户会以为是那个工具坏了）—— 要写通道断开的原因。
//
// "这个环境根本不提供该工具"那一档照旧：它与通道无关（换环境那条结论仍然成立）。
func (s *Server) channelUnavailableStatuses(meta RunnerMeta, reason string) []AgentStatus {
	entries := agentCatalog()
	out := make([]AgentStatus, 0, len(entries))
	for _, entry := range entries {
		if _, unsupportedReason := s.agentBackend(meta, entry); unsupportedReason != "" {
			out = append(out, AgentStatus{ID: entry.ID, Status: agentStatusUnsupported, Reason: unsupportedReason})
			continue
		}
		out = append(out, AgentStatus{ID: entry.ID, Status: agentStatusUnavailable, Reason: reason})
	}
	return out
}

// channelFailureLabel 给"通道坏了"这一档一个能直接给用户看的说法。
func channelFailureLabel(name, reason string) string {
	if name == "" {
		return reason
	}
	return fmt.Sprintf("%s：%s", name, reason)
}
