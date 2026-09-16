package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// 存储用量统计与清理。
//
// 这里的每个查询都跑在唯一那条 SQLite 连接上（app.go 的 SetMaxOpenConns(1)），所以
// "扫全表"不只是慢，而是会把**所有**并发的数据库接口一起卡住。旧实现一次
// /api/system/storage 要跑「1 条三前导通配 like 的全表扫描 + 6 条 count(*) + 4 条
// 全表 sum(length())」，而 cleanupStorage 还持着 storageMu 把它跑两遍（before、after，
// 中间夹一次 VACUUM）。本文件现在的口径：
//
//   - 廉价的照常精确算：文件大小、page/freelist 统计、各表行数；
//   - 昂贵的（各表负载字节数）改为**采样估算**，用最近 N 行的平均行长乘总行数；
//   - 只有遗留的 thinking_tokens 统计仍旧是全表扫，挪到"清理"里按需做一次。
const (
	// storageMeasureTimeout 是一次用量统计的整体上限：超时就返回已测得的部分，
	// 宁可数字不全，也不能把那条唯一连接占住。
	storageMeasureTimeout = 8 * time.Second

	// storagePayloadSampleSize 是负载字节估算的采样行数。
	storagePayloadSampleSize = 2000

	// storageCleanupTimeout 是清理动作（删除 + 空间回收）的上限。它用 runtimeCtx 派生，
	// **不绑请求 ctx**：客户端在 15s/120s 超时后 abort 不能把 VACUUM 掐断 —— 半途取消的
	// VACUUM 白跑一趟，用户还以为清了。同 deleteConversation 的处理方式（app.go）。
	storageCleanupTimeout = 30 * time.Minute

	// thinkingTokenDeleteBatchSize 是清理遗留 thinking_tokens 时单批删除的行数。
	thinkingTokenDeleteBatchSize = 20000
)

type storageTableUsage struct {
	Name         string `json:"name"`
	Rows         int64  `json:"rows"`
	PayloadBytes int64  `json:"payloadBytes,omitempty"`
	// PayloadEstimated 为真表示 PayloadBytes 是采样估算值，不是精确值。
	PayloadEstimated bool `json:"payloadEstimated,omitempty"`
}

type storageUsageResponse struct {
	DatabaseBytes   int64               `json:"databaseBytes"`
	WalBytes        int64               `json:"walBytes"`
	ShmBytes        int64               `json:"shmBytes"`
	PageSize        int64               `json:"pageSize"`
	PageCount       int64               `json:"pageCount"`
	FreelistPages   int64               `json:"freelistPages"`
	FreelistBytes   int64               `json:"freelistBytes"`
	Tables          []storageTableUsage `json:"tables"`
	MeasuredAt      time.Time           `json:"measuredAt"`
	Reclaiming      bool                `json:"reclaiming"`
	MeasurementDone bool                `json:"measurementComplete"`
}

// storageCleanupResult 是清理接口的返回值。thinking_tokens 的精确条数只有在这里才统计
// —— 它是历史遗留数据（appendEvent 早就不再持久化这类事件了），日常面板没必要为它扫全表。
type storageCleanupResult struct {
	Before              storageUsageResponse `json:"before"`
	After               storageUsageResponse `json:"after"`
	FreedBytes          int64                `json:"freedBytes"`
	ThinkingTokenEvents int64                `json:"thinkingTokenEvents"`
	ThinkingTokenBytes  int64                `json:"thinkingTokenBytes"`
}

func isThinkingTokensEvent(eventType string, payload []byte) bool {
	if strings.Contains(strings.ToLower(eventType), "thinking_tokens") || strings.Contains(strings.ToLower(eventType), "thinking.tokens") {
		return true
	}
	if !strings.EqualFold(eventType, "system") {
		return false
	}
	var envelope struct {
		Subtype string `json:"subtype"`
		Type    string `json:"type"`
	}
	if json.Unmarshal(payload, &envelope) != nil {
		return false
	}
	return strings.EqualFold(envelope.Subtype, "thinking_tokens") || strings.EqualFold(envelope.Type, "thinking_tokens")
}

