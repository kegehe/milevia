package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIRuntimeDiagnosisDoesNotGuessInstalledRequirements(t *testing.T) {
	entry := mustAgent(t, "claude-code")
	for _, kind := range []string{installKindNpmManaged, installKindNpmSystem} {
		t.Run(kind, func(t *testing.T) {
			server := &Server{}
			record := agentInstallation{Prefix: t.TempDir(), InstallKind: kind}
			install := agentNpmCLIInstall(entry)
			pkgPath := filepath.Join(install.packageRoot(record.Prefix), install.packageName, "package.json")
			if err := os.MkdirAll(filepath.Dir(pkgPath), 0700); err != nil {
				t.Fatal(err)
			}
			systemDir := t.TempDir()
			writeExecutable(t, filepath.Join(systemDir, "npm"+exeSuffixForTest()), "10.0.0")
			writeExecutable(t, filepath.Join(systemDir, "node"+exeSuffixForTest()), "20.0.0")
			t.Setenv("PATH", systemDir)
			shared := diagnoseShared{runtime: func() runtimeStatus {
				return runtimeStatus{Installed: true, Origin: "managed", Version: "20.0.0", NpmVersion: "10.0.0"}
			}}
			for _, c := range []struct {
				name, contents       string
				wantOld, wantUnknown bool
			}{
				{"missing", "", false, true},
				{"invalid-json", "{", false, true},
				{"wrong-package", `{"name":"other","engines":{"node":">=22"}}`, false, true},
				{"oversized", strings.Repeat(" ", authOutputLimit+1), false, true},
				{"complex-range", fmt.Sprintf(`{"name":%q,"engines":{"node":"^22 || >=24"}}`, entry.NpmPackage), false, true},
				{"no-engine", fmt.Sprintf(`{"name":%q}`, entry.NpmPackage), false, false},
				{"historical", fmt.Sprintf(`{"name":%q,"engines":{"node":">=18"}}`, entry.NpmPackage), false, false},
				{"too-old", fmt.Sprintf(`{"name":%q,"engines":{"node":">=22"}}`, entry.NpmPackage), true, false},
			} {
				t.Run(c.name, func(t *testing.T) {
					if c.contents != "" {
						if err := os.WriteFile(pkgPath, []byte(c.contents), 0600); err != nil {
							t.Fatal(err)
						}
					}
					report := agentDiagnosis{}
					server.assessRuntime(context.Background(), shared, &report, entry, record, true)
					if c.wantOld {
						requireIssue(t, report, issueRuntimeTooOld)
					} else {
						requireNoIssue(t, report, issueRuntimeTooOld)
					}
					finalizeDiagnosis(&report, true)
					if c.wantUnknown && report.Status != diagnosisUnknown {
						t.Fatalf("unknown requirement diagnosed as %s: %+v", report.Status, report)
					}
					if !c.wantUnknown && len(report.Limitations) != 0 {
						t.Fatalf("known requirement has limitations: %+v", report)
					}
				})
			}
		})
	}
}

func TestCLIRuntimeUnknownDoesNotHideConfirmedFailure(t *testing.T) {
	report := agentDiagnosis{}
	report.limit("运行时兼容性未知")
	report.add(issueRuntimeBroken, severityBlocker, "运行时不可执行", []string{"fixture"})
	finalizeDiagnosis(&report, true)
	if report.Status != diagnosisBroken {
		t.Fatalf("known blocker hidden: %+v", report)
	}
}
