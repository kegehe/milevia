package app

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"
)

// PersistedRunner is the durable description of an execution environment.
// Runtime adapters remain in runnerRegistry; this record lets a future process
// reconstruct them instead of treating an in-memory registration as data.
type PersistedRunner struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	DisplayName string    `json:"displayName"`
	ConfigJSON  string    `json:"-"`
	Enabled     bool      `json:"enabled"`
	LastError   string    `json:"lastError,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

func runnerKindForLegacyID(id string) string {
	if id == "windows-local" {
		return "windows-local"
	}
	if id == "wsl-local" {
		return "wsl"
	}
	if strings.HasPrefix(id, "ssh-") {
		return "ssh"
	}
	return "legacy"
}

func runnerDisplayNameForLegacyID(id string) string {
	if id == "windows-local" {
		return "Windows Local"
	}
	if id == "wsl-local" {
		return "WSL Local"
	}
	return id
}

// migratePersistedRunners is deliberately additive. Legacy projects continue
// to expose their existing runner field while runner_id is populated with the
// same stable identifier. This permits staged adapter migration without making
// existing projects, conversations, or active-run recovery unreadable.
func (s *Server) migratePersistedRunners(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `create table if not exists runners (
		id text primary key,
		kind text not null,
		display_name text not null,
		config_json text not null default '{}',
		enabled integer not null default 1,
		last_error text not null default '',
		created_at datetime not null,
		updated_at datetime not null
	)`); err != nil {
		return fmt.Errorf("create runners table: %w", err)
	}
	if err := ensureColumn(ctx, s.db, "projects", "runner_id", "text not null default ''"); err != nil {
		return fmt.Errorf("add projects.runner_id: %w", err)
	}
	now := time.Now().UTC()
	defaultRunnerID, defaultRunnerKind, defaultRunnerName := "wsl-local", "wsl", "WSL Local"
	if runtime.GOOS == "windows" {
		defaultRunnerID, defaultRunnerKind, defaultRunnerName = "windows-local", "windows-local", "Windows Local"
	}
	if _, err := s.db.ExecContext(ctx, `insert into runners (id,kind,display_name,config_json,enabled,last_error,created_at,updated_at)
		values (?,?,?,'{}',1,'',?,?) on conflict(id) do nothing`, defaultRunnerID, defaultRunnerKind, defaultRunnerName, now, now); err != nil {
		return fmt.Errorf("seed local runner: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `select distinct runner from projects where runner <> ''`)
	if err != nil {
		return fmt.Errorf("read legacy project runners: %w", err)
	}
	defer rows.Close()
	legacyIDs := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan legacy project runner: %w", err)
		}
		legacyIDs = append(legacyIDs, id)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate legacy project runners: %w", err)
	}
	for _, id := range legacyIDs {
		if _, err := s.db.ExecContext(ctx, `insert into runners (id,kind,display_name,config_json,enabled,last_error,created_at,updated_at)
			values (?,?,?,'{}',1,'',?,?) on conflict(id) do nothing`, id, runnerKindForLegacyID(id), runnerDisplayNameForLegacyID(id), now, now); err != nil {
			return fmt.Errorf("persist legacy runner %q: %w", id, err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `update projects set runner_id=runner where runner_id=''`); err != nil {
		return fmt.Errorf("backfill projects.runner_id: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `create index if not exists projects_runner_id on projects(runner_id)`); err != nil {
		return fmt.Errorf("index projects.runner_id: %w", err)
	}
	return nil
}
