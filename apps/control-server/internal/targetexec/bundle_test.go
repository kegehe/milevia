package targetexec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHelperBundleBindsPlatformContentAndLaunchMode(t *testing.T) {
	dir := t.TempDir()
	body := []byte("fixture executable")
	artifact := HelperArtifact{OS: "linux", Arch: "amd64", File: "milevia-supervision-target-linux-amd64", Size: int64(len(body)), SHA256: hashBytes(body)}
	bundle := HelperBundle{Protocol: ProtocolVersion, Fingerprint: strings.Repeat("a", 64), GoVersion: "fixture", LaunchArgs: []string{"--mode", "supervision-target"}, Artifacts: []HelperArtifact{artifact}}
	write := func(value HelperBundle) {
		t.Helper()
		raw, _ := json.Marshal(value)
		if err := os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, artifact.File), body, 0600); err != nil {
		t.Fatal(err)
	}
	write(bundle)
	selected, raw, err := LoadHelper(dir, "linux", "amd64")
	if err != nil || selected != artifact || string(raw) != string(body) {
		t.Fatal(selected, err)
	}
	if _, _, err := LoadHelper(dir, "windows", "amd64"); err == nil {
		t.Fatal("missing platform silently fell back")
	}
	for _, mutation := range []func(*HelperBundle){
		func(b *HelperBundle) { b.Protocol++ },
		func(b *HelperBundle) { b.LaunchArgs = []string{"--mode", "web"} },
		func(b *HelperBundle) { b.Artifacts[0].File = "../outside" },
		func(b *HelperBundle) { b.Artifacts[0].SHA256 = strings.Repeat("b", 64) },
		func(b *HelperBundle) { b.Artifacts[0].Size++ },
		func(b *HelperBundle) { b.Artifacts = append(b.Artifacts, b.Artifacts[0]) },
	} {
		copy := bundle
		copy.Artifacts = append([]HelperArtifact{}, bundle.Artifacts...)
		copy.LaunchArgs = append([]string{}, bundle.LaunchArgs...)
		mutation(&copy)
		write(copy)
		if _, _, err := LoadHelper(dir, "linux", "amd64"); err == nil {
			t.Fatal("altered bundle was accepted", copy)
		}
	}
	write(bundle)
	if err := os.WriteFile(filepath.Join(dir, artifact.File), []byte("fixture executablf"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadHelper(dir, "linux", "amd64"); err == nil {
		t.Fatal("replaced helper was accepted")
	}
}
