package app

import (
	"database/sql"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

func (s *Server) supervisionArchiveHTTP(w http.ResponseWriter, r *http.Request) {
	var input struct {
		StateVersion int64 `json:"stateVersion"`
		Archived     bool  `json:"archived"`
	}
	if !decode(w, r, &input) {
		return
	}
	id := chi.URLParam(r, "supervisionID")
	now := time.Now().UTC()
	err := s.withSupervisionRequestTx(r.Context(), id, now, func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		if err := requireSupervisionVersion(ss, input.StateVersion); err != nil {
			return err
		}
		if err := requireSupervisionReleasedTx(r.Context(), tx, *ss); err != nil {
			return err
		}
		if (ss.ArchivedAt != nil) == input.Archived {
			return nil
		}
		typ := "session.unarchived"
		ss.ArchivedAt = nil
		if input.Archived {
			ss.ArchivedAt = &now
			typ = "session.archived"
		}
		ss.StateVersion++
		return appendSupervisionEventTx(r.Context(), tx, ss, typ, map[string]bool{"archived": input.Archived}, now)
	})
	if err != nil {
		writeSupervisionHTTPError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
