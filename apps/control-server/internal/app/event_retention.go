package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// events 表的每会话保留上限。
//
// 背景（真机实测）：`events` 此前没有任何保留期 —— 唯一的裁剪是"每个项目最多 100 个
// 会话"（maxConversationHistoryPerProject），靠 FK 级联删。于是单个会话能一路涨到
// 160,769 条事件 / 52.8 MB，全库 events payload 累积到约 320 MB（`user` 一个类型的
// payload 就有 137 MB）。库文件因此膨胀，连带拖慢所有要扫 events 的操作。
//
// 裁剪顺序刻意如此：**先裁过程事件，不够再裁内容事件**。流式分片（stream_event）、
// 工具进度（tool_progress）、系统事件（system）、用量刷新（usage.updated）都是"过程
// 数据"，对回放一段对话没有价值；user / assistant / result 才是会话内容。这样即使
// 触发裁剪，用户翻历史时看到的对话仍然是完整的。
const (
	// eventsProcessEventRetention 是每个会话保留的过程事件条数上限。
	eventsProcessEventRetention = 50000

	// eventsTotalRetention 是每个会话的事件总量兜底：过程事件裁完还超这么多，才动内容事件。
	eventsTotalRetention = 200000

	// eventsRetentionStartDelay 是启动后到第一次裁剪的静默期。
	eventsRetentionStartDelay = 3 * time.Minute

	// eventsRetentionInterval 是之后两次裁剪的间隔。
	eventsRetentionInterval = 30 * time.Minute

	// eventsReclaimMinPrunedRows 是触发一次增量回收的最小删除行数：删得少就不值得动。
	eventsReclaimMinPrunedRows = 1000

	// eventsReclaimPagesPerPass 是单次增量回收的页数上限，保证它是一笔小额、可预测的开销。
	eventsReclaimPagesPerPass = 10000
)

// eventsProcessEventTypes 是"过程数据"事件类型。裁剪时它们先被削到上限之内。
var eventsProcessEventTypes = []string{"stream_event", "tool_progress", "system", "usage.updated"}

// eventsProcessEventTypesSQL 是上面列表的 SQL 字面量。列表是编译期常量，不涉及注入。
func eventsProcessEventTypesSQL() string {
	quoted := make([]string, 0, len(eventsProcessEventTypes))
	for _, typ := range eventsProcessEventTypes {
		quoted = append(quoted, "'"+typ+"'")
	}
	return strings.Join(quoted, ",")
}

// conversationEventUsage 是一个会话的事件构成。
type conversationEventUsage struct {
	conversationID string
	total          int64
	process        int64
}

// startEventRetentionLoop 起一个周期性的会话事件裁剪循环。由 StartBackgroundMaintenance
// 调用（HTTP 监听可用之后），不会拖慢服务就绪。
func (s *Server) startEventRetentionLoop() {
	s.runWG.Add(1)
	go func() {
		defer s.runWG.Done()
		// 启动后先让开交互窗口：裁剪要扫 events 表，刚打开应用时不该抢那条唯一连接。
		timer := time.NewTimer(eventsRetentionStartDelay)
		select {
		case <-s.runtimeCtx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		for {
			removed, err := s.pruneConversationEvents(s.runtimeCtx)
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("[maintenance] prune conversation events: %v", err)
			}
			if removed > 0 {
				s.reclaimAfterPrune(s.runtimeCtx, removed)
			}
			ticker := time.NewTicker(eventsRetentionInterval)
			select {
			case <-s.runtimeCtx.Done():
				ticker.Stop()
				return
			case <-ticker.C:
				ticker.Stop()
			}
		}
	}()
}

