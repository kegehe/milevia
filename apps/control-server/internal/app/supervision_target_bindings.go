package app

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

type supervisionTargetBinding struct {
	RunnerID     string    `json:"runnerId"`
	TargetID     string    `json:"targetId"`
	Protocol     int       `json:"protocol"`
	HelperSHA256 string    `json:"helperSha256"`
	TargetOS     string    `json:"targetOs"`
	TargetArch   string    `json:"targetArch"`
	VerifiedAt   time.Time `json:"verifiedAt"`
}

func (s *Server) supervisionTargetBinding(ctx context.Context, runnerID string) (supervisionTargetBinding, error) {
	var out supervisionTargetBinding
	err := s.db.QueryRowContext(ctx, `select runner_id,target_id,protocol,helper_sha256,target_os,target_arch,verified_at from supervision_target_bindings where runner_id=?`, runnerID).Scan(&out.RunnerID, &out.TargetID, &out.Protocol, &out.HelperSHA256, &out.TargetOS, &out.TargetArch, &out.VerifiedAt)
	return out, err
}

// Mapping is a durable observation, never permission to execute or decide done.
// A changed target identity is not silently rebound, even if the Runner name is
// unchanged. Active old work must be reconciled before any explicit remapping.
func (s *Server) recordSupervisionTargetBinding(ctx context.Context, runnerID string, link *supervisionTargetLink) error {
	if runnerID == "" || link == nil || link.TargetID == "" || link.Artifact.OS == "" || link.Artifact.SHA256 == "" {
		return targetexec.ErrConflict
	}
	result, err := s.db.ExecContext(ctx, `insert into supervision_target_bindings(runner_id,target_id,protocol,helper_sha256,target_os,target_arch,verified_at) values (?,?,?,?,?,?,?)
		on conflict(runner_id) do update set helper_sha256=excluded.helper_sha256,verified_at=excluded.verified_at
		where supervision_target_bindings.target_id=excluded.target_id and supervision_target_bindings.protocol=excluded.protocol and supervision_target_bindings.target_os=excluded.target_os and supervision_target_bindings.target_arch=excluded.target_arch`, runnerID, link.TargetID, targetexec.ProtocolVersion, link.Artifact.SHA256, link.Artifact.OS, link.Artifact.Arch, time.Now().UTC())
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return errors.New("目标持久身份、协议或平台已改变，原映射保持保护")
	}
	return nil
}

func (s *Server) connectSupervisionTarget(ctx context.Context, runnerID string) (*supervisionTargetLink, error) {
	pool, err := s.supervisionTargetConnections()
	if err != nil {
		return nil, err
	}
	return pool.acquire(ctx, runnerID)
}

func (s *Server) openSupervisionTargetBinding(ctx context.Context, runnerID string) (*supervisionTargetLink, error) {
	runner, installationRunner, err := s.supervisionTargetRouteRunner(ctx, runnerID)
	if err != nil {
		return nil, err
	}
	prior, err := s.supervisionTargetBinding(ctx, runnerID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil && prior.Protocol != targetexec.ProtocolVersion {
		return nil, targetexec.ErrProtocol
	}
	installed, err := s.loadAgentInstallations(ctx, installationRunner)
	if err != nil {
		return nil, err
	}
	paths := map[string]string{}
	for agentID, path := range installed {
		if _, known := agentByID(agentID); known {
			paths[agentID] = path
		}
	}
	link, err := openSupervisionTarget(ctx, s.config, runner, prior.TargetID)
	if err != nil {
		return nil, err
	}
	if err := s.recordSupervisionTargetBinding(ctx, runnerID, link); err != nil {
		link.Close()
		return nil, err
	}
	if _, err := link.Call(ctx, targetexec.Request{ID: uuid.NewString(), Method: "configure.paths", Parameters: mustJSON(paths)}); err != nil {
		link.Close()
		return nil, err
	}
	return link, nil
}
