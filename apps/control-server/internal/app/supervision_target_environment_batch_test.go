package app

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kegehe/milevia/apps/control-server/internal/targetexec"
)

func TestSupervisionTargetEnvironmentsCoverFiveHundredChecksAndDistinctStartupFiles(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node unavailable")
	}
	ctx := context.Background()
	root := t.TempDir()
	writeSupervisionSourceForTest(t, root, "first.cjs", "console.log('first');\n")
	writeSupervisionSourceForTest(t, root, "second.cjs", "console.log('second');\n")
	checks := make([]supervisionCheck, 0, 502)
	for i := 0; i < 500; i++ {
		checks = append(checks, supervisionCheck{ID: fmt.Sprintf("task-%d", i), Program: "node", Args: []string{fmt.Sprintf("task-%d/check.cjs", i)}})
	}
	for _, name := range []string{"first.cjs", "second.cjs"} {
		checks = append(checks, supervisionCheck{ID: name, Program: "node", Args: []string{filepath.Join(root, name)}})
	}
	base, err := supervisionCheckEnvironment(ctx, root, checks[0])
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]string{}
	for i, check := range checks {
		value := base
		if i >= 500 {
			value, err = supervisionCheckEnvironment(ctx, root, check)
			if err != nil || value == base {
				t.Fatal("absolute startup file was not independently observed", i, err)
			}
		}
		expected[supervisionCheckSemanticHash(check)] = value
	}
	actual, err := collectSupervisionTargetEnvironments(ctx, root, checks)
	if err != nil || len(actual.Checks) != len(checks) || actual.SHA256 != supervisionSHA256(mustJSON(actual.Checks)) {
		t.Fatal("selected batch lost check coverage", len(actual.Checks), err)
	}
	for _, observation := range actual.Checks {
		if expected[observation["check"]] != observation["environment"] {
			t.Fatal("runtime observation was reused for a different startup file")
		}
		delete(expected, observation["check"])
	}
	if len(expected) != 0 {
		t.Fatal("incomplete batch environment observations")
	}
}

func TestSupervisionTargetEnvironmentSpecPreservesExistingCommandAndBrowserHashes(t *testing.T) {
	ctx := context.Background()
	node, _, err := supervisionNativeVerificationCommand("node", nil)
	if err != nil {
		t.Skip("node unavailable")
	}
	root := t.TempDir()
	writeSupervisionSourceForTest(t, root, "startup.cjs", "module.exports=1;\n")
	startup := filepath.Join(root, "startup.cjs")
	for _, browser := range []bool{false, true} {
		check := supervisionCheck{Program: "node", Args: []string{startup}}
		if browser {
			check = supervisionCheck{Browser: &supervisionBrowserCheck{ServerProgram: "node", ServerArgs: []string{startup}}}
		}
		expected, err := supervisionEnvironmentFingerprint(ctx, root, node, startup)
		if err != nil {
			t.Fatal(err)
		}
		if browser {
			expected = supervisionSHA256([]byte(expected + "\x00" + supervisionBrowserDriver))
		}
		actual, err := supervisionCheckEnvironment(ctx, root, check)
		if err != nil || actual != expected {
			t.Fatal("existing evidence environment hash changed", browser, err)
		}
	}
}

func TestSupervisionTargetEnvironmentBatchCapacityIsBoundedBeforeReading(t *testing.T) {
	checks := make([]supervisionCheck, supervisionTargetEnvironmentCheckLimit+1)
	if _, err := collectSupervisionTargetEnvironments(context.Background(), "nonexistent", checks); !errors.Is(err, targetexec.ErrLimit) {
		t.Fatal("oversized batch was not rejected before I/O", err)
	}
}
