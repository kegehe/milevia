//go:build windows

package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestSupervisionTargetReleaseGuardsArchiveAndArtifactCleanupAPI(t *testing.T) {
	for _, missingAdmission := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing_admission_%t", missingAdmission), func(t *testing.T) {
			s, _, _, ss, _ := supervisionTargetHandoffFixture(t, nil)
			ctx := context.Background()
			if _, err := s.advanceSupervisionTargetHandoff(ctx, ss.ID, ss.LeaseOwner); err != nil {
				t.Fatal(err)
			}
			ss = supervisionSessionForTest(t, s, ss.ID)
			var executionID string
			if err := s.db.QueryRow(`select execution_id from supervision_role_jobs where session_id=? limit 1`, ss.ID).Scan(&executionID); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(s.checkpointStorageRoot(), "supervision", "artifacts", executionID, "report.json")
			if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
				t.Fatal(err)
			}
			data := []byte(`{"passed":true}`)
			if err := os.WriteFile(file, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`insert into supervision_artifacts(execution_id,session_id,asset,size,sha256,state) values (?,?,'report.json',?,?,'present')`, executionID, ss.ID, len(data), supervisionSHA256(data)); err != nil {
				t.Fatal(err)
			}
			if missingAdmission {
				if _, err := s.db.Exec(`delete from workspace_admissions where id=?`, ss.AdmissionID); err != nil {
					t.Fatal(err)
				}
			}
			archivePath := "/api/supervision/" + ss.ID + "/archive"
			artifactPath := "/api/supervision/" + ss.ID + "/artifacts"
			body := fmt.Sprintf(`{"stateVersion":%d,"archived":true}`, ss.StateVersion)
			if response := supervisionRequestForTest(s, "PUT", archivePath, "before-release", body); response.Code != 409 {
				t.Fatal("unreleased session archived through API", response.Code, response.Body.String())
			}
			if response := supervisionRequestForTest(s, "DELETE", artifactPath, "", ""); response.Code != 409 {
				t.Fatal("unreleased artifacts removed through API", response.Code, response.Body.String())
			}
			if actual, err := os.ReadFile(file); err != nil || string(actual) != string(data) {
				t.Fatal("rejected cleanup changed artifact", err)
			}
			if missingAdmission {
				return // No durable release proof: missing metadata cannot unlock either API.
			}
			if err := s.releaseTerminalSupervision(ctx, ss); err != nil {
				t.Fatal(err)
			}
			if response := supervisionRequestForTest(s, "PUT", archivePath, "after-release", body); response.Code != 204 {
				t.Fatal("released session could not archive", response.Code, response.Body.String())
			}
			if response := supervisionRequestForTest(s, "DELETE", artifactPath, "", ""); response.Code != 204 {
				t.Fatal("released artifacts could not clean", response.Code, response.Body.String())
			}
			if _, err := os.Stat(file); !os.IsNotExist(err) {
				t.Fatal("successful cleanup retained artifact", err)
			}
		})
	}
}
