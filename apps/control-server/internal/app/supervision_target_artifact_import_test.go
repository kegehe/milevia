package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

func TestSupervisionTargetArtifactImportRejectsIncompleteOrChangedChunks(t *testing.T) {
	content := []byte(strings.Repeat("artifact-fixture", 20000))
	execution := targetexec.Execution{Spec: targetexec.ExecutionSpec{TargetID: "target", SessionID: "session", ExecutionID: "execution", WorkspaceID: "workspace"}}
	for _, scenario := range []string{"valid", "wrong_hash", "wrong_asset", "wrong_offset", "changed_size", "early_eof", "no_progress", "oversized", "tampered_data"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			link := &supervisionTargetLink{TargetID: "target", call: func(_ context.Context, request targetexec.Request) (targetexec.Response, error) {
				calls++
				var query supervisionTargetArtifactRead
				if request.Method != "artifact" || request.SessionID != "session" || request.ExecutionID != "execution" || request.WorkspaceID != "workspace" || json.Unmarshal(request.Parameters, &query) != nil {
					t.Fatal("artifact request lost execution binding")
				}
				end := min(query.Offset+int64(query.Limit), int64(len(content)))
				page := supervisionTargetArtifactPage{Asset: "screenshot", SHA256: supervisionSHA256(content), Size: int64(len(content)), Offset: query.Offset, NextOffset: end, Data: append([]byte{}, content[query.Offset:end]...), EOF: end == int64(len(content))}
				switch scenario {
				case "wrong_hash":
					page.SHA256 = strings.Repeat("a", 64)
				case "wrong_asset":
					page.Asset = "report"
				case "wrong_offset":
					page.Offset++
				case "changed_size":
					if calls > 1 {
						page.Size++
					}
				case "early_eof":
					page.EOF = true
				case "no_progress":
					page.Data, page.NextOffset = nil, page.Offset
				case "oversized":
					page.Size = 21 << 20
				case "tampered_data":
					page.Data[0] ^= 1
				}
				return targetexec.Response{Data: mustJSON(page)}, nil
			}}
			actual, err := readSupervisionTargetArtifact(context.Background(), link, execution, "screenshot", supervisionSHA256(content), 20<<20)
			if scenario == "valid" {
				if err != nil || string(actual) != string(content) || calls < 2 {
					t.Fatal("complete artifact was truncated or duplicated", calls, err)
				}
			} else if err == nil || actual != nil {
				t.Fatal("invalid artifact accepted as complete", scenario)
			}
		})
	}
}

func TestSupervisionImportedArtifactNeverReplacesConflictingFile(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if pending, err := writeSupervisionImportedArtifact(root, "report.json", []byte("original")); err != nil || pending {
		t.Fatal(err)
	}
	if pending, err := writeSupervisionImportedArtifact(root, "report.json", []byte("original")); err != nil || pending {
		t.Fatal("identical import failed", err)
	}
	if _, err := writeSupervisionImportedArtifact(root, "report.json", []byte("replaced")); err == nil {
		t.Fatal("conflicting artifact replaced")
	}
	actual, err := os.ReadFile(filepath.Join(dir, "report.json"))
	if err != nil || string(actual) != "original" {
		t.Fatal("conflicting import changed original", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatal("import left unaccounted temporary files", entries, err)
	}
}
