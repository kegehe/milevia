package app

import (
	"context"
	"errors"
	"log"
	"time"
)

// remote_outbox 的投递窗口与容量上限。
//
// 背景（真机实测 2026-09-16）：这个表此前**没有任何上界**。事件洪峰期间它涨到
// 111 万行、单表约 1 GB（库文件 1.76 GB），而且因为云端对某几条永远存不进去的事件
// 既不回 ack 也不回 reject（见 cloud-control 的 storeEvent 错误分类），投递窗口的
// 队头再也没往前挪过 —— agent 于是每秒把同一批 100 条重推 5 次，一条 WebSocket
// 连接 4.5 小时被灌了 792 MB 下行，下行命令被挤到 19~75 秒才落地。
//
// 两道闸门分开管两件事：
//   - 投递窗口（remoteOutboxDeliveryWindow）管"还值不值得留"。手机端的事件是实时
//     增量，快照才是恢复路径（远程快照每次都会整份重传，见 remoteSnapshot）。
//     一小时以前的事件对手机没有任何价值，留着只是拖慢每一次读取。
//   - 容量上限（remoteOutboxMaxRows）管"最坏情况有多大"。取 5000 是为了和云端
//     每个实例保留的事件条数（cloudEventRetentionPerInstance）对齐：本地留得比
//     云端还多没有意义，多出来的部分投上去也会立刻被云端裁掉。
//
// ⚠️ 这两条闸门都在**清理**里生效，不在**读取**里。曾经试过给 readRemoteOutbox
// 加一个 `created_at >= 窗口` 的过滤条件，真机库（138 万行积压）实测把一次读取从
// 0.8ms 变成 2659ms：符合条件的行都在 agent_sequence 的末尾，扫描必须走完整张表
// 才能凑够 limit 的条数。所以"不投递过期事件"由删除保证 —— 删除是批量的、可以
// 让出连接，而过滤是每一次读取都要付的代价。
const (
	remoteOutboxDeliveryWindow = time.Hour
	remoteOutboxMaxRows        = 5000

	// remoteOutboxRetentionStartDelay 是启动后到第一次清理的静默期。
	//
	// 取 5 秒（而不是分钟级）：清理是**分块提交**的，块与块之间会放开那条唯一的
	// SQLite 连接，所以它和启动时的交互并不冲突；而拖得越久，积压就多投出去一批
	// 本该直接删掉的过期事件。
	remoteOutboxRetentionStartDelay = 5 * time.Second
	// remoteOutboxRetentionInterval 是之后两次清理的间隔。
	//
	// 取 5 分钟而不是更长：容量上限只在**清理那一刻**成立，两次清理之间表还会继续
	// 长。中继正常时泵每秒能推几百条、生产端只有几十条，表根本涨不起来；真正会涨是
	// "云端不确认"这种管道断掉的情形，那时上限就是这 5 分钟里能堆出来的量。清理本身
	// 很便宜（两次有上限的 delete），所以这个间隔几乎不花钱。
	remoteOutboxRetentionInterval = 5 * time.Minute
	// remoteOutboxPruneChunk 是单次事务最多删多少行。
	//
	// 必须分块：真机上第一次清理要删掉 112 万行，而整个服务只有**一条** SQLite
	// 连接 —— 一条 delete 删完，桌面端的所有请求就都排在它后面。分块之后每次提交
	// 之间连接会被放开，等待中的请求能插进来；总量还是一样会被清干净。
	remoteOutboxPruneChunk = 20000
)

// startRemoteOutboxRetentionLoop 起一个周期性的出队表清理循环，由
// StartBackgroundMaintenance 调用。没有它，投递不出去的事件会无限堆积。
func (s *Server) startRemoteOutboxRetentionLoop() {
	s.runWG.Add(1)
	go func() {
		defer s.runWG.Done()
		timer := time.NewTimer(remoteOutboxRetentionStartDelay)
		select {
		case <-s.runtimeCtx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		for {
			if err := s.pruneRemoteOutbox(s.runtimeCtx); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("[maintenance] prune remote outbox: %v", err)
			}
			ticker := time.NewTicker(remoteOutboxRetentionInterval)
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

// pruneRemoteOutbox 把出队表压回投递窗口与容量上限之内，返回删除的行数。
//
// 两步都在一个事务里：只按时间删的话，一次突发仍然能把表撑到任意大小（窗口是一小时，
// 但一小时够产生几十万条流式分片）；只按条数删的话，长期空闲时会留着早已过期的行。
func (s *Server) pruneRemoteOutbox(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	removed, err := s.pruneRemoteOutboxWithLimits(ctx, remoteOutboxDeliveryWindow, remoteOutboxMaxRows)
	if err != nil {
		return err
	}
	if removed > 0 {
		// 这一批删除常常是几十万行，值得把空洞还回去；复用事件裁剪那条有界回收路径。
		s.reclaimAfterPrune(ctx, removed)
	}
	return nil
}

// pruneRemoteOutboxWithLimits 是窗口与上限可注入的版本：常量是编译期常量，测试用
// 小数值走同一条路径，避免为了造几万行而拖慢用例。
//
// 内部分块提交，直到没有可删的为止（见 remoteOutboxPruneChunk）。
func (s *Server) pruneRemoteOutboxWithLimits(ctx context.Context, window time.Duration, maxRows int) (int64, error) {
	var total int64
	for {
		removed, err := s.pruneRemoteOutboxChunk(ctx, window, maxRows, remoteOutboxPruneChunk)
		if err != nil {
			return total, err
		}
		total += removed
		// 一块没删满就说明已经清干净了；顺带兜住"每块都能删满"的极端情况。
		if removed < int64(remoteOutboxPruneChunk) {
			return total, nil
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}

// pruneRemoteOutboxChunk 删**一块**，单独一个事务。
func (s *Server) pruneRemoteOutboxChunk(ctx context.Context, window time.Duration, maxRows, chunk int) (int64, error) {
	cutoff := time.Now().UTC().Add(-window)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var removed int64
	// 1) 过期：离开投递窗口的行不再投，也没有保留价值。按 agent_sequence 从小到大
	//    删，保证反复调用时推进方向一致。
	result, err := tx.ExecContext(ctx, `delete from remote_outbox where event_id in (
		select event_id from remote_outbox where created_at < ? order by agent_sequence limit ?
	)`, cutoff, chunk)
	if err != nil {
		return 0, err
	}
	if count, err := result.RowsAffected(); err == nil {
		removed += count
	}
	// 2) 容量：只按时间删挡不住一次突发（窗口内也能堆几十万条）。留下最新的
	//    maxRows 行，更老的直接删 —— 它们排在队尾，本来就轮不到投递。
	//
	// 用 `limit ? offset ?` 表达"第 maxRows 名之后的、最多 chunk 条"。
	// **不能**写成 `agent_sequence <= (select min(...) ... limit ?)`：表里行数不足
	// maxRows 时那个子查询会把最小的序号原样返回，于是每一轮都删掉最老的一行
	// （用例 TestPruneRemoteOutboxKeepsRowsInsideBothBounds 钉住这一点）。
	remaining := int64(chunk) - removed
	if maxRows > 0 && remaining > 0 {
		result, err = tx.ExecContext(ctx, `delete from remote_outbox where event_id in (
			select event_id from remote_outbox order by agent_sequence desc limit ? offset ?
		)`, remaining, maxRows)
		if err != nil {
			return 0, err
		}
		if count, err := result.RowsAffected(); err == nil {
			removed += count
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return removed, nil
}
