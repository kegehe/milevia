//go:build windows

package app

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

// Actual deployment and stdio RPC through the production WSL Runner bridge.
// All Linux state is under a newly created test home, retained for inspection.
func TestSupervisionTargetWSLVerifiedDeploymentUpgradeAndIdentity(t *testing.T) {
	bundle := os.Getenv("MILEVIA_SUPERVISION_TARGET_BUNDLE")
	if bundle == "" {
		t.Skip("set MILEVIA_SUPERVISION_TARGET_BUNDLE to a built target bundle")
	}
	distro := os.Getenv("MILEVIA_SUPERVISION_TARGET_DISTRO")
	if distro == "" {
		distro = "Ubuntu-24.04"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	runner := newWSLAgentRunner(Config{}, distro, ctx)
	wsl, err := wslExePath()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := exec.CommandContext(ctx, wsl, "-d", distro, "--exec", "mktemp", "-d", "/tmp/milevia-target-deployment-XXXXXX").Output()
	if err != nil {
		t.Fatal(err)
	}
	root := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(root, "/tmp/milevia-target-deployment-") || strings.ContainsAny(root, "\r\n") {
		t.Fatal("unexpected test root")
	}
	targetHome := root + "/space ; literal-$value"
	config := Config{SupervisionTargetsDir: bundle}
	link, err := openSupervisionTargetAt(ctx, config, runner, "", targetHome)
	if err != nil {
		t.Fatal("verified WSL deployment failed", err)
	}
	targetID := link.TargetID
	if err := link.Close(); err != nil {
		t.Fatal("target did not close cleanly", err)
	}
	link, err = openSupervisionTargetAt(ctx, config, runner, targetID, targetHome)
	if err != nil || link.TargetID != targetID {
		t.Fatal("reconnection lost durable target identity", err)
	}
	if err := link.Close(); err != nil {
		t.Fatal(err)
	}
	if unexpected, err := openSupervisionTargetAt(ctx, config, runner, "different-target", targetHome); err == nil {
		unexpected.Close()
		t.Fatal("changed target identity silently rebound")
	}
	// Build a second valid content-addressed helper by appending inert ELF data.
	// It must coexist with the original, while using the same persistent store.
	artifact, body, err := targetexec.LoadHelper(bundle, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, []byte("\nMILEVIA-UPGRADE-PROTOCOL-TEST\n")...)
	upgraded := artifact
	upgraded.Size, upgraded.SHA256 = int64(len(body)), supervisionSHA256(body)
	upgradeBundle := t.TempDir()
	manifest := targetexec.HelperBundle{Protocol: 1, Fingerprint: strings.Repeat("a", 64), GoVersion: "integration-fixture", LaunchArgs: []string{"--mode", "supervision-target"}, Artifacts: []targetexec.HelperArtifact{upgraded}}
	for name, data := range map[string][]byte{"manifest.json": mustJSON(manifest), upgraded.File: body} {
		if err := os.WriteFile(filepath.Join(upgradeBundle, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	link, err = openSupervisionTargetAt(ctx, Config{SupervisionTargetsDir: upgradeBundle}, runner, targetID, targetHome)
	if err != nil || link.Artifact.SHA256 != upgraded.SHA256 {
		t.Fatal("verified upgrade failed", err)
	}
	if err := link.Close(); err != nil {
		t.Fatal(err)
	}
	// Both binaries are retained; an upgrade never replaces the old running file.
	for _, sha := range []string{artifact.SHA256, upgraded.SHA256} {
		path := targetHome + "/.local/share/milevia/supervision-targets/" + sha + "/helper"
		if _, err := runner.supervisionTargetCommand(ctx, `test -f "$1"`, nil, path); err != nil {
			t.Fatal("upgrade overwrote original helper", err)
		}
	}
	// Refuse a replaced installed helper, without falling back to an older one.
	if _, err := runner.supervisionTargetCommand(ctx, `printf corrupted > "$1"`, nil, targetHome+"/.local/share/milevia/supervision-targets/"+upgraded.SHA256+"/helper"); err != nil {
		t.Fatal(err)
	}
	if unexpected, err := openSupervisionTargetAt(ctx, Config{SupervisionTargetsDir: upgradeBundle}, runner, targetID, targetHome); err == nil {
		unexpected.Close()
		t.Fatal("tampered target helper executed")
	}
	summary := map[string]any{"passed": true, "distro": distro, "targetHome": targetHome, "targetId": targetID, "initialHelper": artifact, "upgradedHelper": upgraded, "identityChangeRejected": true, "tamperedHelperRejected": true}
	output := filepath.Join(os.TempDir(), "milevia-supervision-target-wsl-deployment-summary.json")
	data, _ := json.MarshalIndent(summary, "", "  ")
	if err := os.WriteFile(output, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Log("retained deployment material:", output)
}
