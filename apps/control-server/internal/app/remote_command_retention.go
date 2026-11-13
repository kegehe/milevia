package app

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"
)

// processed_remote_commands 是手机下发命令的本地幂等台账。它的两列会一直长：
// request_payload 是手机请求正文的完整副本（单条上限 256 KiB），result 是执行回执。
//
// 背景：这张表此前**没有任何清理**，每条消息 / 任务创建 / 任务修改都会把自己的正文
// 永久复制一份。手机长期使用后表只会越来越大，而命令一旦到达终态，正文就不再有读者：
//   - 幂等判据只看 request_hash（不读正文），见 enqueueRemoteCommand；
//   - 排队执行只读非终态行的 request_payload（processOneRemoteCommand）；
//   - GET /api/remote/commands/{id} 的唯一调用方是 Agent 的 monitorCommand，它只取
//     status/result，命令进入终态后就不再轮询；
//   - result 在终态后还要继续留着，Agent 重启时云端会重投 received/executing 的命令，
//     本地靠幂等记录把终态回执再发一次。
//
// 所以分两步收口，而不是一刀切删行：
//  1. 终态命令过了宽限期就清空 request_payload（置 '{}'）。正文占空间的大头在这里，
//     行本身与 request_hash 留着，幂等与状态查询完全不受影响。
//  2. 终态行本身在保留期之后删除，并用条数上限兜住突发。
//
// 保留期必须覆盖手机端可能重试的幂等窗口。手机端每条命令的幂等键是每次操作新生成的
// UUID，轮询不超过 30 秒；云端把滞留的 received/executing 在 6 小时后判为
// indeterminate。取 7 天远大于这两者。即便云端在保留期之后还重投一条旧命令，它的
// expiresAt 仍是原始值（最多创建后 15 分钟），enqueueRemoteCommand 会直接以
// "expiresAt must be within the next 15 minutes" 拒绝 —— 不会出现删除台账后
// 旧命令被重新执行的路径。
const (
	// remoteCommandPayloadGrace 是命令进入终态后仍保留完整请求正文的时长。
	// 留一点窗口是为了线上排查时还能看到"当时到底发了什么"。
	remoteCommandPayloadGrace = time.Hour

	// remoteCommandRetention 是终态行的保留期（按终态更新时刻计算）。
	remoteCommandRetention = 7 * 24 * time.Hour

	// remoteCommandMaxRows 是终态行的条数上限。按时间删挡不住一次突发，
	// 条数上限保证两张闸门之间表的大小始终有界。非终态行永远不参与这条上限。
	remoteCommandMaxRows = 10000

	// remoteCommandRetentionStartDelay 是启动后到第一次清理的静默期：刚打开应用时
	// 不要抢那条唯一的 SQLite 连接。
	remoteCommandRetentionStartDelay = 5 * time.Second

	// remoteCommandRetentionInterval 是之后两次清理的间隔。清理本身是两条有上限的
	// 语句，几乎不花钱，所以取得和出队表清理一样短。
	remoteCommandRetentionInterval = 5 * time.Minute

	// remoteCommandPruneChunk 是单条语句最多处理多少行。服务端只有一条 SQLite
	// 连接，长事务会让桌面端的所有请求排在后面；分块提交让等待中的请求能插进来。
	remoteCommandPruneChunk = 500
)

// remoteCommandTerminalStatuses 是命令的终态。只有这些行会被清正文或删除；
// queued/received/executing 永远原样保留，它们是幂等窗口与执行队列本身。
var remoteCommandTerminalStatuses = []string{"completed", "failed", "expired", "cancelled", "indeterminate"}

// remoteCommandTerminalStatusesSQL 是上面列表的 SQL 字面量。列表是编译期常量，
// 不涉及注入。
func remoteCommandTerminalStatusesSQL() string {
	quoted := make([]string, 0, len(remoteCommandTerminalStatuses))
	for _, status := range remoteCommandTerminalStatuses {
		quoted = append(quoted, "'"+status+"'")
	}
	return strings.Join(quoted, ",")
}

// startRemoteCommandRetentionLoop 起一个周期性的远程命令台账清理循环，由
// StartBackgroundMaintenance 调用。没有它，手机下发的每条命令正文都会永久占位。
func (s *Server) startRemoteCommandRetentionLoop() {
	s.runWG.Add(1)
	go func() {
		defer s.runWG.Done()
		timer := time.NewTimer(remoteCommandRetentionStartDelay)
		defer timer.Stop()
		for {
			select {
			case <-s.runtimeCtx.Done():
				return
			case <-timer.C:
			}
			ctx, cancel := context.WithTimeout(s.runtimeCtx, 30*time.Second)
			scrubbed, removed, err := s.pruneRemoteCommands(ctx, time.Now().UTC())
			cancel()
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("[maintenance] prune remote commands: %v", err)
			}
			if removed > 0 {
				// 删行会留下空洞；复用事件裁剪那条有界回收路径。清正文只缩小行，
				// 不改变行数，所以不在这里触发回收。
				reclaimCtx, reclaimCancel := context.WithTimeout(s.runtimeCtx, 2*time.Minute)
				s.reclaimAfterPrune(reclaimCtx, removed)
				reclaimCancel()
			}
			if scrubbed > 0 || removed > 0 {
				log.Printf("[maintenance] remote command retention scrubbed=%d removed=%d", scrubbed, removed)
			}
			timer.Reset(remoteCommandRetentionInterval)
		}
	}()
}