// thinkingTokensPredicate is shared by storage reporting and cleanup. The
// event type varies across agent versions; some emit system events with a
// thinking_tokens subtype, while others include the marker in the type name.
func thinkingTokensPredicate(alias string) string {
	if alias == "" {
		alias = "events"
	}
	return "(lower(" + alias + ".type) like '%thinking_tokens%' or lower(" + alias + ".type) like '%thinking.tokens%' or (lower(" + alias + ".type)='system' and lower(" + alias + ".payload) like '%thinking_tokens%'))"
}

func (s *Server) getStorageUsage(w http.ResponseWriter, r *http.Request) {
	usage, err := s.measureStorage(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, usage)
}

// measureStorage 统计数据库占用。整体套 storageMeasureTimeout：超时不自作主张报错，
// 而是把已经测到的部分标上 measurementComplete=false 返回 —— 设置页能显示大半数字，
// 总好过让那条唯一连接一直被占着。
func (s *Server) measureStorage(ctx context.Context) (storageUsageResponse, error) {
	usage := storageUsageResponse{Tables: make([]storageTableUsage, 0), MeasuredAt: time.Now().UTC(), Reclaiming: s.storageReclaiming.Load()}
	// 文件大小只是 os.Stat，不碰数据库连接，整理期间也照样准确。
	if info, err := os.Stat(s.config.DatabasePath); err == nil {
		usage.DatabaseBytes = info.Size()
	} else if !errors.Is(err, os.ErrNotExist) {
		return usage, err
	}
	for suffix, target := range map[string]*int64{"-wal": &usage.WalBytes, "-shm": &usage.ShmBytes} {
		if info, err := os.Stat(s.config.DatabasePath + suffix); err == nil {
			*target = info.Size()
		}
	}

	// 从这里开始的每一条查询都走带超时的 ctx：单连接池下，后台空间回收或某个慢查询
	// 会把连接占住几十秒，设置页不能跟着一起挂死。超时按"数字不全"返回已测到的部分。
	measureCtx, cancel := context.WithTimeout(ctx, storageMeasureTimeout)
	defer cancel()
	usage.MeasurementDone = true

	if err := s.db.QueryRowContext(measureCtx, `pragma page_size`).Scan(&usage.PageSize); err != nil {
		return partialStorageUsage(usage, err)
	}
	if err := s.db.QueryRowContext(measureCtx, `pragma page_count`).Scan(&usage.PageCount); err != nil {
		return partialStorageUsage(usage, err)
	}
	if err := s.db.QueryRowContext(measureCtx, `pragma freelist_count`).Scan(&usage.FreelistPages); err != nil {
		return partialStorageUsage(usage, err)
	}
	usage.FreelistBytes = usage.FreelistPages * usage.PageSize

	rows, err := s.db.QueryContext(measureCtx, `select name from sqlite_master where type='table' and name in ('messages','events','tasks','task_runs','task_events','remote_outbox') order by name`)
	if err != nil {
		return partialStorageUsage(usage, err)
	}
	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return partialStorageUsage(usage, err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return partialStorageUsage(usage, err)
	}
	if err := rows.Close(); err != nil {
		return partialStorageUsage(usage, err)
	}

	for _, name := range names {
		item := storageTableUsage{Name: name}
		if err := s.db.QueryRowContext(measureCtx, `select count(*) from `+name).Scan(&item.Rows); err != nil {
			return partialStorageUsage(usage, err)
		}
		if column := storagePayloadColumn(name); column != "" {
			estimated, err := s.estimatePayloadBytes(measureCtx, name, column, item.Rows)
			if err != nil {
				return partialStorageUsage(usage, err)
			}
			item.PayloadBytes = estimated
			item.PayloadEstimated = true
		}
		usage.Tables = append(usage.Tables, item)
	}
	return usage, nil
}

// partialStorageUsage 处理统计中途出错：超时是预期内的（表太大就少报几行），按"数字不全"
// 返回已测到的部分，别让设置页整个读不出来；只有非超时的真实错误才往上抛。
func partialStorageUsage(usage storageUsageResponse, err error) (storageUsageResponse, error) {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		usage.MeasurementDone = false
		return usage, nil
	}
	return usage, err
}

// storagePayloadColumn 返回该表用来衡量负载大小的列；空串表示不统计。
func storagePayloadColumn(table string) string {
	switch table {
	case "messages":
		return "content"
	case "events", "task_events", "remote_outbox":
		return "payload"
	}
	return ""
}

