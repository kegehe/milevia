package app

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// 跨端安装的逐主机授权与审计（docs/42 §9.1）。
//
// 已拍板的范围是"本机 + WSL + SSH 全放开"，但"放开"不等于"默认就能装"：
// 安装意味着**在用户的生产机器上执行任意 npm 包代码 + 落一整套运行时**，
// 与现有的"项目沙箱内读写 + 该仓库 Git 操作"不是一个量级。所以三件事缺一不可：
//
//  1. **逐主机显式授权**（默认关、可撤销）——本文件；
//  2. **一律不用 sudo**（落在远端家目录）——由执行层保证；
//  3. **审计**（谁在什么时候往哪台机器装了什么）——本文件。
//
// 授权与审计都是**按主机**的，不是按项目、也不是全局开关：给 prod-server 授权
// 不该顺带把 staging 也放开。

func (s *Server) migrateRunnerInstallGrants(ctx context.Context) error {
	stmts := []string{
		`create table if not exists runner_install_grants (
			runner_id text primary key,
			granted_at datetime not null
		)`,
		`create table if not exists agent_install_audit (
			id integer primary key autoincrement,
			runner_id text not null,
			agent_id text not null,
			action text not null,
			from_version text not null default '',
			to_version text not null default '',
			result text not null,
			detail text not null default '',
			created_at datetime not null
		)`,
		`create index if not exists agent_install_audit_runner on agent_install_audit(runner_id, created_at desc)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// remoteInstallAllowed 报告该 Runner 上是否已被显式授权安装。
//
// 本机永远为真（那是用户自己的机器，且不涉及提权与远程执行）。
func (s *Server) remoteInstallAllowed(ctx context.Context, runnerID string) bool {
	if isLocalRunnerID(runnerID) {
		return true
	}
	var grantedAt time.Time
	err := s.db.QueryRowContext(ctx, `select granted_at from runner_install_grants where runner_id=?`, runnerID).Scan(&grantedAt)
	return err == nil
}

// grantRunnerInstall 是 POST /api/runners/{runnerID}/remote-install/grant。
func (s *Server) grantRunnerInstall(w http.ResponseWriter, r *http.Request) {
	runnerID := chi.URLParam(r, "runnerID")
	if isLocalRunnerID(runnerID) {
		writeError(w, http.StatusBadRequest, errors.New("本机不需要授权"))
		return
	}
	if _, ok := s.runnerRegistry.getMeta(runnerID); !ok {
		writeError(w, http.StatusNotFound, errors.New("runner not found"))
		return
	}
	if _, err := s.db.ExecContext(r.Context(), `insert into runner_install_grants (runner_id,granted_at)
		values (?,?) on conflict(runner_id) do update set granted_at=excluded.granted_at`, runnerID, time.Now().UTC()); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.recordInstallAudit(r.Context(), installAuditEntry{
		RunnerID: runnerID, AgentID: "*", Action: "grant", Result: "succeeded",
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runnerId": runnerID, "remoteInstallAllowed": true})
}

// revokeRunnerInstall 是 DELETE /api/runners/{runnerID}/remote-install/grant。
func (s *Server) revokeRunnerInstall(w http.ResponseWriter, r *http.Request) {
	runnerID := chi.URLParam(r, "runnerID")
	if _, err := s.db.ExecContext(r.Context(), `delete from runner_install_grants where runner_id=?`, runnerID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.recordInstallAudit(r.Context(), installAuditEntry{
		RunnerID: runnerID, AgentID: "*", Action: "revoke", Result: "succeeded",
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runnerId": runnerID, "remoteInstallAllowed": false})
}

// installAuditEntry 是一条安装审计记录。
type installAuditEntry struct {
	RunnerID    string    `json:"runnerId"`
	AgentID     string    `json:"agentId"`
	Action      string    `json:"action"`
	FromVersion string    `json:"fromVersion,omitempty"`
	ToVersion   string    `json:"toVersion,omitempty"`
	Result      string    `json:"result"`
	Detail      string    `json:"detail,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
}

// recordInstallAudit 落一条审计。
//
// 审计失败要不要让主操作失败？**要**（在调用处决定），因为"往用户的生产机器上
// 装了东西但没留下记录"是这条链路最不能接受的结局。这里只负责写。
func (s *Server) recordInstallAudit(ctx context.Context, entry installAuditEntry) error {
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `insert into agent_install_audit
		(runner_id,agent_id,action,from_version,to_version,result,detail,created_at)
		values (?,?,?,?,?,?,?,?)`,
		entry.RunnerID, entry.AgentID, entry.Action, entry.FromVersion, entry.ToVersion,
		entry.Result, entry.Detail, entry.CreatedAt)
	return err
}

// listInstallAudit 读审计记录（最近的在前）。
func (s *Server) listInstallAudit(ctx context.Context, runnerID string, limit int) ([]installAuditEntry, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `select runner_id,agent_id,action,from_version,to_version,result,detail,created_at
		from agent_install_audit where runner_id=? order by created_at desc, id desc limit ?`, runnerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []installAuditEntry{}
	for rows.Next() {
		var item installAuditEntry
		if err := rows.Scan(&item.RunnerID, &item.AgentID, &item.Action, &item.FromVersion,
			&item.ToVersion, &item.Result, &item.Detail, &item.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// listInstallAuditHandler 是 GET /api/runners/{runnerID}/install-audit。
func (s *Server) listInstallAuditHandler(w http.ResponseWriter, r *http.Request) {
	runnerID := chi.URLParam(r, "runnerID")
	items, err := s.listInstallAudit(r.Context(), runnerID, 50)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runnerId": runnerID, "items": items})
}
