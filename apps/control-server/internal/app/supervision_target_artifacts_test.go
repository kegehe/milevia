package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

func TestSupervisionTargetArtifactRecoveryBindsOnlyRecordedAssets(t *testing.T) {
	runtime, control, _ := targetRuntimeFixture(t)
	now := time.Now().UTC()
	spec := targetexec.ExecutionSpec{Protocol: 1, TargetID: control.TargetID, SessionID: control.SessionID, ExecutionID: "browser", WorkItemID: "browser-work", Attempt: 1, OwnerEpoch: 1, AuthorizationVersion: 1, WorkspaceID: control.WorkspaceID, Role: "test", AgentID: "milevia", Contract: supervisionTargetVerificationContract, InputSHA256: strings.Repeat("a", 64), InstructionsSHA256: strings.Repeat("b", 64), PreparedAt: now, TurnDeadline: now.Add(time.Minute)}
	execution, err := runtime.store.Prepare(spec, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.store.ClaimStart(control.SessionID, spec.ExecutionID, execution.Digest, now); err != nil {
		t.Fatal(err)
	}
	image := []byte("\x89PNG\r\n\x1a\nrecorded-test-image")
	result := supervisionCheckResult{Check: supervisionCheck{Browser: &supervisionBrowserCheck{}}, BrowserResult: json.RawMessage(`{"passed":true,"api_key":"must-not-leak"}`), ScreenshotSHA256: supervisionSHA256(image)}
	payload, err := supervisionTargetPayload(mustJSON(result))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.store.AppendEvent(control.SessionID, spec.ExecutionID, "browser-result", "verification.result", payload); err != nil {
		t.Fatal(err)
	}
	if err := runtime.store.RecordQuiet(control.SessionID, spec.ExecutionID, execution.Digest, supervisionSHA256(payload), false, targetexec.Usage{}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(runtime.storage, "supervision-target", "artifacts", spec.ExecutionID, "screenshot.png")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, image, 0600); err != nil {
		t.Fatal(err)
	}
	control.State, control.Version = "stopped", 2
	if err := runtime.store.ApplyControl(control, now); err != nil {
		t.Fatal(err)
	}
	if err := runtime.store.Release(control.SessionID, control.OwnerEpoch, control.Version); err != nil {
		t.Fatal(err)
	}
	request := targetRequest(control, "artifact")
	request.ExecutionID = spec.ExecutionID
	read := func(input supervisionTargetArtifactRead) (supervisionTargetArtifactPage, error) {
		request.Parameters = mustJSON(input)
		raw, err := runtime.handle(context.Background(), request)
		var page supervisionTargetArtifactPage
		if err == nil {
			err = json.Unmarshal(raw, &page)
		}
		return page, err
	}
	var received []byte
	for {
		page, err := read(supervisionTargetArtifactRead{Asset: "screenshot", Offset: int64(len(received)), Limit: 7})
		if err != nil || page.SHA256 != result.ScreenshotSHA256 || page.Offset != int64(len(received)) || page.NextOffset != page.Offset+int64(len(page.Data)) {
			t.Fatal("asset page lost durable binding", page, err)
		}
		received = append(received, page.Data...)
		if page.EOF {
			break
		}
	}
	if string(received) != string(image) {
		t.Fatal("artifact was truncated or duplicated")
	}
	report, err := read(supervisionTargetArtifactRead{Asset: "report", Limit: 4096})
	if err != nil || !report.EOF || strings.Contains(string(report.Data), "must-not-leak") || !strings.Contains(string(report.Data), "[REDACTED]") {
		t.Fatal("report was not the redacted durable evidence", string(report.Data), err)
	}
	for _, input := range []supervisionTargetArtifactRead{{Asset: "../facts.json", Limit: 7}, {Asset: "report", Offset: -1, Limit: 7}, {Asset: "screenshot", Limit: supervisionTargetArtifactChunk + 1}} {
		if _, err := read(input); !errors.Is(err, targetexec.ErrConflict) {
			t.Fatal("invalid asset access accepted", input, err)
		}
	}
	if err := os.WriteFile(path, []byte("replaced by user after verification"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := read(supervisionTargetArtifactRead{Asset: "screenshot", Limit: 7}); err == nil {
		t.Fatal("unbound replacement served as original screenshot")
	}
}

func TestSupervisionTargetArtifactCleanupChargesUnknownFilesAndNeverRepeatsDeletion(t *testing.T) {
	runtime, control, _ := targetRuntimeFixture(t)
	now := time.Now().UTC()
	spec := targetexec.ExecutionSpec{Protocol: 1, TargetID: control.TargetID, SessionID: control.SessionID, ExecutionID: "browser-cleanup", WorkItemID: "browser-work", Attempt: 1, OwnerEpoch: 1, AuthorizationVersion: 1, WorkspaceID: control.WorkspaceID, Role: "test", AgentID: "milevia", Contract: supervisionTargetVerificationContract, InputSHA256: strings.Repeat("a", 64), InstructionsSHA256: strings.Repeat("b", 64), PreparedAt: now, TurnDeadline: now.Add(time.Minute)}
	execution, err := runtime.store.Prepare(spec, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.store.ClaimStart(control.SessionID, spec.ExecutionID, execution.Digest, now); err != nil {
		t.Fatal(err)
	}
	if err := runtime.reserveArtifacts(execution); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(runtime.storage, "supervision-target", "artifacts", spec.ExecutionID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"screenshot.png": "image", "report.json": "{}", "unknown.txt": "keep"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runtime.artifactBytes(spec.ExecutionID); err == nil {
		t.Fatal("unknown output was not charged conservatively")
	}
	if err := runtime.store.RecordQuiet(control.SessionID, spec.ExecutionID, execution.Digest, "", false, targetexec.Usage{}); err != nil {
		t.Fatal(err)
	}
	request := targetRequest(control, "artifact.clear")
	request.ExecutionID = spec.ExecutionID
	if _, err := runtime.handle(context.Background(), request); !errors.Is(err, targetexec.ErrNotQuiet) {
		t.Fatal("live session evidence could be removed", err)
	}
	control.State, control.Version = "stopped", 2
	if err := runtime.store.ApplyControl(control, now); err != nil {
		t.Fatal(err)
	}
	if err := runtime.store.Release(control.SessionID, 1, 2); err != nil {
		t.Fatal(err)
	}
	request = targetRequest(control, "artifact.clear")
	request.ExecutionID = spec.ExecutionID
	if _, err := runtime.handle(context.Background(), request); err == nil {
		t.Fatal("cleanup deleted unregistered files")
	}
	facts, _ := runtime.store.Facts()
	if facts.Artifacts[spec.ExecutionID].Bytes != supervisionTargetArtifactReserve {
		t.Fatal("failed cleanup freed charged storage")
	}
	if data, err := os.ReadFile(filepath.Join(dir, "unknown.txt")); err != nil || string(data) != "keep" {
		t.Fatal("unknown artifact was deleted", err)
	}
	if err := os.Remove(filepath.Join(dir, "unknown.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.handle(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "report.json")
	if err := os.WriteFile(path, []byte("new user file after cleanup"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.handle(context.Background(), request); err != nil {
		t.Fatal("cleanup receipt was not idempotent", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "new user file after cleanup" {
		t.Fatal("repeated cleanup deleted a new file", err)
	}
}