// estimatePayloadBytes 用采样估算一张表的负载字节数。
//
// 旧实现是 `select sum(length(payload)) from events` —— 在 64 万行 / 320 MB 的真实库上
// 要数秒，而它只是给设置页显示一个量级。这里改成只读最新的 N 行求平均行长，再乘总行数；
// 走 rowid 倒序，成本与表大小无关。返回值标记为估算值（PayloadEstimated）。
func (s *Server) estimatePayloadBytes(ctx context.Context, table, column string, totalRows int64) (int64, error) {
	if totalRows <= 0 {
		return 0, nil
	}
	query := fmt.Sprintf(`select avg(length(%s)) from (select %s from %s order by rowid desc limit ?)`, column, column, table)
	var average sql.NullFloat64
	if err := s.db.QueryRowContext(ctx, query, storagePayloadSampleSize).Scan(&average); err != nil {
		return 0, err
	}
	if !average.Valid {
		return 0, nil
	}
	return int64(average.Float64 * float64(totalRows)), nil
}

func (s *Server) cleanupStorage(w http.ResponseWriter, r *http.Request) {
	s.storageMu.Lock()
	defer s.storageMu.Unlock()
	// 用 runtimeCtx 派生：客户端超时 abort 不能掐断删除与 VACUUM（见 storageCleanupTimeout）。
	ctx, cancel := context.WithTimeout(s.runtimeCtx, storageCleanupTimeout)
	defer cancel()

	active, err := s.activeRunCount(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if active > 0 {
		writeError(w, http.StatusConflict, errors.New("请先等待所有运行中的任务完成后再清理"))
		return
	}
	before, err := s.measureStorage(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	result := storageCleanupResult{Before: before}
	// 精确统计遗留的 thinking_tokens（只有用户主动清理时才值得这一次全表扫）。
	if err := s.db.QueryRowContext(ctx, `select count(*),coalesce(sum(length(payload)),0) from events e where `+thinkingTokensPredicate("e")).Scan(&result.ThinkingTokenEvents, &result.ThinkingTokenBytes); err != nil {
		if ctx.Err() != nil {
			writeError(w, http.StatusGatewayTimeout, errors.New("清理超时，请稍后重试"))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// 分批删除：真机库里遗留的 thinking_tokens 有 42 万行，一条无界 DELETE 会是
	// 一个把唯一 SQLite 连接独占几十秒、WAL 暴涨的巨型事务 —— 与 deleteConversation
	// 记录过的教训同源。分批让每批都是一个短事务，其它请求能穿插进来。
	if _, err := s.deleteThinkingTokenEvents(ctx); err != nil {
		if ctx.Err() != nil {
			writeError(w, http.StatusGatewayTimeout, errors.New("清理超时，请稍后重试"))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if _, err := s.db.ExecContext(ctx, `delete from remote_outbox where `+thinkingTokensPredicate("remote_outbox")); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// 用户显式要求释放空间：不受自动回收的 freelist 阈值限制。
	if err := s.reclaimFreePagesLocked(ctx, true); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	after, err := s.measureStorage(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	result.After = after
	result.FreedBytes = maxInt64(0, before.DatabaseBytes-after.DatabaseBytes)
	writeJSON(w, http.StatusOK, result)
}

// deleteThinkingTokenEvents 分批删除 events 里遗留的 thinking_tokens 遥测，返回删除行数。
func (s *Server) deleteThinkingTokenEvents(ctx context.Context) (int64, error) {
	return s.deleteThinkingTokenEventsBatched(ctx, thinkingTokenDeleteBatchSize)
}

// deleteThinkingTokenEventsBatched 是批大小可注入的版本（测试用小数走同一条路径）。
//
// 每批一条独立的 delete（子查询里带 limit，不需要 SQLITE_ENABLE_UPDATE_DELETE_LIMIT），
// 批与批之间事务提交、连接归还，避免一次几万行的大事务把唯一连接和 WAL 一起撑爆。
func (s *Server) deleteThinkingTokenEventsBatched(ctx context.Context, batchSize int64) (int64, error) {
	predicate := thinkingTokensPredicate("events")
	var removed int64
	for {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		result, err := s.db.ExecContext(ctx, `delete from events where rowid in (
			select rowid from events where `+predicate+` limit ?)`, batchSize)
		if err != nil {
			return removed, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return removed, err
		}
		removed += affected
		if affected < batchSize {
			return removed, nil
		}
	}
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
