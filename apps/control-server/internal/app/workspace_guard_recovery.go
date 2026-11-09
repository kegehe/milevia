package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

type workspaceGuardOperationView struct {
	ID            string       `json:"id"`
	ConnectionKey string       `json:"-"`
	Path          string       `json:"path"`
	State         string       `json:"state"`
	TargetID      string       `json:"-"`
	ValidUntil    sql.NullTime `json:"-"`
	CreatedAt     time.Time    `json:"createdAt"`
	UpdatedAt     time.Time    `json:"updatedAt"`
}

func (s *Server) workspaceGuardOperations(ctx context.Context, connection string, pendingOnly bool, limit int, roots ...string) ([]workspaceGuardOperationView, error) {
	query := `select id,connection_key,path,state,target_id,valid_until,created_at,updated_at from workspace_guard_operations where 1=1`
	var args []any
	if connection != "" {
		query += ` and connection_key=?`
		args = append(args, connection)
	}
	if pendingOnly {
		query += ` and state in ('release_pending','unconfirmed')`
	}
	windowsRoot := len(roots) > 0 && workspaceWindowsDrivePath(roots[0])
	if len(roots) > 0 && !windowsRoot {
		root := strings.TrimRight(strings.ReplaceAll(roots[0], `\`, "/"), "/")
		const normalized = `rtrim(replace(path,char(92),'/'),'/')`
		query += ` and (` + normalized + `=? or instr(` + normalized + `,?)=1 or instr(?,` + normalized + `||'/')=1)`
		args = append(args, root, root+"/", root+"/")
	}
	// Every failed probe rotates updated_at. Ordering by state first would let
	// sixteen permanently disconnected releases starve every unknown operation.
	query += ` order by updated_at,id limit ?`
	queryLimit := limit
	if windowsRoot && limit > 0 {
		// SQLite lower/NOCASE only fold ASCII. Stream this target's records and
		// apply the Unicode-aware Windows comparison before counting matches.
		// A SQL limit here could hide this workspace behind unrelated records.
		queryLimit = -1
	}
	args = append(args, queryLimit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]workspaceGuardOperationView, 0)
	for rows.Next() {
		var o workspaceGuardOperationView
		if err := rows.Scan(&o.ID, &o.ConnectionKey, &o.Path, &o.State, &o.TargetID, &o.ValidUntil, &o.CreatedAt, &o.UpdatedAt); err != nil {
			return nil, err
		}
		if windowsRoot && !workspaceFailurePathsOverlap(roots[0], o.Path) {
			continue
		}
		result = append(result, o)
		if windowsRoot && limit > 0 && len(result) >= limit {
			break
		}
	}
	return result, rows.Err()
}

// Only a durable writer-exit fact permits release. Unknown operations can be
// reconciled against an already released target token, never forced open.
func (s *Server) reconcileWorkspaceGuardOperation(ctx context.Context, o workspaceGuardOperationView) error {
	route, err := loadSupervisionTargetRoute(ctx, s.db, o.ConnectionKey)
	if errors.Is(err, sql.ErrNoRows) {
		// Older local operations predate persisted local route descriptions.
		candidate := s.supervisionProjectRoute(Project{Runner: s.localRunnerID()})
		if candidate.Key == o.ConnectionKey {
			route, err = candidate, nil
		}
	}
	if err != nil {
		return err
	}
	token := workspaceGuardRecord{ID: o.ID, Kind: "operation", JournalID: o.TargetID, ValidUntil: o.ValidUntil.Time}
	var call func(string) (workspaceGuardRecord, error)
	if route.local() {
		call = func(method string) (workspaceGuardRecord, error) {
			return changeWorkspaceGuard(ctx, s.config, method, token)
		}
	} else {
		link, err := s.connectWorkspaceGuard(ctx, route)
		if err != nil {
			return err
		}
		if o.TargetID != "" && link.TargetID != o.TargetID {
			return targetexec.ErrAuthority
		}
		call = func(method string) (workspaceGuardRecord, error) {
			response, err := link.Call(ctx, targetexec.Request{ID: uuid.NewString(), Method: "guard." + method, Parameters: mustJSON(token)})
			var result workspaceGuardRecord
			if err == nil && (json.Unmarshal(response.Data, &result) != nil || result.ID != o.ID || result.Kind != "operation") {
				err = targetexec.ErrProtocol
			}
			return result, err
		}
	}
	// Legacy rows have no pinned journal identity: an existing token must be
	// observed before release. Missing tokens are not proof of target continuity.
	if o.State != "release_pending" || o.TargetID == "" {
		actual, err := call("query")
		if err != nil {
			return err
		}
		if actual.State != "released" && o.State != "release_pending" {
			return errWorkspaceNotQuiescent
		}
	}
	if o.State == "release_pending" {
		actual, err := call("release")
		if err != nil {
			return err
		}
		if actual.State != "released" {
			return targetexec.ErrProtocol
		}
	}
	_, err = s.db.ExecContext(ctx, `delete from workspace_guard_operations where id=? and state=?`, o.ID, o.State)
	return err
}

func (s *Server) reconcilePendingWorkspaceGuardOperations(ctx context.Context) error {
	operations, err := s.workspaceGuardOperations(ctx, "", true, 16)
	if err != nil {
		return err
	}
	for _, operation := range operations {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		probe, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := s.reconcileWorkspaceGuardOperation(probe, operation)
		cancel()
		if err != nil {
			// Rotate failed rows so one disconnected target cannot starve others.
			_, _ = s.db.ExecContext(ctx, `update workspace_guard_operations set updated_at=? where id=? and state=?`, time.Now().UTC(), operation.ID, operation.State)
		}
	}
	return nil
}

func (s *Server) projectWorkspaceGuardOperations(ctx context.Context, id string) ([]workspaceGuardOperationView, error) {
	project, err := s.getProjectByID(ctx, id)
	if err != nil {
		return nil, err
	}
	route, root, err := s.workspaceGuardTargetPath(project)
	if err != nil {
		return nil, err
	}
	all, err := s.workspaceGuardOperations(ctx, route.Key, false, 1001, root)
	if err != nil {
		return nil, err
	}
	if len(all) > 1000 {
		return nil, targetexec.ErrLimit
	}
	result := make([]workspaceGuardOperationView, 0)
	for _, o := range all {
		if workspaceFailurePathsOverlap(root, o.Path) {
			result = append(result, o)
		}
	}
	return result, nil
}

func (s *Server) workspaceGuardOperationsHTTP(w http.ResponseWriter, r *http.Request) {
	operations, err := s.projectWorkspaceGuardOperations(r.Context(), chi.URLParam(r, "projectID"))
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, operations)
}

func (s *Server) reconcileWorkspaceGuardOperationHTTP(w http.ResponseWriter, r *http.Request) {
	operations, err := s.projectWorkspaceGuardOperations(r.Context(), chi.URLParam(r, "projectID"))
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	for _, o := range operations {
		if o.ID != chi.URLParam(r, "operationID") {
			continue
		}
		if o.State == "active" {
			writeError(w, http.StatusConflict, errors.New("操作仍在执行，不能交还目录保护"))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if err := s.reconcileWorkspaceGuardOperation(ctx, o); err != nil {
			if errors.Is(err, errWorkspaceNotQuiescent) {
				writeError(w, http.StatusConflict, errors.New("目标仍有未确认的执行，已保留目录保护；请先核对原目标上的操作"))
				return
			}
			writeSupervisionHTTPError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"state": "released"})
		return
	}
	writeError(w, http.StatusNotFound, sql.ErrNoRows)
}
