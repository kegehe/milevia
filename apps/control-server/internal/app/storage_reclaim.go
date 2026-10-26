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
//	之后（auto_vacuum 已是 INCREMENTAL）→ 每次启动先做 incremental_vacuum，
//	让新产生的空洞逐步归还；**收不动就退回整库 VACUUM**。
//
// 最后那句是修出来的，不是设计出来的：incremental_vacuum 只在文件尾部是空闲页时才截断
// 文件，遇到"占着 2 GB freelist 但一页都放不掉"的库（真机见过：631,934 页里 518,071 页
// 空闲、调用 0.0 秒只少 1 页）时，旧实现照样报成功，库就永远卡在 2.59 GB，每次启动都
// 白跑一遍"回收"。所以这里改成收完复查 freelist，残留占比仍超阈值就整库 VACUUM ——
// 那也是唯一能重建 pointer map、让后续 incremental_vacuum 恢复有效的动作。
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

	// storageReclaimMinForcedVacuumBytes 是用户点「清理」时仍要求的最小可回收空间：
	// 至少 32 MiB，且随库大小放大到库的 1%。force 不看占比阈值，但不能为一个大库里的
	// 零星空洞重写整个库（真机 2.6 GB 库只残留 ~3 MB 的情况）。
	storageReclaimMinForcedVacuumBytes = 32 << 20
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
			return errors.New("idle window timed out: runs stayed active")
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

// needsFullVacuum 判断 freelist 里积压的空洞还值不值得用一次整库 VACUUM 去换。
//
// 判据只看占比：VACUUM 要重写整库、还要一份同等量级的空闲磁盘（真机 2.5 GB 实测 5.5 秒，
// 机械盘或杀软实时扫描下会慢一个数量级），为几百页的空洞动它不划算。
// force 为真（用户点的「清理」）时不看占比阈值，但仍要求可回收空间值得重写：
// 至少 32 MiB，且不低于库大小的 1%（大库里的零星空洞不该触发整库重写）。
func needsFullVacuum(force bool, pageSize, pageCount, freelist int64) bool {
	if freelist <= 0 {
		return false
	}
	if force {
		// force 是"比自动回收更积极"，不是"更保守"：可回收空间够大就做；
		// 小库的高占比空洞仍然值得（重写成本也小），大库的零星残留则跳过。
		// 绝对下限随库大小放大到 1%，避免"20 GB 库为 40 MB 空洞整库重写"。
		minBytes := int64(storageReclaimMinForcedVacuumBytes)
		if pageCount > 0 {
			if scaled := pageCount * pageSize / 100; scaled > minBytes {
				minBytes = scaled
			}
		}
		if freelist*pageSize >= minBytes {
			return true
		}
		if pageCount <= 0 {
			return false
		}
		return float64(freelist)/float64(pageCount) >= storageReclaimMinFreelistRatio
	}
	if pageCount <= 0 {
		return false
	}
	return float64(freelist)/float64(pageCount) >= storageReclaimMinFreelistRatio
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
// force 为真表示调用方（用户点的清理）已经明确要求释放空间，不再看 freelist 占比
// 阈值；但残留小于绝对下限时仍跳过 —— 为几 MB 空洞重写整个库不划算。
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

	// 已经切到 INCREMENTAL：先走便宜路径，把 free pages 一次还回去。
	if autoVacuum == 2 {
		if _, err := s.db.ExecContext(ctx, `pragma incremental_vacuum`); err != nil {
			return fmt.Errorf("incremental_vacuum: %w", err)
		}
		// 但不能就此认定"收完了"：incremental_vacuum 只在**文件尾部**恰好是空闲页时才
		// 截断文件。真机实测出现过 auto_vacuum 已是 INCREMENTAL、freelist 仍压着 2 GB
		// （518,071 页）的一次 —— 这一句 0.0 秒返回、只放掉 1 页，文件照旧 2.59 GB，
		// 于是每次启动都"回收成功"，而库永远不缩小。所以复查一次（页数也要重读：真
		// 收缩过的话 page_count 已经变了）：残留占比仍然超标就退回下面的整库 VACUUM。
		if err := s.db.QueryRowContext(ctx, `pragma page_count`).Scan(&pageCount); err != nil {
			return fmt.Errorf("read page_count after incremental_vacuum: %w", err)
		}
		if err := s.db.QueryRowContext(ctx, `pragma freelist_count`).Scan(&freelist); err != nil {
			return fmt.Errorf("read freelist after incremental_vacuum: %w", err)
		}
		if !needsFullVacuum(force, pageSize, pageCount, freelist) {
			return nil
		}
		log.Printf("[maintenance] incremental_vacuum left %d free pages (~%d MB); falling back to a full VACUUM",
			freelist, freelist*pageSize/(1<<20))
	} else if !needsFullVacuum(force, pageSize, pageCount, freelist) {
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
