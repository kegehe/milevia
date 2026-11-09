package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSupervisionEnvironmentIncludesNestedDependenciesBuildsAndLinkedContent(t *testing.T) {
	binary, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	root := t.TempDir()
	writeSupervisionSourceForTest(t, root, "apps/web/node_modules/pkg/dist/main.js", "module.exports=1;\n")
	fingerprint := func() string {
		t.Helper()
		value, err := supervisionEnvironmentFingerprint(context.Background(), root, binary)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := fingerprint()
	writeSupervisionSourceForTest(t, root, "apps/web/node_modules/pkg/dist/main.js", "module.exports=2;\n")
	after := fingerprint()
	if before == after {
		t.Fatal("nested dependency dist content was ignored")
	}
	writeSupervisionSourceForTest(t, root, "apps/web/node_modules/pkg/.cache/temp", "cache\n")
	if fingerprint() != after {
		t.Fatal("dependency cache invalidated environment")
	}
	writeSupervisionSourceForTest(t, root, "linked-package/runtime.js", "module.exports=1;\n")
	link := filepath.Join(root, "apps", "web", "node_modules", "linked")
	if err := os.Symlink(filepath.Join(root, "linked-package"), link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	before = fingerprint()
	writeSupervisionSourceForTest(t, root, "linked-package/runtime.js", "module.exports=2;\n")
	if fingerprint() == before {
		t.Fatal("linked dependency target content was ignored")
	}
	outside := t.TempDir()
	writeSupervisionSourceForTest(t, outside, "runtime.js", "module.exports=3;\n")
	if err := os.Symlink(outside, filepath.Join(root, "apps", "web", "node_modules", "outside")); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisionEnvironmentFingerprint(context.Background(), root, binary); err == nil {
		t.Fatal("uncovered external dependency link was accepted")
	}
}
