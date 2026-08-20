package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// QuotaGroup represents a real provider limit domain. Multiple credentials
// that are limited by the same organization, workspace, model deployment or
// egress IP must be mapped to the same group.
type QuotaGroup struct {
	ID             string `json:"id"`
	RunnerID       string `json:"runnerId"`
	Name           string `json:"name"`
	Scope          string `json:"scope"`
	ScopeKey       string `json:"scopeKey"`
	RPMLimit       int    `json:"rpmLimit"`
	TPMLimit       int    `json:"tpmLimit"`
	MaxConcurrency int    `json:"maxConcurrency"`
	Enabled        bool   `json:"enabled"`
}

type CredentialPoolMember struct {
	ProfileID         string `json:"profileId"`
	ProfileRevisionID string `json:"profileRevisionId"`
	Name              string `json:"name"`
	AgentID           string `json:"agentId"`
	Model             string `json:"model"`
	Enabled           bool   `json:"enabled"`
}

type CredentialPoolView struct {
	ID                    string                 `json:"id"`
	RunnerID              string                 `json:"runnerId"`
	Name                  string                 `json:"name"`
	Enabled               bool                   `json:"enabled"`
	CurrentRevisionID     string                 `json:"currentRevisionId"`
	Strategy              string                 `json:"strategy"`
	ProjectMaxConcurrency int                    `json:"projectMaxConcurrency"`
	Members               []CredentialPoolMember `json:"members"`
}

type quotaAdmissionError struct{ retryAfter time.Duration }

func (e *quotaAdmissionError) Error() string { return "credential quota is temporarily unavailable" }

const quotaLeaseDuration = 30 * time.Minute

var retryAfterSecondsPattern = regexp.MustCompile(`(?i)retry[-_ ]?after\s*[:=]?\s*([0-9]+(?:\.[0-9]+)?)`)

// retryAfterFromError accepts structured errors from providers when available,
// then falls back to the standard Retry-After text commonly retained by CLIs.
// A provider cannot make the local scheduler unavailable indefinitely.
func retryAfterFromError(err error) time.Duration {
	if err == nil {
		return 0
	}
	if value, ok := err.(interface{ RetryAfter() time.Duration }); ok {
		return boundedRetryAfter(value.RetryAfter())
	}
	match := retryAfterSecondsPattern.FindStringSubmatch(err.Error())
	if len(match) != 2 {
		return 0
	}
	seconds, parseErr := strconv.ParseFloat(match[1], 64)
	if parseErr != nil || seconds <= 0 {
		return 0
	}
	return boundedRetryAfter(time.Duration(seconds * float64(time.Second)))
}

func boundedRetryAfter(value time.Duration) time.Duration {
	if value <= 0 {
		return 0
	}
	if value > time.Hour {
		return time.Hour
	}
	return value
}

func isRateLimited(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "429") || strings.Contains(message, "rate limit") || strings.Contains(message, "rate_limit")
}

func parseQuotaTime(value string) (time.Time, error) {
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999",
		time.DateTime,
	} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid quota timestamp %q", value)
}

