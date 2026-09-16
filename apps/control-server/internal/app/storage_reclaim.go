package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"
)

// 数据库空间回收。
//
// 背景（真机实测）：`auto_vacuum` 从没设过（默认 0），而会话删除 / 历史裁剪 / 孤儿事件
// 清扫都会靠 FK 级联一次删掉上百万人行事件。SQLite 在 auto_vacuum=0 时把腾出来的页
// 放进 freelist 但**永不归还文件系统** —— 用户库里 `page_count` 1,882,664 页而
// `freelist_count` 1,699,556 页，也就是 7.71 GB 的文件里约 6.9 GB 是空转的 freelist，
// 实际数据只有 ~800 MB。文件越大，备份 / 复制 / 杀软扫描 / 一次性 VACUUM 的代价越高。
//
// 处理方式：
//
//	首次（auto_vacuum 仍为 0 且 freelist 占比超阈值）→ 切到 INCREMENTAL 并 VACUUM，
//	一次性把积压的空洞还回去；
//	之后（auto_vacuum 已是 INCREMENTAL）→ 每次启动只做小额 incremental_vacuum，
//	让新产生的空洞逐步归还，不会再攒成几个 GB。
//
// 用 `pragma auto_vacuum` 自身作为"一次性动作是否做过"的状态，不再额外写 app_metadata：
// 数据本身就是标记，不会出现"标记写了但动作没成"的不一致。
const (
	// storageReclaimStartDelay 是启动后到开始回收的静默期：给用户留出启动后的交互窗口，
	// 别在刚打开应用时就把唯一那条 SQLite 连接占住。
	storageReclaimStartDelay = 90 * time.Second

	// storageReclaimIdlePoll 是有活跃运行时重新检查的间隔。
	storageReclaimIdlePoll = 30 * time.Second

	// storageReclaimMaxIdleWait 是等待空闲窗口的上限：一直有任务在跑就放弃这次回收，
	// 下次启动再说，绝不与正在进行的运行抢连接。
	storageReclaimMaxIdleWait = 30 * time.Minute

	// storageReclaimMinFreelistRatio 是触发一次性 VACUUM 的 freelist 占比下限。
	// 低于它就认为没有值得回收的空间（VACUUM 本身也要重写整个库）。
	storageReclaimMinFreelistRatio = 0.25
)

// waitForIdleWindow 先等 delay，再等到没有排队/运行中的 run 为止；预算耗尽返回错误。
// 由调用方决定超时后的动作（当前是放弃这次回收）。
func (s *Server) waitForIdleWindow(ctx context.Context, delay time.Duration) error {
	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	deadline := time.Now().Add(storageReclaimMaxIdleWait)
	for {
		active, err := s.activeRunCount(ctx)
		if err != nil {
			return err
		}
		if active == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("storage reclaim skipped: runs stayed active")
		}
		ticker := time.NewTicker(storageReclaimIdlePoll)
		select {
		case <-ctx.Done():
			ticker.Stop()
			return ctx.Err()
		case <-ticker.C:
			ticker.Stop()
		}
	}
}

// activeRunCount 返回排队中/运行中的 run 数（与 cleanupStorage 的前置判断同口径）。
func (s *Server) activeRunCount(ctx context.Context) (int64, error) {
	var active int64
	if err := s.db.QueryRowContext(ctx, `select count(*) from runs where status in ('queued','running')`).Scan(&active); err != nil {
		return 0, fmt.Errorf("count active runs: %w", err)
	}
	return active, nil
}

// ensureStorageReclaimed 回收 SQLite freelist 占用的文件空间。见文件头注释。
//
// 持 storageMu：与设置页的「清理数据」（cleanupStorage）互斥，避免两个 VACUUM 撞车。
func (s *Server) ensureStorageReclaimed(ctx context.Context) error {
	s.storageMu.Lock()
	defer s.storageMu.Unlock()
	return s.reclaimFreePagesLocked(ctx, false)
}

// reclaimFreePagesLocked 执行空间回收。**调用方必须已持 storageMu。**
//
// force 为真表示调用方（用户点的清理）已经明确要求释放空间，不再看 freelist 阈值；
// 为假表示这是启动期的机会性回收，占比不到阈值就直接跳过，免得白重写一遍库。
func (s *Server) reclaimFreePagesLocked(ctx context.Context, force bool) error {
	var pageSize, pageCount, freelist, autoVacuum int64
	if err := s.db.QueryRowContext(ctx, `pragma page_size`).Scan(&pageSize); err != nil {
		return fmt.Errorf("read page_size: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `pragma page_count`).Scan(&pageCount); err != nil {
		return fmt.Errorf("read page_count: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `pragma freelist_count`).Scan(&freelist); err != nil {
		return fmt.Errorf("read freelist_count: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `pragma auto_vacuum`).Scan(&autoVacuum); err != nil {
		return fmt.Errorf("read auto_vacuum: %w", err)
	}
	if freelist <= 0 {
		return nil
	}

	// 已经切到 INCREMENTAL：把 free pages 一次还回去即可，快且不重写整库。
	if autoVacuum == 2 {
		if _, err := s.db.ExecContext(ctx, `pragma incremental_vacuum`); err != nil {
			return fmt.Errorf("incremental_vacuum: %w", err)
		}
		return nil
	}

	if !force && (pageCount == 0 || float64(freelist)/float64(pageCount) < storageReclaimMinFreelistRatio) {
		return nil
	}

	log.Printf("[maintenance] reclaiming %d free pages (~%d MB) — this rewrites the database and blocks other queries until it finishes",
		freelist, freelist*pageSize/(1<<20))
	s.storageReclaiming.Store(true)
	defer s.storageReclaiming.Store(false)

	// 顺序不能反：auto_vacuum 只有在随后的 VACUUM 里才会真正写进库头（并为之后
	// 的 incremental_vacuum 建立 pointer map）。中途失败则不生效，下次启动重试。
	if _, err := s.db.ExecContext(ctx, `pragma auto_vacuum=incremental`); err != nil {
		return fmt.Errorf("enable incremental auto_vacuum: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `vacuum`); err != nil {
		return fmt.Errorf("vacuum: %w", err)
	}
	// VACUUM 之后 WAL 里可能积着一份完整的重写结果；截断它，别让刚腾出来的空间
	// 立刻以 -wal 的形式长回去。尽力而为，失败不影响回收本身。
	var busy, walPages, checkpointed int64
	if err := s.db.QueryRowContext(ctx, `pragma wal_checkpoint(truncate)`).Scan(&busy, &walPages, &checkpointed); err != nil {
		log.Printf("[maintenance] truncate WAL after reclaim: %v", err)
	}
	if err := s.db.QueryRowContext(ctx, `pragma auto_vacuum`).Scan(&autoVacuum); err != nil {
		return fmt.Errorf("verify auto_vacuum: %w", err)
	}
	log.Printf("[maintenance] storage reclaim finished (auto_vacuum=%d)", autoVacuum)
	return nil
}
