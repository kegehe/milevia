package app

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

func TestSupervisionTargetSnapshotTransfersLargeBlobAndRejectsCorruptionOrDrift(t *testing.T) {
	for _, mode := range []string{"complete", "corrupt-chunk", "changed-after-chunk"} {
		t.Run(mode, func(t *testing.T) {
			r, control, root := targetRuntimeFixture(t)
			content := strings.Repeat("large source line\n", 42000)
			if err := os.WriteFile(filepath.Join(root, "large.txt"), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			supervisor, err := targetexec.NewSupervisor(context.Background(), r.store, r)
			if err != nil {
				t.Fatal(err)
			}
			serverPipe, clientPipe := net.Pipe()
			done := make(chan error, 1)
			transport := &targetexec.Transport{Store: r.store, Supervisor: supervisor, Extension: func(ctx context.Context, request targetexec.Request) (json.RawMessage, error) {
				data, err := r.handle(ctx, request)
				if err != nil || request.Method != "blob" {
					return data, err
				}
				var page supervisionTargetBlobPage
				if json.Unmarshal(data, &page) != nil {
					return nil, targetexec.ErrProtocol
				}
				if mode == "corrupt-chunk" && len(page.Data) > 0 {
					page.Data[0] ^= 1
					return mustJSON(page), nil
				}
				if mode == "changed-after-chunk" && page.Total > 512<<10 && page.Offset+int64(len(page.Data)) == page.Total {
					if err := os.WriteFile(filepath.Join(root, "entry.js"), []byte("externally changed"), 0600); err != nil {
						return nil, err
					}
				}
				return data, nil
			}}
			go func() { done <- transport.Serve(context.Background(), serverPipe) }()
			client := targetexec.NewClient(clientPipe)
			defer func() {
				client.Close()
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(3 * time.Second):
					t.Error("snapshot transport did not settle")
				}
			}()
			// The entire transfer performs several double scans under -race;
			// production per-request and finite grant limits remain unchanged.
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if _, err := client.Call(ctx, targetexec.Request{ID: "hello", Method: "hello"}); err != nil {
				t.Fatal(err)
			}
			link := &supervisionTargetLink{Client: client, TargetID: control.TargetID, Artifact: targetexec.HelperArtifact{OS: runtime.GOOS}}
			collected, err := collectSupervisionTargetWorkspace(ctx, link, control)
			if mode != "complete" {
				if err == nil || len(collected.Blobs) != 0 {
					t.Fatal("incomplete remote capture entered CAS", mode, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(collected.Blobs[supervisionSHA256([]byte(content))]) != content || string(collected.Blobs[supervisionSHA256([]byte("before"))]) != "before" {
				t.Fatal("remote source truncated or lost")
			}
		})
	}
}

func TestSupervisionTargetBlobRequiresSnapshotGrantAndBoundedOffset(t *testing.T) {
	r, control, _ := targetRuntimeFixture(t)
	raw, err := r.handle(context.Background(), targetRequest(control, "snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	var capture supervisionTargetCapture
	if json.Unmarshal(raw, &capture) != nil {
		t.Fatal("invalid snapshot fixture")
	}
	valid := supervisionTargetBlobRequest{RevisionID: capture.RevisionID, RulesHash: capture.RulesHash, SHA256: supervisionSHA256([]byte("before")), Limit: 2}
	for _, mode := range []string{"valid", "negative", "oversize", "beyond-end", "unknown-blob", "wrong-snapshot", "revoked"} {
		query := valid
		request := targetRequest(control, "blob")
		switch mode {
		case "negative":
			query.Offset = -1
		case "oversize":
			query.Limit = 257 << 10
		case "beyond-end":
			query.Offset = 7
		case "unknown-blob":
			query.SHA256 = strings.Repeat("a", 64)
		case "wrong-snapshot":
			query.RevisionID = strings.Repeat("b", 64)
		case "revoked":
			request.AuthorizationVersion++
		}
		request.Parameters = mustJSON(query)
		data, err := r.handle(context.Background(), request)
		if mode != "valid" {
			if err == nil {
				t.Fatal("invalid source chunk authorized", mode)
			}
			continue
		}
		var page supervisionTargetBlobPage
		if err != nil || json.Unmarshal(data, &page) != nil || string(page.Data) != "be" || page.Total != 6 {
			t.Fatal("bounded chunk invalid", string(data), err)
		}
	}
}
