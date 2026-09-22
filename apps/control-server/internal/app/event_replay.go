package app

import "strings"

// 会话历史分页只走"可回放事件"：时间线上真的会被渲染出来的那些。
//
// 背景（真机实测，2026-09-16 的 1.32GB 库）：全库 105.8 万条 events 里可回放的只有 3.15 万条。
// 而 messages 全库最多的一个会话也只有 284 条。getConversation 把 messages 和 events 放在
// 同一个 limit 下、用同一个"还有没有更多"判断，于是 hasMore 几乎恒为真：
//
//   - 界面上"加载更早记录"按钮永远挂着，哪怕首屏就已经把全部消息都取回来了；
//   - 点一次只是把 400 条渲染不出来的遥测追加进内存，画面毫无变化；
//   - 真机上同一个会话要翻 127~204 次（累计 2~7 秒）才能把按钮点掉；
//   - 自动编排页的"完整历史"更狠：它按 hasMore 循环翻页，一个这样的会话要发上百次
//     limit=1000 的请求（OrchestrationPage 的 do/while）。
//
// 刻意用黑名单而不是白名单：将来 CLI 新增的事件类型会**默认保留**。宁可多带回一条，
// 也不能让某个新事件在回放里凭空消失。
//
// 排除项分三处，各管一段：整类排除的见 eventsReplayExcludedTypes；混在 system 里的空转子
// 类型由 unrenderedSystemSubtypePredicate 判定；历史遗留的 thinking_tokens 由
// thinkingTokensPredicate 判定。**后两者都要读 payload，是这条谓词唯一不能只看索引的地方**：
// 真机上遥测堆得最厚的几个会话首屏要 40~160ms（其余个位数毫秒），因为最新的可回放事件压在
// 几万条遥测下面，必须扫过去才能凑够一页。这是一次性的打开开销，换来的是不用再翻上百次页；
// 随保留循环裁剪、或用户执行一次设置页的"清理数据库"，这个数字会回落。
func replayableEventsPredicate(alias string) string {
	if alias == "" {
		alias = "events"
	}
	quoted := make([]string, 0, len(eventsReplayExcludedTypes))
	for _, typ := range eventsReplayExcludedTypes {
		quoted = append(quoted, "'"+typ+"'")
	}
	return "lower(" + alias + ".type) not in (" + strings.Join(quoted, ",") + ") and not " +
		unrenderedSystemSubtypePredicate(alias) + " and not " + thinkingTokensPredicate(alias)
}