func (s *Server) migrateCredentialScheduling(ctx context.Context) error {
	statements := []string{
		`create table if not exists credential_pools (id text primary key, runner_id text not null references runners(id) on delete restrict, name text not null, enabled integer not null default 1, created_at datetime not null, updated_at datetime not null)`,
		`create table if not exists credential_pool_revisions (id text primary key, pool_id text not null references credential_pools(id) on delete cascade, revision integer not null, strategy text not null default 'fair_queue' check(strategy in ('fair_queue','least_loaded','round_robin')), project_max_concurrency integer not null default 1, state text not null default 'active' check(state in ('active','deprecated','revoked')), created_at datetime not null, unique(pool_id,revision))`,
		`create table if not exists credential_pool_revision_members (pool_revision_id text not null references credential_pool_revisions(id) on delete cascade, profile_id text not null references agent_profiles(id) on delete cascade, profile_revision_id text not null default '', weight integer not null default 1, enabled integer not null default 1, primary key(pool_revision_id,profile_id))`,
		`create table if not exists credential_pool_state (pool_revision_id text primary key references credential_pool_revisions(id) on delete cascade, next_member_offset integer not null default 0, updated_at datetime not null)`,
		`create table if not exists quota_groups (
			id text primary key, runner_id text not null references runners(id) on delete restrict,
			name text not null, scope text not null check(scope in ('credential','organization','workspace','model','ip')),
			scope_key text not null, rpm_limit integer not null default 0, tpm_limit integer not null default 0,
			max_concurrency integer not null default 1, enabled integer not null default 1,
			created_at datetime not null, updated_at datetime not null, unique(runner_id,scope,scope_key)
		)`,
		`create table if not exists credential_quota_groups (
			profile_id text not null references agent_profiles(id) on delete cascade,
			quota_group_id text not null references quota_groups(id) on delete cascade,
			primary key(profile_id,quota_group_id)
		)`,
		`create table if not exists quota_group_state (
			quota_group_id text primary key references quota_groups(id) on delete cascade,
			rpm_tokens real not null default 0, tpm_tokens real not null default 0,
			cooldown_until datetime, last_refilled_at datetime not null, updated_at datetime not null
		)`,
		`create table if not exists quota_reservations (
			id text primary key, quota_group_id text not null references quota_groups(id) on delete restrict,
			run_id text not null references runs(id) on delete cascade, profile_revision_id text not null,
			state text not null check(state in ('reserved','running','released','expired')),
			concurrent_lease_until datetime not null, reserved_rpm real not null default 0,
			reserved_tpm real not null default 0, created_at datetime not null, updated_at datetime not null,
			unique(quota_group_id,run_id)
		)`,
		`create table if not exists revocation_jobs (
			id text primary key, profile_revision_id text not null unique,
			state text not null check(state in ('pending','stopping','completed','failed')),
			error text not null default '', created_at datetime not null, updated_at datetime not null, completed_at datetime
		)`,
		`create index if not exists quota_reservations_active on quota_reservations(quota_group_id,state,concurrent_lease_until)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate credential scheduling: %w", err)
		}
	}
	if err := ensureColumn(ctx, s.db, "credential_pool_revision_members", "profile_revision_id", "text not null default ''"); err != nil {
		return fmt.Errorf("add credential pool member revision: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `update credential_pool_revision_members set profile_revision_id=(select current_revision_id from agent_profiles where id=credential_pool_revision_members.profile_id) where profile_revision_id=''`); err != nil {
		return fmt.Errorf("backfill credential pool member revisions: %w", err)
	}
	// Every state row is created together with its quota group. Do not infer an
	// uninitialised row from its balance or timestamps: a fully consumed bucket
	// can legitimately have both balances at zero after an admission.
	rows, err := s.db.QueryContext(ctx, `select id,runner_id from agent_profiles`)
	if err != nil {
		return err
	}
	type profile struct{ id, runner string }
	profiles := []profile{}
	for rows.Next() {
		var p profile
		if err := rows.Scan(&p.id, &p.runner); err != nil {
			rows.Close()
			return err
		}
		profiles = append(profiles, p)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, p := range profiles {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		err = s.ensurePrivateQuotaGroupTx(ctx, tx, p.id, p.runner)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	// A process cannot safely retain a lease after a server restart. Usage tokens
	// remain consumed, but no process remains that can renew an active lease.
	_, err = s.db.ExecContext(ctx, `update quota_reservations set state='expired',updated_at=? where state in ('reserved','running')`, time.Now().UTC())
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `update revocation_jobs set state='completed',completed_at=?,updated_at=? where state in ('pending','stopping') and not exists (select 1 from runs where runs.agent_profile_revision_id=revocation_jobs.profile_revision_id and runs.status in ('queued','running'))`, time.Now().UTC(), time.Now().UTC())
	return err
}

func (s *Server) ensurePrivateQuotaGroupTx(ctx context.Context, tx *sql.Tx, profileID, runnerID string) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `select exists(select 1 from credential_quota_groups where profile_id=?)`, profileID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	now := time.Now().UTC()
	id := uuid.NewString()
	if _, err := tx.ExecContext(ctx, `insert into quota_groups (id,runner_id,name,scope,scope_key,rpm_limit,tpm_limit,max_concurrency,enabled,created_at,updated_at) values (?,?,?,'credential',?,0,0,1,1,?,?)`, id, runnerID, "Private credential", profileID, now, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `insert into quota_group_state (quota_group_id,rpm_tokens,tpm_tokens,last_refilled_at,updated_at) values (?,0,0,?,?)`, id, now, now); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `insert into credential_quota_groups (profile_id,quota_group_id) values (?,?)`, profileID, id)
	return err
}

func (s *Server) reserveProfileQuotaTx(ctx context.Context, tx *sql.Tx, runID, profileRevisionID string) error {
	if profileRevisionID == "" {
		return nil
	}
	var profileID string
	if err := tx.QueryRowContext(ctx, `select profile_id from agent_profile_revisions where id=?`, profileRevisionID).Scan(&profileID); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `select g.id,g.rpm_limit,g.tpm_limit,g.max_concurrency,g.enabled,coalesce(st.rpm_tokens,0),coalesce(st.tpm_tokens,0),coalesce(st.cooldown_until,''),coalesce(st.last_refilled_at,g.created_at)
		from credential_quota_groups m join quota_groups g on g.id=m.quota_group_id left join quota_group_state st on st.quota_group_id=g.id where m.profile_id=? order by g.id`, profileID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	type group struct {
		id                   string
		rpm, tpm, max        int
		enabled              bool
		rpmTokens, tpmTokens float64
		cooldown             string
		refreshed            string
	}
	groups := []group{}
	for rows.Next() {
		var g group
		if err := rows.Scan(&g.id, &g.rpm, &g.tpm, &g.max, &g.enabled, &g.rpmTokens, &g.tpmTokens, &g.cooldown, &g.refreshed); err != nil {
			return err
		}
		groups = append(groups, g)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(groups) == 0 {
		return errors.New("agent profile has no quota group")
	}
	var existingCount int
	if err := tx.QueryRowContext(ctx, `select count(*) from quota_reservations where run_id=? and state in ('reserved','running')`, runID).Scan(&existingCount); err != nil {
		return err
	}
	if existingCount != 0 {
		var matchingCount int
		if err := tx.QueryRowContext(ctx, `select count(*) from quota_reservations where run_id=? and profile_revision_id=? and state in ('reserved','running')`, runID, profileRevisionID).Scan(&matchingCount); err != nil {
			return err
		}
		if existingCount == len(groups) && matchingCount == len(groups) {
			return nil
		}
		return errors.New("run already has an incompatible quota reservation")
	}
	for _, g := range groups {
		if !g.enabled {
			return &quotaAdmissionError{retryAfter: time.Minute}
		}
		if g.cooldown != "" {
			if until, err := parseQuotaTime(g.cooldown); err == nil && until.After(now) {
				return &quotaAdmissionError{retryAfter: until.Sub(now)}
			}
		}
		var active int
		if err := tx.QueryRowContext(ctx, `select count(*) from quota_reservations where quota_group_id=? and state in ('reserved','running') and concurrent_lease_until>?`, g.id, now).Scan(&active); err != nil {
			return err
		}
		if g.max > 0 && active >= g.max {
			return &quotaAdmissionError{retryAfter: 5 * time.Second}
		}
		refreshed, _ := parseQuotaTime(g.refreshed)
		elapsed := now.Sub(refreshed).Seconds()
		if g.rpm > 0 {
			g.rpmTokens = math.Min(float64(g.rpm), g.rpmTokens+elapsed*float64(g.rpm)/60)
			if g.rpmTokens < 1 {
				return &quotaAdmissionError{retryAfter: time.Duration(math.Ceil((1-g.rpmTokens)*60/float64(g.rpm)) * float64(time.Second))}
			}
		}
		if g.tpm > 0 {
			g.tpmTokens = math.Min(float64(g.tpm), g.tpmTokens+elapsed*float64(g.tpm)/60)
			if g.tpmTokens < 1 {
				return &quotaAdmissionError{retryAfter: time.Second}
			}
		}
	}
	for _, g := range groups {
		rpm, tpm := g.rpmTokens, g.tpmTokens
		if g.rpm > 0 {
			rpm--
		}
		if g.tpm > 0 {
			tpm--
		}
		if _, err := tx.ExecContext(ctx, `insert into quota_group_state (quota_group_id,rpm_tokens,tpm_tokens,last_refilled_at,updated_at) values (?,?,?,?,?) on conflict(quota_group_id) do update set rpm_tokens=excluded.rpm_tokens,tpm_tokens=excluded.tpm_tokens,last_refilled_at=excluded.last_refilled_at,updated_at=excluded.updated_at`, g.id, rpm, tpm, now, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `insert into quota_reservations (id,quota_group_id,run_id,profile_revision_id,state,concurrent_lease_until,reserved_rpm,reserved_tpm,created_at,updated_at) values (?,?,?,?, 'running', ?,?,?,?,?)`, uuid.NewString(), g.id, runID, profileRevisionID, now.Add(quotaLeaseDuration), boolToFloat(g.rpm > 0), boolToFloat(g.tpm > 0), now, now); err != nil {
			return err
		}
	}
	return nil
}

func boolToFloat(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

func (s *Server) releaseQuotaReservations(ctx context.Context, tx *sql.Tx, runID string) error {
	_, err := tx.ExecContext(ctx, `update quota_reservations set state='released',updated_at=? where run_id=? and state in ('reserved','running')`, time.Now().UTC(), runID)
	return err
}

// reconcileQuotaUsageTx replaces the one-token TPM admission estimate with
// the usage reported by the CLI. Negative tokens are deliberate: they retain
// an observed provider overage and make subsequent admissions wait for refill
// instead of silently treating the budget as accurate.
func (s *Server) reconcileQuotaUsageTx(ctx context.Context, tx *sql.Tx, runID string) error {
	var input, output, cacheRead, cacheCreated int64
	err := tx.QueryRowContext(ctx, `select input_tokens,output_tokens,cache_read_tokens,cache_creation_tokens from run_usage where run_id=?`, runID).Scan(&input, &output, &cacheRead, &cacheCreated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // The CLI did not expose usage; retain the conservative estimate.
	}
	if err != nil {
		return err
	}
	actual := float64(input + output + cacheRead + cacheCreated)
	if actual < 1 {
		actual = 1
	}
	rows, err := tx.QueryContext(ctx, `select q.quota_group_id,q.reserved_tpm,g.tpm_limit from quota_reservations q join quota_groups g on g.id=q.quota_group_id where q.run_id=? and q.state in ('reserved','running') and g.tpm_limit>0`, runID)
	if err != nil {
		return err
	}
	type reservation struct {
		groupID  string
		reserved float64
	}
	var reservations []reservation
	for rows.Next() {
		var item reservation
		var limit int
		if err := rows.Scan(&item.groupID, &item.reserved, &limit); err != nil {
			rows.Close()
			return err
		}
		reservations = append(reservations, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, item := range reservations {
		delta := actual - item.reserved
		if _, err := tx.ExecContext(ctx, `update quota_group_state set tpm_tokens=tpm_tokens-?,updated_at=? where quota_group_id=?`, delta, now, item.groupID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `update quota_reservations set reserved_tpm=?,updated_at=? where quota_group_id=? and run_id=?`, actual, now, item.groupID, runID); err != nil {
			return err
		}
	}
	return nil
}

// completeRevocationJobsForRunTx transitions a revocation only after every
// affected persisted Run has reached a terminal state. The cancellation signal
// itself is intentionally not treated as proof that a CLI process exited.
func (s *Server) completeRevocationJobsForRunTx(ctx context.Context, tx *sql.Tx, runID string) error {
	var revisionID string
	if err := tx.QueryRowContext(ctx, `select agent_profile_revision_id from runs where id=?`, runID).Scan(&revisionID); err != nil {
		return err
	}
	if revisionID == "" {
		return nil
	}
	var active int
	if err := tx.QueryRowContext(ctx, `select count(*) from runs where agent_profile_revision_id=? and status in ('queued','running')`, revisionID).Scan(&active); err != nil {
		return err
	}
	if active != 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `update revocation_jobs set state='completed',completed_at=?,updated_at=? where profile_revision_id=? and state='stopping'`, time.Now().UTC(), time.Now().UTC(), revisionID)
	return err
}

func (s *Server) renewQuotaReservations(ctx context.Context, runID string) error {
	_, err := s.db.ExecContext(ctx, `update quota_reservations set concurrent_lease_until=?,updated_at=? where run_id=? and state in ('reserved','running')`, time.Now().UTC().Add(quotaLeaseDuration), time.Now().UTC(), runID)
	return err
}

// profileQuotaRetryAfterTx is a non-mutating admission probe. It is used only
// to choose a pool member; reserveProfileQuotaTx repeats the checks before it
// makes the reservation authoritative.
func (s *Server) profileQuotaRetryAfterTx(ctx context.Context, tx *sql.Tx, profileRevisionID string) (time.Duration, error) {
	var profileID string
	if err := tx.QueryRowContext(ctx, `select profile_id from agent_profile_revisions where id=?`, profileRevisionID).Scan(&profileID); err != nil {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, `select g.id,g.rpm_limit,g.tpm_limit,g.max_concurrency,g.enabled,coalesce(st.rpm_tokens,0),coalesce(st.tpm_tokens,0),coalesce(st.cooldown_until,''),coalesce(st.last_refilled_at,g.created_at)
		from credential_quota_groups m join quota_groups g on g.id=m.quota_group_id left join quota_group_state st on st.quota_group_id=g.id where m.profile_id=? order by g.id`, profileID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	now := time.Now().UTC()
	for rows.Next() {
		var id, cooldown, refreshed string
		var rpm, tpm, maximum int
		var enabled bool
		var rpmTokens, tpmTokens float64
		if err := rows.Scan(&id, &rpm, &tpm, &maximum, &enabled, &rpmTokens, &tpmTokens, &cooldown, &refreshed); err != nil {
			return 0, err
		}
		if !enabled {
			return time.Minute, nil
		}
		if cooldown != "" {
			if until, parseErr := parseQuotaTime(cooldown); parseErr == nil && until.After(now) {
				return until.Sub(now), nil
			}
		}
		var active int
		if err := tx.QueryRowContext(ctx, `select count(*) from quota_reservations where quota_group_id=? and state in ('reserved','running') and concurrent_lease_until>?`, id, now).Scan(&active); err != nil {
			return 0, err
		}
		if maximum > 0 && active >= maximum {
			return 5 * time.Second, nil
		}
		refreshedAt, _ := parseQuotaTime(refreshed)
		elapsed := now.Sub(refreshedAt).Seconds()
		if rpm > 0 {
			rpmTokens = math.Min(float64(rpm), rpmTokens+elapsed*float64(rpm)/60)
			if rpmTokens < 1 {
				return time.Duration(math.Ceil((1-rpmTokens)*60/float64(rpm))) * time.Second, nil
			}
		}
		if tpm > 0 {
			tpmTokens = math.Min(float64(tpm), tpmTokens+elapsed*float64(tpm)/60)
			if tpmTokens < 1 {
				return time.Second, nil
			}
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return 0, nil
}

func (s *Server) recordQuotaFailure(ctx context.Context, tx *sql.Tx, runID string, runErr error) error {
	if !isRateLimited(runErr) {
		return nil
	}
	cooldown := retryAfterFromError(runErr)
	if cooldown == 0 {
		cooldown = 30 * time.Second
	}
	until := time.Now().UTC().Add(cooldown)
	_, err := tx.ExecContext(ctx, `update quota_groups set updated_at=? where id in (select quota_group_id from quota_reservations where run_id=?)`, time.Now().UTC(), runID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `update quota_group_state set cooldown_until=?,updated_at=? where quota_group_id in (select quota_group_id from quota_reservations where run_id=?)`, until, time.Now().UTC(), runID)
	return err
}

func (s *Server) listProfileQuotaGroups(w http.ResponseWriter, r *http.Request) {
	profileID := chi.URLParam(r, "profileID")
	rows, err := s.db.QueryContext(r.Context(), `select g.id,g.runner_id,g.name,g.scope,g.scope_key,g.rpm_limit,g.tpm_limit,g.max_concurrency,g.enabled from credential_quota_groups m join quota_groups g on g.id=m.quota_group_id where m.profile_id=? order by g.name`, profileID)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	defer rows.Close()
	out := []QuotaGroup{}
	for rows.Next() {
		var g QuotaGroup
		if err := rows.Scan(&g.ID, &g.RunnerID, &g.Name, &g.Scope, &g.ScopeKey, &g.RPMLimit, &g.TPMLimit, &g.MaxConcurrency, &g.Enabled); err != nil {
			writeError(w, 500, err)
			return
		}
		out = append(out, g)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) listCredentialPools(w http.ResponseWriter, r *http.Request) {
	runnerID := chi.URLParam(r, "runnerID")
	rows, err := s.db.QueryContext(r.Context(), `select p.id,p.runner_id,p.name,p.enabled,r.id,r.strategy,r.project_max_concurrency
		from credential_pools p join credential_pool_revisions r on r.pool_id=p.id
		where p.runner_id=? and p.enabled=1 and r.state='active' and r.revision=(select max(revision) from credential_pool_revisions where pool_id=p.id and state='active') order by p.name`, runnerID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	pools := []CredentialPoolView{}
	for rows.Next() {
		var pool CredentialPoolView
		if err := rows.Scan(&pool.ID, &pool.RunnerID, &pool.Name, &pool.Enabled, &pool.CurrentRevisionID, &pool.Strategy, &pool.ProjectMaxConcurrency); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		pools = append(pools, pool)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := rows.Close(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	for index := range pools {
		pool := &pools[index]
		memberRows, err := s.db.QueryContext(r.Context(), `select m.profile_id,m.profile_revision_id,p.name,p.agent_id,r.model,m.enabled
			from credential_pool_revision_members m join agent_profiles p on p.id=m.profile_id join agent_profile_revisions r on r.id=m.profile_revision_id
			where m.pool_revision_id=? order by p.name`, pool.CurrentRevisionID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		for memberRows.Next() {
			var member CredentialPoolMember
			if err := memberRows.Scan(&member.ProfileID, &member.ProfileRevisionID, &member.Name, &member.AgentID, &member.Model, &member.Enabled); err != nil {
				memberRows.Close()
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			pool.Members = append(pool.Members, member)
		}
		if err := memberRows.Close(); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, pools)
}

func (s *Server) createProfileQuotaGroup(w http.ResponseWriter, r *http.Request) {
	profileID := chi.URLParam(r, "profileID")
	var input struct {
		Name, Scope, ScopeKey              string
		RPMLimit, TPMLimit, MaxConcurrency int
	}
	if !decode(w, r, &input) {
		return
	}
	input.Name, input.Scope, input.ScopeKey = strings.TrimSpace(input.Name), strings.TrimSpace(input.Scope), strings.TrimSpace(input.ScopeKey)
	if input.Name == "" || input.ScopeKey == "" || input.RPMLimit < 0 || input.TPMLimit < 0 || input.MaxConcurrency < 1 {
		writeError(w, 400, errors.New("quota group input is invalid"))
		return
	}
	switch input.Scope {
	case "credential", "organization", "workspace", "model", "ip":
	default:
		writeError(w, 400, errors.New("quota group scope is invalid"))
		return
	}
	var runnerID string
	if err := s.db.QueryRowContext(r.Context(), `select runner_id from agent_profiles where id=?`, profileID).Scan(&runnerID); err != nil {
		writeError(w, 404, errors.New("agent profile not found"))
		return
	}
	if !s.canManageProfileRunner(runnerID) {
		writeError(w, http.StatusConflict, errors.New("target Runner Agent is not installed; remote quota groups are unavailable"))
		return
	}
	now := time.Now().UTC()
	g := QuotaGroup{ID: uuid.NewString(), RunnerID: runnerID, Name: input.Name, Scope: input.Scope, ScopeKey: input.ScopeKey, RPMLimit: input.RPMLimit, TPMLimit: input.TPMLimit, MaxConcurrency: input.MaxConcurrency, Enabled: true}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `insert into quota_groups (id,runner_id,name,scope,scope_key,rpm_limit,tpm_limit,max_concurrency,enabled,created_at,updated_at) values (?,?,?,?,?,?,?,?,1,?,?)`, g.ID, g.RunnerID, g.Name, g.Scope, g.ScopeKey, g.RPMLimit, g.TPMLimit, g.MaxConcurrency, now, now)
	}
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `insert into quota_group_state (quota_group_id,rpm_tokens,tpm_tokens,last_refilled_at,updated_at) values (?, ?, ?, ?, ?)`, g.ID, input.RPMLimit, input.TPMLimit, now, now)
	}
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `insert into credential_quota_groups (profile_id,quota_group_id) values (?,?)`, profileID, g.ID)
	}
	if err != nil {
		if tx != nil {
			_ = tx.Rollback()
		}
		writeError(w, 500, err)
		return
	}
	if err = tx.Commit(); err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, 201, g)
}

func (s *Server) getRevocationJob(w http.ResponseWriter, r *http.Request) {
	var job struct {
		ID, ProfileRevisionID, State, Error string
		CreatedAt, UpdatedAt, CompletedAt   sql.NullString
	}
	err := s.db.QueryRowContext(r.Context(), `select id,profile_revision_id,state,error,created_at,updated_at,coalesce(completed_at,'') from revocation_jobs where profile_revision_id=?`, chi.URLParam(r, "revisionID")).Scan(&job.ID, &job.ProfileRevisionID, &job.State, &job.Error, &job.CreatedAt, &job.UpdatedAt, &job.CompletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("revocation job not found"))
		return
	}
	if err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) attachProfileQuotaGroup(w http.ResponseWriter, r *http.Request) {
	groupID, profileID := chi.URLParam(r, "quotaGroupID"), chi.URLParam(r, "profileID")
	var groupRunner, profileRunner string
	var groupEnabled bool
	err := s.db.QueryRowContext(r.Context(), `select runner_id,enabled from quota_groups where id=?`, groupID).Scan(&groupRunner, &groupEnabled)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("quota group not found"))
		return
	}
	if err != nil {
		writeError(w, 500, err)
		return
	}
	if !groupEnabled {
		writeError(w, http.StatusConflict, errors.New("quota group is disabled"))
		return
	}
	if err := s.db.QueryRowContext(r.Context(), `select runner_id from agent_profiles where id=? and enabled=1`, profileID).Scan(&profileRunner); errors.Is(err, sql.ErrNoRows) {
		writeError(w, 404, errors.New("agent profile not found"))
		return
	} else if err != nil {
		writeError(w, 500, err)
		return
	}
	if groupRunner != profileRunner {
		writeError(w, http.StatusConflict, errors.New("quota group and profile belong to different runners"))
		return
	}
	if !s.canManageProfileRunner(groupRunner) {
		writeError(w, http.StatusConflict, errors.New("target Runner Agent is not installed; remote quota groups are unavailable"))
		return
	}
	if _, err := s.db.ExecContext(r.Context(), `insert into credential_quota_groups (profile_id,quota_group_id) values (?,?) on conflict do nothing`, profileID, groupID); err != nil {
		writeError(w, 500, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) updateQuotaGroup(w http.ResponseWriter, r *http.Request) {
	groupID := chi.URLParam(r, "quotaGroupID")
	var input struct {
		Name           *string `json:"name"`
		RPMLimit       *int    `json:"rpmLimit"`
		TPMLimit       *int    `json:"tpmLimit"`
		MaxConcurrency *int    `json:"maxConcurrency"`
		Enabled        *bool   `json:"enabled"`
	}
	if !decode(w, r, &input) {
		return
	}
	var current QuotaGroup
	if err := s.db.QueryRowContext(r.Context(), `select id,runner_id,name,scope,scope_key,rpm_limit,tpm_limit,max_concurrency,enabled from quota_groups where id=?`, groupID).Scan(&current.ID, &current.RunnerID, &current.Name, &current.Scope, &current.ScopeKey, &current.RPMLimit, &current.TPMLimit, &current.MaxConcurrency, &current.Enabled); errors.Is(err, sql.ErrNoRows) {
		writeError(w, 404, errors.New("quota group not found"))
		return
	} else if err != nil {
		writeError(w, 500, err)
		return
	}
	if !s.canManageProfileRunner(current.RunnerID) {
		writeError(w, http.StatusConflict, errors.New("target Runner Agent is not installed; remote quota groups are unavailable"))
		return
	}
	if input.Name != nil {
		current.Name = strings.TrimSpace(*input.Name)
	}
	if input.RPMLimit != nil {
		current.RPMLimit = *input.RPMLimit
	}
	if input.TPMLimit != nil {
		current.TPMLimit = *input.TPMLimit
	}
	if input.MaxConcurrency != nil {
		current.MaxConcurrency = *input.MaxConcurrency
	}
	if input.Enabled != nil {
		current.Enabled = *input.Enabled
	}
	if current.Name == "" || current.RPMLimit < 0 || current.TPMLimit < 0 || current.MaxConcurrency < 1 {
		writeError(w, 400, errors.New("quota group input is invalid"))
		return
	}
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(r.Context(), `update quota_groups set name=?,rpm_limit=?,tpm_limit=?,max_concurrency=?,enabled=?,updated_at=? where id=?`, current.Name, current.RPMLimit, current.TPMLimit, current.MaxConcurrency, current.Enabled, now, groupID); err != nil {
		writeError(w, 500, err)
		return
	}
	if _, err := s.db.ExecContext(r.Context(), `update quota_group_state set rpm_tokens=min(rpm_tokens,?),tpm_tokens=min(tpm_tokens,?),updated_at=? where quota_group_id=?`, current.RPMLimit, current.TPMLimit, now, groupID); err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, http.StatusOK, current)
}

func (s *Server) createCredentialPool(w http.ResponseWriter, r *http.Request) {
	runnerID := chi.URLParam(r, "runnerID")
	if !s.canManageProfileRunner(runnerID) {
		writeError(w, http.StatusConflict, errors.New("target Runner Agent is not installed; remote credential pools are unavailable"))
		return
	}
	var input struct {
		Name, Strategy        string
		ProjectMaxConcurrency int
		ProfileIDs            []string
	}
	if !decode(w, r, &input) {
		return
	}
	if strings.TrimSpace(input.Name) == "" {
		writeError(w, 400, errors.New("pool name is required"))
		return
	}
	if input.Strategy == "" {
		input.Strategy = "fair_queue"
	}
	if input.ProjectMaxConcurrency < 1 {
		input.ProjectMaxConcurrency = 1
	}
	if input.Strategy != "fair_queue" && input.Strategy != "least_loaded" && input.Strategy != "round_robin" {
		writeError(w, 400, errors.New("pool strategy is invalid"))
		return
	}
	if len(input.ProfileIDs) == 0 {
		writeError(w, 400, errors.New("credential pool requires at least one profile"))
		return
	}
	now := time.Now().UTC()
	poolID, revID := uuid.NewString(), uuid.NewString()
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `insert into credential_pools (id,runner_id,name,enabled,created_at,updated_at) values (?,?,?,1,?,?)`, poolID, runnerID, input.Name, now, now)
	}
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `insert into credential_pool_revisions (id,pool_id,revision,strategy,project_max_concurrency,state,created_at) values (?,?,1,?,?, 'active',?)`, revID, poolID, input.Strategy, input.ProjectMaxConcurrency, now)
	}
	if err == nil {
		var expectedAgent, expectedProtocol, expectedBaseURL, expectedModel string
		seenProfiles := map[string]struct{}{}
		for _, profileID := range input.ProfileIDs {
			if _, duplicate := seenProfiles[profileID]; duplicate {
				err = errors.New("credential pool contains a duplicate profile")
				break
			}
			seenProfiles[profileID] = struct{}{}
			var profileRevisionID, profileRunner, agentID, protocol, baseURL, model string
			if e := tx.QueryRowContext(r.Context(), `select r.id,p.runner_id,p.agent_id,r.protocol,r.base_url,r.model from agent_profiles p join agent_profile_revisions r on r.id=p.current_revision_id where p.id=? and p.enabled=1 and r.state='active'`, profileID).Scan(&profileRevisionID, &profileRunner, &agentID, &protocol, &baseURL, &model); e != nil || profileRunner != runnerID {
				err = errors.New("pool profile is unavailable on this runner")
				break
			}
			baseURL = strings.TrimRight(strings.ToLower(strings.TrimSpace(baseURL)), "/")
			if expectedAgent == "" {
				expectedAgent, expectedProtocol, expectedBaseURL, expectedModel = agentID, protocol, baseURL, model
			} else if agentID != expectedAgent || protocol != expectedProtocol || baseURL != expectedBaseURL || model != expectedModel {
				err = errors.New("pool profiles must use the same agent, protocol, endpoint and model")
				break
			}
			if _, e := tx.ExecContext(r.Context(), `insert into credential_pool_revision_members (pool_revision_id,profile_id,profile_revision_id,weight,enabled) values (?,?,?,1,1)`, revID, profileID, profileRevisionID); e != nil {
				err = e
				break
			}
		}
	}
	if err != nil {
		if tx != nil {
			_ = tx.Rollback()
		}
		writeError(w, 400, err)
		return
	}
	if err = tx.Commit(); err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"poolId": poolID, "poolRevisionId": revID})
}

func (s *Server) setProjectAgentPool(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	var input struct{ AgentID, PoolRevisionID string }
	if !decode(w, r, &input) {
		return
	}
	if !validProfileAgent(input.AgentID) || strings.TrimSpace(input.PoolRevisionID) == "" {
		writeError(w, 400, errors.New("agentId and poolRevisionId are required"))
		return
	}
	var runnerID string
	if err := s.db.QueryRowContext(r.Context(), `select coalesce(nullif(runner_id,''),runner) from projects where id=?`, projectID).Scan(&runnerID); errors.Is(err, sql.ErrNoRows) {
		writeError(w, 404, errors.New("project not found"))
		return
	} else if err != nil {
		writeError(w, 500, err)
		return
	}
	if !s.canManageProfileRunner(runnerID) {
		writeError(w, http.StatusConflict, errors.New("target Runner Agent is not installed; remote managed credentials are unavailable"))
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err == nil {
		var poolRunner string
		if e := tx.QueryRowContext(r.Context(), `select p.runner_id from credential_pool_revisions r join credential_pools p on p.id=r.pool_id where r.id=? and r.state='active' and p.enabled=1`, input.PoolRevisionID).Scan(&poolRunner); e != nil || poolRunner != runnerID {
			err = errors.New("pool revision is unavailable for this project runner")
		}
	}
	if err == nil {
		var compatible bool
		err = tx.QueryRowContext(r.Context(), `select exists(select 1 from credential_pool_revision_members m join agent_profiles p on p.id=m.profile_id join agent_profile_revisions r on r.id=m.profile_revision_id where m.pool_revision_id=? and m.enabled=1 and p.enabled=1 and p.agent_id=? and r.state in ('active','deprecated'))`, input.PoolRevisionID, input.AgentID).Scan(&compatible)
		if err == nil && !compatible {
			err = errors.New("pool has no active profile for this agent")
		}
	}
	if err == nil {
		err = s.replaceProjectAgentRouteWithPoolTx(r.Context(), tx, projectID, input.AgentID, "", "", input.PoolRevisionID, "pool")
	}
	if err != nil {
		if tx != nil {
			_ = tx.Rollback()
		}
		writeError(w, 409, err)
		return
	}
	if err = tx.Commit(); err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"agentId": input.AgentID, "poolRevisionId": input.PoolRevisionID})
}
