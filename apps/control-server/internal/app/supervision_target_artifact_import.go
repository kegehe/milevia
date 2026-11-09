package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

func readSupervisionTargetArtifact(ctx context.Context, link *supervisionTargetLink, execution targetexec.Execution, asset, expected string, limit int64) ([]byte, error) {
	if link == nil || link.TargetID != execution.Spec.TargetID || len(expected) != 64 || limit < 1 || limit > 20<<20 {
		return nil, targetexec.ErrConflict
	}
	var content []byte
	var size int64 = -1
	for {
		response, err := link.Call(ctx, targetexec.Request{ID: uuid.NewString(), Method: "artifact", SessionID: execution.Spec.SessionID, ExecutionID: execution.Spec.ExecutionID, WorkspaceID: execution.Spec.WorkspaceID, Parameters: mustJSON(supervisionTargetArtifactRead{Asset: asset, Offset: int64(len(content)), Limit: supervisionTargetArtifactChunk})})
		if err != nil {
			return nil, err
		}
		var page supervisionTargetArtifactPage
		if json.Unmarshal(response.Data, &page) != nil || page.Asset != asset || page.SHA256 != expected || page.Size < 1 || page.Size > limit || size != -1 && page.Size != size || page.Offset != int64(len(content)) || page.NextOffset != page.Offset+int64(len(page.Data)) || page.NextOffset > page.Size || len(page.Data) == 0 || len(page.Data) > supervisionTargetArtifactChunk || page.EOF != (page.NextOffset == page.Size) {
			return nil, targetexec.ErrProtocol
		}
		size = page.Size
		content = append(content, page.Data...)
		if page.EOF {
			if int64(len(content)) != size || supervisionSHA256(content) != expected {
				return nil, targetexec.ErrConflict
			}
			return content, nil
		}
	}
}

func (s *Server) importSupervisionTargetArtifacts(ctx context.Context, link *supervisionTargetLink, permit supervisionExecutionPermit, outcome supervisionTargetOutcome) (resultErr error) {
	if outcome.Check == nil || outcome.Check.Check.Browser == nil {
		return targetexec.ErrConflict
	}
	result := outcome.Check
	if len(result.BrowserResult) == 0 && result.ScreenshotSHA256 == "" && outcome.RunError != nil {
		return nil // A check that never ran has no artifacts to import.
	}
	if len(result.BrowserResult) == 0 || len(result.BrowserResult) >= 256<<10 || !json.Valid(result.BrowserResult) || result.ScreenshotSHA256 == "" {
		return errors.New("目标浏览器报告或截图不完整，不能验收")
	}
	if err := s.supervisionDiskAvailable(supervisionTargetArtifactReserve); err != nil {
		return err
	}
	if err := s.withSupervisionTx(ctx, permit.Decision.SessionID, time.Now().UTC(), func(tx *sql.Tx, _ map[string]WorkspaceAdmission, ss *supervisionSession) error {
		state, err := checkSupervisionExecutionIdentityTx(ctx, tx, ss.ID, permit)
		if err != nil {
			return err
		}
		if state != "quiescent" || verifySupervisionTargetExecutionFact(ctx, tx, outcome.Execution) != nil {
			return errWorkspaceNotQuiescent
		}
		var cleaned bool
		if err := tx.QueryRowContext(ctx, `select exists(select 1 from supervision_artifacts where execution_id=? and state='cleaned')`, permit.Decision.ExecutionID).Scan(&cleaned); err != nil {
			return err
		}
		if cleaned {
			return errors.New("目标产物已被明确清理，不重新创建")
		}
		_, err = tx.ExecContext(ctx, `insert into supervision_storage_reservations(execution_id,session_id,bytes) values (?,?,?) on conflict(execution_id) do nothing`, permit.Decision.ExecutionID, ss.ID, supervisionTargetArtifactReserve)
		return err
	}); err != nil {
		return err
	}
	storageUnconfirmed := false
	defer func() {
		if resultErr != nil && !storageUnconfirmed {
			// Completed fixed-name files remain inspectable and charged even
			// when a later transfer fails. If there are none, release the unused
			// reservation. An unconfirmed temporary file retains its reservation.
			settle, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			resultErr = errors.Join(resultErr, s.registerSupervisionArtifacts(settle, permit.Decision.SessionID, permit.Decision.ExecutionID))
		}
	}()
	report, err := readSupervisionTargetArtifact(ctx, link, outcome.Execution, "report", supervisionSHA256(result.BrowserResult), 256<<10)
	if err != nil {
		return err
	}
	if !bytes.Equal(report, result.BrowserResult) {
		return targetexec.ErrConflict
	}
	image, err := readSupervisionTargetArtifact(ctx, link, outcome.Execution, "screenshot", result.ScreenshotSHA256, 20<<20)
	if err != nil {
		return err
	}
	if len(image) < 8 || !bytes.Equal(image[:8], []byte{137, 80, 78, 71, 13, 10, 26, 10}) {
		return errors.New("目标浏览器截图格式无效")
	}
	root, err := os.OpenRoot(s.checkpointStorageRoot())
	if err != nil {
		return err
	}
	defer root.Close()
	for _, item := range []struct {
		asset string
		data  []byte
	}{{"report.json", report}, {"screenshot.png", image}} {
		name := filepath.Join("supervision", "artifacts", permit.Decision.ExecutionID, item.asset)
		if err := supervisionRootPathSafe(root, filepath.ToSlash(name)); err != nil {
			return err
		}
		if err := root.MkdirAll(filepath.Dir(name), 0700); err != nil {
			return err
		}
		if err := supervisionRootPathSafe(root, filepath.ToSlash(name)); err != nil {
			return err
		}
		unconfirmed, err := writeSupervisionImportedArtifact(root, name, item.data)
		storageUnconfirmed = storageUnconfirmed || unconfirmed
		if err != nil {
			return err
		}
	}
	return s.registerSupervisionArtifacts(ctx, permit.Decision.SessionID, permit.Decision.ExecutionID)
}