// unrenderedSystemSubtypePredicate 排除"type=system、但客户端一条都渲染不出来"的子类型。
//
// 上一条谓词排除的是**整类**遥测（流式分片、工具心跳、用量刷新），这一条管的是混在
// system 里的那几种空转：Claude CLI 在等模型响应时每几秒发一条
// {"status":"requesting","subtype":"status"}，外加 init、task_progress、以及没有
// is_backgrounded 的 task_updated。判定依据就是客户端自己的渲染规则
// apps/web/src/lib/timeline.ts 的 systemItemFromEvent —— 这里每一条都对应那边的
// 一个 return null。**改这里的排除项时要和那边一起看。**
//
// 两个读者共用同一条谓词，因为它们问的是同一个问题"这条事件到得了屏幕吗"，
// 而答错的代价都不是"多占点带宽"而是"界面上凭空少一段"：
//   - 会话历史分页（replayableEventsPredicate）：窗口是按条数算的，一条空转就顶掉一条真内容。
//     真机实测（2026-09-16 的库，单个 5 万条事件的活跃会话）：整类遥测排掉之后，400 条的
//     首屏里仍有 83 条是这类心跳、只能渲染出 317 项；把子类型也排掉，同一页 400 项全是真内容。
//   - 手机端快照的运行记录（remoteNoticeReplayPredicate）：窗口只有 24 格，心跳占满之后
//     一张"后台任务启动"卡片会在 2~4 分钟内被挤出窗口 —— 手机上看得到，退出再进来就没了。
//     实测那 24 格里只有 4~10 格是真卡片，过滤后 24 格全是。
//
// 黑名单而非白名单，理由同上：CLI 将来新增的 subtype **默认保留**。宁可多带回一条，
// 也不能让某种新卡片整个消失。
//
// 返回的表达式**自带一对括号**：调用方一律写成 `... and not <谓词>`，而 SQL 里 NOT 比 AND
// 结合得紧 —— 少了这层括号，"不是 system 且（子类型命中）"会被解析成
// "（不是 system）且（子类型命中）"，于是**每一条**事件都被排除掉（实测踩过，整页返回空）。
// TestReplayPredicatesKeepWhatTheClientRenders 按调用方的极性钉住了这一点。
func unrenderedSystemSubtypePredicate(alias string) string {
	if alias == "" {
		alias = "events"
	}
	// 先去掉空白再匹配："subtype": "status" 与 "subtype":"status" 都要认——事件 payload
	// 是 CLI 原样写进来的，写法不受我们控制。匹配的都是结构字段，值里不含空格。
	payload := "lower(replace(" + alias + ".payload,' ',''))"
	// status 事件只有两种会渲染：真在压缩上下文（compacting），或带压缩结果。
	//
	// 这里两个 LIKE 都**不能**以引号收尾：SQLite 的 LIKE 是整串锚定的，模式末尾写 `"` 就
	// 要求 payload 以引号结束 —— 而它结束于 `}`，于是 '"compact_result":"success"' 这种
	// 明明存在的字段反而匹配不上（实测踩过）。模式一律以 `%` 收尾。
	//
	// '"compact_result":""' 还要单独排掉：% 能匹配零个字符，空串也会命中上面那条，
	// 而客户端对空串是当"没有"处理的（timeline.ts 的 compactResult 判定）。
	compactRenders := `(` + payload + ` like '%"compact_result":"%' and ` + payload + ` not like '%"compact_result":""%')`
	conditions := []string{
		payload + ` like '%"subtype":"status"%' and ` + payload + ` not like '%"status":"compacting"%' and not ` + compactRenders,
		payload + ` like '%"subtype":"init"%'`,
		payload + ` like '%"subtype":"task_progress"%'`,
		// task_updated 只有"任务转入后台"那一种 patch 会渲染，其余是无声的状态推进。
		payload + ` like '%"subtype":"task_updated"%' and ` + payload + ` not like '%"is_backgrounded":true%'`,
	}
	return "(" + alias + ".type='system' and (" + strings.Join(conditions, " or ") + "))"
}

// eventsReplayExcludedTypes 是**整类**排除出会话历史分页的事件类型。
//
// 刻意与 event_retention.go 的 eventsProcessEventTypes 分开列，不从那一个派生：两个列表
// 回答的不是同一个问题 —— 那个决定"裁剪时先删谁"，这个决定"哪些根本不往客户端带"。保留
// 策略把 system 当过程数据、触发裁剪时优先削它；但分页**必须**留着 system，它里面有上下文
// 压缩、API 重试、后台任务这些真的会渲染成卡片的子类型。用一句 `if typ == "system" {
// continue }` 把两者缝起来，只会让"往保留列表里加个类型"这种改动悄悄把该类型从历史里抹掉。
//
// 各类型被排除的依据：
//   - stream_event：Claude CLI 的流式分片，客户端全仓没有一处读它；
//   - tool_progress：长时间运行工具的心跳，systemItemFromEvent 明确不渲染（会刷屏）；
//   - usage.updated：只有实时 WebSocket 分支拿它触发一次用量刷新，而用量面板走的是
//     /api/conversations/{id}/usage（读 runs / run_usage 表），历史里的这些事件没有消费者。
//
// 这里只管**整类**。同一事件类型里"部分子类型不渲染"的（system 下的 status 心跳 / init /
// task_progress 等）留给 unrenderedSystemSubtypePredicate —— 它们的取舍与这里不同：整类
// 类型是"这条路上根本没人读"，子类型是"读了也画不出来"，后者一旦判错会把同一类型里真正
// 该显示的卡片一起抹掉，所以只能靠 payload 逐条判，不能在这里顺手加一行。
var eventsReplayExcludedTypes = []string{"stream_event", "tool_progress", "usage.updated"}