// pruneRemoteCommands 把远程命令台账压回保留期与条数上限之内，返回清空正文的行数与
// 删除的行数。
func (s *Server) pruneRemoteCommands(ctx context.Context, now time.Time) (int64, int64, error) {
	return s.pruneRemoteCommandsWithLimits(ctx, now, remoteCommandPayloadGrace, remoteCommandRetention, remoteCommandMaxRows)
}

// pruneRemoteCommandsWithLimits 是阈值可注入的版本：常量是编译期常量，测试用小数值
// 走同一条路径，避免为了造上万行而拖慢用例。
//
// 内部分块提交，直到三种操作都不再删满一块为止。
func (s *Server) pruneRemoteCommandsWithLimits(ctx context.Context, now time.Time, grace, retention time.Duration, maxRows int64) (int64, int64, error) {
	var scrubbed, removed int64
	for {
		chunkScrubbed, chunkRemoved, err := s.pruneRemoteCommandsChunk(ctx, now, grace, retention, maxRows)
		if err != nil {
			return scrubbed, removed, err
		}
		scrubbed += chunkScrubbed
		removed += chunkRemoved
		if chunkScrubbed < remoteCommandPruneChunk && chunkRemoved < remoteCommandPruneChunk {
			return scrubbed, removed, nil
		}
		if err := ctx.Err(); err != nil {
			return scrubbed, removed, err
		}
	}
}

// pruneRemoteCommandsChunk 处理一块，单独一个事务。
func (s *Server) pruneRemoteCommandsChunk(ctx context.Context, now time.Time, grace, retention time.Duration, maxRows int64) (int64, int64, error) {
	terminal := remoteCommandTerminalStatusesSQL()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	var scrubbed, removed int64
	// 1) 超过保留期的终态行：整行删除（正文在第 3 步之前就已经清空或一起离开）。
	result, err := tx.ExecContext(ctx, `delete from processed_remote_commands where command_id in (
		select command_id from processed_remote_commands
		where status in (`+terminal+`) and updated_at<?
		order by updated_at limit ?
	)`, now.UTC().Add(-retention), int64(remoteCommandPruneChunk))
	if err != nil {
		return 0, 0, err
	}
	if count, err := result.RowsAffected(); err == nil {
		removed += count
	}
	// 2) 条数上限：只留最新的 maxRows 条终态行，更老的直接删。用
	//    `limit ? offset ?` 表达"第 maxRows 名之后的最多一块"，理由同
	//    remote_outbox_retention.go：行数不足时子查询 min(...) 会误删最新一行。
	//
	//    ⚠️ 只删 expires_at 已过的行。云端在 Agent 重连时会重投 received/executing
	//    的命令，本地靠幂等记录把终态回执再发一次；如果这行在 expiresAt 之前就被删掉，
	//    重投会以"新命令"插入并**重新执行**。而 expiresAt 一过，enqueueRemoteCommand
	//    就会先以过期拒绝，重投再也进不了执行路径。时间保留（7 天）天然满足这一点，
	//    这条上限是唯一可能在 15 分钟内删行的路径，所以必须显式带上。
	//
	//    比较用 julianday() 而不是文本：expires_at 在旧版本里是按云端传来的时区偏移
	//    原样入库的（驱动按值自带的 Location 格式化），而这里绑定的是 UTC。纯文本比较
	//    在负偏移（如 -05:00）下会把还没到期的行判成已过期，正好打破上面那条约束。
	//    julianday() 会解析偏移与小数秒，按真实时刻比较；无法解析的值返回 NULL，
	//    比较结果为假 —— 宁可晚删，不会早删。
	if maxRows > 0 {
		remaining := int64(remoteCommandPruneChunk) - removed
		if remaining > 0 {
			result, err = tx.ExecContext(ctx, `delete from processed_remote_commands
				where julianday(expires_at)<=julianday(?) and command_id in (
					select command_id from processed_remote_commands
					where status in (`+terminal+`) order by updated_at desc limit ? offset ?
				)`, now.UTC(), remaining, maxRows)
			if err != nil {
				return 0, 0, err
			}
			if count, err := result.RowsAffected(); err == nil {
				removed += count
			}
		}
	}
	// 3) 终态行过了宽限期就不再需要请求正文。只改最大的那一列，行、request_hash
	//    与 result 都留着 —— 幂等判据不读正文，终态后也没有读者会回读它。
	result, err = tx.ExecContext(ctx, `update processed_remote_commands set request_payload='{}'
		where command_id in (
			select command_id from processed_remote_commands
			where status in (`+terminal+`) and request_payload<>'{}' and updated_at<?
			order by updated_at limit ?
		)`, now.UTC().Add(-grace), int64(remoteCommandPruneChunk))
	if err != nil {
		return 0, 0, err
	}
	if count, err := result.RowsAffected(); err == nil {
		scrubbed += count
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return scrubbed, removed, nil
}