func writeSupervisionImportedArtifact(root *os.Root, name string, data []byte) (unconfirmed bool, resultErr error) {
	if info, err := root.Lstat(name); err == nil {
		if !info.Mode().IsRegular() || info.Size() != int64(len(data)) {
			return false, targetexec.ErrConflict
		}
		original, err := root.ReadFile(name)
		if err != nil || !bytes.Equal(original, data) {
			return false, targetexec.ErrConflict
		}
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	temporary := name + "." + uuid.NewString() + ".tmp"
	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return false, err
	}
	defer func() {
		if err := root.Remove(temporary); err != nil && !errors.Is(err, os.ErrNotExist) {
			unconfirmed, resultErr = true, errors.Join(resultErr, err)
		}
	}()
	_, writeErr := io.Copy(file, bytes.NewReader(data))
	syncErr, closeErr := file.Sync(), file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return false, err
	}
	// Recheck a concurrently imported destination; retries may share the same
	// content, but must never overwrite a conflicting existing artifact.
	if info, err := root.Lstat(name); err == nil {
		if !info.Mode().IsRegular() || info.Size() != int64(len(data)) {
			return false, targetexec.ErrConflict
		}
		original, err := root.ReadFile(name)
		if err != nil || !bytes.Equal(original, data) {
			return false, targetexec.ErrConflict
		}
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := root.Link(temporary, name); errors.Is(err, os.ErrExist) {
		info, statErr := root.Lstat(name)
		if statErr != nil || !info.Mode().IsRegular() || info.Size() != int64(len(data)) {
			return false, targetexec.ErrConflict
		}
		original, readErr := root.ReadFile(name)
		if readErr != nil || !bytes.Equal(original, data) {
			return false, targetexec.ErrConflict
		}
		return false, nil
	} else {
		return false, err // Atomic no-replace publication within the storage FS.
	}
}
