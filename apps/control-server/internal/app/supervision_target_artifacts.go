package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

const supervisionTargetArtifactChunk = 128 << 10

type supervisionTargetArtifactRead struct {
	Asset  string `json:"asset"`
	Offset int64  `json:"offset"`
	Limit  int    `json:"limit"`
}

type supervisionTargetArtifactPage struct {
	Asset      string `json:"asset"`
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size"`
	Offset     int64  `json:"offset"`
	NextOffset int64  `json:"nextOffset"`
	Data       []byte `json:"data"`
	EOF        bool   `json:"eof"`
}

// Only assets bound to a durable, quiescent verification result are readable.
// Read-only recovery remains possible after pause/stop/release; it does not
// grant execution or arbitrary filesystem access. Missing event prefixes and
// changed artifacts fail explicitly rather than producing partial evidence.
func (r *supervisionTargetRuntime) handleArtifact(ctx context.Context, request targetexec.Request) (json.RawMessage, error) {
	var input supervisionTargetArtifactRead
	if json.Unmarshal(request.Parameters, &input) != nil || input.Offset < 0 || input.Limit < 1 || input.Limit > supervisionTargetArtifactChunk || input.Asset != "report" && input.Asset != "screenshot" {
		return nil, targetexec.ErrConflict
	}
	session, err := r.store.Session(request.SessionID)
	if err != nil {
		return nil, err
	}
	execution, exists := session.Executions[request.ExecutionID]
	if !exists || execution.Spec.WorkspaceID != request.WorkspaceID || execution.Spec.Role != "test" || execution.State != "quiescent" || execution.ResultSHA256 == "" {
		return nil, targetexec.ErrConflict
	}
	var result supervisionCheckResult
	found := false
	var after int64
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, err := r.store.EventsAfter(request.SessionID, after, 100)
		if err != nil {
			return nil, err
		}
		for _, event := range page.Events {
			after = event.Sequence
			if event.ExecutionID == request.ExecutionID && event.Type == "verification.result" && supervisionSHA256(event.Payload) == execution.ResultSHA256 {
				if json.Unmarshal(event.Payload, &result) != nil || result.Check.Browser == nil {
					return nil, targetexec.ErrConflict
				}
				found = true
				break
			}
		}
		if found || !page.More {
			break
		}
	}
	if !found {
		return nil, targetexec.ErrCursor
	}
	var content []byte
	var expected string
	if input.Asset == "report" {
		// Serve the redacted report actually bound into the result, never an
		// unredacted browser log that happens to remain in the artifacts folder.
		if len(result.BrowserResult) == 0 || len(result.BrowserResult) >= 256<<10 || !json.Valid(result.BrowserResult) {
			return nil, errors.New("目标浏览器报告不可用")
		}
		content = result.BrowserResult
		expected = supervisionSHA256(content)
	} else {
		if result.ScreenshotSHA256 == "" {
			return nil, errors.New("目标截图不可用")
		}
		root, err := os.OpenRoot(r.storage)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		name := filepath.Join("supervision-target", "artifacts", request.ExecutionID, "screenshot.png")
		if err := supervisionRootPathSafe(root, filepath.ToSlash(name)); err != nil {
			return nil, err
		}
		info, err := root.Lstat(name)
		if err != nil || !info.Mode().IsRegular() || info.Size() < 8 || info.Size() > 20<<20 {
			return nil, errors.New("目标截图缺失、已清理或超出产物限制")
		}
		file, err := root.Open(name)
		if err != nil {
			return nil, err
		}
		content, err = io.ReadAll(io.LimitReader(file, (20<<20)+1))
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			return nil, errors.Join(err, closeErr)
		}
		expected = result.ScreenshotSHA256
		if len(content) > 20<<20 || supervisionSHA256(content) != expected {
			return nil, errors.New("目标截图与持久化证据不符")
		}
	}
	if input.Offset > int64(len(content)) {
		return nil, targetexec.ErrConflict
	}
	end := min(input.Offset+int64(input.Limit), int64(len(content)))
	return json.Marshal(supervisionTargetArtifactPage{Asset: input.Asset, SHA256: expected, Size: int64(len(content)), Offset: input.Offset, NextOffset: end, Data: content[input.Offset:end], EOF: end == int64(len(content))})
}
