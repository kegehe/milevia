package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

// 20 MiB screenshot + report, driver and the bounded browser-check config.
const supervisionTargetArtifactReserve int64 = 24 << 20

func (r *supervisionTargetRuntime) reserveArtifacts(execution targetexec.Execution) error {
	available, err := supervisionDiskFree(r.storage)
	if err != nil {
		return err
	}
	if available < uint64(supervisionDiskReserve+supervisionTargetArtifactReserve) {
		return errSupervisionDisk
	}
	// The store serializes reservations across all workspaces, and preserves
	// them after restart. Disk-space checks alone cannot prevent over-admission.
	spendable := min(available-uint64(supervisionDiskReserve), uint64(targetexec.MaxArtifactStorageBytes))
	return r.store.ReserveArtifactsWithAvailable(execution.Spec.SessionID, execution.Spec.ExecutionID, supervisionTargetArtifactReserve, int64(spendable), time.Now().UTC())
}

func (r *supervisionTargetRuntime) artifactBytes(executionID string) (int64, error) {
	root, name, err := r.artifactDirectory(executionID)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	dir, err := root.Open(name)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	entries, err := dir.ReadDir(-1)
	dir.Close()
	if err != nil {
		return 0, err
	}
	var bytes int64
	for _, entry := range entries {
		if entry.Name() != "report.json" && entry.Name() != "screenshot.png" {
			return 0, errors.New("目标产物目录含未登记文件，保留完整空间预留")
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return 0, errors.New("目标产物包含链接或非普通文件")
		}
		limit := int64(20 << 20)
		if entry.Name() == "report.json" {
			limit = 256 << 10
		}
		if info.Size() > limit {
			return 0, targetexec.ErrLimit
		}
		bytes += info.Size()
	}
	return bytes, nil
}

// The execution ID is obtained from durable facts before this helper is called.
// Ancestor links are rejected, and rooted operations cannot escape storage.
func (r *supervisionTargetRuntime) artifactDirectory(executionID string) (*os.Root, string, error) {
	root, err := os.OpenRoot(r.storage)
	if err != nil {
		return nil, "", err
	}
	name := filepath.Join("supervision-target", "artifacts", executionID)
	if err := supervisionRootPathSafe(root, filepath.ToSlash(filepath.Join(name, "asset"))); err != nil {
		root.Close()
		return nil, "", err
	}
	return root, name, nil
}

func (r *supervisionTargetRuntime) clearArtifacts(ctx context.Context, request targetexec.Request) (json.RawMessage, error) {
	session, err := r.store.Session(request.SessionID)
	if err != nil {
		return nil, err
	}
	execution, exists := session.Executions[request.ExecutionID]
	if !exists || execution.Spec.WorkspaceID != request.WorkspaceID || session.Control.OwnerEpoch != request.OwnerEpoch || session.Control.Version != request.AuthorizationVersion {
		return nil, targetexec.ErrAuthority
	}
	if err := r.store.BeginArtifactCleanup(request.SessionID, request.ExecutionID); err != nil {
		return nil, err
	}
	facts, err := r.store.Facts()
	if err != nil {
		return nil, err
	}
	if facts.Artifacts[request.ExecutionID].State == "cleared" {
		return json.Marshal(map[string]bool{"cleared": true})
	}
	root, name, err := r.artifactDirectory(request.ExecutionID)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	// Never recursively delete arbitrary output. Unknown files and links keep
	// their allocation charged, and the operator can inspect the exact failure.
	for _, file := range []string{"driver.cjs", "config.json", "report.json", "screenshot.png"} {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path := filepath.Join(name, file)
		info, err := root.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() {
			return nil, errors.New("目标产物清理遇到非普通文件，空间预留保持")
		}
		if err := root.Remove(path); err != nil {
			return nil, err
		}
	}
	if err := root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := r.store.FinishArtifactCleanup(request.SessionID, request.ExecutionID); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]bool{"cleared": true})
}
