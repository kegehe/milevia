package app

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// RunnerAgent is the execution-site capability required before managed
// credentials may cross a Runner boundary. The control server never sends a
// plaintext secret through this interface; the target agent resolves its own
// local credential store and owns quota reservations.
type RunnerAgent interface {
	Capability(ctx context.Context) (RunnerAgentCapability, error)
	ReserveStart(ctx context.Context, request RunnerAgentReservation) error
	Finish(ctx context.Context, runID string, status string) error
}

type RunnerAgentCapability struct {
	ProtocolVersion string `json:"protocolVersion"`
	RunnerID        string `json:"runnerId"`
	ManagedSecrets  bool   `json:"managedSecrets"`
	QuotaScheduling bool   `json:"quotaScheduling"`
}

type RunnerAgentReservation struct {
	RunID                 string    `json:"runId"`
	ProjectID             string    `json:"projectId"`
	RouteRevisionID       string    `json:"routeRevisionId"`
	ProfileRevisionID     string    `json:"profileRevisionId"`
	RemoteRouteRevisionID string    `json:"remoteRouteRevisionId"`
	LeaseUntil            time.Time `json:"leaseUntil"`
}

var errRunnerAgentUnavailable = errors.New("target Runner Agent is unavailable")

type localRunnerAgent struct{ server *Server }

func (s *Server) runnerAgentCapability(w http.ResponseWriter, r *http.Request) {
	runnerID := chi.URLParam(r, "runnerID")
	if runnerID != s.localRunnerID() {
		writeError(w, http.StatusConflict, errRunnerAgentUnavailable)
		return
	}
	capability, err := (localRunnerAgent{server: s}).Capability(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusOK, capability)
}

func (a localRunnerAgent) Capability(context.Context) (RunnerAgentCapability, error) {
	return RunnerAgentCapability{ProtocolVersion: "1", RunnerID: a.server.localRunnerID(), ManagedSecrets: true, QuotaScheduling: true}, nil
}
func (a localRunnerAgent) ReserveStart(ctx context.Context, request RunnerAgentReservation) error {
	if request.RunID == "" || request.ProfileRevisionID == "" {
		return errors.New("runner reservation requires a run and profile revision")
	}
	tx, err := a.server.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := a.server.reserveProfileQuotaTx(ctx, tx, request.RunID, request.ProfileRevisionID); err != nil {
		return err
	}
	return tx.Commit()
}
func (a localRunnerAgent) Finish(ctx context.Context, runID, status string) error {
	tx, err := a.server.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := a.server.releaseQuotaReservations(ctx, tx, runID); err != nil {
		return err
	}
	return tx.Commit()
}
