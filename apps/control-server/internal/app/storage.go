package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"
)

type storageTableUsage struct {
	Name         string `json:"name"`
	Rows         int64  `json:"rows"`
	PayloadBytes int64  `json:"payloadBytes,omitempty"`
}

type storageUsageResponse struct {
	DatabaseBytes       int64               `json:"databaseBytes"`
	WalBytes            int64               `json:"walBytes"`
	ShmBytes            int64               `json:"shmBytes"`
	PageSize            int64               `json:"pageSize"`
	PageCount           int64               `json:"pageCount"`
	FreelistPages       int64               `json:"freelistPages"`
	ThinkingTokenEvents int64               `json:"thinkingTokenEvents"`
	ThinkingTokenBytes  int64               `json:"thinkingTokenBytes"`
	Tables              []storageTableUsage `json:"tables"`
	MeasuredAt          time.Time           `json:"measuredAt"`
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

func (s *Server) measureStorage(ctx context.Context) (storageUsageResponse, error) {
	usage := storageUsageResponse{Tables: make([]storageTableUsage, 0), MeasuredAt: time.Now().UTC()}
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
	if err := s.db.QueryRowContext(ctx, `pragma page_size`).Scan(&usage.PageSize); err != nil {
		return usage, err
	}
	if err := s.db.QueryRowContext(ctx, `pragma page_count`).Scan(&usage.PageCount); err != nil {
		return usage, err
	}
	if err := s.db.QueryRowContext(ctx, `pragma freelist_count`).Scan(&usage.FreelistPages); err != nil {
		return usage, err
	}
	if err := s.db.QueryRowContext(ctx, `select count(*),coalesce(sum(length(payload)),0) from events e where `+thinkingTokensPredicate("e")).Scan(&usage.ThinkingTokenEvents, &usage.ThinkingTokenBytes); err != nil {
		return usage, err
	}
	rows, err := s.db.QueryContext(ctx, `select name from sqlite_master where type='table' and name in ('messages','events','tasks','task_runs','task_events','remote_outbox') order by name`)
	if err != nil {
		return usage, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return usage, err
		}
		item := storageTableUsage{Name: name}
		query := `select count(*) from ` + name
		if err := s.db.QueryRowContext(ctx, query).Scan(&item.Rows); err != nil {
			return usage, err
		}
		if name == "messages" {
			_ = s.db.QueryRowContext(ctx, `select coalesce(sum(length(content)),0) from messages`).Scan(&item.PayloadBytes)
		}
		if name == "events" {
			_ = s.db.QueryRowContext(ctx, `select coalesce(sum(length(payload)),0) from events`).Scan(&item.PayloadBytes)
		}
		if name == "task_events" || name == "remote_outbox" {
			_ = s.db.QueryRowContext(ctx, `select coalesce(sum(length(payload)),0) from `+name).Scan(&item.PayloadBytes)
		}
		usage.Tables = append(usage.Tables, item)
	}
	return usage, rows.Err()
}

func (s *Server) cleanupStorage(w http.ResponseWriter, r *http.Request) {
	s.storageMu.Lock()
	defer s.storageMu.Unlock()
	var active int64
	if err := s.db.QueryRowContext(r.Context(), `select count(*) from runs where status in ('queued','running')`).Scan(&active); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if active > 0 {
		writeError(w, http.StatusConflict, errors.New("请先等待所有运行中的任务完成后再清理"))
		return
	}
	before, err := s.measureStorage(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if _, err = tx.ExecContext(r.Context(), `delete from events where `+thinkingTokensPredicate("events")); err != nil {
		tx.Rollback()
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if _, err = tx.ExecContext(r.Context(), `delete from remote_outbox where `+thinkingTokensPredicate("remote_outbox")); err != nil {
		tx.Rollback()
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err = tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if _, err = s.db.ExecContext(r.Context(), `vacuum`); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	after, err := s.measureStorage(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"before": before, "after": after, "freedBytes": maxInt64(0, before.DatabaseBytes-after.DatabaseBytes)})
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