// reclaimAfterPrune 在裁剪之后做一次**有界**的增量回收。
//
// 单靠启动时那一次 incremental_vacuum 不够：一次裁剪就能腾出上万页，而应用可能连续
// 运行很多天。auto_vacuum 已是 INCREMENTAL 时这只是一次小额、可预测的页归还；仍是 0
// （用户还没经历过一次完整整理）时什么都不做 —— 那种情况下这些空洞只能等下一次
// VACUUM，硬做 incremental_vacuum 也不会有效果。
func (s *Server) reclaimAfterPrune(ctx context.Context, removed int64) {
	if removed < eventsReclaimMinPrunedRows || ctx.Err() != nil {
		return
	}
	s.storageMu.Lock()
	defer s.storageMu.Unlock()
	if ctx.Err() != nil {
		return
	}
	var autoVacuum int64
	if err := s.db.QueryRowContext(ctx, `pragma auto_vacuum`).Scan(&autoVacuum); err != nil {
		log.Printf("[maintenance] read auto_vacuum before incremental reclaim: %v", err)
		return
	}
	if autoVacuum != 2 {
		return
	}
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`pragma incremental_vacuum(%d)`, eventsReclaimPagesPerPass)); err != nil {
		log.Printf("[maintenance] incremental reclaim after prune: %v", err)
	}
}

// pruneConversationEvents 把所有超过保留上限的会话裁回上限之内，返回删除的事件条数。
func (s *Server) pruneConversationEvents(ctx context.Context) (int64, error) {
	return s.pruneConversationEventsWithLimits(ctx, eventsProcessEventRetention, eventsTotalRetention)
}

// pruneConversationEventsWithLimits 是上限可注入的版本：上限是编译期常量，测试用小数
// 走同一条路径，避免为了造 5 万条事件而把用例拖慢。
//
// 先一次性查出超限会话再逐个处理：服务端 SQLite 池只开一条连接，边遍历结果集边写会死锁
// （同 cleanOrphanedConversationEvents 的约定）。
func (s *Server) pruneConversationEventsWithLimits(ctx context.Context, processLimit, totalLimit int64) (int64, error) {
	processTypes := eventsProcessEventTypesSQL()
	query := `select conversation_id, count(*) as total,
			sum(case when type in (` + processTypes + `) then 1 else 0 end) as process
		from events group by conversation_id
		having process > ? or total > ?`
	rows, err := s.db.QueryContext(ctx, query, processLimit, totalLimit)
	if err != nil {
		return 0, err
	}
	overLimit := []conversationEventUsage{}
	for rows.Next() {
		var usage conversationEventUsage
		if err := rows.Scan(&usage.conversationID, &usage.total, &usage.process); err != nil {
			rows.Close()
			return 0, err
		}
		overLimit = append(overLimit, usage)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	var removed int64
	for _, usage := range overLimit {
		count, err := s.pruneConversationEventUsage(ctx, usage, processLimit, totalLimit)
		removed += count
		if err != nil {
			return removed, err
		}
	}
	return removed, nil
}

// pruneConversationEventUsage 裁剪单个会话，返回删除条数。先削过程事件，总量仍超兜底
// 再按时间删最旧的。
//
// **不**排除 remote_outbox 引用着的事件：outbox 行在入队时就把 payload 抄了一份
// （enqueueRemoteEventTxWithConversation），中继侧只读 outbox、全仓没有任何地方 join
// 或按 id 回查 events。曾经加过这个排除项，结果在真机上挡掉了 5.9 万条本该裁掉的过程
// 事件（outbox 有近 6 万行未 ack），等于把保留策略废掉一大半。
func (s *Server) pruneConversationEventUsage(ctx context.Context, usage conversationEventUsage, processLimit, totalLimit int64) (int64, error) {
	var removed int64
	if usage.process > processLimit {
		result, err := s.db.ExecContext(ctx, `delete from events
			where conversation_id=? and type in (`+eventsProcessEventTypesSQL()+`)
				and id not in (
					select id from events where conversation_id=? and type in (`+eventsProcessEventTypesSQL()+`)
					order by created_at desc, id desc limit ?)`,
			usage.conversationID, usage.conversationID, processLimit)
		if err != nil {
			return removed, err
		}
		if affected, err := result.RowsAffected(); err == nil {
			removed += affected
		}
	}
	if usage.total > totalLimit {
		result, err := s.db.ExecContext(ctx, `delete from events
			where conversation_id=?
				and id not in (
					select id from events where conversation_id=?
					order by created_at desc, id desc limit ?)`,
			usage.conversationID, usage.conversationID, totalLimit)
		if err != nil {
			return removed, err
		}
		if affected, err := result.RowsAffected(); err == nil {
			removed += affected
		}
	}
	return removed, nil
}
