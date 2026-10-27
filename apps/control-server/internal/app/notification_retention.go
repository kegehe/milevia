package app

import (
	"context"
	"errors"
	"log"
	"time"
)

const (
	// 保留期均按创建时间计算；已读行短期保留，继续用于通知去重。
	notificationReadRetention   = 7 * 24 * time.Hour
	notificationUnreadRetention = 30 * 24 * time.Hour
	notificationMaxRows         = 1000

	notificationRetentionStartDelay = 5 * time.Second
	notificationRetentionInterval   = 5 * time.Minute
	notificationPruneChunk          = 500
)

// startNotificationRetentionLoop 在监听就绪后清理，避免历史积压拖慢服务启动。
// 每块单独提交，向共用的 SQLite 连接让出执行机会；停机时取消并等待退出。
func (s *Server) startNotificationRetentionLoop() {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return
	}
	s.runWG.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.runWG.Done()
		timer := time.NewTimer(notificationRetentionStartDelay)
		defer timer.Stop()
		for {
			select {
			case <-s.runtimeCtx.Done():
				return
			case <-timer.C:
			}
			ctx, cancel := context.WithTimeout(s.runtimeCtx, 30*time.Second)
			if _, err := s.pruneNotifications(ctx, time.Now().UTC()); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("[maintenance] prune notifications: %v", err)
			}
			cancel()
			timer.Reset(notificationRetentionInterval)
		}
	}()
}

// pruneNotifications 先删过期行，再按创建时间和 ID 保留最新的 notificationMaxRows 行。
func (s *Server) pruneNotifications(ctx context.Context, now time.Time) (total int64, err error) {
	defer func() {
		// 即使后续块失败，已经提交的删除也必须同步给客户端。
		if total > 0 {
			s.broadcastStateEvent(stEvNotifications, "")
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		removed, err := s.pruneNotificationsChunk(ctx, now)
		if err != nil {
			return total, err
		}
		total += removed
		if removed < notificationPruneChunk {
			return total, nil
		}
	}
}

func (s *Server) pruneNotificationsChunk(ctx context.Context, now time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var removed int64
	// 分开两种状态，使各自的 (dismissed,created_at,id) 索引都可用于定位过期行。
	for _, policy := range []struct {
		dismissed int
		retention time.Duration
	}{{1, notificationReadRetention}, {0, notificationUnreadRetention}} {
		result, err := tx.ExecContext(ctx, `delete from notifications where id in (
			select id from notifications where dismissed=? and created_at<?
			order by created_at,id limit ?
		)`, policy.dismissed, now.UTC().Add(-policy.retention), int64(notificationPruneChunk)-removed)
		if err != nil {
			return 0, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		removed += count
	}
	if removed < notificationPruneChunk {
		result, err := tx.ExecContext(ctx, `delete from notifications where id in (
			select id from notifications order by created_at desc,id desc limit ? offset ?
		)`, int64(notificationPruneChunk)-removed, notificationMaxRows)
		if err != nil {
			return 0, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		removed += count
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return removed, nil
}
