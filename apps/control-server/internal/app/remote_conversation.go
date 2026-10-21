package app

import "net/http"

// 手机端能执行的**会话控制**操作。名单类型与合成逻辑在 remote_relay.go。
//
// 为什么只有一条，而且为什么它不在 remote_git.go / remote_fs.go 里：中继目前按
// **落点前缀**分成两个作用域（见 relayScope），这份是会话域那一个 —— 它的请求打到
// `/api/conversations/{id}/...`，不解析工作区，也不需要项目路径。分成单独一份而不是
// 塞进 Git 那张表，是因为那份表的每一条都在 `/api/projects/{projectID}` 之下（它自己
// 的注释就把这条写成前提），混进去会让"前缀"这件事变成逐条判断。
//
// 名字里的 conversation. 前缀与命令通道的 conversation.* 是**两条通道上的两个东西**，
// 不要望文生义：那条是云端持久化的命令（create / message / shortcut），这条是中继上的
// 请求-响应。选这条的理由写在 remote_relay.go 顶部那段（判据：除非真的需要命令通道的
// 幂等键、持久化审计与终态机，否则走中继）。
//
// ⚠️ 本表的 op **不使用信封里的 projectId**，也不校验会话属于哪个项目 —— 与项目域那些
// 每条都要先解析工作区的 op 不同，这里的落点只有会话。写成"故意不校验"而不是留个漏洞：
// 中继本来就用 Agent 令牌鉴权，调用方（这台电脑的机主）能触达的就是全部项目，
// 绑定 projectId 与 conversationId 只是不变量层面的加固，代价却要让中继层认识业务。
// 手机端永远发一致的一对（rpcTransport 用的就是当前项目）。真要加固，正确的位置是
// 在这个文件新加一条"作用域 = 会话 + 校验归属"的 op，而不是把项目解析悄悄塞进来。
// 会话**不存在**仍然是 404（stopConversationRuns 里那道存在性判据）—— 那一条与
// getConversation / clearConversation 那一族对齐，不是可选项。
func (s *Server) remoteConversationOperations() map[string]remoteOperation {
	return map[string]remoteOperation{
		// 历史会话目录按项目读取。它只返回会话元数据与预览，不经云端持久化；
		// 完整消息在用户选中某条会话后由 conversation.history 按页读取。
		"conversation.list": {
			Method: http.MethodGet,
			Path:   "/conversations",
			Query:  true,
			Handle: s.listConversations,
		},
		// 复用桌面端已有的游标分页接口。上限低于中继帧限制，避免一页历史
		// 因为超长助手回复而断开整条 Agent 连接。
		"conversation.history": {
			Method:   http.MethodGet,
			Scope:    relayScopeConversation,
			Path:     "",
			Query:    true,
			MaxBytes: 320 << 10,
			Label:    "会话历史",
			Handle:   s.getConversation,
		},
		// 当前会话是桌面端与手机端共享的项目级选择。复用桌面端的激活
		// handler，避免手机另写一套 is_current 的更新规则。
		"conversation.activate": {
			Method: http.MethodPost,
			Scope:  relayScopeConversation,
			Path:   "/activate",
			Handle: s.activateConversation,
		},
		// 停止这条会话当前在跑的那一轮。
		//
		// 它按**会话**停而不是按 run 停，与桌面端的 `POST /api/runs/{runID}/stop` 是两件事：
		// 桌面端手里有 run id（它跟着事件维护 run 状态），手机端没有 —— 它只有"这条会话在跑"
		// 这一个判据（conversation.status 与实时事件）。让手机端自己维护 run 生命周期，
		// 等于把链路里最容易做错的一段搬到最不容易测的一侧；而"哪一轮是活跃的"在服务端
		// 就是一次查询（见 activeRunIDForConversation），本来也不该由客户端决定。
		// 粒度不同也意味着接口不重复：桌面端停的是"这一轮"，手机端停的是"这条会话现在在跑的"。
		//
		// Query 为 true 是本表唯一的例外（写操作走查询参数而不是请求体），刻意如此：
		// force 与桌面端 `/api/runs/{runID}/stop?force=true` 同形，两处的语义与默认值
		// 只留一份解释。params 里除了 force 没有别的东西可填。
		"conversation.stop": {
			Method: http.MethodPost,
			Scope:  relayScopeConversation,
			Path:   "/stop",
			Query:  true,
			Handle: s.stopConversationRuns,
		},
		// 会话回退节点：列表是只读的，回退是**破坏性**的（改工作区文件、可能删掉快照之后
		// 新建的文件），所以手机端必须先读列表、拿到节点 id，再按 id 回退 —— 与桌面端
		// 「回退到此处」同一条服务端路径，不另写一套判据。
		//
		// 列表带上限：节点本身只有 id/提示词/时间/状态，但一条会话最多保留 50 个节点，
		// 提示词可能很长（每条约 200 字），整帧要留在中继预算之内。
		"conversation.checkpoints": {
			Method:   http.MethodGet,
			Scope:    relayScopeConversation,
			Path:     "/checkpoints",
			MaxBytes: 64 << 10,
			Label:    "回退节点",
			Handle:   s.listConversationCheckpoints,
		},
		// 回退请求体与桌面端逐字同形（checkpointId / mode / fork），复用同一个 handler：
		// 手机端只发 mode，不发 fork（"从节点分支"是电脑端才有的动作）。
		"conversation.rewind": {
			Method: http.MethodPost,
			Scope:  relayScopeConversation,
			Path:   "/rewind",
			Handle: s.rewindConversation,
		},
	}
}
