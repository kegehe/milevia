package app

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

type supervisionRequestContextKey struct{}
type supervisionRequestIdentity struct{ Scope, Key, Hash string }

func supervisionRequestIdentityFromContext(ctx context.Context) *supervisionRequestIdentity {
	value, _ := ctx.Value(supervisionRequestContextKey{}).(*supervisionRequestIdentity)
	return value
}

func (s *Server) migrateSupervisionRequests(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `create table if not exists supervision_requests (
	 scope text not null, request_key text not null, request_hash text not null,
	 session_id text not null references supervision_sessions(id) on delete cascade,
	 created_at datetime not null, primary key(scope,request_key)
	);create index if not exists supervision_requests_session on supervision_requests(session_id);`)
	return err
}

func supervisionRequestReceipt(ctx context.Context, q sqlQueryer) (string, bool, error) {
	identity := supervisionRequestIdentityFromContext(ctx)
	if identity == nil {
		return "", false, nil
	}
	var sessionID, hash string
	err := q.QueryRowContext(ctx, `select session_id,request_hash from supervision_requests where scope=? and request_key=?`, identity.Scope, identity.Key).Scan(&sessionID, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if hash != identity.Hash {
		return "", false, errors.New("同一请求标识不能用于不同内容，请重新提交操作")
	}
	return sessionID, true, nil
}

func recordSupervisionRequestTx(ctx context.Context, tx *sql.Tx, sessionID string) error {
	identity := supervisionRequestIdentityFromContext(ctx)
	if identity == nil {
		return nil
	}
	_, err := tx.ExecContext(ctx, `insert into supervision_requests(scope,request_key,request_hash,session_id,created_at) values (?,?,?,?,?)`, identity.Scope, identity.Key, identity.Hash, sessionID, time.Now().UTC())
	return err
}

// The receipt and the domain mutation commit together. A failed transaction
// leaves no receipt, so a safe retry can perform the operation exactly once.
func (s *Server) withSupervisionRequestTx(ctx context.Context, id string, now time.Time, fn func(*sql.Tx, map[string]WorkspaceAdmission, *supervisionSession) error) error {
	return s.withSupervisionTx(ctx, id, now, func(tx *sql.Tx, next map[string]WorkspaceAdmission, ss *supervisionSession) error {
		previous, replay, err := supervisionRequestReceipt(ctx, tx)
		if err != nil {
			return err
		}
		if replay {
			if previous != id {
				return errSupervisionState
			}
			return nil
		}
		if err := fn(tx, next, ss); err != nil {
			return err
		}
		return recordSupervisionRequestTx(ctx, tx, id)
	})
}

func (s *Server) supervisionRequestMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			next.ServeHTTP(w, r)
			return
		}
		if len(key) > 128 || strings.IndexFunc(key, func(c rune) bool { return c < 33 || c > 126 }) >= 0 {
			writeError(w, http.StatusBadRequest, errors.New("请求标识必须为最多128个可见ASCII字符"))
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes))
		if err != nil {
			writeError(w, http.StatusRequestEntityTooLarge, errors.New("托管请求内容超过上限"))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		identity := &supervisionRequestIdentity{Scope: r.Method + " " + r.URL.EscapedPath(), Key: key, Hash: supervisionSHA256(body)}
		r = r.WithContext(context.WithValue(r.Context(), supervisionRequestContextKey{}, identity))
		id, replay, err := supervisionRequestReceipt(r.Context(), s.db)
		if err != nil {
			writeSupervisionHTTPError(w, err)
			return
		}
		if !replay {
			next.ServeHTTP(w, r)
			return
		}
		ss, err := loadSupervisionSession(r.Context(), s.db, id)
		if err != nil {
			writeSupervisionHTTPError(w, err)
			return
		}
		w.Header().Set("Idempotency-Replayed", "true")
		switch {
		case r.Method == http.MethodPut:
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/start"), strings.HasSuffix(r.URL.Path, "/control"):
			writeJSON(w, http.StatusAccepted, ss)
		case strings.HasSuffix(r.URL.Path, "/supervision"):
			writeJSON(w, http.StatusCreated, ss)
		default:
			writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
		}
	})
}
